package app

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/server"
)

// transcriptionUpstream answers one transcription with a stated billing unit and a
// stated number of billed seconds, after holding the connection for hold.
//
// The hold is what makes the assertion decisive rather than merely correct: it puts a
// measurable, non-zero WALL TIME on the request, so the figure a wall-time
// implementation would charge is a real number that the failure message can print
// beside the invoice.
func transcriptionUpstream(t *testing.T, unit string, seconds float64, hold time.Duration) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(hold)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"text":"a ten minute recording",`+
			`"duration":%f,`+
			`"usage":{"type":%q,"seconds":%f,"input_tokens":0,"output_tokens":0,"total_tokens":0}}`,
			seconds, unit, seconds)
	}))
	t.Cleanup(s.Close)
	return s
}

func transcriptionPost(t *testing.T, a *App, secret, model string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", model); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", "meeting.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "RIFF....WAVEfmt "); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(buf.Bytes()))
	r.Header.Set("Authorization", "Bearer "+secret)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	a.Server.ServeHTTP(w, r)
	return w
}

func audioYAML(upstreamURL, rates string) string {
	return fmt.Sprintf(`
version: 1
observability: {always_full_headers: true}
providers:
  - {name: p1, kind: openai, base_url: %q}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: transcribe
    deployments:
      - {provider: p1, upstream_model: transcribe-1, credentials: [c1]}
pricing:
  rules:
    - id: vendor-transcription
      match: {provider: p1, model: transcribe-1}
      rates: {%s}
