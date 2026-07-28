package openai

import (
	"bytes"

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
// methods that must decode the SAME bytes twice — once into the struct and once
// into the raw member map that feeds Extra. Filtering once and using the result
// for both is what keeps a dropped key from reappearing in Extra and being
// relayed to the next hop.
func strictBytes(b []byte, v any) []byte { return canonical.StrictBytes(b, v) }

func getBuf() *bytes.Buffer  { return wirejson.GetBuf() }
func putBuf(b *bytes.Buffer) { wirejson.PutBuf(b) }

// ptr is shorthand for taking the address of a literal, which this package does
// constantly because "absent" and "zero" are different on this wire
// (COMPATIBILITY 2.1).
func ptr[T any](v T) *T { return &v }
