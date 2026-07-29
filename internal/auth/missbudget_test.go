package auth

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// budgetAuth builds an authenticator over an empty store with a frozen clock.
func budgetAuth(t *testing.T, clk *fakeClock, mut func(*Config)) (*Authenticator, *fakeStore) {
	t.Helper()
	st := newFakeStore()
	cfg := Config{
		Pepper:      testPepper,
		NoMasterKey: true,
		Store:       st,
		Now:         clk.Now,
	}
	if mut != nil {
		mut(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a, st
}

func budgetToken(i int) string { return "sk-budget-" + strconv.Itoa(i) }

// A flood of DISTINCT unknown keys must cost the store a BOUNDED number of round
// trips.
//
// This is the assertion the negative cache could never make. Negative entries
// are keyed by the index key, so a caller who never repeats a key never hits
// one: before this fix, 5000 invented credentials were 5000 LoadByLookup calls
// into the one database every tenant shares, bought at zero cost by an
// unauthenticated caller. The measurement is the round trips, not the presence
// of a cache, because a cache is exactly what does not help here.
//
// The clock is frozen, so the bucket never refills and the bound is the burst
// exactly. Against the unbounded code this reports 5000.
func TestUnknownKeyFloodIsBoundedInStoreRoundTrips(t *testing.T) {
	const floods = 5000

	clk := newClock(testNow)
	a, st := budgetAuth(t, clk, nil)

	ctx := context.Background()
	for i := 0; i < floods; i++ {
		_, err := a.Authenticate(ctx, budgetToken(i))
		if err == nil {
			t.Fatalf("an invented credential authenticated at i=%d", i)
		}
	}

	stats := a.Stats()
	if got := st.callCount(); got != DefaultMissBurst {
		t.Errorf("%d store round trips for %d distinct unknown keys, want %d: an "+
			"unauthenticated caller is still amplifying invented credentials into "+
			"database reads", got, floods, DefaultMissBurst)
	}
	if stats.Misses != floods {
		t.Errorf("misses = %d, want %d", stats.Misses, floods)
	}
	if want := uint64(floods - DefaultMissBurst); stats.LookupsThrottled != want {
		t.Errorf("throttled = %d, want %d: the refusals the store never saw have to "+
			"be counted, or the bound is invisible at the moment it fires",
			stats.LookupsThrottled, want)
	}
}

// A throttled lookup is 503 and retryable, never 401.
//
// The gateway did not consult the store, so it does not KNOW the key is unknown.
// Answering invalid_api_key there would be a claim it cannot support, and it
// would be the wrong answer for a key provisioned one second ago.
func TestThrottledLookupIsRetryableNotARefusal(t *testing.T) {
	clk := newClock(testNow)
	a, _ := budgetAuth(t, clk, func(c *Config) { c.MissBurst = 1 })

	ctx := context.Background()
	if _, err := a.Authenticate(ctx, budgetToken(0)); ReasonOf(err) != ReasonUnknownKey {
		t.Fatalf("the first unknown key: reason = %v, want unknown_key", ReasonOf(err))
	}
	_, err := a.Authenticate(ctx, budgetToken(1))
	if got := ReasonOf(err); got != ReasonUnavailable {
		t.Fatalf("the throttled lookup: reason = %v, want unavailable", got)
	}
	var e *Error
	if !asAuthError(err, &e) {
		t.Fatalf("the refusal is not an *auth.Error: %T", err)
	}
	if e.Status() != 503 {
		t.Errorf("status = %d, want 503: a lookup the gateway declined to make is a "+
			"retryable condition, not a verdict on the credential", e.Status())
	}
}

// The regression the bound could introduce, asserted directly: a key that is
// CREATED after it has already been looked up and missed must become usable
// promptly.
//
// Both halves of the fix could break this. A negative entry that outlived
// provisioning would refuse a real key for a full entry TTL, and a budget that
// throttled every lookup would refuse it for longer than that. The bound is the
// NEGATIVE TTL — five seconds by default — and it is asserted as a bound rather
// than as "eventually".
func TestKeyCreatedAfterAMissIsUsablePromptly(t *testing.T) {
	clk := newClock(testNow)
	a, st := budgetAuth(t, clk, nil)
	h := testHasher(t, LegacyPolicy{})

	const tok = "sk-provisioned-just-now-0123456789" // pragma: allowlist secret — test fixture
	ctx := context.Background()

	// Used before it exists: one store round trip, refused, remembered.
	if _, err := a.Authenticate(ctx, tok); ReasonOf(err) != ReasonUnknownKey {
		t.Fatalf("reason = %v, want unknown_key", ReasonOf(err))
	}
	if got := st.callCount(); got != 1 {
		t.Fatalf("store calls = %d, want 1", got)
	}
	if _, err := a.Authenticate(ctx, tok); ReasonOf(err) != ReasonUnknownKey {
		t.Fatalf("the repeat: reason = %v, want unknown_key", ReasonOf(err))
	}
	if got := st.callCount(); got != 1 {
		t.Fatalf("store calls = %d after a repeat, want 1: the refusal is not cached", got)
	}

	// Now it is provisioned.
	st.put(record(t, h, tok, SchemeDorangV1, Principal{KeyID: "key-new"}))

	// The negative TTL is the whole of the wait, and it is shorter than the
	// entry TTL by construction — New refuses a configuration where it is not.
	if a.NegativeTTL() >= a.EntryTTL() {
		t.Fatalf("negative TTL %s is not shorter than the entry TTL %s",
			a.NegativeTTL(), a.EntryTTL())
	}
	clk.Advance(a.NegativeTTL() + time.Nanosecond)

	p, err := a.Authenticate(ctx, tok)
	if err != nil {
		t.Fatalf("a key created after a miss did not become usable within the "+
			"negative TTL (%s): %v", a.NegativeTTL(), err)
	}
	if p.KeyID != "key-new" {
		t.Errorf("key id = %q, want key-new", p.KeyID)
	}
	if got := st.callCount(); got != 2 {
		t.Errorf("store calls = %d, want 2: the re-check after the negative TTL", got)
	}
}

// First use of REAL credentials is not rate limited, however many there are.
//
// This is the property that makes the bound safe to turn on by default, and it
// is the one a naive token bucket would destroy: a node with no bulk snapshot
// resolves every key it serves through the store, and a budget that charged
// those would turn a cold start into a self-inflicted outage. A lookup that
// finds its row returns its token, so the bucket only ever holds the cost of
// lookups that found nothing.
func TestFirstUseOfRealCredentialsIsNotThrottled(t *testing.T) {
	const keys = 5000

	clk := newClock(testNow)
	a, st := budgetAuth(t, clk, nil)
	h := testHasher(t, LegacyPolicy{})
	for i := 0; i < keys; i++ {
		st.put(record(t, h, budgetToken(i), SchemeDorangV1,
			Principal{KeyID: "key-" + strconv.Itoa(i)}))
	}

	ctx := context.Background()
	for i := 0; i < keys; i++ {
		if _, err := a.Authenticate(ctx, budgetToken(i)); err != nil {
			t.Fatalf("real credential %d was refused: %v", i, err)
		}
	}

	if got := a.Stats().LookupsThrottled; got != 0 {
		t.Errorf("throttled = %d, want 0: %d real credentials resolving through the "+
			"store is a cold node, not a flood", got, keys)
	}
	if got := st.callCount(); got != keys {
		t.Errorf("store calls = %d, want %d", got, keys)
	}
}

// The budget refills, so a bound is not a permanent ceiling. An operator who
// revokes a key and whose clients keep retrying it costs a few lookups a second
// forever, not a few lookups and then nothing.
func TestMissBudgetRefills(t *testing.T) {
	clk := newClock(testNow)
	a, st := budgetAuth(t, clk, func(c *Config) {
		c.MissBurst = 2
		c.MissRate = 4 // one token every 250ms
	})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = a.Authenticate(ctx, budgetToken(i))
	}
	if got := st.callCount(); got != 2 {
		t.Fatalf("store calls = %d, want 2 (the burst)", got)
	}

	clk.Advance(250 * time.Millisecond)
	if _, err := a.Authenticate(ctx, budgetToken(100)); ReasonOf(err) != ReasonUnknownKey {
		t.Fatalf("after a refill: reason = %v, want unknown_key", ReasonOf(err))
	}
	if got := st.callCount(); got != 3 {
		t.Errorf("store calls = %d, want 3: the bucket did not refill", got)
	}

	// And it is still capped: a long quiet period does not bank an unbounded
	// burst.
	clk.Advance(time.Hour)
	for i := 200; i < 210; i++ {
		_, _ = a.Authenticate(ctx, budgetToken(i))
	}
	if got := st.callCount(); got != 5 {
		t.Errorf("store calls = %d, want 5: an hour of quiet banked more than the burst", got)
	}
}

// A negative miss rate is the explicit "no bound", and it has to be honoured
// rather than defaulted, for the reason a zero pre-stop delay is.
func TestNegativeMissRateRemovesTheBound(t *testing.T) {
	clk := newClock(testNow)
	a, st := budgetAuth(t, clk, unlimitedMisses)

	ctx := context.Background()
	const n = DefaultMissBurst + 100
	for i := 0; i < n; i++ {
		_, _ = a.Authenticate(ctx, budgetToken(i))
	}
	if got := st.callCount(); got != n {
		t.Errorf("store calls = %d, want %d: MissRate < 0 did not disable the bound", got, n)
	}
	if got := a.Stats().LookupsThrottled; got != 0 {
		t.Errorf("throttled = %d, want 0", got)
	}
}

// asAuthError is errors.As without importing errors into every assertion.
func asAuthError(err error, out **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*out = e
	}
	return ok
}
