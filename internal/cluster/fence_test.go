package cluster

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// The schedule these tests are built around, and where the numbers come from.
//
// A 15s lease with the default guard band of a third leaves a 5s guard. Before
// the safe window was derived from the granted lease, [Election.Campaign]
// computed it as `now + ttl - guard` with `now` read AFTER the acquire returned,
// so the safe window ended `acquire latency - guard` past the lease itself. At
// an 8s acquire that is three seconds during which the incumbent believes it
// leads and the lease it holds has already been taken by somebody else.
//
// Nothing here perturbs a clock. Both nodes read the same instant throughout;
// the defect needed no skew at all, which is what made the guard band the wrong
// place to have been relying on.
const (
	fenceTTL     = 15 * time.Second
	fenceGuard   = fenceTTL / 3 // 5s, the default
	fenceLatency = 8 * time.Second
)

// slowLock is a Lock whose Acquire costs d of wall clock, which is what a store
// under load looks like to a campaigner. It advances the shared test clock
// rather than sleeping, so the schedule is exact rather than approximate.
type slowLock struct {
	Lock
	clk *clock
	d   time.Duration
}

func (s slowLock) Acquire(ctx context.Context, ttl time.Duration) (bool, uint64, time.Time, error) {
	held, fence, exp, err := s.Lock.Acquire(ctx, ttl)
	s.clk.Add(s.d)
	return held, fence, exp, err
}

// TestSlowAcquireDoesNotProduceTwoLeaders is the guard band, measured against
// the thing it does not bound.
//
// The observable is deliberately not "safeUntil is expires_at minus the guard".
// That is arithmetic, and arithmetic held perfectly well while two nodes ran the
// reservation sweep, the retention pass and the lease reclaim side by side --
// DESIGN §9.2 prices the last of those at every finished row paid for twice.
// What is asserted is that at no instant of the schedule do two nodes believe
// they lead, and that only one node's leader-owned write reaches the store.
func TestSlowAcquireDoesNotProduceTwoLeaders(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()

		mk := func(s *store.Store, id string, latency time.Duration) *Election {
			t.Helper()
			l, err := NewLock(s, LeaderLockName, id, clk.Now)
			if err != nil {
				t.Fatal(err)
			}
			var lock Lock = l
			if latency > 0 {
				lock = slowLock{Lock: l, clk: clk, d: latency}
			}
			e, err := NewElection(ElectionConfig{
				Lock: lock, NodeID: id, TTL: fenceTTL, Now: clk.Now,
			})
			if err != nil {
				t.Fatal(err)
			}
			return e
		}
		a := mk(stores[0], "node-a", fenceLatency)
		b := mk(stores[1], "node-b", 0)

		if lead, err := a.Campaign(ctx); err != nil || !lead {
			t.Fatalf("node-a Campaign = %v, %v; want the first campaigner to win", lead, err)
		}

		// node-a never campaigns again: it is the node whose store went slow, so
		// its next renewal is the one that has not landed yet. node-b campaigns
		// every half second across the whole lease and well past it.
		var overlap time.Duration
		var wrote []string
		start := clk.Now()
		for i := 0; i < 40; i++ {
			clk.Add(500 * time.Millisecond)
			if _, err := b.Campaign(ctx); err != nil {
				t.Fatal(err)
			}
			if a.IsLeader() && b.IsLeader() {
				overlap += 500 * time.Millisecond
			}
			// The observable that costs money: whose leader-owned write is
			// accepted. A node that believes it leads but has been superseded
			// must be refused by the fence, not merely be unlucky.
			for _, e := range []*Election{a, b} {
				if _, f, err := e.Fenced(ctx); err == nil {
					wrote = append(wrote, e.NodeID()+"@"+clk.Now().Sub(start).String()+" fence "+
						itoa(f.Token()))
				}
			}
		}

		if overlap != 0 {
			t.Fatalf("%v of simultaneous leadership with both clocks reading identically: "+
				"an acquire of %v against a guard band of %v", overlap, fenceLatency, fenceGuard)
		}
		// And at every instant at most one node got a fence, so at most one
		// node's reclaim, sweep or batch assignment could have run.
		seen := map[string]int{}
		for _, w := range wrote {
			seen[w[:6]]++
		}
		if len(wrote) == 0 {
			t.Fatal("nobody was ever allowed to do leader work; the schedule proved nothing")
		}
		t.Logf("fenced leader work admitted: %v", seen)
	})
}

