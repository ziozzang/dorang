package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// threeRouteHost serves the three chat-shaped routes an Ollama Cloud host
// serves, each in its own family's shape, and records what reached each.
type threeRouteHost struct {
	mu   sync.Mutex
	hits []routeHit
}

type routeHit struct {
	path, auth string
	body       map[string]any
}

func serveThreeRoutes(t *testing.T, f *fakeUpstream) *threeRouteHost {
	t.Helper()
	h := &threeRouteHost{}
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.Unmarshal(f.last().body, &body)
		h.mu.Lock()
		h.hits = append(h.hits, routeHit{r.URL.Path, r.Header.Get("Authorization"), body})
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages":
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"Unauthorized"}}`))
				return
			}
			if stream, _ := body["stream"].(bool); stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"native\"}}\n\n" +
					"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
				return
			}
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"native"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
		case "/v1/responses":
			_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"native"}]}],"usage":{"input_tokens":3,"output_tokens":1}}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"converted"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
		default:
			http.Error(w, `{"error":"no such route"}`, http.StatusNotFound)
		}
	})
	return h
}

func (h *threeRouteHost) last() routeHit {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits[len(h.hits)-1]
}

// surfacedProvider is an OpenAI-shaped host declaring the Anthropic and
// Responses routes natively, as the catalog declares Ollama Cloud.
func surfacedProvider(t *testing.T, f *fakeUpstream, surfaces ...string) *Provider {
	t.Helper()
	p, err := NewProvider(Spec{Name: "ollama", Kind: "ollama-cloud", API: catalog.APIOpenAIChat,
		BaseURL: f.srv.URL, Surfaces: surfaces})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

// messagesCall is an Anthropic-family caller whose request carries a construct
// the chat crossing loses: a signed thinking block in the prior turn.
func messagesCall() *Call {
	c := chatCall(catalog.APIAnthropicMessages)
	c.Request.Messages = []canonical.Message{
		canonical.TextMessage(canonical.RoleUser, "hi"),
		{Role: canonical.RoleAssistant, Content: canonical.Content{
			canonical.ThinkingBlock("weighing", "sig_abc"), canonical.TextBlock("ok")}},
		canonical.TextMessage(canonical.RoleUser, "go"),
	}
	return c
}

// A caller on the host's own surface reaches it there, untranslated.
//
// docs/SURFACES.md: Ollama Cloud serves /v1/messages and /v1/responses and
// dorang used one route, converting an Anthropic caller's request to chat and
// reporting every construct that crossing loses — against a host that would
// have taken it natively. The observables are the ROUTE the host saw, the
// CREDENTIAL spelling (the host's bearer, not the family's x-api-key, which
// this host answers 401 to), the SHAPE of the body it received, and the loss
// report the caller gets: nothing, where the chat crossing reported a
// downgrade.
func TestACallerOnANativeSurfaceIsNotTranslated(t *testing.T) {
	t.Run("messages caller, host declares the surface", func(t *testing.T) {
		f := newFakeUpstream(t)
		host := serveThreeRoutes(t, f)
		b := testBackend("sk-test")
		c := messagesCall()
		var loss *canonical.LossReport
		c.Accepted = func(l *canonical.LossReport) { loss = l }
		res := b.Do(context.Background(), target(surfacedProvider(t, f, "chat", "messages", "responses")), c, httptest.NewRecorder())
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		hit := host.last()
		if hit.path != "/v1/messages" {
			t.Fatalf("host saw %q; the caller's own surface is served and was not used", hit.path)
		}
		if !strings.HasPrefix(hit.auth, "Bearer ") {
			t.Errorf("credential = %q; this host takes the bearer it takes on every route", hit.auth)
		}
		if _, ok := hit.body["max_tokens"]; !ok || hit.body["messages"] == nil {
			t.Errorf("the body is not the Messages shape: %v", hit.body)
		}
		if !strings.Contains(string(res.Body), `"native"`) || !strings.Contains(string(res.Body), `"type":"message"`) {
			t.Errorf("the caller did not receive the host's own answer as a message:\n%s", res.Body)
		}
		if loss == nil || len(loss.Dropped) != 0 || len(loss.Downgrades) != 0 {
			t.Errorf("a same-family exchange reported a loss: %+v", loss)
		}
	})

	t.Run("messages caller, surface not declared: the chat crossing as before", func(t *testing.T) {
		f := newFakeUpstream(t)
		host := serveThreeRoutes(t, f)
		b := testBackend("sk-test")
		c := messagesCall()
		var loss *canonical.LossReport
		c.Accepted = func(l *canonical.LossReport) { loss = l }
		res := b.Do(context.Background(), target(surfacedProvider(t, f)), c, httptest.NewRecorder())
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		if hit := host.last(); hit.path != "/v1/chat/completions" {
			t.Fatalf("host saw %q, want the chat route for a host that declared nothing", hit.path)
		}
		if loss == nil || len(loss.Dropped)+len(loss.Downgrades) == 0 {
			t.Errorf("the chat crossing reported no loss for a signed thinking block; the native "+
				"case above proves nothing without this contrast: %+v", loss)
		}
	})

	t.Run("responses caller reaches /v1/responses under the versioned base", func(t *testing.T) {
		f := newFakeUpstream(t)
		host := serveThreeRoutes(t, f)
		b := testBackend("sk-test")
		c := chatCall(catalog.APIOpenAIResponses)
		c.Op = OpResponses
		res := b.Do(context.Background(), target(surfacedProvider(t, f, "responses")), c, httptest.NewRecorder())
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		if hit := host.last(); hit.path != "/v1/responses" {
			t.Fatalf("host saw %q", hit.path)
		}
		if !strings.Contains(string(res.Body), `"object":"response"`) {
			t.Errorf("body:\n%s", res.Body)
		}
	})

	t.Run("chat caller still takes the chat route", func(t *testing.T) {
		f := newFakeUpstream(t)
		host := serveThreeRoutes(t, f)
		b := testBackend("sk-test")
		res := b.Do(context.Background(), target(surfacedProvider(t, f, "messages", "responses")),
			chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		if hit := host.last(); hit.path != "/v1/chat/completions" {
			t.Errorf("host saw %q", hit.path)
		}
	})

	t.Run("streaming messages caller is relayed from /v1/messages", func(t *testing.T) {
		f := newFakeUpstream(t)
		host := serveThreeRoutes(t, f)
		b := testBackend("sk-test")
		c := messagesCall()
		c.Stream, c.Request.Stream = true, true
		rec := httptest.NewRecorder()
		res := b.Do(context.Background(), target(surfacedProvider(t, f, "messages")), c, rec)
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		if hit := host.last(); hit.path != "/v1/messages" {
			t.Fatalf("host saw %q", hit.path)
		}
		if body := rec.Body.String(); !strings.Contains(body, "message_start") || !strings.Contains(body, "native") {
			t.Errorf("the client did not receive the relayed Messages stream:\n%s", body)
		}
	})
}

// A surface the build cannot serve natively is refused at start-up, by name.
func TestAnUnservableSurfaceIsRefused(t *testing.T) {
	_, err := NewProvider(Spec{Kind: "anthropic", API: catalog.APIAnthropicMessages,
		BaseURL: "https://example.invalid", Surfaces: []string{"responses"}})
	if err == nil || !strings.Contains(err.Error(), "responses") {
		t.Errorf("a Responses surface on an Anthropic-shaped host was accepted: %v", err)
	}
}
