package backend

import (
	"encoding/json"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
)

// prepareRequest applies everything that depends on the chosen deployment
// rather than on the caller: the priority field, the service tier, and the
// self-hosted engine normalizations of DESIGN §4.4.
//
// It copies rather than mutates. A fail-back hop re-prepares from the caller's
// original request, and a hop that inherited the previous engine's priority
// would send vLLM's number to SGLang — which is not a degradation but an
// inversion (§7.5).
func prepareRequest(x *exchange) *canonical.Request {
	src := x.call.Request
	if src == nil {
		return nil
	}
	req := *src
	engine := x.prov.engine

	// The engine's own priority field, ALREADY direction-normalized for this
	// engine by internal/router (§7.5). Extra is the neutral request's
	// pass-through map, so the number reaches the wire without either encoder
	// needing to know the field exists.
	//
	// Nothing here negates, offsets or clamps. The canonical value and the
	// per-engine direction are the router's, and a second implementation of the
	// negation is how two engines that must disagree end up agreeing.
	if f := x.target.PriorityField; f != "" {
		req.Extra = cloneExtra(req.Extra)
		req.Extra[f] = json.RawMessage(strconv.Itoa(x.target.Priority))
	}

	if engine.SelfHosted() {
		// service_tier is never sent to a self-hosted engine, in either
		// direction of the argument:
		//
		//   - As a priority fallback it is a lie. vLLM accepts the field and
		//     has zero consumers for it (VLLM.md §1.2); SGLang does not have
		//     the field on chat/completions at all and drops it (SGLANG.md
		//     §6.13). A gateway that emitted it to express urgency would be
		//     emitting nothing, silently, with a 200 to show for it.
		//   - As a relayed caller field it is noise the engine ignores, and
		//     §4.4's single-surface commitment is that a caller cannot tell
		//     which engine is behind a model. A field that means something on
		//     one backend and nothing on another is exactly that tell.
		req.ServiceTier = ""

		// VLLM.md §2.1: an explicit tool_choice: null alongside tools passes
		// validation, skips the auto-injection that would have set "auto", and
		// then takes a branch that never invokes the tool parser. The client
		// receives plain content, no tool calls, and NO error. SGLang normalizes
		// the same input to "auto" (SGLANG.md §6.9), so setting it explicitly is
		// correct on both and load-bearing on one.
		//
		// Only the absent case is filled. "none" is left alone: on SGLang it
		// strips the schemas from the prompt entirely, which is a real semantic
		// difference a caller may be relying on.
		if len(req.Tools) > 0 && req.ToolChoice == nil {
			req.ToolChoice = &canonical.ToolChoice{Mode: canonical.ToolChoiceAuto}
		}
	} else if t := x.target.PriorityTier; t != "" {
		// The non-numeric fold of §7.5 — OpenAI's service_tier. dorang's class
		// wins over a caller's own value: §10.5 makes priority an operator
		// grant rather than a caller claim, and honouring the caller's field
		// here would be the self-elevation that section refuses.
		req.ServiceTier = t
	}
	return &req
}

// engineReasoningField is the response field each engine puts reasoning text in.
//
// The two disagree, using each other's spelling for the request field, which is
// the kind of difference §4.4 exists to absorb:
//
//   - vLLM's RESPONSE field is `reasoning` (VLLM.md §2.5), though it still
//     accepts `reasoning_content` on requests.
//   - SGLang uses `reasoning_content` both ways (SGLANG.md §6.10, §7's
//     ReasoningText row).
//
// internal/wire/openai models `reasoning_content` and preserves anything else
// in Extra, so SGLang is already handled and vLLM's name is what needs
// adopting. Without this, reasoning text from a vLLM deployment reaches an
// Anthropic-speaking caller as nothing at all, and an OpenAI-speaking caller as
// an unrecognised passthrough field rather than the field their client reads.
const (
	engineReasoningVLLM   = "reasoning"
	engineReasoningSGLang = "reasoning_content"
)

// adoptEngineReasoning promotes an engine-specific reasoning field into a
// thinking block, so §10.2's reverse mapping has something to map.
//
// It runs for self-hosted engines only. Adopting an unknown provider's
// `reasoning` field would be a guess about a shape nobody checked, and a field
// dorang does not recognise is relayed rather than reinterpreted (§10.5a).
func adoptEngineReasoning(r *canonical.Response, engine Engine) {
	if r == nil || !engine.SelfHosted() {
		return
	}
	for i := range r.Choices {
		m := &r.Choices[i].Message
		raw, ok := m.Extra[engineReasoningVLLM]
		if !ok {
			continue
		}
		// The key is removed whether or not it holds text: leaving it would
		// relay the engine's own spelling beside the normalized one, and a
		// client would see the same reasoning twice under two names.
		delete(m.Extra, engineReasoningVLLM)
		text := jsonString(raw)
		if text == "" {
			continue
		}
		if hasThinking(m.Content) {
			continue
		}
		// Reasoning precedes the visible output, so it goes at the front of the
		// block list — the same position internal/wire/openai gives
		// reasoning_content.
		m.Content = append(canonical.Content{canonical.ThinkingBlock(text, "")}, m.Content...)
	}
}

// hasThinking reports whether a block list already carries reasoning.
func hasThinking(c canonical.Content) bool {
	for i := range c {
		if c[i].Kind == canonical.KindThinking {
			return true
		}
	}
	return false
}

// jsonString decodes a JSON value that should be a string, returning "" for
// anything else rather than failing: a backend that puts an object there is a
// backend whose reasoning dorang does not carry, not a failed request.
func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// cloneExtra copies a pass-through map so the caller's request is not mutated.
func cloneExtra(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}
