package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A passthrough prefix does not exempt a key from its model allow-list.
//
// The route declares NeedsBody false — it relays without parsing — so the gate
// scanned no model, compared "" against the list, and skipped the check
// entirely. A key restricted to gpt-4o-mini could POST /anthropic/v1/messages
// with {"model":"claude-opus-4"} and spend the operator's provider credential
// on a model it was explicitly denied. Authentication passed, the route
// allow-list passed, and the model allow-list was never consulted.
func TestPassthroughEnforcesTheModelAllowList(t *testing.T) {
	reached := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	s := newTestServer(t, func(o *Options) {
		o.Auth = restrictedAuth("gpt-4o-mini")
		o.Passthrough = []PassthroughRoute{
			{Prefix: "/anthropic", Provider: "anthropic-main", BaseURL: up.URL},
		}
	})

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4","messages":[]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(HeaderAuthorization, "Bearer good")
	w := do(s, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401: a passthrough prefix bypassed the model allow-list", w.Code)
	}
	if reached {
		t.Error("the request reached the provider: the operator's credential was spent on " +
			"a model this key may not use")
	}
}

// A model the key IS allowed to use relays normally, and the peeked bytes are
// forwarded rather than consumed.
func TestPassthroughRelaysAnAllowedModelWithTheBodyIntact(t *testing.T) {
	const body = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`
	var got string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, len(body)+16)
		n, _ := r.Body.Read(b)
		got = string(b[:n])
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	s := newTestServer(t, func(o *Options) {
		o.Auth = restrictedAuth("gpt-4o-mini")
		o.Passthrough = []PassthroughRoute{
			{Prefix: "/anthropic", Provider: "p", BaseURL: up.URL},
		}
	})

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(HeaderAuthorization, "Bearer good")
	w := do(s, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body.String())
	}
	if got != body {
		t.Errorf("the relayed body was altered by the peek\n got %q\nwant %q", got, body)
	}
}

// A restricted key cannot escape the allow-list by making the model
// undeterminable. An unrestricted key is unaffected.
func TestPassthroughRefusesWhenTheModelCannotBeDetermined(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	cases := []struct {
		name        string
		models      []string
		contentType string
		body        string
		want        int
	}{
		{
			name: "restricted key, body dorang cannot parse", models: []string{"m1"},
			contentType: "application/octet-stream", body: "\x00\x01binary",
			want: http.StatusUnauthorized,
		},
		{
			name: "restricted key, json that is not an object", models: []string{"m1"},
			contentType: "application/json", body: `["not","an","object"]`,
			want: http.StatusUnauthorized,
		},
		{
			name: "unrestricted key, same body", models: nil,
			contentType: "application/octet-stream", body: "\x00\x01binary",
			want: http.StatusOK,
		},
		{
			name: "restricted key, json object naming no model", models: []string{"m1"},
			contentType: "application/json", body: `{"input":"embed me"}`,
			want: http.StatusOK,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t, func(o *Options) {
				o.Auth = restrictedAuth(c.models...)
				o.Passthrough = []PassthroughRoute{
					{Prefix: "/anthropic", Provider: "p", BaseURL: up.URL},
				}
			})
			r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/x", strings.NewReader(c.body))
			r.Header.Set("Content-Type", c.contentType)
			r.Header.Set(HeaderAuthorization, "Bearer good")
			if w := do(s, r); w.Code != c.want {
				t.Errorf("status %d, want %d: %s", w.Code, c.want, w.Body.String())
			}
		})
	}
}

// An upstream cannot set cookies on dorang's origin.
//
// copyResponseHeaders relayed every header, then stripped hop-by-hop and
// authentication names. Set-Cookie is in neither set, so a compromised backend
// behind a passthrough prefix could clear or overwrite the operator UI's
// session cookie — and set anything else on the gateway's own origin.
func TestPassthroughDoesNotRelaySetCookie(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "dorang_admin_ui=evil; Path=/")
		w.Header().Add("Set-Cookie2", "legacy=evil")
		w.Header().Set("X-Harmless", "kept")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	s := newTestServer(t, func(o *Options) {
		o.Passthrough = []PassthroughRoute{
			{Prefix: "/anthropic", Provider: "p", BaseURL: up.URL},
		}
	})

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader("{}"))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	w := do(s, r)

	if got := w.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("upstream Set-Cookie was relayed onto dorang's origin: %v", got)
	}
	if got := w.Header().Values("Set-Cookie2"); len(got) != 0 {
		t.Errorf("upstream Set-Cookie2 was relayed: %v", got)
	}
	if w.Header().Get("X-Harmless") != "kept" {
		t.Error("an unrelated upstream header was dropped")
	}
}
