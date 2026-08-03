package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The §11.3 half that was never built: the upstream's own type and code come
// back on headers, and its message does not come back at all.

// TestNativeErrorCodeReachesTheHeader is COMPATIBILITY §11.3's other promise.
//
// The envelope must carry a code that satisfies §7.1 — a string, and one dorang
// chose — while the backend's own spelling survives on
// x-dorang-native-error-code. Before this the numeric case was pasted into the
// envelope as its own digits and the object case as its own JSON source text, so
// a client switching on `code` got whichever of those the backend felt like, and
// the header did not exist at all.
func TestNativeErrorCodeReachesTheHeader(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantHeader string
		wantNumber bool
	}{
		{
			// vLLM's integer code (VLLM.md §2.2), and not equal to the status,
			// so it is visible which of the two the header carries.
			name:       "integer code",
			status:     429,
			body:       `{"error":{"message":"slow down","code":42901}}`,
			wantCode:   "429",
			wantHeader: "42901",
			wantNumber: true,
		},
		{
			name:       "code as an object",
			status:     400,
			body:       `{"error":{"message":"m","code":{"inner":"x"}}}`,
			wantCode:   "400",
			wantHeader: `{"inner":"x"}`,
		},
		{
			// A string code IS the envelope's code, so there is nothing left to
			// carry out of band. A header that repeated it would be present on
			// every error and would therefore signal nothing.
			name:       "string code stays in the envelope and sets no header",
			status:     404,
			body:       `{"error":{"message":"gone","code":"model_retired"}}`,
			wantCode:   "model_retired",
			wantHeader: "",
		},
		{
			name:       "no code at all",
			status:     500,
			body:       `{"error":{"message":"boom"}}`,
			wantCode:   "500",
			wantHeader: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := Normalize(c.status, []byte(c.body))
			if e.Code != c.wantCode {
				t.Errorf("envelope code %q, want %q", e.Code, c.wantCode)
			}
			if e.NativeCodeWasNumeric != c.wantNumber {
				t.Errorf("numeric flag %v, want %v", e.NativeCodeWasNumeric, c.wantNumber)
			}
			w := httptest.NewRecorder()
			WriteError(w, e)
			if got := w.Header().Get(HeaderNativeErrorCode); got != c.wantHeader {
				t.Errorf("%s: %q, want %q", HeaderNativeErrorCode, got, c.wantHeader)
			}
			// Whatever the backend sent, the envelope's code is a JSON string.
			if !strings.Contains(w.Body.String(), `"code":"`+c.wantCode+`"`) {
				t.Errorf("body %s", w.Body.String())
			}
		})
	}
}

// TestNativeErrorCodeHeaderIsBoundedAndCTLFree applies the same two controls the
// native type header gets. A code is upstream-controlled and about to become a
// response header value; "no backend does that" is not a control.
func TestNativeErrorCodeHeaderIsBoundedAndCTLFree(t *testing.T) {
	long := strings.Repeat("9", 4096)
	e := Normalize(400, []byte(`{"error":{"message":"m","code":`+long+`}}`))
	w := httptest.NewRecorder()
	WriteError(w, e)
	if got := w.Header().Get(HeaderNativeErrorCode); len(got) > nativeCodeHeaderLimit {
		t.Errorf("native code header is %d bytes, want at most %d", len(got), nativeCodeHeaderLimit)
	}

	split := Normalize(400, []byte(`{"error":{"message":"m","code":"a\r\nx-injected: 1"}}`))
	// A string code lands in the envelope rather than the header, so the header
	// must be absent — and the envelope escapes the CRLF as JSON.
	w2 := httptest.NewRecorder()
	WriteError(w2, split)
	if got := w2.Header().Get("X-Injected"); got != "" {
		t.Errorf("a code injected a header: %q", got)
	}
	if got := w2.Header().Get(HeaderNativeErrorCode); got != "" {
		t.Errorf("a string code set the native header: %q", got)
	}
}

