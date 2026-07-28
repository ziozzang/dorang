package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/store"
)

func TestRegistryRecordsAndBeats(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		r, err := NewRegistry(s, "node-a", 10*time.Second, clk.Now)
		if err != nil {
			t.Fatal(err)
		}
		start := clk.Now()
		if err := r.Register(ctx, NodeInfo{Address: "10.0.0.1:8080", Version: "v1.2.3"}); err != nil {
			t.Fatal(err)
		}

		all, err := r.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 1 {
			t.Fatalf("registered nodes = %d, want 1", len(all))
		}
		got := all[0]
		if got.ID != "node-a" || got.Address != "10.0.0.1:8080" || got.Version != "v1.2.3" {
			t.Fatalf("registry row = %+v", got)
		}
		if !got.StartedAt.Equal(start) {
			t.Fatalf("started_at = %v, want %v", got.StartedAt, start)
		}
		if !got.Alive(clk.Now(), r.NodeTTL()) {
			t.Fatal("a node is not alive immediately after registering")
		}

		// A heartbeat moves last_heartbeat and publishes the leadership claim.
		clk.Add(3 * time.Second)
		if err := r.Heartbeat(ctx, true); err != nil {
			t.Fatal(err)
		}
		all, _ = r.List(ctx)
		if !all[0].IsLeader {
			t.Fatal("the leadership claim was not published")
		}
		if !all[0].LastHeartbeat.Equal(clk.Now()) {
			t.Fatalf("last_heartbeat = %v, want %v", all[0].LastHeartbeat, clk.Now())
		}
	})
}

// TestRegistryDeclaresALapsedNodeDead is what makes a lease reclaimable at all:
// something has to decide the node is gone.
func TestRegistryDeclaresALapsedNodeDead(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		const ttl = 10 * time.Second
		a, _ := NewRegistry(stores[0], "node-a", ttl, clk.Now)
		b, _ := NewRegistry(stores[1], "node-b", ttl, clk.Now)
		if err := a.Register(ctx, NodeInfo{}); err != nil {
			t.Fatal(err)
		}
		if err := b.Register(ctx, NodeInfo{}); err != nil {
			t.Fatal(err)
		}

		live, err := b.Live(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(live) != 2 {
			t.Fatalf("live nodes = %d, want 2", len(live))
		}

		// A single missed beat must not do it. Declaring a node dead returns
		// its leases and redistributes its share, which is not free, and a GC
		// pause is not a death.
		clk.Add(ttl - time.Second)
		if err := b.Heartbeat(ctx, false); err != nil {
			t.Fatal(err)
		}
		dead, err := b.Dead(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(dead) != 0 {
			t.Fatalf("dead nodes = %v just short of the TTL", dead)
		}

		clk.Add(2 * time.Second)
		if err := b.Heartbeat(ctx, false); err != nil {
			t.Fatal(err)
		}
		dead, _ = b.Dead(ctx)
		if len(dead) != 1 || dead[0].ID != "node-a" {
			t.Fatalf("dead nodes = %v, want just node-a", dead)
		}
		if n, err := b.Count(ctx); err != nil || n != 1 {
			t.Fatalf("live count = %d, %v; want 1", n, err)
		}
	})
}

// TestHeartbeatFromAPrunedNodeIsAnError catches the failure the silent path
// hides: a node whose row was pruned while it believed itself alive would go on
// holding leases that nobody counts it as holding.
func TestHeartbeatFromAPrunedNodeIsAnError(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		r, _ := NewRegistry(s, "node-a", time.Minute, clk.Now)
		if err := r.Heartbeat(ctx, false); !errors.Is(err, ErrNotRegistered) {
			t.Fatalf("Heartbeat before Register = %v, want ErrNotRegistered", err)
		}
		if err := r.Register(ctx, NodeInfo{}); err != nil {
			t.Fatal(err)
		}
		if err := r.Heartbeat(ctx, false); err != nil {
			t.Fatal(err)
		}
		if err := r.Deregister(ctx); err != nil {
			t.Fatal(err)
		}
		if err := r.Heartbeat(ctx, false); !errors.Is(err, ErrNotRegistered) {
			t.Fatalf("Heartbeat after Deregister = %v, want ErrNotRegistered", err)
		}
	})
}

// TestPruneKeepsTheRowPastTheTTL checks the deliberate lag: the lease reclaim
// happens on the TTL, but the row survives longer so an operator can still see
// which node failed.
func TestPruneKeepsTheRowPastTheTTL(t *testing.T) {
	eachCluster(t, 2, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		const ttl = 10 * time.Second
		a, _ := NewRegistry(stores[0], "node-a", ttl, clk.Now)
		b, _ := NewRegistry(stores[1], "node-b", ttl, clk.Now)
		_ = a.Register(ctx, NodeInfo{})
		_ = b.Register(ctx, NodeInfo{})

		clk.Add(2 * ttl)
		_ = b.Heartbeat(ctx, true)

		if n, err := b.Prune(ctx, 5*ttl); err != nil || n != 0 {
			t.Fatalf("Prune removed %d rows (%v) while still inside the grace period", n, err)
		}
		if dead, _ := b.Dead(ctx); len(dead) != 1 {
			t.Fatal("the dead node is not visible, which is what the grace period is for")
		}

		clk.Add(10 * ttl)
		_ = b.Heartbeat(ctx, true)
		if n, err := b.Prune(ctx, 5*ttl); err != nil || n != 1 {
			t.Fatalf("Prune removed %d rows (%v), want 1", n, err)
		}
		// A registry must never prune itself, however long its own row has sat.
		all, _ := b.List(ctx)
		if len(all) != 1 || all[0].ID != "node-b" {
			t.Fatalf("after pruning, the registry holds %+v", all)
		}
	})
}

// TestRestartReclaimsItsOwnRow: registration is an upsert on the node id, so a
// restarted node updates started_at rather than appearing twice.
func TestRestartReclaimsItsOwnRow(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		r, _ := NewRegistry(s, "node-a", time.Minute, clk.Now)
		if err := r.Register(ctx, NodeInfo{Version: "v1"}); err != nil {
			t.Fatal(err)
		}
		first, _ := r.List(ctx)

		clk.Add(time.Hour)
		r2, _ := NewRegistry(s, "node-a", time.Minute, clk.Now)
		if err := r2.Register(ctx, NodeInfo{Version: "v2"}); err != nil {
			t.Fatal(err)
		}
		all, _ := r2.List(ctx)
		if len(all) != 1 {
			t.Fatalf("a restart produced %d rows, want 1", len(all))
		}
		if all[0].Version != "v2" {
			t.Fatalf("version = %q after a restart, want v2", all[0].Version)
		}
		if !all[0].StartedAt.After(first[0].StartedAt) {
			t.Fatal("started_at did not move, so a restart is indistinguishable from an uninterrupted run")
		}
	})
}
