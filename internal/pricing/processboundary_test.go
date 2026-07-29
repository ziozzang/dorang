package pricing

import (
	"testing"
	"time"
)

// process is one gateway process's view of a subscription period.
//
// It exists to make the ONE thing these tests are about impossible to fake: a
// process has exactly one clock, and everything it stamps and everything it
// judges comes from that clock. internal/app is wired the same way — it hands
// the catalog `a.now` and stamps `At: d.now()` from that same `a.now` — and a
// fixture that injects two independent clocks passes against a comparison that
// can never be true, which is exactly how the future-settlement clamp shipped.
type process struct {
	t     *testing.T
	c     *Catalog
	clock time.Time
}

// start builds a process whose clock reads `clock`, adopting whatever durable
// state it is given.
func start(t *testing.T, yaml string, clock time.Time, state []SubscriptionState) *process {
	t.Helper()
	p := &process{t: t, clock: clock}
	c, err := ParseCatalog([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	// One clock. Everything below reads it.
	c.SetClock(func() time.Time { return p.clock })
	p.c = c
	if state != nil {
		if _, err := c.RestoreState(state, p.clock); err != nil {
			t.Fatalf("RestoreState: %v", err)
		}
	}
	return p
}

// serve settles one request the way internal/app does: the stamp is this
// process's own reading of the present, taken before the catalog reads its own.
func (p *process) serve() int64 {
	p.t.Helper()
	cost, err := p.c.Settle(Request{Model: "m", Credential: "c1",
		InputTokens: 1_000_000, Requests: 1, At: p.clock})
	if err != nil {
		p.t.Fatal(err)
	}
	return cost.SubscriptionNano
}

// stop is a clean shutdown: the accumulator is written down at the present,
// because nothing is about to be lost.
func (p *process) stop() []SubscriptionState { return p.c.SnapshotState(p.clock) }

// TestARestartDoesNotReattributeThePeriod is the restart half of "a reload is
// not a billing event", and it is the larger half.
//
// The accumulator that bounds a period's attribution lives with the loaded
// catalog. AdoptState carries it across a configuration RELOAD — twenty reloads
// attribute nothing, and TestAReloadIsNotABillingEvent asserts it — and nothing
// carried it across a process boundary, because the state was in memory and
// nowhere else. Measured on a live deployment nine tenths of the way through a
// monthly period:
//
//	after the first process, ledger total     $  90.227737838   of a $100.00 plan
//	restart, one request                      $  90.229299890   attributed again
//	after the second process, ledger total    $ 180.458128041
//
// DESIGN §8.1 states the invariant this breaks in its own words: "the shares a
// period attributes sum to the plan cost, and never to more."
//
// Both arms run. An assertion that the correct arm is correct proves nothing
// about whether the fixture can tell the two apart, and the control arm here is
// exactly the old behaviour — a second process that finds nothing written down.
func TestARestartDoesNotReattributeThePeriod(t *testing.T) {
	start90, end := julyBounds(t)
	span := end.Sub(start90)
	// Nine tenths of the way through July, where the live measurement was taken.
	late := start90.Add(span * 9 / 10)

	run := func(carry bool) (total, second int64) {
		first := start(t, hundredDollarPlan, late, nil)
		total = first.serve()
		saved := first.stop()
		if !carry {
			saved = nil
		}

		// The restart. A new process, the same clock, the same instant: no plan
		// cost has accrued in between, so an honest second process attributes
		// nothing at all.
		next := start(t, hundredDollarPlan, late, saved)
		second = next.serve()
		return total + second, second
	}

	orphaned, orphanedRow := run(false)
	if orphaned <= planNano {
		t.Fatalf("the control arm attributed %s of a %s plan: a restart that carries "+
			"nothing no longer over-attributes, so this test can no longer tell the two "+
			"arms apart and proves nothing", usd(orphaned), usd(planNano))
	}
	t.Logf("control arm (nothing carried across the restart): %s of a %s plan, and the "+
		"first request of the second process alone attributed %s",
		usd(orphaned), usd(planNano), usd(orphanedRow))

	carried, carriedRow := run(true)
	if carried > planNano {
		t.Fatalf("a period spanning one restart attributed %s of a %s plan (%.4fx): the "+
			"accumulator did not survive the process boundary, so the open period "+
			"attributed its whole elapsed share a second time",
			usd(carried), usd(planNano), float64(carried)/float64(planNano))
	}
	// And the second process's first request must not carry a windfall. This is
	// the client-visible half: internal/app reserves the settled cost against the
	// requesting key's budget, so a re-attributed plan share lands on whoever
	// sends the first request after the restart.
	if carriedRow != 0 {
		t.Fatalf("the first request after a restart attributed %s at an instant where "+
			"nothing had accrued since the last settlement: a request's share is an "+
			"increment of the period's running total, not a fresh estimate of it",
			usd(carriedRow))
	}
}

// TestTheFutureSettlementClampComparesTwoObservations is §17.1's shape, applied
// to a control that IS reached and whose condition could never be true.
//
// pricing.Settle clamps a settlement stamped ahead of the present. In a default
// deployment that comparison is a tautology: internal/app gives the catalog
// `a.now` and stamps `At: d.now()` from the same `a.now`, read first, so the
// stamp is never the later of the two readings — and the triggers the rule was
// written for, an NTP step, a VM resume, a bad RTC, move BOTH readings together.
// The original fixture passed because it injected the clock and the stamp
// independently, which is a thing no gateway process does.
//
// A clock check needs two OBSERVATIONS, not two readings of one clock. The
// second observation exists exactly once: at the process boundary, where the
// accumulator was written down by a process whose clock this one does not
// share. That is what RestoreState compares against, and this drives it with
// each process's own clock coupled the way internal/app couples it.
//
// The figures are the ones the closure audit re-drove: a stray $50.00 attributed
// to a period that has not begun, a July left attributing $10.00 of $100.00, and
// an August opening at $49.00 already spent.
func TestTheFutureSettlementClampComparesTwoObservations(t *testing.T) {
	julyStart, julyEnd := julyBounds(t)
	span := julyEnd.Sub(julyStart)
	tenth := julyStart.Add(span / 10)

	// A node whose clock runs a week fast: it is a week into August while the
	// rest of the fleet is a tenth of the way into July. Everything it stamps
	// and everything it judges comes from its own single clock, so nothing on
	// this node can detect the skew — which is the point.
	newFast := func() *process {
		p := start(t, hundredDollarPlan, julyEnd.Add(7*24*time.Hour), nil)
		if p.serve() <= 0 {
			t.Fatalf("the fast node attributed nothing; the fixture is not on a " +
				"subscription and proves nothing")
		}
		return p
	}

	// The July traffic every arm serves, on a clock that is where the calendar
	// says it is.
	julyTotal := func(p *process) int64 {
		var total int64
		for i := 1; i <= 10; i++ {
			p.clock = julyStart.Add(span / 10 * time.Duration(i))
			if i == 10 {
				p.clock = julyEnd.Add(-time.Nanosecond)
			}
			total += p.serve()
		}
		return total
	}

	// The control arm: the state crosses into the healthy process with no clock
	// check at all, which is what happens today at every boundary that has one
	// (AdoptState is the reload path, where both catalogs share a clock and no
	// check is needed). It is here to prove the fixture can tell the two apart.
	{
		fast := newFast()
		blind := start(t, hundredDollarPlan, tenth, nil)
		blind.c.AdoptState(fast.c)
		if july := julyTotal(blind); july > planNano/5 {
			t.Fatalf("the control arm attributed %s to July after adopting a period "+
				"stamped in August: an unchecked adoption no longer silences the open "+
				"period, so this test can no longer tell the two arms apart",
				usd(july))
		} else {
			t.Logf("control arm (adopted with no clock check): July attributed %s of a %s "+
				"plan", usd(july), usd(planNano))
		}
	}

	fast := newFast()
	stray := fast.c.SnapshotState(fast.clock)
	strayNano := int64(0)
	{
		// What the fast node handed out, read back out of the durable form.
		v, err := parseU128(stray[0].Attributed)
		if err != nil {
			t.Fatal(err)
		}
		n, _, err := roundToNano(attoAmt(v), 0)
		if err != nil {
			t.Fatal(err)
		}
		strayNano = n
	}
	t.Logf("the fast node attributed %s to a period that has not begun", usd(strayNano))

	// The healthy node starts and adopts what the fast one wrote down. Its own
	// clock is a tenth of the way into July, and the stored period start is an
	// observation its clock did not produce — which is the whole difference
	// between this comparison and the one inside Settle.
	healthy := start(t, hundredDollarPlan, tenth, stray)
	july := julyTotal(healthy)

	// July is not silenced, and the plan is not attributed twice. The stray share
	// is not un-handed-out — it reached a ledger row on the fast node — so what
	// the accumulator can still guarantee is the §8.1 invariant itself: the
	// shares handed out for ONE plan period, across every process that served
	// it, sum to the plan cost and never to more.
	if total := strayNano + july; total > planNano {
		t.Fatalf("one July attributed %s of a %s plan across two processes (%.4fx)",
			usd(total), usd(planNano), float64(total)/float64(planNano))
	}
	if july <= planNano/2 {
		t.Fatalf("July attributed %s of a %s plan after adopting a future-stamped "+
			"accumulator: the stored period start was taken on trust, every real July row "+
			"found a period the accumulator had already moved past, and the clamp that "+
			"exists for this compared a clock against itself",
			usd(july), usd(planNano))
	}
	t.Logf("clamped at the boundary: the stray %s plus July's %s is %s of a %s plan",
		usd(strayNano), usd(july), usd(strayNano+july), usd(planNano))

	// And August opens at zero rather than pre-attributed.
	augStart, _ := periodBounds(PeriodMonthly, julyEnd.Add(24*time.Hour), time.UTC)
	healthy.clock = augStart.Add(span / 10)
	if got := healthy.serve(); got <= 0 {
		t.Fatalf("the first row of August attributed %s: the period opened with a "+
			"future-stamped row from the previous process already counted against it",
			usd(got))
	}
}

// TestASnapshotIsAheadOfRealityAndNeverBehind is DESIGN §9.6's rule applied to
// the second durable counter.
//
// A checkpoint on a timer cannot be exact: whatever it writes is stale the
// moment it is written. The direction it is stale in is the whole design. §8.1
// permits a period to attribute LESS than the plan cost and forbids more, so
// the durable figure is projected forward by one checkpoint interval — a
// process that dies leaves a little of the period unattributed rather than
// resuming behind and attributing a slice of it twice.
func TestASnapshotIsAheadOfRealityAndNeverBehind(t *testing.T) {
	julyStart, julyEnd := julyBounds(t)
	span := julyEnd.Sub(julyStart)
	half := julyStart.Add(span / 2)

	p := start(t, hundredDollarPlan, half, nil)
	settled := p.serve()

	// The periodic checkpoint: projected one interval past the present.
	const interval = 30 * time.Second
	ahead := p.c.SnapshotState(half.Add(interval))
	if len(ahead) != 1 {
		t.Fatalf("SnapshotState returned %d accumulators, want 1", len(ahead))
	}

	// A process that crashes and restarts against the projected figure resumes
	// having attributed slightly more than it really did, so its next request at
	// the same instant attributes nothing.
	crashed := start(t, hundredDollarPlan, half, ahead)
	if got := crashed.serve(); got != 0 {
		t.Fatalf("a process restarted from a projected checkpoint attributed a further "+
			"%s at the same instant: the durable figure is behind reality, and a crash "+
			"therefore re-attributes the gap", usd(got))
	}
	// It catches up within the interval and no further: one interval later the
	// period is exactly where an uninterrupted process would have been.
	crashed.clock = half.Add(interval)
	if got := crashed.serve(); got != 0 {
		t.Errorf("the projected checkpoint covered less than the interval it was projected "+
			"by: %s attributed at the projection horizon", usd(got))
	}
	crashed.clock = half.Add(2 * interval)
	if got := crashed.serve(); got <= 0 {
		t.Errorf("the period stopped attributing past the projection horizon: %s", usd(got))
	}
	if settled <= 0 {
		t.Fatalf("the first settlement attributed %s; the fixture is not on a plan", usd(settled))
	}
}

// TestSubscriptionStateRoundTripsExactly: the durable form of the accumulator is
// atto-scaled — a 100 USD plan is 10^20 atto, five times more than a uint64
// holds — so it travels as decimal digits. A round trip that lost the low digits
// would drift the period total by a sub-nano amount per restart, which is
// exactly the drift §8.3's carried remainder exists to prevent.
func TestSubscriptionStateRoundTripsExactly(t *testing.T) {
	for _, v := range []u128{
		{}, {lo: 1}, {lo: 999_999_999_999_999_999},
		{lo: digitChunk}, {lo: digitChunk + 1},
		{hi: 5, lo: 7_766_279_631_452_241_920}, // 10^20, a 100 USD plan in atto
		{hi: ^uint64(0), lo: ^uint64(0)},       // 2^128-1
	} {
		s := v.text()
		got, err := parseU128(s)
		if err != nil {
			t.Fatalf("parseU128(%q): %v", s, err)
		}
		if got.cmp(v) != 0 {
			t.Errorf("%v -> %q -> %v: the durable form is not exact", v, s, got)
		}
	}
	if got, err := parseU128(""); err != nil || !got.isZero() {
		t.Errorf("an empty stored value must read as zero, got %v %v", got, err)
	}
}
