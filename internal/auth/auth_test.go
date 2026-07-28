package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testPepper = "test-pepper-not-a-real-one"               // pragma: allowlist secret — test fixture
	testMaster = "sk-master-0000000000000000000000000000"   // pragma: allowlist secret — test fixture
	testToken  = "sk-live-abcdefghijklmnopqrstuvwxyz012345" // pragma: allowlist secret — test fixture
)

var testNow = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

// fakeStore is the narrow Store this package needs, with counters and a gate
// so tests can hold a lookup open and observe coalescing.
type fakeStore struct {
	mu       sync.Mutex
	rows     map[string]Record
	calls    int
	err      error
	gate     chan struct{} // when non-nil, every lookup waits on it
	rehashed map[string]Digest
	rehashCh chan struct{} // when non-nil, Rehash blocks until it is closed
	rehashN  int
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]Record{}, rehashed: map[string]Digest{}}
}

func (s *fakeStore) put(rec Record) { s.rows[rec.Lookup] = rec }

func (s *fakeStore) LoadByLookup(ctx context.Context, lookup string) (Record, error) {
	s.mu.Lock()
	s.calls++
	gate, err := s.gate, s.err
	rec, ok := s.rows[lookup]
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return Record{}, ctx.Err()
		}
	}
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, ErrNotFound
	}
	return rec, nil
}

func (s *fakeStore) Rehash(ctx context.Context, keyID, lookup string, digest Digest) error {
	s.mu.Lock()
	block := s.rehashCh
	s.mu.Unlock()
	if block != nil {
		<-block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rehashed[keyID] = digest
	s.rehashN++
	if rec, ok := s.rows[lookup]; ok {
		rec.Digest = digest
		rec.Scheme = SchemeDorangV1
		s.rows[lookup] = rec
	}
	return nil
}

func (s *fakeStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fakeStore) rehashCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rehashN
}

func testHasher(t *testing.T, legacy LegacyPolicy) *Hasher {
	t.Helper()
	h, err := NewHasher(testPepper, legacy)
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	return h
}

// record builds a stored row for a token under a scheme.
func record(t *testing.T, h *Hasher, token string, scheme Scheme, p Principal) Record {
	t.Helper()
	d, err := h.Hash(scheme, token)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if p.KeyID == "" {
		p.KeyID = "key-1"
	}
	return Record{Lookup: LookupKey(token), Digest: d, Scheme: scheme, Principal: p}
}

