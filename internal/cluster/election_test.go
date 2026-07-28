package cluster

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/store"
)

// TestTwoNodesElectExactlyOneLeader is scenario 11 of DESIGN 14's first half:
// two nodes, one leader, and no interruption when one goes away.
func TestTwoNodesElectExactlyOneLeader(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		a := testNode(t, stores[0], "node-a", clk, nil)
		b := testNode(t, stores[1], "node-b", clk, nil)

		tick(t, a, b)

		got := leaders(a, b)
		if len(got) != 1 {
			t.Fatalf("after one round, leaders = %v, want exactly one", got)
		}
		lead, follow := a, b
		if got[0] == b.ID() {
			lead, follow = b, a
		}

		// Ticking repeatedly must not change the answer. A leader that has to
		// re-win every round has no lease, only luck.
		for i := 0; i < 5; i++ {
			clk.Add(time.Second)
			tick(t, a, b)
			if !lead.IsLeader() {
				t.Fatalf("round %d: the leader lost leadership while renewing", i)
			}
			if follow.IsLeader() {
				t.Fatalf("round %d: the follower became a second leader", i)
			}
		}

		// The store agrees, which is the only opinion that matters when the two
		// nodes disagree.
		holder, _, fence, err := lead.Election().Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if holder != lead.ID() {
			t.Fatalf("the store says the leader is %q, the node says %q", holder, lead.ID())
		}
		if fence == 0 {
			t.Fatal("the fencing token is zero; a token that never moves fences nothing")
		}
	})
}

// TestKillingTheLeaderPromotesTheOther kills the leader by simply not ticking
// it -- which is what a crashed process looks like to everybody else -- and
// requires the survivor to take over once the lease expires.
func TestKillingTheLeaderPromotesTheOther(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		a := testNode(t, stores[0], "node-a", clk, nil)
		b := testNode(t, stores[1], "node-b", clk, nil)

		tick(t, a, b)
		got := leaders(a, b)
		if len(got) != 1 {
			t.Fatalf("leaders = %v, want one", got)
		}
		dead, alive := a, b
		if got[0] == b.ID() {
			dead, alive = b, a
		}

		// Capture the leader-scoped context before the leader dies. It must end
		// up cancelled, and with a cause that says why.
		//
		// The term is read from Term(). Leader()'s second value is the FENCING
		// TOKEN, not the term: the two happen to both be 1 on a first election,
		// which is why binding one to a variable called `term` went unnoticed
		// here until the token started being used for something.
		lctx, _, ok := dead.Election().Leader()
		if !ok {
			t.Fatal("the leader has no leader context")
		}
		term := dead.Election().Term()

		// The lease is 6s. Before it expires nobody may take it: a successor
		// elected early is two leaders, which is the failure this whole
		// mechanism exists to prevent.
		clk.Add(3 * time.Second)
		tick(t, alive)
		if alive.IsLeader() {
			t.Fatal("the survivor took leadership while the incumbent's lease was still valid")
		}

		clk.Add(4 * time.Second) // now past the 6s lease
		tick(t, alive)
		if !alive.IsLeader() {
			t.Fatalf("the survivor did not take over after the lease expired")
		}

		// The dead node has not run any code at all, so nothing has told it it
		// was demoted. Asking is enough: leadership expires from its own clock.
		if dead.IsLeader() {
			t.Fatal("the killed leader still believes it leads")
		}
		select {
		case <-lctx.Done():
			if cause := context.Cause(lctx); !errors.Is(cause, ErrLeadershipLost) {
				t.Fatalf("leader context cancelled with %v, want ErrLeadershipLost", cause)
			}
		default:
			t.Fatal("the killed leader's context was never cancelled")
		}
		if dead.Election().Term() != term {
			t.Fatalf("the term moved without a promotion")
		}
	})
}

