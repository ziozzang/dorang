package app

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/admin"
	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/metrics"
	"github.com/ziozzang/dorang/internal/server"
)

// buildMetrics assembles the DESIGN §12.3 surface.
//
// It is registration and nothing else. Every number here already existed
// somewhere — internal/capacity has published a snapshot since it was written,
// internal/meter has counted its own drops since §12.1 required them to be
// visible — and none of those packages knows this file exists. That is the
// direction the dependency has to run: a subsystem that imports a metrics
// library cannot be tested without one, and a counter that lives in two places
// drifts.
//
// The one exception is [metrics.Requests], which owns the per-model,
// per-provider, per-credential request families. Nothing owned those: the
// request path knew the tuple for one instant and then discarded it.
func (a *App) buildMetrics(cfg *config.Config) *metrics.Registry {
	reg := metrics.New(a.now)

	reg.Register(metrics.NewBuildCollector(Version, Commit, a.now))

	a.requests = metrics.NewRequests(metrics.RequestsOptions{})
	a.applyMetricsConfig(cfg)
	reg.Register(a.requests)

	if a.Broker != nil {
		reg.Register(metrics.NewCapacityCollector(a.Broker, 0))
	}
	if a.Health != nil {
		reg.Register(metrics.NewHealthCollector(a.Health, 0))
	}
	// Registered only when cache-affinity routing is configured. With it off
	// there is no table to read, and a hit ratio of 0.0 would say the cache
	// never helps rather than that there is no cache (DESIGN §12.3, and the
	// `/load` trap of VLLM.md §3.3).
	if a.Prefix != nil {
		reg.Register(metrics.NewPrefixCollector(a.Prefix, cfg.Routing.Prefix.MaxBytes.Bytes()))
	}
	if a.Meter != nil {
		reg.Register(metrics.NewMeterCollector(a.Meter))
	}
	if qc := quotaCollector(a.quota, a.now); qc != nil {
		reg.Register(qc)
	}
	if a.Ledger != nil {
		reg.Register(metrics.NewBudgetCollector(a.Ledger, a.now, 0))
	}
	// Registered only when a transform filter is configured: a page of zeroed
	// masking counters would say the filter found nothing, not that there is no
	// filter (DESIGN §12.3's rule 3).
	if a.dispatch != nil {
		if st := a.dispatch.state(); st != nil && st.filters != nil && len(st.filters.byModel) > 0 {
			reg.Register(a.dispatch.filterMetrics())
		}
	}
	reg.Register(a.clusterCollector(cfg))
	if a.Auth != nil {
		reg.Register(metrics.NewAuthCollector(a.Auth, cfg.Auth.RehashesOnUse(),
			legacySunset(cfg), a.now))
	}
	// Registered only when a credential authenticates by OAuth. A refresh loop
	// that has been failing for three days is invisible until the token it
	// failed to replace expires, and these are the numbers that predict it — but
	// a page of zeroed refresh counters on a deployment with no OAuth credential
	// would say the refreshes are not happening rather than that there is
	// nothing to refresh (DESIGN §12.3's rule 3).
	if a.OAuth != nil {
		reg.Register(metrics.NewOAuthCollector(a.OAuth, a.now, 0))
	}
	if a.Store != nil {
		reg.Register(metrics.NewStoreCollector(a.Store, cfg.Storage.Driver))
	}
	if a.Batch != nil {
		reg.Register(metrics.NewBatchCollector(a.Batch))
	}
	// Shadow is nil for the default `off` mode, and then the whole family is
	// absent: a page of zeroed shadow counters is exactly the "clean report"
	// DESIGN §14.1 warns must not be mistaken for evidence.
	if a.Shadow != nil {
		reg.Register(metrics.NewShadowCollector(a.Shadow))
	}
	return reg
}

// applyMetricsConfig hands [metrics.Requests] the two facts about the running
// configuration it cannot pull for itself.
//
// It is called from [App.buildMetrics] AND from [App.Reload], and the second
// call is the one that matters. The registry is built once, at assembly, and a
// reload does not rebuild it — so a fact copied into it only at start-up is a
// fact that stops being true the first time an operator edits the file:
//
//   - The admitted model set is what bounds the `model` label. A bound fixed at
//     start-up is a bound that a config reload adding a model finds already
//     spent, and the resulting fold does not heal without a restart. That is the
//     exact symptom this wiring exists to prevent, and
//     TestModelLabelAdmissionFollowsAReload is what says the call is here.
//   - `routing.prefix.enabled` gates dorang_prefix_hit_ratio. It hot-reloads
//     like every other section (§4.1) — the dispatcher's prefixOn is swapped on
//     every reload — and this gate did not follow it, so turning cache-affinity
//     routing OFF left the ratio published against a table that had stopped
//     being consulted, and turning it ON left the ratio absent on a gateway that
//     was using it.
func (a *App) applyMetricsConfig(cfg *config.Config) {
	if a.requests == nil {
		return
	}
	a.requests.SetAdmittedModels(clientFacingModelNames(cfg))
	a.requests.SetPrefixEnabled(cfg.Routing.Prefix.IsEnabled())
}