// newAuth builds an Authenticator with a fixed clock and the given store.
func newAuth(t *testing.T, cfg Config) *Authenticator {
	t.Helper()
	if cfg.Pepper == "" {
		cfg.Pepper = testPepper
	}
	if cfg.MasterKey == "" && !cfg.NoMasterKey {
		cfg.MasterKey = testMaster
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return testNow }
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// ---------------------------------------------------------------- headers ---

func TestSixHeadersEachAuthenticate(t *testing.T) {
	h := testHasher(t, LegacyPolicy{})
	a := newAuth(t, Config{})
	if err := a.Load([]Record{record(t, h, testToken, SchemeDorangV1, Principal{})}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		header string
		value  string
	}{
		{HeaderAuthorization, "Bearer " + testToken},
		{HeaderAuthorization, "bearer " + testToken}, // RFC 7235: case-insensitive
		{HeaderAPIKey, testToken},
		{HeaderXAPIKey, testToken},
		{HeaderXGoogAPIKey, testToken},
		{HeaderAzureAPIKey, testToken},
		{HeaderDorangAPIKey, testToken},
	}
	for _, c := range cases {
		t.Run(c.header+"/"+strings.SplitN(c.value, " ", 2)[0], func(t *testing.T) {
			hdr := http.Header{}
			hdr.Set(c.header, c.value)
			got, name, ok := Extract(hdr)
			if !ok || got != testToken {
				t.Fatalf("Extract(%s) = %q, %q, %v; want the token", c.header, got, name, ok)
			}
			p, err := a.AuthenticateHeader(context.Background(), hdr)
			if err != nil {
				t.Fatalf("AuthenticateHeader(%s): %v", c.header, err)
			}
			if p.KeyID != "key-1" {
				t.Fatalf("KeyID = %q", p.KeyID)
			}
		})
	}
	if n := len(Headers()); n != 6 {
		t.Fatalf("Headers() has %d names, want the six of COMPATIBILITY 7.3", n)
	}
}

func TestExtractIgnoresOtherAuthorizationSchemes(t *testing.T) {
	hdr := http.Header{}
	hdr.Set(HeaderAuthorization, "Basic dXNlcjpwYXNz")
	if tok, _, ok := Extract(hdr); ok {
		t.Fatalf("Basic credentials were read as a token: %q", tok)
	}
	// A bare value with no scheme is accepted: clients pointed at a proxy base
	// URL commonly send one.
	hdr.Set(HeaderAuthorization, testToken)
	if tok, _, ok := Extract(hdr); !ok || tok != testToken {
		t.Fatalf("bare Authorization value not accepted: %q %v", tok, ok)
	}
}

func TestExtractFindsNonCanonicalHeaderKeys(t *testing.T) {
	hdr := http.Header{"x-dorang-api-key": []string{testToken}}
	tok, name, ok := Extract(hdr)
	if !ok || tok != testToken || name != HeaderDorangAPIKey {
		t.Fatalf("Extract = %q, %q, %v", tok, name, ok)
	}
}

func TestStripRemovesEveryAcceptedHeader(t *testing.T) {
	hdr := http.Header{}
	for _, name := range Headers() {
		hdr.Set(name, testToken)
	}
	// Non-canonical spellings, as a hand-built header map can hold.
	hdr["x-api-key"] = []string{testToken}
	hdr["OCP-APIM-SUBSCRIPTION-KEY"] = []string{testToken} // pragma: allowlist secret — test fixture
	hdr.Set("Content-Type", "application/json")
	hdr.Set("X-Dorang-Request-Id", "req-1")

	Strip(hdr)

	for k, v := range hdr {
		joined := strings.Join(v, ",")
		if strings.Contains(joined, testToken) {
			t.Fatalf("header %q still carries the credential after Strip", k)
		}
		if acceptedIndex(k) >= 0 {
			t.Fatalf("header %q survived Strip", k)
		}
	}
	if hdr.Get("Content-Type") != "application/json" {
		t.Fatal("Strip removed an unrelated header")
	}
	if _, _, ok := Extract(hdr); ok {
		t.Fatal("Extract still finds a credential after Strip")
	}
}

// ---------------------------------------------------------------- schemes ---

func TestOneLookupServesBothSchemes(t *testing.T) {
	legacy := LegacyPolicy{Enabled: true, Until: testNow.Add(24 * time.Hour)}
	h := testHasher(t, legacy)
	store := newFakeStore()

	const legacyToken = "sk-legacy-9999999999999999999999999999" // pragma: allowlist secret — test fixture
	store.put(record(t, h, testToken, SchemeDorangV1, Principal{KeyID: "modern"}))
	store.put(record(t, h, legacyToken, SchemeLegacySHA256, Principal{KeyID: "imported"}))

	a := newAuth(t, Config{Legacy: legacy, Store: store})

	for _, c := range []struct{ token, keyID string }{
		{testToken, "modern"},
		{legacyToken, "imported"},
	} {
		before := store.callCount()
		p, err := a.Authenticate(context.Background(), c.token)
		if err != nil {
			t.Fatalf("Authenticate(%s): %v", c.keyID, err)
		}
		if p.KeyID != c.keyID {
			t.Fatalf("KeyID = %q, want %q", p.KeyID, c.keyID)
		}
		if n := store.callCount() - before; n != 1 {
			t.Fatalf("%s took %d store lookups, want exactly 1", c.keyID, n)
		}
	}

	// The index key is scheme-independent: it is derived from the token alone.
	raw := sha256.Sum256([]byte(testToken))
	if want := hex.EncodeToString(raw[:LookupBytes]); LookupKey(testToken) != want {
		t.Fatalf("LookupKey = %q, want %q", LookupKey(testToken), want)
	}
	if len(LookupKey(testToken)) != LookupHexLen {
		t.Fatalf("lookup key is %d chars, want %d", len(LookupKey(testToken)), LookupHexLen)
	}
}

func TestDigestsMatchReferenceImplementations(t *testing.T) {
	h := testHasher(t, LegacyPolicy{})
	for _, tok := range []string{
		"sk-a", testToken,
		"sk-" + strings.Repeat("x", MaxTokenLen), // forces the allocating path
	} {
		sum, mac := h.digests(tok)
		if want := sha256.Sum256([]byte(tok)); sum != want {
			t.Fatalf("sha256 mismatch for a %d-byte token", len(tok))
		}
		ref := hmac.New(sha256.New, []byte(testPepper))
		ref.Write([]byte(tok))
		var want Digest
		copy(want[:], ref.Sum(nil))
		if mac != want {
			t.Fatalf("HMAC mismatch for a %d-byte token", len(tok))
		}
	}
}

// TestVerifyComputesBothDigests is the functional half of "constant time": the
// work done does not depend on which scheme the row carries, because both
// digests are always produced before either is selected.
func TestVerifyComputesBothDigests(t *testing.T) {
	h := testHasher(t, LegacyPolicy{Enabled: true, Until: testNow.Add(time.Hour)})
	sum, mac := h.digests(testToken)
	if sum == mac {
		t.Fatal("the two digests must differ")
	}
	legacyDigest, err := h.Hash(SchemeLegacySHA256, testToken)
	if err != nil {
		t.Fatal(err)
	}
	v1Digest, err := h.Hash(SchemeDorangV1, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if legacyDigest != sum || v1Digest != mac {
		t.Fatal("Hash disagrees with digests")
	}
}

// TestVerifyRefusesCrossSchemeDigest catches an implementation that compares
// against whichever digest happens to match: a legacy digest stored under
// dorang_v1 must not verify, and the reverse must not either.
func TestVerifyRefusesCrossSchemeDigest(t *testing.T) {
	h := testHasher(t, LegacyPolicy{Enabled: true, Until: testNow.Add(time.Hour)})
	legacyDigest, _ := h.Hash(SchemeLegacySHA256, testToken)
	v1Digest, _ := h.Hash(SchemeDorangV1, testToken)

	if err := h.VerifyToken(SchemeDorangV1, legacyDigest, testToken, testNow); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("legacy digest verified as dorang_v1: %v", err)
	}
	if err := h.VerifyToken(SchemeLegacySHA256, v1Digest, testToken, testNow); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("dorang_v1 digest verified as legacy: %v", err)
	}
	if err := h.VerifyToken(SchemeUnknown, v1Digest, testToken, testNow); !errors.Is(err, ErrSchemeUnsupported) {
		t.Fatalf("unknown scheme verified: %v", err)
	}
}

