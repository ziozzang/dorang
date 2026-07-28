package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// longToolName is over COMPATIBILITY 5.3's 64-byte limit, so the request
// encoder has to shorten it and every decoder has to restore it.
const longToolName = "mcp__github__create_pull_request_review_comment_on_a_specific_line_number"

// toolCall builds a chat call that declares one over-long tool.
func toolCall(clientAPI catalog.API, stream bool) *Call {
	c := chatCall(clientAPI)
	c.Stream = stream
	c.Request.Tools = []canonical.Tool{{
		Name:       longToolName,
		Parameters: json.RawMessage(`{"type":"object"}`),
	}}
	return c
}

// upstreamToolName reads the tool name the request actually carried upstream.
// It is read off the wire rather than computed, so the test cannot agree with
// the implementation about a value neither of them put on the request.
func upstreamToolName(t *testing.T, f *fakeUpstream) string {
	t.Helper()
	body := f.last().body
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("upstream request is not JSON: %v", err)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("upstream request carried %d tools, want 1: %s", len(req.Tools), body)
	}
	return req.Tools[0].Function.Name
}

// TestToolNameRestoredThroughTheBackend is COMPATIBILITY 5.3 asserted where it
// has to hold: across a real [Backend.Do], with the mapping supplied by
// production code and by nothing else.
//
// The unit test for this rule wires the mapping by hand — it constructs an
// EncodeOptions, keeps it, and hands its ToolNames to the stream writer. That is
// precisely what production did not do: the encoder allocated a mapping into a
// temporary options struct, shortened into it, and dropped it on the floor. So
// the shortening ran on every request and the restoration ran on none, and the
// harness supplied the one value the gateway did not.
//
// These cases construct nothing. If the registry is not threaded from the
// encoder to the decoder of the same exchange, they fail.
func TestToolNameRestoredThroughTheBackend(t *testing.T) {
	t.Run("non-streaming", func(t *testing.T) {
		f := newFakeUpstream(t)
		var short string
		f.setHandler(func(w http.ResponseWriter, r *http.Request) {
			short = upstreamToolName(t, f)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"upstream-model",`+
				`"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
				`{"id":"call_1","type":"function","function":{"name":%q,"arguments":"{}"}}]},`+
				`"finish_reason":"tool_calls"}]}`, short)
		})

		p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
		res := testBackend("k").Do(context.Background(), target(p), toolCall(catalog.APIOpenAIChat, false), nil)
		if res.Err != nil {
			t.Fatalf("Do: %v", res.Err)
		}
		assertRestored(t, string(res.Body), short)
	})

	// The two streaming crossings, and the fragmented spelling on each. A name
	// split across frames is the shape an exact-map Restore cannot handle at all:
	// neither half is a key, so a per-fragment lookup returns both unchanged.
	for _, tc := range []struct {
		name     string
		client   catalog.API
		fragment bool
	}{
		{"streaming to a chat client", catalog.APIOpenAIChat, false},
		{"streaming to a chat client, name split across frames", catalog.APIOpenAIChat, true},
		{"streaming to a messages client", catalog.APIAnthropicMessages, false},
		{"streaming to a messages client, name split across frames", catalog.APIAnthropicMessages, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			var short string
			f.setHandler(func(w http.ResponseWriter, r *http.Request) {
				short = upstreamToolName(t, f)
				w.Header().Set("Content-Type", "text/event-stream")
				for _, frame := range toolCallFrames(short, tc.fragment) {
					_, _ = io.WriteString(w, "data: "+frame+"\n\n")
				}
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			})

			p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
			rec := httptest.NewRecorder()
			res := testBackend("k").Do(context.Background(), target(p), toolCall(tc.client, true), rec)
			if res.Err != nil {
				t.Fatalf("Do: %v", res.Err)
			}
			assertRestored(t, rec.Body.String(), short)
		})
	}
}

