package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/postgres/*.sql migrations/sqlite/*.sql
var migrationFS embed.FS

// migration is one ordered, embedded schema step.
type migration struct {
	Version  int64
	Name     string
	Checksum string
	Body     string
}

// AppliedMigration records one step this process applied.
type AppliedMigration struct {
	Version int64
	Name    string
}

// MigrateReport says what a Migrate call did. An idempotent second run reports
// zero Applied and the same Version, which is what the test asserts.
type MigrateReport struct {
	Applied []AppliedMigration
	// AlreadyApplied counts steps that were already recorded.
	AlreadyApplied int
	// Version is the highest applied version after the run.
	Version int64
}

// createSchemaMigrations is the one piece of DDL outside the migration files,
// because it must exist before they can be recorded. BIGINT rather than
// INTEGER: applied_at is unix microseconds, which overflows PostgreSQL's int4
// by five orders of magnitude. SQLite gives BIGINT integer affinity, so the
// same text serves both.
const createSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    BIGINT PRIMARY KEY,
    name       TEXT   NOT NULL,
    checksum   TEXT   NOT NULL,
    applied_at BIGINT NOT NULL
)`

// Migrate applies every embedded migration that is not yet recorded, in
// version order, inside one transaction.
//
// It is idempotent and safe to run concurrently from several nodes. PostgreSQL
// takes a transaction-scoped advisory lock; SQLite's transaction is already
// BEGIN IMMEDIATE. The loser of the race blocks, then finds every step
// recorded and applies nothing.
//
// An already-applied migration whose file has since changed is ErrDirtySchema.
// Silently ignoring the edit would leave two deployments with the same version
// number and different schemas, which is the failure this check exists for.
func (s *Store) Migrate(ctx context.Context) (MigrateReport, error) {
	migrations, err := loadMigrations(s.d.migrationDir())
	if err != nil {
		return MigrateReport{}, err
	}

	var rep MigrateReport
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.d.lockMigrations(ctx, tx); err != nil {
			return fmt.Errorf("store: migration lock: %w", err)
		}
		if _, err := tx.ExecContext(ctx, createSchemaMigrations); err != nil {
			return fmt.Errorf("store: create schema_migrations: %w", err)
		}

		applied, err := readApplied(ctx, s, tx)
		if err != nil {
			return err
		}

		rep = MigrateReport{}
		for _, m := range migrations {
			if have, ok := applied[m.Version]; ok {
				if have != m.Checksum {
					return fmt.Errorf("%w: version %d (%s): recorded %s, embedded %s",
						ErrDirtySchema, m.Version, m.Name, have, m.Checksum)
				}
				rep.AlreadyApplied++
				if m.Version > rep.Version {
					rep.Version = m.Version
				}
				continue
			}
			for _, stmt := range splitStatements(m.Body) {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("store: migration %d (%s): %w\nstatement: %s",
						m.Version, m.Name, err, firstLine(stmt))
				}
			}
			if _, err := s.txExec(ctx, tx,
				`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
				m.Version, m.Name, m.Checksum, Micros(s.now())); err != nil {
				return fmt.Errorf("store: record migration %d: %w", m.Version, err)
			}
			rep.Applied = append(rep.Applied, AppliedMigration{Version: m.Version, Name: m.Name})
			if m.Version > rep.Version {
				rep.Version = m.Version
			}
		}
		return nil
	})
	if err != nil {
		return MigrateReport{}, err
	}
	return rep, nil
}

// SchemaVersion reports the highest applied migration version, or 0 if the
// schema has never been migrated.
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	var v sql.NullInt64
	err := s.queryRow(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, err
	}
	return v.Int64, nil
}

func readApplied(ctx context.Context, s *Store, tx *sql.Tx) (map[int64]string, error) {
	rows, err := tx.QueryContext(ctx, s.rebind(`SELECT version, checksum FROM schema_migrations`))
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var (
			v int64
			c string
		)
		if err := rows.Scan(&v, &c); err != nil {
			return nil, err
		}
		out[v] = c
	}
	return out, rows.Err()
}

// loadMigrations reads and orders one dialect's embedded migrations. File
// names are NNNN_name.sql; the numeric prefix is the version and the ordering.
func loadMigrations(dir string) ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, path.Join("migrations", dir))
	if err != nil {
		return nil, fmt.Errorf("store: read embedded migrations: %w", err)
	}
	var out []migration
	seen := map[int64]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".sql")
		num, name, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("store: migration %q: want NNNN_name.sql", e.Name())
		}
		v, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("store: migration %q: bad version: %w", e.Name(), err)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("store: duplicate migration version %d (%s and %s)", v, prev, e.Name())
		}
		seen[v] = e.Name()

		body, err := migrationFS.ReadFile(path.Join("migrations", dir, e.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			Version:  v,
			Name:     name,
			Checksum: hex.EncodeToString(sum[:]),
			Body:     string(body),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// splitStatements splits a migration file into individual statements.
//
// It is not a SQL parser; it tracks exactly what a schema file can contain --
// line comments, block comments, single-quoted strings, double-quoted
// identifiers, and dollar-quoted bodies -- so that a semicolon inside any of
// them is not mistaken for a terminator. Statements are executed one at a time
// because pgx's extended protocol does not accept a multi-statement string.
func splitStatements(body string) []string {
	var (
		out []string
		cur strings.Builder
	)
	flush := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s != "" {
			out = append(out, s)
		}
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c == '-' && i+1 < len(body) && body[i+1] == '-':
			for i < len(body) && body[i] != '\n' {
				i++
			}
			cur.WriteByte('\n')
		case c == '/' && i+1 < len(body) && body[i+1] == '*':
			i += 2
			for i+1 < len(body) && !(body[i] == '*' && body[i+1] == '/') {
				i++
			}
			i++
			cur.WriteByte(' ')
		case c == '\'' || c == '"':
			quote := c
			cur.WriteByte(c)
			i++
			for i < len(body) {
				cur.WriteByte(body[i])
				if body[i] == quote {
					if i+1 < len(body) && body[i+1] == quote {
						i++
						cur.WriteByte(body[i])
						i++
						continue
					}
					break
				}
				i++
			}
		case c == '$':
			if tag, end, ok := dollarTag(body, i); ok {
				closeAt := strings.Index(body[end:], tag)
				if closeAt < 0 {
					cur.WriteString(body[i:])
					i = len(body)
					break
				}
				stop := end + closeAt + len(tag)
				cur.WriteString(body[i:stop])
				i = stop - 1
			} else {
				cur.WriteByte(c)
			}
		case c == ';':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// dollarTag recognises a $$ or $tag$ opener at i and returns the tag and the
// offset just past it.
func dollarTag(body string, i int) (tag string, end int, ok bool) {
	j := i + 1
	for j < len(body) {
		c := body[j]
		if c == '$' {
			return body[i : j+1], j + 1, true
		}
		isWord := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9' && j > i+1)
		if !isWord {
			return "", 0, false
		}
		j++
	}
	return "", 0, false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}
