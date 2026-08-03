package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPassthroughCounterIsIncrementedAndExported.
//
// `dorang_passthrough_requests_total` was documented as having no increment
// site. It has had one since the file was written — and nothing asserted it, so
// the counter and the claim about it drifted apart with no test between them to
// notice. This is that test: one relayed request, one increment, and the number
// visible on the endpoint that serves it.
//
// It is the cheap version of the general problem. A counter nobody asserts is
// indistinguishable from a counter nobody increments, and the second kind is
// exactly what this sweep was chasing.
func TestPassthroughCounterIsIncrementedAndExported(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	s := newTestServer(t, func(o *Options) {
		o.MetricsAccess = MetricsPublic
		o.Passthrough = []PassthroughRoute{{
			Prefix:   "/vendor",
			Provider: "p1",
			BaseURL:  upstream.URL,
			Auth:     "none",
		}}
	})

	before := s.Stats().Passthrough
	w := do(s, httptest.NewRequest(http.MethodGet, "/vendor/v1/anything", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("relayed request answered %d: %s", w.Code, w.Body.String())
	}
	if got := s.Stats().Passthrough; got != before+1 {
		t.Fatalf("Stats().Passthrough = %d, want %d", got, before+1)
	}

	// And it reaches the wire. The built-in block and internal/metrics render
	// from two different structs, so "the counter moved" and "the scrape says
	// so" are separate claims.
	scrape := do(s, httptest.NewRequest(http.MethodGet, "/metrics", nil)).Body.String()
	if !strings.Contains(scrape, "dorang_passthrough_requests_total 1") {
		t.Errorf("the increment did not reach the scrape:\n%s",
			linesContaining(scrape, "passthrough"))
	}
}

func linesContaining(body, want string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, want) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// TestMetricsAccessRules covers the three states of the scrape endpoint from the
// HTTP surface's own side: administrative by default, public when a deployment
// says so, and absent when prometheus is off.
func TestMetricsAccessRules(t *testing.T) {
	t.Run("admin by default", func(t *testing.T) {
		s := newTestServer(t, nil)
		if w := do(s, httptest.NewRequest(http.MethodGet, "/metrics", nil)); w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous scrape = %d, want 401", w.Code)
		}
		if w := do(s, get("/metrics")); w.Code != http.StatusForbidden {
			t.Errorf("ordinary key = %d, want 403", w.Code)
		}
		if w := do(s, getAdmin("/metrics")); w.Code != http.StatusOK {
			t.Errorf("admin key = %d, want 200", w.Code)
		}
	})
	t.Run("public", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) { o.MetricsAccess = MetricsPublic })
		if w := do(s, httptest.NewRequest(http.MethodGet, "/metrics", nil)); w.Code != http.StatusOK {
			t.Errorf("public scrape = %d, want 200", w.Code)
		}
	})
	t.Run("off", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) { o.MetricsAccess = MetricsOff })
		w := do(s, getAdmin("/metrics"))
		if w.Code != http.StatusNotImplemented {
			t.Errorf("disabled scrape = %d, want 501: an absent route is not a 200 with an "+
				"empty body", w.Code)
		}
	})
}

// TestHealthProbesStayPublic: the container probes must not be swept up by the
// scrape's new access rule. A liveness probe that needs a credential is a pod
// that never becomes ready.
func TestHealthProbesStayPublic(t *testing.T) {
	s := newTestServer(t, nil)
	for _, p := range []string{"/health", "/health/liveliness", "/health/liveness", "/health/readiness"} {
		if w := do(s, httptest.NewRequest(http.MethodGet, p, nil)); w.Code != http.StatusOK {
			t.Errorf("%s answered %d without a credential", p, w.Code)
		}
	}
}

// TestHealthReporterContributesAnObject is the mechanism DESIGN §12.1's "never
// silent" needs: a subsystem losing data has to be able to say so where an
// operator already looks.
func TestHealthReporterContributesAnObject(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.HealthReporters = []HealthReporter{
			stubReporter{name: "metering", body: `{"degraded":true,"reason":"spool_full"}`},
			stubReporter{name: "silent", body: ""},
		}
	})
	w := do(s, httptest.NewRequest(http.MethodGet, "/health", nil))
	body := w.Body.String()
	if !strings.Contains(body, `"metering":{"degraded":true,"reason":"spool_full"}`) {
		t.Fatalf("the reporter's object is missing: %s", body)
	}
	// A reporter with nothing to say must not leave a dangling key behind: a
	// health body that fails to parse is worse than one that omits a subsystem.
	if strings.Contains(body, "silent") {
		t.Errorf("a silent reporter left its key in the body: %s", body)
	}
	if w.Code != http.StatusOK {
		t.Errorf("a degraded subsystem changed the status to %d", w.Code)
	}

	// Liveness stays minimal: it answers whether the process is up, and a probe
	// that starts failing because a diagnostic stopped is a worse probe.
	live := do(s, httptest.NewRequest(http.MethodGet, "/health/liveliness", nil)).Body.String()
	if strings.Contains(live, "metering") {
		t.Errorf("liveness carries subsystem detail: %s", live)
	}
}

type stubReporter struct{ name, body string }

func (r stubReporter) HealthName() string { return r.name }

func (r stubReporter) Health(dst []byte) []byte {
	if r.body == "" {
		return dst
	}
	return append(dst, r.body...)
}

// TestMeteredModelIsTheCallersOwnBytes is the other half of a claim that lives
// in OPERATIONS.md §6: that no metric carries a per-model label, "deliberately",
// because letting a client mint label values hands out a denial of service on
// the metrics registry.
//
// internal/metrics does carry one, and this is where its value comes from.
// [Event.Model] is [Request.Model], which is whatever the request body said —
// this package never checks it against the configured model set, and it must
// not, because the model that was ASKED for is the fact the ledger and the
// meter are recording. The consequence is the one the §6 argument describes:
// the label value is chosen by the caller.
//
// The refused case is the one worth pinning. `finish` is deferred from
// ServeHTTP, so a request the model allow-list turns away is still metered, and
// still metered with the name it asked for. A key permitted exactly one model
// can therefore mint unbounded label values by asking for models it may not
// have — which means the bound cannot come from authorization, and has to come
// from internal/metrics' series cap. That cap is now the documented one.
func TestMeteredModelIsTheCallersOwnBytes(t *testing.T) {
	cases := []struct {
		name       string
		allowed    []string // the key's model allow-list; nil allows everything
		model      string
		wantStatus int
	}{
		{"served", nil, "no-such-model-\"a\"", http.StatusOK},
		{"refused by the allow-list", []string{"model-x"}, "not-allowed-µ-42", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &recordingMeter{}
			s := newTestServer(t, func(o *Options) {
				o.Meter = m
				o.Auth = &fakeAuth{keys: map[string]*fakePrincipal{
					"good": {id: "key-good", models: c.allowed},
				}}
			})

			w := do(s, post("/v1/chat/completions", `{"model":`+quoteJSON(c.model)+`}`))
			if w.Code != c.wantStatus {
				t.Fatalf("status %d, want %d: %s", w.Code, c.wantStatus, w.Body.String())
			}

			ev := m.last(t)
			if ev.Model != c.model {
				t.Errorf("metered model %q, want the caller's own %q", ev.Model, c.model)
			}
		})
	}
}

// quoteJSON is enough of a JSON string encoder for the fixtures above.
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
