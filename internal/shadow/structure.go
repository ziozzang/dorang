package shadow

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// This file turns a response into its shape: the thing §14.1 says is comparable
// when the content is not.
//
// The rule the whole design follows is that a value is never compared unless it
// is drawn from a bounded vocabulary. Paths and types are compared; ids,
// timestamps and completion text are not — not because they are on an ignore
// list, but because a value comparison is never reached for them. The two
// exceptions are stated where they are made: finish_reason/stop_reason, and an
// error's type and code, all of which are API surface a client branches on
// (COMPATIBILITY §4.1, §11.2).

// jsonType is one bit per JSON type. A path carries a *set* rather than a single
// type because a path that appears in many array elements or many stream frames
// may legitimately hold different types at different ones — `finish_reason` is
// null on every chunk but the last, and comparing a single type there would
// report a difference on every stream.
type jsonType uint8

const (
	typeNull jsonType = 1 << iota
	typeBool
	typeNumber
	typeString
	typeArray
	typeObject

	// typeIgnored marks a path whose presence is compared and whose type is
	// not. See defaultIgnoreType.
	typeIgnored jsonType = 0x80
)

func (t jsonType) String() string {
	if t&typeIgnored != 0 {
		return "(type not compared)"
	}
	var parts []string
	for _, p := range []struct {
		bit  jsonType
		name string
	}{
		{typeNull, "null"}, {typeBool, "bool"}, {typeNumber, "number"},
		{typeString, "string"}, {typeArray, "array"}, {typeObject, "object"},
	} {
		if t&p.bit != 0 {
			parts = append(parts, p.name)
		}
	}
	if len(parts) == 0 {
		return "absent"
	}
	return strings.Join(parts, "|")
}

func typeOf(v any) jsonType {
	switch v.(type) {
	case nil:
		return typeNull
	case bool:
		return typeBool
	case float64, json.Number:
		return typeNumber
	case string:
		return typeString
	case []any:
		return typeArray
	case map[string]any:
		return typeObject
	}
	return typeNull
}

// contentKind is the wire form of a body.
type contentKind uint8

const (
	contentEmpty contentKind = iota
	contentJSON
	contentSSE
	// contentOther is a body that is neither — HTML from a load balancer, a
	// plain-text 502, a binary blob. It is compared as far as it can be and
	// then reported inconclusive, never silently passed.
	contentOther
)

func (c contentKind) String() string {
	switch c {
	case contentJSON:
		return "json"
	case contentSSE:
		return "sse"
	case contentOther:
		return "other"
	}
	return "empty"
}

// shape is everything comparable about one response.
type shape struct {
	status  int
	content contentKind

	// headerKeys is the lower-cased header key set, transport keys removed.
	headerKeys []string

	// paths maps a normalized JSON path to the set of types seen at it. Array
	// indices are collapsed to "[]" and the element structures unioned, so a
	// response with three choices and one with two are the same shape — while a
	// field present in one and absent in the other still differs.
	paths map[string]jsonType

	// frames is the run-length-compressed sequence of stream frame kinds.
	// Compressed because the number of content deltas is model output and is
	// not comparable; the order of the kinds is protocol and is.
	frames []string
	// frameKinds is the same information as a set, so that "a kind appears on
	// one side only" can be told apart from "the same kinds in a different
	// order" — the first can be model nondeterminism (a tool call on one side),
	// the second cannot.
	frameKinds map[string]bool
	// terminator is the last complete frame's kind: `done` for chat
	// completions, `event:message_stop` for messages, `error`, or `none` for an
	// empty stream (COMPATIBILITY §1.4).
	terminator string

	// stopReasons is the set of finish_reason / stop_reason values observed.
	// A bounded vocabulary (COMPATIBILITY §4.1), so it is compared by value.
	stopReasons map[string]bool

	usagePresent bool

	// errEnvelope is the recognized error envelope shape, errType and errCode
	// its vocabulary fields. All three are compared by value: a client's retry
	// loop branches on them (COMPATIBILITY §11).
	errEnvelope string
	errType     string
	errCode     string

	// truncated says the body was larger than the capture windows, so paths and
	// frames describe a prefix and a suffix rather than the whole response.
	truncated bool
	// parseErr, when set, is why the body could not be read as its declared
	// kind. It is never swallowed: an unparseable body on one side and a parsed
	// one on the other is a difference, and on both sides it is inconclusive.
	parseErr string
	// pathLimit records that the path budget was exhausted, which makes the
	// path set a subset rather than the set.
	pathLimit bool
}

