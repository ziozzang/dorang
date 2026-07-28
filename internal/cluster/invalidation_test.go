package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/store"
)

// DESIGN §11.2c, risk W11 — revocation latency, measured end to end.
//
// Every assertion here is that a key STOPS SERVING, and the figure reported is
// the wall-clock time from the control returning to the last node refusing. A
// test that asserted a row had been written would pass against the very defect
// W11 records.

const (
	invPepper = "cluster-invalidation-test-pepper" // pragma: allowlist secret — test fixture
	invMaster = "sk-cluster-inv-master"            // pragma: allowlist secret — test fixture
)

// node is one gateway process: its own store handle, its own auth snapshot, its
// own invalidator. Nothing is shared but the database, which is what a cluster
// actually is.
type node struct {
	st   *store.Store
	a    *auth.Authenticator
	inv  *KeyInvalidator
	ctrl *KeyControl
}

func newInvNode(t *testing.T, st *store.Store, id string, now func() time.Time, poll time.Duration) *node {
	t.Helper()
	inv0, err := NewKeyInvalidator(InvalidatorConfig{Store: st, NodeID: id, Poll: poll, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(auth.Config{
		Pepper: invPepper, MasterKey: invMaster, Now: now, Store: &authStore{st: st},
		Sink: inv0,
		// An hour. If a revocation were relying on the TTL, this test would
		// have to wait an hour to pass — which is the point of choosing it.
		EntryTTL: time.Hour, NegativeTTL: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	inv0.Attach(a)
	t.Cleanup(inv0.Close)

	ctrl, err := NewKeyControl(st, a, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	return &node{st: st, a: a, inv: inv0, ctrl: ctrl}
}

// authStore adapts the store to auth.Store for the test's nodes. It is the same
// shape internal/app uses, kept here so this package's tests do not depend on
// that one.
type authStore struct{ st *store.Store }

func (s *authStore) LoadByLookup(ctx context.Context, lookup string) (auth.Record, error) {
	k, sec, err := s.st.ResolveKeyByLookup(ctx, lookup)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return auth.Record{}, auth.ErrNotFound
		}
		return auth.Record{}, err
	}
	return AuthRecord(k, sec, nil)
}

func newInvKey(t *testing.T, st *store.Store, token string) *store.APIKey {
	t.Helper()
	k := &store.APIKey{ID: store.NewID(), Tier: "commercial", SpendNano: 42}
	if err := st.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAPIKey(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	return k
}

func servingAt(a *auth.Authenticator, token string, now time.Time) error {
	p, err := a.Authenticate(context.Background(), token)
	if err != nil {
		return err
	}
	return p.Authorize(auth.Access{Now: now})
}

func openPepperedStore(t *testing.T, b backend, dsn string, now func() time.Time) *store.Store {
	t.Helper()
	cfg := store.Config{Driver: b.dialect, DSN: dsn, Pepper: []byte(invPepper), Now: now}
	s, err := store.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", b.name, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func eachPepperedCluster(t *testing.T, n int, fn func(t *testing.T, stores []*store.Store, clk *clock)) {
	t.Helper()
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			clk := newClock(epoch)
			dsn := b.env(t)
			stores := make([]*store.Store, n)
			for i := range stores {
				stores[i] = openPepperedStore(t, b, dsn, clk.Now)
			}
			fn(t, stores, clk)
		})
	}
}

// --- single node --------------------------------------------------------------

func TestRevocationLatencySingleNode(t *testing.T) {
	eachPepperedCluster(t, 1, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		tok := "sk-cluster-single" // pragma: allowlist secret — test fixture
		k := newInvKey(t, stores[0], tok)
		n := newInvNode(t, stores[0], "node-1", clk.Now, time.Millisecond)

		if err := servingAt(n.a, tok, clk.Now()); err != nil {
			t.Fatalf("the key was not serving: %v", err)
		}

		// Two figures, because they are two different things. The control's own
		// duration is a durable write the operator paid for and watched; the
		// propagation is what the published bound is about.
		start := time.Now()
		if err := n.ctrl.Revoke(ctx, k.ID); err != nil {
			t.Fatal(err)
		}
		control := time.Since(start)

		// A published bound of ZERO is a claim about a WINDOW, not about
		// latency: the very first request after the control returns is already
		// refused, so there is no interval in which the key is admitted. That
		// is what is asserted. The time that request takes to answer is a
		// separate number — it is a store read, because the invalidation
		// dropped the cached copy — and it is reported rather than folded in,
		// because a bound that included it would be measuring the database.
		afterControl := time.Now()
		if servingAt(n.a, tok, clk.Now()) == nil {
			t.Fatal("a revoked key kept serving after the control returned: the window is not zero")
		}
		refusalLatency := time.Since(afterControl)

		bound := n.inv.Bound(1)
		t.Logf("REVOCATION LATENCY single-node: window %v against a published bound of %v (%s) — "+
			"the first request after the control was already refused; that refusal took %v to answer "+
			"(one store read) and the control's own durable write took %v; "+
			"TTL fallback %v; negative-entry re-check %v",
			time.Duration(0), bound.Bound, bound.Formula, refusalLatency, control,
			bound.Fallback, bound.Negative)
		if bound.Topology != "single-node" || bound.Bound != 0 {
			t.Errorf("the single-node bound is %s, want 0", bound)
		}

		// The same claim under concurrency, which is where a zero window is
		// actually worth asserting: nothing admitted between the control being
		// applied and the requests that follow it.
		const attempts = 200
		for i := 0; i < attempts; i++ {
			if servingAt(n.a, tok, clk.Now()) == nil {
				t.Fatalf("request %d after the revocation was admitted", i)
			}
		}
	})
}

