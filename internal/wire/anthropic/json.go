package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"sync"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/wirejson"
)

// Marshal encodes v the way the reference serializer does.
//
// It differs from json.Marshal in one respect that matters on the wire: HTML
// escaping is OFF. Go's default turns '<', '>' and '&' into <, > and
// &, so a model that emits "a && b" or a fragment of HTML produces bytes no
// other server produces. Both forms decode to the same string, but a
// golden-byte comparison against a reference capture fails, and so does any
// client doing a substring match on the raw frame (COMPATIBILITY 2.1a, whose
// "the serializer itself is normative" applies to every surface, not only to
// chat completions).
func Marshal(v any) ([]byte, error) {
	b := getBuf()
	defer putBuf(b)
	if err := marshalTo(b, v); err != nil {
		return nil, err
	}
	return append([]byte(nil), b.Bytes()...), nil
}

// marshalTo encodes v into dst without an intermediate copy. The streaming
// writer uses it so that emitting an event costs one buffer, not two.
func marshalTo(dst *bytes.Buffer, v any) error {
	enc := json.NewEncoder(dst)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	// Drop the newline Encode appended; the wire format supplies its own framing.
	dst.Truncate(dst.Len() - 1)
	return nil
}

// strictUnmarshal decodes a REQUEST with case-sensitive field matching.
//
// COMPATIBILITY 2.0: encoding/json fills a `json:"model"` field from a member
// spelled "Model". The authorization gate (internal/server/peek.go) compares
// key bytes exactly and so does every backend downstream, so a struct decode
// here would resolve a model the gate never authorized — an allow-list bypass,
// not a cosmetic difference. Every decode on the request path goes through
// this; see [canonical.StrictBytes] for the rule and for what happens to a
// colliding key.
//
// RESPONSES ARE DELIBERATELY NOT STRICT. A response body comes from a backend
// dorang chose, not from a caller: a case variation there is a vendor quirk
// that cannot bypass anything, because nothing is authorized against a
// response, and refusing it would turn an upstream cosmetic bug into a failed
// request and lost usage counts. Types that appear on BOTH paths —
// [ContentBlock] is a request block and a response block — follow the request
// rule, because that is the rule with a security property attached.
func strictUnmarshal(b []byte, v any) error { return canonical.StrictUnmarshal(b, v) }

// strictBytes is [strictUnmarshal]'s filter on its own, for the UnmarshalJSON
// methods that read the SAME bytes twice — once with encoding/json to fill the
// struct, and once with [wirejson.SplitExtra]'s structural walk to collect the
// members the struct does not model. Filtering once and using the result for
// both is what keeps a dropped key from reappearing in Extra and being relayed
// to the next hop, where a Go parser would match it again.
func strictBytes(b []byte, v any) []byte { return canonical.StrictBytes(b, v) }

// decodeSelf decodes b with v's own UnmarshalJSON.
//
// It is not a shortcut around the strict filter — every type it is used with
// calls [strictBytes] as the first thing its method does, so COMPATIBILITY 2.0
// applies exactly as before. What it skips is encoding/json handing v bytes that
// were already v's: json.Unmarshal validates the document and then walks it
// again to find where it ends, before calling the method that validates it once
// more. See [wirejson.UnmarshalSelf] for the precondition and for the
// differential that pins every application of it.
func decodeSelf(b []byte, v json.Unmarshaler) error { return wirejson.UnmarshalSelf(b, v) }

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func getBuf() *bytes.Buffer {
	b := bufPool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func putBuf(b *bytes.Buffer) {
	// A response that ballooned once must not be retained forever.
	if b.Cap() > 1<<20 {
		return
	}
	bufPool.Put(b)
}

// ptr is shorthand for taking the address of a literal, which this package does
// constantly: "absent", "zero" and "null" are three different things on this
// wire, and stop_sequence in particular must render as null and not vanish
// (COMPATIBILITY 6.5).
func ptr[T any](v T) *T { return &v }

func knownKeys(names ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

// splitExtraFold is splitExtra for a RESPONSE type, which is decoded with plain
// json.Unmarshal rather than through [strictBytes].
//
// encoding/json matches field names case-INSENSITIVELY, so a backend that sends
// "Usage" populates the Usage field; an exact-match split then leaves "Usage" in
// the map as well and the re-serialized answer carries the object twice under
// two spellings. Folding loses nothing — the value was already read into the
// struct — and hands the client one key instead of two.
//
// Request types must not use it: they are filtered by [strictBytes] first, so a
// cased key is dropped from the struct decode and from the map together, and
// folding would accept a key the authorization gate never saw
// (COMPATIBILITY 2.0).
func splitExtraFold(b []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	return wirejson.SplitExtraFold(b, known)
}

// splitExtra returns the members of a JSON object that are not in known.
//
// This is how context_management, cache_edits, mcp_servers and every field this
// package has never heard of survive a crossing (DESIGN §10.5a: "not
// recognizing something is not a reason to remove it").
//
// The walk is [wirejson.SplitExtra]'s rather than a copy of it: this package
// used to carry its own second json.Unmarshal, which meant an Anthropic request
// paid the same double parse the chat path did and would have kept paying it
// after the chat path stopped.
func splitExtra(b []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	return wirejson.SplitExtra(b, known)
}

// marshalWithExtra marshals v and splices extra's members into the resulting
// object. Keys are sorted so the bytes are deterministic, which the golden tests
// require. The raw values are spliced verbatim, never re-encoded, so an opaque
// blob's bytes are the bytes that arrived.
func marshalWithExtra(v any, extra map[string]json.RawMessage, known map[string]struct{}) ([]byte, error) {
	b, err := Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return b, nil
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		if _, clash := known[k]; clash {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return b, nil
	}
	sort.Strings(keys)
	if len(b) < 2 || b[len(b)-1] != '}' {
		return nil, errors.New("anthropic: cannot splice extra fields into a non-object")
	}
	out := make([]byte, 0, len(b)+64*len(keys))
	out = append(out, b[:len(b)-1]...)
	empty := len(b) == 2 // "{}"
	for i, k := range keys {
		if i > 0 || !empty {
			out = append(out, ',')
		}
		// json.Marshal, not strconv.Quote: Go string quoting escapes control
		// bytes as \xNN, which is not valid JSON.
		kb, err := Marshal(k)
		if err != nil {
			return nil, err
		}
		out = append(out, kb...)
		out = append(out, ':')
		out = append(out, extra[k]...)
	}
	out = append(out, '}')
	return out, nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 {
		c := b[len(b)-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

type errorString string

func (e errorString) Error() string { return string(e) }
