package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAllSixAuthHeadersAuthenticate is COMPATIBILITY §7.3's first half. Any one
// of the six authenticates; a client configured for Anthropic, for Google, for
// Azure API Management or for the reference proxy all work against the same
// deployment without being reconfigured.
func TestAllSixAuthHeadersAuthenticate(t *testing.T) {
	s := newTestServer(t, nil)
	for _, name := range AuthHeaders() {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"model-x"}`))
			if name == HeaderAuthorization {
				r.Header.Set(name, "Bearer good")
			} else {
				r.Header.Set(name, "good")
			}
			w := do(s, r)
			if w.Code != http.StatusOK {
				t.Fatalf("%s: status %d body %s", name, w.Code, w.Body.String())
			}
		})
	}
	if len(AuthHeaders()) != 6 {
		t.Fatalf("accepted header count %d, want 6", len(AuthHeaders()))
	}
}

// TestAllSixAuthHeadersAreStripped is the second half, and the one that matters
// for security. A client credential that reaches a provider is a leak whether
// or not it was the credential that authenticated — so every one of the six
// comes off, together, regardless of which was used.
func TestAllSixAuthHeadersAreStripped(t *testing.T) {
	h := http.Header{}
	for _, name := range AuthHeaders() {
		h.Set(name, "secret-"+name)
	}
	// Non-canonical spellings, as a map assembled in code would hold them.
	h["authorization"] = []string{"Bearer sneaky"}
	h["X-API-KEY"] = []string{"sneaky"}
	h["Other"] = []string{"kept"}

	StripAuthHeaders(h)

	for k, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, "secret-") || strings.Contains(v, "sneaky") {
				t.Errorf("header %q survived stripping with value %q", k, v)
			}
		}
	}
	if h.Get("Other") != "kept" {
		t.Error("stripping removed an unrelated header")
	}
}

// TestUnauthenticatedIsRefused checks the envelope, not only the status: a
// client branches on the code.
func TestUnauthenticatedIsRefused(t *testing.T) {
	s := newTestServer(t, nil)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-x"}`))
	w := do(s, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
	want := `{"error":{"message":"no credential presented","type":"authentication_error","param":null,"code":"no_credential"}}`
	if got := w.Body.String(); got != want {
		t.Errorf("body\n got %s\nwant %s", got, want)
	}
}

// TestUnimplementedIsNeverASilent404 is DESIGN §0.2 and COMPATIBILITY §9. The
// two codes let a client tell "dorang has not built this yet" from "you have a
// typo", which a bare 404 does not.
func TestUnimplementedIsNeverASilent404(t *testing.T) {
	s := newTestServer(t, nil)
	cases := []struct {
		path, code string
	}{
		{"/v1/responses/resp_1/cancel", "route_not_implemented"},
		{"/v1/ocr", "route_not_implemented"},
		{"/key/generate", "route_not_implemented"},
		{"/v1/vector_stores", "route_not_implemented"},
		{"/definitely/not/a/route", "route_unknown"},
	}
	for _, c := range cases {
		w := do(s, post(c.path, `{}`))
		if w.Code != http.StatusNotImplemented {
			t.Errorf("%s: status %d, want 501", c.path, w.Code)
		}
		if got := w.Header().Get(HeaderUnimplemented); got != c.path {
			t.Errorf("%s: unimplemented header %q", c.path, got)
		}
		body := w.Body.String()
		if !strings.Contains(body, `"code":"`+c.code+`"`) {
			t.Errorf("%s: body %s, want code %s", c.path, body, c.code)
		}
		if !strings.Contains(body, `"param":"`+c.path+`"`) {
			t.Errorf("%s: body %s, want the path in param", c.path, body)
		}
	}
}

