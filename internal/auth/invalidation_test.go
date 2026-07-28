package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// DESIGN §11.2c, risk W11. Every assertion here is about whether a key STOPS
// SERVING, never about whether a flag was set: a flag test passes against an
// implementation with a full entry TTL of hole in it, which is precisely the
// defect W11 records.

// loadOne loads exactly one record for a token, so a test can state what the
// stored row says without building a store.
func loadOne(t *testing.T, a *Authenticator, token string, mutate func(*Record)) {
	t.Helper()
	h := testHasher(t, LegacyPolicy{})
	rec := record(t, h, token, SchemeDorangV1, Principal{})
	if mutate != nil {
		mutate(&rec)
	}
	if err := a.Load([]Record{rec}); err != nil {
		t.Fatal(err)
	}
}

// serving reports whether the token authenticates AND authorizes right now.
// Both halves matter: a pend enforced only in Authorize would let a caller who
// skipped authorization through, and a test that called only one of them would
// not notice.
func serving(a *Authenticator, token string) error {
	p, err := a.Authenticate(context.Background(), token)
	if err != nil {
		return err
	}
	return p.Authorize(Access{Now: a.now()})
}

func TestRevocationTakesEffectImmediatelyOnOneNode(t *testing.T) {
	tok := "sk-revoke-now" // pragma: allowlist secret — test fixture
	a := newAuth(t, Config{})
	loadOne(t, a, tok, func(r *Record) { r.Principal.KeyID = "k1" })

	if err := serving(a, tok); err != nil {
		t.Fatalf("the key was not serving to begin with: %v", err)
	}

	start := time.Now()
	if err := a.Announce(context.Background(), Invalidation{KeyID: "k1", Cause: CauseRevoked}); err != nil {
		t.Fatal(err)
	}
	// Measured end to end: the control returns and the very next request is
	// refused. Not "after a tick", not "after the TTL".
	if err := serving(a, tok); err == nil {
		t.Fatal("a revoked key kept serving after the control returned")
	}
	elapsed := time.Since(start)

	b := a.RevocationBound(1, PropagationDelay{})
	if b.Topology != "single-node" || b.Bound != 0 {
		t.Fatalf("the published single-node bound is %s, want 0", b)
	}
	t.Logf("single-node revocation latency: measured %v against a published bound of %v (%s)",
		elapsed, b.Bound, b.Formula)
	if elapsed > time.Second {
		t.Errorf("the control took %v, which is not what 'immediate' means", elapsed)
	}
}

func TestInvalidationByKeyIDDropsEverySecretIncludingUnknownOnes(t *testing.T) {
	// A rotation leaves two live secrets. A revocation that named only the
	// lookups the publisher knew about would leave the other one serving, on a
	// node that learned it independently.
	h := testHasher(t, LegacyPolicy{})
	old := "sk-two-secrets-old" // pragma: allowlist secret — test fixture
	cur := "sk-two-secrets-new" // pragma: allowlist secret — test fixture
	a := newAuth(t, Config{})
	if err := a.Load([]Record{
		record(t, h, old, SchemeDorangV1, Principal{KeyID: "k1", SecretID: "k1.1", SecretGeneration: 1}),
		record(t, h, cur, SchemeDorangV1, Principal{KeyID: "k1", SecretID: "k1.2", SecretGeneration: 2}),
	}); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{old, cur} {
		if err := serving(a, tok); err != nil {
			t.Fatalf("%s was not serving: %v", tok, err)
		}
	}

	// Name only ONE lookup. The key id has to do the rest.
	n := a.Apply(Invalidation{KeyID: "k1", Lookups: []string{LookupKey(old)}, Cause: CauseRevoked})
	if n != 2 {
		t.Errorf("dropped %d entries, want both secrets of the key", n)
	}
	for _, tok := range []string{old, cur} {
		if err := serving(a, tok); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("%s still resolves after its key was revoked: %v", tok, err)
		}
	}
}

