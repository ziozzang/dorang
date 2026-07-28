package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestMigrateFromEmptyIsIdempotent(t *testing.T) {
	eachEnv(t, func(t *testing.T, b backend, dsn string) {
		ctx := context.Background()

		// Open migrates; do it explicitly so the first run is observable.
		s := openStore(t, b, dsn, func(c *Config) { c.SkipMigrate = true })

		v, err := s.SchemaVersion(ctx)
		if err == nil && v != 0 {
			t.Fatalf("fresh database reports schema version %d", v)
		}

		first, err := s.Migrate(ctx)
		if err != nil {
			t.Fatalf("first Migrate: %v", err)
		}
		if len(first.Applied) == 0 {
			t.Fatal("first Migrate applied nothing")
		}
		if first.Version == 0 {
			t.Fatal("first Migrate left version 0")
		}

		second, err := s.Migrate(ctx)
		if err != nil {
			t.Fatalf("second Migrate: %v", err)
		}
		if len(second.Applied) != 0 {
			t.Fatalf("second Migrate applied %v, want nothing", second.Applied)
		}
		if second.AlreadyApplied != len(first.Applied) {
			t.Fatalf("second Migrate saw %d applied, want %d", second.AlreadyApplied, len(first.Applied))
		}
		if second.Version != first.Version {
			t.Fatalf("version moved from %d to %d", first.Version, second.Version)
		}

		// Every table DESIGN 9.2 names must exist and be selectable.
		for _, table := range designTables {
			if _, err := s.query(ctx, "SELECT * FROM "+table+" WHERE 1 = 0"); err != nil {
				t.Errorf("table %s: %v", table, err)
			}
		}
	})
}

// designTables is the list from DESIGN 9.2, plus the two structural tables the
// design's prose requires: the normalized tag table of 9.3 and the migration
// bookkeeping of 9.1.
var designTables = []string{
	"users", "teams", "team_members", "api_keys",
	"providers", "credentials", "deployments", "model_aliases", "model_classes",
	"pricing_rules",
	"request_logs", "request_traces", "request_log_tags",
	"usage_by_key_hour", "usage_by_model_hour", "usage_by_team_day",
	"quota_buckets", "quota_leases", "budget_state", "credential_state",
	"responses_store",
	"nodes", "capacity_leases", "files", "batches", "batch_requests", "audit_logs",
	"schema_migrations",
}

// TestMigrateConcurrent starts two independent connections against one database
// at the same time. Exactly one must apply the schema and neither may fail:
// several nodes starting together is the normal case, not the exceptional one.
func TestMigrateConcurrent(t *testing.T) {
	eachEnv(t, func(t *testing.T, b backend, dsn string) {
		ctx := context.Background()

		const n = 2
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			applied int
			errs    []error
		)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s := openStore(t, b, dsn, func(c *Config) { c.SkipMigrate = true })
				<-start
				rep, err := s.Migrate(ctx)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err)
					return
				}
				if len(rep.Applied) > 0 {
					applied++
				}
			}()
		}
		close(start)
		wg.Wait()

		for _, err := range errs {
			t.Errorf("concurrent Migrate: %v", err)
		}
		if applied != 1 {
			t.Fatalf("%d of %d connections applied the schema, want exactly 1", applied, n)
		}

		s := openStore(t, b, dsn, nil)
		if _, err := s.query(ctx, "SELECT * FROM api_keys WHERE 1 = 0"); err != nil {
			t.Fatalf("schema not usable after concurrent migration: %v", err)
		}
	})
}

// TestMigrateRefusesModifiedMigration proves a changed migration is caught
// rather than skipped. Skipping it would leave two deployments reporting the
// same schema version with different schemas.
func TestMigrateRefusesModifiedMigration(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		mustExec(t, s, `UPDATE schema_migrations SET checksum = ? WHERE version = 1`, "not-the-real-checksum")
		_, err := s.Migrate(ctx)
		if !errors.Is(err, ErrDirtySchema) {
			t.Fatalf("Migrate after tampering: %v, want ErrDirtySchema", err)
		}
	})
}

func TestLoadMigrationsBothDialects(t *testing.T) {
	for _, dir := range []string{"sqlite", "postgres"} {
		ms, err := loadMigrations(dir)
		if err != nil {
			t.Fatalf("loadMigrations(%s): %v", dir, err)
		}
		if len(ms) == 0 {
			t.Fatalf("loadMigrations(%s): no migrations embedded", dir)
		}
		for i := 1; i < len(ms); i++ {
			if ms[i-1].Version >= ms[i].Version {
				t.Fatalf("%s: migrations are not strictly ordered: %d then %d",
					dir, ms[i-1].Version, ms[i].Version)
			}
		}
	}
}

