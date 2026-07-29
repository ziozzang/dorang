package anthropic

import (
	"encoding/json"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
)

// EncodeOptions controls a canonical -> Anthropic conversion.
type EncodeOptions struct {
	// Capabilities is what the target can express. Zero means
	// [DefaultCapabilities].
	Capabilities canonical.Capability

	// Loss accumulates what the conversion removed. A nil Loss discards the
	// report, which is only correct for a same-protocol pass-through where
	// nothing can be lost.
	//
	// The encoder records; it does not decide. Whether a structural downgrade is
	// a 400 or an accepted loss depends on the caller's x-dorang-allow-lossy
	// header, which the encoder never sees (DESIGN §10.1) — except through
	// AllowLossy below, which is that header already parsed.
	Loss *canonical.LossReport

	// AllowLossy is the parsed x-dorang-allow-lossy opt-in. A construct listed
	// here is downgraded and reported instead of refused. It exists because two
	// of this package's failures are hard errors rather than recorded losses,
	// and a caller must be able to say "yes, I know, proceed" for those too.
	AllowLossy canonical.Capability

	// Model overrides the model field, which is how the real upstream id reaches
	// the backend while the client-facing name stays in the response
	// (DESIGN §7.2). Empty keeps the canonical value.
	Model string

	// DefaultMaxTokens is the model's maximum output from the catalog. It is
	// used when the neutral request has no ceiling of its own.
	//
	// DESIGN §10.7, trap one of three: max_tokens is REQUIRED in this family and
	// optional everywhere else, so a crossing that omits it produces a 400 from
	// the backend. Supplying a constant instead would silently cap the caller's
	// output at a number they never chose, so there is no safe default and the
	// encoder demands the catalog value. Zero with no request ceiling is
	// [ErrMaxTokensRequired].
	DefaultMaxTokens int

	// ReasoningReserve is the gap kept between a thinking budget and the output
	// ceiling (DESIGN §10.2: budget = min(table[effort], max_tokens - reserve),
	// reserve >= 1). Zero means one.
	ReasoningReserve int

	Warn WarnFunc
}

func (o *EncodeOptions) caps() canonical.Capability {
	if o == nil || o.Capabilities == 0 {
		return DefaultCapabilities
	}
	return o.Capabilities
}

func (o *EncodeOptions) loss() *canonical.LossReport {
	if o == nil {
		return nil
	}
	return o.Loss
}

func (o *EncodeOptions) warn() WarnFunc {
	if o == nil {
		return nil
	}
	return o.Warn
}

func (o *EncodeOptions) allows(c canonical.Capability) bool {
	return o != nil && o.AllowLossy&c != 0
}

func (o *EncodeOptions) reserve() int {
	if o == nil || o.ReasoningReserve <= 0 {
		return 1
	}
	return o.ReasoningReserve
}

// MarshalRequest encodes a neutral request as Anthropic request bytes.
func MarshalRequest(req *canonical.Request, opt *EncodeOptions) ([]byte, error) {
	w, err := EncodeRequest(req, opt)
	if err != nil {
		return nil, err
	}
	return Marshal(w)
}