func TestARefusingRowIsCachedForTheNegativeLifetimeNotTheServingOne(t *testing.T) {
	// §11.2c rule 3. A key that was refused is cheap to re-check; a key that is
	// serving is not. Treating them alike is what made the window large.
	tok := "sk-negative-ttl" // pragma: allowlist secret — test fixture
	h := testHasher(t, LegacyPolicy{})
	now := testNow
	st := &fakeStore{rows: map[string]Record{}}
	pended := record(t, h, tok, SchemeDorangV1, Principal{KeyID: "k1"})
	pended.Principal.Key.Pended = true
	st.rows[LookupKey(tok)] = pended

	a := newAuth(t, Config{
		Store:       st,
		EntryTTL:    time.Hour,
		NegativeTTL: 2 * time.Second,
		Now:         func() time.Time { return now },
	})

	if err := serving(a, tok); !errors.Is(err, ErrPended) {
		t.Fatalf("a pended key was refused with %v, want a distinct pended refusal", err)
	}
	before := st.callCount()

	// Inside the negative lifetime: still cached, no second store call.
	now = now.Add(time.Second)
	_ = serving(a, tok)
	if st.callCount() != before {
		t.Error("a refusal inside the negative lifetime went back to the store")
	}

	// Past the negative lifetime and far inside the entry lifetime: re-read.
	now = now.Add(2 * time.Second)
	released := record(t, h, tok, SchemeDorangV1, Principal{KeyID: "k1"})
	st.rows[LookupKey(tok)] = released
	if err := serving(a, tok); err != nil {
		t.Fatalf("a released key did not resume serving after the negative lifetime: %v", err)
	}
	if st.callCount() == before {
		t.Error("the refusal was held for the ENTRY lifetime; rule 3 is not implemented")
	}
	if got := a.Stats().NegativeCached; got == 0 {
		t.Error("nothing counted the found-but-refusing row as a negative entry")
	}
}

func TestNegativeTTLLongerThanEntryTTLIsRefused(t *testing.T) {
	_, err := New(Config{
		Pepper: testPepper, MasterKey: testMaster,
		EntryTTL: time.Second, NegativeTTL: time.Minute,
	})
	if !errors.Is(err, ErrNegativeTTLTooLong) {
		t.Fatalf("a configuration that holds refusals longer than serving rows was accepted: %v", err)
	}
}

func TestAnnounceAppliesLocallyEvenWhenPublishingFails(t *testing.T) {
	// The local drop happens first and unconditionally: a publish that fails
	// must leave THIS node correct rather than every node wrong, and the error
	// must say the fleet is converging on the TTL.
	tok := "sk-publish-fails" // pragma: allowlist secret — test fixture
	sink := &failingSink{}
	a := newAuth(t, Config{Sink: sink})
	loadOne(t, a, tok, func(r *Record) { r.Principal.KeyID = "k1" })

	err := a.Announce(context.Background(), Invalidation{KeyID: "k1", Cause: CauseRevoked})
	if err == nil {
		t.Fatal("a failed publish was reported as success")
	}
	if serving(a, tok) == nil {
		t.Fatal("the local drop did not happen when the publish failed")
	}
	if a.Stats().PublishFailed != 1 {
		t.Error("a failed publish was not counted")
	}
}

func TestMemBusFansAnInvalidationOutToEveryNode(t *testing.T) {
	tok := "sk-fan-out" // pragma: allowlist secret — test fixture
	bus := NewMemBus()
	nodes := make([]*Authenticator, 4)
	for i := range nodes {
		nodes[i] = newAuth(t, Config{Sink: bus})
		loadOne(t, nodes[i], tok, func(r *Record) { r.Principal.KeyID = "k1" })
	}
	bus.Subscribe(nodes...)

	for i, n := range nodes {
		if err := serving(n, tok); err != nil {
			t.Fatalf("node %d was not serving: %v", i, err)
		}
	}
	if err := nodes[0].Announce(context.Background(), Invalidation{KeyID: "k1", Cause: CausePended}); err != nil {
		t.Fatal(err)
	}
	for i, n := range nodes {
		if serving(n, tok) == nil {
			t.Errorf("node %d kept serving a pended key", i)
		}
	}
	if got := len(bus.Published()); got != 1 {
		t.Errorf("the bus carried %d messages, want 1", got)
	}
}

func TestRejoinReloadsRatherThanTrustingAStaleSnapshot(t *testing.T) {
	// §11.2c rule 4. A node that was down must not serve from what it had.
	tok := "sk-rejoin" // pragma: allowlist secret — test fixture
	a := newAuth(t, Config{})
	loadOne(t, a, tok, func(r *Record) { r.Principal.KeyID = "k1" })
	if err := serving(a, tok); err != nil {
		t.Fatal(err)
	}

	// While this node was away, the key was blocked.
	h := testHasher(t, LegacyPolicy{})
	blocked := record(t, h, tok, SchemeDorangV1, Principal{KeyID: "k1"})
	blocked.Principal.Key.Blocked = true
	loader := &fakeLoader{recs: []Record{blocked}}

	if err := a.Rejoin(context.Background(), loader); err != nil {
		t.Fatal(err)
	}
	if serving(a, tok) == nil {
		t.Fatal("a rejoining node kept serving a key that was blocked while it was away")
	}
	if a.Stats().Rejoins != 1 {
		t.Error("the rejoin was not counted")
	}
}

