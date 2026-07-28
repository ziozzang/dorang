package cluster

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// usd is a readable nano-USD literal. Budgets are carried in nano so that
// window arithmetic is exact addition rather than accumulated float error.
func usd(v float64) int64 { return quota.NanoUSD(v) }

func newLedger(t *testing.T, s *store.Store, node string, clk *clock, block int64, ttl time.Duration) *Ledger {
	t.Helper()
	l, err := NewLedger(LedgerConfig{
		Store: s, NodeID: node, Block: block, TTL: ttl, RenewBefore: ttl / 3, Now: clk.Now,
	})
	if err != nil {
		t.Fatalf("NewLedger(%s): %v", node, err)
	}
	return l
}

// spend runs n request-shaped reservations, each reserving an upper bound and
// settling a smaller actual, and returns the total actually spent.
func spend(t *testing.T, l *Ledger, key CounterKey, limit int64, n int, estimate, actual int64) int64 {
	t.Helper()
	ctx := context.Background()
	var total int64
	for i := 0; i < n; i++ {
		h, err := l.Reserve(ctx, key, limit, estimate)
		if err != nil {
			t.Fatalf("Reserve %d: %v", i, err)
		}
		if err := l.Settle(h, actual); err != nil {
			t.Fatalf("Settle %d: %v", i, err)
		}
		total += actual
	}
	return total
}

// TestBudgetSurvivesARestart is risk W9 itself: "a restart resets the windows.
// Safe for concurrency, wrong for accounting: a monthly budget silently starts
// over."
//
// The assertion is the remaining budget after a restart, not merely that
// something was written. A store that recorded the spend and a process that
// ignored it on the way back would satisfy any weaker check.
func TestBudgetSurvivesARestart(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		const limit = 10 * 1_000_000_000 // 10 USD in nano
		key := BudgetKey("team", "team-7", quota.Monthly, clk.Now())

		first := newLedger(t, s, "node-a", clk, usd(1), time.Minute)
		spent := spend(t, first, key, limit, 35, usd(0.2), usd(0.1))
		if spent != usd(3.5) {
			t.Fatalf("the fixture spent %d nano, want %d", spent, usd(3.5))
		}

		// A planned restart drains first (DESIGN 13), which returns the unspent
		// part of the block and leaves the counter holding exactly the spend.
		if err := first.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// The process restarts. Same store, same node id, nothing in memory.
		second := newLedger(t, s, "node-a", clk, usd(1), time.Minute)
		t.Cleanup(func() { _ = second.Close(ctx) })

		committed, err := second.Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if committed == 0 {
			t.Fatal("the budget reset to zero across a restart -- this is W9, unfixed")
		}
		if committed != spent {
			t.Fatalf("after a clean restart the budget shows %d nano spent, want exactly %d", committed, spent)
		}
		avail, err := second.Available(ctx, key, limit)
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(limit) - spent; avail != want {
			t.Fatalf("remaining budget %d nano, want %d", avail, want)
		}

		// And the restarted process spends from where the first one stopped
		// rather than from the top of the month.
		if _, err := second.Reserve(ctx, key, limit, usd(7.0)); !errors.Is(err, ErrExhausted) {
			t.Fatalf("reserving 7.00 USD against a remaining 6.50 returned %v, want ErrExhausted", err)
		}
		if _, err := second.Reserve(ctx, key, limit, usd(6.0)); err != nil {
			t.Fatalf("reserving 6.00 USD against a remaining 6.50 failed: %v", err)
		}
	})
}