func TestLegacyWindow(t *testing.T) {
	until := testNow.Add(time.Hour)
	h := testHasher(t, LegacyPolicy{Enabled: true, Until: until})
	d, _ := h.Hash(SchemeLegacySHA256, testToken)

	if err := h.VerifyToken(SchemeLegacySHA256, d, testToken, testNow); err != nil {
		t.Fatalf("inside the window: %v", err)
	}
	if err := h.VerifyToken(SchemeLegacySHA256, d, testToken, until); !errors.Is(err, ErrLegacyWindowClosed) {
		t.Fatalf("at until: %v, want the window closed", err)
	}
	if err := h.VerifyToken(SchemeLegacySHA256, d, testToken, until.Add(time.Second)); !errors.Is(err, ErrLegacyWindowClosed) {
		t.Fatalf("after until: %v", err)
	}

	off := testHasher(t, LegacyPolicy{})
	if err := off.VerifyToken(SchemeLegacySHA256, d, testToken, testNow); !errors.Is(err, ErrLegacyDisabled) {
		t.Fatalf("legacy disabled: %v", err)
	}
	if _, err := NewHasher(testPepper, LegacyPolicy{Enabled: true}); !errors.Is(err, ErrLegacyNoUntil) {
		t.Fatalf("enabling legacy without a date must be refused, got %v", err)
	}
	if err := (LegacyPolicy{Enabled: true, Until: testNow.Add(-time.Second)}).Validate(testNow); err == nil {
		t.Fatal("a past until date must not validate")
	}
	if err := (LegacyPolicy{Enabled: true, Until: until}).Validate(testNow); err != nil {
		t.Fatalf("a future until date must validate: %v", err)
	}
	if _, err := NewHasher("", LegacyPolicy{}); !errors.Is(err, ErrNoPepper) {
		t.Fatalf("a hasher without a pepper must be refused, got %v", err)
	}
}

// ------------------------------------------------------------- the sk- gate ---

func TestPrefixGateRejectsBeforeAnyLookup(t *testing.T) {
	h := testHasher(t, LegacyPolicy{})
	store := newFakeStore()
	rec := record(t, h, testToken, SchemeDorangV1, Principal{})
	store.put(rec)
	a := newAuth(t, Config{Store: store})

	for _, bad := range []string{
		rec.Digest.Hex(),  // a stored digest replayed as a credential
		rec.Lookup,        // the index key replayed as a credential
		"pk-" + testToken, // wrong prefix
		"Sk-uppercase",    // the prefix is exact
		strings.TrimPrefix(testToken, "sk-"),
	} {
		before := store.callCount()
		_, err := a.Authenticate(context.Background(), bad)
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("Authenticate(%.8q…) = %v, want ErrMalformed", bad, err)
		}
		if n := store.callCount() - before; n != 0 {
			t.Fatalf("%d store lookups happened for a credential that failed the prefix gate", n)
		}
	}
	if _, err := a.Authenticate(context.Background(), ""); !errors.Is(err, ErrMissingCredential) {
		t.Fatal("an empty credential must be ErrMissingCredential")
	}
}

// ------------------------------------------------------------- master key ---

func TestMasterKeyIsNeverARow(t *testing.T) {
	store := newFakeStore()
	a := newAuth(t, Config{Store: store})

	p, err := a.Authenticate(context.Background(), testMaster)
	if err != nil {
		t.Fatalf("master: %v", err)
	}
	if !p.IsMaster() {
		t.Fatal("master credential did not produce a master principal")
	}
	if n := store.callCount(); n != 0 {
		t.Fatalf("the master credential consulted the store %d times", n)
	}

	// It still authenticates when the store is failing entirely — the whole
	// point of R1-A's finding.
	store.mu.Lock()
	store.err = errors.New("database is gone")
	store.mu.Unlock()
	if _, err := a.Authenticate(context.Background(), testMaster); err != nil {
		t.Fatalf("master credential lost when the store failed: %v", err)
	}
	// And with no store at all, which is the imported-database case.
	b := newAuth(t, Config{})
	if _, err := b.Authenticate(context.Background(), testMaster); err != nil {
		t.Fatalf("master credential lost without a store: %v", err)
	}
	// A master principal is authorized unconditionally: it carries no limits.
	if err := p.Authorize(Access{Now: testNow, Model: "anything", Route: "/v1/anything"}); err != nil {
		t.Fatalf("master authorize: %v", err)
	}
	// A near-miss is not the master.
	if _, err := a.Authenticate(context.Background(), testMaster+"x"); err == nil {
		t.Fatal("a credential differing from the master authenticated as master")
	}
}

