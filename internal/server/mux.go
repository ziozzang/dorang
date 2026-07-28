package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"
)

// maxParams bounds the captured path parameters per request. Four is one more
// than any registered pattern needs; the array lives on the pooled request
// struct so that capturing a parameter allocates nothing.
const maxParams = 4

// Param is one captured path segment.
type Param struct {
	Name  string
	Value string
}

// Method is a bitmask of HTTP methods. A route declares the set it answers, so
// a path that matches with the wrong method is a 405 with an Allow header
// rather than a 501 that makes the caller think the route does not exist.
type Method uint16

// The method bits.
const (
	MethodGET Method = 1 << iota
	MethodPOST
	MethodPUT
	MethodDELETE
	MethodPATCH
	MethodHEAD
	MethodOPTIONS
	MethodOther
)

// methodBit maps a request method to its bit.
func methodBit(m string) Method {
	switch m {
	case http.MethodGet:
		return MethodGET
	case http.MethodPost:
		return MethodPOST
	case http.MethodPut:
		return MethodPUT
	case http.MethodDelete:
		return MethodDELETE
	case http.MethodPatch:
		return MethodPATCH
	case http.MethodHead:
		return MethodHEAD
	case http.MethodOptions:
		return MethodOPTIONS
	default:
		return MethodOther
	}
}

// allowHeader renders a method set for the Allow header. It is built once per
// route at compile time, never per request.
func allowHeader(m Method) string {
	var parts []string
	for _, p := range []struct {
		bit  Method
		name string
	}{
		{MethodGET, http.MethodGet},
		{MethodHEAD, http.MethodHead},
		{MethodPOST, http.MethodPost},
		{MethodPUT, http.MethodPut},
		{MethodPATCH, http.MethodPatch},
		{MethodDELETE, http.MethodDelete},
		{MethodOPTIONS, http.MethodOptions},
	} {
		if m&p.bit != 0 {
			parts = append(parts, p.name)
		}
	}
	return strings.Join(parts, ", ")
}

// Family labels a route's protocol shape for the dispatcher. The server does
// not implement any of them; it says which one was asked for.
type Family uint8

// The route families.
//
// New members are appended, never inserted: the value is written into the
// ledger's family column, so renumbering an existing one silently relabels
// every historical row.
const (
	FamilyNone Family = iota
	FamilyOpenAIChat
	FamilyOpenAIEmbeddings
	FamilyAnthropicMessages
	FamilyAnthropicCountTokens
	FamilyModels
	FamilyHealth
	FamilyMetrics
	FamilyPassthrough

	// The T1 inference surface (COMPATIBILITY §0, DESIGN §2.1).
	FamilyOpenAICompletions
	FamilyOpenAIResponses
	FamilyOpenAIModerations
	FamilyOpenAIRerank
	FamilyOpenAISpeech
	FamilyOpenAITranscription
	FamilyOpenAITranslation
	FamilyOpenAIImageGeneration
	FamilyOpenAIImageEdit
	FamilyOpenAIImageVariation

	// FamilyAdmin is the administration surface (DESIGN §2.3). It is one
	// family for the whole of it rather than one per path, because these are
	// metric labels and the surface has forty paths — a label per path is the
	// cardinality problem this table exists to avoid.
	FamilyAdmin
)

// familyNames is the fixed-cardinality label set. It is a table rather than a
// switch so that adding a family without naming it fails to compile the
// exhaustive test rather than silently metering as "none".
var familyNames = [...]string{
	FamilyNone:                  "none",
	FamilyOpenAIChat:            "openai-chat",
	FamilyOpenAIEmbeddings:      "openai-embeddings",
	FamilyAnthropicMessages:     "anthropic-messages",
	FamilyAnthropicCountTokens:  "anthropic-count-tokens",
	FamilyModels:                "models",
	FamilyHealth:                "health",
	FamilyMetrics:               "metrics",
	FamilyPassthrough:           "passthrough",
	FamilyOpenAICompletions:     "openai-completions",
	FamilyOpenAIResponses:       "openai-responses",
	FamilyOpenAIModerations:     "openai-moderations",
	FamilyOpenAIRerank:          "rerank",
	FamilyOpenAISpeech:          "openai-audio-speech",
	FamilyOpenAITranscription:   "openai-audio-transcription",
	FamilyOpenAITranslation:     "openai-audio-translation",
	FamilyOpenAIImageGeneration: "openai-image-generation",
	FamilyOpenAIImageEdit:       "openai-image-edit",
	FamilyOpenAIImageVariation:  "openai-image-variation",
	FamilyAdmin:                 "admin",
}

