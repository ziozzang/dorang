// Package canonical is dorang's protocol-neutral intermediate representation.
//
// # Why it exists
//
// Frontends decode a client's wire format into a [Request]; backends encode a
// [Request] into an upstream wire format. N frontends and M backends therefore
// cost N+M adapters instead of N×M, which is what makes "full protocol"
// affordable (DESIGN §10.1). Conversion is required in both directions: an
// Anthropic-shaped request routinely targets an OpenAI-shaped backend and the
// reverse happens just as often.
//
// The shape here is the *union* of the OpenAI chat and Anthropic messages
// models, not the intersection. An intersection would make the representation
// itself lossy, so every crossing would lose information twice and dorang could
// not even report what it lost.
//
// # Content is a list of blocks, never a string
//
// [Message.Content] is an ordered []Block. A plain string collapses to exactly
// one text block, and [Content.Plain] recovers it, but the list is the model.
// [ToolResult.Content] is likewise a list, because the Anthropic form allows
// text plus images inside a single tool result and flattening that to a string
// destroys the images silently.
//
// # Two kinds of loss (DESIGN §10.1)
//
// A conversion can lose two very different things and they must not be reported
// through the same channel:
//
//   - Droppable parameters — a knob the backend does not have (seed, logit_bias,
//     top_logprobs, an unsupported reasoning control). The request still means
//     what it meant. Reported by name in x-dorang-dropped-params.
//
//   - Structural downgrades — a construct that cannot be expressed at all (a
//     document block, a cache breakpoint, a multi-block tool result). Returning
//     200 after silently discarding a PDF or destroying a caching strategy is
//     worse than an error, because the caller has no way to find out.
//
// [Capability] is the vocabulary for both. [Request.RequiredCapabilities]
// reports what a request actually uses so the router can prefer a backend that
// can express it (capability as a routing filter); [Request.Downgrades] reports,
// against a given backend's capability set, exactly which constructs would be
// destroyed and where. [LossReport] accumulates both kinds during an encode.
//
// # Model names are opaque
//
// [Request.Model] is an opaque string. Nothing in dorang splits it on ':' or
// '/' or any other character (DESIGN §2.1). "gemma4:31b", "zai:glm-5.1",
// "deepseek-v4-flash:cloud" and "qwen3.5:397b" are single names.
package canonical
