package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// The T1 dispatcher: which family a route decodes to, and what is refused.
//
// DESIGN §0.2's rule is that anything not implemented answers 501 with a
// machine-readable code — never a silent 404, and never a 200 that quietly did
// something else. Every refusal below is asserted on its CODE, because that is
// what a client branches on.

// decodeRoute runs the decode half of the dispatcher for one route and body.
func decodeRoute(t *testing.T, family server.Family, body string, form *canonical.Form) (*call, error) {
	t.Helper()
	d := newDispatcher(nil, nil, func(string, ...any) {}, nil)
	st := &dispatchState{}
	d.swap(st)
	rt := &server.Route{Family: family}
	rq := &server.Request{
		HTTP:   httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(body)),
		Route:  rt,
		Form:   form,
		Method: http.MethodPost,
		Path:   "/v1/x",
	}
	c := &call{model: "m", body: []byte(body)}
	err := d.decodeT1(st, rq, c)
	return c, err
}

func assertRefused(t *testing.T, err error, status int, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the request was accepted; want %d %s", status, code)
	}
	e, ok := err.(*server.Error)
	if !ok {
		t.Fatalf("%v is not a server error", err)
	}
	if e.Status != status || e.Code != code {
		t.Fatalf("status %d code %q, want %d %q", e.Status, e.Code, status, code)
	}
}

// TestT1DecodeSelectsTheRightNeutralType. One typed pointer per kind, so a
// consumer that switches on kind cannot read a field that was never filled.
func TestT1DecodeSelectsTheRightNeutralType(t *testing.T) {
	form := &canonical.Form{
		Values: map[string][]string{"model": {"m"}},
		Files:  []canonical.File{{Field: "file", Name: "a.wav", Data: []byte("x")}},
	}
	imageForm := &canonical.Form{
		Values: map[string][]string{"model": {"m"}, "prompt": {"p"}},
		Files:  []canonical.File{{Field: "image", Name: "a.png", Data: []byte("x")}},
	}
	cases := []struct {
		family server.Family
		body   string
		form   *canonical.Form
		kind   callKind
		filled func(*call) bool
	}{
		{server.FamilyOpenAICompletions, `{"model":"m","prompt":"x"}`, nil, callCompletions,
			func(c *call) bool { return c.creq != nil && c.creq.Prompt != nil }},
		{server.FamilyOpenAIModerations, `{"model":"m","input":"x"}`, nil, callModerations,
			func(c *call) bool { return c.modReq != nil }},
		{server.FamilyOpenAIRerank, `{"model":"m","query":"q","documents":["a"]}`, nil, callRerank,
			func(c *call) bool { return c.rerankReq != nil }},
		{server.FamilyOpenAISpeech, `{"model":"m","input":"x","voice":"v"}`, nil, callSpeech,
			func(c *call) bool { return c.speechReq != nil }},
		{server.FamilyOpenAITranscription, ``, form, callTranscription,
			func(c *call) bool { return c.transReq != nil && !c.transReq.Translate }},
		{server.FamilyOpenAITranslation, ``, form, callTranscription,
			func(c *call) bool { return c.transReq != nil && c.transReq.Translate }},
		{server.FamilyOpenAIImageGeneration, `{"model":"m","prompt":"x"}`, nil, callImages,
			func(c *call) bool { return c.imageReq != nil && c.imageReq.Op == canonical.ImageGenerate }},
		{server.FamilyOpenAIImageEdit, ``, imageForm, callImages,
			func(c *call) bool { return c.imageReq != nil && c.imageReq.Op == canonical.ImageEdit }},
		{server.FamilyOpenAIImageVariation, ``, imageForm, callImages,
			func(c *call) bool { return c.imageReq != nil && c.imageReq.Op == canonical.ImageVariation }},
	}
	for _, tc := range cases {
		t.Run(tc.family.String(), func(t *testing.T) {
			c, err := decodeRoute(t, tc.family, tc.body, tc.form)
			if err != nil {
				t.Fatal(err)
			}
			if c.kind != tc.kind {
				t.Errorf("kind %v, want %v", c.kind, tc.kind)
			}
			if !tc.filled(c) {
				t.Errorf("the neutral request for %v was not filled", tc.family)
			}
		})
	}
}