func TestNewRefusesToLoseAdminAuth(t *testing.T) {
	_, err := New(Config{Pepper: testPepper})
	if !errors.Is(err, ErrMasterKeyRequired) {
		t.Fatalf("New without a master key = %v, want ErrMasterKeyRequired", err)
	}
	a, err := New(Config{Pepper: testPepper, NoMasterKey: true})
	if err != nil {
		t.Fatalf("New with NoMasterKey: %v", err)
	}
	a.Close()
	if _, err := New(Config{MasterKey: testMaster}); !errors.Is(err, ErrNoPepper) {
		t.Fatalf("New without a pepper = %v, want ErrNoPepper", err)
	}
}

// ---------------------------------------------------------- authorization ---

// TestAuthorizeRefusesPerField is one case per authorization field R1-A
// requires to be carried. Each asserts a request is refused when it should be.
func TestAuthorizeRefusesPerField(t *testing.T) {
	base := func() *Principal {
		return &Principal{KeyID: "key-1", UserID: "u1", TeamID: "t1"}
	}
	access := Access{Now: testNow, Model: "model-x", Route: "/v1/chat/completions"}

	cases := []struct {
		name   string
		mutate func(*Principal)
		want   Reason
	}{
		{"expiry", func(p *Principal) { p.Key.ExpiresAt = testNow.Add(-time.Second) }, ReasonExpired},
		{"expiry/boundary", func(p *Principal) { p.Key.ExpiresAt = testNow }, ReasonExpired},
		{"blocked", func(p *Principal) { p.Key.Blocked = true }, ReasonBlocked},
		{"models", func(p *Principal) { p.Key.Models = []string{"model-y"} }, ReasonModelNotAllowed},
		{"allowed_routes", func(p *Principal) { p.Key.AllowedRoutes = []string{"/v1/embeddings"} }, ReasonRouteNotAllowed},
		{"budget", func(p *Principal) {
			p.Key.MaxBudgetNanoUSD = Limit(10_000_000_000)
			p.Key.SpentNanoUSD = 10_000_000_000
			p.Key.BudgetResetAt = testNow.Add(time.Hour)
		}, ReasonBudgetExceeded},
		{"rpm_limit", func(p *Principal) { p.Key.RPMLimit = Limit(10) }, ReasonRateLimited},
		{"tpm_limit", func(p *Principal) { p.Key.TPMLimit = Limit(100) }, ReasonRateLimited},
		{"team", func(p *Principal) { p.Team = &Limits{Blocked: true} }, ReasonBlocked},
		{"team/expiry", func(p *Principal) { p.Team = &Limits{ExpiresAt: testNow.Add(-time.Hour)} }, ReasonExpired},
		{"team/models", func(p *Principal) { p.Team = &Limits{Models: []string{"model-z"}} }, ReasonModelNotAllowed},
		{"user", func(p *Principal) { p.User = &Limits{Blocked: true} }, ReasonBlocked},
		{"user/budget", func(p *Principal) {
			p.User = &Limits{MaxBudgetNanoUSD: Limit(1), SpentNanoUSD: 1}
		}, ReasonBudgetExceeded},
		{"owner", func(p *Principal) { p.RequireOwner = true; p.UserID, p.TeamID = "", "" }, ReasonNoPrincipal},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base()
			c.mutate(p)
			a := access
			if c.want == ReasonRateLimited {
				a.ObservedRPM, a.ObservedTPM = 10, 100
			}
			err := p.Authorize(a)
			if got := ReasonOf(err); got != c.want {
				t.Fatalf("Authorize = %v (reason %v), want reason %v", err, got, c.want)
			}
			var e *Error
			if !errors.As(err, &e) || e.Subject == "" {
				t.Fatalf("refusal does not say which subject refused: %v", err)
			}
			if !e.Terminal() {
				t.Fatal("an authorization refusal must be terminal, never a fallback condition")
			}
		})
	}

	// The unrestricted principal passes every check.
	if err := base().Authorize(access); err != nil {
		t.Fatalf("an unrestricted key was refused: %v", err)
	}
}

func TestAuthorizeAllowLists(t *testing.T) {
	p := &Principal{Key: Limits{
		Models:        []string{"model-x", "model-y"},
		AllowedRoutes: []string{"/v1/chat/completions", "/openai/*"},
	}}
	ok := []Access{
		{Now: testNow, Model: "model-x", Route: "/v1/chat/completions"},
		{Now: testNow, Model: "model-y", Route: "/openai/deployments/m/chat/completions"},
	}
	for _, a := range ok {
		if err := p.Authorize(a); err != nil {
			t.Fatalf("Authorize(%+v) = %v", a, err)
		}
	}
	bad := []Access{
		{Now: testNow, Model: "model-z", Route: "/v1/chat/completions"},
		{Now: testNow, Model: "model-x", Route: "/v1/embeddings"},
	}
	for _, a := range bad {
		if err := p.Authorize(a); err == nil {
			t.Fatalf("Authorize(%+v) allowed a request outside the allow-lists", a)
		}
	}
	// A wildcard entry and an empty list both mean "everything".
	star := &Principal{Key: Limits{Models: []string{"*"}}}
	if err := star.Authorize(Access{Now: testNow, Model: "anything"}); err != nil {
		t.Fatalf(`"*" did not allow everything: %v`, err)
	}
}

