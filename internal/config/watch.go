package config

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// DefaultPollInterval is how often a [Watcher] stats the configuration file.
const DefaultPollInterval = 2 * time.Second

// Watcher holds the current configuration and replaces it when the file
// changes or the process receives SIGHUP.
//
// A reload re-reads, defaults, resolves and validates the file, and only then
// swaps the snapshot. A file that fails any of those steps leaves the previous
// configuration in place and surfaces the error: there is no partial apply.
//
// Readers take no lock. [Watcher.Config] returns the current snapshot through
// an atomic pointer, and a snapshot is never mutated after publication, so an
// in-flight request keeps the configuration it started with (§4.1, §15.2).
type Watcher struct {
	path     string
	interval time.Duration
	signals  []os.Signal

	cfg     atomic.Pointer[Config]
	lastErr atomic.Pointer[errBox]

	onReload func(*Config) error
	onError  func(error)

	mu   sync.Mutex // serializes reloads
	stat os.FileInfo

	// also are further files or directories whose change reloads the
	// configuration: layers the running process derives from it, such as the
	// model-catalog overlays, which have no file of their own to watch.
	also      []string
	alsoPrint string // fingerprint of also at the last applied reload

	started   atomic.Bool
	startOnce sync.Once
	closeOnce sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
	sigCh     chan os.Signal
}

type errBox struct{ err error }

// WatchOption configures a [Watcher].
type WatchOption func(*Watcher)

// WithPollInterval sets how often the file is stat'ed for modification.
func WithPollInterval(d time.Duration) WatchOption {
	return func(w *Watcher) {
		if d > 0 {
			w.interval = d
		}
	}
}

// WithReloadHandler registers a callback invoked with each newly applied
// configuration. It runs on the watcher's goroutine, so it must not block.
func WithReloadHandler(f func(*Config) error) WatchOption {
	return func(w *Watcher) { w.onReload = f }
}

// WithErrorHandler registers a callback invoked with every reload failure. The
// previous configuration stays in place; the callback is how the failure
// becomes visible. It runs on the watcher's goroutine, so it must not block.
func WithErrorHandler(f func(error)) WatchOption {
	return func(w *Watcher) { w.onError = f }
}

// WithSignals replaces the set of signals that trigger a reload. Passing no
// signals disables signal handling, which is what a test that must not touch
// process-wide state wants.
func WithSignals(sigs ...os.Signal) WatchOption {
	return func(w *Watcher) { w.signals = sigs }
}

// WithAlsoWatch adds files or directories whose change triggers a reload, the
// same as a change to the configuration file itself. A directory counts as
// changed when a *.yaml or *.yml entry appears, disappears or is rewritten —
// including by rename-into-place, which is how a generated file should be
// written. A path that does not exist is watched for its appearance.
func WithAlsoWatch(paths ...string) WatchOption {
	return func(w *Watcher) {
		for _, p := range paths {
			if p != "" {
				w.also = append(w.also, p)
			}
		}
	}
}

// NewWatcher loads path and returns a watcher holding it. The configuration
// must be valid: a watcher never starts with a broken snapshot.
//
// Call [Watcher.Start] to begin watching, and [Watcher.Close] when done.
func NewWatcher(path string, opts ...WatchOption) (*Watcher, error) {
	w := &Watcher{
		path:     path,
		interval: DefaultPollInterval,
		signals:  []os.Signal{syscall.SIGHUP},
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	for _, o := range opts {
		o(w)
	}
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	w.cfg.Store(cfg)
	if fi, err := os.Stat(path); err == nil {
		w.stat = fi
	}
	w.alsoPrint = fingerprint(w.also)
	return w, nil
}

// fingerprint summarizes the watched extra paths: for a file its identity,
// size and modification time; for a directory the same for each YAML entry.
// Two equal fingerprints mean nothing a loader would read has changed.
func fingerprint(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			fmt.Fprintf(&b, "%s:missing;", p)
			continue
		}
		if !fi.IsDir() {
			fmt.Fprintf(&b, "%s:%s;", p, fileStamp(fi))
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			fmt.Fprintf(&b, "%s:unreadable;", p)
			continue
		}
		for _, e := range entries { // ReadDir returns entries sorted by name
			name := e.Name()
			if e.IsDir() || !(strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")) {
				continue
			}
			if efi, err := e.Info(); err == nil {
				fmt.Fprintf(&b, "%s/%s:%s;", p, name, fileStamp(efi))
			}
		}
	}
	return b.String()
}

func fileStamp(fi os.FileInfo) string {
	id := ""
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		id = strconv.FormatUint(st.Ino, 10)
	}
	return fmt.Sprintf("%s/%d/%d", id, fi.Size(), fi.ModTime().UnixNano())
}

// Path returns the file being watched.
func (w *Watcher) Path() string { return w.path }

