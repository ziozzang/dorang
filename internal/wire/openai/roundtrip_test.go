package openai

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// roundTrip encodes a neutral request to OpenAI bytes and decodes it back.
func roundTrip(t *testing.T, req *canonical.Request, opt *EncodeOptions) *canonical.Request {
	t.Helper()
	b, err := MarshalRequest(req, opt)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := DecodeRequest(b)
	if err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return back
}

// TestRoundTripMultiBlockContent is the reason canonical.Message.Content is a
// list and not a string.
func TestRoundTripMultiBlockContent(t *testing.T) {
	req := &canonical.Request{
		Model: "deepseek-v4-flash:cloud",
		Messages: []canonical.Message{{
			Role: canonical.RoleUser,
			Content: canonical.Content{
				canonical.TextBlock("what is in this picture?"),
				canonical.ImageBlock("image/png", "aGVsbG8="),
				canonical.TextBlock("be brief"),
			},
		}},
	}
	back := roundTrip(t, req, nil)
	if len(back.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(back.Messages))
	}
	got := back.Messages[0].Content
	if len(got) != 3 {
		t.Fatalf("got %d blocks, want 3: %+v", len(got), got)
	}
	if got[0].Kind != canonical.KindText || got[0].Text != "what is in this picture?" {
		t.Errorf("block 0 = %+v", got[0])
	}
	if got[1].Kind != canonical.KindImage {
		t.Fatalf("block 1 kind = %q, want image", got[1].Kind)
	}
	if got[1].Source.MediaType != "image/png" || got[1].Source.Data != "aGVsbG8=" {
		t.Errorf("image source not preserved: %+v", got[1].Source)
	}
	if got[2].Kind != canonical.KindText || got[2].Text != "be brief" {
		t.Errorf("block 2 = %+v", got[2])
	}
	// Ordering is content, not decoration: reordering blocks changes meaning.
	if got[0].Text == got[2].Text {
		t.Error("block order was not preserved")
	}
}

// TestRoundTripMixedToolResult is the reason ToolResult carries blocks.
func TestRoundTripMixedToolResult(t *testing.T) {
	req := &canonical.Request{
		Model: "zai:glm-5.1",
		Messages: []canonical.Message{{
			Role: canonical.RoleTool,
			Content: canonical.Content{canonical.ToolResultBlock("call_7",
				canonical.TextBlock("here is the chart"),
				canonical.ImageBlock("image/jpeg", "/9j/4AAQ"),
			)},
		}},
	}
	back := roundTrip(t, req, nil)
	if len(back.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(back.Messages))
	}
	m := back.Messages[0]
	if m.Role != canonical.RoleTool {
		t.Fatalf("role = %q", m.Role)
	}
	if len(m.Content) != 1 || m.Content[0].Kind != canonical.KindToolResult {
		t.Fatalf("content = %+v", m.Content)
	}
	tr := m.Content[0].ToolResult
	if tr.ToolUseID != "call_7" {
		t.Errorf("tool_use id = %q", tr.ToolUseID)
	}
	if len(tr.Content) != 2 {
		t.Fatalf("tool result has %d blocks, want 2 — the image was destroyed", len(tr.Content))
	}
	if tr.Content[1].Kind != canonical.KindImage || tr.Content[1].Source.MediaType != "image/jpeg" {
		t.Errorf("tool result image not preserved: %+v", tr.Content[1])
	}
}