// TestAConfiguredZeroLimitDeniesAndAnAbsentOneDoesNot guards the fail-open
// shape R1-A warns about: "no limit configured" and "a limit of zero" are
// different answers, and flattening them into a bare 0 is how a configured
// limit quietly stops existing. The storage layer carries the same
// distinction, so this is also what keeps an import faithful.
func TestAConfiguredZeroLimitDeniesAndAnAbsentOneDoesNot(t *testing.T) {
	unconfigured := &Principal{KeyID: "k"}
	if err := unconfigured.Authorize(Access{
		Now: testNow, ObservedRPM: 1_000_000, ObservedTPM: 1_000_000,
	}); err != nil {
		t.Fatalf("an unconfigured limit refused a request: %v", err)
	}
	if err := (&Principal{Key: Limits{SpentNanoUSD: 1 << 40}}).Authorize(Access{Now: testNow}); err != nil {
		t.Fatalf("spend with no ceiling refused a request: %v", err)
	}

	cases := []struct {
		name string
		lim  Limits
		want Reason
	}{
		{"rpm=0", Limits{RPMLimit: Limit(0)}, ReasonRateLimited},
		{"tpm=0", Limits{TPMLimit: Limit(0)}, ReasonRateLimited},
		{"budget=0", Limits{MaxBudgetNanoUSD: Limit(0)}, ReasonBudgetExceeded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Principal{KeyID: "k", Key: c.lim}
			if got := ReasonOf(p.Authorize(Access{Now: testNow})); got != c.want {
				t.Fatalf("a configured limit of zero gave %v, want %v", got, c.want)
			}
		})
	}
}

func TestBudgetOfAnEndedPeriodDoesNotRefuse(t *testing.T) {
	p := &Principal{Key: Limits{
		MaxBudgetNanoUSD: Limit(1000),
		SpentNanoUSD:     5000,
		BudgetResetAt:    testNow.Add(-time.Minute), // the period already ended
	}}
	if err := p.Authorize(Access{Now: testNow}); err != nil {
		t.Fatalf("stale spend refused a request: %v", err)
	}
}

func TestExpiredKeyIsRefusedNotResurrected(t *testing.T) {
	h := testHasher(t, LegacyPolicy{})
	a := newAuth(t, Config{})
	rec := record(t, h, testToken, SchemeDorangV1, Principal{
		KeyID: "expired-1",
		Key:   Limits{ExpiresAt: testNow.Add(-24 * time.Hour)},
	})
	if err := a.Load([]Record{rec}); err != nil {
		t.Fatal(err)
	}
	// Present but refused, and refused as expired rather than as unknown: an
	// importer that dropped expired rows would be indistinguishable from a
	// deleted key, and one that imported them as live would restore revoked
	// access (R1-A).
	_, err := a.Authenticate(context.Background(), testToken)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("Authenticate = %v, want ErrExpired", err)
	}
	if a.Stats().SnapshotSize != 1 {
		t.Fatal("the expired row was not loaded")
	}
}