func TestSplitStatements(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"simple", "CREATE TABLE a (x INT); CREATE TABLE b (y INT);", 2},
		{"trailing semicolon optional", "SELECT 1", 1},
		{"semicolon in string", "INSERT INTO t VALUES ('a;b'); SELECT 1;", 2},
		{"semicolon in identifier", `CREATE TABLE "a;b" (x INT);`, 1},
		{"line comment", "-- drop table x;\nSELECT 1;", 1},
		{"block comment", "/* a; b */ SELECT 1;", 1},
		{"dollar quoted", "CREATE FUNCTION f() RETURNS int AS $$ BEGIN; RETURN 1; END; $$ LANGUAGE plpgsql;", 1},
		{"empty", "\n\n-- nothing\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitStatements(tc.in)
			if len(got) != tc.want {
				t.Fatalf("got %d statements %q, want %d", len(got), got, tc.want)
			}
		})
	}
}

func TestRebind(t *testing.T) {
	pg := postgresDialect{}
	cases := []struct{ in, want string }{
		{"SELECT 1", "SELECT 1"},
		{"SELECT * FROM t WHERE a = ? AND b = ?", "SELECT * FROM t WHERE a = $1 AND b = $2"},
		{"SELECT '?' , a FROM t WHERE b = ?", "SELECT '?' , a FROM t WHERE b = $1"},
		{`SELECT "we?rd" FROM t WHERE b = ?`, `SELECT "we?rd" FROM t WHERE b = $1`},
		{"SELECT a -- ? not a param\n, b FROM t WHERE c = ?", "SELECT a -- ? not a param\n, b FROM t WHERE c = $1"},
		{"SELECT 'it''s ?' FROM t WHERE c = ?", "SELECT 'it''s ?' FROM t WHERE c = $1"},
	}
	for _, tc := range cases {
		if got := pg.rebind(tc.in); got != tc.want {
			t.Errorf("rebind(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
	if got := (sqliteDialect{}).rebind("SELECT ?"); got != "SELECT ?" {
		t.Errorf("sqlite rebind changed the query: %q", got)
	}
}

// TestSQLiteNeedsNothing is the notebook claim of DESIGN 0.2 stated as a test:
// a store opens, migrates and serves a query with zero external dependencies.
func TestSQLiteNeedsNothing(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, Config{
		Driver: DialectSQLite,
		DSN:    t.TempDir() + "/notebook.db",
		Pepper: testPepper,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if s.Partitioning() != PartitioningNone {
		t.Fatalf("sqlite reports partitioning %q", s.Partitioning())
	}
	if _, err := s.EnsurePartitions(ctx, s.now(), 3); !errors.Is(err, ErrNoPartitioning) {
		t.Fatalf("EnsurePartitions on sqlite: %v, want ErrNoPartitioning", err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil || v == 0 {
		t.Fatalf("SchemaVersion = %d, %v", v, err)
	}
}

func TestMigrationsDeclareSameTables(t *testing.T) {
	// The two dialect files must describe the same logical schema. Comparing
	// the set of CREATE TABLE names catches a table added to one and forgotten
	// in the other, which would otherwise only surface in production on the
	// dialect nobody develops against.
	names := map[string]map[string]bool{}
	for _, dir := range []string{"sqlite", "postgres"} {
		ms, err := loadMigrations(dir)
		if err != nil {
			t.Fatal(err)
		}
		set := map[string]bool{}
		for _, m := range ms {
			for _, stmt := range splitStatements(m.Body) {
				upper := strings.ToUpper(stmt)
				if !strings.HasPrefix(upper, "CREATE TABLE") {
					continue
				}
				fields := strings.Fields(stmt)
				if len(fields) < 3 {
					continue
				}
				name := fields[2]
				if i := strings.IndexAny(name, "("); i >= 0 {
					name = name[:i]
				}
				set[name] = true
			}
		}
		names[dir] = set
	}
	for tbl := range names["sqlite"] {
		if !names["postgres"][tbl] {
			t.Errorf("table %s exists in sqlite but not postgres", tbl)
		}
	}
	for tbl := range names["postgres"] {
		if !names["sqlite"][tbl] {
			t.Errorf("table %s exists in postgres but not sqlite", tbl)
		}
	}
}
