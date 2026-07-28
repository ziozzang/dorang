package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// fixedClock is the one clock both paths of a conversion must read.
var fixedClock = time.Unix(1753660800, 0).UTC()

// clockedBackend is [testBackend] with time pinned, so that `created` can be
// compared for EQUALITY across the streaming and non-streaming paths rather
// than for approximate agreement.
func clockedBackend() *Backend {
	return New(Options{
		Credentials: staticCredentials{secrets: map[string]string{"c1": "k"}},
		Client:      NewClient(),
		Now:         func() time.Time { return fixedClock },
	})
}

// A complete Messages answer, and the same answer as a stream. The two carry
// the same content, the same id and the same counts; everything a client sees
// that differs between them is dorang's doing.
const (
	antWholeAnswer = `{"id":"msg_2026","type":"message","role":"assistant",` +
		`"model":"upstream-model","content":[{"type":"text","text":"ok"}],` +
		`"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":7,"output_tokens":2}}`

	antStreamedAnswer = "event: message_start\ndata: " +
		`{"type":"message_start","message":{"id":"msg_2026","type":"message","role":"assistant",` +
		`"model":"upstream-model","content":[],"stop_reason":null,"stop_sequence":null,` +
		`"usage":{"input_tokens":7,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\ndata: " +
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\ndata: " +
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
		"event: content_block_stop\ndata: " +
		`{"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\ndata: " +
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},` +
		`"usage":{"output_tokens":2}}` + "\n\n" +
		"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
)

// TestStreamingAndNonStreamingAgreeOnIdentity is the D4/D5 twin of
// internal/wire/openai's TestStreamingAndNonStreamingAgreeOnUsage.
//
// The equivalence is the property that keeps breaking: usage was the first
// place the two paths disagreed and identity was the second. Converting a
// Messages answer to a chat completion, the non-streaming path emitted
// `"created":0` — a timestamp in 1970 on every converted turn — and crossed the
// upstream's `msg_2026…` id verbatim under `"object":"chat.completion"`, while
// the streaming path for the identical conversion stamped a real timestamp and
// minted a `chatcmpl-` id. Same request, same upstream, two different answers,
// selected by nothing but stream:true.
func TestStreamingAndNonStreamingAgreeOnIdentity(t *testing.T) {
	// --- non-streaming -----------------------------------------------------
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, antWholeAnswer)
	p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)

	res := clockedBackend().Do(context.Background(), target(p),
		chatCall(catalog.APIOpenAIChat), nil)
	if res.Err != nil {
		t.Fatalf("non-streaming Do: %v", res.Err)
	}
	whole := decodeJSON(t, res.Body)

	// --- streaming ---------------------------------------------------------
	sf := streamUpstream(t, antStreamedAnswer)
	sp := testProvider(t, sf, "anthropic", catalog.APIAnthropicMessages)
	sc := chatCall(catalog.APIOpenAIChat)
	sc.Stream = true
	rec := httptest.NewRecorder()

	sres := clockedBackend().Do(context.Background(), target(sp), sc, rec)
	if sres.Err != nil {
		t.Fatalf("streaming Do: %v", sres.Err)
	}
	first := firstChunk(t, rec.Body.String())

	// --- created -----------------------------------------------------------
	//
	// Equal, and equal to the gateway's clock. A zero here is the defect.
	if got, ok := whole["created"].(float64); !ok || int64(got) != fixedClock.Unix() {
		t.Errorf("non-streaming created = %v, want %d", whole["created"], fixedClock.Unix())
	}
	if got, ok := first["created"].(float64); !ok || int64(got) != fixedClock.Unix() {
		t.Errorf("streamed created = %v, want %d", first["created"], fixedClock.Unix())
	}
	if whole["created"] != first["created"] {
		t.Errorf("the two paths disagree about created: %v vs %v — the same request is "+
			"timestamped differently depending on stream:true",
			whole["created"], first["created"])
	}

	// --- id ----------------------------------------------------------------
	//
	// The values differ (they are minted per response) but the SHAPE must not:
	// this answer crossed families, so neither path may hand a chat-completion
	// client an id belonging to another family.
	for path, obj := range map[string]map[string]any{
		"non-streaming": whole, "streamed": first,
	} {
		id, _ := obj["id"].(string)
		if !strings.HasPrefix(id, "chatcmpl-") {
			t.Errorf("%s id = %q, want a chatcmpl- id: this answer crossed from the "+
				"Messages family and the upstream's own id is not one of this family's",
				path, id)
		}
		if strings.HasPrefix(id, "msg_") {
			t.Errorf("%s id = %q — the upstream id crossed verbatim under "+
				"object:chat.completion", path, id)
		}
	}

	// --- model -------------------------------------------------------------
	if whole["model"] != "client-model" || first["model"] != "client-model" {
		t.Errorf("model = %v / %v, want the client's own name on both paths (§7.2)",
			whole["model"], first["model"])
	}
}

// TestSameFamilyKeepsTheUpstreamID is the other half of the §10.7 rule.
//
// D5 was decided rather than defaulted: the upstream's id crosses when the
// answer did NOT change family, because there it is a valid id of the caller's
// own family and it is the only handle a support ticket has on the upstream's
// side of the exchange. It is minted only when the crossing made it invalid.
func TestSameFamilyKeepsTheUpstreamID(t *testing.T) {
	t.Run("chat to chat", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.answer(http.StatusOK, `{"id":"chatcmpl-upstream","object":"chat.completion",`+
			`"created":1700000000,"model":"upstream-model","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

		res := clockedBackend().Do(context.Background(), target(p),
			chatCall(catalog.APIOpenAIChat), nil)
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		out := decodeJSON(t, res.Body)
		if out["id"] != "chatcmpl-upstream" {
			t.Errorf("id = %v, want the upstream's own: nothing crossed families", out["id"])
		}
		if got, _ := out["created"].(float64); int64(got) != 1700000000 {
			t.Errorf("created = %v, want the upstream's own stated value", out["created"])
		}
	})

	t.Run("messages to messages", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.answer(http.StatusOK, antWholeAnswer)
		p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)

		res := clockedBackend().Do(context.Background(), target(p),
			chatCall(catalog.APIAnthropicMessages), nil)
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		out := decodeJSON(t, res.Body)
		if out["id"] != "msg_2026" {
			t.Errorf("id = %v, want the upstream's own: nothing crossed families", out["id"])
		}
	})
}

