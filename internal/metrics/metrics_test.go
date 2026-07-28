package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/server"
)

// ---------------------------------------------------------------------------
// Exposition format
// ---------------------------------------------------------------------------

// TestExpositionParses is the decisive check on the hand-rolled renderer.
//
// A substring assertion would pass on output Prometheus rejects, so the body is
// parsed back and every family is checked for HELP, TYPE and a unit-honest
// name. This is the test that would have caught `kv_cache_usage_perc`.
func TestExpositionParses(t *testing.T) {
	r := fullRegistry(t)
	body := r.Metrics(nil)

	fams, err := Parse(body)
	if err != nil {
		t.Fatalf("scrape does not parse: %v\n%s", err, body)
	}
	if len(fams) < 40 {
		t.Errorf("only %d families; the §12.3 surface is much larger than that", len(fams))
	}
	for _, e := range Validate(body) {
		t.Errorf("validation: %v", e)
	}
}

// TestEveryFamilyHasHelpAndType states the rule on its own, because a family
// that loses its TYPE line loses its aggregation semantics silently: Prometheus
// treats it as untyped and every rate() over it keeps returning a number.
func TestEveryFamilyHasHelpAndType(t *testing.T) {
	r := fullRegistry(t)
	body := r.Metrics(nil)

	fams, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	seenType := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		if name, _, ok := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " "); ok &&
			strings.HasPrefix(line, "# TYPE ") {
			seenType[name] = true
		}
	}
	for _, f := range fams {
		if f.Help == "" {
			t.Errorf("%s has no HELP", f.Name)
		}
		if !seenType[f.Name] {
			t.Errorf("%s has no TYPE", f.Name)
		}
	}
}

// TestGoldenIsStable asserts the scrape does not reorder itself between two
// collections of the same state. A page whose lines move is a diff nobody can
// read and an alert nobody can write.
func TestGoldenIsStable(t *testing.T) {
	r := fullRegistry(t)
	a := string(r.Metrics(nil))
	b := string(r.Metrics(nil))

	// The self-metrics move by construction: the scrape counter increments and
	// the collection duration is a wall time. Everything else must be identical.
	if got, want := stripVolatile(a), stripVolatile(b); got != want {
		t.Errorf("scrape is not stable across two collections of the same state")
		for i, line := range strings.Split(got, "\n") {
			other := strings.Split(want, "\n")
			if i < len(other) && line != other[i] {
				t.Fatalf("first difference at line %d:\n  %s\n  %s", i+1, line, other[i])
			}
		}
	}
}

var volatilePrefixes = []string{
	"dorang_metrics_scrapes_total",
	"dorang_metrics_collection_duration_seconds",
	"dorang_uptime_seconds",
	"dorang_process_start_time_seconds",
	"dorang_memory_",
	"dorang_heap_",
	"dorang_gc_cycles_total",
	"dorang_goroutines",
	"dorang_deployment_latency_seconds",
	// Legitimately cumulative, and it grows by one per scrape whenever a cap
	// is engaged — which is the behaviour, not an instability.
	"dorang_metrics_cardinality_folds_total",
}

