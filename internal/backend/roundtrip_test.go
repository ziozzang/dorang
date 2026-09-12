package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// decodeJSON is the assertion helper: every round-trip test compares parsed
// structure rather than bytes, because key ORDER is the wire adapters' contract
// and re-asserting it here would only duplicate their golden tests.
func decodeJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v\n%s", err, b)
	}
	return m
}

func TestRoundTripOpenAIChat(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"id":"cmpl-1","object":"chat.completion","created":1,"model":"upstream-model",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`)

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	res := testBackend("sk-test").Do(context.Background(), target(p), chatCall(catalog.APIOpenAIChat), nil) // pragma: allowlist secret — test fixture
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}

	req := f.last()
	if req.path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", req.path)
	}
	if got := req.header.Get("Authorization"); got != "Bearer sk-test" { // pragma: allowlist secret — test fixture
		t.Errorf("Authorization = %q", got)
	}
	if got := req.header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding = %q, want identity: a compressed stream cannot be relayed frame by frame", got)
	}
	sent := decodeJSON(t, req.body)
	if sent["model"] != "upstream-model" {
		t.Errorf("upstream model = %v, want the REAL id", sent["model"])
	}

	out := decodeJSON(t, res.Body)
	if out["model"] != "client-model" {
		t.Errorf("answer model = %v, want the name the client asked for (§7.2)", out["model"])
	}
	if res.Usage.InputTokens != 11 || res.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v, want 11 in / 2 out", res.Usage)
	}
}

func TestRoundTripAnthropicMessages(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"id":"msg-1","type":"message","role":"assistant","model":"upstream-model",
		"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":7,"output_tokens":2,"cache_read_input_tokens":3}}`)

	p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)
	res := testBackend("sk-ant").Do(context.Background(), target(p),
		chatCall(catalog.APIAnthropicMessages), nil)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}

	req := f.last()
	if req.path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", req.path)
	}
	if got := req.header.Get("x-api-key"); got != "sk-ant" {
		t.Errorf("x-api-key = %q: this family does not take a bearer token", got)
	}
	if req.header.Get("Authorization") != "" {
		t.Error("Authorization must not be set for the messages family")
	}
	if got := req.header.Get("anthropic-version"); got != DefaultAnthropicVersion {
		t.Errorf("anthropic-version = %q, want %q: the header is mandatory on this surface",
			got, DefaultAnthropicVersion)
	}

	// Cache reads are reported INSIDE the input count (DESIGN §10.7): 7 exclusive
	// plus 3 cached is 10 inclusive, and getting this backwards mis-bills every
	// cached request.
	if res.Usage.InputTokens != 10 || res.Usage.CacheReadTokens != 3 {
		t.Errorf("usage = %+v, want inclusive input 10 with 3 cache reads", res.Usage)
	}
}

func TestRoundTripCrossFamily(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"id":"cmpl-1","object":"chat.completion","created":1,"model":"upstream-model",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)

	// An Anthropic-speaking client against an OpenAI-shaped deployment.
	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	res := testBackend("sk").Do(context.Background(), target(p),
		chatCall(catalog.APIAnthropicMessages), nil)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	out := decodeJSON(t, res.Body)
	if out["type"] != "message" {
		t.Errorf("answer is not a messages-shaped body: %v", out)
	}
	if out["model"] != "client-model" {
		t.Errorf("answer model = %v", out["model"])
	}
}

