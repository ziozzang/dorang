package scenario

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/testing/fake"
)

// DESIGN §14 scenario 15 — an Anthropic-shaped request through an
// OpenAI-shaped backend, tool calls included, over TWO turns.
//
// COMPATIBILITY §6 calls this the highest-risk surface, and DESIGN §10.2 says
// why the second turn is the one that matters: a naive implementation fails on
// turn two of an agentic flow, "which is the worst possible place to discover
// it". Turn one is an adapter exercise; turn two is where the assistant message
// the client received comes back and has to be re-encodable.

// crossFamily routes the Anthropic-shaped client to an OpenAI-shaped backend.
func crossFamily() router.Config {
	return router.Config{
		Groups: []router.Group{{
			Name: "claude-x", Class: "large",
			Deployments: []router.Deployment{{
				ID: "oai-1", Provider: "self-hosted", Kind: "openai", Family: "openai-chat",
				UpstreamModel: "qwen3.5:397b", MaxOutputTokens: 4096,
				Credentials: []router.Credential{{ID: "k1"}},
				// What this deployment can express (§10.1). Left zero and with
				// no catalog to fill it from, capability filtering would remove
				// every candidate before ranking — which is the correct
				// behaviour for a deployment that declares nothing, and the
				// wrong setup for this scenario.
				Capabilities: openai.DefaultCapabilities,
			}},
		}},
	}
}

// toolCallBackend answers with reasoning text and one tool call, in the OpenAI
// shape. The argument object's key order is deliberate: turn two must replay it
// byte-identically rather than re-encode a parse of it.
const toolArgs = `{"city":"Seoul","units":"metric"}`

func toolCallBackend() fake.Options {
	return fake.Options{
		Shape: fake.ShapeOpenAI,
		Script: func(r *fake.Recorded) fake.Script {
			if r.Seq == 1 {
				return fake.Script{
					Model:    r.Model,
					Thinking: "I need the weather before answering.",
					ToolCalls: []fake.ToolCall{{
						ID: "call_wx1", Name: "get_weather", Arguments: toolArgs, ArgChunks: 2,
					}},
					Finish: "tool_calls",
					Usage:  fake.Usage{InputTokens: 41, OutputTokens: 17},
				}
			}
			return fake.Script{
				Model: r.Model, Text: "It is sunny and 21C in Seoul.", TextChunks: 2,
				Finish: "stop",
				Usage:  fake.Usage{InputTokens: 88, OutputTokens: 12},
			}
		},
	}
}

const anthropicTurn1 = `{
  "model": "claude-x",
  "max_tokens": 1024,
  "messages": [{"role":"user","content":"what is the weather in Seoul?"}],
  "tools": [{"name":"get_weather","description":"look up weather",
             "input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]
}`

