package admin

import (
	"net/http"
	"sort"
	"strings"
)

// The native /admin/* surface exists for what dorang has and the incumbent does
// not, so there is no shape to be compatible with. §2.3 says everything else
// answers 501; these are the "everything else" that dorang can actually answer,
// and they are namespaced so that a future shape-compatible path can never
// collide with one.
//
// The figures here are §12.3's metrics at their natural grain: credential
// health, provider quota percentage, budget consumption, capacity in flight and
// waiting per axis. They are served as objects rather than as a scrape format
// because an operator debugging a stuck deployment needs the axis key, and a
// counter labelled with an axis key is unbounded cardinality (see metrics.go).

// ---------------------------------------------------------------------------
// GET /admin/credentials/health
// ---------------------------------------------------------------------------

type quotaWindowView struct {
	Window  string `json:"window"`
	Metric  string `json:"metric"`
	Used    int64  `json:"used"`
	Limit   int64  `json:"limit"`
	UsedPct int    `json:"used_pct"`
	ResetAt Stamp  `json:"reset_at"`
	// Source is local, provider or combined. §6.2 combines the two rather than
	// letting one replace the other, and a percentage whose provenance is
	// unstated is a percentage nobody can act on.
	Source string `json:"source"`
	Stale  bool   `json:"stale"`
}

type credentialView struct {
	CredentialID string `json:"credential_id"`
	ProviderID   string `json:"provider_id"`

	Health              string `json:"health"`
	UnavailableUntil    Stamp  `json:"unavailable_until"`
	ConsecutiveFailures int    `json:"consecutive_failures"`

	Requests int64 `json:"requests"`
	Failures int64 `json:"failures"`
	Opens    int64 `json:"circuit_opens"`

	TTFTMS       int64   `json:"ttft_ms"`
	LatencyMS    int64   `json:"latency_ms"`
	TokensPerSec float64 `json:"tokens_per_sec"`

	Quota     []quotaWindowView `json:"quota"`
	UpdatedAt Stamp             `json:"updated_at"`
}

func viewCredential(s CredentialStatus) credentialView {
	q := make([]quotaWindowView, 0, len(s.Quota))
	for _, w := range s.Quota {
		q = append(q, viewQuotaWindow(w))
	}
	return credentialView{
		CredentialID:        s.ID,
		ProviderID:          s.ProviderID,
		Health:              s.Health,
		UnavailableUntil:    Stamp(s.UnavailableUntil),
		ConsecutiveFailures: s.ConsecutiveFailures,
		Requests:            s.Requests,
		Failures:            s.Failures,
		Opens:               s.Opens,
		TTFTMS:              s.TTFTMS,
		LatencyMS:           s.LatencyMS,
		TokensPerSec:        s.TokensPerSec,
		Quota:               q,
		UpdatedAt:           Stamp(s.UpdatedAt),
	}
}

func (c *call) adminCredentials() error {
	// Deployment configuration and process state, not tenant data: there is
	// no team-scoped view of provider credential health, so a scoped
	// administrator is refused rather than served a filtered fiction.
	if err := c.requireGlobal("provider credential health"); err != nil {
		return err
	}
	if c.a.cfg.Credentials == nil {
		return dependencyOff("credential reporter", "credential health")
	}
	list, err := c.a.cfg.Credentials.Credentials(c.ctx())
	if err != nil {
		return err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	out := make([]credentialView, 0, len(list))
	var unhealthy int
	for _, s := range list {
		if s.Health != "" && s.Health != "healthy" {
			unhealthy++
		}
		out = append(out, viewCredential(s))
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"credentials": out,
		"count":       len(out),
		"unhealthy":   unhealthy,
	})
	return nil
}

