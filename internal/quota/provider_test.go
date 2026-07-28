package quota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSecret = "sk-provider-secret-do-not-print" // pragma: allowlist secret — test fixture

func TestCredentialRedactsItsSecret(t *testing.T) {
	c := NewCredential("acct-1", "plan-a", testSecret)
	if c.Secret() != testSecret {
		t.Fatal("Secret() must return the secret to its prober")
	}
	for _, verb := range []string{"%v", "%s", "%q", "%#v", "%+v", "%x", "%d"} {
		out := fmt.Sprintf(verb, c)
		if strings.Contains(out, testSecret) {
			t.Fatalf("%s leaked the secret: %s", verb, out)
		}
		if !strings.Contains(out, "acct-1") {
			t.Fatalf("%s dropped the identifying part: %s", verb, out)
		}
	}
}

// TestProviderAndLocalAreCombinedNotSubstituted is the correction of §6.2:
// during a burst the local counter is the fresher signal, and a provider
// figure that lags must not be able to erase it.
func TestProviderAndLocalAreCombinedNotSubstituted(t *testing.T) {
	w := Rolling(5 * time.Hour)
	m := newTestMeter(t, Rule{Window: w, Metric: MetricCostUSD, Limit: NanoUSD(10)})
	tr := NewTracker(m)
	m.AttachTracker(tr)

	// The provider says 6 USD used — which includes traffic that never passed
	// through dorang, so it is larger than anything we metered.
	poll1 := base
	tr.Adopt(ProbeResult{FetchedAt: poll1, Windows: []ProviderWindow{
		{Window: w, Metric: MetricCostUSD, Used: NanoUSD(6), Limit: NanoUSD(10)},
	}})
	if got := m.Used(poll1, Rule{Window: w, Metric: MetricCostUSD}); got != NanoUSD(6) {
		t.Fatalf("effective used = %v USD, want 6 (the provider's figure)", USD(got))
	}

	// A burst arrives between polls. The local delta is the fresher signal.
	burst := poll1.Add(30 * time.Second)
	m.Record(burst, Usage{CostNanoUSD: NanoUSD(3)})
	if got := m.Used(burst, Rule{Window: w, Metric: MetricCostUSD}); got != NanoUSD(9) {
		t.Fatalf("effective used during the burst = %v USD, want 9 (6 reported + 3 local)", USD(got))
	}

	// One more unit of spend exhausts the credential, even though the
	// provider's last word was 6 of 10.
	m.Record(burst, Usage{CostNanoUSD: NanoUSD(1)})
	if d := m.Check(burst); d.Allow {
		t.Fatalf("burst was not seen: %+v", d)
	} else if !d.FromProvider {
		t.Fatal("the decision does not record that the provider figure took part")
	}
}

// TestALaggingPollCannotEraseALocalBurst is the specific defect revision 1 had:
// the provider was declared authoritative, so a poll returning a stale, lower
// figure discarded the local delta and routing became most aggressive exactly
// when its information was worst.
func TestALaggingPollCannotEraseALocalBurst(t *testing.T) {
	w := Rolling(5 * time.Hour)
	m := newTestMeter(t, Rule{Window: w, Metric: MetricCostUSD, Limit: NanoUSD(10)})
	tr := NewTracker(m)
	m.AttachTracker(tr)

	tr.Adopt(ProbeResult{FetchedAt: base, Windows: []ProviderWindow{
		{Window: w, Metric: MetricCostUSD, Used: NanoUSD(5), Limit: NanoUSD(10)},
	}})
	burst := base.Add(time.Minute)
	m.Record(burst, Usage{CostNanoUSD: NanoUSD(4)}) // effective is now 9

	// The next poll reports 6: the provider's accounting has not caught up.
	poll2 := base.Add(2 * time.Minute)
	tr.Adopt(ProbeResult{FetchedAt: poll2, Windows: []ProviderWindow{
		{Window: w, Metric: MetricCostUSD, Used: NanoUSD(6), Limit: NanoUSD(10)},
	}})
	got := m.Used(poll2, Rule{Window: w, Metric: MetricCostUSD})
	if got != NanoUSD(9) {
		t.Fatalf("after a lagging poll the effective figure is %v USD, want 9; "+
			"the provider's re-baseline erased the local delta", USD(got))
	}

	// A poll that reports more than the local estimate re-baselines upward,
	// because it can see traffic dorang never saw.
	poll3 := poll2.Add(time.Minute)
	tr.Adopt(ProbeResult{FetchedAt: poll3, Windows: []ProviderWindow{
		{Window: w, Metric: MetricCostUSD, Used: NanoUSD(9.5), Limit: NanoUSD(10)},
	}})
	if got := m.Used(poll3, Rule{Window: w, Metric: MetricCostUSD}); got != NanoUSD(9.5) {
		t.Fatalf("upward re-baseline = %v USD, want 9.5", USD(got))
	}
}

