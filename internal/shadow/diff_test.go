package shadow

import (
	"net/http"
	"strings"
	"testing"
)

// jsonHeader and sseHeader are the two content types every comparison here runs
// under.
func jsonHeader() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return h
}

func sseHeader() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	return h
}

// compareBodies is the whole pipeline for two non-streaming JSON responses.
func compareBodies(t *testing.T, ds, rs int, dBody, rBody string, ignore ...string) ([]Diff, []Inconclusive) {
	t.Helper()
	ig := newIgnoreSet(ignore)
	d := analyze(ds, jsonHeader(), []byte(dBody), nil, false, ig)
	r := analyze(rs, jsonHeader(), []byte(rBody), nil, false, ig)
	return compareShapes(d, r)
}

func compareStreams(t *testing.T, dBody, rBody string, ignore ...string) ([]Diff, []Inconclusive) {
	t.Helper()
	ig := newIgnoreSet(ignore)
	d := analyze(200, sseHeader(), []byte(dBody), nil, false, ig)
	r := analyze(200, sseHeader(), []byte(rBody), nil, false, ig)
	return compareShapes(d, r)
}

func kinds(diffs []Diff) []string {
	out := make([]string, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, d.Kind)
	}
	return out
}

func hasKind(diffs []Diff, kind string) bool {
	for _, d := range diffs {
		if d.Kind == kind {
			return true
		}
	}
	return false
}

func findPath(diffs []Diff, path string) *Diff {
	for i := range diffs {
		if diffs[i].Path == path {
			return &diffs[i]
		}
	}
	return nil
}

// The six positive detections §14.1 requires.

