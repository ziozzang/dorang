package quota

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OnExhaust is what happens when a rule's limit is reached (DESIGN §6.1).
type OnExhaust uint8

const (
	// Cooldown steps the credential aside until the window resets. This is
	// what moves traffic to the next credential; it is the default because it
	// is the only option that keeps serving.
	Cooldown OnExhaust = iota
	// Disable takes the credential out of service until it is re-enabled.
	Disable
	// Passthrough keeps serving and only records that the limit was passed.
	Passthrough
)

// String returns the configuration name.
func (o OnExhaust) String() string {
	switch o {
	case Cooldown:
		return "cooldown"
	case Disable:
		return "disable"
	case Passthrough:
		return "passthrough"
	}
	return "unknown"
}

// ParseOnExhaust decodes a configured on_exhaust value.
func ParseOnExhaust(s string) (OnExhaust, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "cooldown":
		return Cooldown, nil
	case "disable":
		return Disable, nil
	case "passthrough":
		return Passthrough, nil
	}
	return 0, fmt.Errorf("quota: unknown on_exhaust %q", s)
}

// Rule is one configured quota: `{ window: 5h, metric: cost_usd, limit: 3.0,
// on_exhaust: cooldown }`.
type Rule struct {
	Window    Window
	Metric    Metric
	Limit     int64
	OnExhaust OnExhaust
	// Resets declares that the unused remainder of the window is discarded when
	// it rolls over, which is what makes the allowance use-it-or-lose-it and
	// gives it an urgency (DESIGN §7.5a(c), [Meter.Urgency]).
	//
	// It is declared, never inferred. A resetting subscription window and a
	// rolling prepaid balance produce identical numbers, and treating the
	// second as the first spends money early to buy nothing.
	Resets bool
}

// String renders the rule the way configuration writes it.
func (r Rule) String() string {
	if r.Resets {
		return fmt.Sprintf("{window: %s, metric: %s, limit: %d, on_exhaust: %s, resets: true}",
			r.Window, r.Metric, r.Limit, r.OnExhaust)
	}
	return fmt.Sprintf("{window: %s, metric: %s, limit: %d, on_exhaust: %s}",
		r.Window, r.Metric, r.Limit, r.OnExhaust)
}

// Validate checks a rule's shape.
func (r Rule) Validate() error {
	if !r.Window.Valid() {
		return fmt.Errorf("quota: rule %s has an invalid window", r)
	}
	if !r.Metric.Valid() {
		return fmt.Errorf("quota: rule %s has an invalid metric", r)
	}
	if r.Limit <= 0 {
		return fmt.Errorf("quota: rule %s has a non-positive limit", r)
	}
	return nil
}

// State is a meter's admission state.
type State uint8

const (
	// StateOK: within every limit.
	StateOK State = iota
	// StateCooldown: a limit was reached and the credential is standing aside
	// until the window resets.
	StateCooldown
	// StateDisabled: a limit was reached under on_exhaust: disable.
	StateDisabled
	// StateOverPassthrough: a limit was passed under on_exhaust: passthrough.
	// Traffic is still admitted.
	StateOverPassthrough
)

// String returns the state name.
func (s State) String() string {
	switch s {
	case StateOK:
		return "ok"
	case StateCooldown:
		return "cooldown"
	case StateDisabled:
		return "disabled"
	case StateOverPassthrough:
		return "over_passthrough"
	}
	return "unknown"
}

// Decision is the answer to "may this credential serve a request now".
type Decision struct {
	// Allow is the answer. Everything else is why.
	Allow bool
	// State is the meter's state after the check.
	State State
	// Rule is the rule that tripped, when one did.
	Rule Rule
	// Used and Limit are the tripped rule's figures. Used is the effective
	// figure: combined with the provider's when one is tracked.
	Used  int64
	Limit int64
	// ResetAt is when the credential is expected to be serviceable again. It
	// is zero under Disable, which does not recover on its own.
	ResetAt time.Time
	// ProviderStale is how old the provider's figure was, when one was used.
	ProviderStale time.Duration
	// FromProvider reports whether the provider's figure took part in Used.
	FromProvider bool
}

// Meter meters one subject — normally one credential, the unit quotas actually
// attach to (DESIGN §3) — against a set of rules.
//
// A Meter is safe for concurrent use. The critical section is a handful of
// integer operations over the rings and holds no I/O.
type Meter struct {
	mu       sync.Mutex
	rules    []Rule
	counters []*counter
	state    State
	until    time.Time
	tripped  Rule
	tracker  *Tracker
	now      func() time.Time

	// cum is the process-lifetime total per metric. It is read by the
	// provider tracker to compute the local delta since a poll, so it is
	// atomic rather than mutex-guarded: the tracker must never need this
	// meter's lock, or a poll could deadlock against a record.
	cum [numMetrics]atomic.Int64
}

