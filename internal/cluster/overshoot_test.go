package cluster

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

const (
	// overshootNodes is small enough to run on SQLite in a second and large
	// enough that "nodes - 1" is not 1, which is where an off-by-one in a
	// published formula would hide.
	overshootNodes = 4
	overshootLimit = 64
	overshootBlock = 8
)

// TestPublishedOvershootIsNeverExceeded is the gate DESIGN 5.6 asks for:
// "Every mode publishes its maximum possible overshoot as a number.
// 'Approximately accurate' is not an acceptable specification."
//
// The measurement is deliberately adversarial. Every node charges against the
// same limit at once, far more times than the limit allows, so the modes that
// are supposed to be exact have every opportunity to admit one unit too many
// and the mode that is supposed to overshoot does so all the way to its bound.
// A test that charged politely would pass against a broken shared counter.
func TestPublishedOvershootIsNeverExceeded(t *testing.T) {
	eachCluster(t, overshootNodes, func(t *testing.T, stores []*store.Store, clk *clock) {
		for _, mode := range Modes() {
			t.Run(mode.String(), func(t *testing.T) {
				// local is only permitted unclustered; that it is refused in a
				// cluster is TestGuardRefusesClusteredLocal's business, not
				// this test's. Here we measure what it actually does.
				p := Params{
					Limit: overshootLimit, Nodes: overshootNodes,
					Block: overshootBlock, Clustered: mode != ModeLocal,
				}
				pub, err := Publish(mode, p)
				if err != nil {
					t.Fatalf("Publish(%s): %v", mode, err)
				}
				if pub.Formula == "" || pub.Why == "" {
					t.Fatalf("%s published a bare number with no arithmetic behind it", mode)
				}

				coords := coordinators(t, mode, stores, clk, p)
				key := quota.NewKey("credential", "cred-"+mode.String(), quota.Rolling(time.Hour),
					quota.MetricRequests, clk.Now())

				admitted := chargeConcurrently(t, coords, key, overshootLimit)
				overshoot := admitted - overshootLimit
				if overshoot < 0 {
					overshoot = 0
				}

				t.Logf("%s: published max overshoot %d (%s); measured %d (admitted %d against a limit of %d)",
					mode, pub.MaxOvershoot, pub.Formula, overshoot, admitted, overshootLimit)

				if overshoot > pub.MaxOvershoot {
					t.Fatalf("%s admitted %d against a limit of %d: overshoot %d exceeds the published %d",
						mode, admitted, overshootLimit, overshoot, pub.MaxOvershoot)
				}
				// A mode that publishes zero and admits fewer than the limit is
				// not exact, it is merely safe -- and an operator who sized a
				// cluster on that figure would be short of capacity.
				if pub.MaxOvershoot == 0 && admitted != overshootLimit {
					t.Fatalf("%s publishes an exact figure but admitted %d of %d", mode, admitted, overshootLimit)
				}
			})
		}
	})
}

// chargeConcurrently runs every coordinator flat out against one key and
// returns how many units were admitted in total.
func chargeConcurrently(t *testing.T, coords []quota.Coordinator, key quota.Key, limit int64) int64 {
	t.Helper()
	ctx := context.Background()
	var admitted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for _, c := range coords {
		wg.Add(1)
		go func(c quota.Coordinator) {
			defer wg.Done()
			<-start
			for i := int64(0); i < limit; i++ {
				g, err := c.Charge(ctx, key, limit, 1, key.PeriodStart)
				if err != nil {
					t.Errorf("Charge: %v", err)
					return
				}
				if g.OK {
					admitted.Add(1)
				}
			}
		}(c)
	}
	close(start)
	wg.Wait()
	return admitted.Load()
}

