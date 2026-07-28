package cluster

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/store"
)

// TestStartAndCloseRunTheLoop covers the background loop, which the
// deterministic tests bypass by calling Tick directly. Real deployments do not
// call Tick, so something has to.
func TestStartAndCloseRunTheLoop(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		var ran atomic.Int64
		n, err := New(Config{
			Enabled: true, NodeID: "node-a", Mode: ModeSharedPG, Store: s,
			LeaseTTL: 3 * time.Second, Tick: 10 * time.Millisecond, NodeTTL: 3 * time.Second,
			Jobs: []Job{{Name: "counter", Every: time.Nanosecond, Run: func(context.Context) error {
				ran.Add(1)
				return nil
			}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := n.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}

		// A second Start would run two heartbeat loops under one node id.
		if err := n.Start(ctx); err == nil {
			t.Fatal("Start twice was accepted")
		}

		deadline := time.Now().Add(3 * time.Second)
		for ran.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if ran.Load() == 0 {
			t.Fatal("the loop never ran a leader job")
		}
		if !n.IsLeader() {
			t.Fatal("the only node never became leader")
		}

		if err := n.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// Close waits for the loop, so nothing may run afterwards.
		settled := ran.Load()
		time.Sleep(50 * time.Millisecond)
		if got := ran.Load(); got != settled {
			t.Fatalf("%d leader jobs ran after Close returned", got-settled)
		}
		if err := n.Start(ctx); !errors.Is(err, ErrClosed) {
			t.Fatalf("Start after Close = %v, want ErrClosed", err)
		}

		// Draining removes the registry row and releases the lease, so a
		// successor does not have to wait out a TTL (DESIGN 13).
		nodes, err := n.Registry().List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) != 0 {
			t.Fatalf("a drained node left %d registry rows behind", len(nodes))
		}
		// The lease is released rather than left to lapse, so a successor can
		// take it at once. Asserting that a fresh contender wins is the
		// clock-independent way to say it: this node runs on real time while
		// the harness store is on a fake clock, and comparing the two would
		// test the fixture.
		lock, err := NewLock(s, LeaderLockName, "node-successor", time.Now)
		if err != nil {
			t.Fatal(err)
		}
		held, _, _, err := lock.Acquire(ctx, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !held {
			t.Fatal("a drained node left its leadership lease held")
		}
	})
}

// TestDrainingLetsASuccessorTakeOverImmediately is the point of resigning
// rather than merely stopping: a planned restart must not cost the cluster a
// lease TTL of leaderless time.
func TestDrainingLetsASuccessorTakeOverImmediately(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		a := testNode(t, stores[0], "node-a", clk, nil)
		b := testNode(t, stores[1], "node-b", clk, nil)
		tick(t, a, b)

		lead, other := a, b
		if b.IsLeader() {
			lead, other = b, a
		}
		if err := lead.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// No clock advance at all: the lease was released, not left to lapse.
		tick(t, other)
		if !other.IsLeader() {
			t.Fatal("a drained leader made its successor wait out the lease TTL")
		}
	})
}

// TestJobStatsRecordFailures checks that a failing leader job is visible rather
// than swallowed, and that one failing job does not stop the others.
func TestJobStatsRecordFailures(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		boom := errors.New("boom")
		var later atomic.Int64
		n := testNode(t, s, "node-a", clk, func(c *Config) {
			c.Jobs = []Job{
				{Name: "fails", Every: time.Nanosecond, Run: func(context.Context) error { return boom }},
				{Name: "after", Every: time.Nanosecond, Run: func(context.Context) error {
					later.Add(1)
					return nil
				}},
			}
		})
		err := n.Tick(ctx)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("Tick = %v, want the job failure surfaced", err)
		}
		if later.Load() == 0 {
			t.Fatal("one failing job stopped the rest of the pass")
		}
		st := n.JobStats()
		if st["fails"].Failures != 1 || st["fails"].Runs != 1 {
			t.Fatalf("stats for the failing job = %+v", st["fails"])
		}
		if st["after"].Failures != 0 || st["after"].Runs != 1 {
			t.Fatalf("stats for the healthy job = %+v", st["after"])
		}
	})
}

// TestJobsAreLeaderOnly: a follower must run none of them. Two nodes both
// running retention and batch assignment is the failure clustering introduces
// and leadership exists to remove.
func TestJobsAreLeaderOnly(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		var aRuns, bRuns atomic.Int64
		mk := func(c *atomic.Int64) Job {
			return Job{Name: "counter", Every: time.Nanosecond, Run: func(context.Context) error {
				c.Add(1)
				return nil
			}}
		}
		a := testNode(t, stores[0], "node-a", clk, func(c *Config) { c.Jobs = []Job{mk(&aRuns)} })
		b := testNode(t, stores[1], "node-b", clk, func(c *Config) { c.Jobs = []Job{mk(&bRuns)} })

		for i := 0; i < 4; i++ {
			clk.Add(time.Second)
			tick(t, a, b)
		}
		lead, follow := aRuns.Load(), bRuns.Load()
		if a.IsLeader() != (lead > 0) || b.IsLeader() != (bRuns.Load() > 0) {
			t.Fatalf("job runs do not match leadership: a=%d (leader %v), b=%d (leader %v)",
				lead, a.IsLeader(), follow, b.IsLeader())
		}
		if lead > 0 && follow > 0 {
			t.Fatalf("both nodes ran leader jobs: %d and %d", lead, follow)
		}
		if lead == 0 && follow == 0 {
			t.Fatal("no node ran any leader job")
		}
	})
}

// TestAccuracyUsesTheLiveNodeCount: a published overshoot figure computed from
// a node count nobody checked is a figure that describes an imagined cluster.
//
// The BlockSize set alongside LeaseBlock is deliberately a different number,
// and a large one, because it stands for the case that made the two fields
// separate: internal/app draws budget in nano-USD, so the ledger's block is
// tens of millions. If the published concurrency figure came from it, an
// operator sizing a fleet against DESIGN 5.6's number would read a bound eight
// million times too large, in units nobody stated.
func TestAccuracyUsesTheLiveNodeCount(t *testing.T) {
	eachCluster(t, 3, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		nodes := make([]*Node, 3)
		for i := range stores {
			nodes[i] = testNode(t, stores[i], "node-"+string(rune('a'+i)), clk, func(c *Config) {
				c.Mode = ModeLeased
				c.LeaseBlock = 8
				c.BlockSize = 50_000_000
			})
		}
		tick(t, nodes...)

		a, err := nodes[0].Accuracy(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if a.MaxOvershoot != 8*2 {
			t.Fatalf("with three live nodes the figure is %d, want lease block x 2 = 16", a.MaxOvershoot)
		}
		// The ledger's own draw must not be visible here at all.
		if strings.Contains(a.Formula, "50000000") {
			t.Fatalf("the published formula %q is computed from the ledger's block, "+
				"which is denominated in the counter's units and not the metric's", a.Formula)
		}

		// Two nodes go away. The figure has to follow.
		clk.Add(time.Minute)
		if err := nodes[0].Registry().Heartbeat(ctx, true); err != nil {
			t.Fatal(err)
		}
		a, err = nodes[0].Accuracy(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if a.MaxOvershoot != 0 {
			t.Fatalf("with one live node the figure is %d, want 0", a.MaxOvershoot)
		}
	})
}
