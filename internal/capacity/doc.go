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
// # Soft reservations
//
// Aging stops a multi-axis waiter from being overtaken. It does not stop it
// from losing. A waiter needing axes A and B, with both axes saturated, reaches
// the head of A's queue, finds B full, moves to the head of B's queue, finds A
// full again, and repeats forever. It is never passed over; it simply never
// observes both axes free at the same instant. That is DESIGN §5.4 correction 5
// and open risk W8, and the protocol below closes it.
//
// A soft reservation ("claim") is one unit of one axis key set aside for one
// named waiter. It is not a held slot: the waiter is still blocked, still owns
// no reservation, and still holds nothing across its wait. It is a bar on
// granting — while a claim stands, every request other than the claimant sees
// that axis's ceiling reduced by one.
//
// # Soft reservations: the counting invariant
//
// Each bucket keeps at most one claimant, and the unit set aside is counted in
// the bucket's occupancy — the claim is not a second number to reconcile, it is
// occupancy that belongs to a waiter instead of to a reservation. So everyone
// except the claimant sees a claimed axis as simply fuller, at no cost, and the
// invariant to maintain is the one that was already there:
//
//	inUse <= limit
//
// preserved by every operation:
//
//   - A claim is placed only when inUse < limit, and raises inUse by one.
//   - A non-claimant is granted only when inUse < limit (or the lower batch
//     ceiling), and raises inUse by one.
//   - The claimant is granted when inUse - 1 < limit, which the invariant
//     already guarantees. Its claim becomes its reservation, so inUse does not
//     move.
//   - A release, and a claim given back, each lower inUse by one.
//
// The third line is the whole protocol: a claimant can *always* take its claimed
// axis, so once a waiter holds claims on every axis it needs, its next check
// succeeds unconditionally. Note that it holds because a claimant's ceiling is
// the raw limit — which is why batch work, whose ceiling is lower, cannot have
// one; see below.
//
// Observability reports occupancy net of the claim, so a soft reservation shows
// up as AxisState.SoftReserved and never as a phantom reservation.
//
// # Soft reservations: when a claim is placed and released
//
// Claims are placed only during a targeted-wakeup probe, that is, only for a
// waiter that is already queued and is at the head of the queue being served.
// A fresh Acquire never claims, so the uncontended path is untouched and a new
// arrival can never take a unit out from under a waiter that is already queued.
//
// They are also placed only for a waiter that has failed Config.SoftReserveAfter
// probes. A soft reservation is a remedy for a demonstrated failure to make
// progress, and most blocked waiters are served on their first or second probe
// without ever needing one; arming on the first failure would idle capacity for
// waiters that were never in trouble, at a measured cost of four times the
// throughput. The threshold shifts the liveness bound below by a constant and
// nothing else.
//
// On a probe that fails, the waiter walks the axes of its claim candidate — the
// first candidate in attempt order, which under OnCapacity == Wait is the only
// one it will ever use — in axis order, and claims each in turn until it reaches
// one it cannot claim. An axis cannot be claimed when it is full, when someone
// else already claims it, or when an older waiter is queued on it (which keeps
// claims consistent with FIFO and aging). The walk stops at the first such axis:
// claims are always a *prefix* in axis order, never a scattered subset. That
// prefix rule is what rules out deadlock, below.
//
// A claim is released when the waiter that holds it leaves the wait state, by
// any route: granted (the commit consumes the claims on the axes it commits,
// and any left over — possible only when a Spill waiter is served through a
// different candidate — are dropped), failed with ErrUnsatisfiable, failed with
// ErrClosed, or cancelled/timed out by its context. Dropping a claim frees real
// capacity, so every dropped claim re-offers its bucket to that bucket's queue
// before the lock is released; otherwise a claim released by a cancellation
// would idle a slot until some unrelated release happened to wake it.
//
// # Soft reservations: why it cannot deadlock
//
// Two waiters soft-reserving each other's axes is exactly the shape that
// deadlocks. With W1 claiming A and waiting for B while W2 claims B and waits
// for A, and both axes of limit 1, neither can ever proceed — the claims are
// bars on granting rather than held slots, but the circular wait is identical.
//
// The answer is a total order on axes, and it is enforced. The order is the Axis
// constant order — global, principal, route, pgroup, model, cgroup, key — which
// is the DESIGN §5.7 check order. It is a total order over the axes of any one
// candidate because a candidate needs at most one key per axis, and it is the
// *same* order for every waiter, because it is a property of the axis, not of
// the request. Claims are taken in that order and only as a prefix.
//
// Suppose a cycle W1 -> W2 -> ... -> Wn -> W1, where Wi -> Wj means Wi is
// blocked from claiming an axis Xi that Wj claims. Wj holds Xi and its own next
// target is Xj, and because Wj took its claims as a prefix in axis order, every
// axis it already holds precedes the one it is still trying to take: Xi < Xj.
// Chaining around the cycle gives X1 < X2 < ... < Xn < X1, which a total order
// forbids. No cycle exists.
//
// Blocking on committed occupancy rather than on a claim is not part of that
// graph and needs no argument: committed reservations are released, and the
// sweeper reclaims the ones that are not.
//
// # Soft reservations: the liveness guarantee
//
// Once a waiter is the oldest live waiter it is at the head of every queue it
// sits in, so every release on the axis blocking it probes it first. Let its
// claim candidate need m axes (m <= 7). On each such probe it either commits, or
// its claim prefix grows strictly: the axis that blocked it has just been freed
// and it is the head, so that axis is now claimable, and everything before it in
// axis order already passed the check and is therefore claimable too. The prefix
// never shrinks. So the waiter is served within at most
// Config.SoftReserveAfter + m releases of its blocking axes — eleven, at the
// default, against a hole that was previously unbounded.
//
// Reaching "oldest" is what aging already provides: sequence numbers are
// assigned at arrival and never renewed, so the set of waiters older than a
// given one is fixed and finite when it arrives, and each of them leaves in
// bounded time by the same argument.
//
// # Soft reservations: the throughput cost
//
// A claim idles one unit of one axis key for as long as it stands. That is the
// entire cost, and it bounds itself three ways:
//
//   - At most one claim per axis key, so an axis of limit L runs at L-1 while a
//     claim stands: a ceiling of 1/L on the loss, 0 for an unlimited axis.
//   - A single-axis waiter never claims anything. It fails a probe only when its
//     one axis is full, and a full axis cannot be claimed. The overwhelmingly
//     common case therefore pays nothing but one predictable branch.
//   - A claim's lifetime is the interval between the claimant's failed probe and
//     the next release on the axis it is waiting for — one release, not one
//     request lifetime, once the claimant is oldest.
//
// Measured (BenchmarkAcquireContendedSingleAxisSoft, ...TwoAxis, ...Mixed, each
// against itself with the guard off):
//
//   - single-axis contention: no difference. Nothing is ever claimed.
//   - uniform two-axis contention, where every request needs the same two axes:
//     no difference. The axes fill and drain together, so a waiter that reaches
//     the head of one finds the other free and is simply granted.
//   - mixed, a minority of two-axis requests against single-axis streams on each
//     axis: +3.1% ns/op at the default threshold. This is the workload W8
//     describes and the honest number to quote.
//
// The knob exists because that 3% is a real trade, and because the worst case —
// two axes of limit 1, where the claimed axis idles for a whole release interval
// — is worse than 3%. See SoftReservationMode and Config.SoftReserveAfter.
//
// # Soft reservations: batch work is deliberately excluded
//
// Batch waiters never claim, and this is not an omission. Under the interactive
// reserve a batch request's ceiling is floor(limit * (1 - reserve)) while an
// interactive request's is limit. A claim reduces the ceiling of *other*
// requests by one, which cannot protect a slot below the batch ceiling from
// interactive traffic entitled to sit above it: with limit 10 and reserve 0.3,
// interactive may legitimately drive inUse to 9 while a batch claimant needs
// inUse < 7. Making the claim bite would require barring interactive from the
// reserve itself, inverting the guarantee DESIGN §11.1 exists to provide.
//
// So the liveness guarantee is stated for interactive work only. Batch work
// keeps what it had: aging, so it is never overtaken, and the parking rule of
// DESIGN §5.4 correction 3, so it never head-of-line blocks interactive. A batch
// waiter blocked by interactive demand is the reserve working as designed.
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
