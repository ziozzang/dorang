package canonical

import "strings"

// Capability is a bit set naming what a protocol can express.
//
// Two things use it (DESIGN §10.1):
//
//   - A backend declares the set it supports, and routing prefers a deployment
//     whose set covers the request. Capability is a routing filter, so a request
//     that only one protocol family can express is sent there when such a
//     deployment exists, and fails only when none does.
//
//   - An encoder compares the request's required set against the target's set
//     and reports the difference — as a dropped parameter when the request still
//     means what it meant, and as a structural downgrade when it does not.
type Capability uint32

const (
	// CapMultiBlockContent — a message's content can be an ordered list rather
	// than one string.
	CapMultiBlockContent Capability = 1 << iota
	// CapImageBlocks — images can appear in message content.
	CapImageBlocks
	// CapDocumentBlocks — documents (PDFs) can appear in message content.
	CapDocumentBlocks
	// CapCacheBreakpoints — prompt-cache breakpoints can be placed explicitly.
	CapCacheBreakpoints
	// CapThinkingBlocks — reasoning blocks can be carried in the conversation,
	// including whatever integrity material the protocol attaches to them.
	CapThinkingBlocks
	// CapMultiBlockToolResult — one tool result can hold several blocks, e.g.
	// text plus an image.
	CapMultiBlockToolResult
	// CapStructuredSystem — the system prompt can be a list of blocks with
	// per-block attributes rather than a single message.
	CapStructuredSystem
	// CapRichStopReasons — the terminal condition can be reported beyond the
	// five OpenAI finish_reason values.
	CapRichStopReasons
	// CapToolCalls — tools can be declared and called at all.
	CapToolCalls
	// CapJSONSchema — a response JSON Schema can be enforced.
	CapJSONSchema

	// --- from here down the construct is a named request PARAMETER rather than
	// a shape, so a loss is reported by parameter name and never located. That
	// is a statement about how the loss is REPORTED, not about whether it is
	// tolerated: [Material] below picks out the ones that are refused anyway.

	// CapParallelToolCalls — parallel tool calling can be turned off.
	CapParallelToolCalls
	// CapReasoningControl — reasoning effort or budget can be controlled.
	CapReasoningControl
	// CapSeed — sampling can be seeded.
	CapSeed
	// CapLogitBias — per-token bias can be applied.
	CapLogitBias
	// CapLogprobs — token log probabilities can be returned.
	CapLogprobs
	// CapPenalties — frequency and presence penalties are accepted.
	CapPenalties
	// CapTopK — top-k sampling is accepted.
	CapTopK
	// CapMultipleChoices — n > 1 is accepted.
	CapMultipleChoices
	// CapPriority — a request priority hint is accepted.
	CapPriority
	// CapMetadata — caller metadata is accepted.
	CapMetadata
	// CapUser — an end-user identifier is accepted.
	CapUser
	// CapStopSequences — stop sequences are accepted.
	CapStopSequences
	// CapServiceTier — a service tier is accepted.
	CapServiceTier
)

// Structural is the mask of capabilities dorang refuses to lose silently: a
// request needing one that the target lacks is a `400` naming the construct,
// unless the caller opts in with `x-dorang-allow-lossy` (DESIGN §10.1).
//
// # The test, and the test it replaces
//
// This mask used to be defined as "the target cannot REPRESENT it", which reads
// as a statement about shape, and four parameters were filed as droppable on
// that reading even though their absence changes the answer: `stop`, `n`,
// `logprobs` and `service_tier`. Each returned 200 with the parameter named in
// `x-dorang-dropped-params`, which is not consent — it is a header nobody
// parses, carrying news that the reply is not the one that was asked for.
//
// The shape reading was never what the mask actually contained, either.
// [CapCacheBreakpoints] is here because losing a caching topology changes the
// BILL, and [CapRichStopReasons] because collapsing an enumeration loses WHICH
// terminal condition occurred. Neither is about shape. So the axis is restated
// as the question that was always deciding it:
//
//	Does the absence change what the caller RECEIVES or is CHARGED,
//	or only which knobs dorang applied on the way?
//
// A knob is droppable. Everything else needs the caller's word for it.
//
// The two halves differ in how a loss is REPORTED, not in whether it is
// refused: the shape half is located per instance ("messages[2].content[1]:
// application/pdf"), and [Material] is named by parameter. Both are
// [Downgrade]s and both are refused.
const Structural = CapMultiBlockContent | CapImageBlocks | CapDocumentBlocks |
	CapCacheBreakpoints | CapThinkingBlocks | CapMultiBlockToolResult |
	CapStructuredSystem | CapRichStopReasons | CapToolCalls | CapJSONSchema |
	Material

