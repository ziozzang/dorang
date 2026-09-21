package metrics

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
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
	// The runtime's own histograms. A GC that happens BETWEEN the two
	// collections adds a pause, and a goroutine that waits for a P adds a
	// scheduling sample — so these move for the same reason
	// `dorang_gc_cycles_total` above does, and the test scraping them is enough
	// to make it happen. This test is about ORDER: "a page whose lines move is
	// a diff nobody can read". A bucket count that advanced is not a line that
	// moved.
	"dorang_gc_pause_seconds",
	"dorang_sched_latency_seconds",
	// Threads and descriptors are properties of the process at the instant of
	// the read, and the read itself opens a descriptor.
	"dorang_os_threads",
	"dorang_open_file_descriptors",
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
// 249 µs and the p99 as 2 ms; a histogram whose first bucket is 5 ms reports
// "everything is under 5 ms" through a twentyfold regression.
//
// The bound checked here is 100 µs rather than the p50 itself, on purpose. The
// p50 has moved four times (200 µs published, 480 µs measured, then 375 µs and
// 249 µs) and a test that tracked it would have to be edited every time the
// codec got faster, which is a test that asserts the changelog. What must hold
// across all four is that the bounds resolve well BELOW the figure, so the
// median lands with buckets on both sides of it rather than in the floor.
func TestBucketsResolveMicroseconds(t *testing.T) {
	if DurationBounds[0] > 0.0001 {
		t.Errorf("lowest duration bucket is %v s; it must resolve well below the "+
			"249 µs warm-local p50", DurationBounds[0])
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
// What the scrape actually publishes
// ---------------------------------------------------------------------------

// The three tests below exist so OPERATIONS.md §6 can be checked against a
// value rather than against a reading. Both of the claims they pin had been
// wrong in the document for as long as this package has existed, and both were
// wrong in the same way: the document describes internal/server's
// zero-configuration fallback, which no assembled gateway serves.

// TestScrapedDurationBucketsAreTheRangeOperationsPublishes reads the bucket
// bounds off a real registry's output.
//
// OPERATIONS.md §6.1 said "buckets from 100 µs to 60 s". That is
// internal/server's fallback block — [server.durationBuckets] — and
// [Server.handleMetrics] serves it only when no registry is configured. Every
// assembled gateway wires one, which replaces the family wholesale. Asserting
// the endpoints here means the document is checkable: if someone widens or
// narrows [DurationBounds], this fails and names the two numbers the prose has
// to be edited to.
func TestScrapedDurationBucketsAreTheRangeOperationsPublishes(t *testing.T) {
	fams, err := Parse(fullRegistry(t).Metrics(nil))
	if err != nil {
		t.Fatal(err)
	}
	var les []float64
	for _, f := range fams {
		if f.Name != "dorang_request_duration_seconds" {
			continue
		}
		for _, s := range f.Samples {
			if !strings.HasSuffix(s.Name, "_bucket") {
				continue
			}
			le := s.Label("le")
			if le == "" {
				t.Fatalf("a histogram bucket with no le label: %+v", s)
			}
			if le == "+Inf" {
				continue
			}
			v, err := strconv.ParseFloat(le, 64)
			if err != nil {
				t.Fatalf("le=%q does not parse: %v", le, err)
			}
			les = append(les, v)
		}
	}
	if len(les) == 0 {
		t.Fatal("the scrape has no dorang_request_duration_seconds buckets at all")
	}
	sort.Float64s(les)
	lo, hi := les[0], les[len(les)-1]

	// The published range, as two numbers. 50 µs and 300 s.
	if lo != 0.00005 {
		t.Errorf("lowest bucket is %v s, want 5e-05; OPERATIONS.md §6.1 has to say so", lo)
	}
	if hi != 300 {
		t.Errorf("highest finite bucket is %v s, want 300; OPERATIONS.md §6.1 has to say so", hi)
	}
	// And the range the document used to claim is provably not this one, so a
	// future edit cannot quietly reinstate the fallback's numbers.
	if lo == 0.0001 && hi == 60 {
		t.Error("the scraped histogram is internal/server's fallback block, not this package's")
	}
}

// TestScrapedRequestFamiliesCarryCallerFacingLabels pins the labels that are on
// the wire.
//
// OPERATIONS.md §6's preamble said there are "no per-path, per-model or per-key
// labels anywhere". Four of them are here, and one — `credential` — is a per-key
// label under a different name. The sentence was not a description but an
// argument, so the fix is not to soften it: it is to state what bounds each
// label, which is what [DefaultMaxRequestSeries] now spells out and what the
// next test proves is true of only four of the five.
func TestScrapedRequestFamiliesCarryCallerFacingLabels(t *testing.T) {
	fams, err := Parse(fullRegistry(t).Metrics(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"dorang_requests_total":           {"model", "provider", "credential", "status", "endpoint"},
		"dorang_request_duration_seconds": {"model"},
		"dorang_ttft_seconds":             {"model"},
	}
	for _, f := range fams {
		names, ok := want[f.Name]
		if !ok {
			continue
		}
		delete(want, f.Name)
		if len(f.Samples) == 0 {
			t.Fatalf("%s rendered no samples", f.Name)
		}
		s := f.Samples[0]
		for _, n := range names {
			if s.Label(n) == "" {
				t.Errorf("%s carries no %q label; §6.1's label inventory is wrong", f.Name, n)
			}
		}
	}
	for name := range want {
		t.Errorf("%s is not in the scrape at all", name)
	}
}

// TestTheConfiguredSetBoundsTheModelLabel is what replaced the series cap as the
// bound on `model`.
//
// The finding it comes from: a gateway that lets a client mint label values has
// handed out a denial of service on its own registry. `provider`, `credential`,
// `endpoint` and `status` do not let it — they are ids from configuration, a
// route name and an HTTP status. `model` did. internal/server reads it from the
// request body and meters it verbatim on every request including the ones it
// refuses (TestMeteredModelIsTheCallersOwnBytes), so a key allowed exactly one
// model could mint label values by asking for models it may not have, and the
// series cap was the only thing in the way.
//
// The cap bounded memory and nothing else. What it could not bound is stated in
// TestAModelAddedAfterAFloodStillGetsItsOwnSeries, which is the recovery half of
// this pair. This half is the admission rule: a name the configuration does not
// serve never becomes a label value and therefore never occupies an entry, so
// the bound is structural (DESIGN §17.1) rather than a number a caller races to.
func TestTheConfiguredSetBoundsTheModelLabel(t *testing.T) {
	const maxModels = 4
	q := NewRequests(RequestsOptions{MaxModels: maxModels})
	q.SetAdmittedModels([]string{"a-real-model"})

	// A caller-chosen string does not reach the label. The bytes still arrive
	// here — nothing upstream validates them, and nothing should, because the
	// model ASKED FOR is what the ledger records — but they are an input to the
	// label rather than the label.
	minted := `gpt-4o"; DROP` // and the renderer would have had to escape it
	q.Observe(Sample{Model: minted, Provider: "p", Credential: "c",
		Endpoint: "chat_completions", Status: 404, Duration: time.Millisecond})

	fams, err := Parse(renderRequests(q))
	if err != nil {
		t.Fatal(err)
	}
	got := labelValues(fams, "dorang_request_duration_seconds", "model")
	if got[minted] {
		t.Errorf("the caller's own string is the label value: %v", keysOf(got))
	}
	if !got[UnknownModelSentinel] {
		t.Errorf("an unserved model is not in the unknown bucket; got %v", keysOf(got))
	}
	// The same value on the headline family, from the same resolution. Two
	// answers here would be two dashboards that disagree about one request.
	if rt := labelValues(fams, "dorang_requests_total", "model"); rt[minted] || !rt[UnknownModelSentinel] {
		t.Errorf("dorang_requests_total disagrees with the histogram: %v", keysOf(rt))
	}

	// Flooding is now free of consequence: four times the cap in fabrications
	// occupies nothing, so nothing folds and the fold counter stays still.
	for i := 0; i < maxModels*8; i++ {
		q.Observe(Sample{Model: "fabricated-" + strconv.Itoa(i), Provider: "p",
			Credential: "c", Endpoint: "chat_completions", Status: 404,
			Duration: time.Millisecond})
	}
	q.Observe(Sample{Model: "a-real-model", Provider: "p", Credential: "c",
		Endpoint: "chat_completions", Status: 200, Duration: time.Millisecond})

	fams, err = Parse(renderRequests(q))
	if err != nil {
		t.Fatal(err)
	}
	got = labelValues(fams, "dorang_request_duration_seconds", "model")
	if !got["a-real-model"] {
		t.Errorf("the configured model has no series of its own: %v", keysOf(got))
	}
	if got[OverflowSentinel] {
		t.Errorf("the flood folded something; it should not have reached the table: %v", keysOf(got))
	}
	if n := q.models.f.Folds(); n != 0 {
		t.Errorf("dorang_metrics_cardinality_folds_total moved by %d on caller traffic alone", n)
	}
	// Two: the configured model and the one unknown bucket. 32 fabrications
	// bought the caller nothing.
	if len(got) != 2 {
		t.Errorf("%d model series, want 2 (the configured one and %s): %v",
			len(got), UnknownModelSentinel, keysOf(got))
	}
}

// TestAModelAddedAfterAFloodStillGetsItsOwnSeries is the recovery property, and
// it is the half a cap can never have.
//
// Entries are never evicted — deliberately, because evicting one and recreating
// it later restarts its counters, and a counter that restarts is a process
// restart to `rate()`. So a table that a caller can spend stays spent for the
// life of the process, and the operator's first symptom is not the flood: it is
// the config reload afterwards that adds a real model and finds no room for it,
// with no duration, TTFT or prefix-hit series of its own until a restart.
//
// The fix has to make the flood unable to spend the table, which is the previous
// test, and it has to let the reload raise the bound, which is this one. Both,
// or the second model in a two-model configuration is one flood away from being
// invisible.
func TestAModelAddedAfterAFloodStillGetsItsOwnSeries(t *testing.T) {
	// A cap of exactly one entry, so there is no headroom to hide behind: the
	// only way "added-by-reload" gets a series is if admission kept the flood
	// out AND the reload raised the cap for it.
	q := NewRequests(RequestsOptions{MaxModels: 1})
	q.SetAdmittedModels([]string{"first-model"})

	q.Observe(Sample{Model: "first-model", Provider: "p", Credential: "c",
		Endpoint: "chat_completions", Status: 200, Duration: time.Millisecond})
	for i := 0; i < 500; i++ {
		q.Observe(Sample{Model: "fabricated-" + strconv.Itoa(i), Provider: "p",
			Credential: "c", Endpoint: "chat_completions", Status: 404,
			Duration: time.Millisecond})
	}

	// The reload. A model is added to the configuration and traffic arrives for
	// it.
	q.SetAdmittedModels([]string{"first-model", "added-by-reload"})
	q.Observe(Sample{Model: "added-by-reload", Provider: "p", Credential: "c",
		Endpoint: "chat_completions", Status: 200, Duration: 2 * time.Millisecond,
		TTFT: 30 * time.Millisecond, Routed: true})

	fams, err := Parse(renderRequests(q))
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{
		"dorang_requests_total",
		"dorang_request_duration_seconds",
		"dorang_ttft_seconds",
		"dorang_prefix_routed_total",
	} {
		got := labelValues(fams, family, "model")
		if !got["added-by-reload"] {
			t.Errorf("%s has no series for the model the reload added: %v",
				family, keysOf(got))
		}
	}
	// And the model that was already there kept its own series rather than
	// being displaced to make room.
	if got := labelValues(fams, "dorang_request_duration_seconds", "model"); !got["first-model"] {
		t.Errorf("the model observed before the flood lost its series: %v", keysOf(got))
	}

	// A name the reload REMOVES keeps the series it has. Withdrawing it would
	// be the counter reset this design refuses; it simply stops advancing.
	q.SetAdmittedModels([]string{"added-by-reload"})
	q.Observe(Sample{Model: "first-model", Provider: "p", Credential: "c",
		Endpoint: "chat_completions", Status: 200, Duration: time.Millisecond})
	if fams, err = Parse(renderRequests(q)); err != nil {
		t.Fatal(err)
	}
	if got := labelValues(fams, "dorang_request_duration_seconds", "model"); !got["first-model"] {
		t.Errorf("a de-configured model's series vanished from the scrape, which reads "+
			"as a counter reset: %v", keysOf(got))
	}
}

// TestARouteWithNoModelIsNotAnUnknownModel keeps /v1/models, the health probes
// and the passthrough relays out of the unknown bucket.
//
// They carry no model at all, which is not the same claim as "a model dorang
// does not serve", and folding them together would both misreport those routes
// and change what an existing dashboard shows for them.
func TestARouteWithNoModelIsNotAnUnknownModel(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	q.SetAdmittedModels([]string{"m1"})
	q.Observe(Sample{Provider: "p", Credential: "c", Endpoint: "health", Status: 200,
		Duration: time.Millisecond})

	fams, err := Parse(renderRequests(q))
	if err != nil {
		t.Fatal(err)
	}
	got := labelValues(fams, "dorang_requests_total", "model")
	if got[UnknownModelSentinel] {
		t.Errorf("a route with no model was reported as an unserved model: %v", keysOf(got))
	}
	if !got[""] {
		t.Errorf("the empty model label is gone: %v", keysOf(got))
	}
}

// renderRequests collects one [Requests] on its own so a test can read the
// families it owns without the rest of the registry's output.
func renderRequests(q *Requests) []byte {
	r := New(nil)
	r.Register(q)
	return r.Metrics(nil)
}

// labelValues is the set of values a family carries for one label name.
func labelValues(fams []Family, family, label string) map[string]bool {
	out := map[string]bool{}
	for _, f := range fams {
		if f.Name != family {
			continue
		}
		for _, s := range f.Samples {
			out[s.Label(label)] = true
		}
	}
	return out
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

	// MetricsPublic, because this test is about which renderer produced the
	// body rather than about who may read it. The access rule has its own tests
	// in internal/server and internal/app.
	srv, err := server.New(server.Options{Metrics: reg, MetricsAccess: server.MetricsPublic})
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
	srv, err := server.New(server.Options{MetricsAccess: server.MetricsPublic})
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
	srv, err := server.New(server.Options{Metrics: reg, MetricsAccess: server.MetricsPublic})
	if err != nil {
		t.Fatal(err)
	}
	reg.Register(NewServerCollector(srv))

	stop := make(chan struct{})
	// running closes once the reload loop has actually reloaded once. Without
	// it the scrape loop below can finish its two hundred iterations before the
	// goroutine is ever scheduled, and the test then fails on its own
	// "no reload ran" guard — which is the guard doing its job, so the fix
	// belongs here rather than in the guard.
	running := make(chan struct{})
	var reloads atomic.Int64
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = srv.Reload(server.Options{Metrics: reg})
			if reloads.Add(1) == 1 {
				close(running)
			}
		}
	}()
	defer close(stop)
	<-running

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

// BenchmarkObserve measures the observation path in the three states the model
// label's admission rule puts it in.
//
// `no admitted set` is what this package did before [Requests.SetAdmittedModels]
// existed and is the baseline the other two are read against; it is also what a
// bare [NewRequests] still does. `configured model` is what an assembled gateway
// serves — one lookup in an immutable map behind an atomic pointer, on every
// request. `unserved model` is the same lookup missing, which is the path a
// caller minting names drives, and it must not be the cheaper one to reach or
// the flood would be an amplification.
//
// All three must report zero allocations. DESIGN §15.5 forbids formatted string
// construction here, and a map lookup that allocated would be exactly that
// wearing a different shape.
func BenchmarkObserve(b *testing.B) {
	s := Sample{Model: "m", Provider: "p", Credential: "c", Endpoint: "chat",
		Status: 200, Duration: time.Millisecond, TTFT: time.Millisecond, Routed: true}

	// A realistic set rather than a set of one: the lookup's cost is a hash of
	// the name plus a bucket probe, and a map with a single entry would measure
	// a case no deployment runs.
	names := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		names = append(names, "configured-model-"+strconv.Itoa(i))
	}
	names = append(names, "m")

	for _, c := range []struct {
		name  string
		admit []string
		model string
	}{
		{"no admitted set", nil, "m"},
		{"configured model", names, "m"},
		{"unserved model", names, "not-a-configured-model"},
	} {
		b.Run(c.name, func(b *testing.B) {
			q := NewRequests(RequestsOptions{})
			if c.admit != nil {
				q.SetAdmittedModels(c.admit)
			}
			sample := s
			sample.Model = c.model
			q.Observe(sample)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					q.Observe(sample)
				}
			})
		})
	}
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