// TestModelsGolden fixes the exact bytes of GET /v1/models, including the
// constant `created` that clients cache on (COMPATIBILITY §7.4).
func TestModelsGolden(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Models = ModelSlice{
			{ID: "model-x"},
			{ID: "vendor:model-5.1", OwnedBy: "vendor"},
			{ID: "model-flash:cloud"},
		}
	})
	w := do(s, get("/v1/models"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	want := `{"object":"list","data":[` +
		`{"id":"model-x","object":"model","created":1677610602,"owned_by":"dorang"},` +
		`{"id":"vendor:model-5.1","object":"model","created":1677610602,"owned_by":"vendor"},` +
		`{"id":"model-flash:cloud","object":"model","created":1677610602,"owned_by":"dorang"}]}`
	if got := w.Body.String(); got != want {
		t.Errorf("body\n got %s\nwant %s", got, want)
	}
	// The colon-bearing names above are DESIGN §2.1's golden cases: a model
	// name is an opaque string and no component splits it on any character.
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("content type %q", w.Header().Get("Content-Type"))
	}
}

// TestModelsFilteredByKeyAllowList: the list a client reads must not contain a
// name that will be refused at 401 when it picks one.
func TestModelsFilteredByKeyAllowList(t *testing.T) {
	auth := newFakeAuth()
	auth.keys["limited"] = &fakePrincipal{id: "k1", models: []string{"model-y"}}
	s := newTestServer(t, func(o *Options) {
		o.Auth = auth
		o.Models = ModelSlice{{ID: "model-x"}, {ID: "model-y"}, {ID: "model-z"}}
	})
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set(HeaderXAPIKey, "limited")
	w := do(s, r)
	want := `{"object":"list","data":[{"id":"model-y","object":"model","created":1677610602,"owned_by":"dorang"}]}`
	if got := w.Body.String(); got != want {
		t.Errorf("filtered list\n got %s\nwant %s", got, want)
	}
}

// TestModelNotAllowedIs403 pins COMPATIBILITY §11.2: the credential
// authenticated, it is simply not permitted this model, so the answer is 403
// permission_error. A reference proxy answers 401 here; dorang deliberately does
// not follow it, because "re-authenticate" is advice that cannot help.
func TestModelNotAllowedIs403(t *testing.T) {
	auth := newFakeAuth()
	auth.keys["limited"] = &fakePrincipal{id: "k1", models: []string{"model-y"}}
	s := newTestServer(t, func(o *Options) { o.Auth = auth })
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-x"}`))
	r.Header.Set(HeaderXAPIKey, "limited")
	w := do(s, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403 (COMPATIBILITY §11.2)", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"type":"permission_error"`) {
		t.Errorf("type must be permission_error, body %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"model_not_allowed"`) {
		t.Errorf("body %s", w.Body.String())
	}
}

// sseStream is the exact frame sequence of a small chat-completions stream,
// as COMPATIBILITY §1.1 and §1.2 specify: data: <json> per frame, no event:
// line, no id: line, terminated by data: [DONE].
const sseStream = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"model-x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"model-x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"model-x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

func streamDispatcher() Dispatcher {
	return DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
		rq.Result.UpstreamModel = "upstream-model-x"
		rq.Result.Deployment = "dep-1"
		rq.Result.Tokens = Usage{Input: 7, Output: 2}
		rq.Result.CostNanoUSD = 9000
		rq.Result.Priced = true
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		_, err := io.WriteString(w, sseStream)
		return err
	})
}

// TestStreamIsByteFaithfulWithoutOptIn is DESIGN §10.4 [R1-C9]. Without the
// opt-in the stream stays byte-identical to what the dispatcher wrote: no
// dorang-authored frame is injected, and no trailer is set — trailers are a
// dead channel that mainstream LLM clients never surface.
func TestStreamIsByteFaithfulWithoutOptIn(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Dispatcher = streamDispatcher() })
	w := do(s, post("/v1/chat/completions", `{"model":"model-x","stream":true}`))

	if got := w.Body.String(); got != sseStream {
		t.Errorf("stream bytes\n got %q\nwant %q", got, sseStream)
	}
	if len(w.Result().Trailer) != 0 {
		t.Errorf("trailers were set: %v", w.Result().Trailer)
	}
	// The join key is on the response regardless, which is what makes the
	// ledger the answer for a caller who did not opt in.
	if w.Header().Get(HeaderRequestID) == "" {
		t.Error("request id header missing")
	}
}

