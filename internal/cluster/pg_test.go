//go:build integration

package cluster

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// The PostgreSQL backend appends itself to the shared table, so every test in
// this package runs a second time against it under `-tags=integration`. The
// assertions do not change, because the guarantees do not: what changes is that
// the advisory-lock path replaces SQLite's single-writer transaction, and that
// several genuinely concurrent writers now exist.
func init() {
	backends = append(backends, backend{
		name:    "postgres",
		dialect: store.DialectPostgres,
		env:     pgEnv,
	})
}

// pgEnv gives each test its own schema in the shared test database, so tests
// are isolated without needing a database per test.
func pgEnv(t *testing.T) string {
	t.Helper()
	base := os.Getenv("DORANG_TEST_PG")
	if base == "" {
		t.Skip("DORANG_TEST_PG is not set; skipping the PostgreSQL backend")
	}

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()

	schema := "t_" + store.NewID()[:16]
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", base)
		if err != nil {
			return
		}
		defer db.Close()
		_, _ = db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse DORANG_TEST_PG: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func pgBackendOnly(t *testing.T) backend {
	t.Helper()
	for _, b := range backends {
		if b.dialect == store.DialectPostgres {
			return b
		}
	}
	t.Skip("no PostgreSQL backend registered")
	return backend{}
}

// TestPostgresAdvisoryLockSerializesTheCAS is the claim DESIGN 13 makes about
// how a leader is elected -- "through a store lock" -- checked against the lock
// itself rather than against its effects.
//
// SQLite reaches the same guarantee by having one writer, so this is the only
// place the advisory lock is genuinely load-bearing, and therefore the only
// place it can be shown to work.
func TestPostgresAdvisoryLockSerializesTheCAS(t *testing.T) {
	b := pgBackendOnly(t)
	ctx := context.Background()
	dsn := b.env(t)
	clk := newClock(epoch)
	s := openStore(t, b, dsn, clk.Now)

	c := newConn(s)
	if c.dia != store.DialectPostgres {
		t.Fatalf("dialect = %s", c.dia)
	}

	// Two transactions naming the same scope must not overlap. The second one
	// blocks at the lock, so the counter it reads is the value the first one
	// wrote and never the value it read.
	const goroutines = 8
	var inside atomic.Int32
	var overlaps atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := c.withTx(ctx, func(ctx context.Context, t tx) error {
				if err := t.lock(ctx, "test/serialized"); err != nil {
					return err
				}
				if inside.Add(1) > 1 {
					overlaps.Add(1)
				}
				time.Sleep(5 * time.Millisecond)
				inside.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("withTx: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := overlaps.Load(); n != 0 {
		t.Fatalf("%d transactions held the same advisory lock at once", n)
	}

	// A different scope must not block, or the lock would be a global mutex
	// wearing a key.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.withTx(ctx, func(ctx context.Context, t tx) error {
			if err := t.lock(ctx, "test/scope-a"); err != nil {
				return err
			}
			time.Sleep(200 * time.Millisecond)
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	if err := c.withTx(ctx, func(ctx context.Context, t tx) error {
		return t.lock(ctx, "test/scope-b")
	}); err != nil {
		t.Fatalf("second scope: %v", err)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("an unrelated scope waited %v on another scope's lock", d)
	}
	<-done
}

// TestPostgresElectionUnderRealConcurrency runs the election against a store
// that allows several writers at once, which SQLite by construction does not.
func TestPostgresElectionUnderRealConcurrency(t *testing.T) {
	b := pgBackendOnly(t)
	ctx := context.Background()
	dsn := b.env(t)
	clk := newClock(epoch)

	const n = 5
	nodes := make([]*Node, n)
	for i := 0; i < n; i++ {
		s := openStore(t, b, dsn, clk.Now)
		nodes[i] = testNode(t, s, "node-"+string(rune('a'+i)), clk, nil)
	}

	var wg sync.WaitGroup
	for _, nd := range nodes {
		wg.Add(1)
		go func(nd *Node) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := nd.Election().Campaign(ctx); err != nil {
					t.Errorf("Campaign(%s): %v", nd.ID(), err)
					return
				}
			}
		}(nd)
	}
	wg.Wait()

	if got := leaders(nodes...); len(got) != 1 {
		t.Fatalf("after %d concurrent campaigns, leaders = %v, want exactly one", n*20, got)
	}
}

// TestPostgresLeaseTableIsExactUnderConcurrency is the shared-pg accuracy claim
// against a store where two nodes really can write at the same instant. On
// SQLite the same test passes because the database refuses to let them; here
// only the advisory lock stops it.
func TestPostgresLeaseTableIsExactUnderConcurrency(t *testing.T) {
	b := pgBackendOnly(t)
	dsn := b.env(t)
	clk := newClock(epoch)

	const nodes = 6
	stores := make([]*store.Store, nodes)
	for i := range stores {
		stores[i] = openStore(t, b, dsn, clk.Now)
	}

	p := Params{Limit: 100, Nodes: nodes, Clustered: true}
	pub, err := Publish(ModeSharedPG, p)
	if err != nil {
		t.Fatal(err)
	}
	coords := coordinators(t, ModeSharedPG, stores, clk, p)
	key := quota.NewKey("credential", "pg-exact", quota.Rolling(time.Hour),
		quota.MetricRequests, clk.Now())

	admitted := chargeConcurrently(t, coords, key, 100)
	if admitted != 100 {
		t.Fatalf("shared-pg admitted %d against a limit of 100; published overshoot is %d",
			admitted, pub.MaxOvershoot)
	}
}

// TestPostgresLeaderMaintainsPartitions is DESIGN 9.5 restricted to the leader.
// Partitions only exist on PostgreSQL, so this is the only dialect on which the
// maintenance job does anything at all.
func TestPostgresLeaderMaintainsPartitions(t *testing.T) {
	b := pgBackendOnly(t)
	ctx := context.Background()
	dsn := b.env(t)
	clk := newClock(epoch)
	s := openStore(t, b, dsn, clk.Now)

	if s.Partitioning() != store.PartitioningDaily {
		t.Fatalf("partitioning = %s, want daily", s.Partitioning())
	}
	before, err := s.ListPartitions(ctx, "request_logs")
	if err != nil {
		t.Fatal(err)
	}

	n := testNode(t, s, "node-a", clk, nil)
	t.Cleanup(func() { _ = n.Close(ctx) })

	// A week later the leader must have created the days in between, or the
	// first insert after midnight fails -- the failure R1-15 exists to prevent.
	clk.Add(7 * 24 * time.Hour)
	tick(t, n)
	if !n.IsLeader() {
		t.Fatal("the only node did not become leader")
	}
	if st := n.JobStats()["partitions-and-retention"]; st.Runs == 0 || st.Failures != 0 {
		t.Fatalf("the maintenance job ran %d times with %d failures: %v", st.Runs, st.Failures, st.LastErr)
	}

	after, err := s.ListPartitions(ctx, "request_logs")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(before) {
		t.Fatalf("partitions went from %d to %d across a week of leadership", len(before), len(after))
	}
}

// TestPostgresBudgetSurvivesARestart repeats the W9 assertion on the dialect a
// real cluster uses. It is the same test as the SQLite one and deliberately so:
// if durability meant something different here, it would not be durability.
func TestPostgresBudgetSurvivesARestart(t *testing.T) {
	b := pgBackendOnly(t)
	ctx := context.Background()
	dsn := b.env(t)
	clk := newClock(epoch)
	s := openStore(t, b, dsn, clk.Now)

	const limit = 10 * 1_000_000_000
	key := BudgetKey("team", "team-pg", quota.Monthly, clk.Now())

	first := newLedger(t, s, "node-a", clk, usd(1), time.Minute)
	spent := spend(t, first, key, limit, 35, usd(0.2), usd(0.1))
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}

	second := newLedger(t, s, "node-a", clk, usd(1), time.Minute)
	t.Cleanup(func() { _ = second.Close(ctx) })
	got, err := second.Committed(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if got != spent {
		t.Fatalf("after a restart the budget shows %d nano spent, want %d", got, spent)
	}
}