// Material is the half of [Structural] that is a request PARAMETER rather than
// a shape: the target has the field's meaning nowhere, and dropping it changes
// the answer or the price rather than the presentation.
//
// Membership is argued one at a time, because "it changes the answer" proves
// too much — every sampling knob changes the answer — and refusing more
// requests is a real cost. A caller trained to set `x-dorang-allow-lossy` on
// everything has been given a mechanism that reports nothing.
//
//   - CapStopSequences — a stop sequence is a stated TERMINATOR: the caller has
//     said the text will not continue past it. Dropped, generation runs on, the
//     caller is billed for tokens they excluded, and a client that splits the
//     answer on the sequence never fires. Nothing in the response says the
//     terminator was not applied, which is exactly §10.1's "the caller cannot
//     find out".
//
//   - CapMultipleChoices — `n` is a stated RESPONSE SHAPE. This one was already
//     misclassified under the old reading, never mind the new one: `n: 4` and
//     one choice back is the response having a different shape from the one
//     requested, and `choices[3]` is an index error in the client rather than
//     an actionable error from the gateway. It costs the everyday caller
//     nothing, because [Request.RequiredCapabilities] raises the bit only for
//     n > 1 — `n: 1` is the default spelled out and is not a capability claim.
//
//   - CapLogprobs — `logprobs: true` asks for a MEMBER OF THE RESPONSE. Dropped,
//     the body comes back without the thing it was asked for and with a 200 to
//     say it went well. It is the same class as `n` and it was on the same
//     wrong side; it was not in the review that found the others.
//
//   - CapServiceTier — a tier selects a PRICE BAND. Dropping it runs the request
//     in a band the caller did not choose and bills them for it, which is the
//     same reason CapCacheBreakpoints has always been structural. Only a tier
//     that selects something counts: "auto" means "the provider decides", which
//     is precisely what omitting the field means, so
//     [Request.RequiredCapabilities] raises no bit for it.
//
// Deliberately NOT here, and the reasoning is the load-bearing part:
//
//   - CapLogitBias — a bias is a PRIOR OVER SAMPLING, in the same family as
//     top_k and the penalties, and no vendor promises a distribution. The answer
//     under a dropped bias is an answer to the same question, in the same shape,
//     at the same price, and it is indistinguishable from an ordinary re-draw of
//     the same request WITH the bias. There is no postcondition to violate.
//     Refusing on it would commit dorang to refusing on top_k and on
//     frequency_penalty by the identical argument, and dropping top_k on the way
//     to an OpenAI target is an everyday, harmless conversion.
//     TestLogitBiasStaysDroppableBecauseTheAnswerIsTheSameKind pins this.
//   - CapParallelToolCalls — a target that cannot express it does not MAKE
//     parallel tool calls, so `false` is already satisfied there and `true` is a
//     permission rather than a requirement. The constraint holds by default.
//   - CapSeed — reproducibility is vendor-documented as best effort; the same
//     seed does not promise the same bytes.
//   - CapReasoningControl — §10.2 decides this one normatively: an unverified
//     reasoning capability is OMITTED and reported, never guessed.
//   - CapPriority — §10.5 makes ignoring a client hint the DEFAULT policy, not a
//     capability gap; refusing would refuse the configured behaviour.
//   - CapMetadata, CapUser — labels for the vendor's own dashboards. They do not
//     enter the answer, its shape, or its price.
const Material = CapLogprobs | CapMultipleChoices | CapStopSequences | CapServiceTier