// TestRevivedLeaderStopsCleanly is the case no store round trip can catch: a
// process that was frozen -- a long GC pause, a suspended container, a paused
// debugger -- and resumes still believing it leads. It must step down on the
// way back rather than run a retention pass alongside its successor.
func TestRevivedLeaderStopsCleanly(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		var jobRuns atomic.Int64
		job := Job{
			Name:  "counter",
			Every: time.Second,
			Run: func(context.Context) error {
				jobRuns.Add(1)
				return nil
			},
		}
		a := testNode(t, stores[0], "node-a", clk, func(c *Config) { c.Jobs = []Job{job} })
		b := testNode(t, stores[1], "node-b", clk, func(c *Config) { c.Jobs = []Job{job} })

		tick(t, a, b)
		got := leaders(a, b)
		if len(got) != 1 {
			t.Fatalf("leaders = %v, want one", got)
		}
		old, next := a, b
		if got[0] == b.ID() {
			old, next = b, a
		}
		before := jobRuns.Load()
		if before == 0 {
			t.Fatal("the leader ran no jobs")
		}

		// Freeze the old leader and let the survivor take over.
		clk.Add(8 * time.Second)
		tick(t, next)
		if !next.IsLeader() {
			t.Fatal("the survivor did not take over")
		}
		afterHandover := jobRuns.Load()

		// The old leader thaws. Its very first act must be to notice it is not
		// the leader; it must not run a single leader job.
		if err := old.Tick(ctx); err != nil {
			t.Fatalf("the revived node failed its tick: %v", err)
		}
		if old.IsLeader() {
			t.Fatal("the revived leader re-took leadership from a live incumbent")
		}
		if n := jobRuns.Load(); n != afterHandover {
			t.Fatalf("the revived leader ran %d leader jobs after being superseded", n-afterHandover)
		}

		// And the incumbent is undisturbed by the revival.
		if !next.IsLeader() {
			t.Fatal("the revival cost the incumbent its leadership")
		}
	})
}

// TestDemotionMidTaskCancels proves the leader-scoped context reaches the job:
// a task that is running when leadership is lost is cancelled, not left to
// finish under a successor.
func TestDemotionMidTaskCancels(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()

		started := make(chan struct{})
		var sawCancel atomic.Bool
		blocking := Job{
			Name:  "blocking",
			Every: time.Second,
			Run: func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				sawCancel.Store(true)
				return ctx.Err()
			},
		}
		n := testNode(t, s, "node-a", clk, func(c *Config) { c.Jobs = []Job{blocking} })

		done := make(chan error, 1)
		go func() { done <- n.Tick(ctx) }()

		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the leader job never started")
		}

		// Resign while the job is in flight.
		if err := n.Election().Resign(ctx); err != nil {
			t.Fatalf("Resign: %v", err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the tick did not return after leadership was lost")
		}
		if !sawCancel.Load() {
			t.Fatal("the in-flight leader job was never cancelled")
		}
	})
}

// TestLeadershipExpiresWithoutTheStore removes the store from the picture
// entirely. Leadership must end on this node's own clock, because the case it
// has to survive -- a node that is not running -- is one in which no store
// access is possible by definition.
func TestLeadershipExpiresWithoutTheStore(t *testing.T) {
	clk := newClock(epoch)
	lock := &fakeLock{holder: "node-a", now: clk.Now}
	el, err := NewElection(ElectionConfig{
		Lock: lock, NodeID: "node-a", TTL: 9 * time.Second, Guard: 3 * time.Second, Now: clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if leader, err := el.Campaign(context.Background()); err != nil || !leader {
		t.Fatalf("Campaign = %v, %v; want leader", leader, err)
	}

	// Inside the safe window: still the leader.
	clk.Add(5 * time.Second)
	if !el.IsLeader() {
		t.Fatal("leadership ended inside the safe window")
	}

	// Past the safe window but before the lease itself expires. Stepping down
	// here is the guard band, and it is the point: a node that waited for the
	// lease to expire would overlap its successor by however long its last task
	// takes to notice.
	clk.Add(2 * time.Second) // t+7s of a 9s lease, guard 3s => safe until t+6s
	if el.IsLeader() {
		t.Fatal("leadership survived past the guard band; the successor's window overlaps")
	}
	if lock.acquires != 1 {
		t.Fatalf("expiring leadership took %d store round trips, want 0 beyond the first", lock.acquires-1)
	}
}

// TestFenceAdvancesOnlyOnHandover checks the fencing token means what the
// comment says: it moves when the lock changes hands and never otherwise, so a
// stale token is recognisable and a renewal does not look like a takeover.
func TestFenceAdvancesOnlyOnHandover(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		a := testNode(t, stores[0], "node-a", clk, nil)
		b := testNode(t, stores[1], "node-b", clk, nil)

		tick(t, a, b)
		got := leaders(a, b)
		lead, other := a, b
		if got[0] == b.ID() {
			lead, other = b, a
		}
		first := lead.Election().Fence().Token()

		for i := 0; i < 3; i++ {
			clk.Add(time.Second)
			tick(t, lead)
		}
		if f := lead.Election().Fence().Token(); f != first {
			t.Fatalf("the fence moved from %d to %d across renewals; a renewal is not a handover", first, f)
		}

		clk.Add(10 * time.Second)
		tick(t, other)
		if !other.IsLeader() {
			t.Fatal("no handover happened")
		}
		if f := other.Election().Fence().Token(); f <= first {
			t.Fatalf("the fence is %d after a handover, want more than %d", f, first)
		}
	})
}

// TestElectionRefusesABadGuard rejects a guard band that is not shorter than
// the lease, which would end leadership before it began.
func TestElectionRefusesABadGuard(t *testing.T) {
	_, err := NewElection(ElectionConfig{
		Lock: &fakeLock{now: time.Now}, NodeID: "n", TTL: time.Second, Guard: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "guard") {
		t.Fatalf("NewElection with guard == TTL returned %v, want a refusal", err)
	}
}

// TestConcurrentCampaignsElectOne runs many campaigns at once against one lock.
// Under -race this is where a read-then-write election would be caught.
func TestConcurrentCampaignsElectOne(t *testing.T) {
	eachCluster(t, 4, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		nodes := make([]*Node, len(stores))
		for i := range stores {
			nodes[i] = testNode(t, stores[i], "node-"+string(rune('a'+i)), clk, nil)
		}

		var wg sync.WaitGroup
		for _, n := range nodes {
			wg.Add(1)
			go func(n *Node) {
				defer wg.Done()
				for i := 0; i < 5; i++ {
					if _, err := n.Election().Campaign(ctx); err != nil {
						t.Errorf("Campaign(%s): %v", n.ID(), err)
						return
					}
				}
			}(n)
		}
		wg.Wait()

		if got := leaders(nodes...); len(got) != 1 {
			t.Fatalf("after concurrent campaigns, leaders = %v, want exactly one", got)
		}
	})
}

// fakeLock is a Lock with no store behind it, for the assertions that are about
// the election's own clock rather than about persistence.
//
// It returns a nil Fence, which is this package's way of saying "this lock
// cannot express a precondition". That is the honest answer for a lock with no
// row to assert against, and it is why the fencing assertions below use the
// real [NewLock] instead.
type fakeLock struct {
	mu       sync.Mutex
	holder   string
	expires  time.Time
	fence    uint64
	acquires int
	now      func() time.Time
}

func (f *fakeLock) Acquire(_ context.Context, ttl time.Duration) (bool, uint64, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquires++
	now := f.now()
	if f.holder != "" && f.holder != "node-a" && now.Before(f.expires) {
		return false, f.fence, f.expires, nil
	}
	if f.fence == 0 {
		f.fence = 1
	}
	f.holder = "node-a"
	f.expires = now.Add(ttl)
	return true, f.fence, f.expires, nil
}

func (f *fakeLock) Fence(uint64) *Fence { return nil }

func (f *fakeLock) Release(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holder = ""
	return nil
}

func (f *fakeLock) Holder(context.Context) (string, time.Time, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.holder, f.expires, f.fence, nil
}
