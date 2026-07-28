package backend

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// The five ways an upstream can fail a stream it already answered 200 to, in
// the two protocol shapes, and what the relay must make of each.
//
// Every one of them used to be reported as a success. The tests below assert
// the consequence rather than the return value wherever the consequence is
// reachable from this package: an observed failure at the metering seam, a
// circuit that opens, a client that is not handed a truncated answer labelled
// complete.

// oaFrames renders an OpenAI-shaped stream from its data payloads.
func oaFrames(payloads ...string) string {
	var b strings.Builder
	for _, p := range payloads {
		b.WriteString("data: ")
		b.WriteString(p)
		b.WriteString("\n\n")
	}
	return b.String()
}

const (
	oaChunkAlpha = `{"id":"1","object":"chat.completion.chunk","created":1,"model":"upstream-model",` +
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"alpha"}}]}`
	oaChunkBeta = `{"id":"1","object":"chat.completion.chunk","created":1,"model":"upstream-model",` +
		`"choices":[{"index":0,"delta":{"content":" beta"}}]}`
	oaErrorFrame = `{"error":{"message":"upstream failed mid-stream","type":"api_error",` +
		`"param":null,"code":"internal_error"}}`

	antStartFrame = "event: message_start\ndata: " +
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant",` +
		`"model":"upstream-model","content":[],"stop_reason":null,"stop_sequence":null,` +
		`"usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\ndata: " +
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"
	antDeltaFrame = "event: content_block_delta\ndata: " +
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"alpha"}}` + "\n\n"
	antErrorFrame = "event: error\ndata: " +
		`{"type":"error","error":{"type":"api_error","message":"upstream failed mid-stream",` +
		`"code":"internal_error"}}` + "\n\n"
)

// streamUpstream answers one streaming request with a fixed body.
func streamUpstream(t *testing.T, body string) *fakeUpstream {
	t.Helper()
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	})
	return f
}

// countingObserver is the §12.4 metering seam, recorded.
type countingObserver struct{ seen []Failure }

func (o *countingObserver) UpstreamFailure(f Failure) { o.seen = append(o.seen, f) }

// observedBackend is [testBackend] with the observer attached.
func observedBackend(o Observer) *Backend {
	return New(Options{
		Credentials: staticCredentials{secrets: map[string]string{"c1": "k"}},
		Client:      NewClient(),
		Now:         time.Now,
		Observer:    o,
	})
}

