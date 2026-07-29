package cluster

import "errors"

// Sentinel errors. Callers match with errors.Is.
var (
	// ErrLocalInCluster refuses cluster.enabled with capacity_mode "local".
	//
	// DESIGN 5.6 and risk W1: revision 1 of the design called this a
	// recommendation. It is not. Node-local counting is exact on one node only,
	// so with N nodes every ceiling is counted N times over and the provider
	// sees up to N-fold the configured limit. The upstream 429s that follow
	// cascade into the fallback chain (DESIGN 7.6) and consume the capacity of
	// unrelated models in the same class, so the failure surfaces far from its
	// cause.
	//
	// internal/config refuses this configuration at load. This package refuses
	// it again at construction, because a guard that exists in only one place
	// is a guard that a second entry point walks around.
	ErrLocalInCluster = errors.New(
		"cluster: cluster.enabled with capacity_mode \"local\" refuses to start: " +
			"every node would carry the whole limit; use \"leased\" or \"shared-pg\" " +
			"(\"shared-redis\" is the fourth mode and needs a client this build does not ship) (DESIGN 5.6)")

	// ErrNoRedisClient refuses capacity_mode "shared-redis" when no Redis
	// client was supplied.
	//
	// This build ships the shared-redis PROTOCOL -- [RedisClient], the Lua each
	// script stands for, and [NewRedisShared] -- and no client that speaks it,
	// so that the mode costs no dependency. What it must never do is fall back
	// to the SQL lease table: that would be a coordinator reporting
	// Mode() == "shared-redis" and publishing "one round trip to Redis per
	// acquire" (DESIGN 5.6) while touching PostgreSQL. The figures are what an
	// operator sizes a cluster against, so a mode that is not the mode it
	// claims is worse than a mode that refuses.
	//
	// internal/config refuses the same configuration at load, for the reason
	// [ErrLocalInCluster] is refused twice: one entry point is not a guard.
	ErrNoRedisClient = errors.New(
		"cluster: capacity_mode \"shared-redis\" needs a Redis client, which this build does not ship: " +
			"use \"shared-pg\" or \"leased\", or supply one through quota.CoordinatorConfig.Shared " +
			"with cluster.NewRedisShared (DESIGN 5.6)")

	// ErrNotLeader reports an operation that only the leader may perform,
	// attempted by a node that does not hold the lease.
	ErrNotLeader = errors.New("cluster: this node is not the leader")

	// ErrLeadershipLost is the cancellation cause of a leader-scoped context
	// when the lease is lost. A task that sees it must stop rather than race
	// the new leader (DESIGN 13).
	ErrLeadershipLost = errors.New("cluster: leadership lost")

	// ErrNotRegistered reports a heartbeat from a node with no registry row.
	ErrNotRegistered = errors.New("cluster: node is not registered")

	// ErrDuplicateNodeID reports that another live process is running under this
	// node's id. It refuses to start, and an incumbent that observes it stops
	// leading.
	//
	// # Why this is not something the lease can catch
	//
	// The fencing token closes the TIMING case: a superseded holder is
	// recognisable because the token advances every time the lock changes
	// HOLDER. Two processes claiming one id are not a change of holder. The
	// second acquire satisfies `capacity_leases.node_id = excluded.node_id`, so
	// the store reads it as the incumbent renewing: it succeeds, the token does
	// not move, and both processes then hold a token equal to the row's and both
	// pass [Fence.Check]. No amount of fencing helps, because fencing is an
	// argument about terms and this is an argument about who.
	//
	// # Why it refuses to start rather than merely refusing to lead
	//
	// The node id keys more than the leader lease. It keys this process's row in
	// `nodes`, whose heartbeat is what tells the cluster the node is alive and
	// what [LeaseReclaimJob] walks to return a dead node's leases; it keys this
	// node's share of every leased limit in `quota_leases`; and it keys the
	// budget draw in [Ledger]. A second process that started and declined to
	// lead would still be drawing budget from the first one's lease block,
	// consuming its slice of every ceiling, and beating on its row so that
	// neither process is ever declared dead and neither one's leases are ever
	// reclaimed. DESIGN 5.6 publishes an overshoot of `something x (nodes - 1)`
	// computed from [Registry.Count], and two processes behind one row make that
	// figure wrong by the one thing it exists to keep honest.
	//
	// Refusing to start is also the loud failure, which DESIGN 9.2 asks for
	// here: two nodes picking up the same batch pays for every finished row
	// twice. A process that will not come up halts a rollout and appears in
	// every orchestrator's own alerting; a process that quietly never leads is
	// discovered when a sweep is missed.
	//
	// # What it costs
	//
	// A node whose row is still being beaten on cannot take the id, so an
	// UNCLEAN restart under a configured id waits out the heartbeat TTL
	// ([DefaultNodeTTL]) before it can start. That is deliberate: within that
	// window the cluster still believes the previous process is alive and still
	// attributes its leases to it, so adopting the id early is precisely the
	// defect. A clean drain deregisters ([Node.Close]) and its replacement
	// starts at once.
	ErrDuplicateNodeID = errors.New("cluster: another process is already running under this node id")

	// ErrClosed reports use of a closed node, ledger or lease store.
	ErrClosed = errors.New("cluster: closed")

	// ErrExhausted reports that a durable counter has no room left: a budget
	// with no money or a quota window with no units.
	//
	// For a budget this is DESIGN 6.4's terminal outcome, not a fallback
	// condition -- failing is correct, and there is no cheaper deployment that
	// makes the money reappear.
	ErrExhausted = errors.New("cluster: limit exhausted")

	// ErrLimitTooSmall reports a limit below MinLeasable under the leased mode.
	// Single-digit limits cannot be usefully divided across nodes (DESIGN 5.6).
	ErrLimitTooSmall = errors.New("cluster: limit is too small to lease; use a shared mode")

	// ErrUnknownMode reports a capacity mode this package does not implement.
	ErrUnknownMode = errors.New("cluster: unknown capacity mode")
)
