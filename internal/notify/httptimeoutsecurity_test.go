package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWebhookHonorsConfiguredTimeoutAboveTenSeconds(t *testing.T) {
	// The deployed webhook allows 15 seconds. A fixed 10-second client
	// timeout must not silently shorten that operator-selected deadline.
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(11 * time.Second):
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	}))
	defer receiver.Close()
	d := &httpDriver{o: HTTPOptions{URL: receiver.URL, Timeout: 15 * time.Second}}
	if err := d.Deliver(context.Background(), &Message{}); err != nil {
		t.Fatalf("delivery within configured 15-second timeout failed: %v", err)
	}
}

func TestWebhookHonorsShorterDeliveryDeadlines(t *testing.T) {
	for _, parentDeadline := range []bool{false, true} {
		name := "configured timeout"
		if parentDeadline {
			name = "earlier parent deadline"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer receiver.Close()
			defer close(release)
			timeout, parentTimeout := 50*time.Millisecond, 2*time.Second
			if parentDeadline {
				timeout, parentTimeout = parentTimeout, timeout
			}
			ctx, cancel := context.WithTimeout(context.Background(), parentTimeout)
			defer cancel()
			d := &httpDriver{o: HTTPOptions{URL: receiver.URL, Timeout: timeout}}
			if err := d.Deliver(ctx, &Message{}); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("delivery did not report the deadline: %v", err)
			}
			if !parentDeadline && ctx.Err() != nil {
				t.Fatal("delivery waited for the parent instead of its configured timeout")
			}
		})
	}
}
