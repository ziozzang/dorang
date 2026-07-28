package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests in this file are about the SEQUENCE. The ones in drain_test.go
// check each step of the drain in isolation and all of them passed against a
// build in which the pre-stop delay was configured, documented and called by
// nothing: readiness went false, the listener eventually closed, and every
// individual assertion held. What none of them could see is that those two
// things happened in the same instant, which is the whole defect.

// nokeepalive is a client that opens a NEW connection for every request.
//
// This is not a detail. http.DefaultClient pools connections, and a pooled
// connection to a listener that has already closed still carries a request to
// completion — so a test that reuses one cannot tell "the listener is still
// accepting" from "the listener is gone and I am talking down an old socket",
// which is exactly the distinction every assertion below rests on.
var nokeepalive = &http.Client{
	Transport: &http.Transport{DisableKeepAlives: true},
	Timeout:   5 * time.Second,
}

// getFresh issues a GET over a brand-new connection.
func getFresh(addr, path string) (int, error) {
	resp, err := nokeepalive.Get("http://" + addr + path)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// postFresh issues a credentialed completion over a brand-new connection.
func postFresh(addr, body string) (int, error) {
	req, err := http.NewRequest(http.MethodPost,
		"http://"+addr+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderAuthorization, "Bearer good")
	resp, err := nokeepalive.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// TestPreStopDelayKeepsAcceptingWhileUnready is the ordering assertion the
// existing drain tests could not make.
//
// The contract is not "readiness went false" and it is not "the listener
// eventually closed" — both of those were already true of the broken build. It
// is that the two are SEPARATED: from the moment readiness first reports false
// over the wire, the listener must go on accepting new connections for the
// whole pre-stop delay, because that is the window in which a polling balancer
// is still routing here.
func TestPreStopDelayKeepsAcceptingWhileUnready(t *testing.T) {
	const delay = 600 * time.Millisecond
	s := newTestServer(t, func(o *Options) {
		o.PreStopDelay = delay
		o.ShutdownGrace = 5 * time.Second
	})

	ln := listen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx, ln) }()

	if code, err := getFresh(addr, "/health/readiness"); err != nil || code != http.StatusOK {
		t.Fatalf("readiness before the drain: %d, %v", code, err)
	}

	cancel()

	// Find the instant readiness first reports false ON THE WIRE, which is what
	// a balancer sees, rather than the instant the flag flips in memory.
	var unreadyAt time.Time
	for deadline := time.Now().Add(2 * time.Second); ; {
		code, err := getFresh(addr, "/health/readiness")
		if err != nil {
			t.Fatalf("the listener stopped accepting before readiness had even "+
				"reported false: %v — the balancer never got a chance to stop routing", err)
		}
		if code == http.StatusServiceUnavailable {
			unreadyAt = time.Now()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness never went false")
		}
	}

	// The window that matters. A margin is taken off the end so the assertion
	// is about the delay existing, not about the scheduler's precision.
	const margin = 200 * time.Millisecond
	served := 0
	for time.Since(unreadyAt) < delay-margin {
		code, err := postFresh(addr, `{"model":"model-x"}`)
		if err != nil {
			t.Fatalf("a new connection was refused %v after readiness went false, "+
				"inside a %v pre-stop delay: %v", time.Since(unreadyAt), delay, err)
		}
		if code != http.StatusOK {
			t.Fatalf("request during the pre-stop delay: %d, want 200", code)
		}
		served++

		// And readiness stays false throughout: the delay is a delay, not a
		// window in which the process changes its mind.
		rc, err := getFresh(addr, "/health/readiness")
		if err != nil {
			t.Fatalf("readiness probe refused during the pre-stop delay: %v", err)
		}
		if rc != http.StatusServiceUnavailable {
			t.Fatalf("readiness reported %d during the pre-stop delay, want 503", rc)
		}
	}
	if served == 0 {
		t.Fatal("the pre-stop window was never sampled; the test proves nothing")
	}

	// And it does end: the listener closes and the drain completes.
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned %v, want a clean drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the pre-stop delay")
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("the listener is still accepting after the drain finished")
	}
}

// TestNoPreStopDelayClosesImmediately pins the other half of the contract: zero
// means zero. A single node, a notebook, or anything not behind a balancer must
// not pay for a window it has no use for.
func TestNoPreStopDelayClosesImmediately(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.ShutdownGrace = 5 * time.Second })

	ln := listen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx, ln) }()

	if code, err := getFresh(addr, "/health/readiness"); err != nil || code != http.StatusOK {
		t.Fatalf("readiness before the drain: %d, %v", code, err)
	}
	start := time.Now()
	cancel()

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a zero pre-stop delay still waited")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the drain took %v with no pre-stop delay configured", d)
	}
}

