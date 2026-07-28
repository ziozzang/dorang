package quota

import (
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	cases := []struct {
		in   string
		want Window
		str  string
	}{
		{"5h", Rolling(5 * time.Hour), "5h"},
		{"1h", Rolling(time.Hour), "1h"},
		{"rolling:90m", Rolling(90 * time.Minute), "90m"},
		{"rolling:2h", Rolling(2 * time.Hour), "2h"},
		{"daily", Daily, "daily"},
		{"WEEKLY", Weekly, "weekly"},
		{" monthly ", Monthly, "monthly"},
	}
	for _, c := range cases {
		got, err := ParseWindow(c.in)
		if err != nil {
			t.Fatalf("ParseWindow(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("ParseWindow(%q) = %v, want %v", c.in, got, c.want)
		}
		if got.String() != c.str {
			t.Fatalf("ParseWindow(%q).String() = %q, want %q", c.in, got.String(), c.str)
		}
		// Text round trip, which is what configuration decoding uses.
		var w Window
		if err := w.UnmarshalText([]byte(got.String())); err != nil || w != c.want {
			t.Fatalf("round trip of %q: %v %v", c.in, w, err)
		}
	}
	for _, bad := range []string{"", "fortnightly", "30s", "rolling:", "-5h"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Fatalf("ParseWindow(%q) was accepted", bad)
		}
	}
}

func TestPeriodBoundariesAreUTC(t *testing.T) {
	// A Sunday, 23:30 UTC.
	now := time.Date(2026, 3, 8, 23, 30, 0, 0, time.UTC)

	if got := Daily.PeriodStart(now); !got.Equal(time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily start = %v", got)
	}
	if got := Daily.PeriodEnd(now); !got.Equal(time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily end = %v", got)
	}
	// ISO week: Sunday belongs to the week that started on Monday the 2nd.
	if got := Weekly.PeriodStart(now); !got.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly start = %v, want Monday 2026-03-02", got)
	}
	if got := Weekly.PeriodEnd(now); !got.Equal(time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly end = %v", got)
	}
	if got := Monthly.PeriodStart(now); !got.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monthly start = %v", got)
	}
	if got := Monthly.PeriodEnd(now); !got.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monthly end = %v", got)
	}
	// Monday is its own week start.
	mon := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	if got := Weekly.PeriodStart(mon); !got.Equal(mon) {
		t.Fatalf("Monday's week start = %v, want itself", got)
	}
	// Month lengths follow the calendar, not a fixed 30 days.
	feb := time.Date(2028, 2, 10, 0, 0, 0, 0, time.UTC) // leap year
	if d := Monthly.PeriodEnd(feb).Sub(Monthly.PeriodStart(feb)); d != 29*24*time.Hour {
		t.Fatalf("February 2028 is %v long, want 29 days", d)
	}
}

// TestPeriodsAreUnaffectedByDST feeds instants expressed in a zone that has a
// daylight-saving transition. Because every boundary is computed in UTC, the
// day either side of the transition is exactly 24 hours, which is what makes a
// daily quota mean the same thing all year.
func TestPeriodsAreUnaffectedByDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	// 2026-03-08 02:00 local is the spring-forward transition in New York.
	for _, local := range []time.Time{
		time.Date(2026, 3, 7, 12, 0, 0, 0, ny),
		time.Date(2026, 3, 8, 12, 0, 0, 0, ny),
		time.Date(2026, 11, 1, 12, 0, 0, 0, ny), // fall back
	} {
		start, end := Daily.PeriodStart(local), Daily.PeriodEnd(local)
		if d := end.Sub(start); d != 24*time.Hour {
			t.Fatalf("day containing %v is %v long", local, d)
		}
		if start.Location() != time.UTC || end.Location() != time.UTC {
			t.Fatalf("boundaries are not in UTC: %v %v", start, end)
		}
	}
}

func TestRollingPeriodIsAFixedGrid(t *testing.T) {
	w := Rolling(5 * time.Hour)
	a := w.PeriodStart(time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC))
	b := a.Add(5*time.Hour - time.Minute)
	// Two instants inside the same tumbling period share a start, so two nodes
	// key the same shared counter; the next one does not.
	if !w.PeriodStart(a).Equal(w.PeriodStart(b)) {
		t.Fatalf("same period, different starts: %v %v", w.PeriodStart(a), w.PeriodStart(b))
	}
	if w.PeriodStart(a.Add(5 * time.Hour)).Equal(a) {
		t.Fatal("the next period reuses the previous start")
	}
	if d := w.PeriodEnd(a).Sub(w.PeriodStart(a)); d != 5*time.Hour {
		t.Fatalf("rolling period is %v long", d)
	}
}

