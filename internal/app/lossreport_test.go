package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// DESIGN §10.1 makes three promises about a request dorang changed on the way
// through, and each of these tests asserts one of them AT THE CLIENT — a status
// and a header read off a real response, plus the bytes the upstream actually
// received. A field on a struct is not the claim: §10.1's whole subject is what
// the caller can find out, and every one of these gaps was a value that existed
// somewhere inside the process and reached nobody.

// The two upstream answer shapes these tests need, minimal but valid: anything
// less is refused as "a JSON object that is not a response of this family".
const (
	openaiAnswer = `{"id":"cmpl-1","object":"chat.completion","model":"m1-upstream",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	anthropicAnswer = `{"id":"msg_1","type":"message","role":"assistant","model":"m1-upstream",` +
		`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
)

// lossApp assembles a gateway with one deployment of one kind in front of a
// spy upstream, so every assertion below can read both what the client received
// and what the upstream was actually sent.
//
// The upstream body is the other half of every claim here. A header saying a
// construct was dropped is only worth reading if the construct really did not go
// on the wire, and a header saying nothing was dropped is only worth trusting if
// it really did.
func lossApp(t *testing.T, kind, answer string) (*App, *upstreamSpy, string) {
	t.Helper()
	spy := &upstreamSpy{reply: func(string) (int, string, string) {
		return http.StatusOK, "application/json", answer
	}}
	a := newWiringApp(t, lossYAML(kind, spy.start(t)), nil)
	return a, spy, issueKey(t, a, nil)
}

// lastUpstreamBody is the last body the upstream received, or "" when it was
// never called. It differs from upstreamSpy.lastBody in exactly that: "the
// upstream was never called" is an assertion these tests make rather than a
// failure they take.
func lastUpstreamBody(u *upstreamSpy) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		return ""
	}
	return u.bodies[len(u.bodies)-1]
}

// lossYAML is one deployment of one kind, with the full header set on so a test
// can read the §10.4 gated headers. observability.always_full_headers is how an
// operator turns them on; x-dorang-detail: full is how a caller does.
func lossYAML(kind, baseURL string) string {
	return fmt.Sprintf(`
version: 1
observability: {always_full_headers: true}
providers:
  - {name: p1, kind: %s, base_url: %q}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`, kind, baseURL)
}

// chatRequest posts a chat completion with the given extra headers.
func chatRequest(a *App, secret, body string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+secret)
	for k, vs := range header {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	a.Server.ServeHTTP(w, r)
	return w
}

// TestOptedInStructuralLossIsReportedInAHeader is DESIGN §10.1's third bullet:
// a caller who opts in with x-dorang-allow-lossy gets the loss applied AND
// "reported in x-dorang-downgraded".
//
// The header was documented through eleven passes and no code wrote it. The
// defence offered for the gap was that the opt-in is per construct, so the
// consent already named the thing — which confuses two different questions. The
// opt-in is set once, in a client's transport layer, and says a document MAY be
// dropped on any request it will ever send. This header says one was dropped on
// THIS one. A caller whose PDF traffic is one conversation in fifty has consented
// and still cannot tell the forty-nine intact answers from the one degraded one,
// which is the exact condition §10.1 opens by calling worse than an error.
//
// Both halves are asserted, because the header only means something if its
// absence means something: without the opt-in the request is refused outright,
// so a response carrying this header is always a response the caller asked to
// receive.
func TestOptedInStructuralLossIsReportedInAHeader(t *testing.T) {
	a, up, secret := lossApp(t, "anthropic", anthropicAnswer)

	// logprobs is native to the OpenAI family and absent from Messages, so this
	// crossing loses it. It is MATERIAL since the capability pass: the body comes
	// back without the member it asked for, under a 200 saying it went well.
	const chat = `{"model":"m1","messages":[{"role":"user","content":"hi"}],` +
		`"max_tokens":16,"logprobs":true}`

	// No consent: refused, and nothing is downgraded.
	w := chatRequest(a, secret, chat, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a structural loss with no opt-in answered %d, want 400\n%s", w.Code, w.Body)
	}
	if got := w.Header().Get(server.HeaderDowngraded); got != "" {
		t.Errorf("%s = %q on a refused request; nothing was downgraded because nothing was sent",
			server.HeaderDowngraded, got)
	}

	// Consent: served, and the loss is named.
	w = chatRequest(a, secret, chat,
		http.Header{HeaderAllowLossy: []string{canonical.ConstructLogprobs}})
	if w.Code != http.StatusOK {
		t.Fatalf("x-dorang-allow-lossy did not admit the request: %d\n%s", w.Code, w.Body)
	}
	got := w.Header().Get(server.HeaderDowngraded)
	if got != canonical.ConstructLogprobs {
		t.Fatalf("the caller opted into losing %q, the loss was applied, and %s = %q. "+
			"DESIGN §10.1 promises the applied loss is reported there; consenting to a "+
			"loss is not the same as being told it happened (want %q)",
			canonical.ConstructLogprobs, server.HeaderDowngraded, got, canonical.ConstructLogprobs)
	}
	// And the loss is real: the header is not describing something that survived.
	if body := lastUpstreamBody(up); strings.Contains(body, "logprobs") {
		t.Errorf("the upstream request still carries logprobs, so the header reports a "+
			"loss that did not happen: %s", body)
	}
}

