package meter

import (
	"context"
	"sync"
)

// Sink is everything this package needs from persistence. It is deliberately
// narrow and defined here rather than in internal/store so that neither
// package imports the other: store implements Sink, wiring names both, and the
// dependency graph stays acyclic (DESIGN 9.1 -- the hot path does not touch
// the store, so the store must not be reachable from it either).
//
// Both methods may block; the meter never calls them from Record. A returned
// error means "not accepted": rollups are carried over and retried, traces
// stay in the spool and are re-offered. Neither is discarded on error, so an
// implementation must be prepared to see the same batch again and should be
// idempotent on (Key, HourStart) and on RequestID respectively.
//
// Implementations must not retain the slices past the call.
type Sink interface {
	WriteRollups(ctx context.Context, buckets []Bucket) error
	WriteTraces(ctx context.Context, traces []Trace) error
}

// nopSink accepts and discards. It is the default when Config.Sink is nil,
// which makes a zero Config a working meter and makes it possible to benchmark
// the producer without a consumer in the measurement.
type nopSink struct{}

func (nopSink) WriteRollups(context.Context, []Bucket) error { return nil }
func (nopSink) WriteTraces(context.Context, []Trace) error   { return nil }

// NopSink returns a Sink that accepts everything and keeps nothing.
func NopSink() Sink { return nopSink{} }

// MemSink is an in-memory Sink for tests and for the notebook profile's
// "metering with nowhere to put it" case. It copies what it is given, so a
// caller reusing its slices cannot corrupt the record.
//
// MemSink is safe for concurrent use. The Block/Unblock and error hooks exist
// so tests can hold the sink still and observe what the two paths do when a
// store stalls -- the property the whole design turns on.
type MemSink struct {
	mu      sync.Mutex
	buckets []Bucket
	traces  []Trace

	rollupErr error
	traceErr  error

	gate    chan struct{} // non-nil while blocked
	gateAll bool          // block rollups too, not just traces

	rollupCalls int
	traceCalls  int
}

// NewMemSink returns an empty MemSink.
func NewMemSink() *MemSink { return &MemSink{} }

// WriteRollups records buckets, honouring the configured error and gate.
func (s *MemSink) WriteRollups(ctx context.Context, buckets []Bucket) error {
	s.mu.Lock()
	s.rollupCalls++
	gate, all, err := s.gate, s.gateAll, s.rollupErr
	s.mu.Unlock()

	if gate != nil && all {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.buckets = append(s.buckets, buckets...)
	s.mu.Unlock()
	return nil
}

// WriteTraces records traces, honouring the configured error and gate.
func (s *MemSink) WriteTraces(ctx context.Context, traces []Trace) error {
	s.mu.Lock()
	s.traceCalls++
	gate, err := s.gate, s.traceErr
	s.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.traces = append(s.traces, traces...)
	s.mu.Unlock()
	return nil
}

// Buckets returns a copy of every bucket written so far.
func (s *MemSink) Buckets() []Bucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Bucket, len(s.buckets))
	copy(out, s.buckets)
	return out
}

// Traces returns a copy of every trace written so far.
func (s *MemSink) Traces() []Trace {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Trace, len(s.traces))
	copy(out, s.traces)
	return out
}

// Totals collapses every written bucket into one, ignoring keys and hours. It
// is what the numeric-accuracy tests compare against.
func (s *MemSink) Totals() Bucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	var t Bucket
	for i := range s.buckets {
		t.addBucket(&s.buckets[i])
	}
	return t
}

// Calls returns how many times each method has been entered.
func (s *MemSink) Calls() (rollups, traces int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rollupCalls, s.traceCalls
}

// Block makes WriteTraces (and, if all, WriteRollups) park until Unblock.
func (s *MemSink) Block(all bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gate == nil {
		s.gate = make(chan struct{})
	}
	s.gateAll = all
}

// Unblock releases everything parked in Block.
func (s *MemSink) Unblock() {
	s.mu.Lock()
	gate := s.gate
	s.gate = nil
	s.gateAll = false
	s.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

// SetErrors makes subsequent writes fail with the given errors. Nil clears.
func (s *MemSink) SetErrors(rollup, trace error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollupErr, s.traceErr = rollup, trace
}

// Reset discards everything recorded, keeping the error and gate settings.
func (s *MemSink) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets = nil
	s.traces = nil
	s.rollupCalls, s.traceCalls = 0, 0
}
