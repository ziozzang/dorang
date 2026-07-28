package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// zaiNotFound is a real 200 body from a real coding-plan host.
//
// The request was addressed at a route that host does not serve. Rather than
// the 404 its neighbour returns for the same mistake, it answers HTTP 200 with
// this, and it parses cleanly into every wire response struct dorang has.
const zaiNotFound = `{"code":500,"msg":"404 NOT_FOUND","success":false}`

// TestUpstream200WithANonResponseBodyIsNotASuccess asserts what the CLIENT
// receives, in all four protocol crossings.
//
// Asserting that a decoder returns an error would prove nothing about the wire.
// The defect was never in the decoder's return value — it was that the
// zero-valued struct the decoder produced travelled all the way out as a
// finished assistant turn: `{"type":"message","role":"assistant","content":[],
// "stop_reason":null,"usage":{}}` with a synthesized id, or a chat completion
// with an empty `choices` array and a null finish_reason. A caller cannot tell
// either from a real answer, so a misrouted deployment looked like a working
// one to the retry logic, to §7.6 fallback, to internal/health and to the meter
// all at once.
func TestUpstream200WithANonResponseBodyIsNotASuccess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream catalog.API
		client   catalog.API
	}{
		{"messages upstream, messages client", catalog.APIAnthropicMessages, catalog.APIAnthropicMessages},
		{"messages upstream, chat client", catalog.APIAnthropicMessages, catalog.APIOpenAIChat},
		{"chat upstream, chat client", catalog.APIOpenAIChat, catalog.APIOpenAIChat},
		{"chat upstream, messages client", catalog.APIOpenAIChat, catalog.APIAnthropicMessages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(http.StatusOK, zaiNotFound)
			p := testProvider(t, f, "openai", tc.upstream)

			res := testBackend("k").Do(context.Background(), target(p), chatCall(tc.client), nil)

			if res.Err == nil {
				t.Fatalf("the exchange succeeded and handed the client %s;\n"+
					"a vendor error body wearing a 200 must not become an assistant turn",
					res.Body)
			}
			if res.Body != nil {
				t.Errorf("Body = %s, want nothing rendered", res.Body)
			}
			if res.Err.Status != http.StatusBadGateway {
				t.Errorf("status = %d, want 502", res.Err.Status)
			}
			if res.Err.Code != CodeUpstreamShape {
				t.Errorf("code = %q, want %q", res.Err.Code, CodeUpstreamShape)
			}
			if !res.Retryable {
				t.Error("the failure must be offered to the fallback chain: nothing was " +
					"generated, so there is no billed turn a sibling deployment would repeat")
			}
			if res.Usage.InputTokens != 0 || res.Usage.OutputTokens != 0 {
				t.Errorf("usage = %+v, want nothing metered", res.Usage)
			}
		})
	}
}

// TestUpstream200WithANonResponseBodyIsNotAnEmptyStream is the streaming half.
//
// The same misconfiguration answers a streaming request with the same JSON
// object and no SSE framing at all, so the relay reads zero frames. The client
// must not be handed an empty 200 that ends cleanly, and the attempt must reach
// the metering seam as the failure it is.
func TestUpstream200WithANonResponseBodyIsNotAnEmptyStream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream catalog.API
		client   catalog.API
	}{
		{"byte relay", catalog.APIOpenAIChat, catalog.APIOpenAIChat},
		{"crossing", catalog.APIOpenAIChat, catalog.APIAnthropicMessages},
		{"messages upstream", catalog.APIAnthropicMessages, catalog.APIOpenAIChat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.setHandler(func(w http.ResponseWriter, r *http.Request) {
				// No text/event-stream label and no frames: the vendor answers a
				// streaming request exactly as it answers a non-streaming one.
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, zaiNotFound)
			})
			p := testProvider(t, f, "openai", tc.upstream)
			c := chatCall(tc.client)
			c.Stream = true
			obs := &countingObserver{}
			rec := httptest.NewRecorder()

			res := observedBackend(obs).Do(context.Background(), target(p), c, rec)

			if res.Err == nil {
				t.Fatalf("the stream succeeded; the client received %q and a 200",
					rec.Body.String())
			}
			if len(obs.seen) != 1 {
				t.Errorf("the metering seam saw %d failures, want 1", len(obs.seen))
			}
			if res.FirstByteSent {
				t.Error("FirstByteSent is set for a stream that wrote nothing; " +
					"§7.6 fallback is closed for no reason")
			}
			if !res.Retryable {
				t.Error("a stream that committed no bytes must stay retryable")
			}
			if body := rec.Body.String(); strings.Contains(body, "data:") {
				t.Errorf("the client was sent SSE frames for a body that had none: %q", body)
			}
		})
	}
}

