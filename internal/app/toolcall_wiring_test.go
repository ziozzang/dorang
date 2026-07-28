package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/wire/openai"
)

// The tool-call rules of COMPATIBILITY 5.1 and 5.3, asserted through the
// ASSEMBLED gateway rather than through the package that implements them.
//
// This file exists because of how the four defects it covers were found and
// fixed: the fixes landed in internal/backend, internal/backend's own tests
// passed, and internal/app — which is what actually served traffic — still had a
// second copy of the whole path with none of them. Nothing in the suite could
// tell the difference, because every tool-call test called the fixed code
// directly.
//
// So these go in through a.Server.ServeHTTP with a real socket on the other end.
// If the dispatcher ever stops calling internal/backend, every one of them
// fails, which is the only property that could not be asserted before.

// longToolName is over COMPATIBILITY 5.3's 64-byte limit, so the request encoder
// has to shorten it and every decoder has to restore it.
const longToolName = "mcp__github__create_pull_request_review_comment_on_a_specific_line_number"

// toolWiringApp assembles a gateway whose only deployment is h.
func toolWiringApp(t *testing.T, h http.HandlerFunc) (*App, string) {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)

	yaml := fmt.Sprintf(`
version: 1
observability: {always_full_headers: true}
providers:
  - {name: p1, kind: openai, base_url: %q}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`, up.URL)

	a := newWiringApp(t, yaml, nil, func(o *Options) { o.Upstream = up.Client() })
	return a, issueKey(t, a, nil)
}

// toolRequest is a chat request declaring one over-long tool.
func toolRequest(stream bool) string {
	return fmt.Sprintf(`{"model":"m1","stream":%t,
		"messages":[{"role":"user","content":"go"}],
		"tools":[{"type":"function","function":{"name":%q,"parameters":{"type":"object"}}}]}`,
		stream, longToolName)
}

// upstreamToolName reads the tool name the request actually carried upstream. It
// is read off the wire rather than computed, so the test cannot agree with the
// implementation about a value neither of them put on the request.
func upstreamToolName(t *testing.T, r *http.Request, body []byte) string {
	t.Helper()
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Errorf("the upstream request is not JSON: %v", err)
		return ""
	}
	if len(req.Tools) != 1 {
		t.Errorf("the upstream request carried %d tools, want 1: %s", len(req.Tools), body)
		return ""
	}
	return req.Tools[0].Function.Name
}

func readBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("reading the upstream request: %v", err)
	}
	return b
}

// assertShortened is the half of 5.3 the upstream sees.
func assertShortened(t *testing.T, short string) {
	t.Helper()
	switch {
	case short == "":
		t.Fatal("the upstream saw no tool name at all")
	case short == longToolName:
		t.Fatal("the over-long name reached the upstream unshortened")
	case len(short) > openai.MaxToolNameLen:
		t.Fatalf("the name sent upstream is %d bytes, over the %d limit", len(short), openai.MaxToolNameLen)
	}
}

// TestToolNameSurvivesTheAssembledGateway is COMPATIBILITY 5.3 through the live
// path: shortened on the way out, restored on the way back, with the mapping
// supplied by production code and by nothing else.
//
// The name the client declared is the one it can match against its own tool
// table. A caller that receives the shortened form has no way to know what was
// called, and the agentic loop stalls on turn two with no error anywhere.
func TestToolNameSurvivesTheAssembledGateway(t *testing.T) {
	var short string
	a, key := toolWiringApp(t, func(w http.ResponseWriter, r *http.Request) {
		short = upstreamToolName(t, r, readBody(t, r))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"1","object":"chat.completion","created":1,"model":"m1-upstream",
			"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":%q,"arguments":"{}"}}]},
			"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`, short)
	})

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions", toolRequest(false))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	assertShortened(t, short)

	body := w.Body.String()
	if !strings.Contains(body, longToolName) {
		t.Errorf("the client never saw the name it declared:\n%s", body)
	}
	if strings.Contains(body, short) {
		t.Errorf("the client saw the shortened name %q, which it cannot match against its "+
			"own tool table:\n%s", short, body)
	}
}

