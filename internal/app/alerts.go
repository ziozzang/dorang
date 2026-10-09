package app

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/alerts"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/notify"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/store"
)

// buildAlerts assembles the remaining_low / quota_exhausted state machine. It
// is built once, like the notifier it speaks through, and is nil when there is
// no notifier.
func buildAlerts(cfg *config.Config, n *notify.Notifier, st *store.Store,
	logf func(string, ...any), now func() time.Time) *alerts.Engine {

	rl := cfg.Notifications.RemainingLow
	o := alerts.Options{
		Notifier: n,
		Percent:  rl.Percent,
		Remind:   rl.Remind.Duration(),
		// A transport that has not delivered within five sweeps is retried.
		RetryAfter: 5 * rl.Interval.Duration(),
		Now:        now,
		Logf:       logf,
	}
	if st != nil {
		o.Store = alertStore{st}
	}
	if !rl.BalanceFloorUSD.IsZero() {
		if v, err := decimalNano(rl.BalanceFloorUSD.String()); err == nil {
			o.FloorNano = v
		}
	}
	return alerts.New(o)
}

// alertStore adapts internal/store to internal/alerts.
type alertStore struct{ s *store.Store }

func (a alertStore) AlertRaise(ctx context.Context, key, event string, now, remindBefore time.Time,
	pending, payload string) (bool, int64, error) {
	return a.s.AlertRaise(ctx, key, event, now, remindBefore, pending, payload)
}

func (a alertStore) AlertClear(ctx context.Context, key string, now time.Time) (bool, error) {
	return a.s.AlertClear(ctx, key, now)
}

func (a alertStore) AlertDelivered(ctx context.Context, key string, seq int64, driver string, now time.Time) error {
	return a.s.AlertDelivered(ctx, key, seq, driver, now)
}

func (a alertStore) AlertsPending(ctx context.Context, before time.Time, limit int) ([]alerts.Pending, error) {
	ps, err := a.s.AlertsPending(ctx, before, limit)
	out := make([]alerts.Pending, len(ps))
	for i, p := range ps {
		out[i] = alerts.Pending(p)
	}
	return out, err
}

func (a alertStore) AlertRetryClaim(ctx context.Context, key string, seq int64, last, now time.Time) (bool, error) {
	return a.s.AlertRetryClaim(ctx, key, seq, last, now)
}

// evaluateProbes turns every probed credential's last good snapshot into
// remaining_low observations. It runs right after each poll, so a figure is
// judged as soon as it is read; a snapshot older than three poll intervals is
// skipped rather than judged stale.
func (a *App) evaluateProbes(ctx context.Context) {
	eng := a.Alerts
	ps := a.probes.Load()
	if eng == nil || ps == nil {
		return
	}
	now := a.now()
	threshold := alerts.Percent(eng.Percent())
	for _, pc := range ps.creds {
		snap, ok := pc.prober.Snapshot(pc.credential)
		if !ok || now.Sub(snap.FetchedAt) > 3*ps.interval {
			continue
		}
		subj := notify.Subject{Kind: "credential", ID: pc.credential}
		for _, w := range snap.Windows {
			var left, limit float64
			switch {
			case w.Limit > 0:
				left, limit = float64(w.Limit-w.Used), float64(w.Limit)
			case w.Known:
				left, limit = 100-w.UsedPercent, 100
			default:
				continue
			}
			low, pct := eng.LowFraction(left, limit)
			fields := []notify.Field{
				{Name: "source", Value: "provider window"},
				{Name: "provider", Value: pc.provider},
				{Name: "credential_id", Value: pc.credential},
				{Name: "label", Value: w.Label},
				{Name: "percent_left", Value: alerts.Percent(pct)},
				{Name: "threshold", Value: threshold},
			}
			if w.Limit > 0 {
				fields = append(fields, amountFields(w.Metric, w.Limit-w.Used, w.Limit)...)
			}
			if !w.ResetAt.IsZero() {
				fields = append(fields, notify.Field{Name: "resets_at", Value: w.ResetAt.UTC().Format(time.RFC3339)})
			}
			eng.Observe(ctx, alerts.Condition{
				Event: notify.EventRemainingLow, Subject: subj, Low: low, Fields: fields,
				Key: "remaining_low|window|" + pc.provider + "|" + pc.credential + "|" + w.Label,
			})
		}
		for _, b := range snap.Balances {
			low, pct, hasPct := eng.BalanceLow(b.RemainingNano, b.TotalNano)
			if b.Currency != "USD" {
				// The floor is in USD; a balance in another currency is judged by
				// its share alone.
				low = hasPct && pct <= eng.Percent()
			}
			fields := []notify.Field{
				{Name: "source", Value: "provider balance"},
				{Name: "provider", Value: pc.provider},
				{Name: "credential_id", Value: pc.credential},
				{Name: "remaining", Value: alerts.USD(b.RemainingNano)},
				{Name: "unit", Value: b.Currency},
				{Name: "threshold", Value: threshold},
			}
			if hasPct {
				fields = append(fields,
					notify.Field{Name: "limit", Value: alerts.USD(b.TotalNano)},
					notify.Field{Name: "percent_left", Value: alerts.Percent(pct)})
			}
			eng.Observe(ctx, alerts.Condition{
				Event: notify.EventRemainingLow, Subject: subj, Low: low, Fields: fields,
				Key: "remaining_low|balance|" + pc.provider + "|" + pc.credential + "|" + b.Currency,
			})
		}
	}
}