// balancer is a load balancer as far as this test needs one: it discovers
// unreadiness by POLLING, which is the entire reason the pre-stop delay exists.
// A balancer that learned instantly would make the defect invisible.
type balancer struct {
	mu      sync.Mutex
	addrs   []string
	healthy []bool
	next    int
}

func newBalancer(addrs ...string) *balancer {
	b := &balancer{addrs: addrs, healthy: make([]bool, len(addrs))}
	for i := range b.healthy {
		b.healthy[i] = true
	}
	return b
}

// poll runs the health checks until stop is closed. Two consecutive failures
// take a backend out, which is what a readinessProbe failureThreshold means.
func (b *balancer) poll(period time.Duration, threshold int, stop <-chan struct{}) {
	fails := make([]int, len(b.addrs))
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		for i, a := range b.addrs {
			code, err := getFresh(a, "/health/readiness")
			if err != nil || code != http.StatusOK {
				fails[i]++
			} else {
				fails[i] = 0
			}
			b.mu.Lock()
			b.healthy[i] = fails[i] < threshold
			b.mu.Unlock()
		}
	}
}

// pick returns the next backend the balancer believes is healthy.
func (b *balancer) pick() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for range b.addrs {
		i := b.next % len(b.addrs)
		b.next++
		if b.healthy[i] {
			return b.addrs[i], true
		}
	}
	return "", false
}

// TestRollingRestartLosesNoRequests drives the whole thing end to end: two
// nodes, a balancer that polls, traffic in flight, and a signal to one of them.
//
// Zero failed requests at the client is the only acceptable result, and it is
// the claim R14 makes. Against a build where the listener closes in the same
// instant readiness flips, the balancer is still routing to the dying node for
// as long as its poll period, and every request it sends there is a connection
// refused.
func TestRollingRestartLosesNoRequests(t *testing.T) {
	const (
		pollPeriod = 40 * time.Millisecond
		threshold  = 2
		// Comfortably longer than pollPeriod x threshold plus the probe itself,
		// which is the relationship §13 states an operator must satisfy.
		preStop = 500 * time.Millisecond
	)

	type node struct {
		srv     *Server
		addr    string
		cancel  context.CancelFunc
		done    chan error
		drained bool
	}
	nodes := make([]*node, 2)
	for i := range nodes {
		s := newTestServer(t, func(o *Options) {
			o.PreStopDelay = preStop
			o.ShutdownGrace = 5 * time.Second
		})
		ln := listen(t)
		ctx, cancel := context.WithCancel(context.Background())
		n := &node{srv: s, addr: ln.Addr().String(), cancel: cancel, done: make(chan error, 1)}
		go func() { n.done <- s.Serve(ctx, ln) }()
		nodes[i] = n
	}
	defer func() {
		for _, n := range nodes {
			n.cancel()
			if !n.drained {
				<-n.done
			}
		}
	}()

	b := newBalancer(nodes[0].addr, nodes[1].addr)
	stopPoll := make(chan struct{})
	go b.poll(pollPeriod, threshold, stopPoll)
	defer close(stopPoll)

	var (
		ok       atomic.Int64
		failed   atomic.Int64
		firstErr atomic.Value
		noTarget atomic.Int64
	)
	stopLoad := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopLoad:
					return
				default:
				}
				addr, up := b.pick()
				if !up {
					// Every backend is out. That is the balancer's problem and
					// not a dropped request, but it must not happen here.
					noTarget.Add(1)
					time.Sleep(time.Millisecond)
					continue
				}
				code, err := postFresh(addr, `{"model":"model-x"}`)
				if err != nil || code != http.StatusOK {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, err)
					continue
				}
				ok.Add(1)
			}
		}()
	}

	// Let the traffic settle, restart one node, let the balancer converge.
	time.Sleep(300 * time.Millisecond)
	nodes[0].cancel()
	select {
	case err := <-nodes[0].done:
		nodes[0].drained = true
		if err != nil {
			t.Errorf("the restarted node drained with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the restarted node never finished draining")
	}
	time.Sleep(300 * time.Millisecond)
	close(stopLoad)
	wg.Wait()

	if n := failed.Load(); n != 0 {
		t.Errorf("%d of %d requests failed across a rolling restart, first error: %v",
			n, n+ok.Load(), firstErr.Load())
	}
	if ok.Load() < 20 {
		t.Errorf("only %d requests completed; the test did not exercise the restart", ok.Load())
	}
	if n := noTarget.Load(); n > 0 {
		t.Logf("the balancer had no healthy backend on %d attempts", n)
	}
}

