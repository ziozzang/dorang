// Package quota meters how much a credential may consume within a window, and
// names the subjects and refusals a budget is expressed in.
//
// Quota is orthogonal to concurrency (DESIGN §6): internal/capacity answers
// "how many right now", this package answers "how much within a window", and
// "is there money for this" is answered by internal/cluster's durable ledger in
// the vocabulary defined here.
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
// # Expiring quota is a ranking input
//
// A window that resets is use-it-or-lose-it, and routing that ignores that
// wastes the cheapest capacity there is — invisibly, because nothing fails and
// only the bill moves. [Meter.Urgency] scores it (DESIGN §7.5a(c)):
//
//	urgency = unused_fraction ÷ remaining_fraction_of_window
//
// Three properties keep the optimization from doing harm:
//
//   - Only quota that actually expires has any. Rule.Resets is declared, never
//     inferred: a rolling balance and a resetting subscription window look the
//     same from their numbers, and spending the first early buys nothing.
//   - It is damped by the subject's current occupancy, so that a credential
//     which is filling up stops attracting the fleet before its concurrency
//     limit becomes everyone's bottleneck. Occupancy is an input; this package
//     does not import internal/capacity.
//   - It is jittered deterministically, keyed by (node, subject). The node
//     alone would not do: a factor that scaled every candidate a node ranks by
//     the same amount cannot change that node's ordering, so it would be
//     arithmetic with no effect on the stampede it is meant to break.
//
// Every degenerate case — a quota that does not reset, a fully consumed
// allowance, a zero-length window, a window whose reset instant the provider
// never reported — scores zero. An unknown must not be able to attract traffic.
//
// [Ranker] is the Urgency(subject) form routing consumes.
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
// # Budget: the vocabulary, not the mechanism
//
// This package defines what a budget is ABOUT — [Subject], the ceiling's owner;
// [BudgetError], the typed refusal — and does not implement reserving against
// one. internal/cluster's durable ledger does, and it is the only implementation
// in the build.
//
// It used to be both. `Budget` was an exact in-memory reserve/settle path with
// no caller outside this package's tests, kept alive by the scenario suite after
// the request path had moved to the ledger; DESIGN §17's W9 row already said "no
// in-memory path is kept beside it", and this was the copy that had outlived the
// sentence. Every property it carried is now asserted against the path a request
// takes, which is where those properties actually have to hold:
//
//   - The estimate is a pessimistic upper bound — exact input tokens plus output
//     priced at max_tokens (§6.4). It is computed after routing, because a price
//     needs a provider and an upstream model.
//   - Anything that fails before dispatch is refunded in full: the ledger's
//     Release returns the hold, and the block was charged durably when it was
//     drawn, so the correction only ever releases units.
//   - Money nobody spent must not lock the budget (R1-5). The ledger answers it
//     one level up: the whole block is charged before a unit is spent, so a node
//     that dies takes its block out of circulation, and the leader returns it
//     when the LEASE expires — a bounded delay rather than a permanent loss.
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
