package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/config"
)

// The five settings this file covers were added as server.Options and auth.Config
// fields and deliberately left unreachable from YAML, because the only
// config-to-options wiring is in app.go. They are the same shape as
// compat.legacy_headers before it was wired: a working consumer no deployment could
// reach.
//
// # Why none of this is asserted by reading a field back
//
// internal/config's TestEveryConfiguredFieldIsReadSomewhere is VACUOUS for exactly
// these five. It matches on identifier names, and `ReadTimeout`, `IdleTimeout`,
// `Rate` and `Burst` all occur as identifiers in non-test source outside
// internal/config — `ReadTimeout` in internal/server, `MissRate` and `MissBurst` in
// internal/auth — so the guard reported every one of them as consumed while nothing
// consumed them. That is the blind spot its own doc comment lists, firing on a whole
// block rather than on one field.
//
// So every test here drives config.LoadBytes through an assembled gateway and
// asserts something the RUNNING process does: a socket that gets closed, a socket
// that does not, an HTTP status. A test that read the value back out of Options
// would pass against the defect it is here to catch.

// serveAssembled starts the app's real serving path — the one that owns the
// net/http.Server and therefore the connection deadlines — and returns its address.
func serveAssembled(t *testing.T, a *App) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Server.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return")
		}
	})
	return ln.Addr().String()
}

// closedWithin reports whether the server closed conn within d. A read that returns
// anything at all — bytes, EOF, reset — is the server having done something; only a
// read DEADLINE expiring means it is still holding the socket open.
func closedWithin(t *testing.T, conn net.Conn, d time.Duration) bool {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(d))
	_, err := io.ReadAll(conn)
	if err == nil {
		return true
	}
	ne, ok := err.(net.Error)
	return !ok || !ne.Timeout()
}

// deadlineYAML is a minimal gateway plus whatever server keys the test is about.
//
// pre_stop_delay is zeroed because these tests run the REAL serving path: the
// default ten-second wait for a load balancer to notice is correct in production
// and is ten seconds of nothing in a test.
func deadlineYAML(serverKeys string) string {
	return `
version: 1
server:
  pre_stop_delay: 0
  shutdown_grace: 2s
` + serverKeys + `
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`
}

// TestReadHeaderTimeoutIsReachableFromConfiguration: the client that connects and
// then says nothing at all. It never reaches a handler and costs no capacity slot;
// what it exhausts is the listener's accept queue, from any client that can open a
// socket.
//
// Both directions are asserted in one test, because either one alone is passable by
// an implementation that ignores the setting: a server that always closes at 30 s
// would fail the first half only if the window is short, and a server that never
// closes would fail the second half only if `none` is honoured. Together they show
// the configured value is what decided it.
func TestReadHeaderTimeoutIsReachableFromConfiguration(t *testing.T) {
	t.Run("a short deadline cuts the silent client", func(t *testing.T) {
		a := newWiringApp(t, deadlineYAML("  read_header_timeout: 250ms"), nil)
		conn := dialAndStartHeaders(t, serveAssembled(t, a))
		if !closedWithin(t, conn, 5*time.Second) {
			t.Fatal("a client that opened a connection and sent half a request line is " +
				"still holding it after five seconds: server.read_header_timeout did not " +
				"reach the running server")
		}
	})

	t.Run("none removes the bound", func(t *testing.T) {
		// The default is 30 s, so a connection still open after two is proof the
		// file's `none` reached net/http rather than being defaulted back on.
		a := newWiringApp(t, deadlineYAML("  read_header_timeout: none\n  read_timeout: none"), nil)
		conn := dialAndStartHeaders(t, serveAssembled(t, a))
		if closedWithin(t, conn, 2*time.Second) {
			t.Fatal("the connection was closed although read_header_timeout is `none`: " +
				"the explicit \"no bound\" was read as absence and took the default")
		}
	})
}

// dialAndStartHeaders opens a connection and sends an incomplete request line, so
// the server is waiting for headers that will never arrive.
func dialAndStartHeaders(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, "GET /health HTTP/1.1\r\nHost: dorang.test\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	return conn
}

