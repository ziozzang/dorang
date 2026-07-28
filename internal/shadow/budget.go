package shadow

import (
	"sync/atomic"
	"time"
)

// dayBudget is the hard daily cost ceiling of DESIGN §14.1.
//
// Four decisions, three of which a simpler implementation gets wrong:
//
//  1. **Reserve before, settle after.** The estimate is taken out of the
//     remaining allowance before the reference call is issued and corrected
//     when it returns. Charging on completion instead lets every concurrent
//     call see the same pre-spend balance and pass — the identical failure
//     DESIGN §9.6 refuses to accept for budget, and for the identical reason.
//     A reservation is an upper bound, so settling late only ever gives money
//     back.
//
//  2. **A miss stops shadowing; it does not skip the request.** Refusing the
//     request that does not fit and continuing to take the ones that do would
//     bias the whole comparison toward cheap traffic while the counter still
//     read as under budget. §14.1 calls the ceiling a hard stop, and a hard
//     stop is what this is: the first reservation that does not fit disables
//     shadowing until the next UTC day.
//
//  3. **It rearms at the UTC day boundary**, because the ceiling is per day and
//     an operator who set five dollars a day did not mean five dollars ever.
//
//  4. **The stop is a state, not a log line.** [dayBudget.stopped] is read by
//     the metrics endpoint and by the health body, so the answer to "is the
//     cutover gate still running" is available without reading a log.
type dayBudget struct {
	limit int64

	// day is the UTC day number the counters belong to.
	day atomic.Int64
	// spent is settled cost; reserved is cost that has been committed to but
	// whose call has not returned. Their sum is what the ceiling is checked
	// against, which is what makes concurrent calls safe.
	spent    atomic.Int64
	reserved atomic.Int64

	stopped   atomic.Bool
	stoppedAt atomic.Int64 // unix seconds, 0 when running
	stops     atomic.Int64
}

func newDayBudget(limitNanoUSD int64, now time.Time) *dayBudget {
	b := &dayBudget{limit: limitNanoUSD}
	b.day.Store(dayOf(now))
	return b
}

// dayOf is the UTC day number.
func dayOf(t time.Time) int64 { return t.UTC().Unix() / 86400 }

// roll resets the counters when the UTC day has changed, and reports the
// current day. It is idempotent under concurrency: two goroutines may both
// observe the change, and only one wins the compare-and-swap.
func (b *dayBudget) roll(now time.Time) int64 {
	d := dayOf(now)
	cur := b.day.Load()
	if cur == d {
		return d
	}
	if b.day.CompareAndSwap(cur, d) {
		b.spent.Store(0)
		// Reservations outstanding across the boundary are released rather than
		// carried: they belong to yesterday's ceiling, and holding them would
		// charge today for yesterday's calls.
		b.reserved.Store(0)
		b.stopped.Store(false)
		b.stoppedAt.Store(0)
	}
	return d
}

// reserve commits est against today's allowance.
//
// It returns false when the reservation does not fit, and in that case
// shadowing is stopped for the rest of the day — not merely this one request
// refused. See the type comment.
func (b *dayBudget) reserve(now time.Time, est int64) bool {
	b.roll(now)
	if b.stopped.Load() {
		return false
	}
	if est < 0 {
		est = 0
	}
	for {
		spent := b.spent.Load()
		res := b.reserved.Load()
		if spent+res+est > b.limit {
			b.stop(now)
			return false
		}
		if b.reserved.CompareAndSwap(res, res+est) {
			return true
		}
	}
}

// settle replaces a reservation with the amount actually spent.
//
// actual below zero means "no better number arrived", in which case the
// estimate stands: a reference that reported nothing is assumed to have cost
// what dorang's own copy cost, which is the assumption the reservation was
// already made on.
//
// An actual above the estimate can push the day past its ceiling — the estimate
// was dorang's own price for the same request, and the reference may simply be
// dearer. The ceiling has to hold anyway, so settling over it stops shadowing
// here as surely as reserving over it does.
func (b *dayBudget) settle(now time.Time, est, actual int64) {
	if actual < 0 {
		actual = est
	}
	b.reserved.Add(-est)
	if b.spent.Add(actual) > b.limit {
		b.stop(now)
	}
}

// stop disables shadowing for the rest of the day.
func (b *dayBudget) stop(now time.Time) {
	if b.stopped.CompareAndSwap(false, true) {
		b.stoppedAt.Store(now.UTC().Unix())
		b.stops.Add(1)
	}
}

// capped reports whether the ceiling has stopped shadowing, rolling the day
// first so that a process which sat idle overnight rearms on the next question
// rather than on the next request.
func (b *dayBudget) capped(now time.Time) bool {
	b.roll(now)
	return b.stopped.Load()
}

// committed is spent plus outstanding reservations: what today has cost, on the
// pessimistic reading that every in-flight call will complete.
func (b *dayBudget) committed() int64 { return b.spent.Load() + b.reserved.Load() }
