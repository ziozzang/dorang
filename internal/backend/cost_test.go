package backend

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// TestScaleDecimalIsExact pins the conversion of a vendor's cost figure to nano-USD.
// The figures are the ones the two vendors actually sent, in the spellings they used:
// both answer in exponent form, which is exactly where a float would lose the nano.
func TestScaleDecimalIsExact(t *testing.T) {
	for _, tc := range []struct {
		lex   string
		scale int
		want  int64
		ok    bool
	}{
		{"3.4e-07", 9, 340, true}, // DeepInfra chat, 14 tokens
		{"8.8e-7", 9, 880, true},  // OpenRouter, 35 tokens
		{"0.593249638", 9, 593249638, true},
		{"0", 9, 0, true},
		{"0.0", 9, 0, true},
		{"-0", 9, 0, true},
		{"1", 9, 1_000_000_000, true},
		{"1E+2", 9, 100_000_000_000, true},
		{"26", 7, 260_000_000, true}, // 26 cents
		{"0.000333", 7, 3330, true},  // a DeepInfra rate, in cents
		// Half to even on the first dropped digit.
		{"0.0000000005", 9, 0, true},
		{"0.0000000015", 9, 2, true},
		{"0.0000000025", 9, 2, true},
		{"0.00000000251", 9, 3, true},
		{"1e-20", 9, 0, true},
		// Refused: a credit, an overflow, and things that are not numbers.
		{"-0.01", 9, 0, false},
		{"1e300", 9, 0, false},
		{"99999999999", 9, 0, false},
		{"", 9, 0, false},
		{"1.", 9, 0, false},
		{".5", 9, 0, false},
		{"0x10", 9, 0, false},
		{"1e", 9, 0, false},
		{"NaN", 9, 0, false},
	} {
		got, ok := scaleDecimal([]byte(tc.lex), tc.scale)
		if ok != tc.ok || got != tc.want {
			t.Errorf("scaleDecimal(%q, %d) = %d, %v; want %d, %v", tc.lex, tc.scale, got, ok, tc.want, tc.ok)
		}
	}
}

// TestCostSpecTellsAbsentNullAndInvalidApart: none of the three is zero, and a
// reader that conflated them would record "free" for a vendor that said nothing.
func TestCostSpecTellsAbsentNullAndInvalidApart(t *testing.T) {
	s, err := newCostSpec("usage.cost", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		doc  string
		want costState
		nano int64
	}{
		{`{"usage":{"cost":8.8e-7}}`, costStated, 880},
		{`{"usage":{"cost":"0.0001"}}`, costStated, 100_000},
		{`{"usage":{"cost":0}}`, costStated, 0},
		{`{"usage":{"cost":null}}`, costNull, 0},
		{`{"usage":{"cost":-1}}`, costInvalid, 0},
		{`{"usage":{"cost":true}}`, costInvalid, 0},
		{`{"usage":{"prompt_tokens":3}}`, costAbsent, 0},
		{`{"cost":1}`, costAbsent, 0},
		{`{"usage":"cost"}`, costAbsent, 0},
		{`not json "cost"`, costAbsent, 0},
	} {
		n, st := s.read([]byte(tc.doc))
		if st != tc.want || n != tc.nano {
			t.Errorf("read(%s) = %d, %v; want %d, %v", tc.doc, n, st, tc.nano, tc.want)
		}
	}
	var none *costSpec
	if _, st := none.read([]byte(`{"usage":{"cost":1}}`)); st != costAbsent {
		t.Errorf("a host with no declaration read a cost")
	}
}

func TestNewCostSpecRefusesWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct{ path, unit string }{
		{"usage..cost", ""},
		{"usage.cost[0]", ""},
		{"usage.cost", "eur"},
	} {
		if _, err := newCostSpec(tc.path, tc.unit); err == nil {
			t.Errorf("newCostSpec(%q, %q) was accepted", tc.path, tc.unit)
		}
	}
	if s, err := newCostSpec("", ""); s != nil || err != nil {
		t.Errorf("an empty path is a host that states nothing, got %v, %v", s, err)
	}
}