func TestRejoinDropsTheStaleSnapshotEvenWhenTheReloadFails(t *testing.T) {
	// Dropping BEFORE the load is the whole point: a load that fails must not
	// leave the stale set serving.
	tok := "sk-rejoin-fails" // pragma: allowlist secret — test fixture
	a := newAuth(t, Config{})
	loadOne(t, a, tok, func(r *Record) { r.Principal.KeyID = "k1" })

	err := a.Rejoin(context.Background(), &fakeLoader{err: errors.New("store is down")})
	if err == nil {
		t.Fatal("a failed reload was reported as success")
	}
	if serving(a, tok) == nil {
		t.Fatal("a node whose reload failed kept serving from a snapshot it knows is stale")
	}
}

func TestPendIsADistinctReversibleRefusal(t *testing.T) {
	tok := "sk-pend-distinct" // pragma: allowlist secret — test fixture
	a := newAuth(t, Config{})
	loadOne(t, a, tok, func(r *Record) {
		r.Principal.KeyID = "k1"
		r.Principal.Key.Pended = true
		r.Principal.Key.PendReason = "token guard"
	})
	err := serving(a, tok)
	if !errors.Is(err, ErrPended) {
		t.Fatalf("a pended key was refused with %v, want ErrPended", err)
	}
	if errors.Is(err, ErrBlocked) {
		t.Error("a pend is indistinguishable from a block; an operator cannot tell an outage from a policy")
	}
	var e *Error
	if !errors.As(err, &e) || e.Code() != "credential_pended" {
		t.Errorf("the wire code is %q, want a documented distinct code", e.Code())
	}
	if e.Status() != 403 {
		t.Errorf("a pend answered %d; 401 would tell the caller to check a key that is fine", e.Status())
	}
}

func TestARetiredSecretIsRefusedAsASecretAndNotAsAnExpiredKey(t *testing.T) {
	// The fix for a retired secret is "use the secret you were issued", not
	// "ask for a new key". Reporting it as an expired key sends the caller to
	// re-provisioning, which is what rotation exists to avoid.
	tok := "sk-retired-secret" // pragma: allowlist secret — test fixture
	a := newAuth(t, Config{})
	loadOne(t, a, tok, func(r *Record) {
		r.Principal.KeyID = "k1"
		r.Principal.SecretID = "k1.1"
		r.Principal.SecretExpiresAt = testNow.Add(-time.Minute)
	})
	err := serving(a, tok)
	if !errors.Is(err, ErrSecretRetired) {
		t.Fatalf("a retired secret was refused with %v", err)
	}
	if errors.Is(err, ErrExpired) {
		t.Error("a retired secret is reported as an expired key")
	}
}

func TestPublishedBoundIsANumberAndNotAClaim(t *testing.T) {
	bus := NewMemBus()
	a := newAuth(t, Config{Sink: bus, EntryTTL: time.Minute, NegativeTTL: 5 * time.Second,
		ReloadInterval: 30 * time.Second})

	single := a.RevocationBound(1, PropagationDelay{Delay: time.Second})
	if single.Bound != 0 || single.Formula == "" || single.Why == "" {
		t.Errorf("the single-node figure is not a published number: %+v", single)
	}

	clustered := a.RevocationBound(6, PropagationDelay{Delay: 1250 * time.Millisecond, Source: "poll+store"})
	if clustered.Topology != "clustered" || clustered.Nodes != 6 {
		t.Errorf("clustered figure describes the wrong deployment: %+v", clustered)
	}
	if clustered.Bound != 1250*time.Millisecond {
		t.Errorf("clustered bound = %v, want the transport's delivery time", clustered.Bound)
	}
	if clustered.Fallback != time.Minute {
		t.Errorf("the fallback is %v, want the entry TTL", clustered.Fallback)
	}
	// A row placed by a bulk load does NOT expire (§9.1), so the entry TTL is
	// not its fallback and reporting it as one would be false. Its fallback is
	// the reload interval.
	if clustered.SnapshotFallback != 30*time.Second {
		t.Errorf("the snapshot fallback is %v, want the reload interval", clustered.SnapshotFallback)
	}
	if clustered.Negative != 5*time.Second {
		t.Errorf("the negative figure is %v, want the negative TTL", clustered.Negative)
	}
	if clustered.Formula == "" || clustered.Why == "" {
		t.Error(`"revocation is fast" is not a specification: the figure carries no arithmetic`)
	}

	// An unmeasured transport must not report a small number.
	unmeasured := a.RevocationBound(6, PropagationDelay{})
	if unmeasured.Bound != time.Minute {
		t.Errorf("an unmeasured propagation delay published %v; an unmeasured delay is not a small one",
			unmeasured.Bound)
	}
}