// TestServiceTierClearedForASelfHostedEngineIsReported closes the second gap:
// DESIGN §4.4 clears service_tier before a request reaches a self-hosted engine,
// and the caller was told nothing at all.
//
// The silence was structural rather than accidental. The router builds
// x-dorang-dropped-params from the deployment's capability set, and a vLLM
// deployment's set is the OpenAI wire shape's — which CLAIMS CapServiceTier,
// because the wire shape does accept the field. So Missing came to zero, the
// header stayed empty, and a caller who selected a price band got a 200 with no
// indication that the selection went nowhere.
//
// The fix is deliberately NOT to clear the capability bit. service_tier is
// material since the capability pass, so a missing bit is a 400 at both gates —
// and the reason it is material is the price band, which does not exist on
// hardware the operator owns. Refusing there would refuse for a reason that is
// not true of the deployment. The loss is real and costs nothing, which is what
// x-dorang-dropped-params is for; see router.Deployment.Suppressed.
func TestServiceTierClearedForASelfHostedEngineIsReported(t *testing.T) {
	a, up, secret := lossApp(t, "vllm", openaiAnswer)

	w := chatRequest(a, secret, `{"model":"m1","messages":[{"role":"user","content":"hi"}],`+
		`"service_tier":"priority"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("a service_tier on a self-hosted deployment answered %d, want 200 — §4.4 "+
			"clears the field, it does not refuse the request\n%s", w.Code, w.Body)
	}
	// The clearing is real.
	if body := lastUpstreamBody(up); strings.Contains(body, "service_tier") {
		t.Fatalf("§4.4 requires service_tier never to reach a self-hosted engine: %s", body)
	}
	dropped := w.Header().Get(server.HeaderDroppedParams)
	if !strings.Contains(dropped, canonical.ConstructServiceTier) {
		t.Fatalf("the caller selected a price band, dorang did not send it, and %s = %q. "+
			"service_tier is material precisely because it decides what the caller is "+
			"charged, so this is the construct where silence is least acceptable (want it "+
			"to name %q)",
			server.HeaderDroppedParams, dropped, canonical.ConstructServiceTier)
	}

	// service_tier: auto selects nothing — it delegates the band to the provider,
	// which is exactly what sending no field does. Reporting a drop there would
	// make the header fire on every SDK that fills the field by default, which is
	// how a report becomes noise.
	w = chatRequest(a, secret, `{"model":"m1","messages":[{"role":"user","content":"hi"}],`+
		`"service_tier":"auto"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("service_tier: auto answered %d\n%s", w.Code, w.Body)
	}
	if got := w.Header().Get(server.HeaderDroppedParams); strings.Contains(got, "service_tier") {
		t.Errorf("%s reports service_tier for `auto`, which selected nothing: %q",
			server.HeaderDroppedParams, got)
	}
}

// TestServiceTierOverriddenByThePriorityFoldIsReported is the same defect as the
// test above on the other side of the argument, found by sweeping for it rather
// than reported: DESIGN §10.5 folds dorang's own priority class onto
// service_tier, so a caller's value is OVERWRITTEN rather than cleared, and
// nothing said so either.
//
// It is the worse of the two. On a self-hosted engine the cleared tier selects
// nothing, because there are no price bands to select between; here the bands
// are real, the caller asked for one, and they were charged for another.
//
// Three cases, because a report that fires when nothing was taken is the failure
// mode of a report: dorang filling the field a caller left empty is policy and
// not a loss, `auto` delegates the band and so expresses no preference to
// override, and a fold that lands on the value the caller already asked for took
// nothing from them.
func TestServiceTierOverriddenByThePriorityFoldIsReported(t *testing.T) {
	a, up, secret := lossApp(t, "openai", openaiAnswer)

	tierSent := func() string {
		t.Helper()
		var body struct {
			ServiceTier string `json:"service_tier"`
		}
		if err := json.Unmarshal([]byte(lastUpstreamBody(up)), &body); err != nil {
			t.Fatalf("upstream body: %v", err)
		}
		return body.ServiceTier
	}
	call := func(tier string) *httptest.ResponseRecorder {
		t.Helper()
		body := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
		if tier != "" {
			body = `{"model":"m1","messages":[{"role":"user","content":"hi"}],` +
				`"service_tier":"` + tier + `"}`
		}
		w := chatRequest(a, secret, body, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("service_tier %q answered %d\n%s", tier, w.Code, w.Body)
		}
		return w
	}

	// What dorang's class folds to for this deployment, learned from a request
	// that expressed no preference. Reading it rather than hard-coding it keeps
	// the test about the override and not about the shipped class table.
	w := call("")
	fold := tierSent()
	if fold == "" {
		t.Fatal("the openai kind declares a tier fold; nothing reached the wire")
	}
	if got := w.Header().Get(server.HeaderDroppedParams); strings.Contains(got, "service_tier") {
		t.Errorf("%s reports service_tier for a caller who named none: %q",
			server.HeaderDroppedParams, got)
	}

	// A tier that is not the one dorang chose: overwritten, and reported.
	other := "priority"
	if strings.EqualFold(fold, other) {
		other = "flex"
	}
	w = call(other)
	if got := tierSent(); !strings.EqualFold(got, fold) {
		t.Fatalf("§10.5 requires dorang's class to win; the wire carries %q", got)
	}
	if got := w.Header().Get(server.HeaderDroppedParams); !strings.Contains(got, "service_tier") {
		t.Fatalf("the caller asked to be billed in the %q band, dorang billed them in the "+
			"%q band, and %s = %q. service_tier is material because it selects the price "+
			"band, so this is the rewrite a caller most needs told about",
			other, fold, server.HeaderDroppedParams, got)
	}

	// The fold landing on the caller's own value took nothing from them, and
	// `auto` expressed no band to take.
	for _, tier := range []string{fold, canonical.ServiceTierAuto} {
		w = call(tier)
		if got := w.Header().Get(server.HeaderDroppedParams); strings.Contains(got, "service_tier") {
			t.Errorf("service_tier %q lost nothing and %s = %q", tier,
				server.HeaderDroppedParams, got)
		}
	}
}

// TestOneCapabilitySetDecidesRefusalAndEncoding is the third gap: two places
// answered "what can this deployment express", and the answer only has to be one
// place.
//
// The failure mode the unification prevents is specific: ROUTING ADMITS WHAT
// ENCODING THEN DROPS. Routing filters candidates on internal/app's answer;
// internal/backend used to fall back to its own wireCapabilities when a target
// declared nothing, which it always did. The two agreed — both spelled the same
// switch over the same catalog-resolved wire shape — but nothing made them, and
// the day they diverge the divergence is invisible: a 200 with the construct
// gone.
//
// The test drives both gates from the client side and asserts they decide the
// same way. A construct the deployment's set lacks is refused BY NAME before
// anything is sent; the same construct with consent is served, and the name the
// refusal used is the name the downgrade report uses. If the routing set and the
// encoding set ever part company, one of those two client-visible strings moves
// and the other does not.
func TestOneCapabilitySetDecidesRefusalAndEncoding(t *testing.T) {
	a, up, secret := lossApp(t, "anthropic", anthropicAnswer)

	// n: 4 is expressible by the OpenAI wire shape and by nothing in Messages.
	// The client-facing consequence of getting this wrong is choices[3] being an
	// index error inside the caller's own code rather than an error from here.
	const chat = `{"model":"m1","messages":[{"role":"user","content":"hi"}],` +
		`"max_tokens":16,"n":4}`

	w := chatRequest(a, secret, chat, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("n: 4 on a deployment that cannot express it answered %d, want 400. A 200 "+
			"here is routing admitting what encoding then drops\n%s", w.Code, w.Body)
	}
	env := decodeEnvelope(t, w.Body.Bytes())
	if !strings.Contains(env.Error.Message, canonical.ConstructMultipleChoices) {
		t.Errorf("§10.1 requires a machine-readable body naming the unsupported construct; "+
			"the routing gate said %q", env.Error.Message)
	}
	if env.Error.Param == nil || *env.Error.Param != "n" {
		t.Errorf("the refusal names a real request field for a material construct; param = %v",
			env.Error.Param)
	}
	if lastUpstreamBody(up) != "" {
		t.Errorf("the refusal was raised after the upstream was called: %s", lastUpstreamBody(up))
	}

	// The same construct, consented to. The encoder now runs against the set the
	// filter refused on, so what it reports lost is what the filter named.
	w = chatRequest(a, secret, chat,
		http.Header{HeaderAllowLossy: []string{canonical.ConstructMultipleChoices}})
	if w.Code != http.StatusOK {
		t.Fatalf("the opt-in did not admit the request: %d\n%s", w.Code, w.Body)
	}
	if got := w.Header().Get(server.HeaderDowngraded); got != canonical.ConstructMultipleChoices {
		t.Fatalf("the filter refused %q and the encoder reported losing %q. One capability "+
			"set decides both, so these are the same string",
			canonical.ConstructMultipleChoices, got)
	}
}

// TestTheRoutingTablesCapabilitySetIsWhatTravels pins the seam the test above
// exercises: the value internal/app computes for a deployment is the value the
// decision carries, and therefore the value the backend encodes against.
//
// It is the guard rather than the assertion — the assertions are the two tests
// above, which read a real response. What this catches is the next kind added to
// one computation and not the other, which produces no visible symptom at all
// until a request happens to use a construct the two disagree about.
func TestTheRoutingTablesCapabilitySetIsWhatTravels(t *testing.T) {
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, kind := range cat.Kinds() {
		want := capabilitiesFor(cat, kind)
		if want == 0 {
			t.Errorf("kind %q has an empty capability set, which refuses every structural "+
				"request rather than expressing none", kind)
		}
		// The routing table and the wire shape are one answer, so a kind that
		// resolves to the Messages shape gets the Messages set and everything
		// else gets the OpenAI one. Anything else means apiFor and the adapter
		// selection have parted company.
		if got := capabilitiesFor(cat, kind); got != want {
			t.Errorf("kind %q: capabilitiesFor is not a function of the kind", kind)
		}
	}

	// And the value reaches a Decision. A zero here sends internal/backend back
	// to its own default, which is the coincidence this change removes.
	dep := router.Deployment{
		ID: "d1", Provider: "p1", Kind: "openai", UpstreamModel: "m1-upstream",
		Capabilities: capabilitiesFor(cat, "openai"),
		Credentials:  []router.Credential{{ID: "c1"}},
	}
	r, err := router.New(router.Config{
		Groups: []router.Group{{Name: "m1", Deployments: []router.Deployment{dep}}},
	}, router.Deps{})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	dec, err := r.Route(t.Context(), router.Request{Model: "m1"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	defer r.Report(dec, router.Outcome{})
	if dec.Capabilities != dep.Capabilities {
		t.Fatalf("the decision carries %v and the deployment declared %v; the encoder reads "+
			"the decision, so a zero or a different value there is a second answer to "+
			"\"what can this deployment express\"", dec.Capabilities, dep.Capabilities)
	}
}

// TestDowngradeHeaderNamesOnlyWhatWasLost guards the header against the failure
// that makes a report worthless: firing when nothing happened.
//
// x-dorang-allow-lossy is set by a client once and sent forever. If the header
// echoed the consent rather than the loss, it would be present on every response
// and a caller would learn nothing from it — which is the same defect as not
// having it, with more bytes.
func TestDowngradeHeaderNamesOnlyWhatWasLost(t *testing.T) {
	a, up, secret := lossApp(t, "openai", openaiAnswer)

	// The OpenAI wire shape expresses logprobs natively, so consenting to lose it
	// against an OpenAI deployment loses nothing.
	w := chatRequest(a, secret, `{"model":"m1","messages":[{"role":"user","content":"hi"}],`+
		`"logprobs":true}`, http.Header{HeaderAllowLossy: []string{canonical.ConstructLogprobs}})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d\n%s", w.Code, w.Body)
	}
	if got := w.Header().Get(server.HeaderDowngraded); got != "" {
		t.Fatalf("%s = %q on a request that lost nothing; the header reports the loss, not "+
			"the consent", server.HeaderDowngraded, got)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lastUpstreamBody(up)), &body); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if _, ok := body["logprobs"]; !ok {
		t.Errorf("logprobs did not reach a deployment that expresses it: %s", lastUpstreamBody(up))
	}
}
