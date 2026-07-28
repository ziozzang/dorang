package probe

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
)

// These are the tests about DESIGN §6.2 rather than about HTTP: what happens to
// a credential's figures when a real prober is wired to a real tracker.

// zaiPercent builds a payload reporting one five-hour token window at pct.
func zaiPercent(pct float64) string {
	return `{"code":200,"success":true,"data":{"limits":[
	  {"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":` +
		strconv.FormatFloat(pct, 'f', -1, 64) + `,"nextResetTime":"2026-07-28T18:00:00Z"}]}}`
}

// testClock is a settable clock shared by a prober and a quota.Registry, so
// that a test can move both together without racing either.
type testClock struct{ ns atomic.Int64 }

func newTestClock(t time.Time) *testClock {
	c := &testClock{}
	c.ns.Store(t.UnixNano())
	return c
}

func (c *testClock) now() time.Time                   { return time.Unix(0, c.ns.Load()).UTC() }
func (c *testClock) add(d time.Duration)              { c.ns.Add(int64(d)) }
func staleness(d time.Duration, _ bool) time.Duration { return d }

// The five-hour rule an operator would write for a coding plan.
var fiveHourRule = quota.Rule{
	Window: quota.Rolling(5 * time.Hour),
	Metric: quota.MetricTokensTotal,
	Limit:  1000,
	Resets: true,
}

// combineFixture wires a prober, a meter and a tracker the way the gateway
// would, with a settable reported percentage.
type combineFixture struct {
	pct     atomic.Value // string
	prober  *Prober
	meter   *quota.Meter
	tracker *quota.Tracker
	cred    quota.Credential
	calls   atomic.Int64
}

func newCombineFixture(t *testing.T, allowances ...Allowance) *combineFixture {
	t.Helper()
	f := &combineFixture{}
	f.pct.Store(zaiPercent(30))
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		jsonHandler(200, f.pct.Load().(string))(w, r)
	})
	f.prober = newProbe(t, "zai", s.URL, func(c *Config) { c.Allowances = allowances })

	m, err := quota.NewMeter(quota.MeterConfig{Rules: []quota.Rule{fiveHourRule}})
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	f.meter = m
	f.tracker = quota.NewTracker(m)
	m.AttachTracker(f.tracker)
	f.cred = testCredential("zai")
	return f
}

// poll performs one read and adopts it, as quota.Registry does.
func (f *combineFixture) poll(t *testing.T) {
	t.Helper()
	res, err := f.prober.Probe(context.Background(), f.cred)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	f.tracker.Adopt(res)
}

func (f *combineFixture) report(pct float64) { f.pct.Store(zaiPercent(pct)) }

// The formula, with the burst case that is the reason it exists. A poll is up
// to one interval stale, so during a burst the local counter is the fresher
// signal and the provider's figure must not be able to erase it.
func TestMaxOfTwoSourcesIncludingTheBurst(t *testing.T) {
	t.Parallel()
	f := newCombineFixture(t, Allowance{
		Label:  "tokens_limit:5h",
		Window: fiveHourRule.Window,
		Metric: fiveHourRule.Metric,
		Limit:  fiveHourRule.Limit,
	})
	now := time.Now()

	// The provider says 30% of a thousand-token allowance.
	f.poll(t)
	used, limit, ok := f.tracker.Effective(fiveHourRule.Window, fiveHourRule.Metric, now)
	if !ok || used != 300 || limit != 1000 {
		t.Fatalf("after the first poll: used=%d limit=%d ok=%v, want 300/1000", used, limit, ok)
	}

	// A burst of 500 tokens lands between polls.
	f.meter.Record(now, quota.Usage{TokensInput: 200, TokensOutput: 300, Requests: 1})
	used, _, _ = f.tracker.Effective(fiveHourRule.Window, fiveHourRule.Metric, now)
	if used != 800 {
		t.Fatalf("mid-burst used = %d, want 800: the local delta is the fresher signal", used)
	}

	// The next poll still reports the pre-burst figure, one interval behind.
	// Re-baselining on it must not erase the burst.
	f.poll(t)
	used, _, _ = f.tracker.Effective(fiveHourRule.Window, fiveHourRule.Metric, now)
	if used != 800 {
		t.Fatalf("after a stale poll used = %d, want 800: the provider's lag erased the burst", used)
	}

	// The provider catches up and reports out-of-band traffic dorang never saw.
	// Now the provider is the larger figure and wins.
	f.report(95)
	f.poll(t)
	used, _, _ = f.tracker.Effective(fiveHourRule.Window, fiveHourRule.Metric, now)
	if used != 950 {
		t.Fatalf("after the provider caught up used = %d, want 950", used)
	}

	// And the combination is what the meter meters and admits on.
	if got := f.meter.Used(now, fiveHourRule); got != 950 {
		t.Errorf("the meter's effective used = %d, want the provider's 950", got)
	}
	if d := f.meter.Check(now); !d.Allow {
		t.Errorf("decision = %+v, want an allowed check below the limit", d)
	}
	f.report(100)
	f.poll(t)
	if d := f.meter.Check(now); d.Allow {
		t.Errorf("a fully consumed provider allowance still admitted: %+v", d)
	}
}

