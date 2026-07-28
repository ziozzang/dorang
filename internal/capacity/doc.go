// Package capacity reserves the concurrency a request needs across every axis
// that constrains it, atomically.
//
// # Model
//
// A single request is constrained on several axes at once (DESIGN §5.1): the
// provider route, the provider group, the (provider, upstream model) pair, the
// credential group, the individual credential key, the calling principal, and a
// process-wide global ceiling. Each axis is a map from a key to a maximum number
// of concurrent reservations. A limit of zero or less means "unlimited" and is
// not counted at all.
//
// # Atomicity
//
// Check and commit happen in one critical section (DESIGN §5.3), so partial
// occupancy is never observable and a waiter never holds a slot on one axis
// while queuing for another. Deadlock is therefore structurally impossible:
// nothing is ever held across a wait.
//
// A credential candidate is the triple (provider, capacity group, key) and is
// checked and committed as a unit. Provider-scoped axes are re-derived from the
// candidate, not carried over from a previous one, so a Spill may move between
// candidates belonging to different providers. With OnCapacity == Wait only the
// preferred candidate is ever tried (or the first one, when no preference is
// given); with Spill each candidate is tried in turn.
//
// # Waiting
//
// Waiting is per-axis FIFO with targeted wakeup (DESIGN §5.4). Every axis key
// owns its own queue. A blocked waiter enqueues on the axis or axes that blocked
// it, recording the full axis set it needs. On release, an axis wakes only as
// many head-of-queue waiters as slots were freed and hands each a completed
// reservation rather than a signal to re-race, so the number of wakeups per
// release is bounded by a constant and does not grow with the number of waiters.
//
// A waiter whose other axes filled in the meantime is re-enqueued on its newly
// blocking axis carrying its original arrival sequence. That sequence is the
// priority key, so effective priority rises monotonically with total wait time
// (aging) and a multi-axis waiter can never be indefinitely overtaken by a
// stream of single-axis waiters.
//
// # Expiry
//
// Every reservation carries a deadline. A background sweeper reclaims
// reservations that outlive it, so a panic or a leaked goroutine cannot
// permanently consume capacity. The clock is injectable through Config.Now.
//
// # Interactive reserve
//
// When Request.Batch is set, the effective ceiling on every axis is
// floor(limit * (1 - InteractiveReserve)) (DESIGN §11.1). Reserving only on the
// credential axis would let batch take a model's entire limit while staying
// under its credential share, starving interactive traffic for that model.
//
// # Concurrency
//
// A Broker is safe for concurrent use. Version 1 is a single mutex over the
// whole broker (DESIGN §5.7); the critical section is short and holds no I/O.
package capacity