`, upstreamURL, rates)
}

// TestATranscriptIsChargedForTheRecordingThroughTheWholeStack is the assembled-stack
// half of the same defect internal/pricing pins against the vendor's rate card. The
// quantity was decoded correctly, landed on canonical.TranscriptionResponse, and reached
// nothing: internal/app filled pricing.Request.Seconds from the request's ELAPSED TIME
// and there was no second axis for the recording to arrive on.
//
// The two durations are deliberately four orders of magnitude apart — a ten-MINUTE
// recording answered in tens of MILLISECONDS — so an implementation that still prices
// wall time cannot pass by rounding.
//
// The assertion is X-Dorang-Cost-Usd, because that is the charged amount as a client and
// an operator see it: the same number the ledger row, the budget hold and the quota
// counter all take.
func TestATranscriptIsChargedForTheRecordingThroughTheWholeStack(t *testing.T) {
	const hold = 40 * time.Millisecond
	up := transcriptionUpstream(t, "duration", 600, hold)

	// $0.006 per minute of audio, which is what a hosted transcription service
	// charges, expressed per second.
	a := newWiringApp(t, audioYAML(up.URL, `audio_seconds: "0.0001"`), nil,
		func(o *Options) { o.Upstream = up.Client() })
	secret := issueKey(t, a, nil)

	w := transcriptionPost(t, a, secret, "transcribe")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	// The invoice, by hand: 600 s of audio x $0.0001/s = $0.06. The header renders
	// nano-USD with trailing zeroes trimmed, so the exact figure is "0.06".
	const wantCost = "0.06"
	got := w.Header().Get(server.HeaderCostUSD)
	if got == "" {
		t.Fatalf("no %s on the response: the request was recorded UNPRICED, so the "+
			"recording's length never reached the price engine", server.HeaderCostUSD)
	}
	if got != wantCost {
		// What a wall-time implementation charges, for the message: the same rate
		// against ~40 ms instead of 600 s, which is four ten-thousandths of a cent.
		t.Fatalf("charged %s USD, the vendor's invoice is %s USD. The rate was applied "+
			"to something other than the recording's 600 s — %s against the request's "+
			"own %s of wall time is what the substitution this test exists for produces",
			got, wantCost, server.HeaderCostUSD, hold)
	}
}

// TestAWallTimeRateStillChargesWallTimeThroughTheWholeStack is the other axis, joined.
// A GPU-second rate is a real thing and the request's own duration is its correct input,
// so the two rules must not be two spellings of one behaviour once they are assembled.
//
// The upstream states NO billing unit here, and that is the whole of the case a
// per_compute_second rate is for: a self-hosted deployment billed by occupancy reports
// how long the work took and does not claim to have metered anything else. When the
// vendor DOES state an axis, the pairing is refused —
// [TestAWallTimeRateAgainstAVendorBilledRecordingRecordsUnpriced] is that half, and this
// test used to be it, asserting the defect.
//
// It asserts a BAND rather than a figure: the quantity is a real elapsed time and the
// only thing that can be pinned about it is that it is the request's and not the
// recording's. The band is wide enough for a loaded machine and nowhere near 600 s.
func TestAWallTimeRateStillChargesWallTimeThroughTheWholeStack(t *testing.T) {
	up := transcriptionUpstream(t, "", 600, 40*time.Millisecond)
	a := newWiringApp(t, audioYAML(up.URL, `compute_seconds: "0.0001"`), nil,
		func(o *Options) { o.Upstream = up.Client() })
	secret := issueKey(t, a, nil)

	w := transcriptionPost(t, a, secret, "transcribe")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	got := w.Header().Get(server.HeaderCostUSD)
	if got == "" {
		t.Fatalf("no %s: a per_compute_second rate has wall time to price and did not "+
			"price it", server.HeaderCostUSD)
	}
	usd, err := strconv.ParseFloat(got, 64)
	if err != nil {
		t.Fatalf("%s = %q, which is not a number", server.HeaderCostUSD, got)
	}
	// 600 s of audio at this rate would be $0.06; a request that took tens of
	// milliseconds is three orders of magnitude below it. The band is wide enough
	// for a loaded machine and nowhere near the recording.
	if usd <= 0 {
		t.Fatalf("charged %s USD: the request's own wall time did not reach the rate", got)
	}
	if usd >= 0.001 {
		t.Fatalf("charged %s USD for a request that took tens of milliseconds at "+
			"$0.0001/s: a per_compute_second rate reached for the recording's length", got)
	}
}

// TestAWallTimeRateAgainstAVendorBilledRecordingRecordsUnpriced is the direction the
// two-axis split left open, joined.
//
// It is the headline defect of ef94f58 — a ten-minute recording billed as eight seconds
// — reached through the other axis. Splitting `seconds` into `compute_seconds` and
// `audio_seconds` stopped an audio rate from reaching for wall time; it did not stop a
// COMPUTE rate from being the rule that matched a request the vendor billed by the
// recording. The unit guard was written as a list of the token components, and
// compute_seconds is not one, so neither half of the check refused it: the rate applied,
// the arithmetic succeeded, and 40 ms of dorang's own latency was charged against a
// 600-second invoice with NoPrice reporting nothing wrong.
//
// The two durations are four orders of magnitude apart on purpose, exactly as in
// [TestATranscriptIsChargedForTheRecordingThroughTheWholeStack]: what a wrong axis
// produces here is a plausible small number, not an error.
func TestAWallTimeRateAgainstAVendorBilledRecordingRecordsUnpriced(t *testing.T) {
	// The vendor billed the RECORDING and said so. The catalog prices dorang's
	// own wall clock.
	up := transcriptionUpstream(t, "duration", 600, 40*time.Millisecond)
	a := newWiringApp(t, audioYAML(up.URL, `compute_seconds: "0.0001"`), nil,
		func(o *Options) { o.Upstream = up.Client() })
	secret := issueKey(t, a, nil)

	w := transcriptionPost(t, a, secret, "transcribe")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(server.HeaderCostUSD); got != "" {
		t.Fatalf("charged %s USD. The vendor billed 600 s of recording; this rate is "+
			"quoted per second of COMPUTE and was applied to the tens of milliseconds "+
			"the request took. The invoice at $0.0001/s is $0.06 — the charge is three "+
			"orders of magnitude under it, and it is not an error, it is a plausible "+
			"number. A rate on an axis the vendor did not bill must record the request "+
			"unpriced", got)
	}
}

// TestARuleThatCannotPriceTheVendorsUnitRecordsUnpriced is §10.7 one level up, joined.
// The transcript states its billing unit on the wire; a catalog that prices the other
// axis is applying a rate to a quantity nobody was invoiced for, and the arithmetic
// would have succeeded.
//
// The observable is the ABSENCE of the cost header, which is exactly how an unpriced
// request already reports itself (§8.3): a zero there would be indistinguishable from a
// free request, and free is the answer nobody disputes.
func TestARuleThatCannotPriceTheVendorsUnitRecordsUnpriced(t *testing.T) {
	// The backend billed in TOKENS and still reported a duration. Both numbers are
	// real; only one of them is on the invoice.
	up := transcriptionUpstream(t, "tokens", 600, 0)
	a := newWiringApp(t, audioYAML(up.URL, `audio_seconds: "0.0001"`), nil,
		func(o *Options) { o.Upstream = up.Client() })
	secret := issueKey(t, a, nil)

	w := transcriptionPost(t, a, secret, "transcribe")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(server.HeaderCostUSD); got != "" {
		t.Fatalf("charged %s USD against a rate quoted per second of audio for a request "+
			"the vendor billed in tokens; it must be recorded unpriced", got)
	}
}

// TestTheTranscriptStillStatesTheUnitItWasBilledIn guards the half that faces the
// client. The billing unit and the billed seconds now ALSO ride on canonical.Usage so
// that pricing can read them, and a second carrier is a second thing to drift: the
// response the caller gets must still echo the vendor's own `usage.type` and `seconds`.
func TestTheTranscriptStillStatesTheUnitItWasBilledIn(t *testing.T) {
	up := transcriptionUpstream(t, "duration", 600, 0)
	a := newWiringApp(t, audioYAML(up.URL, `audio_seconds: "0.0001"`), nil,
		func(o *Options) { o.Upstream = up.Client() })
	secret := issueKey(t, a, nil)

	w := transcriptionPost(t, a, secret, "transcribe")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"type":"duration"`, `"seconds":600`} {
		if !strings.Contains(body, want) {
			t.Errorf("the transcript does not carry %s: %s", want, body)
		}
	}
}