// TestASupersededLeaderIsFencedOutOfItsWrites is the case arithmetic cannot
// reach.
//
// Deriving the safe window from the granted lease closes the latency half of the
// guard band by construction. It does not close, and nothing on unsynchronised
// clocks can close, a node whose clock disagrees with its successor's by more
// than the guard band: node-a's clock runs behind, so its safe window has not
// ended when node-b has already, correctly, taken the lease. Both believe they
// lead. That is exactly what the fencing token is for -- it moved when the lock
// changed hands, and seeing that requires no agreement about "now" at all.
//
// The assertion is on the write, not on the belief: node-a still thinks it is
// the leader afterwards, and its reclaim returns nobody's units.
func TestASupersededLeaderIsFencedOutOfItsWrites(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		const skew = 8 * time.Second // wider than the 5s guard band

		behind := &clock{t: clk.Now().Add(-skew)}
		mk := func(s *store.Store, id string, now func() time.Time) *Election {
			t.Helper()
			l, err := NewLock(s, LeaderLockName, id, now)
			if err != nil {
				t.Fatal(err)
			}
			e, err := NewElection(ElectionConfig{Lock: l, NodeID: id, TTL: fenceTTL, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			return e
		}
		a := mk(stores[0], "node-a", behind.Now)
		b := mk(stores[1], "node-b", clk.Now)

		if lead, err := a.Campaign(ctx); err != nil || !lead {
			t.Fatalf("node-a Campaign = %v, %v", lead, err)
		}
		// node-a wrote a lease that, read on node-b's clock, expires almost at
		// once. node-b takes it; node-a is still inside its own safe window and
		// has no way to know.
		clk.Add(fenceTTL - skew + time.Second)
		behind.Add(fenceTTL - skew + time.Second)
		if lead, err := b.Campaign(ctx); err != nil || !lead {
			t.Fatalf("node-b Campaign = %v, %v; the schedule did not produce a handover", lead, err)
		}
		if !a.IsLeader() {
			// The schedule is arithmetic on a fake clock, so this cannot drift.
			// If it ever does, the test has stopped reaching the case the fence
			// exists for and must say so rather than pass.
			t.Fatal("the schedule no longer produces two believers; the fence has " +
				"nothing to reject and this test proves nothing")
		}

		// Two believers. Now the observable.
		if _, _, err := b.Fenced(ctx); err != nil {
			t.Fatalf("the node that actually holds the lease was refused: %v", err)
		}
		_, _, err := a.Fenced(ctx)
		if !errors.Is(err, ErrLeadershipLost) {
			t.Fatalf("the superseded leader was allowed to do leader work: %v", err)
		}
		// And being refused demotes it, so anything already running under its
		// term is cancelled rather than left to finish beside the successor.
		if a.IsLeader() {
			t.Fatal("a node the store refused still believes it leads")
		}
	})
}

// TestAFencedWriteIsRefusedInsideItsOwnTransaction is the difference between a
// check and a fence.
//
// [Election.Fenced] verifies at dispatch, which leaves the window between the
// check and the write. [Fence.In] puts the assertion in the writer's own
// transaction, serialized against the election's compare-and-swap by the same
// advisory lock, so a handover cannot land in between. This asserts the second:
// a reclaim carrying a superseded token returns nothing and reports
// ErrLeadershipLost, with the lease row untouched.
func TestAFencedWriteIsRefusedInsideItsOwnTransaction(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		const limit = int64(1_000)
		key := QuotaKey("credential", "c-1", quota.Rolling(time.Hour), quota.MetricRequests, clk.Now())

		// A peer draws a block and then stops: a crash. Its lease is expired, so
		// the reclaim below is legitimate on every ground except the term.
		ghost := newLedger(t, stores[0], "node-ghost", clk, 100, 30*time.Second)
		h, err := ghost.Reserve(ctx, key, limit, 10)
		if err != nil {
			t.Fatal(err)
		}
		if err := ghost.Settle(h, 10); err != nil {
			t.Fatal(err)
		}
		if err := ghost.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		clk.Add(time.Minute)

		lock, err := NewLock(stores[1], LeaderLockName, "node-b", clk.Now)
		if err != nil {
			t.Fatal(err)
		}
		held, token, _, err := lock.Acquire(ctx, fenceTTL)
		if err != nil || !held {
			t.Fatalf("Acquire = %v, %v", held, err)
		}
		leader := newLedger(t, stores[1], "node-b", clk, 100, 30*time.Second)
		t.Cleanup(func() { _ = leader.Close(ctx) })

		before, err := leader.Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}

		// A token from a term that has been superseded. Nothing else about this
		// call is wrong: the node id is right, the lease is expired, the reclaim
		// is due.
		stale := lock.Fence(token - 1)
		res, err := leader.ReclaimExpired(WithFence(ctx, stale), clk.Now())
		if !errors.Is(err, ErrLeadershipLost) {
			t.Fatalf("a reclaim under a stale fence returned %v, want ErrLeadershipLost", err)
		}
		if res.Leases != 0 || res.Returned != 0 {
			t.Fatalf("the fenced-out reclaim still moved %d units across %d leases",
				res.Returned, res.Leases)
		}
		after, err := leader.Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("a fenced-out reclaim moved the counter from %d to %d", before, after)
		}

		// The current term does the same work and it lands, so the refusal above
		// was about the token and not about the reclaim being impossible.
		res, err = leader.ReclaimExpired(WithFence(ctx, lock.Fence(token)), clk.Now())
		if err != nil {
			t.Fatalf("the current term's reclaim was refused: %v", err)
		}
		if res.Leases != 1 || res.Returned != 90 {
			t.Fatalf("reclaim returned %d units across %d leases, want 90 across 1",
				res.Returned, res.Leases)
		}
	})
}

