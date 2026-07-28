package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The whole suite runs against SQLite with no build tag and no external
// service. That is not a convenience -- it is the notebook profile of
// DESIGN 0.2 being provable by `go test`. The PostgreSQL backend appends
// itself in pg_test.go under the `integration` tag.

type backend struct {
	name    string
	dialect Dialect
	// env returns a DSN for a fresh, isolated database. Called once per test;
	// several stores may be opened against the same DSN.
	env func(t *testing.T) string
}

var backends = []backend{{
	name:    "sqlite",
	dialect: DialectSQLite,
	env: func(t *testing.T) string {
		return filepath.Join(t.TempDir(), "dorang.db")
	},
}}

var testPepper = []byte("test-pepper-not-a-real-secret") // pragma: allowlist secret — test fixture

// clock is an injectable, race-safe time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openStore(t *testing.T, b backend, dsn string, mutate func(*Config)) *Store {
	t.Helper()
	cfg := Config{
		Driver: b.dialect,
		DSN:    dsn,
		Pepper: testPepper,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open(%s): %v", b.name, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// eachBackend runs fn against every configured backend with a fresh store.
func eachBackend(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			fn(t, openStore(t, b, b.env(t), nil))
		})
	}
}

// eachBackendCfg is eachBackend for tests that need to shape the Config.
func eachBackendCfg(t *testing.T, mutate func(*Config), fn func(t *testing.T, s *Store)) {
	t.Helper()
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			fn(t, openStore(t, b, b.env(t), mutate))
		})
	}
}

// eachEnv exposes the raw DSN, for tests that open more than one store against
// the same database (concurrent migration, two-node behaviour).
func eachEnv(t *testing.T, fn func(t *testing.T, b backend, dsn string)) {
	t.Helper()
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			fn(t, b, b.env(t))
		})
	}
}

// ---------------------------------------------------------------------------
// Plan assertions
// ---------------------------------------------------------------------------

// assertIndexed is assertIndexedAtScale on a corpus small enough that
// PostgreSQL would legitimately prefer a scan, so the plan assertion runs on
// SQLite only. The PostgreSQL half of the same claim is
// TestPostgresLedgerNeverSequentiallyScans, which builds a corpus where a scan
// is genuinely the expensive option. Asserting it here as well would be
// asserting the fixture: on a few hundred rows a sequential scan IS the right
// plan, and a test that failed for choosing it would be wrong.
func assertIndexed(t *testing.T, s *Store, query string, args []any, wantIndex string) {
	t.Helper()
	if s.Driver() == DialectPostgres {
		return
	}
	assertIndexedAtScale(t, s, query, args, wantIndex)
}

// assertIndexedAtScale fails if the query plan reads request_logs (or one of
// its partitions) sequentially.
//
// DESIGN 9.3 exists because revision 1's single (ts, id) key degraded every
// ledger API to a partition scan. A comment claiming an index is used is not
// evidence; the planner's own output is.
func assertIndexedAtScale(t *testing.T, s *Store, query string, args []any, wantIndex string) {
	t.Helper()
	plan := explain(t, s, query, args)
	switch s.Driver() {
	case DialectPostgres:
		// "request_logs" rather than "request_log", so that a scan of the tag
		// side is not mistaken for a scan of the ledger.
		if strings.Contains(plan, "Seq Scan on request_logs") {
			t.Fatalf("plan contains a sequential scan of the ledger:\n%s", plan)
		}
		// PostgreSQL names a partition's index after the partition and its
		// columns, not after the parent index, so callers pass the column
		// suffix on this dialect.
		wantIndex = pgIndexSuffix[wantIndex]
	case DialectSQLite:
		// SQLite prints "SCAN <table>" for a full scan and
		// "SEARCH <table> USING INDEX <name>" otherwise.
		for _, line := range strings.Split(plan, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "SCAN request_log") {
				t.Fatalf("plan contains a full scan of the ledger:\n%s", plan)
			}
		}
	}
	if wantIndex != "" && !strings.Contains(plan, wantIndex) {
		t.Fatalf("plan does not use %s:\n%s", wantIndex, plan)
	}
}

