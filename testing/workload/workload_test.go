package workload

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/store"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/testing/fake"
)

// -----------------------------------------------------------------------------
// the store-backed metering sink
// -----------------------------------------------------------------------------

// storeSink is metering persisting to a REAL store, which §15.3 requires and
// which is the whole reason the durable number is separate: a nop sink makes the
// served number look excellent and says nothing about whether the writes land.
type storeSink struct {
	s *store.Store

	rollupRows atomic.Int64
	traceRows  atomic.Int64
	failures   atomic.Int64
}

func (k *storeSink) WriteRollups(ctx context.Context, buckets []meter.Bucket) error {
	b := store.NewRollupBatch()
	for _, bk := range buckets {
		d := store.UsageDelta{
			Requests: bk.Requests, Errors: bk.Errors,
			PromptTokens: bk.Tokens.Input, CompletionTokens: bk.Tokens.Output,
			CachedTokens: bk.Tokens.CacheRead, ReasoningTokens: bk.Tokens.Reasoning,
			TotalTokens: bk.Tokens.Input + bk.Tokens.Output,
			CostNano:    bk.CostNano, LatencyMSSum: bk.LatencySum.Milliseconds(),
		}
		if bk.APIKeyID != "" {
			key := store.KeyHourKey{Hour: bk.HourStart, APIKeyID: bk.APIKeyID}
			cur := b.KeyHour[key]
			cur.Add(d)
			b.KeyHour[key] = cur
		}
		if bk.ModelGroup != "" {
			key := store.ModelHourKey{Hour: bk.HourStart, ModelGroup: bk.ModelGroup}
			cur := b.ModelHour[key]
			cur.Add(d)
			b.ModelHour[key] = cur
		}
		if bk.TeamID != "" {
			key := store.TeamDayKey{Day: bk.HourStart.UTC().Truncate(24 * time.Hour), TeamID: bk.TeamID}
			cur := b.TeamDay[key]
			cur.Add(d)
			b.TeamDay[key] = cur
		}
	}
	n := int64(b.Len())
	if _, err := k.s.MergeRollups(ctx, b); err != nil {
		k.failures.Add(1)
		return err
	}
	k.rollupRows.Add(n)
	return nil
}

func (k *storeSink) WriteTraces(ctx context.Context, traces []meter.Trace) error {
	rows := make([]store.RequestTrace, 0, len(traces))
	for _, tr := range traces {
		rows = append(rows, store.RequestTrace{
			RequestID: tr.RequestID, TS: tr.Time, TraceID: tr.TraceID,
			Excerpt: tr.Excerpt, ExcerptBytes: int64(len(tr.Excerpt)), ExcerptMode: "truncated",
		})
	}
	if err := k.s.InsertRequestTraces(ctx, rows); err != nil {
		k.failures.Add(1)
		return err
	}
	k.traceRows.Add(int64(len(rows)))
	return nil
}

// -----------------------------------------------------------------------------
// the harness
// -----------------------------------------------------------------------------

type harness struct {
	// seq makes every request id unique, as a real one is. Reusing an id makes
	// the trace table refuse the insert on its primary key, which looks like a
	// storage-layer failure and is actually the workload's own bug.
	seq atomic.Int64

	router  *router.Router
	broker  *capacity.Broker
	meter   *meter.Meter
	sink    *storeSink
	store   *store.Store
	upURL   string
	client  *http.Client
	catalog *pricing.Catalog
}