func TestCountTokensOnlyOnMessages(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"input_tokens":42}`)

	p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)
	c := chatCall(catalog.APIAnthropicMessages)
	c.Op = OpCountTokens
	res := testBackend("sk").Do(context.Background(), target(p), c, nil)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if f.last().path != "/v1/messages/count_tokens" {
		t.Errorf("path = %q", f.last().path)
	}
	if string(res.Body) != `{"input_tokens":42}` {
		t.Errorf("body = %s, want the count relayed unchanged", res.Body)
	}

	// The same operation against an OpenAI-shaped deployment is refused by name
	// rather than sent somewhere that will not understand it.
	p2 := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	res = testBackend("sk").Do(context.Background(), target(p2), c, nil)
	if res.Err == nil || res.Err.Status != http.StatusNotImplemented {
		t.Fatalf("want a 501, got %+v", res.Err)
	}
	if res.Err.Code != "count_tokens_unsupported" { // pragma: allowlist secret — test fixture
		t.Errorf("code = %q, want count_tokens_unsupported", res.Err.Code)
	}
}

func TestRoundTripEmbeddingsRelay(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"object":"list","model":"upstream-model",
		"data":[{"object":"embedding","index":0,"embedding":[0.5,0.25]}],
		"usage":{"prompt_tokens":9,"total_tokens":9}}`)

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	c := &Call{
		Op: OpEmbeddings, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
		Body: []byte(`{"model":"client-model","input":"hello","encoding_format":"float"}`),
	}
	res := testBackend("sk").Do(context.Background(), target(p), c, nil)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	sent := decodeJSON(t, f.last().body)
	if sent["model"] != "upstream-model" {
		t.Errorf("upstream model = %v", sent["model"])
	}
	if sent["encoding_format"] != "float" {
		t.Error("a relayed field was dropped: not recognising something is not a reason to remove it")
	}
	if sent["input"] != "hello" {
		t.Errorf("input = %v, want the string form preserved for this family", sent["input"])
	}
	out := decodeJSON(t, res.Body)
	if out["model"] != "client-model" {
		t.Errorf("answer model = %v", out["model"])
	}
	if res.Usage.InputTokens != 9 {
		t.Errorf("usage = %+v, want the relayed usage read", res.Usage)
	}
}

func TestGeminiRoundTrip(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi there"}]},
		"finishReason":"STOP","index":0}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,
		"cachedContentTokenCount":6,"thoughtsTokenCount":3,"totalTokenCount":17},
		"modelVersion":"gemini-2.5-pro","responseId":"resp-1"}`)

	p := testProvider(t, f, "google", catalog.APIGemini)
	c := chatCall(catalog.APIOpenAIChat)
	c.Request.System = canonical.Content{canonical.TextBlock("be brief")}
	temp := 0.5
	c.Request.Temperature = &temp
	c.Request.Stop = []string{"END"}

	res := testBackend("goog-key").Do(context.Background(), target(p), c, nil)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}

	req := f.last()
	if req.path != "/v1beta/models/upstream-model:generateContent" {
		t.Errorf("path = %q: the model belongs IN the path for this family", req.path)
	}
	if got := req.header.Get("x-goog-api-key"); got != "goog-key" {
		t.Errorf("x-goog-api-key = %q", got)
	}
	if req.header.Get("Authorization") != "" {
		t.Error("this family does not take a bearer token")
	}
	if strings.Contains(req.query, "key=") {
		t.Error("the credential must never travel in the URL: URLs reach access logs and traces")
	}

	sent := decodeJSON(t, req.body)
	contents, _ := sent["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents = %v", sent["contents"])
	}
	first, _ := contents[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("role = %v", first["role"])
	}
	sys, _ := sent["systemInstruction"].(map[string]any)
	if sys == nil {
		t.Fatal("systemInstruction is missing: the system prompt is not a message in this family")
	}
	cfg, _ := sent["generationConfig"].(map[string]any)
	if cfg == nil {
		t.Fatal("generationConfig is missing")
	}
	if cfg["maxOutputTokens"] != float64(64) {
		t.Errorf("maxOutputTokens = %v, want the caller's ceiling", cfg["maxOutputTokens"])
	}
	if cfg["temperature"] != 0.5 {
		t.Errorf("temperature = %v", cfg["temperature"])
	}
	if _, ok := sent["max_tokens"]; ok {
		t.Error("a chat-completions field leaked into a Gemini request")
	}

	// The usage normalization of §10.7: thoughts are output and are billed as
	// output, prompt already includes the cached prefix. Reported names the four
	// counters this body stated — the fifth, cache write, has no member in this
	// family — so a measured zero on any of them survives to the client's
	// breakdown instead of being omitted as unmeasured.
	want := canonical.Usage{
		InputTokens: 10, OutputTokens: 7, CacheReadTokens: 6, ReasoningTokens: 3,
		Reported: canonical.UsageInput | canonical.UsageOutput |
			canonical.UsageCacheRead | canonical.UsageReasoning,
	}
	if res.Usage != want {
		t.Errorf("usage = %+v, want %+v", res.Usage, want)
	}
	if res.Usage.OutputTokens < res.Usage.ReasoningTokens {
		t.Error("reasoning tokens must be contained in output tokens")
	}
	out := decodeJSON(t, res.Body)
	if out["model"] != "client-model" {
		t.Errorf("answer model = %v", out["model"])
	}
}

func TestGeminiToolCallRoundTrip(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"get_weather","args":{"city":"Seoul"}}}]},
		"finishReason":"STOP","index":0}],
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8}}`)

	p := testProvider(t, f, "google", catalog.APIGemini)
	c := chatCall(catalog.APIOpenAIChat)
	c.Request.Tools = []canonical.Tool{{
		Type: "function", Name: "get_weather", Description: "look it up",
		Parameters: json.RawMessage(`{"type":"object"}`),
	}}
	c.Request.ToolChoice = &canonical.ToolChoice{Mode: canonical.ToolChoiceRequired}

	res := testBackend("k").Do(context.Background(), target(p), c, nil)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	sent := decodeJSON(t, f.last().body)
	tools, _ := sent["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", sent["tools"])
	}
	tc, _ := sent["toolConfig"].(map[string]any)
	fcc, _ := tc["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "ANY" {
		t.Errorf("mode = %v, want ANY for a required tool choice", fcc["mode"])
	}

	out := decodeJSON(t, res.Body)
	choices, _ := out["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	// The API reports STOP for a turn that ended in a function call. A client
	// that branches on the finish reason to decide whether to run a tool would
	// never run one if this were relayed literally.
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
	msg, _ := choice["message"].(map[string]any)
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", msg["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	if call["id"] == "" || call["id"] == nil {
		t.Error("a tool call must carry an id: this family issues none and one has to be synthesized")
	}
}

