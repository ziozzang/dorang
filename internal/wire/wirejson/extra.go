package wirejson

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/ziozzang/dorang/internal/canonical"
)

// Splitting the unmodelled members out of an object, without parsing it again.
//
// # The pass this removes
//
// Every wire type with an Extra map used to decode its own bytes TWICE: once
// into the struct with encoding/json, and once more into a
// map[string]json.RawMessage so the members the struct does not model could be
// kept. The second decode is where the request path's time went, and it was
// worse than "one extra parse" for two reasons.
//
// It is a parse per NESTING LEVEL, not per body. A chat request is an object
// holding an array of messages, each holding a content array, each part an
// object — and every one of those types ran the pair on its own bytes. The
// content of a message was walked by the message's split, by the part's decode
// and by the part's split.
//
// And it materialized what it was about to discard. json.Unmarshal into a
// map[string]json.RawMessage copies the value of every member; for a chat
// request the largest member is `messages`, which is nearly the whole body, and
// the loop's next act was to delete it as a modelled key. Measured on the
// package's own benchmark, the pair cost 38% of DecodeRequest's time and 52% of
// its allocations — 143 allocations per KiB, of which 74 were this.
//
// The walk below reads keys and skips values structurally. It allocates for the
// members that survive, which for a well-formed request is none, and it enters a
// string only to find its end.
//
// # Why a structural walk is allowed to be structural
//
// PRECONDITION: b has already been accepted by encoding/json.
//
// Every caller is an UnmarshalJSON method that ran json.Unmarshal over these
// same bytes and returned on error. json.Unmarshal validates the WHOLE document
// with checkValid before it stores anything, so by the time a split runs, every
// byte of b is proven — which is what lets this skip a value by counting
// brackets instead of re-validating it. That is the same reason
// internal/server/peek.go may scan instead of parse, and it is the only reason.
//
// The precondition is not assumed silently. When the walk cannot complete — a
// body that is not an object, a top level that does not scan, anything after the
// closing brace, a member whose value is not valid JSON — the bytes go to
// [splitExtraSlow], which is the old implementation unchanged. So an input that
// violates the precondition gets encoding/json's error, exactly as before,
// rather than this scanner's opinion of it. `null` takes that path too, and
// costs nothing.
//
// One case is left: a MODELLED member whose value is malformed, which the walk
// skips structurally and therefore does not notice. `{"model":tru}` yields no
// extra members and no error here where encoding/json yields a syntax error. It
// is unreachable — the caller's json.Unmarshal already refused those bytes — and
// closing it would mean validating the whole body, which is the pass being
// removed. FuzzSplitExtra states exactly that: agreement on every input
// encoding/json accepts, and on every input it rejects for any other reason.
//
// TestSplitExtraAgreesWithUnmarshal and FuzzSplitExtra pin the two against each
// other on arbitrary bytes.

// SplitExtra returns the members of a JSON object that are not in known.
//
// It is a structural walk of the top level; see the file comment for the
// precondition that permits that and for what happens when it does not hold.
func SplitExtra(b []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	return splitExtra(b, known, false)
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
	return splitExtra(b, known, true)
}

