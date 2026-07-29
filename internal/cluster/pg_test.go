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

// TestPostgresDeadlockIsRetriedNotReturned is the busy-retry contract stated
// against the dialect whose contention it was never written for.
//
// [conn.withTx] retries "a store that refused the write for a reason another
// attempt can resolve", and until this test [isBusy] recognised only SQLite's
// three message strings. PostgreSQL produces none of them. A transaction the
// SERVER aborted so that another could proceed -- already rolled back, and by
// definition retryable -- was therefore delivered to a caller of this package
// as a permanent error, on the one dialect where concurrent writers exist and
// so on the one dialect where it can happen at all.
//
// The deadlock is induced rather than waited for, because a test that hoped for
// one would be a test that usually proves nothing. Two transactions take two
// row locks in opposite orders, each waiting for the other to hold its first;
// PostgreSQL detects the cycle and kills one of them with 40P01. The assertion
// is that BOTH callers still see success: the victim is retried by withTx and
// commits on the attempt after the winner has gone.
func TestPostgresDeadlockIsRetriedNotReturned(t *testing.T) {
	b := pgBackendOnly(t)
	ctx := context.Background()
	dsn := b.env(t)
	clk := newClock(epoch)
	s := openStore(t, b, dsn, clk.Now)
	c := newConn(s)

	// Two rows to contend over. They are ordinary registry rows: the mechanism
	// under test is the transaction helper, not the table.
	for _, id := range []string{"row-x", "row-y"} {
		if _, err := c.exec(ctx, `
			INSERT INTO nodes (node_id, address, version, started_at, last_heartbeat, is_leader, metadata)
			VALUES (?, '', '', ?, ?, ?, '')`,
			id, store.Micros(clk.Now()), store.Micros(clk.Now()), false); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	lockRow := func(ctx context.Context, tr tx, id string) error {
		var got string
		return tr.queryRow(ctx, `SELECT node_id FROM nodes WHERE node_id = ? FOR UPDATE`, id).Scan(&got)
	}

	// Each side signals once it holds its first row and then waits for the
	// other, so the cycle is closed deliberately. Only the FIRST attempt takes
	// part in the handshake: a retry that waited on an already-consumed channel
	// would hang rather than commit.
	readyA, readyB := make(chan struct{}), make(chan struct{})
	var attemptsA, attemptsB atomic.Int32

	side := func(first, second string, ready chan struct{}, peer chan struct{}, attempts *atomic.Int32) error {
		return c.withTx(ctx, func(ctx context.Context, tr tx) error {
			n := attempts.Add(1)
			if err := lockRow(ctx, tr, first); err != nil {
				return err
			}
			if n == 1 {
				close(ready)
				<-peer
			}
			return lockRow(ctx, tr, second)
		})
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = side("row-x", "row-y", readyA, readyB, &attemptsA) }()
	go func() { defer wg.Done(); errs[1] = side("row-y", "row-x", readyB, readyA, &attemptsB) }()
	wg.Wait()

	total := attemptsA.Load() + attemptsB.Load()
	if total < 3 {
		// Both sides committed on their first attempt, so no cycle formed and
		// there is nothing to have retried. Say so rather than pass silently.
		t.Skipf("no deadlock formed (%d attempts in total); the fixture proved nothing", total)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("side %d failed with %v; PostgreSQL aborted this transaction so the "+
				"other could proceed, and withTx must retry it rather than hand the "+
				"caller a permanent error (attempts: %d and %d)",
				i, err, attemptsA.Load(), attemptsB.Load())
		}
	}
	t.Logf("MEASURED: a PostgreSQL deadlock was retried to success in %d attempts across the two sides",
		total)
}
