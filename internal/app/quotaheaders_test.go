package app

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
)

// A configured rate limit reaches the client, on the response.
//
// `stampHeaders` rendered `Result.RateLimit` and `Result.QuotaUsedPct`
// faithfully and NOTHING ANYWHERE SET EITHER, so neither family had ever left
// this gateway. COMPATIBILITY §7.8 cited the first as the reason dorang does
// not mirror `x-litellm-key-rpm-limit`, and an OPERATIONS runbook step told an
// operator to read them off a response — a procedure that could not work.
//
// This starts from YAML and reads the headers off a request the assembled
// server answered. A router that stopped filling `Decision.Quota`, or a
// dispatcher that stopped copying it, fails here.
func TestAConfiguredRateLimitReachesTheClient(t *testing.T) {
	const rpmYAML = `
server: {listen: "127.0.0.1:0", master_key_env: DORANG_APP_TEST_MASTER, key_pepper_env: DORANG_APP_TEST_PEPPER}
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - provider: p1
        upstream_model: m1-upstream
        credentials: [c1]
        limits:
          - {metric: rpm, value: 120}
          - {metric: tpm, value: 90000}
`
	a := newWiringApp(t, rpmYAML, nil)
	secret := issueKey(t, a, nil)

	// The upstream is unreachable on purpose. What is asserted is the header
	// block, which is written from the routing decision and is therefore on the
	// response whether or not the provider answered — a client being told how
	// much of its allowance is left matters most when things are failing.
	w := callWith(a, secret, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)

	for name, want := range map[string]string{
		"X-Ratelimit-Limit-Requests":     "120",
		"X-Ratelimit-Remaining-Requests": "120",
		"X-Ratelimit-Limit-Tokens":       "90000",
		"X-Ratelimit-Remaining-Tokens":   "90000",
	} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q (status %d) — internal/server renders this family and "+
				"was never given anything to render", name, got, want, w.Code)
		}
	}
	if reset := w.Header().Get("X-Ratelimit-Reset-Requests"); reset == "" || reset == "0s" {
		t.Errorf("X-Ratelimit-Reset-Requests = %q; 0s or absent tells a client nothing about "+
			"a rolling minute that has most of itself left", reset)
	}
	// `x-dorang-quota-<window>-used-pct` is NOT asserted here, and the reason is
	// the distinction internal/server draws deliberately: the rate-limit set is
	// standard HTTP a client acts on and is attached unconditionally, while the
	// per-window percentage is dorang telemetry and waits for
	// `x-dorang-detail: full`. Its mapping is pinned in
	// TestTheTightestQuotaWinsTheHeader; asserting it on a default response
	// would pin the opposite of the design.
	for k := range w.Header() {
		if strings.HasPrefix(strings.ToLower(k), "x-dorang-quota-") {
			t.Errorf("%s arrived without the detail header; the per-window percentage is "+
				"telemetry and must wait for x-dorang-detail: full", k)
		}
	}
}

// An unmetered credential gets no headers rather than a row of zeroes.
//
// A limit of 0 says the opposite of "no limit", and a client reading
// remaining=0 stops. `wiringYAML` configures no rpm or tpm, so silence is the
// correct answer and is worth pinning — the failure this change closes is a
// header family that says something when it knows nothing.
func TestAnUnmeteredCredentialIsNotRateLimitedToZero(t *testing.T) {
	a := newWiringApp(t, wiringYAML, nil)
	secret := issueKey(t, a, nil)

	w := callWith(a, secret, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)

	for _, name := range []string{
		"X-Ratelimit-Limit-Requests", "X-Ratelimit-Remaining-Requests",
		"X-Ratelimit-Limit-Tokens", "X-Ratelimit-Remaining-Tokens",
	} {
		if got := w.Header().Get(name); got != "" {
			t.Errorf("%s = %q for a credential under no quota at all; absent and zero are "+
				"different facts, and a client reading remaining=0 stops", name, got)
		}
	}
}

// The tightest rule wins when two could fill one header.
//
// A client that obeys the number it is given must not be handed the looser of
// two limits it is simultaneously under, or it paces itself into a refusal
// dorang could have warned it about. Driven at the mapping rather than through
// the server, because two rules on one metric is a shape the config builder
// collapses before the router ever sees it.
func TestTheTightestQuotaWinsTheHeader(t *testing.T) {
	rq := &server.Request{}
	dec := &router.Decision{Credential: "c1"}
	now := time.Now()
	dec.Quota.N = 2
	dec.Quota.Rules[0] = quota.RuleUsage{
		Window: quota.Rolling(time.Minute), Metric: quota.MetricRequests,
		Used: 100, Limit: 10000, ResetAt: now.Add(time.Minute),
	}
	dec.Quota.Rules[1] = quota.RuleUsage{
		Window: quota.Daily, Metric: quota.MetricRequests,
		Used: 95, Limit: 100, ResetAt: now.Add(time.Hour),
	}
	fillQuotaResult(rq, dec)

	if got := rq.Result.RateLimit.RemainingRequests; got != 5 {
		t.Errorf("remaining requests = %d, want 5 — the binding limit is the one with 5 "+
			"left, not the one with 9,900", got)
	}
	if rq.Result.QuotaUsedPct["daily"] != 95 {
		t.Errorf("daily used-pct = %d, want 95; every window is reported even when only one "+
			"of them can reach x-ratelimit-*", rq.Result.QuotaUsedPct["daily"])
	}
}

// A limit of zero is skipped rather than reported as full.
//
// A percentage of zero is undefined, not 100, and a rule carrying no limit is
// how a provider-reported window with no stated allowance arrives (§6.2).
func TestAZeroLimitIsNotAHundredPercent(t *testing.T) {
	rq := &server.Request{}
	dec := &router.Decision{Credential: "c1"}
	dec.Quota.N = 1
	dec.Quota.Rules[0] = quota.RuleUsage{
		Window: quota.Daily, Metric: quota.MetricRequests, Used: 7, Limit: 0,
	}
	fillQuotaResult(rq, dec)

	if rq.Result.RateLimit.Set {
		t.Error("a rule with no stated limit produced a rate-limit header set")
	}
	if len(rq.Result.QuotaUsedPct) != 0 {
		t.Errorf("used-pct reported %v against a limit nobody stated", rq.Result.QuotaUsedPct)
	}
}
