package openai

import (
	"reflect"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// Canonical round trips for every T1 type.
//
// A byte golden pins what goes on the wire; this pins that the NEUTRAL form is
// a fixed point. The two catch different bugs: a golden passes for an adapter
// that copies bytes it never understood, and a round trip passes for one that
// emits a shape nobody else does. Both are needed, which is why both exist.
//
// The assertion is decode → encode → decode and a deep equality on the two
// neutral values. Comparing the two byte strings instead would be asserting
// that dorang re-emits the caller's formatting, which it deliberately does not.

func TestT1CanonicalRoundTrips(t *testing.T) {
	cases := []struct {
		name string
		body string
		trip func([]byte) (any, []byte, error)
	}{
		{
			name: "completions",
			body: `{"model":"qwen3.5:397b","prompt":["a","b"],"max_tokens":16,"temperature":0.5,` +
				`"stop":["x","y"],"logprobs":3,"seed":7,"user":"u","echo":true}`,
			trip: func(b []byte) (any, []byte, error) {
				req, err := DecodeCompletionRequest(b)
				if err != nil {
					return nil, nil, err
				}
				out, err := MarshalCompletionRequest(req, nil)
				return req, out, err
			},
		},
		{
			name: "responses",
			body: `{"model":"qwen3.5:397b","input":[` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},` +
				`{"type":"input_image","image_url":"https://example.test/a.png","detail":"low"}]},` +
				`{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},` +
				`{"type":"function_call_output","call_id":"c1","output":"ok"}],` +
				`"instructions":"terse","max_output_tokens":32,"temperature":0.4,"top_p":0.9,` +
				`"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"},"strict":true}],` +
				`"tool_choice":"auto","parallel_tool_calls":false,` +
				`"text":{"format":{"type":"json_schema","name":"s","schema":{"type":"object"},"strict":true}},` +
				`"reasoning":{"effort":"high","summary":"concise"},` +
				`"store":false,"previous_response_id":"resp_prev","metadata":{"k":"v"},"user":"u"}`,
			trip: func(b []byte) (any, []byte, error) {
				req, err := DecodeResponsesRequest(b)
				if err != nil {
					return nil, nil, err
				}
				out, err := MarshalResponsesRequest(req, nil)
				return req, out, err
			},
		},
		{
			name: "moderations",
			body: `{"input":[{"type":"text","text":"a"},` +
				`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}],"model":"omni:latest"}`,
			trip: func(b []byte) (any, []byte, error) {
				req, err := DecodeModerationRequest(b)
				if err != nil {
					return nil, nil, err
				}
				out, err := MarshalModerationRequest(req, "")
				return req, out, err
			},
		},
		{
			name: "speech",
			body: `{"model":"tts:1","input":"hello","voice":"alloy","instructions":"slowly",` +
				`"response_format":"flac","speed":1.25,"an_unmodelled_knob":true}`,
			trip: func(b []byte) (any, []byte, error) {
				req, err := DecodeSpeechRequest(b)
				if err != nil {
					return nil, nil, err
				}
				out, err := MarshalSpeechRequest(req, "")
				return req, out, err
			},
		},
		{
			name: "images",
			body: `{"prompt":"a cat","model":"image:1","n":2,"size":"auto","quality":"high",` +
				`"background":"transparent","response_format":"b64_json","output_format":"webp",` +
				`"output_compression":80,"user":"u"}`,
			trip: func(b []byte) (any, []byte, error) {
				req, err := DecodeImageRequest(b)
				if err != nil {
					return nil, nil, err
				}
				out, err := MarshalImageRequest(req, "")
				return req, out, err
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first, once, err := c.trip([]byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			second, twice, err := c.trip(once)
			if err != nil {
				t.Fatalf("re-decoding dorang's own output failed: %v\n%s", err, once)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("the neutral form is not a fixed point\n first: %+v\nsecond: %+v", first, second)
			}
			if string(once) != string(twice) {
				t.Fatalf("encoding is not idempotent\n once: %s\ntwice: %s", once, twice)
			}
		})
	}
}

// TestT1ResponseCanonicalRoundTrips does the same for the answers.
func TestT1ResponseCanonicalRoundTrips(t *testing.T) {
	t.Run("completions", func(t *testing.T) {
		body := []byte(`{"id":"cmpl-1","object":"text_completion","created":1,"model":"m",` +
			`"choices":[{"index":0,"text":"hi","finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
		first, err := DecodeCompletionResponse(body, nil)
		if err != nil {
			t.Fatal(err)
		}
		once, err := MarshalCompletionResponse(first, nil)
		if err != nil {
			t.Fatal(err)
		}
		second, err := DecodeCompletionResponse(once, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("not a fixed point\n first: %+v\nsecond: %+v", first, second)
		}
	})

	t.Run("responses", func(t *testing.T) {
		body := []byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed",` +
			`"model":"m","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"t"}],` +
			`"encrypted_content":"blob"},{"type":"message","role":"assistant",` +
			`"content":[{"type":"output_text","text":"hi","annotations":[]}]},` +
			`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}],` +
			`"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},` + // pragma: allowlist secret — test fixture
			`"output_tokens":6,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":16}}`) // pragma: allowlist secret — test fixture
		first, err := DecodeResponsesResponse(body, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Inclusive input, and reasoning already inside output (DESIGN §10.7).
		if first.Usage.InputTokens != 10 || first.Usage.CacheReadTokens != 4 {
			t.Fatalf("usage %+v", first.Usage)
		}
		if first.Usage.OutputTokens < first.Usage.ReasoningTokens {
			t.Fatalf("output %d < reasoning %d", first.Usage.OutputTokens, first.Usage.ReasoningTokens)
		}
		once, err := MarshalResponsesResponse(first, &ResponsesOptions{ID: "resp_1"})
		if err != nil {
			t.Fatal(err)
		}
		second, err := DecodeResponsesResponse(once, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("not a fixed point\n first: %+v\nsecond: %+v\n bytes: %s", first, second, once)
		}
	})

	t.Run("images", func(t *testing.T) {
		body := []byte(`{"created":1,"data":[{"url":"https://example.test/a.png"}],"size":"1024x1024",` +
			`"usage":{"input_tokens":3,"output_tokens":100,"total_tokens":103}}`)
		first, err := DecodeImageResponse(body)
		if err != nil {
			t.Fatal(err)
		}
		once, err := MarshalImageResponse(first)
		if err != nil {
			t.Fatal(err)
		}
		second, err := DecodeImageResponse(once)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("not a fixed point\n first: %+v\nsecond: %+v", first, second)
		}
	})

	t.Run("moderations", func(t *testing.T) {
		body := []byte(`{"id":"modr-1","model":"m","results":[{"flagged":false,` +
			`"categories":{"hate":false},"category_scores":{"hate":0.001}}]}`)
		first, err := DecodeModerationResponse(body, "")
		if err != nil {
			t.Fatal(err)
		}
		once, err := MarshalModerationResponse(first)
		if err != nil {
			t.Fatal(err)
		}
		second, err := DecodeModerationResponse(once, "")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("not a fixed point\n first: %+v\nsecond: %+v", first, second)
		}
	})

	t.Run("transcription", func(t *testing.T) {
		body := []byte(`{"text":"hi","language":"english","duration":1.5,` +
			`"segments":[{"id":0,"start":0,"end":1.5,"text":"hi","temperature":0}],` +
			`"words":[{"word":"hi","start":0,"end":0.4}]}`)
		first, err := DecodeTranscriptionResponse(body, "application/json")
		if err != nil {
			t.Fatal(err)
		}
		once, _, err := MarshalTranscriptionResponse(first)
		if err != nil {
			t.Fatal(err)
		}
		second, err := DecodeTranscriptionResponse(once, "application/json")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("not a fixed point\n first: %+v\nsecond: %+v", first, second)
		}
	})
}

// TestRerankNeutralFormIsShared: the rerank neutral types live in
// internal/canonical like every other family's, which is what lets an
// OpenAI-compatible engine and a rerank vendor serve the same request.
func TestRerankNeutralFormIsShared(t *testing.T) {
	var _ *canonical.RerankRequest
	var _ *canonical.RerankResponse
	var _ *canonical.ModerationRequest
	var _ *canonical.SpeechRequest
	var _ *canonical.TranscriptionRequest
	var _ *canonical.ImageRequest
}
