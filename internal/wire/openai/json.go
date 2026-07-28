package openai

import (
	"bytes"
	"encoding/json"
	"sync"
)

// Marshal encodes v the way the reference serializer does.
//
// It differs from json.Marshal in one respect that matters on the wire: HTML
// escaping is OFF. Go's default turns '<', '>' and '&' into <, > and
// &, so a model that emits "a && b" or a fragment of HTML produces bytes
// no other OpenAI-compatible server produces. Both forms decode to the same
// string, but a golden-byte comparison against a reference capture fails, and
// so does any client doing a substring match on the raw frame.
func Marshal(v any) ([]byte, error) {
	b := getBuf()
	defer putBuf(b)
	enc := json.NewEncoder(b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode appends a newline; the wire format supplies its own framing.
	out := b.Bytes()
	out = out[:len(out)-1]
	return append([]byte(nil), out...), nil
}

// marshalTo encodes v into dst without an intermediate copy. The streaming
// writer uses it so that emitting a chunk costs one buffer, not two.
func marshalTo(dst *bytes.Buffer, v any) error {
	enc := json.NewEncoder(dst)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	// Drop the newline Encode appended.
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
// constantly because "absent" and "zero" are different on this wire
// (COMPATIBILITY 2.1).
func ptr[T any](v T) *T { return &v }
