package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// DESIGN §10.1, §10.2 — the RESPONSE direction of the loss report.
//
// The request direction has been threaded and asserted since the report existed.
// The response direction was open in a way that is easy to miss and easy to
// state: the encoders recorded every crossing's cost into `ResponseOptions.Loss`
// and `encodeClient` never set that field, so the calls landed on a nil report —
// discarded at the instant they were produced, by the one line that had to set a
// struct member.
//
// What is asserted here is that the loss REACHES THE CALLER, on the callback
// internal/app turns into x-dorang-downgraded and x-dorang-dropped-params. That
// a field is populated is not asserted anywhere, because a populated field
// nobody reads is exactly what was already there.

// acceptedLoss is what [Call.Accepted] was handed, SNAPSHOT AT THE INSTANT IT
// FIRED.
//
// A snapshot rather than the pointer, and the difference is the whole of what is
// being tested. The report is one allocation reused for the exchange, so a test
// that kept the pointer and read it after Do returned would pass against a
// callback that fired far too early and was handed an empty report — which is
// precisely the shipped defect. internal/app copies at callback time
// (fillRouteResult renders the names into strings), so the snapshot is what the
// client actually gets.
type acceptedLoss struct {
	constructs []string
	dropped    []string
	downgrades []canonical.Downgrade
	called     bool
	// bytesWritten is how many bytes had reached the client when the callback
	// fired. It is what makes "the header block was still open" an assertion
	// rather than a claim.
	bytesWritten int64
}

func (a *acceptedLoss) lossy() bool { return len(a.constructs) > 0 || len(a.dropped) > 0 }

func (a *acceptedLoss) detail(construct string) string {
	for _, d := range a.downgrades {
		if d.Construct == construct {
			return d.Detail
		}
	}
	return ""
}

// countingWriter is the client's connection, counting what has reached it.
type countingWriter struct {
	header http.Header
	n      int64
	status int
}

func newCountingWriter() *countingWriter { return &countingWriter{header: http.Header{}} }

func (c *countingWriter) Header() http.Header { return c.header }
func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
func (c *countingWriter) WriteHeader(status int) { c.status = status }
func (c *countingWriter) Flush()                 {}

func runWithAccepted(t *testing.T, c *Call, p *Provider) (Result, *acceptedLoss) {
	t.Helper()
	got := &acceptedLoss{}
	w := newCountingWriter()
	c.Accepted = func(l *canonical.LossReport) {
		if got.called {
			t.Errorf("Call.Accepted fired twice; its contract is exactly once")
		}
		got.called = true
		got.constructs = l.Constructs()
		got.dropped = append([]string(nil), l.Dropped...)
		got.downgrades = append([]canonical.Downgrade(nil), l.Downgrades...)
		got.bytesWritten = w.n
	}
	res := testBackend("k").Do(context.Background(), target(p), c, w)
	return res, got
}