// TestGeminiToolResultAddressedByName covers the crossing that has no id at all
// on the far side: a tool RESULT is addressed by the tool's name, which is only
// knowable from the call that preceded it in the same conversation.
func TestGeminiToolResultAddressedByName(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"candidates":[{"content":{"role":"model","parts":[{"text":"22C"}]},
		"finishReason":"STOP","index":0}]}`)

	p := testProvider(t, f, "google", catalog.APIGemini)
	c := chatCall(catalog.APIOpenAIChat)
	c.Request.Messages = []canonical.Message{
		canonical.TextMessage(canonical.RoleUser, "weather?"),
		{Role: canonical.RoleAssistant, Content: canonical.Content{
			canonical.ToolUseBlock("call_abc", "get_weather", json.RawMessage(`{"city":"Seoul"}`)),
		}},
		{Role: canonical.RoleTool, Content: canonical.Content{
			canonical.ToolResultBlock("call_abc", canonical.TextBlock("22C")),
		}},
	}
	if res := testBackend("k").Do(context.Background(), target(p), c, nil); res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}

	sent := decodeJSON(t, f.last().body)
	contents, _ := sent["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %d turns, want 3", len(contents))
	}
	last, _ := contents[2].(map[string]any)
	parts, _ := last["parts"].([]any)
	part, _ := parts[0].(map[string]any)
	fr, _ := part["functionResponse"].(map[string]any)
	if fr == nil {
		t.Fatalf("the tool result is not a functionResponse: %v", part)
	}
	if fr["name"] != "get_weather" {
		t.Errorf("functionResponse.name = %v, want the name from the call it answers", fr["name"])
	}
	if _, ok := fr["response"].(map[string]any); !ok {
		t.Errorf("response = %v, want a JSON object: this protocol requires one", fr["response"])
	}
}

func TestCohereRerank(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"id":"r-1","results":[{"index":1,"relevance_score":0.9},
		{"index":0,"relevance_score":0.1}],"meta":{"billed_units":{"search_units":1}}}`)

	p, err := NewProvider(Spec{Name: "co", Kind: "cohere", API: catalog.APICohere, BaseURL: f.srv.URL})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	c := &Call{
		Op: OpRerank, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
		Rerank: &canonical.RerankRequest{
			Model: "client-model", Query: "q",
			Documents: []canonical.RerankDocument{{Text: "a"}, {Text: "b"}},
		},
	}
	res := testBackend("co-key").Do(context.Background(), target(p), c, nil)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if got := f.last().path; got != "/v2/rerank" {
		t.Errorf("path = %q, want /v2/rerank with no version inserted", got)
	}
	sent := decodeJSON(t, f.last().body)
	if sent["model"] != "upstream-model" {
		t.Errorf("model = %v", sent["model"])
	}
	out := decodeJSON(t, res.Body)
	results, _ := out["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v", out["results"])
	}
	meta, _ := out["meta"].(map[string]any)
	if meta == nil {
		t.Error("search units were dropped: a vendor that bills in search units must not have them " +
			"folded into a token count that is then priced per token")
	}
}

