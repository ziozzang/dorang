package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// serveOnListener starts the real serving path — the one that owns the
// net/http.Server and therefore the connection deadlines — and returns its
// address.
func serveOnListener(t *testing.T, s *Server) string {
	t.Helper()
	ln := listen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return")
		}
	})
	return addr
}

// waitInFlightIs polls until the in-flight count reaches want, and reports what
// it saw if it never does.
func waitInFlightIs(t *testing.T, s *Server, want int64, within time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if s.InFlight() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("in-flight = %d after %s, want %d: %s", s.InFlight(), within, want, why)
}

// The slow-body client: complete headers, then a body that never arrives.
//
// ReadHeaderTimeout was already set and does nothing here — the headers are
// perfectly punctual. What this client holds is the thing that costs: the
// request has been ADMITTED, ServeHTTP has taken its in-flight slot, the gate
// has authenticated it, and the handler is parked inside Body.read on a socket
// that will produce one byte a minute forever. That slot is what the drain
// waits on and what dorang_inflight_requests reports, and before ReadTimeout was
// set there was nothing in the process that would ever give it back.
//
// So the assertion is the reservation, not the socket. A test that only checked
// the connection was closed would pass against a fix that closed the connection
// and left the handler parked.
func TestDribblingBodyReleasesTheCapacityReservation(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.ReadHeaderTimeout = 200 * time.Millisecond
		o.ReadTimeout = 400 * time.Millisecond
		o.IdleTimeout = time.Second
	})
	addr := serveOnListener(t, s)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Headers announcing a body far larger than what will ever be sent.
	body := `{"model":"model-x","messages":[]}`
	head := "POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: dorang.test\r\n" +
		"Authorization: Bearer good\r\n" +
		"Content-Type: application/json\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(body)+4096) +
		"\r\n"
	if _, err := io.WriteString(conn, head); err != nil {
		t.Fatalf("write headers: %v", err)
	}
	// One dribble, then nothing. The handler is now holding the slot.
	if _, err := io.WriteString(conn, body[:8]); err != nil {
		t.Fatalf("write partial body: %v", err)
	}

	waitInFlightIs(t, s, 1, 2*time.Second,
		"the request was never admitted, so this test is not measuring what it claims to")

	// The capacity reservation comes back on its own, without the client doing
	// anything and without the process being shut down.
	waitInFlightIs(t, s, 0, 5*time.Second,
		"a client dribbling a body is holding a handler and its capacity slot "+
			"indefinitely; the request read has no deadline")

	// And it is reported through the same gauge an operator reads.
	if got := s.Stats().InFlight; got != 0 {
		t.Errorf("Stats().InFlight = %d, want 0", got)
	}

	// The socket goes too: whatever the server answered, the connection is not
	// left open for the client to keep dribbling into.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		var ne net.Error
		if ok := asNetError(err, &ne); ok && ne.Timeout() {
			t.Errorf("the connection is still open after the read deadline fired: %v", err)
		}
	}
}

// A keep-alive connection that goes quiet between requests is closed.
//
// This is the third of the three and the one with no handler behind it, so it
// costs no capacity slot — it costs a file descriptor and a connection-table
// entry, without limit, from any client that can open a socket. Go defaults
// IdleTimeout to ReadTimeout, which is why it is set explicitly rather than left
// to be one setting wearing two names.
func TestIdleKeepAliveConnectionIsClosed(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.ReadHeaderTimeout = 200 * time.Millisecond
		o.ReadTimeout = 400 * time.Millisecond
		o.IdleTimeout = 300 * time.Millisecond
	})
	addr := serveOnListener(t, s)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	body := `{"model":"model-x"}`
	req := "POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: dorang.test\r\n" +
		"Authorization: Bearer good\r\n" +
		"Content-Type: application/json\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)) + body
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read the whole response, then hold the connection open and do nothing.
	// io.ReadAll returns when the server closes it, which is the assertion.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		var ne net.Error
		if ok := asNetError(err, &ne); ok && ne.Timeout() {
			t.Fatalf("the idle connection was never closed: the server is holding a "+
				"keep-alive socket for a client that has stopped speaking (read %d bytes)",
				len(got))
		}
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(got), "200 OK") {
		t.Errorf("the request itself did not succeed: %q", first(string(got), 200))
	}
}

