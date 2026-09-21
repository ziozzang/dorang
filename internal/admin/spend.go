package admin

import (
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Bounded ranges — DESIGN §9.3
// ---------------------------------------------------------------------------

// rangeSpec is a time window as a caller supplies it.
type rangeSpec struct {
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
}

// resolveRange turns a caller's window into a validated half-open range.
//
// Three refusals, each with its own code, because the caller's fix differs:
//
//   - no window at all is [CodeUnboundedRange]. §9.3 says unbounded search is
//     refused; it does not say "defaulted to the last 24 hours", and a default
//     would answer a different question than the one asked while looking like it
//     answered the right one.
//   - an inverted or empty window is also [CodeUnboundedRange], because
//     end <= start bounds nothing.
//   - a window wider than the configured maximum is [CodeRangeTooWide], and it
//     is refused rather than silently narrowed. A capped query returns a partial
//     answer that looks complete, which is the worse of the two failures.
//
// A bare date is a whole day: end_date=2026-07-31 includes the 31st. That is
// what everyone who types it means, and half-open arithmetic on a date the
// caller wrote inclusively is a silent off-by-one-day in every daily report.
func (c *call) resolveRange(spec rangeSpec) (Range, error) {
	startRaw := firstNonEmpty(spec.StartDate, spec.StartTime,
		queryString(c.r, "start_date", "start_time", "startTime"))
	endRaw := firstNonEmpty(spec.EndDate, spec.EndTime,
		queryString(c.r, "end_date", "end_time", "endTime"))

	if startRaw == "" || endRaw == "" {
		c.a.metrics.rangeRefusals.Add(1)
		f := rangeFault(CodeUnboundedRange,
			"a ledger query requires a bounded time range: supply start_date and end_date "+
				"(YYYY-MM-DD or RFC 3339). DESIGN §9.3 refuses an unbounded search rather than answering it slowly")
		f.Detail = map[string]any{
			"max_range_hours": int64(c.a.cfg.MaxTimeRange / time.Hour),
			"missing":         missingBounds(startRaw, endRaw),
		}
		return Range{}, f
	}

	start, _, err := parseBound(startRaw)
	if err != nil {
		return Range{}, badRequest("start_date is not a recognized timestamp").withParam("start_date")
	}
	end, endDateOnly, err := parseBound(endRaw)
	if err != nil {
		return Range{}, badRequest("end_date is not a recognized timestamp").withParam("end_date")
	}
	if endDateOnly {
		end = end.Add(24 * time.Hour)
	}

	if !end.After(start) {
		c.a.metrics.rangeRefusals.Add(1)
		f := rangeFault(CodeUnboundedRange, "end_date must be after start_date")
		f.Param = "end_date"
		return Range{}, f
	}
	if width := end.Sub(start); width > c.a.cfg.MaxTimeRange {
		c.a.metrics.rangeRefusals.Add(1)
		f := rangeFault(CodeRangeTooWide,
			"the requested window is "+width.String()+", wider than this deployment's maximum of "+
				c.a.cfg.MaxTimeRange.String()+"; narrow it rather than expecting a partial answer")
		f.Detail = map[string]any{
			"requested_hours": int64(width / time.Hour),
			"max_range_hours": int64(c.a.cfg.MaxTimeRange / time.Hour),
		}
		return Range{}, f
	}
	return Range{Start: start, End: end}, nil
}

func missingBounds(start, end string) []string {
	var out []string
	if start == "" {
		out = append(out, "start_date")
	}
	if end == "" {
		out = append(out, "end_date")
	}
	return out
}

// parseBound parses a range bound and reports whether it named a whole day.
func parseBound(s string) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), true, nil
	}
	t, err := parseInstant(s)
	return t, false, err
}

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

