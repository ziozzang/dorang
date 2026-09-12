package config

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// Advisory file locking for the config file.
//
// The config is a single-file bind mount shared across the cluster's nodes, and
// it is edited in place (a rename would swap the inode out from under the
// mount). In-place editing is not atomic on its own, so reads and writes
// coordinate through an advisory lock on the file's inode — which the bind
// mount shares, so the lock serializes the writer against every node's reader,
// not just goroutines in one process. Readers take a shared lock, the writer an
// exclusive one, so a reader never observes a half-written file and two writers
// never interleave.

// readLocked reads a file's whole contents under a shared advisory lock.
func readLocked(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return nil, fmt.Errorf("shared lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return io.ReadAll(f)
}

// EditLocked applies edit to a file's contents with an exclusive advisory lock
// held across the whole read-modify-write, so a concurrent writer on any node
// cannot lose this edit and no reader observes a partial file. edit receives
// the current bytes and returns the new bytes; returning an error writes
// nothing. The new bytes are written over the file's existing inode and it is
// then truncated to the new length, so the file is never empty mid-write.
//
// It does NOT fsync-rename: the file is a bind mount whose inode must be
// preserved. The residual hazard is a crash between the write and the truncate,
// which a validated edit and the reader's refusal of an unparseable file keep
// from taking the fleet down — the running config stays until a good file is
// read.
func EditLocked(path string, edit func(current []byte) ([]byte, error)) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("exclusive lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	cur, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	next, err := edit(cur)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(next, 0); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Truncate(int64(len(next))); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	return f.Sync()
}