func TestModelTokenAndCostAccounting(t *testing.T) {
	q := NewRequests(RequestsOptions{})
	q.SetAdmittedModels([]string{"m"})
	for i := 0; i < 2; i++ {
		q.Observe(Sample{Model: "m", Endpoint: "chat", Status: 200, Routed: true, Tokens: Tokens{Input: 100, Output: 20, CacheRead: 60, CacheWrite: 5, Reasoning: 8}, CostNano: 320000, Priced: true})
	}
	// An unpriced request contributes tokens, never an invented billed amount.
	q.Observe(Sample{Model: "m", Endpoint: "chat", Status: 500, Routed: true, Tokens: Tokens{Input: 10}, CostNano: 999999, Priced: false})
	r := New(nil)
	r.Register(q)
	body := string(r.Metrics(nil))
	for _, want := range []string{
		`dorang_model_tokens_total{model="m",kind="input"} 210`,
		`dorang_model_tokens_total{model="m",kind="output"} 40`,
		`dorang_model_tokens_total{model="m",kind="cache_read"} 120`,
		`dorang_model_tokens_total{model="m",kind="cache_write"} 10`,
		`dorang_model_tokens_total{model="m",kind="reasoning"} 16`,
		`dorang_model_cost_nano_total{model="m"} 640000`,
		`dorang_model_pricing_requests_total{model="m",pricing="priced"} 2`,
		`dorang_model_pricing_requests_total{model="m",pricing="unpriced"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}
