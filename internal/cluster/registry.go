package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ziozzang/dorang/internal/store"
)

// This file also owns the identity half of DESIGN 13, which is not the same
// question as the leadership half and is not answered by the same mechanism.
// See [ErrDuplicateNodeID] for why a fencing token cannot reach it.

// DefaultNodeTTL is how long a node may go without a heartbeat before it is
// considered gone and its leases become reclaimable.
//
// It is deliberately several heartbeat intervals: declaring a node dead is not
// free -- its leases are returned and its share of every leased limit is handed
// to somebody else -- so a single missed beat, which a GC pause or a slow store
// write can produce, must not do it.
const DefaultNodeTTL = 30 * time.Second

// NodeInfo is one row of the node registry (DESIGN 9.2, table `nodes`).
type NodeInfo struct {
	// ID is the node's stable identity, and it is the only field here that is
	// an identity: address and version describe a node, this one names it.
	//
	// Two processes sharing an ID would be one node to every lease in this
	// package -- one row here, one leadership lease, one share of every leased
	// limit -- so the second one is refused rather than accommodated. See
	// [ErrDuplicateNodeID], and [Registry.Incarnation] for what "second" is
	// decided by. A generated ID is per process, not per host, which is why the
	// default cannot produce this at all.
	ID string
	// Address is where other nodes and operators can reach this one.
	Address string
	// Version is the build this node is running. It is recorded so that a
	// mixed-version cluster is visible rather than inferred from behaviour.
	Version string
	// StartedAt is when this node's process started. It distinguishes a
	// restarted node from a node that never went away, which a heartbeat alone
	// cannot.
	StartedAt time.Time
	// LastHeartbeat is when this node last reported itself alive.
	LastHeartbeat time.Time
	// IsLeader is the node's own last claim about leadership. It is published
	// state for operators, never the authority -- the lease row in
	// capacity_leases is (see [Election]).
	IsLeader bool
}

// Alive reports whether this node's heartbeat is within ttl of now.
func (n NodeInfo) Alive(now time.Time, ttl time.Duration) bool {
	return !n.LastHeartbeat.IsZero() && now.Sub(n.LastHeartbeat) < ttl
}

// Registry is the node registry and heartbeat of DESIGN 13.
//
// A Registry is safe for concurrent use; it holds no state of its own beyond
// its configuration, so every answer it gives comes from the store and is
// therefore the same answer every node gets.
type Registry struct {
	c           conn
	nodeID      string
	incarnation string
	ttl         time.Duration
	now         func() time.Time
}

// NewRegistry builds a registry for one node. ttl zero means [DefaultNodeTTL].
func NewRegistry(s *store.Store, nodeID string, ttl time.Duration, now func() time.Time) (*Registry, error) {
	if s == nil {
		return nil, errors.New("cluster: NewRegistry needs a store")
	}
	if nodeID == "" {
		return nil, errors.New("cluster: NewRegistry needs a node id")
	}
	if now == nil {
		now = time.Now
	}
	return &Registry{
		c:           newConn(s),
		nodeID:      nodeID,
		incarnation: `{"incarnation":"` + store.NewID() + `"}`,
		ttl:         orDuration(ttl, DefaultNodeTTL),
		now:         now,
	}, nil
}

// NodeTTL reports the heartbeat lapse after which a node is considered gone.
func (r *Registry) NodeTTL() time.Duration { return r.ttl }

// Incarnation is this PROCESS's identity, as distinct from its node id.
//
// It is generated per Registry and never configured, so no two processes can
// carry the same one however their configuration was written. It is what makes
// "another process is running under my id" an observation rather than an
// inference: the node id says which node a row is about, and this says which
// process wrote it.
//
// It lives in `nodes.metadata`, a column nothing else reads, and the choice is
// documented here rather than inferred -- the same arrangement, for the same
// reason, as [leaseLock]'s use of `capacity_leases.used` for the fencing token.
// The value is a canonical one-key JSON object so the column keeps holding what
// its type says it holds, and so that comparing two of them is string equality
// with no parsing in the hot statement.
func (r *Registry) Incarnation() string { return r.incarnation }