// GET /admin/quota — the quota half of the same snapshot, flattened.
//
// It is a separate endpoint rather than a query parameter because the two
// answer different questions: "which credentials are sick" and "which windows
// are close to exhaustion". Flattening the windows makes the second one a
// sort rather than a nested walk.
func (c *call) adminQuota() error {
	// Deployment configuration and process state, not tenant data: there is
	// no team-scoped view of provider quota, so a scoped
	// administrator is refused rather than served a filtered fiction.
	if err := c.requireGlobal("provider quota"); err != nil {
		return err
	}
	if c.a.cfg.Credentials == nil {
		return dependencyOff("credential reporter", "quota snapshots")
	}
	list, err := c.a.cfg.Credentials.Credentials(c.ctx())
	if err != nil {
		return err
	}
	type flat struct {
		CredentialID string `json:"credential_id"`
		ProviderID   string `json:"provider_id"`
		quotaWindowView
	}
	out := make([]flat, 0, 8)
	for _, s := range list {
		for _, w := range s.Quota {
			out = append(out, flat{
				CredentialID:    s.ID,
				ProviderID:      s.ProviderID,
				quotaWindowView: viewQuotaWindow(w),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UsedPct != out[j].UsedPct {
			return out[i].UsedPct > out[j].UsedPct
		}
		if out[i].CredentialID != out[j].CredentialID {
			return out[i].CredentialID < out[j].CredentialID
		}
		return out[i].Window < out[j].Window
	})
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"windows": out, "count": len(out)})
	return nil
}

func viewQuotaWindow(w QuotaWindow) quotaWindowView {
	return quotaWindowView{
		Window:  w.Window,
		Metric:  w.Metric,
		Used:    w.Used,
		Limit:   w.Limit,
		UsedPct: w.UsedPct,
		ResetAt: Stamp(w.ResetAt),
		Source:  w.Source,
		Stale:   w.Stale,
	}
}

// ---------------------------------------------------------------------------
// GET /admin/capacity
// ---------------------------------------------------------------------------

func (c *call) adminCapacity() error {
	// Deployment configuration and process state, not tenant data: there is
	// no team-scoped view of capacity occupancy, so a scoped
	// administrator is refused rather than served a filtered fiction.
	if err := c.requireGlobal("capacity occupancy"); err != nil {
		return err
	}
	if c.a.cfg.Capacity == nil {
		return dependencyOff("capacity broker", "capacity occupancy")
	}
	occ, err := c.a.cfg.Capacity.Occupancy(c.ctx())
	if err != nil {
		return err
	}
	type axisView struct {
		Axis    string `json:"axis"`
		Key     string `json:"key"`
		InUse   int    `json:"in_use"`
		Limit   int    `json:"limit"`
		Waiting int    `json:"waiting"`
		// UsedPct is omitted rather than reported as zero when there is no
		// limit: "nothing in use" and "no ceiling" are different facts.
		UsedPct *int `json:"used_pct"`
	}
	axes := make([]axisView, 0, len(occ.Axes))
	for _, a := range occ.Axes {
		v := axisView{Axis: a.Axis, Key: a.Key, InUse: a.InUse, Limit: a.Limit, Waiting: a.Waiting}
		if a.Limit > 0 {
			pct := a.InUse * 100 / a.Limit
			v.UsedPct = &pct
		}
		axes = append(axes, v)
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"axes":         axes,
		"waiting":      occ.Waiting,
		"reservations": occ.Reservations,
		"grants":       occ.Grants,
		"wakeups":      occ.Wakeups,
		"expired":      occ.Expired,
	})
	return nil
}

// ---------------------------------------------------------------------------
// Catalog
// ---------------------------------------------------------------------------