// encodeCursor renders a keyset position opaquely.
//
// Opaque on purpose: a cursor a caller can construct is a cursor a caller will
// construct, and then the pagination contract is whatever they guessed rather
// than what the index supports.
func encodeCursor(c Cursor) string {
	raw := strconv.FormatInt(c.TS.UTC().UnixMicro(), 10) + ":" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (*Cursor, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, badRequest("cursor is not a cursor this server issued").withParam("cursor")
	}
	usStr, id, ok := strings.Cut(string(raw), ":")
	if !ok {
		return nil, badRequest("cursor is not a cursor this server issued").withParam("cursor")
	}
	us, err := strconv.ParseInt(usStr, 10, 64)
	if err != nil {
		return nil, badRequest("cursor is not a cursor this server issued").withParam("cursor")
	}
	return &Cursor{TS: time.UnixMicro(us).UTC(), ID: id}, nil
}

func (c *call) pageLimit(body int) (int, error) {
	limit := body
	if limit == 0 {
		v, err := queryInt(c.r, "limit", c.a.cfg.PageSize)
		if err != nil {
			return 0, err
		}
		limit = v
	}
	if limit <= 0 || limit > c.a.cfg.MaxPageSize {
		return 0, badRequest("limit must be between 1 and %d", c.a.cfg.MaxPageSize).withParam("limit")
	}
	return limit, nil
}

// ---------------------------------------------------------------------------
// /spend/logs
// ---------------------------------------------------------------------------