func stripVolatile(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		skip := false
		for _, p := range volatilePrefixes {
			if strings.HasPrefix(line, p) || strings.HasPrefix(line, "# HELP "+p) ||
				strings.HasPrefix(line, "# TYPE "+p) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// TestLabelValuesAreEscaped keeps a model name — which DESIGN §2.1 says is
// opaque and may contain anything — from breaking the exposition. "Configuration
// values are probably safe" is how an injection ships.
func TestLabelValuesAreEscaped(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	q.Observe(Sample{
		Model:    `weird"name` + "\\" + "\nwith-newline",
		Endpoint: "chat",
		Status:   200,
		Duration: time.Millisecond,
	})
	r := New(nil)
	r.Register(q)
	body := r.Metrics(nil)

	fams, err := Parse(body)
	if err != nil {
		t.Fatalf("a hostile model name broke the exposition: %v\n%s", err, body)
	}
	var found bool
	for _, f := range fams {
		if f.Name != "dorang_requests_total" {
			continue
		}
		for _, s := range f.Samples {
			if strings.Contains(s.Label("model"), `weird"name`) {
				found = true
			}
		}
	}
	if !found {
		t.Error("the escaped model name did not survive a round trip")
	}
}

// ---------------------------------------------------------------------------
// Cardinality
// ---------------------------------------------------------------------------

// TestCardinalityCapFoldsAndCounts is rule 1 of the package comment. A cap that
// silently discards resolution produces a dashboard that is quietly wrong; the
// fold counter is what tells the operator the cap needs raising, or that
// something is putting a request id in a label.
func TestCardinalityCapFoldsAndCounts(t *testing.T) {
	q := NewRequests(RequestsOptions{MaxSeries: 4, MaxModels: 2, MaxFallbackSeries: 2})
	for i := 0; i < 50; i++ {
		q.Observe(Sample{
			Model:      "m" + string(rune('a'+i%13)),
			Credential: "c" + string(rune('a'+i%7)),
			Endpoint:   "chat",
			Status:     200,
			Duration:   time.Millisecond,
		})
	}
	r := New(nil)
	r.Register(q)
	body := r.Metrics(nil)

	fams, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Family{}
	for _, f := range fams {
		byName[f.Name] = f
	}

	req := byName["dorang_requests_total"]
	if n := len(req.Samples); n > 5 {
		t.Errorf("%d series past a cap of 4 (+1 overflow)", n)
	}
	var overflow, total float64
	for _, s := range req.Samples {
		total += s.Value
		if s.Label("model") == OverflowSentinel {
			overflow = s.Value
			// Every dimension of a folded series must say it was folded.
			for _, l := range []string{"provider", "credential", "status", "endpoint"} {
				if s.Label(l) != OverflowSentinel {
					t.Errorf("folded series keeps %s=%q; it must not claim the tuple "+
						"that happened to reach the cap", l, s.Label(l))
				}
			}
		}
	}
	if overflow == 0 {
		t.Fatal("nothing was folded, so the cap did not engage")
	}
	// Nothing is lost: the total is still 50, with resolution folded rather
	// than samples dropped.
	if total != 50 {
		t.Errorf("requests total = %v, want 50; a fold must not lose a count", total)
	}

	folds := byName["dorang_metrics_cardinality_folds_total"]
	var counted float64
	for _, s := range folds.Samples {
		if s.Label("family") == "dorang_requests_total" {
			counted = s.Value
		}
	}
	if counted == 0 {
		t.Error("folds happened and dorang_metrics_cardinality_folds_total says zero: " +
			"the overflow is silent, which is the failure this counter exists to prevent")
	}
	if counted != overflow {
		t.Errorf("folds counted %v but the overflow series holds %v", counted, overflow)
	}
}

// TestCapacityKeyFoldIsDeterministic keeps a folded series from appearing and
// vanishing between scrapes, which reads to an alert as a restart.
func TestCapacityKeyFoldIsDeterministic(t *testing.T) {
	src := newFakeCapacity(40)
	c := NewCapacityCollector(src, 3)
	r := New(nil)
	r.Register(c)

	a := stripVolatile(string(r.Metrics(nil)))
	b := stripVolatile(string(r.Metrics(nil)))
	if a != b {
		t.Error("the folded capacity series moved between two identical scrapes")
	}
	if !strings.Contains(a, `key="`+OverflowSentinel+`"`) {
		t.Error("40 keys under a cap of 3 produced no overflow series")
	}
}

// ---------------------------------------------------------------------------
// Absent, not zero
// ---------------------------------------------------------------------------

// TestUnavailableIsAbsentNotZero is rule 3, and it is the most consequential
// rule in the package. VLLM.md §3.3 records vLLM's `/load` returning
// `{"server_load": 0}` forever when its flag is unset: indistinguishable from a
// genuinely idle server, and the single most attractive value to a least-busy
// router. Every metric dorang cannot compute must therefore be missing.
func TestUnavailableIsAbsentNotZero(t *testing.T) {
	tr := health.New(health.Options{})
	// A deployment that exists but has never been measured.
	tr.Report("never-measured", health.Outcome{})

	r := New(nil)
	r.Register(NewHealthCollector(tr, 0))
	body := string(r.Metrics(nil))

	for _, name := range []string{
		"dorang_deployment_ttft_seconds",
		"dorang_deployment_tokens_per_second",
	} {
		if strings.Contains(body, name) {
			t.Errorf("%s is present for a deployment with no sample. §7.5a(a): a "+
				"deployment with no samples has no opinion, and absent must never read "+
				"as fastest.", name)
		}
	}
	// The circuit state, by contrast, is a fact dorang does know.
	if !strings.Contains(body, `dorang_deployment_health{deployment="never-measured",state="closed"} 1`) {
		t.Error("the circuit state is known and must be reported:\n" + body)
	}

	// Now measure it, and the gauges must appear.
	tr.Report("never-measured", health.Outcome{TTFT: 5 * time.Millisecond,
		Total: 100 * time.Millisecond, OutputTokens: 40})
	body = string(r.Metrics(nil))
	if !strings.Contains(body, "dorang_deployment_ttft_seconds") {
		t.Error("a measured deployment still has no latency gauge")
	}
}

// TestRatioAbsentWithoutDenominator keeps a hit ratio of 0.0 from being
// published on an idle process, where it reads as "the cache never works"
// rather than "nothing has been asked yet".
func TestRatioAbsentWithoutDenominator(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	q.SetPrefixEnabled(true)
	r := New(nil)
	r.Register(q)

	if hasFamily(t, r.Metrics(nil), "dorang_prefix_hit_ratio") {
		t.Error("a hit ratio was published with no routed request to divide by")
	}

	q.Observe(Sample{Model: "m", Endpoint: "chat", Status: 200, Routed: true,
		PrefixHit: true, Duration: time.Millisecond})
	body := string(r.Metrics(nil))
	if !strings.Contains(body, `dorang_prefix_hit_ratio{model="m"} 1`) {
		t.Errorf("the ratio did not appear once there was a denominator:\n%s", body)
	}
}

// TestPrefixRatioAbsentWhenDisabled: with cache affinity off, every model's hit
// count is legitimately zero, and a ratio of 0.0 would be an accusation rather
// than a measurement.
func TestPrefixRatioAbsentWhenDisabled(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	q.SetPrefixEnabled(false)
	q.Observe(Sample{Model: "m", Endpoint: "chat", Status: 200, Routed: true,
		Duration: time.Millisecond})
	r := New(nil)
	r.Register(q)
	if hasFamily(t, r.Metrics(nil), "dorang_prefix_hit_ratio") {
		t.Error("a prefix hit ratio was published with prefix routing off")
	}
}

// TestNotionalMissingIsCountedNotZeroed is DESIGN §8.5 rule 5: a model with no
// notional_rate rule reports the figure as unavailable and increments a
// counter. Adding zero to the total would make a subscription look infinitely
// efficient — the most flattering answer, and the least likely to be questioned.
func TestNotionalMissingIsCountedNotZeroed(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	q.Observe(Sample{Model: "m", Endpoint: "chat", Status: 200, Routed: true,
		Priced: true, CostNano: 1000, NotionalPriced: false, Duration: time.Millisecond})
	q.Observe(Sample{Model: "m", Endpoint: "chat", Status: 200, Routed: true,
		Priced: true, CostNano: 1000, NotionalPriced: true, NotionalNano: 50_000,
		Duration: time.Millisecond})

	r := New(nil)
	r.Register(q)
	got := sampleValues(t, r.Metrics(nil))

	if got["dorang_notional_nano_total"] != 50_000 {
		t.Errorf("notional total = %v, want 50000 — the unpriced request must contribute "+
			"nothing rather than zero", got["dorang_notional_nano_total"])
	}
	if got["dorang_notional_missing_total"] != 1 {
		t.Errorf("notional missing = %v, want 1", got["dorang_notional_missing_total"])
	}
	if got["dorang_notional_priced_requests_total"] != 1 {
		t.Errorf("notional priced = %v, want 1", got["dorang_notional_priced_requests_total"])
	}
}

// TestTTFTZeroIsNotASample: a zero TTFT means "not measured", not "instant".
func TestTTFTZeroIsNotASample(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	q.Observe(Sample{Model: "m", Endpoint: "chat", Status: 200, Duration: time.Millisecond})
	r := New(nil)
	r.Register(q)
	if strings.Contains(string(r.Metrics(nil)), "dorang_ttft_seconds") {
		t.Error("a request with no measured first token produced a TTFT sample")
	}

	q.Observe(Sample{Model: "m", Endpoint: "chat", Status: 200,
		Duration: time.Millisecond, TTFT: 20 * time.Millisecond})
	got := sampleValues(t, r.Metrics(nil))
	if got["dorang_ttft_seconds_count"] != 1 {
		t.Errorf("ttft count = %v, want 1", got["dorang_ttft_seconds_count"])
	}
}

// ---------------------------------------------------------------------------
// Histogram buckets
// ---------------------------------------------------------------------------

// TestBucketsResolveMicroseconds keeps the duration histogram able to answer
// the only question it exists for. DESIGN §15.1 states the warm-local p50 as
// 200 µs and the p99 as 2 ms; a histogram whose first bucket is 5 ms reports
// "everything is under 5 ms" through a twentyfold regression.
func TestBucketsResolveMicroseconds(t *testing.T) {
	if DurationBounds[0] > 0.0001 {
		t.Errorf("lowest duration bucket is %v s; it must resolve below the 200 µs p50",
			DurationBounds[0])
	}
	var below2ms int
	for _, b := range DurationBounds {
		if b <= 0.002 {
			below2ms++
		}
	}
	if below2ms < 8 {
		t.Errorf("only %d buckets at or below the 2 ms p99; the distribution around the "+
			"target would be one step", below2ms)
	}

	h := NewHist(DurationBounds)
	h.Observe(180 * time.Microsecond)
	h.Observe(1900 * time.Microsecond)
	h.Observe(3 * time.Second)

	var scratch []uint64
	counts, sum, n := h.snapshot(scratch)
	if n != 3 {
		t.Fatalf("count = %d", n)
	}
	if sum < 3.0 || sum > 3.01 {
		t.Errorf("sum = %v s, want about 3.002", sum)
	}
	// The two sub-millisecond samples must land in different buckets, which is
	// the whole claim.
	var i180, i1900 int
	for i, b := range DurationBounds {
		if 0.00018 <= b && i180 == 0 {
			i180 = i
		}
		if 0.0019 <= b && i1900 == 0 {
			i1900 = i
		}
	}
	if i180 == i1900 {
		t.Error("180 µs and 1.9 ms fall in the same bucket")
	}
	if counts[i180] != 1 || counts[i1900] != 1 {
		t.Errorf("bucket occupancy %d / %d, want 1 / 1", counts[i180], counts[i1900])
	}
}

// ---------------------------------------------------------------------------
// Agreement with the source packages
// ---------------------------------------------------------------------------

// TestCountersAgreeWithServerStats runs a synthetic workload through a real
// server and requires the scrape to agree with the server's own Stats().
//
// This is the point of the whole package. A metric that has drifted from the
// thing it reports is worse than no metric: it is a number someone will act on.
func TestCountersAgreeWithServerStats(t *testing.T) {
	reg := New(nil)
	q := NewRequests(RequestsOptions{})
	reg.Register(q)

	srv, err := server.New(server.Options{
		Metrics: reg,
		Meter: server.MeterFunc(func(ev server.Event) {
			q.Observe(Sample{
				Model:      ev.Model,
				Provider:   ev.Result.Provider,
				Credential: ev.Result.Credential,
				Endpoint:   ev.Route,
				Status:     ev.Status,
				Duration:   time.Duration(ev.DurationNS),
			})
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	reg.Register(NewServerCollector(srv))

	const n = 25
	for i := 0; i < n; i++ {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	}
	// A 501 as well, so the status dimension is not degenerate.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/nonexistent", nil))

	body := reg.Metrics(nil)
	fams, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}

	var scraped float64
	for _, f := range fams {
		if f.Name != "dorang_requests_total" {
			continue
		}
		for _, s := range f.Samples {
			scraped += s.Value
		}
	}
	st := srv.Stats()
	if uint64(scraped) != st.Requests {
		t.Errorf("dorang_requests_total sums to %v, server.Stats().Requests is %d",
			scraped, st.Requests)
	}

	got := sampleValues(t, body)
	if got["dorang_request_duration_seconds_count"] != float64(st.Requests) {
		t.Errorf("duration histogram counted %v of %d requests",
			got["dorang_request_duration_seconds_count"], st.Requests)
	}
	if got["dorang_unimplemented_total"] != float64(st.Unimplemented) || st.Unimplemented != 1 {
		t.Errorf("unimplemented: scrape %v, stats %d",
			got["dorang_unimplemented_total"], st.Unimplemented)
	}

	// And the labelled breakdown has to agree with the class counter too.
	var twoXX float64
	for _, f := range fams {
		if f.Name != "dorang_requests_total" {
			continue
		}
		for _, s := range f.Samples {
			if strings.HasPrefix(s.Label("status"), "2") {
				twoXX += s.Value
			}
		}
	}
	if uint64(twoXX) != st.ByClass[2] {
		t.Errorf("2xx: labelled %v, class counter %d", twoXX, st.ByClass[2])
	}
}

// TestRegistryReplacesTheBuiltInBlock keeps the two renderers from both
// emitting dorang_requests_total, which is a duplicate TYPE line and therefore
// a rejected scrape.
func TestRegistryReplacesTheBuiltInBlock(t *testing.T) {
	reg := New(nil)
	q := NewRequests(RequestsOptions{})
	reg.Register(q)

	srv, err := server.New(server.Options{Metrics: reg})
	if err != nil {
		t.Fatal(err)
	}
	reg.Register(NewServerCollector(srv))
	q.Observe(Sample{Endpoint: "health", Status: 200, Duration: time.Millisecond})

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if _, err := Parse(w.Body.Bytes()); err != nil {
		t.Fatalf("the served scrape does not parse: %v", err)
	}
	if got := strings.Count(w.Body.String(), "# TYPE dorang_requests_total"); got != 1 {
		t.Errorf("%d TYPE lines for dorang_requests_total; a second one makes Prometheus "+
			"reject the whole scrape", got)
	}
}

// TestBuiltInBlockSurvivesWithoutARegistry keeps the zero-configuration server
// working: a deployment that never assembles a registry still has a usable
// endpoint.
func TestBuiltInBlockSurvivesWithoutARegistry(t *testing.T) {
	srv, err := server.New(server.Options{})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(w.Body.String(), "dorang_requests_total") {
		t.Error("the built-in block disappeared when no registry is configured")
	}
}

// ---------------------------------------------------------------------------
// Collection must not be a denial-of-service surface
// ---------------------------------------------------------------------------

// TestCollectionDoesNotBlockARequest is rule 4, in the decisive form
// internal/server uses for its configuration mutex: hold the lock a scrape
// might contend on for the whole duration of a request and require the request
// to finish anyway.
//
// Here the lock is the registry's own render mutex, held by a collector that
// blocks. If the observation path ever learns to take it, this deadlocks and
// the test fails on the timeout.
func TestCollectionDoesNotBlockARequest(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once

	reg := New(nil)
	reg.Register(q, CollectorFunc{Name: "slow", Fn: func(w *Writer) {
		once.Do(func() { close(entered) })
		<-release
	}})

	go func() { reg.Metrics(nil) }()
	<-entered

	// A scrape is now inside the registry, holding its mutex. Observations must
	// still complete.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			q.Observe(Sample{Model: "m", Endpoint: "chat", Status: 200,
				Duration: time.Millisecond})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("observations blocked while a scrape was in progress: the request path " +
			"is taking a lock the scrape holds")
	}
	close(release)
}

// TestScrapeCompletesWhileTheConfigMutexIsHeld reuses internal/server's own
// trick from the other side: a reload holds the server's only mutex, and the
// scrape has to finish anyway. Server.Stats reads atomics for exactly this
// reason.
func TestScrapeCompletesWhileTheConfigMutexIsHeld(t *testing.T) {
	reg := New(nil)
	srv, err := server.New(server.Options{Metrics: reg})
	if err != nil {
		t.Fatal(err)
	}
	reg.Register(NewServerCollector(srv))

	stop := make(chan struct{})
	var reloads atomic.Int64
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = srv.Reload(server.Options{Metrics: reg})
			reloads.Add(1)
		}
	}()
	defer close(stop)

	done := make(chan int, 1)
	go func() {
		n := 0
		for i := 0; i < 200; i++ {
			n += len(reg.Metrics(nil))
		}
		done <- n
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a scrape blocked behind the configuration mutex")
	}
	if reloads.Load() == 0 {
		t.Error("no reload ran; the test did not exercise what it claims to")
	}
}