// TestUpstreamMessageIsOnNoResponseHeader is the deliberate decision about what
// carries the upstream's message, asserted rather than described.
//
// COMPATIBILITY §11.3 keeps the backend's text out of the response body because
// several OpenAI-compatible servers quote the offending key back in a 401. A
// response header is read by the same client over the same connection, so the
// reasoning transfers unchanged and there is no x-dorang-native-error-message.
// The type and the code are enumerated tokens and do go out of band; the free
// text goes to the operator log instead.
func TestUpstreamMessageIsOnNoResponseHeader(t *testing.T) {
	const secret = "sk-live-DEADBEEFdeadbeef0123456789" // pragma: allowlist secret — test fixture
	e := Normalize(401, []byte(`{"error":{"message":"invalid api key `+secret+`","type":"AuthError","code":401}}`))
	if !strings.Contains(e.NativeMessage, secret) {
		t.Fatalf("the fixture did not reach NativeMessage: %q", e.NativeMessage)
	}

	w := httptest.NewRecorder()
	WriteError(w, e)
	for name, vals := range w.Header() {
		for _, v := range vals {
			if strings.Contains(v, secret) {
				t.Errorf("header %s relays the upstream message: %q", name, v)
			}
		}
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Errorf("body relays the upstream message: %s", w.Body.String())
	}
	// And the two that DO go out of band are there, so this test cannot pass by
	// the headers having been removed altogether.
	if got := w.Header().Get(HeaderNativeErrorType); got != "AuthError" {
		t.Errorf("%s %q, want AuthError", HeaderNativeErrorType, got)
	}
	if got := w.Header().Get(HeaderNativeErrorCode); got != "401" {
		t.Errorf("%s %q, want 401", HeaderNativeErrorCode, got)
	}
}

// TestUpstreamMessageReachesTheOperatorLog is the other half of that decision.
//
// The text has to be recoverable without a database round trip, and before this
// it was assigned to Result.NativeErrorMessage and read by nothing anywhere in
// the tree — so §11.3's "recorded in the ledger" was not true either and an
// upstream reason was erased outright.
func TestUpstreamMessageReachesTheOperatorLog(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	s := newTestServer(t, func(o *Options) {
		o.Logf = func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(format, args...))
		}
		o.Dispatcher = DispatchFunc(func(_ context.Context, _ *Request, _ http.ResponseWriter) error {
			return Normalize(503, []byte(
				`{"error":{"message":"glm-5 was retired at 2026-07-15","code":"model_retired"}}`))
		})
	})

	w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "glm-5") {
		t.Fatalf("the upstream's words are in the body: %s", w.Body.String())
	}
	// The upstream's string code DOES survive into the envelope — that is §7.1
	// working, and it is the half of the reason a client can already act on.
	if !strings.Contains(w.Body.String(), `"code":"model_retired"`) {
		t.Errorf("the upstream code did not survive: %s", w.Body.String())
	}
	var found bool
	for _, l := range lines {
		if strings.Contains(l, "glm-5 was retired at 2026-07-15") {
			found = true
		}
	}
	if !found {
		t.Errorf("the upstream reason reached no operator log line; lines: %v", lines)
	}
}

// TestTypeIsChosenByFamily is COMPATIBILITY §11.2's two type columns.
//
// The table gives one type per condition PER FAMILY and they are not the same
// function of the status: a 404 is `invalid_request_error` to an OpenAI client
// and `not_found_error` to an Anthropic one, and a 413 is `invalid_request_error`
// and `request_too_large`. Every 404 dorang raised used to answer with the
// Anthropic spelling on every route.
func TestTypeIsChosenByFamily(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		alt           string
		openai        string
		anthropicWant string
	}{
		{"unknown model", 404, "", TypeInvalidRequest, TypeNotFound},
		{"body over the cap", 413, "", TypeInvalidRequest, TypeRequestTooLarge},
		{"malformed body", 400, "", TypeInvalidRequest, TypeInvalidRequest},
		{"bad credential", 401, "", TypeAuthentication, TypeAuthentication},
		{"rate limit", 429, "", TypeRateLimit, TypeRateLimit},
		// The only rows chosen by condition rather than by status.
		{"no healthy deployment", 429, TypeOverloaded, TypeRateLimit, TypeOverloaded},
		{"gateway fault", 500, "", TypeAPIError, TypeAPIError},
		// §11.2 reads `api_error` in BOTH columns for this row: `timeout_error`
		// is dorang's own invention and in neither vendor's vocabulary.
		{"upstream timeout", 504, "", TypeAPIError, TypeAPIError},
		{"route unimplemented", 501, "", TypeAPIError, TypeAPIError},
		// overloaded_error is Anthropic vocabulary: it appears in no OpenAI
		// column of §11.2, so an OpenAI client branching on it lands in a
		// default case.
		{"upstream overloaded", 503, "", TypeAPIError, TypeOverloaded},
		{"anthropic 529", 529, "", TypeAPIError, TypeOverloaded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mk := func() *Error {
				e := NewError(c.status, "", "m")
				e.AltType = c.alt
				return e
			}
			if got := mk().ForFamily(FamilyOpenAIChat).Type; got != c.openai {
				t.Errorf("openai family type %q, want %q", got, c.openai)
			}
			if got := mk().ForFamily(FamilyAnthropicMessages).Type; got != c.anthropicWant {
				t.Errorf("anthropic family type %q, want %q", got, c.anthropicWant)
			}
		})
	}
}

