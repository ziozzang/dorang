package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/redact"
	"github.com/ziozzang/dorang/internal/store"
)

// These are the assembled-stack assertions for DESIGN §11.2b.
//
// internal/auth has had a working OAuth credential, with its own tests, since it
// was written. internal/backend has had the Applier seam and a compile-time
// assertion that *auth.OAuthCredential satisfies it. Neither could tell you that
// no request had ever carried an OAuth token, because the dispatcher returned nil
// for OAuth and internal/config had no way to declare one. So every test here
// goes in through a.Server.ServeHTTP with a real socket on the other end, and
// reads what the upstream ACTUALLY received — a test asserting the Applier is
// non-nil proves nothing about a request.

// Fixture tokens. They are long and distinctive so that a leak into a log, an
// error, a metric or a ledger row is unmistakable, and over redact.MinLen so
// that the scrubbers treat them as secrets rather than as ordinary words.
const (
	staleAccess  = "oauth-stale-access-token-fixture"      // pragma: allowlist secret — test fixture
	staleRefresh = "oauth-stale-refresh-token-fixture"     // pragma: allowlist secret — test fixture
	freshAccess  = "oauth-refreshed-access-token-fixture"  // pragma: allowlist secret — test fixture
	freshRefresh = "oauth-refreshed-refresh-token-fixture" // pragma: allowlist secret — test fixture
)

// chatOK is the body a fake upstream answers a chat request with.
const chatOK = `{"id":"x","object":"chat.completion","model":"m1-upstream",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},` +
	`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

const chatReq = `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`

// writeTokenStore writes a generic-format store into the test's own temporary
// directory.
//
// It is never a path under a home directory and never a copy of a real one. The
// three vendor layouts are covered by shape in internal/auth; what is under test
// here is the wiring, and pointing a test at a developer's live store could
// refresh their token and write it back — logging them out of their own tooling.
func writeTokenStore(t *testing.T, access, refresh string, expires time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	doc := map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		// A key dorang does not own, so that a write-back that drops it fails.
		"vendor_note": "written by a fixture",
	}
	if !expires.IsZero() {
		doc["expires_at"] = expires.UTC().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeTokenEndpoint is an OAuth token endpoint that mints one known successor.
// It is a fake on purpose: a test that refreshed against a real endpoint would
// spend a real refresh token.
func fakeTokenEndpoint(t *testing.T) (url string, calls func() int) {
	t.Helper()
	var mu sync.Mutex
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":3600}`,
			freshAccess, freshRefresh)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// ownSpool keeps a test's trace spool in its own temporary directory.
//
// metering.spool.dir defaults to ~/.dorang/spool, so without this every
// assembled-gateway test in this package writes its traces under the developer's
// home directory and they accumulate there. It also makes the spool READABLE by
// the artifact sweep below: a trace that has not reached the store yet is still
// a record dorang wrote to disk.
func ownSpool(t *testing.T) func(*config.Config) {
	dir := t.TempDir()
	return func(c *config.Config) { c.Metering.Spool.Dir = dir }
}

// oauthYAMLFor renders a one-credential gateway whose only credential
// authenticates by OAuth.
func oauthYAMLFor(upstream, store, tokenURL string) string {
	refresh := ""
	if tokenURL != "" {
		refresh = fmt.Sprintf(`
      refresh:
        token_url: %q
        client_id: fixture-client-id`, tokenURL)
	}
	return fmt.Sprintf(`
version: 1
observability: {always_full_headers: true}
providers:
  - {name: p1, kind: openai, base_url: %q}
credentials:
  - id: oauth-1
    provider: p1
    auth: oauth
    oauth:
      source: file
      path: %q
      refresh_margin: 1m
      poll_interval: 5s%s
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [oauth-1]}
`, upstream, store, refresh)
}

