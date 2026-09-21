package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type consolePage struct {
	ReportGroups    []string
	BudgetKinds     []string
	SoftBudgetKinds string
	page
	Data           map[string]any
	Extra          map[string]any
	Query          url.Values
	Start, End     string
	Next, Previous string
	Live           bool
	Features       []consoleFeature
}
type consoleFeature struct {
	Name, Path, Screen string
	Available          bool
}

// readConsole invokes the existing API handler so its validation, scope and
// money semantics remain the same for the browser and API clients.
func (s *uiServer) readConsole(r *http.Request, v viewer, path string, body any) (map[string]any, error) {
	raw, err := s.invoke(r, v, path, body)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	err = dec.Decode(&out)
	return out, err
}

var consoleTitles = map[string]string{
	"setup":    "Upstream connections",
	"overview": "Overview", "requests": "Live requests", "history": "Request history", "metrics": "Metrics explorer", "credentials": "Providers & credentials", "capacity": "Capacity & queues", "budgets": "Budgets", "analytics": "Model analytics", "pricing": "Price calculator", "catalog": "Model capabilities", "operations": "System & API",
}

func (s *uiServer) screenConsole(w http.ResponseWriter, r *http.Request, v viewer, screen string) {
	title := consoleTitles[screen]
	if !v.scope.Global {
		s.renderMessage(w, r, http.StatusForbidden, v, title, "This screen requires global operator access.", CodeForbidden)
		return
	}
	pg := consolePage{page: s.newPage(screen, title, v), Query: r.URL.Query(), Data: map[string]any{}, Extra: map[string]any{}}
	pg.ReportGroups = []string{"model", "provider", "key", "team", "user", "tag", "day"}
	if caps, ok := s.api.cfg.Ledger.(interface{ ReportGroups() []string }); ok {
		pg.ReportGroups = caps.ReportGroups()
	}
	pg.BudgetKinds = []string{"key", "user", "team", "credential", "global"}
	if caps, ok := s.api.cfg.Budgets.(interface{ BudgetKinds() []string }); ok {
		pg.BudgetKinds = caps.BudgetKinds()
	}
	pg.SoftBudgetKinds = "key user team credential global"
	if caps, ok := s.api.cfg.Budgets.(interface{ SoftBudgetKinds() []string }); ok {
		pg.SoftBudgetKinds = strings.Join(caps.SoftBudgetKinds(), " ")
	}

	now := s.api.now().UTC()
	pg.Start = firstNonEmpty(pg.Query.Get("start_date"), now.Add(-24*time.Hour).Format(time.RFC3339))
	pg.End = firstNonEmpty(pg.Query.Get("end_date"), now.Format(time.RFC3339))
	var err error
	switch screen {
	case "setup":
		pg.Data, err = s.readConsole(r, v, "/admin/setup", map[string]any{})
		if err == nil {
			for _, section := range []string{"providers", "credentials"} {
				if rows, ok := pg.Data[section].([]any); ok {
					for _, row := range rows {
						m, _ := row.(map[string]any)
						if m["id"] == pg.Query.Get("edit_"+section) {
							pg.Extra[section] = m
						}
					}
				}
			}
		}
	case "overview", "requests", "metrics", "credentials", "capacity":
		pg.Live = true
	case "history":
		body := map[string]any{"start_date": pg.Start, "end_date": pg.End, "limit": min(50, s.api.cfg.MaxPageSize), "errors_only": pg.Query.Get("errors_only") != "false"}
		for _, k := range []string{"key_id", "user_id", "team_id", "trace_id", "tag", "cursor"} {
			if val := pg.Query.Get(k); val != "" {
				body[k] = val
			}
		}
		pg.Data, err = s.readConsole(r, v, "/spend/logs", body)
		if err == nil {
			if cur, ok := pg.Data["next_cursor"].(string); ok && cur != "" {
				q := pg.Query
				q.Set("start_date", pg.Start)
				q.Set("end_date", pg.End)
				q.Set("cursor", cur)
				pg.Next = s.base() + "/history?" + q.Encode()
			}
		}
	case "analytics":
		dim := firstNonEmpty(pg.Query.Get("group_by"), "model")
		pg.Query.Set("group_by", dim)
		pg.Data, err = s.readConsole(r, v, "/global/spend/report", map[string]any{"start_date": pg.Start, "end_date": pg.End, "group_by": []string{dim}, "limit": min(200, s.api.cfg.MaxPageSize)})
	case "budgets":
		offset, e := strconv.Atoi(firstNonEmpty(pg.Query.Get("offset"), "0"))
		if e != nil || offset < 0 {
			err = badRequest("Invalid page offset")
			break
		}
		limit := min(50, s.api.cfg.MaxListLimit)
		pg.Data, err = s.readConsole(r, v, "/budget/list", map[string]any{"limit": limit, "offset": offset})
		if err == nil {
			if rows, ok := pg.Data["budgets"].([]any); ok && len(rows) == limit {
				pg.Next = s.base() + "/budgets?offset=" + strconv.Itoa(offset+limit)
			}
			if offset > 0 {
				pg.Previous = s.base() + "/budgets?offset=" + strconv.Itoa(max(0, offset-limit))
			}
			if id := pg.Query.Get("budget_id"); id != "" {
				pg.Extra, err = s.readConsole(r, v, "/budget/info", map[string]any{"budget_id": id})
			}
		}
	case "pricing":
		if model := strings.TrimSpace(pg.Query.Get("model")); model != "" {
			body := map[string]any{"model": model, "provider": pg.Query.Get("provider"), "credential": pg.Query.Get("credential"), "deployment": pg.Query.Get("deployment")}
			for _, k := range []string{"prompt_tokens", "completion_tokens", "cached_tokens", "cache_write_tokens", "reasoning_tokens", "requests", "characters"} {
				n, e := strconv.ParseInt(firstNonEmpty(pg.Query.Get(k), "0"), 10, 64)
				if e != nil || n < 0 {
					err = badRequest("%s must be a non-negative integer", k)
					break
				}
				body[k] = n
			}
			if err == nil {
				pg.Data, err = s.readConsole(r, v, "/admin/pricing/preview", body)
			}
		}
	case "catalog":
		pg.Extra, err = s.readConsole(r, v, "/admin/catalog/unverified", map[string]any{})
		if err == nil && pg.Query.Get("model") != "" {
			pg.Data, err = s.readConsole(r, v, "/admin/catalog/explain", map[string]any{"model": pg.Query.Get("model"), "kind": pg.Query.Get("kind")})
		}
	case "operations":
		pg.Data, err = s.readConsole(r, v, "/admin/status", map[string]any{})
		for path, route := range s.api.routes {
			pg.Features = append(pg.Features, consoleFeature{Name: strings.Split(strings.TrimPrefix(path, "/"), "/")[0], Path: path, Available: !route.unsupported, Screen: consoleRouteScreen(path)})
		}
		sort.Slice(pg.Features, func(i, j int) bool { return pg.Features[i].Path < pg.Features[j].Path })
	}
	if err != nil {
		f := faultFor(err)
		pg.Problem = f.Message
		if f.Status >= 500 && f.Status != http.StatusNotImplemented {
			pg.Problem = "The data source is unavailable. Check system status and try again."
		}
		if pg.Data == nil {
			pg.Data = map[string]any{}
		}
		if pg.Extra == nil {
			pg.Extra = map[string]any{}
		}
		template := "console"
		if screen == "setup" {
			template = "setup"
		}
		s.render(w, r, f.Status, template, pg)
		return
	}
	template := "console"
	if screen == "setup" {
		template = "setup"
	}
	s.render(w, r, http.StatusOK, template, pg)
}