// TestConvertedStreamOpensWithTheAssistantRole is D6.
//
// DESIGN §10.7's streaming table maps the stream-open event to "first chunk
// with delta.role" on this side and `message_start` on the other. The byte
// relay carries the role because it carries everything; the CONVERTED stream
// dropped it, so two clients of one gateway saw structurally different streams
// depending only on which family their deployment's upstream spoke.
func TestConvertedStreamOpensWithTheAssistantRole(t *testing.T) {
	f := streamUpstream(t, antStreamedAnswer)
	p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)
	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true
	rec := httptest.NewRecorder()

	if res := clockedBackend().Do(context.Background(), target(p), c, rec); res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	first := firstChunk(t, rec.Body.String())
	choices, _ := first["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in the first chunk: %v", first)
	}
	delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
	if delta["role"] != "assistant" {
		t.Errorf("first delta = %v, want role:assistant — the relay path carries it and "+
			"the converted path did not", delta)
	}
	// And exactly once: a role on every chunk is not what this family sends.
	if n := strings.Count(rec.Body.String(), `"role":"assistant"`); n != 1 {
		t.Errorf("the role appears %d times, want 1", n)
	}
}

// firstChunk decodes the first SSE data payload of a chat-completion stream.
func firstChunk(t *testing.T, body string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(payload), &out); err != nil {
			t.Fatalf("first chunk %q: %v", payload, err)
		}
		return out
	}
	t.Fatalf("no data frame in %q", body)
	return nil
}
