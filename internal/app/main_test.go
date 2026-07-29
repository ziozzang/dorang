package app

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/ziozzang/dorang/internal/config"
)

// TestMain redirects the state directory for the whole package, and then makes
// sure nothing uses the one it redirected to.
//
// # Why the redirect exists
//
// Several defaults are written as `~/...` — the trace spool, the embedded
// database, the shadow report — and [config.StateDir] resolves `~` to the
// operator's home when DORANG_STATE_DIR is unset. Nothing in this package's
// tests overrode the spool, so every run appended to the real one: 36 MB had
// accumulated in a live installation's directory by the time it was noticed.
// CI now fails the build if the suite adds a file under $HOME/.dorang, so this
// is load-bearing and not merely tidy.
//
// # Why the redirect was not enough
//
// It moved the problem rather than removing it. One directory for the whole
// package is still shared mutable state that nothing declares, and the trace
// spool is a place where sharing has consequences: it is an append log with a
// read cursor, un-acknowledged records are replayed at open, and the gateway
// that replays them is whichever one opened the directory next. Measured before
// [isolateState] existed: 424 KB of spooled traces at the end of a run with the
// cursor still at the first record — every trace every test produced, queued to
// be shipped into the next test's own database.
//
// Nothing failed, because every test overrode the SQLite path to its own
// t.TempDir() and the only assertions that read the spool used ownSpool. That is
// a property of the tests that happen to exist. The first test to count ledger
// rows without filtering finds it, and finds it as a flake, because what it sees
// depends on what ran before it.
//
// # What holds now
//
// [isolateState] gives every gateway this package assembles its own state
// directory, so the package-level one below should never be written to at all.
// The check after m.Run is what makes that a property of the harness rather than
// a convention: a test added tomorrow that assembles a gateway some other way
// leaves a file here and fails the run, naming what it left and where to fix it.
// A convention that has to be remembered is a convention that will be forgotten,
// which is exactly how the original defect survived.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "dorang-app-test-state-")
	if err != nil {
		panic("state dir: " + err.Error())
	}
	if err := os.Setenv(config.EnvStateDir, dir); err != nil {
		panic("set " + config.EnvStateDir + ": " + err.Error())
	}
	code := m.Run()
	if code == 0 {
		if leaked := stateFiles(dir); len(leaked) > 0 {
			fmt.Fprintf(os.Stderr,
				"internal/app: the suite wrote %d file(s) into the package-wide state "+
					"directory, which is shared by every test in this package:\n", len(leaked))
			for _, f := range leaked {
				fmt.Fprintf(os.Stderr, "  %s\n", f)
			}
			fmt.Fprintf(os.Stderr,
				"The trace spool that lives there is replayed into whichever gateway opens "+
					"it next, so this is state carried between tests. Call isolateState(t) "+
					"before assembling a gateway; see internal/app/helper_test.go.\n")
			code = 1
		}
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// stateFiles lists what was written under dir, relative to it.
func stateFiles(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // a directory that cannot be walked has nothing to report
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			rel = path
		}
		out = append(out, rel)
		return nil
	})
	return out
}
