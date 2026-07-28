package server

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The T1 protocol surface's routing half (COMPATIBILITY §0, DESIGN §2.1).
//
// internal/server owns which paths exist, which model a request is authorized
// against, and what an unimplemented path answers. What the request MEANS is
// the dispatcher's, so these tests use a recording dispatcher and assert on the
// gate's decisions rather than on any protocol.

// recordDispatcher captures the request the gate handed over.
func recordDispatcher(got *Request) Dispatcher {
	return DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
		*got = *rq
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"ok":true}`))
		return err
	})
}

// TestT1RoutesAreServed walks the whole declared T1 surface and asserts that
// every one of them reaches the dispatcher rather than the 501 handler.
//
// It is a list rather than a loop over the table so that DELETING a route is a
// test failure: a route table can only be asserted against something written
// down independently of it.
func TestT1RoutesAreServed(t *testing.T) {
	var seen Request
	s := newTestServer(t, func(o *Options) { o.Dispatcher = recordDispatcher(&seen) })

	json := []string{
		"/v1/completions", "/completions",
		"/v1/rerank", "/rerank", "/v2/rerank",
		"/v1/moderations", "/moderations",
		"/v1/audio/speech",
		"/v1/images/generations",
		"/v1/responses",
		"/engines/model-x/chat/completions",
		"/engines/model-x/completions",
		"/engines/model-x/embeddings",
		"/openai/deployments/model-x/chat/completions",
		"/openai/deployments/model-x/completions",
		"/openai/deployments/model-x/embeddings",
		"/openai/deployments/model-x/audio/speech",
		"/openai/deployments/model-x/images/generations",
		"/openai/deployments/model-x/responses",
	}
	for _, path := range json {
		t.Run(path, func(t *testing.T) {
			w := do(s, post(path, `{"model":"model-x"}`))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
			if seen.Model != "model-x" {
				t.Errorf("model %q reached the dispatcher", seen.Model)
			}
		})
	}

	form := []string{
		"/v1/audio/transcriptions", "/v1/audio/translations",
		"/v1/images/edits", "/v1/images/variations",
		"/openai/deployments/model-x/audio/transcriptions",
		"/openai/deployments/model-x/audio/translations",
		"/openai/deployments/model-x/images/edits",
	}
	for _, path := range form {
		t.Run(path, func(t *testing.T) {
			w := do(s, multipartPost(path, "model-x", "file", "a.wav", []byte("RIFF")))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestDeploymentPathIsNotSwallowedByThePassthroughPrefix is COMPATIBILITY §7.5.
//
// A naive prefix router silently swallows /openai/deployments/{model}/… into
// an /openai/ catch-all, and the symptom is an Azure-shaped client's traffic
// quietly reaching a vendor passthrough instead of the model it named. The
// table is specificity-ordered so registration order cannot change the answer,
// which is why this asserts both halves: the specific route wins AND the
// catch-all still serves everything else under the same prefix.
func TestDeploymentPathIsNotSwallowedByThePassthroughPrefix(t *testing.T) {
	var seen Request
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = recordDispatcher(&seen)
		o.Passthrough = []PassthroughRoute{
			{Prefix: "/openai", Provider: "p", BaseURL: "http://127.0.0.1:1"},
		}
	})

	w := do(s, post("/openai/deployments/vendor:model-5.1/chat/completions", `{"messages":[]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"ok":true}` {
		t.Fatalf("the passthrough catch-all swallowed the specific route: %s", w.Body.String())
	}
	// The model name is opaque and carries a ':' — nothing splits it (§2.1).
	if seen.Model != "vendor:model-5.1" {
		t.Errorf("model %q, want vendor:model-5.1", seen.Model)
	}
	if seen.Route.Name != "chat_completions" {
		t.Errorf("route %q", seen.Route.Name)
	}

	// And the prefix still serves its own paths, which is what makes the
	// swallowing so easy to miss.
	if w := do(s, post("/openai/v1/something", `{}`)); w.Code == http.StatusNotImplemented {
		t.Error("the passthrough prefix stopped serving its own paths")
	}
}

// TestDeploymentPathModelIsAuthorized is the W10 half of the same feature.
//
// The path names the model, so the ALLOW-LIST has to be checked against the
// path's name. Resolving it in the handler instead would authorize the request
// as having no model and dispatch it as having one — the same bypass shape
// COMPATIBILITY 2.0 closes for a differently-cased JSON key.
func TestDeploymentPathModelIsAuthorized(t *testing.T) {
	auth := newFakeAuth("good")
	auth.keys["good"].models = []string{"allowed"}
	var seen Request
	s := newTestServer(t, func(o *Options) {
		o.Auth = auth
		o.Dispatcher = recordDispatcher(&seen)
	})

	// The body names an allowed model and the PATH names a disallowed one. The
	// path wins, so the request must be refused.
	w := do(s, post("/openai/deployments/expensive/chat/completions", `{"model":"allowed"}`))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d body %s — the path model must be the one authorized",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"model_not_allowed"`) {
		t.Errorf("body %s", w.Body.String())
	}

	if w := do(s, post("/openai/deployments/allowed/chat/completions", `{}`)); w.Code != http.StatusOK {
		t.Fatalf("an allowed deployment was refused: %d %s", w.Code, w.Body.String())
	}
	if seen.Model != "allowed" {
		t.Errorf("model %q reached the dispatcher", seen.Model)
	}
}

