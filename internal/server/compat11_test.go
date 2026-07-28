package server

import (
	"context"
	"fmt"
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
			name: "malformed body, anthropic family",
			path: "/v1/messages",
			body: `["not an object"]`,
			want: `{"error":{"message":"request body is not a JSON object","type":"invalid_request_error","param":null,"code":"invalid_request"}}`,
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