// TestStreamUsageEventOnlyWithOptIn checks the other half: with the header, one
// extra frame, named so a client can filter it.
func TestStreamUsageEventOnlyWithOptIn(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Dispatcher = streamDispatcher() })
	r := post("/v1/chat/completions", `{"model":"model-x","stream":true}`)
	r.Header.Set(HeaderUsageEvents, "1")
	w := do(s, r)

	body := w.Body.String()
	if !strings.HasPrefix(body, sseStream) {
		t.Fatalf("upstream frames were altered: %q", body)
	}
	extra := strings.TrimPrefix(body, sseStream)
	want := "event: dorang.usage\ndata: {\"request_id\":\"" + w.Header().Get(HeaderRequestID) +
		"\",\"model\":\"model-x\",\"upstream_model\":\"upstream-model-x\",\"deployment\":\"dep-1\"," +
		"\"usage\":{\"input_tokens\":7,\"output_tokens\":2},\"cost_usd\":\"0.000009\"}\n\n"
	if extra != want {
		t.Errorf("usage frame\n got %q\nwant %q", extra, want)
	}
}

// TestExtensionHeadersAreBounded is DESIGN §10.4's bounding rule: identification
// and cost always, everything else only when asked for.
func TestExtensionHeadersAreBounded(t *testing.T) {
	s := newTestServer(t, nil)

	w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	h := w.Header()
	for _, name := range []string{HeaderRequestID, HeaderModel, HeaderUpstreamModel,
		HeaderDeployment, HeaderCostUSD} {
		if h.Get(name) == "" {
			t.Errorf("always-on header %s missing", name)
		}
	}
	for _, name := range []string{HeaderProvider, HeaderCredential, HeaderAttempt,
		HeaderRouteReason, HeaderLatencyMS, HeaderTokensInput, HeaderSpendUSD} {
		if h.Get(name) != "" {
			t.Errorf("detail header %s present without the opt-in", name)
		}
	}
	if h.Get(HeaderRealModel) != "" {
		t.Error("legacy header mirrored without compat.legacy_headers")
	}

	r := post("/v1/chat/completions", `{"model":"model-x"}`)
	r.Header.Set(HeaderDetail, "full")
	w = do(s, r)
	h = w.Header()
	for _, name := range []string{HeaderProvider, HeaderCredential, HeaderAttempt,
		HeaderRouteReason, HeaderLatencyMS, HeaderTokensInput, HeaderTokensOutput} {
		if h.Get(name) == "" {
			t.Errorf("detail header %s missing with x-dorang-detail: full", name)
		}
	}
	if got := h.Get(HeaderCostUSD); got != "0.000123456" {
		t.Errorf("cost header %q, want 0.000123456", got)
	}
}

// TestAlwaysFullHeadersOption is the deployment-wide form of the same opt-in.
func TestAlwaysFullHeadersOption(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.AlwaysFullHeaders = true
		o.LegacyHeaders = true
	})
	w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	if w.Header().Get(HeaderProvider) == "" {
		t.Error("detail header missing with always_full_headers")
	}
	if w.Header().Get(HeaderRealModel) != "upstream-model-x" {
		t.Errorf("legacy header %q", w.Header().Get(HeaderRealModel))
	}
}

// TestInboundRequestIDIsHonored is COMPATIBILITY §7.8.
func TestInboundRequestIDIsHonored(t *testing.T) {
	s := newTestServer(t, nil)
	r := post("/v1/chat/completions", `{"model":"model-x"}`)
	r.Header.Set("X-Request-Id", "caller-supplied-42")
	w := do(s, r)
	if got := w.Header().Get(HeaderRequestID); got != "caller-supplied-42" {
		t.Errorf("request id %q, want the inbound one", got)
	}

	// A header value a client cannot legally set is not echoed back into one.
	r = post("/v1/chat/completions", `{"model":"model-x"}`)
	r.Header["X-Request-Id"] = []string{"bad\x00value"}
	w = do(s, r)
	if got := w.Header().Get(HeaderRequestID); got == "bad\x00value" {
		t.Error("a control character was echoed into a response header")
	}
}

