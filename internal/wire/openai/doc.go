// Package openai encodes and decodes the OpenAI chat-completions wire format.
//
// It is used in both directions: as a frontend (client bytes -> canonical) and
// as a backend (canonical -> upstream bytes, upstream bytes -> canonical). The
// same types serve both, so a shape that decodes must also encode.
//
// # What the tests are actually protecting
//
// Every exported behavior here corresponds to a numbered row of
// docs/COMPATIBILITY.md, and the numbers appear in the comments so a change can
// be traced back to the contract it breaks. The rows that bite hardest:
//
//   - 2.1 Absent fields are OMITTED, never emitted as null. A Go struct written
//     without pointers-plus-omitempty produces "logprobs":null and
//     "system_fingerprint":null and diverges from every existing client, while
//     passing any test that decodes its own output. [TestMinimalChunkGolden]
//     asserts the exact byte string for that reason.
//
//   - 2.4/2.5 id and created are pinned across the whole stream; model is
//     restamped on every chunk. Both are properties of the stream, not of a
//     chunk, so they live in [StreamWriter] and not in [Chunk].
//
//   - 4.4 A terminal chunk is SYNTHESIZED when the backend never sent one, and
//     it is "tool_calls" — not "stop" — if any tool call was seen. A gateway
//     that merely forwards breaks every agentic client on this one line.
//
//   - 3.3 The usage chunk carries "choices":[{"index":0,"delta":{}}], following
//     the widely-deployed reference proxy rather than OpenAI's "choices":[].
//     [UsageChunkChoicesEmpty] switches to the strict shape.
//
// # Relay path
//
// [Scanner] is the streaming relay (DESIGN §7.2, §15.2.3). It is line-oriented
// and single-pass, never decodes a frame, rewrites only the model value, and
// degrades to a plain copy when there is nothing to rewrite. It exists because
// the alias rewrite of §7.2 is mandatory on every frame (2.5) and a JSON decode
// per frame would put an allocation and a parse on the hot path.
//
// # Model names
//
// Model names are opaque strings. Nothing in this package splits one on ':' or
// '/'. "gemma4:31b", "zai:glm-5.1", "deepseek-v4-flash:cloud" and "qwen3.5:397b"
// are single names and are frozen as golden tests.
package openai
