package perf

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/backend"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/store"
	"github.com/ziozzang/dorang/testing/fake"
)

// -----------------------------------------------------------------------------
// the assembled gateway under load
// -----------------------------------------------------------------------------

// These are test fixtures, not secrets.
const (
	testPepper    = "perf-pepper-not-a-real-secret"     // pragma: allowlist secret — test fixture
	testMasterKey = "perf-master-key-not-a-real-secret" // pragma: allowlist secret — test fixture
	testUpstream  = "perf-upstream-key"                 // pragma: allowlist secret — test fixture
)

// Model is the client-facing name every measured request asks for.
const Model = "chat-large"

// axisCeiling is what every capacity axis is set to. It is well above any load
// this harness offers, so the broker acquires and releases on every request
// without a single one ever queuing — which is the warm-local profile's "local
// capacity" and not "saturated capacity".
const axisCeiling = 4096

// UpstreamModel is the name on the wire to the fake, which is not the client's
// name: §7.2 puts the client's name in the answer and the real upstream id on
// the wire, and a harness that used one string for both would never exercise
// the rewrite.
const UpstreamModel = "qwen3.5:397b"

// Opts configures [NewGateway].
type Opts struct {
	// UpstreamLatency is the fake backend's fixed think time. It is the
	// instrument §15.1's denominator argument needs: with a known delay in the
	// upstream, a measurement that reports the gateway's overhead unchanged is
	// a measurement that has actually separated the two.
	UpstreamLatency time.Duration
	// InterFrame is the fake's per-frame delay on the streaming arms.
	InterFrame time.Duration
	// Frames is how many text chunks a streamed answer is cut into.
	Frames int
	// Prefix enables prefix affinity, which the warm-local profile has on.
	Prefix bool
	// Metering turns the numeric and trace paths on, which the warm-local
	// profile also has on: §15.1's budget explicitly includes "enqueueing
	// metering".
	Metering bool
	// PricingRules is how many marginal rules the catalog holds. §15.3 fixes
	// this at 100; a catalog with one rule makes the price index free.
	PricingRules int
	// Deployments is how many deployments the model group has. More than one
	// exercises the routing strategy rather than a single-candidate shortcut.
	Deployments int
	// Keys is how many api keys are issued. The auth snapshot is a hash lookup
	// and one key does not measure it.
	Keys int
	// MaxConcurrent bounds the principal axis. Zero leaves it unlimited, which
	// is what "local capacity, not saturated" means.
	MaxConcurrent int
	// Logf receives the gateway's diagnostics. Nil discards them, which is what
	// a measurement wants: a t.Logf under load is a mutex in the hot path.
	Logf func(string, ...any)
}

// Gateway is an assembled dorang serving over a real socket, with a fake
// upstream on the other side and a timing tap on both boundaries.
type Gateway struct {
	App       *app.App
	Front     *httptest.Server
	Collector *Collector
	Upstreams []*fake.Upstream
	// Secrets are the issued api key tokens.
	Secrets []string
	// Client is a keep-alive client for the driver.
	Client *http.Client

	url string
}

// UpstreamCount is how many requests the fakes received.
func (g *Gateway) UpstreamCount() int {
	n := 0
	for _, u := range g.Upstreams {
		n += u.Count()
	}
	return n
}

// ResetUpstreams forgets what the fakes recorded.
//
// It is not tidiness. [fake.Upstream] keeps every request it has served —
// verbatim body, cloned header — which is exactly right for a scenario
// asserting on what reached the wire and is a memory bomb under load: a hundred
// thousand 4 KiB requests is a gigabyte of retained harness bookkeeping, and it
// was showing up in this package's own footprint figures as though the gateway
// had leaked it. Every arm resets before it starts and before it reports.
func (g *Gateway) ResetUpstreams() {
	for _, u := range g.Upstreams {
		u.Reset()
	}
}

var keySeq atomic.Int64