// TestMidStreamFailureReachesTheMeteringSeam is the closest observable this
// package owns.
//
// [Observer.UpstreamFailure] is documented as receiving "one record per FAILED
// upstream attempt", and it is the seam DESIGN §12.4 meters from. It fires off
// res.Err and nothing else, so a relay that returned nil made a failed attempt
// unmeterable — which is the same nil that made it invisible to internal/health
// and to the fail-back boundary, one layer further out.
//
// All four crossings are covered because the relay has two entirely separate
// implementations — the byte scanner and the neutral event path — and each one
// had its own reason for missing the failure.
func TestMidStreamFailureReachesTheMeteringSeam(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream catalog.API
		client   catalog.API
		body     string
		code     string
	}{
		{
			name: "byte relay, in-band error frame", upstream: catalog.APIOpenAIChat,
			client: catalog.APIOpenAIChat,
			body:   oaFrames(oaChunkAlpha, oaChunkBeta, oaErrorFrame, "[DONE]"),
			code:   CodeUpstreamStreamError,
		},
		{
			name: "byte relay, truncated", upstream: catalog.APIOpenAIChat,
			client: catalog.APIOpenAIChat,
			body:   oaFrames(oaChunkAlpha, oaChunkBeta),
			code:   CodeUpstreamStreamTruncated,
		},
		{
			// The crossing path decoded this frame to zero events and stepped
			// over it to the [DONE] behind it, so the failure did not merely go
			// unreported — it went unseen.
			name: "crossing, in-band error frame", upstream: catalog.APIOpenAIChat,
			client: catalog.APIAnthropicMessages,
			body:   oaFrames(oaChunkAlpha, oaErrorFrame, "[DONE]"),
			code:   CodeUpstreamStreamError,
		},
		{
			name: "crossing, truncated", upstream: catalog.APIOpenAIChat,
			client: catalog.APIAnthropicMessages,
			body:   oaFrames(oaChunkAlpha, oaChunkBeta),
			code:   CodeUpstreamStreamTruncated,
		},
		{
			name: "messages upstream, in-band error frame", upstream: catalog.APIAnthropicMessages,
			client: catalog.APIOpenAIChat,
			body:   antStartFrame + antDeltaFrame + antErrorFrame,
			code:   CodeUpstreamStreamError,
		},
		{
			name: "messages upstream, truncated", upstream: catalog.APIAnthropicMessages,
			client: catalog.APIOpenAIChat,
			body:   antStartFrame + antDeltaFrame,
			code:   CodeUpstreamStreamTruncated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := streamUpstream(t, tc.body)
			p := testProvider(t, f, "openai", tc.upstream)
			c := chatCall(tc.client)
			c.Stream = true
			obs := &countingObserver{}
			rec := httptest.NewRecorder()

			res := observedBackend(obs).Do(context.Background(), target(p), c, rec)

			if len(obs.seen) != 1 {
				t.Fatalf("the metering seam saw %d failures, want 1: the attempt was reported "+
					"as a success and neither the meter nor internal/health can count it",
					len(obs.seen))
			}
			if obs.seen[0].Code != tc.code {
				t.Errorf("Failure.Code = %q, want %q", obs.seen[0].Code, tc.code)
			}
			if obs.seen[0].Status != http.StatusOK {
				t.Errorf("Failure.Status = %d, want 200: the upstream answered before it failed",
					obs.seen[0].Status)
			}
			// The client keeps everything it already had. §7.6's boundary is
			// about what dorang records, not about un-sending bytes.
			if n := strings.Count(rec.Body.String(), "alpha"); n != 1 {
				t.Errorf("the content the client had already received appears %d times, want 1:\n%s",
					n, rec.Body.String())
			}
			if !res.FirstByteSent {
				t.Error("FirstByteSent is false after content reached the client; §7.6 would permit " +
					"a fail-back that duplicates output")
			}
		})
	}
}

// TestTruncatedStreamIsNotDressedAsCompletion asserts what the CLIENT receives.
//
// This is the worst of the group and the only one no client can detect for
// itself: an Anthropic stream cut off mid-generation reached an OpenAI-speaking
// caller carrying a synthesized finish_reason of "stop" and a [DONE], which is
// the wire form of "here is your complete answer". COMPATIBILITY §4.4 licenses
// synthesizing that terminal for a stream that ENDED without one. A stream that
// broke did not end.
func TestTruncatedStreamIsNotDressedAsCompletion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream catalog.API
		client   catalog.API
		body     string
		banned   []string
		want     string
	}{
		{
			name: "messages upstream to a chat client", upstream: catalog.APIAnthropicMessages,
			client: catalog.APIOpenAIChat,
			body:   antStartFrame + antDeltaFrame,
			banned: []string{`"finish_reason":"stop"`},
			want:   CodeUpstreamStreamTruncated,
		},
		{
			name: "chat upstream to a messages client", upstream: catalog.APIOpenAIChat,
			client: catalog.APIAnthropicMessages,
			body:   oaFrames(oaChunkAlpha),
			banned: []string{`"stop_reason":"end_turn"`, "event: message_stop"},
			want:   CodeUpstreamStreamTruncated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := streamUpstream(t, tc.body)
			p := testProvider(t, f, "openai", tc.upstream)
			c := chatCall(tc.client)
			c.Stream = true
			rec := httptest.NewRecorder()

			res := observedBackend(nil).Do(context.Background(), target(p), c, rec)
			body := rec.Body.String()

			for _, bad := range tc.banned {
				if strings.Contains(body, bad) {
					t.Errorf("a stream that was cut off mid-generation reached the client carrying "+
						"%s — a half answer presented as a whole one:\n%s", bad, body)
				}
			}
			// And it is told, in band, because the status is spent
			// (COMPATIBILITY 1.3).
			if !strings.Contains(body, tc.want) {
				t.Errorf("the client was not told the stream broke:\n%s", body)
			}
			if res.Err == nil || res.Err.Code != tc.want {
				t.Errorf("Result.Err = %v, want code %q", res.Err, tc.want)
			}
		})
	}
}