// spendLogView is one ledger row on the wire. The names follow the
// shape-compatible surface; dorang's own numbers are additive.
type spendLogView struct {
	RequestID string `json:"request_id"`
	StartTime Stamp  `json:"startTime"`

	APIKey       string `json:"api_key"`
	User         string `json:"user"`
	TeamID       string `json:"team_id"`
	CredentialID string `json:"credential_id"`
	ProviderID   string `json:"provider_id"`
	DeploymentID string `json:"deployment_id"`

	Model         string `json:"model"`
	ModelGroup    string `json:"model_group"`
	UpstreamModel string `json:"upstream_model"`
	CallType      string `json:"call_type"`

	Status     int    `json:"status"`
	ErrorClass string `json:"error_class,omitempty"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	Spend            Money `json:"spend"`
	MarginalSpend    Money `json:"marginal_spend"`
	SubscriptionCost Money `json:"subscription_spend"`
	// Notional is the list-rate equivalent (§8.5). It is null — not zero —
	// when no notional rule matched, so that a flat plan cannot be made to
	// look infinitely efficient by an absent rule.
	Notional *Money `json:"notional_spend"`
	// Utilization is the §8.6 disclosure of a price that MOVED, and is absent —
	// not zero — on every rate card that does not price on occupancy. A reader
	// summing invoices needs it for one question: whether this row's charge is
	// the rate card's number or a multiple of it, and which.
	Utilization *utilizationView `json:"utilization,omitempty"`

	LatencyMS      int64 `json:"latency_ms"`
	TTFTMS         int64 `json:"ttft_ms"`
	QueueMS        int64 `json:"queue_ms"`
	CapacityWaitMS int64 `json:"capacity_wait_ms"`

	FallbackCount int      `json:"fallback_count"`
	Streamed      bool     `json:"streamed"`
	TraceID       string   `json:"trace_id,omitempty"`
	SessionID     string   `json:"session_id,omitempty"`
	BatchID       string   `json:"batch_id,omitempty"`
	NodeID        string   `json:"node_id,omitempty"`
	Tags          []string `json:"tags"`
}

// utilizationView is the three values that make a variable charge reconcilable: what the
// rate was multiplied by, what it was multiplied by it FOR, and whether that was a
// measurement at all.
//
// `source` is the load-bearing one. A factor of 1.000000 is what an idle backend and an
// unobserved backend both produce, and they are the two facts VLLM.md §3.1 says must never
// be confused — so a view that carried only the factor would answer the easy half of the
// dispute and drop the half that is actually contested.
type utilizationView struct {
	Multiplier string `json:"multiplier"`
	Occupancy  string `json:"occupancy,omitempty"`
	Source     string `json:"source"`
}

// ppmDecimal renders a parts-per-million figure as a six-place decimal. A factor is a
// term of a bill and has to read as a number, not as 1450000.
func ppmDecimal(ppm int64) string {
	if ppm < 0 {
		ppm = 0
	}
	var b []byte
	b = strconv.AppendInt(b, ppm/1_000_000, 10)
	b = append(b, '.')
	frac := ppm % 1_000_000
	for div := int64(100_000); div > 0; div /= 10 {
		b = append(b, byte('0'+(frac/div)%10))
	}
	return string(b)
}

func viewLog(r LogRow) spendLogView {
	v := spendLogView{
		RequestID:        r.ID,
		StartTime:        Stamp(r.TS),
		APIKey:           r.APIKeyID,
		User:             r.UserID,
		TeamID:           r.TeamID,
		CredentialID:     r.CredentialID,
		ProviderID:       r.ProviderID,
		DeploymentID:     r.DeploymentID,
		Model:            r.ModelGroup,
		ModelGroup:       r.ModelGroup,
		UpstreamModel:    r.UpstreamModel,
		CallType:         r.Endpoint,
		Status:           r.Status,
		ErrorClass:       r.ErrorClass,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		CachedTokens:     r.CachedTokens,
		ReasoningTokens:  r.ReasoningTokens,
		TotalTokens:      r.TotalTokens,
		Spend:            Money(r.CostNano),
		MarginalSpend:    Money(r.MarginalCostNano),
		SubscriptionCost: Money(r.SubscriptionCostNano),
		LatencyMS:        r.LatencyMS,
		TTFTMS:           r.TTFTMS,
		QueueMS:          r.QueueMS,
		CapacityWaitMS:   r.CapacityWaitMS,
		FallbackCount:    r.FallbackCount,
		Streamed:         r.Streamed,
		TraceID:          r.TraceID,
		SessionID:        r.SessionID,
		BatchID:          r.BatchID,
		NodeID:           r.NodeID,
		Tags:             orEmpty(r.Tags),
	}
	if r.NotionalKnown {
		m := Money(r.NotionalNano)
		v.Notional = &m
	}
	if r.UtilSource != "" {
		u := utilizationView{
			Multiplier: ppmDecimal(r.UtilMultiplierPPM),
			Source:     r.UtilSource,
		}
		// The occupancy only when it was actually measured. Rendering 0.000000
		// beside `source: "no_load_header"` would put a measurement of an idle
		// backend on a row that says nobody looked.
		if r.UtilSource == "observed" {
			u.Occupancy = ppmDecimal(r.UtilPPM)
		}
		v.Utilization = &u
	}
	return v
}

type spendLogsRequest struct {
	rangeSpec
	KeyID   string `json:"api_key"`
	KeyID2  string `json:"key_id"`
	UserID  string `json:"user_id"`
	TeamID  string `json:"team_id"`
	TraceID string `json:"trace_id"`
	Tag     string `json:"tag"`
	Errors  bool   `json:"errors_only"`
	Limit   int    `json:"limit"`
	Cursor  string `json:"cursor"`
}

// GET|POST /spend/logs
func (c *call) spendLogs() error {
	led, err := c.a.ledger()
	if err != nil {
		return err
	}
	var body spendLogsRequest
	if err := decodeOptionalBody(c.w, c.r, &body); err != nil {
		return err
	}
	rng, err := c.resolveRange(body.rangeSpec)
	if err != nil {
		return err
	}
	limit, err := c.pageLimit(body.Limit)
	if err != nil {
		return err
	}
	cursor, err := decodeCursor(firstNonEmpty(body.Cursor, queryString(c.r, "cursor")))
	if err != nil {
		return err
	}
	errorsOnly := body.Errors
	if b, err := queryBool(c.r, "errors_only"); err != nil {
		return err
	} else if b != nil {
		errorsOnly = *b
	}

	// The team filter used to be read off the request and handed to the store
	// as-is: naming another tenant's team returned that tenant's request log,
	// prompt sizes, models, costs and trace ids. It is narrowed against the
	// caller's scope, and the rows are filtered again below.
	team, err := c.scopedTeamFilter(firstNonEmpty(body.TeamID, queryString(c.r, "team_id")))
	if err != nil {
		return err
	}
	q := LogQuery{
		Range:      rng,
		Limit:      limit,
		After:      cursor,
		KeyID:      firstNonEmpty(body.KeyID, body.KeyID2, queryString(c.r, "api_key", "key_id")),
		TeamID:     team,
		UserID:     firstNonEmpty(body.UserID, queryString(c.r, "user_id")),
		TraceID:    firstNonEmpty(body.TraceID, queryString(c.r, "trace_id", "request_id")),
		Tag:        firstNonEmpty(body.Tag, queryString(c.r, "tag")),
		ErrorsOnly: errorsOnly,
	}
	page, err := led.ListRequests(c.ctx(), q)
	if err != nil {
		return c.ledgerError(err)
	}
	sc := c.scope()
	rows := make([]spendLogView, 0, len(page.Rows))
	for _, r := range page.Rows {
		// The enforcement, independent of the store honouring the filter. A
		// key_id, user_id, trace_id or tag filter can also select rows from
		// another team — trace ids in particular are handed around in support
		// tickets — so the check is on the ROW's team rather than on which
		// filter produced it.
		if !sc.AllowsTeam(r.TeamID) {
			continue
		}
		if q.KeyID != "" && r.APIKeyID != q.KeyID || q.UserID != "" && r.UserID != q.UserID || q.TeamID != "" && r.TeamID != q.TeamID || q.TraceID != "" && r.TraceID != q.TraceID || q.ErrorsOnly && r.Status < 400 {
			continue
		}
		if q.Tag != "" {
			found := false
			for _, tag := range r.Tags {
				if tag == q.Tag {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		rows = append(rows, viewLog(r))
	}
	out := map[string]any{
		"logs":       rows,
		"count":      len(rows),
		"limit":      limit,
		"start_date": Stamp(rng.Start),
		"end_date":   Stamp(rng.End),
		"has_more":   page.Next != nil,
	}
	if page.Next != nil {
		out["next_cursor"] = encodeCursor(*page.Next)
	} else {
		out["next_cursor"] = nil
	}
	writeJSON(c.w, c.r, http.StatusOK, out)
	return nil
}

// ledgerError surfaces a store-side refusal without paraphrasing it into
// something weaker. A backend that refuses an unbounded range must reach the
// caller as a refusal, not as a 500 — the caller can fix the former.
func (c *call) ledgerError(err error) error {
	switch {
	case errors.Is(err, ErrUnboundedRange), errors.Is(err, ErrRangeTooWide):
		c.a.metrics.rangeRefusals.Add(1)
	}
	return err
}

// ---------------------------------------------------------------------------
// /global/spend/report
// ---------------------------------------------------------------------------

var reportDimensions = map[string]GroupBy{
	"day":      GroupByDay,
	"date":     GroupByDay,
	"model":    GroupByModel,
	"key":      GroupByKey,
	"api_key":  GroupByKey,
	"team":     GroupByTeam,
	"team_id":  GroupByTeam,
	"user":     GroupByUser,
	"user_id":  GroupByUser,
	"tag":      GroupByTag,
	"provider": GroupByProvider,
}

// usageView is one aggregate's counters. Cost and the notional figure travel
// together everywhere, so no caller has to go looking for the second one.
type usageView struct {
	Requests           int64 `json:"api_requests"`
	SuccessfulRequests int64 `json:"successful_requests"`
	FailedRequests     int64 `json:"failed_requests"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	Spend            Money `json:"spend"`
	MarginalSpend    Money `json:"marginal_spend"`
	SubscriptionCost Money `json:"subscription_spend"`
	// NotionalSpend is null when unavailable. §8.5: missing is reported, never
	// zero.
	NotionalSpend *Money `json:"notional_spend"`
	// NotionalAvailable states the same fact as a boolean, so a caller that
	// coerces null to zero still has a field that contradicts it.
	NotionalAvailable bool `json:"notional_available"`

	LatencyMSSum int64 `json:"latency_ms_sum"`
}

