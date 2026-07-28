// Package pricing turns a request's measured usage into money, exactly.
//
// It implements DESIGN.md §8 as revised by REVIEW.md findings 9, 10 and 11. Three
// properties are load-bearing and every change to this package must preserve them:
//
//  1. Rules carry a class (§8.1). A subscription and a per-token price are different
//     kinds of cost, not competing descriptions of one cost. Each class elects its own
//     winner and the results compose:
//
//     total = marginal(winner) + amortized(subscription winner), then adjustments in order
//
//     [Cost.MarginalNano] is the only field routing may read. [Cost.SubscriptionNano] is
//     imputed accounting: a sunk plan cost must never make a saturated plan look cheap.
//
//  2. Rules are indexed by their static dimensions when the catalog loads (§8.2), so a
//     request evaluates only the handful of rules that can possibly apply to it. Only
//     time-dependent predicates are evaluated per request. Cost-based routing prices every
//     candidate deployment, so per-candidate results are memoized within one incoming
//     request by [Evaluator].
//
//  3. Prices are exact decimals, never binary floating point (§8.3). Products are formed
//     in 128-bit intermediate precision, rounded once at the end, half-to-even, with the
//     sub-nano remainder carried into the next settlement. Every value is range-checked
//     before it is returned; overflow is [ErrOverflow], never a negative cost.
//
// Two entry points, deliberately different:
//
//   - [Catalog.Price] is pure. It mutates nothing, takes no lock unless a subscription
//     rule matches, and is safe to call once per candidate deployment while routing.
//   - [Catalog.Settle] is the accounting path. It consumes and updates the carried
//     rounding remainder and the subscription period accumulator, and must be called at
//     most once per request.
//
// An unpriced model is not silently free: [Cost.Missing] is set so the caller can
// increment its counter and warn (§8.3). Silent zero-cost accounting is the failure this
// package exists to prevent.
package pricing
