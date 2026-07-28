package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// The token values every leak assertion in this file hunts for.
	storedAccess  = "at-stored-6f1c9a2b4d8e" // pragma: allowlist secret — test fixture
	storedRefresh = "rt-stored-3e7b5c1a9f04" // pragma: allowlist secret — test fixture
)

// fakeClock is a settable clock: refresh scheduling is entirely about time, and
// a test that slept would be both slow and flaky.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// writeStore writes a vendor-CLI-shaped token store, including a key dorang
// knows nothing about, which every write must preserve.
func writeStore(t *testing.T, path string, access, refresh string, expires time.Time, mode os.FileMode) {
	t.Helper()
	m := map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"account_id":    "acct-42",
		"scopes":        []string{"user:inference"},
		"vendor_state":  map[string]any{"last_login": "2026-07-01T00:00:00Z"},
	}
	if !expires.IsZero() {
		m["expires_at"] = expires.UTC().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func readStore(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("store is not valid JSON (%v): %q", err, b)
	}
	return m
}

// oauthFixture is a file-sourced credential with a fake refresher and a
// settable clock.
type oauthFixture struct {
	cred  *OAuthCredential
	rf    *FakeRefresher
	clock *fakeClock
	path  string
}

func newFixture(t *testing.T, tweak func(*OAuthConfig)) *oauthFixture {
	t.Helper()
	clock := newClock(testNow)
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	writeStore(t, path, storedAccess, storedRefresh, testNow.Add(time.Hour), 0o600)

	rf := &FakeRefresher{TTL: time.Hour, Now: clock.Now}
	cfg := OAuthConfig{
		ID:            "plan-oauth-1",
		Provider:      "some-plan",
		Source:        SourceFile,
		Path:          path,
		RefreshMargin: 5 * time.Minute,
		AccountHeader: "X-Account-Id",
		Now:           clock.Now,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := NewOAuthCredential(cfg, rf)
	if err != nil {
		t.Fatalf("NewOAuthCredential: %v", err)
	}
	if err := c.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return &oauthFixture{cred: c, rf: rf, clock: clock, path: path}
}

// waitFor polls a condition rather than sleeping for a fixed time, so the tests
// are neither slow nor timing-dependent.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRefreshFiresBeforeExpiryNotOnFailure is the mechanism (DESIGN §11.2b).
func TestRefreshFiresBeforeExpiryNotOnFailure(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	tok, err := f.cred.AccessToken()
	if err != nil || tok != storedAccess {
		t.Fatalf("AccessToken = %q, %v", tok, err)
	}

	// Well outside the margin: nothing happens.
	if err := f.cred.RefreshIfDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.rf.Calls(); n != 0 {
		t.Fatalf("%d refreshes outside the margin, want 0", n)
	}

	// One second inside the margin, with the token still perfectly valid.
	f.clock.Advance(55*time.Minute + time.Second)
	if !f.cred.Due(f.clock.Now()) {
		t.Fatal("the credential is inside its margin and does not want a refresh")
	}
	if err := f.cred.RefreshIfDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.rf.Calls(); n != 1 {
		t.Fatalf("%d refreshes inside the margin, want 1", n)
	}

	// The old token had not expired when it was replaced: the refresh did not
	// wait for a failure.
	if !testNow.Add(time.Hour).After(f.clock.Now()) {
		t.Fatal("the test refreshed after expiry, which is not what it claims to check")
	}
	tok, err = f.cred.AccessToken()
	if err != nil || tok == storedAccess {
		t.Fatalf("AccessToken = %q, %v; want the renewed token", tok, err)
	}
	if h := f.cred.Health(); !h.Healthy || h.Refreshes != 1 {
		t.Fatalf("health = %v", h)
	}
}

// TestRefreshIsNotOnTheRequestPath: a refresh in flight must be invisible to
// requests using the token it is replacing. The reader path takes no lock the
// refresher's I/O can hold.
func TestRefreshIsNotOnTheRequestPath(t *testing.T) {
	f := newFixture(t, nil)
	block := make(chan struct{})
	f.rf.Block = block
	f.clock.Advance(56 * time.Minute) // inside the margin

	refreshDone := make(chan error, 1)
	go func() { refreshDone <- f.cred.Refresh(context.Background()) }()

	// Wait until the refresh is actually inside the refresher, observed through
	// the refresher itself rather than through the single-flight: the claim is
	// about the reader path, not about how the writer got there.
	waitFor(t, "the refresh to start", func() bool { return f.rf.Calls() > 0 })

	const reads = 20000
	start := time.Now()
	for i := range reads {
		tok, err := f.cred.AccessToken()
		if err != nil {
			t.Fatalf("read %d during a refresh: %v", i, err)
		}
		if tok != storedAccess {
			t.Fatalf("read %d saw %q; the old token must serve until the new one lands", i, tok)
		}
	}
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("%d reads took %v while a refresh was in flight; the read path is blocking",
			reads, elapsed)
	}
	// Header application is on the same path.
	h := http.Header{}
	if err := f.cred.Apply(h); err != nil {
		t.Fatal(err)
	}
	if got := h.Get(HeaderAuthorization); got != "Bearer "+storedAccess {
		t.Fatalf("Authorization = %q", got)
	}
	if got := h.Get("X-Account-Id"); got != "acct-42" {
		t.Fatalf("account header = %q", got)
	}

	close(block)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	if tok, _ := f.cred.AccessToken(); tok == storedAccess {
		t.Fatal("the refresh did not land")
	}
	t.Logf("%d reads in %v with a refresh in flight", reads, elapsed)
}

