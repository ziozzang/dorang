package router

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// clock is an injectable clock that is safe under -race.
type clock struct{ ns atomic.Int64 }

var epoch = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

func newClock() *clock {
	c := &clock{}
	c.ns.Store(epoch.UnixNano())
	return c
}

func (c *clock) now() time.Time            { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) advance(d time.Duration)   { c.ns.Add(int64(d)) }
func (c *clock) set(t time.Time)           { c.ns.Store(t.UnixNano()) }
func (c *clock) nowFunc() func() time.Time { return c.now }

// stubQuota is a QuotaSource whose answers a test states directly.
type stubQuota map[string]quota.Decision

func (s stubQuota) Check(cred string, _ time.Time) quota.Decision {
	if d, ok := s[cred]; ok {
		return d
	}
	return quota.Decision{Allow: true}
}

// Fill reports the stated decision as a one-rule view, so a test that says
// "this credential is at 900 of 1000" gets headers describing that and not an
// empty view that would silently omit them.
func (s stubQuota) Fill(cred string, _ time.Time, dst *quota.View) {
	if dst == nil {
		return
	}
	dst.N, dst.Truncated = 0, false
	d, ok := s[cred]
	if !ok || d.Limit == 0 {
		return
	}
	dst.Rules[0] = quota.RuleUsage{
		Window: d.Rule.Window, Metric: d.Rule.Metric,
		Used: d.Used, Limit: d.Limit, ResetAt: d.ResetAt,
	}
	dst.N = 1
}

// harness wires a router to real subsystems with an injected clock.
type harness struct {
	t        *testing.T
	clock    *clock
	broker   *capacity.Broker
	health   *health.Tracker
	prefix   *prefix.Table
	interner *prefix.Interner
	quota    stubQuota
	r        *Router
}

type harnessOpts struct {
	capacity capacity.Config
	health   health.Options
	prefixOn bool
	prefixTL time.Duration
	pricing  string
}

// newHarnessWithCatalog wires a router that has a model catalog, which is the
// only way to reach the branch of [Router.compile] that FILLS a window or an
// output ceiling rather than reading one from configuration.
func newHarnessWithCatalog(t *testing.T, cfg Config, cat *catalog.Catalog) *harness {
	t.Helper()
	return newHarnessWith(t, cfg, harnessOpts{}, func(d *Deps) { d.Catalog = cat })
}

func newHarness(t *testing.T, cfg Config, o harnessOpts) *harness {
	t.Helper()
	return newHarnessWith(t, cfg, o, nil)
}

func newHarnessWith(t *testing.T, cfg Config, o harnessOpts, extra func(*Deps)) *harness {
	t.Helper()
	h := &harness{t: t, clock: newClock(), quota: stubQuota{}}

	o.capacity.Now = h.clock.now
	if o.capacity.SweepInterval == 0 {
		o.capacity.SweepInterval = -1
	}
	h.broker = capacity.New(o.capacity)
	t.Cleanup(h.broker.Close)

	o.health.Now = h.clock.now
	h.health = health.New(o.health)
	h.interner = prefix.NewInterner()

	deps := Deps{Capacity: h.broker, Health: h.health, Interner: h.interner, Quota: h.quota}
	if o.prefixOn {
		ttl := o.prefixTL
		if ttl == 0 {
			ttl = time.Hour
		}
		h.prefix = prefix.NewTable(prefix.Options{TTL: ttl, Now: h.clock.now})
		deps.Prefix = h.prefix
	}
	if o.pricing != "" {
		c, err := pricing.ParseCatalog([]byte(o.pricing))
		if err != nil {
			t.Fatalf("pricing catalog: %v", err)
		}
		deps.Pricing = c
	}
	if extra != nil {
		extra(&deps)
	}

	cfg.Now = h.clock.now
	r, err := New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.r = r
	return h
}

func (h *harness) route(req Request) *Decision {
	h.t.Helper()
	d, err := h.r.Route(context.Background(), req)
	if err != nil {
		h.t.Fatalf("Route(%s): unexpected error: %v", req.Model, err)
	}
	return d
}

func (h *harness) routeErr(req Request) *Error {
	h.t.Helper()
	d, err := h.r.Route(context.Background(), req)
	if err == nil {
		h.t.Fatalf("Route(%s): expected an error, got deployment %s", req.Model, d.Deployment)
	}
	re, ok := err.(*Error)
	if !ok {
		h.t.Fatalf("Route(%s): expected *router.Error, got %T: %v", req.Model, err, err)
	}
	return re
}

// ok reports a successful attempt.
func (h *harness) ok(d *Decision) { h.r.Report(d, Outcome{Total: 10 * time.Millisecond}) }

// fail reports a failed attempt with an explicit cause.
func (h *harness) fail(d *Decision, c Cause) {
	h.r.Report(d, Outcome{Err: errFake, Cause: c, Total: 5 * time.Millisecond})
}

type fakeErr struct{}

func (fakeErr) Error() string { return "upstream said no" }

var errFake error = fakeErr{}

// dep builds a deployment with one credential of the same name.
func dep(id, provider, kind, model string, creds ...string) Deployment {
	d := Deployment{ID: id, Provider: provider, Kind: kind, UpstreamModel: model}
	if len(creds) == 0 {
		creds = []string{id + "-key"}
	}
	for _, c := range creds {
		d.Credentials = append(d.Credentials, Credential{ID: c})
	}
	return d
}

// prefixFor computes a request's hash chain the way the frontend would, seeded
// with the model group so two groups can never share an entry.
func prefixFor(group, body string) []prefix.Digest {
	return prefix.Compute("t", group, []byte(body), 0)
}