// GET /admin/catalog/explain?kind=&model=
func (c *call) adminCatalogExplain() error {
	// Deployment configuration and process state, not tenant data: there is
	// no team-scoped view of the model catalog, so a scoped
	// administrator is refused rather than served a filtered fiction.
	if err := c.requireGlobal("the model catalog"); err != nil {
		return err
	}
	if c.a.cfg.Catalog == nil {
		return dependencyOff("model catalog", "catalog provenance")
	}
	var body struct {
		Kind  string `json:"kind"`
		Model string `json:"model"`
	}
	if err := decodeOptionalBody(c.w, c.r, &body); err != nil {
		return err
	}
	kind := firstNonEmpty(body.Kind, queryString(c.r, "kind", "provider_kind"))
	model := firstNonEmpty(body.Model, queryString(c.r, "model"))
	if model == "" {
		return badRequest("model is required").withParam("model")
	}
	ex, err := c.a.cfg.Catalog.ExplainModel(kind, model)
	if err != nil {
		return err
	}
	fields := make([]map[string]any, 0, len(ex.Fields))
	for _, f := range ex.Fields {
		fields = append(fields, map[string]any{
			"field":  f.Field,
			"value":  f.Value,
			"layer":  f.Layer,
			"origin": f.Origin,
			"source": f.Source,
		})
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"kind":           ex.Kind,
		"model":          ex.Model,
		"fields":         fields,
		"layers":         orEmpty(ex.Layers),
		"matched_prefix": ex.MatchedPrefix,
		"verified":       ex.Verified,
		"kind_known":     ex.KindKnown,
		"model_known":    ex.ModelKnown,
		"note":           ex.Note,
	})
	return nil
}

// GET /admin/catalog/unverified
func (c *call) adminCatalogUnverified() error {
	// Deployment configuration and process state, not tenant data: there is
	// no team-scoped view of the model catalog, so a scoped
	// administrator is refused rather than served a filtered fiction.
	if err := c.requireGlobal("the model catalog"); err != nil {
		return err
	}
	if c.a.cfg.Catalog == nil {
		return dependencyOff("model catalog", "the unverified-model list")
	}
	list := c.a.cfg.Catalog.UnverifiedModels()
	sort.Strings(list)
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"models": orEmpty(list),
		"count":  len(list),
		"note": "a model listed here has no confirmed verification date; §4.3 treats an " +
			"unverified capability claim as a defect rather than a warning",
	})
	return nil
}

// ---------------------------------------------------------------------------
// POST /admin/pricing/preview
// ---------------------------------------------------------------------------

type priceComponentView struct {
	Name     string `json:"name"`
	RuleID   string `json:"rule_id"`
	Rate     string `json:"rate"`
	Unit     string `json:"unit"`
	Quantity int64  `json:"quantity"`
	Scale    int32  `json:"scale"`
	Subtotal Money  `json:"subtotal"`
}

type priceView struct {
	Currency string `json:"currency"`

	Marginal     Money `json:"marginal"`
	Subscription Money `json:"subscription"`
	Adjustment   Money `json:"adjustment"`
	// Total is marginal + subscription + adjustment. The notional figure is
	// deliberately not a term: §8.5 keeps it out of the total by construction,
	// and out of the component breakdown too, so that a caller who sums
	// components instead of reading the total still gets the billed figure.
	Total Money `json:"total"`
	// Missing reports that no marginal rule matched. The cost is zero and that
	// is a defect, not free traffic.
	Missing bool `json:"missing"`

	Components []priceComponentView `json:"components"`
	Applied    []map[string]any     `json:"applied_rules"`
	Classes    []map[string]any     `json:"class_traces"`

	Notional notionalView `json:"notional"`
	Notes    []string     `json:"notes"`
}

// notionalView carries the estimate and its provenance together, because §8.5
// requires source and as_of of every notional rule and an estimate that arrives
// without them is a guess wearing a currency symbol.
type notionalView struct {
	// Amount is null when no notional rule matched — never zero.
	Amount     *Money               `json:"amount"`
	Available  bool                 `json:"available"`
	RuleID     string               `json:"rule_id,omitempty"`
	Source     string               `json:"source,omitempty"`
	AsOf       string               `json:"as_of,omitempty"`
	AgeSeconds int64                `json:"age_seconds,omitempty"`
	Components []priceComponentView `json:"components"`
	// Note states the contract in the payload, since this is the field most
	// likely to be read by someone who has not read §8.5.
	Note string `json:"note"`
}

