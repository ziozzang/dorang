package anthropic

import (
	"crypto/rand"
	"strconv"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
)

// TotalTokensMode selects COMPATIBILITY 6.8's asymmetry.
type TotalTokensMode string

const (
	// TotalTokensCompat is the default and reproduces the reference proxy:
	// non-streaming responses carry a non-spec usage.total_tokens and streaming
	// responses do not, so the two shapes differ by exactly one field. It is
	// compat.anthropic_total_tokens in configuration.
	TotalTokensCompat TotalTokensMode = ""
	// TotalTokensOmit is the strict vendor shape: no total_tokens anywhere.
	TotalTokensOmit TotalTokensMode = "omit"
)

// ReasoningHeader reports how reasoning content reached the caller
// (DESIGN §10.2). [ReasoningDerived] means it was normalized from another
// family's plain-text reasoning field and carries no integrity material — so
// echoing it back on the next turn will not work, and the caller can see that
// on turn one instead of discovering it on turn two.
const (
	ReasoningHeader  = "x-dorang-reasoning"
	ReasoningDerived = "derived"
	ReasoningNative  = "native"
)

// ResponseOptions controls a canonical -> Anthropic response conversion.
type ResponseOptions struct {
	// ID and Model override the neutral values. Model in particular is the
	// CLIENT-FACING name: the body never carries the upstream id (DESIGN §7.2).
	ID    string
	Model string

	// Capabilities is what the client's view can express. Zero means
	// [DefaultCapabilities].
	Capabilities canonical.Capability
	Loss         *canonical.LossReport

	// StopReason selects 6.4's collapse or this family's own enumeration.
	StopReason StopReasonMode
	// TotalTokens selects 6.8's non-spec field.
	TotalTokens TotalTokensMode

	Warn WarnFunc
}

func (o *ResponseOptions) caps() canonical.Capability {
	c := DefaultCapabilities
	if o != nil && o.Capabilities != 0 {
		c = o.Capabilities
	}
	if o.stopMode() == StopReasonNative {
		// With the collapse off, this family's own enumeration is reachable and
		// a rich stop reason is no longer a loss.
		c |= canonical.CapRichStopReasons
	}
	return c
}

func (o *ResponseOptions) loss() *canonical.LossReport {
	if o == nil {
		return nil
	}
	return o.Loss
}

func (o *ResponseOptions) warn() WarnFunc {
	if o == nil {
		return nil
	}
	return o.Warn
}

func (o *ResponseOptions) stopMode() StopReasonMode {
	if o == nil {
		return StopReasonCollapse
	}
	return o.StopReason
}

func (o *ResponseOptions) totalMode() TotalTokensMode {
	if o == nil {
		return TotalTokensCompat
	}
	return o.TotalTokens
}

// EncodedResponse is a wire response plus the out-of-band facts the HTTP layer
// must turn into headers.
//
// It exists because 6.4's collapse is only defensible WITH the header: the wire
// says end_turn, the header says content_filter, and a caller who wants to
// distinguish them has a way. Returning only the body would make the collapse a
// lie rather than a compatibility choice.
type EncodedResponse struct {
	Response *Response
	// NativeStopReason is empty when the wire value already told the truth.
	// Non-empty means it goes in [NativeStopReasonHeader].
	NativeStopReason string
	// ReasoningDerived reports that a reasoning block was emitted without
	// integrity material, for [ReasoningHeader].
	ReasoningDerived bool
}

// MarshalResponse encodes a neutral response as Anthropic response bytes.
func MarshalResponse(r *canonical.Response, opt *ResponseOptions) ([]byte, error) {
	enc, err := EncodeResponse(r, opt)
	if err != nil {
		return nil, err
	}
	return marshalAppender(enc.Response)
}