// TestCostTapReadsWholeEvents feeds the tap a stream in awkward pieces: an event
// split across reads, a payload spread over two data lines, CRLF endings, the null
// DeepInfra sends before the figure, an invalid figure after it, and a stream whose
// last event has no closing blank line.
func TestCostTapReadsWholeEvents(t *testing.T) {
	spec, _ := newCostSpec("usage.estimated_cost", "")
	for _, tc := range []struct {
		name   string
		stream string
		want   int64
		found  bool
	}{
		{"DeepInfra: null first, then the figure",
			"data: {\"usage\":{\"estimated_cost\":null}}\n\n" +
				"data: {\"usage\":{\"estimated_cost\":3.4e-7}}\n\ndata: [DONE]\n\n", 340, true},
		{"an invalid figure later erases nothing",
			"data: {\"usage\":{\"estimated_cost\":3.4e-7}}\n\n" +
				"data: {\"usage\":{\"estimated_cost\":\"x\"}}\n\n", 340, true},
		{"a payload over two data lines",
			"data: {\"usage\":\ndata: {\"estimated_cost\":1e-6}}\n\n", 1000, true},
		{"CRLF line endings",
			"data: {\"usage\":{\"estimated_cost\":1e-6}}\r\n\r\n", 1000, true},
		{"no closing blank line",
			"data: {\"usage\":{\"estimated_cost\":1e-6}}", 1000, true},
		{"nothing stated", "data: {\"choices\":[]}\n\ndata: [DONE]\n\n", 0, false},
		{"other SSE fields are ignored",
			"event: x\nid: 1\ndata: {\"usage\":{\"estimated_cost\":2e-9}}\n\n", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tap := newCostTap(io.NopCloser(strings.NewReader(tc.stream)), spec)
			got, err := io.ReadAll(&chunkedReader{r: tap, n: 7})
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.stream {
				t.Fatalf("the tap changed the stream")
			}
			n, found := tap.reported()
			if n != tc.want || found != tc.found {
				t.Errorf("reported = %d, %v; want %d, %v", n, found, tc.want, tc.found)
			}
		})
	}
}

// TestCostTapSkipsAnOversizedEvent: the bound is on what the tap watches, never on
// the stream, and the event after the oversized one is still read.
func TestCostTapSkipsAnOversizedEvent(t *testing.T) {
	spec, _ := newCostSpec("usage.cost", "")
	big := "data: {\"usage\":{\"cost\":5},\"pad\":\"" + strings.Repeat("a", costTapMaxEvent) + "\"}\n\n"
	stream := big + "data: {\"usage\":{\"cost\":1e-9}}\n\n"
	tap := newCostTap(io.NopCloser(strings.NewReader(stream)), spec)
	got, _ := io.ReadAll(tap)
	if len(got) != len(stream) {
		t.Fatalf("the stream was cut: %d of %d bytes", len(got), len(stream))
	}
	if n, found := tap.reported(); !found || n != 1 {
		t.Errorf("reported = %d, %v; want the figure from the event after the oversized one", n, found)
	}
}

type chunkedReader struct {
	r io.Reader
	n int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}

func costProvider(t *testing.T, f *fakeUpstream, path string) *Provider {
	t.Helper()
	p, err := NewProvider(Spec{Name: "p1", Kind: "deepinfra", API: catalog.APIOpenAIChat,
		BaseURL: f.srv.URL, ReportedCostPath: path,
		TranscriptionDuration: catalog.TranscriptionDurationVerboseJSON})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestABufferedAnswerCarriesTheReportedCost(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"upstream-model",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14,"estimated_cost":3.4e-07}}`)
	})
	res := testBackend("k").Do(context.Background(), target(costProvider(t, f, "usage.estimated_cost")),
		chatCall(catalog.APIOpenAIChat), nil)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err.Message)
	}
	if !res.ReportedCost || res.ReportedCostNano != 340 {
		t.Errorf("reported = %d, %v; want 340 nano-USD", res.ReportedCostNano, res.ReportedCost)
	}
}

// TestAVendorErrorWearingA200StatesNoCost: the figure is read only after the answer
// converted, so an envelope that is not an answer is never recorded as a charge.
func TestAVendorErrorWearingA200StatesNoCost(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":500,"msg":"404 NOT_FOUND","success":false,"billing":{"cost":1}}`)
	})
	res := testBackend("k").Do(context.Background(), target(costProvider(t, f, "billing.cost")),
		chatCall(catalog.APIOpenAIChat), nil)
	if res.Err == nil {
		t.Fatal("an error envelope was served as an answer")
	}
	if res.ReportedCost {
		t.Errorf("a refused answer recorded a reported cost of %d", res.ReportedCostNano)
	}
}