func viewUsage(u Usage) usageView {
	v := usageView{
		Requests:           u.Requests,
		SuccessfulRequests: u.Requests - u.Errors,
		FailedRequests:     u.Errors,
		PromptTokens:       u.PromptTokens,
		CompletionTokens:   u.CompletionTokens,
		CachedTokens:       u.CachedTokens,
		ReasoningTokens:    u.ReasoningTokens,
		TotalTokens:        u.TotalTokens,
		Spend:              Money(u.CostNano),
		MarginalSpend:      Money(u.MarginalCostNano),
		SubscriptionCost:   Money(u.SubscriptionCostNano),
		NotionalAvailable:  u.NotionalKnown,
		LatencyMSSum:       u.LatencyMSSum,
	}
	if u.NotionalKnown {
		m := Money(u.NotionalNano)
		v.NotionalSpend = &m
	}
	return v
}

type reportRowView struct {
	Date       string    `json:"date,omitempty"`
	ModelGroup string    `json:"model_group,omitempty"`
	APIKey     string    `json:"api_key,omitempty"`
	TeamID     string    `json:"team_id,omitempty"`
	UserID     string    `json:"user_id,omitempty"`
	Tag        string    `json:"tag,omitempty"`
	ProviderID string    `json:"provider_id,omitempty"`
	Metrics    usageView `json:"metrics"`
}