// Droppable is the complement of [Structural] within the defined bits: a knob
// dorang did not apply, reported by name in x-dorang-dropped-params, with the
// request continuing because it still means what it meant.
const Droppable = CapParallelToolCalls | CapReasoningControl | CapSeed | CapLogitBias |
	CapPenalties | CapTopK | CapPriority | CapMetadata | CapUser

// ServiceTierAuto is the tier that selects nothing: it delegates the choice to
// the provider, which is what omitting the field already does. It is the one
// value of the field that is safe to drop, and it is named rather than spelled
// inline because it is the difference between a correct request and a 400.
const ServiceTierAuto = "auto"

// Has reports whether every bit of want is present in c.
func (c Capability) Has(want Capability) bool { return c&want == want }

// Missing returns the bits of want that c lacks.
func (c Capability) Missing(want Capability) Capability { return want &^ c }

// Structural returns the structural subset of c — what §10.1 refuses without an
// opt-in. It includes [Capability.Material].
func (c Capability) Structural() Capability { return c & Structural }

// Material returns the subset of c that is refused and reported by parameter
// name rather than located. See [Material].
func (c Capability) Material() Capability { return c & Material }

// Droppable returns the droppable subset of c.
func (c Capability) Droppable() Capability { return c & Droppable }

type capName struct {
	bit Capability
	// construct is the machine-readable id used in a 400 body and in the
	// x-dorang-allow-lossy opt-in list.
	construct string
	// params are the wire parameter names reported in x-dorang-dropped-params.
	params []string
}

// capNames is ordered by bit so that String() and Names() are deterministic,
// which golden tests depend on.
var capNames = []capName{
	{CapMultiBlockContent, ConstructMultiBlockContent, nil},
	{CapImageBlocks, ConstructImageBlock, nil},
	{CapDocumentBlocks, ConstructDocumentBlock, nil},
	{CapCacheBreakpoints, ConstructCacheBreakpoints, nil},
	{CapThinkingBlocks, ConstructThinkingBlock, nil},
	{CapMultiBlockToolResult, ConstructMultiBlockToolResult, nil},
	{CapStructuredSystem, ConstructStructuredSystem, nil},
	{CapRichStopReasons, ConstructRichStopReason, nil},
	{CapToolCalls, ConstructToolCalls, []string{"tools", "tool_choice"}},
	{CapJSONSchema, ConstructJSONSchema, []string{"response_format"}},

	{CapParallelToolCalls, "parallel_tool_calls", []string{"parallel_tool_calls"}},
	{CapReasoningControl, "reasoning", []string{"reasoning"}},
	{CapSeed, "seed", []string{"seed"}},
	{CapLogitBias, "logit_bias", []string{"logit_bias"}},
	{CapLogprobs, ConstructLogprobs, []string{"logprobs", "top_logprobs"}},
	{CapPenalties, "penalties", []string{"frequency_penalty", "presence_penalty"}},
	{CapTopK, "top_k", []string{"top_k"}},
	{CapMultipleChoices, ConstructMultipleChoices, []string{"n"}},
	{CapPriority, "priority", []string{"priority"}},
	{CapMetadata, "metadata", []string{"metadata"}},
	{CapUser, "user", []string{"user"}},
	{CapStopSequences, ConstructStopSequences, []string{"stop"}},
	{CapServiceTier, ConstructServiceTier, []string{"service_tier"}},
}