func newHarness(t testing.TB, prefixOn bool) *harness {
	t.Helper()

	up := fake.New(fake.Options{
		Shape: fake.ShapeOpenAI,
		Script: func(r *fake.Recorded) fake.Script {
			return fake.Script{
				Model: r.Model, Text: "ok",
				Usage: fake.Usage{InputTokens: 812, OutputTokens: 133, CacheReadTokens: 640},
			}
		},
	})
	t.Cleanup(up.Close)

	cat, err := pricing.ParseCatalog([]byte(PricingCatalog(100)))
	if err != nil {
		t.Fatalf("pricing catalog: %v", err)
	}
	if n := len(cat.Rules()); n < 100 {
		t.Fatalf("catalog compiled %d rules, want at least 100", n)
	}

	broker := capacity.New(capacity.Config{
		Global:        0, // unlimited: this measures the path, not the admission ceiling
		SweepInterval: -1,
	})
	t.Cleanup(broker.Close)

	st, err := store.Open(context.Background(), store.Config{
		Driver: store.DialectSQLite,
		DSN:    filepath.Join(t.TempDir(), "workload.db"),
		Pepper: []byte("workload-pepper-not-a-real-secret"),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sink := &storeSink{s: st}
	m := meter.New(meter.Config{
		Sink:     sink,
		SpoolDir: t.TempDir(),
		// The real cadence: the numeric path merges every few hundred
		// milliseconds and the trace path drains continuously.
		FlushInterval: 250 * time.Millisecond,
		DrainInterval: 10 * time.Millisecond,
		ShipInterval:  50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = m.Close() })

	deps := router.Deps{
		Capacity: broker,
		Health:   health.New(health.Options{}),
		Pricing:  cat,
		Interner: prefix.NewInterner(),
	}
	if prefixOn {
		deps.Prefix = prefix.NewTable(prefix.Options{TTL: time.Hour})
	}
	rt, err := router.New(router.Config{
		Groups: []router.Group{{
			Name: Model, Class: "large",
			Strategy: []router.Strategy{router.StrategyPrefixSticky, router.StrategyLowestCost},
			Deployments: []router.Deployment{
				{ID: "d1", Provider: "self-hosted", Kind: "vllm", UpstreamModel: "qwen3.5:397b",
					Capabilities: openai.DefaultCapabilities,
					Credentials:  []router.Credential{{ID: "acct-1"}}},
				{ID: "d2", Provider: "self-hosted-b", Kind: "vllm", UpstreamModel: "qwen3.5:397b",
					Capabilities: openai.DefaultCapabilities,
					Credentials:  []router.Credential{{ID: "acct-2"}}},
			},
		}},
		Prefix: router.PrefixConfig{Enabled: prefixOn},
	}, deps)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	return &harness{
		router: rt, broker: broker, meter: m, sink: sink, store: st,
		upURL:   up.Endpoint(),
		client:  &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 512}},
		catalog: cat,
	}
}

// serve runs one request end to end: decode, prefix chain, route, price, encode
// upstream, dispatch, relay, report, meter.
func (h *harness) serve(ctx context.Context, body []byte, key string, prefixOn bool) error {
	req, err := openai.DecodeRequest(body)
	if err != nil {
		return err
	}
	rr := router.Request{
		Model: req.Model, Principal: key, Required: req.RequiredCapabilities(),
		InputTokens: int64(len(body)/3 + 16),
	}
	if prefixOn {
		group, _ := h.router.Resolve(req.Model)
		rr.Digests = prefix.Compute("t", group, body, 0)
	}
	d, err := h.router.Route(ctx, rr)
	if err != nil {
		return err
	}

	up, err := openai.MarshalRequest(req, &openai.EncodeOptions{Model: d.UpstreamModel})
	if err != nil {
		h.router.Report(d, router.Outcome{Err: err})
		return err
	}
	start := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.upURL, bytes.NewReader(up))
	if err != nil {
		h.router.Report(d, router.Outcome{Err: err})
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept-Encoding", "identity")
	resp, err := h.client.Do(httpReq)
	if err != nil {
		h.router.Report(d, router.Outcome{Err: err})
		return err
	}
	out, err := readAllAndClose(resp)
	if err != nil {
		h.router.Report(d, router.Outcome{Err: err})
		return err
	}
	cr, err := openai.DecodeResponse(out, &openai.DecodeOptions{Model: req.Model})
	if err != nil {
		h.router.Report(d, router.Outcome{Err: err})
		return err
	}
	elapsed := time.Since(start)

	var in, outTok int64
	if cr.Usage != nil {
		in, outTok = int64(cr.Usage.InputTokens), int64(cr.Usage.OutputTokens)
	}
	// §8.1: routing already priced this candidate with Catalog.Price, which
	// mutates nothing. Settle is the accounting path and runs once.
	cost, err := h.catalog.Settle(pricing.Request{
		Provider: d.Provider, Model: d.UpstreamModel, Credential: d.Credential,
		Deployment: d.Deployment, InputTokens: in, OutputTokens: outTok,
		CacheReadTokens: 640, Requests: 1,
	})
	if err != nil {
		cost = pricing.Cost{}
	}
	h.router.Report(d, router.Outcome{
		Status: 200, Total: elapsed, InputTokens: in, OutputTokens: outTok,
	})
	h.meter.Record(meter.Event{
		APIKeyID: key, TeamID: "team-1", ModelGroup: req.Model,
		Provider: d.Provider, CredentialID: d.Credential,
		Endpoint: "/v1/chat/completions", Status: 200,
		Tokens:   meter.Tokens{Input: in, Output: outTok, CacheRead: 640},
		CostNano: cost.TotalNano, Latency: elapsed,
		Trace: meter.TraceInfo{
			RequestID:     "req-" + strconv.FormatInt(h.seq.Add(1), 36),
			UpstreamModel: d.UpstreamModel,
			Excerpt:       "workload",
		},
	})
	return nil
}