// TestAnOAuthCredentialAuthorizesAnUpstreamRequest is the definition of done:
// an OAuth credential is declarable in configuration and reaches an outbound
// request. The assertion is the header the upstream received.
func TestAnOAuthCredentialAuthorizesAnUpstreamRequest(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatOK)
	}))
	defer up.Close()

	store := writeTokenStore(t, staleAccess, staleRefresh, time.Now().Add(time.Hour))
	a := newWiringApp(t, oauthYAMLFor(up.URL, store, ""), ownSpool(t),
		func(o *Options) { o.Upstream = up.Client() })
	secret := issueKey(t, a, nil)

	if a.OAuth == nil {
		t.Fatal("no OAuth credential set was built from a configuration that declares one")
	}
	if w := callWith(a, secret, http.MethodPost, "/v1/chat/completions", chatReq); w.Code != http.StatusOK {
		t.Fatalf("request answered %d: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if want := "Bearer " + staleAccess; gotAuth != want {
		t.Fatalf("the upstream received Authorization %q, want %q: the OAuth credential "+
			"never reached the request", gotAuth, want)
	}
}

// TestACredentialWithNoRefresherStillReportsThatItIsBeingRead.
//
// No `refresh` block is the safe default and the common case: the vendor's CLI
// keeps its own token current and dorang only reads the store. That deployment
// never exchanges anything, so dorang_oauth_refreshes_total sits at zero for the
// life of the process — and a store that silently stopped being updated would
// look exactly like one that is fine. The store-load count is the only number
// that moves for it, so it has to be published.
func TestACredentialWithNoRefresherStillReportsThatItIsBeingRead(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatOK)
	}))
	defer up.Close()

	store := writeTokenStore(t, staleAccess, staleRefresh, time.Now().Add(time.Hour))
	a := newWiringApp(t, oauthYAMLFor(up.URL, store, ""), ownSpool(t),
		func(o *Options) { o.Upstream = up.Client() })

	c, ok := a.OAuth.Credential("oauth-1")
	if !ok {
		t.Fatal("the credential is not registered")
	}
	if c.Refreshes() != 0 {
		t.Fatalf("a credential with no refresh endpoint exchanged %d tokens", c.Refreshes())
	}
	waitFor(t, "the store to be read", func() bool { return c.StoreLoads() > 0 })

	page := callWith(a, testMasterKey, http.MethodGet, "/metrics", "").Body.String()
	var line string
	for _, l := range strings.Split(page, "\n") {
		if strings.HasPrefix(l, "dorang_oauth_store_loads_total{") &&
			strings.Contains(l, `credential="oauth-1"`) {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("the metrics page publishes no store-load count for this credential, so a " +
			"deployment with no refresh endpoint has no number that moves at all")
	}
	if strings.HasSuffix(strings.TrimSpace(line), " 0") {
		t.Errorf("the store-load count is zero after the store was read: %s", line)
	}
}

// TestATokenIsRefreshedAheadOfExpiryAndTheNextRequestCarriesIt is DESIGN
// §11.2b's mechanism, asserted end to end: the refresh happens against a fake
// endpoint, the successor is persisted in the vendor's own store without losing
// a key, and the request AFTER it succeeds with the new token on the wire.
func TestATokenIsRefreshedAheadOfExpiryAndTheNextRequestCarriesIt(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatOK)
	}))
	defer up.Close()

	tokenURL, tokenCalls := fakeTokenEndpoint(t)
	// Thirty seconds of life against a one-minute margin: inside the window, so
	// the credential is due the moment its loop starts, and still valid — which
	// is the point. A refresh that only happens after a token dies is the 401
	// fallback, not the mechanism.
	store := writeTokenStore(t, staleAccess, staleRefresh, time.Now().Add(30*time.Second))
	a := newWiringApp(t, oauthYAMLFor(up.URL, store, tokenURL), ownSpool(t),
		func(o *Options) { o.Upstream = up.Client() })
	secret := issueKey(t, a, nil)

	c, ok := a.OAuth.Credential("oauth-1")
	if !ok {
		t.Fatal("the credential is not registered")
	}
	waitFor(t, "the background loop to renew the token", func() bool { return c.Refreshes() >= 1 })
	if n := tokenCalls(); n != 1 {
		t.Fatalf("the token endpoint was called %d times, want exactly 1", n)
	}

	if w := callWith(a, secret, http.MethodPost, "/v1/chat/completions", chatReq); w.Code != http.StatusOK {
		t.Fatalf("the request after a refresh answered %d: %s", w.Code, w.Body.String())
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "Bearer "+freshAccess {
		t.Fatalf("the upstream received %q, want the refreshed token: a renewal that does "+
			"not reach the wire is a renewal of nothing", got)
	}

	// The successor is in the vendor's store, and the key dorang does not own is
	// still there (§11.2b: the store is shared with the vendor's own CLI).
	raw, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("the rewritten store is not JSON: %v", err)
	}
	if back["access_token"] != freshAccess || back["refresh_token"] != freshRefresh {
		t.Errorf("the store does not hold the successor: %v", back["access_token"])
	}
	if back["vendor_note"] != "written by a fixture" {
		t.Errorf("the write-back dropped a key dorang does not own")
	}
	if !c.Healthy() {
		t.Errorf("the credential is unhealthy after a successful refresh: %v", c.Health())
	}
}

