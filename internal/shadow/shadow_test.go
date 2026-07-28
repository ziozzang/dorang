package shadow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/server"
)

// syncWriter is a report destination a test can read back safely.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) lines() []Record {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []Record
	for _, l := range strings.Split(strings.TrimSpace(w.buf.String()), "\n") {
		if l == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(l), &r); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// newTestShadower builds a compare-mode Shadower pointed at ref, sampling
// everything, writing its report to a buffer.
func newTestShadower(t *testing.T, refURL string, w *syncWriter, tune func(*Options)) *Shadower {
	t.Helper()
	opts := Options{
		Mode:                 ModeCompare,
		ReferenceURL:         refURL,
		ReferenceKey:         "ref-secret", // pragma: allowlist secret — a test literal
		ReferenceTimeout:     2 * time.Second,
		SampleRate:           1,
		Structural:           true,
		MaxCostNanoUSDPerDay: 1_000_000_000_000,
		ReportWriter:         w,
		Workers:              2,
		QueueSize:            8,
		CloseGrace:           100 * time.Millisecond,
		Logf:                 func(string, ...any) {},
	}
	if tune != nil {
		tune(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// observation builds one, with the same slice-borrowing contract the server has.
func observation(id string, status int, body string) *server.Observation {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	req := http.Header{}
	req.Set("Content-Type", "application/json")
	req.Set("Authorization", "Bearer client-secret") // pragma: allowlist secret — a test literal
	return &server.Observation{
		RequestID:      id,
		Method:         http.MethodPost,
		Path:           "/v1/chat/completions",
		Route:          "chat_completions",
		Family:         server.FamilyOpenAIChat,
		Model:          "m",
		RequestHeader:  req,
		RequestBody:    []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
		Status:         status,
		ResponseHeader: h,
		ResponseHead:   []byte(body),
		BodyBytes:      int64(len(body)),
		Duration:       5 * time.Millisecond,
		CostNanoUSD:    1_000_000,
		Priced:         true,
	}
}

// waitFor polls until cond or the deadline, so a test never sleeps a fixed
// amount for an asynchronous worker.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

const okBody = `{"id":"x","object":"chat.completion","created":1,"model":"m",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func TestObserveNeverBlocksOnAReferenceThatHangsForever(t *testing.T) {
	// §14.1's hard requirement: a slow or broken reference must not add a
	// microsecond to what the client sees. Observe runs where the meter runs —
	// still inside the request's own goroutine — so if it waited on the
	// reference, every client would wait too.
	release := make(chan struct{})
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	// Cleanups run last-registered-first, so the handlers are released before
	// the server is closed. httptest.Server.Close waits for its handlers, and a
	// handler blocked on a channel nobody closed is a deadlock in the teardown.
	t.Cleanup(ref.Close)
	t.Cleanup(func() { close(release) })

	w := &syncWriter{}
	s := newTestShadower(t, ref.URL, w, func(o *Options) {
		o.ReferenceTimeout = time.Hour // it really does hang
		o.Workers = 2
		o.QueueSize = 4
	})

	start := time.Now()
	for i := 0; i < 50; i++ {
		s.Observe(observation(fmt.Sprintf("hang-%d", i), 200, okBody))
	}
	elapsed := time.Since(start)

	// Two workers are blocked in the reference call and the queue holds four
	// more; the remaining 44 are dropped. None of that may take time.
	if elapsed > 2*time.Second {
		t.Fatalf("50 Observe calls against a hanging reference took %v", elapsed)
	}
	st := s.Stats()
	if st.Dropped == 0 {
		t.Error("a hanging reference with a bounded queue must produce visible drops")
	}
	if st.Queued+st.Dropped != 50 {
		t.Errorf("queued %d + dropped %d != 50 observed", st.Queued, st.Dropped)
	}
}

func TestReferenceFailuresNeverStopTheGatewayAndAreAlwaysRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  func(t *testing.T) string
	}{
		{"500", func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"api_error"}}`))
			}))
			t.Cleanup(srv.Close)
			return srv.URL
		}},
		{"timeout", func(t *testing.T) string {
			block := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-block
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(func() { close(block) })
			return srv.URL
		}},
		{"unreachable", func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			url := srv.URL
			srv.Close() // nothing is listening now
			return url
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &syncWriter{}
			s := newTestShadower(t, tc.ref(t), w, func(o *Options) {
				o.ReferenceTimeout = 150 * time.Millisecond
			})
			s.Observe(observation("fail-1", 200, okBody))

			// Wait for the REPORT, not for the counter. m.sent is incremented
			// as soon as the reference call returns and the record is written
			// afterwards, so waiting on Sent races the write and this test
			// failed roughly one run in eight.
			waitFor(t, 5*time.Second, "the comparison to be reported", func() bool {
				return len(w.lines()) > 0
			})
			st := s.Stats()
			if st.Panics != 0 {
				t.Errorf("worker panicked %d times", st.Panics)
			}
			// A comparison that did not happen is never counted clean.
			if st.Clean != 0 {
				t.Errorf("clean = %d; a failed or 500 reference is not a clean comparison", st.Clean)
			}
			recs := w.lines()
			if len(recs) == 0 {
				t.Fatal("a failed comparison must appear in the report, not vanish")
			}
			if tc.name != "500" && recs[0].ReferenceError == "" {
				t.Errorf("record does not name the transport failure: %+v", recs[0])
			}
		})
	}
}

