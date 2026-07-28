package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
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
		// This family sends no terminator frame at all, so "the stream ended"
		// and "the stream ended correctly" are the same event to its reader.
		// The generic no-frames guard is the only thing separating them.
		{"gemini upstream", catalog.APIGemini, catalog.APIOpenAIChat},
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

// ---------------------------------------------------------------------------
// The rest of the class: the T1 surfaces, rerank and Gemini
// ---------------------------------------------------------------------------

// Minimal calls for the surfaces the first pass did not reach. Each is the
// smallest request its operation accepts; none of them matters to the assertion,
// because the defect is entirely on the answer side.

func moderationCall() *Call {
	return &Call{
		Op: OpModerations, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
		Moderation: &canonical.ModerationRequest{
			Model:  "client-model",
			Inputs: []canonical.ModerationInput{{Kind: canonical.ModerationText, Text: "hello"}},
		},
	}
}

func speechCall() *Call {
	return &Call{
		Op: OpSpeech, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
		Speech: &canonical.SpeechRequest{Model: "client-model", Input: "hello", Voice: "alloy"},
	}
}

func transcriptionCall() *Call {
	return &Call{
		Op: OpTranscription, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
		Transcription: &canonical.TranscriptionRequest{
			Model: "client-model",
			File:  canonical.File{Field: "file", Name: "a.mp3", Data: []byte{0x00, 0x01, 0x02}},
		},
	}
}

func imageCall() *Call {
	return &Call{
		Op: OpImageGenerate, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
		Image: &canonical.ImageRequest{
			Op: canonical.ImageGenerate, Model: "client-model", Prompt: "a cat",
		},
	}
}

func rerankCall() *Call {
	return &Call{
		Op: OpRerank, ClientAPI: catalog.APIOpenAIChat, Model: "client-model",
		Rerank: &canonical.RerankRequest{
			Model: "client-model", Query: "q",
			Documents: []canonical.RerankDocument{{Text: "d"}},
		},
	}
}