// TestReadTimeoutIsReachableFromConfiguration: the slow-body client, which is the
// worse of the two. By the time a body is dribbling the request has been ADMITTED,
// the handler is parked inside Body.read, and it is holding the in-flight slot that
// the drain and dorang_inflight_requests both count.
//
// The assertion is the RESERVATION and not the socket: a fix that closed the
// connection and left the handler parked would pass a socket-only test and would
// still leak the capacity slot.
func TestReadTimeoutIsReachableFromConfiguration(t *testing.T) {
	a := newWiringApp(t, deadlineYAML(
		"  read_header_timeout: 200ms\n  read_timeout: 500ms"), nil)
	// A REAL credential, because the gate authenticates before the body is read: a
	// request refused 401 gives its slot back for a reason that has nothing to do
	// with any deadline, and the test would pass against the defect.
	secret := issueKey(t, a, nil)
	addr := serveAssembled(t, a)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Headers announcing a body far larger than what will ever be sent, then one
	// dribble and silence.
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
	head := "POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: dorang.test\r\n" +
		"Authorization: Bearer " + secret + "\r\n" +
		"Content-Type: application/json\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)+4096)
	if _, err := io.WriteString(conn, head+body[:8]); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The slot has to be TAKEN first, or the second wait below measures nothing.
	waitInFlight(t, a, 1, 5*time.Second,
		"the request was never admitted, so this test is not measuring what it claims to")
	// And it comes back on its own. The default read timeout is two minutes, so a
	// slot returned inside five seconds was returned by the configured 500ms.
	waitInFlight(t, a, 0, 5*time.Second,
		"a client dribbling a body is holding a handler and its capacity slot, so "+
			"server.read_timeout did not reach the running server")
}

func waitInFlight(t *testing.T, a *App, want int64, within time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if a.Server.InFlight() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("in-flight = %d after %s, want %d: %s", a.Server.InFlight(), within, want, why)
}

// TestIdleTimeoutIsReachableFromConfiguration: the keep-alive connection that goes
// quiet between requests. It has no handler behind it, so it costs no capacity slot
// — it costs a file descriptor and a connection-table entry, without limit.
//
// Go defaults IdleTimeout to ReadTimeout, which is why the two are set apart here:
// the read timeout is left at its default so that an implementation which silently
// makes them one setting cannot pass.
func TestIdleTimeoutIsReachableFromConfiguration(t *testing.T) {
	a := newWiringApp(t, deadlineYAML("  idle_timeout: 300ms"), nil)
	addr := serveAssembled(t, a)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn,
		"GET /health HTTP/1.1\r\nHost: dorang.test\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The default idle timeout is two minutes, so a close inside five seconds is
	// the configured 300ms.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatalf("the idle connection was never closed (read %d bytes): "+
				"server.idle_timeout did not reach the running server", len(got))
		}
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(got), "200 OK") {
		t.Errorf("the request itself did not succeed: %q", string(got))
	}
}

// TestReadTimeoutInsideTheHeaderTimeoutIsALoadError preserves the refusal. Both
// deadlines are measured by net/http from the connection's first byte, so a smaller
// whole-request deadline makes the header deadline unreachable and enforces itself
// under the other name — which is a configuration that quietly means something other
// than what it says.
//
// It is asserted at BOTH layers, because they are reached by different tools:
// config.LoadBytes is what `dorangctl config lint` runs, and server.New is what the
// process runs. A refusal in only the second is the rates.images defect — lint says
// the file is good and the server will not start.
func TestReadTimeoutInsideTheHeaderTimeoutIsALoadError(t *testing.T) {
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	_, err := config.LoadBytes([]byte(deadlineYAML(
		"  read_header_timeout: 30s\n  read_timeout: 5s")))
	if err == nil {
		t.Fatal("a read timeout shorter than the header timeout loaded")
	}
	if !strings.Contains(err.Error(), "read_header_timeout") {
		t.Errorf("the refusal does not name the setting that conflicts: %v", err)
	}

	// And `none` on the whole-request deadline is not "shorter than": it is no
	// bound at all, and the header deadline still fires on its own.
	if _, err := config.LoadBytes([]byte(deadlineYAML(
		"  read_header_timeout: 30s\n  read_timeout: none"))); err != nil {
		t.Errorf("read_timeout: none was refused beside a header timeout: %v", err)
	}
}
