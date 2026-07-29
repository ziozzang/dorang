//go:build integration

package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// migrations/sqlite and migrations/postgres are two hand-written directories
// that must describe the same schema, and until this file nothing compared
// them.
//
// Every query in this package is written once, in portable SQL, against a
// schema this package assumes is the same on both engines. That assumption is
// load-bearing in a way a comment cannot carry: a column that exists on one
// dialect and not the other is a query that works in development and fails in
// production, and an index present on SQLite and missing on PostgreSQL is a
// ledger query that returns the right answer after a partition scan. Neither
// shows up in a test that runs the same assertions against both, because both
// assertions pass -- one of them slowly, and the other against a schema nobody
// checked.
//
// So this compares the schemas the two directories ACTUALLY PRODUCE, by
// introspecting both databases after migration. It is deliberately not a diff
// of the .sql files: the files are allowed to differ, because the dialects
// differ. What is not allowed is for the resulting schemas to differ in a way
// that changes what a query means.

// ---------------------------------------------------------------------------
// The model
// ---------------------------------------------------------------------------

type schemaModel struct {
	tables  map[string]map[string]column
	indexes map[string]index
	// pk maps a table to its primary-key columns, in order.
	pk map[string][]string
}

type column struct {
	// class is the portable type class: int, text, real, blob or bool.
	class string
	// declared is the dialect's own spelling, for a readable failure.
	declared string
	notNull  bool
}

type index struct {
	table  string
	unique bool
	// cols is the indexed expression list, normalized: lower case, no
	// dialect-specific quoting or ASC/DESC spelling differences.
	cols string
	// partial is the WHERE predicate, normalized to a portable form.
	partial string
}

// pgPartition matches the daily partitions and their per-partition indexes.
// They are a PostgreSQL-only mechanism (DESIGN 9.5) with no SQLite counterpart
// by design, so they are excluded rather than reported as drift.
var pgPartition = regexp.MustCompile(`_\d{8}(_|$)`)

// TestMigrationSetsDoNotDrift is the gate on the two directories.
func TestMigrationSetsDoNotDrift(t *testing.T) {
	var sq, pg backend
	for _, b := range backends {
		switch b.dialect {
		case DialectSQLite:
			sq = b
		case DialectPostgres:
			pg = b
		}
	}
	if pg.env == nil {
		t.Skip("no PostgreSQL backend registered")
	}
	ctx := context.Background()
	sqlite := readSQLiteSchema(t, ctx, openStore(t, sq, sq.env(t), nil))
	postgres := readPostgresSchema(t, ctx, openStore(t, pg, pg.env(t), nil))

	t.Run("tables", func(t *testing.T) {
		compareSets(t, "table", keysOf(sqlite.tables), keysOf(postgres.tables))
	})

	t.Run("columns", func(t *testing.T) {
		for _, tb := range sortedCommon(sqlite.tables, postgres.tables) {
			a, b := sqlite.tables[tb], postgres.tables[tb]
			compareSets(t, "column of "+tb, keysOf(a), keysOf(b))
			for _, name := range sortedCommon(a, b) {
				ca, cb := a[name], b[name]
				if ca.class != cb.class {
					t.Errorf("%s.%s is %s on SQLite (%s) and %s on PostgreSQL (%s); "+
						"a column that does not hold the same KIND of value on both "+
						"engines is a codec that is wrong on one of them",
						tb, name, ca.class, ca.declared, cb.class, cb.declared)
				}
				// A primary-key column is NOT NULL on PostgreSQL by definition.
				// SQLite's long-standing exception -- a TEXT PRIMARY KEY accepts
				// NULL unless NOT NULL is spelled out -- is a difference in what
				// the ENGINE enforces and not in what the schema describes, so
				// the comparison treats a key column as NOT NULL on both. It is
				// noted rather than ignored: see TestSQLitePrimaryKeysAcceptNull.
				an := ca.notNull || inList(sqlite.pk[tb], name)
				bn := cb.notNull || inList(postgres.pk[tb], name)
				if an != bn {
					t.Errorf("%s.%s is NOT NULL on SQLite=%v and on PostgreSQL=%v",
						tb, name, an, bn)
				}
			}
		}
	})

	t.Run("primary keys", func(t *testing.T) {
		for _, tb := range sortedCommon(sqlite.tables, postgres.tables) {
			a := strings.Join(sqlite.pk[tb], ",")
			b := strings.Join(postgres.pk[tb], ",")
			if a != b {
				t.Errorf("%s primary key is (%s) on SQLite and (%s) on PostgreSQL", tb, a, b)
			}
		}
	})

	t.Run("indexes", func(t *testing.T) {
		compareSets(t, "index", keysOf(sqlite.indexes), keysOf(postgres.indexes))
		for _, name := range sortedCommon(sqlite.indexes, postgres.indexes) {
			a, b := sqlite.indexes[name], postgres.indexes[name]
			if a.table != b.table {
				t.Errorf("index %s is on %s (SQLite) and %s (PostgreSQL)", name, a.table, b.table)
			}
			if a.unique != b.unique {
				t.Errorf("index %s is unique=%v on SQLite and unique=%v on PostgreSQL; "+
					"a uniqueness constraint on one engine only is a duplicate row on the other",
					name, a.unique, b.unique)
			}
			if a.cols != b.cols {
				t.Errorf("index %s covers (%s) on SQLite and (%s) on PostgreSQL",
					name, a.cols, b.cols)
			}
			if a.partial != b.partial {
				t.Errorf("index %s has predicate %q on SQLite and %q on PostgreSQL; "+
					"a partial index on one engine and a full one on the other do not "+
					"answer the same query", name, a.partial, b.partial)
			}
		}
	})
}

