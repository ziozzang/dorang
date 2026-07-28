package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// TestRequestByteRoundTrip: anthropic -> canonical -> anthropic, byte for byte.
//
// Every construct here is one this family owns and another loses: a structured
// system prompt with a cache breakpoint, a cache breakpoint on message content
// and on a tool, a signed thinking block, a mixed-block tool result, and an
// unmodelled top-level object (context_management) that must be relayed
// untouched.
func TestRequestByteRoundTrip(t *testing.T) {
	const body = `{"model":"claude-x","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig-abc"},` +
		`{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Seoul"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
		`{"type":"text","text":"sunny"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"cGl4"}}]}]}` +
		`],"max_tokens":1024,` +
		`"system":[{"type":"text","text":"be terse","cache_control":{"type":"ephemeral"}}],` +
		`"tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],` +
		`"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]}}`

	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := MarshalRequest(req, nil)
	if err != nil {
		t.Fatalf("MarshalRequest: %v", err)
	}
	if string(out) != body {
		t.Errorf("round trip changed the request\ngot:  %s\nwant: %s", out, body)
	}
}

// TestCanonicalRoundTripPreservesStructure is the direction the task names:
// canonical -> anthropic -> canonical, with multi-block content, a mixed-block
// tool result and cache breakpoints intact.
func TestCanonicalRoundTripPreservesStructure(t *testing.T) {
	in := &canonical.Request{
		Model:     "claude-x",
		MaxTokens: ptr(1024),
		System: canonical.Content{
			{Kind: canonical.KindText, Text: "You are terse.",
				CacheControl: &canonical.CacheControl{Type: "ephemeral", TTL: "1h"}},
			canonical.TextBlock("Second system block."),
		},
		Messages: []canonical.Message{
			{Role: canonical.RoleUser, Content: canonical.Content{
				canonical.TextBlock("look at this"),
				canonical.ImageBlock("image/png", "aGk="),
				canonical.DocumentBlock("application/pdf", "JVBER", "spec.pdf"),
			}},
			{Role: canonical.RoleAssistant, Content: canonical.Content{
				canonical.ThinkingBlock("hmm", "sig-abc"),
				canonical.TextBlock("calling a tool"),
				canonical.ToolUseBlock("toolu_1", "get_weather", json.RawMessage(`{"city":"Seoul"}`)),
			}},
			{Role: canonical.RoleTool, Content: canonical.Content{
				{Kind: canonical.KindToolResult,
					CacheControl: &canonical.CacheControl{Type: "ephemeral"},
					ToolResult: &canonical.ToolResult{
						ToolUseID: "toolu_1",
						Content: canonical.Content{
							canonical.TextBlock("sunny"),
							canonical.ImageBlock("image/png", "cGl4"),
						},
					}},
			}},
		},
		Tools: []canonical.Tool{{
			Type: "function", Name: "get_weather", Description: "d",
			Parameters:   json.RawMessage(`{"type":"object"}`),
			CacheControl: &canonical.CacheControl{Type: "ephemeral"},
		}},
		ToolChoice:  &canonical.ToolChoice{Mode: canonical.ToolChoiceAuto, DisableParallel: ptr(true)},
		Temperature: ptr(0.5),
		TopK:        ptr(5),
		TopP:        ptr(0.9),
		Stop:        []string{"STOP"},
		User:        "u-1",
		Extra: map[string]json.RawMessage{
			"context_management": json.RawMessage(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`),
		},
	}

	b, err := MarshalRequest(in, nil)
	if err != nil {
		t.Fatalf("MarshalRequest: %v", err)
	}
	got, err := DecodeRequest(b)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}

	// Scalars.
	if got.Model != in.Model || *got.MaxTokens != *in.MaxTokens ||
		*got.Temperature != *in.Temperature || *got.TopK != *in.TopK || *got.TopP != *in.TopP {
		t.Errorf("scalar mismatch: %+v", got)
	}
	if len(got.Stop) != 1 || got.Stop[0] != "STOP" {
		t.Errorf("stop = %v", got.Stop)
	}
	if got.User != "u-1" {
		t.Errorf("user = %q; metadata.user_id is where this family puts it (§10.7)", got.User)
	}

	// Structured system, with the breakpoint on the first block.
	if len(got.System) != 2 {
		t.Fatalf("system blocks = %d, want 2", len(got.System))
	}
	if got.System[0].CacheControl == nil || got.System[0].CacheControl.TTL != "1h" {
		t.Errorf("system cache breakpoint lost: %+v", got.System[0].CacheControl)
	}

	// Multi-block user content, images and documents.
	u := got.Messages[0]
	if len(u.Content) != 3 {
		t.Fatalf("user blocks = %d, want 3", len(u.Content))
	}
	if u.Content[1].Kind != canonical.KindImage || u.Content[1].Source.Data != "aGk=" {
		t.Errorf("image block lost: %+v", u.Content[1])
	}
	if u.Content[2].Kind != canonical.KindDocument || u.Content[2].Source.Name != "spec.pdf" {
		t.Errorf("document block lost: %+v", u.Content[2])
	}

	// The signed thinking block, byte-identical.
	a := got.Messages[1]
	if a.Content[0].Kind != canonical.KindThinking || a.Content[0].Thinking == nil ||
		a.Content[0].Thinking.Signature != "sig-abc" {
		t.Fatalf("thinking signature lost: %+v", a.Content[0])
	}
	if string(a.Content[2].ToolUse.Input) != `{"city":"Seoul"}` {
		t.Errorf("tool input reordered: %s", a.Content[2].ToolUse.Input)
	}

	// The tool result: this family puts it in a USER turn, so the role changes
	// and the block does not. That asymmetry is the whole reason
	// canonical.ToolResult holds blocks rather than a string.
	tr := got.Messages[2]
	if tr.Role != canonical.RoleUser {
		t.Errorf("tool result turn role = %q, want user", tr.Role)
	}
	if len(tr.Content) != 1 || tr.Content[0].ToolResult == nil {
		t.Fatalf("tool result lost: %+v", tr.Content)
	}
	if n := len(tr.Content[0].ToolResult.Content); n != 2 {
		t.Errorf("mixed-block tool result flattened to %d blocks", n)
	}
	if tr.Content[0].ToolResult.Content[1].Kind != canonical.KindImage {
		t.Error("the image inside the tool result was dropped")
	}
	if tr.Content[0].CacheControl == nil {
		t.Error("cache breakpoint on the tool result block was dropped")
	}

	// Tools and the inverted parallel flag.
	if len(got.Tools) != 1 || got.Tools[0].CacheControl == nil {
		t.Errorf("tool cache breakpoint lost: %+v", got.Tools)
	}
	if got.Tools[0].Type != "function" {
		t.Errorf("tool type = %q, want function", got.Tools[0].Type)
	}
	if got.ToolChoice == nil || got.ToolChoice.DisableParallel == nil || !*got.ToolChoice.DisableParallel {
		t.Errorf("disable_parallel_tool_use lost: %+v", got.ToolChoice)
	}
	if got.ParallelToolCalls == nil || *got.ParallelToolCalls {
		t.Errorf("parallel_tool_calls = %v, want false; the two spellings are inverses (§10.7)", got.ParallelToolCalls)
	}

	// Opaque state, byte-identical.
	if string(got.Extra["context_management"]) != `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}` {
		t.Errorf("context_management was rewritten: %s", got.Extra["context_management"])
	}
}

// TestParallelToolCallsIsInverted is DESIGN §10.7's second named trap, tested on
// its own because a naive copy passes every structural assertion above.
func TestParallelToolCallsIsInverted(t *testing.T) {
	for _, tc := range []struct {
		parallel bool
		want     bool // disable_parallel_tool_use
	}{{true, false}, {false, true}} {
		req := &canonical.Request{
			Model: "m", MaxTokens: ptr(16),
			ParallelToolCalls: ptr(tc.parallel),
			Tools:             []canonical.Tool{{Type: "function", Name: "f"}},
		}
		w, err := EncodeRequest(req, nil)
		if err != nil {
			t.Fatalf("EncodeRequest: %v", err)
		}
		if w.ToolChoice == nil || w.ToolChoice.DisableParallelToolUse == nil {
			t.Fatalf("parallel_tool_calls=%v produced no disable_parallel_tool_use", tc.parallel)
		}
		if got := *w.ToolChoice.DisableParallelToolUse; got != tc.want {
			t.Errorf("parallel_tool_calls=%v -> disable_parallel_tool_use=%v, want %v", tc.parallel, got, tc.want)
		}
	}
}

// TestMaxTokensRequired is DESIGN §10.7's first named trap. Omitting the field
// is a 400 from the backend; inventing a constant silently caps the caller's
// output. The only correct answer is the catalog value, so the encoder demands
// one.
func TestMaxTokensRequired(t *testing.T) {
	req := &canonical.Request{Model: "m", Messages: []canonical.Message{
		canonical.TextMessage(canonical.RoleUser, "hi"),
	}}
	_, err := EncodeRequest(req, nil)
	var oe *OpaqueError
	if !asOpaque(err, &oe) || oe.Reason != ReasonMaxTokensUnknown {
		t.Fatalf("EncodeRequest with no ceiling: err = %v, want %s", err, ReasonMaxTokensUnknown)
	}

	w, err := EncodeRequest(req, &EncodeOptions{DefaultMaxTokens: 8192})
	if err != nil {
		t.Fatalf("EncodeRequest with a catalog value: %v", err)
	}
	if w.MaxTokens == nil || *w.MaxTokens != 8192 {
		t.Errorf("max_tokens = %v, want the catalog value", w.MaxTokens)
	}

	// The caller's own ceiling always wins over the catalog's.
	req.MaxTokens = ptr(64)
	w, err = EncodeRequest(req, &EncodeOptions{DefaultMaxTokens: 8192})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if *w.MaxTokens != 64 {
		t.Errorf("max_tokens = %d, want the caller's 64", *w.MaxTokens)
	}
}

// TestStructuralDowngradesAreReported: a target that cannot express a construct
// gets a located report, not a silent 200. DESIGN §10.1.
func TestStructuralDowngradesAreReported(t *testing.T) {
	req := &canonical.Request{
		Model: "m", MaxTokens: ptr(16),
		Messages: []canonical.Message{{Role: canonical.RoleUser, Content: canonical.Content{
			canonical.TextBlock("see"),
			canonical.DocumentBlock("application/pdf", "JVBER", "spec.pdf"),
		}}},
		ResponseFormat: &canonical.ResponseFormat{Kind: canonical.FormatJSONSchema, Name: "s"},
	}
	var loss canonical.LossReport
	// A deliberately narrow target: no documents, no schema.
	caps := DefaultCapabilities &^ canonical.CapDocumentBlocks
	if _, err := EncodeRequest(req, &EncodeOptions{Capabilities: caps, Loss: &loss}); err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !loss.HasStructural() {
		t.Fatal("dropping a PDF was not reported; DESIGN §10.1 calls a silent 200 worse than an error")
	}
	want := map[string]bool{
		canonical.ConstructDocumentBlock: false,
		canonical.ConstructJSONSchema:    false,
	}
	for _, d := range loss.Downgrades {
		if _, ok := want[d.Construct]; ok {
			want[d.Construct] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("downgrade %s not reported: %+v", k, loss.Downgrades)
		}
	}
	for _, d := range loss.Downgrades {
		if d.Construct == canonical.ConstructDocumentBlock && d.Detail == "" {
			t.Error(`"a document was dropped" is not actionable; the detail must locate it`)
		}
	}
}

// TestConsecutiveTurnsAreMerged. An OpenAI-shaped conversation has one tool
// message per result; this family rejects consecutive same-role turns outright,
// with an error naming nothing the caller wrote.
func TestConsecutiveTurnsAreMerged(t *testing.T) {
	req := &canonical.Request{
		Model: "m", MaxTokens: ptr(16),
		Messages: []canonical.Message{
			canonical.TextMessage(canonical.RoleUser, "go"),
			{Role: canonical.RoleAssistant, Content: canonical.Content{
				canonical.ToolUseBlock("t1", "f", json.RawMessage(`{}`)),
				canonical.ToolUseBlock("t2", "g", json.RawMessage(`{}`)),
			}},
			{Role: canonical.RoleTool, Content: canonical.Content{
				canonical.ToolResultBlock("t1", canonical.TextBlock("a")),
			}},
			{Role: canonical.RoleTool, Content: canonical.Content{
				canonical.ToolResultBlock("t2", canonical.TextBlock("b")),
			}},
			canonical.TextMessage(canonical.RoleUser, "thanks"),
		},
	}
	w, err := EncodeRequest(req, nil)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	roles := make([]string, len(w.Messages))
	for i := range w.Messages {
		roles[i] = w.Messages[i].Role
	}
	if len(roles) != 3 || roles[0] != "user" || roles[1] != "assistant" || roles[2] != "user" {
		t.Fatalf("roles = %v, want [user assistant user]", roles)
	}
	last := w.Messages[2].Content.Blocks
	if len(last) != 3 {
		t.Fatalf("merged turn has %d blocks, want 3 (two results plus the text)", len(last))
	}
	if last[0].Type != BlockToolResult || last[1].Type != BlockToolResult || last[2].Type != BlockText {
		t.Errorf("merge reordered the turn: %+v", last)
	}
}

// TestSystemMessagesFoldIntoSystemField: there is no system role here.
func TestSystemMessagesFoldIntoSystemField(t *testing.T) {
	req := &canonical.Request{
		Model: "m", MaxTokens: ptr(16),
		System: canonical.Content{canonical.TextBlock("first")},
		Messages: []canonical.Message{
			canonical.TextMessage(canonical.RoleSystem, "second"),
			canonical.TextMessage(canonical.RoleUser, "hi"),
		},
	}
	w, err := EncodeRequest(req, nil)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if len(w.Messages) != 1 || w.Messages[0].Role != "user" {
		t.Fatalf("system message leaked into messages: %+v", w.Messages)
	}
	if w.System == nil || len(w.System.Blocks) != 2 {
		t.Fatalf("system = %+v, want two blocks", w.System)
	}
}

// TestUnknownBlockTypeSurvives: not recognizing something is not a reason to
// remove it (DESIGN §10.5a).
func TestUnknownBlockTypeSurvives(t *testing.T) {
	const body = `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"search_result","source":"https://example.invalid","title":"t"}]}],"max_tokens":16}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := MarshalRequest(req, nil)
	if err != nil {
		t.Fatalf("MarshalRequest: %v", err)
	}
	if string(out) != body {
		t.Errorf("unknown block type was rewritten\ngot:  %s\nwant: %s", out, body)
	}
}