// Construct ids. These are the values a caller lists in x-dorang-allow-lossy and
// the values a 400 body names, so they are a stable vocabulary, not log text.
const (
	ConstructMultiBlockContent    = "multi_block_content"
	ConstructImageBlock           = "image_block"
	ConstructDocumentBlock        = "document_block"
	ConstructCacheBreakpoints     = "cache_breakpoints"
	ConstructThinkingBlock        = "thinking_block"
	ConstructMultiBlockToolResult = "multi_block_tool_result"
	ConstructStructuredSystem     = "structured_system"
	ConstructRichStopReason       = "rich_stop_reason"
	ConstructToolCalls            = "tool_calls"
	ConstructJSONSchema           = "json_schema"

	// The [Material] four. Their construct id IS their wire parameter name,
	// which the located constructs above have no equivalent of — that is the
	// whole difference between the two halves of [Structural]. They are named
	// here rather than spelled inline because they are now what a 400 body
	// carries and what x-dorang-allow-lossy accepts, and a vocabulary that
	// exists only as a string literal in a table drifts.
	ConstructLogprobs        = "logprobs"
	ConstructMultipleChoices = "n"
	ConstructStopSequences   = "stop"
	ConstructServiceTier     = "service_tier"
)

// Names returns the construct id of every set bit, low bit first.
func (c Capability) Names() []string {
	if c == 0 {
		return nil
	}
	out := make([]string, 0, 4)
	for _, n := range capNames {
		if c&n.bit != 0 {
			out = append(out, n.construct)
		}
	}
	return out
}

// Params returns the wire parameter names covered by the set bits of c. It is
// what x-dorang-dropped-params is built from.
func (c Capability) Params() []string {
	if c == 0 {
		return nil
	}
	out := make([]string, 0, 4)
	for _, n := range capNames {
		if c&n.bit != 0 {
			out = append(out, n.params...)
		}
	}
	return out
}

func (c Capability) String() string {
	if c == 0 {
		return "none"
	}
	return strings.Join(c.Names(), "|")
}

// ParseCapability resolves a construct id. It is the inverse of the construct
// column and is what parses x-dorang-allow-lossy.
func ParseCapability(construct string) (Capability, bool) {
	for _, n := range capNames {
		if n.construct == construct {
			return n.bit, true
		}
	}
	return 0, false
}

// Downgrade is one structural loss, named and located.
//
// Detail exists because "a document was dropped" is not actionable and
// "messages[2].content[1]: application/pdf" is.
type Downgrade struct {
	// Construct is one of the Construct* ids.
	Construct string
	// Detail locates or describes the specific instance.
	Detail string
}

// LossReport accumulates both kinds of loss for one conversion.
//
// An encoder fills it; the caller decides the policy. The encoder cannot decide
// on its own, because whether a structural downgrade is a 400 or an accepted
// loss depends on the request's x-dorang-allow-lossy header, which the encoder
// does not see (DESIGN §10.1).
type LossReport struct {
	// Dropped is the parameter-name list for x-dorang-dropped-params.
	Dropped []string
	// Downgrades is the structural list for x-dorang-downgraded, or for the
	// machine-readable 400 body when the caller did not opt in.
	Downgrades []Downgrade
}

// DropParam records a droppable parameter, ignoring repeats.
func (l *LossReport) DropParam(names ...string) {
	if l == nil {
		return
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		dup := false
		for _, have := range l.Dropped {
			if have == name {
				dup = true
				break
			}
		}
		if !dup {
			l.Dropped = append(l.Dropped, name)
		}
	}
}

// DropCapability records every parameter name behind the set bits of c.
func (l *LossReport) DropCapability(c Capability) {
	if l == nil || c == 0 {
		return
	}
	l.DropParam(c.Params()...)
}

// Downgrade records one structural loss.
func (l *LossReport) Downgrade(construct, detail string) {
	if l == nil {
		return
	}
	l.Downgrades = append(l.Downgrades, Downgrade{Construct: construct, Detail: detail})
}

// Lossy reports whether anything at all was lost.
func (l *LossReport) Lossy() bool {
	return l != nil && (len(l.Dropped) > 0 || len(l.Downgrades) > 0)
}

// HasStructural reports whether a construct was destroyed, which is the
// condition the caller answers with 400 unless the caller opted in.
func (l *LossReport) HasStructural() bool { return l != nil && len(l.Downgrades) > 0 }

