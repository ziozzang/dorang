package app

import (
	"context"
	"github.com/ziozzang/dorang/internal/admin"
	"github.com/ziozzang/dorang/internal/quota"
	"sort"
)

// Credential health reports quota/OAuth admission. Deployment circuit state is
// a separate Prometheus family and must not be attributed to every credential
// that happens to share a deployment. Request counters here cover the bounded
// recent metadata buffer, explicitly named by SampleScope.
type adminCredentials struct{ a *App }

func (p *adminCredentials) Credentials(context.Context) ([]admin.CredentialStatus, error) {
	a := p.a
	now := a.now()
	cfg := a.Config()
	st := a.dispatch.state()
	rows := make(map[string]*admin.CredentialStatus, len(cfg.Credentials))
	for _, c := range cfg.Credentials {
		v := &admin.CredentialStatus{ID: c.ID, ProviderID: c.Provider, Health: "unknown", UpdatedAt: now, SampleScope: "recent captured requests · 15m / 256 records"}
		if st != nil && st.quota != nil {
			if meter := st.quota.meters[c.ID]; meter != nil {
				var view quota.View
				meter.Fill(now, &view)
				for _, rule := range view.Rules[:view.N] {
					pct := 0
					if rule.Limit > 0 {
						pct = int(float64(rule.Used) / float64(rule.Limit) * 100)
					}
					source := "local"
					if rule.FromProvider {
						source = "combined"
					}
					v.Quota = append(v.Quota, admin.QuotaWindow{Window: rule.Window.String(), Metric: rule.Metric.String(), Used: rule.Used, Limit: rule.Limit, UsedPct: pct, ResetAt: rule.ResetAt, Source: source})
				}
				switch meter.State(now) {
				case quota.StateCooldown, quota.StateDisabled:
					v.Health = "unavailable"
				}
			}
		}
		rows[c.ID] = v
	}
	if a.OAuth != nil {
		for _, h := range a.OAuth.Snapshot() {
			if v := rows[h.ID]; v != nil {
				if !h.Healthy {
					v.Health = "unavailable"
					v.UnavailableUntil = h.NextAttempt
				} else if v.Health != "unavailable" {
					v.Health = "healthy"
				}
			}
		}
	}
	for _, r := range a.traffic.Recent(now) {
		if v := rows[r.Credential]; v != nil {
			v.Requests++
			if r.Status >= 400 {
				v.Failures++
			}
			v.LatencyMS += r.LatencyMS
			v.TTFTMS += r.TTFTMS
		}
	}
	out := make([]admin.CredentialStatus, 0, len(rows))
	for _, v := range rows {
		if v.Requests > 0 {
			v.LatencyMS /= v.Requests
			v.TTFTMS /= v.Requests
		}
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
