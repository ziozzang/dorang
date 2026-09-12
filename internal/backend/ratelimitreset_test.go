package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// A 429 that names its reset in a rate-limit header is a wait and a cooldown.
//
// The upstream said when its window recovers; it said it in the header its
// family uses rather than in Retry-After, and until now that was the same as
// saying nothing: the client got no Retry-After, the deployment got the
// configured default cooldown, and Outcome.ResetAt had no producer anywhere
// (DESIGN §17.1's third harness row). Asserted on the Result the dispatcher
// reads — the seconds the client will see and the instant the router will use.
func TestAnUpstreamResetHeaderIsAWaitAndACooldown(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers func(h http.Header)
		wantSec int
	}{
		{"openai-shaped, requests exhausted", 429, func(h http.Header) {
			h.Set("x-ratelimit-remaining-requests", "0")
			h.Set("x-ratelimit-reset-requests", "6m0s")
			h.Set("x-ratelimit-remaining-tokens", "1500")
			h.Set("x-ratelimit-reset-tokens", "20ms")
		}, 360},
		{"openai-shaped, tokens exhausted picks the token window", 429, func(h http.Header) {
			h.Set("x-ratelimit-remaining-requests", "40")
			h.Set("x-ratelimit-reset-requests", "1s")
			h.Set("x-ratelimit-remaining-tokens", "0")
			h.Set("x-ratelimit-reset-tokens", "2m")
		}, 120},
		{"anthropic instant", 429, func(h http.Header) {
			h.Set("anthropic-ratelimit-requests-remaining", "0")
			h.Set("anthropic-ratelimit-requests-reset", time.Now().Add(90*time.Second).UTC().Format(time.RFC3339))
		}, 90},
		{"ietf delta seconds on an overloaded 503", 503, func(h http.Header) {
			h.Set("RateLimit-Reset", "45")
		}, 45},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.setHandler(func(w http.ResponseWriter, r *http.Request) {
				tc.headers(w.Header())
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error"}}`))
			})
			b := testBackend("sk-test")
			res := b.Do(context.Background(), target(testProvider(t, f, "openai", catalog.APIOpenAIChat)),
				chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
			if res.Err == nil {
				t.Fatal("a 429 became a success")
			}
			// One second of slack on the instant-shaped headers, whose value
			// was computed before the request was made.
			if got := res.Err.RetryAfterSeconds; got < tc.wantSec-1 || got > tc.wantSec {
				t.Errorf("RetryAfterSeconds = %d, want %d: the client is told nothing it can wait on", got, tc.wantSec)
			}
			if res.RetryAfter != 0 {
				t.Errorf("RetryAfter = %v: the literal Retry-After header was not sent, and the field must say so", res.RetryAfter)
			}
			if res.ResetAt.IsZero() {
				t.Error("ResetAt is zero: Outcome.ResetAt still has no producer")
			}
		})
	}

	t.Run("a stray reset header on a 400 says nothing", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.setHandler(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("x-ratelimit-remaining-requests", "0")
			w.Header().Set("x-ratelimit-reset-requests", "6m0s")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad"}}`))
		})
		b := testBackend("sk-test")
		res := b.Do(context.Background(), target(testProvider(t, f, "openai", catalog.APIOpenAIChat)),
			chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
		if res.Err == nil || res.Err.RetryAfterSeconds != 0 || !res.ResetAt.IsZero() {
			t.Errorf("a refusal acquired a wait: %+v reset=%v", res.Err, res.ResetAt)
		}
	})

	t.Run("Retry-After wins when both are present", func(t *testing.T) {
		f := newFakeUpstream(t)
		f.setHandler(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.Header().Set("x-ratelimit-remaining-requests", "0")
			w.Header().Set("x-ratelimit-reset-requests", "6m0s")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
		})
		b := testBackend("sk-test")
		res := b.Do(context.Background(), target(testProvider(t, f, "openai", catalog.APIOpenAIChat)),
			chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
		if res.Err == nil || res.Err.RetryAfterSeconds != 7 {
			t.Errorf("RetryAfterSeconds = %v, want the literal header's 7", res.Err)
		}
	})
}
