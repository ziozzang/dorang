package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

// drainPollInterval is how often the drain checks whether the last in-flight
// request has finished. Short enough that a healthy shutdown is not padded by
// a poll interval; long enough that a 30-second grace does not spin a core.
const drainPollInterval = 2 * time.Millisecond

// Serve serves ln until ctx is cancelled, then drains.
//
// The shutdown sequence is the one a rolling deploy needs, in this order:
//
//  1. Readiness goes false the instant the signal arrives, so the load balancer
//     stops sending new work while the process is still perfectly able to
//     answer it. This is the step that makes the rest possible; a process that
//     starts refusing before it stops being routed to just drops requests.
//  2. The listener closes, so nothing new arrives even from a client that
//     bypassed the balancer.
//  3. In-flight requests run to completion, up to the grace period.
//  4. Whatever is still running past the grace is cut, because a stuck stream
//     must not hold a deploy open forever.
//
// Serve returns nil on a clean drain and [context.DeadlineExceeded] when the
// grace expired with work still running — a distinction worth having in an exit
// code.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	hs := &http.Server{
		Handler: s,
		// The gateway's own request timeout bounds the handler; a
		// ReadHeaderTimeout bounds the client that connects and then says
		// nothing, which is a different failure and the one that exhausts a
		// listener's accept queue.
		ReadHeaderTimeout: 30 * time.Second,
	}
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
	drainErr := s.drain(hs)
	<-errc
	return drainErr
}

// ServeSignals is Serve with SIGINT and SIGTERM wired to the drain.
//
// It exists so that the signal handling is in one place and is the same code
// the drain test exercises, rather than something cmd/dorang reimplements and
// nobody tests.
func (s *Server) ServeSignals(ln net.Listener) error {
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return s.Serve(ctx, ln)
}

// Shutdown drains an already-running server. It is what Serve calls, exposed
// for a caller that owns its own [net/http.Server].
func (s *Server) Shutdown(ctx context.Context) error {
	s.draining.Store(true)
	return s.waitInFlight(ctx)
}

// StartDrain flips readiness to false without waiting. A deployment that wants
// a fixed pre-stop delay before the drain proper calls this, sleeps, then
// shuts down.
func (s *Server) StartDrain() { s.draining.Store(true) }

// drain runs the shutdown sequence against hs.
func (s *Server) drain(hs *http.Server) error {
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
	if waitErr != nil {
		// Past the grace with work still running: close hard so the process
		// can exit rather than waiting on a stream that may never end.
		_ = hs.Close()
	}
	return waitErr
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