func TestQueueFullDropsAndCountsRatherThanBlocking(t *testing.T) {
	gate := make(chan struct{})
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
	}))
	t.Cleanup(ref.Close)
	t.Cleanup(func() { close(gate) })

	s := newTestShadower(t, ref.URL, &syncWriter{}, func(o *Options) {
		o.Workers = 1
		o.QueueSize = 2
		o.ReferenceTimeout = time.Hour
	})

	const n = 20
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			s.Observe(observation(fmt.Sprintf("q-%d", i), 200, okBody))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Observe blocked on a full queue; §9.6 rule 3 forbids back-pressure into the request path")
	}

	st := s.Stats()
	// One worker holds one job, the queue holds two, the rest are dropped.
	if st.Dropped < n-4 {
		t.Errorf("dropped %d of %d; the queue is bounded at %d", st.Dropped, n, st.QueueCapacity)
	}
	if st.Queued+st.Dropped != n {
		t.Errorf("queued %d + dropped %d != %d", st.Queued, st.Dropped, n)
	}
	if !strings.Contains(string(s.Metrics(nil)), "dorang_shadow_dropped_total") {
		t.Error("drops must be visible on the metrics endpoint, not only in Stats")
	}
}

func TestDailyCostCapStopsShadowingAndIsVisible(t *testing.T) {
	var hits atomic.Int64
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(ref.Close)

	w := &syncWriter{}
	// Three cents of allowance and one cent per call.
	s := newTestShadower(t, ref.URL, w, func(o *Options) {
		o.MaxCostNanoUSDPerDay = 30_000_000
		o.Workers = 1
	})

	for i := 0; i < 10; i++ {
		ob := observation(fmt.Sprintf("cost-%d", i), 200, okBody)
		ob.CostNanoUSD = 10_000_000
		s.Observe(ob)
		// Let the call settle so the reservations do not simply queue up; the
		// point being tested is the ceiling, not the queue.
		waitFor(t, 2*time.Second, "the call to settle", func() bool {
			st := s.Stats()
			return st.Sent+st.SkippedCapped >= uint64(i+1)
		})
	}

	st := s.Stats()
	if !st.CostCapped {
		t.Fatalf("the ceiling did not stop shadowing: %+v", st)
	}
	if st.Sent > 3 {
		t.Errorf("sent %d calls against a ceiling that allows 3", st.Sent)
	}
	if st.SkippedCapped == 0 {
		t.Error("requests refused by the ceiling must be counted")
	}

	// Visible in metrics.
	m := string(s.Metrics(nil))
	if !strings.Contains(m, "dorang_shadow_cost_capped 1") {
		t.Errorf("the cap is not visible in metrics:\n%s", m)
	}
	// Visible in health, and the gate says so in one word.
	h := string(s.Health(nil))
	if !strings.Contains(h, `"cost_capped":true`) {
		t.Errorf("the cap is not visible in health: %s", h)
	}
	if !strings.Contains(h, `"gate":"stopped_cost_capped"`) {
		t.Errorf("the health verdict does not say the gate stopped: %s", h)
	}

	// And a capped day stops sampling too, so the capture tap is not paid for
	// either.
	if s.Sample("anything", nil) {
		t.Error("a capped day must stop sampling, not merely stop dispatching")
	}
}