func TestASnapshotRowHasNoTTLFallbackAndSaysSo(t *testing.T) {
	// The hole this test exists to close: Load'ed rows never expire, so a node
	// that missed an invalidation would serve a revoked key FOREVER, not for an
	// entry TTL. Publishing the entry TTL as their fallback would have been a
	// number that is simply wrong.
	tok := "sk-snapshot-fallback" // pragma: allowlist secret — test fixture
	now := testNow
	a := newAuth(t, Config{
		Sink: NewMemBus(), EntryTTL: time.Minute, Now: func() time.Time { return now },
	})
	loadOne(t, a, tok, func(r *Record) { r.Principal.KeyID = "k1" })

	now = now.Add(24 * time.Hour) // far past any TTL
	if err := serving(a, tok); err != nil {
		t.Fatalf("a Load'ed row expired, which §9.1 says it must not: %v", err)
	}
	// So with no reload scheduled the honest figure is "never", not the TTL.
	b := a.RevocationBound(4, PropagationDelay{Delay: time.Second, Source: "bus"})
	if b.SnapshotFallback != 0 {
		t.Errorf("SnapshotFallback = %v with no reload scheduled, want 0 meaning never",
			b.SnapshotFallback)
	}
	if !strings.Contains(b.String(), "never") {
		t.Errorf("the rendered figure does not say the snapshot has no fallback: %s", b)
	}

	// Refresh is what gives them one, and it corrects a missed invalidation
	// without dropping the snapshot first.
	h := testHasher(t, LegacyPolicy{})
	blocked := record(t, h, tok, SchemeDorangV1, Principal{KeyID: "k1"})
	blocked.Principal.Key.Blocked = true
	if err := a.Refresh(context.Background(), &fakeLoader{recs: []Record{blocked}}); err != nil {
		t.Fatal(err)
	}
	if serving(a, tok) == nil {
		t.Fatal("a refresh did not correct a missed invalidation")
	}
	if a.Stats().Refreshes != 1 {
		t.Error("the refresh was not counted; a node whose refreshes stopped has no fallback at all")
	}
}

func TestRefreshDoesNotDropTheSnapshotBeforeLoading(t *testing.T) {
	// Rejoin drops first, because a node that was down must not serve what it
	// had. Refresh must NOT, or every interval would send every key to the
	// store in the gap — a stampede on a schedule, to fix nothing.
	tok := "sk-refresh-no-gap" // pragma: allowlist secret — test fixture
	a := newAuth(t, Config{})
	loadOne(t, a, tok, func(r *Record) { r.Principal.KeyID = "k1" })

	before := a.Stats().SnapshotSize
	if err := a.Refresh(context.Background(), &fakeLoader{err: errors.New("store is down")}); err == nil {
		t.Fatal("a failed refresh was reported as success")
	}
	if a.Stats().SnapshotSize != before {
		t.Fatal("a failed refresh emptied the snapshot; every key would go to a store that is down")
	}
	if err := serving(a, tok); err != nil {
		t.Fatalf("the key stopped serving because a refresh failed: %v", err)
	}
}

func TestApplyIsIdempotentUnderConcurrency(t *testing.T) {
	tok := "sk-idempotent" // pragma: allowlist secret — test fixture
	a := newAuth(t, Config{})
	loadOne(t, a, tok, func(r *Record) { r.Principal.KeyID = "k1" })

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 32; j++ {
				a.Apply(Invalidation{KeyID: "k1", Cause: CauseRevoked})
				_ = serving(a, tok)
			}
		}()
	}
	wg.Wait()
	if serving(a, tok) == nil {
		t.Fatal("the key is serving after being invalidated many times")
	}
}

// --- doubles -----------------------------------------------------------------

type failingSink struct{}

func (failingSink) Publish(context.Context, Invalidation) error {
	return errors.New("the bus is unreachable")
}

type fakeLoader struct {
	recs []Record
	err  error
}

func (f *fakeLoader) LoadAll(context.Context) ([]Record, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.recs, nil
}
