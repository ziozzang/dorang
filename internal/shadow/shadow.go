package shadow

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ziozzang/dorang/internal/server"
)

// Shadower implements [server.Observer].
//
// Its whole contract with the request path is: decide fast, copy a bounded
// amount, and never wait. Everything expensive — the reference call, the
// structural diff, the report write — happens on a worker.
type Shadower struct {
	opts    Options
	ig      *ignoreSet
	sampler sampler
	budget  *dayBudget
	report  *reporter

	scheme, host, prefix string

	q       chan *job
	workers sync.WaitGroup
	stop    chan struct{}
	closing atomic.Bool

	// ctx is cancelled when a shutdown's grace period runs out, which is the
	// only thing that can free a worker waiting on a reference gateway that
	// does not answer. Without it, Close waits for the reference timeout —
	// which is the reference's decision, not dorang's.
	ctx    context.Context
	cancel context.CancelFunc

	m stats
}

// job is one queued comparison. Everything in it is copied out of the
// observation, because [server.Observation] borrows the pooled request and is
// invalid the moment Observe returns.
type job struct {
	requestID string
	method    string
	path      string
	rawQuery  string
	route     string
	model     string
	stream    bool

	reqHeader http.Header
	reqBody   []byte

	status     int
	respHeader http.Header
	head, tail []byte
	truncated  bool

	durationMS int64
	// estNanoUSD is what was reserved against the daily ceiling before this job
	// was queued, and what will be settled after the call returns.
	estNanoUSD int64
	unpriced   bool
}

