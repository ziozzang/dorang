package openai

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/wirejson"
)

// Marshal encodes v the way the reference serializer does.
//
// It differs from json.Marshal in one respect that matters on the wire: HTML
// escaping is OFF. Go's default turns '<', '>' and '&' into <, > and
// &, so a model that emits "a && b" or a fragment of HTML produces bytes
// no other OpenAI-compatible server produces. Both forms decode to the same
// string, but a golden-byte comparison against a reference capture fails, and
// so does any client doing a substring match on the raw frame.
func Marshal(v any) ([]byte, error) { return wirejson.Marshal(v) }

// marshalTo encodes v into dst without an intermediate copy. The streaming
// writer uses it so that emitting a chunk costs one buffer, not two.
func marshalTo(dst *bytes.Buffer, v any) error { return wirejson.MarshalTo(dst, v) }

// strictUnmarshal decodes a REQUEST with case-sensitive field matching.
//
// COMPATIBILITY 2.0: encoding/json fills a `json:"model"` field from a member
// spelled "Model". The authorization gate (internal/server/peek.go) compares
// key bytes exactly and so does every backend downstream, so a struct decode
// here would resolve a model the gate never authorized. Every decode on the
// request path goes through this; see [canonical.StrictBytes] for the rule and
// for what happens to a colliding key.
//
// RESPONSES ARE DELIBERATELY NOT STRICT. A response body comes from a backend
// dorang chose, not from a caller: a case variation there is a vendor quirk and
// cannot bypass anything, because nothing is authorized against a response.
// Refusing to read "Usage" from a backend that spells it that way would turn a
// cosmetic upstream bug into a failed request and lost metering, so the
// response path keeps encoding/json's leniency. The one place the line is not
// clean is a type that appears on both paths — [Message] is a request message
// and a response choice — and those follow the request rule, because the
// request rule is the one with a security property attached.
func strictUnmarshal(b []byte, v any) error { return canonical.StrictUnmarshal(b, v) }

// strictBytes is [strictUnmarshal]'s filter on its own, for the UnmarshalJSON
// methods that read the SAME bytes twice — once with encoding/json to fill the
// struct, and once with [wirejson.SplitExtra]'s structural walk to collect the
// members the struct does not model. Filtering once and using the result for
// both is what keeps a dropped key from reappearing in Extra and being relayed
// to the next hop.
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

func getBuf() *bytes.Buffer  { return wirejson.GetBuf() }
func putBuf(b *bytes.Buffer) { wirejson.PutBuf(b) }

// appendWithExtra is marshalWithExtra writing into the caller's buffer.
//
// It is what a nested wire type uses so that encoding/json never sees it: a
// Marshaler's result is COMPACTED into its parent, which is a full scan of the
// subtree at every level of the nesting, and on a chat request that was 70% of
// the encode. See [wirejson.Append] for the plan and for what it refuses.
func appendWithExtra(dst []byte, v any, extra map[string]json.RawMessage, known map[string]struct{}) ([]byte, error) {
	return wirejson.AppendWithExtra(dst, v, extra, known)
}

// marshalAppender renders v as a standalone document through the append path.
// It is what a top-level Marshal entry point calls in place of [Marshal]: the
// document is built once instead of being compacted into a buffer once per
// level of the type it came from.
func marshalAppender(v any) ([]byte, error) { return wirejson.MarshalAppended(v) }

// appendValue writes v into dst, using v's own AppendJSON where it has one.
func appendValue(dst []byte, v any) ([]byte, error) { return wirejson.Append(dst, v) }

// appendString appends s as a JSON string literal with HTML escaping off.
func appendString(dst []byte, s string) []byte { return wirejson.AppendString(dst, s) }

// ptr is shorthand for taking the address of a literal, which this package does
// constantly because "absent" and "zero" are different on this wire
// (COMPATIBILITY 2.1).
func ptr[T any](v T) *T { return &v }

// isJSONObject reports whether b is, in its entirety, a JSON object.
//
// It is the precondition for asking a shape question at all. A body that is not
// an object cannot be checked for the presence of a member, and refusing it on
// that ground would refuse every non-JSON answer this package serves — an SRT
// transcript, a WAV file, an empty body.
func isJSONObject(b []byte) bool {
	b = trimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return false
	}
	return json.Valid(b)
}

// hasAnyMember reports whether a JSON object carries at least one of the named
// top-level keys.
//
// PRESENCE is the test, never the value. `{"text":""}` is a correct transcript
// of silence and `{"results":[]}` is a correct verdict on an empty input array;
// a check that looked at the value would refuse both.
//
// The comparison folds case for the same reason [wirejson.SplitExtraFold]
// exists: encoding/json fills these response structs case-insensitively, so a
// backend that spells the member "Results" has already populated the field, and
// a gate that then refused the body would reject an answer dorang can read.
func hasAnyMember(b []byte, names ...string) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return false
	}
	for k := range obj {
		for _, want := range names {
			if k == want || strings.EqualFold(k, want) {
				return true
			}
		}
	}
	return false
}
