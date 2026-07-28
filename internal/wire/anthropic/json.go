package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"sync"
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

// splitExtra returns the members of a JSON object that are not in known.
//
// This is how context_management, cache_edits, mcp_servers and every field this
// package has never heard of survive a crossing (DESIGN §10.5a: "not
// recognizing something is not a reason to remove it").
func splitExtra(b []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	for k := range raw {
		if _, ok := known[k]; ok {
			delete(raw, k)
		}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return raw, nil
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
