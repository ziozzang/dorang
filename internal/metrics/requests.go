package metrics

import (
	"sync/atomic"
	"time"
)

// Tokens is one request's token accounting.
type Tokens struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
}

// Sample is one finished request, flattened.
//
// It carries no pointer into the pooled request struct and is passed by value,
// so the observation point can hand it over without the lifetime rules
// internal/server's Observation contract needs.
type Sample struct {
	// Model is the client-facing model name, "" for a route with none.
	Model string
	// Provider, Credential and Endpoint are ids and fixed-cardinality route
	// names. Endpoint is the route's name, never the raw path: a path is
	// caller-controlled and a caller-controlled label is a memory leak with an
	// HTTP interface.
	Provider   string
	Credential string
	Endpoint   string
	Status     int

	Duration     time.Duration
	TTFT         time.Duration
	CapacityWait time.Duration

	Tokens Tokens

	// CostNano is the billed figure and Priced says whether it means anything.
	// DESIGN §8.3: an unpriced model is not free, it is unpriced.
	CostNano int64
	Priced   bool
	// NotionalNano is DESIGN §8.5's list-rate equivalent and NotionalPriced
	// says whether a notional_rate rule matched. §8.5 rule 5 is explicit that
	// missing is reported and never rendered as zero, because a zero notional
	// makes a subscription look infinitely efficient — the most flattering
	// possible answer and the one least likely to be questioned.
	NotionalNano   int64
	NotionalPriced bool

	// Routed is true once a deployment was chosen, which is what separates a
	// request that could have had a price from one that never reached routing.
	Routed bool
	// PrefixHit reports that cache-affinity routing chose the deployment
	// (DESIGN §7.4).
	PrefixHit bool

	// FallbackFrom, FallbackTo and FallbackReason describe a hop of DESIGN
	// §7.6. Empty From means this was not a fallback.
	FallbackFrom   string
	FallbackTo     string
	FallbackReason string
}

// reqKey is the label tuple of `dorang_requests_total`.
//
// A comparable struct rather than a concatenated string: a string key would
// allocate on every observation, and DESIGN §15.5 prohibits formatted string
// construction on the hot path. Status is an int32 so that the folded series
// can carry a sentinel the label renderer recognises.
type reqKey struct {
	model      string
	provider   string
	credential string
	endpoint   string
	status     int32
}

const overflowStatus int32 = -1

type reqEntry struct {
	n atomic.Uint64
}

type modelKey struct{ model string }

type modelEntry struct {
	duration *Hist
	ttft     *Hist
	// prefixHits and routed are the numerator and the denominator of
	// `dorang_prefix_hit_ratio`. They are counted here rather than read from
	// internal/prefix because the table is keyed by content digest and has
	// never been told which model a lookup belonged to — see [PrefixCollector]
	// for the global figures it does have.
	prefixHits atomic.Uint64
	routed     atomic.Uint64
}

type fallbackKey struct{ from, to, reason string }

type fallbackEntry struct{ n atomic.Uint64 }

// RequestsOptions tunes the caps.
type RequestsOptions struct {
	MaxSeries         int
	MaxModels         int
	MaxFallbackSeries int
}

// Requests owns the per-request families of DESIGN §12.3.
//
// Everything else in this package is pulled from a subsystem that was already
// counting. These are the numbers nobody owned: the request path knew the
// model, the provider, the credential and the status at one instant and then
// threw the tuple away, so §12.3's headline family did not exist.
//
// It is fed from internal/server's existing observation point — the meter
// hand-off in Server.finish, which runs after the client's last byte and
// already sees every request including health probes and the metrics scrape
// itself.
type Requests struct {
	series    *table[reqKey, reqEntry]
	models    *table[modelKey, modelEntry]
	fallbacks *table[fallbackKey, fallbackEntry]

	tokens [5]atomic.Uint64

	costNano     atomic.Int64
	notionalNano atomic.Int64

	unpriced        atomic.Uint64
	notionalMissing atomic.Uint64
	priced          atomic.Uint64
	notionalPriced  atomic.Uint64

	capacityWait *Hist

	// prefixEnabled gates the per-model prefix ratio. When cache-affinity
	// routing is off, every model's hit count is legitimately zero and a ratio
	// of 0.0 would read as "the cache is useless" rather than "there is no
	// cache" — rule 3 of the package comment, and the exact shape of vLLM's
	// `/load` trap in VLLM.md §3.3.
	prefixEnabled atomic.Bool
}

// NewRequests builds the recorder.
func NewRequests(o RequestsOptions) *Requests {
	if o.MaxSeries <= 0 {
		o.MaxSeries = DefaultMaxRequestSeries
	}
	if o.MaxModels <= 0 {
		o.MaxModels = DefaultMaxModelSeries
	}
	if o.MaxFallbackSeries <= 0 {
		o.MaxFallbackSeries = DefaultMaxFallbackSeries
	}
	return &Requests{
		series: newTable[reqKey, reqEntry]("dorang_requests_total", o.MaxSeries, nil),
		models: newTable[modelKey, modelEntry]("dorang_request_duration_seconds", o.MaxModels,
			func(m *modelEntry) {
				m.duration = NewHist(DurationBounds)
				m.ttft = NewHist(TTFTBounds)
			}),
		fallbacks:    newTable[fallbackKey, fallbackEntry]("dorang_fallback_total", o.MaxFallbackSeries, nil),
		capacityWait: NewHist(WaitBounds),
	}
}