// TestConcurrentRequestsCauseExactlyOneRefresh is the constraint that matters
// most: some providers invalidate the previous refresh token on use, so a
// stampede does not waste calls, it locks the account out.
func TestConcurrentRequestsCauseExactlyOneRefresh(t *testing.T) {
	f := newFixture(t, nil)
	f.clock.Advance(56 * time.Minute)
	block := make(chan struct{})
	f.rf.Block = block

	const n = 64
	var wg sync.WaitGroup
	tokens := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tokens[i], errs[i] = f.cred.RefreshOn401(context.Background(), storedAccess)
		}()
	}
	close(start)

	// Let them pile up behind the leader, then release it. Waiting on the
	// refresher's own counter rather than on the single-flight is deliberate:
	// an implementation that dropped the coalescing would arrive here too, and
	// then fail the count below instead of timing out here.
	waitFor(t, "a refresh to start", func() bool { return f.rf.Calls() > 0 })
	time.Sleep(50 * time.Millisecond)
	close(block)
	wg.Wait()

	if got := f.rf.Calls(); got != 1 {
		t.Fatalf("%d concurrent 401s caused %d refreshes, want exactly 1", n, got)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if tokens[i] != tokens[0] || tokens[i] == storedAccess {
			t.Fatalf("caller %d got %q, caller 0 got %q", i, tokens[i], tokens[0])
		}
	}

	// A second wave, already holding the fresh token, exchanges nothing: the
	// 401 fallback is idempotent against a token that has already moved on.
	for range n {
		if _, err := f.cred.RefreshOn401(context.Background(), tokens[0]); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.rf.Calls(); got != 2 {
		t.Fatalf("a second wave on the current token caused %d refreshes in total, want 2", got)
	}
}

// TestRefreshedTokenIsWrittenBackAtomically. The store is shared with the
// vendor's own CLI: every key dorang does not own survives, and so does the
// file mode.
func TestRefreshedTokenIsWrittenBackAtomically(t *testing.T) {
	f := newFixture(t, nil)
	if err := os.Chmod(f.path, 0o640); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(56 * time.Minute)
	if err := f.cred.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	m := readStore(t, f.path)
	tok, _ := f.cred.AccessToken()
	if m["access_token"] != tok {
		t.Fatalf("store holds %v, credential holds %v", m["access_token"], tok)
	}
	if m["account_id"] != "acct-42" {
		t.Fatalf("account_id was lost: %v", m["account_id"])
	}
	if _, ok := m["scopes"]; !ok {
		t.Fatal("the vendor's scopes key was dropped")
	}
	if _, ok := m["vendor_state"]; !ok {
		t.Fatal("the vendor's own state was dropped")
	}
	if _, err := time.Parse(time.RFC3339, m["expires_at"].(string)); err != nil {
		t.Fatalf("expires_at was rewritten in another format: %v", m["expires_at"])
	}
	fi, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Fatalf("file mode = %v, want the original 0640", got)
	}
	// No temporary files left behind.
	assertNoTempFiles(t, filepath.Dir(f.path))
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("a temporary file was left behind: %s", e.Name())
		}
	}
}

