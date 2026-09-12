package canonical

import (
	"encoding/json"
	"sort"
	"strings"
)

// Operator-requested parameter removal — DESIGN §10.3's `params.drop[]`.
//
// # Why this is not the capability set
//
// A capability states what a KIND can express. It is declared beside the
// encoder that implements it ([Capability], internal/wire), it is the same for
// every deployment of that kind, and dorang decides it. A drop states what ONE
// upstream instance REJECTS, and that is a fact only the operator has: the same
// `kind: openai` reaches OpenAI itself, a vLLM build from March, a vendor's
// compatibility shim and an internal proxy, and they do not accept the same
// fields.
//
// `max_tokens` is the case that makes the difference concrete. Every encoder
// writes it, no capability bit will ever describe it — a bit means "this
// construct cannot cross", and the ceiling crosses fine everywhere — and an
// upstream that answers 400 to it is a real deployment, not a hypothetical. If
// the drop list were folded into the capability set the operator would have no
// way to say it, which is why the two mechanisms are separate and why the
// removal is reported through the same channel: [LossReport.DropParam], hence
// `x-dorang-dropped-params`.
//
// # Two halves, and both are needed
//
// A name in [dropByName] clears a field dorang MODELS. Every name, modelled or
// not, is also deleted from [Request.Extra] — the pass-through map that carries
// a same-family crossing's unrecognised fields to the wire. Without the second
// half the mechanism would cover only dorang's own vocabulary and would silently
// forward exactly the vendor-specific knob an operator is most likely to be
// fighting with.
//
// # The names that are refused rather than dropped
//
// [CheckDropParam] refuses a name whose removal changes WHAT the model is asked
// rather than HOW it samples. Dropping `tools` is not a parameter drop, it is
// deleting the caller's function calling and answering 200; dropping `messages`
// or `model` is not expressible at all. §10.1 already has a mechanism for a
// construct that cannot cross — it refuses, and `x-dorang-allow-lossy` is how a
// caller consents — and an operator setting that could route around it silently
// would be a second, disagreeing answer to the same question.

// dropRule clears one modelled parameter, reporting whether the request
// actually carried it. "Carried it" is what decides whether the removal is
// reported: naming a parameter in x-dorang-dropped-params that the caller never
// sent reads as "your value was ignored", which is the opposite of what
// happened.
type dropRule func(*Request) bool

// dropByName is the modelled half of the vocabulary.
//
// `max_tokens`, `max_completion_tokens` and `max_output_tokens` are one entry
// three times over, because they are one field three times over:
// internal/wire/openai emits [Request.MaxTokens] under whichever spelling the
// adapter selected — the third is the Responses surface's — and an operator who
// has to name a spelling to get a drop has been asked to know something dorang
// decided. An incumbent configuration lists both chat spellings for the same
// reason, and a Responses-only host that refuses the field ("Unsupported
// parameter: max_output_tokens", measured) is refused in ITS spelling.
var dropByName = map[string]dropRule{
	"max_tokens":            func(r *Request) bool { return clearPtr(&r.MaxTokens) },
	"max_completion_tokens": func(r *Request) bool { return clearPtr(&r.MaxTokens) },
	"max_output_tokens":     func(r *Request) bool { return clearPtr(&r.MaxTokens) },
	"temperature":           func(r *Request) bool { return clearPtr(&r.Temperature) },
	"top_p":                 func(r *Request) bool { return clearPtr(&r.TopP) },
	"top_k":                 func(r *Request) bool { return clearPtr(&r.TopK) },
	"n":                     func(r *Request) bool { return clearPtr(&r.N) },
	"seed":                  func(r *Request) bool { return clearPtr(&r.Seed) },
	"logprobs":              func(r *Request) bool { return clearPtr(&r.Logprobs) },
	"top_logprobs":          func(r *Request) bool { return clearPtr(&r.TopLogprobs) },
	"frequency_penalty":     func(r *Request) bool { return clearPtr(&r.FrequencyPenalty) },
	"presence_penalty":      func(r *Request) bool { return clearPtr(&r.PresencePenalty) },
	"parallel_tool_calls":   func(r *Request) bool { return clearPtr(&r.ParallelToolCalls) },
	"priority":              func(r *Request) bool { return clearPtr(&r.Priority) },
	"stream_options":        func(r *Request) bool { return clearPtr(&r.StreamOptions) },
	"reasoning":             func(r *Request) bool { return clearPtr(&r.Reasoning) },
	"reasoning_effort":      func(r *Request) bool { return clearPtr(&r.Reasoning) },
	"stop": func(r *Request) bool {
		had := len(r.Stop) > 0
		r.Stop = nil
		return had
	},
	"logit_bias": func(r *Request) bool {
		had := len(r.LogitBias) > 0
		r.LogitBias = nil
		return had
	},
	"metadata": func(r *Request) bool {
		had := len(r.Metadata) > 0
		r.Metadata = nil
		return had
	},
	"user": func(r *Request) bool {
		had := r.User != ""
		r.User = ""
		return had
	},
	"service_tier": func(r *Request) bool {
		had := r.ServiceTier != ""
		r.ServiceTier = ""
		return had
	},
}