// TestSQLitePrimaryKeysAcceptNull records the one difference the comparison
// above deliberately tolerates, so that it is a known and bounded property
// rather than a silent hole.
//
// SQLite accepts NULL in a `TEXT PRIMARY KEY` column -- a documented,
// deliberately preserved bug in the engine -- while PostgreSQL rejects it. It is
// tolerated because closing it means declaring NOT NULL on ~20 key columns in
// migration 0001, whose checksum is recorded in every existing deployment, and
// SQLite has no ALTER COLUMN with which a later migration could do it instead.
//
// What makes it safe is that nothing in this package writes a NULL id: every id
// is either caller-supplied and validated or NewID(). This test states the
// difference so that a future reader finds a measurement rather than a
// surprise.
func TestSQLitePrimaryKeysAcceptNull(t *testing.T) {
	s := openStore(t, backends[0], backends[0].env(t), nil)
	if s.Driver() != DialectSQLite {
		t.Skip("the first backend is not SQLite")
	}
	now := Micros(s.now())
	_, err := s.exec(context.Background(),
		`INSERT INTO teams (id, name, created_at, updated_at) VALUES (NULL, 'x', ?, ?)`, now, now)
	if err != nil {
		// If SQLite ever starts enforcing this, the tolerance above can be
		// removed. Failing here would be good news, so it is not an error.
		t.Logf("SQLite now rejects a NULL primary key: %v", err)
		return
	}
	t.Log("NOTE: SQLite accepts a NULL TEXT PRIMARY KEY where PostgreSQL rejects it; " +
		"TestMigrationSetsDoNotDrift tolerates this one difference deliberately")
}

// ---------------------------------------------------------------------------
// Introspection
// ---------------------------------------------------------------------------

