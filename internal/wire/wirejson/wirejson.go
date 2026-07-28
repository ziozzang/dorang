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
	"sort"
	"strings"
	"sync"
)

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

// SplitExtra returns the members of a JSON object that are not in known.
func SplitExtra(b []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
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

// SplitExtraFold is [SplitExtra] for a type decoded with plain json.Unmarshal
// instead of the strict filter — which is every RESPONSE type, because a
// differently-cased key from a backend is a vendor quirk that must not cost the
// caller its usage counts.
//
// The difference is the whole reason it exists. encoding/json matches field
// names case-INSENSITIVELY, so a backend that sends "Usage" populates the Usage
// field; an exact-match split then leaves "Usage" in the map as well, and the
// re-serialized answer carries the object twice under two spellings. Nothing is
// lost by folding — the value was already read into the struct — and a client
// is not handed a duplicate it has to guess about.
//
// Request types must NOT use this. They are filtered by StrictBytes first, so
// the cased key is dropped from the struct decode and from the map together;
// folding there would silently accept a key the authorization gate never saw
// (COMPATIBILITY 2.0).
func SplitExtraFold(b []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	for k := range raw {
		if _, ok := known[k]; ok {
			delete(raw, k)
			continue
		}
		for want := range known {
			if strings.EqualFold(k, want) {
				delete(raw, k)
				break
			}
		}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return raw, nil
}

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
		return nil, errors.New("wirejson: cannot splice extra fields into a non-object")
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
