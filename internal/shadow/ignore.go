package shadow

import "strings"

// defaultIgnoreType is the built-in set of names whose *presence* is compared
// and whose *type* is not.
//
// It is two entries, and the shortness is the point.
//
// §14.1 asks for ids, timestamps, model output text and token counts to be
// ignored. None of them needs an entry here, because this comparison never
// reaches a value: it compares which paths exist and what type sits at each
// one. Two different ids, two different `created` stamps and two entirely
// different completions produce no difference by construction. Listing them
// would be theatre — and worse, it would suppress real signal, because a
// gateway that stopped emitting `id` at all is a break every client notices and
// an ignore rule on `id` would hide exactly that.
//
// What is listed is the narrower thing an ignore list is actually for: fields
// whose *type* varies within one gateway from one response to the next, so that
// comparing them produces noise rather than signal. `system_fingerprint` is
// string on some responses and null on others; `logprobs` is object or null
// depending on the request, and COMPATIBILITY §2.1 records that the two
// implementations dorang has to satisfy disagree about whether it appears as
// null at all.
var defaultIgnoreType = []string{"system_fingerprint", "logprobs"}

// ignoreSet matches paths against the built-in and configured ignore rules.
//
// Three forms, because operators write all three:
//
//	system_fingerprint          a bare name, matched at any depth
//	$.choices[].message.content an exact normalized path
//	$.metadata.*                a prefix
//
// A path prefixed with a stream frame kind ("content|$.…") is matched on the
// part after the bar as well, so a rule written against a non-streaming
// response also applies to the streamed form of the same field.
type ignoreSet struct {
	names    map[string]bool
	exact    map[string]bool
	prefixes []string

	typeNames map[string]bool
}

func newIgnoreSet(fields []string) *ignoreSet {
	ig := &ignoreSet{
		names:     make(map[string]bool),
		exact:     make(map[string]bool),
		typeNames: make(map[string]bool, len(defaultIgnoreType)),
	}
	for _, n := range defaultIgnoreType {
		ig.typeNames[n] = true
	}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		switch {
		case f == "":
		case strings.HasSuffix(f, "*"):
			ig.prefixes = append(ig.prefixes, strings.TrimSuffix(f, "*"))
		case strings.HasPrefix(f, "$."):
			ig.exact[f] = true
		default:
			ig.names[f] = true
		}
	}
	return ig
}

// ignored reports whether a path and everything under it is excluded.
func (ig *ignoreSet) ignored(path string) bool {
	if ig == nil {
		return false
	}
	bare := stripFrameKind(path)
	if ig.exact[bare] || ig.exact[path] {
		return true
	}
	if n := lastSegment(bare); n != "" && ig.names[n] {
		return true
	}
	for _, p := range ig.prefixes {
		if strings.HasPrefix(bare, p) || strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// typeIgnored reports whether a path's presence is compared but its type is not.
func (ig *ignoreSet) typeIgnored(path string) bool {
	if ig == nil {
		return false
	}
	n := lastSegment(stripFrameKind(path))
	return n != "" && ig.typeNames[n]
}

// stripFrameKind removes the "kind|" prefix a streamed path carries.
func stripFrameKind(path string) string {
	if i := strings.IndexByte(path, '|'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// lastSegment is the final object key of a path, "" when the path ends in an
// array or is the root. `$.choices[].delta.content` yields "content";
// `$.choices[]` yields "".
func lastSegment(path string) string {
	if strings.HasSuffix(path, "[]") {
		return ""
	}
	i := strings.LastIndexByte(path, '.')
	if i < 0 {
		return ""
	}
	return path[i+1:]
}
