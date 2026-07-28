package quota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func testKey(now time.Time) Key {
	return NewKey("credential", "acct-1", Rolling(time.Hour), MetricRequests, now)
}

func TestModeParsing(t *testing.T) {
	for s, want := range map[string]Mode{
		"":             ModeLocal,
		"local":        ModeLocal,
		"shared-redis": ModeSharedRedis,
		"shared-pg":    ModeSharedPG,
		"leased":       ModeLeased,
	} {
		got, err := ParseMode(s)
		if err != nil || got != want {
			t.Fatalf("ParseMode(%q) = %v, %v", s, got, err)
		}
		if s != "" && got.String() != s {
			t.Fatalf("%v.String() = %q, want %q", got, got.String(), s)
		}
	}
	if _, err := ParseMode("eventual"); err == nil {
		t.Fatal("ParseMode accepted an unknown mode")
	}
}

// TestEveryModeReportsItsOvershootAsANumber is the requirement of §5.6/§6.3:
// "approximately accurate" is not an acceptable specification.
func TestEveryModeReportsItsOvershootAsANumber(t *testing.T) {
	shared := NewMemShared(func() time.Time { return base })
	const limit = 1000

	cases := []struct {
		mode  Mode
		nodes int
		want  int64
	}{
		{ModeLocal, 1, 0},
		{ModeLocal, 4, 3 * limit}, // every node carries the whole limit
		{ModeSharedRedis, 4, 0},
		{ModeSharedPG, 9, 0},
		{ModeLeased, 1, 0},
		{ModeLeased, 4, 3 * DefaultBlockSize},
	}
	for _, c := range cases {
		cfg := CoordinatorConfig{
			Mode: c.mode, Nodes: c.nodes, Shared: shared,
			Clustered: c.nodes > 1 && c.mode != ModeLocal,
			Now:       func() time.Time { return base },
		}
		co, err := NewCoordinator(cfg)
		if err != nil {
			t.Fatalf("%v: %v", c.mode, err)
		}
		got, err := co.MaxOvershoot(limit, c.nodes)
		if err != nil {
			t.Fatalf("%v.MaxOvershoot: %v", c.mode, err)
		}
		if got != c.want {
			t.Fatalf("%v with %d nodes reports overshoot %d, want %d", c.mode, c.nodes, got, c.want)
		}
		if err := co.Close(); err != nil {
			t.Fatalf("%v.Close: %v", c.mode, err)
		}
	}
}

// TestClusteredLocalIsRefused: revision 1 called this a recommendation
// (DESIGN §5.6). It is a hard guard.
func TestClusteredLocalIsRefused(t *testing.T) {
	_, err := NewCoordinator(CoordinatorConfig{Mode: ModeLocal, Clustered: true, Nodes: 3})
	if !errors.Is(err, ErrLocalInCluster) {
		t.Fatalf("NewCoordinator = %v, want ErrLocalInCluster", err)
	}
	// The same configuration on one node is fine.
	if _, err := NewCoordinator(CoordinatorConfig{Mode: ModeLocal}); err != nil {
		t.Fatalf("single-node local: %v", err)
	}
}

func TestSharedModesNeedASharedStore(t *testing.T) {
	for _, m := range []Mode{ModeSharedRedis, ModeSharedPG, ModeLeased} {
		if _, err := NewCoordinator(CoordinatorConfig{Mode: m}); !errors.Is(err, ErrSharedStoreRequired) {
			t.Fatalf("%v without a store = %v", m, err)
		}
	}
}

func TestLocalCoordinatorCounts(t *testing.T) {
	co, err := NewCoordinator(CoordinatorConfig{Mode: ModeLocal, Now: func() time.Time { return base }})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	ctx := context.Background()
	k := testKey(base)

	for i := range 10 {
		g, err := co.Charge(ctx, k, 10, 1, base)
		if err != nil || !g.OK {
			t.Fatalf("charge %d: %+v %v", i, g, err)
		}
	}
	if g, _ := co.Charge(ctx, k, 10, 1, base); g.OK {
		t.Fatal("the eleventh unit was granted against a limit of 10")
	}
	// A refund gives one back.
	if err := co.Refund(ctx, k, 1, base); err != nil {
		t.Fatal(err)
	}
	if g, _ := co.Charge(ctx, k, 10, 1, base); !g.OK {
		t.Fatal("a refunded unit was not reusable")
	}
	// The window ages out and the counter recovers.
	if g, _ := co.Charge(ctx, k, 10, 1, base.Add(2*time.Hour)); !g.OK {
		t.Fatal("the rolling window never aged out")
	}
	if used, err := co.Used(ctx, k, base.Add(2*time.Hour)); err != nil || used != 1 {
		t.Fatalf("used = %d, %v", used, err)
	}
}

