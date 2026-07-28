package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ziozzang/dorang/internal/store"
)

// Lock is a store-backed mutual-exclusion lease.
//
// It is deliberately a lease and not a mutex: a node that dies while holding a
// mutex holds it forever, and there is no supervisor in this design to notice.
// A lease expires, so the failure mode of a dead holder is a delay bounded by
// the TTL rather than a cluster that never elects again.
type Lock interface {
	// Acquire takes the lock, or renews it if this node already holds it, for
	// ttl. It reports whether this node holds the lock afterwards and the
	// fencing token of the current holder.
	//
	// The fencing token increases every time the lock changes hands and never
	// otherwise, so a write tagged with a stale token can be recognised as
	// coming from a superseded holder.
	Acquire(ctx context.Context, ttl time.Duration) (held bool, fence uint64, err error)

	// Release gives the lock up if this node holds it. Releasing a lock this
	// node does not hold is not an error: it is what a demoted node does on the
	// way out, and by then somebody else legitimately owns it.
	Release(ctx context.Context) error

	// Holder reports who holds the lock, until when, and with which fencing
	// token. A zero node id means nobody holds it.
	Holder(ctx context.Context) (nodeID string, expires time.Time, fence uint64, err error)
}

// leaseLock implements [Lock] as one row of capacity_leases.
//
// DESIGN 13 elects the leader "through a store lock". On PostgreSQL that is an
// advisory lock, taken here to serialize the compare-and-swap so that two
// campaigning nodes are ordered at the lock rather than by retrying against
// each other. On SQLite the immediate transaction has already done the same
// job, so the same code is correct with one fewer statement -- which matters,
// because a SQLite deployment is single-node and the leader is trivially
// itself, yet the election still has to be real enough to test.
//
// The row lives in capacity_leases with axis "leader": that table already
// carries exactly the columns a lease needs -- holder, acquisition, expiry --
// and adding a table for one row would be schema churn for nothing. The `used`
// column, which a capacity lease uses for consumption, carries the fencing
// token here; it is documented rather than inferred.
type leaseLock struct {
	c      conn
	id     string
	name   string
	nodeID string
	now    func() time.Time
}

// LeaderLockName is the axis_key of the cluster-wide leader lease. It is a
// constant so that a second implementation cannot elect a second leader by
// spelling the name differently.
const LeaderLockName = "cluster"

// NewLock builds a store lock. name scopes it, so that independent leaderships
// (the cluster leader, a future per-shard leader) cannot collide.
func NewLock(s *store.Store, name, nodeID string, now func() time.Time) (Lock, error) {
	if s == nil {
		return nil, errors.New("cluster: NewLock needs a store")
	}
	if nodeID == "" {
		return nil, errors.New("cluster: NewLock needs a node id")
	}
	if name == "" {
		name = LeaderLockName
	}
	if now == nil {
		now = time.Now
	}
	return &leaseLock{
		c:      newConn(s),
		id:     rowID("leader", name),
		name:   name,
		nodeID: nodeID,
		now:    now,
	}, nil
}

// Acquire is one compare-and-swap: take the row if nobody holds it or the
// holder's lease has expired, renew it if this node is the holder, and do
// nothing otherwise.
//
// The whole decision is one statement, so there is no window between reading
// the holder and writing it in which a second campaigner can slip through. The
// ON CONFLICT ... WHERE clause is what makes that true; an implementation that
// read first and wrote second would elect two leaders under exactly the load
// that makes election matter.
func (l *leaseLock) Acquire(ctx context.Context, ttl time.Duration) (bool, uint64, error) {
	if ttl <= 0 {
		return false, 0, errors.New("cluster: lock TTL must be positive")
	}
	now := l.now()
	nowUS := store.Micros(now)
	expUS := store.Micros(now.Add(ttl))

	var (
		held  bool
		fence uint64
	)
	err := l.c.withTx(ctx, func(ctx context.Context, t tx) error {
		if err := t.lock(ctx, "leader/"+l.name); err != nil {
			return err
		}
		// used is the fencing token: it advances only when the holder changes,
		// so a renewal by the incumbent leaves it alone and every takeover
		// makes every token issued before it recognisably stale.
		var got int64
		err := t.queryRow(ctx, `
			INSERT INTO capacity_leases (id, node_id, axis, axis_key, amount, used, acquired_at, expires_at)
			VALUES (?, ?, 'leader', ?, 1, 1, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
			    node_id     = excluded.node_id,
			    used        = CASE WHEN capacity_leases.node_id = excluded.node_id
			                       THEN capacity_leases.used ELSE capacity_leases.used + 1 END,
			    acquired_at = CASE WHEN capacity_leases.node_id = excluded.node_id
			                       THEN capacity_leases.acquired_at ELSE excluded.acquired_at END,
			    expires_at  = excluded.expires_at
			WHERE capacity_leases.node_id = excluded.node_id
			   OR capacity_leases.expires_at <= ?
			RETURNING used`,
			l.id, l.nodeID, l.name, nowUS, expUS, nowUS).Scan(&got)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Somebody else holds an unexpired lease. Not an error: losing an
			// election is the normal outcome for all but one node.
			held = false
			return nil
		case err != nil:
			return err
		}
		held, fence = true, uint64(got) //nolint:gosec // the token is a counter, never negative
		return nil
	})
	if err != nil {
		return false, 0, fmt.Errorf("cluster: acquire lock %s: %w", l.name, err)
	}
	return held, fence, nil
}

// Release expires this node's lease immediately, so a successor does not have
// to wait out the TTL after a clean handover. It touches nothing if this node
// is not the holder.
func (l *leaseLock) Release(ctx context.Context) error {
	_, err := l.c.exec(ctx,
		`UPDATE capacity_leases SET expires_at = ? WHERE id = ? AND node_id = ?`,
		store.Micros(l.now()), l.id, l.nodeID)
	if err != nil {
		return fmt.Errorf("cluster: release lock %s: %w", l.name, err)
	}
	return nil
}

// Holder reads the lease row.
func (l *leaseLock) Holder(ctx context.Context) (string, time.Time, uint64, error) {
	var (
		node string
		exp  int64
		used int64
	)
	err := l.c.queryRow(ctx,
		`SELECT node_id, expires_at, used FROM capacity_leases WHERE id = ?`, l.id).
		Scan(&node, &exp, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, 0, nil
	}
	if err != nil {
		return "", time.Time{}, 0, err
	}
	return node, store.TimeAt(exp), uint64(used), nil //nolint:gosec // counter
}