func TestCapDoesNotPreferentiallySampleCheapRequests(t *testing.T) {
	// The subtle failure the hard stop exists to prevent: a ceiling that skips
	// the expensive request and keeps the cheap one produces high coverage over
	// cheap traffic and none over the traffic that matters.
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(ref.Close)

	s := newTestShadower(t, ref.URL, &syncWriter{}, func(o *Options) {
		o.MaxCostNanoUSDPerDay = 15_000_000
		o.Workers = 1
	})

	// One expensive request exhausts the ceiling.
	expensive := observation("expensive", 200, okBody)
	expensive.CostNanoUSD = 20_000_000
	s.Observe(expensive)

	if !s.Stats().CostCapped {
		t.Fatal("an over-ceiling request must trip the stop")
	}
	// Now a stream of cheap ones. Not one of them may be admitted.
	for i := 0; i < 20; i++ {
		cheap := observation(fmt.Sprintf("cheap-%d", i), 200, okBody)
		cheap.CostNanoUSD = 1
		s.Observe(cheap)
	}
	if st := s.Stats(); st.Queued != 0 {
		t.Fatalf("queued %d cheap requests after the ceiling stopped; "+
			"skipping instead of stopping biases coverage toward cheap traffic", st.Queued) // pragma: allowlist secret — test fixture
	}
}

func TestUnpricedRequestsAreChargedAnEstimateRatherThanZero(t *testing.T) {
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(ref.Close)

	s := newTestShadower(t, ref.URL, &syncWriter{}, func(o *Options) {
		o.MaxCostNanoUSDPerDay = 25_000_000
		o.UnpricedEstimateNanoUSD = 10_000_000
		o.Workers = 1
	})
	for i := 0; i < 6; i++ {
		ob := observation(fmt.Sprintf("unpriced-%d", i), 200, okBody)
		ob.Priced = false
		ob.CostNanoUSD = 0
		s.Observe(ob)
		waitFor(t, 2*time.Second, "settle", func() bool {
			st := s.Stats()
			return st.Sent+st.SkippedCapped >= uint64(i+1)
		})
	}
	st := s.Stats()
	if st.UnpricedEstimates == 0 {
		t.Error("unpriced requests must be counted, so an operator can see the ceiling is enforced against a guess")
	}
	if !st.CostCapped {
		t.Fatalf("charging zero for an unpriced request makes the ceiling unenforceable: %+v", st)
	}
}

func TestReferenceNeverReceivesTheClientCredential(t *testing.T) {
	var got http.Header
	var mu sync.Mutex
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(ref.Close)

	s := newTestShadower(t, ref.URL, &syncWriter{}, nil)
	ob := observation("cred-1", 200, okBody)
	ob.RequestHeader.Set("X-Api-Key", "client-anthropic-secret") // pragma: allowlist secret
	ob.RequestHeader.Set("X-Dorang-Api-Key", "client-dorang-secret")
	s.Observe(ob)

	waitFor(t, 3*time.Second, "the reference call", func() bool { return s.Stats().Sent > 0 })
	mu.Lock()
	defer mu.Unlock()
	if v := got.Get("Authorization"); v != "Bearer ref-secret" { // pragma: allowlist secret — test fixture
		t.Errorf("Authorization = %q, want the reference's own credential", v)
	}
	for _, name := range []string{"X-Dorang-Api-Key", "Api-Key", "X-Goog-Api-Key"} {
		if v := got.Get(name); v != "" {
			t.Errorf("client credential leaked to the reference in %s: %q", name, v)
		}
	}
	if v := got.Get("X-Api-Key"); strings.Contains(v, "client") {
		t.Errorf("client credential leaked to the reference in X-Api-Key: %q", v)
	}
	if got.Get(HeaderShadow) == "" {
		t.Error("the shadow call must be marked, or two gateways pointed at each other amplify without bound")
	}
	if got.Get(HeaderShadowRequestID) != "cred-1" {
		t.Error("the original request id must travel with the shadow call, or a difference cannot be traced")
	}
}

func TestSampleRefusesARequestThatIsItselfAShadowCopy(t *testing.T) {
	s := newTestShadower(t, "http://127.0.0.1:1", &syncWriter{}, nil)
	if !s.Sample("plain", nil) {
		t.Fatal("sample rate 1 must admit an ordinary request")
	}
	h := http.Header{}
	h.Set(HeaderShadow, "1")
	if s.Sample("copy", h) {
		t.Fatal("a shadow copy must never be shadowed again")
	}
	if s.Stats().SkippedLoop == 0 {
		t.Error("loop-guard refusals must be counted")
	}
}