// TestBudgetSurvivesACrash is the unplanned half. Nothing drains, so the
// counter is left holding whole blocks -- which is the conservative direction
// and the reason the block is charged before it is spent. The leader's reclaim
// then gives back what the dead node had not used.
func TestBudgetSurvivesACrash(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		const limit = 10 * 1_000_000_000
		const ttl = time.Minute
		key := BudgetKey("key", "key-3", quota.Monthly, clk.Now())

		crashed := newLedger(t, s, "node-a", clk, usd(1), ttl)
		spent := spend(t, crashed, key, limit, 35, usd(0.2), usd(0.1))

		// A live node ticks, which checkpoints consumption. This is the
		// difference between a reclaim that returns the unspent part and one
		// that returns the whole lease.
		if err := crashed.Maintain(ctx); err != nil {
			t.Fatalf("Maintain: %v", err)
		}

		// The process is killed. No Close, no release, nothing.
		survivor := newLedger(t, s, "node-b", clk, usd(1), ttl)
		t.Cleanup(func() { _ = survivor.Close(ctx) })

		committed, err := survivor.Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if committed < spent {
			t.Fatalf("after a crash the budget shows %d nano spent but %d was actually spent: "+
				"under-counting here is what lets a budget be exceeded", committed, spent)
		}
		if over := committed - spent; over > usd(1) {
			t.Fatalf("a crash cost %d nano of budget, more than the %d block size", over, usd(1))
		}

		// The leader reclaims once the lease lapses, and the unspent part comes
		// back. Without this the budget shrinks by a block every time a node
		// dies, which over a month is a budget nothing spent.
		clk.Add(ttl + time.Second)
		res, err := survivor.ReclaimExpired(ctx, clk.Now())
		if err != nil {
			t.Fatalf("ReclaimExpired: %v", err)
		}
		if res.Leases != 1 {
			t.Fatalf("reclaimed %d leases, want 1", res.Leases)
		}
		after, err := survivor.Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if after != spent {
			t.Fatalf("after reclaim the budget shows %d nano spent, want exactly %d "+
				"(reclaimed %d nano)", after, spent, res.Returned)
		}
	})
}

// TestLeaseOutlivingItsNodeIsReclaimed is the property the whole mechanism
// rests on: "a lease that does not outlive its node is not a lease" (W9). This
// checks the other direction -- one that outlives its node must not outlive it
// forever.
func TestLeaseOutlivingItsNodeIsReclaimed(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		const limit = 10 * 1_000_000_000
		const ttl = 30 * time.Second
		key := BudgetKey("global", "", quota.Monthly, clk.Now())

		dead := newLedger(t, stores[0], "node-a", clk, usd(1), ttl)
		h, err := dead.Reserve(ctx, key, limit, usd(0.25))
		if err != nil {
			t.Fatal(err)
		}
		if err := dead.Settle(h, usd(0.25)); err != nil {
			t.Fatal(err)
		}
		if err := dead.Checkpoint(ctx); err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}

		leader := newLedger(t, stores[1], "node-b", clk, usd(1), ttl)
		t.Cleanup(func() { _ = leader.Close(ctx) })

		// While the lease is live the leader must leave it alone. Reclaiming a
		// lease whose holder is still spending against it is the one thing the
		// published overshoot figure assumes cannot happen.
		res, err := leader.ReclaimExpired(ctx, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if res.Leases != 0 {
			t.Fatalf("the leader reclaimed %d live leases", res.Leases)
		}
		if v, _ := leader.Committed(ctx, key); v != usd(1) {
			t.Fatalf("committed %d, want a whole block of %d charged up front", v, usd(1))
		}

		// node-a dies. The lease lapses and the leader takes it back.
		clk.Add(ttl + time.Second)
		res, err = leader.ReclaimExpired(ctx, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if res.Leases != 1 || res.Returned != usd(0.75) {
			t.Fatalf("reclaim returned %d leases and %d nano, want 1 and %d", res.Leases, res.Returned, usd(0.75))
		}
		v, err := leader.Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if v != usd(0.25) {
			t.Fatalf("after reclaim the budget shows %d nano spent, want the %d the dead node used", v, usd(0.25))
		}
	})
}

// TestDeadNodeLeasesAreReclaimedByHeartbeat covers the other trigger: a node
// whose heartbeat lapsed. Waiting for a TTL its own writes had been extending
// would keep its budget out of circulation for as long as it was alive.
func TestDeadNodeLeasesAreReclaimedByHeartbeat(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		const limit = 10 * 1_000_000_000
		key := BudgetKey("user", "u-1", quota.Monthly, clk.Now())

		a := testNode(t, stores[0], "node-a", clk, func(c *Config) { c.BlockSize = usd(1) })
		b := testNode(t, stores[1], "node-b", clk, func(c *Config) { c.BlockSize = usd(1) })
		tick(t, a, b)

		h, err := a.Ledger().Reserve(ctx, key, limit, usd(0.4))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Ledger().Settle(h, usd(0.4)); err != nil {
			t.Fatal(err)
		}
		if err := a.Ledger().Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}

		// node-a stops heartbeating. The node TTL is 6s in the harness; the
		// ledger lease TTL is a minute, so only the heartbeat can catch this.
		clk.Add(10 * time.Second)
		tick(t, b)
		if !b.IsLeader() {
			t.Fatal("the survivor did not become leader")
		}

		dead, err := b.Registry().Dead(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(dead) != 1 || dead[0].ID != "node-a" {
			t.Fatalf("dead nodes = %v, want just node-a", dead)
		}
		v, err := b.Ledger().Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if v != usd(0.4) {
			t.Fatalf("after the leader reclaimed a dead node's lease the budget shows %d nano, want %d",
				v, usd(0.4))
		}
	})
}