// String names the family.
func (f Family) String() string {
	if int(f) < len(familyNames) && familyNames[f] != "" {
		return familyNames[f]
	}
	return "none"
}

// Anthropic reports whether the caller is speaking the Anthropic protocol,
// which decides the error vocabulary they get back (COMPATIBILITY §11.2's two
// type columns) AND the envelope it is delivered in (§11.1's two objects, and
// §11.1a's two SSE framings for a mid-stream failure).
//
// The second half is the one that was missing. A vocabulary projected correctly
// and then serialized into the other family's object is not half right: that
// SDK dispatches on the outer `"type":"error"` member, so it never reaches the
// correctly-spelled type inside.
//
// It is a whitelist rather than "not one of the OpenAI ones" because every
// family that is neither — models, health, metrics, admin, passthrough, and the
// zero value a route that never matched carries — is served an OpenAI-shaped
// envelope today, and a new family must be classified deliberately rather than
// inherit an answer from where it happened to be appended.
//
// [FamilyPassthrough] is the one entry that is a judgement rather than a fact.
// A prefix relayed to a native Anthropic surface is being called by an Anthropic
// SDK, and a gateway-authored refusal on it — an unauthorized model, a relay
// failure — reaches that SDK in the OpenAI object. It stays here because
// [PassthroughRoute] declares no protocol: the prefix, the base URL and the
// provider id are all opaque strings, so nothing in the route table knows which
// envelope the caller expects, and guessing from a configured provider NAME
// would be a new contract rather than a fix. Closing it means declaring the
// family per prefix in configuration.
func (f Family) Anthropic() bool {
	switch f {
	case FamilyAnthropicMessages, FamilyAnthropicCountTokens:
		return true
	}
	return false
}

// Inference reports whether a family ends in an upstream model call, which is
// what decides whether a request is priced and metered as one.
func (f Family) Inference() bool {
	switch f {
	case FamilyNone, FamilyModels, FamilyHealth, FamilyMetrics, FamilyPassthrough:
		return false
	}
	return true
}

// Handler serves one matched request. Returning a non-nil error hands the
// response back to the server, which writes an envelope if nothing has been
// written yet and an in-band SSE error if a stream has already started.
type Handler func(w http.ResponseWriter, rq *Request) error

// ModelAuth declares how a route satisfies the per-key model allow-list.
//
// It exists because "the check is written" and "the check is reached" turned
// out to be different facts three separate times: the gate consults the
// allow-list only when it managed to scan a model out of the body, so a route
// whose body has no top-level model (batch create) or whose body is never
// parsed at all (passthrough) sailed past a restriction the operator had set.
// The bypass was silent in both cases because a skipped check and a passed
// check look identical from the outside.
//
// Making the field required removes the silence. [ModelAuthUnset] is the zero
// value and [newRouteTable] refuses to compile a route that still carries it,
// so a new route cannot reach the mux until somebody has answered the question
// — and answering it wrong is at least a visible answer in the route table
// rather than an omission nobody can grep for.
type ModelAuth uint8

