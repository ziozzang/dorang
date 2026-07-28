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
	// ttl. It reports whether this node holds the lock afterwards, the fencing
	// token of the current holder, and the instant the lease it just wrote
	// expires.
	//
	// The fencing token increases every time the lock changes hands and never
	// otherwise, so a write tagged with a stale token can be recognised as
	// coming from a superseded holder.
	//
	// expires is the lease the AUTHORITY recorded, not ttl added to the
	// caller's clock after the call returned. The difference is the whole of
	// the guard band: a caller that computed the expiry itself would place it
	// one round trip later than the row says, and would then believe itself
	// safe for that long past the moment a successor may legitimately take
	// over. See [Election.Campaign], which derives its safe window from this
	// value and from nothing else.
	Acquire(ctx context.Context, ttl time.Duration) (held bool, fence uint64, expires time.Time, err error)

	// Release gives the lock up if this node holds it. Releasing a lock this
	// node does not hold is not an error: it is what a demoted node does on the
	// way out, and by then somebody else legitimately owns it.
	Release(ctx context.Context) error

	// Holder reports who holds the lock, until when, and with which fencing
	// token. A zero node id means nobody holds it.
	Holder(ctx context.Context) (nodeID string, expires time.Time, fence uint64, err error)

	// Fence returns the precondition a leader-owned write carries under token,
	// or nil if this lock cannot express one -- in which case leader work is
	// guarded by the guard band alone and by nothing that survives a clock
	// disagreement. Every Lock backed by the store returns one.
	Fence(token uint64) *Fence
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
func (l *leaseLock) Acquire(ctx context.Context, ttl time.Duration) (bool, uint64, time.Time, error) {
	if ttl <= 0 {
		return false, 0, time.Time{}, errors.New("cluster: lock TTL must be positive")
	}
	now := l.now()
	nowUS := store.Micros(now)
	expUS := store.Micros(now.Add(ttl))

	var (
		held    bool
		fence   uint64
		expires time.Time
	)
	err := l.c.withTx(ctx, func(ctx context.Context, t tx) error {
		if err := t.lock(ctx, "leader/"+l.name); err != nil {
			return err
		}
		// used is the fencing token: it advances only when the holder changes,
		// so a renewal by the incumbent leaves it alone and every takeover
		// makes every token issued before it recognisably stale.
		//
		// expires_at comes back from the row rather than being reconstructed by
		// the caller. It is the same number this statement wrote, so returning
		// it looks redundant -- and is not, because it is the number the NEXT
		// campaigner tests against, and a caller that recomputed it from its own
		// clock would be comparing against a different one.
		var (
			got int64
			exp int64
		)
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
			RETURNING used, expires_at`,
			l.id, l.nodeID, l.name, nowUS, expUS, nowUS).Scan(&got, &exp)
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
		expires = store.TimeAt(exp)
		return nil
	})
	if err != nil {
		return false, 0, time.Time{}, fmt.Errorf("cluster: acquire lock %s: %w", l.name, err)
	}
	return held, fence, expires, nil
}

// Fence returns the precondition a leader-owned write carries under token.
func (l *leaseLock) Fence(token uint64) *Fence {
	return &Fence{c: l.c, lockID: l.id, name: l.name, nodeID: l.nodeID, token: token, now: l.now}
}

// Fence is proof that a leader-owned write belongs to the term still in force.
//
// # What it is for
//
// The guard band ends leadership before the lease does, so that an incumbent
// has stopped by the time a successor may start. That argument is about
// DURATIONS, and it holds only while both nodes measure them the same way. Two
// things break it and neither is exotic: an acquire slower than the guard band
// (closed by deriving the safe window from [Lock.Acquire]'s expiry rather than
// from the clock afterwards) and a clock disagreement wider than the guard band
// (not closeable by any amount of arithmetic on unsynchronised clocks).
//
// A fencing token is the mechanism that does not care. It advances every time
// the lock changes hands, so a holder that has been superseded is recognisable
// as superseded by a single read of the row -- no clock, no duration, no
// assumption about how far apart two nodes' idea of "now" is.
//
// # What it can and cannot guarantee
//
// It can reject leader-owned work that goes through this package: [Fence.Check]
// is what [Node] calls before dispatching each due leader job, and [Fence.In]
// makes the assertion part of a caller's own transaction, serialized against
// the election's compare-and-swap by the same advisory lock, so a handover
// cannot land between the assertion and the write.
//
// It cannot fence a write this package never sees. A [Job] that opens its own
// transaction through internal/store -- retention, partition pre-creation, the
// budget reservation sweep, batch assignment -- is fenced only at dispatch,
// which leaves the window between the check and that job's own commit. Such a
// job that must be fenced has to carry the token into its own write, which is
// why [Fence.In] and [Node.Fence] are exported.
type Fence struct {
	c      conn
	lockID string
	name   string
	nodeID string
	token  uint64
	now    func() time.Time
}

// Token returns the fencing token this precondition asserts.
func (f *Fence) Token() uint64 {
	if f == nil {
		return 0
	}
	return f.token
}

// NodeID returns the node the token was issued to.
func (f *Fence) NodeID() string {
	if f == nil {
		return ""
	}
	return f.nodeID
}

// fenceQuery reads the three columns a term is proved by. It is one string so
// that [Fence.Check] and [Fence.In] cannot come to differ in what they test.
const fenceQuery = `SELECT node_id, used, expires_at FROM capacity_leases WHERE id = ?`

// Check asserts the term against the store: one read, outside any transaction.
//
// It is a CHECK and deliberately not a fence. Nothing serializes it against a
// handover, so it answers "had this term ended a moment ago" -- which is what a
// caller about to dispatch work wants to know, and is not enough for a caller
// about to write. Use [Fence.In] for that.
//
// A nil Fence passes. A caller with no token is not claiming to be fenced, and
// inventing a failure for it would push every non-leader path -- a test, a
// single-node gateway with no election at all -- into an error it cannot act on.
func (f *Fence) Check(ctx context.Context) error {
	if f == nil {
		return nil
	}
	return f.verify(f.c.queryRow(ctx, fenceQuery, f.lockID))
}

// In asserts the term inside a caller's transaction, so that the assertion and
// the write it guards commit together or not at all.
//
// It takes the same advisory lock [leaseLock.Acquire] does, which is what makes
// this a fence rather than a check: a handover either commits before this
// transaction reads the row -- and is seen -- or waits behind it. Take it as
// the FIRST statement of the transaction, before any other scope, so that the
// package's lock ordering stays leader-before-everything and two fenced writers
// cannot deadlock.
func (f *Fence) In(ctx context.Context, t tx) error {
	if f == nil {
		return nil
	}
	if err := t.lock(ctx, "leader/"+f.name); err != nil {
		return err
	}
	return f.verify(t.queryRow(ctx, fenceQuery, f.lockID))
}

// verify decides one scanned lease row against this term.
func (f *Fence) verify(row *sql.Row) error {
	var (
		node string
		used int64
		exp  int64
	)
	err := row.Scan(&node, &used, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: the leader lease row is gone", ErrLeadershipLost)
	}
	if err != nil {
		return err
	}
	switch {
	case node != f.nodeID:
		return fmt.Errorf("%w: %s holds the lease, this is %s", ErrLeadershipLost, node, f.nodeID)
	case uint64(used) != f.token: //nolint:gosec // the token is a counter, never negative
		return fmt.Errorf("%w: the lease is at fence %d, this term is %d", ErrLeadershipLost, used, f.token)
	case !store.TimeAt(exp).After(f.now()):
		return fmt.Errorf("%w: the lease expired at %s", ErrLeadershipLost, store.TimeAt(exp))
	}
	return nil
}

// fenceKey is the context key the leader term travels under.
type fenceKey struct{}

// WithFence returns a context carrying the leader term a write belongs to.
//
// [Node] sets it on every leader job's context, so a job that reclaims through
// [Ledger] or [LeaseStore] is fenced without asking and without those methods
// growing a parameter that a non-leader caller would have to pass nil for. It
// is the same shape as a deadline: ambient authority of the call, not an
// argument about the data.
//
// A context with no fence is not a failure. [Fence.Check] and [Fence.In] treat
// a nil Fence as "this caller is not claiming a term", which is the honest
// answer for [Ledger.ReclaimExpired] called from a test or from a single-node
// gateway that has no election at all.
func WithFence(ctx context.Context, f *Fence) context.Context {
	if f == nil {
		return ctx
	}
	return context.WithValue(ctx, fenceKey{}, f)
}

// FenceFrom returns the leader term a context carries, or nil.
func FenceFrom(ctx context.Context) *Fence {
	f, _ := ctx.Value(fenceKey{}).(*Fence)
	return f
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
