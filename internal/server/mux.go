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
)

// String names the family.
func (f Family) String() string {
	switch f {
	case FamilyOpenAIChat:
		return "openai-chat"
	case FamilyOpenAIEmbeddings:
		return "openai-embeddings"
	case FamilyAnthropicMessages:
		return "anthropic-messages"
	case FamilyAnthropicCountTokens:
		return "anthropic-count-tokens"
	case FamilyModels:
		return "models"
	case FamilyHealth:
		return "health"
	case FamilyMetrics:
		return "metrics"
	case FamilyPassthrough:
		return "passthrough"
	default:
		return "none"
	}
}

// Handler serves one matched request. Returning a non-nil error hands the
// response back to the server, which writes an envelope if nothing has been
// written yet and an in-band SSE error if a stream has already started.
type Handler func(w http.ResponseWriter, rq *Request) error

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
	// Public skips authentication. Container probes and the metrics scrape
	// are the only public routes; everything else authenticates.
	Public bool
	// NeedsBody makes the server read and cap the request body before the
	// handler runs. Passthrough sets it false: it streams.
	NeedsBody bool
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
