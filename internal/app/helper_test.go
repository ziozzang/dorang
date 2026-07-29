package app

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/store"
)

// isolateState gives one test its own writable state directory, and every
// gateway this package assembles goes through it.
//
// [TestMain] points DORANG_STATE_DIR at a temporary directory so that no test
// writes into the operator's `~/.dorang` — a defect that had put 36 MB into a
// live installation before it was noticed, and that CI now fails the build over.
// That fixed the rudeness and left the second half of the same problem standing:
// ONE directory for the whole package is still shared mutable state, and the
// piece of it that matters is the trace spool. It is an append log with a read
// cursor, [meter.openDiskSpool] adopts and re-validates whatever segments it
// finds at open, and un-acknowledged records are replayed — into the store of
// whichever gateway opened it next. Measured on this package before this
// existed: 424 KB of spooled traces at the end of a run with the cursor still at
// the first record, which is every trace every test produced, waiting to be
// shipped into the next test's database.
//
// It survived only because every test overrode the SQLite path to its own
// t.TempDir() and the only assertions that read the spool used ownSpool. That is
// a property of the tests that happen to exist, not of the harness: a test that
// counts ledger rows without filtering finds it immediately, and it presents as
// a flake because it depends on what ran before.
//
// So the state directory is per test, and there is nothing left to remember —
// t.Setenv restores the package-level value afterwards, and TestMain fails the
// run if anything was written into it, which is what catches the next test that
// assembles a gateway some other way.
//
// t.Setenv rather than a config field because the defaults that resolve against
// `~` are several (the spool, the embedded database, the shadow report, the
// generated pepper) and naming them one at a time is the arrangement that let
// this one through. It also forbids t.Parallel, which no test in this package
// uses.
func isolateState(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvStateDir, t.TempDir())
}

// testPepper is the HMAC pepper every test in this package hashes under. The
// adapters here join two packages that must agree on it, so it is one constant
// rather than a literal repeated per test.
const testPepper = "app-test-pepper-not-a-real-secret" // pragma: allowlist secret — test fixture

// testMasterKey is the out-of-band administrative credential these tests use.
// It is what reaches an Admin route such as /metrics.
const testMasterKey = "app-test-master-key" // pragma: allowlist secret — test fixture

// testUpstreamKey is the value the fixtures' key_env points at. Fixtures used
// to spell their secret key_ref, which resolved to nothing — which is exactly
// why key_ref is now refused.
const testUpstreamKey = "app-test-upstream-key" // pragma: allowlist secret — test fixture

// adminRequest stamps the master credential on a request.
func adminRequest(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer "+testMasterKey)
	return r
}

// openTestStore opens a migrated SQLite store in a temporary directory.
//
// The path is returned as well as the store so that a test can close one store
// and open another against the same file — which is what "survives a restart"
// means in a single process.
func openTestStore(t *testing.T, mutate func(*store.Config)) (*store.Store, string) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "dorang.db")
	return openTestStoreAt(t, dsn, mutate), dsn
}

// openTestStoreAt opens a store against an existing path.
func openTestStoreAt(t *testing.T, dsn string, mutate func(*store.Config)) *store.Store {
	t.Helper()
	cfg := store.Config{
		Driver: store.DialectSQLite,
		DSN:    dsn,
		Pepper: []byte(testPepper),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	st, err := store.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// waitFor polls until cond holds or fails the test. Nothing in this package is
// driven by a fixed sleep: the asynchronous paths under test (the rehash worker,
// the batch scheduler) finish in microseconds and a sleep long enough to be safe
// would dominate the suite.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
