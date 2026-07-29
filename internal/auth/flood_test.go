package auth

import (
	"context"
	"testing"
)

// An unauthenticated flood of unknown keys must not make every miss cost a full
// snapshot copy.
//
// The mechanism was written and did not fire. insert folded the overlay once it
// held mergeThreshold entries, and mergeLocked promoted only POSITIVE entries,
// handing the negatives straight back as the new overlay. A flood produces only
// negatives, so after the 64th unknown key len(overlay) never fell below 64
// again and the fold ran on essentially every subsequent miss — each one an
// O(loaded keys) map allocation and copy, taken with a.mu held exclusively,
// with every legitimate authentication miss queued behind it.
//
// This test fails against that code: with 2000 distinct unknown keys it
// observed on the order of two thousand merges. It passes now because the
// threshold counts promotable entries, of which a flood produces none.
func TestUnknownKeyFloodDoesNotMergeOnEveryMiss(t *testing.T) {
	const floods = 2000

	// The miss budget is off here on purpose. It bounds the flood in a
	// different place — the store round trip — and leaving it on would hide
	// whether the merge threshold still does its own job for the misses that
	// DO get through.
	a := newFloodAuth(t, 500, unlimitedMisses)
	before := a.Stats().Merges

	ctx := context.Background()
	for i := 0; i < floods; i++ {
		// Distinct every time: the negative cache cannot coalesce these, which
		// is the point of the attack.
		_, _ = a.Authenticate(ctx, unknownToken(i))
	}

	merges := a.Stats().Merges - before
	// A handful is fine — the overlay cap still fires, and a legitimate key
	// learned alongside the flood may promote. Anything proportional to the
	// flood is the defect.
	if merges > floods/50 {
		t.Errorf("%d snapshot merges for %d unknown keys: a flood of unknown keys is "+
			"driving an O(loaded keys) copy per request", merges, floods)
	}
}

// The negative entries a flood produces are bounded on their own, and bounding
// them does not throw away the real credentials learned beside them.
func TestNegativeFloodDoesNotEvictLearnedKeys(t *testing.T) {
	// Unbounded misses, so that maxNegative is actually reached: with the miss
	// budget on, a flood is cut off long before the negative cap fires and this
	// test would assert nothing. The bound on the round trips is
	// TestUnknownKeyFloodIsBoundedInStoreRoundTrips; this one is about what
	// happens to the positives when the negative cap DOES fire.
	a := newFloodAuth(t, 0, unlimitedMisses)
	ctx := context.Background()

	// One real key, learned at runtime through the store.
	if _, err := a.Authenticate(ctx, floodKnownToken); err != nil {
		t.Fatalf("the known key does not authenticate: %v", err)
	}

	// Well past maxNegative, so the negative cap fires more than once.
	for i := 0; i < maxNegative*3; i++ {
		_, _ = a.Authenticate(ctx, unknownToken(i))
	}
	afterFlood := a.Stats().StoreCalls

	// The real key is still cached: authenticating it again costs no new store
	// call. If the flood had dropped the overlay wholesale, it would.
	if _, err := a.Authenticate(ctx, floodKnownToken); err != nil {
		t.Fatalf("the known key stopped authenticating after a flood: %v", err)
	}
	if got := a.Stats().StoreCalls; got != afterFlood {
		t.Errorf("the known key cost %d new store call(s) after the flood: the flood "+
			"evicted a legitimate credential and forced it to be re-read",
			got-afterFlood)
	}
}

const floodKnownToken = "sk-flood-known-key-0000000000" // pragma: allowlist secret — fabricated

// unlimitedMisses removes the unknown-key lookup budget, for the tests that are
// measuring one of the other bounds and need the flood to actually reach it.
func unlimitedMisses(c *Config) { c.MissRate = -1 }

// newFloodAuth builds an authenticator holding n loaded keys plus one the store
// can supply on demand.
func newFloodAuth(t *testing.T, loaded int, opts ...func(*Config)) *Authenticator {
	t.Helper()
	h, err := NewHasher("pepper-for-the-flood-test", LegacyPolicy{})
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	st := &floodStore{h: h}
	cfg := Config{
		Pepper:      "pepper-for-the-flood-test",
		NoMasterKey: true,
		Store:       st,
	}
	for _, o := range opts {
		o(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if loaded > 0 {
		recs := make([]Record, 0, loaded)
		for i := 0; i < loaded; i++ {
			recs = append(recs, st.record(loadedToken(i)))
		}
		if err := a.Load(recs); err != nil {
			t.Fatalf("Load: %v", err)
		}
	}
	return a
}

func loadedToken(i int) string  { return "sk-loaded-" + pad(i) }
func unknownToken(i int) string { return "sk-unknown-" + pad(i) }

func pad(i int) string {
	b := []byte("0000000000")
	for p := len(b) - 1; p >= 0 && i > 0; p-- {
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b)
}

// floodStore knows exactly one key and answers ErrNotFound for everything else.
type floodStore struct{ h *Hasher }

func (s *floodStore) record(token string) Record {
	sum, mac := s.h.digests(token)
	var l Lookup
	copy(l[:], sum[:LookupBytes])
	return Record{
		Lookup: l.Hex(), Digest: mac, Scheme: SchemeDorangV1,
		Principal: Principal{KeyID: "key-" + l.Hex()[:8]},
	}
}

func (s *floodStore) LoadByLookup(_ context.Context, lookup string) (Record, error) {
	if rec := s.record(floodKnownToken); rec.Lookup == lookup {
		return rec, nil
	}
	return Record{}, ErrNotFound
}