// EncodeRequest converts a neutral request to the Messages wire shape.
//
// It fails, rather than recording a loss, in exactly two situations, and both
// are refusals to invent a value dorang is not entitled to invent:
//
//   - no output ceiling and no catalog value ([ErrMaxTokensRequired]);
//   - an assistant reasoning block with no integrity material, which this
//     family cannot accept and dorang will not sign ([OpaqueError]). This is
//     the failure DESIGN §10.2 says lands on the SECOND turn of an agentic
//     flow, which is why it is loud.
//
// Everything else is recorded in opt.Loss and the conversion continues.
func EncodeRequest(req *canonical.Request, opt *EncodeOptions) (*Request, error) {
	if req == nil {
		return nil, errNilRequest
	}
	caps := opt.caps()
	loss := opt.loss()

	// Droppable parameters first: one bit-mask subtraction reports every knob
	// this family does not have, by wire name — seed, logit_bias, penalties.
	missing := caps.Missing(req.RequiredCapabilities())
	loss.DropCapability(missing.Droppable())
	// The MATERIAL half of the same subtraction — n, logprobs, service_tier,
	// none of which this family has — is a downgrade rather than a dropped
	// knob, because its absence changes the answer or the price
	// (canonical.Material). It is recorded, not refused: this encoder still does
	// not decide policy, and the two things it DOES refuse are refusals to
	// invent a value, which is a different act.
	req.MaterialLoss(caps, loss)

	out := &Request{
		Model:  req.Model,
		Stream: req.Stream,
		Extra:  req.Extra,
	}
	if opt != nil && opt.Model != "" {
		out.Model = opt.Model
	}

	maxTokens, err := resolveMaxTokens(req, opt)
	if err != nil {
		return nil, err
	}
	out.MaxTokens = ptr(maxTokens)

	// System, plus any system/developer message folded in. This family has no
	// system ROLE; the prompt is a top-level field.
	sys := append(canonical.Content(nil), req.System...)

	msgs := make([]Message, 0, len(req.Messages))
	for i := range req.Messages {
		m := &req.Messages[i]
		where := "messages[" + strconv.Itoa(i) + "]"
		switch m.Role {
		case canonical.RoleSystem, canonical.RoleDeveloper:
			sys = append(sys, m.Content...)
			continue
		}
		wm, err := encodeMessage(m, caps, loss, opt, where)
		if err != nil {
			return nil, err
		}
		if wm != nil {
			msgs = append(msgs, *wm)
		}
	}
	// Roles must alternate on this wire. One neutral tool message per result —
	// which is what an OpenAI-shaped conversation looks like — produces a run of
	// consecutive user turns, and the backend rejects the whole request with a
	// message about alternation that names nothing the caller wrote. Merging is
	// not a nicety.
	out.Messages = mergeAdjacent(msgs)

	if len(sys) > 0 {
		sysCaps := caps
		if !caps.Has(canonical.CapStructuredSystem) {
			sysCaps &^= canonical.CapMultiBlockContent
			if _, plain := sys.Plain(); !plain {
				loss.Downgrade(canonical.ConstructStructuredSystem, "system")
			}
		}
		l, err := encodeBlockList(sys, sysCaps, loss, opt, "system")
		if err != nil {
			return nil, err
		}
		out.System = l
	}

	if len(req.Tools) > 0 {
		if !caps.Has(canonical.CapToolCalls) {
			loss.Downgrade(canonical.ConstructToolCalls, "tools")
		} else {
			out.Tools = make([]Tool, 0, len(req.Tools))
			for i := range req.Tools {
				t := &req.Tools[i]
				wt := Tool{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: t.Parameters,
					Extra:       t.Extra,
				}
				// "function" is the neutral spelling of the ordinary custom
				// tool, which is untyped on this wire. A provider-native type is
				// passed through verbatim.
				if t.Type != "" && t.Type != "function" {
					wt.Type = t.Type
				}
				if t.CacheControl != nil {
					if caps.Has(canonical.CapCacheBreakpoints) {
						wt.CacheControl = encodeCacheControl(t.CacheControl)
					} else {
						loss.Downgrade(canonical.ConstructCacheBreakpoints, "tools["+strconv.Itoa(i)+"]")
					}
				}
				out.Tools = append(out.Tools, wt)
			}
		}
	}
	if req.ToolChoice != nil && caps.Has(canonical.CapToolCalls) {
		out.ToolChoice = encodeToolChoice(req.ToolChoice)
	}
	// disable_parallel_tool_use is the INVERSE of parallel_tool_calls
	// (DESIGN §10.7). The neutral request carries both spellings; the
	// Anthropic-shaped one wins when set, and the OpenAI-shaped one is negated,
	// never copied.
	if caps.Has(canonical.CapParallelToolCalls) {
		var disable *bool
		if req.ToolChoice != nil && req.ToolChoice.DisableParallel != nil {
			disable = req.ToolChoice.DisableParallel
		} else if req.ParallelToolCalls != nil {
			disable = ptr(!*req.ParallelToolCalls)
		}
		if disable != nil {
			if out.ToolChoice == nil {
				out.ToolChoice = &ToolChoice{Type: ToolChoiceAuto}
			}
			out.ToolChoice.DisableParallelToolUse = disable
		}
	}

	out.Temperature = req.Temperature
	out.TopP = req.TopP
	if req.TopK != nil && caps.Has(canonical.CapTopK) {
		out.TopK = req.TopK
	}
	if len(req.Stop) > 0 && caps.Has(canonical.CapStopSequences) {
		out.StopSequences = req.Stop
	}
	if md := encodeMetadata(req, caps); md != nil {
		out.Metadata = md
	}

	if req.ResponseFormat != nil {
		// There is no response_format here. Expressing a schema means rewriting
		// the request into a forced tool call, which changes what the model is
		// asked to do — DESIGN §10.1 would rather report the loss than invent
		// one.
		if req.ResponseFormat.Kind == canonical.FormatJSONSchema {
			loss.Downgrade(canonical.ConstructJSONSchema, "response_format")
		} else {
			loss.DropParam("response_format")
		}
	}

	if req.Reasoning != nil {
		if !caps.Has(canonical.CapReasoningControl) {
			loss.DropParam("thinking")
		} else {
			out.Thinking = encodeThinking(req.Reasoning, maxTokens, loss, opt)
		}
	}
	return out, nil
}

