package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// drainPollInterval is how often the drain checks whether the last in-flight
// request has finished. Short enough that a healthy shutdown is not padded by
// a poll interval; long enough that a 30-second grace does not spin a core.
const drainPollInterval = 2 * time.Millisecond

// drainCutover is how long the drain waits, twice, after the grace has expired:
// once for a cancelled request to write its terminal error frame and unwind,
// and once more after the hard close for whatever ignored the cancellation.
//
// It is a constant rather than a setting because it does not describe the
// deployment, it describes one handler's unwind: write one frame, flush it,
// hand an event to the meter. Every one of those is bounded by microseconds,
// and the only reason it is a whole second is that the handler must first
// notice a cancelled context it may be parked on. Two of them fit inside the
// margin §13's termination-budget formula already carries.
const drainCutover = time.Second

// ErrShuttingDown is the cause a drain attaches to every in-flight request's
// context when the grace expires.
//
// It exists so that a handler's error can be told apart from the two things
// that look identical at the wire: the client hung up, and the upstream broke.
// A request cut by a planned restart is neither, and the client's retry
// decision differs for all three ([shutdownError] is what says so).
var ErrShuttingDown = errors.New("server: the gateway is shutting down")

// Serve serves ln until ctx is cancelled, then drains.
//
// The shutdown sequence is the one a rolling deploy needs, in this order:
//
//  1. Readiness goes false the instant the signal arrives, so the load balancer
//     stops sending new work while the process is still perfectly able to
//     answer it. This is the step that makes the rest possible; a process that
//     starts refusing before it stops being routed to just drops requests.
//  2. The process keeps serving for the pre-stop delay
//     ([Options.PreStopDelay]). A balancer discovers unreadiness by POLLING, so
//     between the flip and the poll that notices it there is a window in which
//     the balancer is still routing here. Closing the listener inside that
//     window is connection-refused at the client, which is the failure the
//     whole sequence exists to prevent. Nothing else changes during the delay:
//     requests arrive and are served exactly as before.
//  3. The listener closes, so nothing new arrives even from a client that
//     bypassed the balancer.
//  4. In-flight requests run to completion, up to the grace period.
//  5. Whatever is still running past the grace is CANCELLED with
//     [ErrShuttingDown] — which gives a stream one last frame naming the reason
//     and gives every request its ledger row — and only then cut, because a
//     stuck stream must not hold a deploy open forever and a client that is
//     merely reset cannot tell a deploy from a crash.
//
// Serve returns nil on a clean drain and [context.DeadlineExceeded] when the
// grace expired with work still running — a distinction worth having in an exit
// code.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	return s.serveListener(ctx, nil, ln)
}

// serveListener is [Server.Serve] with a channel that cuts the pre-stop delay
// short.
//
// impatient is the second SIGTERM: an operator who signals twice is saying "I
// know there is no balancer to wait for", and making them wait anyway is how a
// developer learns to reach for SIGKILL, which skips the drain entirely.
func (s *Server) serveListener(ctx context.Context, impatient <-chan struct{}, ln net.Listener) error {
	// Every in-flight request's context descends from this one, because
	// net/http derives the connection context from BaseContext and the request
	// context from that. It is the only handle the drain has on a handler that
	// is already running, and step 5 above is the whole reason it exists:
	// without it the only alternative to waiting forever is
	// [net/http.Server.Close], which severs the connection and tells the client
	// nothing.
	baseCtx, cancelInFlight := context.WithCancelCause(context.Background())
	defer cancelInFlight(ErrShuttingDown)

	hs := s.newHTTPServer(baseCtx)
	errc := make(chan error, 1)
	go func() {
		err := hs.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Steps 1 and 2. StartDrain only flips the readiness bit: the listener is
	// untouched, the route table is untouched, and a request that arrives
	// during the delay is served in full.
	s.StartDrain()
	if d := s.snap.Load().preStopDelay; d > 0 {
		t := time.NewTimer(d)
		select {
		case err := <-errc:
			// The listener died underneath us. There is nothing left to hold
			// open for the balancer's benefit.
			t.Stop()
			return err
		case <-impatient:
			t.Stop()
		case <-t.C:
		}
	}

	drainErr := s.drain(hs, cancelInFlight)
	<-errc
	return drainErr
}

// newHTTPServer builds the [net/http.Server] the listener is served with.
//
// It is a function rather than a literal inside serveListener so that the
// connection deadlines can be asserted as CONFIGURATION as well as behaviour: a
// deadline that is set correctly and never reaches net/http is the same outage
// as one that was never set.
func (s *Server) newHTTPServer(baseCtx context.Context) *http.Server {
	cfg := s.snap.Load()
	return &http.Server{
		Handler:     s,
		BaseContext: func(net.Listener) context.Context { return baseCtx },
		// The gateway's own request timeout bounds the handler; a
		// ReadHeaderTimeout bounds the client that connects and then says
		// nothing, which is a different failure and the one that exhausts a
		// listener's accept queue.
		ReadHeaderTimeout: cfg.readHeaderTimeout,
		// And ReadTimeout bounds that client's sibling, which is worse: the one
		// that sends complete headers and then DRIBBLES a body. It is past the
		// accept queue, past the gate, holding an in-flight slot and parked in
		// Body.read, and before this it held all of that for as long as it cared
		// to. net/http applies this to the whole request read and clears it when
		// the body hits EOF, so it costs a streaming response nothing: the body
		// is finished long before the first token comes back.
		ReadTimeout: cfg.readTimeout,
		// IdleTimeout is set explicitly because net/http's default for it is
		// ReadTimeout, and a keep-alive connection sitting between requests is a
		// different question from a request being read.
		IdleTimeout: cfg.idleTimeout,
		// Deliberately no WriteTimeout. It is measured from the start of the
		// request and would cut a legitimate long generation mid-stream, which
		// is the failure this gateway exists to avoid; the read half above is
		// the part that can be bounded without lying about how long an answer
		// takes.
	}
}

// ServeSignals is Serve with SIGINT and SIGTERM wired to the drain.
//
// It exists so that the signal handling is in one place and is the same code
// the drain test exercises, rather than something cmd/dorang reimplements and
// nobody tests.
//
// The FIRST signal starts the sequence [Server.Serve] documents. A SECOND
// signal skips the pre-stop delay and goes straight to the drain proper: on a
// workstation there is no balancer to wait for, and a Ctrl-C that appears to
// hang is a Ctrl-C the operator escalates to SIGKILL.
func (s *Server) ServeSignals(ln net.Listener) error {
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	impatient := make(chan struct{})
	// done releases the watcher whether or not the signals ever arrive, so that
	// a Serve which returned on its own does not leave a goroutine parked on a
	// channel nothing will write to (DESIGN §14: zero leaked goroutines).
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sigc:
		case <-done:
			return
		}
		cancel()
		select {
		case <-sigc:
			close(impatient)
		case <-done:
		}
	}()

	return s.serveListener(ctx, impatient, ln)
}

