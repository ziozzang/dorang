package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// leaseScopeShared is the quota_leases.scope value of a shared-mode counter
// row. It separates these rows from the block leases [Ledger] writes into the
// same table, so one reclaim pass can handle both without guessing.
const leaseScopeShared = "shared"

// LeaseStore is the shared-pg capacity and quota mode of DESIGN 5.6: "a lease
// table with advisory locks".
//
// # Shape
//
// One row of quota_leases per (counter key, node). The counter's value is the
// sum of the live rows, so three facts are one fact:
//
//   - how much of a limit is in use -- the sum;
//   - who is using it -- the node_id column;
//   - what happens when a holder dies -- the row expires and its units return,
//     with no supervisor and no explicit release.
//
// A single shared counter row would give the first and neither of the others.
// This is why DESIGN 5.6 says lease table rather than counter: the accounting
// and the failure recovery are the same mechanism.
//
// # Accuracy
//
// Exact. Every acquire is a read-modify-write inside one transaction, and the
// transaction is serialized against every other transaction touching the same
// key -- by pg_advisory_xact_lock on PostgreSQL, by the immediate transaction
// on SQLite. Two nodes cannot both see room for the last unit. The cost is one
// round trip on the hot path, which is what DESIGN 5.6 prices this mode at.
// [Publish] reports its maximum overshoot as zero, and that is a claim the
// tests check rather than a claim this comment makes.
//
// # Dialects
//
// Written against internal/store rather than against PostgreSQL specifically,
// so the identical code and the identical tests run on SQLite. That is not a
// convenience: it is what lets the overshoot property be tested by plain
// `go test`, and a mode whose accuracy claim can only be checked when an
// external service happens to be running is a mode whose accuracy claim is not
// checked.
//
// A LeaseStore is safe for concurrent use.
type LeaseStore struct {
	c      conn
	nodeID string
	now    func() time.Time
}

// NewLeaseStore builds the shared-pg backend for a node.
//
// The returned value implements [quota.SharedStore], so it drops into
// [quota.NewCoordinator] for the shared-pg and leased modes without either side
// knowing about the other.
func NewLeaseStore(s *store.Store, nodeID string, now func() time.Time) (*LeaseStore, error) {
	if s == nil {
		return nil, errors.New("cluster: NewLeaseStore needs a store")
	}
	if nodeID == "" {
		return nil, errors.New("cluster: NewLeaseStore needs a node id")
	}
	if now == nil {
		now = time.Now
	}
	return &LeaseStore{c: newConn(s), nodeID: nodeID, now: now}, nil
}

// compile-time proof that the shared modes have a real backend.
var _ quota.SharedStore = (*LeaseStore)(nil)