func TestBlockedKeyIsRefusedAtAuthentication(t *testing.T) {
	h := testHasher(t, LegacyPolicy{})
	a := newAuth(t, Config{})
	rec := record(t, h, testToken, SchemeDorangV1, Principal{KeyID: "b", Key: Limits{Blocked: true}})
	if err := a.Load([]Record{rec}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(context.Background(), testToken); !errors.Is(err, ErrBlocked) {
		t.Fatalf("a blocked key authenticated: %v", err)
	}
}

func TestCheckCombinesBothHalves(t *testing.T) {
	h := testHasher(t, LegacyPolicy{})
	a := newAuth(t, Config{})
	rec := record(t, h, testToken, SchemeDorangV1, Principal{
		KeyID: "k", Key: Limits{Models: []string{"model-x"}},
	})
	if err := a.Load([]Record{rec}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Check(context.Background(), testToken, Access{Now: testNow, Model: "model-x"}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	_, err := a.Check(context.Background(), testToken, Access{Now: testNow, Model: "model-q"})
	if !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("Check = %v, want ErrModelNotAllowed", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Status() != http.StatusForbidden || e.Code() != "model_not_allowed" {
		t.Fatalf("error envelope fields: %+v", e)
	}
}

// ------------------------------------------------------------ cache misses ---

func TestConcurrentMissesAreCoalescedIntoOneStoreCall(t *testing.T) {
	h := testHasher(t, LegacyPolicy{})
	store := newFakeStore()
	store.gate = make(chan struct{})
	store.put(record(t, h, testToken, SchemeDorangV1, Principal{KeyID: "shared"}))
	a := newAuth(t, Config{Store: store})

	const n = 64
	var wg sync.WaitGroup
	errs := make([]error, n)
	ready := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready
			_, errs[i] = a.Authenticate(context.Background(), testToken)
		}()
	}
	close(ready)
	// Let the goroutines pile up behind the gated lookup, then release it.
	for a.fl.inFlight() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(store.gate)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if got := store.callCount(); got != 1 {
		t.Fatalf("%d concurrent misses caused %d store lookups, want 1", n, got)
	}
	if c := a.Stats().Coalesced; c == 0 {
		t.Fatal("no lookup was recorded as coalesced")
	}
}

func TestUnknownKeyIsNegativelyCachedAndBounded(t *testing.T) {
	store := newFakeStore()
	a := newAuth(t, Config{Store: store, NegativeTTL: time.Minute})
	const unknown = "sk-unknown-000000000000000000000000000" // pragma: allowlist secret — test fixture

	for range 5 {
		if _, err := a.Authenticate(context.Background(), unknown); !errors.Is(err, ErrUnknownKey) {
			t.Fatalf("want ErrUnknownKey, got %v", err)
		}
	}
	if n := store.callCount(); n != 1 {
		t.Fatalf("an unknown key was looked up %d times, want 1 while negatively cached", n)
	}
	if a.Stats().SnapshotSize != 0 {
		t.Fatal("a negative entry was promoted into the lock-free snapshot")
	}
}

func TestTTLExpiryAndInvalidate(t *testing.T) {
	h := testHasher(t, LegacyPolicy{})
	store := newFakeStore()
	store.put(record(t, h, testToken, SchemeDorangV1, Principal{KeyID: "v1"}))

	now := testNow
	a := newAuth(t, Config{Store: store, EntryTTL: time.Minute, Now: func() time.Time { return now }})

	if _, err := a.Authenticate(context.Background(), testToken); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(context.Background(), testToken); err != nil {
		t.Fatal(err)
	}
	if n := store.callCount(); n != 1 {
		t.Fatalf("cached entry was re-read %d times", n)
	}

	now = now.Add(2 * time.Minute) // TTL elapsed
	store.put(record(t, h, testToken, SchemeDorangV1, Principal{KeyID: "v2"}))
	p, err := a.Authenticate(context.Background(), testToken)
	if err != nil {
		t.Fatal(err)
	}
	if p.KeyID != "v2" {
		t.Fatalf("after the TTL the row was not re-read: KeyID = %q", p.KeyID)
	}

	// Explicit invalidation, independent of the TTL.
	store.put(record(t, h, testToken, SchemeDorangV1, Principal{KeyID: "v3"}))
	if err := a.Invalidate(LookupKey(testToken)); err != nil {
		t.Fatal(err)
	}
	p, err = a.Authenticate(context.Background(), testToken)
	if err != nil {
		t.Fatal(err)
	}
	if p.KeyID != "v3" {
		t.Fatalf("Invalidate did not force a re-read: KeyID = %q", p.KeyID)
	}

	// Load'ed rows do not expire; the snapshot is the authority (DESIGN §9.1).
	if err := a.Load([]Record{record(t, h, testToken, SchemeDorangV1, Principal{KeyID: "loaded"})}); err != nil {
		t.Fatal(err)
	}
	calls := store.callCount()
	now = now.Add(24 * time.Hour)
	p, err = a.Authenticate(context.Background(), testToken)
	if err != nil {
		t.Fatal(err)
	}
	if p.KeyID != "loaded" || store.callCount() != calls {
		t.Fatalf("a loaded row expired: KeyID = %q, store calls %d→%d", p.KeyID, calls, store.callCount())
	}

	a.InvalidateAll()
	if s := a.Stats(); s.SnapshotSize != 0 || s.OverlaySize != 0 {
		t.Fatalf("InvalidateAll left %+v", s)
	}
}

func TestOverlayIsFoldedIntoTheSnapshot(t *testing.T) {
	h := testHasher(t, LegacyPolicy{})
	store := newFakeStore()
	tokens := make([]string, mergeThreshold+2)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("sk-bulk-%030d", i)
		store.put(record(t, h, tokens[i], SchemeDorangV1, Principal{KeyID: fmt.Sprint(i)}))
	}
	a := newAuth(t, Config{Store: store})
	for _, tok := range tokens {
		if _, err := a.Authenticate(context.Background(), tok); err != nil {
			t.Fatalf("%s: %v", tok, err)
		}
	}
	s := a.Stats()
	if s.SnapshotSize < mergeThreshold {
		t.Fatalf("overlay was never folded into the snapshot: %+v", s)
	}
	if s.OverlaySize > mergeThreshold {
		t.Fatalf("overlay kept growing: %+v", s)
	}
	// Everything still resolves after the fold.
	calls := store.callCount()
	for _, tok := range tokens {
		if _, err := a.Authenticate(context.Background(), tok); err != nil {
			t.Fatalf("after fold %s: %v", tok, err)
		}
	}
	if store.callCount() != calls {
		t.Fatal("entries were lost by the fold")
	}
}

func TestStoreFailureIsNotAnInvalidKey(t *testing.T) {
	store := newFakeStore()
	store.err = errors.New("connection refused")
	a := newAuth(t, Config{Store: store})
	_, err := a.Authenticate(context.Background(), testToken)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Authenticate = %v, want ErrUnavailable", err)
	}
	var e *Error
	if errors.As(err, &e) && e.Status() != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", e.Status())
	}
}

// ------------------------------------------------------------ rehash on use ---

func TestRehashOnUseUpgradesLegacyRows(t *testing.T) {
	legacy := LegacyPolicy{Enabled: true, Until: testNow.Add(time.Hour)}
	h := testHasher(t, legacy)
	store := newFakeStore()
	store.put(record(t, h, testToken, SchemeLegacySHA256, Principal{KeyID: "imported"}))
	a := newAuth(t, Config{Legacy: legacy, Store: store, RehashOnUse: true})

	if _, err := a.Authenticate(context.Background(), testToken); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for store.rehashCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if store.rehashCount() != 1 {
		t.Fatalf("the legacy row was not upgraded (%d upgrades)", store.rehashCount())
	}
	store.mu.Lock()
	got := store.rehashed["imported"]
	store.mu.Unlock()
	want, _ := h.Hash(SchemeDorangV1, testToken)
	if got != want {
		t.Fatal("the upgrade wrote the wrong digest")
	}

	// The upgraded row keeps authenticating, now under dorang_v1.
	p, err := a.Authenticate(context.Background(), testToken)
	if err != nil {
		t.Fatalf("after the upgrade: %v", err)
	}
	if p.KeyID != "imported" {
		t.Fatalf("KeyID = %q", p.KeyID)
	}
}

