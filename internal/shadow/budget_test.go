package shadow

import (
	"sync"
	"testing"
	"time"
)

func TestBudgetStopsRatherThanSkipping(t *testing.T) {
	// The distinction §14.1 turns on. A ceiling that refuses the request that
	// does not fit and keeps taking smaller ones compares only cheap traffic
	// while its own counter still reads under the limit.
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	b := newDayBudget(100, now)

	if !b.reserve(now, 60) {
		t.Fatal("first reservation should fit")
	}
	b.settle(now, 60, 60)
	if !b.reserve(now, 30) {
		t.Fatal("second reservation should fit")
	}
	b.settle(now, 30, 30)

	if b.reserve(now, 30) {
		t.Fatal("a reservation past the ceiling must not be granted")
	}
	if !b.capped(now) {
		t.Fatal("a reservation past the ceiling must stop shadowing, not merely refuse one request")
	}
	// The cheap request that would still fit is refused too. That is the point.
	if b.reserve(now, 1) {
		t.Fatal("shadowing is stopped for the day; a cheaper request must not slip through")
	}
}

func TestBudgetRearmsAtTheUTCDayBoundary(t *testing.T) {
	day1 := time.Date(2026, 7, 28, 23, 59, 0, 0, time.UTC)
	b := newDayBudget(100, day1)
	if b.reserve(day1, 200) {
		t.Fatal("an over-limit reservation must fail")
	}
	if !b.capped(day1) {
		t.Fatal("want capped")
	}

	day2 := day1.Add(2 * time.Minute)
	if b.capped(day2) {
		t.Fatal("the ceiling is per day and must rearm at the boundary")
	}
	if !b.reserve(day2, 50) {
		t.Fatal("the new day should have its full allowance")
	}
	if got := b.committed(); got != 50 {
		t.Errorf("committed = %d, want 50 (yesterday's spend must not carry)", got)
	}
}

func TestConcurrentReservationsCannotExceedTheCeiling(t *testing.T) {
	// Charging on completion instead of reserving up front lets every
	// concurrent call see the same pre-spend balance and pass. This is the test
	// that would catch that.
	now := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	const limit = 1000
	const each = 10
	b := newDayBudget(limit, now)

	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 400; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.reserve(now, each) {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if granted*each > limit {
		t.Fatalf("granted %d reservations of %d against a ceiling of %d", granted, each, limit)
	}
	if b.committed() > limit {
		t.Fatalf("committed %d exceeds the ceiling %d", b.committed(), limit)
	}
	if !b.capped(now) {
		t.Error("400 × 10 against 1000 must have tripped the stop")
	}
}

func TestSettlementCorrectsAnEstimate(t *testing.T) {
	now := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	b := newDayBudget(1000, now)

	if !b.reserve(now, 500) {
		t.Fatal("reservation should fit")
	}
	// The reference reported a real number, well under the estimate.
	b.settle(now, 500, 20)
	if got := b.committed(); got != 20 {
		t.Fatalf("committed = %d, want 20", got)
	}
	// The released allowance is usable again, which is what makes a
	// conservative estimate safe rather than wasteful.
	if !b.reserve(now, 900) {
		t.Fatal("the corrected allowance should admit a larger reservation")
	}
}

func TestRefundedReservationDoesNotSpendTheCeiling(t *testing.T) {
	// A queue-full drop refunds. Without it, a burst that overran the queue
	// would spend the day's allowance on calls that were never made.
	now := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	b := newDayBudget(100, now)
	for i := 0; i < 50; i++ {
		if !b.reserve(now, 10) {
			t.Fatalf("reservation %d refused after refunds", i)
		}
		b.settle(now, 10, 0)
	}
	if b.capped(now) {
		t.Fatal("fifty reserved-and-refunded calls must not trip a ceiling nothing was spent against")
	}
}