func TestReportRecordsOnlyDiffsAndInconclusiveComparisons(t *testing.T) {
	// The completion criterion depends on this asymmetry: a clean comparison
	// writes nothing, so an empty file means every comparison decided every
	// dimension and found nothing.
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(ref.Close)

	w := &syncWriter{}
	s := newTestShadower(t, ref.URL, w, nil)
	s.Observe(observation("clean-1", 200, okBody))
	waitFor(t, 3*time.Second, "the clean comparison", func() bool { return s.Stats().Clean > 0 })
	if recs := w.lines(); len(recs) != 0 {
		t.Fatalf("a clean comparison must write nothing to the report, got %+v", recs)
	}

	// Now one that differs: the reference drops usage.
	s.Observe(observation("diff-1", 200,
		`{"id":"y","object":"chat.completion","created":2,"model":"m",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2},"extra":true}`))
	waitFor(t, 3*time.Second, "the differing comparison", func() bool { return s.Stats().WithDiffs > 0 })

	recs := w.lines()
	if len(recs) != 1 {
		t.Fatalf("want exactly one report line, got %d: %+v", len(recs), recs)
	}
	if recs[0].RequestID != "diff-1" || len(recs[0].Diffs) == 0 {
		t.Fatalf("report record is not the differing comparison: %+v", recs[0])
	}
	if recs[0].CostNanoUSD == 0 {
		t.Error("a report record must say what the shadow call cost")
	}
}

func TestReportByteCapDropsVisiblyRatherThanFillingADisk(t *testing.T) {
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"different":true}`))
	}))
	t.Cleanup(ref.Close)

	w := &syncWriter{}
	s := newTestShadower(t, ref.URL, w, func(o *Options) {
		o.ReportMaxBytes = 200 // one record, maybe
		o.Workers = 1
	})
	for i := 0; i < 10; i++ {
		s.Observe(observation(fmt.Sprintf("cap-%d", i), 200, okBody))
	}
	waitFor(t, 5*time.Second, "records to be refused", func() bool {
		return s.Stats().ReportDropped > 0
	})
	if !strings.Contains(string(s.Metrics(nil)), "dorang_shadow_report_dropped_total") {
		t.Error("report drops must be visible, or a truncated report is read as an empty one")
	}
	if v := gateVerdict(s.Stats()); v == "clean" {
		t.Error("a run with dropped report records must not read as clean")
	}
}

func TestMirrorModeRecordsWithoutComparing(t *testing.T) {
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"totally":"different"}`))
	}))
	t.Cleanup(ref.Close)

	w := &syncWriter{}
	s := newTestShadower(t, ref.URL, w, func(o *Options) { o.Mode = ModeMirror })
	s.Observe(observation("mirror-1", 200, okBody))
	waitFor(t, 3*time.Second, "the mirrored call", func() bool { return s.Stats().Mirrored > 0 })

	st := s.Stats()
	if st.Compared != 0 {
		t.Errorf("mirror mode must not compare, compared = %d", st.Compared)
	}
	if len(w.lines()) != 0 {
		t.Error("mirror mode writes no diff records")
	}
	if v := gateVerdict(st); v != "mirroring_only" {
		t.Errorf("gate verdict = %q, want mirroring_only", v)
	}
}

func TestGateVerdictReservesCleanForACompleteRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   Stats
		want string
	}{
		{"nothing yet", Stats{Mode: "compare"}, "no_data"},
		{"all clean", Stats{Mode: "compare", Compared: 10, Clean: 10}, "clean"},
		{"one diff", Stats{Mode: "compare", Compared: 10, Clean: 9, WithDiffs: 1}, "diffs"},
		{"one inconclusive", Stats{Mode: "compare", Compared: 10, Clean: 9, Inconclusive: 1}, "incomplete"},
		{"queue drops", Stats{Mode: "compare", Compared: 10, Clean: 10, Dropped: 1}, "incomplete"},
		{"reference errors", Stats{Mode: "compare", Compared: 10, Clean: 10, ReferenceErrors: 1}, "incomplete"},
		{"report drops", Stats{Mode: "compare", Compared: 10, Clean: 10, ReportDropped: 1}, "incomplete"},
		{"oversize skips", Stats{Mode: "compare", Compared: 10, Clean: 10, SkippedOversize: 1}, "incomplete"},
		{"capped", Stats{Mode: "compare", Compared: 10, Clean: 10, CostCapped: true}, "stopped_cost_capped"},
	} {
		if got := gateVerdict(tc.st); got != tc.want {
			t.Errorf("%s: verdict = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestObserveCopiesEverythingItIsHanded(t *testing.T) {
	// server.Observation borrows the pooled request. An implementation that
	// kept a slice would read another request's body off a worker goroutine —
	// which is a data race and a cross-tenant leak in the same bug.
	gate := make(chan struct{})
	var seen []byte
	var mu sync.Mutex
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		mu.Lock()
		seen = b[:n]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(ref.Close)

	s := newTestShadower(t, ref.URL, &syncWriter{}, nil)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"original"}]}`)
	ob := observation("copy-1", 200, okBody)
	ob.RequestBody = body
	s.Observe(ob)

	// Scribble over the caller's buffer, as the request pool would.
	for i := range body {
		body[i] = 'X'
	}
	close(gate)

	waitFor(t, 3*time.Second, "the reference call", func() bool { return s.Stats().Sent > 0 })
	mu.Lock()
	defer mu.Unlock()
	if bytes.Contains(seen, []byte("XXXX")) {
		t.Fatalf("the queued job aliased the caller's buffer: reference received %q", seen)
	}
	if !bytes.Contains(seen, []byte("original")) {
		t.Fatalf("reference received %q, want the body as it was at Observe", seen)
	}
}

func TestOversizeRequestBodyIsSkippedAndCountedNotTruncated(t *testing.T) {
	// Replaying a truncated request body would ask the reference a different
	// question and then compare the answers.
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(ref.Close)

	s := newTestShadower(t, ref.URL, &syncWriter{}, func(o *Options) { o.MaxCaptureBytes = 64 })
	ob := observation("big-1", 200, okBody)
	ob.RequestBody = []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("a", 4096) + `"}]}`)
	s.Observe(ob)

	st := s.Stats()
	if st.SkippedOversize != 1 {
		t.Fatalf("SkippedOversize = %d, want 1", st.SkippedOversize)
	}
	if st.Queued != 0 {
		t.Error("an oversize body must not be replayed truncated")
	}
	if st.SpentNanoUSD != 0 {
		t.Errorf("a skipped call must refund its reservation, spent = %d", st.SpentNanoUSD)
	}
}

func TestNewRefusesConfigurationsThatWouldProduceAnUnbelievableReport(t *testing.T) {
	base := func() Options {
		return Options{
			Mode: ModeCompare, ReferenceURL: "https://ref.example",
			SampleRate: 0.05, Structural: true,
			MaxCostNanoUSDPerDay: 1000, ReportWriter: &syncWriter{},
		}
	}
	for _, tc := range []struct {
		name string
		tune func(*Options)
	}{
		{"no ceiling", func(o *Options) { o.MaxCostNanoUSDPerDay = 0 }},
		{"compare with nothing enabled", func(o *Options) { o.Structural = false }},
		{"compare with no report", func(o *Options) { o.ReportWriter = nil; o.ReportPath = "" }},
		{"rate above one", func(o *Options) { o.SampleRate = 1.5 }},
		{"non-http reference", func(o *Options) { o.ReferenceURL = "file:///etc/passwd" }},
	} {
		o := base()
		tc.tune(&o)
		if s, err := New(o); err == nil {
			_ = s.Close()
			t.Errorf("%s: New accepted it", tc.name)
		}
	}
	if _, err := New(Options{Mode: ModeOff}); err != ErrOff {
		t.Errorf("mode off: err = %v, want ErrOff", err)
	}
}

func TestOffShadowerIsAConstant(t *testing.T) {
	s := NewOff()
	if s.Sample("x", nil) {
		t.Error("an off shadower samples nothing")
	}
	s.Observe(observation("x", 200, okBody))
	if st := s.Stats(); st.Sampled != 0 || st.Queued != 0 {
		t.Errorf("an off shadower does nothing: %+v", st)
	}
	if !strings.Contains(string(s.Health(nil)), `"gate":"off"`) {
		t.Error("health should say the gate is off")
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestCloseIsIdempotentAndDrains(t *testing.T) {
	var served atomic.Int64
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(ref.Close)

	s, err := New(Options{
		Mode: ModeCompare, ReferenceURL: ref.URL, SampleRate: 1, Structural: true,
		MaxCostNanoUSDPerDay: 1_000_000_000_000, ReportWriter: &syncWriter{},
		Workers: 2, QueueSize: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		s.Observe(observation(fmt.Sprintf("drain-%d", i), 200, okBody))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := s.Stats().Queued; int64(got) != served.Load() {
		t.Errorf("queued %d but the reference served %d; Close must drain what was paid for",
			got, served.Load())
	}
}

// A Shadower is a server.Observer. If this stops compiling, the wiring broke.
var _ server.Observer = (*Shadower)(nil)