// Constructs returns the distinct construct ids in the downgrade list.
func (l *LossReport) Constructs() []string {
	if l == nil || len(l.Downgrades) == 0 {
		return nil
	}
	out := make([]string, 0, len(l.Downgrades))
	for _, d := range l.Downgrades {
		dup := false
		for _, have := range out {
			if have == d.Construct {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, d.Construct)
		}
	}
	return out
}

// RequiredCapabilities reports what this request actually uses.
//
// "Actually uses" is the operative word: declaring tools sets [CapToolCalls],
// but a temperature does not set anything, because every protocol has one. Only
// constructs that some target cannot express are reported, so the returned set
// is directly usable as a routing filter.
func (r *Request) RequiredCapabilities() Capability {
	if r == nil {
		return 0
	}
	var c Capability

	if len(r.System) > 0 {
		if _, plain := r.System.Plain(); !plain {
			c |= CapStructuredSystem
		}
		c |= contentCapabilities(r.System) &^ CapMultiBlockContent
	}

	for i := range r.Messages {
		c |= messageCapabilities(&r.Messages[i])
	}

	if len(r.Tools) > 0 {
		c |= CapToolCalls
		for i := range r.Tools {
			if r.Tools[i].CacheControl != nil {
				c |= CapCacheBreakpoints
			}
		}
	}
	if r.ToolChoice != nil {
		c |= CapToolCalls
		if r.ToolChoice.DisableParallel != nil {
			c |= CapParallelToolCalls
		}
	}
	if r.ParallelToolCalls != nil {
		c |= CapParallelToolCalls
	}
	if r.ResponseFormat != nil && r.ResponseFormat.Kind == FormatJSONSchema {
		c |= CapJSONSchema
	}
	if r.Reasoning != nil {
		c |= CapReasoningControl
	}
	if r.Seed != nil {
		c |= CapSeed
	}
	if len(r.LogitBias) > 0 {
		c |= CapLogitBias
	}
	if r.Logprobs != nil || r.TopLogprobs != nil {
		c |= CapLogprobs
	}
	if r.FrequencyPenalty != nil || r.PresencePenalty != nil {
		c |= CapPenalties
	}
	if r.TopK != nil {
		c |= CapTopK
	}
	// n: 1 is the default written down, not a claim on a capability. Only a
	// caller who asked for more than one choice is asking for something a target
	// without `n` cannot give them, which is why reclassifying [CapMultipleChoices]
	// as structural costs the "always sends n: 1" caller nothing.
	if r.N != nil && *r.N > 1 {
		c |= CapMultipleChoices
	}
	if r.Priority != nil {
		c |= CapPriority
	}
	if len(r.Metadata) > 0 {
		c |= CapMetadata
	}
	if r.User != "" {
		c |= CapUser
	}
	if len(r.Stop) > 0 {
		c |= CapStopSequences
	}
	// Same rule as n: 1 above. "auto" delegates the band to the provider, and so
	// does sending no field at all, so dropping it is not a change and must not
	// become a refusal. Case-insensitively, because a caller who spells it
	// "Auto" has still selected nothing and a 400 for casing would be absurd.
	if r.ServiceTier != "" && !strings.EqualFold(r.ServiceTier, ServiceTierAuto) {
		c |= CapServiceTier
	}
	return c
}