func splitExtra(b []byte, known map[string]struct{}, fold bool) (map[string]json.RawMessage, error) {
	out, ok := scanExtra(b, known, fold)
	if !ok {
		return splitExtraSlow(b, known, fold)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// scanExtra walks the top-level members of b and collects the unmodelled ones.
//
// ok is false when the walk did not reach a clean end of document, which is the
// signal to fall back rather than a verdict about the body.
func scanExtra(b []byte, known map[string]struct{}, fold bool) (map[string]json.RawMessage, bool) {
	i := canonical.ScanSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		// Not an object: `null`, a bare string, a number. encoding/json has an
		// answer for each of those and it is not this function's to invent.
		return nil, false
	}
	i = canonical.ScanSpace(b, i+1)
	if i >= len(b) {
		return nil, false
	}
	if b[i] == '}' {
		return nil, tailIsBlank(b, i+1)
	}
	var out map[string]json.RawMessage
	for {
		// A member, and nothing else. The separators are matched exactly rather
		// than skipped past, because the alternative tolerates `{"a":1,}` and
		// `{,}` — which encoding/json rejects, and which the caller's decode has
		// therefore already rejected, but a walk that accepted them would be
		// answering a question about a document that does not exist. The
		// differential fuzzer found both.
		i = canonical.ScanSpace(b, i)
		if i >= len(b) || b[i] != '"' {
			return nil, false
		}
		keyStart := i
		keyEnd, esc, ok := canonical.ScanString(b, i)
		if !ok {
			return nil, false
		}
		j := canonical.ScanSpace(b, keyEnd)
		if j >= len(b) || b[j] != ':' {
			return nil, false
		}
		j = canonical.ScanSpace(b, j+1)
		if j >= len(b) {
			return nil, false
		}
		end := canonical.ScanValue(b, j)
		if end < 0 {
			return nil, false
		}
		key, modelled, ok := memberKey(b[keyStart:keyEnd], esc, known, fold)
		if !ok {
			return nil, false
		}
		if !modelled {
			// A member that survives is spliced VERBATIM into the next hop's body
			// by MarshalWithExtra, so it is the one span of the document whose
			// validity this function is on the hook for. Under the precondition it
			// is already valid and this scan finds nothing; without it, the
			// alternative is emitting bytes no parser will read.
			if !json.Valid(b[j:end]) {
				return nil, false
			}
			if out == nil {
				out = make(map[string]json.RawMessage, 4)
			}
			// A COPY, not a sub-slice of b. This is what json.RawMessage's own
			// UnmarshalJSON does, and it is what keeps an Extra map that outlives
			// the request buffer — every one of them does, they are relayed to the
			// next hop — from pointing into bytes somebody else may reuse. A key
			// that repeats resolves to the last member, which is encoding/json's
			// rule for a map.
			out[key] = append(json.RawMessage(nil), b[j:end]...)
		}
		i = canonical.ScanSpace(b, end)
		if i >= len(b) {
			return nil, false
		}
		switch b[i] {
		case ',':
			i++
		case '}':
			return out, tailIsBlank(b, i+1)
		default:
			return nil, false
		}
	}
}

// tailIsBlank reports whether nothing but JSON whitespace follows i. Anything
// else is trailing data, which encoding/json rejects and which the fallback
// therefore has to be the one to report.
func tailIsBlank(b []byte, i int) bool { return canonical.ScanSpace(b, i) == len(b) }

// memberKey resolves one key literal, quotes included, against the modelled set.
// key is meaningful only when the member is not modelled — that is the only case
// anything needs the string.
//
// The exact-match test runs on the RAW BYTES. A map lookup written as
// known[string(raw)] does not copy them, so for a body whose members are all
// modelled — which is every request dorang did not have to learn something new
// from — the walk finishes without allocating a single string. A member that is
// NOT modelled is about to be kept, and keeping it allocates anyway.
//
// The literal decodes to itself only when it is plain: no backslash escape, no
// control byte, nothing outside ASCII. Everything else goes through
// encoding/json, because encoding/json is what produced the keys this replaces
// and it does two things a copy would not — it resolves escapes, so "model"
// IS the key "model" and is therefore modelled, and it coerces invalid UTF-8 to
// U+FFFD, which is the spelling MarshalWithExtra will re-emit.
func memberKey(lit []byte, esc bool, known map[string]struct{}, fold bool) (key string, modelled, ok bool) {
	inner := lit[1 : len(lit)-1]
	if esc || !plainKey(inner) {
		var s string
		if err := json.Unmarshal(lit, &s); err != nil {
			return "", false, false
		}
		return s, isKnown(s, known, fold), true
	}
	if _, hit := known[string(inner)]; hit {
		return "", true, true
	}
	// The exact lookup has already missed, so without folding it is not modelled.
	s := string(inner)
	return s, fold && isKnown(s, known, true), true
}

// plainKey reports whether the bytes between the quotes decode to themselves.
// It mirrors the condition encoding/json's unquoteBytes uses to return its input
// untouched; a backslash cannot appear here because that is what esc reports.
func plainKey(k []byte) bool {
	for _, c := range k {
		if c < ' ' || c >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// isKnown reports whether a key is modelled. Under fold, a key that differs from
// a modelled name only by case is modelled too; see [SplitExtraFold].
func isKnown(key string, known map[string]struct{}, fold bool) bool {
	if _, ok := known[key]; ok {
		return true
	}
	if !fold {
		return false
	}
	for want := range known {
		if strings.EqualFold(key, want) {
			return true
		}
	}
	return false
}

// splitExtraSlow is the reference implementation: decode the object into a map
// and delete the modelled members. It is what [scanExtra] falls back to, and
// what the differential test measures against.
func splitExtraSlow(b []byte, known map[string]struct{}, fold bool) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	for k := range raw {
		if isKnown(k, known, fold) {
			delete(raw, k)
		}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return raw, nil
}