// -----------------------------------------------------------------------------
// the two numbers
// -----------------------------------------------------------------------------

// TestWorkloadThroughput reports the two figures of §15.3.
//
// It is a test rather than a benchmark because the two numbers are a gate, not a
// per-op cost, and because a benchmark's adaptive iteration count is the wrong
// shape for "run at steady state for a fixed period and see whether the backlog
// grows".
func TestWorkloadThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: the §15.3 workload is a measurement, not a unit test")
	}
	d := Duration()
	if !FullRun() {
		t.Logf("running %v per arm; §15.3 specifies ten minutes of steady state — "+
			"set DORANG_WORKLOAD_SECONDS=600 for the quotable figure", d)
	}

	// The skew has to actually be a skew, or the whole key population is a
	// uniform draw wearing a Zipf's name.
	picker := NewKeyPicker(Keys, 1)
	top := Distribution(NewKeyPicker(Keys, 1), 100_000, 5)
	t.Logf("key distribution, top 5 of %d keys over 100k draws: %v", Keys, top)
	if top[0] < top[4]*2 {
		t.Errorf("the key distribution is not skewed: top five are %v", top)
	}

	var served []Result
	for _, arm := range []struct {
		name     string
		prefixOn bool
	}{
		{"served, prefix off", false},
		{"served, prefix on", true},
	} {
		served = append(served, runServed(t, arm.name, arm.prefixOn, d, picker))
	}

	durable := runDurable(t, d)

	t.Log("")
	t.Log("=== DESIGN §15.3 workload =========================================")
	t.Logf("bodies 1 KiB / 32 KiB / 400 KiB, %d keys drawn Zipf(1.2), 100 pricing rules, "+
		"metering to a real SQLite store", Keys)
	for _, r := range served {
		t.Log("  " + r.String())
	}
	t.Log("  " + durable.String())
	t.Log("===================================================================")

	for _, r := range served {
		if r.Errors > 0 {
			t.Errorf("%s: %d requests failed", r.Name, r.Errors)
		}
		if r.Requests == 0 {
			t.Errorf("%s: nothing was served", r.Name)
		}
	}
	if durable.Requests == 0 {
		t.Error("the storage layer absorbed nothing")
	}
}

func runServed(t *testing.T, name string, prefixOn bool, d time.Duration, picker *KeyPicker) Result {
	t.Helper()
	h := newHarness(t, prefixOn)
	ctx := context.Background()

	// The corpus is rendered before the clock starts.
	corpus := NewCorpus(16)

	// Warm the path: the first requests pay for connection setup, the pricing
	// memo, the accumulator's first keys and SQLite's first pages.
	for i := range 64 {
		if err := h.serve(ctx, corpus.At(i), "key-0", prefixOn); err != nil {
			t.Fatalf("warmup: %v", err)
		}
	}
	warmup := int64(64)

	workers := runtime.NumCPU()
	if workers < 2 {
		workers = 2
	}
	var (
		requests atomic.Int64
		bytes    atomic.Int64
		failures atomic.Int64
	)
	_ = picker // the shared picker is only used for the distribution report
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each worker draws from its own picker. A shared one behind a
			// mutex would put a contended lock in the measurement loop and the
			// figure would be about that lock.
			keys := NewKeyPicker(Keys, uint64(w)+1)
			i := w
			for {
				select {
				case <-stop:
					return
				default:
				}
				body := corpus.At(i)
				if err := h.serve(ctx, body, keys.Next(), prefixOn); err != nil {
					failures.Add(1)
				}
				requests.Add(1)
				bytes.Add(int64(len(body)))
				i += workers
			}
		}()
	}
	start := time.Now()
	time.Sleep(d)
	close(stop)
	wg.Wait()
	elapsed := time.Since(start)

	// Drain the metering pipeline and report whether it kept up. A served
	// number quoted while the backlog is still growing is the exact claim §15.3
	// splits in two.
	flushCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.meter.Flush(flushCtx); err != nil {
		t.Errorf("%s: draining the meter failed: %v", name, err)
	}
	st := h.meter.Stats()
	if st.PendingBuckets != 0 {
		t.Errorf("%s: %d rollup buckets are still pending after the drain: "+
			"the storage layer did not keep up with the request path",
			name, st.PendingBuckets)
	}
	if degraded, reason := h.meter.Degraded(); degraded {
		t.Logf("%s: metering degraded (%v): %d traces dropped of %d recorded — "+
			"the served figure is above what the trace path sustains",
			name, reason, st.TracesDropped, st.Recorded)
	}
	if st.Recorded != requests.Load()+warmup {
		t.Errorf("%s: metered %d of %d requests; the numeric path has no drop",
			name, st.Recorded, requests.Load()+warmup)
	}

	return Result{
		Name: name, Unit: "req", Requests: requests.Load(), Bytes: bytes.Load(),
		Elapsed: elapsed, Errors: failures.Load(),
	}
}

