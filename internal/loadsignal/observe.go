package loadsignal

import (
	"encoding/json"
	"strings"
	"time"
)

// RequestHeader asks a vLLM backend for a load report on the response, and Header is the
// response header it comes back on (VLLM.md §3.2).
//
// The request header is sent only to engines known to honour it. Sending it to a hosted
// vendor would be a dorang-invented header on somebody else's API, which at best is
// ignored and at worst is a 400.
const (
	RequestHeader = "endpoint-load-metrics-format"
	// RequestFormat is the only format this package reads. See [RefusalWrongFormat] for
	// why the other two are refused rather than parsed.
	RequestFormat = "JSON"
	Header        = "endpoint-load-metrics"
)

// kvMetric is the name vLLM gives KV-cache occupancy inside the ORCA report's
// named_metrics map. It is a FRACTION in [0,1] — the Prometheus series of the same
// quantity is called `kv_cache_usage_perc` and is also a fraction, which is the naming
// defect VLLM.md §3.1 warns about.
const kvMetric = "kv_cache_utilization"

// Refusal says why no usable occupancy came back. It is the whole point of the package:
// an absent measurement has to be reported as a specific absence, because the alternative
// — reporting the number zero — is a claim about the backend that nobody made.
type Refusal uint8

const (
	// RefusalNone: an occupancy was observed.
	RefusalNone Refusal = iota
	// RefusalNoHeader: the response carried no load report at all. An older vLLM, a
	// non-vLLM backend, or an intermediary that dropped an unknown header.
	RefusalNoHeader
	// RefusalWrongFormat: the report came back in TEXT or BIN when dorang asked for
	// JSON, so the server chose the format rather than honouring the request header.
	//
	// It is refused rather than parsed on purpose. A second parser would consume it and
	// hide the fact that dorang's request header is not being honoured — and if that is
	// true for the format it may be true for other things this integration assumes.
	RefusalWrongFormat
	// RefusalMalformed: the report was present and could not be parsed.
	RefusalMalformed
	// RefusalNoKVMetric: the report parsed and carries no KV-cache occupancy. This is
	// "endpoint reachable, no series" in its header form — a successful, well-formed,
	// empty answer — and it must not read as idle.
	RefusalNoKVMetric
	// RefusalZeroIsAmbiguous: the report claims an occupancy of exactly zero.
	//
	// vLLM's own construction defaults this field to 0.0 when the engine had no metrics
	// for the request, so an empty machine and an unreported one produce the identical
	// number. Refusing costs nothing: at zero occupancy the price factor is 1.0, which
	// is what the fallback charges anyway. What it buys is that the row says "unknown"
	// instead of "idle". See the package documentation.
	RefusalZeroIsAmbiguous
	// RefusalNotAFraction: the value is outside [0,1]. vLLM's metric is a fraction
	// whose name says percent, so a reading above 1 is a percentage-scaled source that
	// would multiply the price by a hundred.
	RefusalNotAFraction
	// RefusalStreamed: the answer was streamed, and the load report is a non-streaming
	// feature (VLLM.md §3.2). Streamed traffic is charged the base rate.
	//
	// This is the largest hole in the feature and it is named rather than papered over:
	// in an agent deployment nearly all traffic is streamed, so nearly all traffic is
	// charged at 1.0x. The alternative was a scraped gauge, which is refused for the
	// reason the package documentation gives.
	RefusalStreamed
	// RefusalWrongDeployment: the observation belongs to a different deployment than
	// the one being priced. A request that failed over from one backend to another must
	// not be priced on the contention of the machine it did not run on.
	RefusalWrongDeployment
	// RefusalStale: the observation is older than the caller's bound.
	//
	// It is not a poll interval — nothing is polled. It is a guard on the WIRING: an
	// observation is meant to travel from the response that produced it to the
	// settlement of that same request, a span of microseconds, and a bound makes it
	// impossible for a future refactor to carry one across a request boundary without
	// the price saying so.
	RefusalStale
)

// String is the token that reaches the response header and the ledger row. It is stable
// vocabulary: an operator greps it, and a support script parses it.
func (r Refusal) String() string {
	switch r {
	case RefusalNone:
		return "observed"
	case RefusalNoHeader:
		return "no_load_header"
	case RefusalWrongFormat:
		return "wrong_format"
	case RefusalMalformed:
		return "malformed"
	case RefusalNoKVMetric:
		return "no_kv_metric"
	case RefusalZeroIsAmbiguous:
		return "zero_is_ambiguous"
	case RefusalNotAFraction:
		return "not_a_fraction"
	case RefusalStreamed:
		return "streamed"
	case RefusalWrongDeployment:
		return "wrong_deployment"
	case RefusalStale:
		return "stale"
	}
	return "unknown"
}

