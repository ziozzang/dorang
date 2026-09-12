package admin

import (
	"net/http"
	"sort"
	"time"
)

// The live monitoring screen.
//
// It answers the question a spend report cannot — "what is happening right
// now" — from the reporters the process already exposes: the ledger for the
// last few minutes of traffic, the credential reporter for per-credential
// health and quota, and the capacity broker for occupancy. Nothing here is a
// new data path; it is the same data /spend/logs, /admin/credentials/health
// and /admin/capacity return, arranged for a glance and re-fetched on a timer
// by app.js (the [data-live] region), so the numbers move without a full
// navigation.
//
// The window is the last [monitorWindow]. It is read once per render; the
// auto-refresh re-renders. A deployment with no ledger still gets the health
// and capacity halves — each section is independent and says so when its
// dependency is absent, the same rule the nav and the other screens follow.

const monitorWindow = 5 * time.Minute
const monitorRecent = 25 // rows in the recent-requests table
const monitorScan = 5000 // ledger rows scanned for the window's stats

func (s *uiServer) screenMonitoring(w http.ResponseWriter, r *http.Request, v viewer) {
	if !v.scope.Global {
		s.renderMessage(w, r, http.StatusForbidden, v, "Monitoring",
			"Live monitoring is a deployment-wide view; this session is scoped to a team.", CodeForbidden)
		return
	}
	now := s.api.now()
	pg := monitorPage{
		page:      s.newPage("monitoring", "Monitoring", v),
		Window:    "5m",
		HasLedger: s.api.cfg.Ledger != nil,
		HasCreds:  s.api.cfg.Credentials != nil,
		HasCap:    s.api.cfg.Capacity != nil,
	}

	if pg.HasLedger {
		page, err := s.api.cfg.Ledger.ListRequests(r.Context(), LogQuery{
			Range: Range{Start: now.Add(-monitorWindow), End: now}, Limit: monitorScan,
		})
		if err == nil {
			pg.fillTraffic(page.Rows, now)
		} else {
			pg.LedgerErr = true
		}
	}
	if pg.HasCreds {
		if list, err := s.api.cfg.Credentials.Credentials(r.Context()); err == nil {
			sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
			for _, cs := range list {
				row := credHealthRow{
					ID: cs.ID, Provider: cs.ProviderID, Health: cs.Health,
					Failures: cs.ConsecutiveFailures, LatencyMS: cs.LatencyMS,
				}
				if cs.Health == "unavailable" && !cs.UnavailableUntil.IsZero() {
					if d := cs.UnavailableUntil.Sub(now); d > 0 {
						row.Cooldown = d.Round(time.Second).String()
					}
				}
				row.Quota = quotaSummary(cs.Quota, now)
				if cs.Health != "" && cs.Health != "healthy" {
					pg.Unhealthy++
				}
				pg.Creds = append(pg.Creds, row)
			}
			pg.CredTotal = len(pg.Creds)
		} else {
			pg.CredErr = true
		}
	}
	if pg.HasCap {
		if occ, err := s.api.cfg.Capacity.Occupancy(r.Context()); err == nil {
			pg.Waiting = occ.Waiting
			pg.Reservations = occ.Reservations
			for _, a := range occ.Axes {
				pg.Axes = append(pg.Axes, capRow{Axis: a.Axis, Key: a.Key, InUse: a.InUse, Limit: a.Limit, Waiting: a.Waiting})
			}
		} else {
			pg.CapErr = true
		}
	}

	s.render(w, r, http.StatusOK, "monitoring", pg)
}

type monitorPage struct {
	page
	Window string

	HasLedger, LedgerErr bool
	Requests             int64
	Errors               int64
	ErrorRate            string
	RPM                  string
	P50, P95             string
	Recent               []recentRow

	HasCreds  bool
	CredErr   bool
	CredTotal int
	Unhealthy int
	Creds     []credHealthRow

	HasCap       bool
	CapErr       bool
	Waiting      int
	Reservations int
	Axes         []capRow
}

type recentRow struct {
	Time, Model, Provider, Endpoint string
	Status                          int
	StatusOK                        bool
	LatencyMS                       int64
}
type credHealthRow struct {
	ID, Provider, Health, Cooldown, Quota string
	Failures                              int
	LatencyMS                             int64
}
type capRow struct {
	Axis, Key             string
	InUse, Limit, Waiting int
}

func (pg *monitorPage) fillTraffic(rows []LogRow, now time.Time) {
	pg.Requests = int64(len(rows))
	lat := make([]int64, 0, len(rows))
	for _, r := range rows {
		if r.Status >= 400 {
			pg.Errors++
		}
		if r.LatencyMS > 0 {
			lat = append(lat, r.LatencyMS)
		}
	}
	if pg.Requests > 0 {
		pg.ErrorRate = pct(pg.Errors, pg.Requests)
		pg.RPM = oneDecimal(float64(pg.Requests) / monitorWindow.Minutes())
	} else {
		pg.ErrorRate, pg.RPM = "0%", "0"
	}
	pg.P50 = percentile(lat, 50)
	pg.P95 = percentile(lat, 95)

	// Newest first for the recent table.
	sort.Slice(rows, func(i, j int) bool { return rows[i].TS.After(rows[j].TS) })
	for i, r := range rows {
		if i >= monitorRecent {
			break
		}
		pg.Recent = append(pg.Recent, recentRow{
			Time: r.TS.UTC().Format("15:04:05"), Model: r.ModelGroup, Provider: r.ProviderID,
			Endpoint: shortEndpoint(r.Endpoint), Status: r.Status, StatusOK: r.Status < 400, LatencyMS: r.LatencyMS,
		})
	}
}

func percentile(v []int64, p int) string {
	if len(v) == 0 {
		return "—"
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	idx := (p * (len(v) - 1)) / 100
	return itoaMS(v[idx])
}

func itoaMS(ms int64) string { return humanInt(ms) + " ms" }

func pct(n, d int64) string {
	if d == 0 {
		return "0%"
	}
	return oneDecimal(100*float64(n)/float64(d)) + "%"
}

func oneDecimal(f float64) string {
	// One place, without pulling in fmt's float formatting rules elsewhere.
	whole := int64(f)
	frac := int64((f - float64(whole)) * 10)
	if frac < 0 {
		frac = -frac
	}
	return humanInt(whole) + "." + string(rune('0'+frac))
}

func shortEndpoint(e string) string {
	switch e {
	case "chat_completions":
		return "chat"
	case "responses":
		return "responses"
	case "messages":
		return "messages"
	case "embeddings":
		return "embeddings"
	}
	return e
}

func quotaSummary(ws []QuotaWindow, _ time.Time) string {
	if len(ws) == 0 {
		return "—"
	}
	// The tightest window: the one closest to its ceiling. A window with no
	// declared limit carries no fraction and is skipped.
	best := ""
	bestPct := -1
	for _, q := range ws {
		if q.Limit <= 0 {
			continue
		}
		if q.UsedPct > bestPct {
			bestPct = q.UsedPct
			best = humanInt(q.Limit-q.Used) + " left / " + humanInt(q.Limit) + " " + q.Window +
				" (" + humanInt(int64(q.UsedPct)) + "% used)"
		}
	}
	if best == "" {
		return "—"
	}
	return best
}