// TestStreamedToolCallsSurviveTheAssembledGateway covers the two streaming
// defects at once, because one upstream can exhibit both:
//
//   - A shortened name split across frames. An exact-map restore handles neither
//     half, so the client receives a name nobody declared.
//   - Two parallel calls arriving under the same wire index. Concatenating them
//     produces one call whose arguments are two interleaved JSON documents —
//     which is not a repair, it is a corruption a client will try to parse.
func TestStreamedToolCallsSurviveTheAssembledGateway(t *testing.T) {
	var short string
	a, key := toolWiringApp(t, func(w http.ResponseWriter, r *http.Request) {
		short = upstreamToolName(t, r, readBody(t, r))
		w.Header().Set("Content-Type", "text/event-stream")
		half := len(short) / 2
		const head = `{"id":"1","object":"chat.completion.chunk","created":1,"model":"m1-upstream",` +
			`"choices":[{"index":0,"delta":{"tool_calls":[`
		const tail = `]},"finish_reason":null}]}`
		q := func(s string) string { b, _ := json.Marshal(s); return string(b) }

		for _, frame := range []string{
			// The name, in two pieces, under one id.
			head + `{"index":0,"id":"call_1","type":"function","function":{"name":` + q(short[:half]) + `}}` + tail,
			head + `{"index":0,"function":{"name":` + q(short[half:]) + `}}` + tail,
			head + `{"index":0,"function":{"arguments":"{\"line\":12}"}}` + tail,
			// A SECOND call, under the same wire index. COMPATIBILITY 5.1 makes
			// the index required; a backend that reuses it is the case the id has
			// to settle.
			head + `{"index":0,"id":"call_2","type":"function","function":` +
				`{"name":"get_weather","arguments":"{\"city\":\"Seoul\"}"}}` + tail,
			`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m1-upstream",` +
				`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			"[DONE]",
		} {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}
	})

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions", toolRequest(true))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	assertShortened(t, short)

	// The routing decision has to reach the client on a STREAM, which is the one
	// case where the first write happens inside the backend: DESIGN §10.4 stamps
	// the extension headers on that write and anything set afterwards is
	// invisible. A stream whose headers are empty is how "it works" and "it was
	// filled a microsecond too late" look identical.
	if got := w.Header().Get(server.HeaderDeployment); got == "" {
		t.Errorf("no %s on a streamed answer: the routing decision was stamped after the "+
			"first byte, where the client can no longer see it", server.HeaderDeployment)
	}
	if got := w.Header().Get(server.HeaderUpstreamModel); got != "m1-upstream" {
		t.Errorf("%s = %q, want m1-upstream", server.HeaderUpstreamModel, got)
	}

	calls := collectStreamedToolCalls(t, w.Body.String())
	if len(calls) != 2 {
		t.Fatalf("the client saw %d tool calls, want 2 — two calls under one wire index are "+
			"two calls:\n%s", len(calls), w.Body.String())
	}
	for _, c := range calls {
		if c.name == short || strings.Contains(c.name, short[:8]) && c.name != longToolName &&
			c.name != "get_weather" {
			t.Errorf("a call reached the client under the shortened name %q: %+v", short, c)
		}
		if !json.Valid([]byte(c.args)) {
			t.Errorf("tool call %q carries arguments that are not a JSON document: %q\n%s",
				c.name, c.args, w.Body.String())
		}
	}
	if calls[0].name != longToolName {
		t.Errorf("the fragmented name was restored to %q, want the name the client declared %q",
			calls[0].name, longToolName)
	}
	if calls[1].name != "get_weather" {
		t.Errorf("the second call's name is %q, want get_weather", calls[1].name)
	}
}

// streamedCall is one tool call as a client would accumulate it off the wire.
type streamedCall struct {
	name string
	args string
}

// collectStreamedToolCalls replays the client's own accumulation: fragments are
// concatenated per index, exactly as an SDK does.
func collectStreamedToolCalls(t *testing.T, sse string) []streamedCall {
	t.Helper()
	byIndex := map[int]*streamedCall{}
	var order []int

	for _, line := range strings.Split(sse, "\n") {
		payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("the client was sent a frame that is not JSON: %v\n%s", err, payload)
		}
		for _, ch := range chunk.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				c := byIndex[tc.Index]
				if c == nil {
					c = &streamedCall{}
					byIndex[tc.Index] = c
					order = append(order, tc.Index)
				}
				c.name += tc.Function.Name
				c.args += tc.Function.Arguments
			}
		}
	}

	out := make([]streamedCall, 0, len(order))
	for _, i := range order {
		out = append(out, *byIndex[i])
	}
	return out
}

// TestMalformedToolArgumentsRefusedByTheAssembledGateway. dorang does not execute
// tools and cannot repair a call; handing the client a 200 carrying arguments
// that are not a JSON document makes the failure theirs to diagnose, at the point
// where they try to parse it. The status has not been spent yet here, so the
// refusal is one the caller can act on.
func TestMalformedToolArgumentsRefusedByTheAssembledGateway(t *testing.T) {
	a, key := toolWiringApp(t, func(w http.ResponseWriter, r *http.Request) {
		_ = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"1","object":"chat.completion","created":1,"model":"m1-upstream",
			"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]},
			"finish_reason":"tool_calls"}]}`)
	})

	w := callWith(a, key, http.MethodPost, "/v1/chat/completions", toolRequest(false))
	if w.Code == http.StatusOK {
		t.Fatalf("a tool call with unparseable arguments was passed on as a finished call:\n%s",
			w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "malformed_tool_arguments") {
		t.Errorf("the refusal does not name the condition: %d\n%s", w.Code, w.Body.String())
	}
}