// TestAnOAuthTokenReachesNoArtifact is DESIGN §4.1 for the token that did not
// exist when the credential was declared.
//
// The token asserted about is the REFRESHED one, applied by internal/auth's
// refresher long after internal/backend built its scrubber list — which is why
// collectSecrets reads the outbound HEADERS rather than the credential table. A
// test that planted only the stored token would pass with that read broken.
func TestAnOAuthTokenReachesNoArtifact(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The first request succeeds, so the ledger ends up with a settled row
		// that names the credential — an artifact with something in it, rather
		// than an empty table a sweep passes over.
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, chatOK)
			return
		}
		// The second is refused with the credential echoed back. Several
		// OpenAI-compatible servers do exactly this, and it is the channel
		// DESIGN §10.6 rule 4 exists for.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"Invalid credentials: %s","type":"invalid_request_error"}}`,
			r.Header.Get("Authorization"))
	}))
	defer up.Close()

	tokenURL, _ := fakeTokenEndpoint(t)
	store := writeTokenStore(t, staleAccess, staleRefresh, time.Now().Add(30*time.Second))

	var logMu sync.Mutex
	var logs strings.Builder
	a := newWiringApp(t, oauthYAMLFor(up.URL, store, tokenURL), ownSpool(t), func(o *Options) {
		o.Upstream = up.Client()
		o.Logf = func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			fmt.Fprintf(&logs, format+"\n", args...)
		}
	})
	secret := issueKey(t, a, nil)

	c, _ := a.OAuth.Credential("oauth-1")
	waitFor(t, "the token to be renewed", func() bool { return c.Refreshes() >= 1 })

	if ok := callWith(a, secret, http.MethodPost, "/v1/chat/completions", chatReq); ok.Code != http.StatusOK {
		t.Fatalf("the first request answered %d: %s", ok.Code, ok.Body.String())
	}
	w := callWith(a, secret, http.MethodPost, "/v1/chat/completions", chatReq)
	if w.Code == http.StatusOK {
		t.Fatalf("the upstream 401 was served as a success")
	}

	// Everything dorang recorded or answered with.
	logMu.Lock()
	logText := logs.String()
	logMu.Unlock()

	if err := a.Meter.Flush(context.Background()); err != nil {
		t.Logf("meter flush: %v", err)
	}
	// A trace reaches the ledger through the spool on the meter's own schedule,
	// not on Flush. The sweep waits for it rather than reading an empty database
	// and calling it clean.
	waitFor(t, "both requests' traces to reach the ledger", func() bool {
		return a.Meter.Stats().TracesFlushed >= 2
	})
	dbBytes := readAll(t, a.Config().Storage.SQLite.Path)
	spoolBytes := readDir(t, a.Config().Metering.Spool.Dir)
	ledgerRows := readLedger(t, a, "key-"+secret)

	snapshot := fmt.Sprintf("%v", a.OAuth.Snapshot())
	manager := fmt.Sprintf("%v %+v %#v %s", a.OAuth, a.OAuth, a.OAuth, a.OAuth)
	credential := fmt.Sprintf("%v %+v %#v %s", c, c, c, c)
	health := fmt.Sprintf("%v %+v", c.Health(), c.Health())
	metricsPage := callWith(a, testMasterKey, http.MethodGet, "/metrics", "").Body.String()

	for _, artifact := range []struct{ name, body string }{
		{"the client-visible error body", w.Body.String()},
		{"the client-visible response headers", fmt.Sprint(w.Header())},
		{"the operator log", logText},
		{"the OAuth snapshot", snapshot},
		{"a formatted OAuthManager", manager},
		{"a formatted OAuthCredential", credential},
		{"a formatted CredentialHealth", health},
		{"the metrics page", metricsPage},
		{"the ledger database", dbBytes},
		{"the trace spool", spoolBytes},
		{"the ledger rows themselves", ledgerRows},
	} {
		for _, tok := range []string{freshAccess, freshRefresh, staleAccess, staleRefresh} {
			if redact.Contains(artifact.body, tok) {
				t.Errorf("%s carries the token %q", artifact.name, tok)
			}
		}
	}

	// The health record still says WHY, which is the point of scrubbing rather
	// than suppressing: an operator needs the reason.
	if h := c.Health(); h.Healthy && h.Reason == "" {
		// A 401 does not itself mark the credential unhealthy — a failed
		// REFRESH does — so this is a statement about the shape of the record,
		// not about this request.
		if len(a.OAuth.Snapshot()) != 1 {
			t.Errorf("the snapshot does not report the credential at all")
		}
	}
	// The metric family has to be published, or grepping the page for a token
	// asserts nothing: an absent family carries no labels at all.
	if !strings.Contains(metricsPage, "dorang_oauth") {
		t.Errorf("the metrics page publishes no oauth family, so the label sweep above is " +
			"vacuous: a deployment with OAuth credentials has no way to see a refresh loop " +
			"that has been failing for three days")
	}
	if !strings.Contains(metricsPage, "oauth-1") {
		t.Errorf("the metrics page does not carry the credential's opaque id, which is what " +
			"an alert has to be keyed on")
	}
	// The ledger has to hold a row naming this credential, or grepping the
	// database for a token is grepping an empty table.
	//
	// What that row does NOT contain is the upstream's own error text:
	// internal/server puts it on Result.NativeErrorMessage and nothing in this
	// package reads it, so today no upstream message reaches a ledger row at
	// all. The live channel for a relayed upstream body is internal/backend's
	// ErrorBody, which the batch path writes into a file the client downloads,
	// and the refreshed token is asserted absent from THAT where it lives — see
	// TestARefreshedOAuthTokenIsScrubbedFromARelayedErrorBody in internal/backend.
	// Saying so beats an assertion that passes because the artifact is empty.
	if !strings.Contains(ledgerRows, "oauth-1") {
		t.Errorf("no ledger row names the credential, so the ledger sweep above is vacuous:\n%s",
			ledgerRows)
	}
	if !strings.Contains(snapshot, "oauth-1") {
		t.Errorf("the snapshot does not carry the credential's opaque id, which is the one "+
			"thing that is supposed to leave the subsystem: %s", snapshot)
	}
}

// readLedger renders every ledger row of the last hour, read back through the
// store's own query rather than by grepping the database file: a column dorang
// wrote is only an artifact if something can read it out again.
func readLedger(t *testing.T, a *App, keyID string) string {
	t.Helper()
	now := a.now()
	page, err := a.Store.ListRequestsByKey(context.Background(), keyID,
		store.TimeRange{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}, store.Page{Limit: 50})
	if err != nil {
		t.Fatalf("ListRequestsByKey: %v", err)
	}
	var b strings.Builder
	for _, row := range page.Rows {
		fmt.Fprintf(&b, "%+v\n", row)
	}
	if b.Len() == 0 {
		t.Fatal("the ledger holds no row for a request that completed, so the sweep over " +
			"it would pass vacuously")
	}
	return b.String()
}

// readDir returns every file in a directory concatenated, so a sweep can grep a
// spool without knowing its segment naming.
func readDir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("the trace spool at %s cannot be read: %v", dir, err)
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		b.Write(data)
	}
	// An empty spool is not a failure here: the meter drains it into the ledger,
	// which is the artifact the assertion below actually requires to be
	// populated. This one is swept because a trace that has not been drained yet
	// is still a record dorang wrote to disk.
	return b.String()
}

// readAll returns a file's bytes as a string, and every sibling SQLite writes
// beside it — a row that is only in the write-ahead log is still a row dorang
// persisted.
func readAll(t *testing.T, path string) string {
	t.Helper()
	var b strings.Builder
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			continue
		}
		b.Write(data)
	}
	if b.Len() == 0 {
		t.Fatalf("no ledger database was found at %s: this assertion would pass vacuously", path)
	}
	return b.String()
}