func TestTrackerDerivesUsedFromPercent(t *testing.T) {
	w := Daily
	tr := NewTracker(nil)
	tr.Adopt(ProbeResult{FetchedAt: base, Windows: []ProviderWindow{
		{Window: w, Metric: MetricTokensTotal, Limit: 1000, UsedPercent: 25},
	}})
	used, limit, ok := tr.Effective(w, MetricTokensTotal, base)
	if !ok || used != 250 || limit != 1000 {
		t.Fatalf("Effective = %d/%d ok=%v, want 250/1000", used, limit, ok)
	}
}

// TestFailedFetchNeverDisablesACredential: a failed read is not an exhausted
// quota (DESIGN §6.2).
func TestFailedFetchNeverDisablesACredential(t *testing.T) {
	w := Rolling(time.Hour)
	m := newTestMeter(t, Rule{Window: w, Metric: MetricCostUSD, Limit: NanoUSD(10)})
	tr := NewTracker(m)
	m.AttachTracker(tr)

	tr.Adopt(ProbeResult{FetchedAt: base, Windows: []ProviderWindow{
		{Window: w, Metric: MetricCostUSD, Used: NanoUSD(4), Limit: NanoUSD(10)},
	}})

	fail := base.Add(time.Minute)
	boom := errors.New("connection reset")
	for i := range 5 {
		tr.Fail(boom, fail.Add(time.Duration(i)*time.Minute))
	}

	// The credential still serves, on the last good snapshot plus local delta.
	later := fail.Add(10 * time.Minute)
	if d := m.Check(later); !d.Allow {
		t.Fatalf("a failed probe took the credential out of service: %+v", d)
	}
	if got := m.Used(later, Rule{Window: w, Metric: MetricCostUSD}); got != NanoUSD(4) {
		t.Fatalf("the last good snapshot was lost: %v USD", USD(got))
	}
	// And the staleness is visible rather than hidden.
	st, ok := tr.Staleness(later)
	if !ok || st != later.Sub(base) {
		t.Fatalf("staleness = %v ok=%v, want %v", st, ok, later.Sub(base))
	}
	if !errors.Is(tr.LastError(), boom) || tr.Failures() != 5 {
		t.Fatalf("last error %v, failures %d", tr.LastError(), tr.Failures())
	}
	// A later success clears the failure count and re-baselines.
	tr.Adopt(ProbeResult{FetchedAt: later, Windows: []ProviderWindow{
		{Window: w, Metric: MetricCostUSD, Used: NanoUSD(5), Limit: NanoUSD(10)},
	}})
	if tr.Failures() != 0 || tr.LastError() != nil {
		t.Fatalf("a success did not clear the failure state: %d %v", tr.Failures(), tr.LastError())
	}
	if st, ok := tr.Staleness(later); !ok || st != 0 {
		t.Fatalf("staleness after a fresh poll = %v", st)
	}
}