const notionalNote = "an estimate of what this traffic would have cost at the provider's list " +
	"price; never billed, never budgeted, never routed on, and never discounted"

const notionalMissingNote = "no notional_rate rule matched, so the list-rate equivalent is " +
	"unavailable rather than zero; a zero here would make a subscription look infinitely efficient"

func viewPrice(ex PriceExplanation) priceView {
	v := priceView{
		Currency:     orDefault(ex.Currency, "USD"),
		Marginal:     Money(ex.MarginalNano),
		Subscription: Money(ex.SubscriptionNano),
		Adjustment:   Money(ex.AdjustmentNano),
		Total:        Money(ex.TotalNano),
		Missing:      ex.Missing,
		Components:   viewComponents(ex.Components),
		Notes:        orEmpty(ex.Notes),
	}
	for _, a := range ex.Applied {
		v.Applied = append(v.Applied, map[string]any{
			"rule_id":  a.RuleID,
			"class":    a.Class,
			"level":    a.Level,
			"priority": a.Priority,
			"order":    a.Order,
			"why":      a.Why,
		})
	}
	if v.Applied == nil {
		v.Applied = []map[string]any{}
	}
	for _, ct := range ex.Classes {
		considered := make([]map[string]any, 0, len(ct.Considered))
		for _, cd := range ct.Considered {
			considered = append(considered, map[string]any{
				"rule_id":  cd.RuleID,
				"level":    cd.Level,
				"priority": cd.Priority,
				"order":    cd.Order,
				"eligible": cd.Eligible,
				"selected": cd.Selected,
				"reason":   cd.Reason,
			})
		}
		v.Classes = append(v.Classes, map[string]any{
			"class":      ct.Class,
			"considered": considered,
		})
	}
	if v.Classes == nil {
		v.Classes = []map[string]any{}
	}

	n := notionalView{
		RuleID:     ex.Notional.RuleID,
		Source:     ex.Notional.Source,
		AsOf:       ex.Notional.AsOf,
		AgeSeconds: ex.Notional.AgeSeconds,
		Components: viewComponents(ex.Notional.Components),
		Note:       notionalNote,
	}
	if ex.Notional.Missing {
		n.Note = notionalMissingNote
	} else {
		amount := Money(ex.Notional.Nano)
		n.Amount = &amount
		n.Available = true
	}
	v.Notional = n
	return v
}

