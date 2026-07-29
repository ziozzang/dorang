package cluster

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/store"
)

// dupID is the id both processes are configured with.
//
// Every way this happens in production produces the same row: a StatefulSet
// whose template forgot the ordinal, an id baked into a container image, a
// configuration file copied to a second host. None of them is exotic and none
// of them is visible in the configuration a validator sees, because each node's
// file is individually correct.
const dupID = "dorang-0"

// twoProcessesOneID builds two independent Nodes over one database under one
// configured node id, with a leader-only job that counts its runs. Separate
// store handles, because that is what two processes are.
func twoProcessesOneID(t *testing.T, stores []*store.Store, clk *clock, ran *atomic.Int64) (*Node, *Node) {
	t.Helper()
	mk := func(s *store.Store) *Node {
		n, err := New(Config{
			Enabled:  true,
			NodeID:   dupID,
			Address:  "127.0.0.1:0",
			Version:  "test",
			Mode:     ModeSharedPG,
			Store:    s,
			LeaseTTL: 6 * time.Second,
			// An hour, so the background loop cannot fire during the test: every
			// pass below is driven explicitly, against the injected clock.
			Tick:      time.Hour,
			NodeTTL:   6 * time.Second,
			BlockSize: 16,
			Now:       clk.Now,
			Jobs: []Job{{
				Name:  "leader-only",
				Every: time.Second,
				Run: func(context.Context) error {
					ran.Add(1)
					return nil
				},
			}},
		})
		if err != nil {
			t.Fatalf("New(%s): %v", dupID, err)
		}
		t.Cleanup(func() { _ = n.Close(context.Background()) })
		return n
	}
	return mk(stores[0]), mk(stores[1])
}

// TestDuplicateNodeIDRefusesTheSecondProcess asserts what the SECOND process
// does, which is the only thing that settles this.
//
// A validator cannot: it sees one file at a time, and both files are valid. The
// fencing token cannot either, and that is worth being precise about, because
// the token was added for a failure that looks similar and is not. It closes the
// TIMING case — a stale leader whose lease expired and which does not know it —
// by advancing every time the lock changes HOLDER. Two processes claiming one id
// are, to the store, one holder renewing: the second acquire satisfies
// `capacity_leases.node_id = excluded.node_id`, so it succeeds, the token does
// not advance, and both processes then hold a token equal to the row's. Both
// pass [Election.Fenced]. The mechanism is working exactly as designed and the
// design has nothing to say about identity.
//
// What is observable is the registry: a second holder writing under the same id
// has to overwrite a row somebody is still beating on. That is where this is
// caught, and the answer is to refuse to START rather than merely to refuse to
// lead. Leadership is not the only thing the id keys — it also keys this node's
// row in `nodes`, its share of every quota lease, and its budget draw from the
// ledger — so a process that started and declined to lead would still be
// drawing budget from another process's lease block and masking its death in
// the heartbeat. Refusing to start is also the loud failure: a process that
// will not come up stops a rollout, and a process that quietly never leads is
// discovered when a sweep is missed.
func TestDuplicateNodeIDRefusesTheSecondProcess(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		var ran atomic.Int64
		first, second := twoProcessesOneID(t, stores, clk, &ran)

		if err := first.Register(ctx); err != nil {
			t.Fatalf("the first process could not register: %v", err)
		}
		if _, err := first.Election().Campaign(ctx); err != nil {
			t.Fatalf("the first process could not campaign: %v", err)
		}
		if !first.IsLeader() {
			t.Fatal("the first process did not become leader")
		}

		err := second.Start(ctx)
		if err == nil {
			t.Fatalf("the second process started under node id %q while the first "+
				"was beating on that row; both now hold the id that keys the leader "+
				"lease, every quota lease and the budget ledger draw", dupID)
		}
		if !errors.Is(err, ErrDuplicateNodeID) {
			t.Fatalf("the second process refused to start with %v, want %v",
				err, ErrDuplicateNodeID)
		}

		// It must not lead, and it must not be able to prove a term. Before this
		// was closed the second process campaigned successfully, was promoted,
		// and read back the same fencing token as the first — both 1 — so
		// Fenced() returned a valid term to both of them.
		_, _ = second.Election().Campaign(ctx)
		if second.IsLeader() {
			t.Error("the second process leads after campaigning anyway")
		}
		if _, _, ferr := second.Election().Fenced(ctx); ferr == nil {
			t.Error("the second process passed the fence check: two processes hold " +
				"a term the store cannot tell apart")
		}

		// And the first is undisturbed. A defence that costs the legitimate node
		// its leadership has replaced two leaders with none.
		if !first.IsLeader() {
			t.Error("the first process lost leadership to the refusal of the second")
		}
	})
}

// TestDuplicateNodeIDRunsEachLeaderJobOnce asserts the harm directly.
//
// §9.2: two nodes picking up the same batch pays for every finished row twice.
// The leader-only jobs are retention, partition pre-creation, the capacity and
// budget reservation sweeps, batch assignment and lease reclaim; every one of
// them is written on the promise that exactly one node runs it.
func TestDuplicateNodeIDRunsEachLeaderJobOnce(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		var ran atomic.Int64
		first, second := twoProcessesOneID(t, stores, clk, &ran)

		if err := first.Register(ctx); err != nil {
			t.Fatalf("Register(first): %v", err)
		}
		if err := first.Tick(ctx); err != nil {
			t.Fatalf("Tick(first): %v", err)
		}
		// The second process's pass is expected to refuse. What is asserted is
		// not the error but what it did NOT do.
		_ = second.Tick(ctx)

		if n := ran.Load(); n != 1 {
			t.Fatalf("the leader-only job ran %d times in one pass over a cluster of "+
				"one node; it must run once. Two processes shared node id %q, both "+
				"led, and every leader job — retention, the reservation sweeps, "+
				"batch assignment — ran twice against one database (§9.2)", n, dupID)
		}
		if got := leaders(first, second); len(got) != 1 {
			t.Fatalf("leaders = %v, want exactly one", got)
		}
	})
}