// TestUpstream200WithANonResponseBodyOnEveryRemainingSurface closes the class.
//
// The first pass fixed the two surfaces a live probe happened to cover. These
// are the ones its case table never reached, and every one of them had the same
// defect: the vendor's 200-wrapped error envelope unmarshalled into an
// all-optional struct, and the zero value was rendered as a successful answer.
//
// The assertion is what the CLIENT receives, not what a decoder returns. On
// these surfaces that distinction has teeth beyond the chat ones: the wire types
// all carry an `Extra` map for members dorang does not model, so the vendor's
// own `code`/`msg`/`success` members were re-serialized INTO the answer
// alongside dorang's spliced `model`. That is the relay failure mode reached
// through a converting path — the client got the vendor's error envelope whole,
// wearing dorang's fingerprints, over a 200.
func TestUpstream200WithANonResponseBodyOnEveryRemainingSurface(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  catalog.API
		call *Call
	}{
		{"moderations", catalog.APIOpenAIChat, moderationCall()},
		{"image generation", catalog.APIOpenAIChat, imageCall()},
		{"transcription", catalog.APIOpenAIChat, transcriptionCall()},
		{"speech", catalog.APIOpenAIChat, speechCall()},
		{"rerank, generic dialect", catalog.APIOpenAIChat, rerankCall()},
		{"rerank, jina dialect", catalog.APIJina, rerankCall()},
		{"rerank, cohere dialect", catalog.APICohere, rerankCall()},
		{"gemini generateContent", catalog.APIGemini, chatCall(catalog.APIOpenAIChat)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(http.StatusOK, zaiNotFound)
			p := testProvider(t, f, "openai", tc.api)

			res := testBackend("k").Do(context.Background(), target(p), tc.call, nil)

			if res.Err == nil {
				t.Fatalf("the exchange succeeded and handed the client %q;\n"+
					"a vendor error body wearing a 200 must not become an answer", res.Body)
			}
			if res.Body != nil {
				t.Errorf("Body = %q, want nothing rendered", res.Body)
			}
			if got := string(res.Body); strings.Contains(got, "404 NOT_FOUND") {
				t.Errorf("the vendor's error envelope reached the client: %q", got)
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

// TestSpeechRelaysBytesAndRefusesAnErrorEnvelope is the speech surface stated on
// its own, because its gate cannot be the JSON rule the others use.
//
// A speech answer is an audio container: bytes, with no members to be present or
// absent and no discriminator to read. Asking "is this a response of this
// family" of an MP3 has no JSON answer at all, so the test is the pair the
// surface does have — the upstream did not label the body as JSON, and there are
// bytes to play. Nothing weaker distinguishes an error envelope from audio;
// nothing stronger can be applied without parsing a container format, which
// would refuse every codec dorang has not heard of.
//
// Getting this wrong is not the quiet empty answer the JSON surfaces produced.
// The relay stamps dorang's OWN content type on whatever came back, derived from
// the container the caller asked for, so the client is handed a JSON error
// envelope labelled `audio/mpeg` — and a browser, a player or an `ffmpeg` in a
// pipeline fails on it far from the gateway that produced it.
func TestSpeechRelaysBytesAndRefusesAnErrorEnvelope(t *testing.T) {
	t.Run("an error envelope labelled JSON is not audio", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, zaiNotFound)
		})
		p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

		res := testBackend("k").Do(context.Background(), target(p), speechCall(), nil)
		if res.Err == nil {
			t.Fatalf("the client was handed %q as %s", res.Body, res.ContentType)
		}
		if res.Err.Code != CodeUpstreamShape {
			t.Errorf("code = %q, want %q", res.Err.Code, CodeUpstreamShape)
		}
	})

	t.Run("an unlabelled error envelope is not audio", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			// Self-hosted engines routinely answer application/octet-stream, so
			// the label alone cannot decide. A body that is a whole JSON object
			// is not an audio container.
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, zaiNotFound)
		})
		p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

		res := testBackend("k").Do(context.Background(), target(p), speechCall(), nil)
		if res.Err == nil {
			t.Fatalf("the client was handed %q as %s", res.Body, res.ContentType)
		}
	})

	t.Run("an empty body is not audio", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.answer(http.StatusOK, "")
		p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

		res := testBackend("k").Do(context.Background(), target(p), speechCall(), nil)
		if res.Err == nil {
			t.Fatalf("zero bytes of audio were served as a successful answer, as %s", res.ContentType)
		}
	})

	t.Run("audio the backend mislabelled is still audio", func(t *testing.T) {
		// The whole reason the gate is not "the label must say audio": an
		// engine that answers application/octet-stream is the ordinary case,
		// and refusing it would break every self-hosted speech deployment.
		f := newFakeUpstream(t)
		f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte{0xff, 0xfb, 0x90, 0x64, 0x00})
		})
		p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

		res := testBackend("k").Do(context.Background(), target(p), speechCall(), nil)
		if res.Err != nil {
			t.Fatalf("a mislabelled but real audio body was refused: %v", res.Err.Message)
		}
		if res.ContentType != "audio/mpeg" {
			t.Errorf("ContentType = %q, want the container the caller asked for", res.ContentType)
		}
	})
}

// TestTranscriptionRefusesAnErrorEnvelopeOnBothOfItsPaths.
//
// This surface has two answer paths and the defect was on both, which is why the
// gate runs before the split rather than inside the JSON branch.
//
// The JSON path is the ordinary one. The RAW path is a byte relay — a transcript
// asked for as srt, vtt or text is carried through verbatim — and it is reached
// by more traffic than it looks: [openai.DecodeTranscriptionResponse] trusts a
// media type that clearly says something other than JSON, and a body with no
// Content-Type at all is labelled `text/plain` by content sniffing before dorang
// ever sees it. So a misrouted upstream's error envelope arrived on the raw path
// and was served to the client AS THE TRANSCRIPT.
func TestTranscriptionRefusesAnErrorEnvelopeOnBothOfItsPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ctype string
	}{
		{"the JSON path", "application/json"},
		{"the raw path, reached by content sniffing", "text/plain; charset=utf-8"},
		{"the raw path, labelled as a subtitle file", "text/vtt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				_, _ = io.WriteString(w, zaiNotFound)
			})
			p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

			res := testBackend("k").Do(context.Background(), target(p), transcriptionCall(), nil)
			if res.Err == nil {
				t.Fatalf("the client was handed %q as the transcript", res.Body)
			}
			if res.Err.Code != CodeUpstreamShape {
				t.Errorf("code = %q, want %q", res.Err.Code, CodeUpstreamShape)
			}
		})
	}
}