// clearPtr zeroes a pointer field and reports whether it was set.
func clearPtr[T any](p **T) bool {
	had := *p != nil
	*p = nil
	return had
}

// undroppable names the parameters an operator may not drop, each with the
// reason. A refusal that only says "no" makes the operator guess which of two
// mechanisms they were supposed to reach for.
var undroppable = map[string]string{
	"model":    "the model is the routing decision itself; change models[].deployments[].upstream_model",
	"messages": "removing the conversation is not a parameter drop",
	"system":   "removing the system prompt is not a parameter drop",
	"prompt":   "removing the prompt is not a parameter drop",
	"input":    "removing the input is not a parameter drop",
	"stream":   "the streaming mode is the response contract, not a sampling knob",
	"tools": "dropping tools deletes the caller's function calling and answers 200. " +
		"A construct a deployment cannot express is refused by §10.1, and x-dorang-allow-lossy " +
		"is how a caller consents to losing it",
	"tool_choice": "dropping tool_choice changes which tools may be called. See §10.1 and " +
		"x-dorang-allow-lossy",
	"response_format": "dropping response_format changes the shape of the answer. §10.1 " +
		"downgrades json_schema on a deployment that cannot express it, and says so",
	"previous_response_id": "the Responses chain is dorang's own state (§9.2), not a caller parameter",
	"store":                "the retention flag is dorang's own state (§9.2), not a caller parameter",
}

// CheckDropParam validates one entry of an operator's drop list. A nil return
// means the name may be dropped.
//
// An unknown name is deliberately ACCEPTED: [Request.Extra] carries every field
// dorang does not model straight to the wire on a same-family crossing, so a
// vendor-specific knob is exactly the case the list exists for, and refusing
// what dorang has no struct field for would refuse the majority of real uses.
func CheckDropParam(name string) error {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return &DropParamError{Name: name, Reason: "must not be empty"}
	case trimmed != name:
		return &DropParamError{Name: name,
			Reason: "must not be padded with whitespace: it is compared to a wire field name byte for byte"}
	}
	if why, refused := undroppable[name]; refused {
		return &DropParamError{Name: name, Reason: why}
	}
	return nil
}

// DropParamError is a rejected drop-list entry.
type DropParamError struct {
	Name   string
	Reason string
}

func (e *DropParamError) Error() string { return e.Name + ": " + e.Reason }

// ModelledDropParam reports whether name clears a field dorang models, as
// opposed to one that can only be removed from [Request.Extra].
//
// It exists for the importer: a drop that can only reach Extra is honoured on a
// same-family crossing and is a no-op on a converting one, and an operator
// migrating a load-bearing setting is owed that distinction rather than a
// success message.
func ModelledDropParam(name string) bool { _, ok := dropByName[name]; return ok }

// ModelledDropParams lists every name that clears a modelled field, sorted.
func ModelledDropParams() []string {
	out := make([]string, 0, len(dropByName))
	for k := range dropByName {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DropParams removes every named parameter from r and returns the names the
// request actually carried, in the order they were asked for.
//
// It mutates r, which is safe only on a copy. internal/backend calls it on the
// per-hop copy [prepareRequest] already makes, because a fail-back hop lands on
// a different deployment with a different list and must re-derive from the
// caller's original.
func (r *Request) DropParams(names []string) []string {
	if r == nil || len(names) == 0 {
		return nil
	}
	var dropped []string
	for _, name := range names {
		hit := false
		if rule, ok := dropByName[name]; ok {
			hit = rule(r)
		}
		// Extra is consulted for EVERY name, modelled or not. A field dorang
		// models can also arrive in Extra under a different spelling, and a
		// field it does not model is only ever here.
		if _, ok := r.Extra[name]; ok {
			r.Extra = deleteExtra(r.Extra, name)
			hit = true
		}
		if hit {
			dropped = append(dropped, name)
		}
	}
	return dropped
}

// deleteExtra removes one key, copying first. The caller's own map is shared
// with the retained request body and with any concurrent fail-back hop; deleting
// in place would edit what the next hop is supposed to re-derive from.
func deleteExtra(in map[string]json.RawMessage, key string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		if k != key {
			out[k] = v
		}
	}
	return out
}
