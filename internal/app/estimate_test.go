package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/tokenest"
)

// The dispatcher's half of DESIGN §10.5a: which estimate a call uses, and what
// an upstream 400 is classified as. The end-to-end properties — that a
// multimodal request is admitted and that an overflow lands on a larger
// deployment — are in testing/scenario, because neither is a property of this
// package alone.

// TestATypedRequestIsNeverEstimatedByItsBodyLength. The byte rule is the
// fallback for the surfaces dorang relays without decoding, and for nothing
// else. Reaching it with a decoded request in hand is the defect: a base64
// image is body bytes and costs several hundred times what an image costs.
func TestATypedRequestIsNeverEstimatedByItsBodyLength(t *testing.T) {
	image := strings.Repeat("A", 2_700_000)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"..."}]}`)

	cases := []struct {
		name string
		c    *call
	}{
		{"chat", &call{kind: callChat, body: body, creq: &canonical.Request{
			Messages: []canonical.Message{{Role: canonical.RoleUser, Content: canonical.Content{
				canonical.ImageBlock("image/jpeg", image)}}}}}},
		{"moderations", &call{kind: callModerations, body: body,
			modReq: &canonical.ModerationRequest{Inputs: []canonical.ModerationInput{
				{Kind: canonical.ModerationImage,
					Source: &canonical.Source{Kind: canonical.SourceBase64, Data: image}}}}}},
		{"transcription", &call{kind: callTranscription, body: body,
			transReq: &canonical.TranscriptionRequest{
				File: canonical.File{Field: "file", Name: "a.wav", Data: make([]byte, 20<<20)}}}},
		{"images", &call{kind: callImages, body: body,
			imageReq: &canonical.ImageRequest{Op: canonical.ImageEdit, Prompt: "make it blue",
				Images: []canonical.File{{Field: "image", Data: make([]byte, 4<<20)}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			est := tc.c.estimate()
			if est.Method != tokenest.MethodStructural {
				t.Fatalf("method = %q, want %q: a decoded request fell back to the byte rule",
					est.Method, tokenest.MethodStructural)
			}
			if est.Tokens > 100_000 {
				t.Errorf("estimated at %d tokens; the media is being counted as text", est.Tokens)
			}
			if est.Exact {
				t.Error("the estimate claims to be exact")
			}
		})
	}

	t.Run("embeddings falls back to bytes, because there is nothing typed", func(t *testing.T) {
		c := &call{kind: callEmbeddings, body: body}
		if est := c.estimate(); est.Method != tokenest.MethodBytes {
			t.Errorf("method = %q, want %q", est.Method, tokenest.MethodBytes)
		}
	})
}

// TestAnUpstreamOverflowIsRecognisedFromTheBody.
//
// router.Classify reads the status line and stops, so every 400 arrived as
// CauseNone and the context_window chain could not be entered by an upstream
// signal. The signatures below are the shapes the deployed backends actually
// send; the inverse cases are the ones that must stay terminal, because
// retrying a malformed request elsewhere burns capacity to reach the identical
// refusal.
func TestAnUpstreamOverflowIsRecognisedFromTheBody(t *testing.T) {
	overflow := []struct {
		name string
		body string
	}{
		{"openai code", `{"error":{"message":"too long","type":"invalid_request_error",` +
			`"code":"context_length_exceeded"}}`},
		{"vllm message", `{"object":"error","message":"This model's maximum context length is ` +
			`4096 tokens. However, you requested 5000 tokens.","type":"BadRequestError","code":400}`},
		{"anthropic message", `{"type":"error","error":{"type":"invalid_request_error",` +
			`"message":"prompt is too long: 250000 tokens > 200000 maximum"}}`},
		{"sglang flat", `{"object":"error","message":"Input is too long for the context window",` +
			`"type":"BadRequest"}`},
	}
	for _, tc := range overflow {
		t.Run(tc.name, func(t *testing.T) {
			e := server.Normalize(http.StatusBadRequest, []byte(tc.body))
			if got := upstreamCause(http.StatusBadRequest, e); got != router.CauseContextWindow {
				t.Errorf("cause = %v, want context_window", got)
			}
		})
	}

	terminal := []struct {
		name   string
		status int
		body   string
	}{
		{"validation failure", 400,
			`{"detail":[{"loc":["body","temperature"],"msg":"must be <= 2"}]}`},
		{"unknown parameter", 400,
			`{"error":{"message":"Unrecognized request argument: foo","type":"invalid_request_error"}}`},
		{"content policy", 400,
			`{"error":{"message":"Your request was rejected by our safety system","code":"content_policy_violation"}}`},
		{"opaque html", 400, `<html><body>Bad Request</body></html>`},
	}
	for _, tc := range terminal {
		t.Run(tc.name, func(t *testing.T) {
			e := server.Normalize(tc.status, []byte(tc.body))
			if got := upstreamCause(tc.status, e); got != router.CauseNone {
				t.Errorf("cause = %v, want none: this 400 is terminal", got)
			}
		})
	}

	t.Run("the status line still decides where it can", func(t *testing.T) {
		// A 429 whose body happens to mention a context length is a rate limit.
		// Classify answers first and the body is never consulted.
		e := server.Normalize(429, []byte(`{"error":{"message":"maximum context length"}}`))
		if got := upstreamCause(429, e); got != router.CauseRateLimit {
			t.Errorf("cause = %v, want rate_limit", got)
		}
	})
}