// TestAtomicWriteLeavesTheOldFileOrTheNewOne fails the write halfway through.
// The vendor's CLI reads this file too, and a half-written credential store
// breaks it — with nothing pointing at the gateway as the cause.
func TestAtomicWriteLeavesTheOldFileOrTheNewOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	writeStore(t, path, storedAccess, storedRefresh, testNow.Add(time.Hour), 0o600)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	failure := errors.New("disk went away")
	newContent := []byte(`{"access_token":"at-brand-new","refresh_token":"rt-brand-new"}`)
	err = atomicWrite(path, 0o600, func(w io.Writer) error {
		// Write half of it, then fail — the shape of a truncated write.
		if _, err := w.Write(newContent[:len(newContent)/2]); err != nil {
			return err
		}
		return failure
	})
	if err == nil {
		t.Fatal("a failing write reported success")
	}
	if strings.Contains(err.Error(), "at-brand-new") {
		t.Fatalf("the error carries token material: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("the original file was modified by a failed write:\n%s", after)
	}
	var m map[string]any
	if err := json.Unmarshal(after, &m); err != nil {
		t.Fatalf("the file is no longer parseable: %v", err)
	}
	assertNoTempFiles(t, dir)

	// And the successful write replaces it wholly.
	if err := atomicWrite(path, 0o600, func(w io.Writer) error {
		_, err := w.Write(newContent)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(path)
	if string(after) != string(newContent) {
		t.Fatalf("the successful write left %q", after)
	}
	assertNoTempFiles(t, dir)
}

// TestFailedRefreshMarksUnhealthyAndBacksOff. A refresh failure steps the
// credential aside exactly as an exhausted quota does; what it must never do is
// retry in a tight loop against an auth server.
func TestFailedRefreshMarksUnhealthyAndBacksOff(t *testing.T) {
	f := newFixture(t, func(c *OAuthConfig) {
		c.BackoffBase = time.Second
		c.BackoffCeiling = 8 * time.Second
	})
	f.rf.SetError(errors.New("the authorization server said no"))
	f.clock.Advance(56 * time.Minute)
	ctx := context.Background()

	if err := f.cred.RefreshIfDue(ctx); err == nil {
		t.Fatal("a failing refresh reported success")
	}
	h := f.cred.Health()
	if h.Healthy || h.Reason != OAuthRefreshFailed || h.Failures != 1 {
		t.Fatalf("health after one failure = %v", h)
	}
	if !h.NextAttempt.Equal(f.clock.Now().Add(time.Second)) {
		t.Fatalf("next attempt = %v, want +1s", h.NextAttempt)
	}

	// The tight loop. Every one of these is refused locally.
	for range 1000 {
		_ = f.cred.RefreshIfDue(ctx)
	}
	if n := f.rf.Calls(); n != 1 {
		t.Fatalf("1000 attempts inside the backoff reached the auth server %d times, want 1", n)
	}

	// The wait doubles to a ceiling, and never past it.
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		if i > 0 {
			f.clock.Advance(w)
			if err := f.cred.RefreshIfDue(ctx); err == nil {
				t.Fatalf("attempt %d succeeded unexpectedly", i)
			}
		}
		h := f.cred.Health()
		if h.Failures != i+1 {
			t.Fatalf("attempt %d: failures = %d", i, h.Failures)
		}
		if got := h.NextAttempt.Sub(f.clock.Now()); got != w {
			t.Fatalf("attempt %d: backoff = %v, want %v", i, got, w)
		}
	}
	if n := f.rf.Calls(); n != len(want) {
		t.Fatalf("auth server was called %d times over %d backoff windows", n, len(want))
	}

	// A 401 cannot walk around the backoff either.
	callsBefore := f.rf.Calls()
	for range 100 {
		if _, err := f.cred.RefreshOn401(ctx, storedAccess); !errors.Is(err, ErrBackingOff) {
			t.Fatalf("the 401 path bypassed the backoff: %v", err)
		}
	}
	if n := f.rf.Calls(); n != callsBefore {
		t.Fatalf("the 401 path made %d extra calls during a backoff", n-callsBefore)
	}

	// The fleet did not fail: the token that could not be replaced still serves
	// until it actually expires.
	if tok, err := f.cred.AccessToken(); err != nil || tok != storedAccess {
		t.Fatalf("AccessToken during an outage = %q, %v", tok, err)
	}

	// And recovery clears the failure.
	f.rf.SetError(nil)
	f.clock.Advance(8 * time.Second)
	if err := f.cred.RefreshIfDue(ctx); err != nil {
		t.Fatal(err)
	}
	if h := f.cred.Health(); !h.Healthy || h.Failures != 0 || h.Reason != "" {
		t.Fatalf("health after recovery = %v", h)
	}
}

// leakyError is a Refresher error that carries the credential's own tokens,
// which is what a careless provider implementation produces.
type leakyError struct{ msg string }

func (e *leakyError) Error() string { return e.msg }

// TestNoTokenEverLeaves sweeps every path that produces text: errors, health,
// snapshots, and every fmt verb over every type that holds a token.
func TestNoTokenEverLeaves(t *testing.T) {
	f := newFixture(t, nil)
	f.clock.Advance(56 * time.Minute)
	ctx := context.Background()

	secrets := []string{storedAccess, storedRefresh}
	check := func(what, s string) {
		t.Helper()
		for _, secret := range secrets {
			if strings.Contains(s, secret) {
				t.Fatalf("%s leaked a token: %q", what, s)
			}
		}
	}

	// Every verb over every type that holds key material.
	for _, v := range []any{
		f.cred, f.cred.current(), f.cred.Health(), f.cred.cfg,
	} {
		for _, verb := range []string{"%v", "%s", "%+v", "%#v", "%q", "%x", "%d"} {
			check(fmt.Sprintf("%T under %s", v, verb), fmt.Sprintf(verb, v))
		}
	}

	// A refresher that puts the credential's tokens into its error.
	f.rf.SetError(&leakyError{msg: "refused for token " + storedAccess +
		" (refresh " + storedRefresh + ")"})
	err := f.cred.RefreshIfDue(ctx)
	if err == nil {
		t.Fatal("expected a failure")
	}
	check("the refresh error", err.Error())
	check("the wrapped refresh error", fmt.Sprintf("%v", err))

	h := f.cred.Health()
	check("health.Detail", h.Detail)
	check("health under %v", fmt.Sprintf("%v", h))
	check("health under %+v", fmt.Sprintf("%+v", h))
	if !strings.Contains(h.Detail, "(redacted)") {
		t.Fatalf("the leaked token was removed but left no trace: %q", h.Detail)
	}

	// The manager's snapshot is the administrative view.
	m := NewOAuthManager()
	if err := m.Add(f.cred); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %v", snap)
	}
	check("the snapshot", fmt.Sprintf("%+v", snap))
	check("the manager", fmt.Sprintf("%#v", m))

	// Only the id and the health leave.
	if snap[0].ID != "plan-oauth-1" || snap[0].Healthy {
		t.Fatalf("snapshot[0] = %v", snap[0])
	}

	// A store that cannot be parsed reports the field, never its value.
	_, derr := decodeToken([]byte(`{"access_token": 12345, "refresh_token": "`+storedRefresh+`"}`), TokenFields{}.withDefaults())
	if derr == nil {
		t.Fatal("a malformed store parsed")
	}
	check("the decode error", derr.Error())

	_, derr = decodeToken([]byte(`{"expires_at": "not-a-date", "access_token": "`+storedAccess+`"}`), TokenFields{}.withDefaults())
	if derr == nil {
		t.Fatal("a malformed expiry parsed")
	}
	check("the expiry error", derr.Error())
}

