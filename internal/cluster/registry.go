package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ziozzang/dorang/internal/store"
)

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
	// ID is the node's stable identity. Two processes sharing an ID are one
	// node as far as every lease in this package is concerned, which is why a
	// generated ID is per process, not per host.
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
	c      conn
	nodeID string
	ttl    time.Duration
	now    func() time.Time
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
	return &Registry{c: newConn(s), nodeID: nodeID, ttl: orDuration(ttl, DefaultNodeTTL), now: now}, nil
}

// NodeTTL reports the heartbeat lapse after which a node is considered gone.
func (r *Registry) NodeTTL() time.Duration { return r.ttl }

// Register records this node and beats once, so that a node is visible from the
// instant it starts rather than from its first tick.
//
// Registration is an upsert on the node id: a restarted node reclaims its own
// row and updates started_at, which is what lets [NodeInfo.StartedAt]
// distinguish a restart from an uninterrupted run.
func (r *Registry) Register(ctx context.Context, info NodeInfo) error {
	now := r.now()
	if info.StartedAt.IsZero() {
		info.StartedAt = now
	}
	_, err := r.c.exec(ctx, `
		INSERT INTO nodes (node_id, address, version, started_at, last_heartbeat, is_leader, metadata)
		VALUES (?, ?, ?, ?, ?, ?, '{}')
		ON CONFLICT (node_id) DO UPDATE SET
		    address        = excluded.address,
		    version        = excluded.version,
		    started_at     = excluded.started_at,
		    last_heartbeat = excluded.last_heartbeat,
		    is_leader      = excluded.is_leader`,
		r.nodeID, info.Address, info.Version,
		store.Micros(info.StartedAt), store.Micros(now), false)
	if err != nil {
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
func (r *Registry) Heartbeat(ctx context.Context, leader bool) error {
	res, err := r.c.exec(ctx,
		`UPDATE nodes SET last_heartbeat = ?, is_leader = ? WHERE node_id = ?`,
		store.Micros(r.now()), leader, r.nodeID)
	if err != nil {
		return fmt.Errorf("cluster: heartbeat %s: %w", r.nodeID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotRegistered, r.nodeID)
	}
	return nil
}

// Deregister removes this node's row. It is the draining path of DESIGN 13:
// readiness off, in-flight requests finish, leases and reservations released,
// then exit -- and a node that left on purpose should not have to wait out a
// heartbeat TTL before the cluster stops counting it.
func (r *Registry) Deregister(ctx context.Context) error {
	_, err := r.c.exec(ctx, `DELETE FROM nodes WHERE node_id = ?`, r.nodeID)
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