func amountFields(m quota.Metric, left, limit int64) []notify.Field {
	if m == quota.MetricCostUSD {
		return []notify.Field{
			{Name: "remaining", Value: alerts.USD(left)},
			{Name: "limit", Value: alerts.USD(limit)},
			{Name: "unit", Value: "USD"},
		}
	}
	return []notify.Field{
		{Name: "remaining", Value: itoa64(left)},
		{Name: "limit", Value: itoa64(limit)},
		{Name: "unit", Value: m.String()},
	}
}

// planExhausted recognizes an upstream refusal that says a plan or account
// allowance is used up, as opposed to an ordinary rate limit. It is the only
// signal Ollama Cloud and the Qwen token plan give: neither publishes what is
// left, and both answer 429 with the reason in words.
//
// Measured 2026-10-09 on Ollama Cloud: "You reached your Pro 5-hour limit. Max
// is $100/month …". The other spellings are the common vendor phrasings of the
// same fact (OpenAI's insufficient_quota, "usage limit", "quota exceeded").
var planExhausted = regexp.MustCompile(`(?i)(reached (your|the) [^.]{0,40}limit|usage limit|` +
	`quota (exceeded|exhausted)|exceeded your current quota|insufficient[ _](quota|balance|credits)|` +
	`out of credits)`)

// observePlan raises quota_exhausted when an upstream says a credential's plan
// is used up, and clears it on that credential's next success. The success
// path costs one atomic load unless some condition is known to be low.
func (d *dispatcher) observePlan(ctx context.Context, dec *router.Decision, res result) {
	eng := d.alerts
	if eng == nil || dec == nil || dec.Credential == "" {
		return
	}
	if res.err == nil {
		if eng.AnyLow() {
			eng.Observe(ctx, alerts.Condition{Event: notify.EventQuotaExhausted,
				Key: "quota_exhausted|" + dec.Provider + "|" + dec.Credential})
		}
		return
	}
	var se *server.Error
	if !errors.As(res.err, &se) {
		return
	}
	reason := se.NativeMessage
	if reason == "" {
		reason = se.Message
	}
	if res.outcome.Status != 429 || !planExhausted.MatchString(reason) {
		return
	}
	if len(reason) > 240 {
		reason = reason[:240] + "…"
	}
	fields := []notify.Field{
		{Name: "subject", Value: "credential:" + dec.Credential},
		{Name: "provider", Value: dec.Provider},
		{Name: "credential_id", Value: dec.Credential},
		{Name: "model", Value: dec.UpstreamModel},
		{Name: "reason", Value: strings.TrimSpace(reason)},
	}
	if !res.outcome.ResetAt.IsZero() {
		fields = append(fields, notify.Field{Name: "resets_at", Value: res.outcome.ResetAt.UTC().Format(time.RFC3339)})
	}
	eng.Observe(ctx, alerts.Condition{
		Event: notify.EventQuotaExhausted, Subject: notify.Subject{Kind: "credential", ID: dec.Credential},
		Key: "quota_exhausted|" + dec.Provider + "|" + dec.Credential, Low: true, Fields: fields,
	})
}

// decimalNano parses a non-negative decimal amount into nano-units.
func decimalNano(s string) (int64, error) {
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, err
	}
	if len(frac) > 9 {
		frac = frac[:9]
	}
	frac += strings.Repeat("0", 9-len(frac))
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, err
	}
	return w*1_000_000_000 + f, nil
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