// TestExecAndEnvErrorsCarryNoOutput. A helper that prints a token to stdout and
// then fails would put it in an error if the error quoted its output; a badly
// behaved one prints it to stderr as well.
func TestExecAndEnvErrorsCarryNoOutput(t *testing.T) {
	s := &execStore{
		command: []string{"sh", "-c", "echo " + storedAccess + "; echo " + storedRefresh + " >&2; exit 3"},
		fields:  TokenFields{}.withDefaults(),
		timeout: 5 * time.Second,
	}
	_, err := s.Load()
	if err == nil {
		t.Fatal("a failing command reported success")
	}
	if strings.Contains(err.Error(), storedAccess) || strings.Contains(err.Error(), storedRefresh) {
		t.Fatalf("the exec error carries command output: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("the exec error does not say why: %v", err)
	}

	// The success path still works, both shapes.
	s.command = []string{"sh", "-c", "echo " + storedAccess}
	tok, err := s.Load()
	if err != nil || tok.Access != storedAccess {
		t.Fatalf("bare-token command: %v, %v", tok, err)
	}
	if !tok.ExpiresAt.IsZero() {
		t.Fatal("a bare token cannot have an expiry")
	}
	// A bare token has no expiry, so it is never refreshed ahead of time: the
	// alternative is a refresh loop against an unknown deadline.
	if tok.NeedsRefresh(testNow, time.Hour) {
		t.Fatal("a token with no expiry was scheduled for refresh")
	}
	if s.Writable() {
		t.Fatal("an exec source is not writable")
	}
	if err := s.Save(tok); !errors.Is(err, ErrTokenStoreReadOnly) {
		t.Fatalf("exec Save = %v", err)
	}

	// env, both shapes.
	t.Setenv("DORANG_TEST_OAUTH", storedAccess)
	e := &envStore{name: "DORANG_TEST_OAUTH", fields: TokenFields{}.withDefaults()}
	tok, err = e.Load()
	if err != nil || tok.Access != storedAccess {
		t.Fatalf("env store: %v, %v", tok, err)
	}
	t.Setenv("DORANG_TEST_OAUTH", `{"access_token":"`+storedAccess+`","expires_at":1800000000}`)
	tok, err = e.Load()
	if err != nil || tok.Access != storedAccess || tok.ExpiresAt.Unix() != 1800000000 {
		t.Fatalf("env store JSON: %v, %v", tok, err)
	}
	e2 := &envStore{name: "DORANG_TEST_OAUTH_MISSING", fields: TokenFields{}.withDefaults()}
	if _, err := e2.Load(); !errors.Is(err, ErrTokenStoreUnreadable) {
		t.Fatalf("a missing variable gave %v", err)
	}
}

// TestAffinityPinSurvivesARefresh. An OAuth credential is still a credential:
// a conversation pinned to an account stays pinned across a token refresh,
// because the account is the same account (DESIGN §7.4a2, §11.2b).
func TestAffinityPinSurvivesARefresh(t *testing.T) {
	f := newFixture(t, nil)
	other := newFixture(t, func(c *OAuthConfig) { c.ID = "plan-oauth-2" })

	m := NewOAuthManager()
	for _, c := range []*OAuthCredential{f.cred, other.cred} {
		if err := m.Add(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Add(f.cred); !errors.Is(err, ErrDuplicateCredential) {
		t.Fatalf("a duplicate id was accepted: %v", err)
	}

	// A conversation pins the account it started on.
	pinned := f.cred.ID()
	before, ok := m.Credential(pinned)
	if !ok {
		t.Fatal("the pin does not resolve")
	}
	beforeToken, _ := before.AccessToken()

	f.clock.Advance(56 * time.Minute)
	if err := f.cred.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	after, ok := m.Credential(pinned)
	if !ok {
		t.Fatal("the pin stopped resolving across a refresh")
	}
	if after != before {
		t.Fatal("the pin resolved to a different credential across a refresh")
	}
	if after.ID() != pinned {
		t.Fatalf("the credential id changed from %q to %q", pinned, after.ID())
	}
	afterToken, err := after.AccessToken()
	if err != nil {
		t.Fatal(err)
	}
	if afterToken == beforeToken {
		t.Fatal("the token did not change, so this proves nothing")
	}
	if after.AccountID() != "acct-42" {
		t.Fatalf("the account id changed to %q", after.AccountID())
	}
	if ids := m.IDs(); len(ids) != 2 || ids[0] != "plan-oauth-1" || ids[1] != "plan-oauth-2" {
		t.Fatalf("manager ids = %v", ids)
	}
	// The other account was untouched.
	if other.rf.Calls() != 0 {
		t.Fatal("refreshing one credential refreshed another")
	}
}

// TestBackgroundLoopRefreshesOffTheRequestPath runs the real loop.
func TestBackgroundLoopRefreshesOffTheRequestPath(t *testing.T) {
	f := newFixture(t, func(c *OAuthConfig) {
		c.RefreshMargin = 4 * time.Millisecond
		c.PollInterval = time.Millisecond
	})
	// The store's token expires almost immediately on the fake clock.
	writeStore(t, f.path, storedAccess, storedRefresh, f.clock.Now().Add(time.Millisecond), 0o600)

	m := NewOAuthManager()
	if err := m.Add(f.cred); err != nil {
		t.Fatal(err)
	}
	m.Start(context.Background())
	defer m.Close()

	waitFor(t, "the background loop to refresh", func() bool { return f.cred.Refreshes() > 0 })
	if tok, err := f.cred.AccessToken(); err != nil || tok == storedAccess {
		t.Fatalf("AccessToken = %q, %v", tok, err)
	}
	m.Close() // idempotent
	m.Close()
}

// TestVendorCLIRefreshIsAdoptedRatherThanRacedIsSingleFlightAcrossProcesses.
//
// Single-flight is per process, and this store is shared with the vendor's own
// CLI. Re-reading before exchanging turns a cross-process stampede — the
// account-lockout case §11.2b describes — into a file read.
func TestVendorCLIRefreshIsAdoptedRatherThanRaced(t *testing.T) {
	f := newFixture(t, nil)
	f.clock.Advance(56 * time.Minute)

	// The vendor's CLI refreshed the token behind dorang's back.
	writeStore(t, f.path, "at-from-the-cli", "rt-from-the-cli", f.clock.Now().Add(2*time.Hour), 0o600)

	loadsBefore := f.cred.StoreLoads()
	if err := f.cred.RefreshIfDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.rf.Calls(); n != 0 {
		t.Fatalf("%d refreshes; the CLI's token should have been adopted instead", n)
	}
	if got := f.cred.StoreLoads() - loadsBefore; got != 1 {
		t.Fatalf("%d tokens adopted from the store, want 1", got)
	}
	if tok, _ := f.cred.AccessToken(); tok != "at-from-the-cli" {
		t.Fatalf("AccessToken = %q, want the CLI's token", tok)
	}
	if !f.cred.Healthy() {
		t.Fatalf("adopting the CLI's token left the credential unhealthy: %v", f.cred.Health())
	}
}

// TestTokenStoreRoundTrip covers the three expiry spellings vendor stores use,
// and that a rewrite keeps the one it found.
func TestTokenStoreRoundTrip(t *testing.T) {
	fields := TokenFields{}.withDefaults()
	cases := []struct {
		name string
		json string
		want time.Time
	}{
		{"rfc3339", `{"access_token":"a","expires_at":"2026-07-28T13:00:00Z"}`,
			time.Date(2026, 7, 28, 13, 0, 0, 0, time.UTC)},
		{"unix seconds", `{"access_token":"a","expires_at":1785243600}`, time.Unix(1785243600, 0).UTC()},
		{"unix millis", `{"access_token":"a","expires_at":1785243600000}`, time.UnixMilli(1785243600000).UTC()},
		{"absent", `{"access_token":"a"}`, time.Time{}},
		{"empty string", `{"access_token":"a","expires_at":""}`, time.Time{}},
		{"zero", `{"access_token":"a","expires_at":0}`, time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tok, err := decodeToken([]byte(c.json), fields)
			if err != nil {
				t.Fatal(err)
			}
			if !tok.ExpiresAt.Equal(c.want) {
				t.Fatalf("ExpiresAt = %v, want %v", tok.ExpiresAt, c.want)
			}
			b, err := encodeToken(tok, fields)
			if err != nil {
				t.Fatal(err)
			}
			back, err := decodeToken(b, fields)
			if err != nil {
				t.Fatalf("re-decoding what we wrote: %v", err)
			}
			if !back.ExpiresAt.Equal(c.want) || back.Access != "a" {
				t.Fatalf("round trip lost the token: %v %v", back.Access, back.ExpiresAt)
			}
			if c.name == "unix millis" && !strings.Contains(string(b), "1785243600000") {
				t.Fatalf("the expiry was rewritten in another format: %s", b)
			}
		})
	}
}

// TestOAuthConfigValidation. A poll slower than the margin could step over the
// whole refresh window and find the token already expired — which is the
// request-path refresh this design exists to avoid.
func TestOAuthConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  OAuthConfig
		ok   bool
	}{
		{"no id", OAuthConfig{Source: SourceFile, Path: "/tmp/a.json"}, false},
		{"file without a path", OAuthConfig{ID: "a", Source: SourceFile}, false},
		{"exec without a command", OAuthConfig{ID: "a", Source: SourceExec}, false},
		{"env without a variable", OAuthConfig{ID: "a", Source: SourceEnv}, false},
		{"file", OAuthConfig{ID: "a", Source: SourceFile, Path: "/tmp/a.json"}, true},
		{"exec", OAuthConfig{ID: "a", Source: SourceExec, Command: []string{"true"}}, true},
		{"env", OAuthConfig{ID: "a", Source: SourceEnv, EnvVar: "X"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg
			err := cfg.Validate()
			if (err == nil) != c.ok {
				t.Fatalf("Validate = %v, want ok=%v", err, c.ok)
			}
			if err != nil {
				return
			}
			if cfg.RefreshMargin != DefaultRefreshMargin {
				t.Fatalf("margin = %v", cfg.RefreshMargin)
			}
			if cfg.PollInterval > cfg.RefreshMargin/4 {
				t.Fatalf("poll %v can step over a margin of %v", cfg.PollInterval, cfg.RefreshMargin)
			}
		})
	}

	slow := OAuthConfig{ID: "a", Source: SourceEnv, EnvVar: "X",
		RefreshMargin: time.Minute, PollInterval: time.Hour}
	if err := slow.Validate(); err != nil {
		t.Fatal(err)
	}
	if slow.PollInterval != 15*time.Second {
		t.Fatalf("a poll slower than the margin was not clamped: %v", slow.PollInterval)
	}

	for _, s := range []string{"file", "exec", "env", ""} {
		if _, err := ParseTokenSource(s); err != nil {
			t.Fatalf("ParseTokenSource(%q): %v", s, err)
		}
	}
	if _, err := ParseTokenSource("vault"); err == nil {
		t.Fatal("an unknown source parsed")
	}
	if SourceFile.String() != "file" || SourceExec.String() != "exec" || SourceEnv.String() != "env" {
		t.Fatal("source names do not round-trip")
	}
}

