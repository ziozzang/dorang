package quota

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Metric is what a quota counts (DESIGN §6.1).
type Metric uint8

const (
	// MetricCostUSD counts money, in nano-USD.
	MetricCostUSD Metric = iota
	// MetricTokensTotal counts input plus output tokens.
	MetricTokensTotal
	// MetricTokensInput counts input tokens.
	MetricTokensInput
	// MetricTokensOutput counts output tokens.
	MetricTokensOutput
	// MetricRequests counts requests.
	MetricRequests

	numMetrics = int(MetricRequests) + 1
)

// String returns the configuration name of the metric.
func (m Metric) String() string {
	switch m {
	case MetricCostUSD:
		return "cost_usd"
	case MetricTokensTotal:
		return "tokens_total"
	case MetricTokensInput:
		return "tokens_input"
	case MetricTokensOutput:
		return "tokens_output"
	case MetricRequests:
		return "requests"
	}
	return "unknown"
}

// ParseMetric decodes a configured metric name.
func ParseMetric(s string) (Metric, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "cost_usd":
		return MetricCostUSD, nil
	case "tokens_total":
		return MetricTokensTotal, nil
	case "tokens_input":
		return MetricTokensInput, nil
	case "tokens_output":
		return MetricTokensOutput, nil
	case "requests":
		return MetricRequests, nil
	}
	return 0, fmt.Errorf("quota: unknown metric %q", s)
}

// Valid reports whether m is one of the defined metrics.
func (m Metric) Valid() bool { return int(m) < numMetrics }

// nanoPerUSD is the fixed-point scale for money. int64 nano-USD spans about
// ±9.2e9 USD, far beyond any budget, and makes window arithmetic exact
// addition rather than accumulated floating-point error (R1-11).
const nanoPerUSD = 1_000_000_000

// NanoUSD converts USD to the internal fixed-point unit, rounding half away
// from zero.
func NanoUSD(usd float64) int64 {
	if math.IsNaN(usd) {
		return 0
	}
	v := usd * nanoPerUSD
	switch {
	case v > math.MaxInt64:
		return math.MaxInt64
	case v < math.MinInt64:
		return math.MinInt64
	}
	return int64(math.Round(v))
}

// USD converts the internal fixed-point unit back to USD, for rendering.
func USD(nano int64) float64 { return float64(nano) / nanoPerUSD }

// Usage is one request's measured consumption.
type Usage struct {
	// CostNanoUSD is the cost in nano-USD.
	CostNanoUSD int64
	// TokensInput and TokensOutput are the measured token counts.
	TokensInput  int64
	TokensOutput int64
	// Requests is normally 1.
	Requests int64
}

// Value projects a usage onto one metric.
func (u Usage) Value(m Metric) int64 {
	switch m {
	case MetricCostUSD:
		return u.CostNanoUSD
	case MetricTokensTotal:
		return u.TokensInput + u.TokensOutput
	case MetricTokensInput:
		return u.TokensInput
	case MetricTokensOutput:
		return u.TokensOutput
	case MetricRequests:
		return u.Requests
	}
	return 0
}

// Add sums two usages, for aggregating the hops of one logical request.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		CostNanoUSD:  u.CostNanoUSD + o.CostNanoUSD,
		TokensInput:  u.TokensInput + o.TokensInput,
		TokensOutput: u.TokensOutput + o.TokensOutput,
		Requests:     u.Requests + o.Requests,
	}
}

// WindowKind distinguishes a rolling window from a calendar-aligned one.
type WindowKind uint8

const (
	// KindRolling is a window that always ends now: the last d of traffic.
	KindRolling WindowKind = iota
	// KindDaily is the UTC calendar day.
	KindDaily
	// KindWeekly is the ISO week, starting Monday 00:00 UTC.
	KindWeekly
	// KindMonthly is the UTC calendar month.
	KindMonthly
)

// Window is a quota's time scope (DESIGN §6.1): a rolling duration such as 5h
// or 1h, an explicit rolling:<dur>, or one of the calendar windows daily,
// weekly and monthly.
//
// Every boundary is computed in UTC. That is not a detail: a local-time daily
// window would be 23 or 25 hours long twice a year, and a weekly one would
// have an ambiguous start hour.
//
// The zero Window is invalid; build one with [Rolling], [ParseWindow], or the
// Daily/Weekly/Monthly variables.
type Window struct {
	kind WindowKind
	dur  time.Duration
}