// EncodeResponse converts a neutral response to the Messages wire shape.
func EncodeResponse(r *canonical.Response, opt *ResponseOptions) (*EncodedResponse, error) {
	if r == nil {
		return nil, errNilResponse
	}
	caps := opt.caps()
	loss := opt.loss()

	// Unmodelled members are forwarded only when the answer is going back out in
	// the shape it arrived in. A chat completion's `timings` has no meaning here
	// and inventing it is the compatibility break §10.7 exists to prevent.
	same := r.SameFamily(canonical.FamilyAnthropicMessages)

	out := &Response{
		ID:    r.ID,
		Type:  TypeMessage,
		Role:  RoleAssistant,
		Model: r.Model,
		// stop_sequence is null on the adapter path (COMPATIBILITY 6.5). The
		// field is present and null, not absent.
		StopSequence: nil,
	}
	if same {
		out.Extra = r.Extra
	}
	if opt != nil {
		if opt.ID != "" {
			out.ID = opt.ID
		}
		if opt.Model != "" {
			out.Model = opt.Model
		}
	}
	if out.ID == "" {
		out.ID = NewMessageID()
	}
	if r.Usage != nil {
		out.Usage = EncodeUsage(*r.Usage, opt.totalMode())
		if same && r.UsageExtra != nil {
			out.Usage.Extra = r.UsageExtra.Usage
		}
	}

	enc := &EncodedResponse{Response: out}

	if len(r.Choices) > 1 {
		// One message, one turn. n > 1 has no shape here and COMPATIBILITY 10
		// marks multi-choice as unspecified, so it is reported and not guessed.
		//
		// A DOWNGRADE, not a dropped param, and the distinction is the whole
		// difference between the two channels. [canonical.CapMultipleChoices] is
		// Material — the caller is handed a different answer, not the same answer
		// with a knob unapplied — and the REQUEST half already says so at
		// [canonical.Request.MaterialLoss], which raises exactly this construct
		// with exactly this detail. Reporting it here as droppable was one
		// construct with two classifications: `n: 4` refused with a 400 on the
		// way out, and the same fact whispered into x-dorang-dropped-params on
		// the way back, where a client that consented to the loss reads the
		// header that means "a knob was not applied".
		loss.Downgrade(canonical.ConstructMultipleChoices,
			"n: "+strconv.Itoa(len(r.Choices))+" choice(s) collapsed to one message")
	}
	out.Content = []ContentBlock{}
	if len(r.Choices) > 0 {
		c := &r.Choices[0]
		blocks, derived := encodeResponseBlocks(c.Message.Content, caps, loss, opt, "content")
		if c.Message.Refusal != "" {
			loss.DropParam("refusal")
			blocks = append(blocks, ContentBlock{Type: BlockText, Text: ptr(c.Message.Refusal)})
		}
		if blocks != nil {
			out.Content = blocks
		}
		enc.ReasoningDerived = derived

		wire, native := StopReasonOf(c.StopReason, opt.stopMode())
		if native == "" {
			native = c.NativeStopReason
		}
		if wire != "" {
			out.StopReason = ptr(wire)
		}
		if native != "" && native != wire {
			enc.NativeStopReason = native
			opt.warn().warn(WarnCollapsedStopReason, native)
			if !caps.Has(canonical.CapRichStopReasons) && !isExpressibleHere(c.StopReason) {
				loss.Downgrade(canonical.ConstructRichStopReason, "stop_reason: "+native)
			}
		}
		out.StopSequence = StopSequenceValue(c.StopReason, "", opt.stopMode())
	}
	return enc, nil
}

// isExpressibleHere reports whether this family's own enumeration can name the
// reason without collapsing it onto a different meaning.
func isExpressibleHere(r canonical.StopReason) bool {
	switch r {
	case canonical.StopUnspecified, canonical.StopEndTurn, canonical.StopMaxTokens,
		canonical.StopToolUse, canonical.StopStopSequence, canonical.StopRefusal,
		canonical.StopPauseTurn:
		return true
	}
	return false
}