// TestCredentialWithoutARefresherStandsAside covers the degenerate
// configurations: no refresher, and a store with no refresh token.
func TestCredentialWithoutARefresherStandsAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	clock := newClock(testNow)
	writeStore(t, path, storedAccess, "", testNow.Add(time.Hour), 0o600)

	cfg := OAuthConfig{ID: "c", Source: SourceFile, Path: path, Now: clock.Now}
	c, err := NewOAuthCredential(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); err != nil {
		t.Fatal(err)
	}
	// It serves while the token is valid.
	if tok, err := c.AccessToken(); err != nil || tok != storedAccess {
		t.Fatalf("AccessToken = %q, %v", tok, err)
	}
	clock.Advance(56 * time.Minute)
	if err := c.RefreshIfDue(context.Background()); !errors.Is(err, ErrNoRefresher) {
		t.Fatalf("RefreshIfDue = %v, want ErrNoRefresher", err)
	}
	if h := c.Health(); h.Healthy || h.Reason != OAuthNoRefresher {
		t.Fatalf("health = %v", h)
	}
	// Past expiry it refuses rather than serving a dead token.
	clock.Advance(time.Hour)
	if _, err := c.AccessToken(); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("AccessToken past expiry = %v", err)
	}

	// No refresh token, with a refresher present.
	c2, err := NewOAuthCredential(OAuthConfig{ID: "d", Source: SourceFile, Path: path, Now: clock.Now},
		&FakeRefresher{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := c2.Refresh(context.Background()); !errors.Is(err, ErrNoRefreshToken) {
		t.Fatalf("Refresh with no refresh token = %v", err)
	}
	if h := c2.Health(); h.Healthy || h.Reason != OAuthNoRefreshToken {
		t.Fatalf("health = %v", h)
	}

	// A missing store is unreadable, not a crash.
	c3, err := NewOAuthCredential(OAuthConfig{ID: "e", Source: SourceFile,
		Path: filepath.Join(dir, "absent.json"), Now: clock.Now}, &FakeRefresher{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := c3.Reload(); !errors.Is(err, ErrTokenStoreUnreadable) {
		t.Fatalf("Reload of a missing store = %v", err)
	}
	if _, err := c3.AccessToken(); !errors.Is(err, ErrNoToken) {
		t.Fatalf("AccessToken with no store = %v", err)
	}
	if h := c3.Health(); h.Healthy || h.Reason != OAuthStoreUnreadable {
		t.Fatalf("health = %v", h)
	}
}

// TestStoreWriteFailureStandsTheCredentialAside. The exchange consumed a
// refresh token the vendor's CLI still holds, and the successor could not be
// recorded: the account's stored state no longer matches reality.
func TestStoreWriteFailureStandsTheCredentialAside(t *testing.T) {
	f := newFixture(t, nil)
	f.clock.Advance(56 * time.Minute)
	// Make the directory unwritable, so the temporary file cannot be created.
	dir := filepath.Dir(f.path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unwritable directory is still writable")
	}

	err := f.cred.Refresh(context.Background())
	if err == nil {
		t.Fatal("a failed store write reported success")
	}
	h := f.cred.Health()
	if h.Healthy || h.Reason != OAuthStoreWriteFailed {
		t.Fatalf("health = %v", h)
	}
	// The token itself is valid upstream and still serves.
	if tok, err := f.cred.AccessToken(); err != nil || tok == storedAccess {
		t.Fatalf("AccessToken = %q, %v; the exchanged token should still serve", tok, err)
	}
	if !strings.Contains(err.Error(), f.path) {
		t.Fatalf("the error does not name the store: %v", err)
	}
}

// TestSingleFlightSurvivesAPanickingRefresher: a follower must never be handed
// a zero token with no error, which would read as "here is your new token".
func TestSingleFlightSurvivesAPanickingRefresher(t *testing.T) {
	var f flight[string, Token]
	var wg sync.WaitGroup
	results := make([]error, 8)
	release := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { _ = recover() }()
		_, _, _ = f.do("cred", func() (Token, error) {
			close(release)
			time.Sleep(10 * time.Millisecond)
			panic("provider client blew up")
		})
	}()
	<-release
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err, _ := f.do("cred", func() (Token, error) {
				return Token{Access: "later"}, nil
			})
			if err == nil && tok.Empty() {
				results[i] = errors.New("a follower saw an empty token with no error")
			}
		}()
	}
	wg.Wait()
	for i, err := range results {
		if err != nil {
			t.Fatalf("follower %d: %v", i, err)
		}
	}
	if n := f.inFlight(); n != 0 {
		t.Fatalf("%d calls still in flight after a panic", n)
	}
}