// maxPaths bounds one shape's path set. A response deep or wide enough to
// exceed it makes the comparison a subset comparison, which is reported rather
// than assumed away.
const maxPaths = 4096

// maxDepth bounds recursion into a body. JSON nested past it is adversarial or
// broken; either way the path names stop being useful.
const maxDepth = 32

// analyze builds the shape of one response.
//
// head and tail are contiguous when truncated is false — the body was smaller
// than the two windows together — so they are joined and the whole body is
// analyzed. That is not a special case worth avoiding: it is what makes an
// ordinary chat response, which fits comfortably in the windows, produce a
// conclusive comparison instead of an inconclusive one.
func analyze(status int, header http.Header, head, tail []byte, truncated bool, ig *ignoreSet) *shape {
	if !truncated && len(tail) > 0 {
		joined := make([]byte, 0, len(head)+len(tail))
		joined = append(joined, head...)
		joined = append(joined, tail...)
		head, tail = joined, nil
	}
	s := &shape{
		status:      status,
		paths:       make(map[string]jsonType, 32),
		frameKinds:  make(map[string]bool, 8),
		stopReasons: make(map[string]bool, 2),
		truncated:   truncated,
		headerKeys:  headerKeySet(header),
	}

	ct := ""
	if header != nil {
		ct = strings.ToLower(header.Get("Content-Type"))
	}
	switch {
	case len(head) == 0 && len(tail) == 0:
		s.content = contentEmpty
	case strings.HasPrefix(ct, "text/event-stream"):
		s.content = contentSSE
	case strings.Contains(ct, "json"):
		s.content = contentJSON
	default:
		s.content = sniff(head)
	}

	switch s.content {
	case contentSSE:
		s.analyzeStream(head, tail, truncated, ig)
	case contentJSON:
		s.analyzeJSON(head, tail, truncated, ig)
	}
	return s
}

// sniff guesses a body's kind when the Content-Type did not say. A gateway that
// answers 502 with HTML and no Content-Type is a real thing, and guessing here
// is what lets the comparison say "one side sent JSON and the other did not"
// instead of comparing two empty path sets and calling them equal.
func sniff(b []byte) contentKind {
	t := bytes.TrimLeft(b, " \t\r\n")
	if len(t) == 0 {
		return contentEmpty
	}
	if bytes.HasPrefix(t, []byte("data:")) || bytes.HasPrefix(t, []byte("event:")) {
		return contentSSE
	}
	if t[0] == '{' || t[0] == '[' {
		return contentJSON
	}
	return contentOther
}

// analyzeJSON reads a non-streaming body.
func (s *shape) analyzeJSON(head, tail []byte, truncated bool, ig *ignoreSet) {
	if truncated {
		// A cut JSON document cannot be parsed at all, and guessing at its
		// structure from a prefix would put invented paths in the report.
		s.parseErr = "body larger than the capture window; JSON cannot be parsed from a prefix"
		return
	}
	var v any
	if err := json.Unmarshal(head, &v); err != nil {
		s.parseErr = "body is not valid JSON: " + err.Error()
		return
	}
	s.walk(v, "$", ig, 0)
	s.collectSemantics(v)
	s.readError(v)
}

// analyzeStream reads an SSE body.
func (s *shape) analyzeStream(head, tail []byte, truncated bool, ig *ignoreSet) {
	// A truncated head ends mid-frame; a truncated tail begins mid-frame. Both
	// partials are dropped rather than parsed, because half a frame classifies
	// as a different kind than the whole one and would put a fabricated
	// difference in the report.
	frames := parseSSE(head, false, truncated)
	if truncated && len(tail) > 0 {
		frames = append(frames, parseSSE(tail, !tailStartsAtBoundary(tail), false)...)
	}

	var seq []string
	for i := range frames {
		f := &frames[i]
		kind, body := classifyFrame(f)
		s.frameKinds[kind] = true
		if len(seq) == 0 || seq[len(seq)-1] != kind {
			seq = append(seq, kind)
		}
		if body != nil {
			// Frames are bucketed by kind: a message_start and a
			// content_block_delta have nothing to do with each other, and
			// unioning their paths would hide a field that went missing from
			// one of them.
			s.walk(body, kind+"|$", ig, 0)
			s.collectSemantics(body)
			s.readError(body)
		}
	}
	s.frames = seq
	if len(frames) == 0 {
		s.terminator = "none"
	} else {
		k, _ := classifyFrame(&frames[len(frames)-1])
		s.terminator = k
	}
}