// TestFamilyErrorWireShape drives the conditions internal/server itself raises
// through a real request and asserts the bytes, on both families.
//
// It asserts the ENVELOPE and not a constant. A test comparing two constants
// passes whatever they are; these fixed all four keys as a client reads them.
func TestFamilyErrorWireShape(t *testing.T) {
	s := newTestServer(t, nil)
	cases := []struct {
		name string
		path string
		body string
		want string
	}{
		{
			// §11.2 "Malformed request body": invalid_request_error /
			// invalid_request. It answered `invalid_body` for the life of the
			// project, which is dorang's word and not the contract's.
			name: "malformed body, openai family",
			path: "/v1/chat/completions",
			body: `["not an object"]`,
			want: `{"error":{"message":"request body is not a JSON object","type":"invalid_request_error","param":null,"code":"invalid_request"}}`,
		},
		{
			// The SAME condition on the other family, and §11.1 gives that
			// family a different OBJECT and not merely a different `type`
			// inside the same one. This golden asserted the OpenAI envelope on
			// an Anthropic route for the life of the project: the outer
			// `"type":"error"` the SDK dispatches on was simply absent, so the
			// correctly-spelled type inside was never reached.
			//
			// `param` and `code` are present because dorang emits the union of
			// §11.1's Anthropic object and §7.1's four keys; internal/wire/
			// anthropic's Error documents why, and every SDK in this family
			// tolerates unknown members of the error object.
			name: "malformed body, anthropic family",
			path: "/v1/messages",
			body: `["not an object"]`,
			want: `{"type":"error","error":{"type":"invalid_request_error","message":"request body is not a JSON object","param":null,"code":"invalid_request"}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(s, post(c.path, c.body))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", w.Code)
			}
			if got := w.Body.String(); got != c.want {
				t.Errorf("body\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// wireEnvelope is what a client decodes: BOTH of COMPATIBILITY §11.1's objects
// at once, so a test can ask which one arrived rather than assume.
//
// The outer Type is the whole point. §11.1 says of it: "The outer
// `"type":"error"` is load-bearing: the Anthropic SDK dispatches on it." A
// decoder that only knows the OpenAI object cannot tell the two apart — it
// reads the inner keys and reports success — which is how an envelope with the
// discriminator missing survived every existing test.
type wireEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string  `json:"type"`
		Message string  `json:"message"`
		Param   *string `json:"param"`
		Code    string  `json:"code"`
	} `json:"error"`
}

func decodeWire(t *testing.T, body []byte) wireEnvelope {
	t.Helper()
	var e wireEnvelope
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("body is not an error envelope: %v\n%s", err, body)
	}
	return e
}

// assertDispatchable checks the envelope a client of family f receives.
//
// "Dispatchable" is not a figure of speech: the Anthropic SDK selects its error
// class from the OUTER type, so its absence is not a cosmetic difference. The
// converse is checked too — the OpenAI object must not grow one — because §7.1
// fixes that object at four keys and a client diffing dorang's bytes against
// the reference would see an extra member on every error it ever receives.
func assertDispatchable(t *testing.T, f Family, condition string, e wireEnvelope) {
	t.Helper()
	switch {
	case f.Anthropic() && e.Type != "error":
		t.Errorf("§11.1 %q on %v: outer discriminator is %q, want \"error\" — "+
			"this family's SDK dispatches on it and cannot classify the failure without it",
			condition, f, e.Type)
	case !f.Anthropic() && e.Type != "":
		t.Errorf("§11.1 %q on %v: the OpenAI object grew an outer type %q",
			condition, f, e.Type)
	}
	if e.Error.Message == "" {
		t.Errorf("§7.1 %q on %v: message is empty on the wire", condition, f)
	}
}

// compat11Row is one row of COMPATIBILITY §11.2, as a test case: the condition,
// the status, the two type columns, and the code both columns share.
type compat11Row struct {
	name      string
	condition string
	err       func() *Error
	status    int
	openai    string
	anthropic string
	code      string
	param     *string
}

// compat11Rows is §11.2's table, every row of it, built the way each condition's
// raiser builds it.
//
// The two capacity rows set [Error.AltType] because their Anthropic spelling is
// chosen by CONDITION rather than by status — a 429 is `rate_limit_error` to
// both families when it is an actual rate limit — and that is what their raisers
// in internal/router do.
func compat11Rows() []compat11Row {
	raise := func(status int, code string) func() *Error {
		return func() *Error { return NewError(status, "", "m").WithCode(code) }
	}
	raiseAlt := func(status int, code, alt string) func() *Error {
		return func() *Error {
			e := NewError(status, "", "m").WithCode(code)
			e.AltType = alt
			return e
		}
	}
	raiseParam := func(status int, code, param string) func() *Error {
		return func() *Error { return NewError(status, "", "m").WithCode(code).WithParam(param) }
	}
	model, messages := "model", "messages"
	return []compat11Row{
		{"malformed body", "Malformed request body", raise(400, CodeInvalidRequest),
			400, TypeInvalidRequest, TypeInvalidRequest, CodeInvalidRequest, nil},
		{"unknown parameter", "Unknown or disallowed parameter", raise(400, "invalid_parameter"),
			400, TypeInvalidRequest, TypeInvalidRequest, "invalid_parameter", nil},
		{"downgrade refused", "Structural downgrade refused", raise(400, "unsupported_construct"),
			400, TypeInvalidRequest, TypeInvalidRequest, "unsupported_construct", nil},
		{"context window", "Context window exceeded", raiseParam(400, "context_length_exceeded", messages),
			400, TypeInvalidRequest, TypeInvalidRequest, "context_length_exceeded", &messages},
		{"budget exhausted", "Budget exhausted", raise(400, "budget_exceeded"),
			400, TypeInvalidRequest, TypeInvalidRequest, "budget_exceeded", nil},
		{"missing credential", "Missing or malformed credential", raise(401, "invalid_api_key"),
			401, TypeAuthentication, TypeAuthentication, "invalid_api_key", nil},
		{"revoked credential", "Expired or revoked credential", raise(401, "invalid_api_key"),
			401, TypeAuthentication, TypeAuthentication, "invalid_api_key", nil},
		{"model not allowed", "Model not in the key's allow-list", raise(403, "model_not_allowed"),
			403, TypePermission, TypePermission, "model_not_allowed", nil},
		{"route not allowed", "Route not permitted for this key", raise(403, "route_not_allowed"),
			403, TypePermission, TypePermission, "route_not_allowed", nil},
		{"key blocked", "Key blocked", raise(403, "key_blocked"),
			403, TypePermission, TypePermission, "key_blocked", nil},
		{"unknown model", "Unknown model", raiseParam(404, CodeModelNotFound, model),
			404, TypeInvalidRequest, TypeNotFound, CodeModelNotFound, &model},
		{"unknown resource", "Unknown resource", raise(404, "not_found"),
			404, TypeInvalidRequest, TypeNotFound, "not_found", nil},
		{"body too large", "Body over the size cap", raise(413, CodeRequestTooLarge),
			413, TypeInvalidRequest, TypeRequestTooLarge, CodeRequestTooLarge, nil},
		{"rate limit", "Rate limit (RPM/TPM)", raise(429, "rate_limit_exceeded"),
			429, TypeRateLimit, TypeRateLimit, "rate_limit_exceeded", nil},
		{"quota exhausted", "Provider quota exhausted", raise(429, "insufficient_quota"),
			429, TypeRateLimit, TypeRateLimit, "insufficient_quota", nil},
		{"no healthy deployment", "No healthy deployment",
			raiseAlt(429, "no_healthy_deployment", TypeOverloaded),
			429, TypeRateLimit, TypeOverloaded, "no_healthy_deployment", nil},
		{"capacity wait", "Capacity wait timed out",
			raiseAlt(429, "capacity_unavailable", TypeOverloaded),
			429, TypeRateLimit, TypeOverloaded, "capacity_unavailable", nil},
		{"gateway fault", "Gateway fault", raise(500, CodeInternalError),
			500, TypeAPIError, TypeAPIError, CodeInternalError, nil},
		{"route unimplemented", "Route declared but unimplemented", raise(501, CodeRouteNotImplemented),
			501, TypeAPIError, TypeAPIError, CodeRouteNotImplemented, nil},
		{
			// §11.2's 502 row, whose code column is "the upstream's own string
			// code when it sent one". It is built through [Normalize] rather
			// than by hand so that the pass-through internal/backend's own
			// tests assert is pinned on BOTH families: an Anthropic envelope
			// that dropped `code` would silently discard it.
			name:      "upstream 5xx",
			condition: "Upstream 5xx after fallback",
			err: func() *Error {
				return Normalize(502, []byte(`{"error":{"message":"gone","code":"model_retired"}}`))
			},
			status: 502, openai: TypeAPIError, anthropic: TypeAPIError, code: "model_retired",
		},
		{"upstream timeout", "Upstream timeout", raise(504, CodeTimeout),
			504, TypeAPIError, TypeAPIError, CodeTimeout, nil},
	}
}

// TestCompat11EnvelopeIsDispatchableOnBothFamilies is the test that would have
// caught it.
//
// Every §11.2 condition, rendered through the response path a client actually
// receives — [WriteError], the one [Server.fail] calls — and decoded from the
// bytes. For the whole life of the project every one of these produced the
// OpenAI object on an Anthropic route, so the outer discriminator that family's
// SDK dispatches on was absent on every error dorang has ever returned to a
// `/v1/messages` caller.
//
// It is deliberately the same table internal/app/compat11_test.go drives
// end-to-end, so the two families are pinned side by side and a row cannot be
// fixed on one and left on the other.
func TestCompat11EnvelopeIsDispatchableOnBothFamilies(t *testing.T) {
	families := []struct {
		name   string
		family Family
	}{
		{"openai", FamilyOpenAIChat},
		{"anthropic", FamilyAnthropicMessages},
		// The other Anthropic-shaped route. §11.1 is about the protocol the
		// caller speaks, not about one path.
		{"anthropic count_tokens", FamilyAnthropicCountTokens},
	}
	for _, c := range compat11Rows() {
		t.Run(c.name, func(t *testing.T) {
			for _, f := range families {
				want := c.openai
				if f.family.Anthropic() {
					want = c.anthropic
				}
				w := httptest.NewRecorder()
				WriteError(w, c.err().ForFamily(f.family))

				if w.Code != c.status {
					t.Errorf("§11.2 %q on %s: status %d, want %d",
						c.condition, f.name, w.Code, c.status)
				}
				e := decodeWire(t, w.Body.Bytes())
				assertDispatchable(t, f.family, c.condition, e)
				if e.Error.Type != want {
					t.Errorf("§11.2 %q on %s: type %q, want %q",
						c.condition, f.name, e.Error.Type, want)
				}
				if e.Error.Code != c.code {
					t.Errorf("§11.2 %q on %s: code %q, want %q",
						c.condition, f.name, e.Error.Code, c.code)
				}
				switch {
				case c.param == nil && e.Error.Param != nil:
					t.Errorf("§11.1 %q on %s: param %q, want null",
						c.condition, f.name, *e.Error.Param)
				case c.param != nil && e.Error.Param == nil:
					t.Errorf("§11.1 %q on %s: param null, want %q",
						c.condition, f.name, *c.param)
				case c.param != nil && *e.Error.Param != *c.param:
					t.Errorf("§11.1 %q on %s: param %q, want %q",
						c.condition, f.name, *e.Error.Param, *c.param)
				}
			}
		})
	}
}

// TestCompat11EnvelopeOverHTTP drives the conditions this package raises
// through a real request on both families, so the assertion covers the wiring
// and not only the renderer.
//
// [WriteError] rendering the right object proves nothing if [Server.fail] never
// tells it which family is asking — which is exactly how the defect survived: a
// correct renderer sat in internal/wire/anthropic with no caller anywhere in
// the tree.
func TestCompat11EnvelopeOverHTTP(t *testing.T) {
	cases := []struct {
		name      string
		condition string
		opts      func(*Options)
		body      string
		status    int
		openai    string
		anthropic string
		code      string
		auth      bool
	}{
		{
			name: "malformed body", condition: "Malformed request body",
			body: `["not an object"]`, status: 400, auth: true,
			openai: TypeInvalidRequest, anthropic: TypeInvalidRequest, code: CodeInvalidRequest,
		},
		{
			name: "missing credential", condition: "Missing or malformed credential",
			body: `{"model":"model-x"}`, status: 401, auth: false,
			openai: TypeAuthentication, anthropic: TypeAuthentication, code: "no_credential",
		},
		{
			name: "body over the cap", condition: "Body over the size cap",
			opts:   func(o *Options) { o.MaxBodyBytes = 8 },
			body:   `{"model":"model-x","messages":[{"role":"user","content":"a long body"}]}`,
			status: 413, auth: true,
			openai: TypeInvalidRequest, anthropic: TypeRequestTooLarge, code: CodeRequestTooLarge,
		},
		{
			name: "gateway fault", condition: "Gateway fault",
			opts: func(o *Options) {
				o.Logf = func(string, ...any) {}
				o.Dispatcher = DispatchFunc(func(context.Context, *Request, http.ResponseWriter) error {
					panic("the handler exploded")
				})
			},
			body: `{"model":"model-x"}`, status: 500, auth: true,
			openai: TypeAPIError, anthropic: TypeAPIError, code: CodeInternalError,
		},
		{
			// The upstream's own string code, through the real normalizer, on
			// both families.
			name: "upstream 5xx", condition: "Upstream 5xx after fallback",
			opts: func(o *Options) {
				o.Logf = func(string, ...any) {}
				o.Dispatcher = DispatchFunc(func(context.Context, *Request, http.ResponseWriter) error {
					return Normalize(502, []byte(`{"error":{"message":"gone","code":"model_retired"}}`))
				})
			},
			body: `{"model":"model-x"}`, status: 502, auth: true,
			openai: TypeAPIError, anthropic: TypeAPIError, code: "model_retired",
		},
		{
			name: "upstream timeout", condition: "Upstream timeout",
			opts: func(o *Options) {
				o.Dispatcher = DispatchFunc(func(context.Context, *Request, http.ResponseWriter) error {
					return NewError(http.StatusGatewayTimeout, TypeTimeout, "m").WithCode(CodeTimeout)
				})
			},
			body: `{"model":"model-x"}`, status: 504, auth: true,
			openai: TypeAPIError, anthropic: TypeAPIError, code: CodeTimeout,
		},
	}
	routes := []struct {
		name   string
		path   string
		family Family
	}{
		{"openai", "/v1/chat/completions", FamilyOpenAIChat},
		{"anthropic", "/v1/messages", FamilyAnthropicMessages},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t, c.opts)
			for _, rt := range routes {
				want := c.openai
				if rt.family.Anthropic() {
					want = c.anthropic
				}
				r := post(rt.path, c.body)
				if !c.auth {
					r.Header.Del(HeaderAuthorization)
				}
				w := do(s, r)
				if w.Code != c.status {
					t.Fatalf("§11.2 %q on %s: status %d, want %d\n%s",
						c.condition, rt.name, w.Code, c.status, w.Body.String())
				}
				e := decodeWire(t, w.Body.Bytes())
				assertDispatchable(t, rt.family, c.condition, e)
				if e.Error.Type != want {
					t.Errorf("§11.2 %q on %s: type %q, want %q",
						c.condition, rt.name, e.Error.Type, want)
				}
				if e.Error.Code != c.code {
					t.Errorf("§11.2 %q on %s: code %q, want %q",
						c.condition, rt.name, e.Error.Code, c.code)
				}
			}
		})
	}
}

// TestMidStreamErrorUsesTheFamilysFraming is the half a status code cannot
// reach: once the first frame is out the response is a 200 and the only channel
// left is the body.
//
// The two families frame that channel differently and the difference decides
// whether the client sees the error at all. COMPATIBILITY §1.1 and §1.3 give
// chat completions a data-only frame followed by `data: [DONE]`; §6.1 and §6.2
// give this other protocol `event: error` with BOTH lines, no `[DONE]`, and no
// `message_stop` after it. An Anthropic client dispatches on the event NAME, so
// a data-only frame is not misread — it is discarded, and a failed stream
// becomes indistinguishable from a truncated one.
func TestMidStreamErrorUsesTheFamilysFraming(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"x\":1}\n\n")
			return NewError(http.StatusBadGateway, TypeAPIError, "upstream died").
				WithCode("upstream_gone")
		})
	})

	t.Run("openai", func(t *testing.T) {
		w := do(s, post("/v1/chat/completions", `{"model":"model-x","stream":true}`))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 — the status went out with the first frame", w.Code)
		}
		body := w.Body.String()
		want := "data: {\"error\":{\"message\":\"upstream died\",\"type\":\"api_error\"," +
			"\"param\":null,\"code\":\"upstream_gone\"}}\n\ndata: [DONE]\n\n"
		if !strings.HasSuffix(body, want) {
			t.Errorf("§1.3 frames\n got %q\nwant a suffix of %q", body, want)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		w := do(s, post("/v1/messages", `{"model":"model-x","stream":true}`))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d, want 200 — the status went out with the first frame", w.Code)
		}
		body := w.Body.String()
		want := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\"," +
			"\"message\":\"upstream died\",\"param\":null,\"code\":\"upstream_gone\"}}\n\n"
		if !strings.HasSuffix(body, want) {
			t.Errorf("§6.1 frames\n got %q\nwant a suffix of %q", body, want)
		}
		// §6.2: this protocol has no [DONE]. Emitting one leaves a conforming
		// parser with a frame it cannot name.
		if strings.Contains(body, "[DONE]") {
			t.Errorf("§6.2: the chat-completions terminator is in an Anthropic stream: %q", body)
		}
		// And the frame a client dispatches on is decodable as this family's
		// envelope, which is the assertion the framing exists to make possible.
		name, data, ok := lastSSEFrame(body)
		if !ok {
			t.Fatalf("no SSE frame in %q", body)
		}
		if name != "error" {
			t.Errorf("§6.2: last frame is event %q, want \"error\"", name)
		}
		e := decodeWire(t, []byte(data))
		assertDispatchable(t, FamilyAnthropicMessages, "mid-stream failure", e)
		if e.Error.Code != "upstream_gone" {
			t.Errorf("code %q, want upstream_gone", e.Error.Code)
		}
	})
}

// lastSSEFrame returns the event name and data of the final frame in an SSE
// body. It parses rather than string-matches, because the claim under test is
// that a conforming parser can route the frame.
func lastSSEFrame(body string) (name, data string, ok bool) {
	frames := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
	if len(frames) == 0 {
		return "", "", false
	}
	for _, line := range strings.Split(frames[len(frames)-1], "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	return name, data, data != ""
}

// TestUnknownModelOnTheModelsRoute is §11.1's own worked example, byte for byte.
//
// §11.1 prints `{"error":{…,"type":"invalid_request_error","param":"model",
// "code":"model_not_found"}}` for exactly this condition, and §11 opens by
// describing a gateway whose error surface does not match the contract it
// publishes.
func TestUnknownModelOnTheModelsRoute(t *testing.T) {
	s := newTestServer(t, nil)
	w := do(s, get("/v1/models/never-existed"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", w.Code)
	}
	want := `{"error":{"message":"no such model","type":"invalid_request_error","param":"model","code":"model_not_found"}}`
	if got := w.Body.String(); got != want {
		t.Errorf("body\n got %s\nwant %s", got, want)
	}
}

// TestLegacyHeadersMirrorTheReferenceProxy is COMPATIBILITY §7.7.
//
// The document has claimed this mirroring since the section was written and no
// legacy spelling existed anywhere in the tree, so anything reading
// x-litellm-call-id broke silently on cutover — silently, because an absent
// header is not an error, it is a zero.
func TestLegacyHeadersMirrorTheReferenceProxy(t *testing.T) {
	// Off by default: they are another vendor's names.
	plain := newTestServer(t, nil)
	w := do(plain, post("/v1/chat/completions", `{"model":"model-x"}`))
	for _, name := range []string{LegacyHeaderCallID, LegacyHeaderModelID,
		LegacyHeaderResponseCost, HeaderRealModel} {
		if got := w.Header().Get(name); got != "" {
			t.Errorf("%s mirrored without compat.legacy_headers: %q", name, got)
		}
	}

	s := newTestServer(t, func(o *Options) { o.LegacyHeaders = true })
	r := post("/v1/chat/completions", `{"model":"model-x"}`)
	r.Header.Set(HeaderDetail, "full")
	w = do(s, r)
	h := w.Header()

	// Each mirror carries the same value as the dorang header it copies, which
	// is the whole contract: a reader of either name sees one answer.
	for _, p := range []struct{ legacy, native string }{
		{LegacyHeaderCallID, HeaderRequestID},
		{LegacyHeaderModelID, HeaderDeployment},
		{LegacyHeaderResponseCost, HeaderCostUSD},
		{LegacyHeaderKeySpend, HeaderSpendUSD},
		{LegacyHeaderResponseDuration, HeaderLatencyMS},
		{HeaderRealModel, HeaderUpstreamModel},
	} {
		got, want := h.Get(p.legacy), h.Get(p.native)
		if want == "" {
			t.Fatalf("the fixture set no %s, so %s proves nothing", p.native, p.legacy)
		}
		if got != want {
			t.Errorf("%s = %q, want %q (the value of %s)", p.legacy, got, want, p.native)
		}
	}
	// Retries, not attempts: dorang's first attempt is 1 and the reference
	// proxy's retry count for it is 0.
	if got, want := h.Get(LegacyHeaderAttemptedRetries), "0"; got != want {
		t.Errorf("%s = %q, want %q for a request that never retried",
			LegacyHeaderAttemptedRetries, got, want)
	}
}

// TestLegacyHeadersInheritTheDetailGate: a mirror that arrived when its source
// did not would be a second, disagreeing answer to what §10.4 says is always on.
func TestLegacyHeadersInheritTheDetailGate(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.LegacyHeaders = true })
	h := do(s, post("/v1/chat/completions", `{"model":"model-x"}`)).Header()

	for _, name := range []string{LegacyHeaderCallID, LegacyHeaderModelID, LegacyHeaderResponseCost} {
		if h.Get(name) == "" {
			t.Errorf("always-on mirror %s missing", name)
		}
	}
	for _, name := range []string{LegacyHeaderKeySpend, LegacyHeaderKeyMaxBudget,
		LegacyHeaderAttemptedRetries, LegacyHeaderResponseDuration} {
		if got := h.Get(name); got != "" {
			t.Errorf("detail mirror %s present without the opt-in: %q", name, got)
		}
	}
}

// TestEveryTypeOnTheWireIsInTheTable turns §11.2's own sentence — "dorang never
// emits a `type` outside this table" — into something that fails.
//
// It was not true. `timeout_error`, `not_implemented_error` and
// `service_unavailable_error` are in dorang's [knownTypes] and in neither
// vendor's vocabulary, and the first two went out on real 504s and 501s.
func TestEveryTypeOnTheWireIsInTheTable(t *testing.T) {
	families := []Family{FamilyOpenAIChat, FamilyAnthropicMessages, FamilyModels,
		FamilyAdmin, FamilyPassthrough, FamilyNone}
	for status := 100; status < 600; status++ {
		for _, f := range families {
			got := NewError(status, "", "m").ForFamily(f).Type
			if _, ok := compat11Types[got]; !ok {
				t.Fatalf("status %d, family %v: type %q is outside COMPATIBILITY §11.2's table",
					status, f, got)
			}
		}
		// And with an alternate spelling attached, which is the other way a
		// type reaches the wire.
		e := NewError(status, "", "m")
		e.AltType = TypeOverloaded
		if got := e.ForFamily(FamilyAnthropicMessages).Type; got != TypeOverloaded {
			t.Fatalf("status %d: AltType was not honoured, got %q", status, got)
		}
	}
	// 529 is Anthropic's own overloaded status and is not in the loop above.
	if got := NewError(529, "", "m").ForFamily(FamilyAnthropicMessages).Type; got != TypeOverloaded {
		t.Errorf("529 on the anthropic family: %q, want %q", got, TypeOverloaded)
	}
	if got := NewError(529, "", "m").ForFamily(FamilyOpenAIChat).Type; got != TypeAPIError {
		t.Errorf("529 on the openai family: %q, want %q", got, TypeAPIError)
	}
}

// TestGatewayFaultCarriesTheCompat11Code is §11.2's "Gateway fault" row.
//
// A 500 is the one status a client can infer nothing from, so the code is the
// whole of what it learns. The panic path answered with the stringified status,
// which tells it what it already had.
func TestGatewayFaultCarriesTheCompat11Code(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Logf = func(string, ...any) {}
		o.Dispatcher = DispatchFunc(func(context.Context, *Request, http.ResponseWriter) error {
			panic("the handler exploded")
		})
	})
	w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", w.Code, w.Body.String())
	}
	want := `{"error":{"message":"internal error","type":"api_error","param":null,"code":"internal_error"}}`
	if got := w.Body.String(); got != want {
		t.Errorf("body\n got %s\nwant %s", got, want)
	}
}

// TestCompat11RetryAfterOnEveryRetrySignallingStatus is COMPATIBILITY §11.4,
// pinned rather than asserted in prose.
//
// §11.4 is a wire contract and README calls this file's subject "wire contracts
// pinned as golden tests"; the contract said "429 and 503" while the code tested
// `status == 429` in both places that could emit the header, so a provider's own
// `Retry-After` on an overloaded 503 or 529 was parsed by internal/backend,
// carried on Error.RetryAfterSeconds the whole way to WriteError and dropped
// there. The document was the only record of the promise, and prose does not
// fail a build.
//
// Both directions are pinned. The negative half matters as much as the positive
// one: §11.4 is now a claim about exactly three statuses, so a 500 or a 502
// carrying an upstream's stray Retry-After must NOT publish it — a status that
// makes no claim about recovery cannot be handed a delay to act on.
func TestCompat11RetryAfterOnEveryRetrySignallingStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"rate limited", http.StatusTooManyRequests, "11"},
		{"unavailable", http.StatusServiceUnavailable, "11"},
		{"anthropic overloaded", 529, "11"},
		{"gateway fault carries no delay", http.StatusInternalServerError, ""},
		{"bad gateway carries no delay", http.StatusBadGateway, ""},
		{"a refusal is not a wait", http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, func(o *Options) {
				o.Logf = func(string, ...any) {}
				o.Dispatcher = DispatchFunc(func(context.Context, *Request, http.ResponseWriter) error {
					e := NewError(tc.status, TypeForStatus(tc.status), "upstream said wait")
					// Exactly what internal/backend's upstreamError fills from
					// the provider's own header, for any status it relays.
					e.RetryAfterSeconds = 11
					return e
				})
			})
			w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if got := w.Header().Get(HeaderRetryAfter); got != tc.want {
				t.Errorf("Retry-After %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCompat11RetryAfterIsNotGatedBehindTheDetailHeader is the other half of
// §11.4's opening sentence, checked on the status the old gate did not cover.
//
// DESIGN §10.4's bounding rule is about the x-dorang-* set: a header the client
// ACTS ON is unconditional, a header the client merely READS may be gated. A 503
// that carries the delay only for callers who asked for telemetry would be the
// rule applied backwards.
func TestCompat11RetryAfterIsNotGatedBehindTheDetailHeader(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Logf = func(string, ...any) {}
		o.Dispatcher = DispatchFunc(func(context.Context, *Request, http.ResponseWriter) error {
			e := NewError(http.StatusServiceUnavailable, TypeOverloaded, "the upstream provider is overloaded")
			e.RetryAfterSeconds = 30
			return e.WithCode("overloaded")
		})
	})
	r := post("/v1/chat/completions", `{"model":"model-x"}`)
	// No x-dorang-detail: full. The plain caller is the one this is for.
	w := do(s, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(HeaderRetryAfter); got != "30" {
		t.Fatalf("Retry-After %q, want 30 without the detail opt-in", got)
	}
	if got := w.Header().Get(HeaderAttempt); got != "" {
		t.Errorf("the detail set leaked onto a plain caller: x-dorang-attempt = %q", got)
	}
}