// resolveMaxTokens applies DESIGN §10.7's first trap.
func resolveMaxTokens(req *canonical.Request, opt *EncodeOptions) (int, error) {
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		return *req.MaxTokens, nil
	}
	if opt != nil && opt.DefaultMaxTokens > 0 {
		return opt.DefaultMaxTokens, nil
	}
	return 0, ErrMaxTokensRequired
}

func encodeMetadata(req *canonical.Request, caps canonical.Capability) *Metadata {
	if !caps.Has(canonical.CapUser) && !caps.Has(canonical.CapMetadata) {
		return nil
	}
	var md *Metadata
	if req.User != "" && caps.Has(canonical.CapUser) {
		md = &Metadata{UserID: req.User}
	}
	if len(req.Metadata) > 0 && caps.Has(canonical.CapMetadata) {
		if md == nil {
			md = &Metadata{}
		}
		md.Extra = make(map[string]json.RawMessage, len(req.Metadata))
		for k, v := range req.Metadata {
			raw, err := Marshal(v)
			if err != nil {
				continue
			}
			md.Extra[k] = raw
		}
	}
	return md
}

// encodeMessage converts one neutral message.
//
// A neutral tool message becomes a USER message holding tool_result blocks,
// because that is where this family puts them. mergeAdjacent then folds a run of
// them into one turn.
func encodeMessage(m *canonical.Message, caps canonical.Capability, loss *canonical.LossReport, opt *EncodeOptions, where string) (*Message, error) {
	role := string(m.Role)
	if m.Role == canonical.RoleTool {
		role = RoleUser
	}
	content := m.Content
	if m.Refusal != "" {
		// There is no refusal field on a message here; the refusal is a stop
		// reason. Carrying the text as a block keeps the words the user must
		// see, and the field name is reported so the caller knows it moved.
		loss.DropParam("refusal")
		content = append(append(canonical.Content(nil), content...), canonical.TextBlock(m.Refusal))
	}
	l, err := encodeBlockList(content, caps, loss, opt, where)
	if err != nil {
		return nil, err
	}
	if l == nil {
		// A turn with nothing left in it is not sent. An empty content array is
		// rejected outright by this family, and a turn that lost every block was
		// already reported as a downgrade by the block encoder.
		return nil, nil
	}
	return &Message{Role: role, Content: *l, Extra: m.Extra}, nil
}

// mergeAdjacent folds consecutive same-role turns into one.
func mergeAdjacent(msgs []Message) []Message {
	if len(msgs) < 2 {
		return msgs
	}
	out := msgs[:1]
	for i := 1; i < len(msgs); i++ {
		last := &out[len(out)-1]
		if msgs[i].Role != last.Role {
			out = append(out, msgs[i])
			continue
		}
		// Merging forces the array form: two turns' worth of content is not one
		// string, and joining the text would silently concatenate two separate
		// user utterances.
		merged := blocksOf(&last.Content)
		merged = append(merged, blocksOf(&msgs[i].Content)...)
		last.Content = *BlocksList(merged)
		if len(msgs[i].Extra) > 0 {
			if last.Extra == nil {
				last.Extra = msgs[i].Extra
			} else {
				for k, v := range msgs[i].Extra {
					if _, dup := last.Extra[k]; !dup {
						last.Extra[k] = v
					}
				}
			}
		}
	}
	return out
}