// The calendar windows.
var (
	Daily   = Window{kind: KindDaily}
	Weekly  = Window{kind: KindWeekly}
	Monthly = Window{kind: KindMonthly}
)

// Rolling builds a rolling window of duration d.
func Rolling(d time.Duration) Window { return Window{kind: KindRolling, dur: d} }

// Kind returns the window's kind.
func (w Window) Kind() WindowKind { return w.kind }

// Duration returns the rolling duration, or 0 for a calendar window whose
// length is not constant.
func (w Window) Duration() time.Duration {
	if w.kind == KindRolling {
		return w.dur
	}
	return 0
}

// Valid reports whether the window is usable.
func (w Window) Valid() bool {
	if w.kind == KindRolling {
		return w.dur >= time.Minute
	}
	return w.kind <= KindMonthly
}

// String returns the configuration spelling of the window.
func (w Window) String() string {
	switch w.kind {
	case KindDaily:
		return "daily"
	case KindWeekly:
		return "weekly"
	case KindMonthly:
		return "monthly"
	}
	return formatDuration(w.dur)
}

// formatDuration prints whole hours and minutes the way configuration writes
// them (5h, not 5h0m0s).
func formatDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "invalid"
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return d.String()
}

// ParseWindow decodes a configured window: "daily", "weekly", "monthly",
// "rolling:<dur>", or any duration ("5h", "1h", "90m").
func ParseWindow(s string) (Window, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	switch t {
	case "daily":
		return Daily, nil
	case "weekly":
		return Weekly, nil
	case "monthly":
		return Monthly, nil
	}
	t = strings.TrimPrefix(t, "rolling:")
	d, err := time.ParseDuration(t)
	if err != nil {
		return Window{}, fmt.Errorf("quota: unknown window %q "+
			"(want daily, weekly, monthly, rolling:<dur> or a duration)", s)
	}
	if d < time.Minute {
		return Window{}, fmt.Errorf("quota: window %q is shorter than the one-minute bucket", s)
	}
	return Rolling(d), nil
}

// MarshalText implements encoding.TextMarshaler.
func (w Window) MarshalText() ([]byte, error) { return []byte(w.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (w *Window) UnmarshalText(b []byte) error {
	p, err := ParseWindow(string(b))
	if err != nil {
		return err
	}
	*w = p
	return nil
}

// PeriodStart returns the start of the period containing now.
//
// For calendar windows this is the calendar boundary in UTC. For a rolling
// window there is no natural period — the window always ends now — so this
// returns a tumbling grid aligned to a fixed epoch. Only the shared and leased
// coordinators use it, to key a shared counter that must reset in step on
// every node; local metering uses the ring, which is genuinely rolling.
func (w Window) PeriodStart(now time.Time) time.Time {
	u := now.UTC()
	switch w.kind {
	case KindDaily:
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	case KindWeekly:
		// ISO 8601: the week starts on Monday.
		off := (int(u.Weekday()) + 6) % 7
		d := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
		return d.AddDate(0, 0, -off)
	case KindMonthly:
		return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return u.Truncate(w.dur)
}

// PeriodEnd returns the instant the period containing now ends, which is also
// the instant the next one starts.
func (w Window) PeriodEnd(now time.Time) time.Time {
	start := w.PeriodStart(now)
	switch w.kind {
	case KindDaily:
		return start.AddDate(0, 0, 1)
	case KindWeekly:
		return start.AddDate(0, 0, 7)
	case KindMonthly:
		return start.AddDate(0, 1, 0)
	}
	return start.Add(w.dur)
}

// buckets is how many one-minute buckets a rolling window needs.
func (w Window) buckets() int {
	if w.kind != KindRolling {
		return 0
	}
	n := int((w.dur + time.Minute - 1) / time.Minute)
	if n < 1 {
		n = 1
	}
	return n
}

// maxBuckets caps the ring at 31 days of minutes. A longer rolling window
// would cost more memory per counter than it is worth; use a calendar window,
// which needs no ring at all.
const maxBuckets = 31 * 24 * 60