// TestAffinitySurvivesARefresh is DESIGN §7.4a2 through §11.2b: a conversation
// pinned to an account stays pinned across a token refresh, because the account
// is the same account and the token is an implementation detail of talking to it.
//
// Two credentials, so that "the same one" is a claim with content.
func TestAffinitySurvivesARefresh(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatOK)
	}))
	defer up.Close()

	// Two token endpoints so that the minted token identifies which credential
	// refreshed, and two stores so that neither write can touch the other.
	storeA := writeTokenStore(t, "oauth-a-access-token-fixture", "oauth-a-refresh-token-fixture", time.Now().Add(time.Hour))
	storeB := writeTokenStore(t, "oauth-b-access-token-fixture", "oauth-b-refresh-token-fixture", time.Now().Add(time.Hour))
	mint := func(prefix string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":3600}`,
				prefix+"-renewed-access-token", prefix+"-renewed-refresh-token")
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}

	// round_robin, so that the credential the router would choose on its OWN
	// advances between requests. Without it both requests land on the first
	// credential whatever the pin says, and "the pin survived" is unobservable —
	// a test that passes with the pin's credential thrown away entirely.
	yaml := fmt.Sprintf(`
version: 1
observability: {always_full_headers: true}
key_rotation: {strategy: round_robin}
routing:
  sticky: {enabled: true, ttl: 10m}
providers:
  - {name: p1, kind: openai, base_url: %q}
credentials:
  - id: oauth-a
    provider: p1
    auth: oauth
    oauth:
      source: file
      path: %q
      refresh_margin: 1m
      refresh: {token_url: %q, client_id: fixture-client-id}
  - id: oauth-b
    provider: p1
    auth: oauth
    oauth:
      source: file
      path: %q
      refresh_margin: 1m
      refresh: {token_url: %q, client_id: fixture-client-id}
models:
  - name: m1
    strategy: [sticky, least_busy]
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [oauth-a, oauth-b]}
`, up.URL, storeA, mint("oauth-a"), storeB, mint("oauth-b"))

	a := newWiringApp(t, yaml, ownSpool(t), func(o *Options) { o.Upstream = up.Client() })
	secret := issueKey(t, a, nil)

	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatReq))
		r.Header.Set("Authorization", "Bearer "+secret)
		r.Header.Set(HeaderSession, "sess-affinity-1")
		w := httptest.NewRecorder()
		a.Server.ServeHTTP(w, r)
		return w
	}

	first := call()
	if first.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", first.Code, first.Body.String())
	}
	pinned := first.Header().Get("X-Dorang-Credential")

	// An unpinned session on the same gateway moves, which is what makes the
	// pinned one's stability a claim about the pin rather than about the router
	// happening to be deterministic.
	unpinned := func() string {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatReq))
		r.Header.Set("Authorization", "Bearer "+secret)
		w := httptest.NewRecorder()
		a.Server.ServeHTTP(w, r)
		return w.Header().Get("X-Dorang-Credential")
	}
	if a1, a2 := unpinned(), unpinned(); a1 == a2 {
		t.Fatalf("two sessionless requests both chose credential %q, so the rotation is not "+
			"moving and this test cannot tell a pin from a coincidence", a1)
	}
	if pinned == "" {
		t.Fatalf("the response does not report which credential served, so this test "+
			"cannot observe a pin at all\n%v", first.Header())
	}

	// Renew the pinned credential's token. Driven directly rather than waited
	// for, because what is under test is the pin's response to a new token, not
	// the schedule that produces one.
	c, ok := a.OAuth.Credential(pinned)
	if !ok {
		t.Fatalf("the pinned credential %q is not in the manager", pinned)
	}
	before := c.Refreshes()
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if c.Refreshes() != before+1 {
		t.Fatalf("no refresh happened")
	}

	second := call()
	if second.Code != http.StatusOK {
		t.Fatalf("the request after a refresh: %d %s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("X-Dorang-Credential"); got != pinned {
		t.Fatalf("the session moved from credential %q to %q across a token refresh: "+
			"the pin is keyed on the token rather than on the account (§7.4a2)", pinned, got)
	}

	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("the upstream saw nothing")
	}
	last := got[len(got)-1]
	if last == got[0] {
		t.Fatalf("the request after the refresh carried the same token as the first, so "+
			"nothing was actually renewed and this test proves nothing: %q", last)
	}
	if want := "Bearer " + pinned + "-renewed-access-token"; last != want {
		t.Fatalf("after the refresh the upstream received %q, want %q", last, want)
	}
}

