package store

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// dialect is the only place a dialect difference is allowed to live. Queries
// are written once, in portable SQL with ? placeholders; this interface covers
// the four things that genuinely cannot be written once.
type dialect interface {
	name() Dialect
	// rebind rewrites ? placeholders into the dialect's parameter syntax.
	rebind(q string) string
	// migrationDir is the subdirectory of the embedded migrations FS.
	migrationDir() string
	// lockMigrations serializes concurrent migration runs from several nodes.
	lockMigrations(ctx context.Context, tx *sql.Tx) error
	partitioning() Partitioning
	// ensureDays creates the ledger partitions for the given UTC days, if any
	// are missing. Days already covered cost one catalog lookup and nothing else.
	ensureDays(ctx context.Context, s *Store, days []time.Time) ([]string, error)
	// dropPartitionsBefore drops whole partitions older than cutoff.
	dropPartitionsBefore(ctx context.Context, s *Store, table string, cutoff time.Time) ([]string, error)
	// isMissingPartition reports whether err is "this row has nowhere to go",
	// which the writer recovers from by creating the partition and retrying.
	isMissingPartition(err error) bool
}

// partitionedTables are the day-partitioned ledger tables. All three are keyed
// by the same ts, so one maintenance pass covers them and retention keeps them
// consistent with each other.
var partitionedTables = []string{"request_logs", "request_log_tags", "request_traces"}

// ---------------------------------------------------------------------------
// SQLite
// ---------------------------------------------------------------------------

type sqliteDialect struct{}

func (sqliteDialect) name() Dialect              { return DialectSQLite }
func (sqliteDialect) rebind(q string) string     { return q }
func (sqliteDialect) migrationDir() string       { return "sqlite" }
func (sqliteDialect) partitioning() Partitioning { return PartitioningNone }

// lockMigrations is a no-op because Store.withTx already opened the
// transaction with BEGIN IMMEDIATE (see sqliteDSN), which is SQLite's whole
// mutual-exclusion story: the second node blocks on the write lock until
// busy_timeout expires, and by then the first has committed.
func (sqliteDialect) lockMigrations(context.Context, *sql.Tx) error { return nil }

func (sqliteDialect) ensureDays(context.Context, *Store, []time.Time) ([]string, error) {
	return nil, ErrNoPartitioning
}

func (sqliteDialect) dropPartitionsBefore(context.Context, *Store, string, time.Time) ([]string, error) {
	return nil, ErrNoPartitioning
}

func (sqliteDialect) isMissingPartition(error) bool { return false }

// ---------------------------------------------------------------------------
// PostgreSQL
// ---------------------------------------------------------------------------

type postgresDialect struct{}

func (postgresDialect) name() Dialect              { return DialectPostgres }
func (postgresDialect) migrationDir() string       { return "postgres" }
func (postgresDialect) partitioning() Partitioning { return PartitioningDaily }

// migrationLockKey is a stable advisory-lock key derived from a fixed string,
// so every dorang build agrees on it without a shared constant to mistype.
var migrationLockKey = func() int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("dorang.schema_migrations"))
	return int64(h.Sum64()) //nolint:gosec // deliberate wrap into the signed space pg_advisory_lock uses
}()

// partitionLockKey serializes partition maintenance between nodes, so two
// leaders racing to create tomorrow do not collide in the catalog.
var partitionLockKey = func() int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("dorang.partitions"))
	return int64(h.Sum64()) //nolint:gosec // as above
}()

// lockMigrations takes a transaction-scoped advisory lock. It is released by
// COMMIT or ROLLBACK, so a crashed migrator cannot wedge the cluster.
func (postgresDialect) lockMigrations(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockKey)
	return err
}

