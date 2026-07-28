package router

// Direction is which way an engine's scheduler reads a priority number.
//
// This is the field DESIGN §7.5 calls "a required property of the emit rule,
// not a detail", and the reason is worth restating where the code is: vLLM and
// SGLang use the SAME field name, the SAME type, and disagree about what the
// number means. vLLM schedules the lowest value first; SGLang schedules the
// highest first by default. Both return 200 either way and nothing in either
// response reveals which reading applied. Sending one shared constant to both
// does not degrade priority on one of them — it INVERTS it, so batch outranks
// realtime, silently and permanently.
//
// Citations: VLLM.md §1.2 (queue docstring, the min-heap comparison operator,
// and the config documentation all agree on lowest-first) and SGLANG.md §3.2
// (priority_sign at schedule_policy.py:181, the ascending sort at :376-381, the
// help text at server_args.py:810, and the missing-priority fill at
// scheduler.py:2469-2473, which uses the *worst* value in both directions).
type Direction uint8

const (
	// Ascending schedules the lowest value first. This is the canonical
	// direction, and vLLM's, so the canonical value travels unchanged.
	Ascending Direction = iota
	// Descending schedules the highest value first. This is SGLang's, and the
	// adapter negates the canonical value for it.
	Descending
)

// String returns the configuration spelling of the direction.
func (d Direction) String() string {
	if d == Descending {
		return "descending"
	}
	return "ascending"
}

// ParseDirection decodes a configured emit direction.
func ParseDirection(s string) (Direction, bool) {
	switch s {
	case "ascending":
		return Ascending, true
	case "descending":
		return Descending, true
	}
	return Ascending, false
}

// EmitRule is how one engine is told a priority (DESIGN §7.5).
type EmitRule struct {
	// Field is the request field the number goes in ("priority"). Empty means
	// this engine takes no native field and receives only the header.
	Field string
	// Direction is which way this engine reads the number. Getting it wrong is
	// an inversion, not a degradation.
	Direction Direction
	// Base offsets a descending engine's wire value: wire = Base - canonical.
	//
	// Zero, the default, is the plain negation DESIGN §7.5 specifies, which
	// maps the canonical scale onto the non-positive half-line. That is
	// internally consistent — dorang's own classes order correctly against each
	// other — but it is not competitive with a co-tenant that sends a naive
	// positive priority to the same SGLang server, because any positive value
	// outranks all of dorang's traffic including realtime. An operator sharing
	// an engine sets Base above the largest priority any other client sends.
	Base int
	// Map folds a class name onto a non-numeric field, e.g. OpenAI's
	// service_tier. When it matches, the string is carried on the decision and
	// the numeric field is not emitted.
	Map map[string]string
}

// PriorityConfig maps dorang's priority classes onto what each backend
// understands (DESIGN §7.5).
type PriorityConfig struct {
	// Classes maps a class name to its canonical value. The canonical scale is
	// LOWER IS MORE URGENT, matching the majority convention and §7.5's own
	// example {realtime: 0, interactive: 2, batch: 10}.
	Classes map[string]int
	// Default names the class used by a request that names none.
	Default string
	// Emit maps a provider kind to its emit rule.
	Emit map[string]EmitRule
	// Header is the vendor-neutral header every backend receives. It is
	// harmless to an engine that ignores it, which is why an unknown backend
	// gets only this.
	Header string
	// Min and Max clamp a client's priority hint to the principal's permitted
	// range (§10.5). They are canonical values, so Min is the MOST urgent the
	// caller may ask for. Both zero means hints are not accepted and the class
	// alone decides.
	Min, Max int
}

// DefaultPriority is the mapping of DESIGN §7.5, including the two self-hosted
// engines' opposite directions.
//
// The class values are spaced two apart deliberately. vLLM's /v1/responses
// decrements priority by one after each built-in-tool round trip (VLLM.md
// §1.2), so bands closer than two would let turn 2 of a tool loop cross into
// the band above the one the caller was granted.
func DefaultPriority() PriorityConfig {
	return PriorityConfig{
		Classes: map[string]int{"realtime": 0, "interactive": 2, "batch": 10},
		Default: "interactive",
		Emit: map[string]EmitRule{
			"vllm":   {Field: "priority", Direction: Ascending},
			"sglang": {Field: "priority", Direction: Descending},
			"openai": {Map: map[string]string{
				"realtime": "priority", "interactive": "default", "batch": "flex",
			}},
		},
		Header: "X-Request-Priority",

		// A client-supplied hint is IGNORED by default (DESIGN §10.5).
		// Min == Max disables the clamp path entirely.
		//
		// This was 0..10 — the widest possible range — which meant a caller in
		// the batch class could send a hint of 0 and be served as realtime, for
		// free. Clamping bounds how far a caller can self-elevate but leaves
		// the incentive intact, and priority is a claim on shared capacity: if
		// callers may set it, every caller eventually sets the most urgent
		// value, not maliciously but because it costs nothing and appears to
		// help. The scale then carries no information and the callers who left
		// it alone are the ones penalised.
		//
		// An operator grants a range per principal, having seen the whole
		// fleet. A caller cannot claim one — which class a request belongs to
		// is a statement about its importance relative to other tenants' work,
		// and the caller is the one party with no view of that.
		Min: 0,
		Max: 0,
	}
}

// Canonical resolves a request's class and optional hint to a canonical value
// on the lower-is-more-urgent scale.
//
// A hint is clamped into the principal's permitted range (§10.5); it is never
// allowed to widen it. A hint that names no range at all (Min == Max == 0) is
// ignored, because an unclamped client hint is a way for one caller to outrank
// every other.
func (p PriorityConfig) Canonical(class string, hint *int) int {
	if class == "" {
		class = p.Default
	}
	v, ok := p.Classes[class]
	if !ok {
		v = p.Classes[p.Default]
	}
	if hint != nil && p.Max > p.Min {
		h := *hint
		if h < p.Min {
			h = p.Min
		}
		if h > p.Max {
			h = p.Max
		}
		v = h
	}
	return v
}

// Wire converts a canonical value into the number a specific engine must
// receive, and reports the field it goes in.
//
// For an ascending engine the canonical value travels unchanged. For a
// descending engine it is negated (offset by Base), because the canonical scale
// and the engine's scale run in opposite directions. The bool reports whether
// this engine takes a numeric field at all; when it is false the caller emits
// only the header, or the service tier from Tier.
func (p PriorityConfig) Wire(kind string, canonical int) (value int, field string, ok bool) {
	r, found := p.Emit[kind]
	if !found || r.Field == "" {
		return 0, "", false
	}
	if r.Direction == Descending {
		return r.Base - canonical, r.Field, true
	}
	return canonical, r.Field, true
}

// Tier folds a class onto a non-numeric field such as OpenAI's service_tier.
// The empty string means this engine has no tier mapping.
//
// vLLM's service_tier is accepted and has zero consumers (VLLM.md §1.2), so it
// must never be configured as a priority fallback for that kind. Nothing here
// invents a tier for a kind that did not declare one.
func (p PriorityConfig) Tier(kind, class string) string {
	if class == "" {
		class = p.Default
	}
	r, ok := p.Emit[kind]
	if !ok || r.Map == nil {
		return ""
	}
	return r.Map[class]
}