// TestQuotaWindowSurvivesARestart is the other half of W9. Quota and budget go
// through one mechanism, so the property is the same and so is the check.
func TestQuotaWindowSurvivesARestart(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		const limit = int64(200_000)
		key := QuotaKey("credential", "cred-1", quota.Weekly, quota.MetricTokensTotal, clk.Now())

		first := newLedger(t, s, "node-a", clk, 1000, time.Minute)
		used := spend(t, first, key, limit, 50, 400, 300)
		if err := first.Close(ctx); err != nil {
			t.Fatal(err)
		}

		second := newLedger(t, s, "node-a", clk, 1000, time.Minute)
		t.Cleanup(func() { _ = second.Close(ctx) })
		got, err := second.Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if got != used {
			t.Fatalf("the weekly quota window shows %d units used after a restart, want %d", got, used)
		}

		// And it landed in quota_buckets, not somewhere convenient.
		var v int64
		row := s.DB().QueryRow(rebind(s.Driver(), `
			SELECT value FROM quota_buckets
			 WHERE scope = ? AND scope_key = ? AND "window" = ? AND metric = ? AND bucket_start = ?`),
			"credential", "cred-1", "weekly", "tokens_total", store.Micros(key.PeriodStart))
		if err := row.Scan(&v); err != nil {
			t.Fatalf("quota_buckets row: %v", err)
		}
		if v != used {
			t.Fatalf("quota_buckets holds %d, want %d", v, used)
		}
	})
}

// TestBudgetIsNeverExceededUnderConcurrency is DESIGN 14 scenario 5 against the
// durable ledger: a budget cap must not be exceeded under concurrent load, and
// the cap is the durable figure rather than a per-node one.
func TestBudgetIsNeverExceededUnderConcurrency(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		const limit = 4 * 1_000_000_000 // 4 USD
		key := BudgetKey("global", "", quota.Monthly, clk.Now())

		ledgers := []*Ledger{
			newLedger(t, stores[0], "node-a", clk, usd(0.5), time.Minute),
			newLedger(t, stores[1], "node-b", clk, usd(0.5), time.Minute),
		}
		t.Cleanup(func() {
			for _, l := range ledgers {
				_ = l.Close(ctx)
			}
		})

		var granted atomic.Int64
		var refused atomic.Int64
		var wg sync.WaitGroup
		for _, l := range ledgers {
			for g := 0; g < 4; g++ {
				wg.Add(1)
				go func(l *Ledger) {
					defer wg.Done()
					for i := 0; i < 30; i++ {
						h, err := l.Reserve(ctx, key, limit, usd(0.1))
						switch {
						case errors.Is(err, ErrExhausted):
							refused.Add(1)
							continue
						case err != nil:
							t.Errorf("Reserve: %v", err)
							return
						}
						granted.Add(usd(0.1))
						if err := l.Settle(h, usd(0.1)); err != nil {
							t.Errorf("Settle: %v", err)
							return
						}
					}
				}(l)
			}
		}
		wg.Wait()

		if refused.Load() == 0 {
			t.Fatal("nothing was refused; the limit was never reached and the test proved nothing")
		}
		if got := granted.Load(); got > limit {
			t.Fatalf("granted %d nano against a limit of %d: the budget was exceeded", got, limit)
		}
		committed, err := ledgers[0].Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if committed > limit {
			t.Fatalf("the durable counter holds %d nano against a limit of %d", committed, limit)
		}
	})
}

// TestHotPathWritesPerBlockNotPerRequest is DESIGN 9.6's actual claim, measured:
// "The store sees a write when a lease is taken or renewed, which is per block
// of budget rather than per request."
func TestHotPathWritesPerBlockNotPerRequest(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		const limit = 100 * 1_000_000_000
		key := BudgetKey("key", "k-1", quota.Monthly, clk.Now())

		l := newLedger(t, s, "node-a", clk, usd(1), time.Minute)
		t.Cleanup(func() { _ = l.Close(ctx) })

		const requests = 400
		spend(t, l, key, limit, requests, usd(0.02), usd(0.01))

		draws := l.Draws()
		// 400 requests at 0.01 USD net is 4 USD, so four blocks of 1 USD, plus
		// at most one more for the reserve/settle sawtooth.
		if draws > 6 {
			t.Fatalf("%d requests took %d store round trips; the block is meant to amortise them", requests, draws)
		}
		if draws == 0 {
			t.Fatal("no store round trips at all: nothing was made durable")
		}
		t.Logf("%d requests cost %d store writes (block %s USD)", requests, draws, "1.00")
	})
}