// The deadlines have to reach net/http, not merely be resolved into a snapshot.
func TestConnectionDeadlinesReachTheHTTPServer(t *testing.T) {
	s := newTestServer(t, nil)
	hs := s.newHTTPServer(context.Background())

	if hs.ReadHeaderTimeout != DefaultReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %s, want %s", hs.ReadHeaderTimeout, DefaultReadHeaderTimeout)
	}
	if hs.ReadTimeout != DefaultReadTimeout {
		t.Errorf("ReadTimeout = %s, want %s: a request read with no deadline is a "+
			"handler and a capacity slot a client can hold for free", hs.ReadTimeout,
			DefaultReadTimeout)
	}
	if hs.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("IdleTimeout = %s, want %s", hs.IdleTimeout, DefaultIdleTimeout)
	}
	// The one that must stay unset. A write deadline is measured from the start
	// of the request and would cut a long generation mid-stream.
	if hs.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %s, want none: a legitimate response streams for "+
			"minutes and this would cut it", hs.WriteTimeout)
	}

	// The read deadline must leave room for the header deadline, or the header
	// deadline can never fire and the two settings are one.
	if DefaultReadTimeout <= DefaultReadHeaderTimeout {
		t.Errorf("DefaultReadTimeout %s does not exceed DefaultReadHeaderTimeout %s",
			DefaultReadTimeout, DefaultReadHeaderTimeout)
	}
	// And the idle deadline must outlast the idle timeout of a typical proxy in
	// front (ALB 60s, nginx keepalive_timeout 75s), so the gateway is not the
	// side that closes a connection the proxy is about to reuse.
	if DefaultIdleTimeout < 90*time.Second {
		t.Errorf("DefaultIdleTimeout %s is below the idle timeout of a proxy that may "+
			"sit in front, which turns a reused connection into a 502", DefaultIdleTimeout)
	}
}

// Each deadline is configurable, and a negative value is the explicit "none" —
// distinct from zero, which is absence and takes the default.
func TestConnectionDeadlinesAreConfigurable(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.ReadHeaderTimeout = 5 * time.Second
		o.ReadTimeout = 11 * time.Second
		o.IdleTimeout = -1
	})
	hs := s.newHTTPServer(context.Background())

	if hs.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %s, want 5s", hs.ReadHeaderTimeout)
	}
	if hs.ReadTimeout != 11*time.Second {
		t.Errorf("ReadTimeout = %s, want 11s", hs.ReadTimeout)
	}
	if hs.IdleTimeout != 0 {
		t.Errorf("IdleTimeout = %s, want 0: a negative option is the explicit 'no "+
			"deadline' and must not be defaulted back on", hs.IdleTimeout)
	}
}

// A read timeout inside the header timeout is refused rather than silently
// honoured under the wrong name: net/http measures both from the connection's
// first byte, so the smaller one would be the only one that ever fires.
func TestReadTimeoutShorterThanHeaderTimeoutIsRefused(t *testing.T) {
	_, err := New(Options{
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       5 * time.Second,
	})
	if err == nil {
		t.Fatal("a read timeout shorter than the header timeout was accepted")
	}
	if !strings.Contains(err.Error(), "ReadHeaderTimeout") {
		t.Errorf("the error does not name the setting that conflicts: %v", err)
	}
}

func asNetError(err error, out *net.Error) bool {
	ne, ok := err.(net.Error)
	if ok {
		*out = ne
	}
	return ok
}

func first(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
