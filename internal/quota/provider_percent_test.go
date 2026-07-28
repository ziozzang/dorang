package quota

import (
	"testing"
	"time"
)

// The dominant real shape of a provider-reported quota is a percentage with no
// absolute ceiling: "37% of your five-hour allowance", never "3,700 of 10,000".
// DESIGN §6.2's correction is unit-homogeneous arithmetic, so such a window has
// nothing to combine — and the way it had nothing to combine used to be a
// silent, unrecoverable failure. These tests pin the behavior down.

// fixedCounter is a local counter a test can move by hand.
type fixedCounter struct{ v [numMetrics]int64 }

func (f *fixedCounter) Cumulative(m Metric) int64 { return f.v[m] }

func percentWindow(pct float64, reset time.Time) ProviderWindow {
	return ProviderWindow{
		Window:      Rolling(5 * time.Hour),
		Metric:      MetricTokensTotal,
		UsedPercent: pct,
		ResetAt:     reset,
	}
}

// A percentage of an unknown total is not a figure in the metric's units, and
// Effective says so rather than answering in the wrong unit.
func TestPercentOnlyWindowReportsNoUsage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	tr := NewTracker(&fixedCounter{})

	tr.Adopt(ProbeResult{FetchedAt: now, Windows: []ProviderWindow{percentWindow(37, reset)}})

	if _, _, ok := tr.Effective(Rolling(5*time.Hour), MetricTokensTotal, now); ok {
		t.Error("a percentage was reported as a used figure")
	}
	// What it does carry is the reset instant, which is the whole of what
	// §7.5a(c) needs and cannot get from a rolling window any other way.
	got, ok := tr.ResetAt(Rolling(5*time.Hour), MetricTokensTotal)
	if !ok || !got.Equal(reset) {
		t.Errorf("ResetAt = %v,%v want %v", got, ok, reset)
	}
	// And the percentage itself is readable, in its own unit.
	if pct, ok := tr.UsedPercent(Rolling(5*time.Hour), MetricTokensTotal); !ok || pct != 37 {
		t.Errorf("UsedPercent = %v,%v want 37,true", pct, ok)
	}
}

// The regression this guard exists for. Re-baselining a percent-only window as
// "previous + local delta" accumulates a figure that is monotone while the
// rolling window it is compared against is not, so it climbs past any limit and
// never comes back down.
func TestPercentOnlyWindowDoesNotAccumulate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	local := &fixedCounter{}
	tr := NewTracker(local)

	for i := range 50 {
		local.v[MetricTokensTotal] = int64(i) * 1000
		tr.Adopt(ProbeResult{
			FetchedAt: now.Add(time.Duration(i) * time.Minute),
			Windows:   []ProviderWindow{percentWindow(30, now.Add(3*time.Hour))},
		})
		if used, _, ok := tr.Effective(Rolling(5*time.Hour), MetricTokensTotal, now); ok || used != 0 {
			t.Fatalf("poll %d: used = %d, ok = %v; the provider's percentage grew a body", i, used, ok)
		}
	}
}

// A provider that starts publishing a ceiling makes the same window usable, and
// one that stops publishing it makes it unusable again — without either
// transition leaving a stale absolute figure behind.
func TestWindowGainingAndLosingAnAbsoluteFigure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	local := &fixedCounter{}
	tr := NewTracker(local)
	w, m := Rolling(5*time.Hour), MetricTokensTotal

	tr.Adopt(ProbeResult{FetchedAt: now, Windows: []ProviderWindow{percentWindow(30, time.Time{})}})
	if _, _, ok := tr.Effective(w, m, now); ok {
		t.Fatal("percent-only reported a figure")
	}

	// Now with a ceiling: the percentage becomes 300 of 1000.
	pw := percentWindow(30, time.Time{})
	pw.Limit = 1000
	tr.Adopt(ProbeResult{FetchedAt: now.Add(time.Minute), Windows: []ProviderWindow{pw}})
	used, limit, ok := tr.Effective(w, m, now)
	if !ok || used != 300 || limit != 1000 {
		t.Fatalf("used=%d limit=%d ok=%v, want 300/1000", used, limit, ok)
	}

	// The local delta still applies from here, which is §6.2's whole point.
	local.v[MetricTokensTotal] = 250
	if used, _, _ := tr.Effective(w, m, now); used != 550 {
		t.Fatalf("used = %d, want 550", used)
	}

	// And back to percent-only: no stale absolute figure survives.
	tr.Adopt(ProbeResult{FetchedAt: now.Add(2 * time.Minute),
		Windows: []ProviderWindow{percentWindow(30, time.Time{})}})
	if used, _, ok := tr.Effective(w, m, now); ok {
		t.Fatalf("used = %d ok = %v; the previous ceiling outlived its report", used, ok)
	}
}

// A provider that publishes both figures is unaffected by any of the above.
func TestAbsoluteWindowsStillCombine(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	local := &fixedCounter{}
	tr := NewTracker(local)
	w, m := Rolling(5*time.Hour), MetricTokensTotal

	tr.Adopt(ProbeResult{FetchedAt: now, Windows: []ProviderWindow{
		{Window: w, Metric: m, Used: 300, Limit: 1000},
	}})
	local.v[MetricTokensTotal] = 500
	// A stale poll must not erase the burst local metering saw.
	tr.Adopt(ProbeResult{FetchedAt: now.Add(time.Minute), Windows: []ProviderWindow{
		{Window: w, Metric: m, Used: 300, Limit: 1000},
	}})
	if used, _, _ := tr.Effective(w, m, now); used != 800 {
		t.Fatalf("used = %d, want 800", used)
	}
}

// UsedPercent is unknown rather than zero when there is nothing to report.
func TestUsedPercentUnknown(t *testing.T) {
	t.Parallel()
	tr := NewTracker(nil)
	if _, ok := tr.UsedPercent(Rolling(5*time.Hour), MetricTokensTotal); ok {
		t.Error("a window that was never reported has a percentage")
	}
	now := time.Now()
	tr.Adopt(ProbeResult{FetchedAt: now, Windows: []ProviderWindow{
		{Window: Rolling(5 * time.Hour), Metric: MetricTokensTotal, Used: 1, Limit: 0},
	}})
	if _, ok := tr.UsedPercent(Rolling(5*time.Hour), MetricTokensTotal); ok {
		t.Error("a window with no ceiling produced a percentage")
	}
}
