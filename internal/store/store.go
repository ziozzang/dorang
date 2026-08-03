package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
	_ "modernc.org/sqlite"             // database/sql driver "sqlite", pure Go
)

// Dialect names a supported storage backend.
type Dialect string

// Supported dialects. These are the storage.driver values of DESIGN 4.2.
const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// Partitioning describes how the ledger is physically divided, which is the
// one storage difference a caller may legitimately need to know about. It is
// surfaced rather than hidden so that retention behaviour is never guessed.
type Partitioning string

// Partitioning modes.
const (
	// PartitioningNone: the ledger is one table and retention deletes rows.
	PartitioningNone Partitioning = "none"
	// PartitioningDaily: the ledger is range-partitioned by day and retention
	// drops whole partitions.
	PartitioningDaily Partitioning = "daily"
)

// MaxAmountNano bounds a single stored monetary amount to 1e9 currency units.
// The int64 ceiling is ~9.22e9 units in nano; stopping an order of magnitude
// short leaves headroom for the SUM() in a spend query to stay exact rather
// than wrap. DESIGN 8.3: every write is range-checked.
const MaxAmountNano int64 = 1_000_000_000_000_000_000

// Defaults for Config. They are the notebook profile of DESIGN 0.2: correct
// with no tuning, and overridable for the tiers above it.
const (
	DefaultMaxTimeRange = 92 * 24 * time.Hour
	DefaultPageSize     = 100
	DefaultMaxPageSize  = 1000
	// DefaultPartitionAhead is the "at least two days ahead" of DESIGN 9.5.
	DefaultPartitionAhead = 3
)

// LegacyAuth mirrors auth.legacy of DESIGN 4.2. Enabling legacy_sha256
// verification requires a sunset date; after it, legacy verification is
// refused. An open-ended legacy window is a permanent legacy window.
type LegacyAuth struct {
	Enabled bool
	Until   time.Time
}

func (l LegacyAuth) allowed(now time.Time) bool {
	return l.Enabled && !l.Until.IsZero() && now.Before(l.Until)
}

// Config configures Open.
type Config struct {
	// Driver selects the dialect. Required.
	Driver Dialect

	// DSN is a filesystem path for SQLite, or a libpq/pgx URL for PostgreSQL.
	// A SQLite DSN that already carries a query string is used verbatim;
	// otherwise the pragmas dorang needs (WAL, busy_timeout, foreign_keys) and
	// _txlock=immediate are added.
	DSN string

	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration

	// Pepper is the HMAC pepper for hash_scheme dorang_v1, from
	// server.key_pepper_env. Without it dorang_v1 hashing and verification
	// return ErrNoPepper.
	Pepper []byte

	// Legacy controls whether legacy_sha256 credentials may still verify.
	Legacy LegacyAuth

	// MaxTimeRange caps the width of a ledger query's time range.
	// Zero means DefaultMaxTimeRange.
	MaxTimeRange time.Duration

	// DefaultPageSize and MaxPageSize bound ledger pagination.
	DefaultPageSize int
	MaxPageSize     int

	// PartitionAhead is how many days of ledger partitions to keep pre-created
	// ahead of now. Zero means DefaultPartitionAhead. Values below 2 are
	// raised to 2: DESIGN 9.5 requires at least two days of headroom.
	PartitionAhead int

	// SkipMigrate suppresses the automatic migration Open otherwise performs.
	// Migrations are applied at startup (DESIGN 9.1); this exists for tooling
	// that wants to inspect the state first.
	SkipMigrate bool

	// Now is the clock. Zero means time.Now. Injectable so that partition
	// rollover and reservation expiry are testable without waiting.
	Now func() time.Time
}

