package store

import (
	"context"
	"time"
)

// DESIGN §11.2c, risk W11 — the durable invalidation bus.
//
// §11.2's hot path answers from a lock-free snapshot with a TTL, which means a
// revoked, pended or rotation-cut key keeps serving until the snapshot
// refreshes, on every node independently. Nothing in the design said how long
// that window is: the mechanism existed and its guarantee was never stated.
//
// This table is what makes it stateable. A revocation, a pend and an early
// grace cut write a row here; every node polls for rows after its watermark and
// drops those keys from its snapshot on receipt. The TTL becomes the fallback
// for a node that missed the message rather than the mechanism, and the
// clustered worst case is the poll interval plus one store round trip — a
// number, published, the same rule §5.6 applies to overshoot.
//
// The table is append-only and pruned by age. It is deliberately not a queue
// with acknowledgements: an invalidation is idempotent, so a subscriber that
// replays a window is correct, and per-subscriber cursors would make the bus a
// thing that can leak rows when a node never comes back.

// KeyInvalidation is one published message.
type KeyInvalidation struct {
	// Seq is the store-assigned monotonic sequence. A subscriber's watermark.
	Seq int64
	// KeyID is the durable key id. It is authoritative: a node that learned a
	// secret this publisher never saw must still drop it, which dropping by
	// lookup alone cannot do.
	KeyID string
	// Lookups are the index keys the publisher knew about. An optimization, not
	// the contract.
	Lookups []string
	// Cause is the spelling of auth.InvalidationCause.
	Cause string
	// CreatedAt is when the control was applied.
	CreatedAt time.Time
}

// PublishInvalidation appends a message and returns it with its sequence.
//
// The sequence comes from the database rather than from a caller's clock,
// because a watermark compared across nodes must not depend on their clocks
// agreeing. Skew that would merely misorder a log line here loses a revocation.
func (s *Store) PublishInvalidation(ctx context.Context, inv KeyInvalidation) (KeyInvalidation, error) {
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = s.now()
	}
	if inv.Cause == "" {
		inv.Cause = "unspecified"
	}
	res, err := s.exec(ctx, `
		INSERT INTO key_invalidations (key_id, lookups, cause, created_at) VALUES (?, ?, ?, ?)`,
		inv.KeyID, encodeStrings(inv.Lookups), inv.Cause, Micros(inv.CreatedAt))
	if err != nil {
		return KeyInvalidation{}, err
	}
	// LastInsertId is available on SQLite and not on pgx. Where it is not, the
	// sequence is read back by the subscriber's poll, which is the only thing
	// that needs it; the publisher's copy being zero costs nothing.
	if id, err := res.LastInsertId(); err == nil && id > 0 {
		inv.Seq = id
	}
	return inv, nil
}

// InvalidationsSince returns every message after a watermark, oldest first.
//
// limit bounds one poll so that a node rejoining after a long outage does not
// read the whole table into memory in one statement; the caller polls again
// from the new watermark. Zero means [DefaultInvalidationPoll].
func (s *Store) InvalidationsSince(ctx context.Context, seq int64, limit int) ([]KeyInvalidation, error) {
	if limit <= 0 {
		limit = DefaultInvalidationBatch
	}
	rows, err := s.query(ctx, `
		SELECT seq, key_id, lookups, cause, created_at
		  FROM key_invalidations WHERE seq > ? ORDER BY seq ASC LIMIT ?`, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyInvalidation
	for rows.Next() {
		var (
			inv     KeyInvalidation
			lookups string
			created int64
		)
		if err := rows.Scan(&inv.Seq, &inv.KeyID, &lookups, &inv.Cause, &created); err != nil {
			return nil, err
		}
		inv.Lookups = decodeStrings(lookups)
		inv.CreatedAt = TimeAt(created)
		out = append(out, inv)
	}
	return out, rows.Err()
}

// DefaultInvalidationBatch bounds one poll.
const DefaultInvalidationBatch = 512

// LatestInvalidationSeq returns the highest sequence in the table, or zero.
//
// A node that has just reloaded the whole credential set starts from here
// rather than from zero: it has already seen the effect of every message that
// precedes its own read, and replaying them would only re-fetch rows.
func (s *Store) LatestInvalidationSeq(ctx context.Context) (int64, error) {
	var seq *int64
	if err := s.queryRow(ctx, `SELECT MAX(seq) FROM key_invalidations`).Scan(&seq); err != nil {
		return 0, err
	}
	if seq == nil {
		return 0, nil
	}
	return *seq, nil
}

// PruneInvalidations deletes messages older than retain and reports how many.
//
// retain must exceed the longest outage a node may return from and still be
// caught up by replay; below that, a returning node has to reload
// (auth.Rejoin), which is what it does anyway on start. The table is small —
// one row per revocation — so this is hygiene, not a scaling measure.
func (s *Store) PruneInvalidations(ctx context.Context, retain time.Duration) (int64, error) {
	if retain <= 0 {
		return 0, nil
	}
	cutoff := s.now().Add(-retain)
	res, err := s.exec(ctx, `DELETE FROM key_invalidations WHERE created_at < ?`, Micros(cutoff))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}
