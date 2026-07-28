package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var mtimeBump atomic.Int64

// writeConfig writes a configuration file and gives it a distinct modification
// time, so the test does not depend on the clock's resolution.
func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(time.Duration(mtimeBump.Add(1)) * time.Second)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func modelConfig(name string) string {
	return strings.Replace(buildYAML(fragments{}), "  - name: m1", "  - name: "+name, 1)
}

func TestWatcherRejectsAnInvalidStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dorang.yaml")
	writeConfig(t, path, "version: 1\ncluster: {enabled: true, capacity_mode: local}\n")
	if _, err := NewWatcher(path); err == nil {
		t.Fatal("a watcher must not start on an invalid configuration")
	}
}

// TestWatcherAppliesAValidChange is the happy path: the file changes and the
// snapshot follows.
func TestWatcherAppliesAValidChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dorang.yaml")
	writeConfig(t, path, modelConfig("m1"))

	reloaded := make(chan struct{}, 4)
	w, err := NewWatcher(path,
		WithPollInterval(5*time.Millisecond),
		WithSignals(),
		WithReloadHandler(func(*Config) { reloaded <- struct{}{} }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Start()

	if _, ok := w.Config().Model("m1"); !ok {
		t.Fatal("initial configuration is missing model m1")
	}

	writeConfig(t, path, modelConfig("m2"))
	waitFor(t, reloaded, "a reload")

	if _, ok := w.Config().Model("m2"); !ok {
		t.Error("the new configuration was not applied")
	}
	if w.LastError() != nil {
		t.Errorf("LastError() = %v, want nil", w.LastError())
	}
}

// TestWatcherKeepsTheOldConfigOnAnInvalidChange is the rule that matters: a
// broken file must never be applied, not even partly.
func TestWatcherKeepsTheOldConfigOnAnInvalidChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dorang.yaml")
	writeConfig(t, path, modelConfig("m1"))

	failed := make(chan struct{}, 4)
	reloaded := make(chan struct{}, 4)
	w, err := NewWatcher(path,
		WithPollInterval(5*time.Millisecond),
		WithSignals(),
		WithErrorHandler(func(error) { failed <- struct{}{} }),
		WithReloadHandler(func(*Config) { reloaded <- struct{}{} }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Start()

	before := w.Config()

	// Invalid: a cluster with node-local capacity, plus a dangling alias.
	writeConfig(t, path, modelConfig("m1")+
		"cluster: {enabled: true, capacity_mode: local}\naliases: {a: nowhere}\n")
	waitFor(t, failed, "a reload failure")

	if w.Config() != before {
		t.Error("an invalid configuration replaced the running one")
	}
	if _, ok := w.Config().Model("m1"); !ok {
		t.Error("the running configuration was damaged")
	}
	err = w.LastError()
	if err == nil {
		t.Fatal("LastError() is nil after a failed reload")
	}
	if len(Problems(err)) != 2 {
		t.Errorf("want both problems reported, got %v", err)
	}

	// Half-broken is still broken: only a fully valid file is applied.
	writeConfig(t, path, modelConfig("m3"))
	waitFor(t, reloaded, "a reload")
	if _, ok := w.Config().Model("m3"); !ok {
		t.Error("a valid configuration was not applied after a failure")
	}
	if w.LastError() != nil {
		t.Errorf("LastError() was not cleared: %v", w.LastError())
	}
}

func TestWatcherExplicitReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dorang.yaml")
	writeConfig(t, path, modelConfig("m1"))

	w, err := NewWatcher(path, WithSignals(), WithPollInterval(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	writeConfig(t, path, modelConfig("m2"))
	if err := w.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := w.Config().Model("m2"); !ok {
		t.Error("an explicit reload did not apply the new configuration")
	}

	before := w.Config()
	writeConfig(t, path, "version: 1\nproviders: [{name: p1, kind: openai}]\nmodels: [{name: m1, deployments: [{provider: nope, upstream_model: u}]}]\n")
	if err := w.Reload(); err == nil {
		t.Fatal("an explicit reload of an invalid file must return the error")
	}
	if w.Config() != before {
		t.Error("a failed explicit reload replaced the running configuration")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := w.Reload(); err == nil {
		t.Fatal("reloading a missing file must fail")
	}
	if w.Config() != before {
		t.Error("a missing file replaced the running configuration")
	}
}

// TestWatcherReloadsOnSIGHUP covers the second trigger.
func TestWatcherReloadsOnSIGHUP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dorang.yaml")
	writeConfig(t, path, modelConfig("m1"))

	reloaded := make(chan struct{}, 4)
	w, err := NewWatcher(path,
		WithPollInterval(time.Hour), // only the signal can trigger a reload
		WithReloadHandler(func(*Config) { reloaded <- struct{}{} }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Start()

	writeConfig(t, path, modelConfig("m2"))
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, reloaded, "a reload on SIGHUP")
	if _, ok := w.Config().Model("m2"); !ok {
		t.Error("SIGHUP did not apply the new configuration")
	}
}

// TestWatcherReadersTakeNoLock hammers the snapshot from several goroutines
// while it is being replaced. Run under -race, this is the test that the
// snapshot is really immutable.
func TestWatcherReadersTakeNoLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dorang.yaml")
	writeConfig(t, path, modelConfig("m1"))

	w, err := NewWatcher(path, WithSignals(), WithPollInterval(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Start()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c := w.Config()
				if c == nil {
					t.Error("Config() returned nil")
					return
				}
				_ = c.Server.Listen
				_, _ = c.Model("m1")
				_ = c.ResolveAlias("anything")
			}
		}()
	}
	for i := 0; i < 20; i++ {
		writeConfig(t, path, modelConfig("m1"))
		if err := w.Reload(); err != nil {
			t.Errorf("reload: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestWatcherPathAndClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dorang.yaml")
	writeConfig(t, path, modelConfig("m1"))
	w, err := NewWatcher(path, WithSignals())
	if err != nil {
		t.Fatal(err)
	}
	if w.Path() != path {
		t.Errorf("Path() = %q, want %q", w.Path(), path)
	}
	w.Start()
	w.Start() // idempotent
	if err := w.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}