func TestMetricAndUsage(t *testing.T) {
	u := Usage{CostNanoUSD: 1500, TokensInput: 30, TokensOutput: 12, Requests: 1}
	cases := map[Metric]int64{
		MetricCostUSD:      1500,
		MetricTokensTotal:  42,
		MetricTokensInput:  30,
		MetricTokensOutput: 12,
		MetricRequests:     1,
	}
	for m, want := range cases {
		if got := u.Value(m); got != want {
			t.Fatalf("%v = %d, want %d", m, got, want)
		}
		parsed, err := ParseMetric(m.String())
		if err != nil || parsed != m {
			t.Fatalf("ParseMetric(%q) = %v, %v", m.String(), parsed, err)
		}
	}
	if _, err := ParseMetric("carbon"); err == nil {
		t.Fatal("ParseMetric accepted an unknown metric")
	}
	sum := u.Add(Usage{CostNanoUSD: 500, TokensInput: 1, Requests: 1})
	if sum.CostNanoUSD != 2000 || sum.TokensInput != 31 || sum.Requests != 2 {
		t.Fatalf("Add = %+v", sum)
	}
}

func TestNanoUSDConversion(t *testing.T) {
	if got := NanoUSD(3.0); got != 3_000_000_000 {
		t.Fatalf("NanoUSD(3.0) = %d", got)
	}
	if got := NanoUSD(0.000000001); got != 1 {
		t.Fatalf("NanoUSD(1e-9) = %d", got)
	}
	if got := USD(2_500_000_000); got != 2.5 {
		t.Fatalf("USD = %v", got)
	}
	// Exactness is the point: summing a million small charges in fixed point
	// gives the same answer as multiplying.
	var total int64
	for range 1_000_000 {
		total += NanoUSD(0.000001)
	}
	if total != 1_000_000_000 {
		t.Fatalf("accumulated %d nano-USD, want exactly 1 USD", total)
	}
}

// ------------------------------------------------------------- the ring ---

func TestRollingCounterAgesOutAcrossTheBoundary(t *testing.T) {
	start := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	c, err := newCounter(Rolling(time.Hour), start)
	if err != nil {
		t.Fatal(err)
	}
	// One unit a minute for an hour.
	for i := range 60 {
		c.add(start.Add(time.Duration(i)*time.Minute), 1)
	}
	if got := c.sum(start.Add(59 * time.Minute)); got != 60 {
		t.Fatalf("sum = %d, want 60", got)
	}
	// One minute later the oldest bucket has left the window.
	if got := c.sum(start.Add(60 * time.Minute)); got != 59 {
		t.Fatalf("sum after one minute = %d, want 59", got)
	}
	if got := c.sum(start.Add(90 * time.Minute)); got != 29 {
		t.Fatalf("sum after 30 minutes = %d, want 29", got)
	}
	// A gap longer than the window empties it in one step.
	if got := c.sum(start.Add(10 * time.Hour)); got != 0 {
		t.Fatalf("sum after a long gap = %d, want 0", got)
	}
	// And it keeps working afterwards.
	c.add(start.Add(10*time.Hour), 5)
	if got := c.sum(start.Add(10 * time.Hour)); got != 5 {
		t.Fatalf("sum after reuse = %d", got)
	}
}

func TestCalendarCounterResetsAtTheBoundary(t *testing.T) {
	start := time.Date(2026, 7, 28, 23, 0, 0, 0, time.UTC)
	c, err := newCounter(Daily, start)
	if err != nil {
		t.Fatal(err)
	}
	c.add(start, 10)
	if got := c.sum(start.Add(30 * time.Minute)); got != 10 {
		t.Fatalf("same day = %d", got)
	}
	next := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	if got := c.sum(next); got != 0 {
		t.Fatalf("new day = %d, want 0", got)
	}
	c.add(next, 3)
	if got := c.sum(next.Add(time.Hour)); got != 3 {
		t.Fatalf("after the boundary = %d", got)
	}
}

func TestCounterResetAt(t *testing.T) {
	start := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	c, err := newCounter(Rolling(time.Hour), start)
	if err != nil {
		t.Fatal(err)
	}
	// Ten units in the first minute, then nothing.
	c.add(start, 10)
	now := start.Add(5 * time.Minute)
	// With a limit of 10 the counter is exhausted, and it recovers when that
	// first minute ages out: one hour after it.
	got := c.resetAt(now, 10)
	want := start.Add(time.Hour).Truncate(time.Minute)
	if !got.Equal(want) {
		t.Fatalf("resetAt = %v, want %v", got, want)
	}
	// Spread the excess: only the oldest units need to age out.
	c2, _ := newCounter(Rolling(time.Hour), start)
	c2.add(start, 5)
	c2.add(start.Add(10*time.Minute), 5)
	if got := c2.resetAt(start.Add(20*time.Minute), 10); !got.Equal(start.Add(time.Hour)) {
		t.Fatalf("resetAt with spread usage = %v, want %v", got, start.Add(time.Hour))
	}
	// Under the limit, the reset is now.
	c3, _ := newCounter(Rolling(time.Hour), start)
	c3.add(start, 1)
	if got := c3.resetAt(start, 10); !got.Equal(start) {
		t.Fatalf("resetAt below the limit = %v, want now", got)
	}
}

func TestCounterRefusesAnAbsurdRollingWindow(t *testing.T) {
	now := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	if _, err := newCounter(Rolling(60*24*time.Hour), now); err == nil {
		t.Fatal("a 60-day rolling window was accepted; it should demand a calendar window")
	}
	if _, err := newCounter(Window{}, now); err == nil {
		t.Fatal("the zero window was accepted")
	}
}