// TestRoundTripCacheBreakpoints covers the construct that is invisible when
// lost: caching topology, and therefore cost.
func TestRoundTripCacheBreakpoints(t *testing.T) {
	bp := &canonical.CacheControl{Type: "ephemeral", TTL: "1h"}
	sys := canonical.TextBlock(strings.Repeat("policy ", 8))
	sys.CacheControl = bp
	req := &canonical.Request{
		Model:  "gemma4:31b",
		System: canonical.Content{sys},
		Messages: []canonical.Message{{
			Role: canonical.RoleUser,
			Content: canonical.Content{
				func() canonical.Block {
					b := canonical.TextBlock("long document")
					b.CacheControl = &canonical.CacheControl{Type: "ephemeral"}
					return b
				}(),
				canonical.TextBlock("question"),
			},
		}},
		Tools: []canonical.Tool{{
			Name:         "search",
			Parameters:   json.RawMessage(`{"type":"object"}`),
			CacheControl: &canonical.CacheControl{Type: "ephemeral"},
		}},
	}
	back := roundTrip(t, req, nil)

	// The system prompt became message 0.
	if len(back.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(back.Messages))
	}
	sysCC := back.Messages[0].Content[0].CacheControl
	if sysCC == nil || sysCC.Type != "ephemeral" || sysCC.TTL != "1h" {
		t.Errorf("system breakpoint lost: %+v", sysCC)
	}
	userCC := back.Messages[1].Content[0].CacheControl
	if userCC == nil || userCC.Type != "ephemeral" {
		t.Errorf("user breakpoint lost: %+v", userCC)
	}
	if back.Messages[1].Content[1].CacheControl != nil {
		t.Error("a breakpoint appeared on a block that had none")
	}
	if len(back.Tools) != 1 || back.Tools[0].CacheControl == nil {
		t.Errorf("tool breakpoint lost: %+v", back.Tools)
	}
	if !back.RequiredCapabilities().Has(canonical.CapCacheBreakpoints) {
		t.Error("round-tripped request no longer reports CapCacheBreakpoints")
	}
}

// TestStrictTargetReportsStructuralDowngrades is the §10.1 behavior: when the
// target cannot express a construct, the loss is named and located, not
// swallowed behind a 200.
func TestStrictTargetReportsStructuralDowngrades(t *testing.T) {
	req := &canonical.Request{
		Model: "qwen3.5:397b",
		Messages: []canonical.Message{
			{Role: canonical.RoleUser, Content: canonical.Content{
				canonical.TextBlock("read this"),
				canonical.DocumentBlock("application/pdf", "JVBERi0=", "spec.pdf"),
			}},
			{Role: canonical.RoleTool, Content: canonical.Content{
				canonical.ToolResultBlock("call_1",
					canonical.TextBlock("ok"),
					canonical.ImageBlock("image/png", "iVBOR"),
				),
			}},
		},
		Seed:     ptr(int64(7)),
		TopK:     ptr(40),
		Priority: ptr(3),
	}
	var loss canonical.LossReport
	opt := &EncodeOptions{Capabilities: StrictCapabilities, Loss: &loss}
	if _, err := EncodeRequest(req, opt); err != nil {
		t.Fatal(err)
	}

	if !loss.HasStructural() {
		t.Fatal("a PDF and an image inside a tool result were dropped with no structural report")
	}
	constructs := loss.Constructs()
	wantConstructs := []string{canonical.ConstructDocumentBlock, canonical.ConstructMultiBlockToolResult}
	for _, w := range wantConstructs {
		if !contains(constructs, w) {
			t.Errorf("missing structural downgrade %q; got %v", w, constructs)
		}
	}
	// The detail must locate the instance, not just name the class.
	found := false
	for _, d := range loss.Downgrades {
		if d.Construct == canonical.ConstructDocumentBlock && strings.Contains(d.Detail, "application/pdf") {
			found = true
		}
	}
	if !found {
		t.Errorf("document downgrade did not locate the instance: %+v", loss.Downgrades)
	}

	// Droppable parameters are reported by name and are NOT structural.
	if !contains(loss.Dropped, "top_k") || !contains(loss.Dropped, "priority") {
		t.Errorf("droppable params not reported: %v", loss.Dropped)
	}
	if contains(loss.Dropped, "seed") {
		t.Errorf("seed was reported dropped but strict OpenAI has it: %v", loss.Dropped)
	}
	for _, d := range loss.Downgrades {
		if d.Construct == "top_k" || d.Construct == "priority" {
			t.Errorf("a droppable parameter was reported as a structural downgrade: %+v", d)
		}
	}
}