// TestReservationSweepsReclaimBothKinds is the pairing DESIGN 13 is explicit
// about: "expiry sweeps (capacity reservations 5.3 AND budget reservations
// 6.4)". Revision 1 of the design had only the first. They are the same pattern
// with the same failure mode, and a sweep that does one of them is a sweep that
// silently leaks the other.
func TestReservationSweepsReclaimBothKinds(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()

		// The capacity half: a real broker, with the background sweeper off so
		// the job is the only thing that can reclaim.
		broker := capacity.New(capacity.Config{
			Global: 4, SweepInterval: -1, ReservationTTL: 2 * time.Second, Now: clk.Now,
		})
		t.Cleanup(broker.Close)
		res, ok := broker.TryAcquire(capacity.Request{Provider: "p", Model: "m"})
		if !ok || res == nil {
			t.Fatal("could not acquire a capacity reservation")
		}
		if n := broker.Snapshot().Reservations; n != 1 {
			t.Fatalf("live reservations = %d, want 1", n)
		}

		// The budget half: a store reservation whose holder dies before settling.
		sub := store.Subject{Kind: store.SubjectTeam, ID: "team-1"}
		periodStart := quota.Monthly.PeriodStart(clk.Now())
		if _, err := s.ReserveBudget(ctx, store.ReserveRequest{
			Subject: sub, Period: "monthly", PeriodStart: periodStart,
			AmountNano: usd(2), Until: clk.Now().Add(2 * time.Second),
		}); err != nil {
			t.Fatalf("ReserveBudget: %v", err)
		}
		st, err := s.GetBudgetState(ctx, sub, "monthly", periodStart)
		if err != nil {
			t.Fatal(err)
		}
		if st.ReservedNano != usd(2) {
			t.Fatalf("reserved %d nano, want %d", st.ReservedNano, usd(2))
		}

		job := ReservationSweepJob(s, broker.Sweep, time.Second, clk.Now)

		// Before either deadline, the sweep must reclaim nothing. A sweep that
		// is merely eager releases live reservations out from under requests in
		// flight.
		if err := job.Run(ctx); err != nil {
			t.Fatalf("early sweep: %v", err)
		}
		if n := broker.Snapshot().Reservations; n != 1 {
			t.Fatalf("the early sweep reclaimed a live capacity reservation")
		}
		st, _ = s.GetBudgetState(ctx, sub, "monthly", periodStart)
		if st.ReservedNano != usd(2) {
			t.Fatal("the early sweep released a live budget reservation")
		}

		clk.Add(5 * time.Second)
		if err := job.Run(ctx); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if n := broker.Snapshot().Reservations; n != 0 {
			t.Fatalf("capacity reservations after the sweep = %d, want 0", n)
		}
		if e := broker.Snapshot().Expired; e != 1 {
			t.Fatalf("the broker recorded %d expiries, want 1", e)
		}
		st, err = s.GetBudgetState(ctx, sub, "monthly", periodStart)
		if err != nil {
			t.Fatal(err)
		}
		if st.ReservedNano != 0 {
			t.Fatalf("budget reservations after the sweep = %d nano, want 0", st.ReservedNano)
		}
	})
}

// TestReleaseRefundsInFull is DESIGN 6.4 R1-20: a request that reserved at the
// gate and was then refused while waiting for capacity never reached an
// upstream and must cost nothing.
func TestReleaseRefundsInFull(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		const limit = 1_000_000_000
		key := BudgetKey("key", "k-9", quota.Monthly, clk.Now())

		l := newLedger(t, s, "node-a", clk, usd(1), time.Minute)
		before, err := l.Available(ctx, key, limit)
		if err != nil {
			t.Fatal(err)
		}
		h, err := l.Reserve(ctx, key, limit, usd(0.6))
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Release(h); err != nil {
			t.Fatal(err)
		}
		after, err := l.Available(ctx, key, limit)
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("a released reservation cost %d nano; it must cost nothing", before-after)
		}
		// Settling a released hold a second time must not double-refund.
		if err := l.Settle(h, usd(0.6)); err != nil {
			t.Fatal(err)
		}
		again, err := l.Available(ctx, key, limit)
		if err != nil {
			t.Fatal(err)
		}
		if again != after {
			t.Fatalf("settling an already-released hold moved the budget by %d nano", again-after)
		}
		if err := l.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}