// toolCallFrames streams one tool call under the name the upstream was given.
func toolCallFrames(name string, fragment bool) []string {
	const head = `{"id":"1","object":"chat.completion.chunk","created":1,"model":"upstream-model",` +
		`"choices":[{"index":0,"delta":{"tool_calls":[`
	const tail = `]},"finish_reason":null}]}`
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }

	var out []string
	if fragment {
		half := len(name) / 2
		out = append(out,
			head+`{"index":0,"id":"call_1","type":"function","function":{"name":`+q(name[:half])+`}}`+tail,
			head+`{"index":0,"function":{"name":`+q(name[half:])+`}}`+tail)
	} else {
		out = append(out,
			head+`{"index":0,"id":"call_1","type":"function","function":{"name":`+q(name)+`}}`+tail)
	}
	out = append(out,
		head+`{"index":0,"function":{"arguments":"{}"}}`+tail,
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"upstream-model",`+
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	return out
}

func assertRestored(t *testing.T, body, short string) {
	t.Helper()
	if short == "" {
		t.Fatal("the upstream saw no tool name at all")
	}
	if short == longToolName {
		t.Fatalf("the over-long name reached the upstream unshortened: %q", short)
	}
	if len(short) > openai.MaxToolNameLen {
		t.Fatalf("the name sent upstream is %d bytes, over the %d limit", len(short), openai.MaxToolNameLen)
	}
	if !strings.Contains(body, longToolName) {
		t.Errorf("the client never saw the name it declared:\n%s", body)
	}
	if strings.Contains(body, short) {
		t.Errorf("the client saw the shortened name %q, which it cannot match against its own tool table:\n%s",
			short, body)
	}
}

// TestToolNameRegistryDoesNotCostTheFastPath. The byte relay is skipped only
// when a name was actually shortened, which is a property of the request and not
// of the deployment.
func TestToolNameRegistryDoesNotCostTheFastPath(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"1","object":"chat.completion.chunk","created":1,`+
			`"model":"upstream-model","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true
	// A tool whose name fits: nothing is shortened, so nothing has to be
	// restored and the relay stays a byte copy.
	c.Request.Tools = []canonical.Tool{{Name: "get_weather"}}
	rec := httptest.NewRecorder()

	if res := testBackend("k").Do(context.Background(), target(p), c, rec); res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	// The scanner forwards the upstream's own frames untouched apart from the
	// model name; a decode-and-re-encode pass would renumber the id.
	if !strings.Contains(rec.Body.String(), `"id":"1"`) {
		t.Errorf("the stream was decoded and re-encoded when nothing needed restoring:\n%s", rec.Body.String())
	}
}

// TestMalformedToolArgumentsAreCountedAndSaidInBand is finding 4's decision at
// the backend seam: the client is told through the only channel left once the
// status is spent, and the exchange carries a code the observer can count.
//
// It runs on the decoding paths. The same-family BYTE relay is deliberately not
// one of them: it forwards the upstream's own frames and its own terminal, so it
// never synthesizes the tool_calls finish that would claim a broken call is
// ready. Nothing is hidden there — the client sees the truncated stream the
// backend sent — and validating it would mean parsing every frame, which is the
// entire cost that path exists to avoid.
// # The two endings are different findings
//
// A tool call whose arguments never parse is finding 4 only when the stream
// ENDED: the frames all arrived, [DONE] arrived, and what the backend produced
// is a call nobody can execute. The same broken document with no [DONE] is a
// stream that stopped mid-write, and the operator's next step is the connection
// rather than the backend's tool serialization. Both are asserted below,
// because until the relay could see a terminator at all only the second fixture
// existed and it was reported as the first.
func TestMalformedToolArgumentsAreCountedAndSaidInBand(t *testing.T) {
	const callFrame = `data: {"id":"1","object":"chat.completion.chunk","created":1,` +
		`"model":"upstream-model","choices":[{"index":0,"delta":{"tool_calls":[` +
		`{"index":0,"id":"call_1","type":"function","function":` +
		`{"name":%s,"arguments":"{\"a\":"}}]}}]}` + "\n\n"

	for _, tc := range []struct {
		name   string
		client catalog.API
		errKey string
	}{
		{"messages client", catalog.APIAnthropicMessages, `"type":"error"`},
		{"chat client", catalog.APIOpenAIChat, `"error"`},
	} {
		for _, end := range []struct {
			name string
			tail string
			code string
		}{
			{"the stream ended on the broken call", "data: [DONE]\n\n", CodeMalformedToolArguments},
			{"the stream stopped mid-document", "", CodeUpstreamStreamTruncated},
		} {
			t.Run(tc.name+"/"+end.name, func(t *testing.T) {
				f := newFakeUpstream(t)
				f.setHandler(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, callFrame, strconv.Quote(longToolName))
					_, _ = io.WriteString(w, end.tail)
				})

				p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
				rec := httptest.NewRecorder()
				res := testBackend("k").Do(context.Background(), target(p), toolCall(tc.client, true), rec)
				if res.Err == nil {
					t.Fatal("a stream that ended mid-JSON was recorded as a clean success")
				}
				if res.Err.Code != end.code {
					t.Errorf("code = %q, want %q", res.Err.Code, end.code)
				}
				body := rec.Body.String()
				if strings.Contains(body, `"finish_reason":"tool_calls"`) ||
					strings.Contains(body, `"stop_reason":"tool_use"`) {
					t.Errorf("the client was told a broken call is ready:\n%s", body)
				}
				if !strings.Contains(body, tc.errKey) {
					t.Errorf("no in-band error reached the client:\n%s", body)
				}
			})
		}
	}
}

// TestNonStreamingMalformedToolArguments. The complete-answer half of the same
// rule. Here the status has not been spent yet, so the client gets an error it
// can act on rather than a 200 carrying a call it cannot parse.
func TestNonStreamingMalformedToolArguments(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"id":"1","object":"chat.completion","created":1,"model":"upstream-model",`+
		`"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]},`+
		`"finish_reason":"tool_calls"}]}`)

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	c := chatCall(catalog.APIOpenAIChat)
	c.Request.Tools = []canonical.Tool{{Name: "f"}}

	res := testBackend("k").Do(context.Background(), target(p), c, nil)
	if res.Err == nil {
		t.Fatalf("a tool call with unparseable arguments was passed on as a finished call: %s", res.Body)
	}
	if res.Err.Code != CodeMalformedToolArguments {
		t.Errorf("code = %q, want %q", res.Err.Code, CodeMalformedToolArguments)
	}
}
