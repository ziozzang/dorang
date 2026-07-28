// Package cluster coordinates several dorang nodes: who is registered, who
// leads, who holds which share of a limit, and what happens to all of that when
// a node dies.
//
// It owns nothing on the request path. DESIGN 13 makes the request path
// stateless and any node able to serve any request; this package exists so that
// periodic work is done once instead of N times, and so that a limit configured
// once is not enforced N times over.
//
// # The hard guard
//
// cluster.enabled with capacity_mode "local" refuses to start ([Guard],
// [ErrLocalInCluster]). Revision 1 of the design called this a recommendation.
// It is not, and the reason is worth stating because the symptom is so far from
// the cause: node-local counting is exact on one node only, so N nodes each
// carry the whole ceiling and the provider sees up to N-fold the configured
// limit. The upstream 429s that follow cascade into the fallback chain
// (DESIGN 7.6) and consume the capacity of unrelated models in the same class.
// By the time anyone notices, the failure looks like a routing problem in a
// model nobody changed.
//
// internal/config refuses the combination at load. This package refuses it
// again at construction, and [Node.Coordinator] overrides the mode rather than
// accepting one, because a guard with a single entry point is a guard that the
// second entry point walks around. The mode is immutable after [New] and there
// is no setter; a test asserts the shape as well as the behaviour.
//
// # Accuracy is a number
//
// DESIGN 5.6: "Every mode publishes its maximum possible overshoot as a number.
// 'Approximately accurate' is not an acceptable specification." [Publish] is
// that number, with the arithmetic that produced it and the hot-path cost that
// buys it:
//
//	local         limit x (nodes - 1)    no store access; permitted only on one node
//	shared-redis  0                      one Redis round trip per acquire
//	shared-pg     0                      one store round trip per acquire
//	leased        block x (nodes - 1)    one round trip per block, none in between
//
// The leased figure is the concurrent bound: a node holds one lease per key, so
// at most one reclaimed-but-still-spending block per other node can exist at a
// time. Repeated crashes inside one period can each contribute again, which is
// a property of the arithmetic rather than of this implementation, and is
// stated here rather than left implicit.
//
// A test runs every mode under concurrent load from several nodes and asserts
// the measured overshoot never exceeds the published one -- and, for the modes
// that publish zero, that they admit exactly the limit rather than merely no
// more than it. A mode that under-admits is not exact, it is only safe, and an
// operator who sized a cluster on that figure would be short of capacity.
//
// # Leadership, and how it ends
//
// [Election] holds a lease row through a store lock ([NewLock]): a
// compare-and-swap in one statement, serialized on PostgreSQL by
// pg_advisory_xact_lock and on SQLite by the immediate transaction that dialect
// already takes. SQLite deployments are single-node, so their leader is
// trivially themselves -- but the election is real on both, which is what lets
// the two-node behaviour be tested by plain `go test`.
//
// The interesting half is un-electing. Four mechanisms, all needed:
//
//  1. Every promotion opens a leader-scoped context; every demotion cancels it
//     with [ErrLeadershipLost]. Leader jobs take that context, so losing
//     leadership mid-task stops the task.
//  2. Leadership expires from the node's own clock, with no store access.
//     [Election.IsLeader] demotes a node whose safe window has passed, so a
//     process frozen by a GC pause, a suspended container or a paused debugger
//     steps down on the way back instead of resuming as a second leader.
//  3. The safe window ends one guard band before the lease does, because a
//     successor may take the lease the instant it expires and an incumbent that
//     stopped exactly then would still overlap by however long its last task
//     takes to notice.
//  4. A fencing token accompanies leadership for the case no timeout closes: a
//     write already in flight at the moment of demotion. It advances when the
//     lock changes hands and never on a renewal.
//
// The leader owns partition pre-creation and retention, expiry sweeps for both
// capacity reservations (DESIGN 5.3) and budget reservations (DESIGN 6.4),
// lease rebalancing, and -- through [RollupCompactionJob] and
// [BatchAssignmentJob] -- rollup compaction and batch assignment, which live in
// packages this one must not import.
//
// # Closing W9
//
// [Ledger] is quota and budget that survive a restart. Risk W9: they were
// in-memory only, so a restart reset the windows -- safe for concurrency, wrong
// for accounting, since a monthly budget silently started over. A lease that
// does not outlive its node is not a lease, and clustering is built on leases,
// so this had to close first.
//
// DESIGN 9.6 classifies budget as the one write that cannot be deferred:
// deferring the reservation means two concurrent requests both see the pre-spend
// balance. So it is made cheap rather than asynchronous. The hot path takes
// units from an in-memory block with an atomic compare-and-swap. Durability
// comes from the lease: the block is charged to budget_state (or quota_buckets)
// when it is drawn, before a single unit of it is spent. The store therefore
// sees one write per block, and the block size is the knob that trades store
// traffic against the published overshoot.
//
// Charging the block up front is what makes this safe rather than merely fast.
// A node that dies mid-block has already had the whole block counted, so the
// failure mode is a budget that under-spends until the lease is reclaimed,
// never one that overspends. [Ledger.ReclaimExpired] returns the part the dead
// node had not used, from the `used` column checkpointed on every draw, every
// renewal and every tick; a graceful [Ledger.Close] returns the exact
// remainder, so a planned restart costs nothing at all.
//
// # Shapes worth knowing
//
//   - [Registry] is the node registry and heartbeat. A node whose heartbeat
//     lapses is gone and its leases are reclaimable.
//   - [LeaseStore] is shared-pg: one row of quota_leases per (key, node), where
//     the counter's value, its owner and its recovery from a dead holder are
//     one mechanism rather than three.
//   - [RedisClient] is shared-redis, kept to a single atomic script evaluation
//     so the mode ships without a Redis dependency. [MemRedis] implements it
//     completely in memory, expiry included, and is what the tests coordinate
//     through. The Lua each script stands for is exported ([LuaReserve] and
//     friends) so a real client is an adapter rather than a reimplementation.
//   - [Node] ties them together. [Node.Tick] is one pass -- heartbeat,
//     campaign, renew leases, run due leader jobs -- and is exported so that a
//     caller, and every test here, can drive the loop instead of sleeping
//     through it.
//
// # How it is wired
//
// internal/app constructs one [Node] per process and takes its [Ledger] from
// it rather than building a second one. That single ownership is the point: two
// ledgers under one node id would be two in-memory block caches over the same
// rows, each returning on close what the other had already returned.
//
// The node is built whether or not clustering is on, because the durable budget
// is on the request path either way (DESIGN 9.6, risk W9). What `cluster.enabled`
// decides is whether it JOINS: [Node.Start] -- and with it registration, the
// heartbeat, the election and every leader job -- runs only when it is true. An
// unclustered gateway therefore writes nothing to `nodes`, holds no leadership
// lease, and runs no goroutine of this package's, which is what DESIGN 0.2's
// "no required dependencies" costs in a package about coordination.
//
// The two block sizes are separate on purpose. [Config.BlockSize] is the
// ledger's draw, in the counter's own units -- nano-USD for the budget --
// and [Config.LeaseBlock] is what a leased coordinator takes, in the metric's
// units. Publishing an overshoot in the wrong denomination is the failure the
// split exists to prevent.
//
// Two things this package can do that the gateway does not ask it to: the
// request path does not route quota through [Node.Coordinator] -- internal/
// capacity still counts per node -- and [RollupCompactionJob] and
// [BatchAssignmentJob] have no caller, so rollup compaction and batch
// assignment still run on every node rather than on the leader.
//
// # Storage
//
// Four tables, all already in internal/store's schema: nodes, capacity_leases
// (which carries the leadership lease under axis "leader", with `used` as the
// fencing token), quota_leases (shared counters and block leases, told apart by
// their scope) and the two durable counters, budget_state and quota_buckets.
// No migration is added; the dialect difference lives in one file.
package cluster