// TestObserveAndGatherRaceFree runs both concurrently. Under -race this is what
// catches a map read that escaped its lock.
func TestObserveAndGatherRaceFree(t *testing.T) {
	q := NewRequests(RequestsOptions{MaxSeries: 32, MaxModels: 8})
	reg := New(nil)
	reg.Register(q)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				q.Observe(Sample{
					Model:      "m" + string(rune('a'+n%20)),
					Provider:   "p" + string(rune('a'+i)),
					Credential: "c" + string(rune('a'+n%5)),
					Endpoint:   "chat",
					Status:     200,
					Duration:   time.Duration(n%1000) * time.Microsecond,
					TTFT:       time.Millisecond,
					Routed:     true,
				})
			}
		}(i)
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := Parse(reg.Metrics(nil)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestCollectorPanicIsIsolated: one broken adapter must not cost the operator
// every other number on the page, and the panic must be visible rather than
// swallowed.
func TestCollectorPanicIsIsolated(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	q.Observe(Sample{Model: "m", Endpoint: "chat", Status: 200, Duration: time.Millisecond})

	reg := New(nil)
	reg.Register(CollectorFunc{Name: "broken", Fn: func(w *Writer) {
		w.Metric("dorang_half_written_total", Counter, "half a family")
		w.Label("x", "y")
		panic("boom")
	}}, q)

	body := reg.Metrics(nil)
	if _, err := Parse(body); err != nil {
		t.Fatalf("a panicking collector corrupted the scrape: %v\n%s", err, body)
	}
	if strings.Contains(string(body), "dorang_half_written_total") {
		t.Error("the panicking collector's partial family survived the rollback")
	}
	if !strings.Contains(string(body), `dorang_metrics_collector_panics_total{collector="broken"} 1`) {
		t.Errorf("the panic was not counted:\n%s", body)
	}
	if !strings.Contains(string(body), "dorang_requests_total") {
		t.Error("a later collector was lost to an earlier one's panic")
	}
}

// TestGatherAllocatesWithinBound states the bound rather than merely measuring
// it. A scrape that allocates per series turns a metrics endpoint into a source
// of garbage-collection pressure proportional to how closely it is watched.
func TestGatherAllocatesWithinBound(t *testing.T) {
	const series = 200
	q := NewRequests(RequestsOptions{})
	for i := 0; i < series; i++ {
		q.Observe(Sample{
			Model:      "model-" + string(rune('a'+i%20)),
			Provider:   "provider-" + string(rune('a'+i%5)),
			Credential: "cred-" + string(rune('a'+i%7)),
			Endpoint:   "chat",
			Status:     200,
			Duration:   time.Millisecond,
			TTFT:       10 * time.Millisecond,
			Routed:     true,
		})
	}
	reg := New(nil)
	reg.Register(q)

	buf := make([]byte, 0, 1<<20)
	reg.Gather(buf) // warm the reused scratch

	// The stated bound: **zero** allocations for a scrape of two hundred series,
	// and the same zero at two thousand. Every buffer the renderer touches is
	// reused — the output byte slice, the label scratch, the histogram bucket
	// scratch, the per-table key and value scratch, the folder list. A source
	// that allocates its own snapshot (capacity.Broker.Snapshot documents that
	// it does) contributes its own allocations on top; this bound is on the
	// renderer, which is the part that scales with the number of series.
	//
	// The slack is one allocation, not zero, so that a Go release which decides
	// to heap-allocate something incidental fails this test loudly rather than
	// after the tenth such decision.
	const bound = 1
	got := testing.AllocsPerRun(50, func() {
		reg.Gather(buf[:0])
	})
	if got > bound {
		t.Errorf("Gather allocates %.0f times for %d series, bound is %d", got, series, bound)
	}
	t.Logf("Gather over %d series: %.0f allocs/op", series, got)
}

// TestObserveDoesNotAllocate keeps the observation point free of allocation.
// It runs where DESIGN §12.1's numeric path runs, once per request.
func TestObserveDoesNotAllocate(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	s := Sample{Model: "m", Provider: "p", Credential: "c", Endpoint: "chat",
		Status: 200, Duration: time.Millisecond, TTFT: time.Millisecond, Routed: true}
	q.Observe(s) // create the series

	if got := testing.AllocsPerRun(200, func() { q.Observe(s) }); got != 0 {
		t.Errorf("Observe allocates %.0f times per request", got)
	}
}

// ---------------------------------------------------------------------------
// HTTP surface
// ---------------------------------------------------------------------------

func TestServeHTTP(t *testing.T) {
	reg := fullRegistry(t)

	w := httptest.NewRecorder()
	reg.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != ContentType {
		t.Errorf("content type %q", got)
	}
	if w.Body.Len() == 0 {
		t.Fatal("empty body")
	}

	w = httptest.NewRecorder()
	reg.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/metrics", nil))
	if w.Body.Len() != 0 {
		t.Error("HEAD returned a body")
	}
	if w.Header().Get("Content-Length") == "0" {
		t.Error("HEAD did not report the length")
	}

	w = httptest.NewRecorder()
	reg.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST answered %d", w.Code)
	}
}

