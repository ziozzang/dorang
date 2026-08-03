package loadsignal

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

// TestParseReadsARealVLLMReport. The value carries a FORMAT PREFIX before the document,
// which is the shape a JSON decoder rejects and a careless reader strips with a regex.
func TestParseReadsARealVLLMReport(t *testing.T) {
	o := Parse(`JSON {"named_metrics": {"kv_cache_utilization": 0.4}}`, "d1", now)
	if o.Refused != RefusalNone {
		t.Fatalf("refused a well-formed report: %v (%s)", o.Refused, o.Refused.Why())
	}
	if o.Fraction != 0.4 {
		t.Fatalf("fraction = %v, want 0.4", o.Fraction)
	}
	if o.Deployment != "d1" || !o.At.Equal(now) {
		t.Fatalf("observation is not tied to its deployment and instant: %+v", o)
	}
	if r := o.Usable("d1", now, time.Second); r != RefusalNone {
		t.Fatalf("a fresh observation of the right deployment was refused: %v", r)
	}
}

// TestAnEmptyOrAbsentReportIsNotAnIdleBackend is the silent-zero failure in the form this
// path can take it, and it has four forms because a 200 with no signal has four spellings.
//
// Every one of them must land on a NAMED refusal and never on a fraction of zero. The
// difference matters on the invoice: a fraction of zero says "this request ran on an empty
// machine", which is a measurement nobody made.
func TestAnEmptyOrAbsentReportIsNotAnIdleBackend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  Refusal
	}{
		// The header never arrived: an older vLLM, a non-vLLM backend, a proxy that
		// stripped an unknown header.
		{"absent", "", RefusalNoHeader},
		// The report arrived, parsed, and says nothing. This is
		// `--disable-log-stats` in its header form: 200, well-formed, no series.
		{"no named_metrics", `JSON {}`, RefusalNoKVMetric},
		{"empty named_metrics", `JSON {"named_metrics": {}}`, RefusalNoKVMetric},
		{"other metrics only", `JSON {"named_metrics": {"waiting_queue_size": 3}}`, RefusalNoKVMetric},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := Parse(tc.value, "d1", now)
			if o.Refused != tc.want {
				t.Fatalf("refusal = %v (%s), want %v", o.Refused, o.Refused, tc.want)
			}
			if o.Fraction != 0 {
				t.Fatalf("a refusal carried a fraction of %v", o.Fraction)
			}
			if r := o.Usable("d1", now, time.Second); r != tc.want {
				t.Fatalf("Usable = %v, want the parse refusal %v", r, tc.want)
			}
			if tc.want.Why() == "" {
				t.Fatalf("refusal %v has no explanation, so the ledger row cannot say "+
					"which fallback fired", tc.want)
			}
		})
	}
}

// TestAnExactZeroIsRefusedBecauseVLLMEmitsItForBothMeanings is the finding that decided
// this design, and it is not a hypothetical: it is vLLM's own construction.
//
//	kv_cache_utilization=(last_req_metrics.gpu_kv_cache_utilisation
//	                      if last_req_metrics is not None else 0.0)
//
// An engine with no metrics for the request emits a well-formed header claiming an empty
// machine. Unlike the `/metrics` version of the same trap — where the scrape at least
// fails visibly with zero series — this one SUCCEEDS and is wrong, so a reader that trusts
// the header is worse off than one that scrapes.
//
// The refusal is free, and that is what makes it the right call rather than a cautious
// one: at zero occupancy the price factor is 1 + slope x 0 = 1.0, which is exactly what
// the fallback charges. Nobody pays a different amount. What changes is that the row says
// "we do not know" instead of "the machine was empty".
func TestAnExactZeroIsRefusedBecauseVLLMEmitsItForBothMeanings(t *testing.T) {
	o := Parse(`JSON {"named_metrics": {"kv_cache_utilization": 0.0}}`, "d1", now)
	if o.Refused != RefusalZeroIsAmbiguous {
		t.Fatalf("an exact zero was accepted as an observation (refusal %v). vLLM emits "+
			"0.0 both for an idle engine and for an engine that reported nothing, so "+
			"accepting it files an unmeasured request as a measured one", o.Refused)
	}
	if !strings.Contains(o.Refused.Why(), "1.0 either way") {
		t.Errorf("the refusal does not say that it costs nothing: %s", o.Refused.Why())
	}
	// The smallest reading above zero is a real measurement and is kept. The line
	// between them is exactly where vLLM's default sits and nowhere else, so the rule
	// discards one value rather than a range.
	tiny := Parse(`JSON {"named_metrics": {"kv_cache_utilization": 0.000001}}`, "d1", now)
	if tiny.Refused != RefusalNone {
		t.Fatalf("a reading just above zero was refused as %v; the rule must discard the "+
			"one ambiguous value, not a neighbourhood of it", tiny.Refused)
	}
}

