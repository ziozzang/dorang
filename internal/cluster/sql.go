package cluster

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ziozzang/dorang/internal/store"
)

// This file is the whole of this package's SQL plumbing.
//
// internal/store owns the schema and every query over the tables it models.
// Coordination writes to four of those tables -- nodes, capacity_leases,
// quota_leases and budget_state -- and does it through [store.Store.DB] rather
// than by growing a dozen cluster-shaped methods on Store that a single-node
// deployment would never call. The dialect difference is confined here, in the
// same spirit as internal/store's own dialect strategy: one place, not many.

// conn is a dialect-aware handle over a Store's connection pool.
type conn struct {
	db  *sql.DB
	dia store.Dialect
}

func newConn(s *store.Store) conn { return conn{db: s.DB(), dia: s.Driver()} }

func (c conn) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return c.db.ExecContext(ctx, rebind(c.dia, q), args...)
}

func (c conn) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return c.db.QueryContext(ctx, rebind(c.dia, q), args...)
}

func (c conn) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return c.db.QueryRowContext(ctx, rebind(c.dia, q), args...)
}

// tx is one transaction with its dialect attached.
type tx struct {
	t   *sql.Tx
	dia store.Dialect
}

func (t tx) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.t.ExecContext(ctx, rebind(t.dia, q), args...)
}

func (t tx) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return t.t.QueryRowContext(ctx, rebind(t.dia, q), args...)
}

func (t tx) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.t.QueryContext(ctx, rebind(t.dia, q), args...)
}

// txAttempts bounds the retries withTx makes against a store that refused the
// write for a reason another attempt can resolve.
const txAttempts = 8

// withTx runs fn inside a write transaction.
//
// The two dialects reach the same guarantee by different means, and both are
// load-bearing for every read-modify-write in this package:
//
//   - SQLite has exactly one writer, and internal/store opens every connection
//     with _txlock=immediate, so the transaction takes the write lock up front
//     rather than discovering mid-transaction that it cannot upgrade. That is
//     mutual exclusion for free, which is why SQLite needs no advisory lock and
//     why DESIGN 13 can call a SQLite deployment single-node without losing
//     anything.
//   - PostgreSQL allows concurrent writers, so the caller takes a
//     transaction-scoped advisory lock as its first statement (see [tx.lock]).
//     It is released by COMMIT or ROLLBACK, so a crashed node cannot wedge the
//     cluster.
//
// Both dialects can refuse a write for a reason that is transient, and both are
// retried here so that no caller has to know which store it is talking to. A
// SQLite writer that loses the race waits out busy_timeout and then reports
// SQLITE_BUSY. A PostgreSQL transaction that loses a lock cycle is aborted with
// deadlock_detected. Neither is a failure of the operation; both are the store
// saying "not on this attempt". See [isBusy].
func (c conn) withTx(ctx context.Context, fn func(context.Context, tx) error) error {
	var err error
	for i := 0; i < txAttempts; i++ {
		err = c.txOnce(ctx, fn)
		if err == nil || !isBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * 2 * time.Millisecond):
		}
	}
	return err
}

func (c conn) txOnce(ctx context.Context, fn func(context.Context, tx) error) error {
	t, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = t.Rollback() }()
	if err := fn(ctx, tx{t: t, dia: c.dia}); err != nil {
		return err
	}
	return t.Commit()
}

// lock serializes this transaction against every other one naming the same
// scope. On PostgreSQL that is pg_advisory_xact_lock; on SQLite the immediate
// transaction has already done it, so this is a no-op rather than a weaker
// guarantee.
func (t tx) lock(ctx context.Context, scope string) error {
	if t.dia != store.DialectPostgres {
		return nil
	}
	_, err := t.exec(ctx, "SELECT pg_advisory_xact_lock(?)", advisoryKey(scope))
	return err
}

// advisoryKey derives PostgreSQL's 64-bit advisory-lock key from a name, so
// every node agrees on it without a shared constant to mistype. It matches how
// internal/store derives its migration and partition lock keys.
func advisoryKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("dorang.cluster/"))
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64()) //nolint:gosec // deliberate wrap into the signed space advisory locks use
}

// greatest is the dialect's two-argument scalar maximum. SQLite spells it
// max(), PostgreSQL GREATEST() -- and PostgreSQL's max() is an aggregate, so
// using the wrong one is a syntax error rather than a subtle wrong answer.
func greatest(d store.Dialect) string {
	if d == store.DialectPostgres {
		return "GREATEST"
	}
	return "max"
}

// rebind rewrites the portable ? placeholders into the dialect's own form.
// Quoted runs are copied verbatim: a ? inside them is data or an identifier.
func rebind(d store.Dialect, q string) string {
	if d != store.DialectPostgres || !strings.ContainsRune(q, '?') {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); i++ {
		switch c := q[i]; c {
		case '\'', '"':
			b.WriteByte(c)
			i++
			for i < len(q) {
				if q[i] == c {
					if i+1 < len(q) && q[i+1] == c {
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

// isBusy reports whether err is the store refusing this attempt at the write
// rather than refusing the write.
//
// It used to test only for SQLite's messages, and the whole classifier was
// therefore shaped by the dialect that needs it least: SQLite has one writer,
// so its contention is common and benign, while PostgreSQL has many, so its
// contention is rarer and worse. On PostgreSQL every one of those strings is
// absent, so a transaction the server aborted so that another could proceed --
// which is a retry instruction, and which the server has already rolled back --
// came back to a caller of this package as a permanent failure. In a two-node
// deployment that is a leader job that gives up, a lease that is not reclaimed,
// or a budget draw that refuses a request the ceiling had room for.
//
// PostgreSQL is classified by SQLSTATE, the way internal/store already
// classifies a missing partition, because pgx exports a typed error and a
// classifier that reads localized server text is a classifier that stops
// working when the server's lc_messages changes:
//
//   - 40001 serialization_failure: the transaction could not be serialized.
//     Not reachable at READ COMMITTED, which is what this package opens, but it
//     is the same instruction and costs nothing to honour.
//   - 40P01 deadlock_detected: this transaction was chosen as the victim of a
//     lock cycle and rolled back so another could proceed. Retrying is the
//     documented response.
//
// Nothing else is retried. A constraint violation, a missing column and a
// refused connection are all permanent on a second attempt too, and retrying
// them would turn a clear failure into eight of them.
//
// SQLite stays matched on the message because modernc.org/sqlite exports no
// typed error for it.
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", "40P01":
			return true
		}
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") ||
		strings.Contains(s, "database is locked") ||
		strings.Contains(s, "database table is locked")
}

// rowID derives a stable, bounded primary key from a composite identity. The
// lease tables are keyed by a single TEXT id, and the identities this package
// leases against are composites of caller-supplied strings with no length
// bound, so they are hashed rather than concatenated.
func rowID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// asBool normalizes a scanned boolean. PostgreSQL returns a bool and SQLite an
// integer, and a caller of this package should not have to know which.
func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []byte:
		return len(t) == 1 && t[0] != 0
	case string:
		return t == "1" || strings.EqualFold(t, "true")
	}
	return false
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func orInt64(v, def int64) int64 {
	if v <= 0 {
		return def
	}
	return v
}