// Why renders the refusal for an operator's log and the price explanation.
func (r Refusal) Why() string {
	switch r {
	case RefusalNone:
		return "the backend reported its occupancy on this request's own response"
	case RefusalNoHeader:
		return "the backend returned no endpoint-load-metrics header, so nothing was " +
			"observed; an engine that reports no load is not an idle one"
	case RefusalWrongFormat:
		return "the backend chose a load-report format dorang did not ask for (it asked " +
			"for JSON), so the per-request format header is not being honoured"
	case RefusalMalformed:
		return "the endpoint-load-metrics header could not be parsed"
	case RefusalNoKVMetric:
		return "the load report parsed and carries no " + kvMetric + ": a reachable " +
			"backend reporting no series is not an idle backend (VLLM.md §3.1)"
	case RefusalZeroIsAmbiguous:
		return "the load report claims an occupancy of exactly zero, which vLLM also " +
			"emits when the engine had no metrics for the request; the two are " +
			"indistinguishable, and at zero the price factor is 1.0 either way"
	case RefusalNotAFraction:
		return "the reported occupancy is outside [0,1]; " + kvMetric + " is a FRACTION " +
			"despite the percent in the Prometheus spelling of its name, so a larger " +
			"value is a percentage-scaled source that would overcharge by 100x"
	case RefusalStreamed:
		return "the answer was streamed and vLLM's load report is non-streaming only, so " +
			"this request was charged the base rate"
	case RefusalWrongDeployment:
		return "the observation belongs to another deployment; a request that failed over " +
			"is not priced on the contention of the backend it did not run on"
	case RefusalStale:
		return "the observation is older than the bound an observation may travel across"
	}
	return ""
}

// Observation is one backend's reported occupancy at one instant, tied to the deployment
// it describes and the moment it was read.
//
// The zero value is "nothing observed" and is safe to pass anywhere: [Observation.Usable]
// refuses it.
type Observation struct {
	// Fraction is KV-cache occupancy in [0,1]. 1.0 is a saturated cache.
	Fraction float64
	// Deployment is the deployment whose response carried the report.
	Deployment string
	// At is when dorang read the header.
	At time.Time
	// Refused is why there is no usable reading, or RefusalNone.
	Refused Refusal
}

// Usable reports whether this observation may price a request for the named deployment at
// the given instant, and why not when it may not.
//
// The three checks are in the order a reader would ask them: was anything observed, was it
// observed about THIS backend, and was it observed recently enough to still be about this
// request.
func (o Observation) Usable(deployment string, now time.Time, maxAge time.Duration) Refusal {
	if o.Refused != RefusalNone {
		return o.Refused
	}
	if o.At.IsZero() {
		return RefusalNoHeader
	}
	if o.Deployment != deployment {
		return RefusalWrongDeployment
	}
	if maxAge > 0 {
		// A negative age is a clock that stepped, not a fresh observation. It is
		// treated as staleness for the same reason every degenerate case in this
		// package resolves against applying the factor: an unknown must never be able
		// to raise a price.
		if age := now.Sub(o.At); age > maxAge || age < 0 {
			return RefusalStale
		}
	}
	return RefusalNone
}

// Refuse builds an observation that carries only a reason. It is what the caller records
// when there was never a response to read a header from — a streamed answer, most of all.
func Refuse(deployment string, at time.Time, r Refusal) Observation {
	return Observation{Deployment: deployment, At: at, Refused: r}
}

// orcaReport is the ORCA load report vLLM serialises after the format prefix.
type orcaReport struct {
	NamedMetrics map[string]float64 `json:"named_metrics"`
}

// Parse reads a vLLM `endpoint-load-metrics` header value.
//
// The value is NOT bare JSON. vLLM writes the format name, a space, and then the document:
//
//	endpoint-load-metrics: JSON {"named_metrics": {"kv_cache_utilization": 0.4}}
//
// Handing that to a JSON decoder fails, and failing is the good outcome — the bad one is a
// decoder lenient enough to skip the prefix and a reader who never learns the value has a
// shape. The prefix is consumed here, deliberately, in the one place that knows about it.
//
// Every rejection is a named [Refusal] and never a zero fraction, so a caller cannot
// accidentally treat a failure as an idle backend: the fraction returned alongside a
// refusal is always zero AND the refusal is always set.
func Parse(value, deployment string, at time.Time) Observation {
	refuse := func(r Refusal) Observation { return Refuse(deployment, at, r) }

	v := strings.TrimSpace(value)
	if v == "" {
		return refuse(RefusalNoHeader)
	}
	format, body, ok := strings.Cut(v, " ")
	if !ok {
		return refuse(RefusalMalformed)
	}
	switch strings.ToUpper(strings.TrimSpace(format)) {
	case RequestFormat:
	case "TEXT", "BIN":
		return refuse(RefusalWrongFormat)
	default:
		return refuse(RefusalMalformed)
	}

	var rep orcaReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &rep); err != nil {
		return refuse(RefusalMalformed)
	}
	// A parsed report with no KV metric is the header's version of "200 with no series".
	// It is a separate refusal from a malformed one because the fix is different: this
	// backend answered correctly and had nothing to say, which is an engine
	// configuration question, not a wire question.
	f, ok := rep.NamedMetrics[kvMetric]
	if !ok {
		return refuse(RefusalNoKVMetric)
	}
	// The order of these two matters. An out-of-range value is a SCALE error and has to
	// be reported as one even though it is also, technically, not a usable reading; a
	// caller seeing "not_a_fraction" in a ledger row goes and looks at their engine's
	// units, which is exactly the right thing to go and look at.
	if !(f >= 0) || f > 1 { // the !(>=0) form also catches NaN
		return refuse(RefusalNotAFraction)
	}
	if f == 0 {
		return refuse(RefusalZeroIsAmbiguous)
	}
	return Observation{Fraction: f, Deployment: deployment, At: at, Refused: RefusalNone}
}
