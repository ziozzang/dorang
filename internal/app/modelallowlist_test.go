package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/store"
)

// The model allow-list had two implementations: internal/auth's `allowedIn`,
// which the request GATE applies, and internal/app's own `modelAllowed`, which
// filtered GET /v1/models. They agreed, and agreeing is not a reason to keep two
// — the two copies in internal/redact agreed as well, right up until one of them
// leaked a credential and the other corrupted ordinary text.
//
// There is now one function, [auth.ModelAllowed]. What follows is not a
// differential between two copies of it: comparing a function against itself is
// what DESIGN §17.1 rule 3 calls documentation, because both readings come from
// the same place. What can still differ, and what actually matters to a client,
// is whether the two INDEPENDENT paths that consume the rule reach the same
// verdict — the listing that says which models exist, and the gate that decides
// whether a request for one is served.

// allowListCorpus is the input set both paths are checked over. Each entry is
// one key's `Models` list and a name to ask about, with the verdict written out
// so that a change to the rule has to be made deliberately in one place.
var allowListCorpus = []struct {
	name  string
	list  []string
	model string
	want  bool
}{
	{"an empty list is an unrestricted key", nil, "gpt-4o", true},
	{"an empty non-nil list is the same", []string{}, "gpt-4o", true},
	{"a star allows everything explicitly", []string{"*"}, "gpt-4o", true},
	{"a star among names still allows everything", []string{"a", "*"}, "gpt-4o", true},
	{"an exact name is admitted", []string{"gpt-4o"}, "gpt-4o", true},
	{"a name not in the list is refused", []string{"gpt-4o"}, "claude", false},
	{"one of several", []string{"a", "gpt-4o", "b"}, "gpt-4o", true},
	// A model name is opaque and nothing splits it (DESIGN §2.1). The ROUTE
	// allow-list next door does honour a trailing "/*", and a copy of the rule
	// that picked up that clause would silently widen every model list an
	// operator wrote with a slash in it.
	{"a trailing /* is not a prefix rule for models", []string{"gpt-4/*"}, "gpt-4/turbo", false},
	{"a trailing /* is not even a self-match", []string{"gpt-4/*"}, "gpt-4/", false},
	{"a slashed name matches whole", []string{"vendor/model-1"}, "vendor/model-1", true},
	// Case and whitespace are part of the name. An upstream id is used verbatim.
	{"names are case sensitive", []string{"GPT-4o"}, "gpt-4o", false},
	{"trailing space is part of the name", []string{"gpt-4o "}, "gpt-4o", false},
	// internal/auth's tier intersection writes this sentinel when a key and its
	// tier grant disjoint sets: "restricted to nothing", which is the opposite
	// of the empty list's "restricted by nothing". A rule that treated it as an
	// ordinary name would still refuse everything, but a rule that normalized it
	// away would grant everything.
	{"the deny-all sentinel admits nothing", []string{"\x00none"}, "gpt-4o", false},
	{"the deny-all sentinel does not admit itself as a model", []string{"\x00none"}, "\x00none", true},
	{"an empty model name against a restricted list", []string{"gpt-4o"}, "", false},
}

// TestModelAllowedIsTheOneRule pins the semantics of the collapsed function, so
// that the two paths below are agreeing about something stated rather than
// merely agreeing with each other.
func TestModelAllowedIsTheOneRule(t *testing.T) {
	for _, c := range allowListCorpus {
		if got := auth.ModelAllowed(c.list, c.model); got != c.want {
			t.Errorf("%s: ModelAllowed(%q, %q) = %v, want %v",
				c.name, c.list, c.model, got, c.want)
		}
	}
}

// allowListYAML declares two models so that a filtered listing is distinguishable
// from an empty one.
const allowListYAML = `
version: 1
observability: {always_full_headers: true}
providers:
  - {name: p1, kind: openai, base_url: %q}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: allowed-model
    deployments:
      - {provider: p1, upstream_model: u1, credentials: [c1]}
  - name: other-model
    deployments:
      - {provider: p1, upstream_model: u2, credentials: [c1]}
`

// listedModels reads the ids GET /v1/models returned.
func listedModels(t *testing.T, a *App, key string) map[string]bool {
	t.Helper()
	w := callWith(a, key, http.MethodGet, "/v1/models", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d\n%s", w.Code, w.Body.String())
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the model list: %v\n%s", err, w.Body.String())
	}
	out := make(map[string]bool, len(body.Data))
	for _, m := range body.Data {
		out[m.ID] = true
	}
	return out
}

// gateServes reports whether a chat request for model was admitted, and fails
// the test on any refusal that is NOT the model allow-list — a 429, a 502 or a
// budget refusal would otherwise read as "the list hid it".
func gateServes(t *testing.T, a *App, key, model string) bool {
	t.Helper()
	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"go"}]}`, model))
	if w.Code == http.StatusOK {
		return true
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the refusal: %v\n%s", err, w.Body.String())
	}
	switch body.Error.Code {
	case "model_not_allowed", "model_not_found":
		return false
	}
	t.Fatalf("model %q was refused for an unrelated reason (%d %s: %s) — the test cannot "+
		"tell the allow-list's verdict from this", model, w.Code, body.Error.Code, body.Error.Message)
	return false
}

// TestTheListingAndTheGateAgreeAboutTheAllowList is the control the collapse
// leaves behind, and it is deliberately not a comparison of two functions.
//
// The two readings come from two different places: GET /v1/models runs the rule
// through server.Principal.AllowsModel over the whole catalog, and
// POST /v1/chat/completions runs it through auth.Principal.Authorize on one
// name. Those were two implementations until now; they are one call each today,
// and this is what notices if a second one grows back — including in the shape
// the duplication most plausibly returns in, which is one path gaining a
// normalization (a case fold, a prefix rule, a trim) that the other does not.
//
// A model the listing shows and the gate refuses is a 403 on a name the client
// was just handed. A model the listing hides and the gate serves is an operator
// who cannot see what their own key can reach.
func TestTheListingAndTheGateAgreeAboutTheAllowList(t *testing.T) {
	p := &upstreamProbe{}
	up := httptest.NewServer(p)
	t.Cleanup(up.Close)
	a := newWiringApp(t, fmt.Sprintf(allowListYAML, up.URL), nil,
		func(o *Options) { o.Upstream = up.Client() })

	for _, c := range []struct {
		name string
		list []string
	}{
		{"unrestricted", nil},
		{"star", []string{"*"}},
		{"one model", []string{"allowed-model"}},
		{"both models", []string{"allowed-model", "other-model"}},
		{"a model that is not deployed", []string{"absent-model"}},
		{"the deny-all sentinel", []string{"\x00none"}},
		// The clause the route allow-list has and this one must not.
		{"a would-be prefix entry", []string{"allowed-model/*"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			key := issueKey(t, a, func(k *store.APIKey) { k.Models = c.list })
			listed := listedModels(t, a, key)
			for _, model := range []string{"allowed-model", "other-model"} {
				served := gateServes(t, a, key, model)
				if listed[model] != served {
					t.Errorf("models=%q, %q: the listing says %v and the gate says %v — "+
						"one rule, two answers",
						c.list, model, listed[model], served)
				}
			}
		})
	}
}
