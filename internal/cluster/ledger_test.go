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

// TestDeadNodeLeasesAreReclaimedOnTheLeaseAndNotTheHeartbeat covers the other
// trigger and the correction to it.
//
// A lapsed heartbeat is what makes the leader LOOK at a node's leases. It is
// not what makes them safe to take: [Registry.Dead] reports a heartbeat that
// did not arrive, which one slow store write produces on a node that is serving
// perfectly well, and the block's hot path reads an in-memory expiry and no
// store at all -- so the only thing that stops a holder spending is its own
// lease running out. This test asserts both halves, because the earlier version
// asserted only the second and would pass against the behaviour that admitted
// 190 against a limit of 100.
func TestDeadNodeLeasesAreReclaimedOnTheLeaseAndNotTheHeartbeat(t *testing.T) {
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

		// node-a stops heartbeating. The node TTL is 6s in the harness and the
		// ledger lease TTL is a minute, so for the next fifty seconds node-a is
		// declared dead and is still able to spend every unit it drew.
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
		if v != usd(1) {
			t.Fatalf("the leader returned %d nano of a DECLARED-dead node's live lease; "+
				"the block is still spendable from memory, so those units would be spent twice",
				usd(1)-v)
		}

		// Once the lease itself lapses the holder's hot path refuses it too, and
		// the two agree on the same instant. Now the reclaim is safe, and it
		// gives back exactly the part node-a had not used.
		clk.Add(time.Minute)
		tick(t, b)
		v, err = b.Ledger().Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if v != usd(0.4) {
			t.Fatalf("after the lease lapsed the budget shows %d nano, want the %d node-a used",
				v, usd(0.4))
		}
	})
}

// TestReclaimingALiveNodeCannotExceedTheLimit is the schedule that admitted 190
// against a limit of 100, asserted on the spend rather than on the reclaim.
//
// The arithmetic of the defect, so that the numbers below are not magic. A block
// of 100 against a limit of 100: node-a draws the lot, charges it to the durable
// counter before spending a unit of it (DESIGN §9.6), spends 10 and checkpoints.
// Its heartbeat then lapses -- ten seconds, against a node TTL of six -- while
// its block lease still has fifty seconds to run and its hot path, which reads
// an in-memory expiry and no store at all, is perfectly willing to keep serving.
// The leader declares it dead and returns the unspent 90 to the counter, which
// drops to 10. The leader draws 90 of the 90 that now look free and spends them.
// node-a spends the 90 it still holds. 10 + 90 + 90 = 190, against 100, with
// nobody dead.
//
// [Ledger.ReclaimNode] used to be guarded by node_id alone, on the argument that
// a dead node's lease would otherwise stay out of circulation "for as long as it
// was alive". It would not: a node that dies stops renewing, so its last
// renewal lapses one lease TTL later. That is the delay this guard costs, and it
// buys the only version of "the limit is not exceeded" that can be demonstrated.
func TestReclaimingALiveNodeCannotExceedTheLimit(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		const limit = int64(100)
		const block = int64(100)
		key := QuotaKey("credential", "c-1", quota.Rolling(time.Hour), quota.MetricRequests, clk.Now())

		live := newLedger(t, stores[0], "node-a", clk, block, time.Minute)
		leader := newLedger(t, stores[1], "node-b", clk, block, time.Minute)
		t.Cleanup(func() {
			_ = live.Close(ctx)
			_ = leader.Close(ctx)
		})

		var admitted int64
		take := func(l *Ledger, who string, n int64) {
			t.Helper()
			h, err := l.Reserve(ctx, key, limit, n)
			if errors.Is(err, ErrExhausted) {
				t.Logf("%s was refused %d units: %v", who, n, err)
				return
			}
			if err != nil {
				t.Fatalf("%s Reserve(%d): %v", who, n, err)
			}
			if err := l.Settle(h, n); err != nil {
				t.Fatal(err)
			}
			admitted += n
		}

		take(live, "node-a", 10)
		if err := live.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}

		// node-a's heartbeat lapses. It is not dead: it never stopped, its lease
		// has fifty seconds left, and it is about to serve more traffic.
		clk.Add(10 * time.Second)
		res, err := leader.ReclaimNode(ctx, "node-a", clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		// Reported and not fatal, so the run reaches the assertion that actually
		// matters: what the two nodes together are allowed to spend.
		if res.Returned != 0 {
			t.Errorf("the leader returned %d units of a live node's lease; the holder's hot "+
				"path can still spend every one of them", res.Returned)
		}

		// Both nodes now go for whatever the counter says is left. Whatever the
		// answer, the two of them together must not get more than the limit.
		take(leader, "node-b", 90)
		take(live, "node-a", 90)

		if admitted > limit {
			t.Fatalf("admitted %d against a limit of %d", admitted, limit)
		}
		if admitted != limit {
			t.Fatalf("admitted %d of a limit of %d: the guard is refusing traffic it should "+
				"serve, which is a different defect and not an acceptable fix for this one",
				admitted, limit)
		}

		// And once the lease really has lapsed the reclaim happens, so the guard
		// delays the return rather than preventing it.
		clk.Add(2 * time.Minute)
		res, err = leader.ReclaimNode(ctx, "node-a", clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if res.Leases != 1 {
			t.Fatalf("after the lease lapsed the reclaim took %d leases, want 1", res.Leases)
		}
	})
}