// Without a declared allowance the provider publishes a proportion of a number
// dorang does not know. §6.2's max() is unit-homogeneous arithmetic and cannot
// combine that with a token count, so the figure gates nothing — and, the part
// that matters, it does not accumulate into one either.
//
// Before this was fixed, each poll re-baselined a percent-only window as
// "previous + local delta". That sum is monotone while the rolling window is
// not, so it climbed past any configured limit and parked the credential in a
// cooldown that nothing could clear: a provider that reports honestly and a
// gateway that meters correctly, combining into a dead credential.
func TestPercentOnlyWindowDoesNotAccumulateIntoAFakeFigure(t *testing.T) {
	t.Parallel()
	f := newCombineFixture(t) // no allowance
	now := time.Now()

	for i := range 10 {
		f.poll(t)
		// Traffic between every poll, which is what fed the runaway.
		f.meter.Record(now, quota.Usage{TokensInput: 25, TokensOutput: 25, Requests: 1})

		if _, _, ok := f.tracker.Effective(fiveHourRule.Window, fiveHourRule.Metric, now); ok {
			t.Fatalf("poll %d: a percentage was reported as a figure in the metric's units", i)
		}
		// The effective figure is the local one exactly — not the local one plus
		// an accumulating ghost.
		if want, got := int64(i+1)*50, f.meter.Used(now, fiveHourRule); got != want {
			t.Fatalf("poll %d: effective used = %d, want the local %d", i, got, want)
		}
		if d := f.meter.Check(now); !d.Allow {
			t.Fatalf("poll %d: the credential was refused: %+v", i, d)
		}
	}

	// Local metering is untouched and still the thing that gates.
	if got := f.meter.Used(now, fiveHourRule); got != 500 {
		t.Errorf("local used = %d, want 500", got)
	}
	// And the percentage is not lost: it is readable, in its own unit.
	if pct, ok := f.tracker.UsedPercent(fiveHourRule.Window, fiveHourRule.Metric); !ok || pct != 30 {
		t.Errorf("UsedPercent = %v,%v want 30,true", pct, ok)
	}
}

// The reason this package exists at all: a rolling window has no reset of its
// own, so §7.5a(c) scores zero until a provider reports one. A percent-only
// window still carries that instant, which is most of its value.
func TestProviderResetIsWhatMakesUrgencyNonZero(t *testing.T) {
	t.Parallel()
	f := newCombineFixture(t)
	now := time.Now()
	f.meter.Record(now, quota.Usage{TokensInput: 100, Requests: 1})

	// Before any poll: a rolling window's reset is unknown, and unknown scores
	// zero rather than imminent.
	if u := f.meter.Urgency(now, quota.UrgencyInput{}); u != 0 {
		t.Fatalf("urgency without a provider reset = %v, want 0", u)
	}

	f.poll(t)
	reset, ok := f.tracker.ResetAt(fiveHourRule.Window, fiveHourRule.Metric)
	if !ok {
		t.Fatal("the provider's reset instant did not reach the tracker")
	}
	want := time.Date(2026, 7, 28, 18, 0, 0, 0, time.UTC)
	if !reset.Equal(want) {
		t.Fatalf("reset = %v, want %v", reset, want)
	}

	// With a reset in the future the allowance can be scored. (The instant is
	// fixed in the payload, so this asserts the plumbing, not the arithmetic —
	// quota's own tests own the formula.)
	at := want.Add(-30 * time.Minute)
	a, found := f.meter.Allowance(at, fiveHourRule)
	if !found {
		t.Fatal("no allowance for the rule")
	}
	if !a.ResetAt.Equal(want) {
		t.Errorf("allowance reset = %v, want the provider's %v", a.ResetAt, want)
	}
	if u := a.Urgency(at); u <= 0 {
		t.Errorf("urgency with a provider reset = %v, want more than zero", u)
	}
}