// MeterConfig configures a Meter.
type MeterConfig struct {
	// Rules is the quota set. An empty set meters usage and never refuses.
	Rules []Rule
	// Now overrides the clock.
	Now func() time.Time
}

// NewMeter builds a meter.
func NewMeter(cfg MeterConfig) (*Meter, error) {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	m := &Meter{rules: append([]Rule(nil), cfg.Rules...), now: now}
	t := now()
	for _, r := range m.rules {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		c, err := newCounter(r.Window, t)
		if err != nil {
			return nil, err
		}
		m.counters = append(m.counters, c)
	}
	return m, nil
}

// Rules returns the configured rules.
func (m *Meter) Rules() []Rule { return append([]Rule(nil), m.rules...) }

// AttachTracker makes the meter consult a provider's reported usage as well as
// its own (DESIGN §6.2). Pass nil to detach.
func (m *Meter) AttachTracker(t *Tracker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tracker = t
}

// Cumulative returns the process-lifetime total for a metric. It is the local
// side of the provider combination, and is deliberately monotone: a restart
// resets it, and the next poll re-baselines.
func (m *Meter) Cumulative(metric Metric) int64 {
	if !metric.Valid() {
		return 0
	}
	return m.cum[metric].Load()
}

// Record adds one request's measured usage.
//
// A negative value is ignored rather than subtracted. Consumption is the only
// thing a meter measures: every metric it carries — cost, tokens, requests — is
// a quantity a request USED, and none of them can be un-used. Subtracting one
// would not merely mis-count, it would MANUFACTURE allowance, handing back
// window capacity that no reset granted, and it would do so on the one path
// that decides whether a credential may keep serving.
//
// Nothing upstream can produce one today ([pricing.Cost.TotalNano] is floored at
// zero, and token counts and request counts are measured), so this costs one
// comparison that already existed and defends the invariant at the place that
// depends on it rather than at each caller that might one day feed it.
func (m *Meter) Record(now time.Time, u Usage) {
	for i := range numMetrics {
		if v := u.Value(Metric(i)); v > 0 {
			m.cum[i].Add(v)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.rules {
		if v := u.Value(r.Metric); v > 0 {
			m.counters[i].add(now, v)
		}
	}
}

// Check reports whether the subject may serve a request at now, and applies
// each rule's on_exhaust.
//
// Cooldown recovers on its own: once now reaches the reset instant the state
// clears and the rules are re-evaluated, which is what lets traffic come back
// to a credential that stepped aside. Disable does not recover; it waits for
// [Meter.Enable].
func (m *Meter) Check(now time.Time) Decision {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == StateDisabled {
		return Decision{State: StateDisabled, Rule: m.tripped, Limit: m.tripped.Limit}
	}
	if m.state == StateCooldown {
		if now.Before(m.until) {
			return Decision{State: StateCooldown, Rule: m.tripped, Limit: m.tripped.Limit, ResetAt: m.until}
		}
		m.state, m.until, m.tripped = StateOK, time.Time{}, Rule{}
	}

	d := Decision{Allow: true, State: StateOK}
	for i, r := range m.rules {
		used, stale, fromProvider := m.effectiveUsed(r, i, now)
		if used < r.Limit {
			continue
		}
		switch r.OnExhaust {
		case Passthrough:
			// Still admitted; the fact is recorded, not enforced.
			if d.State == StateOK {
				d.State, d.Rule, d.Used, d.Limit = StateOverPassthrough, r, used, r.Limit
				d.ProviderStale, d.FromProvider = stale, fromProvider
			}
		case Disable:
			m.state, m.tripped, m.until = StateDisabled, r, time.Time{}
			return Decision{State: StateDisabled, Rule: r, Used: used, Limit: r.Limit,
				ProviderStale: stale, FromProvider: fromProvider}
		default: // Cooldown
			reset := m.resetFor(r, i, now, fromProvider)
			m.state, m.tripped, m.until = StateCooldown, r, reset
			return Decision{State: StateCooldown, Rule: r, Used: used, Limit: r.Limit,
				ResetAt: reset, ProviderStale: stale, FromProvider: fromProvider}
		}
	}
	return d
}

// resetFor picks the instant a tripped rule is expected to recover. A
// provider-reported reset instant wins when it is in the future, because the
// provider's own window is what actually gates the credential.
func (m *Meter) resetFor(r Rule, i int, now time.Time, fromProvider bool) time.Time {
	local := m.counters[i].resetAt(now, r.Limit)
	if fromProvider && m.tracker != nil {
		if pr, ok := m.tracker.ResetAt(r.Window, r.Metric); ok && pr.After(now) && pr.After(local) {
			return pr
		}
	}
	return local
}

// effectiveUsed combines the local counter with the provider's figure, when
// one is tracked (DESIGN §6.2).
func (m *Meter) effectiveUsed(r Rule, i int, now time.Time) (used int64, stale time.Duration, fromProvider bool) {
	local := m.counters[i].sum(now)
	if m.tracker == nil {
		return local, 0, false
	}
	pu, _, ok := m.tracker.Effective(r.Window, r.Metric, now)
	if !ok {
		return local, 0, false
	}
	st, _ := m.tracker.Staleness(now)
	// The provider's figure covers traffic that never passed through dorang,
	// so it can only ever be larger than what this process metered. Taking the
	// larger of the two keeps a local burst visible even when the provider's
	// figure is stale, and keeps out-of-band usage visible when it is not.
	if pu > local {
		return pu, st, true
	}
	return local, st, false
}

// Used returns the effective used figure for one rule at now.
func (m *Meter) Used(now time.Time, r Rule) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, rr := range m.rules {
		if rr.Window == r.Window && rr.Metric == r.Metric {
			u, _, _ := m.effectiveUsed(rr, i, now)
			return u
		}
	}
	return 0
}

// Local returns the locally metered figure for one rule at now, ignoring any
// provider report.
func (m *Meter) Local(now time.Time, r Rule) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, rr := range m.rules {
		if rr.Window == r.Window && rr.Metric == r.Metric {
			return m.counters[i].sum(now)
		}
	}
	return 0
}