func TestJinaRerankAndEmbeddings(t *testing.T) {
	f := newFakeUpstream(t)
	p, err := NewProvider(Spec{Name: "jina", Kind: "jina", API: catalog.APIJina, BaseURL: f.srv.URL})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	be := testBackend("jina-key")

	f.answer(http.StatusOK, `{"model":"m","results":[{"index":0,"relevance_score":0.7}],
		"usage":{"total_tokens":15,"prompt_tokens":15}}`)
	rr := &Call{
		Op: OpRerank, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
		Rerank: &canonical.RerankRequest{Model: "client-model", Query: "q",
			Documents: []canonical.RerankDocument{{Text: "a"}}},
	}
	res := be.Do(context.Background(), target(p), rr, nil)
	if res.Err != nil {
		t.Fatalf("rerank: %v", res.Err)
	}
	if got := f.last().path; got != "/v1/rerank" {
		t.Errorf("rerank path = %q", got)
	}
	if res.Usage.InputTokens != 15 {
		t.Errorf("usage = %+v, want the token billing read", res.Usage)
	}

	f.answer(http.StatusOK, `{"object":"list","model":"m","data":[{"index":0,"embedding":[1]}],
		"usage":{"prompt_tokens":3,"total_tokens":3}}`)
	er := &Call{
		Op: OpEmbeddings, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
		Body: []byte(`{"model":"client-model","input":"hello"}`),
	}
	if res := be.Do(context.Background(), target(p), er, nil); res.Err != nil {
		t.Fatalf("embeddings: %v", res.Err)
	}
	if got := f.last().path; got != "/v1/embeddings" {
		t.Errorf("embeddings path = %q", got)
	}
	sent := decodeJSON(t, f.last().body)
	arr, ok := sent["input"].([]any)
	if !ok || len(arr) != 1 || arr[0] != "hello" {
		t.Errorf("input = %v, want a one-element array: this vendor rejects the bare string OpenAI accepts",
			sent["input"])
	}
}

func TestCohereRefusesChatAndEmbeddings(t *testing.T) {
	f := newFakeUpstream(t)
	p, err := NewProvider(Spec{Name: "co", Kind: "cohere", API: catalog.APICohere, BaseURL: f.srv.URL})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	be := testBackend("k")

	res := be.Do(context.Background(), target(p), chatCall(catalog.APIOpenAIChat), nil)
	if res.Err == nil || res.Err.Status != http.StatusNotImplemented {
		t.Fatalf("chat: want 501, got %+v", res.Err)
	}
	if !strings.Contains(res.Err.Message, "compatibility") {
		t.Errorf("the refusal should name the route that does work: %q", res.Err.Message)
	}
	emb := &Call{Op: OpEmbeddings, ClientAPI: catalog.APIOpenAIChat, Model: "m", Body: []byte(`{}`)}
	res = be.Do(context.Background(), target(p), emb, nil)
	if res.Err == nil || res.Err.Code != "embeddings_unsupported" {
		t.Fatalf("embeddings: want a named 501, got %+v", res.Err)
	}
	if f.count() != 0 {
		t.Error("a refused operation must not reach the upstream")
	}
}

// TestUnsupportedKinds is the other half of the same rule: three kinds whose
// catalogued api names a shape their real service does not serve are refused by
// name rather than dispatched at a URL that does not exist.
func TestUnsupportedKinds(t *testing.T) {
	f := newFakeUpstream(t)
	// azure, vertex and bedrock left this table as each gained an adapter
	// (azure_test.go, vertex_test.go, bedrock_test.go). anthropic-vertex —
	// Claude on Vertex, a different route — remains, with the reason its
	// refusal names.
	for kind, code := range map[string]string{
		"anthropic-vertex": "anthropic_vertex_unsupported",
	} {
		p, err := NewProvider(Spec{Name: kind, Kind: kind, API: catalog.APIOpenAIChat, BaseURL: f.srv.URL})
		if err != nil {
			t.Fatalf("%s: NewProvider: %v", kind, err)
		}
		res := testBackend("k").Do(context.Background(), target(p), chatCall(catalog.APIOpenAIChat), nil)
		if res.Err == nil {
			t.Fatalf("%s: want a refusal", kind)
		}
		if res.Err.Status != http.StatusNotImplemented || res.Err.Code != code {
			t.Errorf("%s: got %d/%s, want 501/%s", kind, res.Err.Status, res.Err.Code, code)
		}
		if len(res.Err.Message) < 40 {
			t.Errorf("%s: the message must name what is missing, got %q", kind, res.Err.Message)
		}
	}
	if f.count() != 0 {
		t.Error("an unsupported kind must not reach any URL")
	}
}
