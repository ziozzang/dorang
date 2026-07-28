package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// The Responses API.
//
// The tests that matter are not the field renames — those are a table in
// DESIGN §10.7 — but the two places where this family is structurally different
// from the other two: the FLAT item sequence, and the state fields that have no
// wire equivalent anywhere else.

func TestResponsesRequestGolden(t *testing.T) {
	body := []byte(`{"model":"qwen3.5:397b","input":"hello","instructions":"be terse",` +
		`"max_output_tokens":64,"temperature":0.3,"store":true,` +
		`"text":{"format":{"type":"json_object"}},"reasoning":{"effort":"medium"}}`)
	req, err := DecodeResponsesRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	// §10.7: instructions is System, max_output_tokens is MaxTokens, text.format
	// is ResponseFormat, reasoning is the one neutral reasoning control.
	if got := req.System.Flatten(); got != "be terse" {
		t.Errorf("instructions became %q", got)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 64 {
		t.Errorf("max_output_tokens %v", req.MaxTokens)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Kind != "json_object" {
		t.Errorf("text.format %+v", req.ResponseFormat)
	}
	if req.Reasoning == nil || req.Reasoning.Effort != "medium" {
		t.Errorf("reasoning %+v", req.Reasoning)
	}
	if req.Store == nil || !*req.Store {
		t.Errorf("store %v", req.Store)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != canonical.RoleUser {
		t.Fatalf("a bare string input did not become a user turn: %+v", req.Messages)
	}

	got, err := MarshalResponsesRequest(req, &EncodeOptions{Model: "upstream-id"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"upstream-id","input":[{"type":"message","role":"user",` +
		`"content":[{"type":"input_text","text":"hello"}]}],"instructions":"be terse",` +
		`"max_output_tokens":64,"temperature":0.3,"text":{"format":{"type":"json_object"}},` +
		`"reasoning":{"effort":"medium"},"store":true}`
	if string(got) != want {
		t.Fatalf("request bytes\n got: %s\nwant: %s", got, want)
	}
}

// TestResponsesItemsRegroup is the conversion that is NOT a rename.
//
// A function_call is a sibling of the message before it here and a member of it
// in chat completions; a function_call_output is a sibling here and a message of
// its own there. Getting the regroup wrong produces a conversation the model
// reads as a different one.
func TestResponsesItemsRegroup(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Seoul\"}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"18C"},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"and tomorrow?"}]}]}`)
	req, err := DecodeResponsesRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages %d, want 4: %+v", len(req.Messages), req.Messages)
	}
	roles := []canonical.Role{
		canonical.RoleUser, canonical.RoleAssistant, canonical.RoleTool, canonical.RoleUser,
	}
	for i, want := range roles {
		if req.Messages[i].Role != want {
			t.Errorf("message %d role %q, want %q", i, req.Messages[i].Role, want)
		}
	}
	tu := req.Messages[1].Content[0]
	if tu.Kind != canonical.KindToolUse || tu.ToolUse.ID != "call_1" || tu.ToolUse.Name != "get_weather" {
		t.Fatalf("tool call %+v", tu)
	}
	// The argument object is kept RAW: a model that emitted a specific member
	// ordering on turn one must see the same bytes on turn two.
	if string(tu.ToolUse.Input) != `{"city":"Seoul"}` {
		t.Errorf("arguments %s", tu.ToolUse.Input)
	}
	tr := req.Messages[2].Content[0]
	if tr.Kind != canonical.KindToolResult || tr.ToolResult.ToolUseID != "call_1" {
		t.Fatalf("tool result %+v", tr)
	}

	// And back out again, flat.
	got, err := MarshalResponsesRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{
		`{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Seoul\"}"}`,
		`{"type":"function_call_output","call_id":"call_1","output":"18C"}`,
	} {
		if !strings.Contains(string(got), frag) {
			t.Errorf("the item sequence did not survive:\n got: %s\nwant a fragment: %s", got, frag)
		}
	}
}

func TestResponsesResponseGolden(t *testing.T) {
	upstream := []byte(`{"id":"resp_up","object":"response","created_at":1753660800,` +
		`"status":"completed","error":null,"incomplete_details":null,` +
		`"model":"upstream-id","output":[{"type":"message","id":"msg_1","status":"completed",` +
		`"role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],` +
		`"usage":{"input_tokens":36,"input_tokens_details":{"cached_tokens":30},` +
		`"output_tokens":87,"output_tokens_details":{"reasoning_tokens":40},"total_tokens":123}}`)
	resp, err := DecodeResponsesResponse(upstream, &DecodeOptions{Model: "qwen3.5:397b"})
	if err != nil {
		t.Fatal(err)
	}
	// DESIGN §10.7's mandatory normalization: input_tokens is INCLUSIVE of the
	// cached reads on this wire and stays inclusive in the neutral form. Reading
	// 36 as "36 uncached, plus 30 cached" is the inversion that bills every
	// cached request ~1.8x over with no error anywhere.
	if resp.Usage.InputTokens != 36 || resp.Usage.CacheReadTokens != 30 {
		t.Fatalf("usage %+v — input must remain inclusive", resp.Usage)
	}
	if resp.Usage.TotalTokens() != 123 {
		t.Errorf("total %d, want 123 — total is input+output, cache is a breakdown of input",
			resp.Usage.TotalTokens())
	}
	// §10.7 rule 2: reasoning tokens are already inside the output count.
	if resp.Usage.OutputTokens < resp.Usage.ReasoningTokens {
		t.Errorf("output %d < reasoning %d", resp.Usage.OutputTokens, resp.Usage.ReasoningTokens)
	}
	if resp.Choices[0].StopReason != canonical.StopEndTurn {
		t.Errorf("stop reason %q", resp.Choices[0].StopReason)
	}

	got, err := MarshalResponsesResponse(resp, &ResponsesOptions{
		ID: "resp_dorang", Model: "qwen3.5:397b", Store: ptr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"resp_dorang","object":"response","created_at":1753660800,"status":"completed",` +
		`"error":null,"incomplete_details":null,"instructions":null,"max_output_tokens":null,` +
		`"model":"qwen3.5:397b","output":[{"type":"message","status":"completed","role":"assistant",` +
		`"content":[{"type":"output_text","text":"hi","annotations":[]}]}],` +
		`"previous_response_id":null,"store":true,"temperature":null,"tools":[],"top_p":null,` +
		`"usage":{"input_tokens":36,"input_tokens_details":{"cached_tokens":30},"output_tokens":87,` +
		`"output_tokens_details":{"reasoning_tokens":40},"total_tokens":123}}`
	if string(got) != want {
		t.Fatalf("response bytes\n got: %s\nwant: %s", got, want)
	}
	// The SDK reads these unconditionally: a missing key is an AttributeError
	// where a null is None.
	for _, key := range []string{`"error":null`, `"incomplete_details":null`, `"previous_response_id":null`} {
		if !strings.Contains(string(got), key) {
			t.Errorf("%s was omitted rather than emitted as null", key)
		}
	}
}

// TestResponsesIncompleteStatus is the §10.7 mapping of StopReason onto
// status + incomplete_details.reason, in both directions.
func TestResponsesIncompleteStatus(t *testing.T) {
	upstream := []byte(`{"id":"r","object":"response","created_at":1,"status":"incomplete",` +
		`"incomplete_details":{"reason":"max_output_tokens"},"model":"m","output":[]}`)
	resp, err := DecodeResponsesResponse(upstream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].StopReason != canonical.StopMaxTokens {
		t.Fatalf("stop reason %q, want max_tokens", resp.Choices[0].StopReason)
	}
	got, err := MarshalResponsesResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"status":"incomplete"`) ||
		!strings.Contains(string(got), `"incomplete_details":{"reason":"max_output_tokens"}`) {
		t.Fatalf("the incomplete reason did not survive: %s", got)
	}
}

// TestResponsesToolUseStatus: a completed turn that issued a tool call is
// tool_use in the neutral enumeration, which is what an agentic client branches
// on after the crossing (COMPATIBILITY 4.4's concern, on this surface).
func TestResponsesToolUseStatus(t *testing.T) {
	upstream := []byte(`{"id":"r","object":"response","created_at":1,"status":"completed","model":"m",` +
		`"output":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`)
	resp, err := DecodeResponsesResponse(upstream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].StopReason != canonical.StopToolUse {
		t.Errorf("stop reason %q, want tool_use", resp.Choices[0].StopReason)
	}
}

// TestResponsesReasoningHandleIsReplayedVerbatim: the encrypted reasoning blob
// is integrity material. dorang stores and replays it and never synthesizes one
// (DESIGN §10.2).
func TestResponsesReasoningHandleIsReplayedVerbatim(t *testing.T) {
	const blob = "gAAAAABm-opaque-handle"
	body := []byte(`{"model":"m","input":[{"type":"reasoning","id":"rs_1",` +
		`"summary":[{"type":"summary_text","text":"thinking"}],` +
		`"encrypted_content":"` + blob + `"}]}`)
	req, err := DecodeResponsesRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	b := req.Messages[0].Content[0]
	if b.Kind != canonical.KindThinking || b.Thinking == nil || b.Thinking.Signature != blob {
		t.Fatalf("reasoning block %+v", b)
	}
	got, err := MarshalResponsesRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"encrypted_content":"`+blob+`"`) {
		t.Fatalf("the handle was not replayed byte-identically: %s", got)
	}
}

// TestResponsesItemsRoundTripThroughTheStore is what `responses_store` persists:
// a conversation, replayable as the history of the next request.
func TestResponsesItemsRoundTripThroughTheStore(t *testing.T) {
	msgs := []canonical.Message{
		canonical.TextMessage(canonical.RoleUser, "weather?"),
		{Role: canonical.RoleAssistant, Content: canonical.Content{
			canonical.ToolUseBlock("call_1", "get_weather", json.RawMessage(`{"city":"Seoul"}`)),
		}},
		{Role: canonical.RoleTool, Content: canonical.Content{
			canonical.ToolResultBlock("call_1", canonical.TextBlock("18C")),
		}},
	}
	enc, err := MarshalResponsesItems(msgs)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ResponsesItemsToMessages(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(msgs) {
		t.Fatalf("replayed %d messages, want %d: %s", len(back), len(msgs), enc)
	}
	if back[1].Content[0].ToolUse.ID != "call_1" || back[2].Content[0].ToolResult.ToolUseID != "call_1" {
		t.Errorf("the tool-call pairing did not survive the store: %s", enc)
	}
	if got := back[0].Content.Flatten(); got != "weather?" {
		t.Errorf("first turn %q", got)
	}
}

// TestResponsesCrossFamilyRequest: the neutral form is complete enough that a
// Responses request reaches a chat-completions or Anthropic-shaped deployment.
// That is the N+M claim of §10.1, tested rather than asserted.
func TestResponsesCrossFamilyRequest(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","input":"hi","instructions":"terse",` +
		`"max_output_tokens":8,"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := MarshalRequest(req, &EncodeOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	// instructions became a system message and the flat tool became a nested
	// one — both §10.7 rows.
	for _, frag := range []string{
		`"role":"system"`,
		`"tools":[{"type":"function","function":{"name":"f"`,
		`"max_tokens":8`,
	} {
		if !strings.Contains(string(got), frag) {
			t.Errorf("crossing into chat completions lost %s:\n%s", frag, got)
		}
	}
}
