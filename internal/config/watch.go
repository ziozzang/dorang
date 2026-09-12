package config

import (
	"os"
	"os/signal"
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
	return w, nil
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