// TestThinkingBudgetIsAFunctionOfTheRequest. DESIGN §10.2: the budget stays
// strictly below the output ceiling, and dorang never raises the ceiling to fit
// the table.
func TestThinkingBudgetIsAFunctionOfTheRequest(t *testing.T) {
	cases := []struct {
		name      string
		maxTokens int
		effort    string
		want      int  // 0 means "reasoning omitted"
		disabled  bool // thinking:{type:disabled}
	}{
		{"high fits", 32000, canonical.EffortHigh, 16384, false},
		{"high clamped by a small ceiling", 4096, canonical.EffortHigh, 4095, false},
		{"ceiling too small for any budget", 512, canonical.EffortHigh, 0, false},
		{"explicitly off", 32000, canonical.EffortNone, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var loss canonical.LossReport
			req := &canonical.Request{
				Model: "m", MaxTokens: ptr(tc.maxTokens),
				Reasoning: &canonical.Reasoning{Effort: tc.effort, Enabled: ptr(tc.effort != canonical.EffortNone)},
			}
			w, err := EncodeRequest(req, &EncodeOptions{Loss: &loss})
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			if *w.MaxTokens != tc.maxTokens {
				t.Fatalf("max_tokens was changed to %d; the ceiling is the caller's", *w.MaxTokens)
			}
			switch {
			case tc.disabled:
				if w.Thinking == nil || w.Thinking.Type != ThinkingDisabled {
					t.Errorf("thinking = %+v, want disabled", w.Thinking)
				}
			case tc.want == 0:
				if w.Thinking != nil {
					t.Errorf("thinking = %+v, want omitted", w.Thinking)
				}
				if len(loss.Dropped) == 0 {
					t.Error("reasoning was dropped without being reported")
				}
			default:
				if w.Thinking == nil || w.Thinking.BudgetTokens == nil || *w.Thinking.BudgetTokens != tc.want {
					t.Errorf("budget = %+v, want %d", w.Thinking, tc.want)
				}
			}
		})
	}
}

