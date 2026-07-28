// Package fake provides configurable fake upstream backends that reproduce the
// wire contracts of docs/COMPATIBILITY.md.
//
// # Why the bytes are hand-written here
//
// The frames below are assembled from types declared in THIS package, not by
// calling internal/wire. That is deliberate and it is the whole value of the
// package: a fake built on the encoder under test agrees with it by
// construction, so every scenario that passes against such a fake proves
// nothing about the encoder. Two independent encoders fed the same logical
// content must produce the same bytes, and [selfcheck_test.go] asserts exactly
// that against internal/wire/openai and internal/wire/anthropic. When the fake
// drifts, the self-check fails loudly instead of the scenarios passing quietly.
//
// # What is reproduced
//
// OpenAI chat completions:
//   - 1.1 every frame is exactly "data: <json>\n\n"; no event: line, no id: line
//   - 1.2 the stream terminates with "data: [DONE]\n\n"
//   - 1.3 a mid-stream error is in band, followed by [DONE]
//   - 1.4 an empty upstream stream is an empty body with NO [DONE]
//   - 2.1 absent fields are omitted, never null
//   - 2.1a compact separators, raw UTF-8, HTML escaping OFF
//   - 2.3 object is always "chat.completion.chunk"
//   - 2.4 id and created are pinned across every frame of one stream
//   - 3.1 a usage chunk only when stream_options.include_usage is exactly true
//   - 3.3 the usage chunk carries choices:[{"index":0,"delta":{}}]
//   - 4.4 a terminal chunk is synthesized when the script gave none, defaulting
//     to "stop" and UPGRADED to "tool_calls" when a tool call was seen
//   - 5.1 delta.tool_calls[].index is present on every fragment
//
// Anthropic messages:
//   - 6.1 framing is "event: <type>\ndata: <json>\n\n" — both lines
//   - 6.2 six event types plus error; no ping, no [DONE], message_stop once,
//     and no message_stop after an error
//   - 6.5 stop_sequence is null
//   - 6.6 content_block_stop always precedes the terminal message_delta
//   - 6.7 cache fields appear only when greater than zero; input_tokens on the
//     wire is cache-EXCLUSIVE
//   - 6.8 non-streaming carries the non-spec usage.total_tokens, streaming does
//     not
//
// # Injectable behaviours
//
// [Behaviour] carries latency, TTFT, inter-frame delay (slow generation), error
// status with retry-after, quota exhaustion, mid-stream failure, and silent
// truncation. [Options.Behaviour] is a function of the recorded request, so a
// test can make the first attempt fail and the second succeed without any
// shared mutable state of its own.
package fake
