package anthropic

import "github.com/ziozzang/dorang/internal/canonical"

// StrictCapabilities is what the Messages format can express on the adapter
// path of COMPATIBILITY §6.
//
// It is the near-mirror of the OpenAI set: everything the other family loses
// structurally is native here, and what it gains instead is a list of knobs
// this family simply does not have.
//
// Notably absent, and each absence is a real loss this package reports rather
// than a shrug:
//
//   - CapRichStopReasons — 6.4 collapses stop_reason to THREE values on the
//     adapter path, so a filtered turn is reported as a normal end. The true
//     reason is surfaced in [NativeStopReasonHeader] instead. Turning the
//     collapse off with [StopReasonNative] restores the capability, which is
//     why [ResponseOptions.caps] adds the bit back in that mode.
//   - CapJSONSchema — there is no response_format. A schema is expressed here
//     by forcing a tool, which is a rewrite of the request, not a translation
//     of it, so dorang reports the loss instead of inventing one.
//   - CapSeed, CapLogitBias, CapLogprobs, CapPenalties, CapMultipleChoices,
//     CapServiceTier — no such parameters.
const StrictCapabilities = canonical.CapMultiBlockContent |
	canonical.CapImageBlocks |
	canonical.CapDocumentBlocks |
	canonical.CapCacheBreakpoints |
	canonical.CapThinkingBlocks |
	canonical.CapMultiBlockToolResult |
	canonical.CapStructuredSystem |
	canonical.CapToolCalls |
	canonical.CapParallelToolCalls |
	canonical.CapReasoningControl |
	canonical.CapTopK |
	canonical.CapMetadata |
	canonical.CapUser |
	canonical.CapStopSequences

// DefaultCapabilities is what an encoder assumes when the caller declared
// nothing.
//
// It equals [StrictCapabilities]: unlike the OpenAI side, there is no widely
// accepted extension set to add. CapPriority is excluded even though a
// deployment may accept a priority hint, because assuming an unverified
// capability is exactly what DESIGN §10.2 forbids — an unverified control is
// omitted and reported, not guessed.
const DefaultCapabilities = StrictCapabilities
