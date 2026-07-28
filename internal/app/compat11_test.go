package app

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
)

// COMPATIBILITY §11.2 asserted where a client stands: through the assembled
// gateway, on the four keys of the envelope plus the status.
//
// These are wire-shape assertions on purpose. A test comparing two constants
// passes whatever the constants are, which is how `invalid_body`,
// `invalid_credential` and `no_capacity` each survived a document that says
// otherwise — §11 opens by describing exactly that failure and the
// implementation was an instance of it.

// envelope is the four keys §7.1 fixes, decoded from the response body.
type envelope struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    string  `json:"code"`
	} `json:"error"`
}

func decodeEnvelope(t *testing.T, body []byte) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("body is not an error envelope: %v\n%s", err, body)
	}
	return e
}

// TestCompat11ErrorWireShape walks the §11.2 rows that a client can provoke
// over HTTP, on both families.
//
// The `param` column is checked too: §11.1's own worked example prints
// `"param":"model"` for an unknown model, and dorang answered null.
func TestCompat11ErrorWireShape(t *testing.T) {
	a := newWiringApp(t, wiringYAML, nil)
	secret := issueKey(t, a, nil)

	cases := []struct {
		name      string
		secret    string
		path      string
		body      string
		status    int
		typ       string
		code      string
		param     *string
		condition string
	}{
		{
			name:      "malformed request body, openai family",
			secret:    secret,
			path:      "/v1/chat/completions",
			body:      `["not an object"]`,
			status:    http.StatusBadRequest,
			typ:       "invalid_request_error",
			code:      "invalid_request",
			condition: "Malformed request body",
		},
		{
			name:      "malformed request body, anthropic family",
			secret:    secret,
			path:      "/v1/messages",
			body:      `["not an object"]`,
			status:    http.StatusBadRequest,
			typ:       "invalid_request_error",
			code:      "invalid_request",
			condition: "Malformed request body",
		},
		{
			name:      "missing credential",
			secret:    "",
			path:      "/v1/chat/completions",
			body:      `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`,
			status:    http.StatusUnauthorized,
			typ:       "authentication_error",
			code:      "invalid_api_key",
			condition: "Missing or malformed credential",
		},
		{
			name:      "malformed credential",
			secret:    "not-a-dorang-key", // pragma: allowlist secret — fixture
			path:      "/v1/chat/completions",
			body:      `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`,
			status:    http.StatusUnauthorized,
			typ:       "authentication_error",
			code:      "invalid_api_key",
			condition: "Missing or malformed credential",
		},
		{
			name:      "unknown credential",
			secret:    "sk-wiring-never-issued", // pragma: allowlist secret — test fixture
			path:      "/v1/chat/completions",
			body:      `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`,
			status:    http.StatusUnauthorized,
			typ:       "authentication_error",
			code:      "invalid_api_key",
			condition: "Missing or malformed credential",
		},
		{
			// §11.1's worked example, on the family it is printed for.
			name:      "unknown model, openai family",
			secret:    secret,
			path:      "/v1/chat/completions",
			body:      `{"model":"never-configured","messages":[{"role":"user","content":"hi"}]}`,
			status:    http.StatusNotFound,
			typ:       "invalid_request_error",
			code:      "model_not_found",
			param:     strPtr("model"),
			condition: "Unknown model",
		},
		{
			// Same condition, other column: the Anthropic family spells a 404
			// `not_found_error`.
			name:      "unknown model, anthropic family",
			secret:    secret,
			path:      "/v1/messages",
			body:      `{"model":"never-configured","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			status:    http.StatusNotFound,
			typ:       "not_found_error",
			code:      "model_not_found",
			param:     strPtr("model"),
			condition: "Unknown model",
		},
		{
			name:      "unknown resource, openai family",
			secret:    secret,
			path:      "/v1/models/never-configured",
			body:      "",
			status:    http.StatusNotFound,
			typ:       "invalid_request_error",
			code:      "model_not_found",
			param:     strPtr("model"),
			condition: "Unknown model",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			method := http.MethodPost
			if c.body == "" {
				method = http.MethodGet
			}
			w := callWith(a, c.secret, method, c.path, c.body)
			if w.Code != c.status {
				t.Fatalf("§11.2 %q: status %d, want %d\n%s",
					c.condition, w.Code, c.status, w.Body.String())
			}
			e := decodeEnvelope(t, w.Body.Bytes())
			if e.Error.Type != c.typ {
				t.Errorf("§11.2 %q: type %q, want %q", c.condition, e.Error.Type, c.typ)
			}
			if e.Error.Code != c.code {
				t.Errorf("§11.2 %q: code %q, want %q", c.condition, e.Error.Code, c.code)
			}
			switch {
			case c.param == nil && e.Error.Param != nil:
				t.Errorf("§11.1: param %q, want null", *e.Error.Param)
			case c.param != nil && e.Error.Param == nil:
				t.Errorf("§11.1: param null, want %q", *c.param)
			case c.param != nil && *e.Error.Param != *c.param:
				t.Errorf("§11.1: param %q, want %q", *e.Error.Param, *c.param)
			}
			if e.Error.Message == "" {
				t.Error("§7.1: message is empty on the wire")
			}
		})
	}
}

// TestCompat11RoutingRefusalWireShape covers the §11.2 rows a routing refusal
// raises. They are provoked from the real [router.Error] values internal/router
// constructs and rendered through the real response path, so the assertion is
// on the bytes and the status a client receives rather than on a constant.
//
// Reaching "everything is saturated" over HTTP needs a live capacity squeeze;
// what matters for §11.2 is the mapping from the refusal to the envelope, and
// that is what this pins.
func TestCompat11RoutingRefusalWireShape(t *testing.T) {
	cases := []struct {
		name       string
		in         *router.Error
		status     int
		openai     string
		anthropic  string
		code       string
		param      *string
		conditions string
	}{
		{
			name:       "no healthy deployment",
			in:         &router.Error{Status: 429, Code: router.CodeNoCandidate, Message: "m"},
			status:     429,
			openai:     "rate_limit_error",
			anthropic:  "overloaded_error",
			code:       "no_healthy_deployment",
			conditions: "No healthy deployment",
		},
		{
			name:       "capacity wait",
			in:         &router.Error{Status: 429, Code: router.CodeNoCapacity, Message: "m"},
			status:     429,
			openai:     "rate_limit_error",
			anthropic:  "overloaded_error",
			code:       "capacity_unavailable",
			conditions: "Capacity wait timed out",
		},
		{
			name:       "unknown model",
			in:         &router.Error{Status: 404, Code: router.CodeModelNotFound, Message: "m"},
			status:     404,
			openai:     "invalid_request_error",
			anthropic:  "not_found_error",
			code:       "model_not_found",
			param:      strPtr("model"),
			conditions: "Unknown model",
		},
		{
			name:       "context window exceeded",
			in:         &router.Error{Status: 400, Code: router.CodeContextWindow, Message: "m"},
			status:     400,
			openai:     "invalid_request_error",
			anthropic:  "invalid_request_error",
			code:       "context_length_exceeded",
			param:      strPtr("messages"),
			conditions: "Context window exceeded",
		},
		{
			name:       "provider quota exhausted",
			in:         &router.Error{Status: 429, Code: router.CodeQuotaExhausted, Message: "m"},
			status:     429,
			openai:     "rate_limit_error",
			anthropic:  "rate_limit_error",
			code:       "insufficient_quota",
			conditions: "Provider quota exhausted",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, f := range []struct {
				name   string
				family server.Family
				typ    string
			}{
				{"openai", server.FamilyOpenAIChat, c.openai},
				{"anthropic", server.FamilyAnthropicMessages, c.anthropic},
			} {
				out, ok := routeError(c.in).(*server.Error)
				if !ok {
					t.Fatalf("routeError did not produce a server error")
				}
				out = out.ForFamily(f.family)
				if out.StatusCode() != c.status {
					t.Errorf("§11.2 %q: status %d, want %d",
						c.conditions, out.StatusCode(), c.status)
				}
				e := decodeEnvelope(t, server.EncodeError(out))
				if e.Error.Type != f.typ {
					t.Errorf("§11.2 %q, %s family: type %q, want %q",
						c.conditions, f.name, e.Error.Type, f.typ)
				}
				if e.Error.Code != c.code {
					t.Errorf("§11.2 %q: code %q, want %q", c.conditions, e.Error.Code, c.code)
				}
				switch {
				case c.param == nil && e.Error.Param != nil:
					t.Errorf("§11.1 %q: param %q, want null", c.conditions, *e.Error.Param)
				case c.param != nil && e.Error.Param == nil:
					t.Errorf("§11.1 %q: param null, want %q", c.conditions, *c.param)
				case c.param != nil && *e.Error.Param != *c.param:
					t.Errorf("§11.1 %q: param %q, want %q", c.conditions, *e.Error.Param, *c.param)
				}
			}
		})
	}
}

// TestLegacyHeadersAreReachableFromConfiguration closes the other half of §7.7:
// the flag existed on server.Options and no configuration could set it, so the
// mirroring was unreachable in every real deployment however well it worked.
func TestLegacyHeadersAreReachableFromConfiguration(t *testing.T) {
	off := newWiringApp(t, wiringYAML, nil)
	secret := issueKey(t, off, nil)
	w := callWith(off, secret, http.MethodGet, "/v1/models", "")
	if got := w.Header().Get(server.LegacyHeaderCallID); got != "" {
		t.Errorf("%s mirrored with compat.legacy_headers unset: %q",
			server.LegacyHeaderCallID, got)
	}

	on := newWiringApp(t, wiringYAML+"\ncompat: {legacy_headers: true}\n", nil)
	secret = issueKey(t, on, nil)
	w = callWith(on, secret, http.MethodGet, "/v1/models", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	got := w.Header().Get(server.LegacyHeaderCallID)
	want := w.Header().Get(server.HeaderRequestID)
	if want == "" {
		t.Fatal("no request id on the response, so the mirror proves nothing")
	}
	if got != want {
		t.Errorf("%s = %q, want %q", server.LegacyHeaderCallID, got, want)
	}
}

func strPtr(s string) *string { return &s }