// GET|POST /global/spend/report
func (c *call) globalSpendReport() error {
	led, err := c.a.ledger()
	if err != nil {
		return err
	}
	// The name is the specification: this aggregates the whole deployment, and
	// ReportQuery carries no subject filter to narrow it with. A scoped
	// administrator gets the per-dimension endpoints below, which do narrow.
	// Serving it a "global" report containing one team's numbers would be a
	// different endpoint wearing this one's name.
	if err := c.requireGlobal("the deployment-wide spend report"); err != nil {
		return err
	}
	var body struct {
		rangeSpec
		GroupBy []string `json:"group_by"`
		Limit   int      `json:"limit"`
	}
	if err := decodeOptionalBody(c.w, c.r, &body); err != nil {
		return err
	}
	rng, err := c.resolveRange(body.rangeSpec)
	if err != nil {
		return err
	}
	names := body.GroupBy
	if len(names) == 0 {
		names = queryList(c.r, "group_by")
	}
	if len(names) == 0 {
		names = []string{"day"}
	}
	dims := make([]GroupBy, 0, len(names))
	for _, n := range names {
		g, ok := reportDimensions[strings.ToLower(strings.TrimSpace(n))]
		if !ok {
			return badRequest("group_by %q is not one of day, model, key, team, user, tag, provider", n).
				withParam("group_by")
		}
		dims = append(dims, g)
	}
	limit, err := c.pageLimit(body.Limit)
	if err != nil {
		return err
	}

	rep, err := led.Report(c.ctx(), ReportQuery{Range: rng, GroupBy: dims, Limit: limit})
	if err != nil {
		return c.ledgerError(err)
	}
	if !rep.Total.NotionalKnown {
		c.a.metrics.notionalMissing.Add(1)
	}
	rows := make([]reportRowView, 0, len(rep.Rows))
	for _, r := range rep.Rows {
		rows = append(rows, viewReportRow(r))
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"start_date": Stamp(rng.Start),
		"end_date":   Stamp(rng.End),
		"group_by":   names,
		"results":    rows,
		"total":      viewUsage(rep.Total),
	})
	return nil
}

