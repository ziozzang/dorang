// Package wirejson holds the JSON primitives every wire adapter needs.
//
// They live here rather than being copied into each adapter because two of them
// are normative, not conveniences: COMPATIBILITY 2.1a fixes the serializer
// itself as part of the byte contract, and the pass-through Extra mechanism has
// to splice unknown members back in the same deterministic order everywhere or
// a golden test passes in one adapter and fails in the next.
package wirejson

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
)

// errNotObject is what a splice reports when the value it was handed did not
// marshal to an object. Nothing that reaches it can, which is why it is a
// sentinel and not a formatted message.
var errNotObject = errors.New("wirejson: cannot splice extra fields into a non-object")

// Marshal encodes v the way the reference serializer does.
//
// It differs from json.Marshal in one respect that matters on the wire: HTML
// escaping is OFF. Go's default turns '<', '>' and '&' into <, > and
// &, so a model that emits "a && b" or a fragment of HTML produces bytes no
// other OpenAI-compatible server produces. Both forms decode to the same string,
// but a golden-byte comparison against a reference capture fails, and so does
// any client doing a substring match on the raw frame (COMPATIBILITY 2.1a).
func Marshal(v any) ([]byte, error) {
	b := GetBuf()
	defer PutBuf(b)
	if err := MarshalTo(b, v); err != nil {
		return nil, err
	}
	return append([]byte(nil), b.Bytes()...), nil
}

// MarshalTo encodes v into dst without an intermediate copy.
func MarshalTo(dst *bytes.Buffer, v any) error {
	enc := json.NewEncoder(dst)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	// Encode appends a newline; the wire format supplies its own framing.
	dst.Truncate(dst.Len() - 1)
	return nil
}

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// GetBuf takes a scratch buffer from the shared pool.
func GetBuf() *bytes.Buffer {
	b := bufPool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

// PutBuf returns one. A buffer that ballooned once is dropped rather than
// retained for the life of the process.
func PutBuf(b *bytes.Buffer) {
	if b.Cap() > 1<<20 {
		return
	}
	bufPool.Put(b)
}

// KnownKeys builds the modelled-member set of a wire type.
func KnownKeys(names ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

// SplitExtra and SplitExtraFold live in extra.go.

// MarshalWithExtra marshals v and splices extra's members into the resulting
// object. Keys are sorted so the bytes are deterministic, which the golden
// tests require.
func MarshalWithExtra(v any, extra map[string]json.RawMessage, known map[string]struct{}) ([]byte, error) {
	b, err := Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return b, nil
	}
	keys := extraKeys(extra, known)
	if len(keys) == 0 {
		return b, nil
	}
	if len(b) < 2 || b[len(b)-1] != '}' {
		return nil, errNotObject
	}
	out := make([]byte, 0, len(b)+64*len(keys))
	out = append(out, b[:len(b)-1]...)
	empty := len(b) == 2 // "{}"
	for i, k := range keys {
		if i > 0 || !empty {
			out = append(out, ',')
		}
		// Marshal, not strconv.Quote: Go string quoting escapes control bytes
		// as \xNN, which is not valid JSON.
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

// TrimSpace strips the four JSON whitespace bytes from both ends. It is not
// bytes.TrimSpace: that one is Unicode-aware and JSON's whitespace set is four
// ASCII bytes.
func TrimSpace(b []byte) []byte {
	for len(b) > 0 && isSpace(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && isSpace(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