// TestTheOAuthCredentialSetIsNotHotReloaded. §4.1 says every section reloads;
// this one holds a token, a backoff and a background loop established at
// start-up, and rebuilding them on SIGHUP would discard all three. The refusal
// is explicit rather than a silent no-op.
func TestTheOAuthCredentialSetIsNotHotReloaded(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatOK)
	}))
	defer up.Close()

	store := writeTokenStore(t, staleAccess, staleRefresh, time.Now().Add(time.Hour))
	yaml := oauthYAMLFor(up.URL, store, "")
	a := newWiringApp(t, yaml, ownSpool(t), func(o *Options) { o.Upstream = up.Client() })

	// An unrelated edit still reloads.
	same, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	same.Storage.SQLite.Path = a.Config().Storage.SQLite.Path
	if err := a.Reload(same); err != nil {
		t.Fatalf("a reload that does not touch the oauth set was refused: %v", err)
	}

	// Moving the store is a different credential in every way that matters.
	moved, err := config.LoadBytes([]byte(oauthYAMLFor(up.URL,
		writeTokenStore(t, staleAccess, staleRefresh, time.Now().Add(time.Hour)), "")))
	if err != nil {
		t.Fatal(err)
	}
	moved.Storage.SQLite.Path = a.Config().Storage.SQLite.Path
	err = a.Reload(moved)
	if err == nil {
		t.Fatal("a reload that changes the oauth credential set was applied")
	}
	if !strings.Contains(err.Error(), "Restart") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
}

// TestConfigAndAuthAgreeOnOAuthSpellings. internal/config must not import a
// sibling (DESIGN §1), so it repeats the source and format names that
// internal/auth parses. Two lists that can drift are one list that is wrong, and
// this is the place both packages are already imported.
func TestConfigAndAuthAgreeOnOAuthSpellings(t *testing.T) {
	for _, format := range auth.StoreFormats() {
		yaml := fmt.Sprintf(`
version: 1
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, auth: oauth, oauth: {source: file, path: /tmp/a.json, format: %s}}
`, format)
		if _, err := config.LoadBytes([]byte(yaml)); err != nil {
			t.Errorf("internal/auth parses format %q and internal/config refuses it: %v",
				format, err)
		}
	}
	for _, source := range []string{"file", "exec", "env"} {
		if _, err := auth.ParseTokenSource(source); err != nil {
			t.Errorf("internal/config accepts source %q and internal/auth refuses it: %v",
				source, err)
		}
	}
	for _, enc := range []string{"form", "json"} {
		if _, err := auth.ParseRefreshEncoding(enc); err != nil {
			t.Errorf("internal/config accepts encoding %q and internal/auth refuses it: %v",
				enc, err)
		}
	}
}
