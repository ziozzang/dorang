package metrics

import (
	"github.com/ziozzang/dorang/internal/health"
)

// HealthSource is the part of [health.Tracker] this package needs.
type HealthSource interface {
	IDs() []string
	Stats(id string) health.Stats
}

// healthStates is every value `state` can take. All of them are emitted for
// every deployment, one at 1 and the rest at 0, because a state label that
// simply disappears when the state changes leaves a stale series behind in
// every dashboard that graphed it.
var healthStates = [...]health.State{health.Closed, health.Open, health.HalfOpen}

// HealthCollector renders the circuit breaker and the measured routing signals
// of DESIGN §7.5a.
//
// The two latency gauges are the reason this collector exists rather than a
// generic one: §7.5a(a) says a deployment with no samples has **no opinion**,
// and "absent must never read as fastest or as slowest". internal/health
// encodes that as a zero EWMA. A metric that published that zero would tell a
// least-latency dashboard that the untested backend is the fastest one on the
// fleet — the same failure VLLM.md §3.3 records for vLLM's `/load`. So a
// deployment with no sample gets no series.
type HealthCollector struct {
	src HealthSource
	max int
	f   folder

	ids []string
}

// NewHealthCollector builds the collector.
func NewHealthCollector(src HealthSource, maxDeployments int) *HealthCollector {
	if maxDeployments <= 0 {
		maxDeployments = DefaultMaxCredentialSeries
	}
	return &HealthCollector{src: src, max: maxDeployments,
		f: folder{family: "dorang_deployment_health"}}
}

// CollectorName implements [Collector].
func (c *HealthCollector) CollectorName() string { return "health" }

func (c *HealthCollector) folders() []*folder { return []*folder{&c.f} }

// Collect implements [Collector].
func (c *HealthCollector) Collect(w *Writer) {
	c.ids = append(c.ids[:0], c.src.IDs()...)
	sortSlice(c.ids, func(a, b string) bool { return a < b })

	// The folded tail is dropped rather than summed, because a circuit state
	// does not add up: "half of the folded deployments are open" is not a
	// number, and inventing one is worse than reporting that resolution ran out
	// — which the fold counter does. The drop is counted per scrape, so the
	// counter rises while the cap is being exceeded and stops when it is not.
	ids := c.ids
	if len(ids) > c.max {
		for range ids[c.max:] {
			c.f.fold()
		}
		ids = ids[:c.max]
	}

	w.Metric("dorang_deployment_health", Gauge,
		"Circuit state per deployment (DESIGN §7.6): 1 for the state in effect, 0 for the "+
			"others. Every state is emitted every scrape so a transition does not leave a "+
			"stale series behind.")
	for _, id := range ids {
		st := c.src.Stats(id)
		for _, s := range healthStates {
			w.Label("deployment", id)
			w.Label("state", s.String())
			w.Bool(st.State == s)
		}
	}

	w.Metric("dorang_deployment_requests_total", Counter,
		"Requests reported to the health tracker per deployment.")
	for _, id := range ids {
		w.Label("deployment", id)
		w.Int(c.src.Stats(id).Requests)
	}

	w.Metric("dorang_deployment_failures_total", Counter,
		"Outcomes the router marked as counting against availability. A 429 or a "+
			"content-policy refusal is not one: it is an error to the caller and says "+
			"nothing about whether the deployment is alive.")
	for _, id := range ids {
		w.Label("deployment", id)
		w.Int(c.src.Stats(id).Failures)
	}

	w.Metric("dorang_deployment_circuit_opens_total", Counter,
		"Times the circuit opened.")
	for _, id := range ids {
		w.Label("deployment", id)
		w.Int(c.src.Stats(id).Opens)
	}

	w.Metric("dorang_deployment_consecutive_failures", Gauge,
		"Consecutive failures right now. DESIGN §7.5a(b) feeds this to ordering, not "+
			"only to admission: a deployment still technically closed but visibly "+
			"degrading should sink in the ranking before it trips.")
	for _, id := range ids {
		w.Label("deployment", id)
		w.Int(c.src.Stats(id).ConsecFails)
	}

	w.Metric("dorang_deployment_ttft_seconds", Gauge,
		"Smoothed time to first token per deployment. A deployment with no sample is "+
			"ABSENT, never zero: §7.5a(a) requires that absent never reads as fastest.")
	for _, id := range ids {
		if d := c.src.Stats(id).TTFT; d > 0 {
			w.Label("deployment", id)
			w.Float(d.Seconds())
		}
	}

	w.Metric("dorang_deployment_tokens_per_second", Gauge,
		"Smoothed generation rate per deployment, excluding time to first token so a deep "+
			"queue is not mistaken for a slow generator. Absent with no sample.")
	for _, id := range ids {
		if r := c.src.Stats(id).TokensPerSec; r > 0 {
			w.Label("deployment", id)
			w.Float(r)
		}
	}

	w.Metric("dorang_deployment_latency_seconds", Gauge,
		"Smoothed total request duration per deployment. Absent with no sample.")
	for _, id := range ids {
		if d := c.src.Stats(id).Total; d > 0 {
			w.Label("deployment", id)
			w.Float(d.Seconds())
		}
	}
}