func TestSharedCoordinatorIsExact(t *testing.T) {
	shared := NewMemShared(func() time.Time { return base })
	newNode := func() Coordinator {
		co, err := NewCoordinator(CoordinatorConfig{
			Mode: ModeSharedRedis, Clustered: true, Nodes: 3, Shared: shared,
			Now: func() time.Time { return base },
		})
		if err != nil {
			t.Fatal(err)
		}
		return co
	}
	a, b, c := newNode(), newNode(), newNode()
	defer a.Close()
	defer b.Close()
	defer c.Close()

	ctx := context.Background()
	k := testKey(base)
	const limit = 20

	granted := 0
	for range 30 {
		for _, co := range []Coordinator{a, b, c} {
			if g, err := co.Charge(ctx, k, limit, 1, base); err != nil {
				t.Fatal(err)
			} else if g.OK {
				granted++
			}
		}
	}
	if granted != limit {
		t.Fatalf("three nodes granted %d units against a shared limit of %d", granted, limit)
	}
	// An all-or-nothing charge that does not fit leaves nothing stranded.
	before, _ := shared.Value(ctx, k.String())
	if g, _ := a.Charge(ctx, k, limit, 5, base); g.OK {
		t.Fatal("a charge past the limit was granted")
	}
	if after, _ := shared.Value(ctx, k.String()); after != before {
		t.Fatalf("a refused charge stranded %d units", after-before)
	}
}