// The model-authorization modes.
const (
	// ModelAuthUnset is the zero value. A route carrying it is refused at
	// registration: the mode is a decision, and the zero value is the absence
	// of one.
	ModelAuthUnset ModelAuth = iota
	// ModelAuthGate means the server's gate enforces the allow-list from the
	// model it scanned out of the body. Only valid together with NeedsBody:
	// without a body there is nothing to scan and the gate would silently
	// enforce nothing, which is the defect this type exists to prevent.
	ModelAuthGate
	// ModelAuthHandler means the handler names every model it is about to
	// dispatch to [Request.AuthorizeModel]. Routes that carry many models in
	// one request (a batch input file) or that never parse their body
	// (passthrough) use this.
	ModelAuthHandler
	// ModelAuthNone means the route cannot reach a paid model call at all:
	// health, metrics, the model listing, and the batch/file management
	// endpoints that only read records back.
	ModelAuthNone
)

// String names the mode.
func (m ModelAuth) String() string {
	switch m {
	case ModelAuthGate:
		return "gate"
	case ModelAuthHandler:
		return "handler"
	case ModelAuthNone:
		return "none"
	default:
		return "unset"
	}
}

// Route is one registered pattern.
//
// Patterns are '/'-separated. A segment may be a literal, a parameter
// "{name}", or a terminal wildcard "{name...}" which matches the rest of the
// path including nothing at all.
type Route struct {
	// Pattern is the registered path pattern.
	Pattern string
	// Methods is the set this route answers.
	Methods Method
	// Name identifies the route in metrics and logs. Fixed cardinality:
	// never the request path.
	Name string
	// Family labels the protocol shape.
	Family Family
	// Public skips authentication. Container probes are the only public routes
	// by default; everything else authenticates.
	Public bool
	// Admin requires an administrative caller on top of authentication: the
	// master credential, or a key an [AdminPrincipal] reports as admin.
	//
	// It exists because /metrics is neither public nor ordinary. The scrape
	// carries per-key spend, per-credential quota state and every configured
	// model name, which is a description of a deployment's commercial
	// arrangements — so any authenticated tenant reading it would be reading
	// every other tenant's numbers.
	Admin bool
	// NeedsBody makes the server read and cap the request body before the
	// handler runs. Passthrough sets it false: it streams.
	NeedsBody bool
	// Multipart declares that the body is multipart/form-data rather than
	// JSON. The server parses it into [Request.Form] and reads the model from
	// the `model` FIELD, so the authorization gate sees the same model the
	// adapter will — the multipart equivalent of the strict-JSON rule
	// (COMPATIBILITY 2.0, DESIGN §18 W10).
	Multipart bool
	// ModelParam names the path parameter that carries the model, for the
	// deployment-in-the-path aliases (`/engines/{model}/…`,
	// `/openai/deployments/{model}/…`). When set, the path wins over the body:
	// that is what those clients mean, and it is resolved BEFORE authorization
	// so the allow-list is checked against the name that will actually be
	// dispatched.
	ModelParam string
	// ModelAuth declares how this route satisfies the model allow-list. It has
	// no default: [ModelAuthUnset] is refused at registration.
	ModelAuth ModelAuth
	// Handler serves the route.
	Handler Handler

	segs  []segment
	allow string
}

// segment is one compiled pattern segment.
type segment struct {
	kind segKind
	lit  string
	name string
}

type segKind uint8

const (
	segLiteral segKind = iota
	segParam
	segWildcard
)

// ErrBadPattern is returned by the table builder for a malformed pattern.
var ErrBadPattern = errors.New("server: malformed route pattern")

// ErrModelAuthUnset is returned by the table builder for a route that did not
// declare a [ModelAuth] mode, or that declared [ModelAuthGate] without a body
// for the gate to read the model out of.
var ErrModelAuthUnset = errors.New("server: route does not declare a ModelAuth mode")