// streamingDispatcher writes one SSE frame, flushes it, and then parks until
// the request context ends — a generation that outlives the grace period, which
// is the ordinary case for an LLM completion and the one §13 has to answer for.
func streamingDispatcher(started chan<- struct{}) Dispatcher {
	var once sync.Once
	return DispatchFunc(func(ctx context.Context, rq *Request, w http.ResponseWriter) error {
		rq.Result.Provider = "prov-1"
		rq.Result.Tokens = Usage{Input: 7, Output: 3, Total: 10}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		once.Do(func() { close(started) })
		<-ctx.Done()
		// A real handler does not unwind in zero time: it stops the upstream
		// read, finishes the frame it was mid-way through, and only then
		// returns to the deferred finish() that meters it. The whole purpose of
		// the drain's bounded cut-over wait is to cover that interval, so the
		// fake has to have one or the test cannot tell whether it is covered.
		time.Sleep(20 * time.Millisecond)
		return ctx.Err()
	})
}

// TestCutStreamEndsWithAnInBandError: at the grace boundary the client must be
// TOLD, in the stream it is already reading, that a restart cut it.
//
// A reset is the worst available answer. The status line is long gone, so the
// client cannot distinguish a deploy from a crash from a bad network — and it
// has already been billed for the tokens it received. §10.5 establishes that
// dorang rewrites streams as a matter of course; this is one more frame.
func TestCutStreamEndsWithAnInBandError(t *testing.T) {
	started := make(chan struct{})
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = streamingDispatcher(started)
		o.ShutdownGrace = 150 * time.Millisecond
	})

	ln := listen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx, ln) }()

	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost,
			"http://"+addr+"/v1/chat/completions",
			strings.NewReader(`{"model":"model-x","stream":true}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(HeaderAuthorization, "Bearer good")
		resp, err := nokeepalive.Do(req)
		if err != nil {
			res <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		res <- result{body: string(b), err: err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never started")
	}
	cancel()

	select {
	case err := <-serveErr:
		if err == nil {
			t.Fatal("Serve reported a clean drain, but the stream was cut")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}

	var r result
	select {
	case r = <-res:
	case <-time.After(5 * time.Second):
		t.Fatal("the client never finished reading")
	}
	if r.err != nil {
		t.Fatalf("the client's read failed with %v; a cut stream must end with a "+
			"frame, not an error the client cannot interpret", r.err)
	}
	if !strings.Contains(r.body, `"content":"hi"`) {
		t.Errorf("the frames written before the cut are missing:\n%s", r.body)
	}
	if !strings.Contains(r.body, "gateway_shutting_down") {
		t.Errorf("the stream does not name the reason it ended; a client cannot "+
			"tell a deploy from a crash:\n%s", r.body)
	}
	if !strings.HasSuffix(r.body, "data: [DONE]\n\n") {
		t.Errorf("the stream was not terminated:\n%s", r.body)
	}
}

// TestCutStreamIsStillMetered: work that was done and billed upstream but never
// recorded is silent revenue loss, and the grace boundary is precisely when it
// would happen — the drain is also when the spool is being flushed, so a
// handler still unwinding after Serve returns hands its event to a meter that
// has already closed.
func TestCutStreamIsStillMetered(t *testing.T) {
	started := make(chan struct{})
	m := &recordingMeter{}
	s := newTestServer(t, func(o *Options) {
		o.Dispatcher = streamingDispatcher(started)
		o.Meter = m
		o.ShutdownGrace = 150 * time.Millisecond
	})

	ln := listen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx, ln) }()

	go func() {
		req, _ := http.NewRequest(http.MethodPost,
			"http://"+addr+"/v1/chat/completions",
			strings.NewReader(`{"model":"model-x","stream":true}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(HeaderAuthorization, "Bearer good")
		resp, err := nokeepalive.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never started")
	}
	cancel()
	<-serveErr

	// By the time Serve returns, the ledger row exists. This is the assertion
	// that matters: the caller's very next act is to close the meter, so an
	// event that has not been handed over by now is an event that never will
	// be.
	m.mu.Lock()
	n := len(m.events)
	var ev Event
	if n > 0 {
		ev = m.events[n-1]
	}
	m.mu.Unlock()

	if n == 0 {
		t.Fatal("the cut stream was never metered: tokens the upstream billed for " +
			"left with the connection")
	}
	if ev.Model != "model-x" {
		t.Errorf("metered model %q, want model-x", ev.Model)
	}
	if ev.Result.Tokens.Total != 10 {
		t.Errorf("metered %d tokens, want the 10 the upstream had already produced",
			ev.Result.Tokens.Total)
	}
	if ev.RequestID == "" {
		t.Error("the metered event carries no request id, so it cannot be traced")
	}
}