// NewGateway assembles the whole gateway: config, store, catalog, capacity,
// health, prefix, pricing, quota, auth, meter, router and HTTP surface.
//
// It is [app.New] and not a hand-built composition. A load harness that
// assembled its own request path would measure a path production does not have
// — which is exactly the defect DESIGN §17.1 records, where a second dispatch
// path carried four fixes production never got with a green suite either side
// of it.
func NewGateway(t testing.TB, o Opts) *Gateway {
	t.Helper()
	if o.Deployments <= 0 {
		o.Deployments = 2
	}
	if o.Keys <= 0 {
		o.Keys = 64
	}
	if o.Frames <= 0 {
		o.Frames = 16
	}
	if o.PricingRules <= 0 {
		o.PricingRules = 100
	}

	dir := t.TempDir()
	t.Setenv(config.EnvStateDir, dir)
	t.Setenv("DORANG_PERF_UPSTREAM_KEY", testUpstream)

	g := &Gateway{Collector: NewCollector()}

	// One fake per deployment, so a routing decision that moves traffic is
	// visible as a different socket rather than as nothing at all.
	base := o.UpstreamLatency
	inter := o.InterFrame
	frames := o.Frames
	var urls []string
	for i := range o.Deployments {
		u := fake.New(fake.Options{
			Shape: fake.ShapeOpenAI,
			Name:  "up" + strconv.Itoa(i),
			Script: func(r *fake.Recorded) fake.Script {
				return fake.Script{
					Model: r.Model,
					Text:  perfAnswer,
					// A streamed answer arrives in frames. One frame is not a
					// stream: it never exercises the partial-frame carry that
					// §15.2.3's scanner exists for.
					TextChunks: frames,
					Usage:      fake.Usage{InputTokens: 812, OutputTokens: 133, CacheReadTokens: 640},
				}
			},
			Behaviour: func(*fake.Recorded) fake.Behaviour {
				return fake.Behaviour{Latency: base, InterFrame: inter}
			},
		})
		t.Cleanup(u.Close)
		g.Upstreams = append(g.Upstreams, u)
		urls = append(urls, u.URL)
	}

	cfg, err := config.LoadBytes([]byte(gatewayYAML(o, urls)))
	if err != nil {
		t.Fatalf("perf: config: %v", err)
	}
	cfg.Storage.SQLite.Path = filepath.Join(dir, "dorang.db")
	cfg.Metering.Spool.Dir = filepath.Join(dir, "spool")
	if o.PricingRules > 0 {
		cfg.Pricing.Rules = pricingRules(o.PricingRules)
	}
	t.Setenv(cfg.Server.KeyPepperEnv, testPepper)
	t.Setenv(cfg.Server.MasterKeyEnv, testMasterKey)

	a, err := app.New(context.Background(), app.Options{
		Config:   cfg,
		Upstream: UpstreamClient(backend.NewClient()),
		Logf:     o.Logf,
	})
	if err != nil {
		t.Fatalf("perf: app.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = a.Close(ctx)
	})
	g.App = a

	for range o.Keys {
		g.Secrets = append(g.Secrets, issueKey(t, a))
	}

	g.Front = httptest.NewServer(NewHandler(a.Server, g.Collector.Sink))
	t.Cleanup(g.Front.Close)
	g.url = g.Front.URL + "/v1/chat/completions"
	g.Client = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        1024,
			MaxIdleConnsPerHost: 1024,
			DisableCompression:  true,
		},
	}
	return g
}

// perfAnswer is the assistant text every fake answer carries. It is long enough
// that a streamed answer has something to cut into frames and short enough that
// the measurement is not about memcpy.
const perfAnswer = "The gateway overhead budget is stated per profile because a single " +
	"latency number is unfalsifiable, and the boundary is fixed so the figure means one thing."

func issueKey(t testing.TB, a *app.App) string {
	t.Helper()
	token := "sk-perf-" + strconv.FormatInt(keySeq.Add(1), 36) // pragma: allowlist secret — test fixture
	k := &store.APIKey{ID: "key-" + token, TeamID: "team-1"}
	if err := a.Store.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatalf("perf: NewAPIKeyFromToken: %v", err)
	}
	if err := a.Store.InsertAPIKey(context.Background(), k); err != nil {
		t.Fatalf("perf: InsertAPIKey: %v", err)
	}
	return token
}