// TestAPercentageScaledReadingIsRefusedAsAScaleError. `kv_cache_usage_perc` is a fraction
// (VLLM.md §3.1: "1 means 100 percent usage"). A source that answers in percent is 100x
// out and every downstream step succeeds.
//
// It gets its own refusal rather than folding into "not usable" because the two send an
// operator to different places: `no_kv_metric` is an engine that is not reporting,
// `not_a_fraction` is an engine reporting in the wrong units.
func TestAPercentageScaledReadingIsRefusedAsAScaleError(t *testing.T) {
	for _, v := range []string{"45", "45.0", "100", "1.0001"} {
		o := Parse(`JSON {"named_metrics": {"kv_cache_utilization": `+v+`}}`, "d1", now)
		if o.Refused != RefusalNotAFraction {
			t.Fatalf("%s was accepted as a fraction (refusal %v); read as one it is 100x "+
				"the real occupancy and the price follows it", v, o.Refused)
		}
	}
	// The boundary is inclusive: a saturated cache reads exactly 1.0 and is a real
	// measurement, not an error.
	if o := Parse(`JSON {"named_metrics": {"kv_cache_utilization": 1.0}}`, "d1", now); o.Refused != RefusalNone {
		t.Fatalf("a saturated cache (1.0) was refused as %v", o.Refused)
	}
	// Negative and NaN resolve the same way, against applying the factor.
	for _, v := range []string{"-0.5", "-1"} {
		if o := Parse(`JSON {"named_metrics": {"kv_cache_utilization": `+v+`}}`, "d1", now); o.Refused != RefusalNotAFraction {
			t.Fatalf("%s was accepted (refusal %v)", v, o.Refused)
		}
	}
	if !strings.Contains(RefusalNotAFraction.Why(), "100x") {
		t.Error("the refusal does not name the 100x")
	}
}

// TestTheFormatPrefixIsPartOfTheValue. vLLM writes `JSON ` before the document, so the
// header value is not a JSON document and a decoder handed it whole fails.
func TestTheFormatPrefixIsPartOfTheValue(t *testing.T) {
	// Bare JSON with no prefix is not what vLLM sends and is not accepted: silently
	// tolerating it would hide the day the prefix changes.
	if o := Parse(`{"named_metrics": {"kv_cache_utilization": 0.4}}`, "d1", now); o.Refused != RefusalMalformed {
		t.Errorf("bare JSON with no format prefix was accepted as %v", o.Refused)
	}
	// TEXT and BIN are refused rather than parsed. dorang asked for JSON per request; a
	// server that answers in another format is not honouring the request header, and a
	// second parser would consume the evidence of that.
	for _, v := range []string{
		"TEXT named_metrics.kv_cache_utilization=0.4",
		"BIN CZqZmZmZmbk/MQAAAAAAAABA",
	} {
		if o := Parse(v, "d1", now); o.Refused != RefusalWrongFormat {
			t.Errorf("%q gave refusal %v, want wrong_format", v, o.Refused)
		}
	}
	for _, v := range []string{"JSON not-json", "JSON", "JSON []", "garbage value"} {
		o := Parse(v, "d1", now)
		if o.Refused == RefusalNone {
			t.Errorf("%q was accepted", v)
		}
		if o.Fraction != 0 {
			t.Errorf("%q produced a fraction", v)
		}
	}
}

// TestAStaleOrMisattributedObservationFallsBack.
//
// Neither guard describes a poll interval — nothing here is polled, and an observation
// normally travels microseconds from the response that produced it to the settlement of
// that same request. They are guards on the WIRING, and they are the reason a future
// refactor cannot start pricing one request on another request's contention without the
// row saying so.
func TestAStaleOrMisattributedObservationFallsBack(t *testing.T) {
	o := Parse(`JSON {"named_metrics": {"kv_cache_utilization": 0.4}}`, "d1", now)

	if r := o.Usable("d1", now.Add(2*time.Second), time.Second); r != RefusalStale {
		t.Fatalf("a two-second-old observation under a one-second bound gave %v, want stale", r)
	}
	if r := o.Usable("d1", now.Add(900*time.Millisecond), time.Second); r != RefusalNone {
		t.Fatalf("an observation inside the bound was refused: %v", r)
	}
	// A clock that stepped backwards makes the observation appear to come from the
	// future. Every degenerate case in this package resolves against applying the
	// factor, because an unknown must never raise a price.
	if r := o.Usable("d1", now.Add(-time.Second), time.Second); r != RefusalStale {
		t.Fatalf("an observation from the future gave %v, want stale", r)
	}
	// The spatial version of the same rule, and the one a fallback chain hits: attempt
	// one observed backend A and failed, attempt two ran on backend B. Pricing B's
	// request with A's contention charges one machine's load against another's rate.
	if r := o.Usable("d2", now, time.Second); r != RefusalWrongDeployment {
		t.Fatalf("an observation of d1 was accepted for d2: %v", r)
	}
	// A refusal manufactured by the caller — a streamed answer is the common one —
	// survives Usable unchanged, so the specific reason reaches the row.
	streamed := Refuse("d1", now, RefusalStreamed)
	if r := streamed.Usable("d1", now, time.Second); r != RefusalStreamed {
		t.Fatalf("a streamed refusal became %v on the way to the ledger", r)
	}
	// And the zero value is never usable, which is what makes it safe to pass around.
	if r := (Observation{}).Usable("d1", now, time.Second); r == RefusalNone {
		t.Fatal("the zero Observation is usable, so a caller that forgot to set one " +
			"would price at an occupancy of zero")
	}
}

// TestEveryRefusalHasAStableTokenAndAnExplanation. The token reaches a response header and
// a ledger column; the explanation reaches a log line and the price preview. A refusal
// with neither is a fallback nobody can diagnose.
func TestEveryRefusalHasAStableTokenAndAnExplanation(t *testing.T) {
	seen := map[string]Refusal{}
	for r := RefusalNone; r <= RefusalStale; r++ {
		tok := r.String()
		if tok == "unknown" {
			t.Fatalf("refusal %d has no token", r)
		}
		if prev, dup := seen[tok]; dup {
			t.Fatalf("refusals %v and %v share the token %q", prev, r, tok)
		}
		seen[tok] = r
		if r.Why() == "" {
			t.Errorf("refusal %q has no explanation", tok)
		}
		if strings.ContainsAny(tok, " ,;\"") {
			t.Errorf("token %q is not safe in an HTTP header value", tok)
		}
	}
	if len(seen) < 10 {
		t.Fatalf("only %d refusals enumerated; the test is not reading the type", len(seen))
	}
}
