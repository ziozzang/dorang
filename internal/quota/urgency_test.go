package quota

import (
	"math"
	"testing"
	"time"
)

// exact is the shaping that leaves the raw ratio alone, so a test can assert
// the formula rather than the formula times two guards.
var exact = UrgencyInput{Jitter: -1, Damping: -1}

func closeTo(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// TestUrgencyFormulaAcrossTheWindow pins the design's own worked example and
// walks the same allowance across the whole window: the ratio rises as the
// remainder of the window shrinks, which is the entire point of the signal.
func TestUrgencyFormulaAcrossTheWindow(t *testing.T) {
	start := base
	reset := base.Add(7 * 24 * time.Hour)
	a := Allowance{Used: 20, Limit: 100, Start: start, ResetAt: reset, Resets: true}

	cases := []struct {
		elapsed float64 // fraction of the window
		want    float64 // unused ÷ remaining
	}{
		{0.0, 0.8},        // at the start: 0.8 ÷ 1.0
		{0.5, 1.6},        // halfway: 0.8 ÷ 0.5
		{0.9, 8.0},        // DESIGN §7.5a(c)'s example: 0.8 ÷ 0.1
		{0.99, 80.0},      // the last percent of the window
		{0.75, 3.2},       // 0.8 ÷ 0.25
		{0.999, 800.0},    // still finite
		{0.9999, 800.0},   // floored: minRemainingFraction caps the ratio
		{1.0, 800.0},      // exactly at the reset
		{1.5, 800.0},      // past it, before the counter has rolled over
		{-0.25, 0.8},      // clock skew: the window has not started
		{0.999999, 800.0}, // no divergence to +Inf
	}
	total := reset.Sub(start)
	for _, c := range cases {
		now := start.Add(time.Duration(c.elapsed * float64(total)))
		closeTo(t, a.Urgency(now), c.want, "urgency at "+now.Format(time.RFC3339))
	}
	// The ceiling is a documented constant, not an accident.
	if MaxUrgency != 1000 {
		t.Fatalf("MaxUrgency = %v", MaxUrgency)
	}
}

// TestUrgencyIsZeroForQuotaThatDoesNotReset is constraint 1. A rolling balance
// or a pay-as-you-go allowance is worth exactly as much tomorrow: spending it
// early buys nothing and forfeits the option of spending it elsewhere.
func TestUrgencyIsZeroForQuotaThatDoesNotReset(t *testing.T) {
	start := base
	reset := base.Add(7 * 24 * time.Hour)
	now := start.Add(time.Duration(0.9 * float64(reset.Sub(start))))

	expiring := Allowance{Used: 20, Limit: 100, Start: start, ResetAt: reset, Resets: true}
	rollover := expiring
	rollover.Resets = false

	if got := expiring.Urgency(now); got == 0 {
		t.Fatal("the expiring allowance scored zero")
	}
	if got := rollover.Urgency(now); got != 0 {
		t.Fatalf("a quota that does not reset scored %v, want 0", got)
	}

	// And through the meter, where the flag is the configured property.
	for _, resets := range []bool{true, false} {
		r := Rule{Window: Weekly, Metric: MetricRequests, Limit: 100, Resets: resets}
		m := newTestMeter(t, r)
		m.Record(base, Usage{Requests: 20})
		at := Weekly.PeriodEnd(base).Add(-time.Hour)
		got := m.Urgency(at, exact)
		if resets && got <= 0 {
			t.Fatalf("resetting rule scored %v", got)
		}
		if !resets && got != 0 {
			t.Fatalf("non-resetting rule scored %v, want 0", got)
		}
	}
}

// TestUrgencyDegenerateCases: every one of them resolves to zero rather than to
// a large number. The direction matters — this signal moves traffic, so an
// unknown must never be able to attract it.
func TestUrgencyDegenerateCases(t *testing.T) {
	start := base
	reset := base.Add(5 * time.Hour)
	full := Allowance{Used: 0, Limit: 100, Start: start, ResetAt: reset, Resets: true}

	t.Run("zero elapsed time", func(t *testing.T) {
		// Not an error case: the remaining fraction is exactly 1, so urgency is
		// the unused fraction. It is listed because 0/0 is one arithmetic slip
		// away.
		closeTo(t, full.Urgency(start), 1.0, "urgency at the window start")
		half := full
		half.Used = 50
		closeTo(t, half.Urgency(start), 0.5, "half-consumed at the window start")
	})

	t.Run("fully consumed", func(t *testing.T) {
		spent := full
		spent.Used = 100
		for _, at := range []time.Time{start, start.Add(4 * time.Hour), reset} {
			if got := spent.Urgency(at); got != 0 {
				t.Fatalf("a fully consumed allowance scored %v at %v", got, at)
			}
		}
		// Over-consumed, which the provider-combined figure can produce.
		over := full
		over.Used = 140
		if got := over.Urgency(start.Add(4 * time.Hour)); got != 0 {
			t.Fatalf("an over-consumed allowance scored %v", got)
		}
	})

	t.Run("reset instant unknown", func(t *testing.T) {
		unknown := full
		unknown.ResetAt = time.Time{}
		if got := unknown.Urgency(start.Add(4 * time.Hour)); got != 0 {
			t.Fatalf("an unknown reset scored %v, want 0", got)
		}
	})

	t.Run("zero length window", func(t *testing.T) {
		degenerate := full
		degenerate.Start, degenerate.ResetAt = reset, reset
		if got := degenerate.Urgency(reset); got != 0 {
			t.Fatalf("a zero-length window scored %v", got)
		}
		inverted := full
		inverted.Start = reset.Add(time.Hour)
		if got := inverted.Urgency(start); got != 0 {
			t.Fatalf("an inverted window scored %v", got)
		}
	})

	t.Run("no limit", func(t *testing.T) {
		unlimited := full
		unlimited.Limit = 0
		if got := unlimited.Urgency(start); got != 0 {
			t.Fatalf("a zero limit scored %v", got)
		}
	})

	t.Run("shaping a zero stays zero", func(t *testing.T) {
		if got := Shape(0, UrgencyInput{Subject: "c", NodeID: "n"}); got != 0 {
			t.Fatalf("Shape(0) = %v", got)
		}
	})
}

// TestRollingWindowHasNoKnownResetWithoutTheProvider is the case §7.5a(c) does
// not address. A rolling window is a ring that always ends now; declaring
// resets: true on one asserts the provider tumbles a window of that length
// without saying when. The phase is unknown until the provider reports it, and
// an unknown phase scores zero rather than guessing.
func TestRollingWindowHasNoKnownResetWithoutTheProvider(t *testing.T) {
	r := Rule{Window: Rolling(5 * time.Hour), Metric: MetricRequests, Limit: 100, Resets: true}
	m := newTestMeter(t, r)
	m.Record(base, Usage{Requests: 20})

	at := base.Add(time.Hour)
	if got := m.Urgency(at, exact); got != 0 {
		t.Fatalf("a rolling window with no provider report scored %v, want 0", got)
	}
	a, ok := m.Allowance(at, r)
	if !ok {
		t.Fatal("Allowance did not find the rule")
	}
	if !a.ResetAt.IsZero() {
		t.Fatalf("ResetAt = %v, want zero", a.ResetAt)
	}

	// Once the provider reports its reset instant the window has a phase, and
	// the signal comes alive.
	tr := NewTracker(m)
	reset := at.Add(30 * time.Minute) // 10% of a five-hour window left
	tr.Adopt(ProbeResult{FetchedAt: at, Windows: []ProviderWindow{
		{Window: r.Window, Metric: r.Metric, Used: 20, Limit: 100, ResetAt: reset},
	}})
	m.AttachTracker(tr)

	got := m.Urgency(at, exact)
	closeTo(t, got, 8.0, "urgency with a provider-reported reset")

	a, _ = m.Allowance(at, r)
	if !a.ResetAt.Equal(reset) {
		t.Fatalf("ResetAt = %v, want the provider's %v", a.ResetAt, reset)
	}
	if want := reset.Add(-5 * time.Hour); !a.Start.Equal(want) {
		t.Fatalf("Start = %v, want %v", a.Start, want)
	}
}

// TestProviderResetOverridesTheCalendarBoundary: a calendar window has a
// boundary dorang defines, but the provider's own window is what actually
// discards the remainder — the same precedence Check already applies when it
// reports when a cooled-down credential recovers.
func TestProviderResetOverridesTheCalendarBoundary(t *testing.T) {
	r := Rule{Window: Weekly, Metric: MetricRequests, Limit: 100, Resets: true}
	m := newTestMeter(t, r)
	m.Record(base, Usage{Requests: 20})

	local := m.Urgency(base, exact)

	tr := NewTracker(m)
	reset := base.Add(30 * time.Minute)
	tr.Adopt(ProbeResult{FetchedAt: base, Windows: []ProviderWindow{
		{Window: Weekly, Metric: MetricRequests, Used: 20, Limit: 100, ResetAt: reset},
	}})
	m.AttachTracker(tr)

	withProvider := m.Urgency(base, exact)
	if !(withProvider > local) {
		t.Fatalf("provider reset %v did not raise urgency above the calendar one %v",
			withProvider, local)
	}
	// 30 minutes left of a seven-day window, on an allowance 20% consumed.
	want := 0.8 / (float64(30*time.Minute) / float64(7*24*time.Hour))
	closeTo(t, withProvider, want, "urgency against the provider's reset")
}

// TestMeterUrgencyTakesTheLargestExpiringRule. A meter carries several rules
// and they do not expire together; the answer is how much paid-for capacity is
// about to be discarded, which is the maximum.
func TestMeterUrgencyTakesTheLargestExpiringRule(t *testing.T) {
	near := Rule{Window: Daily, Metric: MetricRequests, Limit: 100, Resets: true}
	far := Rule{Window: Monthly, Metric: MetricTokensTotal, Limit: 1000, Resets: true}
	rolling := Rule{Window: Weekly, Metric: MetricCostUSD, Limit: NanoUSD(100)} // does not reset
	m := newTestMeter(t, near, far, rolling)
	m.Record(base, Usage{Requests: 20, TokensInput: 100, CostNanoUSD: NanoUSD(1)})

	at := Daily.PeriodEnd(base).Add(-time.Hour) // an hour left of the day
	got := m.Urgency(at, exact)

	a, _ := m.Allowance(at, near)
	closeTo(t, got, a.Urgency(at), "meter urgency")

	b, _ := m.Allowance(at, far)
	if !(a.Urgency(at) > b.Urgency(at)) {
		t.Fatalf("the daily rule (%v) should out-rank the monthly one (%v)",
			a.Urgency(at), b.Urgency(at))
	}
	// The non-resetting rule contributes nothing even though it is barely used.
	c, _ := m.Allowance(at, rolling)
	if got := c.Urgency(at); got != 0 {
		t.Fatalf("the non-resetting rule scored %v", got)
	}
}

// TestUrgencyIsZeroWhileStandingAside: an allowance that cannot be spent before
// it expires has nothing at stake. This is a floor on the ranking input, not
// admission control — Check remains the only thing that admits.
func TestUrgencyIsZeroWhileStandingAside(t *testing.T) {
	// Two rules, so that the one holding the credential out of service is not
	// the one carrying the unspent allowance: the allowance stays 80% unused
	// throughout, and only the meter's state can hold its urgency down.
	stake := Rule{Window: Daily, Metric: MetricTokensTotal, Limit: 1000, Resets: true}
	at := Daily.PeriodEnd(base).Add(-time.Hour)

	for _, tc := range []struct {
		name   string
		trip   OnExhaust
		state  State
		revive func(m *Meter) time.Time
	}{
		{"disable", Disable, StateDisabled, func(m *Meter) time.Time { m.Enable(); return at }},
		{"cooldown", Cooldown, StateCooldown, func(m *Meter) time.Time { return Daily.PeriodEnd(base) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := Rule{Window: Daily, Metric: MetricRequests, Limit: 10, OnExhaust: tc.trip}
			m := newTestMeter(t, gate, stake)
			m.Record(base, Usage{TokensInput: 200})
			if m.Urgency(at, exact) <= 0 {
				t.Fatal("a healthy meter with an expiring allowance scored zero")
			}

			m.Record(base, Usage{Requests: 10})
			if d := m.Check(at); d.Allow || d.State != tc.state {
				t.Fatalf("the meter did not step aside: %+v", d)
			}
			if got := m.Urgency(at, exact); got != 0 {
				t.Fatalf("a meter standing aside scored %v, want 0", got)
			}
			// The allowance itself still has everything to lose; it is the
			// credential that cannot spend it.
			a, _ := m.Allowance(at, stake)
			if a.Urgency(at) <= 0 {
				t.Fatal("the allowance itself lost its urgency")
			}

			back := tc.revive(m)
			if got := m.Urgency(back, exact); got <= 0 {
				t.Fatalf("urgency did not come back with the credential: %v", got)
			}
		})
	}
}

// urgentMeter builds a meter whose weekly allowance is `unused` fraction
// unspent, so that a fleet simulation has candidates that differ.
func urgentMeter(t *testing.T, unused float64) *Meter {
	t.Helper()
	const limit = 1000
	m := newTestMeter(t, Rule{Window: Weekly, Metric: MetricRequests, Limit: limit, Resets: true})
	m.Record(base, Usage{Requests: int64(float64(limit) * (1 - unused))})
	return m
}

// TestDampingKeepsTheFleetOffOneCredential is constraint 3, the stampede.
//
// Every node computes the same urgency from the same figures at the same
// moment. Undamped, they all converge on the same credential and its
// concurrency limit becomes the whole fleet's bottleneck — the simulation shows
// exactly that. Damped by occupancy, the winner's score falls as it fills and
// the fleet redistributes before it saturates.
func TestDampingKeepsTheFleetOffOneCredential(t *testing.T) {
	const (
		nodes    = 18
		capacity = 8.0
	)
	subjects := []string{"cred-a", "cred-b", "cred-c"}
	meters := map[string]*Meter{
		"cred-a": urgentMeter(t, 0.90),
		"cred-b": urgentMeter(t, 0.60),
		"cred-c": urgentMeter(t, 0.30),
	}
	at := Weekly.PeriodEnd(base).Add(-2 * time.Hour)

	// Jitter is disabled so that only damping can spread the picks: with three
	// candidates and a per-node factor, jitter alone would also spread them and
	// the test would not be measuring what it claims.
	run := func(damping float64) map[string]int {
		inflight := map[string]float64{}
		picks := map[string]int{}
		for n := range nodes {
			node := "node-" + string(rune('a'+n))
			bestSubject, best := "", 0.0
			for _, s := range subjects {
				u := meters[s].Urgency(at, UrgencyInput{
					Subject:   s,
					NodeID:    node,
					Occupancy: inflight[s] / capacity,
					Jitter:    -1,
					Damping:   damping,
				})
				if u > best {
					bestSubject, best = s, u
				}
			}
			if bestSubject == "" {
				t.Fatalf("node %s found no candidate", node)
			}
			picks[bestSubject]++
			inflight[bestSubject]++
		}
		return picks
	}

	undamped := run(-1)
	if undamped["cred-a"] != nodes {
		t.Fatalf("undamped picks = %v; every node should converge on one credential", undamped)
	}

	damped := run(0) // 0 means DefaultDamping
	for s, n := range damped {
		if float64(n) > capacity {
			t.Fatalf("damped picks = %v: %s took %d of a capacity of %v", damped, s, n, capacity)
		}
	}
	if len(damped) < 2 {
		t.Fatalf("damped picks = %v; damping did not spread the fleet", damped)
	}
	t.Logf("undamped %v, damped %v", undamped, damped)
}

// TestJitterIsDeterministicPerNode. Deterministic is a requirement, not a
// convenience: a random perturbation would be untestable and would re-roll on
// every request, turning a stable preference into flapping.
func TestJitterIsDeterministicPerNode(t *testing.T) {
	in := UrgencyInput{Subject: "cred-a", NodeID: "node-1"}
	first := Shape(1, in)
	for range 100 {
		if got := Shape(1, in); got != first {
			t.Fatalf("jitter is not deterministic: %v then %v", first, got)
		}
	}
	// Within the documented amplitude, and never zero or negative.
	if first <= 1-DefaultJitter/2-1e-9 || first >= 1+DefaultJitter/2+1e-9 {
		t.Fatalf("jitter factor %v is outside ±%v/2", first, DefaultJitter)
	}
	other := Shape(1, UrgencyInput{Subject: "cred-a", NodeID: "node-2"})
	if other == first {
		t.Fatal("two nodes jitter identically")
	}

	// A ranker rebuilt with the same node id reproduces its predecessor: the
	// factor is a function of the ids, not of process state.
	m := urgentMeter(t, 0.8)
	at := Weekly.PeriodEnd(base).Add(-time.Hour)
	r1 := NewRanker(RankerConfig{NodeID: "node-1"})
	r1.Track("cred-a", m)
	r2 := NewRanker(RankerConfig{NodeID: "node-1"})
	r2.Track("cred-a", m)
	if a, b := r1.Urgency("cred-a", 0.25, at), r2.Urgency("cred-a", 0.25, at); a != b {
		t.Fatalf("two rankers on the same node disagree: %v vs %v", a, b)
	}
}

// TestJitterIsKeyedByNodeAndSubject is the correction to "jittered per node".
//
// A factor that depended on the node alone would scale every candidate that
// node ranks by the same amount, so it could not change that node's ordering:
// it would be arithmetic with no effect on the stampede it exists to break. The
// key is the pair, and the test that proves it matters is that nodes holding
// identical candidates at identical occupancy do not all choose the same one.
func TestJitterIsKeyedByNodeAndSubject(t *testing.T) {
	const nodes = 64
	subjects := []string{"cred-a", "cred-b", "cred-c"}
	meters := map[string]*Meter{}
	for _, s := range subjects {
		meters[s] = urgentMeter(t, 0.8) // identical allowances
	}
	at := Weekly.PeriodEnd(base).Add(-2 * time.Hour)

	pick := func(node string, jitter float64) string {
		bestSubject, best := "", -1.0
		for _, s := range subjects {
			u := meters[s].Urgency(at, UrgencyInput{
				Subject: s, NodeID: node, Occupancy: 0, Jitter: jitter, Damping: -1,
			})
			if u > best {
				bestSubject, best = s, u
			}
		}
		return bestSubject
	}

	counts := map[string]int{}
	unjittered := map[string]int{}
	for n := range nodes {
		node := "node-" + string(rune('a'+n%26)) + string(rune('0'+n/26))
		counts[pick(node, 0)]++
		unjittered[pick(node, -1)]++
	}
	// Without jitter, identical candidates at identical occupancy are a tie and
	// every node breaks it the same way. That is the stampede.
	if len(unjittered) != 1 {
		t.Fatalf("unjittered picks = %v, expected every node on one credential", unjittered)
	}
	if len(counts) != len(subjects) {
		t.Fatalf("jittered picks = %v, expected every credential to be chosen", counts)
	}
	for s, n := range counts {
		if n < nodes/8 {
			t.Fatalf("jittered picks = %v: %s took only %d of %d", counts, s, n, nodes)
		}
	}
	t.Logf("jittered %v", counts)
}

// TestOccupancyIsClampedAndNeverImported. Occupancy is an input, not a
// dependency: this package must not reach into internal/capacity. Out-of-range
// values come from a caller that measured something odd and must not produce a
// negative rank.
func TestOccupancyIsClampedAndNeverImported(t *testing.T) {
	in := func(occ float64) UrgencyInput {
		return UrgencyInput{Subject: "c", NodeID: "n", Occupancy: occ, Jitter: -1}
	}
	if got := Shape(4, in(2)); got != 0 {
		t.Fatalf("occupancy 2 gave %v, want 0", got)
	}
	if got := Shape(4, in(-1)); got != 4 {
		t.Fatalf("occupancy -1 gave %v, want the undamped value", got)
	}
	if got := Shape(4, in(math.NaN())); got != 4 {
		t.Fatalf("NaN occupancy gave %v, want the undamped value", got)
	}
	if got := Shape(4, in(1)); got != 0 {
		t.Fatalf("a saturated credential scored %v, want 0", got)
	}
	closeTo(t, Shape(4, in(0.25)), 3, "damped urgency")
}

// TestRankerScoresUnknownSubjectsZero: an unmetered credential has no expiring
// allowance, so it has nothing at stake. It is emphatically not "infinitely
// urgent", which is the failure mode of treating absence as an extreme.
func TestRankerScoresUnknownSubjectsZero(t *testing.T) {
	r := NewRanker(RankerConfig{NodeID: "node-1"})
	at := Weekly.PeriodEnd(base).Add(-time.Hour)
	if got := r.Urgency("nobody", 0, at); got != 0 {
		t.Fatalf("an unknown subject scored %v", got)
	}
	m := urgentMeter(t, 0.8)
	r.Track("cred-a", m)
	if got := r.Urgency("cred-a", 0, at); got <= 0 {
		t.Fatalf("a tracked subject scored %v", got)
	}
	r.Untrack("cred-a")
	if got := r.Urgency("cred-a", 0, at); got != 0 {
		t.Fatalf("an untracked subject scored %v", got)
	}
	r.Track("cred-a", m)
	r.Track("cred-a", nil) // nil untracks
	if got := r.Urgency("cred-a", 0, at); got != 0 {
		t.Fatalf("a nil meter scored %v", got)
	}
}

// TestPeriodBoundsMatchesTheGeneralPath guards the shortcut the ranking path
// takes. periodBounds computes the start once and derives the end from it,
// which is only sound because every boundary here is UTC; a day that could be
// 23 or 25 hours long would break it, and so would a month treated as a fixed
// offset. The sweep crosses every month length and a leap February.
func TestPeriodBoundsMatchesTheGeneralPath(t *testing.T) {
	from := time.Date(2023, 11, 1, 3, 17, 0, 0, time.UTC)
	for _, w := range []Window{Daily, Weekly, Monthly, Rolling(time.Hour), Rolling(5 * time.Hour)} {
		for d := range 900 {
			now := from.AddDate(0, 0, d).Add(time.Duration(d) * 37 * time.Minute)
			s, e := w.periodBounds(now)
			if !s.Equal(w.PeriodStart(now)) {
				t.Fatalf("%s periodBounds start at %v = %v, want %v", w, now, s, w.PeriodStart(now))
			}
			if !e.Equal(w.PeriodEnd(now)) {
				t.Fatalf("%s periodBounds end at %v = %v, want %v", w, now, e, w.PeriodEnd(now))
			}
		}
	}
}

// TestUrgencyIsConcurrencySafe runs the query against a meter that is being
// recorded into, because the router ranks while requests are settling.
func TestUrgencyIsConcurrencySafe(t *testing.T) {
	m := urgentMeter(t, 0.9)
	r := NewRanker(RankerConfig{NodeID: "node-1"})
	r.Track("cred-a", m)
	at := Weekly.PeriodEnd(base).Add(-time.Hour)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 2000 {
			m.Record(at, Usage{Requests: 1})
		}
	}()
	for range 2000 {
		if got := r.Urgency("cred-a", 0.5, at); got < 0 {
			t.Errorf("negative urgency %v", got)
			break
		}
	}
	<-done
}