// TestEmptyStreamCommitsNothing is FirstByteSent meaning what it says.
//
// An upstream that answers 200 and then writes nothing at all (COMPATIBILITY
// 1.4) used to set FirstByteSent unconditionally, because the flag was set from
// the fact that the call was a stream rather than from anything that was
// written. §7.6 correctly refuses to fail back once the client has seen output,
// so the flag closed the only escape from a deployment that produced no output
// at all: nothing to serve, and nowhere else to go.
func TestEmptyStreamCommitsNothing(t *testing.T) {
	for _, api := range []catalog.API{catalog.APIOpenAIChat, catalog.APIAnthropicMessages} {
		t.Run(string(api), func(t *testing.T) {
			f := streamUpstream(t, "")
			p := testProvider(t, f, "openai", api)
			c := chatCall(catalog.APIOpenAIChat)
			c.Stream = true
			rec := httptest.NewRecorder()

			res := observedBackend(nil).Do(context.Background(), target(p), c, rec)

			if rec.Body.Len() != 0 {
				t.Errorf("the client received %d bytes from an empty upstream stream:\n%s",
					rec.Body.Len(), rec.Body.String())
			}
			if res.FirstByteSent {
				t.Error("FirstByteSent is set on a stream that wrote nothing: the client has seen " +
					"no output, and §7.6 has nothing to protect")
			}
			if res.Err == nil {
				t.Fatal("an upstream that answered nothing was recorded as having answered")
			}
			if !res.Retryable {
				t.Error("nothing was committed, so another deployment may still be tried")
			}
		})
	}
}

// TestMidStreamFailureOpensTheCircuit is the consequence the whole change
// exists for: a deployment that fails every stream after the first frame must
// stop being selected.
//
// internal/health is fed by internal/router's Report, which internal/app calls
// with the outcome its backendResult derives from this Result — a chain this
// package cannot reach from a test. What it can reach is the one fact that
// chain turns on: a 200-status stream with Result.Err set classifies as
// CauseUpstream5xx (internal/app's backendCause: no 4xx, no timeout, Status >
// 0), and CauseUpstream5xx is one of the two causes
// internal/router.countsAgainstAvailability admits. The two lines below are
// that derivation, and nothing else about the chain is assumed.
func TestMidStreamFailureOpensTheCircuit(t *testing.T) {
	f := streamUpstream(t, oaFrames(oaChunkAlpha, oaChunkBeta, oaErrorFrame, "[DONE]"))
	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	tr := health.New(health.Options{FailureThreshold: 3, Cooldown: time.Minute})

	for i := range 3 {
		if !tr.Allow("d1") {
			t.Fatalf("the deployment was already unselectable before request %d", i+1)
		}
		c := chatCall(catalog.APIOpenAIChat)
		c.Stream = true
		res := observedBackend(nil).Do(context.Background(), target(p), c, httptest.NewRecorder())
		tr.Report("d1", health.Outcome{
			Err:     res.Err,
			Failure: res.Err != nil,
			TTFT:    res.TTFT, Total: res.Total,
		})
	}

	if st := tr.Stats("d1"); st.Failures != 3 {
		t.Errorf("health counted %d failures over three failed streams, want 3", st.Failures)
	}
	if tr.Allow("d1") {
		t.Error("a deployment that failed three streams in a row is still taking its full share " +
			"of traffic: the circuit breaker cannot see the failure it exists to see")
	}
}

