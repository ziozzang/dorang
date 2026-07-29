package wirejson

import "encoding/json"

// UnmarshalSelf decodes b into v by calling v's own UnmarshalJSON, instead of
// asking encoding/json to hand v the bytes it was already given.
//
// # What the indirection costs
//
// json.Unmarshal(b, v) where v decodes itself scans b TWICE before v's method
// runs. checkValid walks the whole document to prove it, and then the decoder's
// skip() walks it again to find where the top-level value ends — and the answer
// both times is "all of it", because b was one value to begin with. v's method
// is then handed those same bytes and validates them a third time, since its own
// first act is a json.Unmarshal. Two of the three passes exist only to satisfy
// an interface. On a 4 KiB chat request they are 28% of the entire decode.
//
// # Why this is the same function and not merely a faster one
//
// The document is still validated exactly once, up front, and an invalid one is
// still handed to encoding/json so that the caller's error is the reference
// parser's error rather than this function's opinion. What is dropped is the
// SECOND walk — the decoder's skip() — whose only product is a slice of the
// bytes that were already in hand.
//
// Dropping the validation instead was tried and is wrong, which the differential
// fuzzer showed within a second: the strict filter (COMPATIBILITY 2.0) REMOVES a
// case-colliding member before the method decodes, so `{"Model":A}` becomes `{}`
// and the malformed value is never looked at. json.Unmarshal rejects that body
// and dorang must keep rejecting it — a request no other parser accepts must not
// become a request dorang forwards. The json.Valid below is what keeps that
// true, and it is the reason this function is not simply v.UnmarshalJSON(b).
//
// Given a valid document the two forms are the same by construction, not by
// observation. json.Valid reports that b holds exactly one value with nothing
// but whitespace around it; [TrimSpace] then yields precisely the bytes
// json.Unmarshal would have passed to the method. The one case where
// json.Unmarshal does not call the method at all is a top-level `null`, where it
// leaves the target untouched — so this does too, rather than relying on every
// type's method to decode "null" to the zero value it already held.
//
// Every application is still enumerated and run both ways over its package's
// corpus and its fuzzer; see TestDirectDecodeMatchesUnmarshal.
//
// # Where it is deliberately not used
//
// The chat and messages request and response decoders, and nothing else. The
// other entry points — speech, image, moderation, rerank, the Responses API —
// have the same shape and would take the same change, and they are left on
// json.Unmarshal because each application has to be added to a differential
// before it is trusted, and those endpoints are not on the path §15.1 measures.
// Adding one is adding a row to that table, not just a call.
func UnmarshalSelf(b []byte, v json.Unmarshaler) error {
	if !json.Valid(b) {
		return json.Unmarshal(b, v)
	}
	b = TrimSpace(b)
	if string(b) == "null" {
		return nil
	}
	return v.UnmarshalJSON(b)
}
