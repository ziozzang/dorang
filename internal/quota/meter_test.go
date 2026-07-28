package quota

import (
	"testing"
	"time"
)

var base = time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)

func newTestMeter(t *testing.T, rules ...Rule) *Meter {
	t.Helper()
	m, err := NewMeter(MeterConfig{Rules: rules, Now: func() time.Time { return base }})
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	return m
}

func TestMeterAdmitsUntilTheLimit(t *testing.T) {
	m := newTestMeter(t, Rule{Window: Rolling(time.Hour), Metric: MetricRequests, Limit: 3})
	for i := range 3 {
		if d := m.Check(base); !d.Allow {
			t.Fatalf("request %d refused: %+v", i, d)
		}
		m.Record(base, Usage{Requests: 1})
	}
	d := m.Check(base)
	if d.Allow {
		t.Fatal("the fourth request was admitted past a limit of 3")
	}
	if d.State != StateCooldown || d.Used != 3 || d.Limit != 3 {
		t.Fatalf("decision = %+v", d)
	}
}

// TestCooldownRecoversAfterTheWindowResets is the behavior that moves traffic
// to the next credential and then brings it back.
func TestCooldownRecoversAfterTheWindowResets(t *testing.T) {
	r := Rule{Window: Rolling(time.Hour), Metric: MetricCostUSD, Limit: NanoUSD(3), OnExhaust: Cooldown}
	m := newTestMeter(t, r)

	m.Record(base, Usage{CostNanoUSD: NanoUSD(3)})
	d := m.Check(base)
	if d.Allow || d.State != StateCooldown {
		t.Fatalf("expected cooldown, got %+v", d)
	}
	if !d.ResetAt.After(base) {
		t.Fatalf("cooldown has no future reset: %v", d.ResetAt)
	}
	// Still down halfway through the window.
	if d := m.Check(base.Add(30 * time.Minute)); d.Allow {
		t.Fatal("cooldown ended early")
	}
	// And back once the spend has aged out.
	after := d.ResetAt.Add(time.Second)
	if d := m.Check(after); !d.Allow || d.State != StateOK {
		t.Fatalf("cooldown did not recover: %+v", d)
	}
	if got := m.State(after); got != StateOK {
		t.Fatalf("state after recovery = %v", got)
	}
	// The credential serves again.
	m.Record(after, Usage{CostNanoUSD: NanoUSD(1)})
	if d := m.Check(after); !d.Allow {
		t.Fatalf("refused after recovery: %+v", d)
	}
}

func TestOnExhaustDisableDoesNotRecoverOnItsOwn(t *testing.T) {
	r := Rule{Window: Rolling(time.Hour), Metric: MetricRequests, Limit: 1, OnExhaust: Disable}
	m := newTestMeter(t, r)
	m.Record(base, Usage{Requests: 1})
	if d := m.Check(base); d.Allow || d.State != StateDisabled {
		t.Fatalf("expected disabled, got %+v", d)
	}
	// A day later it is still disabled: nothing but an operator brings it back.
	if d := m.Check(base.Add(24 * time.Hour)); d.Allow {
		t.Fatal("a disabled credential came back on its own")
	}
	m.Enable()
	if d := m.Check(base.Add(24 * time.Hour)); !d.Allow {
		t.Fatalf("Enable did not restore service: %+v", d)
	}
}

func TestOnExhaustPassthroughKeepsServing(t *testing.T) {
	r := Rule{Window: Daily, Metric: MetricTokensTotal, Limit: 100, OnExhaust: Passthrough}
	m := newTestMeter(t, r)
	m.Record(base, Usage{TokensInput: 60, TokensOutput: 60})
	d := m.Check(base)
	if !d.Allow {
		t.Fatal("passthrough refused a request")
	}
	if d.State != StateOverPassthrough || d.Used != 120 {
		t.Fatalf("decision = %+v", d)
	}
}

func TestSeveralRulesTheMostRestrictiveWins(t *testing.T) {
	hourly := Rule{Window: Rolling(time.Hour), Metric: MetricRequests, Limit: 100}
	daily := Rule{Window: Daily, Metric: MetricRequests, Limit: 2}
	m := newTestMeter(t, hourly, daily)
	m.Record(base, Usage{Requests: 2})
	d := m.Check(base)
	if d.Allow {
		t.Fatal("the daily limit was not applied")
	}
	if d.Rule.Window != Daily {
		t.Fatalf("tripped on %v, want the daily rule", d.Rule.Window)
	}
}

func TestWindowsAreMeteredIndependently(t *testing.T) {
	hourly := Rule{Window: Rolling(time.Hour), Metric: MetricRequests, Limit: 5}
	daily := Rule{Window: Daily, Metric: MetricRequests, Limit: 1000}
	m := newTestMeter(t, hourly, daily)
	for i := range 5 {
		m.Record(base.Add(time.Duration(i)*time.Minute), Usage{Requests: 1})
	}
	// Two hours later the rolling window is empty but the day is not.
	later := base.Add(2 * time.Hour)
	if got := m.Local(later, hourly); got != 0 {
		t.Fatalf("rolling window still holds %d", got)
	}
	if got := m.Local(later, daily); got != 5 {
		t.Fatalf("daily window holds %d, want 5", got)
	}
	if d := m.Check(later); !d.Allow {
		t.Fatalf("refused after the rolling window emptied: %+v", d)
	}
}

func TestMeterRejectsBadRules(t *testing.T) {
	bad := []Rule{
		{Window: Window{}, Metric: MetricRequests, Limit: 1},
		{Window: Daily, Metric: Metric(99), Limit: 1},
		{Window: Daily, Metric: MetricRequests, Limit: 0},
	}
	for _, r := range bad {
		if _, err := NewMeter(MeterConfig{Rules: []Rule{r}}); err == nil {
			t.Fatalf("NewMeter accepted %v", r)
		}
	}
}

func TestOnExhaustParsing(t *testing.T) {
	for s, want := range map[string]OnExhaust{
		"":            Cooldown,
		"cooldown":    Cooldown,
		"disable":     Disable,
		"passthrough": Passthrough,
	} {
		got, err := ParseOnExhaust(s)
		if err != nil || got != want {
			t.Fatalf("ParseOnExhaust(%q) = %v, %v", s, got, err)
		}
	}
	if _, err := ParseOnExhaust("explode"); err == nil {
		t.Fatal("ParseOnExhaust accepted an unknown value")
	}
}

func TestMeterIsConcurrencySafe(t *testing.T) {
	m := newTestMeter(t, Rule{Window: Rolling(time.Hour), Metric: MetricRequests, Limit: 1_000_000})
	done := make(chan struct{})
	for range 8 {
		go func() {
			for range 500 {
				m.Check(base)
				m.Record(base, Usage{Requests: 1})
			}
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}
	if got := m.Local(base, Rule{Window: Rolling(time.Hour), Metric: MetricRequests}); got != 4000 {
		t.Fatalf("recorded %d requests, want 4000", got)
	}
	if got := m.Cumulative(MetricRequests); got != 4000 {
		t.Fatalf("cumulative = %d", got)
	}
}