// TestCleanStreamsAreStillClean is the other half of every assertion above: a
// detector that fires on a healthy stream would open the circuit on a working
// backend, which is a worse outage than the one it was built to catch.
func TestCleanStreamsAreStillClean(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream catalog.API
		client   catalog.API
		body     string
	}{
		{
			// COMPATIBILITY §4.4's actual case: an ending with no
			// finish_reason, but with the terminator the family defines.
			name: "no finish_reason, but [DONE] arrived", upstream: catalog.APIOpenAIChat,
			client: catalog.APIOpenAIChat,
			body:   oaFrames(oaChunkAlpha, "[DONE]"),
		},
		{
			name: "crossing, no finish_reason, but [DONE] arrived", upstream: catalog.APIOpenAIChat,
			client: catalog.APIAnthropicMessages,
			body:   oaFrames(oaChunkAlpha, "[DONE]"),
		},
		{
			// A model writing about error handling. The word is in the frame;
			// there is no error member, and the stream is fine.
			name: "content that talks about errors", upstream: catalog.APIOpenAIChat,
			client: catalog.APIOpenAIChat,
			body: oaFrames(`{"id":"1","object":"chat.completion.chunk","created":1,`+
				`"model":"upstream-model","choices":[{"index":0,"delta":{"content":`+
				`"use the \"error\" member of the envelope"}}]}`, "[DONE]"),
		},
		{
			name: "messages upstream, complete", upstream: catalog.APIAnthropicMessages,
			client: catalog.APIOpenAIChat,
			body: antStartFrame + antDeltaFrame +
				"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
				"event: message_delta\ndata: " +
				`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},` +
				`"usage":{"output_tokens":5}}` + "\n\n" +
				"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := streamUpstream(t, tc.body)
			p := testProvider(t, f, "openai", tc.upstream)
			c := chatCall(tc.client)
			c.Stream = true
			obs := &countingObserver{}

			res := observedBackend(obs).Do(context.Background(), target(p), c, httptest.NewRecorder())
			if res.Err != nil {
				t.Errorf("a complete stream was reported as a failure: %v", res.Err)
			}
			if len(obs.seen) != 0 {
				t.Errorf("the metering seam counted %d failures on a complete stream", len(obs.seen))
			}
		})
	}
}

// TestInBandErrorDoesNotEchoTheCredential is DESIGN §10.6 rule 4 in the
// direction it was only half implemented.
//
// The 4xx/5xx path scrubs the upstream's body before anything renders it. The
// STREAMING path scrubbed nothing: an upstream that answers mid-stream with its
// own diagnostic — "invalid x-api-key: sk-…" is a real message from a real
// server — had that sentence re-encoded into the client's error frame with the
// operator's provider credential inside it. No attacker is required; a helpful
// error message is enough.
//
// The same-family BYTE relay is covered too, and it is the one that took a
// design decision to reach: the detector used to write the bytes and then look
// at them, so the credential was already on the wire before anything had an
// opinion. It now inspects a line before forwarding it — see [relayWatch].
func TestInBandErrorDoesNotEchoTheCredential(t *testing.T) {
	const leaked = "sk-CANARY-relay-9f3b21" // pragma: allowlist secret — test fixture

	for _, tc := range []struct {
		name     string
		upstream catalog.API
		client   catalog.API
		body     string
	}{
		{
			// The fast path: no decoding, no crossing, bytes only.
			name: "byte relay", upstream: catalog.APIOpenAIChat,
			client: catalog.APIOpenAIChat,
			body: oaFrames(oaChunkAlpha,
				`{"error":{"message":"invalid api key: `+leaked+`","type":"api_error",`+
					`"param":null,"code":"invalid_api_key"}}`, "[DONE]"),
		},
		{
			// SGLang's flat envelope, which has no nested error member at all.
			name: "byte relay, flat envelope", upstream: catalog.APIOpenAIChat,
			client: catalog.APIOpenAIChat,
			body: oaFrames(oaChunkAlpha,
				`{"object":"error","message":"invalid api key: `+leaked+`","code":401}`),
		},
		{
			name: "chat upstream to a messages client", upstream: catalog.APIOpenAIChat,
			client: catalog.APIAnthropicMessages,
			body: oaFrames(oaChunkAlpha,
				`{"error":{"message":"invalid api key: `+leaked+`","type":"api_error",`+
					`"param":null,"code":"invalid_api_key"}}`, "[DONE]"),
		},
		{
			name: "messages upstream to a chat client", upstream: catalog.APIAnthropicMessages,
			client: catalog.APIOpenAIChat,
			body: antStartFrame + antDeltaFrame + "event: error\ndata: " +
				`{"type":"error","error":{"type":"api_error","message":"invalid x-api-key: ` +
				leaked + `"}}` + "\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := streamUpstream(t, tc.body)
			p := testProvider(t, f, "openai", tc.upstream)
			c := chatCall(tc.client)
			c.Stream = true
			rec := httptest.NewRecorder()

			b := New(Options{
				Credentials: staticCredentials{secrets: map[string]string{"c1": leaked}},
				Client:      NewClient(),
				Now:         time.Now,
			})
			res := b.Do(context.Background(), target(p), c, rec)

			if strings.Contains(rec.Body.String(), leaked) {
				t.Errorf("the provider credential was echoed to the client in an in-band error "+
					"frame:\n%s", rec.Body.String())
			}
			if res.Err != nil && strings.Contains(res.Err.NativeMessage, leaked) {
				t.Errorf("the provider credential was recorded in the failure: %q",
					res.Err.NativeMessage)
			}
		})
	}
}