func TestLeasedCoordinatorRefusesASmallLimit(t *testing.T) {
	shared := NewMemShared(func() time.Time { return base })
	co, err := NewCoordinator(CoordinatorConfig{
		Mode: ModeLeased, Clustered: true, Nodes: 4, Shared: shared,
		Now: func() time.Time { return base },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Single-digit limits cannot be usefully divided across nodes.
	for _, limit := range []int64{1, 5, DefaultMinLeasable - 1} {
		if _, err := co.MaxOvershoot(limit, 4); !errors.Is(err, ErrLimitTooSmall) {
			t.Fatalf("MaxOvershoot(%d) = %v, want ErrLimitTooSmall", limit, err)
		}
		if _, err := co.Charge(context.Background(), testKey(base), limit, 1, base); !errors.Is(err, ErrLimitTooSmall) {
			t.Fatalf("Charge against a limit of %d = %v, want ErrLimitTooSmall", limit, err)
		}
	}
	// At the minimum it works.
	if _, err := co.MaxOvershoot(DefaultMinLeasable, 4); err != nil {
		t.Fatalf("MaxOvershoot at the minimum: %v", err)
	}
}

func TestLeasedCoordinatorLeasesBlocksAndStaysWithinTheLimit(t *testing.T) {
	now := base
	shared := NewMemShared(func() time.Time { return now })
	newNode := func(id string) Coordinator {
		co, err := NewCoordinator(CoordinatorConfig{
			Mode: ModeLeased, Clustered: true, Nodes: 2, NodeID: id, Shared: shared,
			BlockSize: 8, LeaseTTL: time.Minute, Now: func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		return co
	}
	a, b := newNode("a"), newNode("b")
	defer a.Close()
	defer b.Close()

	ctx := context.Background()
	k := testKey(now)
	const limit = 40

	// One charge takes a whole block from the authority, and the next seven
	// come out of it without touching the authority again.
	if g, err := a.Charge(ctx, k, limit, 1, now); err != nil || !g.OK {
		t.Fatalf("first charge: %+v %v", g, err)
	}
	if v, _ := shared.Value(ctx, k.String()); v != 8 {
		t.Fatalf("the authority holds %d after one charge, want a whole block of 8", v)
	}
	for range 7 {
		if g, _ := a.Charge(ctx, k, limit, 1, now); !g.OK {
			t.Fatal("a charge inside the leased block was refused")
		}
	}
	if v, _ := shared.Value(ctx, k.String()); v != 8 {
		t.Fatalf("the authority was consulted mid-block: %d", v)
	}

	// Both nodes together never exceed the limit.
	granted := 8
	for range 100 {
		for _, co := range []Coordinator{a, b} {
			if g, _ := co.Charge(ctx, k, limit, 1, now); g.OK {
				granted++
			}
		}
	}
	if granted > limit {
		t.Fatalf("two leasing nodes granted %d units against a limit of %d", granted, limit)
	}
	if granted != limit {
		t.Fatalf("granted %d of %d: leasing left units stranded", granted, limit)
	}
}

func TestLeasedCoordinatorReturnsItsLeaseOnClose(t *testing.T) {
	now := base
	shared := NewMemShared(func() time.Time { return now })
	co, err := NewCoordinator(CoordinatorConfig{
		Mode: ModeLeased, Clustered: true, Nodes: 2, Shared: shared,
		BlockSize: 16, LeaseTTL: time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k := testKey(now)
	if g, _ := co.Charge(ctx, k, 100, 1, now); !g.OK {
		t.Fatal("charge refused")
	}
	if v, _ := shared.Value(ctx, k.String()); v != 16 {
		t.Fatalf("authority holds %d", v)
	}
	if err := co.Close(); err != nil {
		t.Fatal(err)
	}
	// The 15 unspent units go back; the 1 spent unit stays spent.
	if v, _ := shared.Value(ctx, k.String()); v != 1 {
		t.Fatalf("after Close the authority holds %d, want 1", v)
	}
}

func TestLeasedCoordinatorDoesNotSpendAnExpiredLease(t *testing.T) {
	now := base
	shared := NewMemShared(func() time.Time { return now })
	co, err := NewCoordinator(CoordinatorConfig{
		Mode: ModeLeased, Clustered: true, Nodes: 2, Shared: shared,
		BlockSize: 8, LeaseTTL: time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	ctx := context.Background()
	k := testKey(now)

	if g, _ := co.Charge(ctx, k, 100, 1, now); !g.OK {
		t.Fatal("charge refused")
	}
	lc := co.(*leasedCoordinator)
	if rem := lc.LeaseRemaining(k); rem != 7 {
		t.Fatalf("lease remaining = %d, want 7", rem)
	}
	// Past the TTL the lease is not spendable: the authority may already have
	// reclaimed it, and spending it twice is exactly the overshoot the bound
	// is meant to cap.
	now = now.Add(2 * time.Minute)
	if g, _ := co.Charge(ctx, k, 100, 1, now); !g.OK {
		t.Fatal("a fresh lease was not taken after expiry")
	}
	if rem := lc.LeaseRemaining(k); rem != 7 {
		t.Fatalf("after re-leasing, remaining = %d, want 7 from a fresh block", rem)
	}
}

func TestCoordinatorsAreConcurrencySafe(t *testing.T) {
	shared := NewMemShared(func() time.Time { return base })
	modes := []CoordinatorConfig{
		{Mode: ModeLocal, Now: func() time.Time { return base }},
		{Mode: ModeSharedRedis, Clustered: true, Nodes: 2, Shared: shared, Now: func() time.Time { return base }},
		{Mode: ModeLeased, Clustered: true, Nodes: 2, Shared: shared, BlockSize: 4,
			LeaseTTL: time.Hour, Now: func() time.Time { return base }},
	}
	for _, cfg := range modes {
		co, err := NewCoordinator(cfg)
		if err != nil {
			t.Fatal(err)
		}
		const limit = 200
		k := NewKey("credential", "conc-"+cfg.Mode.String(), Rolling(time.Hour), MetricRequests, base)
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			granted int64
		)
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n := int64(0)
				for range 50 {
					if g, err := co.Charge(context.Background(), k, limit, 1, base); err == nil && g.OK {
						n++
					}
				}
				mu.Lock()
				granted += n
				mu.Unlock()
			}()
		}
		wg.Wait()
		if granted > limit {
			t.Fatalf("%v granted %d against a limit of %d", cfg.Mode, granted, limit)
		}
		if err := co.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMemSharedSweepsExpiredCounters(t *testing.T) {
	now := base
	shared := NewMemShared(func() time.Time { return now })
	ctx := context.Background()
	if _, err := shared.Reserve(ctx, "k", 5, 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := shared.SweepExpired(now); n != 0 {
		t.Fatalf("swept %d live counters", n)
	}
	now = now.Add(2 * time.Minute)
	if n := shared.SweepExpired(now); n != 1 {
		t.Fatalf("swept %d expired counters, want 1", n)
	}
	if v, _ := shared.Value(ctx, "k"); v != 0 {
		t.Fatalf("value after the sweep = %d", v)
	}
}

func TestKeyStringIsStableAndPeriodScoped(t *testing.T) {
	a := NewKey("credential", "acct-1", Daily, MetricCostUSD, base)
	b := NewKey("credential", "acct-1", Daily, MetricCostUSD, base.Add(time.Hour))
	if a.String() != b.String() {
		t.Fatalf("two instants in the same day gave different keys:\n%s\n%s", a, b)
	}
	c := NewKey("credential", "acct-1", Daily, MetricCostUSD, base.Add(24*time.Hour))
	if a.String() == c.String() {
		t.Fatal("the next day reuses the previous key, so the counter would never reset")
	}
	d := NewKey("credential", "acct-2", Daily, MetricCostUSD, base)
	if a.String() == d.String() {
		t.Fatal("two credentials share one key")
	}
}
