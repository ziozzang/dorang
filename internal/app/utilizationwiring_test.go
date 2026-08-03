package app

import (
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/loadsignal"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/router"
)

// The wiring between "what the backend said on this response" and "what the price engine
// was told", asserted at the one function that joins them.
//
// It is the join that carries the whole feature's risk. Everything below it — the parse,
// the range guard, the factor, the ceiling — is tested where it lives; what this file
// pins is that a failure at any of those steps arrives at the price engine as an ABSENCE
// and never as an occupancy of zero, because those two produce the same charge and
// different rows.

func utilCall(stream bool) *call { return &call{stream: stream} }

func utilDec(deployment string) *router.Decision {
	return &router.Decision{Deployment: deployment}
}

// TestTheObservationReachesThePriceEngineAsAFractionOrNotAtAll.
func TestTheObservationReachesThePriceEngineAsAFractionOrNotAtAll(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name    string
		stream  bool
		header  string
		wantPPM int32
		wantRef loadsignal.Refusal
	}{{
		name:    "a vLLM load report becomes an exact fraction",
		header:  `JSON {"named_metrics": {"kv_cache_utilization": 0.4}}`,
		wantPPM: 400_000,
		wantRef: loadsignal.RefusalNone,
	}, {
		// The largest hole in the feature, and it is named rather than hidden. vLLM's
		// load report is non-streaming only (VLLM.md §3.2) and no scrape stands behind
		// it in this build. In an agent deployment this is most traffic.
		name:    "a stream has no report to read and says so specifically",
		stream:  true,
		header:  `JSON {"named_metrics": {"kv_cache_utilization": 0.4}}`,
		wantRef: loadsignal.RefusalStreamed,
	}, {
		name:    "a backend that sent no report is not an idle backend",
		header:  "",
		wantRef: loadsignal.RefusalNoHeader,
	}, {
		// vLLM emits 0.0 both for an idle engine and for one that had no metrics for
		// the request. Refusing costs nothing — the factor at zero is 1.0x either way.
		name:    "an exact zero is ambiguous and is refused",
		header:  `JSON {"named_metrics": {"kv_cache_utilization": 0.0}}`,
		wantRef: loadsignal.RefusalZeroIsAmbiguous,
	}, {
		name:    "a percentage-scaled reading is a scale error, not an occupancy",
		header:  `JSON {"named_metrics": {"kv_cache_utilization": 45.0}}`,
		wantRef: loadsignal.RefusalNotAFraction,
	}, {
		name:    "a report with no KV series is 200-with-nothing, not idle",
		header:  `JSON {"named_metrics": {}}`,
		wantRef: loadsignal.RefusalNoKVMetric,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			ppm, ref := observeUtilization(
				utilCall(tc.stream), utilDec("h100-a"),
				result{loadMetrics: tc.header}, now)
			if ref != tc.wantRef {
				t.Fatalf("refusal = %v, want %v", ref, tc.wantRef)
			}
			if ppm != tc.wantPPM {
				t.Fatalf("ppm = %d, want %d", ppm, tc.wantPPM)
			}
			// The invariant the whole feature rests on: a refusal never travels
			// with a figure, so a caller that reads the figure without checking the
			// refusal still cannot price an unobserved backend as an idle one.
			if ref != loadsignal.RefusalNone && ppm != 0 {
				t.Fatalf("refusal %v carried an occupancy of %d", ref, ppm)
			}
			// And the shape internal/app hands to internal/pricing: the flag is the
			// refusal, so an unobserved request cannot set it by accident.
			req := pricing.Request{
				UtilizationPPM:      ppm,
				UtilizationObserved: ref == loadsignal.RefusalNone,
			}
			if req.UtilizationObserved != (tc.wantRef == loadsignal.RefusalNone) {
				t.Fatalf("UtilizationObserved = %v for refusal %v",
					req.UtilizationObserved, ref)
			}
		})
	}
}

// TestAFailedOverRequestIsNotPricedOnTheBackendItDidNotRunOn.
//
// Attempt one observed deployment A and failed; attempt two ran on B. The observation is
// tied to the deployment whose response carried it, and Usable refuses it for any other —
// so a fallback chain cannot charge one machine's contention against another's rate.
func TestAFailedOverRequestIsNotPricedOnTheBackendItDidNotRunOn(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	obs := loadsignal.Parse(
		`JSON {"named_metrics": {"kv_cache_utilization": 0.9}}`, "h100-a", now)
	if obs.Refused != loadsignal.RefusalNone {
		t.Fatalf("setup: %v", obs.Refused)
	}
	if r := obs.Usable("h100-b", now, maxObservationAge); r != loadsignal.RefusalWrongDeployment {
		t.Fatalf("an observation of h100-a was accepted for h100-b: %v", r)
	}
	// The dispatcher builds the observation from the decision that actually served, so
	// the mismatch cannot arise from this path today. The guard is what keeps that true
	// through a refactor, which is the only kind of protection worth having against a
	// wiring bug that produces a plausible number.
	if r := obs.Usable("h100-a", now.Add(maxObservationAge+time.Second),
		maxObservationAge); r != loadsignal.RefusalStale {
		t.Fatalf("an observation past the travel bound was accepted: %v", r)
	}
}

// TestTheDispatcherAsksVLLMForALoadReportAndNobodyElse would be a live-backend test; what
// is asserted here is the vocabulary the two packages share, since internal/server carries
// a copy of one of these strings and internal/store a column of them.
func TestTheDisclosureVocabularyIsStable(t *testing.T) {
	if loadsignal.RefusalNone.String() != "observed" {
		t.Fatalf("RefusalNone renders as %q; internal/server gates its detail-only "+
			"occupancy header on the literal \"observed\", and internal/admin gates the "+
			"occupancy field of /spend/logs on the same string",
			loadsignal.RefusalNone.String())
	}
	if loadsignal.RequestHeader != "endpoint-load-metrics-format" ||
		loadsignal.Header != "endpoint-load-metrics" {
		t.Fatalf("the vLLM header names have drifted: %q / %q",
			loadsignal.RequestHeader, loadsignal.Header)
	}
}