// Shutdown drains an already-running server. It is what Serve calls, exposed
// for a caller that owns its own [net/http.Server].
//
// It flips readiness and waits, and that is all it CAN do: the pre-stop delay
// and the cancellation that ends a cut stream with a named frame both need the
// listener and the [net/http.Server.BaseContext] this call does not own. A
// caller that wants the whole sequence calls [Server.Serve].
func (s *Server) Shutdown(ctx context.Context) error {
	s.draining.Store(true)
	return s.waitInFlight(ctx)
}

// StartDrain flips readiness to false without waiting. A deployment that wants
// a fixed pre-stop delay before the drain proper calls this, sleeps, then shuts
// down — which is what [Server.Serve] does with [Options.PreStopDelay].
func (s *Server) StartDrain() { s.draining.Store(true) }

// drain runs the shutdown sequence against hs.
func (s *Server) drain(hs *http.Server, cancelInFlight context.CancelCauseFunc) error {
	grace := s.snap.Load().shutdownGrace
	s.draining.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	// Shutdown closes the listener immediately and then waits for idle
	// connections; the in-flight wait below is what actually bounds the
	// handlers, and running them concurrently means a keep-alive connection
	// with no request on it does not add to the wall clock.
	shutErr := make(chan error, 1)
	go func() { shutErr <- hs.Shutdown(ctx) }()

	waitErr := s.waitInFlight(ctx)
	<-shutErr
	if waitErr == nil {
		return nil
	}

	// Past the grace with work still running.
	//
	// One grace covers streams and plain requests alike, and deliberately so. A
	// second, longer stream grace would buy nothing: the pod does not go away
	// until the LONGER window elapses, so the deployment's termination budget is
	// sized off that one either way, and the shorter window can only cut short
	// the requests that were going to finish first anyway — waitInFlight already
	// returns the instant the last one does, so a generous grace costs nothing
	// when nothing is slow. What a cut stream actually needs is not more time,
	// it is an ANSWER, which is the next two steps.
	//
	// So: cancel, naming the reason. A handler parked on an upstream read wakes
	// with a cancelled context, its error becomes the in-band frame
	// [shutdownError] describes, and — the part that is easy to miss — its
	// deferred finish() runs, so the tokens the upstream already billed for
	// reach the meter instead of leaving with the connection.
	if cancelInFlight != nil {
		cancelInFlight(ErrShuttingDown)
	}
	s.reap(drainCutover)

	// Whatever ignored the cancellation is cut; a deploy does not wait on it.
	// The second reap is not for the client, which is already gone — it is for
	// the ledger. Serve returns to a caller that closes the meter next, and a
	// handler still unwinding at that moment is billed work with no row.
	_ = hs.Close()
	s.reap(drainCutover)

	return waitErr
}

// reap waits up to d for the last handler to unwind. The outcome is not
// something a caller can act on — the deadline has already passed — so it is
// not returned; what matters is that the wait is bounded.
func (s *Server) reap(d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_ = s.waitInFlight(ctx)
}

// shutdownError is what a request cut by the drain reports.
//
// It reads the CAUSE rather than the server's draining flag, so it fires for
// exactly the requests the drain cancelled and not for a client that hung up
// during the same second. The code is machine-readable on purpose: "retry this
// against another node" and "this request is broken" are different client
// behaviours, and mid-stream there is no status line left to distinguish them.
func shutdownError(ctx context.Context) *Error {
	if ctx == nil || !errors.Is(context.Cause(ctx), ErrShuttingDown) {
		return nil
	}
	return NewError(http.StatusServiceUnavailable, TypeServiceUnavailable,
		"the gateway is shutting down for a restart; this request was interrupted and can be retried").
		WithCode("gateway_shutting_down")
}

// waitInFlight blocks until no request is being served or ctx expires.
func (s *Server) waitInFlight(ctx context.Context) error {
	if s.inflight.Load() == 0 {
		return nil
	}
	t := time.NewTicker(drainPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if s.inflight.Load() == 0 {
				return nil
			}
			return context.DeadlineExceeded
		case <-t.C:
			if s.inflight.Load() == 0 {
				return nil
			}
		}
	}
}
