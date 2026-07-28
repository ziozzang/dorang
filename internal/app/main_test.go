package app

import (
	"os"
	"testing"

	"github.com/ziozzang/dorang/internal/config"
)

// TestMain redirects the state directory for the whole package.
//
// Several defaults are written as `~/...` — the trace spool, the embedded
// database, the shadow report — and [config.StateDir] resolves `~` to the
// operator's home when DORANG_STATE_DIR is unset. Nothing in this package's
// tests overrode the spool, so every run appended to the real one: 36 MB had
// accumulated in a live installation's directory by the time it was noticed.
//
// Two separate harms, and the second is the one that matters. Writing into the
// operator's own state is rude. But the spool is *shared*, so it also carries
// state between test runs and between tests — and it had grown large enough to
// slow the meter under test, which means results measured against it were
// results measured against whatever earlier runs had left behind.
//
// This is set here rather than in each test's config because a default that has
// to be remembered is a default that will be forgotten by the next test added,
// and the failure is silent: nothing fails, a directory just grows.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "dorang-app-test-state-")
	if err != nil {
		panic("state dir: " + err.Error())
	}
	if err := os.Setenv(config.EnvStateDir, dir); err != nil {
		panic("set " + config.EnvStateDir + ": " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