// TestByteRelayStaysByteFaithful is the other side of the scrubber's bargain.
//
// [relayWatch] now holds a line before forwarding it, so the fast path's
// defining property has to be asserted rather than assumed: everything that is
// not an echoed credential leaves exactly as it arrived, in the same order,
// however the upstream chose to chop it up.
func TestByteRelayStaysByteFaithful(t *testing.T) {
	// A frame that talks about errors, a frame with an "error" member that is
	// null, an oversized line the detector must give up on rather than buffer,
	// and the terminator.
	long := strings.Repeat("y", maxStreamFrame+512)
	stream := oaFrames(
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m",`+
			`"choices":[{"index":0,"delta":{"content":"the \"error\" member is null here"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","error":null,`+
			`"choices":[{"index":0,"delta":{"content":"still fine"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m",`+
			`"choices":[{"index":0,"delta":{"content":"`+long+`"}}]}`,
		"[DONE]",
	)

	for _, chunk := range []int{1, 7, 4096, len(stream)} {
		t.Run("upstream writes "+strconv.Itoa(chunk)+" bytes at a time", func(t *testing.T) {
			var got bytes.Buffer
			w := &relayWatch{w: &got, secrets: []string{"sk-nothing-here"}}
			for i := 0; i < len(stream); i += chunk {
				j := min(i+chunk, len(stream))
				if n, err := w.Write([]byte(stream[i:j])); err != nil || n != j-i {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			if err := w.flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if got.String() != stream {
				t.Errorf("the relay altered a stream that carried nothing to alter "+
					"(%d bytes out, %d in)", got.Len(), len(stream))
			}
			if !w.done {
				t.Error("the terminator was not seen")
			}
			if w.frame != nil {
				t.Errorf("a frame that merely mentions errors was taken for one: %s", w.frame)
			}
		})
	}
}

// TestByteRelayHoldsOnlyOneLine bounds what the scrubber may accumulate.
//
// The hold-back is what lets an echoed credential be caught before it is on the
// wire, and it is also the one place this type can be made to grow: an upstream
// that opens `data: ` and then never sends a newline is on the far side of the
// trust boundary. maxStreamFrame is the ceiling, and past it the line is
// forwarded uninspected rather than buffered — the same degradation
// internal/wire/openai's Scanner applies for the same reason.
func TestByteRelayHoldsOnlyOneLine(t *testing.T) {
	var got bytes.Buffer
	w := &relayWatch{w: &got}
	const step = 64 << 10
	for sent := 0; sent < maxStreamFrame+(1<<20); sent += step {
		if _, err := w.Write(bytes.Repeat([]byte("z"), step)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if len(w.line) > maxStreamFrame {
			t.Fatalf("the detector is holding %d bytes of one line, over the %d ceiling",
				len(w.line), maxStreamFrame)
		}
	}
	if !w.over {
		t.Error("a line past the ceiling did not degrade to pass-through")
	}
	if got.Len() == 0 {
		t.Error("the held bytes were dropped rather than forwarded")
	}
}

// BenchmarkByteRelay measures what the detector costs the fast path, against the
// same stream written straight through.
func BenchmarkByteRelay(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 512; i++ {
		sb.WriteString(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,` +
			`"model":"upstream-model","choices":[{"index":0,"delta":{"content":"` +
			strings.Repeat("t", 96) + `"}}]}` + "\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	stream := []byte(sb.String())

	b.Run("scanner only", func(b *testing.B) {
		b.SetBytes(int64(len(stream)))
		for b.Loop() {
			sc := openai.NewScanner(io.Discard, openai.ScannerOptions{
				From: "upstream-model", To: "client-model", CollectUsage: true})
			_, _ = sc.Write(stream)
			_ = sc.Flush()
		}
	})
	b.Run("scanner and detector", func(b *testing.B) {
		b.SetBytes(int64(len(stream)))
		for b.Loop() {
			w := &relayWatch{w: io.Discard}
			sc := openai.NewScanner(w, openai.ScannerOptions{
				From: "upstream-model", To: "client-model", CollectUsage: true})
			_, _ = sc.Write(stream)
			_ = sc.Flush()
			_ = w.flush()
		}
	})
}