// TestRehashOnUseAddsNoLatency holds the upgrade open for the whole test. The
// request that triggers it must not wait for it: the queue is asynchronous and
// the enqueue is non-blocking.
func TestRehashOnUseAddsNoLatency(t *testing.T) {
	legacy := LegacyPolicy{Enabled: true, Until: testNow.Add(time.Hour)}
	h := testHasher(t, legacy)
	store := newFakeStore()
	block := make(chan struct{})
	store.rehashCh = block
	defer close(block)
	store.put(record(t, h, testToken, SchemeLegacySHA256, Principal{KeyID: "imported"}))
	a := newAuth(t, Config{Legacy: legacy, Store: store, RehashOnUse: true, RehashQueue: 1})

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		for range 200 {
			if _, err := a.Authenticate(context.Background(), testToken); err != nil {
				t.Error(err)
				break
			}
		}
		done <- time.Since(start)
	}()
	select {
	case d := <-done:
		if d > time.Second {
			t.Fatalf("200 authentications behind a stuck upgrade took %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("authentication blocked on the rehash worker")
	}
	// Queue depth 1 with a stuck worker: further upgrades are dropped, not
	// waited on.
	if s := a.Stats(); s.RehashQueued == 0 {
		t.Fatalf("no upgrade was ever queued: %+v", s)
	}
}

func TestRehashRequiresAStoreThatImplementsIt(t *testing.T) {
	legacy := LegacyPolicy{Enabled: true, Until: testNow.Add(time.Hour)}
	h := testHasher(t, legacy)
	plain := &noRehashStore{inner: newFakeStore()}
	plain.put(record(t, h, testToken, SchemeLegacySHA256, Principal{KeyID: "imported"}))
	a := newAuth(t, Config{Legacy: legacy, Store: plain, RehashOnUse: true})
	if _, err := a.Authenticate(context.Background(), testToken); err != nil {
		t.Fatal(err)
	}
	if s := a.Stats(); s.RehashQueued != 0 {
		t.Fatalf("queued an upgrade with no Rehasher: %+v", s)
	}
}

// noRehashStore is a Store that deliberately does not implement Rehasher.
type noRehashStore struct{ inner *fakeStore }

func (s *noRehashStore) put(rec Record) { s.inner.put(rec) }

func (s *noRehashStore) LoadByLookup(ctx context.Context, lookup string) (Record, error) {
	return s.inner.LoadByLookup(ctx, lookup)
}

// ---------------------------------------------------------------- secrets ---

// TestNoOutputContainsASecret formats everything this package can hand a
// caller and asserts that no credential, pepper or master key appears in any
// of it.
func TestNoOutputContainsASecret(t *testing.T) {
	legacy := LegacyPolicy{Enabled: true, Until: testNow.Add(time.Hour)}
	h := testHasher(t, legacy)
	store := newFakeStore()
	store.put(record(t, h, testToken, SchemeDorangV1, Principal{KeyID: "k", Label: "sk-…2345"}))
	cfg := Config{
		Pepper: testPepper, MasterKey: testMaster, Legacy: legacy,
		Store: store, Now: func() time.Time { return testNow },
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	secrets := []string{testToken, testPepper, testMaster}
	var out []string

	// Every fmt verb over every type that touches key material.
	for _, verb := range []string{"%v", "%s", "%q", "%#v", "%+v", "%x", "%X", "%d"} {
		out = append(out,
			fmt.Sprintf(verb, cfg),
			fmt.Sprintf(verb, &cfg),
			fmt.Sprintf(verb, h),
			fmt.Sprintf(verb, *h),
			fmt.Sprintf(verb, a),
		)
	}

	// Every error this package produces for a failing credential.
	bad := []string{"", "not-a-key", testToken + "x", testMaster + "x", "sk-nope"} // pragma: allowlist secret — test fixture
	for _, tok := range bad {
		if _, err := a.Authenticate(context.Background(), tok); err != nil {
			out = append(out, err.Error(), fmt.Sprintf("%v|%+v|%#v", err, err, err))
		}
	}
	p, err := a.Authenticate(context.Background(), testToken)
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, fmt.Sprintf("%v %+v %#v", p, p, p), fmt.Sprintf("%+v", a.Stats()))

	// Authorization refusals.
	blocked := &Principal{KeyID: "k", Key: Limits{Blocked: true}}
	if err := blocked.Authorize(Access{Now: testNow}); err != nil {
		out = append(out, err.Error())
	}
	// The legacy configuration errors.
	if _, err := NewHasher("", LegacyPolicy{}); err != nil {
		out = append(out, err.Error())
	}
	if err := legacy.Validate(testNow.Add(48 * time.Hour)); err != nil {
		out = append(out, err.Error())
	}

	for _, s := range out {
		for _, secret := range secrets {
			if strings.Contains(s, secret) {
				t.Fatalf("output leaked a secret: %q", s)
			}
			// Any 12-character run of a secret is already too much.
			if len(secret) > 12 && strings.Contains(s, secret[:12]) {
				t.Fatalf("output leaked a secret prefix: %q", s)
			}
		}
	}
}

func TestPrincipalCarriesNoCredential(t *testing.T) {
	h := testHasher(t, LegacyPolicy{})
	a := newAuth(t, Config{})
	if err := a.Load([]Record{record(t, h, testToken, SchemeDorangV1, Principal{KeyID: "k"})}); err != nil {
		t.Fatal(err)
	}
	p, err := a.Authenticate(context.Background(), testToken)
	if err != nil {
		t.Fatal(err)
	}
	rendered := fmt.Sprintf("%#v", *p)
	if strings.Contains(rendered, "sk-") {
		t.Fatalf("a principal rendered something that looks like a credential: %s", rendered)
	}
}

// ----------------------------------------------------------------- errors ---

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		err    *Error
		status int
		code   string
	}{
		// COMPATIBILITY §11.2's "Missing or malformed credential" and "Expired
		// or revoked credential" rows both pin `invalid_api_key`. Five distinct
		// spellings here were five ways for a client's invalid_api_key branch
		// to miss.
		{ErrMissingCredential, http.StatusUnauthorized, "invalid_api_key"},
		{ErrMalformed, http.StatusUnauthorized, "invalid_api_key"},
		{ErrUnknownKey, http.StatusUnauthorized, "invalid_api_key"},
		{ErrDigestMismatch, http.StatusUnauthorized, "invalid_api_key"},
		{ErrExpired, http.StatusUnauthorized, "invalid_api_key"},
		// Not collapsed, because the fix is different: §11.2 carries these as
		// dorang's own rows.
		{ErrSecretRetired, http.StatusUnauthorized, "secret_retired"},
		{ErrPended, http.StatusForbidden, "credential_pended"},
		{ErrBlocked, http.StatusForbidden, "key_blocked"},
		{ErrModelNotAllowed, http.StatusForbidden, "model_not_allowed"},
		{ErrRouteNotAllowed, http.StatusForbidden, "route_not_allowed"},
		{ErrRateLimited, http.StatusTooManyRequests, "rate_limit_exceeded"},
		{ErrBudgetExceeded, http.StatusBadRequest, "budget_exceeded"},
		{ErrUnavailable, http.StatusServiceUnavailable, "auth_unavailable"},
	}
	for _, c := range cases {
		if got := c.err.Status(); got != c.status {
			t.Errorf("%v: status %d, want %d", c.err.Reason, got, c.status)
		}
		if got := c.err.Code(); got != c.code {
			t.Errorf("%v: code %q, want %q", c.err.Reason, got, c.code)
		}
		// COMPATIBILITY 7.1: the code is a string, and it is machine-readable.
		if strings.ContainsAny(c.err.Code(), " ") {
			t.Errorf("%v: code %q is not machine-readable", c.err.Reason, c.err.Code())
		}
	}
	// Budget refusal is terminal, never a fallback condition (DESIGN §6.4).
	if !ErrBudgetExceeded.Terminal() {
		t.Fatal("budget exceeded must be terminal")
	}
	wrapped := fmt.Errorf("front door: %w", refuse(ReasonExpired, "key", "k1"))
	if !errors.Is(wrapped, ErrExpired) || ReasonOf(wrapped) != ReasonExpired {
		t.Fatal("wrapped refusals must stay identifiable")
	}
}