func (c *Config) fill() error {
	switch c.Driver {
	case DialectSQLite, DialectPostgres:
	case "":
		return errors.New("store: Config.Driver is required")
	default:
		return fmt.Errorf("store: unknown driver %q", c.Driver)
	}
	if c.DSN == "" {
		return errors.New("store: Config.DSN is required")
	}
	if c.MaxTimeRange <= 0 {
		c.MaxTimeRange = DefaultMaxTimeRange
	}
	if c.DefaultPageSize <= 0 {
		c.DefaultPageSize = DefaultPageSize
	}
	if c.MaxPageSize <= 0 {
		c.MaxPageSize = DefaultMaxPageSize
	}
	if c.DefaultPageSize > c.MaxPageSize {
		c.DefaultPageSize = c.MaxPageSize
	}
	if c.PartitionAhead <= 0 {
		c.PartitionAhead = DefaultPartitionAhead
	}
	if c.PartitionAhead < 2 {
		c.PartitionAhead = 2
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return nil
}

// Store is the persistence handle. It is safe for concurrent use.
//
// There is one implementation for both dialects; the differences are held by
// an unexported dialect strategy. A caller never branches on Driver, and no
// method's contract changes with it.
type Store struct {
	db  *sql.DB
	cfg Config
	d   dialect

	// stmts counts statements issued through this Store. It exists so that
	// "lookup is always one query" (DESIGN 2.4) is an assertion a test can
	// make rather than a claim a comment makes.
	stmts atomic.Uint64
}

// StatementCount reports how many statements this Store has issued. Intended
// for tests and diagnostics.
func (s *Store) StatementCount() uint64 { return s.stmts.Load() }

// Open connects, applies migrations (unless Config.SkipMigrate), and, on a
// partitioned dialect, ensures the ledger has partitions for today and the
// configured days ahead -- so a freshly opened store can accept a write
// immediately and keeps accepting them across midnight.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if err := cfg.fill(); err != nil {
		return nil, err
	}

	var (
		db  *sql.DB
		d   dialect
		err error
	)
	switch cfg.Driver {
	case DialectSQLite:
		dsn := sqliteDSN(cfg.DSN)
		db, err = sql.Open("sqlite", dsn)
		if err != nil {
			return nil, fmt.Errorf("store: open sqlite: %w", err)
		}
		d = sqliteDialect{}
		// An in-memory database is per-connection unless shared, so a pool
		// larger than one silently becomes a pool of distinct databases.
		if cfg.MaxOpenConns == 0 && isMemoryDSN(cfg.DSN) {
			cfg.MaxOpenConns = 1
		}
	case DialectPostgres:
		db, err = sql.Open("pgx", cfg.DSN)
		if err != nil {
			return nil, fmt.Errorf("store: open postgres: %w", err)
		}
		d = postgresDialect{}
	}

	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if cfg.Driver == DialectSQLite {
		if err := enableWAL(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
	}

	s := &Store{db: db, cfg: cfg, d: d}

	if !cfg.SkipMigrate {
		if _, err := s.Migrate(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
		if s.Partitioning() != PartitioningNone {
			if _, err := s.EnsurePartitions(ctx, cfg.Now(), cfg.PartitionAhead); err != nil {
				_ = db.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

// Close releases the underlying pool.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying pool for operations this package does not model.
// Anything routed through it loses the dialect abstraction; prefer a method.
func (s *Store) DB() *sql.DB { return s.db }

// Driver reports the dialect in use. Provided for diagnostics and for the
// health endpoint, not as a branch point for query construction.
func (s *Store) Driver() Dialect { return s.cfg.Driver }

// Partitioning reports how the ledger is physically divided, so that a caller
// scheduling maintenance knows whether retention will drop partitions or
// delete rows. See Maintain.
func (s *Store) Partitioning() Partitioning { return s.d.partitioning() }

func (s *Store) now() time.Time { return s.cfg.Now() }

// rebind rewrites the portable ? placeholders into the dialect's own form.
func (s *Store) rebind(q string) string { return s.d.rebind(q) }

func (s *Store) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	s.stmts.Add(1)
	return s.db.ExecContext(ctx, s.rebind(q), args...)
}

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	s.stmts.Add(1)
	return s.db.QueryContext(ctx, s.rebind(q), args...)
}

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	s.stmts.Add(1)
	return s.db.QueryRowContext(ctx, s.rebind(q), args...)
}

// withTx runs fn in a write transaction. On SQLite the transaction begins
// IMMEDIATE, so a writer takes the write lock up front instead of discovering
// mid-transaction that it cannot upgrade -- the deadlock that busy_timeout
// cannot rescue.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) txExec(ctx context.Context, tx *sql.Tx, q string, args ...any) (sql.Result, error) {
	s.stmts.Add(1)
	return tx.ExecContext(ctx, s.rebind(q), args...)
}

// ---------------------------------------------------------------------------
// Encoding helpers. Exported because callers construct and read the structs
// this package stores, and the encoding must not be folklore.
// ---------------------------------------------------------------------------

// Micros converts a time to the canonical storage encoding: unix microseconds
// in UTC. The zero time maps to 0, which is also how a NULL timestamp reads
// back, so "unset" survives a round trip.
func Micros(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMicro()
}

// TimeAt is the inverse of Micros.
func TimeAt(us int64) time.Time {
	if us == 0 {
		return time.Time{}
	}
	return time.UnixMicro(us).UTC()
}

// checkAmount enforces the nano-unit range. DESIGN 8.3.
func checkAmount(nano int64, what string) error {
	if nano > MaxAmountNano || nano < -MaxAmountNano {
		return fmt.Errorf("%w: %s = %d nano", ErrAmountRange, what, nano)
	}
	return nil
}

// NewID returns a 128-bit random identifier in lower-case hex. Ledger rows are
// written from several nodes with no coordination, so identifiers are random
// rather than sequential.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// nullStr turns "" into a SQL NULL. Optional foreign identifiers are stored as
// NULL so an index over them is not full of empty strings that mean nothing.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullZeroInt turns a zero into a SQL NULL, for a column whose zero is a claim rather
// than a value. A utilization factor of 0 is not "1.0x applied"; it is "this rule does
// not price on utilization", and a NULL says that where a 0 would not.
func nullZeroInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// nullMicros turns the zero time into a SQL NULL.
func nullMicros(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return Micros(t)
}

func str(ns sql.NullString) string { return ns.String }

func nullInt(v sql.NullInt64) int64 { return v.Int64 }

// sqliteDSN adds the per-connection pragmas dorang depends on, unless the
// caller has supplied their own query string.
//
//   - busy_timeout: a second writer waits instead of failing immediately.
//   - foreign_keys=ON: SQLite ignores foreign keys unless asked.
//   - synchronous=NORMAL: the WAL-appropriate durability point.
//   - _txlock=immediate: see Store.withTx.
//
// journal_mode is deliberately NOT here. It is a persistent property of the
// database rather than of a connection, and setting it needs a brief exclusive
// lock that busy_timeout does not cover -- so two processes opening the same
// notebook database at once would race, and one would fail to start. It is set
// once, with a retry, by enableWAL.
func sqliteDSN(path string) string {
	if strings.Contains(path, "?") {
		return path
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	return "file:" + path + "?" + q.Encode()
}

// enableWAL puts the database into write-ahead logging so readers do not block
// the writer, retrying while another opener holds the lock it needs.
func enableWAL(ctx context.Context, db *sql.DB) error {
	const attempts = 100
	var lastErr error
	for i := 0; i < attempts; i++ {
		var mode string
		err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode)
		switch {
		case err != nil:
			lastErr = err
		case strings.EqualFold(mode, "wal"), strings.EqualFold(mode, "memory"):
			// "memory" is what an in-memory database reports; it has no
			// journal to switch and nothing to contend over.
			return nil
		default:
			lastErr = fmt.Errorf("store: sqlite journal_mode is %q, want wal", mode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("store: could not enable WAL: %w", lastErr)
}

func isMemoryDSN(path string) bool {
	return strings.Contains(path, ":memory:") || strings.Contains(path, "mode=memory")
}
