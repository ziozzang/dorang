package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// The z.ai coding plan answers a request naming `glm-5.1` with 200, a
// well-formed body, real tokens — and `"model":"glm-5.2"`. Nothing about the
// status, the shape or the usage says so. The response body's own model field
// is the only witness, and this is the buffered half of reading it.
func TestBufferedAnswerReportsASubstitutedModel(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, chatBody("glm-5.2"))
	p := testProvider(t, f, "glm", catalog.APIOpenAIChat)
	b := testBackend("k")

	tg := target(p)
	tg.UpstreamModel = "glm-5.1"
	tg.ModelKnown = true

	res := b.Do(context.Background(), tg, chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the substitution arrives as a success", res.Status)
	}
	if res.ModelAgreement != canonical.ModelSubstituted {
		t.Errorf("ModelAgreement = %v, want substituted", res.ModelAgreement)
	}
	if res.ServedModel != "glm-5.2" {
		t.Errorf("ServedModel = %q, want %q", res.ServedModel, "glm-5.2")
	}
	// §7.2 still wins on the wire: the client is told the name it asked for.
	// That is exactly what makes the header and the counter the only surfaces
	// the disagreement survives on.
	if !strings.Contains(string(res.Body), `"model":"client-model"`) {
		t.Errorf("client body does not carry the client-facing name: %s", res.Body)
	}
}

func TestBufferedAnswerIsSilentWhenTheModelEchoes(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, chatBody("glm-5.2"))
	p := testProvider(t, f, "glm", catalog.APIOpenAIChat)
	b := testBackend("k")

	tg := target(p)
	tg.UpstreamModel = "glm-5.2"
	tg.ModelKnown = true

	res := b.Do(context.Background(), tg, chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if res.ModelAgreement != canonical.ModelEcho || res.ServedModel != "" {
		t.Errorf("agreement=%v served=%q, want echo and no name", res.ModelAgreement, res.ServedModel)
	}
}

// Ordinary version pinning must not report anything. OpenAI answers `gpt-4o`
// with `gpt-4o-2024-08-06` on every request; a check that fired here would fire
// on ordinary traffic and would be switched off.
func TestBufferedAnswerIsSilentOnARefinement(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, chatBody("gpt-4o-2024-08-06"))
	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	b := testBackend("k")

	tg := target(p)
	tg.UpstreamModel = "gpt-4o"
	tg.ModelKnown = true

	res := b.Do(context.Background(), tg, chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if res.ModelAgreement != canonical.ModelRefined {
		t.Errorf("ModelAgreement = %v, want refined", res.ModelAgreement)
	}
	if res.ServedModel != "" {
		t.Errorf("ServedModel = %q, want empty — a dated build is not a substitution", res.ServedModel)
	}
}

// A deployment whose model the catalog does not know does not pay for the
// check. This is the operator's `local` alias, answered by whatever the server
// loaded: the classification still runs, and the NAME is not copied out,
// because nothing downstream could prove anything with it.
func TestUnknownModelDoesNotCarryAName(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, chatBody("Gemma-4-Garnet-V2-31B-it-ultra-uncensored-heretic.i1-Q6_K.gguf"))
	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	b := testBackend("k")

	tg := target(p)
	tg.UpstreamModel = "local"
	tg.ModelKnown = false

	res := b.Do(context.Background(), tg, chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if res.ServedModel != "" {
		t.Errorf("ServedModel = %q, want empty for a model the catalog does not know",
			res.ServedModel)
	}
}

// The streaming half. The relay never buffers and never decodes a frame, and —
// on the ordinary aliasing path — REWRITES the model field on its way out, so
// the upstream's own answer reaches neither the client nor any later reader.
// The reading has to be taken during the scan or not at all.
func TestStreamedAnswerReportsASubstitutedModel(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseChunk("glm-5.2", "he") + sseChunk("glm-5.2", "llo") +
			sseUsage("glm-5.2") + "data: [DONE]\n\n"))
	})
	p := testProvider(t, f, "glm", catalog.APIOpenAIChat)
	b := testBackend("k")

	tg := target(p)
	tg.UpstreamModel = "glm-5.1"
	tg.ModelKnown = true

	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true
	rec := httptest.NewRecorder()
	res := b.Do(context.Background(), tg, c, rec)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if res.ModelAgreement != canonical.ModelSubstituted {
		t.Errorf("ModelAgreement = %v, want substituted", res.ModelAgreement)
	}
	if res.ServedModel != "glm-5.2" {
		t.Errorf("ServedModel = %q, want %q", res.ServedModel, "glm-5.2")
	}
	// The evidence is gone from the stream itself, which is the point: the
	// relay rewrote every frame to the client-facing name (COMPATIBILITY 2.5).
	if body := rec.Body.String(); strings.Contains(body, "glm-5.2") {
		t.Errorf("the relayed stream still names the upstream model; §7.2 requires the "+
			"client-facing name on every chunk: %s", body)
	}
}

func TestStreamedAnswerIsSilentWhenTheModelEchoes(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseChunk("glm-5.2", "hi") + sseUsage("glm-5.2") +
			"data: [DONE]\n\n"))
	})
	p := testProvider(t, f, "glm", catalog.APIOpenAIChat)
	b := testBackend("k")

	tg := target(p)
	tg.UpstreamModel = "glm-5.2"
	tg.ModelKnown = true

	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true
	res := b.Do(context.Background(), tg, c, httptest.NewRecorder())
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if res.ModelAgreement != canonical.ModelEcho || res.ServedModel != "" {
		t.Errorf("agreement=%v served=%q, want echo and no name", res.ModelAgreement, res.ServedModel)
	}
}

// The other decoder that overwrites the upstream's name with the client's. The
// Anthropic family is where the refinement half is easiest to get wrong for
// real: `claude-haiku-4-5` and `claude-haiku-4-5-20251001` are BOTH entries in
// this build's catalog, under the same kind, with identical figures — so a rule
// that asked only "is the returned name a different catalog entry" would fire
// on every request Anthropic serves.
func TestAnthropicAnswerDistinguishesARefinementFromASubstitution(t *testing.T) {
	for _, c := range []struct {
		name, asked, served string
		want                canonical.ModelAgreement
		wantName            string
	}{
		{"dated build", "claude-haiku-4-5", "claude-haiku-4-5-20251001",
			canonical.ModelRefined, ""},
		{"a different model", "claude-haiku-4-5", "claude-opus-4-8",
			canonical.ModelSubstituted, "claude-opus-4-8"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(http.StatusOK, messagesBody(c.served))
			p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)
			b := testBackend("k")

			tg := target(p)
			tg.UpstreamModel = c.asked
			tg.ModelKnown = true

			res := b.Do(context.Background(), tg, chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
			if res.Err != nil {
				t.Fatalf("Do: %v", res.Err)
			}
			if res.ModelAgreement != c.want {
				t.Errorf("ModelAgreement = %v, want %v", res.ModelAgreement, c.want)
			}
			if res.ServedModel != c.wantName {
				t.Errorf("ServedModel = %q, want %q", res.ServedModel, c.wantName)
			}
		})
	}
}

func messagesBody(model string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"` + model + `",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":3,"output_tokens":2}}`
}

func chatBody(model string) string {
	return `{"id":"c1","object":"chat.completion","created":1,"model":"` + model + `",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,` +
		`"total_tokens":5}}`
}

func sseChunk(model, text string) string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"` +
		model + `","choices":[{"index":0,"delta":{"content":"` + text + `"}}]}` + "\n\n"
}

func sseUsage(model string) string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"` +
		model + `","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,` +
		`"total_tokens":5}}` + "\n\n"
}