// TestDefaultTargetIsLossless checks that the same request crosses the default
// capability set with no structural loss, so the 400-on-loss rule does not fire
// on constructs dorang can in fact carry.
func TestDefaultTargetIsLossless(t *testing.T) {
	req := &canonical.Request{
		Model: "qwen3.5:397b",
		Messages: []canonical.Message{
			{Role: canonical.RoleUser, Content: canonical.Content{
				canonical.TextBlock("read this"),
				canonical.DocumentBlock("application/pdf", "JVBERi0=", "spec.pdf"),
			}},
			{Role: canonical.RoleTool, Content: canonical.Content{
				canonical.ToolResultBlock("call_1",
					canonical.TextBlock("ok"),
					canonical.ImageBlock("image/png", "iVBOR"),
				),
			}},
		},
	}
	var loss canonical.LossReport
	if _, err := EncodeRequest(req, &EncodeOptions{Loss: &loss}); err != nil {
		t.Fatal(err)
	}
	if loss.HasStructural() {
		t.Fatalf("default capability set reported a structural loss it does not have: %+v", loss.Downgrades)
	}

	back := roundTrip(t, req, nil)
	doc := back.Messages[0].Content[1]
	if doc.Kind != canonical.KindDocument || doc.Source.MediaType != "application/pdf" || doc.Source.Name != "spec.pdf" {
		t.Errorf("document not round-tripped: %+v", doc)
	}
}

// TestToolCallIndexStrippedFromInboundMessages covers COMPATIBILITY 5.2.
func TestToolCallIndexStrippedFromInboundMessages(t *testing.T) {
	// A client echoing back a full assistant message it read off a stream.
	body := []byte(`{"model":"gemma4:31b","messages":[
		{"role":"assistant","content":null,"tool_calls":[
			{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"42"}]}`)

	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := MarshalRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"index"`) {
		t.Fatalf("index survived into the forwarded request: %s", out)
	}

	// The pass-through helper does the same without a canonical crossing.
	var w Request
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatal(err)
	}
	if w.Messages[0].ToolCalls[0].Index == nil {
		t.Fatal("inbound index was not decoded, so the strip is untested")
	}
	StripToolCallIndexes(w.Messages)
	out2, err := Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out2), `"index"`) {
		t.Fatalf("StripToolCallIndexes left an index behind: %s", out2)
	}
}

// TestOpaqueModelNames freezes DESIGN §2.1 as a golden test: a model name is
// never split on ':' or '/'.
func TestOpaqueModelNames(t *testing.T) {
	for _, name := range append(opaqueModels, "openai/gpt-4o", "a:b:c/d:e") {
		req := &canonical.Request{Model: name, Messages: []canonical.Message{
			canonical.TextMessage(canonical.RoleUser, "hi"),
		}}
		b, err := MarshalRequest(req, nil)
		if err != nil {
			t.Fatal(err)
		}
		back, err := DecodeRequest(b)
		if err != nil {
			t.Fatal(err)
		}
		if back.Model != name {
			t.Errorf("model %q became %q", name, back.Model)
		}

		var buf strings.Builder
		s := NewStreamWriter(&buf, StreamConfig{ID: testID, Created: testCreated, Model: name})
		if err := s.WriteChunk(TextChunk("x")); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), `"model":"`+name+`"`) {
			t.Errorf("model %q was mangled in a chunk: %s", name, buf.String())
		}
	}
}

// TestExtraFieldsPassThrough covers the pass-through half of DESIGN §10.3: an
// unmodeled field is carried, not filtered.
func TestExtraFieldsPassThrough(t *testing.T) {
	body := []byte(`{"model":"gemma4:31b","messages":[{"role":"user","content":"hi","x_client":"v2"}],"x_vendor_knob":{"a":1}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := req.Extra["x_vendor_knob"]; !ok {
		t.Fatalf("request extra lost: %+v", req.Extra)
	}
	if _, ok := req.Messages[0].Extra["x_client"]; !ok {
		t.Fatalf("message extra lost: %+v", req.Messages[0].Extra)
	}
	out, err := MarshalRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"x_vendor_knob":{"a":1}`, `"x_client":"v2"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("re-encode dropped %s: %s", want, out)
		}
	}
}