// TestUpstream200WithANonResponseBodyOnARelaySurface.
//
// Probing for the same class rather than reading it off a report: the relay
// surfaces had the identical defect, and on them it is worse. A converted
// answer at least becomes an EMPTY answer; a relayed one is handed to the
// client whole, with dorang's own `model` member spliced into the vendor's
// error envelope, over a 200.
func TestUpstream200WithANonResponseBodyOnARelaySurface(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  catalog.API
		call *Call
	}{
		{
			name: "openai-shaped embeddings", api: catalog.APIOpenAIChat,
			call: &Call{Op: OpEmbeddings, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
				Body: []byte(`{"model":"client-model","input":"hello"}`)},
		},
		{
			name: "jina embeddings", api: catalog.APIJina,
			call: &Call{Op: OpEmbeddings, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
				Body: []byte(`{"model":"client-model","input":"hello"}`)},
		},
		{
			name: "messages count_tokens", api: catalog.APIAnthropicMessages,
			call: func() *Call {
				c := chatCall(catalog.APIAnthropicMessages)
				c.Op = OpCountTokens
				return c
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(http.StatusOK, zaiNotFound)
			p := testProvider(t, f, "openai", tc.api)

			res := testBackend("k").Do(context.Background(), target(p), tc.call, nil)
			if res.Err == nil {
				t.Fatalf("the exchange succeeded and relayed %s to the client", res.Body)
			}
			if res.Err.Code != CodeUpstreamShape {
				t.Errorf("code = %q, want %q", res.Err.Code, CodeUpstreamShape)
			}
			if res.Err.Status != http.StatusBadGateway {
				t.Errorf("status = %d, want 502", res.Err.Status)
			}
		})
	}
}

// TestRelaySurfacesStillAcceptTheirOwnAnswers is the boundary check for the
// relay guard: the members it requires are the ones every real answer has.
func TestRelaySurfacesStillAcceptTheirOwnAnswers(t *testing.T) {
	t.Run("an embeddings answer with an empty data array", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.answer(http.StatusOK, `{"object":"list","model":"upstream-model","data":[]}`)
		p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
		c := &Call{Op: OpEmbeddings, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
			Body: []byte(`{"model":"client-model","input":[]}`)}
		if res := testBackend("k").Do(context.Background(), target(p), c, nil); res.Err != nil {
			t.Fatalf("a valid empty embeddings answer was refused: %v", res.Err.Message)
		}
	})

	t.Run("a token count of zero", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.answer(http.StatusOK, `{"input_tokens":0}`)
		p := testProvider(t, f, "anthropic", catalog.APIAnthropicMessages)
		c := chatCall(catalog.APIAnthropicMessages)
		c.Op = OpCountTokens
		if res := testBackend("k").Do(context.Background(), target(p), c, nil); res.Err != nil {
			t.Fatalf("a measured zero was refused: %v", res.Err.Message)
		}
	})
}

// TestAMinimalResponseIsStillAResponse is the other half of the D2 boundary,
// and the reason the shape test accepts on EITHER ground rather than requiring
// both.
//
// Every body here is a legitimate answer that a real backend sends, and a check
// tightened one notch further would refuse each of them: an empty turn, a
// backend that omits the discriminator, a backend that sends only usage.
func TestAMinimalResponseIsStillAResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream catalog.API
		body     string
	}{
		{
			// vLLM answering a probe. The existing TestNoCredentialIsNotAnError
			// depends on this one.
			name: "chat completion with an empty choices array", upstream: catalog.APIOpenAIChat,
			body: `{"id":"1","object":"chat.completion","model":"m","choices":[]}`,
		},
		{
			name: "chat completion with no object member", upstream: catalog.APIOpenAIChat,
			body: `{"id":"1","model":"m","choices":[{"index":0,` +
				`"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
		},
		{
			name: "chat completion carrying only usage", upstream: catalog.APIOpenAIChat,
			body: `{"id":"1","model":"m","usage":{"prompt_tokens":3,"completion_tokens":0}}`,
		},
		{
			name: "a message with empty content", upstream: catalog.APIAnthropicMessages,
			body: `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
				`"content":[],"stop_reason":"end_turn"}`,
		},
		{
			name: "a message with no type member", upstream: catalog.APIAnthropicMessages,
			body: `{"id":"msg_1","role":"assistant","model":"m",` +
				`"content":[{"type":"text","text":"hi"}]}`,
		},
		{
			name: "a message carrying only usage", upstream: catalog.APIAnthropicMessages,
			body: `{"id":"msg_1","model":"m","usage":{"input_tokens":3,"output_tokens":0}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(http.StatusOK, tc.body)
			p := testProvider(t, f, "openai", tc.upstream)

			res := testBackend("k").Do(context.Background(), target(p), chatCall(catalog.APIOpenAIChat), nil)
			if res.Err != nil {
				t.Fatalf("a valid minimal response was refused: %v (%s)", res.Err.Message, res.Err.Code)
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(res.Body, &obj); err != nil {
				t.Fatalf("the rendered answer is not a JSON object: %v", err)
			}
		})
	}
}