// --- clustered ----------------------------------------------------------------

func TestRevocationLatencyClustered(t *testing.T) {
	const nodes = 4
	eachPepperedCluster(t, nodes, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		tok := "sk-cluster-many" // pragma: allowlist secret — test fixture
		k := newInvKey(t, stores[0], tok)

		// A real poll interval, and the loops actually running: the measured
		// figure has to include the wait, not skip it by calling Poll().
		poll := 20 * time.Millisecond
		ns := make([]*node, nodes)
		for i := range ns {
			ns[i] = newInvNode(t, stores[i], "node-"+string(rune('1'+i)), clk.Now, poll)
			if err := ns[i].inv.Start(ctx); err != nil {
				t.Fatal(err)
			}
		}

		// Every node is serving from its OWN snapshot, learned independently.
		for i, n := range ns {
			if err := servingAt(n.a, tok, clk.Now()); err != nil {
				t.Fatalf("node %d was not serving: %v", i, err)
			}
		}

		bound := ns[0].inv.Bound(nodes)
		called := time.Now()
		if err := ns[0].ctrl.Revoke(ctx, k.ID); err != nil {
			t.Fatal(err)
		}
		control := time.Since(called)
		start := time.Now()
		// The publishing node is immediate — Announce applies locally before it
		// returns — which is worth asserting separately, because a control whose
		// own node has a window is a control the operator cannot verify.
		if servingAt(ns[0].a, tok, clk.Now()) == nil {
			t.Fatal("the publishing node kept serving")
		}

		deadline := time.Now().Add(10 * time.Second)
		var last time.Duration
		for i := 1; i < len(ns); i++ {
			for {
				if servingAt(ns[i].a, tok, clk.Now()) != nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("node %d was still serving a revoked key after %v", i, time.Since(start))
				}
				time.Sleep(time.Millisecond)
			}
			if d := time.Since(start); d > last {
				last = d
			}
		}

		t.Logf("REVOCATION LATENCY clustered (%d nodes, poll %v): propagation to the LAST node %v "+
			"against a published bound of %v (%s); the control's own durable write took %v; "+
			"TTL fallback %v; negative-entry re-check %v",
			nodes, poll, last, bound.Bound, bound.Formula, control, bound.Fallback, bound.Negative)

		if bound.Topology != "clustered" {
			t.Fatalf("the published figure describes %s", bound.Topology)
		}
		if last > bound.Bound {
			t.Errorf("measured %v, which exceeds the published bound of %v (%s)",
				last, bound.Bound, bound.Formula)
		}
		// And the bound must be far below the TTL, or the invalidation path is
		// decoration on top of the thing it was built to replace.
		if bound.Bound >= bound.Fallback {
			t.Errorf("the published bound %v is not better than the TTL fallback %v",
				bound.Bound, bound.Fallback)
		}
	})
}