func TestScenario15_AnthropicRequestThroughOpenAIBackendTwoTurns(t *testing.T) {
	g := newGateway(t, crossFamily(), rigOpts{}, map[string]fake.Options{
		"oai-1": toolCallBackend(),
	})

	// ---- turn 1 -------------------------------------------------------------
	turn1, err := g.do(t, Call{Family: FamilyAnthropic, Body: []byte(anthropicTurn1)})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if turn1.Status != 200 {
		t.Fatalf("turn 1 status = %d: %s", turn1.Status, turn1.Body)
	}

	// The backend saw an OpenAI-shaped request: a messages array and a tool
	// declared under the function wrapper this family does not have.
	up1 := g.ups["oai-1"].Last()
	var upReq struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Tools    []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(up1.Body, &upReq); err != nil {
		t.Fatalf("upstream body is not OpenAI-shaped JSON: %s", up1.Body)
	}
	if upReq.Model != "qwen3.5:397b" {
		t.Errorf("upstream model = %q, want the real id", upReq.Model)
	}
	if len(upReq.Tools) != 1 || upReq.Tools[0].Type != "function" || upReq.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("the tool did not cross into the OpenAI shape: %s", up1.Body)
	}
	if upReq.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want the caller's 1024", upReq.MaxTokens)
	}
	if strings.Contains(string(up1.Body), "input_schema") {
		t.Errorf("this family's schema key reached an OpenAI backend: %s", up1.Body)
	}

	// The client saw an Anthropic-shaped response.
	var resp struct {
		ID           string          `json:"id"`
		Type         string          `json:"type"`
		Role         string          `json:"role"`
		Model        string          `json:"model"`
		Content      []anthropicPart `json:"content"`
		StopReason   *string         `json:"stop_reason"`
		StopSequence *string         `json:"stop_sequence"`
		Usage        struct {
			InputTokens  int  `json:"input_tokens"`
			OutputTokens int  `json:"output_tokens"`
			TotalTokens  *int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(turn1.Body, &resp); err != nil {
		t.Fatalf("client body is not Anthropic-shaped: %s", turn1.Body)
	}
	if resp.Type != "message" || resp.Role != "assistant" {
		t.Errorf("envelope = %q/%q, want message/assistant", resp.Type, resp.Role)
	}
	if resp.Model != "claude-x" {
		t.Errorf("the body must carry the requested name; got %q", resp.Model)
	}
	// 6.4: tool_calls collapses to tool_use. 6.5: stop_sequence is null.
	if resp.StopReason == nil || *resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", resp.StopReason)
	}
	if resp.StopSequence != nil {
		t.Errorf("stop_sequence = %v, want null", *resp.StopSequence)
	}
	// 6.8: the non-streaming shape carries the non-spec total_tokens.
	if resp.Usage.TotalTokens == nil {
		t.Error("the non-streaming shape must carry total_tokens")
	}

	var toolBlock *anthropicPart
	var thinkingBlock *anthropicPart
	for i := range resp.Content {
		switch resp.Content[i].Type {
		case "tool_use":
			toolBlock = &resp.Content[i]
		case "thinking":
			thinkingBlock = &resp.Content[i]
		}
	}
	if toolBlock == nil {
		t.Fatalf("the tool call did not reach the client as a tool_use block: %s", turn1.Body)
	}
	if toolBlock.Name != "get_weather" || toolBlock.ID != "call_wx1" {
		t.Errorf("tool_use block = %+v", toolBlock)
	}
	if string(toolBlock.Input) != toolArgs {
		t.Errorf("the argument object was re-encoded: got %s, want the original bytes %s",
			toolBlock.Input, toolArgs)
	}
	// The reasoning crossed as a thinking block with NO signature, because the
	// OpenAI side has no field that carries one and dorang never mints one.
	if thinkingBlock == nil {
		t.Fatalf("the backend's reasoning did not reach the client: %s", turn1.Body)
	}
	if thinkingBlock.Signature != "" {
		t.Fatalf("a signature was fabricated for reasoning that never carried one: %q",
			thinkingBlock.Signature)
	}

	// ---- turn 2: the client echoes the assistant turn back ------------------
	//
	// Verbatim. This is the whole point of running two turns: whatever dorang
	// handed the client on turn one has to be something dorang accepts back.
	echoed, err := json.Marshal(resp.Content)
	if err != nil {
		t.Fatal(err)
	}
	turn2Body := `{
      "model": "claude-x",
      "max_tokens": 1024,
      "messages": [
        {"role":"user","content":"what is the weather in Seoul?"},
        {"role":"assistant","content":` + string(echoed) + `},
        {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_wx1",
          "content":[{"type":"text","text":"sunny, 21C"}]}]}
      ],
      "tools": [{"name":"get_weather","input_schema":{"type":"object"}}]
    }`

	turn2, err := g.do(t, Call{Family: FamilyAnthropic, Body: []byte(turn2Body)})
	if err != nil {
		t.Fatalf("turn 2 failed — this is the turn DESIGN §10.2 says a naive "+
			"implementation breaks on: %v", err)
	}
	if turn2.Status != 200 {
		t.Fatalf("turn 2 status = %d: %s", turn2.Status, turn2.Body)
	}

	// The echoed tool call and its result reached the backend in the OpenAI
	// shape: an assistant message with tool_calls, and a tool message keyed by
	// tool_call_id.
	up2 := g.ups["oai-1"].Last()
	var upTurn2 struct {
		Messages []struct {
			Role      string `json:"role"`
			Content   json.RawMessage
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
				Index *int `json:"index"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(up2.Body, &upTurn2); err != nil {
		t.Fatalf("turn 2 upstream body: %v\n%s", err, up2.Body)
	}
	var sawAssistantToolCall, sawToolResult bool
	for _, m := range upTurn2.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) == 1 {
			sawAssistantToolCall = true
			tc := m.ToolCalls[0]
			if tc.ID != "call_wx1" || tc.Function.Name != "get_weather" {
				t.Errorf("echoed tool call = %+v", tc)
			}
			if tc.Function.Arguments != toolArgs {
				t.Errorf("the argument object did not survive the round trip: got %s, want %s",
					tc.Function.Arguments, toolArgs)
			}
			// COMPATIBILITY 5.2: index is STRIPPED from an echoed assistant
			// message, so a client replaying what it saw in a stream is not
			// rejected by an upstream that refuses the field.
			if tc.Index != nil {
				t.Errorf("an echoed tool call carried index=%d upstream", *tc.Index)
			}
		}
		if m.Role == "tool" && m.ToolCallID == "call_wx1" {
			sawToolResult = true
		}
	}
	if !sawAssistantToolCall {
		t.Errorf("the echoed assistant tool call did not reach the backend:\n%s", up2.Body)
	}
	if !sawToolResult {
		t.Errorf("the tool result did not reach the backend as a tool message:\n%s", up2.Body)
	}

	t.Run("the echoed unsigned block is refused by the family that requires one", func(t *testing.T) {
		// The inverse, and the reason turn two exists. The same bytes that are
		// perfectly valid toward an OpenAI backend cannot go to an Anthropic
		// one: the reasoning was derived from another family's plain-text field
		// and carries no integrity material, and dorang will not mint one.
		//
		// A gateway that dropped the block instead would produce a backend
		// error naming a field the caller never wrote.
		cfg := crossFamily()
		cfg.Groups[0].Deployments[0].Kind = "anthropic"
		cfg.Groups[0].Deployments[0].Family = "anthropic-messages"
		g := newGateway(t, cfg, rigOpts{}, map[string]fake.Options{
			"oai-1": {Shape: fake.ShapeAnthropic, Script: func(r *fake.Recorded) fake.Script {
				return fake.Script{Model: r.Model, Text: "should never be reached"}
			}},
		})
		_, err := g.do(t, Call{Family: FamilyAnthropic, Body: []byte(turn2Body)})
		if err == nil {
			t.Fatal("an unsigned reasoning block was accepted by the family that requires a signature")
		}
		// internal/wire/anthropic raises a typed *anthropic.OpaqueError; L5
		// renders it into the one envelope dorang answers with. What a caller
		// sees is that envelope, so that is what this asserts.
		var se *server.Error
		if !errors.As(err, &se) {
			t.Fatalf("the refusal must be a typed gateway error; got %T: %v", err, err)
		}
		if se.Status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400: no sibling of this family accepts an unsigned block either",
				se.Status)
		}
		if !strings.Contains(se.Message, anthropic.ReasonUnsignedThinking) {
			t.Errorf("the refusal must name the reason; got %q", se.Message)
		}
		if g.ups["oai-1"].Count() != 0 {
			t.Error("the request reached the backend anyway")
		}

		t.Run("DIVERGENCE: the machine-readable half of the refusal does not survive L5",
			func(t *testing.T) {
				// Characterization, not endorsement, and it became visible the
				// moment this harness stopped encoding requests itself: the
				// harness called anthropic.MarshalRequest directly and therefore
				// saw the typed error that production never delivers.
				//
				// *anthropic.OpaqueError carries Reason and — the load-bearing
				// one — Construct, whose whole purpose is that "the caller can
				// put it in x-dorang-allow-lossy and retry deliberately". Its own
				// ToError() renders the reason as the wire `code`.
				// backend.encodeError does neither: it flattens the error into a
				// message string under the generic code conversion_failed, and it
				// sets no Unwrap, so the reason cannot be recovered downstream
				// either. A caller is told what went wrong in prose and is given
				// nothing to act on.
				var oe *anthropic.OpaqueError
				if errors.As(err, &oe) {
					t.Skipf("the typed refusal now survives L5 (reason %q, construct %q): "+
						"restore the typed assertion and delete this subtest", oe.Reason, oe.Construct)
				}
				t.Logf("the caller receives code %q and a sentence; the construct id that "+
					"x-dorang-allow-lossy takes is lost", se.Code)
			})
	})

	t.Run("streaming crosses the families too", func(t *testing.T) {
		// The same crossing on the streaming path, where the block boundaries
		// have to be synthesized rather than read (COMPATIBILITY 6.6).
		g := newGateway(t, crossFamily(), rigOpts{}, map[string]fake.Options{
			"oai-1": toolCallBackend(),
		})
		body := strings.Replace(anthropicTurn1, `"max_tokens": 1024,`,
			`"max_tokens": 1024, "stream": true,`, 1)
		rep, err := g.do(t, Call{Family: FamilyAnthropic, Body: []byte(body)})
		if err != nil {
			t.Fatalf("streaming turn 1: %v", err)
		}
		out := string(rep.Body)
		for _, want := range []string{
			"event: message_start", "event: content_block_start",
			"event: content_block_delta", "event: content_block_stop",
			"event: message_delta", "event: message_stop",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("the relayed stream is missing %s:\n%s", want, out)
			}
		}
		if strings.Contains(out, "[DONE]") {
			t.Error("the chat-completions terminator leaked into an Anthropic stream")
		}
		if strings.Contains(out, "event: ping") {
			t.Error("dorang must not emit ping")
		}
		if n := strings.Count(out, "event: message_stop"); n != 1 {
			t.Errorf("message_stop appeared %d times, want 1", n)
		}
		lastStop := strings.LastIndex(out, "event: content_block_stop")
		delta := strings.Index(out, "event: message_delta")
		if lastStop < 0 || delta < 0 || lastStop > delta {
			t.Errorf("a content_block_stop must precede the terminal message_delta:\n%s", out)
		}
		if !strings.Contains(out, `"type":"tool_use"`) {
			t.Errorf("the tool call did not survive the streaming crossing:\n%s", out)
		}
		if !strings.Contains(out, `"stop_reason":"tool_use"`) {
			t.Errorf("the terminal stop reason is not tool_use:\n%s", out)
		}
		// 6.8: total_tokens is on the non-streaming shape only.
		if strings.Contains(out, "total_tokens") {
			t.Errorf("a stream must not carry total_tokens:\n%s", out)
		}
	})
}

// anthropicPart is the subset of a content block the scenario reads back.
type anthropicPart struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text,omitempty"`
	Thinking  *string         `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}