// TestDuplicateFamilyIsCounted: two collectors on one name is a rejected
// scrape, so it must be reported by the scrape itself rather than discovered in
// Prometheus's logs.
func TestDuplicateFamilyIsCounted(t *testing.T) {
	dup := CollectorFunc{Name: "dup", Fn: func(w *Writer) {
		w.Metric("dorang_dup_total", Counter, "twice")
		w.Uint(1)
	}}
	reg := New(nil)
	reg.Register(dup, dup)
	body := string(reg.Metrics(nil))
	if !strings.Contains(body, "dorang_metrics_duplicate_families_total 1") {
		t.Errorf("a duplicated family was not reported:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkGather(b *testing.B) {
	q := NewRequests(RequestsOptions{})
	for i := 0; i < 500; i++ {
		q.Observe(Sample{
			Model:      "model-" + string(rune('a'+i%25)),
			Provider:   "provider-" + string(rune('a'+i%5)),
			Credential: "cred-" + string(rune('a'+i%10)),
			Endpoint:   "chat",
			Status:     200 + (i%3)*100,
			Duration:   time.Millisecond,
			TTFT:       10 * time.Millisecond,
			Routed:     true,
		})
	}
	reg := New(nil)
	reg.Register(q, NewCapacityCollector(newFakeCapacity(50), 0))

	buf := make([]byte, 0, 1<<20)
	n := len(reg.Gather(buf))
	b.Logf("scrape is %d bytes", n)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reg.Gather(buf[:0])
	}
}

func BenchmarkObserve(b *testing.B) {
	q := NewRequests(RequestsOptions{})
	s := Sample{Model: "m", Provider: "p", Credential: "c", Endpoint: "chat",
		Status: 200, Duration: time.Millisecond, TTFT: time.Millisecond, Routed: true}
	q.Observe(s)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.Observe(s)
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fullRegistry builds a registry over every collector that can be driven
// without a store or a network.
func fullRegistry(t *testing.T) *Registry {
	t.Helper()

	q := NewRequests(RequestsOptions{})
	q.SetPrefixEnabled(true)
	for i := 0; i < 12; i++ {
		q.Observe(Sample{
			Model:          "model-" + string(rune('a'+i%3)),
			Provider:       "openai",
			Credential:     "cred-1",
			Endpoint:       "chat_completions",
			Status:         200,
			Duration:       time.Duration(100+i) * time.Microsecond,
			TTFT:           time.Duration(20+i) * time.Millisecond,
			CapacityWait:   time.Microsecond,
			Tokens:         Tokens{Input: 100, Output: 50, CacheRead: 10},
			CostNano:       12345,
			Priced:         true,
			NotionalNano:   99999,
			NotionalPriced: true,
			Routed:         true,
			PrefixHit:      i%2 == 0,
		})
	}
	q.Observe(Sample{
		Model: "model-a", Provider: "openai", Credential: "cred-2",
		Endpoint: "chat_completions", Status: 503, Duration: time.Millisecond,
		Routed: true, FallbackFrom: "dep-1", FallbackTo: "dep-2",
		FallbackReason: "fallback:rate_limited",
	})

	tr := health.New(health.Options{})
	tr.Report("dep-1", health.Outcome{TTFT: 30 * time.Millisecond,
		Total: 500 * time.Millisecond, OutputTokens: 120})
	tr.Report("dep-2", health.Outcome{Err: errTest, Failure: true})

	srv, err := server.New(server.Options{})
	if err != nil {
		t.Fatal(err)
	}

	reg := New(nil)
	reg.Register(
		NewBuildCollector("1.2.3", "abcdef", nil),
		q,
		NewCapacityCollector(newFakeCapacity(6), 0),
		NewHealthCollector(tr, 0),
		NewServerCollector(srv),
	)
	return reg
}

var errTest = &testError{}

type testError struct{}

func (*testError) Error() string { return "test" }

// fakeCapacity produces a deterministic broker snapshot.
//
// The axes are built once. The real [capacity.Broker.Snapshot] allocates its
// own slice on every call and says so in its doc comment ("It allocates and is
// not for the hot path"); a fixture that allocated fifty strings per scrape
// would put that cost in the benchmark of the renderer, where it does not
// belong.
type fakeCapacity struct{ axes []capacity.AxisState }

func newFakeCapacity(n int) fakeCapacity {
	var f fakeCapacity
	for i := 0; i < n; i++ {
		f.axes = append(f.axes, capacity.AxisState{
			Axis:  capacity.AxisKey,
			Key:   "openai|cred-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			InUse: i % 4, Limit: 8, Waiting: i % 2,
			SoftReserved: i == 0,
		})
	}
	f.axes = append(f.axes, capacity.AxisState{
		Axis: capacity.AxisGlobal, Key: "", InUse: 3, Limit: 0,
	})
	// Snapshot's contract is sorted by (axis, key).
	sortSlice(f.axes, func(a, b capacity.AxisState) bool {
		if a.Axis != b.Axis {
			return a.Axis < b.Axis
		}
		return a.Key < b.Key
	})
	return f
}

func (f fakeCapacity) Snapshot() capacity.Snapshot {
	return capacity.Snapshot{
		Axes:    f.axes,
		Waiting: 2, Reservations: 7, Wakeups: 11, Grants: 100, Expired: 1,
		SoftReserved: 1, SoftReservations: 3,
	}
}

// hasFamily reports whether a family has any series in the scrape. A substring
// search would match the name where another family's HELP text mentions it.
func hasFamily(t *testing.T, body []byte, name string) bool {
	t.Helper()
	fams, err := Parse(body)
	if err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	for _, f := range fams {
		if f.Name == name {
			return true
		}
	}
	return false
}

// sampleValues collapses a scrape to name -> summed value, for assertions that
// do not care about labels.
func sampleValues(t *testing.T, body []byte) map[string]float64 {
	t.Helper()
	fams, err := Parse(body)
	if err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	out := map[string]float64{}
	for _, f := range fams {
		for _, s := range f.Samples {
			out[s.Name] += s.Value
		}
	}
	return out
}