// Reserve grants up to want units against limit for key, and returns how many
// it granted -- zero when the limit is already reached.
//
// Expired rows are reclaimed in the same transaction that reads the sum, which
// is the only place it can happen without a race: reclaiming outside the lock
// would let a node see room that another node's expiring lease is about to give
// back, and grant it twice.
func (l *LeaseStore) Reserve(ctx context.Context, key string, want, limit int64, ttl time.Duration) (int64, error) {
	if want <= 0 {
		return 0, nil
	}
	if ttl <= 0 {
		return 0, errors.New("cluster: Reserve needs a positive TTL")
	}
	now := l.now()
	nowUS, expUS := store.Micros(now), store.Micros(now.Add(ttl))

	var granted int64
	err := l.c.withTx(ctx, func(ctx context.Context, t tx) error {
		if err := t.lock(ctx, "shared/"+key); err != nil {
			return err
		}
		if _, err := t.exec(ctx,
			`DELETE FROM quota_leases WHERE scope = ? AND scope_key = ? AND expires_at <= ?`,
			leaseScopeShared, key, nowUS); err != nil {
			return err
		}
		var used int64
		if err := t.queryRow(ctx,
			`SELECT COALESCE(SUM(amount), 0) FROM quota_leases WHERE scope = ? AND scope_key = ?`,
			leaseScopeShared, key).Scan(&used); err != nil {
			return err
		}
		room := limit - used
		if room <= 0 {
			granted = 0
			return nil
		}
		granted = minInt64(want, room)
		_, err := t.exec(ctx, `
			INSERT INTO quota_leases
			    (id, node_id, scope, scope_key, "window", metric, amount, used, acquired_at, expires_at)
			VALUES (?, ?, ?, ?, '-', '-', ?, 0, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
			    amount     = quota_leases.amount + excluded.amount,
			    expires_at = excluded.expires_at`,
			l.rowID(key), l.nodeID, leaseScopeShared, key, granted, nowUS, expUS)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("cluster: reserve %s: %w", key, err)
	}
	return granted, nil
}

// Release returns units this node holds. It cannot return units another node
// holds, which is a property rather than a limitation: a release is always the
// undo of an acquire by the same node.
func (l *LeaseStore) Release(ctx context.Context, key string, n int64) error {
	if n <= 0 {
		return nil
	}
	g := greatest(l.c.dia)
	err := l.c.withTx(ctx, func(ctx context.Context, t tx) error {
		if err := t.lock(ctx, "shared/"+key); err != nil {
			return err
		}
		if _, err := t.exec(ctx,
			`UPDATE quota_leases SET amount = `+g+`(amount - ?, 0) WHERE id = ?`,
			n, l.rowID(key)); err != nil {
			return err
		}
		// An empty lease is not a lease. Dropping it keeps the table's size
		// proportional to what is actually held rather than to every key any
		// node ever touched.
		_, err := t.exec(ctx, `DELETE FROM quota_leases WHERE id = ? AND amount <= 0`, l.rowID(key))
		return err
	})
	if err != nil {
		return fmt.Errorf("cluster: release %s: %w", key, err)
	}
	return nil
}

// Value reports the counter: the sum of every live lease on the key, across all
// nodes.
func (l *LeaseStore) Value(ctx context.Context, key string) (int64, error) {
	var v int64
	err := l.c.queryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0) FROM quota_leases
		  WHERE scope = ? AND scope_key = ? AND expires_at > ?`,
		leaseScopeShared, key, store.Micros(l.now())).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return v, nil
}

// Held reports how much of the key this node itself holds.
func (l *LeaseStore) Held(ctx context.Context, key string) (int64, error) {
	var v int64
	err := l.c.queryRow(ctx,
		`SELECT amount FROM quota_leases WHERE id = ? AND expires_at > ?`,
		l.rowID(key), store.Micros(l.now())).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// ReclaimExpired drops shared leases whose TTL has passed, returning their
// units to the counter, and reports how many rows it removed.
//
// This is leader work (DESIGN 13, lease rebalancing). [LeaseStore.Reserve] also
// reclaims the key it is about to touch, so a busy key needs no sweep at all;
// this exists for the keys nobody is asking about, whose units would otherwise
// stay held by a node that no longer exists.
func (l *LeaseStore) ReclaimExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := l.c.exec(ctx,
		`DELETE FROM quota_leases WHERE scope = ? AND expires_at <= ?`,
		leaseScopeShared, store.Micros(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReclaimNode drops every shared lease held by a node, whether or not it has
// expired. It is what the leader does with a node whose heartbeat lapsed: a
// node that is gone is not coming back to release its own units, and waiting
// out a TTL that its own writes were extending would leave them held for as
// long as the node was alive.
func (l *LeaseStore) ReclaimNode(ctx context.Context, nodeID string) (int64, error) {
	if nodeID == "" {
		return 0, nil
	}
	res, err := l.c.exec(ctx,
		`DELETE FROM quota_leases WHERE scope = ? AND node_id = ?`, leaseScopeShared, nodeID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Close releases every unit this node still holds, so a draining node's share
// is available to the others immediately rather than after a TTL (DESIGN 13,
// draining).
func (l *LeaseStore) Close(ctx context.Context) error {
	_, err := l.c.exec(ctx,
		`DELETE FROM quota_leases WHERE scope = ? AND node_id = ?`, leaseScopeShared, l.nodeID)
	return err
}

func (l *LeaseStore) rowID(key string) string { return rowID(leaseScopeShared, key, l.nodeID) }
