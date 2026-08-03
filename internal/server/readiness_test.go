package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/health"
)

// The gate this package was written for is health.Dependency, so that is what
// these tests drive. Importing it from a test file rather than from the package
// keeps the compile-time edge out of the HTTP surface — deps.go exists so that
// internal/server depends on nobody — while still proving that the two halves
// of the mechanism actually fit together. A hand-rolled stub here would prove
// only that the stub fits.

// depClock is a hand-driven time source. Every threshold in the gate is elapsed
// wall time, so these tests move time rather than sleep through it.
func depClock() (func() time.Time, func(time.Duration)) {
	var mu sync.Mutex
	now := time.Unix(1_800_000_000, 0)
	return func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}, func(d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(d)
		}
}

// storeGate builds the gate a deployment would wire to its credential store.
func storeGate(now func() time.Time) *health.Dependency {
	return health.NewDependency(health.DependencyOptions{
		Name:        "store",
		MinOutage:   15 * time.Second,
		MinFailures: 3,
		Now:         now,
	})
}

// healthProbe reads one health endpoint and returns the status and the decoded
// body.
func healthProbe(t *testing.T, s *Server, path string) (int, map[string]any) {
	t.Helper()
	w := do(s, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s answered unparseable JSON %q: %v", path, w.Body.String(), err)
	}
	return w.Code, body
}

// gateOf digs one gate out of a health body.
func gateOf(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	gates, ok := body["gates"].(map[string]any)
	if !ok {
		t.Fatalf("no gates object in %v", body)
	}
	g, ok := gates[name].(map[string]any)
	if !ok {
		t.Fatalf("no gate %q in %v", name, gates)
	}
	return g
}