// State returns the meter's state at now, letting a finished cooldown lapse.
func (m *Meter) State(now time.Time) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == StateCooldown && !now.Before(m.until) {
		m.state, m.until, m.tripped = StateOK, time.Time{}, Rule{}
	}
	return m.state
}

// Enable clears a disable or a cooldown. It is the administrative path back
// into service.
func (m *Meter) Enable() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state, m.until, m.tripped = StateOK, time.Time{}, Rule{}
}

// Reset clears every counter and the state. It exists for tests and for an
// administrative reset of a window.
func (m *Meter) Reset(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.counters {
		c.reset(now)
	}
	m.state, m.until, m.tripped = StateOK, time.Time{}, Rule{}
}

// MaxViewRules bounds [View]. Four windows times five metrics is the whole
// expressible space and no real configuration uses it; a credential with more
// rules than this reports its first MaxViewRules and sets [View.Truncated],
// because a silently short answer is worse than a flagged one.
const MaxViewRules = 12

// RuleUsage is one rule's state at an instant.
type RuleUsage struct {
	Window  Window
	Metric  Metric
	Used    int64
	Limit   int64
	ResetAt time.Time
	// FromProvider reports that the provider's own figure took part in Used,
	// so this number includes usage that never went through this gateway.
	FromProvider bool
}

// View is a credential's whole quota state, reported without allocating.
//
// [Decision] answers "may this serve" and therefore carries only the rule that
// TRIPPED — on the allow path it carries nothing at all, which is why the
// figures the enforcement point already computed were being discarded on every
// successful request. This is the same numbers, reported rather than thrown
// away, and it is what `x-ratelimit-*` and `x-dorang-quota-<window>-used-pct`
// are rendered from: the headers report what enforces, not a second count kept
// beside it that could drift.
//
// It is a value with a fixed array so a caller can keep one on a pooled request
// and fill it per request without touching the heap.
type View struct {
	Rules     [MaxViewRules]RuleUsage
	N         int
	Truncated bool
}

// Fill writes m's state at now into dst, replacing whatever it held.
//
// A meter with no rules leaves dst empty, which is the honest report for an
// unmetered credential: it has no limit, and a limit of zero would say the
// opposite.
func (m *Meter) Fill(now time.Time, dst *View) {
	if dst == nil {
		return
	}
	dst.N, dst.Truncated = 0, false

	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.rules {
		if dst.N >= MaxViewRules {
			dst.Truncated = true
			return
		}
		used, _, fromProvider := m.effectiveUsed(r, i, now)
		dst.Rules[dst.N] = RuleUsage{
			Window:       r.Window,
			Metric:       r.Metric,
			Used:         used,
			Limit:        r.Limit,
			ResetAt:      r.Window.PeriodEnd(now),
			FromProvider: fromProvider,
		}
		dst.N++
	}
}