// A failed read must not disable anything, and the staleness of the figures it
// failed to refresh must keep growing.
func TestFailedPollLeavesTheTrackerIntactAndStale(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			jsonHandler(500, `{"error":"boom"}`)(w, r)
			return
		}
		jsonHandler(200, zaiPercent(30))(w, r)
	})
	clock := newTestClock(time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC))
	p := newProbe(t, "zai", s.URL, func(c *Config) {
		c.Now = clock.now
		c.Allowances = []Allowance{{Label: "tokens_limit:5h", Window: fiveHourRule.Window,
			Metric: fiveHourRule.Metric, Limit: fiveHourRule.Limit}}
	})

	m, err := quota.NewMeter(quota.MeterConfig{Rules: []quota.Rule{fiveHourRule}})
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	tr := quota.NewTracker(m)
	m.AttachTracker(tr)

	reg := quota.NewRegistry(clock.now)
	reg.Register(p, time.Second)
	cred := testCredential("zai")
	if err := reg.Track(cred, tr); err != nil {
		t.Fatalf("Track: %v", err)
	}

	if st := reg.PollOnce(context.Background()); st.Succeeded != 1 {
		t.Fatalf("first poll = %+v", st)
	}
	used, _, ok := tr.Effective(fiveHourRule.Window, fiveHourRule.Metric, clock.now())
	if !ok || used != 300 {
		t.Fatalf("used = %d ok=%v", used, ok)
	}

	// Ten minutes later, the endpoint is down.
	fail.Store(true)
	clock.add(10 * time.Minute)
	if st := reg.PollOnce(context.Background()); st.Failed != 1 {
		t.Fatalf("second poll = %+v", st)
	}

	now := clock.now()
	used, _, ok = tr.Effective(fiveHourRule.Window, fiveHourRule.Metric, now)
	if !ok || used != 300 {
		t.Errorf("a failed read changed the figures: used=%d ok=%v", used, ok)
	}
	if d := staleness(tr.Staleness(now)); d != 10*time.Minute {
		t.Errorf("staleness = %v, want 10m", d)
	}
	if tr.LastError() == nil {
		t.Error("the failure was not recorded")
	}
	if d := m.Check(now); !d.Allow {
		t.Error("a failed read disabled the credential; a failed read is not an exhausted quota")
	}
}

// One slow provider must not delay the others. quota.Registry polls in
// parallel with a per-provider timeout, and this asserts the property end to
// end rather than trusting the loop.
func TestSlowProviderDoesNotDelayOthers(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	slowEntered := make(chan struct{})
	fastServed := make(chan struct{})

	slow := serve(t, func(w http.ResponseWriter, r *http.Request) {
		close(slowEntered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	fast := serve(t, func(w http.ResponseWriter, r *http.Request) {
		jsonHandler(200, `{"is_available":true,"balance_infos":[
		  {"currency":"USD","total_balance":"1.85"}]}`)(w, r)
		close(fastServed)
	})

	slowP := newProbe(t, "zai", slow.URL)
	fastP := newProbe(t, "deepseek", fast.URL)

	slowTracker, fastTracker := quota.NewTracker(nil), quota.NewTracker(nil)
	reg := quota.NewRegistry(nil)
	reg.Register(slowP, 250*time.Millisecond) // a per-provider timeout
	reg.Register(fastP, 5*time.Second)
	if err := reg.Track(testCredential("zai"), slowTracker); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := reg.Track(quota.NewCredential("cred-2", "deepseek", testSecret), fastTracker); err != nil {
		t.Fatalf("Track: %v", err)
	}

	done := make(chan quota.PollStats, 1)
	go func() { done <- reg.PollOnce(context.Background()) }()

	// The fast provider must be answered while the slow one is still blocked.
	<-slowEntered
	select {
	case <-fastServed:
	case <-time.After(2 * time.Second):
		t.Fatal("the fast provider was still waiting behind the slow one")
	}

	// And the slow one is cut off by its own timeout, not by the round.
	st := <-done
	close(release)
	if st.Succeeded != 1 || st.Failed != 1 {
		t.Errorf("stats = %+v, want one of each", st)
	}
	if _, ok := slowTracker.Staleness(time.Now()); ok {
		t.Error("the timed-out provider recorded a successful read")
	}
	if slowTracker.Failures() != 1 {
		t.Errorf("slow failures = %d, want 1", slowTracker.Failures())
	}
}