// TestReadinessFollowsAStoreOutageAndRecoversWithoutARestart is the whole
// defect and the whole fix, in the order an operator hit them.
//
// With the store stopped, /health/readiness kept answering `ready` throughout.
// Cached principals kept serving, so nothing aggregate looked wrong, while
// every caller whose key this node had not recently seen got a 503 — from a
// load balancer that was still routing here. §13 draws readiness as the signal
// that decides whether new work should arrive, and a node that cannot
// authenticate anyone new is exactly a node that should stop receiving it.
//
// The second half matters as much as the first: the same outage recovered with
// no restart at all. So the gate has to let go by itself, the moment the store
// answers, without anything being redeployed.
func TestReadinessFollowsAStoreOutageAndRecoversWithoutARestart(t *testing.T) {
	now, advance := depClock()
	gate := storeGate(now)
	s := newTestServer(t, func(o *Options) { o.ReadinessGates = []ReadinessGate{gate} })

	// 1. Healthy. The gate reports even when it is open, for the reason the
	//    meter reports when it is healthy: an operator has to be able to tell
	//    "the store is fine" from "this build does not check".
	code, body := healthProbe(t, s, "/health/readiness")
	if code != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("a healthy node answered %d %v", code, body)
	}
	if g := gateOf(t, body, "store"); g["ready"] != true {
		t.Fatalf("the store gate is closed on a healthy node: %v", g)
	}

	// 2. The store goes away, and the node stops accepting new work — but only
	//    after the outage is sustained. Four probes over fifteen seconds.
	for i := 0; i < 4; i++ {
		gate.Fail()
		advance(5 * time.Second)
	}
	code, body = healthProbe(t, s, "/health/readiness")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("the store is unreachable and readiness answered %d %v", code, body)
	}
	// `not_ready`, not `draining`. The two are different instructions: a
	// draining pod is being replaced, this one is waiting for a dependency and
	// will come back by itself.
	if body["status"] != "not_ready" {
		t.Errorf("status = %v, want not_ready", body["status"])
	}
	if g := gateOf(t, body, "store"); g["ready"] != false || g["reason"] != "unreachable" {
		t.Errorf("the gate did not say why: %v", g)
	}

	// 3. Liveness is untouched. Restarting a node whose database is down does
	//    not give it a database, and a liveness probe that followed readiness
	//    here would turn a recoverable outage into a cold start — the same
	//    mistake as pointing liveness at /health/readiness during a drain.
	for _, p := range []string{"/health/liveness", "/health/liveliness"} {
		code, body := healthProbe(t, s, p)
		if code != http.StatusOK || body["status"] != "alive" {
			t.Errorf("%s answered %d %v during a store outage", p, code, body)
		}
		if _, ok := body["gates"]; ok {
			t.Errorf("%s carries gate detail: %v", p, body)
		}
	}

	// 4. And the process is not draining. Nothing about a closed gate starts a
	//    shutdown; requests that reach this node are still served, which is
	//    what keeps the cached principals working while the balancer moves the
	//    rest away.
	if w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`)); w.Code != http.StatusOK {
		t.Errorf("a request answered %d while the gate was closed", w.Code)
	}

	// 5. The store returns. One successful consultation is enough — the node
	//    can serve, so it must serve, and a hysteresis on the way back is
	//    capacity withheld from a store that is already answering.
	gate.OK()
	code, body = healthProbe(t, s, "/health/readiness")
	if code != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("the store came back and readiness answered %d %v", code, body)
	}
	if g := gateOf(t, body, "store"); g["ready"] != true {
		t.Errorf("the gate stayed closed after recovery: %v", g)
	}
}

// TestAStoreBlipDoesNotDrainTheNode is the constraint that stops the fix being
// worse than the defect.
//
// The outage this models recovered on its own: the process reconnected
// transparently, its restart count stayed zero, and the metering buffered
// during the gap was written when the store returned. A gate that fired on the
// first failed query would take a node out of rotation for a two-second blip —
// and when every node shares one store, it would take the whole fleet out at
// once, converting a blip nobody would have noticed into an outage.
func TestAStoreBlipDoesNotDrainTheNode(t *testing.T) {
	now, advance := depClock()
	gate := storeGate(now)
	s := newTestServer(t, func(o *Options) { o.ReadinessGates = []ReadinessGate{gate} })

	// A busy node during a two-second interruption: two hundred failures, which
	// is far past any count threshold, inside far less than the fifteen seconds
	// the duration threshold asks for.
	for i := 0; i < 200; i++ {
		gate.Fail()
		advance(10 * time.Millisecond)
	}
	if code, body := healthProbe(t, s, "/health/readiness"); code != http.StatusOK {
		t.Fatalf("a two-second blip drained the node: %d %v", code, body)
	}
	if !s.Ready() {
		t.Error("Ready() went false through a blip")
	}
}

// TestDrainAndAClosedGateAreReportedApart: both answer 503 and they are not the
// same instruction. A drain is terminal and this process is going away; a
// closed gate is a dependency that is expected back. An operator reading the
// body has to be able to tell which, and the drain must win when both hold —
// a process on its way out is not coming back whatever its store does.
func TestDrainAndAClosedGateAreReportedApart(t *testing.T) {
	now, advance := depClock()
	gate := storeGate(now)
	s := newTestServer(t, func(o *Options) { o.ReadinessGates = []ReadinessGate{gate} })

	s.StartDrain()
	code, body := healthProbe(t, s, "/health/readiness")
	if code != http.StatusServiceUnavailable || body["status"] != "draining" {
		t.Fatalf("a drain answered %d %v", code, body)
	}

	for i := 0; i < 4; i++ {
		gate.Fail()
		advance(5 * time.Second)
	}
	code, body = healthProbe(t, s, "/health/readiness")
	if code != http.StatusServiceUnavailable || body["status"] != "draining" {
		t.Fatalf("a closed gate relabelled a drain: %d %v", code, body)
	}
}

// TestClosedGateShowsInTheReadyGauge: the scrape and the probe are read by
// different people and must not disagree. dorang_ready is what an alert fires
// on when nobody is watching the probe.
func TestClosedGateShowsInTheReadyGauge(t *testing.T) {
	now, advance := depClock()
	gate := storeGate(now)
	s := newTestServer(t, func(o *Options) {
		o.ReadinessGates = []ReadinessGate{gate}
		o.MetricsAccess = MetricsPublic
	})

	if body := do(s, httptest.NewRequest(http.MethodGet, "/metrics", nil)).Body.String(); !strings.Contains(body, "dorang_ready 1") {
		t.Fatalf("a healthy node scrapes as not ready:\n%s", body)
	}
	for i := 0; i < 4; i++ {
		gate.Fail()
		advance(5 * time.Second)
	}
	if body := do(s, httptest.NewRequest(http.MethodGet, "/metrics", nil)).Body.String(); !strings.Contains(body, "dorang_ready 0") {
		t.Fatalf("a closed gate did not reach the scrape:\n%s", body)
	}
}

// TestGatesAreAbsentWhenNoneAreConfigured: the body an existing deployment
// parses must not grow a key because this mechanism was added. No gates, no
// object.
func TestGatesAreAbsentWhenNoneAreConfigured(t *testing.T) {
	s := newTestServer(t, nil)
	for _, p := range []string{"/health", "/health/readiness", "/health/liveness"} {
		code, body := healthProbe(t, s, p)
		if code != http.StatusOK {
			t.Errorf("%s answered %d", p, code)
		}
		if _, ok := body["gates"]; ok {
			t.Errorf("%s grew a gates object with no gates configured: %v", p, body)
		}
	}
}

// TestOverallHealthFollowsTheGate: /health is the third probe and it is the one
// a human curls. It must not disagree with /health/readiness about whether this
// node can take work.
func TestOverallHealthFollowsTheGate(t *testing.T) {
	now, advance := depClock()
	gate := storeGate(now)
	s := newTestServer(t, func(o *Options) { o.ReadinessGates = []ReadinessGate{gate} })

	if code, body := healthProbe(t, s, "/health"); code != http.StatusOK || body["status"] != "healthy" {
		t.Fatalf("a healthy node answered %d %v", code, body)
	}
	for i := 0; i < 4; i++ {
		gate.Fail()
		advance(5 * time.Second)
	}
	code, body := healthProbe(t, s, "/health")
	if code != http.StatusServiceUnavailable || body["status"] != "not_ready" {
		t.Fatalf("/health answered %d %v with the store unreachable", code, body)
	}
}

// TestAReloadKeepsTheGates: the gates travel in the snapshot like everything
// else the request path reads, so a SIGHUP must not silently un-gate a node
// whose store is still down.
func TestAReloadKeepsTheGates(t *testing.T) {
	now, advance := depClock()
	gate := storeGate(now)
	s := newTestServer(t, func(o *Options) { o.ReadinessGates = []ReadinessGate{gate} })

	for i := 0; i < 4; i++ {
		gate.Fail()
		advance(5 * time.Second)
	}
	if s.Ready() {
		t.Fatal("the gate did not close")
	}
	if err := s.Reload(Options{
		Auth:           newFakeAuth("good"),
		Dispatcher:     jsonDispatcher(`{"ok":true}`),
		Models:         ModelSlice{{ID: "model-x"}},
		ReadinessGates: []ReadinessGate{gate},
	}); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if s.Ready() {
		t.Error("a reload reopened a closed gate")
	}
}
