//go:build integration

package store

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// The PostgreSQL backend appends itself to the shared table, so every test in
// the package runs against both dialects under `-tags=integration`. That is
// the point of one Go API over two dialects: the assertions do not change.
func init() {
	backends = append(backends, backend{
		name:    "postgres",
		dialect: DialectPostgres,
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

	schema := "t_" + NewID()[:16]
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoteIdent(schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", base)
		if err != nil {
			return
		}
		defer db.Close()
		_, _ = db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+quoteIdent(schema)+" CASCADE")
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

// TestPostgresLedgerNeverSequentiallyScans is the DESIGN 9.3 gate stated
// against the planner rather than against a comment. Revision 1's single
// (ts, id) key degraded every ledger API to a partition scan; this fails if
// that ever comes back.
func TestPostgresLedgerNeverSequentiallyScans(t *testing.T) {
	b := pgBackendOnly(t)
	s := openStore(t, b, b.env(t), nil)

	// Enough rows that a sequential scan is genuinely the expensive option.
	// This matters: on a few thousand rows PostgreSQL prefers a scan and is
	// right to, so an index assertion at that size would be testing the
	// fixture. 40k rows over three daily partitions is a small team-tier day
	// (DESIGN 0.2 puts the team tier at ~4.3M ledger rows/day), and it is the
	// smallest corpus at which the claim under test means anything.
	seedLedger(t, s, ledgerEpoch, 72*time.Hour, 40000)
	analyze(t, s)

	r := TimeRange{Start: ledgerEpoch.Add(-time.Hour), End: ledgerEpoch.Add(73 * time.Hour)}
	page := Page{Limit: 50}

	cases := []struct {
		name  string
		spec  ledgerSpec
		index string
	}{
		{"by key", ledgerSpec{where: "l.api_key_id = ?", args: []any{"key-3"}}, "request_logs_key_ts_idx"}, // pragma: allowlist secret — test fixture
		{"by team", ledgerSpec{where: "l.team_id = ?", args: []any{"team-2"}}, "request_logs_team_ts_idx"},
		{"by trace", ledgerSpec{where: "l.trace_id = ?", args: []any{"trace-99"}}, "request_logs_trace_ts_idx"},
		{"errors", ledgerSpec{where: "l.status >= 400"}, "request_logs_errors_idx"},
		{"by tag", ledgerSpec{
			join:          "JOIN request_log_tags t ON t.ts = l.ts AND t.request_id = l.id",
			where:         "t.tag = ?",
			args:          []any{"tag-7"},
			extraRangeCol: "t.ts",
		}, "request_log_tags_tag_ts_idx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, args, err := s.buildLedgerQuery(tc.spec, r, page)
			if err != nil {
				t.Fatal(err)
			}
			assertIndexedAtScale(t, s, q, args, tc.index)
		})
	}

	t.Run("errors uses the partial index, not the primary key", func(t *testing.T) {
		q, args, err := s.buildLedgerQuery(ledgerSpec{where: "l.status >= 400"}, r, page)
		if err != nil {
			t.Fatal(err)
		}
		plan := explain(t, s, q, args)
		// The partial index's own predicate is status >= 400, so a plan using
		// it has nothing left to filter. A residual filter would mean the
		// planner fell back to the (ts, id) key and is reading every row in
		// the range to throw most of them away.
		if strings.Contains(plan, "Filter: (status >= 400)") {
			t.Fatalf("the partial index was not used; the plan re-checks status:\n%s", plan)
		}
	})

	t.Run("spend by credential", func(t *testing.T) {
		const q = `SELECT COUNT(*), COALESCE(SUM(l.cost_nano), 0) FROM request_logs l
			WHERE l.credential_id = ? AND l.ts >= ? AND l.ts < ?`
		assertIndexedAtScale(t, s, q, []any{"cred-4", Micros(r.Start), Micros(r.End)}, "request_logs_cred_ts_idx")
	})

	t.Run("range prunes partitions", func(t *testing.T) {
		// A one-hour window must not open every day's partition. Partition
		// pruning is half of why the bounded range is mandatory.
		narrow := TimeRange{Start: ledgerEpoch.Add(time.Hour), End: ledgerEpoch.Add(2 * time.Hour)}
		q, args, err := s.buildLedgerQuery(ledgerSpec{where: "l.api_key_id = ?", args: []any{"key-3"}}, narrow, page)
		if err != nil {
			t.Fatal(err)
		}
		plan := explain(t, s, q, args)
		if n := strings.Count(plan, "request_logs_2"); n > 2 {
			t.Fatalf("a one-hour query touched %d partition references:\n%s", n, plan)
		}
	})
}

// TestPostgresPartitionMaintenance covers the pieces that only exist on a
// partitioned dialect.
func TestPostgresPartitionMaintenance(t *testing.T) {
	b := pgBackendOnly(t)
	ctx := context.Background()
	clk := newClock(beforeMidnight)
	s := openStore(t, b, b.env(t), func(c *Config) { c.Now = clk.Now })

	if s.Partitioning() != PartitioningDaily {
		t.Fatalf("partitioning = %s", s.Partitioning())
	}

	for _, tbl := range partitionedTables {
		parts, err := s.ListPartitions(ctx, tbl)
		if err != nil {
			t.Fatal(err)
		}
		// yesterday + today + PartitionAhead + 1
		if len(parts) < 5 {
			t.Fatalf("%s has only %d partitions after Open: %v", tbl, len(parts), parts)
		}
	}

	// Creating them again is a no-op, and two nodes doing it at once is fine.
	created, err := s.EnsurePartitions(ctx, clk.Now(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("re-running EnsurePartitions created %v", created)
	}

	errc := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := s.EnsurePartitions(ctx, clk.Now().AddDate(0, 0, 10), 3)
			errc <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil {
			t.Fatalf("concurrent EnsurePartitions: %v", err)
		}
	}
}

// TestPostgresInsertWithoutPartitionFailsThenRecovers proves the recovery path
// is real by removing the partition out from under the writer.
func TestPostgresInsertWithoutPartitionFailsThenRecovers(t *testing.T) {
	b := pgBackendOnly(t)
	ctx := context.Background()
	clk := newClock(beforeMidnight)
	s := openStore(t, b, b.env(t), func(c *Config) { c.Now = clk.Now })

	day := dayFloor(beforeMidnight).AddDate(0, 0, 1)
	name := partitionName("request_logs", day)
	if _, err := s.db.ExecContext(ctx, "DROP TABLE "+quoteIdent(name)); err != nil {
		t.Fatalf("drop %s: %v", name, err)
	}

	// The bare insert now fails the way it would at midnight with no
	// pre-creation, and isMissingPartition must recognise it.
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		return s.insertLogChunk(ctx, tx, []RequestLog{{
			ID: "x", TS: day.Add(time.Hour), ModelGroup: "m", Metadata: "{}",
		}})
	})
	if err == nil {
		t.Fatal("insert into a missing partition succeeded")
	}
	if !s.d.isMissingPartition(err) {
		t.Fatalf("isMissingPartition did not recognise %v", err)
	}

	// The public writer recovers.
	if err := s.InsertRequestLogs(ctx, []RequestLog{{
		ID: "y", TS: day.Add(time.Hour), APIKeyID: "key-1", ModelGroup: "m",
	}}); err != nil {
		t.Fatalf("InsertRequestLogs did not recover: %v", err)
	}
	page, err := s.ListRequestsByKey(ctx, "key-1",
		TimeRange{Start: day, End: day.AddDate(0, 0, 1)}, Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("got %d rows after recovery, want 1", len(page.Rows))
	}
}

func pgBackendOnly(t *testing.T) backend {
	t.Helper()
	for _, b := range backends {
		if b.dialect == DialectPostgres {
			return b
		}
	}
	t.Skip("no PostgreSQL backend registered")
	return backend{}
}