// TestANonTokenRateIsExpressibleInline is the rates.images defect at its full width.
//
// The main file's `pricing.rules[]` has no `unit:` key and wrote none into the
// assembled catalog, which defaults an absent unit to per_1m_tokens — so EVERY
// non-token component the schema advertised (`request`, `characters`, and both
// second axes) passed `dorangctl config lint` and then refused to assemble with
// "rate does not belong to unit per_1m_tokens". The unit is derived from the
// components a rule prices, which is possible because a component belongs to exactly
// one unit, and a rule mixing two is refused at lint rather than at start-up.
func TestANonTokenRateIsExpressibleInline(t *testing.T) {
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	for _, rates := range []string{
		`request: "0.002"`,
		`characters: "0.030"`,
		`compute_seconds: "0.0001"`,
		`audio_seconds: "0.0001"`,
	} {
		cfg, err := config.LoadBytes([]byte(audioYAML("https://example.invalid", rates)))
		if err != nil {
			t.Fatalf("rates {%s}: %v", rates, err)
		}
		if _, err := buildPricing(cfg); err != nil {
			t.Errorf("rates {%s} loads and does not assemble: %v — a load error one "+
				"layer too late is lint passing and the server refusing to start", rates, err)
		}
	}

	// And the one a derived unit cannot express: two units in one rule.
	_, err := config.LoadBytes([]byte(audioYAML("https://example.invalid",
		`input: "1.00", audio_seconds: "0.0001"`)))
	if err == nil {
		t.Fatal("a rule pricing tokens and audio seconds together loaded; a rule carries " +
			"one unit, so one of the two rates could never be applied")
	}
	if !strings.Contains(err.Error(), "different units") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}