// New builds a Shadower.
//
// It returns [ErrOff] for [ModeOff] rather than a working no-op, so that a
// caller wires nothing rather than paying for a mechanism it disabled. Use
// [NewOff] when a non-nil observer is wanted regardless.
func New(opts Options) (*Shadower, error) {
	if opts.Mode == ModeOff {
		return nil, ErrOff
	}
	opts.setDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	rep, err := newReporter(opts.ReportPath, opts.ReportWriter, opts.ReportMaxBytes)
	if err != nil {
		return nil, err
	}
	s := &Shadower{
		opts:    opts,
		ig:      newIgnoreSet(opts.IgnoreFields),
		sampler: newSampler(opts.SampleRate),
		budget:  newDayBudget(opts.MaxCostNanoUSDPerDay, opts.Now()),
		report:  rep,
		q:       make(chan *job, opts.QueueSize),
		stop:    make(chan struct{}),
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.scheme, s.host, s.prefix = opts.baseURL()

	s.workers.Add(opts.Workers)
	for i := 0; i < opts.Workers; i++ {
		go s.run()
	}
	return s, nil
}

// NewOff builds a Shadower that samples nothing. It exists so a caller can hold
// a non-nil [server.Observer] unconditionally; every method on it is a
// constant.
func NewOff() *Shadower {
	s := &Shadower{
		opts:    Options{Mode: ModeOff, Now: time.Now, Logf: func(string, ...any) {}},
		sampler: newSampler(0),
		budget:  newDayBudget(1, time.Now()),
		stop:    make(chan struct{}),
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

// Sample implements [server.Observer].
//
// It is on the request path, so it does the cheap checks first and in
// increasing order of cost: a mode test, a hash of the request id, then the
// daily ceiling, then the loop-guard header. The ceiling is checked here rather
// than at dispatch so that a capped day stops paying for the capture tap too.
//
// The determinism §14.1 asks for is only as good as the id. A client that sets
// one of the inbound call-id headers (COMPATIBILITY §7.8) gets exactly what the
// section describes: a retry carries the same id, hashes the same way, and is
// not shadowed a second time. A client that sets none gets a fresh id per
// request, so its retry is an independent draw — the sampler cannot recognize a
// retry it has no way to see. That is a property of the caller, not something
// this package can fix, and it is worth knowing before reading a cost report.
func (s *Shadower) Sample(requestID string, h http.Header) bool {
	if s.opts.Mode == ModeOff || s.closing.Load() {
		return false
	}
	if !s.sampler.admit(requestID) {
		return false
	}
	if s.budget.capped(s.opts.Now()) {
		s.m.skippedCapped.Add(1)
		return false
	}
	// A request that is itself a shadow copy is never shadowed again. Without
	// this, two gateways pointed at each other amplify one request without
	// bound — and "run alongside" (DESIGN §0.3) is exactly the arrangement that
	// produces two gateways pointed at each other.
	if h != nil && h.Get(HeaderShadow) != "" {
		s.m.skippedLoop.Add(1)
		return false
	}
	s.m.sampled.Add(1)
	return true
}

// Observe implements [server.Observer].
//
// It copies, reserves, and pushes. It never calls the reference, never writes
// the report, and never blocks: a full queue is a counted drop (DESIGN §9.6
// rule 3), because the alternative — waiting for a worker — would put a slow
// reference gateway on dorang's response path, which is the one thing §14.1
// forbids outright.
func (s *Shadower) Observe(ob *server.Observation) {
	if s.opts.Mode == ModeOff || s.closing.Load() {
		return
	}
	// Safety before money: a request that must not be replayed must not reserve
	// budget either, or a stream of them would spend the day's ceiling on calls
	// that are never made.
	if !replayable(ob.Method, ob.Family, ob.Path) {
		s.m.skippedUnsafe.Add(1)
		return
	}
	now := s.opts.Now()

	// Reserve before queueing, not after the call returns. A reservation taken
	// on completion lets every concurrent call see the same pre-spend balance
	// and pass — see dayBudget.
	est := ob.CostNanoUSD
	unpriced := !ob.Priced || est <= 0
	if unpriced {
		est = s.opts.UnpricedEstimateNanoUSD
		s.m.unpricedEstimates.Add(1)
	}
	if !s.budget.reserve(now, est) {
		s.m.skippedCapped.Add(1)
		return
	}

	j := &job{
		requestID:  ob.RequestID,
		method:     ob.Method,
		path:       ob.Path,
		rawQuery:   ob.RawQuery,
		route:      ob.Route,
		model:      ob.Model,
		stream:     ob.Stream,
		reqHeader:  cloneHeader(ob.RequestHeader),
		reqBody:    clampCopy(ob.RequestBody, s.opts.MaxCaptureBytes),
		status:     ob.Status,
		respHeader: cloneHeader(ob.ResponseHeader),
		head:       clampCopy(ob.ResponseHead, s.opts.MaxCaptureBytes),
		tail:       clampCopy(ob.ResponseTail, s.opts.MaxCaptureBytes),
		truncated:  ob.Truncated,
		durationMS: ob.Duration.Milliseconds(),
		estNanoUSD: est,
		unpriced:   unpriced,
	}
	// The copy budget can cut a window the server considered complete. Left
	// unmarked, the comparison would treat a prefix as the whole response and
	// report it clean — which is precisely the silent skip a cutover gate
	// cannot have.
	if len(j.head) < len(ob.ResponseHead) || len(j.tail) < len(ob.ResponseTail) {
		j.truncated = true
	}

	// A request body that did not fit the copy budget cannot be replayed
	// faithfully, and replaying a truncated body would compare the reference's
	// answer to a different question.
	if len(j.reqBody) < len(ob.RequestBody) {
		s.budget.settle(now, est, 0)
		s.m.skippedOversize.Add(1)
		return
	}

	select {
	case s.q <- j:
		s.m.queued.Add(1)
	default:
		// The queue is full. Give the reservation back — the call will not be
		// made, so charging for it would spend the day's ceiling on comparisons
		// that never happened and throttle the ones that would have.
		s.budget.settle(now, est, 0)
		s.m.dropped.Add(1)
	}
}

// run is one worker.
func (s *Shadower) run() {
	defer s.workers.Done()
	for {
		select {
		case j := <-s.q:
			s.process(j)
		case <-s.stop:
			// Drain what is already queued before exiting, so a shutdown does
			// not silently discard comparisons that were paid for.
			for {
				select {
				case j := <-s.q:
					s.process(j)
				default:
					return
				}
			}
		}
	}
}

// process makes the reference call and records what came back.
func (s *Shadower) process(j *job) {
	defer func() {
		if v := recover(); v != nil {
			s.m.panics.Add(1)
			s.opts.Logf("shadow: worker panicked, request unaffected: %v", v)
		}
	}()

	ctx, cancel := context.WithTimeout(s.ctx, s.opts.ReferenceTimeout)
	start := s.opts.Now()
	rp := s.call(ctx, j)
	elapsed := s.opts.Now().Sub(start)
	cancel()

	s.budget.settle(start, j.estNanoUSD, rp.costNanoUSD)
	s.m.sent.Add(1)

	rec := Record{
		Time:            rfc3339(start),
		RequestID:       j.requestID,
		Mode:            s.opts.Mode.String(),
		Route:           j.route,
		Method:          j.method,
		Path:            j.path,
		Model:           j.model,
		Stream:          j.stream,
		DorangStatus:    j.status,
		ReferenceStatus: rp.status,
		DorangMS:        j.durationMS,
		ReferenceMS:     elapsed.Milliseconds(),
		CostNanoUSD:     settledCost(j.estNanoUSD, rp.costNanoUSD),
		CostEstimated:   j.unpriced && rp.costNanoUSD < 0,
	}

	if rp.err != nil {
		// A reference that could not be reached is not a clean comparison and
		// must never be counted as one. It is written to the report as its own
		// kind of finding: during a migration, "the incumbent was unreachable
		// for 3% of the sample" is information, not noise.
		s.m.refErrors.Add(1)
		rec.ReferenceError = rp.err.Error()
		rec.Diffs = []Diff{{
			Path: "transport", Kind: KindTransport,
			Dorang: "responded", Reference: rp.err.Error(),
		}}
		s.report.write(&rec)
		return
	}

	if s.opts.Mode != ModeCompare || !s.opts.Structural {
		// mirror: the reference's result is recorded, nothing is compared.
		s.m.mirrored.Add(1)
		return
	}

	dShape := analyze(j.status, j.respHeader, j.head, j.tail, j.truncated, s.ig)
	rShape := analyze(rp.status, rp.header, rp.head, rp.tail, rp.truncated, s.ig)
	diffs, inc := compareShapes(dShape, rShape)

	s.m.compared.Add(1)
	switch {
	case len(diffs) > 0:
		s.m.withDiffs.Add(1)
		s.m.diffs.Add(uint64(len(diffs)))
	case len(inc) > 0:
		s.m.inconclusive.Add(1)
	default:
		s.m.clean.Add(1)
		return
	}
	rec.Diffs = diffs
	rec.Inconclusive = inc
	s.report.write(&rec)
}

// settledCost is what a shadow call is recorded as having cost.
func settledCost(est, actual int64) int64 {
	if actual < 0 {
		return est
	}
	return actual
}

// Close stops the workers, drains the queue within the grace period, and closes
// the report.
//
// It is safe to call twice, and safe to call while requests are in flight:
// Sample and Observe both go quiet the moment closing is set, so a shutdown
// cannot leave a worker holding a job nobody will drain.
//
// The grace period is the point. Waiting unconditionally for the workers means
// waiting for the reference gateway, and a reference that never answers would
// hold dorang's shutdown open for the whole reference timeout — letting a
// diagnostic decide how long a deploy takes. Past the grace, in-flight calls
// are cancelled and whatever is left in the queue is discarded.
func (s *Shadower) Close() error {
	if !s.closing.CompareAndSwap(false, true) {
		return nil
	}
	if s.stop != nil {
		close(s.stop)
	}
	done := make(chan struct{})
	go func() {
		s.workers.Wait()
		close(done)
	}()
	grace := s.opts.CloseGrace
	if grace <= 0 {
		grace = DefaultCloseGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		s.cancel()
		<-done
	}
	s.cancel()
	return s.report.close()
}

func cloneHeader(h http.Header) http.Header {
	if len(h) == 0 {
		return nil
	}
	return h.Clone()
}

// clampCopy copies at most max bytes. It always copies: the source belongs to
// the pooled request and is reused the moment Observe returns.
func clampCopy(b []byte, max int) []byte {
	if len(b) == 0 {
		return nil
	}
	n := len(b)
	if max > 0 && n > max {
		n = max
	}
	out := make([]byte, n)
	copy(out, b[:n])
	return out
}
