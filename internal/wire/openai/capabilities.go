package openai

import "github.com/ziozzang/dorang/internal/canonical"

// StrictCapabilities is what the OpenAI chat-completions format can express
// with no extensions at all.
//
// Notably absent, and each absence is a real loss reported by this package
// rather than a shrug:
//
//   - CapCacheBreakpoints — OpenAI caches automatically and has no breakpoint.
//   - CapDocumentBlocks — no document part in the strict schema.
//   - CapMultiBlockToolResult — a tool message's content is a string.
//   - CapThinkingBlocks — no way to carry a reasoning block back into the
//     conversation, let alone the integrity material attached to one.
//   - CapStructuredSystem — one system message, no per-block attributes.
//   - CapRichStopReasons — five finish_reason values, full stop.
//   - CapTopK, CapPriority — no such parameters.
const StrictCapabilities = canonical.CapMultiBlockContent |
	canonical.CapImageBlocks |
	canonical.CapToolCalls |
	canonical.CapJSONSchema |
	canonical.CapParallelToolCalls |
	canonical.CapReasoningControl |
	canonical.CapSeed |
	canonical.CapLogitBias |
	canonical.CapLogprobs |
	canonical.CapPenalties |
	canonical.CapMultipleChoices |
	canonical.CapUser |
	canonical.CapStopSequences |
	canonical.CapServiceTier |
	canonical.CapMetadata

// DefaultCapabilities adds the extensions that the widely-deployed
// OpenAI-compatible surface actually accepts, and that this encoder emits:
// file parts for documents, cache_control on content parts, array-form content
// on tool messages, and reasoning_content for reasoning text.
//
// It is the default because reporting a structural downgrade for a construct
// dorang can in fact carry would make the 400-on-loss rule of DESIGN §10.1
// fire constantly and train callers to set x-dorang-allow-lossy on everything —
// which is how a safety mechanism becomes noise.
//
// CapThinkingBlocks being present here covers the reasoning *text* only. A
// signature is reported as a downgrade unconditionally, because no OpenAI field
// carries one and dorang never fabricates one (DESIGN §10.2).
const DefaultCapabilities = StrictCapabilities |
	canonical.CapDocumentBlocks |
	canonical.CapCacheBreakpoints |
	canonical.CapMultiBlockToolResult |
	canonical.CapStructuredSystem |
	canonical.CapThinkingBlocks