func TestParseHelpers(t *testing.T) {
	if _, err := ParseLookup("short"); err == nil {
		t.Fatal("ParseLookup accepted a short key")
	}
	if _, err := ParseLookup(strings.Repeat("z", LookupHexLen)); err == nil {
		t.Fatal("ParseLookup accepted non-hex")
	}
	l, err := ParseLookup(LookupKey(testToken))
	if err != nil || l.Hex() != LookupKey(testToken) {
		t.Fatalf("ParseLookup round trip: %v %q", err, l.Hex())
	}
	if _, err := ParseDigest("abcd"); err == nil {
		t.Fatal("ParseDigest accepted a short digest")
	}
	for _, s := range []string{"dorang_v1", "legacy_sha256"} {
		sc, err := ParseScheme(s)
		if err != nil || sc.String() != s {
			t.Fatalf("ParseScheme(%q) = %v, %v", s, sc, err)
		}
	}
	if _, err := ParseScheme("bcrypt"); err == nil {
		t.Fatal("ParseScheme accepted an unknown scheme")
	}
	if err := (&Authenticator{}).Invalidate("nope"); err == nil {
		t.Fatal("Invalidate accepted a malformed lookup key")
	}
}

func TestLoadRejectsAMalformedLookupKey(t *testing.T) {
	a := newAuth(t, Config{})
	err := a.Load([]Record{{Lookup: "not-hex", Principal: Principal{KeyID: "k"}}})
	if err == nil {
		t.Fatal("Load accepted a malformed lookup key")
	}
	if strings.Contains(err.Error(), "sk-") {
		t.Fatal("the error leaked something credential-shaped")
	}
}