// SetPrefixEnabled records whether cache-affinity routing is configured. The
// per-model hit ratio is omitted entirely when it is not.
func (q *Requests) SetPrefixEnabled(on bool) { q.prefixEnabled.Store(on) }

// Observe records one finished request.
//
// In the steady state this is: one read-lock, one map lookup and a handful of
// atomic adds per table it touches. No allocation, no formatting, no string
// construction.
func (q *Requests) Observe(s Sample) {
	k := reqKey{
		model:      s.Model,
		provider:   s.Provider,
		credential: s.Credential,
		endpoint:   s.Endpoint,
		status:     int32(s.Status),
	}
	// A folded observation lands in the one overflow series, whose labels say
	// so; nothing is lost except the resolution the cap refused, and the fold
	// itself is counted.
	e, _ := q.series.get(k)
	e.n.Add(1)

	q.tokens[0].Add(uint64(nonNeg(s.Tokens.Input)))
	q.tokens[1].Add(uint64(nonNeg(s.Tokens.Output)))
	q.tokens[2].Add(uint64(nonNeg(s.Tokens.CacheRead)))
	q.tokens[3].Add(uint64(nonNeg(s.Tokens.CacheWrite)))
	q.tokens[4].Add(uint64(nonNeg(s.Tokens.Reasoning)))

	if s.Priced {
		q.costNano.Add(s.CostNano)
		q.priced.Add(1)
	} else if s.Routed {
		// Only a routed request could have been priced. Counting a 401 as an
		// unpriced model would make the ratio meaningless.
		q.unpriced.Add(1)
	}
	if s.NotionalPriced {
		q.notionalNano.Add(s.NotionalNano)
		q.notionalPriced.Add(1)
	} else if s.Routed {
		q.notionalMissing.Add(1)
	}

	m, _ := q.models.get(modelKey{s.Model})
	m.duration.Observe(s.Duration)
	if s.TTFT > 0 {
		// A zero TTFT means "not measured", not "instant". §7.5a(a) makes the
		// same distinction for routing: absent must never read as fastest.
		m.ttft.Observe(s.TTFT)
	}
	if s.Routed {
		m.routed.Add(1)
		if s.PrefixHit {
			m.prefixHits.Add(1)
		}
		q.capacityWait.Observe(s.CapacityWait)
	}

	if s.FallbackFrom != "" {
		f, _ := q.fallbacks.get(fallbackKey{s.FallbackFrom, s.FallbackTo, s.FallbackReason})
		f.n.Add(1)
	}
}

func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// CollectorName implements [Collector].
func (q *Requests) CollectorName() string { return "requests" }

func (q *Requests) folders() []*folder {
	return []*folder{&q.series.f, &q.models.f, &q.fallbacks.f}
}

