package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// codexStream is the shape a Responses-only host answers with: a reasoning
// item, a message, a tool call whose arguments arrive in two fragments, and a
// terminal event carrying usage and server-side tool spend.
const codexStream = `data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5","created_at":1700000000}}

data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"thinking"}

data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","role":"assistant","id":"msg_1"}}

data: {"type":"response.output_text.delta","output_index":1,"delta":"ye"}

data: {"type":"response.output_text.delta","output_index":1,"delta":"s"}

data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","name":"lookup","call_id":"call_1"}}

data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"q\":"}

data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":"\"seoul\"}"}

data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-5.5","output":[{"type":"function_call","name":"lookup"}],"usage":{"input_tokens":11,"output_tokens":20,"output_tokens_details":{"reasoning_tokens":13}},"tool_usage":{"web_search":{"num_requests":2}}}}

data: [DONE]

`

// responsesOnlyProvider is a host that serves /responses and refuses anything
// but store:false + stream:true — the ChatGPT Codex contract. ResponsesOnly is
// what the catalog says about the kind and what app copies into the Spec; here
// it is stated directly, because this package's tests do not load a catalog.
func responsesOnlyProvider(t *testing.T, f *fakeUpstream) *Provider {
	t.Helper()
	p, err := NewProvider(Spec{
		Name: "codex", Kind: "codex-responses", API: catalog.APIOpenAIResponses,
		BaseURL:              f.srv.URL,
		ResponsesOnly:        true,
		ResponsesForceStream: true,
		ResponsesStoreFalse:  true,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

// serveCodex answers like the real surface: 400 unless the contract is met,
// and an event stream when it is.
func serveCodex(t *testing.T, f *fakeUpstream) {
	t.Helper()
	serveStream(t, f, codexStream)
}

// serveStream answers the given events under the Codex contract.
func serveStream(t *testing.T, f *fakeUpstream, sse string) {
	t.Helper()
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.Error(w, `{"detail":"no such route"}`, http.StatusForbidden)
			return
		}
		// The harness drains r.Body to record it before this runs, so the
		// contract is checked against what it recorded — the same bytes.
		var body map[string]any
		_ = json.Unmarshal(f.last().body, &body)
		if s, _ := body["store"].(bool); s || body["store"] == nil {
			http.Error(w, `{"detail":"Store must be set to false"}`, http.StatusBadRequest)
			return
		}
		if s, _ := body["stream"].(bool); !s {
			http.Error(w, `{"detail":"Stream must be set to true"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})
}

// A non-streaming caller gets one answer from a host that only speaks streams.
//
// The transport is the host's contract and the SHAPE is the caller's choice;
// the two are different decisions and only the second is the caller's. This is
// asserted on the body the caller receives and the bytes the host received —
// not on any field dorang set for itself.
func TestABufferedCallerGetsOneAnswerFromAStreamOnlyHost(t *testing.T) {
	f := newFakeUpstream(t)
	serveCodex(t, f)
	p := responsesOnlyProvider(t, f)
	b := testBackend("sk-test")
	tg := target(p)

	res := b.Do(context.Background(), tg, chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())

	// What reached the host, asserted FIRST: the route the host serves and the
	// contract it enforces, whatever the caller said. A wrong wire is the more
	// diagnostic failure than the 400 it produces.
	last := f.last()
	if res.Err != nil {
		t.Fatalf("Do: %v\n  host saw %s %s\n  body: %s", res.Err, last.method, last.path, last.body)
	}
	if last.path != "/responses" {
		t.Errorf("host was asked at %q; a Responses-only host serves /responses and 403s the rest", last.path)
	}
	var sent map[string]any
	if err := json.Unmarshal(last.body, &sent); err != nil {
		t.Fatalf("host received non-JSON: %v", err)
	}
	if v, _ := sent["stream"].(bool); !v {
		t.Errorf("stream=%v on the wire for a host that refuses anything else", sent["stream"])
	}
	if v, ok := sent["store"].(bool); !ok || v {
		t.Errorf("store=%v on the wire for a host that requires false", sent["store"])
	}

	// What the caller got: one JSON chat completion, assembled from the events.
	if !strings.HasPrefix(res.ContentType, "application/json") {
		t.Fatalf("content type %q, want JSON for a caller that did not ask to stream", res.ContentType)
	}
	var out struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatalf("caller received non-JSON: %v\n%s", err, res.Body)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("%d choices, want 1:\n%s", len(out.Choices), res.Body)
	}
	c := out.Choices[0]
	if c.Message.Content != "yes" {
		t.Errorf("content = %q, want the two text deltas joined; got body:\n%s", c.Message.Content, res.Body)
	}
	if strings.Contains(c.Message.Content, "thinking") {
		t.Error("the model's reasoning reached the caller as its answer")
	}
	if len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].Function.Name != "lookup" ||
		c.Message.ToolCalls[0].Function.Arguments != `{"q":"seoul"}` {
		t.Errorf("tool call not assembled from its fragments: %+v", c.Message.ToolCalls)
	}
	if c.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls for a turn that ended in a call", c.FinishReason)
	}
	if out.Usage.PromptTokens != 11 || out.Usage.CompletionTokens != 20 {
		t.Errorf("usage = %d/%d, want 11/20 from the terminal event", out.Usage.PromptTokens, out.Usage.CompletionTokens)
	}
	// §7.2: the body carries the name the CLIENT asked for, and the shared
	// encoder's `created` — a collected answer must not lose either by being
	// rendered on a side path.
	if out.Model != "client-model" {
		t.Errorf("model = %q in the body, want the client's own name (§7.2)", out.Model)
	}
	if out.Created == 0 {
		t.Error("created = 0; the shared encoder fills it and a side path did not")
	}
	if res.Usage.InputTokens != 11 || res.Usage.ReasoningTokens != 13 {
		t.Errorf("Result.Usage = %+v; the ledger bills from this, not from the body", res.Usage)
	}
	// Server-side tool spend must survive to the Result, or pricing charges nobody.
	if res.UsageExtra == nil || len(res.UsageExtra.ToolUsage) == 0 {
		t.Errorf("tool_usage did not reach the Result: %+v", res.UsageExtra)
	}
	// The upstream's own name was compared against the deployment's, by the
	// same line that compares it for a JSON answer. Unobserved is the reading a
	// side path leaves behind: "the response carried no model name", when it
	// carried one on its first event.
	if res.ModelAgreement == canonical.ModelUnobserved {
		t.Error("ModelAgreement = unobserved; the collected answer's model name never reached " +
			"the substitution check")
	}
}

// A collected answer is rendered in the CALLER's protocol, not always as chat.
//
// The shape the caller receives is decided by [Backend.convert] for every
// adapter, and a collected stream is no exception: a Messages caller gets a
// message, a Responses caller gets a response. Rendering chat JSON on a side
// path would have handed an Anthropic-SDK client a body it cannot parse.
func TestACollectedAnswerIsRenderedInTheCallersProtocol(t *testing.T) {
	t.Run("anthropic messages caller", func(t *testing.T) {
		f := newFakeUpstream(t)
		serveCodex(t, f)
		b := testBackend("sk-test")
		res := b.Do(context.Background(), target(responsesOnlyProvider(t, f)),
			chatCall(catalog.APIAnthropicMessages), httptest.NewRecorder())
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		var out struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
				Name string `json:"name"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
		}
		if err := json.Unmarshal(res.Body, &out); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, res.Body)
		}
		if out.Type != "message" || out.Role != "assistant" {
			t.Fatalf("a Messages caller received type=%q role=%q — not a message:\n%s",
				out.Type, out.Role, res.Body)
		}
		var text, tool bool
		for _, c := range out.Content {
			switch c.Type {
			case "text":
				text = c.Text == "yes"
			case "tool_use":
				tool = c.Name == "lookup"
			}
		}
		if !text || !tool {
			t.Errorf("content lost in translation (text=%t tool=%t):\n%s", text, tool, res.Body)
		}
		if out.StopReason != "tool_use" {
			t.Errorf("stop_reason = %q, want tool_use", out.StopReason)
		}
	})

	t.Run("responses caller", func(t *testing.T) {
		f := newFakeUpstream(t)
		serveCodex(t, f)
		b := testBackend("sk-test")
		c := chatCall(catalog.APIOpenAIResponses)
		c.Op = OpResponses
		res := b.Do(context.Background(), target(responsesOnlyProvider(t, f)), c, httptest.NewRecorder())
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		var out struct {
			Object string `json:"object"`
			Output []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"output"`
		}
		if err := json.Unmarshal(res.Body, &out); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, res.Body)
		}
		if out.Object != "response" {
			t.Fatalf("a /v1/responses caller received object=%q:\n%s", out.Object, res.Body)
		}
		var call bool
		for _, o := range out.Output {
			if o.Type == "function_call" && o.Name == "lookup" {
				call = true
			}
		}
		if !call {
			t.Errorf("the tool call did not reach the Responses caller:\n%s", res.Body)
		}
	})
}

// The caller's §10.5b transform is applied to a collected answer.
//
// A filter that masked the caller's text on the way in restores it on the way
// out, on the neutral form, once. A collected answer that skipped that step
// would hand the caller their own placeholders back — and only on this one
// provider, which is the kind of defect nobody finds until it ships.
func TestTheCallersTransformIsAppliedToACollectedAnswer(t *testing.T) {
	f := newFakeUpstream(t)
	serveCodex(t, f)
	b := testBackend("sk-test")
	c := chatCall(catalog.APIOpenAIChat)
	c.Transform = upperTransform{}
	res := b.Do(context.Background(), target(responsesOnlyProvider(t, f)), c, httptest.NewRecorder())
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if !strings.Contains(string(res.Body), `"content":"YES"`) {
		t.Errorf("the transform did not run on the collected answer:\n%s", res.Body)
	}
}

// upperTransform is the smallest observable §10.5b transform.
type upperTransform struct{}

func (upperTransform) Response(r *canonical.Response) {
	for i := range r.Choices {
		for j := range r.Choices[i].Message.Content {
			blk := &r.Choices[i].Message.Content[j]
			if blk.Kind == canonical.KindText {
				blk.Text = strings.ToUpper(blk.Text)
			}
		}
	}
}

func (upperTransform) Stream() StreamTransform { return nil }

// A streaming caller is relayed, not collected.
func TestAStreamingCallerIsRelayedFromAStreamOnlyHost(t *testing.T) {
	f := newFakeUpstream(t)
	serveCodex(t, f)
	p := responsesOnlyProvider(t, f)
	b := testBackend("sk-test")
	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true
	c.Request.Stream = true
	rec := httptest.NewRecorder()

	res := b.Do(context.Background(), target(p), c, rec)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data:") {
		t.Fatalf("a streaming caller received no event stream:\n%s", body)
	}
	if !strings.Contains(body, "yes") && !strings.Contains(body, `"ye"`) {
		t.Errorf("the relayed stream carries no text:\n%s", body)
	}
	if res.Usage.InputTokens != 11 {
		t.Errorf("Result.Usage = %+v after a relayed stream", res.Usage)
	}
}

// A mid-stream error is an error, is terminal, and does not carry the credential.
//
// Three claims, one event. The upstream failed after it had begun generating:
// the caller must see a failure and not an empty 200; the failure must NOT be
// offered to the fallback chain, because the host bills the generation it
// started and a sibling deployment would buy a second one; and the message the
// upstream wrote — which quotes the bearer it rejected, as several do — must
// leave dorang with the credential replaced, exactly as the relay would have
// replaced it had the caller asked to stream.
func TestAMidStreamErrorIsTerminalAndScrubbed(t *testing.T) {
	const secret = "sk-test-0123456789abcdef"
	f := newFakeUpstream(t)
	serveStream(t, f, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"gpt-5.5\"}}\n\n"+
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\"}}\n\n"+
		"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"partial\"}\n\n"+
		"data: {\"type\":\"error\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"slow down, token "+secret+" is over quota\"}}\n\n")
	b := testBackend(secret)
	res := b.Do(context.Background(), target(responsesOnlyProvider(t, f)),
		chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
	if res.Err == nil {
		t.Fatalf("a stream that ended in an error produced a successful answer:\n%s", res.Body)
	}
	if res.Err.Code != CodeUpstreamStreamError {
		t.Errorf("code = %q, want %q: the upstream failed inside a generation it had begun",
			res.Err.Code, CodeUpstreamStreamError)
	}
	if res.Retryable {
		t.Error("a failure after generation began was offered to the fallback chain; " +
			"the host bills the turn it started, and a hop repeats the charge")
	}
	if !strings.Contains(res.Err.NativeMessage, "slow down") {
		t.Errorf("the upstream's reason was lost: %+v", res.Err)
	}
	for _, field := range []string{res.Err.Message, res.Err.NativeMessage, res.Err.NativeType, res.Err.Type} {
		if strings.Contains(field, secret) {
			t.Fatalf("the credential left dorang in an error field: %q", field)
		}
	}
	if !strings.Contains(res.Err.NativeMessage, redacted) {
		t.Errorf("the credential was removed without a placeholder, so the operator cannot "+
			"tell that the upstream quoted one: %q", res.Err.NativeMessage)
	}
}

// A stream that stops before its terminal event is not an answer.
//
// What arrived is a prefix. Handing it on as a 200 with `finish_reason: stop`
// tells the caller the model finished when it did not, which is §10.5a's
// objection in its buffered form. It is terminal for the same reason a
// mid-stream error is: the generation was billed.
//
// A stream with NO events is the other case: the host answered 200 and said
// nothing, no generation began, and the chain the operator configured for a
// deployment that answers wrongly is exactly the right next hop.
func TestATruncatedStreamIsNotAPartialAnswer(t *testing.T) {
	t.Run("cut off after content", func(t *testing.T) {
		f := newFakeUpstream(t)
		serveStream(t, f, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"gpt-5.5\"}}\n\n"+
			"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\"}}\n\n"+
			"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"the beginning of\"}\n\n")
		b := testBackend("sk-test")
		res := b.Do(context.Background(), target(responsesOnlyProvider(t, f)),
			chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
		if res.Err == nil {
			t.Fatalf("a cut-off stream became a 200:\n%s", res.Body)
		}
		if res.Err.Code != CodeUpstreamStreamTruncated {
			t.Errorf("code = %q, want %q", res.Err.Code, CodeUpstreamStreamTruncated)
		}
		if res.Retryable {
			t.Error("a truncated generation was offered to the fallback chain")
		}
		if len(res.Body) != 0 {
			t.Errorf("a failed collection still produced a body:\n%s", res.Body)
		}
	})

	t.Run("no events at all", func(t *testing.T) {
		f := newFakeUpstream(t)
		serveStream(t, f, "")
		b := testBackend("sk-test")
		res := b.Do(context.Background(), target(responsesOnlyProvider(t, f)),
			chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
		if res.Err == nil {
			t.Fatalf("an empty stream became a 200:\n%s", res.Body)
		}
		if res.Err.Code != CodeUpstreamShape {
			t.Errorf("code = %q, want %q for a host that answered 200 and said nothing",
				res.Err.Code, CodeUpstreamShape)
		}
		if !res.Retryable {
			t.Error("nothing was generated, and the sibling deployment was not offered")
		}
	})
}

// A collected tool call with unparseable arguments is refused, as a JSON one is.
//
// The rule lives in [Backend.convert] and applies to whatever the adapter
// returns. A collected answer that rendered itself would have skipped it and
// handed the client a finished call it cannot execute.
func TestACollectedMalformedToolCallIsRefused(t *testing.T) {
	f := newFakeUpstream(t)
	serveStream(t, f, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"gpt-5.5\"}}\n\n"+
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"name\":\"lookup\",\"call_id\":\"call_1\"}}\n\n"+
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"q\\\":\"}\n\n"+
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"name\":\"lookup\"}]}}\n\n")
	b := testBackend("sk-test")
	res := b.Do(context.Background(), target(responsesOnlyProvider(t, f)),
		chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
	if res.Err == nil {
		t.Fatalf("a tool call with arguments that are not JSON was handed on as finished:\n%s", res.Body)
	}
	if res.Err.Code != CodeMalformedToolArguments {
		t.Errorf("code = %q, want %q", res.Err.Code, CodeMalformedToolArguments)
	}
	if !strings.Contains(res.Err.Message, "lookup") {
		t.Errorf("the refusal does not name the call: %v", res.Err)
	}
}

// The contract settings are refused on a host that is not Responses-only.
//
// `force_stream` and `store_false` are read by one adapter. On any other
// provider they load, sit in the operator's file looking effective, and change
// nothing on the wire — CONFIG §23.2's defect. Whether a host is Responses-only
// is the catalog's declaration about the kind and arrives on the Spec; this is
// asserted on what NewProvider does with it, not on the table it reads.
func TestTheContractSettingsAreRefusedOffAResponsesOnlyHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		want string
	}{
		{"force_stream on a chat host", Spec{Kind: "openai", API: catalog.APIOpenAIChat,
			BaseURL: "https://example.invalid", ResponsesForceStream: true}, "force_stream"},
		{"store_false on a chat host", Spec{Kind: "openai", API: catalog.APIOpenAIChat,
			BaseURL: "https://example.invalid", ResponsesStoreFalse: true}, "store_false"},
		{"both on a responses-shaped host that serves chat too", Spec{Kind: "openai", API: catalog.APIOpenAIResponses,
			BaseURL: "https://example.invalid", ResponsesForceStream: true, ResponsesStoreFalse: true}, "force_stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewProvider(tc.spec)
			if err == nil {
				t.Fatal("a setting only the Responses-only adapter reads was accepted on a provider " +
					"that adapter does not serve; it loads and changes nothing")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name the key: %v", err)
			}
		})
	}

	t.Run("accepted where the adapter reads them", func(t *testing.T) {
		p, err := NewProvider(Spec{Kind: "codex-responses", API: catalog.APIOpenAIResponses,
			BaseURL: "https://example.invalid", ResponsesOnly: true,
			ResponsesForceStream: true, ResponsesStoreFalse: true})
		if err != nil {
			t.Fatalf("the settings were refused on the one host they are for: %v", err)
		}
		if _, ok := p.ad.(responsesAdapter); !ok {
			t.Errorf("adapter = %T, want responsesAdapter for a Responses-only host", p.ad)
		}
	})

	t.Run("the flag alone selects the adapter", func(t *testing.T) {
		// Not the kind name: a self-hosted Responses-only proxy under any kind
		// the operator declares responses_only in their catalog overlay.
		p, err := NewProvider(Spec{Kind: "openai", API: catalog.APIOpenAIResponses,
			BaseURL: "https://example.invalid", ResponsesOnly: true})
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		if _, ok := p.ad.(responsesAdapter); !ok {
			t.Errorf("adapter = %T; the catalog flag, not a kind-name table, decides", p.ad)
		}
	})
}
