package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const systemOneYAML = `
version: 1
observability: {always_full_headers: true}
providers:
  - {name: ts, kind: typesafe, base_url: %[1]q}
  - {name: oc, kind: ollama-cloud, base_url: "%[1]s/v1"}
credentials:
  - {id: c1, provider: ts, key_env: DORANG_APP_TEST_KEY}
  - {id: c2, provider: oc, key_env: DORANG_APP_TEST_KEY}
models:
  - name: decide
    deployments:
      - {provider: ts, upstream_model: jev-latest, credentials: [c1]}
  - name: cloud-decide
    deployments:
      - {provider: oc, upstream_model: gpt-oss:20b, credentials: [c2]}
`

// fakeDecisionHost answers POST /v1/systemone the way TypeSafe documents it.
type fakeDecisionHost struct {
	mu    sync.Mutex
	paths []string
	model string
}

func (f *fakeDecisionHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.model = req.Model
	f.mu.Unlock()
	if r.URL.Path != "/v1/systemone" {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"model":"jev-1.13.0","answers":{"department":{"type":"choice","choice":"billing",
		"probabilities":{"billing":0.88,"technical":0.12},"confidence":0.81}},
		"usage":{"input_tokens":318,"output_tokens":34}}`)
}

// TestSystemOneThroughTheGateway is the whole path: a client speaking the
// TypeSafe contract to dorang's /v1/systemone reaches the deployment's host
// with the deployment's model, and gets the typed answers back under the name
// it asked for. A deployment on Ollama Cloud — which does not serve the route —
// is a named 501 that never leaves the gateway, and a malformed body is a 422
// that never reaches routing.
func TestSystemOneThroughTheGateway(t *testing.T) {
	up := &fakeDecisionHost{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	a := newWiringApp(t, fmt.Sprintf(systemOneYAML, srv.URL), nil,
		func(o *Options) { o.Upstream = srv.Client() })
	key := issueKey(t, a, nil)

	req := `{"model":"decide","state":"Help! My payouts have been failing for 3 days.",
		"questions":{"department":{"type":"choice","instructions":"Which team should handle this?",
		"criteria":{"billing":"Payments, invoicing, refunds","technical":"Bugs, outages, integrations"}}}}`
	w := callWith(a, key, http.MethodPost, "/v1/systemone", req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /v1/systemone = %d\n%s", w.Code, w.Body.String())
	}
	var got struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type       string             `json:"type"`
			Choice     string             `json:"choice"`
			Confidence float64            `json:"confidence"`
			Probs      map[string]float64 `json:"probabilities"`
		} `json:"answers"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v\n%s", err, w.Body.String())
	}
	if got.Model != "decide" {
		t.Errorf("model %q, want the client-facing name", got.Model)
	}
	if a := got.Answers["department"]; a.Type != "choice" || a.Choice != "billing" || a.Confidence != 0.81 || a.Probs["billing"] != 0.88 {
		t.Errorf("answer %+v did not survive the relay", a)
	}
	if got.Usage.InputTokens != 318 || got.Usage.OutputTokens != 34 {
		t.Errorf("usage %+v", got.Usage)
	}
	up.mu.Lock()
	paths, model := append([]string(nil), up.paths...), up.model
	up.mu.Unlock()
	if len(paths) != 1 || paths[0] != "POST /v1/systemone" || model != "jev-latest" {
		t.Errorf("upstream saw %v with model %q, want one POST /v1/systemone for jev-latest", paths, model)
	}

	// Ollama Cloud: refused by name, before any request.
	w = callWith(a, key, http.MethodPost, "/v1/systemone", strings.Replace(req, `"decide"`, `"cloud-decide"`, 1))
	if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "systemone_unsupported") {
		t.Errorf("Ollama Cloud deployment: %d %s, want a named 501", w.Code, w.Body.String())
	}

	// Malformed: a one-option Choice.
	w = callWith(a, key, http.MethodPost, "/v1/systemone", `{"model":"decide","state":"s",
		"questions":{"q":{"type":"choice","instructions":"i","criteria":{"only":"one"}}}}`)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "questions.q.criteria") {
		t.Errorf("malformed: %d %s, want a 422 naming the field", w.Code, w.Body.String())
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.paths) != 1 {
		t.Errorf("the refused requests reached the upstream: %v", up.paths)
	}
}