func consoleRouteScreen(p string) string {
	if strings.HasPrefix(p, "/admin/setup") {
		return "setup"
	}
	switch {
	case strings.HasPrefix(p, "/key/"):
		return "keys"
	case strings.HasPrefix(p, "/user/"), strings.HasPrefix(p, "/team/"):
		return "users"
	case strings.HasPrefix(p, "/budget/"):
		return "budgets"
	case strings.HasPrefix(p, "/model/"), strings.HasPrefix(p, "/model_group/"):
		return "models"
	case strings.Contains(p, "pricing"), p == "/spend/calculate":
		return "pricing"
	case strings.Contains(p, "catalog"):
		return "catalog"
	case strings.Contains(p, "credentials"), strings.Contains(p, "quota"):
		return "credentials"
	case strings.Contains(p, "capacity"):
		return "capacity"
	case p == "/spend/logs":
		return "history"
	case strings.Contains(p, "activity"), strings.Contains(p, "spend/report"):
		return "analytics"
	case strings.Contains(p, "telemetry"):
		return "metrics"
	case strings.Contains(p, "requests/recent"):
		return "requests"
	}
	return "operations"
}

func budgetFormBody(f url.Values) (any, error) {
	body := map[string]any{"subject_kind": f.Get("subject_kind"), "subject_id": f.Get("subject_id"), "budget_duration": f.Get("budget_duration")}
	for _, k := range []string{"max_budget", "soft_budget"} {
		if value := strings.TrimSpace(f.Get(k)); value != "" {
			var m Money
			if err := m.UnmarshalJSON([]byte(value)); err != nil || m < 0 {
				return nil, errors.New("Budget amounts must be non-negative decimal numbers")
			}
			body[k] = m
		}
	}
	return body, nil
}

func init() {
	for _, a := range []struct{ name, path string }{{"budget_create", "/budget/new"}, {"budget_update", "/budget/update"}, {"budget_delete", "/budget/delete"}} {
		action := uiAction{api: a.path, verb: "update budget", past: "updated", needs: func(s *uiServer) bool { return s.api.cfg.Budgets != nil }, body: budgetFormBody, notice: func(f url.Values, _ map[string]any) string { return "Budget changes applied." }}
		if a.name == "budget_delete" {
			action.body = func(f url.Values) (any, error) { return map[string]any{"budget_id": f.Get("budget_id")}, nil }
			action.notice = func(url.Values, map[string]any) string { return "Budget ceiling removed. Recorded spend is preserved." }
		}
		uiActions[a.name] = action
	}
	uiActions["config_reload"] = uiAction{api: "/admin/config/reload", verb: "reload configuration", past: "reloaded", needs: func(s *uiServer) bool { return s.api.cfg.Reloader != nil }, body: func(url.Values) (any, error) { return map[string]any{}, nil }, notice: func(_ url.Values, res map[string]any) string {
		if unchanged, _ := res["unchanged"].(bool); unchanged {
			return "Configuration checked; no changes."
		}
		return "Configuration reloaded on this node. Verify the other nodes separately."
	}}
}