// TestAHandoverMidPassStopsTheRestOfThePass is the same property one layer up,
// where the cost actually is: DESIGN §9.2 on batch assignment, "two nodes
// picking up the same batch pays for every finished row twice". A [Job] is a
// black box to this package, so what is asserted is that one is never dispatched
// under a term that has moved on.
//
// The schedule is the one case a leader can reach a dispatch while superseded,
// and it is not exotic. A pass campaigns once and then runs its jobs in order;
// a node whose clock STOPS during that pass -- a suspended container, a VM
// paused for migration, which DESIGN §13 names -- comes back with a safe window
// that has not moved while the rest of the cluster's has. Its campaign is
// behind it, so nothing between the jobs will tell it anything, and every
// remaining job in the pass runs beside the successor's copy.
//
// Between the jobs is where the fencing token used to be read and thrown away.
func TestAHandoverMidPassStopsTheRestOfThePass(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		// Two clocks that start together. node-a's stops; node-b's does not.
		aClock := &clock{t: clk.Now()}
		bClock := &clock{t: clk.Now()}

		successor, err := NewLock(stores[1], LeaderLockName, "node-b", bClock.Now)
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewElection(ElectionConfig{
			Lock: successor, NodeID: "node-b", TTL: fenceTTL, Now: bClock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}

		var first, second atomic.Int64
		takeover := Job{Name: "first", Every: time.Nanosecond, Run: func(context.Context) error {
			first.Add(1)
			// node-a's clock is stopped for the duration of this job. The rest
			// of the cluster moves on, node-a's lease lapses as far as anybody
			// else can see, and node-b legitimately takes it.
			bClock.Add(fenceTTL + time.Second)
			held, err := b.Campaign(ctx)
			if err != nil {
				return err
			}
			if !held {
				t.Error("node-b did not take the lapsed lease; the schedule is wrong")
			}
			return nil
		}}
		counter := Job{Name: "second", Every: time.Nanosecond, Run: func(context.Context) error {
			second.Add(1)
			return nil
		}}

		a, err := New(Config{
			Enabled: true, NodeID: "node-a", Mode: ModeSharedPG, Store: stores[0],
			LeaseTTL: fenceTTL, Tick: time.Second, NodeTTL: time.Minute,
			Jobs: []Job{takeover, counter}, Now: aClock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Register(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close(context.Background()) })

		// One pass. node-a campaigns while it still holds everything, runs the
		// first job, and loses the lease inside it.
		if err := a.Tick(ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if first.Load() != 1 {
			t.Fatalf("the first job ran %d times, want 1: the schedule never started", first.Load())
		}
		if _, _, held := b.Leader(); !held {
			t.Fatal("node-b is not the leader; nothing was superseded")
		}
		if n := second.Load(); n != 0 {
			t.Fatalf("the superseded leader ran %d more jobs of the pass after the lock "+
				"changed hands, alongside the node that now holds it", n)
		}
		// And node-a knows: being refused by the store demotes it, so anything
		// still running under its term is cancelled rather than left to finish.
		if a.IsLeader() {
			t.Fatal("node-a still believes it leads after the store refused its term")
		}
	})
}

// itoa avoids pulling strconv in for one label.
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
