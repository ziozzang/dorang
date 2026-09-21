package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// The streamed Responses surface at the relay: a responses-family caller
// crossed onto chat-shaped and responses-shaped upstreams, the failure frame,
// and the terminal object handed to the store.

func responsesCall() *Call {
	max := 64
	return &Call{
		Op:         OpResponses,
		ClientAPI:  catalog.APIOpenAIResponses,
		Model:      "client-model",
		ResponseID: "resp_test",
		ResponseEcho: &openai.ResponsesOptions{
			ID: "resp_test", Model: "client-model",
		},
		Request: &canonical.Request{
			Model:     "client-model",
			Messages:  []canonical.Message{canonical.TextMessage(canonical.RoleUser, "hello")},
			MaxTokens: &max,
		},
		DefaultMaxTokens: 1024,
	}
}

// A chat upstream serving a responses caller: the crossing path decodes chat
// chunks and re-encodes the event tree, pinning the caller's identity and
// never leaking the chat id or the upstream model.
func TestAResponsesCallerStreamsFromAChatUpstream(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"m","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	c := responsesCall()
	c.Stream = true
	rec := httptest.NewRecorder()

	res := testBackend("k").Do(context.Background(), target(p), c, rec)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_text.delta",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the responses stream is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Error("the chat-completions terminator leaked into a responses stream")
	}
	if strings.Contains(body, "chatcmpl-") || strings.Contains(body, `"id":"1"`) {
		t.Error("the upstream chat id leaked into the caller's stream")
	}
	if !strings.Contains(body, `"id":"resp_test"`) {
		t.Error("the pinned response id must be the caller's")
	}
	if strings.Contains(body, `"model":"m"`) || strings.Contains(body, "upstream-model") {
		t.Error("the served model must never appear; the client-facing name does")
	}
	if !strings.Contains(body, "hello") {
		t.Errorf("the text did not reach the client:\n%s", body)
	}
}

// A responses-shaped upstream serving a responses caller: the relay decodes
// the typed tree to neutral events and re-encodes it — a dorang fed by a
// dorang, which is the symmetry the emitted event set was chosen for.
func TestAResponsesCallerStreamsFromAResponsesOnlyHost(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\n"+
			`data: {"type":"response.created","response":{"id":"up_1","model":"m","created_at":1}}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.added\n"+
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"u_0"}}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","item_id":"u_0","output_index":0,"delta":"round "}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","item_id":"u_0","output_index":0,"delta":"trip"}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"up_1","model":"m","created_at":1,`+
			`"status":"completed","usage":{"input_tokens":4,"output_tokens":2}}}`+"\n\n")
	})

	p := responsesOnlyProvider(t, f)
	c := responsesCall()
	c.Stream = true
	rec := httptest.NewRecorder()

	res := testBackend("k").Do(context.Background(), target(p), c, rec)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "round trip") && !(strings.Contains(body, "round ") && strings.Contains(body, "trip")) {
		t.Errorf("the text did not survive the crossing:\n%s", body)
	}
	if strings.Contains(body, `"id":"up_1"`) {
		t.Error("the upstream's response id must be replaced by the caller's")
	}
	if res.Usage.InputTokens != 4 || res.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v, want the counts the terminal carried", res.Usage)
	}
}

// A mid-stream upstream failure is response.failed, exactly once, with nothing
// after it — the responses spelling of COMPATIBILITY 11.1a.
func TestAResponsesStreamMidStreamFailureIsResponseFailed(t *testing.T) {
	const canary = "sk-CANARY-resp-77e1d0" // pragma: allowlist secret — test fixture
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"m","choices":[{"index":0,"delta":{"content":"par"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"error":{"message":"invalid api key: `+canary+`","code":"bad"}}`+"\n\n")
	})

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	c := responsesCall()
	c.Stream = true
	rec := httptest.NewRecorder()

	res := testBackend(canary).Do(context.Background(), target(p), c, rec)
	if res.Err == nil {
		t.Fatal("the attempt must be recorded as the failure it is")
	}
	body := rec.Body.String()
	if n := strings.Count(body, "event: response.failed"); n != 1 {
		t.Fatalf("%d response.failed frames, want exactly 1:\n%s", n, body)
	}
	if strings.Contains(body, "event: response.completed") {
		t.Error("a completed after a failed tells the client the generation finished")
	}
	// The scrubbing of DESIGN §10.6 rule 4: the upstream's sentence quoted the
	// operator's credential, and the frame is bound for a tenant.
	if strings.Contains(body, canary) {
		t.Error("the upstream's credential leaked into the failure frame")
	}
	if res.CompletedResponse != nil {
		t.Error("a failed stream hands the store nothing")
	}
}

// A truncated stream is never dressed as completion: no response.completed is
// synthesized and no terminal object reaches the store.
func TestAResponsesTruncatedStreamIsNotCompleted(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"m","choices":[{"index":0,"delta":{"content":"hal"}}]}`+"\n\n")
		// No finish chunk, no [DONE].
	})

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	c := responsesCall()
	c.Stream = true
	rec := httptest.NewRecorder()

	res := testBackend("k").Do(context.Background(), target(p), c, rec)
	if res.Err == nil {
		t.Fatal("a stream that stopped before its terminal must be an error")
	}
	if strings.Contains(rec.Body.String(), "event: response.completed") {
		t.Error("a synthesized completed over a truncated answer is the one failure no client can detect")
	}
	if res.CompletedResponse != nil {
		t.Error("the store must not receive a terminal object the stream never produced")
	}
}

// The completed body exposed on the Result is the response object the terminal
// event carried — byte for byte what GET /v1/responses/{id} will serve.
func TestTheCompletedResponsesBodyIsExposedForTheStore(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"stored"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":`+
			`{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	c := responsesCall()
	c.Stream = true
	rec := httptest.NewRecorder()

	res := testBackend("k").Do(context.Background(), target(p), c, rec)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if res.CompletedResponse == nil {
		t.Fatal("a completed stream must expose its terminal object for the store")
	}
	// The exposed object is the one the stream carried: same id, same status,
	// and it is a complete Responses response, not a fragment of one.
	body := string(res.CompletedResponse)
	for _, want := range []string{
		`"id":"resp_test"`, `"status":"completed"`, `"stored"`, `"object":"response"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("completed body missing %q:\n%s", want, body)
		}
	}
}