// compile turns a pattern into segments.
func compile(pattern string) ([]segment, error) {
	if pattern == "" || pattern[0] != '/' {
		return nil, ErrBadPattern
	}
	raw := strings.Split(pattern[1:], "/")
	segs := make([]segment, 0, len(raw))
	nparams := 0
	for i, s := range raw {
		switch {
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") && len(s) > 2:
			name := s[1 : len(s)-1]
			if strings.HasSuffix(name, "...") {
				if i != len(raw)-1 {
					return nil, ErrBadPattern // a wildcard must be terminal
				}
				segs = append(segs, segment{kind: segWildcard, name: name[:len(name)-3]})
			} else {
				segs = append(segs, segment{kind: segParam, name: name})
			}
			nparams++
		case strings.ContainsAny(s, "{}"):
			return nil, ErrBadPattern
		default:
			segs = append(segs, segment{kind: segLiteral, lit: s})
		}
	}
	if nparams > maxParams {
		return nil, ErrBadPattern
	}
	return segs, nil
}

// hasParams reports whether the pattern needs the scanning matcher rather than
// the exact map.
func hasParams(segs []segment) bool {
	for i := range segs {
		if segs[i].kind != segLiteral {
			return true
		}
	}
	return false
}

// match tests path against the route, filling params.
//
// It walks the path with index arithmetic — no Split, no allocation, and no
// regular expression (DESIGN §15.5).
func (rt *Route) match(path string, params *[maxParams]Param) (int, bool) {
	if len(path) == 0 || path[0] != '/' {
		return 0, false
	}
	i, np := 1, 0
	for si := 0; si < len(rt.segs); si++ {
		s := &rt.segs[si]
		if s.kind == segWildcard {
			params[np] = Param{Name: s.name, Value: path[i:]}
			return np + 1, true
		}
		j := i
		for j < len(path) && path[j] != '/' {
			j++
		}
		switch s.kind {
		case segLiteral:
			if path[i:j] != s.lit {
				return 0, false
			}
		case segParam:
			if i == j {
				return 0, false // an empty segment is not a parameter value
			}
			params[np] = Param{Name: s.name, Value: path[i:j]}
			np++
		}
		if j >= len(path) {
			// The path ran out. That is a match only if this was the last
			// segment, or if the one segment left is a wildcard — so that
			// prefix /anthropic matches the bare path /anthropic and not only
			// /anthropic/something.
			switch {
			case si == len(rt.segs)-1:
				return np, true
			case si == len(rt.segs)-2 && rt.segs[si+1].kind == segWildcard:
				params[np] = Param{Name: rt.segs[si+1].name, Value: ""}
				return np + 1, true
			default:
				return 0, false
			}
		}
		i = j + 1
	}
	return 0, false // pattern ran out before the path did
}

// routeTable is the compiled, specificity-ordered route set.
//
// Exact patterns go in a map, so every T0 path is one hash lookup. Patterned
// routes are a slice sorted most-specific-first and scanned linearly; there are
// a handful of them and the scan touches no memory the map lookup did not.
type routeTable struct {
	exact map[string]*Route
	pats  []*Route
}

// overrideByPattern keeps the LAST route registered on each pattern, preserving
// registration order otherwise.
//
// Order is preserved rather than rebuilt because newRouteTable sorts patterned
// routes by specificity with a stable sort, and a reordering here would change
// which of two equally-specific patterns is scanned first for no stated reason.
func overrideByPattern(routes []*Route) []*Route {
	last := make(map[string]int, len(routes))
	for i, rt := range routes {
		last[rt.Pattern] = i
	}
	if len(last) == len(routes) {
		return routes
	}
	out := routes[:0]
	for i, rt := range routes {
		if last[rt.Pattern] == i {
			out = append(out, rt)
		}
	}
	return out
}