// metricsAccess renders observability.prometheus and observability.metrics.public
// onto the HTTP surface's access rule.
//
// Both were unread before this. `prometheus: false` changed nothing, and
// /metrics served per-key spend, per-credential quota state and the whole model
// list to anyone who could reach the port — on a gateway whose entire reason for
// existing is that those numbers are worth keeping.
func metricsAccess(cfg *config.Config) server.MetricsAccess {
	switch {
	case !cfg.Observability.PrometheusEnabled():
		return server.MetricsOff
	case cfg.Observability.MetricsPublic():
		return server.MetricsPublic
	}
	return server.MetricsAdmin
}

// healthReporters are the subsystems that contribute to GET /health.
//
// Metering is the one that matters: DESIGN §12.1 says a drop is never silent,
// and until this existed the meter tracked five degradation reasons with
// hysteresis and nothing anywhere read the result.
func (a *App) healthReporters() []server.HealthReporter {
	var out []server.HealthReporter
	if a.Meter != nil {
		out = append(out, &meterHealth{m: a.Meter})
	}
	if a.Node != nil && a.Node.Enabled() {
		out = append(out, &clusterHealth{a: a})
	}
	return out
}

// clusterHealth reports this node's membership into /health.
//
// It exists because [App.noteConflict] flips readiness through
// [server.Server.StartDrain], which is the only lever internal/app has on the
// signal a load balancer polls — and which has one word for two states. A
// disqualified node and a node shutting down both report `draining`, and an
// operator has to be able to tell them apart: one wants a rollback of the
// node_id, the other wants no action at all.
//
// It is registered only for a clustered gateway. `cluster.enabled: false`
// cannot produce a conflict — nothing registers, so nothing can be superseded —
// and an object that is always `false` on the notebook tier is a field nobody
// reads on the deployment §0.2 exists for.
type clusterHealth struct{ a *App }

// HealthName implements server.HealthReporter.
func (h *clusterHealth) HealthName() string { return "cluster" }

// Health implements server.HealthReporter.
//
// Like [meterHealth] it always reports, so that "this build does not say" and
// "this node is fine" are distinguishable. Unlike it, the condition it reports
// HAS already changed the status code, by the separate act of starting the
// drain; this is the explanation beside it, not the mechanism.
func (h *clusterHealth) Health(dst []byte) []byte {
	dst = append(dst, `{"node":`...)
	dst = appendJSONString(dst, h.a.Node.ID())
	dst = append(dst, `,"leader":`...)
	dst = strconv.AppendBool(dst, h.a.Node.IsLeader())
	dst = append(dst, `,"disqualified":`...)
	disq := h.a.Disqualified()
	dst = strconv.AppendBool(dst, disq)
	if disq {
		if err := h.a.Node.Conflict(); err != nil {
			dst = append(dst, `,"reason":`...)
			dst = appendJSONString(dst, err.Error())
		}
	}
	return append(dst, '}')
}

// appendJSONString appends a JSON string literal. internal/server has its own
// copy for the same reason: neither package may import the other, and a health
// body that fails to parse because a node id contained a quote is worse than no
// body at all.
func appendJSONString(dst []byte, s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		return append(dst, `""`...)
	}
	return append(dst, b...)
}

// meterHealth reports the metering pipeline's degraded state into /health.
type meterHealth struct{ m *meter.Meter }

// HealthName implements server.HealthReporter.
func (h *meterHealth) HealthName() string { return "metering" }

// Health implements server.HealthReporter.
//
// It always reports, degraded or not. An operator checking whether metering is
// healthy must be able to tell "not degraded" from "this build does not report
// it", and an object that appears only on failure cannot answer that — the same
// reason the metrics collector emits the gauge at 0 rather than omitting it.
//
// It never changes the status code. Losing trace payloads is a data-quality
// failure, not a serving failure, and taking the pod out of rotation for it
// would turn a metering incident into an outage.
func (h *meterHealth) Health(dst []byte) []byte {
	st := h.m.Stats()
	dst = append(dst, `{"degraded":`...)
	dst = strconv.AppendBool(dst, st.Degraded)
	dst = append(dst, `,"reason":"`...)
	dst = append(dst, st.Reason.String()...)
	dst = append(dst, `","dropped":`...)
	dst = strconv.AppendInt(dst, st.TracesDropped, 10)
	dst = append(dst, `,"spool_bytes":`...)
	dst = strconv.AppendInt(dst, st.SpoolBytes, 10)
	return append(dst, '}')
}