// MaterialLoss records into l every [Material] capability that a target holding
// `have` cannot express, one [Downgrade] each.
//
// It exists separately from [Request.Downgrades] because the encoders build
// their loss report as they walk, and this half needs no walk — the capability
// mask decides it outright. Both entry points call it, so the two cannot drift.
//
// Note the receiver argument, same trap as Downgrades: Missing reports what the
// RECEIVER lacks, so the target's capabilities go on the left.
func (r *Request) MaterialLoss(have Capability, l *LossReport) {
	if r == nil || l == nil {
		return
	}
	missing := have.Missing(r.RequiredCapabilities()) & Material
	if missing == 0 {
		return
	}
	// Detail is the value the caller wrote, because "n was dropped" is not
	// actionable and "n: 4" is — the same reason [Downgrade.Detail] exists for
	// the located half.
	if missing&CapStopSequences != 0 {
		l.Downgrade(ConstructStopSequences, "stop: "+itoa(len(r.Stop))+" sequence(s) the target cannot honour")
	}
	if missing&CapMultipleChoices != 0 && r.N != nil {
		l.Downgrade(ConstructMultipleChoices, "n: "+itoa(*r.N))
	}
	if missing&CapLogprobs != 0 {
		if r.TopLogprobs != nil {
			l.Downgrade(ConstructLogprobs, "top_logprobs: "+itoa(*r.TopLogprobs))
		} else {
			l.Downgrade(ConstructLogprobs, "logprobs")
		}
	}
	if missing&CapServiceTier != 0 {
		l.Downgrade(ConstructServiceTier, "service_tier: "+r.ServiceTier)
	}
}

// messageCapabilities is the per-message half of RequiredCapabilities.
func messageCapabilities(m *Message) Capability {
	var c Capability
	// Tool-use blocks leave the content array on the OpenAI wire (they become
	// tool_calls), so they do not by themselves force multi-block content.
	// Everything else does.
	carried := 0
	for i := range m.Content {
		b := &m.Content[i]
		switch b.Kind {
		case KindToolUse:
			c |= CapToolCalls
		case KindToolResult:
			c |= toolResultCapabilities(b.ToolResult)
		case KindImage:
			c |= CapImageBlocks
			carried++
		case KindDocument:
			c |= CapDocumentBlocks
			carried++
		case KindThinking:
			c |= CapThinkingBlocks
			carried++
		default:
			carried++
		}
		if b.CacheControl != nil {
			c |= CapCacheBreakpoints
		}
	}
	// More than one carried block, or a single carried block that is not plain
	// text, needs the array form.
	if carried > 1 {
		c |= CapMultiBlockContent
	} else if carried == 1 {
		for i := range m.Content {
			switch m.Content[i].Kind {
			case KindToolUse, KindToolResult:
				continue
			case KindText:
				if m.Content[i].CacheControl != nil || len(m.Content[i].Extra) > 0 {
					c |= CapMultiBlockContent
				}
			default:
				c |= CapMultiBlockContent
			}
		}
	}
	return c
}

func toolResultCapabilities(tr *ToolResult) Capability {
	if tr == nil {
		return 0
	}
	c := CapToolCalls
	if _, plain := Content(tr.Content).Plain(); !plain {
		c |= CapMultiBlockToolResult
	}
	c |= contentCapabilities(tr.Content) &^ CapMultiBlockContent
	return c
}

// contentCapabilities reports the block kinds present, without deciding whether
// the array form is required — callers that care apply their own rule.
func contentCapabilities(c Content) Capability {
	var out Capability
	for i := range c {
		switch c[i].Kind {
		case KindImage:
			out |= CapImageBlocks
		case KindDocument:
			out |= CapDocumentBlocks
		case KindThinking:
			out |= CapThinkingBlocks
		case KindToolUse:
			out |= CapToolCalls
		case KindToolResult:
			out |= toolResultCapabilities(c[i].ToolResult)
		}
		if c[i].CacheControl != nil {
			out |= CapCacheBreakpoints
		}
	}
	if len(c) > 1 {
		out |= CapMultiBlockContent
	}
	return out
}