// newRouteTable compiles and orders a route set.
//
// COMPATIBILITY §7.5 is the whole reason this is not a prefix map:
// /openai/deployments/{model}/chat/completions must match before
// /openai/{rest...}, and a naive prefix router silently swallows the specific
// route into the catch-all. Ordering is by specificity, so registration order
// cannot change the answer.
func newRouteTable(routes []*Route) (*routeTable, error) {
	t := &routeTable{exact: make(map[string]*Route, len(routes))}
	for _, rt := range routes {
		// The allow-list decision is checked before the pattern, because a
		// route that forgot it is a security defect and a route with a bad
		// pattern is a typo.
		//
		// ModelAuthGate without NeedsBody is refused rather than tolerated: the
		// gate enforces the allow-list from the model it scanned out of the
		// body, and with no body it scans nothing, compares "" against the
		// list, and skips the check — which is exactly how passthrough came to
		// dispatch unrestricted models while looking authorized.
		switch {
		case rt.ModelAuth == ModelAuthUnset:
			return nil, ErrModelAuthUnset
		case rt.ModelAuth == ModelAuthGate && !rt.NeedsBody:
			return nil, ErrModelAuthUnset
		}
		segs, err := compile(rt.Pattern)
		if err != nil {
			return nil, err
		}
		rt.segs = segs
		rt.allow = allowHeader(rt.Methods)
		if hasParams(segs) {
			t.pats = append(t.pats, rt)
			continue
		}
		if prev, dup := t.exact[rt.Pattern]; dup {
			// Two exact routes on one path is a configuration bug, not a
			// precedence question. Merging the method sets silently would let
			// one of the two handlers become unreachable.
			_ = prev
			return nil, ErrBadPattern
		}
		t.exact[rt.Pattern] = rt
	}
	sort.SliceStable(t.pats, func(a, b int) bool {
		return moreSpecific(t.pats[a], t.pats[b])
	})
	return t, nil
}

// moreSpecific orders two patterned routes.
//
// Segment by segment, a literal beats a parameter beats a wildcard. The first
// difference decides. If neither differs and one has more segments, the longer
// pattern is more specific — a pattern cannot be less specific than its own
// prefix. Ties fall back to the pattern string so the order is total and the
// table is reproducible.
func moreSpecific(a, b *Route) bool {
	n := len(a.segs)
	if len(b.segs) < n {
		n = len(b.segs)
	}
	for i := 0; i < n; i++ {
		ka, kb := a.segs[i].kind, b.segs[i].kind
		if ka != kb {
			return ka < kb
		}
	}
	if len(a.segs) != len(b.segs) {
		return len(a.segs) > len(b.segs)
	}
	return a.Pattern < b.Pattern
}

// lookupResult says what the table found.
type lookupResult uint8

const (
	// lookupMiss: no pattern matched.
	lookupMiss lookupResult = iota
	// lookupMethod: a pattern matched the path but not the method.
	lookupMethod
	// lookupHit: matched.
	lookupHit
)

// lookup resolves a method and path.
//
// A single trailing slash is tolerated on non-passthrough paths: SDKs and shell
// scripts append one, and answering 501 for /v1/models/ helps nobody. It is
// tolerated by retrying the exact map, never by redirecting — a 307 on a POST
// costs a round trip and some clients drop the body on the retry. Patterned
// routes see the path verbatim, because for a passthrough prefix a trailing
// slash is a meaningful part of the upstream path.
func (t *routeTable) lookup(method, path string, params *[maxParams]Param) (*Route, int, lookupResult) {
	bit := methodBit(method)
	if rt, ok := t.exact[path]; ok {
		if rt.Methods&bit == 0 {
			return rt, 0, lookupMethod
		}
		return rt, 0, lookupHit
	}
	if n := len(path); n > 1 && path[n-1] == '/' {
		if rt, ok := t.exact[path[:n-1]]; ok {
			if rt.Methods&bit == 0 {
				return rt, 0, lookupMethod
			}
			return rt, 0, lookupHit
		}
	}
	var methodMiss *Route
	for _, rt := range t.pats {
		np, ok := rt.match(path, params)
		if !ok {
			continue
		}
		if rt.Methods&bit == 0 {
			if methodMiss == nil {
				methodMiss = rt
			}
			continue
		}
		return rt, np, lookupHit
	}
	if methodMiss != nil {
		return methodMiss, 0, lookupMethod
	}
	return nil, 0, lookupMiss
}