// TestStreamOptionsRoundTrip guards COMPATIBILITY 3.1's "exactly true".
func TestStreamOptionsRoundTrip(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"model":"m","messages":[],"stream":true}`, false},
		{`{"model":"m","messages":[],"stream":true,"stream_options":{}}`, false},
		{`{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":false}}`, false},
		{`{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":true}}`, true},
	}
	for _, c := range cases {
		req, err := DecodeRequest([]byte(c.body))
		if err != nil {
			t.Fatal(err)
		}
		if got := req.IncludeUsage(); got != c.want {
			t.Errorf("%s: IncludeUsage() = %v, want %v", c.body, got, c.want)
		}
	}
}

// TestChunkDecodeToEvents covers the backend direction.
func TestChunkDecodeToEvents(t *testing.T) {
	raw := []byte(`{"id":"x","object":"chat.completion.chunk","created":1,"model":"upstream-real",` +
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"eos_token"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	var warned []Warning
	_, events, err := DecodeChunk(raw, &DecodeOptions{
		Model: "gemma4:31b",
		Warn:  func(w Warning) { warned = append(warned, w) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want delta+stop+usage: %+v", len(events), events)
	}
	if events[0].Model != "gemma4:31b" {
		t.Errorf("event model = %q, want the client-facing name", events[0].Model)
	}
	if events[1].Delta.StopReason != canonical.StopEndTurn {
		t.Errorf("stop reason = %q", events[1].Delta.StopReason)
	}
	if events[1].Delta.NativeStopReason != "" && events[1].Delta.NativeStopReason != "eos_token" {
		t.Errorf("native stop reason = %q", events[1].Delta.NativeStopReason)
	}
	if events[2].Usage.InputTokens != 3 {
		t.Errorf("usage = %+v", events[2].Usage)
	}
	if len(warned) != 0 {
		t.Errorf("eos_token is in the table and must not warn: %+v", warned)
	}
}

// TestReasoningRoundTrip checks the neutral reasoning control survives.
func TestReasoningRoundTrip(t *testing.T) {
	req := &canonical.Request{
		Model:     "zai:glm-5.1",
		Messages:  []canonical.Message{canonical.TextMessage(canonical.RoleUser, "think")},
		Reasoning: &canonical.Reasoning{Effort: canonical.EffortHigh, Summary: canonical.SummaryConcise, BudgetTokens: 4096},
	}
	back := roundTrip(t, req, nil)
	if back.Reasoning == nil {
		t.Fatal("reasoning lost")
	}
	if back.Reasoning.Effort != canonical.EffortHigh ||
		back.Reasoning.Summary != canonical.SummaryConcise ||
		back.Reasoning.BudgetTokens != 4096 {
		t.Fatalf("reasoning = %+v", back.Reasoning)
	}

	var loss canonical.LossReport
	if _, err := EncodeRequest(req, &EncodeOptions{
		Capabilities: StrictCapabilities &^ canonical.CapReasoningControl,
		Loss:         &loss,
	}); err != nil {
		t.Fatal(err)
	}
	if !contains(loss.Dropped, "reasoning") {
		t.Errorf("unsupported reasoning control was dropped silently: %v", loss.Dropped)
	}
	if loss.HasStructural() {
		t.Errorf("a missing reasoning knob is droppable, not structural: %+v", loss.Downgrades)
	}
}

// TestResponseFormatRoundTrip covers the json_schema construct.
func TestResponseFormatRoundTrip(t *testing.T) {
	req := &canonical.Request{
		Model:    "gemma4:31b",
		Messages: []canonical.Message{canonical.TextMessage(canonical.RoleUser, "x")},
		ResponseFormat: &canonical.ResponseFormat{
			Kind:   canonical.FormatJSONSchema,
			Name:   "answer",
			Schema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
			Strict: ptr(true),
		},
	}
	back := roundTrip(t, req, nil)
	if back.ResponseFormat == nil || back.ResponseFormat.Kind != canonical.FormatJSONSchema {
		t.Fatalf("response format = %+v", back.ResponseFormat)
	}
	if !reflect.DeepEqual([]byte(back.ResponseFormat.Schema), []byte(req.ResponseFormat.Schema)) {
		t.Errorf("schema bytes changed: %s", back.ResponseFormat.Schema)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