// Register records this node and beats once, so that a node is visible from the
// instant it starts rather than from its first tick.
//
// It is a compare-and-swap, not an upsert, and the difference is the whole of
// [ErrDuplicateNodeID]. The row is taken when it is free, when its heartbeat has
// lapsed, or when it is already this process's own; it is NOT taken from a
// process that is still beating on it. Two nodes deployed with one id therefore
// produce one registered node and one refusal, rather than two nodes the cluster
// counts as one -- which is the state in which every leader-only job runs twice
// and every leased limit is divided by a node count that is short by one.
//
// The decision is one statement. An implementation that read the row and then
// wrote it would elect two holders under exactly the condition that makes this
// matter, which is two processes starting at the same moment.
//
// A restart is not a duplicate. A node that died left a row whose heartbeat
// lapses within [Registry.NodeTTL]; the restart adopts it, updates started_at,
// and reclaims its own leases instead of waiting out theirs -- which is the
// reason to configure a stable `cluster.node_id` at all. A node that drained
// cleanly deregistered, so its replacement starts at once.
func (r *Registry) Register(ctx context.Context, info NodeInfo) error {
	now := r.now()
	if info.StartedAt.IsZero() {
		info.StartedAt = now
	}
	var got string
	err := r.c.queryRow(ctx, `
		INSERT INTO nodes (node_id, address, version, started_at, last_heartbeat, is_leader, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (node_id) DO UPDATE SET
		    address        = excluded.address,
		    version        = excluded.version,
		    started_at     = excluded.started_at,
		    last_heartbeat = excluded.last_heartbeat,
		    is_leader      = excluded.is_leader,
		    metadata       = excluded.metadata
		WHERE nodes.metadata = excluded.metadata
		   OR nodes.last_heartbeat <= ?
		RETURNING metadata`,
		r.nodeID, info.Address, info.Version,
		store.Micros(info.StartedAt), store.Micros(now), false, r.incarnation,
		store.Micros(now.Add(-r.ttl))).Scan(&got)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The row exists, it is not this process's, and it is still being beaten
		// on. Somebody else is this node.
		return r.duplicate(ctx)
	case err != nil:
		return fmt.Errorf("cluster: register node %s: %w", r.nodeID, err)
	}
	return nil
}

// Heartbeat reports this node alive, and publishes its leadership claim.
//
// A node whose heartbeat lapses is considered gone and its leases become
// reclaimable ([Registry.Dead], [Ledger.ReclaimNode]). Returning
// [ErrNotRegistered] rather than silently inserting is deliberate: a heartbeat
// that finds no row means the row was pruned while this node believed itself
// alive, and continuing as if nothing happened is how a node that the cluster
// has already written off keeps holding leases.
//
// It is scoped by incarnation as well as by id, so it also answers the half
// [Registry.Register] cannot reach. A process frozen past the TTL -- a long GC
// pause, a suspended container, a paused debugger -- is indistinguishable from a
// dead one, so a successor may legitimately have adopted its row while it was
// away. It is not the one refusing to start; it is the one already running, and
// it learns here, from the row, that its identity is no longer its own. That is
// the same argument [Fence] makes about elapsed time, applied to who rather than
// to when.
func (r *Registry) Heartbeat(ctx context.Context, leader bool) error {
	res, err := r.c.exec(ctx,
		`UPDATE nodes SET last_heartbeat = ?, is_leader = ? WHERE node_id = ? AND metadata = ?`,
		store.Micros(r.now()), leader, r.nodeID, r.incarnation)
	if err != nil {
		return fmt.Errorf("cluster: heartbeat %s: %w", r.nodeID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return r.duplicate(ctx)
	}
	return nil
}

// duplicate says which of the two ways a write scoped by incarnation can match
// nothing actually happened: the row is gone, or it belongs to somebody else.
//
// They are reported apart because the responses differ and are close to
// opposite. A missing row is recovered from by registering again -- it is what a
// node does when a [Registry.Prune] raced it. A row held by another process is
// not recoverable at all, and registering again would take the id from a process
// that is using it.
func (r *Registry) duplicate(ctx context.Context) error {
	var meta string
	err := r.c.queryRow(ctx, `SELECT metadata FROM nodes WHERE node_id = ?`, r.nodeID).Scan(&meta)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrNotRegistered, r.nodeID)
	case err != nil:
		return fmt.Errorf("cluster: read node %s: %w", r.nodeID, err)
	case meta == r.incarnation:
		// The row is this process's after all, so the write matched nothing for
		// some other reason. Nothing here can say what, and claiming a duplicate
		// on this evidence would stop a healthy node.
		return fmt.Errorf("%w: %s", ErrNotRegistered, r.nodeID)
	}
	return fmt.Errorf("%w: node id %q is held by another process (this process is %s, "+
		"the row is %s). Give every node a distinct cluster.node_id, or leave it empty "+
		"and let one be derived per process (DESIGN 9.2)",
		ErrDuplicateNodeID, r.nodeID, r.incarnation, meta)
}