// Config returns the current snapshot. It takes no lock.
func (w *Watcher) Config() *Config { return w.cfg.Load() }

// LastError returns the most recent reload failure, or nil if the last reload
// succeeded.
func (w *Watcher) LastError() error {
	if b := w.lastErr.Load(); b != nil {
		return b.err
	}
	return nil
}

// Start begins watching. It is safe to call more than once; only the first
// call has an effect.
func (w *Watcher) Start() {
	w.startOnce.Do(func() {
		w.started.Store(true)
		if len(w.signals) > 0 {
			w.sigCh = make(chan os.Signal, 1)
			signal.Notify(w.sigCh, w.signals...)
		}
		go w.run()
	})
}

// Close stops watching and releases the signal registration.
func (w *Watcher) Close() error {
	w.closeOnce.Do(func() {
		if w.sigCh != nil {
			signal.Stop(w.sigCh)
		}
		close(w.stopCh)
		if !w.started.Load() {
			return // nothing is running, so there is nothing to wait for
		}
		select {
		case <-w.doneCh:
		case <-time.After(5 * time.Second):
		}
	})
	return nil
}

func (w *Watcher) run() {
	defer close(w.doneCh)
	if w.interval <= 0 {
		w.interval = DefaultPollInterval
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	var sigCh <-chan os.Signal
	if w.sigCh != nil {
		sigCh = w.sigCh
	}
	for {
		select {
		case <-w.stopCh:
			return
		case <-sigCh:
			w.reload(true)
		case <-t.C:
			if w.changed() {
				w.reload(false)
			} else if len(w.also) > 0 && w.alsoChanged() {
				// The configuration file is unchanged, so the reload is forced:
				// what changed is a layer the process builds from it.
				w.reload(true)
			}
		}
	}
}

// changed reports whether the file looks different from the one last applied.
// A rename-into-place, which is how most editors and configuration management
// tools write, changes the identity as well as the timestamp.
func (w *Watcher) changed() bool {
	fi, err := os.Stat(w.path)
	if err != nil {
		// The file vanished (or a rename is in flight). Report it once and
		// keep the current configuration.
		w.mu.Lock()
		w.failLocked(err, true)
		w.mu.Unlock()
		return false
	}
	w.mu.Lock()
	prev := w.stat
	w.mu.Unlock()
	if prev == nil {
		return true
	}
	return !os.SameFile(prev, fi) ||
		!prev.ModTime().Equal(fi.ModTime()) ||
		prev.Size() != fi.Size()
}

// alsoChanged reports whether the extra watched paths differ from the last
// applied reload.
func (w *Watcher) alsoChanged() bool {
	now := fingerprint(w.also)
	w.mu.Lock()
	defer w.mu.Unlock()
	return now != w.alsoPrint
}

// Reload re-reads the file now. On failure the previous configuration stays in
// place and the error is returned as well as reported to the error handler.
// It is safe for concurrent use and is what an admin reload endpoint calls.
func (w *Watcher) Reload() error { return w.reload(true) }

func (w *Watcher) reload(force bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	fi, statErr := os.Stat(w.path)
	if statErr != nil {
		return w.failLocked(statErr, false)
	}
	// Taken before the load, so an extra path that changes while this reload
	// runs is seen as changed on the next tick rather than lost. Recorded
	// whatever the outcome, like the file's stat: a broken layer that stays
	// broken is not re-applied on every tick.
	w.alsoPrint = fingerprint(w.also)
	if !force && w.stat != nil && os.SameFile(w.stat, fi) &&
		w.stat.ModTime().Equal(fi.ModTime()) && w.stat.Size() == fi.Size() {
		return nil
	}

	cfg, err := Load(w.path)
	if err != nil {
		// Record what we saw so a file that is broken and unchanged is not
		// re-read on every tick; a further edit changes the stat again.
		w.stat = fi
		return w.failLocked(err, false)
	}
	w.stat = fi
	w.cfg.Store(cfg)
	w.lastErr.Store(nil)
	if w.onReload != nil {
		// The handler applies the new config; if the process refuses it (a
		// valid file that still cannot be built into a running gateway), that
		// refusal is the reload's outcome and is returned to an explicit caller
		// so an operator sees it, rather than being swallowed into a success.
		// The stat is already recorded above, so the poll path does not retry a
		// file it has seen; a further edit changes the stat again.
		if err := w.onReload(cfg); err != nil {
			return err
		}
	}
	return nil
}

// failLocked records a reload failure. dedup suppresses the callback for a
// repeat of the same error, which is what the poll path wants when the file
// stays missing; an explicit reload always reports.
func (w *Watcher) failLocked(err error, dedup bool) error {
	prev := w.lastErr.Load()
	repeat := dedup && prev != nil && prev.err != nil && prev.err.Error() == err.Error()
	w.lastErr.Store(&errBox{err: err})
	if w.onError != nil && !repeat {
		w.onError(err)
	}
	return err
}
