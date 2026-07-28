package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// FuzzPeekRequest holds the hand-rolled scanner to encoding/json's answer.
//
// A body scanner that is faster than the reference parser and disagrees with it
// is worse than no optimization: the model name decides authorization and
// routing, so disagreeing means dorang authorizes one model and dispatches
// another. The oracle is the parser every client uses.
func FuzzPeekRequest(f *testing.F) {
	seeds := []string{
		`{"model":"model-x"}`,
		`{"model":"model-x","stream":true}`,
		`{"stream":false,"model":"a:b:c"}`,
		`{"model":"a:b"}`,
		`{"model":"slow-path"}`,
		`{"messages":[{"role":"user","content":"{\"model\":\"decoy\"}"}],"model":"real"}`,
		`{"model":"x","model":"y"}`,
		`{ "model" : "spaced" , "stream" : true }`,
		`{"model":"🚀"}`,
		`{"tools":[{"function":{"name":"a\"b"}}],"model":"m"}`,
		`{}`,
		`{"model":""}`,
		`{"a":[[[[]]]],"model":"deep"}`,
		`{"a":null,"b":-1.5e10,"model":"m","stream":true}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	// The oracle is a map decode, not a struct decode. encoding/json matches
	// struct tags **case-insensitively** — it will fill a `json:"stream"` field
	// from a key spelled "streAm" — and that is a Go-specific behavior, not
	// JSON semantics. Every server dorang talks to is case-sensitive here, so
	// matching Go's leniency at the gate would authorize a request against a
	// field the backend is about to ignore. The map decode gives exact-key
	// semantics, which is the contract the scanner is held to.
	//
	// This also flags a hazard for any package that decodes the same body into
	// a struct: it will read {"Model":"x"} as a model and this scanner will
	// not. The gate and the wire adapter must agree, and the way they agree is
	// that neither invents a key the client did not send.
	f.Fuzz(func(t *testing.T, data []byte) {
		model, stream, ok := peekRequest(data)

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
			// Not a JSON object. The scanner may say anything as long as it
			// did not panic; the server refuses the request on its own path.
			return
		}
		if !ok {
			t.Fatalf("scanner rejected a JSON object: %q", data)
		}

		if v, present := raw["model"]; present {
			var want string
			if err := json.Unmarshal(v, &want); err == nil && model != want {
				t.Fatalf("model %q, JSON says %q, body %q", model, want, data)
			}
		} else if model != "" {
			t.Fatalf("model %q invented for a body with no model key: %q", model, data)
		}

		wantStream := string(bytes.TrimSpace(raw["stream"])) == "true"
		if stream != wantStream {
			t.Fatalf("stream %v, JSON says %v, body %q", stream, wantStream, data)
		}
	})
}

// FuzzNormalize requires the error normalizer to survive any upstream body and
// always produce the envelope of COMPATIBILITY §7.1.
//
// The input here is attacker-adjacent: a compromised or merely broken backend
// controls it completely, and its output goes straight to a client that will
// parse it. "Always four keys, code always a string, always valid JSON" is what
// makes that safe.
func FuzzNormalize(f *testing.F) {
	seeds := []string{
		`{"error":{"message":"m","type":"t","param":"p","code":"c"}}`,
		`{"object":"error","message":"m","type":"BadRequest","code":400}`,
		`{"error":"Unauthorized"}`,
		`{"type":"error","error":{"type":"invalid_request_error","message":"m"}}`,
		`{"detail":[{"loc":["body"],"msg":"bad"}]}`,
		`<html>502</html>`,
		``,
		`{"error":{"message":"\ud800","code":{"a":[1,2]}}}`,
		`{"error":{}}`,
		`{"error":[1,2,3]}`,
	}
	for _, s := range seeds {
		f.Add(400, []byte(s))
	}

	f.Fuzz(func(t *testing.T, status int, body []byte) {
		if status < 0 || status > 5999 {
			return
		}
		e := Normalize(status, body)
		if e == nil {
			t.Fatal("Normalize returned nil")
		}
		enc := EncodeError(e)

		var round struct {
			Error *struct {
				Message *string          `json:"message"`
				Type    *string          `json:"type"`
				Param   *string          `json:"param"`
				Code    *json.RawMessage `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(enc, &round); err != nil {
			t.Fatalf("envelope is not valid JSON: %s (%v)", enc, err)
		}
		if round.Error == nil {
			t.Fatalf("envelope has no error object: %s", enc)
		}
		if round.Error.Message == nil || *round.Error.Message == "" {
			t.Fatalf("envelope has no message: %s", enc)
		}
		if round.Error.Type == nil || !KnownTypes(*round.Error.Type) {
			t.Fatalf("envelope type is outside the canonical vocabulary: %s", enc)
		}
		if round.Error.Code == nil {
			t.Fatalf("envelope has no code: %s", enc)
		}
		if raw := *round.Error.Code; len(raw) == 0 || raw[0] != '"' {
			t.Fatalf("code is not a JSON string: %s", enc)
		}
	})
}

// FuzzRouteLookup requires the matcher to survive any path, and requires every
// claimed match to be substantiable: a captured parameter must actually appear
// in the path it was captured from.
func FuzzRouteLookup(f *testing.F) {
	for _, s := range []string{
		"/v1/chat/completions", "/v1/chat/completions/", "//", "/",
		"/openai/deployments/m/chat/completions", "/anthropic", "/anthropic/",
		"/anthropic/v1/messages", "", "no-leading-slash", "/a//b",
		"/anthropic/../x", "/openai/deployments//chat/completions",
	} {
		f.Add(s)
	}

	noop := func(w http.ResponseWriter, rq *Request) error { return nil }
	table, err := newRouteTable([]*Route{
		{Pattern: "/v1/chat/completions", Methods: MethodPOST, Name: "chat", ModelAuth: ModelAuthNone, Handler: noop},
		{Pattern: "/openai/deployments/{model}/chat/completions",
			Methods: MethodPOST, Name: "azure", ModelAuth: ModelAuthNone, Handler: noop},
		{Pattern: "/anthropic/{ptpath...}", Methods: MethodPOST, Name: "pt", ModelAuth: ModelAuthNone, Handler: noop},
	})
	if err != nil {
		f.Fatalf("newRouteTable: %v", err)
	}

	f.Fuzz(func(t *testing.T, path string) {
		var params [maxParams]Param
		rt, np, res := table.lookup("POST", path, &params)
		if res != lookupHit {
			return
		}
		if rt == nil {
			t.Fatalf("hit with a nil route for %q", path)
		}
		if np < 0 || np > maxParams {
			t.Fatalf("%q: %d parameters captured, array holds %d", path, np, maxParams)
		}
		for i := 0; i < np; i++ {
			if params[i].Name == "" {
				t.Fatalf("%q: parameter %d has no name", path, i)
			}
			if !strings.Contains(path, params[i].Value) {
				t.Fatalf("%q: captured %q, which is not in the path",
					path, params[i].Value)
			}
		}
	})
}