// TestHealthProbes covers all three paths and both methods, including the
// misspelling that is in the field and therefore in the contract.
func TestHealthProbes(t *testing.T) {
	s := newTestServer(t, nil)
	for _, p := range []string{"/health/liveliness", "/health/liveness", "/health/readiness", "/health"} {
		// No credential: probes are public, because a kubelet has none.
		w := do(s, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status %d", p, w.Code)
		}
		w = do(s, httptest.NewRequest(http.MethodOptions, p, nil))
		if w.Code != http.StatusNoContent {
			t.Errorf("OPTIONS %s: status %d, want 204", p, w.Code)
		}
	}
}

// TestMetricsEndpoint checks the scrape parses as exposition format and carries
// the counters a deployment actually alerts on.
func TestMetricsEndpoint(t *testing.T) {
	s := newTestServer(t, nil)
	do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	w := do(s, getAdmin("/metrics"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"dorang_requests_total", "dorang_request_duration_seconds_bucket",
		"dorang_inflight_requests", "dorang_ready 1", "dorang_replay_bytes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

// TestBodyCapIs413 keeps max_body_bytes a hard limit rather than a suggestion,
// and names the limit so a client can act on it.
func TestBodyCapIs413(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.MaxBodyBytes = 64 })
	w := do(s, post("/v1/chat/completions", `{"model":"model-x","padding":"`+strings.Repeat("x", 200)+`"}`))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"code":"request_too_large"`) || !strings.Contains(body, "64 bytes") {
		t.Errorf("body %s", body)
	}
}

// TestReplayBudgetMarksNonReplayable is DESIGN §15.4: past the process-wide
// budget a request is marked non-replayable rather than retained anyway. It
// still succeeds — losing a fallback is a smaller harm than losing the request.
func TestReplayBudgetMarksNonReplayable(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.ReplayBudgetBytes = 8 })
	r := post("/v1/chat/completions", `{"model":"model-x"}`)
	r.Header.Set(HeaderDetail, "full")
	w := do(s, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want the request to succeed anyway", w.Code)
	}
	if got := w.Header().Get(HeaderReplayable); got != "false" {
		t.Errorf("replayable header %q, want false", got)
	}

	// With room, the body is retained and the header is absent.
	s = newTestServer(t, func(o *Options) { o.ReplayBudgetBytes = 1 << 20 })
	r = post("/v1/chat/completions", `{"model":"model-x"}`)
	r.Header.Set(HeaderDetail, "full")
	w = do(s, r)
	if got := w.Header().Get(HeaderReplayable); got != "" {
		t.Errorf("replayable header %q on a retained body", got)
	}
	if s.ReplayBytesUsed() != 0 {
		t.Errorf("replay budget leaked %d bytes after the request finished", s.ReplayBytesUsed())
	}
}

// TestMeterPanicNeverFailsTheRequest: metering failure is never the request's
// failure (DESIGN §10.6 step 5, generalized).
func TestMeterPanicNeverFailsTheRequest(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Meter = panicMeter{} })
	w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	if w.Body.String() != `{"ok":true}` {
		t.Errorf("body %s", w.Body.String())
	}
}

// TestMeterEvent checks what the meter is handed.
func TestMeterEvent(t *testing.T) {
	m := &recordingMeter{}
	s := newTestServer(t, func(o *Options) { o.Meter = m })
	do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	ev := m.last(t)
	if ev.Route != "chat_completions" || ev.Family != FamilyOpenAIChat {
		t.Errorf("route %q family %v", ev.Route, ev.Family)
	}
	if ev.Model != "model-x" || ev.KeyID != "key-good" || ev.Status != 200 {
		t.Errorf("event %+v", ev)
	}
	if ev.BytesOut != int64(len(`{"ok":true}`)) {
		t.Errorf("bytes out %d", ev.BytesOut)
	}
	if ev.Result.CostNanoUSD != 123456 {
		t.Errorf("cost %d", ev.Result.CostNanoUSD)
	}
}

// TestHandlerPanicBecomes500 and does not take the process with it.
func TestHandlerPanicBecomes500(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = DispatchFunc(func(context.Context, *Request, http.ResponseWriter) error {
			panic("dispatcher exploded")
		})
	})
	w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"type":"api_error"`) {
		t.Errorf("body %s", w.Body.String())
	}
}

