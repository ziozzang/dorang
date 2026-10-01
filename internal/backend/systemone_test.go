package backend

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/wire/systemone"
	"github.com/ziozzang/dorang/pkg/catalog"
)

const s1Answer = `{"model":"jev-1.13.0","answers":{"department":{"type":"choice","choice":"billing",
	"probabilities":{"billing":0.88,"technical":0.12},"confidence":0.81}},
	"usage":{"input_tokens":318,"output_tokens":34}}`

func s1Call(t *testing.T, body string) *Call {
	t.Helper()
	req, err := systemone.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &Call{Op: OpSystemOne, ClientAPI: catalog.APISystemOne, Model: "decide",
		Body: []byte(body), SystemOne: req}
}

const s1Request = `{"model":"decide","state":"Help!","questions":{"department":{"type":"choice",
	"instructions":"Which team?","criteria":{"billing":"Payments","technical":"Bugs"}}}}`

// TestSystemOneRelaysToEachHost: TypeSafe is addressed on its bare host and a
// local Ollama on its /v1 base, both at /v1/systemone; the body goes up with
// only the model replaced and comes back with the client-facing name restored
// and the token usage read.
func TestSystemOneRelaysToEachHost(t *testing.T) {
	for _, c := range []struct {
		kind string
		api  catalog.API
		base string
	}{
		{"typesafe", catalog.APISystemOne, ""},
		{"systemone", catalog.APISystemOne, ""},
		{"ollama", catalog.APIOpenAIChat, "/v1"},
	} {
		f := newFakeUpstream(t)
		p, err := NewProvider(Spec{Name: c.kind, Kind: c.kind, API: c.api, BaseURL: f.srv.URL + c.base})
		if err != nil {
			t.Fatalf("%s: NewProvider: %v", c.kind, err)
		}
		f.answer(http.StatusOK, s1Answer)
		res := testBackend("s1-key").Do(context.Background(), target(p), s1Call(t, s1Request), nil)
		if res.Err != nil {
			t.Fatalf("%s: %v", c.kind, res.Err)
		}
		got := f.last()
		if got.path != "/v1/systemone" || got.method != http.MethodPost {
			t.Errorf("%s: %s %s, want POST /v1/systemone", c.kind, got.method, got.path)
		}
		if got.header.Get("Authorization") != "Bearer s1-key" {
			t.Errorf("%s: credential not sent as a bearer", c.kind)
		}
		sent := decodeJSON(t, got.body)
		if sent["model"] != "upstream-model" {
			t.Errorf("%s: model sent %v, want the deployment's upstream model", c.kind, sent["model"])
		}
		if _, ok := sent["questions"].(map[string]any)["department"]; !ok {
			t.Errorf("%s: the questions did not survive the relay: %s", c.kind, got.body)
		}
		out := decodeJSON(t, res.Body)
		if out["model"] != "decide" {
			t.Errorf("%s: answer model %v, want the client-facing name", c.kind, out["model"])
		}
		if _, ok := out["answers"].(map[string]any)["department"]; !ok {
			t.Errorf("%s: the answers did not survive: %s", c.kind, res.Body)
		}
		if res.Usage.InputTokens != 318 || res.Usage.OutputTokens != 34 {
			t.Errorf("%s: usage %+v, want 318 in / 34 out", c.kind, res.Usage)
		}
	}
}

// TestSystemOneRefusesHostsThatDoNotServeIt: Ollama Cloud answers 501 at this
// path and its documentation says System One is local-only; a chat host has no
// such route at all. Both are refused by name, without a request.
func TestSystemOneRefusesHostsThatDoNotServeIt(t *testing.T) {
	for _, c := range []struct {
		kind string
		api  catalog.API
		hint string
	}{
		{"ollama-cloud", catalog.APIOpenAIChat, "local server only"},
		{"openai", catalog.APIOpenAIChat, "does not serve System One"},
	} {
		f := newFakeUpstream(t)
		p, err := NewProvider(Spec{Name: c.kind, Kind: c.kind, API: c.api, BaseURL: f.srv.URL + "/v1"})
		if err != nil {
			t.Fatalf("%s: NewProvider: %v", c.kind, err)
		}
		res := testBackend("k").Do(context.Background(), target(p), s1Call(t, s1Request), nil)
		if res.Err == nil || res.Err.Status != http.StatusNotImplemented || res.Err.Code != "systemone_unsupported" {
			t.Fatalf("%s: want a named 501, got %+v", c.kind, res.Err)
		}
		if !strings.Contains(res.Err.Message, c.hint) {
			t.Errorf("%s: the refusal does not say why: %q", c.kind, res.Err.Message)
		}
		if f.count() != 0 {
			t.Errorf("%s: a refused operation reached the upstream", c.kind)
		}
	}
}