// walk records the path set and the type at each path.
//
// Ignored subtrees are not descended into: ignoring `$.choices[].message`
// ignores everything under it, which is what an operator writing an ignore rule
// means and is also the only way to suppress a whole nondeterministic subtree.
func (s *shape) walk(v any, path string, ig *ignoreSet, depth int) {
	if depth > maxDepth || len(s.paths) >= maxPaths {
		s.pathLimit = true
		return
	}
	if ig.ignored(path) {
		return
	}
	t := typeOf(v)
	if ig.typeIgnored(path) {
		t = typeIgnored
	}
	s.paths[path] |= t

	switch x := v.(type) {
	case map[string]any:
		for k, cv := range x {
			s.walk(cv, path+"."+k, ig, depth+1)
		}
	case []any:
		// Indices collapse: three choices and two choices are the same shape.
		for _, cv := range x {
			s.walk(cv, path+"[]", ig, depth+1)
		}
	}
}

// collectSemantics picks out the two things that are compared by value.
func (s *shape) collectSemantics(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if u, ok := m["usage"]; ok && u != nil {
		s.usagePresent = true
	}
	// Anthropic puts stop_reason at the top level of a message and inside
	// message_delta's delta; OpenAI puts finish_reason on each choice.
	addStop(s.stopReasons, m["stop_reason"])
	if d, ok := m["delta"].(map[string]any); ok {
		addStop(s.stopReasons, d["stop_reason"])
	}
	if msg, ok := m["message"].(map[string]any); ok {
		addStop(s.stopReasons, msg["stop_reason"])
	}
	if choices, ok := m["choices"].([]any); ok {
		for _, c := range choices {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			addStop(s.stopReasons, cm["finish_reason"])
		}
	}
}

func addStop(set map[string]bool, v any) {
	if str, ok := v.(string); ok && str != "" {
		set[str] = true
	}
}

// readError recognizes the error envelope. The shapes are the ones
// COMPATIBILITY §11.1 and internal/server's normalizer already enumerate,
// because a reference gateway producing a different one of them is exactly the
// kind of divergence a client's retry loop notices.
func (s *shape) readError(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	switch {
	case m["type"] == "error":
		// {"type":"error","error":{"type","message"}} — Anthropic. The outer
		// "type":"error" is load-bearing: the SDK dispatches on it.
		s.errEnvelope = "anthropic"
		if e, ok := m["error"].(map[string]any); ok {
			s.errType, _ = e["type"].(string)
			s.errCode = codeOf(e["code"])
		}
		return
	case m["error"] != nil:
		switch e := m["error"].(type) {
		case map[string]any:
			s.errEnvelope = "nested"
			s.errType, _ = e["type"].(string)
			s.errCode = codeOf(e["code"])
		case string:
			s.errEnvelope = "bare_string"
		default:
			s.errEnvelope = "unknown"
		}
		return
	case m["object"] == "error":
		s.errEnvelope = "flat"
		s.errType, _ = m["type"].(string)
		s.errCode = codeOf(m["code"])
		return
	case m["detail"] != nil:
		s.errEnvelope = "detail"
		return
	}
}

// codeOf renders an error code. COMPATIBILITY §11.1 requires a string, and a
// backend sending a number there is itself worth reporting — so the numeric
// form is rendered with a marker rather than normalized away.
func codeOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case float64:
		return "number:" + trimFloat(x)
	}
	return "non-string"
}

func trimFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// transportHeaderKeys are header keys that describe this hop rather than the
// API, and differ between any two servers for reasons no client cares about.
//
// The list is short on purpose. Every key not on it is compared, including the
// reference gateway's own proprietary headers — a client that reads one of
// those and stops receiving it after a cutover is precisely the failure §14.1
// exists to catch, so suppressing them to keep the report quiet would defeat
// the report.
var transportHeaderKeys = map[string]bool{
	"date": true, "content-length": true, "transfer-encoding": true,
	"connection": true, "keep-alive": true, "server": true, "via": true,
	"alt-svc": true, "trailer": true, "upgrade": true, "vary": true,
}

// headerKeySet renders the compared header key set.
//
// dorang's own x-dorang-* headers are excluded: they are additive telemetry the
// incumbent has no reason to emit, so listing every one of them as "missing
// from the reference" would bury the report under thirty guaranteed
// differences. Their absence from dorang would be a defect in dorang's own
// header contract (DESIGN §10.4), which its own tests cover.
func headerKeySet(h http.Header) []string {
	if len(h) == 0 {
		return nil
	}
	out := make([]string, 0, len(h))
	for k := range h {
		lk := strings.ToLower(k)
		if transportHeaderKeys[lk] || strings.HasPrefix(lk, "x-dorang-") {
			continue
		}
		out = append(out, lk)
	}
	sort.Strings(out)
	return out
}