// TestDuplicateNodeIDDemotesTheIncumbentWhoseRowWasTaken is the other half, and
// it is the half a start-up check alone cannot reach.
//
// A process frozen past its heartbeat TTL — a long GC pause, a suspended
// container, a paused debugger — is indistinguishable from a dead one, so a
// successor may legitimately adopt its row and its id. When the frozen process
// resumes it is the second holder, and it is already running. It cannot refuse
// to start; what it must do is stop leading, and it must notice from the store
// rather than from its own clock, which is the same argument [Fence] makes about
// time and applied to identity.
func TestDuplicateNodeIDDemotesTheIncumbentWhoseRowWasTaken(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		var ran atomic.Int64
		frozen, successor := twoProcessesOneID(t, stores, clk, &ran)

		if err := frozen.Register(ctx); err != nil {
			t.Fatalf("Register(frozen): %v", err)
		}
		if err := frozen.Tick(ctx); err != nil {
			t.Fatalf("Tick(frozen): %v", err)
		}
		if !frozen.IsLeader() {
			t.Fatal("the first process did not become leader")
		}

		// Past the heartbeat TTL: the cluster is now entitled to call it gone.
		clk.Add(7 * time.Second)
		if err := successor.Register(ctx); err != nil {
			t.Fatalf("the successor could not adopt a lapsed row: %v", err)
		}

		// The frozen process resumes and beats. It must learn from the row that
		// its identity is no longer its own.
		err := frozen.Tick(ctx)
		if !errors.Is(err, ErrDuplicateNodeID) {
			t.Fatalf("the revived process's tick returned %v, want %v", err, ErrDuplicateNodeID)
		}
		if frozen.IsLeader() {
			t.Error("the revived process still leads under an id another process now holds")
		}
		if _, _, ferr := frozen.Election().Fenced(ctx); ferr == nil {
			t.Error("the revived process still passes the fence check")
		}
		// And it stays stopped. A node that resumed campaigning on the next pass
		// would be two leaders again one tick later.
		if err := frozen.Tick(ctx); !errors.Is(err, ErrDuplicateNodeID) {
			t.Errorf("a second tick returned %v, want a node that stays out: %v",
				err, ErrDuplicateNodeID)
		}
		if frozen.IsLeader() {
			t.Error("the revived process campaigned its way back to leadership")
		}
	})
}

// TestNodeRestartAdoptsItsOwnLapsedRow guards the case the refusal must not
// break: a node that died and came back is not a duplicate.
//
// This is the whole reason `cluster.node_id` is worth configuring at all — a
// restarted node reclaims its own leases instead of waiting out their TTL — so a
// check that made a restart fail would have removed the feature to protect it.
func TestNodeRestartAdoptsItsOwnLapsedRow(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		var ran atomic.Int64
		dead, restarted := twoProcessesOneID(t, stores, clk, &ran)

		if err := dead.Register(ctx); err != nil {
			t.Fatalf("Register: %v", err)
		}
		// It stops without draining: no Deregister, so the row outlives it. That
		// is the case the heartbeat TTL exists for.
		clk.Add(7 * time.Second)

		if err := restarted.Register(ctx); err != nil {
			t.Fatalf("a restart under its own lapsed id was refused: %v", err)
		}
		if err := restarted.Tick(ctx); err != nil {
			t.Fatalf("Tick after restart: %v", err)
		}
		if !restarted.IsLeader() {
			t.Error("the restarted node could not take leadership of a cluster of one")
		}
	})
}

// TestCleanShutdownLetsTheIDBeReusedAtOnce pins the other end of it: a node that
// drained removed its row, so its replacement does not wait out a TTL.
func TestCleanShutdownLetsTheIDBeReusedAtOnce(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		var ran atomic.Int64
		old, replacement := twoProcessesOneID(t, stores, clk, &ran)

		if err := old.Register(ctx); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if err := old.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := replacement.Register(ctx); err != nil {
			t.Fatalf("the replacement was refused after a clean drain: %v", err)
		}
	})
}

// TestDeregisterDoesNotRemoveAnotherProcessRow is the smaller edge of the same
// identity question: `nodes` is keyed by node id, so a DELETE scoped by id alone
// is a DELETE of whoever holds it — including a node that legitimately took the
// id over.
func TestDeregisterDoesNotRemoveAnotherProcessRow(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		a, err := NewRegistry(stores[0], dupID, 6*time.Second, clk.Now)
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewRegistry(stores[1], dupID, 6*time.Second, clk.Now)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Register(ctx, NodeInfo{Address: "10.0.0.1:8080"}); err != nil {
			t.Fatal(err)
		}
		clk.Add(7 * time.Second)
		if err := b.Register(ctx, NodeInfo{Address: "10.0.0.2:8080"}); err != nil {
			t.Fatalf("the successor could not adopt a lapsed row: %v", err)
		}

		if err := a.Deregister(ctx); err != nil {
			t.Fatalf("Deregister: %v", err)
		}
		all, err := b.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 1 || all[0].Address != "10.0.0.2:8080" {
			t.Fatalf("registry after the superseded process drained = %+v; it deleted "+
				"the row of the process that now holds the id", all)
		}
	})
}