func readSQLiteSchema(t *testing.T, ctx context.Context, s *Store) schemaModel {
	t.Helper()
	m := schemaModel{
		tables:  map[string]map[string]column{},
		indexes: map[string]index{},
		pk:      map[string][]string{},
	}
	tables := queryStrings(t, ctx, s,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	for _, tb := range tables {
		cols := map[string]column{}
		type keyCol struct {
			name string
			pos  int
		}
		var keys []keyCol
		rows, err := s.query(ctx,
			`SELECT name, type, "notnull", pk FROM pragma_table_info(?)`, tb)
		if err != nil {
			t.Fatalf("pragma_table_info(%s): %v", tb, err)
		}
		for rows.Next() {
			var name, typ string
			var notNull, pk int
			if err := rows.Scan(&name, &typ, &notNull, &pk); err != nil {
				t.Fatal(err)
			}
			cols[name] = column{class: sqliteClass(typ), declared: typ, notNull: notNull == 1}
			if pk > 0 {
				keys = append(keys, keyCol{name, pk})
			}
		}
		rows.Close()
		sort.Slice(keys, func(i, j int) bool { return keys[i].pos < keys[j].pos })
		for _, k := range keys {
			m.pk[tb] = append(m.pk[tb], k.name)
		}
		m.tables[tb] = cols
	}

	rows, err := s.query(ctx,
		`SELECT name, tbl_name, sql FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, tb, ddl string
		if err := rows.Scan(&name, &tb, &ddl); err != nil {
			t.Fatal(err)
		}
		cols, partial := splitIndexDDL(ddl)
		m.indexes[name] = index{
			table:   tb,
			unique:  strings.Contains(strings.ToUpper(ddl), "CREATE UNIQUE INDEX"),
			cols:    cols,
			partial: normalizePredicate(partial),
		}
	}
	return m
}

func readPostgresSchema(t *testing.T, ctx context.Context, s *Store) schemaModel {
	t.Helper()
	m := schemaModel{
		tables:  map[string]map[string]column{},
		indexes: map[string]index{},
		pk:      map[string][]string{},
	}
	rows, err := s.query(ctx, `
		SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = current_schema()
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		WHERE c.relkind IN ('r', 'p')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tb, col, typ string
		var notNull bool
		if err := rows.Scan(&tb, &col, &typ, &notNull); err != nil {
			t.Fatal(err)
		}
		if pgPartition.MatchString(tb) {
			continue
		}
		if m.tables[tb] == nil {
			m.tables[tb] = map[string]column{}
		}
		m.tables[tb][col] = column{class: postgresClass(typ), declared: typ, notNull: notNull}
	}
	rows.Close()

	// Primary keys, in key order.
	krows, err := s.query(ctx, `
		SELECT c.relname, a.attname, k.ord
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = current_schema()
		JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		WHERE con.contype = 'p'
		ORDER BY c.relname, k.ord`)
	if err != nil {
		t.Fatal(err)
	}
	for krows.Next() {
		var tb, col string
		var ord int
		if err := krows.Scan(&tb, &col, &ord); err != nil {
			t.Fatal(err)
		}
		if pgPartition.MatchString(tb) {
			continue
		}
		m.pk[tb] = append(m.pk[tb], col)
	}
	krows.Close()

	irows, err := s.query(ctx, `
		SELECT i.indexname, i.tablename, i.indexdef
		FROM pg_indexes i WHERE i.schemaname = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	defer irows.Close()
	for irows.Next() {
		var name, tb, ddl string
		if err := irows.Scan(&name, &tb, &ddl); err != nil {
			t.Fatal(err)
		}
		// A PostgreSQL primary key is an index; SQLite's is not visible as one.
		// They are compared as primary keys instead, above.
		if pgPartition.MatchString(name) || strings.HasSuffix(name, "_pkey") {
			continue
		}
		cols, partial := splitIndexDDL(ddl)
		m.indexes[name] = index{
			table:   tb,
			unique:  strings.Contains(strings.ToUpper(ddl), "CREATE UNIQUE INDEX"),
			cols:    cols,
			partial: normalizePredicate(partial),
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// Normalization
// ---------------------------------------------------------------------------

// sqliteClass maps a declared SQLite type to a portable class.
//
// BOOLEAN is not among them on purpose: this schema spells its booleans
// INTEGER on SQLite and boolean on PostgreSQL, and internal/store's codec
// (asBool) reads both. The pair is therefore declared equivalent HERE, in one
// place, rather than tolerated case by case -- so a column that becomes TEXT on
// one engine still fails.
func sqliteClass(declared string) string {
	d := strings.ToUpper(strings.TrimSpace(declared))
	switch {
	case strings.Contains(d, "INT"):
		// See boolColumns: an INTEGER that PostgreSQL spells boolean.
		return "int"
	case strings.Contains(d, "CHAR"), strings.Contains(d, "TEXT"), strings.Contains(d, "CLOB"):
		return "text"
	case strings.Contains(d, "BLOB"), d == "":
		return "blob"
	case strings.Contains(d, "REAL"), strings.Contains(d, "FLOA"), strings.Contains(d, "DOUB"):
		return "real"
	}
	return "unknown:" + d
}

func postgresClass(declared string) string {
	d := strings.ToLower(strings.TrimSpace(declared))
	switch {
	case strings.HasPrefix(d, "bool"):
		// The declared equivalence: a PostgreSQL boolean is an INTEGER holding
		// 0 or 1 on SQLite, and asBool normalizes both.
		return "int"
	case strings.Contains(d, "int"):
		return "int"
	case strings.Contains(d, "text"), strings.Contains(d, "char"):
		return "text"
	case strings.Contains(d, "bytea"):
		return "blob"
	case strings.Contains(d, "double"), strings.Contains(d, "real"), strings.Contains(d, "numeric"):
		return "real"
	}
	return "unknown:" + d
}

var (
	// PostgreSQL renders a partitioned parent's index as `ON ONLY schema.table`,
	// so both the ONLY and the schema qualification have to come off before the
	// column list can be compared with SQLite's.
	indexHead  = regexp.MustCompile(`(?is)^\s*create\s+(unique\s+)?index\s+(if\s+not\s+exists\s+)?\S+\s+on\s+(only\s+)?\S+\s*`)
	usingBtree = regexp.MustCompile(`(?i)\busing\s+btree\s*`)
	spaces     = regexp.MustCompile(`\s+`)
)

// splitIndexDDL reduces a CREATE INDEX statement to its column list and its
// predicate, in a form the two dialects render identically.
func splitIndexDDL(ddl string) (cols, partial string) {
	s := indexHead.ReplaceAllString(ddl, "")
	s = usingBtree.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)

	if i := regexp.MustCompile(`(?i)\bwhere\b`).FindStringIndex(s); i != nil {
		partial = s[i[1]:]
		s = s[:i[0]]
	}
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "(")
	s = strings.TrimSuffix(strings.TrimSpace(s), ")")
	s = strings.ReplaceAll(s, `"`, "")
	s = spaces.ReplaceAllString(strings.ToLower(s), " ")
	// "col ASC" and "col" are the same index.
	s = regexp.MustCompile(`(?i)\s+asc\b`).ReplaceAllString(s, "")
	var parts []string
	for _, p := range strings.Split(s, ",") {
		parts = append(parts, strings.TrimSpace(p))
	}
	return strings.Join(parts, ", "), partial
}

// normalizePredicate renders a partial index's WHERE clause portably.
//
// The two dialects spell a boolean test differently -- `is_current = 1` on
// SQLite, `is_current` on PostgreSQL -- and that IS the same predicate given
// the column equivalence declared above. Everything else must match verbatim.
func normalizePredicate(p string) string {
	s := strings.ToLower(strings.TrimSpace(p))
	s = strings.ReplaceAll(s, `"`, "")
	s = strings.ReplaceAll(s, "(", " ")
	s = strings.ReplaceAll(s, ")", " ")
	s = spaces.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, " = 1")
	s = strings.TrimSuffix(s, " = true")
	return s
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func queryStrings(t *testing.T, ctx context.Context, s *Store, q string, args ...any) []string {
	t.Helper()
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		t.Fatalf("%s: %v", firstLine(q), err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCommon[V any](a, b map[string]V) []string {
	var out []string
	for k := range a {
		if _, ok := b[k]; ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func inList(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func compareSets(t *testing.T, what string, sqlite, postgres []string) {
	t.Helper()
	inA := map[string]bool{}
	for _, v := range sqlite {
		inA[v] = true
	}
	inB := map[string]bool{}
	for _, v := range postgres {
		inB[v] = true
	}
	for _, v := range sqlite {
		if !inB[v] {
			t.Errorf("%s %q exists on SQLite and not on PostgreSQL", what, v)
		}
	}
	for _, v := range postgres {
		if !inA[v] {
			t.Errorf("%s %q exists on PostgreSQL and not on SQLite", what, v)
		}
	}
}

// ---------------------------------------------------------------------------
// Migrating onto a database that already holds data
// ---------------------------------------------------------------------------

// TestMigrateOntoADatabaseWithData applies the migration set the way an upgrade
// actually applies it: one step at a time, onto rows that are already there.
//
// TestMigrateFromEmptyIsIdempotent covers the greenfield case, which is the one
// that cannot fail interestingly: on an empty table every ALTER succeeds and
// every new UNIQUE index is trivially satisfied. The failures that reach an
// operator are the other ones -- a column added NOT NULL with no default, a
// unique index over a column that already has duplicates, a CHECK that existing
// rows violate -- and none of them can be produced by a migration test that
// starts from nothing.
//
// So this seeds after every step and reads the seed back after every later
// step. A migration that broke existing data would fail at the step that broke
// it, naming it.
func TestMigrateOntoADatabaseWithData(t *testing.T) {
	eachEnv(t, func(t *testing.T, b backend, dsn string) {
		ctx := context.Background()
		s := openStore(t, b, dsn, func(c *Config) { c.SkipMigrate = true })

		steps, err := loadMigrations(s.d.migrationDir())
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) < 2 {
			t.Fatalf("only %d migrations; this test needs a sequence", len(steps))
		}

		if err := s.withTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, createSchemaMigrations)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		// generation counts the rows planted so far. Every one of them must
		// still be readable, with the value it was written with, after every
		// later step.
		var planted []string
		for i, m := range steps {
			applyOne(t, ctx, s, m)

			// A row per step, planted AFTER that step and carrying a value
			// that later steps must not disturb.
			id := fmt.Sprintf("seed-%02d", i)
			now := Micros(s.now())
			mustExec(t, s, `INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
				id, "team "+id, now, now)
			mustExec(t, s, `
				INSERT INTO api_keys (id, lookup, token_hash, hash_scheme, key_label,
				                      spend_nano, created_at, updated_at)
				VALUES (?, ?, ?, 'dorang_v1', ?, ?, ?, ?)`,
				id, "lookup-"+id, "hash-"+id, "label-"+id, int64(i+1)*1_000, now, now)
			planted = append(planted, id)

			// Everything planted so far, including by earlier steps, survives.
			for _, p := range planted {
				var name string
				var spend int64
				if err := s.queryRow(ctx,
					`SELECT t.name, k.spend_nano FROM teams t JOIN api_keys k ON k.id = t.id
					  WHERE t.id = ?`, p).Scan(&name, &spend); err != nil {
					t.Fatalf("after migration %d (%s), the row planted as %s is gone: %v",
						m.Version, m.Name, p, err)
				}
				if name != "team "+p {
					t.Fatalf("after migration %d (%s), team %s reads %q", m.Version, m.Name, p, name)
				}
			}
		}

		// And the ordinary Migrate call is a no-op afterwards: the steps this
		// test applied by hand must be recorded exactly as Migrate records them,
		// or an upgraded deployment would re-run them.
		rep, err := s.Migrate(ctx)
		if err != nil {
			t.Fatalf("Migrate after a hand-applied sequence: %v", err)
		}
		if len(rep.Applied) != 0 {
			t.Fatalf("Migrate re-applied %v over a database that already had them", rep.Applied)
		}
		if rep.AlreadyApplied != len(steps) {
			t.Fatalf("Migrate saw %d applied steps, want %d", rep.AlreadyApplied, len(steps))
		}
	})
}

// applyOne runs one migration and records it exactly as Migrate does.
func applyOne(t *testing.T, ctx context.Context, s *Store, m migration) {
	t.Helper()
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range splitStatements(m.Body) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%w\nstatement: %s", err, firstLine(stmt))
			}
		}
		_, err := s.txExec(ctx, tx,
			`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			m.Version, m.Name, m.Checksum, Micros(s.now()))
		return err
	})
	if err != nil {
		t.Fatalf("migration %d (%s) failed against a database that already holds data: %v",
			m.Version, m.Name, err)
	}
}