func TestAStreamCarriesTheReportedCost(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			`data: {"id":"x","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14,"estimated_cost":null}}`+"\n\n"+
				`data: {"id":"x","object":"chat.completion.chunk","model":"upstream-model","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14,"estimated_cost":3.4e-7}}`+"\n\n"+
				"data: [DONE]\n\n")
	})
	// Both relay paths: the byte relay a same-family caller takes, and the
	// decoded path a caller of another family takes.
	for _, api := range []catalog.API{catalog.APIOpenAIChat, catalog.APIAnthropicMessages} {
		t.Run(string(api), func(t *testing.T) {
			call := chatCall(api)
			call.Stream = true
			rec := httptest.NewRecorder()
			res := testBackend("k").Do(context.Background(), target(costProvider(t, f, "usage.estimated_cost")), call, rec)
			if res.Err != nil {
				t.Fatalf("Do: %v", res.Err.Message)
			}
			if !res.ReportedCost || res.ReportedCostNano != 340 {
				t.Errorf("reported = %d, %v; want 340 nano-USD", res.ReportedCostNano, res.ReportedCost)
			}
			if api == catalog.APIOpenAIChat && !strings.Contains(rec.Body.String(), `"estimated_cost":3.4e-7`) {
				t.Errorf("the client's stream lost the vendor's usage chunk:\n%s", rec.Body.String())
			}
		})
	}
}

// TestAJSONTranscriptIsAskedForVerboseAndMeteredByDuration is phase one end to end:
// the caller asks for `json`, the host is sent verbose_json, the recording's length
// reaches the usage pricing reads, and the caller gets the `json` shape back.
func TestAJSONTranscriptIsAskedForVerboseAndMeteredByDuration(t *testing.T) {
	for _, format := range []string{"", "json"} {
		t.Run("format="+format, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"text":" hello","task":"transcribe","language":"en",`+
					`"duration":4.008,"segments":[{"id":0,"start":0,"end":4,"text":" hello"}]}`)
			})
			call := transcriptionCall()
			call.Transcription.Format = format
			res := testBackend("k").Do(context.Background(), target(costProvider(t, f, "")), call, nil)
			if res.Err != nil {
				t.Fatalf("Do: %v", res.Err.Message)
			}
			if sent := formValue(t, f.last(), "response_format"); sent != "verbose_json" {
				t.Errorf("upstream was asked for %q, want verbose_json", sent)
			}
			if res.Usage.AudioSeconds != 4.008 {
				t.Errorf("AudioSeconds = %v, want 4.008", res.Usage.AudioSeconds)
			}
			if string(res.Body) != `{"text":" hello"}` {
				t.Errorf("the caller asked for json and got %s", res.Body)
			}
		})
	}
}

// TestOtherTranscriptFormatsAreSentAsAsked: a subtitle file is never re-rendered
// from parsed fields, so those formats are not upgraded, and a host without the
// declaration is never upgraded either.
func TestOtherTranscriptFormatsAreSentAsAsked(t *testing.T) {
	for _, tc := range []struct {
		name, format string
		declared     bool
	}{
		{"srt", "srt", true},
		{"verbose_json asked for", "verbose_json", true},
		{"json on a host that does not declare it", "json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"text":"hi","duration":2}`)
			})
			p := costProvider(t, f, "")
			if !tc.declared {
				p = testProvider(t, f, "openai", catalog.APIOpenAIChat)
			}
			call := transcriptionCall()
			call.Transcription.Format = tc.format
			res := testBackend("k").Do(context.Background(), target(p), call, nil)
			if res.Err != nil {
				t.Fatalf("Do: %v", res.Err.Message)
			}
			if sent := formValue(t, f.last(), "response_format"); sent != tc.format {
				t.Errorf("upstream was asked for %q, want %q as sent", sent, tc.format)
			}
			// A verbose answer states the duration whoever asked for it.
			if tc.format == "verbose_json" && res.Usage.AudioSeconds != 2 {
				t.Errorf("AudioSeconds = %v, want the verbose answer's duration", res.Usage.AudioSeconds)
			}
		})
	}
}

// TestAVendorUsageBlockWinsOverTheDuration: a usage block is the invoice, and a
// rounded billing minute must not be replaced by the length of the file.
func TestAVendorUsageBlockWinsOverTheDuration(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"hi","duration":12.5,"usage":{"type":"duration","seconds":60}}`)
	})
	res := testBackend("k").Do(context.Background(), target(costProvider(t, f, "")), transcriptionCall(), nil)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err.Message)
	}
	if res.Usage.AudioSeconds != 60 {
		t.Errorf("AudioSeconds = %v, want the billed 60", res.Usage.AudioSeconds)
	}
	if !strings.Contains(string(res.Body), `"usage"`) {
		t.Errorf("the vendor's usage block was cut from the json answer: %s", res.Body)
	}
}

func formValue(t *testing.T, r recorded, name string) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(r.header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("content type: %v", err)
	}
	mr := multipart.NewReader(bytes.NewReader(r.body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if p.FormName() == name {
			b, _ := io.ReadAll(p)
			return string(b)
		}
	}
}