func TestStalenessBeforeAnySuccessfulPoll(t *testing.T) {
	tr := NewTracker(nil)
	if _, ok := tr.Staleness(base); ok {
		t.Fatal("a tracker that has never polled reported a staleness; that is not the same as fresh")
	}
	if _, _, ok := tr.Effective(Daily, MetricCostUSD, base); ok {
		t.Fatal("Effective reported a figure with no snapshot")
	}
}

func TestMeterWithoutProviderFiguresUsesLocalOnly(t *testing.T) {
	w := Rolling(time.Hour)
	r := Rule{Window: w, Metric: MetricRequests, Limit: 2}
	m := newTestMeter(t, r)
	m.AttachTracker(NewTracker(m)) // attached, but never polled
	m.Record(base, Usage{Requests: 2})
	if d := m.Check(base); d.Allow {
		t.Fatal("local metering stopped working when a tracker was attached")
	}
}

// ---------------------------------------------------------------- registry ---

func TestRegistryPollsConcurrentlyOffTheRequestPath(t *testing.T) {
	prober := NewStaticProber("plan-a")
	prober.SetDelay(50 * time.Millisecond)
	reg := NewRegistry(func() time.Time { return base })
	reg.Register(prober, time.Second)

	const n = 8
	trackers := make([]*Tracker, n)
	for i := range n {
		id := fmt.Sprintf("acct-%d", i)
		prober.Set(id, base, ProviderWindow{Window: Daily, Metric: MetricRequests, Used: int64(i), Limit: 100})
		trackers[i] = NewTracker(nil)
		if err := reg.Track(NewCredential(id, "plan-a", testSecret), trackers[i]); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	stats := reg.PollOnce(context.Background())
	elapsed := time.Since(start)

	if stats.Polled != n || stats.Succeeded != n || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if elapsed > 40*time.Millisecond*n/2 {
		t.Fatalf("%d probes of 50ms took %v; they are not running concurrently", n, elapsed)
	}
	for i, tr := range trackers {
		used, _, ok := tr.Effective(Daily, MetricRequests, base)
		if !ok || used != int64(i) {
			t.Fatalf("tracker %d did not adopt: %d %v", i, used, ok)
		}
	}
}

func TestRegistryTimeoutIsPerProvider(t *testing.T) {
	slow := NewStaticProber("slow")
	slow.SetDelay(time.Second)
	fast := NewStaticProber("fast")

	reg := NewRegistry(func() time.Time { return base })
	reg.Register(slow, 20*time.Millisecond)
	reg.Register(fast, time.Second)

	slowT, fastT := NewTracker(nil), NewTracker(nil)
	slow.Set("s1", base, ProviderWindow{Window: Daily, Metric: MetricRequests, Used: 1, Limit: 10})
	fast.Set("f1", base, ProviderWindow{Window: Daily, Metric: MetricRequests, Used: 2, Limit: 10})
	if err := reg.Track(NewCredential("s1", "slow", testSecret), slowT); err != nil {
		t.Fatal(err)
	}
	if err := reg.Track(NewCredential("f1", "fast", testSecret), fastT); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	stats := reg.PollOnce(context.Background())
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("the slow provider's timeout did not bound the round: %v", d)
	}
	if stats.Succeeded != 1 || stats.Failed != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	// The slow one failed and kept serving; the fast one adopted.
	if slowT.LastError() == nil {
		t.Fatal("the timed-out probe recorded no error")
	}
	if used, _, ok := fastT.Effective(Daily, MetricRequests, base); !ok || used != 2 {
		t.Fatalf("fast tracker = %d %v", used, ok)
	}
}

