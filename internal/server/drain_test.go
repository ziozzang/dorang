package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// listen opens a loopback listener for the drain tests.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// blockingDispatcher holds a request until release is closed.
func blockingDispatcher(entered chan<- struct{}, release <-chan struct{}) Dispatcher {
	var once bool
	return DispatchFunc(func(_ context.Context, rq *Request, w http.ResponseWriter) error {
		if !once {
			once = true
			close(entered)
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"finished":true}`)
		return err
	})
}

// TestDrainOrdering is the whole shutdown contract, in order.
//
// The step that matters is the first one: readiness goes false while the
// process is still perfectly able to answer, so the load balancer stops routing
// to it *before* it stops working. A process that starts refusing before it
// stops being routed to does not drain — it drops.
func TestDrainOrdering(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = blockingDispatcher(entered, release)
		o.ShutdownGrace = 10 * time.Second
	})

	ln := listen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx, ln) }()

	// Readiness is true before the drain.
	if code := probe(t, addr, "/health/readiness"); code != http.StatusOK {
		t.Fatalf("readiness before drain: %d, want 200", code)
	}

	// Put one request in flight and wait until it is actually inside the
	// dispatcher, so the drain has something to wait for.
	inflight := make(chan int, 1)
	go func() {
		resp, err := authedPost(addr, `{"model":"model-x"}`)
		if err != nil {
			inflight <- -1
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if string(b) != `{"finished":true}` {
			inflight <- -2
			return
		}
		inflight <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the dispatcher")
	}

	// Signal the drain. Readiness must be false immediately — before the
	// in-flight request finishes, which is the point.
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if !s.Ready() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness did not go false when the drain started")
		}
		time.Sleep(time.Millisecond)
	}
	// The health handler agrees, and says so with a 503 rather than an error
	// envelope: a probe reads the status, not the body.
	w := do(s, httptest.NewRequest(http.MethodGet, "/health/readiness", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness handler during drain: %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "draining") {
		t.Errorf("readiness body %q", w.Body.String())
	}
	// Liveness stays 200: the process is alive, and killing it mid-drain is
	// exactly what a liveness failure would cause.
	w = do(s, httptest.NewRequest(http.MethodGet, "/health/liveness", nil))
	if w.Code != http.StatusOK {
		t.Errorf("liveness during drain: %d, want 200", w.Code)
	}
	if s.InFlight() == 0 {
		t.Error("the in-flight request was not counted")
	}

	// Let it finish; the drain completes cleanly.
	close(release)
	select {
	case code := <-inflight:
		if code != http.StatusOK {
			t.Fatalf("in-flight request finished with %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request never completed")
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned %v, want a clean drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the drain")
	}
}

// authedPost issues a credentialed chat-completions request over the loopback
// listener.
func authedPost(addr, body string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost,
		"http://"+addr+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderAuthorization, "Bearer good")
	return http.DefaultClient.Do(req)
}

// probe issues a GET and returns the status.
func probe(t *testing.T, addr, path string) int {
	t.Helper()
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestDrainGraceExpires: a stuck stream must not hold a deploy open forever, so
// the grace is a bound and not a hope. The distinct error exists so an operator
// can tell a clean shutdown from a cut one in an exit code.
func TestDrainGraceExpires(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)

	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = blockingDispatcher(entered, release)
		o.ShutdownGrace = 100 * time.Millisecond
	})

	ln := listen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx, ln) }()

	go func() {
		resp, err := authedPost(addr, `{"model":"model-x"}`)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the dispatcher")
	}

	cancel()
	select {
	case err := <-serveErr:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Serve returned %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the grace expired")
	}
}

// TestSIGTERMDrains exercises the signal wiring itself rather than a stand-in
// for it. signal.NotifyContext replaces the default terminate action for the
// duration, so sending the signal to this process is safe once ServeSignals is
// listening — which the successful probe below establishes.
func TestSIGTERMDrains(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.ShutdownGrace = 5 * time.Second })

	ln := listen(t)
	addr := ln.Addr().String()
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.ServeSignals(ln) }()

	if code := probe(t, addr, "/health/readiness"); code != http.StatusOK {
		t.Fatalf("readiness: %d", code)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("ServeSignals returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SIGTERM did not drain the server")
	}
	if s.Ready() {
		t.Error("readiness is still true after a SIGTERM drain")
	}
}

// TestStartDrainIsIndependentOfShutdown supports a deployment that wants a
// fixed pre-stop delay: flip readiness, let the balancer notice, then stop.
func TestStartDrainIsIndependentOfShutdown(t *testing.T) {
	s := newTestServer(t, nil)
	if !s.Ready() {
		t.Fatal("a fresh server is not ready")
	}
	s.StartDrain()
	if s.Ready() {
		t.Fatal("StartDrain did not flip readiness")
	}
	// Requests still work: the point of the delay is that the process keeps
	// serving while the balancer catches up.
	if code := do(s, post("/v1/chat/completions", `{"model":"model-x"}`)).Code; code != http.StatusOK {
		t.Fatalf("status %d during the pre-stop delay", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