// anthropicMultiChoice is an OpenAI-shaped upstream answer with FOUR choices,
// which the Messages family has no shape for.
const anthropicMultiChoice = `{"id":"chatcmpl-1","object":"chat.completion","created":1,
	"model":"upstream-model","choices":[
	{"index":0,"message":{"role":"assistant","content":"one"},"finish_reason":"stop"},
	{"index":1,"message":{"role":"assistant","content":"two"},"finish_reason":"stop"},
	{"index":2,"message":{"role":"assistant","content":"three"},"finish_reason":"stop"},
	{"index":3,"message":{"role":"assistant","content":"four"},"finish_reason":"stop"}],
	"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

// TestABufferedResponseSideDowngradeReachesTheClient is the buffered half.
//
// An OpenAI deployment answers four choices; the caller speaks Messages, which
// carries one message per turn. That is a response-side loss with no request-side
// equivalent — the caller did not ask for `n`, the UPSTREAM volunteered four —
// and it was reported to nobody.
func TestABufferedResponseSideDowngradeReachesTheClient(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, anthropicMultiChoice)
	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

	c := chatCall(catalog.APIAnthropicMessages)
	res, got := runWithAccepted(t, c, p)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if !got.called {
		t.Fatal("Call.Accepted never fired on a buffered answer")
	}
	if got.bytesWritten != 0 {
		t.Fatalf("%d bytes had already reached the client when Accepted fired; the "+
			"header block must still be open", got.bytesWritten)
	}
	if !got.lossy() {
		t.Fatal("the report handed to the caller was empty. Four choices were folded " +
			"into one message, and either the encoder wrote that into a report nobody " +
			"held or the handover happened before the answer had been read")
	}
	if !hasConstruct(got.constructs, canonical.ConstructMultipleChoices) {
		t.Errorf("Constructs() = %v, want it to name %q — this is what reaches "+
			"x-dorang-downgraded", got.constructs, canonical.ConstructMultipleChoices)
	}
	// And the answer really was folded, so the report is describing something
	// that happened rather than a bit that was set.
	if n := strings.Count(string(res.Body), `"type":"text"`); n != 1 {
		t.Errorf("the rendered answer has %d text blocks; this test is measuring the "+
			"wrong thing if the four choices survived", n)
	}
}

// TestAResponseSideDowngradeIsNotClassifiedAsADroppedParam pins the
// classification.
//
// [canonical.CapMultipleChoices] is Material: the caller is handed a DIFFERENT
// answer, not the same answer with a knob unapplied. The request half has always
// said so — MaterialLoss raises ConstructMultipleChoices as a Downgrade, and the
// §10.1 gate refuses on it — while the response half called DropParam("n"), so
// one construct had two classifications and a client that consented to the loss
// read about it in the header that means "a knob was not applied".
func TestAResponseSideDowngradeIsNotClassifiedAsADroppedParam(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, anthropicMultiChoice)
	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

	_, got := runWithAccepted(t, chatCall(catalog.APIAnthropicMessages), p)
	for _, d := range got.dropped {
		if d == canonical.ConstructMultipleChoices {
			t.Fatalf("n went out the DROPPABLE channel; it is Material, and the "+
				"request half of the same construct is a Downgrade (Dropped = %v)",
				got.dropped)
		}
	}
	if !hasConstruct(got.constructs, canonical.ConstructMultipleChoices) {
		t.Fatal("n was reported through neither channel")
	}
	// The detail carries the number, for the same reason the request half's does:
	// "n was dropped" is not actionable and "4 choices" is.
	if detail := got.detail(canonical.ConstructMultipleChoices); !strings.Contains(detail, "4") {
		t.Errorf("downgrade detail = %q, want it to say how many choices were folded", detail)
	}
}

// anthropicRichStop is a Messages answer whose stop reason has no OpenAI
// spelling, which is the located half of a response-side downgrade.
const anthropicRichStop = `{"id":"msg-1","type":"message","role":"assistant","model":"upstream-model",
	"content":[{"type":"text","text":"hi"}],"stop_reason":"pause_turn",
	"usage":{"input_tokens":7,"output_tokens":2}}`

// TestAStopReasonCollapsedIntoTheClientsFamilyIsReported is the other buffered
// case, and it is the one no capability mask can reconstruct: the fact belongs
// to the ANSWER, not to the request, so internal/app's mask half — which
// subtracts the deployment's set from what the REQUEST uses — cannot see it at
// all.
func TestAStopReasonCollapsedIntoTheClientsFamilyIsReported(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, anthropicRichStop)
	p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)

	res, got := runWithAccepted(t, chatCall(catalog.APIOpenAIChat), p)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if !hasConstruct(got.constructs, canonical.ConstructRichStopReason) {
		t.Errorf("Constructs() = %v, want %q: pause_turn has no spelling in the chat "+
			"completion enumeration and was collapsed onto one that does",
			got.constructs, canonical.ConstructRichStopReason)
	}
}

// TestTheResponseIsEncodedAgainstTheClientsCapabilitySetOnPurpose closes the
// silent fallback.
//
// `opt.caps()` answers DefaultCapabilities when none is supplied, and none was.
// The value that produced was RIGHT — an anthropic client's default set is
// exactly CapabilitiesForAPI(APIAnthropicMessages) — which is worse than being
// wrong, because it made the response direction correct by coincidence through a
// nil check whose comment says "none supplied" rather than "the caller's".
//
// The assertion is on the seam rather than on the encoder: the set the response
// is encoded against must be the CLIENT's, and it must differ from the
// DEPLOYMENT's on a crossing, or there is nothing here to get wrong.
func TestTheResponseIsEncodedAgainstTheClientsCapabilitySetOnPurpose(t *testing.T) {
	f := newFakeUpstream(t)
	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	x := &exchange{
		call:   chatCall(catalog.APIAnthropicMessages),
		target: &Target{Provider: p, UpstreamModel: "upstream-model"},
		prov:   p,
	}
	client, deployment := x.clientCapabilities(), x.capabilities()
	if client == deployment {
		t.Fatal("the client's set and the deployment's are equal on a crossing; " +
			"this test cannot tell which one the encoder used")
	}
	if want := CapabilitiesForAPI(catalog.APIAnthropicMessages); client != want {
		t.Errorf("clientCapabilities() = %v, want the Messages family's %v", client, want)
	}
	// And the deployment's set is the one the REQUEST half uses, unchanged.
	if want := CapabilitiesForAPI(catalog.APIOpenAIChat); deployment != want {
		t.Errorf("capabilities() = %v, want the deployment's %v", deployment, want)
	}
}

// --- the streamed half: the finding ------------------------------------------

// TestAStreamedAnswerHasNoHeaderBlockLeftForAResponseSideLoss records the
// negative result, with the same rigour as the positive one.
//
// The question was whether a point exists that BOTH kinds of answer pass through
// with their header block still open and their response-side loss already known.
// For a buffered answer it does — [Backend.finish] returns before a byte is
// written, which is why the test above passes. For a stream it does not, and
// this is the proof rather than the assertion of it:
//
//   - [Call.Accepted] fires before [Backend.relay], whose first event write
//     stamps the headers. Zero bytes have reached the client at that instant,
//     which is asserted below.
//   - Not one byte of the upstream's answer has been READ at that instant
//     either, which is also asserted below: the report is empty of anything the
//     response could contribute, and the upstream's own body proves there was
//     something to contribute.
//   - The sinks write each event as they convert it, so every later instant has
//     bytes already on the wire.
//
// Three facts, and together they say there is no fourth point. Putting the
// response half on [Result] would not help: Do returns after the stream is
// finished, later still.
//
// This test therefore asserts the CONSTRAINT. If someone later finds a way to
// deliver a stream's response-side loss, this test is what they have to come and
// change, with the reasoning above in front of them.
func TestAStreamedAnswerHasNoHeaderBlockLeftForAResponseSideLoss(t *testing.T) {
	// A Messages upstream whose stop reason has no chat-completion spelling —
	// the same response-side downgrade the buffered test asserts is reported —
	// delivered as a stream.
	events := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg-1","type":"message",` +
			`"role":"assistant","model":"upstream-model","content":[],` +
			`"usage":{"input_tokens":7,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"},` +
			`"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, events)
	})
	p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)

	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true

	w := httptest.NewRecorder()
	var (
		called       bool
		atCallback   []string
		bytesAtCall  int
		sawRichStop  bool
		reportHandle *canonical.LossReport
	)
	c.Accepted = func(l *canonical.LossReport) {
		called = true
		reportHandle = l
		atCallback = l.Constructs()
		bytesAtCall = w.Body.Len()
	}
	res := testBackend("k").Do(context.Background(), target(p), c, w)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if !called {
		t.Fatal("Call.Accepted never fired on a streamed answer")
	}

	// (1) The header block was still open — and that is the LAST such instant.
	if bytesAtCall != 0 {
		t.Errorf("%d bytes had reached the client when Accepted fired", bytesAtCall)
	}
	if !res.FirstByteSent {
		t.Fatal("the stream sent nothing; this test is measuring the wrong thing")
	}

	// (2) The response direction had contributed nothing at that instant, because
	// the answer had not been read. The upstream's own body is what says there
	// was something to contribute.
	if hasConstruct(atCallback, canonical.ConstructRichStopReason) {
		t.Fatal("a response-side construct was already in the report at Accepted time; " +
			"if a stream can now report one, this test's reasoning is stale and the " +
			"comment above it has to be rewritten rather than deleted")
	}
	if !strings.Contains(events, "pause_turn") {
		t.Fatal("the fixture no longer carries a rich stop reason")
	}
	if !strings.Contains(w.Body.String(), "chat.completion.chunk") {
		t.Fatal("the stream did not cross families; there was no downgrade to lose")
	}
	sawRichStop = strings.Contains(w.Body.String(), `"finish_reason"`)
	if !sawRichStop {
		t.Fatal("the client's stream carried no finish_reason, so nothing was collapsed")
	}

	// (3) Nothing filled the report afterwards either. The caller still holds the
	// pointer, so if a later writer ever appears this is where it shows up — and
	// it would show up too late to be a header, which is the finding.
	if reportHandle.Lossy() {
		t.Errorf("the report grew to %v after the headers were gone. That is not a "+
			"report; it is a value written where nobody can read it",
			reportHandle.Constructs())
	}
}

func hasConstruct(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
