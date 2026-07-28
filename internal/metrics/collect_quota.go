package metrics

import (
	"time"

	"github.com/ziozzang/dorang/internal/quota"
)

// QuotaSource is one credential's locally metered quota (DESIGN §6.1) combined
// with whatever the provider has reported (§6.2).
type QuotaSource interface {
	Rules() []quota.Rule
	Used(now time.Time, r quota.Rule) int64
	Local(now time.Time, r quota.Rule) int64
	Allowance(now time.Time, r quota.Rule) (quota.Allowance, bool)
	State(now time.Time) quota.State
}

// quotaEntry pairs a credential with its meter and the rules it was built with.
//
// The rules are captured once. quota.Meter.Rules copies its slice on every
// call, and a scrape that allocated one slice per credential per rule family
// would allocate more than the scrape it produces.
type quotaEntry struct {
	credential string
	src        QuotaSource
	rules      []quota.Rule
}

// QuotaCollector renders provider-reported quota consumption and the expiring-
// quota urgency of DESIGN §7.5a(c).
//
// The `_percent` spelling is DESIGN §12.3's and the value really is a
// percentage in [0,100], which is the whole point: VLLM.md §3.1 records a
// backend gauge named `kv_cache_usage_perc` whose value is a 0–1 fraction, and
// whose own documentation string admits "1 means 100 percent usage". A name and
// a unit that disagree produce a dashboard that is wrong by a factor of a
// hundred and looks entirely plausible.
type QuotaCollector struct {
	entries []quotaEntry
	now     func() time.Time
	max     int
	f       folder
}

// NewQuotaCollector builds the collector over the credential meters.
func NewQuotaCollector(now func() time.Time, maxCredentials int) *QuotaCollector {
	if now == nil {
		now = time.Now
	}
	if maxCredentials <= 0 {
		maxCredentials = DefaultMaxCredentialSeries
	}
	return &QuotaCollector{now: now, max: maxCredentials,
		f: folder{family: "dorang_provider_quota_used_percent"}}
}

// Track registers one credential's meter. A nil meter is ignored, which is what
// makes "this credential has no quota rules" absent rather than zero.
func (c *QuotaCollector) Track(credential string, src QuotaSource) {
	if src == nil {
		return
	}
	rules := src.Rules()
	if len(rules) == 0 {
		return
	}
	c.entries = append(c.entries, quotaEntry{credential: credential, src: src, rules: rules})
	sortSlice(c.entries, func(a, b quotaEntry) bool { return a.credential < b.credential })
}

// Len is the number of tracked credentials.
func (c *QuotaCollector) Len() int { return len(c.entries) }

// CollectorName implements [Collector].
func (c *QuotaCollector) CollectorName() string { return "quota" }

func (c *QuotaCollector) folders() []*folder { return []*folder{&c.f} }

// quotaStates is the state set, emitted whole so a transition leaves nothing
// stale behind.
var quotaStates = [...]quota.State{
	quota.StateOK, quota.StateCooldown, quota.StateDisabled,
}

// Collect implements [Collector].
func (c *QuotaCollector) Collect(w *Writer) {
	if len(c.entries) == 0 {
		return
	}
	now := c.now()
	// A quota percentage does not add up across credentials, so the tail past
	// the cap is dropped and counted rather than summed into a meaningless
	// aggregate.
	entries := c.entries
	if len(entries) > c.max {
		for range entries[c.max:] {
			c.f.fold()
		}
		entries = entries[:c.max]
	}

	w.Metric("dorang_provider_quota_used_percent", Gauge,
		"Consumption of a credential's quota window, in [0,100] — a percentage, as the "+
			"name says. Combines the local meter with the provider's own report "+
			"(DESIGN §6.2). A rule with no limit is absent: there is no percentage of "+
			"an absent ceiling.")
	for _, e := range entries {
		for _, r := range e.rules {
			if r.Limit <= 0 {
				continue
			}
			used := e.src.Used(now, r)
			w.Label("credential", e.credential)
			w.Label("window", r.Window.String())
			w.Label("metric", r.Metric.String())
			w.Float(clamp(float64(used)*100/float64(r.Limit), 0, 100))
		}
	}

	w.Metric("dorang_provider_quota_used", Gauge,
		"Units consumed in a credential's quota window, in the metric's own units.")
	for _, e := range entries {
		for _, r := range e.rules {
			w.Label("credential", e.credential)
			w.Label("window", r.Window.String())
			w.Label("metric", r.Metric.String())
			w.Int(e.src.Used(now, r))
		}
	}

	w.Metric("dorang_provider_quota_limit", Gauge,
		"A quota rule's ceiling. A rule with no limit emits nothing rather than zero.")
	for _, e := range entries {
		for _, r := range e.rules {
			if r.Limit <= 0 {
				continue
			}
			w.Label("credential", e.credential)
			w.Label("window", r.Window.String())
			w.Label("metric", r.Metric.String())
			w.Int(r.Limit)
		}
	}

	w.Metric("dorang_quota_urgency", Gauge,
		"DESIGN §7.5a(c)'s use-it-or-lose-it score: unused fraction over remaining "+
			"fraction of the window, undamped and unjittered. Emitted only for a rule "+
			"declared `resets: true` and with a known reset instant — a rolling window "+
			"whose phase no provider has reported scores nothing, and reporting a zero "+
			"there would be indistinguishable from a fully consumed allowance.")
	for _, e := range entries {
		for _, r := range e.rules {
			a, ok := e.src.Allowance(now, r)
			if !ok || !a.Resets || a.ResetAt.IsZero() {
				continue
			}
			w.Label("credential", e.credential)
			w.Label("window", r.Window.String())
			w.Label("metric", r.Metric.String())
			w.Float(a.Urgency(now))
		}
	}

	w.Metric("dorang_quota_window_reset_seconds", Gauge,
		"Seconds until a resetting window discards its unused remainder. Absent when the "+
			"reset instant is unknown, which for a rolling window is the normal case "+
			"until the provider reports one (DESIGN §7.5a(c) correction 1).")
	for _, e := range entries {
		for _, r := range e.rules {
			a, ok := e.src.Allowance(now, r)
			if !ok || !a.Resets || a.ResetAt.IsZero() {
				continue
			}
			w.Label("credential", e.credential)
			w.Label("window", r.Window.String())
			w.Label("metric", r.Metric.String())
			w.Float(a.ResetAt.Sub(now).Seconds())
		}
	}

	w.Metric("dorang_quota_state", Gauge,
		"1 against a credential's quota state, 0 against the others (DESIGN §6.1).")
	for _, e := range entries {
		st := e.src.State(now)
		for _, s := range quotaStates {
			w.Label("credential", e.credential)
			w.Label("state", s.String())
			w.Bool(st == s)
		}
	}
}

func clamp(v, lo, hi float64) float64 {
	switch {
	case v != v:
		return lo
	case v < lo:
		return lo
	case v > hi:
		return hi
	}
	return v
}

var _ QuotaSource = (*quota.Meter)(nil)
