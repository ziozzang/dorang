package backend

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// discardWriter is an http.ResponseWriter that keeps nothing.
//
// A recorder would buffer the whole response, which is the very thing the
// allocation test is trying to detect, so the test would measure its own
// harness and pass regardless.
type discardWriter struct {
	header  http.Header
	written int64
	flushes int
	status  int
}

func newDiscardWriter() *discardWriter { return &discardWriter{header: http.Header{}} }

func (d *discardWriter) Header() http.Header { return d.header }
func (d *discardWriter) Write(p []byte) (int, error) {
	d.written += int64(len(p))
	return len(p), nil
}
func (d *discardWriter) WriteHeader(status int) { d.status = status }
func (d *discardWriter) Flush()                 { d.flushes++ }

// TestStreamRelayDoesNotBuffer sends 16 MiB through the relay and measures what
// the process allocated to move it.
//
// The property is not "it is fast": it is that the relay holds a WINDOW, never
// the stream. A gateway that buffers a response to rewrite it works perfectly in
// every functional test and then holds a gigabyte per concurrent request in
// production, so the bound is asserted rather than assumed.
func TestStreamRelayDoesNotBuffer(t *testing.T) {
	const (
		total = 16 << 20
		frame = 16 << 10
	)
	chunk := []byte("data: " + `{"id":"1","object":"chat.completion.chunk","created":1,` +
		`"model":"upstream-model","choices":[{"index":0,"delta":{"content":"` +
		strings.Repeat("x", frame) + `"}}]}` + "\n\n")

	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for sent := 0; sent < total; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	})

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true
	w := newDiscardWriter()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	res := testBackend("k").Do(context.Background(), target(p), c, w)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}

	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	t.Logf("relayed %d bytes for %d allocated", w.written, allocated)
	if w.written < total {
		t.Fatalf("relayed %d bytes, want at least %d", w.written, total)
	}
	if w.flushes == 0 {
		t.Error("nothing was flushed: a response that arrives as one block is not a stream")
	}
	// The bound is a quarter of the payload. It is loose on purpose — this
	// counts every allocation in the process, including the fake upstream's own
	// writes — and still fails by an order of magnitude if the body is held.
	if limit := uint64(total / 4); allocated > limit {
		t.Errorf("relaying %d bytes allocated %d, over the %d bound: the stream is being buffered",
			total, allocated, limit)
	}
}

// TestStreamRelayPassesBytesThroughAndRewritesTheModel covers the fast path's
// one obligation: COMPATIBILITY 2.5 requires the client-facing name on every
// chunk, and §7.2 requires it in the body.
func TestStreamRelayRewritesTheModel(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\","+
				"\"created\":1,\"model\":\"upstream-model\","+
				"\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%d\"}}]}\n\n", i)
		}
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"upstream-model","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,`+
			`"total_tokens":8}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true
	rec := httptest.NewRecorder()

	res := testBackend("k").Do(context.Background(), target(p), c, rec)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	body := rec.Body.String()
	if strings.Contains(body, "upstream-model") {
		t.Error("a chunk carries the upstream model id; every chunk must carry the client's name")
	}
	if !strings.Contains(body, "client-model") {
		t.Error("no chunk carries the client-facing name")
	}
	if !strings.Contains(body, "[DONE]") {
		t.Error("the stream terminator was not relayed")
	}
	if res.Usage.InputTokens != 5 || res.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v, want the terminal usage frame read without decoding the stream",
			res.Usage)
	}
	if !res.FirstByteSent {
		t.Error("FirstByteSent must be set: it closes fail-back for good (§7.6)")
	}
	if f.last().header.Get("Accept") != "text/event-stream" {
		t.Error("the upstream was not asked for a stream")
	}
}

// TestStreamCrossFamily exercises the path that must decode: OpenAI chunks in,
// Anthropic events out. The two families disagree about block boundaries, so
// there is nothing to relay verbatim.
func TestStreamCrossFamily(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"m","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	c := chatCall(catalog.APIAnthropicMessages)
	c.Stream = true
	rec := httptest.NewRecorder()

	res := testBackend("k").Do(context.Background(), target(p), c, rec)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start", "event: content_block_start",
		"event: content_block_delta", "event: content_block_stop",
		"event: message_delta", "event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the messages stream is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Error("the chat-completions terminator leaked into a messages stream")
	}
}

// TestStreamCrossFamilyAdoptsEngineReasoning is the streaming half of
// VLLM.md §2.5. internal/wire/openai's Delta models reasoning_content and
// nothing else, so a vLLM frame's `reasoning` is dropped by the chunk decoder —
// and an Anthropic-speaking caller sees no reasoning at all.
func TestStreamCrossFamilyAdoptsEngineReasoning(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"thinking"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"m","choices":[{"index":0,"delta":{"content":"4"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	p := testProvider(t, f, "vllm", catalog.APIOpenAIChat)
	c := chatCall(catalog.APIAnthropicMessages)
	c.Stream = true
	rec := httptest.NewRecorder()

	if res := testBackend("k").Do(context.Background(), target(p), c, rec); res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if !strings.Contains(rec.Body.String(), "thinking") {
		t.Errorf("the engine's streamed reasoning did not reach the caller:\n%s", rec.Body.String())
	}
}

// TestGeminiStream covers the third streaming shape: whole parts per frame,
// usage in a trailing frame, and no terminator of its own.
func TestGeminiStream(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"role":"model",`+
			`"parts":[{"text":"he"}]},"index":0}],"responseId":"r1"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"role":"model",`+
			`"parts":[{"text":"llo"}]},"finishReason":"STOP","index":0}],`+
			`"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6}}`+"\n\n") // pragma: allowlist secret — test fixture
	})

	p := testProvider(t, f, "google", catalog.APIGemini)
	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true
	c.IncludeUsage = true
	rec := httptest.NewRecorder()

	res := testBackend("k").Do(context.Background(), target(p), c, rec)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if q := f.last().query; !strings.Contains(q, "alt=sse") {
		t.Errorf("query = %q, want alt=sse: without it the route answers a chunked JSON array", q)
	}
	if !strings.Contains(f.last().path, ":streamGenerateContent") {
		t.Errorf("path = %q, want the streaming method", f.last().path)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hello") && !(strings.Contains(body, "he") && strings.Contains(body, "llo")) {
		t.Errorf("the text did not reach the client:\n%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Error("a chat-completions client needs its terminator even when the upstream has none")
	}
	if res.Usage.InputTokens != 4 || res.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

// TestNextSSEData pins the frame reader's edge cases, which are where an SSE
// implementation usually goes wrong: multi-line data, comments, keep-alives and
// CRLF framing.
func TestNextSSEData(t *testing.T) {
	in := ": keep-alive\n\n" +
		"data: one\n\n" +
		"event: ignored\ndata: two\ndata: three\n\n" +
		"\n" +
		"data: four\r\n\r\n"
	br := bufio.NewReader(strings.NewReader(in))
	var buf strings.Builder

	var got []string
	for {
		p, err := nextSSEData(br, &buf, maxStreamFrame)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("nextSSEData: %v", err)
		}
		got = append(got, p)
	}
	want := []string{"one", "two\nthree", "four"}
	if len(got) != len(want) {
		t.Fatalf("frames = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %q, want %q", i, got[i], want[i])
		}
	}
}