// Version and Commit are stamped at link time by the Makefile and read by
// `dorang_build_info`. They are variables rather than constants for exactly
// that reason.
var (
	Version = ""
	Commit  = ""
)

// quotaCollector tracks every credential that has quota rules. A credential
// with none is not tracked, so it contributes no series rather than a row of
// zeroes against limits nobody set.
func quotaCollector(qs *quotaSet, now func() time.Time) *metrics.QuotaCollector {
	if qs == nil || len(qs.meters) == 0 {
		return nil
	}
	c := metrics.NewQuotaCollector(now, 0)
	for credential, m := range qs.meters {
		if m == nil {
			continue
		}
		c.Track(credential, m)
	}
	if c.Len() == 0 {
		return nil
	}
	return c
}

// clusterCollector publishes DESIGN §5.6's overshoot figures as numbers.
//
// The limit the figures are computed against is the largest configured
// concurrency ceiling. §5.6 requires a published maximum overshoot; publishing
// it against the tightest limit would understate the fleet-wide risk, and
// against an arbitrary one would be meaningless, so it is the widest ceiling —
// the one whose overshoot costs the most.
//
// The node count is READ, not assumed. Every figure here is
// `something × (nodes − 1)`, so the 1 this used to hardcode published a
// maximum overshoot of 0 — "exact" — from every node of every cluster,
// including the deployments the mode exists for. An operator sizes against
// that number; a bound that is wrong in the multi-node case is worse than no
// bound, because no bound is not acted on.
func (a *App) clusterCollector(cfg *config.Config) *metrics.ClusterCollector {
	mode, err := cluster.ParseMode(cfg.Cluster.CapacityMode)
	if err != nil {
		mode = cluster.ModeLocal
	}
	c := &metrics.ClusterCollector{
		Enabled: cfg.Cluster.Enabled,
		NodeID:  nodeID(cfg),
		Mode:    mode,
		Params: cluster.Params{
			Limit:       widestCapacityLimit(cfg),
			Nodes:       1,
			MinLeasable: int64(cfg.Cluster.MinLeasable),
			Clustered:   cfg.Cluster.Enabled,
		},
	}
	// shared-redis is refused by internal/config, so the comparison set must
	// not offer it. The whole reason every mode is published rather than only
	// the configured one is that an operator can see the cost of a different
	// choice before making it — and "exact, one round trip to Redis" next to a
	// loader that rejects the value is worse than not showing the row.
	c.Unavailable = map[cluster.Mode]error{cluster.ModeSharedRedis: cluster.ErrNoRedisClient}

	if a.Node == nil {
		return c
	}
	// The block the leased figure multiplies is the node's, so the published
	// number is the one the coordinator would actually overshoot by rather
	// than internal/cluster's default standing in for it.
	if p, perr := a.Node.AccuracyParams(context.Background(), c.Params.Limit); perr == nil {
		c.Params.Block = p.Block
	}
	if !a.Node.Enabled() {
		// No election and no peers. Leader and Term stay nil so the gauges are
		// absent: an unclustered process is not "not the leader", and a term of
		// 0 is a claim about an election that never happened.
		return c
	}
	c.Nodes = a.nodes.get
	c.Leader = a.Node.IsLeader
	c.Term = a.Node.Election().Term
	return c
}

// widestCapacityLimit is the largest configured concurrency ceiling, or 0 when
// nothing is capped.
func widestCapacityLimit(cfg *config.Config) int64 {
	var m int64
	consider := func(v int) {
		if int64(v) > m {
			m = int64(v)
		}
	}
	if cfg.Capacity.Global != nil {
		consider(cfg.Capacity.Global.MaxConcurrency)
	}
	for _, l := range cfg.Capacity.ProviderGroups {
		consider(l.MaxConcurrency)
	}
	for _, l := range cfg.Capacity.CredentialGroups {
		consider(l.MaxConcurrency)
	}
	for _, mc := range cfg.Capacity.Models {
		consider(mc.Limits.MaxConcurrency)
	}
	for _, l := range cfg.Capacity.Principals {
		consider(l.MaxConcurrent)
	}
	return m
}

// legacySunset is auth.legacy.until, or the zero time when no legacy window is
// configured — in which case the countdown gauge is omitted rather than shown
// as a negative number of seconds.
func legacySunset(cfg *config.Config) time.Time {
	if !cfg.Auth.Legacy.Enabled {
		return time.Time{}
	}
	t, err := time.Parse(time.DateOnly, cfg.Auth.Legacy.Until)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, cfg.Auth.Legacy.Until); err != nil {
			return time.Time{}
		}
	}
	return t.UTC()
}

