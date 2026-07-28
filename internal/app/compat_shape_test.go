package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// COMPATIBILITY §3.3 and §6.8, asserted as EMITTED FRAMES and driven from a
// loaded configuration.
//
// Both settings existed at both values in internal/wire, were parsed and
// defaulted by internal/config, and were documented as operator-settable — and
// the non-default value was refused at load, because nothing carried it from one
// to the other. The refusal was the honest response to that gap and is not a
// substitute for closing it.
//
// These tests are written the way they are because of how the gap survived: a
// wire-package test proved the encoder can emit either shape, and passed for the
// whole time no configuration could select one. So nothing here constructs a
// StreamConfig or a ResponseOptions. Each test writes YAML, loads it through
// config.LoadBytes, assembles the gateway, sends an HTTP request and reads bytes
// off the response. If the value stops travelling at any hop between the file
// and the socket, they fail.

// compatApp assembles a gateway whose single deployment is h, with a `compat:`
// block spliced into the configuration.
func compatApp(t *testing.T, compat string, h http.HandlerFunc) (*App, string) {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)

	yaml := fmt.Sprintf(`
version: 1
observability: {always_full_headers: true}
%s
providers:
  - {name: p1, kind: openai, base_url: %q}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`, compat, up.URL)

	a := newWiringApp(t, yaml, nil, func(o *Options) { o.Upstream = up.Client() })
	return a, issueKey(t, a, nil)
}

// upstreamChatStream answers one SSE stream carrying a usage chunk in the
// reference proxy's shape.
//
// The usage chunk is the upstream's own, deliberately. A same-family stream
// takes the byte-relay fast path, on which dorang forwards these bytes rather
// than rendering its own — so this is what the client sees whenever the relay
// runs, and the strict setting is only honoured because it takes the request off
// that path.
func upstreamChatStream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, frame := range []string{
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"m1-upstream",` +
			`"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}`,
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"m1-upstream",` +
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"m1-upstream",` +
			`"choices":[{"index":0,"delta":{}}],` +
			`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
	} {
		_, _ = io.WriteString(w, "data: "+frame+"\n\n")
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// usageChunkChoicesFrame pulls the `choices` array off the one streamed chunk that
// carries a usage object, as raw JSON.
//
// Raw rather than decoded: the difference between `[]` and
// `[{"index":0,"delta":{}}]` is exactly the kind a struct round-trip erases, and
// the shape on the wire is the entire subject of §3.3.
func usageChunkChoicesFrame(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage   json.RawMessage `json:"usage"`
			Choices json.RawMessage `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("a streamed frame is not JSON: %v\n%s", err, payload)
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			return string(chunk.Choices)
		}
	}
	t.Fatalf("the stream carried no usage chunk:\n%s", body)
	return ""
}

// TestUsageChunkChoicesIsSelectableFromConfiguration is COMPATIBILITY §3.3.
func TestUsageChunkChoicesIsSelectableFromConfiguration(t *testing.T) {
	const request = `{"model":"m1","stream":true,"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"hi"}]}`

	for _, tc := range []struct {
		name   string
		compat string
		want   string
	}{
		{
			name: "absent compat block", compat: "",
			want: `[{"index":0,"delta":{}}]`,
		},
		{
			name: "stub, named explicitly", compat: "compat: {usage_chunk_choices: stub}",
			want: `[{"index":0,"delta":{}}]`,
		},
		{
			// The value that was refused at load until the field to carry it
			// existed. `[]` is what strict OpenAI sends.
			name: "empty", compat: "compat: {usage_chunk_choices: empty}",
			want: `[]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, secret := compatApp(t, tc.compat, upstreamChatStream)

			w := callWith(a, secret, http.MethodPost, "/v1/chat/completions", request)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if got := usageChunkChoicesFrame(t, w.Body.String()); got != tc.want {
				t.Errorf("the usage chunk's choices array is %s, want %s\n"+
					"the configured value did not reach openai.StreamConfig\n%s",
					got, tc.want, w.Body.String())
			}
		})
	}
}

// upstreamChatResponse answers one non-streaming chat completion with usage.
func upstreamChatResponse(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"m1-upstream",`+
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
}

// TestAnthropicTotalTokensIsSelectableFromConfiguration is COMPATIBILITY §6.8.
//
// The presence of the member is the assertion, not its value, and the body is
// decoded into a raw map for that reason: §6.8 is about a field that is there or
// is not, and a struct with an int would report absence as zero.
func TestAnthropicTotalTokensIsSelectableFromConfiguration(t *testing.T) {
	const request = `{"model":"m1","max_tokens":64,` +
		`"messages":[{"role":"user","content":"hi"}]}`

	for _, tc := range []struct {
		name   string
		compat string
		want   bool
	}{
		{"absent compat block", "", true},
		{"true, named explicitly", "compat: {anthropic_total_tokens: true}", true},
		{
			// The value that was refused at load. The strict vendor shape has
			// no total_tokens anywhere.
			name: "false", compat: "compat: {anthropic_total_tokens: false}", want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, secret := compatApp(t, tc.compat, upstreamChatResponse)

			w := callWith(a, secret, http.MethodPost, "/v1/messages", request)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			var body struct {
				Usage map[string]json.RawMessage `json:"usage"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("the answer is not a message: %v\n%s", err, w.Body.String())
			}
			if _, got := body.Usage["total_tokens"]; got != tc.want {
				t.Errorf("usage.total_tokens present = %v, want %v\n"+
					"the configured value did not reach anthropic.ResponseOptions\n%s",
					got, tc.want, w.Body.String())
			}
		})
	}
}

// TestStreamingAnthropicNeverCarriesTotalTokens pins the half of §6.8 that is
// NOT a switch.
//
// The asymmetry between the two shapes is the divergence being reproduced, not
// an oversight in the switch: anthropic.StreamConfig has no TotalTokens field at
// all, deliberately. So `anthropic_total_tokens: true` must not start emitting
// the member on a stream, which is what a plausible reading of the setting's
// name would have it do.
func TestStreamingAnthropicNeverCarriesTotalTokens(t *testing.T) {
	for _, compat := range []string{
		"compat: {anthropic_total_tokens: true}",
		"compat: {anthropic_total_tokens: false}",
	} {
		a, secret := compatApp(t, compat, upstreamChatStream)
		w := callWith(a, secret, http.MethodPost, "/v1/messages",
			`{"model":"m1","max_tokens":64,"stream":true,`+
				`"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", compat, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "total_tokens") {
			t.Errorf("%s: a streamed message carried total_tokens\n%s",
				compat, w.Body.String())
		}
	}
}