// TestReclaimNodeWillNotTakeTheCallersOwnLeases is the self-guard, on both
// implementations rather than on the one that had it.
//
// The leader is a node like any other: it holds leases against the same limits
// from its own request path. A registry that briefly reports it dead -- its own
// heartbeat write lost a race with a slow store, or its clock stepped -- would
// otherwise have it delete its own live rows and hand the units out again.
// [Ledger.ReclaimNode] guarded against this; [LeaseStore.ReclaimNode] did not.
func TestReclaimNodeWillNotTakeTheCallersOwnLeases(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		const limit = int64(64)
		const key = "q/self/1"

		shared, err := NewLeaseStore(s, "node-a", clk.Now)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := shared.Reserve(ctx, key, 40, limit, time.Minute); err != nil || got != 40 {
			t.Fatalf("Reserve = %d, %v; want 40", got, err)
		}

		// The lease is live and it is this node's. Both facts are reasons to
		// refuse; either alone would be enough.
		if n, err := shared.ReclaimNode(ctx, "node-a", clk.Now()); err != nil || n != 0 {
			t.Fatalf("the store reclaimed %d of its own rows (err %v)", n, err)
		}
		if v, err := shared.Held(ctx, key); err != nil || v != 40 {
			t.Fatalf("this node now holds %d of the 40 it reserved (err %v)", v, err)
		}

		ledger := newLedger(t, s, "node-a", clk, 16, time.Minute)
		t.Cleanup(func() { _ = ledger.Close(ctx) })
		ck := QuotaKey("credential", "c-self", quota.Rolling(time.Hour), quota.MetricRequests, clk.Now())
		h, err := ledger.Reserve(ctx, ck, limit, 8)
		if err != nil {
			t.Fatal(err)
		}
		if err := ledger.Settle(h, 8); err != nil {
			t.Fatal(err)
		}
		clk.Add(2 * time.Minute) // the lease has lapsed; only the self-guard is left
		res, err := ledger.ReclaimNode(ctx, "node-a", clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if res.Leases != 0 {
			t.Fatalf("the ledger reclaimed %d of its own leases", res.Leases)
		}
	})
}