func TestDetectsMissingField(t *testing.T) {
	const withUsage = `{"id":"a","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	const withoutUsage = `{"id":"b","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}]}`

	diffs, _ := compareBodies(t, 200, 200, withUsage, withoutUsage)
	if !hasKind(diffs, KindFieldMissing) {
		t.Fatalf("missing usage was not reported: %v", diffs)
	}
	d := findPath(diffs, "$.usage.prompt_tokens")
	if d == nil || d.Reference != absent {
		t.Errorf("want $.usage.prompt_tokens absent on the reference, got %+v", d)
	}
	if !hasKind(diffs, KindUsage) {
		t.Errorf("the usage presence dimension itself was not reported: %v", kinds(diffs))
	}
}

func TestDetectsChangedType(t *testing.T) {
	// COMPATIBILITY §11.1: code is a string or null, never a number. A backend
	// that sends an integer there raises inside every SDK.
	const strCode = `{"error":{"message":"nope","type":"invalid_request_error","code":"model_not_found"}}`
	const numCode = `{"error":{"message":"nope","type":"invalid_request_error","code":404}}`

	diffs, _ := compareBodies(t, 404, 404, strCode, numCode)
	d := findPath(diffs, "$.error.code")
	if d == nil || d.Kind != KindFieldType {
		t.Fatalf("want a type difference at $.error.code, got %+v (all: %v)", d, diffs)
	}
	if d.Dorang != "string" || d.Reference != "number" {
		t.Errorf("want string vs number, got %q vs %q", d.Dorang, d.Reference)
	}
	if !hasKind(diffs, KindErrCode) {
		t.Errorf("the error code vocabulary difference was not reported: %v", kinds(diffs))
	}
}

func TestDetectsDifferentFrameSequence(t *testing.T) {
	// Same kinds, different order. The protocol fixes the order, so this can
	// never be model nondeterminism.
	const inOrder = "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	const reordered = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	diffs, inc := compareStreams(t, inOrder, reordered)
	if !hasKind(diffs, KindFrameSequence) {
		t.Fatalf("reordered frames were not reported: %v / %v", diffs, inc)
	}
	if hasKind(diffs, KindFrameKinds) {
		t.Errorf("the kind sets are equal; only the order differs: %v", diffs)
	}
}

func TestFrameCountIsNotAFrameSequenceDifference(t *testing.T) {
	// Two responses of different lengths are the same shape. If this fires,
	// every single streamed comparison produces a difference and the report is
	// worthless.
	short := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	long := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		strings.Repeat("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"xyz\"}}]}\n\n", 40) +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	diffs, inc := compareStreams(t, short, long)
	if len(diffs) != 0 {
		t.Fatalf("different chunk counts must not be a difference, got %v", diffs)
	}
	if len(inc) != 0 {
		t.Fatalf("nothing here is undecidable, got %v", inc)
	}
}

func TestDetectsDifferentTerminator(t *testing.T) {
	const withDone = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: [DONE]\n\n"
	// COMPATIBILITY §1.2 requires the [DONE] sentinel. A gateway that stops
	// sending it hangs every client that waits for it.
	const withoutDone = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n"

	diffs, _ := compareStreams(t, withDone, withoutDone)
	d := findPath(diffs, "stream/terminator")
	if d == nil {
		t.Fatalf("missing terminator was not reported: %v", diffs)
	}
	if d.Dorang != "done" || d.Reference != "content" {
		t.Errorf("want done vs content, got %q vs %q", d.Dorang, d.Reference)
	}
}

func TestDetectsDifferentStatus(t *testing.T) {
	// COMPATIBILITY §11.2 argues at length that a model outside a key's
	// allow-list is 403 and not 401 — a divergence in exactly this dimension.
	diffs, _ := compareBodies(t, 403, 401,
		`{"error":{"message":"x","type":"permission_error","code":"model_not_allowed"}}`,
		`{"error":{"message":"x","type":"permission_error","code":"model_not_allowed"}}`)
	d := findPath(diffs, "status")
	if d == nil || d.Dorang != "403" || d.Reference != "401" {
		t.Fatalf("status difference not reported: %+v (all %v)", d, diffs)
	}
}

func TestDetectsDifferentErrorEnvelopeShape(t *testing.T) {
	// The Anthropic SDK dispatches on the outer "type":"error"
	// (COMPATIBILITY §11.1); the OpenAI family has no such wrapper.
	const anthropic = `{"type":"error","error":{"type":"invalid_request_error","message":"x"}}`
	const openai = `{"error":{"message":"x","type":"invalid_request_error","code":"invalid_request"}}`

	diffs, _ := compareBodies(t, 400, 400, anthropic, openai)
	d := findPath(diffs, "error/envelope")
	if d == nil {
		t.Fatalf("envelope shape difference not reported: %v", diffs)
	}
	if d.Dorang != "anthropic" || d.Reference != "nested" {
		t.Errorf("want anthropic vs nested, got %q vs %q", d.Dorang, d.Reference)
	}
}

func TestDetectsFlatAndBareStringEnvelopes(t *testing.T) {
	// SGLANG.md §6.2: one process serves five envelope shapes. A migration that
	// changes which one a client sees is a break, so each is recognized.
	for _, tc := range []struct{ body, want string }{
		{`{"object":"error","message":"x","type":"BadRequestError","code":400}`, "flat"},
		{`{"error":"Unauthorized"}`, "bare_string"},
		{`{"detail":[{"loc":["body"],"msg":"field required"}]}`, "detail"},
	} {
		diffs, _ := compareBodies(t, 400, 400, tc.body,
			`{"error":{"message":"x","type":"invalid_request_error","code":"invalid_request"}}`)
		d := findPath(diffs, "error/envelope")
		if d == nil || d.Dorang != tc.want {
			t.Errorf("body %s: want envelope %q, got %+v", tc.body, tc.want, d)
		}
	}
}

// The negatives: things that must never produce a difference.

func TestDoesNotFireOnDifferentIdsTimestampsOrOutputText(t *testing.T) {
	const a = `{"id":"chatcmpl-aaaaaaaa","object":"chat.completion","created":1700000000,` +
		`"model":"m","system_fingerprint":"fp_1","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"The capital of France is Paris."},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18}}`
	const b = `{"id":"chatcmpl-zzzzzzzz","object":"chat.completion","created":1799999999,` +
		`"model":"m","system_fingerprint":null,"choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"Paris is the capital city of France, and has been since 987."},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":19,"total_tokens":29}}`

	diffs, inc := compareBodies(t, 200, 200, a, b)
	if len(diffs) != 0 {
		t.Fatalf("ids, timestamps, fingerprints, output text and token counts must not "+
			"produce differences, got %v", diffs)
	}
	if len(inc) != 0 {
		t.Fatalf("nothing here is undecidable, got %v", inc)
	}
}

func TestDoesNotFireOnDifferentChoiceCounts(t *testing.T) {
	const one = `{"choices":[{"index":0,"message":{"role":"a","content":"x"},"finish_reason":"stop"}]}`
	const two = `{"choices":[{"index":0,"message":{"role":"a","content":"x"},"finish_reason":"stop"},` +
		`{"index":1,"message":{"role":"a","content":"y"},"finish_reason":"stop"}]}`
	diffs, _ := compareBodies(t, 200, 200, one, two)
	if len(diffs) != 0 {
		t.Fatalf("array length is not structure, got %v", diffs)
	}
}

func TestPresenceOfIdIsStillCompared(t *testing.T) {
	// The complement of the test above, and the reason the built-in ignore list
	// is short: an id that varies is invisible, but an id that stopped being
	// emitted is a break.
	diffs, _ := compareBodies(t, 200, 200,
		`{"id":"a","object":"chat.completion"}`,
		`{"object":"chat.completion"}`)
	d := findPath(diffs, "$.id")
	if d == nil || d.Kind != KindFieldMissing {
		t.Fatalf("a dropped id must be reported: %+v (all %v)", d, diffs)
	}
}

func TestNullIsItsOwnType(t *testing.T) {
	// COMPATIBILITY §2.1: real OpenAI emits "logprobs":null and the reference
	// proxy omits it. Absent and null are different answers, and the comparison
	// has to be able to say which one each side gave.
	diffs, _ := compareBodies(t, 200, 200,
		`{"choices":[{"index":0,"finish_reason":null}]}`,
		`{"choices":[{"index":0}]}`)
	if !hasKind(diffs, KindFieldMissing) {
		t.Fatalf("null present vs field absent must differ, got %v", diffs)
	}

	diffs, _ = compareBodies(t, 200, 200,
		`{"x":null}`, `{"x":"s"}`)
	d := findPath(diffs, "$.x")
	if d == nil || d.Dorang != "null" || d.Reference != "string" {
		t.Fatalf("null vs string must be a type difference, got %+v", d)
	}
}

func TestFinishReasonNullOnAllButTheLastFrameIsNotADifference(t *testing.T) {
	// The type at a path is a set, precisely so that a field which is null on
	// every chunk but one does not report a difference on every stream.
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	diffs, _ := compareStreams(t, stream, stream)
	if len(diffs) != 0 {
		t.Fatalf("a stream compared with itself must be clean, got %v", diffs)
	}
}

func TestStopReasonValueIsCompared(t *testing.T) {
	// COMPATIBILITY §4.2a: an error-shaped native reason mapped to "stop" tells
	// the client a failed turn ended normally. Two gateways disagreeing about
	// the mapping is exactly what the gate exists to find.
	diffs, _ := compareBodies(t, 200, 200,
		`{"choices":[{"index":0,"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"index":0,"finish_reason":"stop"}]}`)
	if !hasKind(diffs, KindStopReason) {
		t.Fatalf("differing finish_reason values must be reported: %v", diffs)
	}
}

func TestHeaderKeySetIsComparedAndValuesAreNot(t *testing.T) {
	dh := jsonHeader()
	dh.Set("X-Ratelimit-Remaining-Requests", "99")
	dh.Set("Date", "Mon, 01 Jan 2024 00:00:00 GMT")
	dh.Set("X-Dorang-Request-Id", "abc")

	rh := jsonHeader()
	rh.Set("X-Ratelimit-Remaining-Requests", "1") // different value, same key
	rh.Set("Date", "Tue, 02 Jan 2024 00:00:00 GMT")
	rh.Set("Retry-After", "30") // key the incumbent has and dorang does not

	ig := newIgnoreSet(nil)
	d := analyze(429, dh, []byte(`{}`), nil, false, ig)
	r := analyze(429, rh, []byte(`{}`), nil, false, ig)
	diffs, _ := compareShapes(d, r)

	if findPath(diffs, "header/x-ratelimit-remaining-requests") != nil {
		t.Errorf("header values are not compared: %v", diffs)
	}
	if findPath(diffs, "header/date") != nil {
		t.Errorf("transport headers are not compared: %v", diffs)
	}
	if findPath(diffs, "header/x-dorang-request-id") != nil {
		t.Errorf("dorang's own headers are not compared: %v", diffs)
	}
	ra := findPath(diffs, "header/retry-after")
	if ra == nil || ra.Dorang != absent {
		t.Fatalf("a header the incumbent sends and dorang does not must be reported: %+v (all %v)",
			ra, diffs)
	}
}

func TestIgnoreFieldsSuppressesASubtree(t *testing.T) {
	const a = `{"keep":1,"drop":{"x":1,"y":[{"z":true}]}}`
	const b = `{"keep":1}`

	if diffs, _ := compareBodies(t, 200, 200, a, b); len(diffs) == 0 {
		t.Fatal("without an ignore rule this must differ")
	}
	diffs, _ := compareBodies(t, 200, 200, a, b, "drop")
	if len(diffs) != 0 {
		t.Fatalf("ignoring `drop` must suppress it and everything under it, got %v", diffs)
	}
}

func TestIgnoreFieldsAcceptsExactPathsAndPrefixes(t *testing.T) {
	const a = `{"meta":{"a":1,"b":2},"other":1}`
	const b = `{"other":1}`

	if diffs, _ := compareBodies(t, 200, 200, a, b, "$.meta"); len(diffs) != 0 {
		t.Errorf("exact path rule did not apply: %v", diffs)
	}
	if diffs, _ := compareBodies(t, 200, 200, a, b, "$.meta*"); len(diffs) != 0 {
		t.Errorf("prefix rule did not apply: %v", diffs)
	}
}

// Trustworthiness: the report must never be silently clean.

func TestUnparseableOnOneSideIsADifferenceNotASkip(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	ig := newIgnoreSet(nil)
	d := analyze(502, h, []byte(`{"error":{"message":"x","type":"api_error"}}`), nil, false, ig)
	r := analyze(502, h, []byte(`<html><body>502 Bad Gateway</body></html>`), nil, false, ig)

	diffs, inc := compareShapes(d, r)
	if !hasKind(diffs, KindUnparseable) && !hasKind(diffs, KindContentKind) {
		t.Fatalf("JSON on one side and HTML on the other must be reported: %v / %v", diffs, inc)
	}
}

func TestBothSidesUnparseableIsInconclusiveNotClean(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	ig := newIgnoreSet(nil)
	d := analyze(502, h, []byte(`not json at all`), nil, false, ig)
	r := analyze(502, h, []byte(`also not json`), nil, false, ig)

	diffs, inc := compareShapes(d, r)
	if len(diffs) != 0 {
		t.Errorf("two unreadable bodies of the same kind are not a difference: %v", diffs)
	}
	if len(inc) == 0 {
		t.Fatal("two unreadable bodies must be inconclusive, not clean")
	}
}

func TestTruncatedCaptureIsInconclusiveRatherThanClean(t *testing.T) {
	// The one case where an implementation is most tempted to call a comparison
	// clean when it only saw part of it.
	head := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n"
	tail := "data: [DONE]\n\n"
	ig := newIgnoreSet(nil)
	d := analyze(200, sseHeader(), []byte(head), []byte(tail), true, ig)
	r := analyze(200, sseHeader(), []byte(head), []byte(tail), true, ig)

	diffs, inc := compareShapes(d, r)
	if len(diffs) != 0 {
		t.Errorf("identical prefixes and suffixes are not a difference: %v", diffs)
	}
	if len(inc) == 0 {
		t.Fatal("a truncated comparison must be inconclusive, not clean")
	}
	// The terminator is still decided, which is the whole point of keeping a
	// tail window.
	if d.terminator != "done" {
		t.Errorf("terminator lost to truncation: %q", d.terminator)
	}
}

func TestTruncationDoesNotSuppressARealDifference(t *testing.T) {
	ig := newIgnoreSet(nil)
	d := analyze(200, sseHeader(),
		[]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n"),
		[]byte("data: [DONE]\n\n"), true, ig)
	r := analyze(200, sseHeader(),
		[]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n"),
		[]byte("event: message_stop\ndata: {}\n\n"), true, ig)

	diffs, _ := compareShapes(d, r)
	if findPath(diffs, "stream/terminator") == nil {
		t.Fatalf("a terminator difference survives truncation: %v", diffs)
	}
}

func TestAnthropicStreamShape(t *testing.T) {
	// COMPATIBILITY §6.1/§6.2: both lines, no ping, no [DONE], message_stop
	// exactly once. A reference that inserts a ping changes the kind set.
	const clean = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	withPing := strings.Replace(clean,
		"event: content_block_delta",
		"event: ping\ndata: {\"type\":\"ping\"}\n\nevent: content_block_delta", 1)

	diffs, _ := compareStreams(t, clean, withPing)
	if !hasKind(diffs, KindFrameKinds) {
		t.Fatalf("an injected ping frame must be reported: %v", diffs)
	}
	if diffs, _ := compareStreams(t, clean, clean); len(diffs) != 0 {
		t.Fatalf("a stream compared with itself must be clean, got %v", diffs)
	}
}

func TestStreamPathsAreBucketedByFrameKind(t *testing.T) {
	// A field dropped from message_start must not be masked by the same field
	// still being present in message_delta.
	const full = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	const missing = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	diffs, _ := compareStreams(t, full, missing)
	d := findPath(diffs, "event:message_start|$.message.usage.input_tokens")
	if d == nil {
		t.Fatalf("usage dropped from message_start must be reported, got %v", diffs)
	}
}

func TestEmptyStreamTerminator(t *testing.T) {
	// COMPATIBILITY §1.4: an empty upstream stream yields an empty body with no
	// [DONE]. A reference that synthesizes one differs.
	diffs, _ := compareStreams(t, "", "data: [DONE]\n\n")
	if findPath(diffs, "stream/terminator") == nil && !hasKind(diffs, KindContentKind) {
		t.Fatalf("empty vs terminated stream must be reported: %v", diffs)
	}
}

func TestIdenticalResponsesAreClean(t *testing.T) {
	const body = `{"id":"a","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	diffs, inc := compareBodies(t, 200, 200, body, body)
	if len(diffs) != 0 || len(inc) != 0 {
		t.Fatalf("identical responses must be clean: %v / %v", diffs, inc)
	}
}
