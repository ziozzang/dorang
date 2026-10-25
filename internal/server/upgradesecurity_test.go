package server

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestDeclinedUpgradeBodyStopsWhenRequestIsCancelled(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	headers := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	go func() {
		if _, err := http.ReadRequest(bufio.NewReader(peer)); err != nil {
			return
		}
		_, _ = peer.Write([]byte("HTTP/1.1 401 Unauthorized\r\nContent-Length: 100\r\n\r\n"))
		close(headers)
		<-release
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rq := &Request{HTTP: httptest.NewRequest(http.MethodGet, "http://example.invalid/socket", nil), ctx: ctx}
	pt := &passthroughRoute{dial: func(context.Context, string, string) (net.Conn, error) { return client, nil }}
	target, _ := url.Parse("http://example.invalid/socket")
	result := make(chan error, 1)
	w := httptest.NewRecorder()
	s := newTestServer(t, nil)
	go func() { result <- s.relayWebSocket(w, rq, pt, target) }()
	select {
	case <-headers:
	case <-time.After(2 * time.Second):
		t.Fatal("handshake did not finish")
	}
	cancel()
	select {
	case err := <-result:
		if err != nil || w.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d err=%v", w.Code, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("declined response retained a cancelled request")
	}
}