// Downgrades reports the structural losses that converting this request for a
// backend with capability set `have` would cause.
//
// The shape half is located to the instance; the [Material] half is named by
// parameter and carries the value the caller wrote, because there is no
// instance to point at — `service_tier` appears once and means one thing.
//
// Droppable losses are NOT reported here: they are reported by name, and the
// name is all there is to say about them. Callers get those from
// have.Missing(RequiredCapabilities()).Droppable().Params().
//
// Note the receiver: Missing reports what the RECEIVER lacks of its argument, so the
// backend's capabilities go on the left. Written the other way round it yields the
// backend's *surplus* capabilities, which is silently plausible — the first caller to
// follow an earlier version of this line implemented exactly that bug.
func (r *Request) Downgrades(have Capability) []Downgrade {
	if r == nil {
		return nil
	}
	missing := have.Missing(r.RequiredCapabilities()).Structural()
	if missing == 0 {
		return nil
	}
	var out []Downgrade
	add := func(bit Capability, construct, detail string) {
		if missing&bit != 0 {
			out = append(out, Downgrade{Construct: construct, Detail: detail})
		}
	}

	if len(r.System) > 0 {
		if _, plain := r.System.Plain(); !plain {
			add(CapStructuredSystem, ConstructStructuredSystem, "system")
		}
		for i := range r.System {
			if r.System[i].CacheControl != nil {
				add(CapCacheBreakpoints, ConstructCacheBreakpoints, "system["+itoa(i)+"]")
			}
		}
	}

	for mi := range r.Messages {
		m := &r.Messages[mi]
		where := "messages[" + itoa(mi) + "]"
		if missing&CapMultiBlockContent != 0 && messageCapabilities(m)&CapMultiBlockContent != 0 {
			add(CapMultiBlockContent, ConstructMultiBlockContent, where+".content")
		}
		for bi := range m.Content {
			b := &m.Content[bi]
			at := where + ".content[" + itoa(bi) + "]"
			switch b.Kind {
			case KindImage:
				add(CapImageBlocks, ConstructImageBlock, at+": "+sourceDetail(b.Source))
			case KindDocument:
				add(CapDocumentBlocks, ConstructDocumentBlock, at+": "+sourceDetail(b.Source))
			case KindThinking:
				add(CapThinkingBlocks, ConstructThinkingBlock, at)
			case KindToolUse:
				add(CapToolCalls, ConstructToolCalls, at+": "+b.ToolUse.nameOrEmpty())
			case KindToolResult:
				if b.ToolResult != nil {
					if _, plain := Content(b.ToolResult.Content).Plain(); !plain {
						add(CapMultiBlockToolResult, ConstructMultiBlockToolResult, at)
					}
					for ri := range b.ToolResult.Content {
						rb := &b.ToolResult.Content[ri]
						rat := at + ".content[" + itoa(ri) + "]"
						switch rb.Kind {
						case KindImage:
							add(CapImageBlocks, ConstructImageBlock, rat+": "+sourceDetail(rb.Source))
						case KindDocument:
							add(CapDocumentBlocks, ConstructDocumentBlock, rat+": "+sourceDetail(rb.Source))
						}
					}
				}
			}
			if b.CacheControl != nil {
				add(CapCacheBreakpoints, ConstructCacheBreakpoints, at)
			}
		}
	}

	if len(r.Tools) > 0 || r.ToolChoice != nil {
		add(CapToolCalls, ConstructToolCalls, "tools")
	}
	for i := range r.Tools {
		if r.Tools[i].CacheControl != nil {
			add(CapCacheBreakpoints, ConstructCacheBreakpoints, "tools["+itoa(i)+"]")
		}
	}
	if r.ResponseFormat != nil && r.ResponseFormat.Kind == FormatJSONSchema {
		add(CapJSONSchema, ConstructJSONSchema, "response_format")
	}

	// The named half. One implementation, two entry points (see [Request.MaterialLoss]).
	var ml LossReport
	r.MaterialLoss(have, &ml)
	return append(out, ml.Downgrades...)
}

func (t *ToolUse) nameOrEmpty() string {
	if t == nil {
		return ""
	}
	return t.Name
}

func sourceDetail(s *Source) string {
	if s == nil {
		return "<no source>"
	}
	if s.MediaType != "" {
		return s.MediaType
	}
	return string(s.Kind)
}

// itoa formats a small non-negative int without importing strconv into the
// detail path, and without fmt — DESIGN §15.5 bars formatted string
// construction on the hot path and downgrade detail is built per request.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