// encodeResponseBlocks renders an assistant turn.
//
// It differs from the request path in exactly one place, and the difference is
// DESIGN §10.2's two halves. On the way OUT to a caller, reasoning content is
// normalized best-effort and marked derived when it carries no integrity
// material. On the way IN to a backend it is refused instead, because a block
// the backend cannot verify is not a degraded block, it is an invalid one.
func encodeResponseBlocks(content canonical.Content, caps canonical.Capability, loss *canonical.LossReport, opt *ResponseOptions, where string) ([]ContentBlock, bool) {
	if len(content) == 0 {
		return nil, false
	}
	// encodeBlock needs an EncodeOptions for nested tool-result content; the
	// response path never opts into lossy conversion, because there is nothing
	// to refuse here.
	eo := &EncodeOptions{Capabilities: caps, Loss: loss, Warn: opt.warn()}
	out := make([]ContentBlock, 0, len(content))
	derived := false
	for i := range content {
		b := &content[i]
		at := where + "[" + strconv.Itoa(i) + "]"
		if b.Kind == canonical.KindThinking {
			if !caps.Has(canonical.CapThinkingBlocks) {
				loss.Downgrade(canonical.ConstructThinkingBlock, at)
				continue
			}
			if b.Thinking != nil && b.Thinking.Redacted {
				data := redactedData(b.Extra)
				if data == "" {
					// The payload IS the block: an empty one would be a
					// fabrication, and the text was withheld so there is nothing
					// to show instead.
					loss.Downgrade(canonical.ConstructThinkingBlock, at+": redacted payload lost")
					continue
				}
				out = append(out, ContentBlock{Type: BlockRedactedThinking, Data: data, Extra: b.Extra})
				continue
			}
			blk := ContentBlock{Type: BlockThinking, Thinking: ptr(b.Text), Extra: b.Extra}
			if b.Thinking != nil && b.Thinking.Signature != "" {
				blk.Signature = b.Thinking.Signature
			} else {
				derived = true
			}
			out = append(out, blk)
			continue
		}
		blk, ok, err := encodeBlock(b, caps, loss, eo, at)
		if err != nil || !ok {
			continue
		}
		out = append(out, blk)
	}
	return out, derived
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

// ExclusiveInputTokens applies COMPATIBILITY 6.7's formula:
//
//	input_tokens = prompt - cache_read - cache_creation, clamped at zero
//
// canonical.Usage.InputTokens is the full prompt count (see the package
// comment); this family reports the part of it that was neither read from nor
// written to cache. The clamp matters because a backend that reports a cached
// count larger than its own prompt count — which happens — must not produce a
// negative token count on an invoice.
func ExclusiveInputTokens(u canonical.Usage) int {
	n := u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens
	if n < 0 {
		return 0
	}
	return n
}

// EncodeUsage renders neutral counts on the wire.
//
// COMPATIBILITY 6.7: cache fields appear ONLY when greater than zero. A zeroed
// cache breakdown on every response is noise that some clients render as
// "0 cached tokens" on a backend that has no cache at all.
//
// COMPATIBILITY 6.8: total_tokens is not in the vendor specification and is
// emitted here — on the non-streaming shape — because the reference proxy emits
// it. [TotalTokensOmit] turns it off.
func EncodeUsage(u canonical.Usage, mode TotalTokensMode) *Usage {
	w := &Usage{
		InputTokens:  ptr(ExclusiveInputTokens(u)),
		OutputTokens: ptr(u.OutputTokens),
	}
	// COMPATIBILITY 6.7's ">0 only" rule still governs a count dorang produced
	// itself, which reports nothing. A count the BACKEND stated is emitted as
	// stated, zero included: 6.7 records that the vendor emits explicit zeros
	// and that a golden captured from the vendor therefore will not match one
	// captured from a proxy — this is that gap, and dropping a measured zero was
	// the wrong half of it to keep.
	if u.CacheWriteTokens > 0 || u.Reports(canonical.UsageCacheWrite) {
		w.CacheCreationInputTokens = ptr(u.CacheWriteTokens)
	}
	if u.CacheReadTokens > 0 || u.Reports(canonical.UsageCacheRead) {
		w.CacheReadInputTokens = ptr(u.CacheReadTokens)
	}
	if mode != TotalTokensOmit {
		// The neutral total is inclusive input plus output, which is also the
		// sum of the four fields above — the two readings agree, and a client
		// adding up what it sees gets the same number.
		w.TotalTokens = ptr(u.TotalTokens())
	}
	return w
}

// NewMessageID generates a message id.
func NewMessageID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a time-derived fallback keeps
		// the message identifiable rather than panicking mid-response.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (8 * (i % 8)))
		}
	}
	const prefix = "msg_"
	out := make([]byte, len(prefix)+len(b)*2)
	copy(out, prefix)
	for i, v := range b {
		out[len(prefix)+i*2] = hexDigits[v>>4]
		out[len(prefix)+i*2+1] = hexDigits[v&0xf]
	}
	return string(out)
}

const hexDigits = "0123456789abcdef"