func viewReportRow(r ReportRow) reportRowView {
	v := reportRowView{
		ModelGroup: r.ModelGroup,
		APIKey:     r.KeyID,
		TeamID:     r.TeamID,
		UserID:     r.UserID,
		Tag:        r.Tag,
		ProviderID: r.ProviderID,
		Metrics:    viewUsage(r.Usage),
	}
	if !r.Day.IsZero() {
		v.Date = r.Day.UTC().Format("2006-01-02")
	}
	return v
}

// ---------------------------------------------------------------------------
// /{user,team,tag}/daily/activity
// ---------------------------------------------------------------------------

// dailyActivity builds the handler for one dimension.
func dailyActivity(dim GroupBy, idParams ...string) handler {
	return func(c *call) error {
		led, err := c.a.ledger()
		if err != nil {
			return err
		}
		var body struct {
			rangeSpec
			IDs   []string `json:"ids"`
			Limit int      `json:"limit"`
		}
		if err := decodeOptionalBody(c.w, c.r, &body); err != nil {
			return err
		}
		rng, err := c.resolveRange(body.rangeSpec)
		if err != nil {
			return err
		}
		limit, err := c.pageLimit(body.Limit)
		if err != nil {
			return err
		}

		wanted := map[string]bool{}
		for _, id := range body.IDs {
			wanted[id] = true
		}
		for _, p := range idParams {
			for _, id := range queryList(c.r, p) {
				wanted[id] = true
			}
		}
		// The requested ids are intersected with what the caller may see. An
		// empty request means "everything", which for a scoped administrator
		// means "everything in scope" and not "everything" — the difference is
		// the finding. Ledger.Report takes no subject filter, so this set is
		// the only place the narrowing can happen, and every row below is
		// admitted through it including the ones that build the total.
		allowed, err := c.activityScope(dim)
		if err != nil {
			return err
		}
		if allowed != nil {
			if len(wanted) == 0 {
				wanted = allowed
			} else {
				for id := range wanted {
					if !allowed[id] {
						delete(wanted, id)
					}
				}
				if len(wanted) == 0 {
					// Every id asked for is out of scope. Answering with an
					// empty result would read as "that team spent nothing".
					return outOfScope(string(dim), "", c.scope())
				}
			}
		}

		// Two queries: one for the per-day figures, one for the per-day
		// per-model breakdown. Two is a deliberate ceiling — a breakdown per
		// dimension would be a query per dimension, and an endpoint whose cost
		// grows with how much detail it renders is an endpoint that falls over
		// on the day someone asks for all of it.
		base, err := led.Report(c.ctx(), ReportQuery{
			Range: rng, GroupBy: []GroupBy{GroupByDay, dim}, Limit: limit,
		})
		if err != nil {
			return c.ledgerError(err)
		}
		byModel, err := led.Report(c.ctx(), ReportQuery{
			Range: rng, GroupBy: []GroupBy{GroupByDay, dim, GroupByModel}, Limit: limit,
		})
		if err != nil {
			return c.ledgerError(err)
		}

		type dayKey struct{ date, value string }
		breakdown := map[dayKey][]reportRowView{}
		for _, r := range byModel.Rows {
			v := dimensionValue(dim, r)
			if len(wanted) > 0 && !wanted[v] {
				continue
			}
			k := dayKey{dayString(r.Day), v}
			breakdown[k] = append(breakdown[k], reportRowView{
				ModelGroup: r.ModelGroup,
				Metrics:    viewUsage(r.Usage),
			})
		}

		var total Usage
		total.NotionalKnown = true
		results := make([]map[string]any, 0, len(base.Rows))
		for _, r := range base.Rows {
			v := dimensionValue(dim, r)
			if len(wanted) > 0 && !wanted[v] {
				continue
			}
			total.Add(r.Usage)
			k := dayKey{dayString(r.Day), v}
			models := breakdown[k]
			sort.Slice(models, func(i, j int) bool { return models[i].ModelGroup < models[j].ModelGroup })
			results = append(results, map[string]any{
				"date":              k.date,
				string(dim) + "_id": v,
				"metrics":           viewUsage(r.Usage),
				"breakdown":         map[string]any{"models": models},
			})
		}
		if !total.NotionalKnown {
			c.a.metrics.notionalMissing.Add(1)
		}

		writeJSON(c.w, c.r, http.StatusOK, map[string]any{
			"results": results,
			"metadata": map[string]any{
				"dimension":  string(dim),
				"start_date": Stamp(rng.Start),
				"end_date":   Stamp(rng.End),
				"total":      viewUsage(total),
				"count":      len(results),
				"limit":      limit,
				"has_more":   len(base.Rows) >= limit,
			},
		})
		return nil
	}
}