// TestFinishedPeriodsAreReclaimed is the third defect: [Ledger.blocks] had no
// delete anywhere, so a long-running node accumulated one entry per key per
// period for the life of the process.
//
// The bound asserted is a constant and not "smaller than it was". Twenty
// one-minute windows for one key must leave at most the periods still inside
// their retention grace, whatever the loop count -- which is what makes this a
// statement about the map being reclaimed rather than about it growing slower.
func TestFinishedPeriodsAreReclaimed(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		const limit = int64(1 << 40)
		const periods = 20
		w := quota.Rolling(time.Minute)

		l, err := NewLedger(LedgerConfig{
			Store: s, NodeID: "node-a", Block: 16, TTL: 30 * time.Second,
			RenewBefore: 10 * time.Second, Retain: time.Minute, Now: clk.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close(ctx) })

		var spent int64
		for i := 0; i < periods; i++ {
			key := QuotaKey("key", "k", w, quota.MetricRequests, clk.Now())
			h, err := l.Reserve(ctx, key, limit, 4)
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Settle(h, 3); err != nil {
				t.Fatal(err)
			}
			spent += 3
			clk.Add(time.Minute)
			if err := l.Maintain(ctx); err != nil {
				t.Fatal(err)
			}
		}
		// The last period ends, plus the retention grace.
		clk.Add(2 * time.Minute)
		if err := l.Maintain(ctx); err != nil {
			t.Fatal(err)
		}

		// One block per period would be `periods`. The bound is the periods a
		// grace of one minute can still be covering, and nothing more.
		if got := len(l.Stats()); got > 2 {
			t.Fatalf("%d blocks retained after %d finished one-minute periods; the map is "+
				"append-only for the life of the process", got, periods)
		}

		// Reclaimed does not mean forgotten. The counters are the truth and they
		// still hold every unit, so a backfilled row against a closed period
		// draws again rather than starting the period over.
		first := QuotaKey("key", "k", w, quota.MetricRequests, epoch)
		v, err := l.Committed(ctx, first)
		if err != nil {
			t.Fatal(err)
		}
		if v != 3 {
			t.Fatalf("the first period's counter reads %d after its block was reclaimed, want 3", v)
		}
		// And no lease rows were stranded: a block dropped without being
		// returned would leave its row to expire on its own TTL, holding units
		// nobody could see.
		var rows int
		if err := s.DB().QueryRow(rebind(s.Driver(),
			`SELECT COUNT(*) FROM quota_leases WHERE node_id = ?`), "node-a").Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("%d lease rows outlived their reclaimed blocks", rows)
		}
	})
}