// TestLateErrorGoesInBand is COMPATIBILITY §1.3 through the handler: once the
// first frame is out the status is 200 and the only channel left is the body.
func TestLateErrorGoesInBand(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"x\":1}\n\n")
			return NewError(http.StatusBadGateway, TypeAPIError, "upstream died")
		})
	})
	w := do(s, post("/v1/chat/completions", `{"model":"model-x","stream":true}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 — the status was already sent", w.Code)
	}
	want := "data: {\"x\":1}\n\n" +
		"data: {\"error\":{\"message\":\"upstream died\",\"type\":\"api_error\",\"param\":null,\"code\":\"502\"}}\n\n" +
		"data: [DONE]\n\n"
	if got := w.Body.String(); got != want {
		t.Errorf("body\n got %q\nwant %q", got, want)
	}
}

// TestNoDispatcherIs501 rather than a nil dereference or a 404.
func TestNoDispatcherIs501(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Dispatcher = nil })
	w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", w.Code)
	}
	if !strings.Contains(w.Body.String(), "dispatcher_not_configured") {
		t.Errorf("body %s", w.Body.String())
	}
}

// TestMissingModelIsRefused before anything downstream has to guess.
func TestMissingModelIsRefused(t *testing.T) {
	s := newTestServer(t, nil)
	w := do(s, post("/v1/chat/completions", `{"messages":[]}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"code":"missing_model"`) {
		t.Errorf("body %s", w.Body.String())
	}
}

// TestPeekRequest exercises the scanner that reads model and stream without
// unmarshalling (DESIGN §15.2.2).
func TestPeekRequest(t *testing.T) {
	cases := []struct {
		body   string
		model  string
		stream bool
		ok     bool
	}{
		{`{"model":"model-x"}`, "model-x", false, true},
		{`{"model":"model-x","stream":true}`, "model-x", true, true},
		{`{"stream":false,"model":"a:b:c"}`, "a:b:c", false, true},
		{`{"messages":[{"role":"user","content":"{\"model\":\"decoy\"}"}],"model":"real"}`, "real", false, true},
		{`{"tools":[{"function":{"name":"x"}}],"model":"real","stream":true}`, "real", true, true},
		// An escaped value: the model name is opaque (DESIGN §2.1) and half
		// of one is a different model, so the escape is decoded rather than
		// sliced.
		{`{"model":"a\u003ab"}`, "a:b", false, true},
		// An escaped *key* spells the same key to a conforming parser. The
		// fast path does not decode keys, so this is the fallback case.
		{`{"mod\u0065l":"slow-path"}`, "slow-path", false, true},
		{`{}`, "", false, true},
		{`[]`, "", false, false},
		{`not json`, "", false, false},
		{`{"stream":"true","model":"m"}`, "m", false, true}, // truthy is not true
	}
	for _, c := range cases {
		model, stream, ok := peekRequest([]byte(c.body))
		if model != c.model || stream != c.stream || ok != c.ok {
			t.Errorf("peekRequest(%s) = (%q,%v,%v), want (%q,%v,%v)",
				c.body, model, stream, ok, c.model, c.stream, c.ok)
		}
	}
}

// TestConcurrentReloadDoesNotBreakRequests: configuration swaps by pointer, so
// a reload in flight is not a request in flight.
func TestConcurrentReloadDoesNotBreakRequests(t *testing.T) {
	s := newTestServer(t, nil)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.Reload(Options{
				Auth:       newFakeAuth("good"),
				Dispatcher: jsonDispatcher(`{"ok":true}`),
				Models:     ModelSlice{{ID: "model-x"}},
			})
		}
	}()
	for i := 0; i < 300; i++ {
		w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
		if w.Code != http.StatusOK {
			t.Fatalf("iteration %d: status %d", i, w.Code)
		}
	}
	close(stop)
	<-done
}

// TestCustomRouteWithParam exercises the Azure-style deployment path end to end
// through the server, not just the table: the specific route wins over the
// passthrough catch-all, and the captured parameter reaches the handler.
func TestCustomRouteWithParam(t *testing.T) {
	var seen string
	s := newTestServer(t, func(o *Options) {
		o.Passthrough = []PassthroughRoute{
			{Prefix: "/openai", Provider: "p", BaseURL: "http://127.0.0.1:1"},
		}
		o.Routes = []Route{{
			Pattern:   "/openai/deployments/{model}/chat/completions",
			Methods:   MethodPOST,
			Name:      "azure_chat",
			Family:    FamilyOpenAIChat,
			NeedsBody: true,
			Handler: func(w http.ResponseWriter, rq *Request) error {
				seen = rq.Param("model")
				w.Header().Set("Content-Type", "application/json")
				_, err := io.WriteString(w, `{"routed":true}`)
				return err
			},
		}}
	})

	w := do(s, post("/openai/deployments/vendor:model-5.1/chat/completions",
		`{"messages":[]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if seen != "vendor:model-5.1" {
		t.Errorf("captured model %q — a colon-bearing name must survive intact", seen)
	}
	if w.Body.String() != `{"routed":true}` {
		t.Errorf("the passthrough catch-all swallowed the specific route: %s", w.Body.String())
	}

	// And the catch-all still serves everything else under the same prefix,
	// which is what makes swallowing so easy to miss.
	w = do(s, post("/openai/v1/something", `{}`))
	if w.Code == http.StatusNotImplemented {
		t.Error("the passthrough prefix stopped serving its own paths")
	}
}

// TestDispatcherNormalizedUpstreamError is the error contract wired through the
// whole request: the dispatcher normalizes what the backend said, the server
// writes the four-key envelope, the native type goes out of band, and the shape
// reaches the meter so a deployment can be told which of the five envelopes its
// backend produces.
func TestDispatcherNormalizedUpstreamError(t *testing.T) {
	m := &recordingMeter{}
	upstream := []byte(`{"object":"error","message":"context length exceeded",` +
		`"type":"BadRequestError","param":null,"code":400}`)
	s := newTestServer(t, func(o *Options) {
		o.Meter = m
		o.Dispatcher = DispatchFunc(func(context.Context, *Request, http.ResponseWriter) error {
			return Normalize(http.StatusBadRequest, upstream)
		})
	})

	w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
	want := `{"error":{"message":"context length exceeded","type":"invalid_request_error","param":null,"code":"400"}}`
	if got := w.Body.String(); got != want {
		t.Errorf("body\n got %s\nwant %s", got, want)
	}
	if got := w.Header().Get(HeaderNativeErrorType); got != "BadRequestError" {
		t.Errorf("native error type %q", got)
	}
	if w.Header().Get(HeaderRequestID) == "" {
		t.Error("an error response has no request id to look the ledger up by")
	}
	if ev := m.last(t); ev.ErrorShape != ShapeFlat {
		t.Errorf("meter recorded shape %v, want flat", ev.ErrorShape)
	}
}

// TestRetryAfterOnRateLimit: Retry-After is a standard header a client acts on,
// so it is attached whether or not the caller asked for the detail set.
// COMPATIBILITY §7.2 also fixes the status — "no healthy deployment" is 429,
// not 503, because clients back off on a 429 and treat a 503 as a dead gateway.
func TestRetryAfterOnRateLimit(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = DispatchFunc(func(context.Context, *Request, http.ResponseWriter) error {
			e := NewError(http.StatusTooManyRequests, TypeRateLimit,
				"no healthy deployment for this model")
			e.RetryAfterSeconds = 5
			return e.WithCode("no_healthy_deployment")
		})
	})
	w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", w.Code)
	}
	if got := w.Header().Get(HeaderRetryAfter); got != "5" {
		t.Errorf("Retry-After %q, want 5", got)
	}
}