// pgIndexSuffix maps a parent index name to the tail PostgreSQL gives the
// per-partition copy of it. request_logs_errors_idx is the partial index; a
// plan that used the (ts, id) primary key instead would carry an explicit
// "Filter: (status >= 400)", which the partial index makes unnecessary.
var pgIndexSuffix = map[string]string{
	"":                            "",
	"request_logs_key_ts_idx":     "_api_key_id_ts_id_idx", // pragma: allowlist secret — test fixture
	"request_logs_team_ts_idx":    "_team_id_ts_id_idx",
	"request_logs_cred_ts_idx":    "_credential_id_ts_id_idx",
	"request_logs_trace_ts_idx":   "_trace_id_ts_id_idx",
	"request_logs_errors_idx":     "_ts_id_idx",
	"request_log_tags_tag_ts_idx": "_tag_ts_request_id_idx",
}

func explain(t *testing.T, s *Store, query string, args []any) string {
	t.Helper()
	var prefix string
	switch s.Driver() {
	case DialectPostgres:
		prefix = "EXPLAIN "
	case DialectSQLite:
		prefix = "EXPLAIN QUERY PLAN "
	}
	rows, err := s.db.QueryContext(context.Background(), prefix+s.rebind(query), args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\nquery: %s", err, query)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, c := range cells {
			b.WriteString(asString(c))
			b.WriteByte(' ')
		}
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// analyze refreshes planner statistics so an EXPLAIN assertion reflects the
// data present rather than the defaults for an empty table.
func analyze(t *testing.T, s *Store) {
	t.Helper()
	// Both dialects spell it the same way.
	if _, err := s.db.ExecContext(context.Background(), "ANALYZE"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func mustExec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.exec(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %s: %v", firstLine(q), err)
	}
}

func seedTeam(t *testing.T, s *Store, id string) {
	t.Helper()
	now := Micros(s.now())
	mustExec(t, s, `INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		id, id, now, now)
}

func seedUser(t *testing.T, s *Store, id string) {
	t.Helper()
	now := Micros(s.now())
	mustExec(t, s, `INSERT INTO users (id, email, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		id, id+"@example.test", now, now)
}

// Seed cardinalities. They are not arbitrary: a corpus where one team owns a
// fifth of the rows makes a full scan the CORRECT plan, and an index assertion
// against it would be testing the fixture rather than the schema.
const (
	seedKeys   = 200
	seedTeams  = 50
	seedCreds  = 40
	seedUsers  = 60
	seedModels = 8
	seedTags   = 200
	// seedErrorEvery and seedProdEvery keep the error and tag populations
	// small enough to be worth an index.
	seedErrorEvery = 50
	seedProdEvery  = 13
)

// seedLedger writes n rows spread over the given span, cycling through the
// seed cardinalities above.
func seedLedger(t *testing.T, s *Store, start time.Time, span time.Duration, n int) []RequestLog {
	t.Helper()
	rows := make([]RequestLog, 0, n)
	step := span / time.Duration(n)
	for i := 0; i < n; i++ {
		r := RequestLog{
			ID:            NewID(),
			TS:            start.Add(time.Duration(i) * step),
			APIKeyID:      "key-" + itoa(i%seedKeys),
			TeamID:        "team-" + itoa(i%seedTeams),
			CredentialID:  "cred-" + itoa(i%seedCreds),
			UserID:        "user-" + itoa(i%seedUsers),
			ProviderID:    "prov-1",
			DeploymentID:  "depl-1",
			ModelGroup:    "model-" + itoa(i%seedModels),
			UpstreamModel: "upstream-x",
			Endpoint:      "/v1/chat/completions",
			Status:        200,
			PromptTokens:  int64(10 + i),
			TotalTokens:   int64(20 + i),
			CostNano:      int64(1000 + i),
			LatencyMS:     int64(i % 500),
			TraceID:       "trace-" + itoa(i),
			Metadata:      "{}",
		}
		if i%seedErrorEvery == 0 {
			r.Status = 503
			r.ErrorClass = "upstream_5xx"
		}
		r.Tags = []string{"tag-" + itoa(i%seedTags)}
		if i%seedProdEvery == 0 {
			r.Tags = append(r.Tags, "prod")
		}
		rows = append(rows, r)
	}
	if err := s.InsertRequestLogs(context.Background(), rows); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}
	return rows
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