func viewComponents(cs []PriceComponent) []priceComponentView {
	out := make([]priceComponentView, 0, len(cs))
	for _, c := range cs {
		out = append(out, priceComponentView{
			Name:     c.Name,
			RuleID:   c.RuleID,
			Rate:     c.Rate,
			Unit:     c.Unit,
			Quantity: c.Quantity,
			Scale:    c.Scale,
			Subtotal: Money(c.SubtotalNano),
		})
	}
	return out
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// POST /admin/pricing/preview
func (c *call) adminPricingPreview() error {
	// Deployment configuration and process state, not tenant data: there is
	// no team-scoped view of price explanation, so a scoped
	// administrator is refused rather than served a filtered fiction.
	if err := c.requireGlobal("price explanation"); err != nil {
		return err
	}
	if c.a.cfg.Pricing == nil {
		return dependencyOff("pricing engine", "the price preview")
	}
	var body calculateRequest
	if err := decodeBody(c.w, c.r, &body); err != nil {
		return err
	}
	if strings.TrimSpace(body.Model) == "" {
		return badRequest("model is required").withParam("model")
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

// ---------------------------------------------------------------------------
// POST /admin/config/reload
// ---------------------------------------------------------------------------

func (c *call) adminConfigReload() error {
	// Deployment configuration and process state, not tenant data: there is
	// no team-scoped view of reloading the configuration, so a scoped
	// administrator is refused rather than served a filtered fiction.
	if err := c.requireGlobal("reloading the configuration"); err != nil {
		return err
	}
	if c.a.cfg.Reloader == nil {
		return dependencyOff("configuration reloader", "reload")
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	res, err := c.a.cfg.Reloader.Reload(c.ctx())
	if err != nil {
		return err
	}
	after := map[string]any{
		"version":   res.Version,
		"loaded_at": Stamp(res.LoadedAt),
		"changed":   orEmpty(res.Changed),
		"warnings":  orEmpty(res.Warnings),
		"unchanged": res.Unchanged,
	}
	// A reload is a mutation of what the process is doing, so it is audited
	// like one. The before state is the version that was running, which is the
	// only thing anyone reconstructing an incident will want.
	if err := c.recordAudit("config.reload", "config", res.Version,
		map[string]any{"reloaded": false}, after); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, after)
	return nil
}

// ---------------------------------------------------------------------------
// GET /admin/status
// ---------------------------------------------------------------------------

// adminStatus reports which dependencies this process actually has, so that an
// operator meeting a 501 can confirm the cause in one request instead of
// guessing.
func (c *call) adminStatus() error {
	// Deployment configuration and process state, not tenant data: there is
	// no team-scoped view of process status, so a scoped
	// administrator is refused rather than served a filtered fiction.
	if err := c.requireGlobal("process status"); err != nil {
		return err
	}
	cfg := c.a.cfg
	m := c.a.Metrics()
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"dependencies": map[string]bool{
			"keys":      cfg.Keys != nil,
			"hasher":    cfg.Hasher != nil,
			"directory": cfg.Directory != nil,
			"models":    cfg.Models != nil,
			"budgets":   cfg.Budgets != nil,
			"ledger":    cfg.Ledger != nil,
			"audit":     cfg.Audit != nil,
			// An absent invalidator refuses nothing, so it is the one dependency
			// whose absence is invisible from every other endpoint: the
			// mutations still apply and the fleet honours them a credential
			// cache TTL later instead of within §11.2c's bound. This line is
			// where an operator finds that out.
			"invalidator": cfg.Invalidator != nil,
			"credentials": cfg.Credentials != nil,
			"capacity":    cfg.Capacity != nil,
			"health":      cfg.Health != nil,
			"catalog":     cfg.Catalog != nil,
			"pricing":     cfg.Pricing != nil,
			"reloader":    cfg.Reloader != nil,
		},
		"limits": map[string]any{
			"max_range_hours": int64(cfg.MaxTimeRange.Hours()),
			"max_page_size":   cfg.MaxPageSize,
			"max_list_limit":  cfg.MaxListLimit,
		},
		"metrics": map[string]any{
			"requests":    m.Requests,
			"ui_requests": m.UIRequests,
			// The operator UI mutates now, so the two numbers that describe
			// that are here: what it did, and what it refused for want of proof
			// that the UI itself sent it. A rising second number is a gateway
			// being posted at from somewhere else.
			"ui_mutations":   m.UIMutations,
			"ui_forgeries":   m.UIForgeries,
			"auth_failures":  m.AuthFailures,
			"unimplemented":  m.Unimplemented,
			"server_errors":  m.ServerErrors,
			"mutations":      m.Mutations,
			"audit_failures": m.AuditFailures,
			"invalidations":  m.Invalidations,
			// The number that matters of the two: a deployment where this is
			// non-zero has revocations landing on the credential cache TTL
			// rather than within the published bound.
			"invalidation_failures": m.InvalidationFailures,
			"keys_issued":           m.KeysIssued,
			"range_refusals":        m.RangeRefusals,
			"notional_missing":      m.NotionalMissing,
		},
	})
	return nil
}