// TestModelRetrieveIsFilteredByTheAllowList is COMPATIBILITY §7.4 applied to
// the single-model endpoint.
//
// A model the calling key cannot see must be indistinguishable from one that
// does not exist. A 403 here would confirm the model's existence to a key that
// the list endpoint deliberately hid it from, which makes the pair an
// enumeration oracle over another key's catalogue.
func TestModelRetrieveIsFilteredByTheAllowList(t *testing.T) {
	auth := newFakeAuth("good")
	auth.keys["good"].models = []string{"model-x"}
	s := newTestServer(t, func(o *Options) {
		o.Auth = auth
		o.Models = ModelSlice{{ID: "model-x"}, {ID: "vendor:model-5.1", OwnedBy: "vendor"}}
	})

	w := do(s, get("/v1/models/model-x"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	want := `{"id":"model-x","object":"model","created":1677610602,"owned_by":"dorang"}`
	if got := w.Body.String(); got != want {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}

	// Present in the catalogue, absent from this key's allow-list...
	hidden := do(s, get("/v1/models/vendor:model-5.1"))
	// ...and absent from the catalogue entirely.
	missing := do(s, get("/v1/models/never-existed"))
	for name, w := range map[string]*httptest.ResponseRecorder{"hidden": hidden, "missing": missing} {
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, w.Code)
		}
		if !strings.Contains(w.Body.String(), `"code":"model_not_found"`) {
			t.Errorf("%s: body %s", name, w.Body.String())
		}
	}
	if hidden.Body.String() != missing.Body.String() {
		t.Errorf("a hidden model is distinguishable from a missing one:\n %s\n %s",
			hidden.Body.String(), missing.Body.String())
	}

	// And the list endpoint agrees, which is the invariant that makes the
	// filter meaningful rather than cosmetic.
	list := do(s, get("/v1/models"))
	if strings.Contains(list.Body.String(), "vendor:model-5.1") {
		t.Errorf("the list leaked a model the retrieve endpoint hides: %s", list.Body.String())
	}
}