// TestT1StreamingRefusalsAreNamed. Three of these surfaces have a streaming
// mode dorang does not implement. Answering the non-streaming body to a client
// that asked for frames is a half-working endpoint; a named 501 is the contract.
func TestT1StreamingRefusalsAreNamed(t *testing.T) {
	cases := []struct {
		name   string
		family server.Family
		body   string
		form   *canonical.Form
		code   string
	}{
		{"responses", server.FamilyOpenAIResponses,
			`{"model":"m","input":"x","stream":true}`, nil, "responses_stream_not_implemented"},
		{"speech", server.FamilyOpenAISpeech,
			`{"model":"m","input":"x","voice":"v","stream_format":"sse"}`, nil, "speech_stream_not_implemented"},
		{"images", server.FamilyOpenAIImageGeneration,
			`{"model":"m","prompt":"x","stream":true}`, nil, "image_stream_not_implemented"},
		{"transcription", server.FamilyOpenAITranscription, ``,
			&canonical.Form{
				Values: map[string][]string{"model": {"m"}, "stream": {"true"}},
				Files:  []canonical.File{{Field: "file", Name: "a.wav", Data: []byte("x")}},
			}, "transcription_stream_not_implemented"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeRoute(t, tc.family, tc.body, tc.form)
			assertRefused(t, err, http.StatusNotImplemented, tc.code)
		})
	}
}

// TestT1MalformedBodiesAre400 rather than 501: the route exists, the request
// does not parse.
func TestT1MalformedBodiesAre400(t *testing.T) {
	_, err := decodeRoute(t, server.FamilyOpenAIRerank, `{"model":"m","query":"q"}`, nil)
	assertRefused(t, err, http.StatusBadRequest, "invalid_parameter")

	_, err = decodeRoute(t, server.FamilyOpenAICompletions, `{"model":`, nil)
	assertRefused(t, err, http.StatusBadRequest, "invalid_request")

	_, err = decodeRoute(t, server.FamilyOpenAITranscription, ``,
		&canonical.Form{Values: map[string][]string{"model": {"m"}}})
	assertRefused(t, err, http.StatusBadRequest, "invalid_request")
}

// TestFamilyMismatchIsANamed501 is the crossing that has no meaning.
//
// Encoding a rerank request as chat completions because the deployment happens
// to speak that would answer 200 with something that is not a ranking. The
// refusal names the operation, which is what the caller can change.
func TestFamilyMismatchIsANamed501(t *testing.T) {
	cases := []struct {
		kind callKind
		api  catalog.API
		code string
	}{
		{callRerank, catalog.APIAnthropicMessages, "rerank_family_mismatch"},
		{callEmbeddings, catalog.APIAnthropicMessages, "embeddings_family_mismatch"},
		{callSpeech, catalog.APIAnthropicMessages, "audio_speech_family_mismatch"},
		{callImages, catalog.APIGemini, "images_family_mismatch"},
		{callModerations, catalog.APICohere, "moderations_family_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			assertRefused(t, checkFamily(&call{kind: tc.kind}, tc.api), http.StatusNotImplemented, tc.code)
		})
	}

	// A streaming legacy completion is the one crossing that is refused for a
	// reason the non-streaming form is not: the relay emits chat chunks and a
	// /v1/completions client reads text_completion ones.
	assertRefused(t, checkFamily(&call{kind: callCompletions, stream: true}, catalog.APIAnthropicMessages),
		http.StatusNotImplemented, "completions_family_mismatch")

	// And the crossings that DO have a meaning are permitted. Rerank in
	// particular is served by the two vendors whose protocol it is AND by every
	// OpenAI-compatible self-hosted engine, which all expose /v1/rerank.
	for _, ok := range []struct {
		kind callKind
		api  catalog.API
	}{
		{callRerank, catalog.APICohere},
		{callRerank, catalog.APIJina},
		{callRerank, catalog.APIOpenAIChat},
		{callSpeech, catalog.APIOpenAIChat},
		{callImages, catalog.APIOpenAIResponses},
		{callChat, catalog.APIAnthropicMessages},
		{callCompletions, catalog.APIAnthropicMessages},
	} {
		if err := checkFamily(&call{kind: ok.kind}, ok.api); err != nil {
			t.Errorf("%v on %s was refused: %v", ok.kind, ok.api, err)
		}
	}
}

// Endpoint derivation is asserted in internal/backend, which owns it:
// TestEndpointDerivation covers both rerank spellings — the vendor's own
// /v2/rerank on a bare host and the ordinary /v1/rerank every OpenAI-compatible
// engine serves — alongside every other catalogued kind.