func dimensionValue(dim GroupBy, r ReportRow) string {
	switch dim {
	case GroupByUser:
		return r.UserID
	case GroupByTeam:
		return r.TeamID
	case GroupByTag:
		return r.Tag
	case GroupByKey:
		return r.KeyID
	case GroupByModel:
		return r.ModelGroup
	case GroupByProvider:
		return r.ProviderID
	default:
		return ""
	}
}

func dayString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

// ---------------------------------------------------------------------------
// /spend/calculate
// ---------------------------------------------------------------------------

type calculateRequest struct {
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Credential string `json:"credential"`
	Deployment string `json:"deployment"`

	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	Requests         int64   `json:"requests"`
	Characters       int64   `json:"characters"`
	Seconds          float64 `json:"seconds"`

	At Stamp `json:"at"`

	// Messages is accepted so that a caller sending the incumbent's shape gets
	// an explanation instead of a confusing zero. It is refused, with a code:
	// counting tokens from messages needs the model's tokenizer, dorang does
	// not carry one, and estimating with a wrong tokenizer would produce a
	// figure that looks authoritative and is not.
	Messages []map[string]any `json:"messages"`
	// CompletionResponse is refused for the same reason.
	CompletionResponse map[string]any `json:"completion_response"`
}

// POST /spend/calculate
func (c *call) spendCalculate() error {
	if c.a.cfg.Pricing == nil {
		return dependencyOff("pricing engine", "cost calculation")
	}
	var body calculateRequest
	if err := decodeBody(c.w, c.r, &body); err != nil {
		return err
	}
	if strings.TrimSpace(body.Model) == "" {
		return badRequest("model is required").withParam("model")
	}
	if (len(body.Messages) > 0 || len(body.CompletionResponse) > 0) &&
		body.PromptTokens == 0 && body.CompletionTokens == 0 {
		f := unimplemented("tokenizer_unavailable",
			"cost from messages requires the model's tokenizer, which dorang does not carry; "+
				"send prompt_tokens and completion_tokens, which is what the ledger records anyway")
		f.Param = "messages"
		return f
	}

	req := PriceRequest{
		Provider:         body.Provider,
		Model:            body.Model,
		Credential:       body.Credential,
		Deployment:       body.Deployment,
		InputTokens:      body.PromptTokens,
		OutputTokens:     body.CompletionTokens,
		CacheReadTokens:  body.CachedTokens,
		CacheWriteTokens: body.CacheWriteTokens,
		ReasoningTokens:  body.ReasoningTokens,
		Requests:         body.Requests,
		Characters:       body.Characters,
		Seconds:          body.Seconds,
		At:               body.At.Time(),
	}
	if req.At.IsZero() {
		req.At = c.a.now()
	}
	ex, err := c.a.cfg.Pricing.Explain(c.ctx(), req)
	if err != nil {
		return err
	}
	if ex.Notional.Missing {
		c.a.metrics.notionalMissing.Add(1)
	}
	writeJSON(c.w, c.r, http.StatusOK, viewPrice(ex))
	return nil
}