func (postgresDialect) rebind(q string) string {
	if !strings.ContainsRune(q, '?') {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch c {
		case '\'', '"':
			// Copy the quoted run verbatim; a ? inside it is data or an
			// identifier, not a placeholder. Doubled quotes escape.
			quote := c
			b.WriteByte(c)
			i++
			for i < len(q) {
				if q[i] == quote {
					if i+1 < len(q) && q[i+1] == quote {
						b.WriteByte(q[i])
						b.WriteByte(q[i+1])
						i += 2
						continue
					}
					b.WriteByte(q[i])
					break
				}
				b.WriteByte(q[i])
				i++
			}
		case '-':
			if i+1 < len(q) && q[i+1] == '-' {
				for i < len(q) && q[i] != '\n' {
					b.WriteByte(q[i])
					i++
				}
				if i < len(q) {
					b.WriteByte(q[i])
				}
				continue
			}
			b.WriteByte(c)
		case '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// isMissingPartition matches SQLSTATE 23514 with PostgreSQL's "no partition of
// relation ... found for row". That is the exact error the first insert after
// midnight raises when nobody pre-created the day, and it is the error R1-15
// exists to prevent -- so the writer recognises it and recovers rather than
// dropping the row.
func (postgresDialect) isMissingPartition(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23514" && strings.Contains(pgErr.Message, "no partition of relation")
}

func (postgresDialect) ensureDays(ctx context.Context, s *Store, days []time.Time) ([]string, error) {
	if len(days) == 0 {
		return nil, nil
	}
	var created []string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// Serialize maintenance between nodes so two of them racing to create
		// tomorrow do not collide in the catalog.
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", partitionLockKey); err != nil {
			return err
		}
		for _, day := range days {
			for _, parent := range partitionedTables {
				name, made, err := createDayPartition(ctx, tx, parent, day)
				if err != nil {
					return err
				}
				if made {
					created = append(created, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// createDayPartition creates one day's partition if it is missing. It reports
// whether it did the creating, so EnsurePartitions can say what changed rather
// than what it looked at.
func createDayPartition(ctx context.Context, tx *sql.Tx, parent string, day time.Time) (string, bool, error) {
	name := partitionName(parent, day)
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
		return "", false, err
	}
	if exists {
		return name, false, nil
	}
	lo := Micros(day)
	hi := Micros(day.AddDate(0, 0, 1))
	// Identifiers here are constructed from a fixed table list and a date, so
	// there is nothing caller-controlled to inject; bounds are integers.
	stmt := "CREATE TABLE IF NOT EXISTS " + quoteIdent(name) +
		" PARTITION OF " + quoteIdent(parent) +
		" FOR VALUES FROM (" + strconv.FormatInt(lo, 10) + ") TO (" + strconv.FormatInt(hi, 10) + ")"
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		// Two nodes can pass the existence check together; the loser sees
		// duplicate_table and has nothing left to do.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "42P07" || pgErr.Code == "23505") {
			return name, false, nil
		}
		return "", false, err
	}
	return name, true, nil
}

func (postgresDialect) dropPartitionsBefore(ctx context.Context, s *Store, table string, cutoff time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.relname FROM pg_inherits i
		   JOIN pg_class c ON c.oid = i.inhrelid
		  WHERE i.inhparent = to_regclass($1)`, table)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	cut := dayFloor(cutoff)
	var dropped []string
	for _, name := range candidates {
		day, ok := partitionDay(table, name)
		if !ok {
			// Not ours to reason about: leave it alone and say nothing rather
			// than drop a table whose naming we do not understand.
			continue
		}
		if !day.Before(cut) {
			continue
		}
		if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+quoteIdent(name)); err != nil {
			return dropped, err
		}
		dropped = append(dropped, name)
	}
	return dropped, nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

const partitionDateLayout = "20060102"

func partitionName(parent string, day time.Time) string {
	return parent + "_" + day.UTC().Format(partitionDateLayout)
}

// partitionDay parses a partition name back into its day, and refuses anything
// that is not exactly parent_YYYYMMDD.
func partitionDay(parent, name string) (time.Time, bool) {
	suffix, ok := strings.CutPrefix(name, parent+"_")
	if !ok || len(suffix) != len(partitionDateLayout) {
		return time.Time{}, false
	}
	d, err := time.ParseInLocation(partitionDateLayout, suffix, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return d, true
}

func dayFloor(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func hourFloor(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 0, 0, 0, time.UTC)
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// validIdent reports whether s is a plain unquoted SQL identifier. Used for
// the one identifier a caller supplies: the import source table.
func validIdent(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