// TestModelRetrieveWithNoCatalog answers 501 with a code rather than 404.
func TestModelRetrieveWithNoCatalog(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Models = nil })
	w := do(s, get("/v1/models/model-x"))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"code":"catalog_not_configured"`) {
		t.Errorf("body %s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Multipart
// ---------------------------------------------------------------------------

// TestMultipartModelIsResolvedInTheGate is the multipart form of W10.
//
// The model on these routes is a form FIELD, and the allow-list check runs
// before the handler. A handler that parsed the form itself would leave the
// gate authorizing a request with no model at all.
func TestMultipartModelIsResolvedInTheGate(t *testing.T) {
	auth := newFakeAuth("good")
	auth.keys["good"].models = []string{"allowed"}
	var seen Request
	s := newTestServer(t, func(o *Options) {
		o.Auth = auth
		o.Dispatcher = recordDispatcher(&seen)
	})

	w := do(s, multipartPost("/v1/audio/transcriptions", "expensive", "file", "a.wav", []byte("RIFF")))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d body %s — the form's model must be the one authorized",
			w.Code, w.Body.String())
	}

	w = do(s, multipartPost("/v1/audio/transcriptions", "allowed", "file", "a.wav", []byte("RIFF")))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if seen.Form == nil {
		t.Fatal("the parsed form did not reach the handler")
	}
	if seen.Model != "allowed" {
		t.Errorf("model %q", seen.Model)
	}
	f := seen.Form.File("file")
	if f == nil || string(f.Data) != "RIFF" || f.Name != "a.wav" {
		t.Errorf("file part %+v", f)
	}
}

// TestMultipartFieldLookupIsCaseSensitive: an exact map key, matching the
// strict-JSON rule the body paths get (COMPATIBILITY 2.0).
func TestMultipartFieldLookupIsCaseSensitive(t *testing.T) {
	var seen Request
	s := newTestServer(t, func(o *Options) { o.Dispatcher = recordDispatcher(&seen) })

	body, ct := buildMultipart(map[string]string{"Model": "expensive"}, "file", "a.wav", []byte("x"))
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	r.Header.Set("Content-Type", ct)
	do(s, r)

	if seen.Model != "" {
		t.Errorf("model %q resolved from a differently-cased form field", seen.Model)
	}
}

// TestMultipartFileOverTheCapIs413 is the size rule, and it is the request's
// cap rather than a second one invented for files: max_body_bytes bounds the
// whole body, so an oversized upload is refused before anything is parsed.
func TestMultipartFileOverTheCapIs413(t *testing.T) {
	const cap = 4 << 10
	s := newTestServer(t, func(o *Options) { o.MaxBodyBytes = cap })

	body, ct := buildMultipart(map[string]string{"model": "model-x"},
		"file", "big.wav", bytes.Repeat([]byte("A"), cap*2))
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	r.Header.Set("Content-Type", ct)
	w := do(s, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"request_too_large"`) {
		t.Errorf("body %s", w.Body.String())
	}
	// COMPATIBILITY §11.2 fixes the type for this row.
	if !strings.Contains(w.Body.String(), `"type":"request_too_large"`) {
		t.Errorf("body %s", w.Body.String())
	}
}

// TestMultipartRejectsANonMultipartBody: a JSON body on a form route is a
// client error with a code, not a panic and not a silent empty form.
func TestMultipartRejectsANonMultipartBody(t *testing.T) {
	s := newTestServer(t, nil)
	w := do(s, post("/v1/audio/transcriptions", `{"model":"model-x"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"invalid_body"`) {
		t.Errorf("body %s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// The 501 contract
// ---------------------------------------------------------------------------

// TestUnservedT1PathsCarryACode is DESIGN §0.2 for the parts of the declared
// surface this build does not serve. Never a silent 404, always a code.
func TestUnservedT1PathsCarryACode(t *testing.T) {
	s := newTestServer(t, nil)
	for _, path := range []string{
		"/v1/responses/resp_1",
		"/v1/responses/resp_1/cancel",
		"/v1/responses/resp_1/input_items",
		"/v1/ocr",
	} {
		w := do(s, post(path, `{}`))
		if w.Code != http.StatusNotImplemented {
			t.Errorf("%s: status %d, want 501", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), `"code":"route_not_implemented"`) {
			t.Errorf("%s: body %s", path, w.Body.String())
		}
		if got := w.Header().Get(HeaderUnimplemented); got != path {
			t.Errorf("%s: unimplemented header %q", path, got)
		}
	}
}

// TestAppSuppliedRouteOverridesTheBuiltIn: internal/app mounts the stateful
// half of the Responses API and the batch surface, and its handler has to be
// the one that runs. Registering behind a built-in declaration would make it
// unreachable — the same defect §7.5 describes, one layer up.
func TestAppSuppliedRouteOverridesTheBuiltIn(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Routes = []Route{{
			Pattern: "/v1/responses", Methods: MethodPOST,
			Name: "responses_override", NeedsBody: true,
			ModelAuth: ModelAuthGate,
			Handler: func(w http.ResponseWriter, rq *Request) error {
				_, err := w.Write([]byte(`{"override":true}`))
				return err
			},
		}}
	})
	w := do(s, post("/v1/responses", `{"model":"model-x"}`))
	if w.Body.String() != `{"override":true}` {
		t.Fatalf("the built-in route shadowed the app's: %s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func buildMultipart(fields map[string]string, fileField, filename string, data []byte) ([]byte, string) {
	buf := new(bytes.Buffer)
	mw := multipart.NewWriter(buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if fileField != "" {
		w, _ := mw.CreateFormFile(fileField, filename)
		_, _ = w.Write(data)
	}
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func multipartPost(path, model, fileField, filename string, data []byte) *http.Request {
	body, ct := buildMultipart(map[string]string{"model": model}, fileField, filename, data)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	r.Header.Set("Content-Type", ct)
	return r
}
