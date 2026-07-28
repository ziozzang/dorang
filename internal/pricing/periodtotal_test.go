package pricing

import (
	"testing"
	"time"
)

// julyBounds is the period every test in this file works over.
func julyBounds(t *testing.T) (start, end time.Time) {
	t.Helper()
	return periodBounds(PeriodMonthly, at(t, "2026-07-15T12:00:00Z"), time.UTC)
}

// settler drives one catalog through a period with a clock that tracks the traffic.
type settler struct {
	t     *testing.T
	c     *Catalog
	now   time.Time
	total int64
	max   int64
}

func newSettler(t *testing.T, yaml string) *settler {
	t.Helper()
	s := &settler{t: t}
	s.c = mustCatalogAt(t, yaml, time.Time{})
	s.c.SetClock(func() time.Time { return s.now })
	return s
}

// at settles one request stamped `when`, having first advanced the clock to `clock`.
func (s *settler) settle(clock, when time.Time) int64 {
	s.t.Helper()
	s.now = clock
	cost, err := s.c.Settle(Request{Model: "m", Credential: "c1",
		InputTokens: 1_000_000, Requests: 1, At: when})
	if err != nil {
		s.t.Fatal(err)
	}
	if cost.SubscriptionNano < 0 {
		s.t.Fatalf("a row recorded a negative plan share (%d)", cost.SubscriptionNano)
	}
	s.total += cost.SubscriptionNano
	if cost.SubscriptionNano > s.max {
		s.max = cost.SubscriptionNano
	}
	if s.total > planNano {
		s.t.Fatalf("the period passed the plan cost mid-way: %s attributed of %s",
			usd(s.total), usd(planNano))
	}
	return cost.SubscriptionNano
}

// reload rebuilds the catalog the way App.Reload does.
func (s *settler) reload(yaml string, adopt bool) {
	s.t.Helper()
	next := mustCatalogAt(s.t, yaml, time.Time{})
	next.SetClock(func() time.Time { return s.now })
	if adopt {
		next.AdoptState(s.c)
	}
	s.c = next
}

// TestAReloadIsNotABillingEvent is the residual the subscription rewrite recorded and
// under-stated.
//
// The accumulator that bounds a period's attribution lives with the catalog, and a reload
// builds a fresh one. Recorded as "bounded by one plan cost per reload"; measured at twenty
// reloads attributing 1,030 USD of a 100 USD plan, with one reload nine tenths of the way
// through a period putting 91.00 USD on a SINGLE request — which internal/app then holds
// against that request's own budget.
//
// The bound was also not one-per-deliberate-change. A SIGHUP re-applies whatever it finds
// with no content check, on purpose — it is the only way to pick up an edited external price
// catalog or a rotated key_file secret — so a config-management agent that HUPs on a timer
// reloaded, and billed, on a timer. That is why the fix is to make a reload free rather than
// to make it conditional.
//
// Both arms run, because an assertion that the correct arm is correct proves nothing about
// whether the fixture can tell the two apart.
func TestAReloadIsNotABillingEvent(t *testing.T) {
	start, end := julyBounds(t)
	span := end.Sub(start)
	const reloads = 20

	// The instants: one settlement per 1/20th of the period, a reload before each.
	instants := make([]time.Time, 0, reloads)
	for i := 1; i <= reloads; i++ {
		when := start.Add(span / reloads * time.Duration(i))
		if i == reloads {
			when = end.Add(-time.Nanosecond)
		}
		instants = append(instants, when)
	}

	run := func(adopt bool) (total, max int64) {
		s := newSettler(t, hundredDollarPlan)
		for _, when := range instants {
			s.reload(hundredDollarPlan, adopt)
			if !adopt {
				// The guard inside settle would abort the control arm at the
				// moment it over-attributes, which is the thing being measured.
				s.now = when
				cost, err := s.c.Settle(Request{Model: "m", Credential: "c1",
					InputTokens: 1_000_000, Requests: 1, At: when})
				if err != nil {
					t.Fatal(err)
				}
				total += cost.SubscriptionNano
				if cost.SubscriptionNano > max {
					max = cost.SubscriptionNano
				}
				continue
			}
			s.settle(when, when)
		}
		if adopt {
			return s.total, s.max
		}
		return total, max
	}

	orphaned, orphanedMax := run(false)
	if orphaned <= planNano {
		t.Fatalf("the control arm attributed %s of a %s plan: dropping the accumulator on "+
			"every reload no longer over-attributes, so this test can no longer tell the "+
			"two arms apart and proves nothing", usd(orphaned), usd(planNano))
	}
	t.Logf("control arm (state dropped on every reload): %s of a %s plan, largest single "+
		"row %s", usd(orphaned), usd(planNano), usd(orphanedMax))

	carried, carriedMax := run(true)
	if carried != planNano {
		t.Fatalf("%d reloads across one period attributed %s of a %s plan (%.2fx); the "+
			"accumulator must survive the catalog swap, or an open period attributes the "+
			"rest of itself once per reload",
			reloads, usd(carried), usd(planNano), float64(carried)/float64(planNano))
	}
	// No single row may carry a windfall either. Evenly spread traffic takes
	// plan_cost/N each; a row that follows a reload took the whole period-to-date.
	if want := planNano / reloads; carriedMax > 2*want {
		t.Fatalf("the largest single row attributed %s where an even share is %s: a reload "+
			"put a period's accrual on one request, and internal/app holds that against "+
			"that request's budget", usd(carriedMax), usd(want))
	}
}