// TestRateLimitHeadersFollowTheDispatcher checks the standard-form set, which
// is attached without the detail opt-in for the same reason Retry-After is.
func TestRateLimitHeadersFollowTheDispatcher(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			rq.Result.RateLimit = RateLimit{
				Set: true, LimitRequests: 100, RemainingRequests: 97,
				ResetRequests: "60s", LimitTokens: 100000, RemainingTokens: 99000,
			}
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, `{}`)
			return err
		})
	})
	w := do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	h := w.Header()
	if h.Get(HeaderRateLimitLimitRequests) != "100" ||
		h.Get(HeaderRateLimitRemainingRequests) != "97" ||
		h.Get(HeaderRateLimitResetRequests) != "60s" ||
		h.Get(HeaderRateLimitLimitTokens) != "100000" {
		t.Errorf("rate limit headers: %v", h)
	}
}

// TestWantsUsageEvents is the accessor a dispatcher uses to decide whether to
// place the frame itself, before its protocol's terminator.
func TestWantsUsageEvents(t *testing.T) {
	var opted, notOpted bool
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			if rq.WantsUsageEvents() {
				opted = true
			} else {
				notOpted = true
			}
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, `{}`)
			return err
		})
	})
	do(s, post("/v1/chat/completions", `{"model":"model-x"}`))
	r := post("/v1/chat/completions", `{"model":"model-x"}`)
	r.Header.Set(HeaderUsageEvents, "1")
	do(s, r)
	if !opted || !notOpted {
		t.Errorf("opted=%v notOpted=%v", opted, notOpted)
	}
}