// Deregister removes this node's row. It is the draining path of DESIGN 13:
// readiness off, in-flight requests finish, leases and reservations released,
// then exit -- and a node that left on purpose should not have to wait out a
// heartbeat TTL before the cluster stops counting it.
//
// Scoped by incarnation, because `nodes` is keyed by node id and a DELETE by id
// alone deletes whoever holds it. A process that was superseded while it was
// away would otherwise remove, on its way out, the row of the process that
// legitimately took the id over -- and that node then looks dead to
// [LeaseReclaimJob] while it is serving.
func (r *Registry) Deregister(ctx context.Context) error {
	_, err := r.c.exec(ctx,
		`DELETE FROM nodes WHERE node_id = ? AND metadata = ?`, r.nodeID, r.incarnation)
	return err
}

// List returns every registered node, oldest start first.
func (r *Registry) List(ctx context.Context) ([]NodeInfo, error) {
	rows, err := r.c.query(ctx, `
		SELECT node_id, address, version, started_at, last_heartbeat, is_leader
		  FROM nodes ORDER BY started_at, node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NodeInfo
	for rows.Next() {
		var (
			n             NodeInfo
			started, beat int64
			leader        any
			addr, ver     sql.NullString
		)
		if err := rows.Scan(&n.ID, &addr, &ver, &started, &beat, &leader); err != nil {
			return nil, err
		}
		n.Address, n.Version = addr.String, ver.String
		n.StartedAt, n.LastHeartbeat = store.TimeAt(started), store.TimeAt(beat)
		n.IsLeader = asBool(leader)
		out = append(out, n)
	}
	return out, rows.Err()
}

// Live returns the nodes whose heartbeat is within the TTL.
func (r *Registry) Live(ctx context.Context) ([]NodeInfo, error) {
	return r.partition(ctx, true)
}

// Dead returns the nodes whose heartbeat has lapsed. Their leases are
// reclaimable; DESIGN 13 makes that the leader's job.
func (r *Registry) Dead(ctx context.Context) ([]NodeInfo, error) {
	return r.partition(ctx, false)
}

func (r *Registry) partition(ctx context.Context, alive bool) ([]NodeInfo, error) {
	all, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	now := r.now()
	out := make([]NodeInfo, 0, len(all))
	for _, n := range all {
		if n.Alive(now, r.ttl) == alive {
			out = append(out, n)
		}
	}
	return out, nil
}

// Count reports how many nodes are currently live. It is what a caller feeds to
// [Publish] as the node count, so that a published overshoot figure describes
// the cluster that exists rather than the one the configuration imagined.
func (r *Registry) Count(ctx context.Context) (int, error) {
	live, err := r.Live(ctx)
	if err != nil {
		return 0, err
	}
	return len(live), nil
}

// Prune deletes node rows whose heartbeat lapsed longer ago than grace, and
// returns how many it removed.
//
// grace is on top of the TTL, not instead of it: a node is considered gone as
// soon as its heartbeat lapses, but its row is kept for a while afterwards so
// that an operator looking at a failure can still see which node it was. Only
// the row is delayed -- the lease reclaim that matters for correctness happens
// on the TTL.
func (r *Registry) Prune(ctx context.Context, grace time.Duration) (int64, error) {
	cutoff := r.now().Add(-r.ttl - grace)
	res, err := r.c.exec(ctx,
		`DELETE FROM nodes WHERE last_heartbeat < ? AND node_id <> ?`,
		store.Micros(cutoff), r.nodeID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