// gatewayYAML renders the configuration. It is a literal rather than a fixture
// file because the upstream URLs are only known once the fakes are listening.
func gatewayYAML(o Opts, urls []string) string {
	var b strings.Builder
	b.WriteString("version: 1\n")
	b.WriteString("server:\n  env: development\n  request_timeout: 60s\n")
	b.WriteString("storage:\n  driver: sqlite\n")
	b.WriteString("cluster:\n  enabled: false\n  capacity_mode: local\n")

	// Every axis of §5.1 is given a real ceiling, sized far above the offered
	// load. That is deliberate: an unconfigured axis is not acquired at all, so
	// a harness that left them open would report a broker that was never asked
	// a question, and "the multi-axis broker does not contend" would be a
	// statement about a broker with no axes.
	b.WriteString("providers:\n")
	for i, u := range urls {
		fmt.Fprintf(&b, "  - {name: p%d, kind: vllm, base_url: %q, timeout: 60s, "+
			"max_concurrency: %d, capacity_group: pool}\n", i, u, axisCeiling)
	}
	b.WriteString("credentials:\n")
	for i := range urls {
		fmt.Fprintf(&b, "  - {id: c%d, provider: p%d, key_env: DORANG_PERF_UPSTREAM_KEY, "+
			"capacity_group: acct-%d}\n", i, i, i)
	}

	b.WriteString("capacity:\n")
	fmt.Fprintf(&b, "  provider_groups: {pool: {max_concurrency: %d}}\n", axisCeiling)
	b.WriteString("  credential_groups:\n")
	for i := range urls {
		fmt.Fprintf(&b, "    acct-%d: {max_concurrency: %d}\n", i, axisCeiling)
	}
	b.WriteString("  models:\n")
	for i := range urls {
		fmt.Fprintf(&b, "    - {provider: p%d, model: %s, max_concurrency: %d}\n",
			i, UpstreamModel, axisCeiling)
	}
	b.WriteString("  principals:\n    default: {max_concurrent: ")
	if o.MaxConcurrent > 0 {
		fmt.Fprintf(&b, "%d", o.MaxConcurrent)
	} else {
		fmt.Fprintf(&b, "%d", axisCeiling)
	}
	b.WriteString(", max_queue_wait: 30s}\n")

	b.WriteString("models:\n  - name: " + Model + "\n    class: large\n")
	b.WriteString("    strategy: [prefix_sticky, lowest_cost, least_busy]\n")
	b.WriteString("    deployments:\n")
	for i := range urls {
		fmt.Fprintf(&b, "      - {provider: p%d, upstream_model: %s, credentials: [c%d]}\n", i, UpstreamModel, i)
	}

	b.WriteString("classes: {large: [" + Model + "]}\n")
	fmt.Fprintf(&b, "routing:\n  prefix:\n    enabled: %v\n    chunk_bytes: 4096\n"+
		"    checkpoints: logarithmic\n    max_bytes: 64MiB\n    ttl: 1h\n", o.Prefix)

	fmt.Fprintf(&b, "metering:\n  numeric: {enabled: %v}\n", o.Metering)
	if o.Metering {
		b.WriteString("  trace: {store_messages: truncated, truncate_chars: 512, sample_rate: 1.0}\n")
	} else {
		b.WriteString("  trace: {store_messages: none, sample_rate: 0}\n")
	}
	b.WriteString("  flush_interval: 250ms\n")

	b.WriteString("observability: {prometheus: true, log_level: error}\n")
	b.WriteString("extensions:\n  lua:\n    enabled: false\n")
	return b.String()
}

// pricingRules builds n marginal rules inline, so no file has to exist.
//
// §8.2 indexes rules by their static dimensions at load: a request evaluates
// only the handful that can possibly apply to it. Most of these name a
// deployment this workload never touches, which is what makes the index the
// thing under measurement.
func pricingRules(n int) []config.PricingRule {
	out := make([]config.PricingRule, 0, n)
	for i := range n {
		match := config.PricingMatch{Deployment: "dep-" + strconv.Itoa(i)}
		if i == 0 {
			// Exactly one rule can apply to the traffic this harness sends. The
			// other n-1 are the index's job.
			match = config.PricingMatch{Model: UpstreamModel}
		}
		out = append(out, config.PricingRule{
			ID:    "r" + strconv.Itoa(i),
			Class: config.PricingMarginalUsage,
			Match: match,
			Rates: map[string]config.Decimal{
				"input":  config.Decimal(fmt.Sprintf("%d.%02d", i%7, i%100)),
				"output": config.Decimal(fmt.Sprintf("%d.%02d", i%5+1, (i*7)%100)),
			},
		})
	}
	return out
}