// TestASettlementStampedInTheFutureDoesNotZeroTheRestOfThePeriod is the mirror of the
// backfill guard, which the rewrite covered, and this case, which it did not.
//
// The accumulator only moves forward. A row stamped ahead of the present attributes
// everything the plan will have accrued by that instant, so the real remainder of the period
// attributes nothing; and when the stamp lands in the NEXT period it moves periodStart
// forward as well, so every later row of the real period takes the backfill branch and the
// next period opens already depressed. Measured: one such row left 10.00 USD attributed to a
// 100.00 USD July.
//
// A clock that runs ahead on one node of a cluster reaches this without anything malicious,
// which is why the instant is clamped rather than trusted.
func TestASettlementStampedInTheFutureDoesNotZeroTheRestOfThePeriod(t *testing.T) {
	start, end := julyBounds(t)
	span := end.Sub(start)
	tenPercent := start.Add(span / 10)

	s := newSettler(t, hundredDollarPlan)

	// The present is a tenth of the way into July. This row is stamped a week into
	// AUGUST — a different period entirely.
	got := s.settle(tenPercent, end.Add(7*24*time.Hour))
	if want := planNano / 10; got != want {
		t.Fatalf("a row stamped in the next period attributed %s; only what the plan has "+
			"accrued by the present (%s) exists to be attributed",
			usd(got), usd(want))
	}

	// The rest of July, settled honestly, must still find 90.00 USD to attribute.
	for i := 2; i <= 10; i++ {
		when := start.Add(span / 10 * time.Duration(i))
		if i == 10 {
			when = end.Add(-time.Nanosecond)
		}
		s.settle(when, when)
	}
	if s.total != planNano {
		t.Fatalf("July attributed %s of a %s plan after one future-stamped row: the row "+
			"took the accumulator into August and every real July row after it found a "+
			"period the accumulator had already moved past",
			usd(s.total), usd(planNano))
	}

	// And August opens at zero rather than pre-attributed. It is settled directly
	// rather than through the helper, whose running total is July's and is by now
	// exactly the plan cost.
	augStart, _ := periodBounds(PeriodMonthly, end.Add(24*time.Hour), time.UTC)
	augTenth := augStart.Add(span / 10)
	s.now = augTenth
	cost, err := s.c.Settle(Request{Model: "m", Credential: "c1",
		InputTokens: 1_000_000, Requests: 1, At: augTenth})
	if err != nil {
		t.Fatal(err)
	}
	if cost.SubscriptionNano <= 0 {
		t.Fatalf("the first row of the next period attributed %s: the period opened with "+
			"the previous period's future-stamped row already counted against it",
			usd(cost.SubscriptionNano))
	}
}

// TestAPeriodSurvivesReloadsDisorderAndSkew is the property the two tests above are
// instances of, and the one that matters: whatever happens to the process during a period —
// configuration reloads, rows that arrive out of order, a node whose clock runs ahead — the
// period's rows sum to the plan cost. Exactly once.
//
// A per-request figure checked against a rate card is one instance of an accounting rule.
// The period total is the rule.
func TestAPeriodSurvivesReloadsDisorderAndSkew(t *testing.T) {
	start, end := julyBounds(t)
	span := end.Sub(start)
	const n = 200

	s := newSettler(t, hundredDollarPlan)
	for i := 1; i <= n; i++ {
		when := start.Add(span / n * time.Duration(i))
		if i == n {
			when = end.Add(-time.Nanosecond)
		}
		s.settle(when, when)

		switch i {
		case n / 4:
			// A configuration reload, mid-period.
			s.reload(hundredDollarPlan, true)
		case n / 2:
			// A row that arrives out of order: the meter is asynchronous, so a
			// row stamped before the last one settled is ordinary.
			s.settle(when, when.Add(-time.Hour))
		case 3 * n / 4:
			// A row from a node whose clock runs a week ahead, into next month.
			s.settle(when, end.Add(7*24*time.Hour))
		}
	}
	if s.total != planNano {
		t.Fatalf("a period with reloads, out-of-order rows and clock skew attributed %s of "+
			"a %s plan (%.4fx)", usd(s.total), usd(planNano), float64(s.total)/float64(planNano))
	}
}

// TestAdoptStateCarriesTheRoundingRemainder keeps §8.3's exactness promise across a reload.
// Two thousand single-token requests sum to exactly 1000 nano; dropping the carried
// remainder on every reload is drift with a schedule.
func TestAdoptStateCarriesTheRoundingRemainder(t *testing.T) {
	const halfNanoRate = `
currency: USD
rules:
  - { id: tiny, match: { model: m }, unit: per_1m_tokens, input: "0.0005" }
`
	c := mustCatalog(t, halfNanoRate)
	var total int64
	for i := 0; i < 2000; i++ {
		if i%100 == 0 && i > 0 {
			next := mustCatalog(t, halfNanoRate)
			next.AdoptState(c)
			c = next
		}
		cost, err := c.Settle(Request{Model: "m", InputTokens: 1,
			At: at(t, "2026-07-15T12:00:00Z")})
		if err != nil {
			t.Fatal(err)
		}
		total += cost.MarginalNano
	}
	if total != 1000 {
		t.Fatalf("2000 single-token settlements across 19 reloads summed to %d nano, want "+
			"1000: the sub-nano remainder is state, and a reload that drops it rounds "+
			"every request down to zero again", total)
	}
}
