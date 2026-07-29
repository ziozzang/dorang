package canonical

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequiredCapabilitiesReportsOnlyWhatIsUsed(t *testing.T) {
	// A plain request uses nothing a smaller protocol cannot express, so the
	// set must be empty — otherwise capability routing would filter on noise.
	plain := &Request{
		Model:       "gemma4:31b",
		Messages:    []Message{TextMessage(RoleUser, "hello")},
		Temperature: ptr(0.7),
		MaxTokens:   ptr(100),
		Stream:      true,
	}
	if got := plain.RequiredCapabilities(); got != 0 {
		t.Fatalf("a plain request requires %v, want none", got)
	}
}

func TestRequiredCapabilitiesPerConstruct(t *testing.T) {
	cases := []struct {
		name string
		req  *Request
		want Capability
	}{
		{
			"multi-block content",
			&Request{Messages: []Message{{Role: RoleUser, Content: Content{
				TextBlock("a"), TextBlock("b"),
			}}}},
			CapMultiBlockContent,
		},
		{
			"a single image still needs the array form",
			&Request{Messages: []Message{{Role: RoleUser, Content: Content{
				ImageBlock("image/png", "x"),
			}}}},
			CapMultiBlockContent | CapImageBlocks,
		},
		{
			"document",
			&Request{Messages: []Message{{Role: RoleUser, Content: Content{
				TextBlock("read"), DocumentBlock("application/pdf", "x", "a.pdf"),
			}}}},
			CapMultiBlockContent | CapDocumentBlocks,
		},
		{
			"cache breakpoint on an otherwise plain message",
			&Request{Messages: []Message{{Role: RoleUser, Content: Content{
				func() Block { b := TextBlock("a"); b.CacheControl = &CacheControl{Type: "ephemeral"}; return b }(),
			}}}},
			CapCacheBreakpoints | CapMultiBlockContent,
		},
		{
			"single-text tool result is not multi-block",
			&Request{Messages: []Message{{Role: RoleTool, Content: Content{
				ToolResultBlock("c1", TextBlock("42")),
			}}}},
			CapToolCalls,
		},
		{
			"tool result with text plus an image",
			&Request{Messages: []Message{{Role: RoleTool, Content: Content{
				ToolResultBlock("c1", TextBlock("42"), ImageBlock("image/png", "x")),
			}}}},
			CapToolCalls | CapMultiBlockToolResult | CapImageBlocks,
		},
		{
			"tool_use blocks do not force the array content form",
			&Request{Messages: []Message{{Role: RoleAssistant, Content: Content{
				TextBlock("calling"), ToolUseBlock("c1", "f", json.RawMessage(`{}`)),
			}}}},
			CapToolCalls,
		},
		{
			"thinking block",
			&Request{Messages: []Message{{Role: RoleAssistant, Content: Content{
				ThinkingBlock("hmm", "sig"), TextBlock("answer"),
			}}}},
			CapThinkingBlocks | CapMultiBlockContent,
		},
		{
			"structured system",
			&Request{System: Content{TextBlock("a"), TextBlock("b")}},
			CapStructuredSystem,
		},
		{
			"plain system is not structured",
			&Request{System: Content{TextBlock("a")}},
			0,
		},
		{
			"droppable knobs",
			&Request{Seed: ptr(int64(1)), TopK: ptr(5), Priority: ptr(2),
				LogitBias: map[string]float64{"a": 1}, User: "u"},
			CapSeed | CapTopK | CapPriority | CapLogitBias | CapUser,
		},
		{
			"material parameters",
			&Request{Stop: []string{"x"}, N: ptr(4), Logprobs: ptr(true), ServiceTier: "flex"},
			CapStopSequences | CapMultipleChoices | CapLogprobs | CapServiceTier,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.req.RequiredCapabilities(); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestStructuralAndDroppableAreDisjoint is the invariant behind DESIGN §10.1's
// two-kinds-of-loss split: a capability is one or the other, never both and
// never neither.
//
// [Material] is a sub-mask of [Structural] rather than a third policy: the
// question "does dorang refuse this" still has two answers. What it names is
// how the refusal is REPORTED — by parameter rather than located — and that
// distinction has to hold, because a material construct with no parameter name
// would produce a 400 the caller cannot act on.
func TestStructuralAndDroppableAreDisjoint(t *testing.T) {
	if Structural&Droppable != 0 {
		t.Fatalf("overlap: %v", Structural&Droppable)
	}
	for _, n := range capNames {
		if Structural&n.bit == 0 && Droppable&n.bit == 0 {
			t.Errorf("%q is in neither mask", n.construct)
		}
	}
	// Every droppable capability names at least one wire parameter, because
	// x-dorang-dropped-params is a list of parameter names and an entry with
	// nothing to say is useless to the caller.
	for _, n := range capNames {
		if Droppable&n.bit != 0 && len(n.params) == 0 {
			t.Errorf("droppable %q names no parameter", n.construct)
		}
	}
	if Material&^Structural != 0 {
		t.Errorf("material bits outside the refusal mask would be dropped silently: %v", Material&^Structural)
	}
	if Material&Droppable != 0 {
		t.Errorf("a capability cannot be both refused and dropped: %v", Material&Droppable)
	}
	// A material refusal names the wire parameter, and its construct id is that
	// parameter — which is what lets a 400 body set `param` at all, and what the
	// located half has no equivalent of.
	for _, n := range capNames {
		if Material&n.bit == 0 {
			continue
		}
		if len(n.params) == 0 {
			t.Errorf("material %q names no parameter, so its 400 cannot locate anything", n.construct)
		}
		if !containsString(n.params, n.construct) {
			t.Errorf("material %q is not among its own parameters %v; the construct id a caller "+
				"puts in x-dorang-allow-lossy should be the field they wrote", n.construct, n.params)
		}
	}
}

// TestMaterialCapabilitiesAreClaimedOnlyWhenTheyChangeSomething is the cost side
// of the reclassification, and the reason it is not a blanket refusal on the
// field being present.
//
// `n: 1` and `service_tier: auto` are the values that mean "do the default".
// Dropping them changes nothing, so raising a bit for them would refuse
// requests dorang serves correctly — which is how a safety mechanism turns into
// noise and callers learn to set x-dorang-allow-lossy on everything.
func TestMaterialCapabilitiesAreClaimedOnlyWhenTheyChangeSomething(t *testing.T) {
	cases := []struct {
		name string
		req  *Request
		want Capability
	}{
		{"n: 1 is the default written down", &Request{N: ptr(1)}, 0},
		{"n: 2 is a claim on the response shape", &Request{N: ptr(2)}, CapMultipleChoices},
		{"service_tier: auto delegates, exactly as omitting it does",
			&Request{ServiceTier: ServiceTierAuto}, 0},
		{"casing does not turn a delegation into a refusal", &Request{ServiceTier: "Auto"}, 0},
		{"a named band is a price the caller chose", &Request{ServiceTier: "flex"}, CapServiceTier},
		{"service_tier: default forces standard pricing, which is a choice",
			&Request{ServiceTier: "default"}, CapServiceTier},
		{"logprobs asks for a member of the response", &Request{Logprobs: ptr(true)}, CapLogprobs},
		{"top_logprobs alone asks for it too", &Request{TopLogprobs: ptr(5)}, CapLogprobs},
		{"an empty stop list states no terminator", &Request{Stop: nil}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.req.RequiredCapabilities(); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestMaterialLossCarriesTheValueTheCallerWrote: a downgrade exists to be acted
// on, and "service_tier was dropped" is not actionable in the way
// "service_tier: flex" is. The located half of the mask has an index to point
// at; this half has the value.
func TestMaterialLossCarriesTheValueTheCallerWrote(t *testing.T) {
	req := &Request{
		Messages:    []Message{TextMessage(RoleUser, "hi")},
		Stop:        []string{"a", "b"},
		N:           ptr(4),
		TopLogprobs: ptr(5),
		ServiceTier: "flex",
	}
	// A target with none of the four — the Messages family, plus a deployment
	// without stop sequences.
	ds := req.Downgrades(0)
	byConstruct := map[string]string{}
	for _, d := range ds {
		byConstruct[d.Construct] = d.Detail
	}
	for construct, want := range map[string]string{
		ConstructStopSequences:   "2",
		ConstructMultipleChoices: "4",
		ConstructLogprobs:        "5",
		ConstructServiceTier:     "flex",
	} {
		detail, ok := byConstruct[construct]
		if !ok {
			t.Errorf("%q is not reported as a downgrade at all: %+v", construct, ds)
			continue
		}
		if !strings.Contains(detail, want) {
			t.Errorf("%q detail = %q, does not carry %q", construct, detail, want)
		}
	}

	// A target that has them all reports nothing, so the refusal cannot fire on
	// a deployment that serves the request as written.
	if ds := req.Downgrades(req.RequiredCapabilities()); len(ds) != 0 {
		t.Errorf("a capable target reported a downgrade: %+v", ds)
	}
}

// containsString is the test's own membership check; the package deliberately
// has no such helper on the hot path.
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestMissingIsRelativeToWhatIsHeld(t *testing.T) {
	have := CapMultiBlockContent | CapImageBlocks
	want := CapMultiBlockContent | CapDocumentBlocks | CapSeed
	got := have.Missing(want)
	if got != CapDocumentBlocks|CapSeed {
		t.Fatalf("Missing = %v", got)
	}
	if got.Structural() != CapDocumentBlocks {
		t.Errorf("Structural = %v", got.Structural())
	}
	if got.Droppable() != CapSeed {
		t.Errorf("Droppable = %v", got.Droppable())
	}
	if !have.Has(CapMultiBlockContent) || have.Has(CapDocumentBlocks) {
		t.Error("Has is wrong")
	}
}

// TestDowngradesLocateTheInstance is why Downgrade carries a Detail: "a
// document was dropped" is not actionable.
func TestDowngradesLocateTheInstance(t *testing.T) {
	req := &Request{
		System: Content{TextBlock("a"), TextBlock("b")},
		Messages: []Message{
			TextMessage(RoleUser, "fine"),
			{Role: RoleUser, Content: Content{
				TextBlock("look"),
				DocumentBlock("application/pdf", "x", "spec.pdf"),
			}},
			{Role: RoleTool, Content: Content{
				ToolResultBlock("c1", TextBlock("ok"), ImageBlock("image/png", "y")),
			}},
		},
	}
	// A target that can do multi-block content and images but nothing else.
	have := CapMultiBlockContent | CapImageBlocks | CapToolCalls
	ds := req.Downgrades(have)
	if len(ds) == 0 {
		t.Fatal("no downgrades reported for a request full of unsupported constructs")
	}
	byConstruct := map[string]string{}
	for _, d := range ds {
		byConstruct[d.Construct] = d.Detail
	}
	for _, want := range []string{ConstructStructuredSystem, ConstructDocumentBlock, ConstructMultiBlockToolResult} {
		if _, ok := byConstruct[want]; !ok {
			t.Errorf("missing %q; got %+v", want, ds)
		}
	}
	if d := byConstruct[ConstructDocumentBlock]; !strings.Contains(d, "messages[1].content[1]") {
		t.Errorf("document downgrade detail = %q, does not locate the block", d)
	}
	if d := byConstruct[ConstructMultiBlockToolResult]; !strings.Contains(d, "messages[2]") {
		t.Errorf("tool-result downgrade detail = %q", d)
	}

	// A target that can express everything reports nothing.
	if ds := req.Downgrades(req.RequiredCapabilities()); len(ds) != 0 {
		t.Errorf("a fully capable target reported downgrades: %+v", ds)
	}
}

func TestLossReportDeduplicatesParams(t *testing.T) {
	var l LossReport
	l.DropParam("seed", "seed", "top_k")
	l.DropCapability(CapSeed | CapLogprobs)
	if len(l.Dropped) != 4 {
		t.Fatalf("Dropped = %v, want 4 distinct names", l.Dropped)
	}
	if l.HasStructural() {
		t.Error("dropped parameters must not count as structural")
	}
	if !l.Lossy() {
		t.Error("a report with dropped params is lossy")
	}
	l.Downgrade(ConstructDocumentBlock, "a")
	l.Downgrade(ConstructDocumentBlock, "b")
	if !l.HasStructural() {
		t.Error("a downgrade must be structural")
	}
	if got := l.Constructs(); len(got) != 1 || got[0] != ConstructDocumentBlock {
		t.Errorf("Constructs = %v", got)
	}
}

func TestCapabilityNamesRoundTrip(t *testing.T) {
	for _, n := range capNames {
		got, ok := ParseCapability(n.construct)
		if !ok || got != n.bit {
			t.Errorf("ParseCapability(%q) = %v,%v", n.construct, got, ok)
		}
	}
	if _, ok := ParseCapability("not_a_construct"); ok {
		t.Error("ParseCapability accepted an unknown construct")
	}
	c := CapDocumentBlocks | CapSeed
	if s := c.String(); s != "document_block|seed" {
		t.Errorf("String = %q", s)
	}
	if Capability(0).String() != "none" {
		t.Error("the empty set must render as none")
	}
}

func TestContentPlainIsLosslessOnlyForOneBareTextBlock(t *testing.T) {
	cases := []struct {
		c     Content
		plain bool
		text  string
	}{
		{Content{TextBlock("hi")}, true, "hi"},
		{Content{TextBlock("a"), TextBlock("b")}, false, "ab"},
		{Content{ImageBlock("image/png", "x")}, false, ""},
		{Content{}, false, ""},
		{Content{func() Block {
			b := TextBlock("hi")
			b.CacheControl = &CacheControl{Type: "ephemeral"}
			return b
		}()}, false, "hi"},
	}
	for i, c := range cases {
		got, plain := c.c.Plain()
		if plain != c.plain || got != c.text {
			t.Errorf("case %d: Plain() = %q,%v want %q,%v", i, got, plain, c.text, c.plain)
		}
	}
}

func TestResponseCapabilitiesReportRichStopReasons(t *testing.T) {
	for _, r := range []StopReason{StopEndTurn, StopMaxTokens, StopToolUse, StopContentFilter, StopFunctionCall} {
		resp := &Response{Choices: []Choice{{StopReason: r}}}
		if resp.RequiredCapabilities().Has(CapRichStopReasons) {
			t.Errorf("%q is expressible by OpenAI and must not require rich stop reasons", r)
		}
	}
	for _, r := range []StopReason{StopStopSequence, StopRefusal, StopSafety, StopRecitation, StopError, StopPauseTurn} {
		resp := &Response{Choices: []Choice{{StopReason: r}}}
		if !resp.RequiredCapabilities().Has(CapRichStopReasons) {
			t.Errorf("%q collapses onto another meaning and must require rich stop reasons", r)
		}
	}
}

func TestUsageArithmetic(t *testing.T) {
	// Cache counts are a breakdown of the input and must not be added twice.
	u := Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 80, CacheWriteTokens: 10}
	if u.TotalTokens() != 120 {
		t.Fatalf("TotalTokens = %d, want 120", u.TotalTokens())
	}
	if (Usage{}).Empty() != true {
		t.Error("the zero usage must be empty")
	}
	var acc Usage
	acc.Add(Usage{InputTokens: 1, ReasoningTokens: 2})
	acc.Add(Usage{OutputTokens: 3, ReasoningTokens: 4})
	if acc != (Usage{InputTokens: 1, OutputTokens: 3, ReasoningTokens: 6}) {
		t.Fatalf("Add = %+v", acc)
	}
}

// TestModelNamesAreOpaque freezes DESIGN §2.1.
func TestModelNamesAreOpaque(t *testing.T) {
	for _, name := range []string{"gemma4:31b", "zai:glm-5.1", "deepseek-v4-flash:cloud", "qwen3.5:397b", "a/b:c"} {
		r := &Request{Model: name}
		if r.Model != name {
			t.Errorf("model %q became %q", name, r.Model)
		}
		if strings.ContainsAny(name, ":/") && r.RequiredCapabilities() != 0 {
			t.Errorf("a colon in a model name changed the capability set for %q", name)
		}
	}
}

func TestDataURLSplitAndJoin(t *testing.T) {
	b := ImageURLBlock("data:image/png;base64,aGk=")
	if b.Source.Kind != SourceBase64 || b.Source.MediaType != "image/png" || b.Source.Data != "aGk=" {
		t.Fatalf("data URL not split: %+v", b.Source)
	}
	if got := b.Source.DataURL(); got != "data:image/png;base64,aGk=" {
		t.Fatalf("DataURL = %q", got)
	}
	u := ImageURLBlock("https://example.test/a.png")
	if u.Source.Kind != SourceURL || u.Source.Data != "https://example.test/a.png" {
		t.Fatalf("plain URL was mangled: %+v", u.Source)
	}
	if got := u.Source.DataURL(); got != "https://example.test/a.png" {
		t.Fatalf("DataURL = %q", got)
	}
}

func TestItoa(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{{0, "0"}, {7, "7"}, {10, "10"}, {12345, "12345"}, {-3, "-3"}} {
		if got := itoa(c.n); got != c.want {
			t.Errorf("itoa(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }
