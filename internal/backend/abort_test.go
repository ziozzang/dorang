package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// An upstream that sends the first chunk and then keeps the stream open until
// the request ends, like a long generation.
func stallingStream(t *testing.T, first bool) *fakeUpstream {
	t.Helper()
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if first {
			_, _ = io.WriteString(w, oaFrames(oaChunkAlpha))
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	return f
}

func streamProvider(t *testing.T, f *fakeUpstream, timeout time.Duration) *Provider {
	t.Helper()
	p, err := NewProvider(Spec{Name: "p1", Kind: "openai", API: catalog.APIOpenAIChat,
		BaseURL: f.srv.URL, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The caller hanging up mid-stream is not the deployment failing (board #1623).
func TestClientHangUpMidStreamIsAborted(t *testing.T) {
	f := stallingStream(t, true)
	p := streamProvider(t, f, 0)
	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	res := testBackend("k").Do(ctx, target(p), c, httptest.NewRecorder())
	if res.Err == nil {
		t.Fatal("expected an error")
	}
	if !res.Aborted {
		t.Fatalf("a client hang-up was not marked Aborted: %+v", res.Err)
	}
	if res.Retryable {
		t.Error("a hang-up must not be offered to another deployment")
	}
}

// A stream that was delivering when the provider's time limit fired is a long
// generation, not a dead deployment.
func TestDeadlineOnAFlowingStreamIsAborted(t *testing.T) {
	f := stallingStream(t, true)
	p := streamProvider(t, f, 300*time.Millisecond)
	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true

	res := testBackend("k").Do(context.Background(), target(p), c, httptest.NewRecorder())
	if res.Err == nil || !res.Aborted || !res.Timeout {
		t.Fatalf("flowing stream cut by the limit: aborted=%v timeout=%v err=%v", res.Aborted, res.Timeout, res.Err)
	}
}

// But an upstream that never delivered anything before the limit did not answer
// at all, and that is the evidence availability tracking exists for.
func TestDeadlineBeforeAnyByteIsStillATimeout(t *testing.T) {
	f := stallingStream(t, false)
	p := streamProvider(t, f, 300*time.Millisecond)
	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true

	res := testBackend("k").Do(context.Background(), target(p), c, httptest.NewRecorder())
	if res.Err == nil || res.Aborted || !res.Timeout {
		t.Fatalf("silent upstream: aborted=%v timeout=%v err=%v", res.Aborted, res.Timeout, res.Err)
	}
}

// A stream the upstream itself cut short stays the deployment's failure.
func TestUpstreamTruncationIsNotAborted(t *testing.T) {
	f := streamUpstream(t, oaFrames(oaChunkAlpha, oaChunkBeta))
	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	c := chatCall(catalog.APIOpenAIChat)
	c.Stream = true

	res := testBackend("k").Do(context.Background(), target(p), c, httptest.NewRecorder())
	if res.Err == nil || res.Aborted {
		t.Fatalf("upstream truncation: aborted=%v err=%v", res.Aborted, res.Err)
	}
}