// coordinators builds one coordinator per simulated node, all sharing whatever
// authority the mode coordinates through.
func coordinators(t *testing.T, mode Mode, stores []*store.Store, clk *clock, p Params) []quota.Coordinator {
	t.Helper()
	n := len(stores)
	out := make([]quota.Coordinator, 0, n)

	// One authority for every node, which is the whole point of a shared mode.
	var redis *MemRedis
	if mode == ModeSharedRedis || mode == ModeLeased {
		redis = NewMemRedis(clk.Now)
	}

	for i := 0; i < n; i++ {
		id := "node-" + string(rune('a'+i))
		cfg := quota.CoordinatorConfig{
			Mode: mode, Nodes: n, NodeID: id, Now: clk.Now,
			BlockSize: p.Block, MinLeasable: p.MinLeasable,
		}
		switch mode {
		case ModeLocal:
			// Clustered stays false: the guard refuses the pairing, and this
			// test is measuring what the mode does, not whether it is allowed.
		case ModeSharedRedis, ModeLeased:
			cfg.Clustered = true
			shared, err := NewRedisShared(redis)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Shared = shared
		case ModeSharedPG:
			cfg.Clustered = true
			// Each node gets its own lease store over its own pool, because
			// that is what a node is. They meet only in the table.
			ls, err := NewLeaseStore(stores[i], id, clk.Now)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Shared = ls
		}
		c, err := quota.NewCoordinator(cfg)
		if err != nil {
			t.Fatalf("NewCoordinator(%s): %v", mode, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		out = append(out, c)
	}
	return out
}

// TestEveryModePublishesANumber walks the mode list rather than a hand-written
// one, so a mode added later cannot ship without an accuracy figure.
func TestEveryModePublishesANumber(t *testing.T) {
	p := Params{Limit: 100, Nodes: 3, Block: 10}
	for _, m := range Modes() {
		a, err := Publish(m, p)
		if err != nil {
			t.Fatalf("Publish(%s): %v", m, err)
		}
		if a.Mode != m {
			t.Fatalf("Publish(%s) described %s", m, a.Mode)
		}
		if a.MaxOvershoot < 0 {
			t.Fatalf("%s published a negative overshoot %d", m, a.MaxOvershoot)
		}
		if a.HotPathCost == "" {
			t.Fatalf("%s published an accuracy figure with no price attached", m)
		}
		t.Logf("%v", a)
	}
}

// TestPublishedFiguresAreTheDocumentedArithmetic pins the formulas of
// DESIGN 5.6 to actual numbers, so a change to either has to change the other.
func TestPublishedFiguresAreTheDocumentedArithmetic(t *testing.T) {
	cases := []struct {
		mode Mode
		p    Params
		want int64
	}{
		{ModeLocal, Params{Limit: 50, Nodes: 1}, 0},
		{ModeLocal, Params{Limit: 50, Nodes: 4}, 150}, // limit x (nodes-1)
		{ModeSharedRedis, Params{Limit: 50, Nodes: 9}, 0},
		{ModeSharedPG, Params{Limit: 50, Nodes: 9}, 0},
		{ModeLeased, Params{Limit: 50, Nodes: 1, Block: 16}, 0},
		{ModeLeased, Params{Limit: 50, Nodes: 4, Block: 16}, 48}, // block x (nodes-1)
		{ModeLeased, Params{Limit: 50, Nodes: 4, Block: 4}, 12},  // the knob moves the figure
	}
	for _, tc := range cases {
		a, err := Publish(tc.mode, tc.p)
		if err != nil {
			t.Fatalf("Publish(%s, %+v): %v", tc.mode, tc.p, err)
		}
		if a.MaxOvershoot != tc.want {
			t.Fatalf("Publish(%s, nodes=%d, block=%d) = %d, want %d",
				tc.mode, tc.p.Nodes, tc.p.Block, a.MaxOvershoot, tc.want)
		}
	}
}

// TestLeasedRefusesLimitsItCannotDivide is DESIGN 5.6's other refusal: a
// single-digit limit cannot be usefully split across nodes, so leased says so
// instead of handing one node everything.
func TestLeasedRefusesLimitsItCannotDivide(t *testing.T) {
	_, err := Publish(ModeLeased, Params{Limit: 5, Nodes: 3, MinLeasable: 16})
	if !errors.Is(err, ErrLimitTooSmall) {
		t.Fatalf("Publish(leased, limit 5) = %v, want ErrLimitTooSmall", err)
	}
	// And the shared modes take it happily, which is what the refusal points at.
	for _, m := range []Mode{ModeSharedPG, ModeSharedRedis} {
		if _, err := Publish(m, Params{Limit: 5, Nodes: 3}); err != nil {
			t.Fatalf("Publish(%s, limit 5) = %v, want success", m, err)
		}
	}
}

// TestSharedRedisAndSharedPGAgree runs the same load through both exact modes
// and requires the same answer. They are different code over different storage;
// if "exact" means anything, it means they cannot disagree.
func TestSharedRedisAndSharedPGAgree(t *testing.T) {
	eachCluster(t, 3, func(t *testing.T, stores []*store.Store, clk *clock) {
		p := Params{Limit: 40, Nodes: 3, Clustered: true}
		results := map[Mode]int64{}
		for _, m := range []Mode{ModeSharedRedis, ModeSharedPG} {
			coords := coordinators(t, m, stores, clk, p)
			key := quota.NewKey("key", "k-"+m.String(), quota.Rolling(time.Hour),
				quota.MetricTokensTotal, clk.Now())
			results[m] = chargeConcurrently(t, coords, key, 40)
		}
		if results[ModeSharedRedis] != results[ModeSharedPG] {
			t.Fatalf("shared-redis admitted %d and shared-pg admitted %d against the same limit",
				results[ModeSharedRedis], results[ModeSharedPG])
		}
		if results[ModeSharedPG] != 40 {
			t.Fatalf("the exact modes admitted %d of a limit of 40", results[ModeSharedPG])
		}
	})
}

// TestLeaseStoreReturnsUnitsWhenALeaseExpires proves the shared-pg table is a
// lease table and not a counter with extra columns: a node that stops renewing
// gives its units back without releasing them, which is what makes the mode
// survive a node that dies mid-request.
func TestLeaseStoreReturnsUnitsWhenALeaseExpires(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		a, err := NewLeaseStore(stores[0], "node-a", clk.Now)
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewLeaseStore(stores[1], "node-b", clk.Now)
		if err != nil {
			t.Fatal(err)
		}

		const key, limit = "q/shared/1", int64(10)
		if got, err := a.Reserve(ctx, key, 10, limit, 30*time.Second); err != nil || got != 10 {
			t.Fatalf("node-a Reserve = %d, %v; want 10", got, err)
		}
		if got, err := b.Reserve(ctx, key, 1, limit, 30*time.Second); err != nil || got != 0 {
			t.Fatalf("node-b Reserve = %d, %v; want 0 -- node-a holds the lot", got, err)
		}

		// node-a dies. Nothing releases; the lease simply lapses.
		clk.Add(31 * time.Second)
		if got, err := b.Reserve(ctx, key, 10, limit, 30*time.Second); err != nil || got != 10 {
			t.Fatalf("after the lease expired, node-b Reserve = %d, %v; want 10", got, err)
		}
		if v, err := b.Value(ctx, key); err != nil || v != 10 {
			t.Fatalf("counter = %d, %v; want 10 -- the expired lease must not still be counted", v, err)
		}
	})
}