// TestARawTranscriptIsNotCheckedForBeingEmpty pins the asymmetry with speech.
//
// Silence transcribes to an empty string, so zero bytes is a CORRECT answer on
// this surface — the exact opposite of the speech relay, where zero bytes is not
// a container. A gate that refused an empty body here would turn every silent
// recording into a 502.
func TestARawTranscriptIsNotCheckedForBeingEmpty(t *testing.T) {
	for _, tc := range []struct{ name, ctype, body string }{
		{"a transcript of silence", "text/plain", ""},
		{"an srt file", "application/x-subrip",
			"1\n00:00:00,000 --> 00:00:01,000\nhello\n"},
		{"a vtt file with no cues", "text/vtt", "WEBVTT\n\n"},
		{"a JSON transcript the backend mislabelled as text", "text/plain", `{"text":"hi"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				_, _ = io.WriteString(w, tc.body)
			})
			p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

			res := testBackend("k").Do(context.Background(), target(p), transcriptionCall(), nil)
			if res.Err != nil {
				t.Fatalf("a valid raw transcript was refused: %v (%s)", res.Err.Message, res.Err.Code)
			}
		})
	}
}

// TestAMinimalT1OrRerankResponseIsStillAResponse is the boundary check for the
// gates above, and the reason each accepts on EITHER ground rather than both.
//
// Every body here is a legitimate answer a real backend sends. A gate tightened
// one notch — requiring a non-empty result set, or requiring a discriminator
// three of these four surfaces do not even define — turns each of them into a
// 502, which is a worse failure than the one being fixed because it breaks
// traffic that works today.
func TestAMinimalT1OrRerankResponseIsStillAResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  catalog.API
		call *Call
		body string
	}{
		{
			// An empty input array is a legal moderations request, and its
			// answer has an empty result set.
			name: "a moderation answer with an empty results array",
			api:  catalog.APIOpenAIChat, call: moderationCall(),
			body: `{"id":"modr-1","model":"m","results":[]}`,
		},
		{
			name: "a moderation answer with no id or model",
			api:  catalog.APIOpenAIChat, call: moderationCall(),
			body: `{"results":[{"flagged":false,"categories":{"hate":false},` +
				`"category_scores":{"hate":0.0001}}]}`,
		},
		{
			// A flagged generation returns no image but is still billed, and
			// `created` alone is not what makes this a response — `usage` is.
			name: "an image answer carrying only usage",
			api:  catalog.APIOpenAIChat, call: imageCall(),
			body: `{"created":1,"usage":{"input_tokens":9,"output_tokens":0,"total_tokens":9}}`,
		},
		{
			name: "an image answer with no created member",
			api:  catalog.APIOpenAIChat, call: imageCall(),
			body: `{"data":[{"b64_json":"aGk="}]}`,
		},
		{
			// Silence transcribes to an empty string. The key is present; its
			// value is not what is being tested.
			name: "a transcript of silence",
			api:  catalog.APIOpenAIChat, call: transcriptionCall(),
			body: `{"text":""}`,
		},
		{
			name: "a verbose transcript with no text member",
			api:  catalog.APIOpenAIChat, call: transcriptionCall(),
			body: `{"task":"transcribe","language":"en","duration":1.5,"segments":[]}`,
		},
		{
			// TEI and Infinity send results and nothing else — no id, no model,
			// no billing block of either spelling.
			name: "a rerank answer with results only",
			api:  catalog.APIOpenAIChat, call: rerankCall(),
			body: `{"results":[{"index":0,"relevance_score":0.9}]}`,
		},
		{
			name: "a rerank answer over an empty corpus",
			api:  catalog.APIJina, call: rerankCall(),
			body: `{"model":"m","results":[],"usage":{"total_tokens":4}}`,
		},
		{
			name: "a cohere rerank answer, which has no discriminator at all",
			api:  catalog.APICohere, call: rerankCall(),
			body: `{"id":"r-1","results":[{"index":0,"relevance_score":0.9}],` +
				`"meta":{"billed_units":{"search_units":1}}}`,
		},
		{
			// A blocked prompt IS an answer of this family: no candidates at
			// all, only the feedback that says why. Refusing it would report a
			// working safety filter as a broken gateway.
			name: "a gemini answer with no candidates, only prompt feedback",
			api:  catalog.APIGemini, call: chatCall(catalog.APIOpenAIChat),
			body: `{"promptFeedback":{"blockReason":"SAFETY"},"modelVersion":"m",` +
				`"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7}}`,
		},
		{
			name: "a gemini answer with an empty candidates array",
			api:  catalog.APIGemini, call: chatCall(catalog.APIOpenAIChat),
			body: `{"candidates":[],"modelVersion":"m"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(http.StatusOK, tc.body)
			p := testProvider(t, f, "openai", tc.api)

			res := testBackend("k").Do(context.Background(), target(p), tc.call, nil)
			if res.Err != nil {
				t.Fatalf("a valid minimal response was refused: %v (%s)", res.Err.Message, res.Err.Code)
			}
		})
	}
}
