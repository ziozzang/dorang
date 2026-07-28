package server

import "testing"

// A JSON escape for the letter that starts the key, so these spell "model" and
// "stream" to every conforming parser while a raw byte comparison sees
// something else. Kept as constants so the intent survives copy-paste, which
// strips them.
const (
	escModelKey  = `\u006dodel`
	escStreamKey = `\u0073tream`
)

// The three ways the gate used to disagree with every adapter. Each authorized
// one thing and dispatched another; the first failed OPEN, which is why it is
// first.
func TestGateAgreesWithAdaptersOnAdversarialBodies(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantModel  string
		wantStream bool
	}{
		{
			// Was: the fast path stopped at "y", and the escaped-key fallback ran
			// only when no model had been found at all — so it never ran. Every
			// adapter unescapes and takes the last duplicate, "x", meaning the
			// key's allow-list was checked against a model never dispatched.
			name:      "escaped duplicate key overrides the plain one",
			body:      `{"model":"y","` + escModelKey + `":"x"}`,
			wantModel: "x",
		},
		{
			// Was: the fallback decoded into a tagged struct, and encoding/json
			// matches tags case-insensitively with Unicode folding — putting
			// W10's defect back inside the gate itself.
			name:      "an escaped key differing only in case is not the model",
			body:      `{"\u004Dodel":"x"}`,
			wantModel: "",
		},
		{
			// Was: stream was OR-ed, never assigned, so the gate prepared an SSE
			// response — in-band error path and all — for a call that comes back
			// whole. Found by fuzzing, not by reading.
			name:       "a later stream:false clears an earlier true",
			body:       `{"model":"m","stream":true,"stream":false}`,
			wantModel:  "m",
			wantStream: false,
		},
		{
			name:       "a later stream:true sets it",
			body:       `{"model":"m","stream":false,"stream":true}`,
			wantModel:  "m",
			wantStream: true,
		},
		{
			name:      "a plain duplicate takes the last",
			body:      `{"model":"a","model":"b"}`,
			wantModel: "b",
		},
		{
			// encoding/json documents that null has no effect on the value, and
			// the adapters inherit that. The gate must agree.
			name:      "a null never overwrites an earlier name",
			body:      `{"model":"a","model":null}`,
			wantModel: "a",
		},
		{
			name:      "an escaped key spelling model is honoured",
			body:      `{"` + escModelKey + `":"x"}`,
			wantModel: "x",
		},
		{
			name:       "an escaped stream key is honoured",
			body:       `{"model":"m","` + escStreamKey + `":true}`,
			wantModel:  "m",
			wantStream: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, stream, ok := peekRequest([]byte(tc.body))
			if !ok {
				t.Fatalf("peekRequest reported failure on %s", tc.body)
			}
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q\nbody: %s", model, tc.wantModel, tc.body)
			}
			if stream != tc.wantStream {
				t.Errorf("stream = %v, want %v\nbody: %s", stream, tc.wantStream, tc.body)
			}
		})
	}
}