// TestResponseDecodeIsTheBackendDirection: the same package must read what a
// deployment of this family sends, including the stop reasons the adapter path
// would have collapsed. Decoding must recover everything the wire carried —
// the collapse is an ENCODE rule, and applying it here would destroy the
// information a second hop still needs (COMPATIBILITY 4.3).
func TestResponseDecodeIsTheBackendDirection(t *testing.T) {
	const body = `{"id":"msg_9","type":"message","role":"assistant","model":"claude-x","content":[` +
		`{"type":"thinking","thinking":"hmm","signature":"sig"},` +
		`{"type":"text","text":"here"},` +
		`{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}],` +
		`"stop_reason":"refusal","stop_sequence":null,` +
		`"usage":{"input_tokens":50,"cache_creation_input_tokens":150,"cache_read_input_tokens":800,"output_tokens":20}}`

	got, err := DecodeResponse([]byte(body), nil)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d", len(got.Choices))
	}
	c := got.Choices[0]
	if c.StopReason != canonical.StopRefusal {
		t.Errorf("stop reason = %q, want refusal; decoding must not apply 6.4's collapse", c.StopReason)
	}
	if len(c.Message.Content) != 3 {
		t.Fatalf("blocks = %d, want 3", len(c.Message.Content))
	}
	if c.Message.Content[0].Thinking.Signature != "sig" {
		t.Error("signature lost on decode")
	}
	want := canonical.Usage{InputTokens: 1000, OutputTokens: 20, CacheReadTokens: 800, CacheWriteTokens: 150}
	if *got.Usage != want {
		t.Errorf("usage = %+v, want %+v", *got.Usage, want)
	}

	// The model reported to the caller is the one they asked for, never the
	// upstream id (DESIGN §7.2).
	got, err = DecodeResponse([]byte(body), &DecodeOptions{Model: "alias-name"})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got.Model != "alias-name" {
		t.Errorf("model = %q, want the client-facing alias", got.Model)
	}
}

func asOpaque(err error, out **OpaqueError) bool {
	oe, ok := err.(*OpaqueError)
	if ok {
		*out = oe
	}
	return ok
}