// TestDispatcherPlacesUsageEventItself: a chat-completions stream terminates
// with data: [DONE] and clients stop reading there, so a dispatcher that must
// put the frame before it calls EmitUsageEvent at that point. The server must
// then not append a second copy.
func TestDispatcherPlacesUsageEventItself(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
			rq.Result.Tokens = Usage{Input: 1, Output: 2}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"x\":1}\n\n")
			rq.EmitUsageEvent(w)
			_, err := io.WriteString(w, "data: [DONE]\n\n")
			return err
		})
	})
	r := post("/v1/chat/completions", `{"model":"model-x","stream":true}`)
	r.Header.Set(HeaderUsageEvents, "1")
	w := do(s, r)

	body := w.Body.String()
	if n := strings.Count(body, "event: dorang.usage"); n != 1 {
		t.Fatalf("%d usage frames, want exactly 1:\n%s", n, body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("the usage frame was placed after the terminator:\n%s", body)
	}
}

// TestNoHeaderInjectionFromPath: the refused path is echoed into a response
// header, and a path is entirely client-controlled. net/http would drop an
// invalid header value on write, but the check belongs here — the envelope's
// param carries the same string and is JSON-escaped, so the two channels are
// each safe on their own terms.
func TestNoHeaderInjectionFromPath(t *testing.T) {
	s := newTestServer(t, nil)
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	r.URL.Path = "/evil\r\nX-Injected: yes"
	r.Header.Set(HeaderAuthorization, "Bearer good")
	w := do(s, r)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status %d", w.Code)
	}
	if w.Header().Get("X-Injected") != "" {
		t.Fatal("a response header was injected from the request path")
	}
	if got := w.Header().Get(HeaderUnimplemented); got != "" {
		t.Errorf("an unsafe path was echoed into a header: %q", got)
	}
	// The path still reaches the client, in the one place where escaping makes
	// it safe.
	if !strings.Contains(w.Body.String(), `"param":"/evil\r\nX-Injected: yes"`) {
		t.Errorf("body %s", w.Body.String())
	}
}