func blocksOf(l *BlockList) []ContentBlock {
	if l.Blocks != nil {
		return l.Blocks
	}
	if l.Text == "" {
		return nil
	}
	return []ContentBlock{{Type: BlockText, Text: ptr(l.Text)}}
}

// encodeBlockList renders neutral blocks as the string or array form.
func encodeBlockList(blocks canonical.Content, caps canonical.Capability, loss *canonical.LossReport, opt *EncodeOptions, where string) (*BlockList, error) {
	if len(blocks) == 0 {
		return nil, nil
	}
	if s, plain := blocks.Plain(); plain {
		return TextList(s), nil
	}
	if !caps.Has(canonical.CapMultiBlockContent) {
		loss.Downgrade(canonical.ConstructMultiBlockContent, where+".content")
		reportLostBlocks(blocks, loss, where+".content")
		return TextList(blocks.Flatten()), nil
	}
	out := make([]ContentBlock, 0, len(blocks))
	for i := range blocks {
		b, ok, err := encodeBlock(&blocks[i], caps, loss, opt, where+".content["+strconv.Itoa(i)+"]")
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return BlocksList(out), nil
}

// encodeBlock renders one neutral block. The bool is false when the block could
// not be carried at all.
func encodeBlock(b *canonical.Block, caps canonical.Capability, loss *canonical.LossReport, opt *EncodeOptions, where string) (ContentBlock, bool, error) {
	out := ContentBlock{Extra: b.Extra}
	switch b.Kind {
	case canonical.KindText:
		out.Type = BlockText
		out.Text = ptr(b.Text)

	case canonical.KindImage:
		if !caps.Has(canonical.CapImageBlocks) || b.Source == nil {
			loss.Downgrade(canonical.ConstructImageBlock, where)
			return ContentBlock{}, false, nil
		}
		out.Type = BlockImage
		out.Source = encodeSource(b.Source)

	case canonical.KindDocument:
		if !caps.Has(canonical.CapDocumentBlocks) || b.Source == nil {
			// Returning 200 having silently discarded a PDF is the case
			// DESIGN §10.1 calls worse than an error.
			loss.Downgrade(canonical.ConstructDocumentBlock, where+": "+sourceMediaType(b.Source))
			return ContentBlock{}, false, nil
		}
		out.Type = BlockDocument
		out.Source = encodeSource(b.Source)
		out.Title = b.Source.Name

	case canonical.KindToolUse:
		if b.ToolUse == nil {
			return ContentBlock{}, false, nil
		}
		if !caps.Has(canonical.CapToolCalls) {
			loss.Downgrade(canonical.ConstructToolCalls, where+": "+b.ToolUse.Name)
			return ContentBlock{}, false, nil
		}
		out.Type = BlockToolUse
		out.ID = b.ToolUse.ID
		out.Name = b.ToolUse.Name
		out.Input = b.ToolUse.Input
		if len(out.Input) == 0 {
			out.Input = json.RawMessage("{}")
		}

	case canonical.KindToolResult:
		if b.ToolResult == nil {
			return ContentBlock{}, false, nil
		}
		out.Type = BlockToolResult
		out.ToolUseID = b.ToolResult.ToolUseID
		out.IsError = b.ToolResult.IsError
		c, err := encodeToolResultContent(b.ToolResult, caps, loss, opt, where)
		if err != nil {
			return ContentBlock{}, false, err
		}
		out.Content = c

	case canonical.KindThinking:
		blk, ok, err := encodeThinkingBlock(b, caps, loss, opt, where)
		if err != nil || !ok {
			return ContentBlock{}, false, err
		}
		out = blk

	default:
		// An unmodelled kind, carried through with whatever members it arrived
		// with. Dropping it would be a filter, and §10.5a is explicit that not
		// recognizing something is not a reason to remove it.
		out.Type = string(b.Kind)
		if b.Text != "" {
			out.Text = ptr(b.Text)
		}
	}

	if b.CacheControl != nil {
		if caps.Has(canonical.CapCacheBreakpoints) {
			out.CacheControl = encodeCacheControl(b.CacheControl)
		} else {
			// Losing a breakpoint changes the caching topology and therefore the
			// bill, invisibly. It is structural for that reason.
			loss.Downgrade(canonical.ConstructCacheBreakpoints, where)
		}
	}
	return out, true, nil
}

// encodeThinkingBlock is the opaque-handle rule (DESIGN §10.2, EXTENSIONS §B).
//
// A reasoning block reaching this family must carry the material that makes it
// verifiable: a signature, or — for a redacted block — the payload itself. There
// is no third option. dorang does not sign, does not re-sign, and does not send
// an empty shell and hope, because the backend rejects it with an error that
// names nothing the caller wrote and the caller has no way to connect it to the
// turn where the material was lost.
func encodeThinkingBlock(b *canonical.Block, caps canonical.Capability, loss *canonical.LossReport, opt *EncodeOptions, where string) (ContentBlock, bool, error) {
	if !caps.Has(canonical.CapThinkingBlocks) {
		loss.Downgrade(canonical.ConstructThinkingBlock, where)
		return ContentBlock{}, false, nil
	}
	if b.Thinking != nil && b.Thinking.Redacted {
		data := redactedData(b.Extra)
		if data == "" {
			if opt.allows(canonical.CapThinkingBlocks) {
				loss.Downgrade(canonical.ConstructThinkingBlock, where+": redacted payload lost")
				return ContentBlock{}, false, nil
			}
			return ContentBlock{}, false, &OpaqueError{
				Reason:    ReasonRedactedWithoutData,
				Construct: canonical.ConstructThinkingBlock,
				Detail:    where,
			}
		}
		return ContentBlock{Type: BlockRedactedThinking, Data: data, Extra: b.Extra}, true, nil
	}
	if b.Thinking == nil || b.Thinking.Signature == "" {
		if opt.allows(canonical.CapThinkingBlocks) {
			loss.Downgrade(canonical.ConstructThinkingBlock, where+": unsigned")
			return ContentBlock{}, false, nil
		}
		return ContentBlock{}, false, &OpaqueError{
			Reason:    ReasonUnsignedThinking,
			Construct: canonical.ConstructThinkingBlock,
			Detail:    where,
		}
	}
	return ContentBlock{
		Type:      BlockThinking,
		Thinking:  ptr(b.Text),
		Signature: b.Thinking.Signature,
		Extra:     b.Extra,
	}, true, nil
}

// redactedData recovers the payload parked by the decoder (EXTENSIONS §B18).
func redactedData(extra map[string]json.RawMessage) string {
	raw, ok := extra["data"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func encodeToolResultContent(tr *canonical.ToolResult, caps canonical.Capability, loss *canonical.LossReport, opt *EncodeOptions, where string) (*BlockList, error) {
	if len(tr.Content) == 0 {
		return TextList(""), nil
	}
	if s, plain := canonical.Content(tr.Content).Plain(); plain {
		return TextList(s), nil
	}
	if !caps.Has(canonical.CapMultiBlockToolResult) {
		loss.Downgrade(canonical.ConstructMultiBlockToolResult, where)
		reportLostBlocks(tr.Content, loss, where)
		return TextList(canonical.Content(tr.Content).Flatten()), nil
	}
	out := make([]ContentBlock, 0, len(tr.Content))
	for i := range tr.Content {
		b, ok, err := encodeBlock(&tr.Content[i], caps, loss, opt, where+".content["+strconv.Itoa(i)+"]")
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, b)
		}
	}
	return BlocksList(out), nil
}

func reportLostBlocks(blocks canonical.Content, loss *canonical.LossReport, where string) {
	for i := range blocks {
		at := where + "[" + strconv.Itoa(i) + "]"
		switch blocks[i].Kind {
		case canonical.KindImage:
			loss.Downgrade(canonical.ConstructImageBlock, at)
		case canonical.KindDocument:
			loss.Downgrade(canonical.ConstructDocumentBlock, at)
		case canonical.KindThinking:
			loss.Downgrade(canonical.ConstructThinkingBlock, at)
		}
		if blocks[i].CacheControl != nil {
			loss.Downgrade(canonical.ConstructCacheBreakpoints, at)
		}
	}
}

func encodeSource(s *canonical.Source) *Source {
	if s == nil {
		return nil
	}
	switch s.Kind {
	case canonical.SourceURL:
		return &Source{Type: SourceURL, URL: s.Data}
	case canonical.SourceFileID:
		return &Source{Type: SourceFile, FileID: s.Data}
	case canonical.SourceText:
		return &Source{Type: SourceText, MediaType: mediaTypeOr(s.MediaType, "text/plain"), Data: s.Data}
	default:
		return &Source{Type: SourceBase64, MediaType: s.MediaType, Data: s.Data}
	}
}

func mediaTypeOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func encodeCacheControl(c *canonical.CacheControl) *CacheControl {
	if c == nil {
		return nil
	}
	t := c.Type
	if t == "" {
		t = "ephemeral"
	}
	return &CacheControl{Type: t, TTL: c.TTL}
}

func encodeToolChoice(tc *canonical.ToolChoice) *ToolChoice {
	out := &ToolChoice{}
	switch tc.Mode {
	case canonical.ToolChoiceRequired:
		out.Type = ToolChoiceAny
	case canonical.ToolChoiceTool:
		out.Type = ToolChoiceTool
		out.Name = tc.Name
	case canonical.ToolChoiceNone:
		out.Type = ToolChoiceNone
	case canonical.ToolChoiceAuto, "":
		out.Type = ToolChoiceAuto
	default:
		out.Type = string(tc.Mode)
	}
	return out
}

func sourceMediaType(s *canonical.Source) string {
	if s == nil || s.MediaType == "" {
		return "unknown"
	}
	return s.MediaType
}

// ---------------------------------------------------------------------------
// Reasoning
// ---------------------------------------------------------------------------

// MinThinkingBudget is the smallest budget this family accepts. Below it,
// reasoning is disabled for the request and the omission is reported —
// DESIGN §10.2 is explicit that dorang never raises the caller's max_tokens to
// make a budget fit.
const MinThinkingBudget = 1024

// effortBudget is the effort -> budget table. It is a table of intentions, not
// of limits: the value that reaches the wire is always min(table, ceiling -
// reserve), so a caller asking for a small output does not silently get their
// ceiling multiplied.
var effortBudget = map[string]int{
	canonical.EffortMinimal: 1024,
	canonical.EffortLow:     2048,
	canonical.EffortMedium:  8192,
	canonical.EffortHigh:    16384,
	canonical.EffortXHigh:   24576,
	canonical.EffortMax:     32768,
}

// effortLadder is effortBudget in ascending order, for the reverse mapping.
var effortLadder = []struct {
	effort string
	budget int
}{
	{canonical.EffortMinimal, 1024},
	{canonical.EffortLow, 2048},
	{canonical.EffortMedium, 8192},
	{canonical.EffortHigh, 16384},
	{canonical.EffortXHigh, 24576},
	{canonical.EffortMax, 32768},
}

// effortForBudget is the best-effort reverse mapping (DESIGN §10.2: "reverse
// mapping is best-effort and says so"). It names the highest level the budget
// covers, so a crossing into an effort-scale backend has something truthful to
// clamp rather than nothing at all.
func effortForBudget(budget int) string {
	if budget <= 0 {
		return ""
	}
	out := canonical.EffortMinimal
	for _, e := range effortLadder {
		if budget >= e.budget {
			out = e.effort
		}
	}
	return out
}

// encodeThinking folds the neutral reasoning control onto thinking{}.
func encodeThinking(r *canonical.Reasoning, maxTokens int, loss *canonical.LossReport, opt *EncodeOptions) *Thinking {
	if (r.Enabled != nil && !*r.Enabled) || r.Effort == canonical.EffortNone {
		return &Thinking{Type: ThinkingDisabled}
	}
	budget := r.BudgetTokens
	if budget <= 0 {
		if v, ok := effortBudget[r.Effort]; ok {
			budget = v
		}
	}
	if budget <= 0 {
		if r.Enabled == nil {
			// Nothing was actually asked for.
			return nil
		}
		// Enabled with no level and no budget. This family requires a number, so
		// the middle of the table is used rather than a guess at the ceiling.
		budget = effortBudget[canonical.EffortMedium]
	}
	// budget = min(table[effort], max_tokens - reserve), reserve >= 1
	// (DESIGN §10.2). The ceiling is the caller's and is never raised to fit.
	if ceiling := maxTokens - opt.reserve(); budget > ceiling {
		budget = ceiling
	}
	if budget < MinThinkingBudget {
		opt.warn().warn(WarnReasoningDisabled, strconv.Itoa(budget))
		loss.DropParam("thinking")
		return nil
	}
	return &Thinking{Type: ThinkingEnabled, BudgetTokens: ptr(budget)}
}