// Collect implements [Collector].
func (q *Requests) Collect(w *Writer) {
	w.Metric("dorang_requests_total", Counter,
		"Requests served, by model, provider, credential, status and route. "+
			"Every label is bounded by configuration; the tuple is bounded by a cap "+
			"whose folds are counted in dorang_metrics_cardinality_folds_total.")
	q.series.each(lessReqKey, func(k reqKey, e *reqEntry, over bool) {
		if over {
			k = reqKey{OverflowSentinel, OverflowSentinel, OverflowSentinel,
				OverflowSentinel, overflowStatus}
		}
		w.Label("model", k.model)
		w.Label("provider", k.provider)
		w.Label("credential", k.credential)
		w.Label("status", statusOrOverflow(k.status))
		w.Label("endpoint", k.endpoint)
		w.Uint(e.n.Load())
	})

	w.Metric("dorang_request_duration_seconds", Histogram,
		"Gateway request duration. Buckets resolve microseconds because DESIGN §15.1 "+
			"states the warm-local target as p50 200 µs and p99 2 ms.")
	q.models.each(lessModelKey, func(k modelKey, m *modelEntry, over bool) {
		if m.duration == nil {
			return
		}
		w.Label("model", modelLabel(k, over))
		w.HistogramSample(m.duration)
	})

	w.Metric("dorang_ttft_seconds", Histogram,
		"Time to first token, measured upstream. A request with no measured first "+
			"token contributes no sample: zero would mean instant.")
	q.models.each(lessModelKey, func(k modelKey, m *modelEntry, over bool) {
		if m.ttft == nil || m.ttft.Count() == 0 {
			return
		}
		w.Label("model", modelLabel(k, over))
		w.HistogramSample(m.ttft)
	})

	w.Metric("dorang_capacity_wait_seconds", Histogram,
		"Time a request spent waiting for a capacity reservation (DESIGN §5.4). "+
			"Not broken down per axis: the broker grants atomically across every axis "+
			"and does not record which one blocked.")
	if q.capacityWait.Count() > 0 {
		w.HistogramSample(q.capacityWait)
	}

	w.Metric("dorang_tokens_total", Counter, "Tokens accounted, by kind.")
	for i, kind := range [...]string{"input", "output", "cache_read", "cache_write", "reasoning"} {
		w.Label("kind", kind)
		w.Uint(q.tokens[i].Load())
	}

	w.Metric("dorang_cost_nano_total", Counter,
		"Billed cost in nano-USD (DESIGN §8.3). Requests dorang could not price are "+
			"excluded and counted in dorang_unpriced_requests_total, never charged as zero.")
	w.Int(q.costNano.Load())

	w.Metric("dorang_notional_nano_total", Counter,
		"What the priced traffic would have cost at the provider's pay-as-you-go list "+
			"rate, in nano-USD (DESIGN §8.5). Never billed, never budgeted, never routed "+
			"on. Divided by an amortized subscription it is the plan's realized leverage — "+
			"read it per period, not per request.")
	w.Int(q.notionalNano.Load())

	w.Metric("dorang_priced_requests_total", Counter,
		"Requests a marginal pricing rule matched.")
	w.Uint(q.priced.Load())

	w.Metric("dorang_unpriced_requests_total", Counter,
		"Routed requests no marginal pricing rule matched (DESIGN §8.3). The cost "+
			"counter does not include them; they are not free.")
	w.Uint(q.unpriced.Load())

	w.Metric("dorang_notional_priced_requests_total", Counter,
		"Routed requests a notional_rate rule matched.")
	w.Uint(q.notionalPriced.Load())

	w.Metric("dorang_notional_missing_total", Counter,
		"Routed requests with no notional_rate rule (DESIGN §8.5 rule 5). The list-rate "+
			"figure is unavailable for these, not zero — a zero would make a subscription "+
			"look infinitely efficient.")
	w.Uint(q.notionalMissing.Load())

	w.Metric("dorang_fallback_total", Counter,
		"Fallback hops taken (DESIGN §7.6), by the deployment left, the deployment "+
			"tried and the cause.")
	q.fallbacks.each(lessFallbackKey, func(k fallbackKey, e *fallbackEntry, over bool) {
		if over {
			k = fallbackKey{OverflowSentinel, OverflowSentinel, OverflowSentinel}
		}
		w.Label("from", k.from)
		w.Label("to", k.to)
		w.Label("reason", k.reason)
		w.Uint(e.n.Load())
	})

	w.Metric("dorang_prefix_routed_total", Counter,
		"Routed requests eligible for cache-affinity routing, per model. The "+
			"denominator of dorang_prefix_hit_ratio.")
	q.models.each(lessModelKey, func(k modelKey, m *modelEntry, over bool) {
		if n := m.routed.Load(); n > 0 {
			w.Label("model", modelLabel(k, over))
			w.Uint(n)
		}
	})

	w.Metric("dorang_prefix_hits_total", Counter,
		"Routed requests whose deployment was chosen by cache affinity (DESIGN §7.4).")
	q.models.each(lessModelKey, func(k modelKey, m *modelEntry, over bool) {
		if m.routed.Load() > 0 {
			w.Label("model", modelLabel(k, over))
			w.Uint(m.prefixHits.Load())
		}
	})

	// The ratio is derived, and a derived value with no denominator is the one
	// shape rule 3 exists for. It is omitted when prefix routing is off and
	// per model when that model has routed nothing.
	if q.prefixEnabled.Load() {
		w.Metric("dorang_prefix_hit_ratio", Gauge,
			"Fraction in [0,1] of this model's routed requests placed by cache affinity. "+
				"Absent when cache-affinity routing is off, and absent for a model that "+
				"has routed nothing — an unmeasurable ratio is not a ratio of zero.")
		q.models.each(lessModelKey, func(k modelKey, m *modelEntry, over bool) {
			n := m.routed.Load()
			if n == 0 {
				return
			}
			w.Label("model", modelLabel(k, over))
			w.Float(float64(m.prefixHits.Load()) / float64(n))
		})
	}
}

func modelLabel(k modelKey, over bool) string {
	if over {
		return OverflowSentinel
	}
	return k.model
}

func statusOrOverflow(s int32) string {
	if s == overflowStatus {
		return OverflowSentinel
	}
	return statusLabel(int(s))
}

func lessReqKey(a, b reqKey) bool {
	if a.model != b.model {
		return a.model < b.model
	}
	if a.provider != b.provider {
		return a.provider < b.provider
	}
	if a.credential != b.credential {
		return a.credential < b.credential
	}
	if a.endpoint != b.endpoint {
		return a.endpoint < b.endpoint
	}
	return a.status < b.status
}

func lessModelKey(a, b modelKey) bool { return a.model < b.model }

func lessFallbackKey(a, b fallbackKey) bool {
	if a.from != b.from {
		return a.from < b.from
	}
	if a.to != b.to {
		return a.to < b.to
	}
	return a.reason < b.reason
}
