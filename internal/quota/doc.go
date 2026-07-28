// Package quota meters how much a credential may consume within a window, and
// reserves budget before it is spent.
//
// Quota is orthogonal to concurrency (DESIGN §6): internal/capacity answers
// "how many right now", this package answers "how much within a window" and
// "is there money for this".
//
// # Local metering
//
// A [Meter] carries a set of [Rule]s — a window, a metric and a limit — over
// minute-bucket rings (DESIGN §6.1). Maintenance and query are O(1): each ring
// keeps a running total and subtracts only the buckets that fall out as time
// advances, so a query never sums the ring. Calendar windows need no ring at
// all, only a period start and an accumulator.
//
// On exhaustion a rule either puts the credential in cooldown until the window
// resets — which is what moves traffic to the next credential — disables it, or
// lets traffic pass and only records the fact.
//
// # Provider-reported quota is combined, not substituted
//
// Where a provider exposes its own usage figure, that figure covers traffic
// that never passed through dorang, so local metering alone under-counts. But a
// poll is up to one interval stale, and during a burst the local counter is the
// fresher signal. Revision 1 of the design declared the provider authoritative
// and discarded the local delta, which meant routing most aggressively exactly
// when its information was worst. [Tracker] implements the correction
// (DESIGN §6.2):
//
//	effective_used = max( provider_reported_used,
//	                      reported_at_last_poll + local_delta_since_that_poll )
//
// The provider figure re-baselines on every poll; local metering supplies the
// delta in between; neither source can hide a burst.
//
// A failed fetch never disables a credential — a failed read is not an
// exhausted quota. The last good snapshot is retained and its staleness is
// exposed ([Tracker.Staleness]).
//
// [Prober] is the provider-side interface, and [Registry] polls the registered
// probers concurrently, off the request path, with a per-provider timeout.
// [StaticProber] is a shipped test double; real provider clients live with
// their providers, not here.
//
// # Accuracy across nodes
//
// Quota uses the same accuracy vocabulary as capacity (DESIGN §5.6, §6.3):
// local, shared-redis, shared-pg, leased. Every mode publishes its maximum
// possible overshoot as a number through [Coordinator.MaxOvershoot] —
// "approximately accurate" is not a specification. Running clustered with
// local quota is refused, not discouraged. leased refuses limits below
// MinLeasable because single-digit limits cannot be usefully divided across
// nodes.
//
// This package implements local and leased fully, defines [SharedStore] as the
// interface the shared modes implement, and ships [MemShared], an in-memory
// SharedStore that is exact and is what the tests coordinate through.
//
// # Budget
//
// [Budget] reserves before spending so concurrent requests cannot overshoot
// (DESIGN §6.4). Three properties are load-bearing, each of them a correction
// from review:
//
//   - The estimate is an upper bound: exact input tokens plus output priced at
//     max_tokens ([Estimate]).
//   - Reservations expire. Every reservation carries reserved_until and
//     [Budget.SweepExpired] reclaims the ones that outlive it. Without that, a
//     process killed between reserve and settle locks the amount forever and
//     the budget is eventually exhausted by money nobody spent.
//   - A reservation is a soft hold at the gate and becomes a hard hold only
//     once capacity is acquired ([Budget.Harden]). Anything that fails before
//     dispatch is refunded in full ([Budget.Release]), because a request can
//     reserve budget and then be rejected while waiting for capacity, never
//     reaching an upstream.
//
// Exceeding a budget is not a fallback condition: [ErrBudgetExceeded] is
// terminal, and failing is the correct outcome.
//
// # Units
//
// Cost is carried in nano-USD as an int64, so that window arithmetic is exact
// addition rather than accumulated floating-point error. [NanoUSD] and [USD]
// convert at the edges. Token and request metrics are plain counts.
//
// # Clock
//
// Every window boundary is computed in UTC, so no daylight-saving transition
// can shorten, lengthen or duplicate a period. Clocks are injectable for tests.
package quota