// recordMetrics feeds one finished request into the DESIGN §12.3 families.
//
// This is internal/server's existing observation point: Server.finish hands
// every request to the meter after the client's last byte, health probes and
// the metrics scrape included. Nothing new is measured and nothing is measured
// twice.
func (a *App) recordMetrics(ev *server.Event) {
	r := &ev.Result
	// Retain inference metadata only; console and health requests must not flood it.
	if ev.Model != "" || r.Deployment != "" {
		a.traffic.Record(admin.LiveRequest{At: a.now(), ID: ev.RequestID, Model: ev.Model, Provider: r.Provider, Deployment: r.Deployment, Credential: r.Credential, Key: ev.KeyID, Team: ev.TeamID, User: ev.UserID, Endpoint: ev.Route, Status: ev.Status, Error: r.NativeErrorType, LatencyMS: ev.DurationNS / 1_000_000, TTFTMS: r.TTFTNS / 1_000_000, WaitMS: r.QueueNS / 1_000_000, Input: r.Tokens.Input, Output: r.Tokens.Output, CacheRead: r.Tokens.CacheRead, CacheWrite: r.Tokens.CacheWrite, Reasoning: r.Tokens.Reasoning, Cost: admin.Money(r.CostNanoUSD), Priced: r.Priced, Streamed: ev.Streamed})
	}
	// The token half of the rolling minute, for every subject the request
	// belonged to. The request half was counted at the gate, because a ceiling
	// enforced only on FINISHED requests cannot refuse a burst; tokens are not
	// known until here, so they land here. All three subjects, because a team's
	// tpm_limit is a statement about the team.
	if a.rates != nil {
		if n := totalTokens(r.Tokens); n > 0 {
			a.rates.recordTokens(ev.KeyID, ev.UserID, ev.TeamID, n)
		}
	}
	if a.requests == nil {
		return
	}
	a.requests.Observe(metrics.Sample{
		Model:        ev.Model,
		Provider:     r.Provider,
		Credential:   r.Credential,
		Endpoint:     ev.Route,
		Status:       ev.Status,
		Duration:     time.Duration(ev.DurationNS),
		TTFT:         time.Duration(r.TTFTNS),
		CapacityWait: time.Duration(r.QueueNS),
		Tokens: metrics.Tokens{
			Input:      r.Tokens.Input,
			Output:     r.Tokens.Output,
			CacheRead:  r.Tokens.CacheRead,
			CacheWrite: r.Tokens.CacheWrite,
			Reasoning:  r.Tokens.Reasoning,
		},
		CostNano:       r.CostNanoUSD,
		Priced:         r.Priced,
		NotionalNano:   r.NotionalNanoUSD,
		NotionalPriced: r.NotionalPriced,
		Routed:         r.Deployment != "",
		PrefixHit:      strings.HasPrefix(r.RouteReason, prefixHitPrefix),
		FallbackFrom:   r.FallbackFromDeployment,
		FallbackTo:     r.Deployment,
		FallbackReason: r.FallbackFrom,
		Deployment:     r.Deployment,
		// Empty on every request an upstream answered honestly, which is what
		// keeps dorang_model_substitutions_total absent rather than zero on a
		// healthy gateway (DESIGN §17.1 rule 3).
		ServedModel: r.ServedModel,
	})
}

// totalTokens is how many tokens a finished request consumed. It is THE
// definition on this side of the process: the tokens-per-minute ceiling, the
// token guard of §11.6 and the metric all read it, and nothing computes its own.
//
// Input plus output, and nothing else. CacheRead and CacheWrite are the parts of
// Input that came from and went to cache, and Reasoning is the part of Output
// that was reasoning; adding any of them again counts the cached prefix and the
// reasoning tokens twice. Same rule as canonical.Usage.TotalTokens,
// meter.Tokens.Total and server.normalizeUsage — that is the point, and a
// second rule anywhere is how the ledger came to disagree with the wire.
//
// Total is consulted first only because every producer now sets it to exactly
// this sum (server.normalizeUsage on the passthrough path, the dispatcher on
// every other), so the two branches cannot disagree; TestTokenTotalHasOneRule
// pins that they do not. It used to be a genuinely different rule — a stated
// upstream total was preferred here and re-derived in the ledger — which made
// this the second of the three answers to "how many tokens was that".
func totalTokens(u server.Usage) int64 {
	if u.Total > 0 {
		return u.Total
	}
	return u.Input + u.Output
}

// prefixHitPrefix is what internal/router stamps on a decision made by cache
// affinity: "prefix_hit:depth=N". Matching the prefix rather than the whole
// string keeps the depth out of the label, which is the difference between one
// series per model and one per model per chain depth.
const prefixHitPrefix = "prefix_hit"