// TestAPeriodThatIsOverIsNotAPeriodThatIsSettled is the boundary the reclaim has
// to get right.
//
// Settlement is the path that RETURNS units: the estimate is an upper bound and
// the actual is normally smaller (DESIGN §6.4). A request that reserved just
// before a period boundary settles just after it, so a block dropped on the
// clock alone would forfeit that refund into a period nobody accounts any more.
// Counting the outstanding holds answers this exactly; a grace period long
// enough to cover it would only be a guess.
func TestAPeriodThatIsOverIsNotAPeriodThatIsSettled(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		const limit = int64(1 << 40)
		w := quota.Rolling(time.Minute)

		l, err := NewLedger(LedgerConfig{
			Store: s, NodeID: "node-a", Block: 100, TTL: time.Minute, Retain: 0, Now: clk.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close(ctx) })

		key := QuotaKey("key", "late", w, quota.MetricRequests, clk.Now())
		h, err := l.Reserve(ctx, key, limit, 40)
		if err != nil {
			t.Fatal(err)
		}

		// The period ends, and every grace period anybody would have chosen goes
		// with it. The hold is still open.
		clk.Add(time.Hour)
		if err := l.Maintain(ctx); err != nil {
			t.Fatal(err)
		}
		if got := len(l.Stats()); got != 1 {
			t.Fatalf("a block with an unsettled hold was reclaimed after %d blocks remained; "+
				"the refund below has nowhere to land", got)
		}

		// The settlement arrives, an hour late, and the refund reaches the block
		// it was drawn from.
		if err := l.Settle(h, 5); err != nil {
			t.Fatal(err)
		}
		if err := l.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		if err := l.Maintain(ctx); err != nil {
			t.Fatal(err)
		}
		if got := len(l.Stats()); got != 0 {
			t.Fatalf("%d blocks retained once the last settlement arrived", got)
		}
		v, err := l.Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if v != 5 {
			t.Fatalf("the counter reads %d after a late settlement of 5 against an estimate "+
				"of 40; the refund was lost with the block", v)
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

// TestLeaderJobsReclaimBothKindsOfAbandonedHold is the pairing DESIGN 13 is
// explicit about: "expiry sweeps (capacity reservations 5.3 AND budget
// reservations 6.4)". They are the same pattern with the same failure mode, and
// a leader that reclaims one of them silently leaks the other.
//
// This was TestReservationSweepsReclaimBothKinds and its budget half drove
// store.ReserveBudget, which had no caller outside tests: the row it planted
// was one the gateway could not produce, which is §17.1 rule 1 inverted — a
// harness supplying the value the system under test is responsible for. The
// mechanism is gone and the pairing is not, because the budget half moved to
// the job that holds the budget: a block lease, charged in full when it is
// drawn, returned by LeaseReclaimJob when its TTL passes.
//
// So both halves are still asserted, and the budget half is now planted the way
// a real node plants it — by drawing a block and dying.
func TestLeaderJobsReclaimBothKindsOfAbandonedHold(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		const (
			limit = 10 * 1_000_000_000
			ttl   = 2 * time.Second
		)

		// The capacity half: a real broker, with the background sweeper off so
		// the job is the only thing that can reclaim.
		broker := capacity.New(capacity.Config{
			Global: 4, SweepInterval: -1, ReservationTTL: ttl, Now: clk.Now,
		})
		t.Cleanup(broker.Close)
		res, ok := broker.TryAcquire(capacity.Request{Provider: "p", Model: "m"})
		if !ok || res == nil {
			t.Fatal("could not acquire a capacity reservation")
		}
		if n := broker.Snapshot().Reservations; n != 1 {
			t.Fatalf("live reservations = %d, want 1", n)
		}

		// The budget half: a node draws a block, spends a quarter of it and is
		// killed. Nobody settles the rest, and the whole block is already
		// charged to the durable counter -- which is the state a process killed
		// mid-request actually leaves behind, and the one no reservation sweep
		// could ever have reclaimed.
		key := BudgetKey("team", "team-1", quota.Monthly, clk.Now())
		dead := newLedger(t, stores[0], "node-a", clk, usd(2), ttl)
		h, err := dead.Reserve(ctx, key, limit, usd(0.5))
		if err != nil {
			t.Fatal(err)
		}
		if err := dead.Settle(h, usd(0.5)); err != nil {
			t.Fatal(err)
		}
		if err := dead.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}

		leader := newLedger(t, stores[1], "node-b", clk, usd(2), ttl)
		t.Cleanup(func() { _ = leader.Close(ctx) })
		if v, err := leader.Committed(ctx, key); err != nil || v != usd(2) {
			t.Fatalf("committed %d nano (err %v), want the whole block of %d charged "+
				"up front", v, err, usd(2))
		}

		sweep := CapacitySweepJob(broker.Sweep, time.Second)
		reclaim := LeaseReclaimJob(nil, leader, nil, time.Second, clk.Now)

		// Before either deadline, neither job may reclaim anything. A sweep
		// that is merely eager releases live holds out from under requests in
		// flight -- for the ledger that is the case measured at 190 admitted
		// against a limit of 100.
		if err := sweep.Run(ctx); err != nil {
			t.Fatalf("early capacity sweep: %v", err)
		}
		if err := reclaim.Run(ctx); err != nil {
			t.Fatalf("early lease reclaim: %v", err)
		}
		if n := broker.Snapshot().Reservations; n != 1 {
			t.Fatal("the early sweep reclaimed a live capacity reservation")
		}
		if v, _ := leader.Committed(ctx, key); v != usd(2) {
			t.Fatalf("the early reclaim took %d nano off a live block lease", usd(2)-v)
		}

		clk.Add(5 * time.Second)
		if err := sweep.Run(ctx); err != nil {
			t.Fatalf("capacity sweep: %v", err)
		}
		if err := reclaim.Run(ctx); err != nil {
			t.Fatalf("lease reclaim: %v", err)
		}
		if n := broker.Snapshot().Reservations; n != 0 {
			t.Fatalf("capacity reservations after the sweep = %d, want 0", n)
		}
		if e := broker.Snapshot().Expired; e != 1 {
			t.Fatalf("the broker recorded %d expiries, want 1", e)
		}
		// Exactly the unspent part came back. The dead node's 0.5 is still
		// charged; the 1.5 it was holding and never spent is spendable again.
		v, err := leader.Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if v != usd(0.5) {
			t.Fatalf("after reclaim the budget shows %d nano spent, want the %d the "+
				"dead node actually used", v, usd(0.5))
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