func TestPendAndGraceCutPropagateOnTheSamePath(t *testing.T) {
	const nodes = 3
	eachPepperedCluster(t, nodes, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		old := "sk-cluster-rot-1" // pragma: allowlist secret — test fixture
		neu := "sk-cluster-rot-2" // pragma: allowlist secret — test fixture
		k := newInvKey(t, stores[0], old)

		ns := make([]*node, nodes)
		for i := range ns {
			ns[i] = newInvNode(t, stores[i], "node-"+string(rune('1'+i)), clk.Now, time.Hour)
		}

		hash, err := store.HashDorangV1([]byte(invPepper), neu)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ns[0].ctrl.Rotate(ctx, k.ID, store.KeySecret{
			Lookup: store.KeyLookup(neu), TokenHash: hash, HashScheme: store.SchemeDorangV1,
		}, store.RotationPolicy{Grace: 30 * 24 * time.Hour}); err != nil {
			t.Fatal(err)
		}

		// Every node serves both secrets during the grace period.
		for i, n := range ns {
			for _, tok := range []string{old, neu} {
				if err := servingAt(n.a, tok, clk.Now()); err != nil {
					t.Fatalf("node %d refused %s during the grace period: %v", i, tok, err)
				}
			}
		}

		// An early cut, then one deterministic poll per node — the mechanism,
		// not the TTL.
		if _, err := ns[0].ctrl.CutGrace(ctx, k.ID); err != nil {
			t.Fatal(err)
		}
		for i, n := range ns {
			if i > 0 {
				if _, err := n.inv.Poll(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := servingAt(n.a, old, clk.Now()); !errors.Is(err, auth.ErrSecretRetired) {
				t.Errorf("node %d still serves the cut secret: %v", i, err)
			}
			if err := servingAt(n.a, neu, clk.Now()); err != nil {
				t.Errorf("node %d stopped serving the CURRENT secret after a grace cut: %v", i, err)
			}
		}

		// A pend travels the same path and is reversible on every node.
		if err := ns[0].ctrl.Pend(ctx, k.ID, "token guard"); err != nil {
			t.Fatal(err)
		}
		for i, n := range ns {
			if i > 0 {
				if _, err := n.inv.Poll(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := servingAt(n.a, neu, clk.Now()); !errors.Is(err, auth.ErrPended) {
				t.Errorf("node %d serves a pended key: %v", i, err)
			}
		}
		if err := ns[0].ctrl.Release(ctx, k.ID); err != nil {
			t.Fatal(err)
		}
		for i, n := range ns {
			if i > 0 {
				if _, err := n.inv.Poll(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := servingAt(n.a, neu, clk.Now()); err != nil {
				t.Errorf("node %d still refuses a released key: %v", i, err)
			}
		}
	})
}

func TestANodeThatWasDownReloadsOnRejoin(t *testing.T) {
	// §11.2c rule 4. The absent node must not serve what it had, and it must
	// not miss a message published while it was reloading either.
	eachPepperedCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		tok := "sk-cluster-rejoin" // pragma: allowlist secret — test fixture
		k := newInvKey(t, stores[0], tok)

		live := newInvNode(t, stores[0], "live", clk.Now, time.Hour)
		away := newInvNode(t, stores[1], "away", clk.Now, time.Hour)

		if err := servingAt(away.a, tok, clk.Now()); err != nil {
			t.Fatal(err)
		}

		// While `away` is not polling, the key is pended elsewhere. Its cached
		// entry is an hour from expiring.
		if err := live.ctrl.Pend(ctx, k.ID, "compromised"); err != nil {
			t.Fatal(err)
		}
		if err := servingAt(away.a, tok, clk.Now()); err == nil {
			// It is allowed to still be serving here — that is the window the
			// mechanism exists to close — but it must not survive the rejoin.
			t.Log("the absent node is still serving, which is the window rule 4 closes")
		}

		if err := away.inv.Rejoin(ctx, NewKeyLoader(stores[1], 0, nil)); err != nil {
			t.Fatal(err)
		}
		if err := servingAt(away.a, tok, clk.Now()); !errors.Is(err, auth.ErrPended) {
			t.Fatalf("a rejoining node is serving a key that was pended while it was away: %v", err)
		}
		// And the watermark is at the head, so it does not replay everything on
		// its first poll.
		head, err := stores[1].LatestInvalidationSeq(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if away.inv.Seq() != head {
			t.Errorf("the rejoined watermark is %d, want the head %d", away.inv.Seq(), head)
		}
	})
}

func TestTheRejoinLoaderCarriesEverySecretOfAKey(t *testing.T) {
	eachPepperedCluster(t, 1, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		old := "sk-loader-1" // pragma: allowlist secret — test fixture
		neu := "sk-loader-2" // pragma: allowlist secret — test fixture
		k := newInvKey(t, stores[0], old)
		hash, err := store.HashDorangV1([]byte(invPepper), neu)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stores[0].RotateKey(ctx, k.ID, store.KeySecret{
			Lookup: store.KeyLookup(neu), TokenHash: hash, HashScheme: store.SchemeDorangV1,
		}, store.RotationPolicy{Grace: time.Hour}); err != nil {
			t.Fatal(err)
		}

		recs, err := NewKeyLoader(stores[0], 0, auth.DefaultTiers()).LoadAll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 2 {
			t.Fatalf("the loader returned %d records for a key in a grace period, want one per secret", len(recs))
		}
		gens := map[int]bool{}
		for _, r := range recs {
			if r.Principal.KeyID != k.ID {
				t.Errorf("a record belongs to key %s", r.Principal.KeyID)
			}
			if r.Principal.SecretID == "" {
				t.Error("a record does not say which secret it is")
			}
			gens[r.Principal.SecretGeneration] = true
		}
		if !gens[1] || !gens[2] {
			t.Errorf("generations present: %v, want both", gens)
		}
	})
}

func TestAuthRecordAppliesTheTierAndNarrowsNothingUpward(t *testing.T) {
	// A tier that only decided a default somewhere else would be a tier the
	// request path never sees. The conversion is where it has to be applied.
	tiers, err := auth.NewTierSet([]auth.Tier{
		{Name: "free", Rank: 0, PriorityClass: "batch", RPMLimit: auth.Limit(10)},
		{Name: "commercial", Rank: 1, PriorityClass: "interactive", RPMLimit: auth.Limit(600)},
	}, "free")
	if err != nil {
		t.Fatal(err)
	}
	k := &store.APIKey{
		ID: "k1", Tier: "free", PriorityClass: "realtime",
		RPMLimit: func() *int64 { v := int64(100_000); return &v }(),
	}
	sec := &store.KeySecret{
		ID: "k1.1", KeyID: "k1", Generation: 1, Lookup: "0123456789abcdef0123456789abcdef",
		TokenHash:  "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		HashScheme: store.SchemeDorangV1,
	}
	rec, err := AuthRecord(k, sec, tiers)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Principal.Key.RPMLimit == nil || *rec.Principal.Key.RPMLimit != 10 {
		t.Errorf("rpm = %v, want the free tier's ceiling", rec.Principal.Key.RPMLimit)
	}
	if rec.Principal.PriorityClass != "batch" {
		t.Errorf("a free-tier key claiming realtime is scheduled as %q", rec.Principal.PriorityClass)
	}
	if rec.Principal.Tier != "free" {
		t.Errorf("tier = %q", rec.Principal.Tier)
	}

	// A row naming a tier the configuration no longer has is an error, not a
	// silent move to whichever tier the default happens to be.
	k.Tier = "platinum"
	if _, err := AuthRecord(k, sec, tiers); err == nil {
		t.Error("a key naming an unconfigured tier was resolved silently")
	}
}