// TestSystemOneAppliesEachHostsLimits: the gateway admits what either contract
// takes, so the deployment a request lands on applies its own documented
// ceiling — 422, naming the field and the host — before anything is sent.
//
// Revert check: drop the checkSystemOneLimits call in encode and every case
// reaches the upstream.
func TestSystemOneAppliesEachHostsLimits(t *testing.T) {
	levels := func(n int) string {
		return `{"model":"m","state":"s","questions":{"q":{"type":"score","instructions":"i","criteria":[` +
			strings.TrimSuffix(strings.Repeat(`"l",`, n), ",") + `]}}}`
	}
	options := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`"o` + strings.Repeat("x", i) + `":"d"`)
		}
		return `{"model":"m","state":"s","questions":{"q":{"type":"choice","instructions":"i","criteria":{` + b.String() + `}}}}`
	}
	many := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`"q` + strings.Repeat("x", i) + `":{"type":"noul","instructions":"i"}`)
		}
		return `{"model":"m","state":"s","questions":{` + b.String() + `}}`
	}
	big := `{"model":"m","state":"` + strings.Repeat("a", 70<<10) + `","questions":{"q":{"type":"noul","instructions":"i"}}}`
	structured := `{"model":"m","state":"s","questions":{"q":{"type":"choice","instructions":"i","criteria":{"a":{"x":1},"b":"B"}}}}`

	for _, c := range []struct {
		name, kind, body, param, says string
	}{
		{"typesafe score of 11", "typesafe", levels(11), "questions.q.criteria", "TypeSafe"},
		{"ollama choice of 27", "ollama", options(27), "questions.q.criteria", "Ollama"},
		{"ollama score of 27 is the gateway's", "ollama", levels(26), "", ""}, // 26 passes both
		{"ollama 65 questions", "ollama", many(65), "questions", "Ollama"},
		{"ollama structured criteria", "ollama", structured, "questions", "strings only"},
		{"ollama body over 64 KiB", "ollama", big, "body", "64 KiB"},
	} {
		f := newFakeUpstream(t)
		api := catalog.APISystemOne
		base := f.srv.URL
		if c.kind == "ollama" {
			api, base = catalog.APIOpenAIChat, f.srv.URL+"/v1"
		}
		p, err := NewProvider(Spec{Name: c.kind, Kind: c.kind, API: api, BaseURL: base})
		if err != nil {
			t.Fatal(err)
		}
		f.answer(http.StatusOK, s1Answer)
		res := testBackend("k").Do(context.Background(), target(p), s1Call(t, c.body), nil)
		if c.param == "" {
			if res.Err != nil {
				t.Errorf("%s: refused: %+v", c.name, res.Err)
			}
			continue
		}
		if res.Err == nil || res.Err.Status != http.StatusUnprocessableEntity || res.Err.Code != "systemone_limit" {
			t.Errorf("%s: want a 422 systemone_limit, got %+v", c.name, res.Err)
			continue
		}
		if res.Err.Param == nil || *res.Err.Param != c.param || !strings.Contains(res.Err.Message, c.says) {
			t.Errorf("%s: param %v message %q", c.name, res.Err.Param, res.Err.Message)
		}
		if f.count() != 0 {
			t.Errorf("%s: a request over the host's limit was sent", c.name)
		}
	}
}

// TestSystemOneRefusesAnErrorBodyWearingA200: an upstream that answers 200 with
// an error envelope must not reach the client as an answer.
func TestSystemOneRefusesAnErrorBodyWearingA200(t *testing.T) {
	f := newFakeUpstream(t)
	p, err := NewProvider(Spec{Name: "ts", Kind: "typesafe", API: catalog.APISystemOne, BaseURL: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	f.answer(http.StatusOK, `{"error":"model not found"}`)
	res := testBackend("k").Do(context.Background(), target(p), s1Call(t, s1Request), nil)
	if res.Err == nil {
		t.Fatalf("an error envelope on a 200 was handed back as an answer: %s", res.Body)
	}
}