// TestFailedPollThroughTheRegistryKeepsServing is the end-to-end version of
// "a failed fetch never disables a credential": one probe succeeds, the next
// fails, and the meter goes on deciding from the last good snapshot.
func TestFailedPollThroughTheRegistryKeepsServing(t *testing.T) {
	w := Rolling(time.Hour)
	m := newTestMeter(t, Rule{Window: w, Metric: MetricCostUSD, Limit: NanoUSD(10)})
	tr := NewTracker(m)
	m.AttachTracker(tr)

	now := base
	prober := NewStaticProber("plan-a")
	reg := NewRegistry(func() time.Time { return now })
	reg.Register(prober, time.Second)
	cred := NewCredential("acct-1", "plan-a", testSecret)
	if err := reg.Track(cred, tr); err != nil {
		t.Fatal(err)
	}

	prober.Set("acct-1", now, ProviderWindow{
		Window: w, Metric: MetricCostUSD, Used: NanoUSD(4), Limit: NanoUSD(10),
		ResetAt: now.Add(time.Hour),
	})
	if s := reg.PollOnce(context.Background()); s.Succeeded != 1 {
		t.Fatalf("first poll: %+v", s)
	}

	now = now.Add(5 * time.Minute)
	prober.SetError("acct-1", errors.New("502 from the provider"))
	if s := reg.PollOnce(context.Background()); s.Failed != 1 {
		t.Fatalf("second poll: %+v", s)
	}

	d := m.Check(now)
	if !d.Allow {
		t.Fatalf("a failed poll took the credential out of service: %+v", d)
	}
	if got := m.Used(now, Rule{Window: w, Metric: MetricCostUSD}); got != NanoUSD(4) {
		t.Fatalf("used = %v USD, want the last good snapshot's 4", USD(got))
	}
	st, ok := tr.Staleness(now)
	if !ok || st != 5*time.Minute {
		t.Fatalf("staleness = %v, %v", st, ok)
	}

	// And once the credential is genuinely exhausted, the cooldown honors the
	// provider's own reset instant rather than guessing from local metering.
	m.Record(now, Usage{CostNanoUSD: NanoUSD(6)})
	d = m.Check(now)
	if d.Allow {
		t.Fatalf("exhausted credential still admitted: %+v", d)
	}
	if !d.FromProvider || d.ProviderStale != 5*time.Minute {
		t.Fatalf("decision does not report the provider's part: %+v", d)
	}
	if want := base.Add(time.Hour); !d.ResetAt.Equal(want) {
		t.Fatalf("cooldown until %v, want the provider's reset at %v", d.ResetAt, want)
	}
}

func TestRegistryRefusesAnUntrackedProvider(t *testing.T) {
	reg := NewRegistry(nil)
	err := reg.Track(NewCredential("x", "nobody", testSecret), NewTracker(nil))
	if !errors.Is(err, ErrNoProber) {
		t.Fatalf("Track = %v, want ErrNoProber", err)
	}
	if strings.Contains(fmt.Sprint(err), testSecret) {
		t.Fatal("the error leaked the secret")
	}
}

func TestRegistryRunStopsWithTheContext(t *testing.T) {
	prober := NewStaticProber("plan-a")
	prober.Set("a", base, ProviderWindow{Window: Daily, Metric: MetricRequests, Used: 1, Limit: 10})
	reg := NewRegistry(nil)
	reg.Register(prober, time.Second)
	if err := reg.Track(NewCredential("a", "plan-a", testSecret), NewTracker(nil)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); reg.Run(ctx, 5*time.Millisecond) }()
	// Wait for the poll, not for thirty milliseconds. A 5 ms interval is a
	// promise about the gap between ticks, not about how many of them fit in an
	// arbitrary window on a machine running the rest of this suite — and
	// cancelling before the first one turns "Run never polled" into a report
	// about the scheduler. The cancel below still tests what it is here for:
	// Run has to return, which wg.Wait establishes.
	deadline := time.Now().Add(10 * time.Second)
	for prober.Calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	wg.Wait()
	if prober.Calls() == 0 {
		t.Fatal("Run never polled")
	}
	reg.Untrack("a")
	before := prober.Calls()
	reg.PollOnce(context.Background())
	if prober.Calls() != before {
		t.Fatal("Untrack did not stop the polling")
	}
}
