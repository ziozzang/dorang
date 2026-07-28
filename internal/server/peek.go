package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

// peekRequest scans a request body for the two fields the gate needs — the
// model name and the stream flag — without unmarshalling it.
//
// DESIGN §15.2.2: "only the fields needed (model, stream, size markers) are
// scanned, never a full unmarshal". A full unmarshal of a chat request with a
// long message array is the single most expensive thing a gateway can do before
// it has even decided where to send the request, and it is entirely wasted work
// when the body is going to be relayed to a backend that will parse it anyway.
//
// The scanner is a flat walk of the top-level object: it reads keys, compares
// them against the two it wants, and skips every other value structurally.
// Nested objects are never entered.
//
// ok is false when the body is not a JSON object at all. A body that is a valid
// object with neither field returns ok true and a zero model.
func peekRequest(b []byte) (model string, stream, ok bool) {
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return "", false, false
	}
	i++
	sawEscapedKey := false
	for {
		i = skipSpace(b, i)
		if i >= len(b) {
			return model, stream, false
		}
		if b[i] == '}' {
			break
		}
		if b[i] == ',' {
			i++
			continue
		}
		if b[i] != '"' {
			return model, stream, false
		}
		keyStart := i + 1
		keyEnd, esc, ok2 := scanString(b, i)
		if !ok2 {
			return model, stream, false
		}
		key := b[keyStart : keyEnd-1]
		if esc {
			sawEscapedKey = true
		}
		i = skipSpace(b, keyEnd)
		if i >= len(b) || b[i] != ':' {
			return model, stream, false
		}
		i = skipSpace(b, i+1)
		if i >= len(b) {
			return model, stream, false
		}
		switch {
		case !esc && string(key) == "model" && b[i] == '"':
			vEnd, vEsc, ok3 := scanString(b, i)
			if !ok3 {
				return model, stream, false
			}
			raw := b[i+1 : vEnd-1]
			if vEsc || !utf8.Valid(raw) {
				// Two cases where the bytes on the wire are not the string:
				// a JSON escape, and invalid UTF-8, which encoding/json
				// replaces with U+FFFD on decode. Both go through the real
				// decoder so that the name the gate authorizes is byte-identical
				// to the name a later full parse will see. A model name is an
				// opaque string (DESIGN §2.1) and two spellings of it are two
				// models — the fuzzer found this by disagreeing with
				// encoding/json on one high byte.
				var s string
				if err := json.Unmarshal(b[i:vEnd], &s); err == nil {
					model = s
				}
			} else {
				model = string(raw)
			}
			i = vEnd
		case !esc && string(key) == "stream":
			// Only the literal true. COMPATIBILITY §3.1 makes the same point
			// about stream_options.include_usage: truthy is not enough, and a
			// gateway that accepts "1" or "yes" here streams a response the
			// client's parser is not expecting.
			// Assign, never OR. A later `"stream": false` must clear an earlier
			// true, because every adapter and backend takes the last duplicate.
			// A gate that disagrees prepares an SSE response — including
			// COMPATIBILITY 1.3's in-band error path — for a call that comes
			// back whole. The fuzzer found this; reading the code did not.
			stream = hasPrefixAt(b, i, "true")
			i = skipValue(b, i)
		default:
			i = skipValue(b, i)
		}
		if i < 0 {
			return model, stream, false
		}
	}
	if sawEscapedKey {
		// An escaped key can spell "model" — it is the same key to every
		// conforming parser, and the fast path deliberately does not decode keys.
		// Two things this must get right, both of which an earlier version did not:
		//
		// The fallback runs whenever an escaped key was seen, NOT only when no
		// model was found. Given a body naming the model twice, once plainly and
		// once with an escaped key, the fast path stops at the first while every
		// adapter unescapes and takes the last — so the gate authorized one model
		// and the backend served another. That failed OPEN: the key's allow-list
		// was checked against a name that was never dispatched.
		//
		// And it decodes into a map, not a tagged struct. encoding/json matches
		// tags case-insensitively, with Unicode folding, which is the very defect
		// W10 exists to close — using a struct here would put it back inside the
		// gate. A map yields unescaped keys and exact matching, and takes the last
		// duplicate, which is what the adapters do.
		var slow map[string]json.RawMessage
		if err := json.Unmarshal(b, &slow); err == nil {
			m, st := model, stream
			if raw, found := slow["model"]; found {
				var v string
				if json.Unmarshal(raw, &v) == nil {
					m = v
				}
			}
			if raw, found := slow["stream"]; found {
				var v bool
				if json.Unmarshal(raw, &v) == nil {
					st = v
				}
			}
			return m, st, true
		}
	}
	return model, stream, true
}

// hasPrefixAt reports whether b at i begins with s.
func hasPrefixAt(b []byte, i int, s string) bool {
	if i+len(s) > len(b) {
		return false
	}
	return string(b[i:i+len(s)]) == s
}

// skipSpace advances past JSON whitespace. A negative index — which is how
// skipValue reports a malformed value — passes straight through, so that a
// caller that forgot to check cannot index out of bounds.
func skipSpace(b []byte, i int) int {
	if i < 0 {
		return i
	}
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// scanString consumes a string literal starting at b[i] == '"' and returns the
// index just past the closing quote, whether the literal contained a backslash
// escape, and whether it terminated.
//
// The closing quote is found with [bytes.IndexByte] rather than a byte-at-a-time
// loop. That matters more than it looks: the single longest string in a chat
// request is the user's message content, the scanner skips over it on every
// request, and a byte loop there is most of the cost of not unmarshalling.
func scanString(b []byte, i int) (end int, escaped, ok bool) {
	start := i + 1
	j := start
	for {
		n := bytes.IndexByte(b[j:], '"')
		if n < 0 {
			return len(b), true, false
		}
		k := j + n
		// A quote is only the terminator if an even number of backslashes
		// precedes it; an odd number means the quote is itself escaped.
		bs := 0
		for k-1-bs >= start && b[k-1-bs] == '\\' {
			bs++
		}
		if bs%2 == 0 {
			return k + 1, bytes.IndexByte(b[start:k], '\\') >= 0, true
		}
		j = k + 1
	}
}

// skipValue consumes one JSON value and returns the index just past it, or -1.
func skipValue(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		end, _, ok := scanString(b, i)
		if !ok {
			return -1
		}
		return end
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				end, _, ok := scanString(b, i)
				if !ok {
					return -1
				}
				i = end
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return -1
	default:
		for i < len(b) {
			switch b[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return i
			}
			i++
		}
		return i
	}
}

// detailRequested reads the x-dorang-detail request header.
//
// Only the exact token "full" turns the full header set on. DESIGN §10.4 bounds
// the set deliberately — revision 1 attached roughly thirty headers to every
// response, which risks intermediary header-size limits and puts bytes ahead of
// the first streamed byte, against the very target it was serving.
func detailRequested(h http.Header) bool {
	return strings.EqualFold(h.Get(HeaderDetail), "full")
}

// usageEventsRequested reads the x-dorang-usage-events request header. Only "1"
// opts in.
func usageEventsRequested(h http.Header) bool {
	return h.Get(HeaderUsageEvents) == "1"
}
