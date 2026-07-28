// Package anthropic encodes and decodes the Anthropic Messages wire format.
//
// It is used in both directions: as a frontend (client bytes -> canonical) and
// as a backend (canonical -> upstream bytes, upstream bytes -> canonical). The
// same types serve both, so a shape that decodes must also encode.
//
// # What the tests are actually protecting
//
// Every exported behavior corresponds to a numbered row of
// docs/COMPATIBILITY.md §6, and the numbers appear in the comments so a change
// can be traced back to the contract it breaks. The rows that bite hardest:
//
//   - 6.1 The framing is "event: <type>\ndata: <json>\n\n" — BOTH lines. A
//     chat-completions writer that only emits the data line produces a stream
//     the Anthropic SDK silently discards, because it dispatches on the event
//     name.
//
//   - 6.2 Six event types, no ping, no [DONE], message_stop exactly once.
//
//   - 6.6 Content-block indexing is stateful. This protocol has EXPLICIT block
//     boundaries and chat-completions has implicit ones, so crossing in means
//     synthesizing content_block_stop -> content_block_start pairs at every
//     transition and HOLDING the terminal message_delta until the open block is
//     closed (DESIGN §10.7 calls this the costliest asymmetry). [Emitter] is
//     that state machine, separated from the framing so it can be fuzzed
//     directly: [FuzzEmitter].
//
//   - 6.7 Cache counters are seeded at zero in message_start and filled by the
//     final message_delta, cache fields appear only when greater than zero, and
//     input_tokens = prompt - cache_read - cache_creation, clamped at zero.
//
// # Token accounting
//
// This is the part that produces a wrong invoice rather than an error, so it is
// stated once, here. [canonical.Usage.InputTokens] is INCLUSIVE — the full
// prompt count, cache reads and cache writes included — which is what
// internal/canonical documents and what internal/wire/openai implements. The
// Anthropic wire is EXCLUSIVE. Therefore:
//
//	decode:  canonical.InputTokens = input_tokens + cache_read + cache_creation
//	encode:  input_tokens = max(0, canonical.InputTokens - cache_read - cache_creation)
//
// Doing this in only one direction mis-counts every cached request, which in an
// agentic workload is most of them. [TestUsageBothDirections] tests both.
//
// # Opaque state
//
// A thinking block's signature, and a redacted_thinking block's data, are
// integrity material whose correctness is established on the server that minted
// it (DESIGN §10.2, EXTENSIONS §B). This package stores and replays them
// byte-identically and never fabricates one. An assistant thinking block that
// arrives without either — because it was derived from another family's
// plain-text reasoning field — cannot be sent to this family at all, and that is
// an [OpaqueError], not a silent drop. The failure lands on turn two of a
// tool-use exchange, which is why [TestTwoTurnToolUseConformance] exists.
//
// # context_management
//
// Relayed, never modelled. It is object-shaped here and array-shaped on the
// OpenAI Responses surface (EXTENSIONS §A.4a), so a neutral type keyed by name
// would be wrong for one of them. It rides in Extra, byte-identical, in both
// directions.
//
// # Model names
//
// Model names are opaque strings. Nothing in this package splits one on ':' or
// '/'.
package anthropic
