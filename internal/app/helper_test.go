package app

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/store"
)

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