// runDurable measures what the storage layer absorbs without growing a backlog.
//
// It is a separate measurement rather than a by-product of the served run
// because the served run offers whatever load the request path happens to
// produce, which says nothing about the ceiling. Here the store is offered work
// as fast as it will take it, and the number reported is the rate at which it
// accepted rows with nothing left queued.
func runDurable(t *testing.T, d time.Duration) Result {
	t.Helper()
	h := newHarness(t, false)
	ctx := context.Background()

	const batch = 256
	var rows int64
	start := time.Now()
	deadline := start.Add(d)
	hour := time.Now().UTC().Truncate(time.Hour)
	for n := 0; time.Now().Before(deadline); n++ {
		buckets := make([]meter.Bucket, 0, batch)
		for i := range batch {
			// A realistic key set: the rollup rows are per (key, hour), so the
			// same skew that shapes the request path shapes the write path.
			buckets = append(buckets, meter.Bucket{
				Key: meter.Key{
					APIKeyID:   fmt.Sprintf("key-%d", (n*batch+i)%Keys),
					TeamID:     "team-1",
					ModelGroup: Model,
					Provider:   "self-hosted",
					Endpoint:   "/v1/chat/completions",
				},
				HourStart: hour,
				Requests:  1,
				Tokens:    meter.Tokens{Input: 812, Output: 133, CacheRead: 640},
				CostNano:  41_000,
			})
		}
		if err := h.sink.WriteRollups(ctx, buckets); err != nil {
			t.Fatalf("the store refused a rollup batch: %v", err)
		}
		rows += int64(len(buckets))
	}
	elapsed := time.Since(start)

	if got := h.sink.failures.Load(); got != 0 {
		t.Errorf("%d store writes failed", got)
	}
	return Result{
		Name: "durable (store absorbs)", Unit: "rows", Requests: rows, Elapsed: elapsed,
	}
}

// -----------------------------------------------------------------------------
// per-operation benchmarks
// -----------------------------------------------------------------------------

// BenchmarkServe is the per-request cost of the composed path, per body size.
func BenchmarkServe(b *testing.B) {
	for _, prefixOn := range []bool{false, true} {
		for _, size := range []int{Size1KiB, Size32KiB, Size400KiB} {
			name := fmt.Sprintf("prefix=%v/size=%dKiB", prefixOn, size>>10)
			b.Run(name, func(b *testing.B) {
				h := newHarness(b, prefixOn)
				ctx := context.Background()
				body := Body(0, size)
				if err := h.serve(ctx, body, "key-0", prefixOn); err != nil {
					b.Fatal(err)
				}
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; b.Loop(); i++ {
					if err := h.serve(ctx, body, "key-0", prefixOn); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkPrefixChain isolates the hash chain, which is the component whose
// cost scales with body size (§7.4b: O(log n) digests, one hash pass).
func BenchmarkPrefixChain(b *testing.B) {
	for _, size := range []int{Size1KiB, Size32KiB, Size400KiB} {
		b.Run(fmt.Sprintf("size=%dKiB", size>>10), func(b *testing.B) {
			body := Body(0, size)
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				if d := prefix.Compute("t", Model, body, 0); len(d) == 0 {
					b.Fatal("no digests")
				}
			}
		})
	}
}

// BenchmarkPricingWithHundredRules is §8.2's claim measured: a request evaluates
// only the rules that can possibly apply to it, so the cost must not scale with
// the size of the catalog.
func BenchmarkPricingWithHundredRules(b *testing.B) {
	for _, n := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("rules=%d", n), func(b *testing.B) {
			cat, err := pricing.ParseCatalog([]byte(PricingCatalog(n)))
			if err != nil {
				b.Fatal(err)
			}
			req := pricing.Request{
				Provider: "self-hosted", Model: "qwen3.5:397b", Deployment: "d1",
				Credential: "acct-1", InputTokens: 812, OutputTokens: 133, Requests: 1,
				At: time.Unix(1753660800, 0),
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := cat.Price(req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------------

func readAllAndClose(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
