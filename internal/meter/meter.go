package meter

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// Meter is the split metering front end of DESIGN 12.1. A nil *Meter and a
// Meter from Off() are both valid and cost a predictable branch, so a caller
// never has to guard the call site.
type Meter struct {
	off bool

	cfg     Config
	now     func() time.Time
	sink    Sink
	maxKeys int
	evict   uint8

	shards []shard
	mask   uint64
	// rr rotates the shard scan start after a collision. It is only touched
	// on the contended path, so the uncontended hot path performs no atomic
	// read-modify-write on shared memory at all.
	rr atomic.Uint64

	ring *ring
	smpl sampler
	sp   spooler

	// numMu serialises numeric flushes; drainMu the ring-to-spool drain;
	// shipMu the spool-to-sink ship. Three locks, not one, because a sink
	// stalled inside WriteTraces must not be able to stop either of the other
	// two -- that separation is the entire point of the split.
	numMu   sync.Mutex
	pending []Bucket
	scratch map[bucketKey]int
	// carryDir is where the numeric carry-over is kept durably, empty when no
	// spool directory is configured. carryOnDisk records whether a file is
	// currently there, so a healthy meter pays no syscall per flush. Both are
	// guarded by numMu.
	carryDir    string
	carryOnDisk bool

	drainMu  sync.Mutex
	drainBuf []Trace

	shipMu  sync.Mutex
	shipBuf []Trace

	// wake lets a burst poke the drainer without waiting for its tick. Only
	// one Record per drain cycle wins the CAS and performs the channel send,
	// so the channel lock cannot become the contended lock we are avoiding.
	wake     chan struct{}
	wakeFlag atomic.Uint32
	wakeAt   uint64

	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error

	st        statsCounters
	degraded  atomic.Uint32
	dropsSeen atomic.Int64
}

type statsCounters struct {
	recorded            atomic.Int64
	tracesRecorded      atomic.Int64
	tracesSampledOut    atomic.Int64
	tracesBudgetDropped atomic.Int64
	droppedQueue        atomic.Int64
	droppedSpool        atomic.Int64
	spooled             atomic.Int64
	tracesFlushed       atomic.Int64
	bucketsFlushed      atomic.Int64
	pendingBuckets      atomic.Int64
	pendingFolds        atomic.Int64
	overflowFolds       atomic.Int64
	flushErrors         atomic.Int64
	shardCollisions     atomic.Int64
}

// Off returns a meter that records nothing. It exists so "metering disabled"
// is the same code path with the same call sites as metering enabled, which is
// what makes BenchmarkRecordOff a fair comparison rather than a different
// program.
func Off() *Meter { return &Meter{off: true} }

// New builds a Meter from cfg. It never fails: an unusable value is clamped to
// its default and an unusable spool directory degrades to an in-memory spool
// with Degraded raised. Metering must not be the reason a gateway refuses to
// start.
func New(cfg Config) *Meter {
	cfg = cfg.withDefaults()

	m := &Meter{
		cfg:     cfg,
		now:     cfg.Now,
		sink:    cfg.Sink,
		maxKeys: cfg.MaxKeysPerShard,
		evict:   uint8(cfg.EvictAfterFlushes),
		shards:  make([]shard, cfg.Shards),
		mask:    uint64(cfg.Shards - 1),
		scratch: make(map[bucketKey]int, cfg.Shards*8),
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
	}
	initSampler(&m.smpl, cfg.SampleRate, cfg.DailyByteBudget)
	for i := range m.shards {
		initShard(&m.shards[i], cfg.MaxKeysPerShard)
	}
	m.ring = newRing(cfg.TraceQueueSize, cfg.excerptBytes(), cfg.ExcerptMode, cfg.ExcerptChars)
	m.wakeAt = uint64(cfg.TraceQueueSize / 2)
	m.drainBuf = make([]Trace, 0, min(cfg.TraceQueueSize, 1024))
	m.shipBuf = make([]Trace, 0, cfg.SpoolBatch)

	if cfg.SpoolDir != "" {
		sp, err := openDiskSpool(cfg.SpoolDir, cfg.SpoolMaxBytes, cfg.SpoolSegmentBytes, cfg.SpoolSync)
		if err != nil {
			m.sp = newMemSpool(cfg.SpoolMaxBytes)
			m.setDegraded(ReasonSpoolError)
		} else {
			m.sp = sp
			m.carryDir = cfg.SpoolDir
		}
	} else {
		m.sp = newMemSpool(cfg.SpoolMaxBytes)
	}

	// Rollups a previous process could not ship are replayed into the
	// carry-over before anything new is counted, so they merge with this
	// process's first flush rather than arriving as a second row for the same
	// hour. It is the numeric path's half of the restart guarantee the trace
	// spool already made.
	if m.carryDir != "" {
		kept, err := readCarry(m.carryDir)
		switch {
		case err != nil:
			m.setDegraded(ReasonSpoolError)
		case len(kept) > 0:
			m.pending = kept
			m.carryOnDisk = true
			m.st.pendingBuckets.Store(int64(len(kept)))
		}
	}

	if cfg.FlushInterval > 0 {
		m.wg.Add(1)
		go m.numericLoop(cfg.FlushInterval)
	}
	if cfg.DrainInterval > 0 {
		m.wg.Add(1)
		go m.drainLoop(cfg.DrainInterval)
	}
	if cfg.ShipInterval > 0 {
		m.wg.Add(1)
		go m.shipLoop(cfg.ShipInterval)
	}
	return m
}

// ------------------------------------------------------------------ hot path

// stackHint is a goroutine-affinity hint: the address of a stack local.
// Concurrent goroutines have distinct stacks, so distinct hints; the same
// goroutine gets a stable hint until its stack is copied on growth. Shifting
// by 11 discards the offset-within-frame bits, which are identical across
// goroutines, and keeps the bits that distinguish one stack from another --
// the minimum goroutine stack is 2 KiB, so those start at bit 11.
//
// runtime.procPin is not available outside the runtime, and an atomic
// round-robin counter would put a contended cache line on the hot path. This
// costs nothing and is only a hint: shard selection below verifies with
// TryLock, so a degenerate hint costs a fan-out, never a wrong count.
func stackHint() uint64 {
	var x byte
	return uint64(uintptr(unsafe.Pointer(&x))) >> 11
}

// lockShard returns a shard with its mutex held.
func (m *Meter) lockShard() *shard {
	i := stackHint() & m.mask
	s := &m.shards[i]
	if s.mu.TryLock() {
		return s
	}
	m.st.shardCollisions.Add(1)

	i = (i + m.rr.Add(1)) & m.mask
	for n := uint64(0); n <= m.mask; n++ {
		s = &m.shards[i]
		if s.mu.TryLock() {
			return s
		}
		i = (i + 1) & m.mask
	}
	// Every shard held at once. Reachable only with more simultaneous Record
	// calls in flight than there are shards, each inside a critical section
	// of a few tens of nanoseconds. Blocking here is still correct, and it is
	// still better than dropping the count.
	s = &m.shards[i]
	s.mu.Lock()
	return s
}

// Record accounts one completed request. It does not block, does not allocate,
// and does not take a contended lock; see the package documentation for how
// each of those is achieved and TestRecordZeroAlloc for the proof of the
// second.
//
// The numeric half is always kept. The trace half is kept only if the event
// carries a request id, survives sampling and fits the daily byte budget --
// and if it is dropped for any other reason, the drop is counted and Degraded
// rises.
func (m *Meter) Record(ev Event) {
	if m == nil || m.off {
		return
	}

	ts := ev.Time
	if ts.IsZero() {
		ts = m.now()
	}
	unix := ts.Unix()
	hour := unix - mod(unix, 3600)

	k := bucketKey{Key: ev.key(), hour: hour}

	s := m.lockShard()
	if c := s.slotFor(k, m.maxKeys); c != nil {
		c.add(&ev)
	}
	s.mu.Unlock()

	m.st.recorded.Add(1)

	if ev.Trace.RequestID != "" || ev.Trace.TraceID != "" {
		m.recordTrace(&ev, ts)
	}
}

func mod(a, b int64) int64 {
	r := a % b
	if r < 0 {
		r += b
	}
	return r
}

func (m *Meter) recordTrace(ev *Event, ts time.Time) {
	id := ev.Trace.RequestID
	if id == "" {
		id = ev.Trace.TraceID
	}
	if !m.smpl.admit(id) {
		m.st.tracesSampledOut.Add(1)
		return
	}
	excerpt := ev.Trace.Excerpt
	if m.cfg.ExcerptMode == ExcerptNone {
		excerpt = ""
	}
	if n := m.ring.excSize; len(excerpt) > n {
		excerpt = excerpt[:n]
	}
	size := estimateTraceBytes(&ev.Trace, len(excerpt))
	if !m.smpl.charge(ts.Unix(), size) {
		m.st.tracesBudgetDropped.Add(1)
		return
	}

	t := Trace{
		RequestID:       ev.Trace.RequestID,
		TraceID:         ev.Trace.TraceID,
		SpanID:          ev.Trace.SpanID,
		ParentSpanID:    ev.Trace.ParentSpanID,
		Time:            ts,
		APIKeyID:        ev.APIKeyID,
		SecretID:        ev.SecretID,
		UserID:          ev.UserID,
		TeamID:          ev.TeamID,
		ModelGroup:      ev.ModelGroup,
		Provider:        ev.Provider,
		CredentialID:    ev.CredentialID,
		Endpoint:        ev.Endpoint,
		UpstreamModel:   ev.Trace.UpstreamModel,
		Status:          ev.Status,
		Latency:         ev.Latency,
		TTFT:            ev.TTFT,
		QueueWait:       ev.Trace.QueueWait,
		RouteTime:       ev.Trace.RouteTime,
		CapacityWait:    ev.Trace.CapacityWait,
		UpstreamConnect: ev.Trace.UpstreamConnect,
		Tokens:          ev.Tokens,
		CostNano:        ev.CostNano,
		Retries:         ev.Trace.Retries,
		FallbackReason:  ev.Trace.FallbackReason,
		ErrorMessage:    ev.Trace.ErrorMessage,
	}
	if !m.ring.push(&t, excerpt) {
		m.smpl.refund(size)
		m.st.droppedQueue.Add(1)
		m.setDegraded(ReasonQueueFull)
		return
	}
	m.st.tracesRecorded.Add(1)

	if m.ring.depth() >= m.wakeAt && m.wakeFlag.CompareAndSwap(0, 1) {
		select {
		case m.wake <- struct{}{}:
		default:
			m.wakeFlag.Store(0)
		}
	}
}

// ------------------------------------------------------------------ flushing

// Flush merges the accumulator and writes rollups, then drains the trace ring
// to the spool and offers the spool to the sink. It is safe to call
// concurrently with Record and with the background loops.
//
// The two paths are flushed in order but not atomically: a failure on one is
// reported without preventing the other. Errors are joined so a caller can
// tell which happened with errors.Is.
func (m *Meter) Flush(ctx context.Context) error {
	if m == nil || m.off {
		return nil
	}
	errNum := m.flushNumeric(ctx)
	errDrain := m.drain()
	errShip := m.ship(ctx)
	return errors.Join(errNum, errDrain, errShip)
}

func (m *Meter) flushNumeric(ctx context.Context) error {
	m.numMu.Lock()
	defer m.numMu.Unlock()
	// Whatever this flush decides, the carry-over on disk matches the
	// carry-over in memory by the time the lock is released. That is what makes
	// the promise "nothing on this path is droppable" survive the process:
	// counters the sink refused are on disk, not merely in a slice.
	defer m.persistPendingLocked()

	seed := m.pending
	m.pending = nil
	buckets, folds := mergeShards(m.shards, m.scratch, seed, m.evict)
	if folds > 0 {
		m.st.overflowFolds.Add(folds)
	}
	if len(buckets) == 0 {
		return nil
	}
	if err := m.sink.WriteRollups(ctx, buckets); err != nil {
		// Never discard. Carry over, bounded, and fold the tail if the
		// carry-over itself hits its cap.
		kept, pf := foldPending(buckets, m.cfg.MaxPendingBuckets)
		m.pending = kept
		m.st.pendingBuckets.Store(int64(len(kept)))
		if pf > 0 {
			m.st.pendingFolds.Add(pf)
		}
		m.st.flushErrors.Add(1)
		return err
	}
	m.st.pendingBuckets.Store(0)
	m.st.bucketsFlushed.Add(int64(len(buckets)))
	return nil
}

// drain empties the trace ring into the spool. It touches the sink not at all,
// which is what lets a stalled store cost disk instead of data.
func (m *Meter) drain() error {
	if m == nil || m.off {
		return nil
	}
	m.drainMu.Lock()
	defer m.drainMu.Unlock()
	m.wakeFlag.Store(0)

	for {
		buf := m.drainBuf[:0]
		for len(buf) < cap(m.drainBuf) {
			var t Trace
			if !m.ring.pop(&t) {
				break
			}
			buf = append(buf, t)
		}
		m.drainBuf = buf
		if len(buf) == 0 {
			return nil
		}
		n, err := m.sp.Append(buf)
		if n > 0 {
			m.st.spooled.Add(int64(n))
		}
		if err != nil {
			if lost := int64(len(buf) - n); lost > 0 {
				m.st.droppedSpool.Add(lost)
				if errors.Is(err, ErrSpoolFull) {
					m.setDegraded(ReasonSpoolFull)
				} else {
					m.setDegraded(ReasonSpoolError)
				}
			}
			return err
		}
	}
}

// ship offers spooled traces to the sink in batches, acknowledging each batch
// only after the sink has accepted it.
func (m *Meter) ship(ctx context.Context) error {
	if m == nil || m.off {
		return nil
	}
	m.shipMu.Lock()
	defer m.shipMu.Unlock()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := m.sp.Next(m.cfg.SpoolBatch, m.shipBuf[:0])
		m.shipBuf = batch
		if err != nil {
			m.setDegraded(ReasonSpoolError)
			return err
		}
		if len(batch) == 0 {
			break
		}
		if err := m.sink.WriteTraces(ctx, batch); err != nil {
			m.st.flushErrors.Add(1)
			m.setDegraded(ReasonSinkError)
			return err
		}
		if err := m.sp.Commit(); err != nil {
			m.setDegraded(ReasonSpoolError)
			return err
		}
		m.st.tracesFlushed.Add(int64(len(batch)))
	}
	m.maybeRecover()
	return nil
}

// maybeRecover clears Degraded once a whole ship cycle has completed with the
// spool empty and no new drop since the previous cycle. One cycle of
// hysteresis keeps a steady drop rate from flapping the health signal.
func (m *Meter) maybeRecover() {
	total := m.st.droppedQueue.Load() + m.st.droppedSpool.Load()
	if total == m.dropsSeen.Load() {
		m.degraded.Store(uint32(ReasonNone))
		return
	}
	m.dropsSeen.Store(total)
}

func (m *Meter) setDegraded(r Reason) {
	m.degraded.Store(uint32(r))
}

// Degraded reports whether metering is currently losing or failing to ship
// data, and why. This is what the health endpoint and the metering_degraded
// metric of DESIGN 12.3 read.
//
// Sampling and byte-budget exclusions never set it: those are the configured
// policy doing its job, and conflating them with failure would make the signal
// useless on any deployment that samples.
func (m *Meter) Degraded() (bool, Reason) {
	if m == nil || m.off {
		return false, ReasonNone
	}
	r := Reason(m.degraded.Load())
	return r != ReasonNone, r
}

// Stats snapshots the meter's own accounting.
//
// Every field is read from an atomic or from the spool's own short critical
// section; Stats never waits on the flush locks. That is deliberate: Stats and
// Degraded are what the health and metrics endpoints call, and an endpoint
// that blocks whenever the store is wedged is exactly useless at the moment it
// is needed. The counters are consistent individually, not as a set.
func (m *Meter) Stats() Stats {
	if m == nil || m.off {
		return Stats{}
	}
	dq := m.st.droppedQueue.Load()
	ds := m.st.droppedSpool.Load()
	recs, bytes := m.sp.Depth()
	deg, reason := m.Degraded()

	return Stats{
		Recorded:            m.st.recorded.Load(),
		TracesRecorded:      m.st.tracesRecorded.Load(),
		TracesSampledOut:    m.st.tracesSampledOut.Load(),
		TracesBudgetDropped: m.st.tracesBudgetDropped.Load(),
		TracesDroppedQueue:  dq,
		TracesDroppedSpool:  ds,
		TracesDropped:       dq + ds,
		TracesSpooled:       m.st.spooled.Load(),
		TracesFlushed:       m.st.tracesFlushed.Load(),
		BucketsFlushed:      m.st.bucketsFlushed.Load(),
		PendingBuckets:      m.st.pendingBuckets.Load(),
		PendingFolds:        m.st.pendingFolds.Load(),
		OverflowFolds:       m.st.overflowFolds.Load(),
		QueueDepth:          int64(m.ring.depth()),
		SpoolDepth:          recs,
		SpoolBytes:          bytes,
		FlushErrors:         m.st.flushErrors.Load(),
		ShardCollisions:     m.st.shardCollisions.Load(),
		Shards:              len(m.shards),
		Degraded:            deg,
		Reason:              reason,
	}
}

// Close stops the background loops and makes a bounded final attempt to flush.
//
// Whatever the sink does, both paths land on disk: the ring is drained onto the
// trace spool, the counters the sink would not take are written to the numeric
// carry-over file, and the spool is closed cleanly. A clean shutdown against an
// unreachable store therefore loses no counter and no spooled trace — the next
// process start replays both (see carry.go and openDiskSpool).
//
// The one thing that forfeits this is running with no Config.SpoolDir, which
// forfeits the trace spool in the same way and for the same reason.
func (m *Meter) Close() error {
	if m == nil || m.off {
		return nil
	}
	m.closeOnce.Do(func() {
		close(m.stop)
		m.wg.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.CloseTimeout)
		defer cancel()

		errNum := m.flushNumeric(ctx)
		errDrain := m.drain()
		errShip := m.ship(ctx)
		errSync := m.sp.Sync()
		errClose := m.sp.Close()
		m.closeErr = errors.Join(errNum, errDrain, errShip, errSync, errClose)
	})
	return m.closeErr
}

// ----------------------------------------------------------------- the loops

// stopCtx returns a context cancelled when the meter is closed, so a loop
// parked inside a sink call is interrupted by Close instead of making Close
// wait out the sink. The watcher is part of the WaitGroup, so Close does not
// return while it is still alive.
func (m *Meter) stopCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		<-m.stop
		cancel()
	}()
	return ctx, cancel
}

func (m *Meter) numericLoop(every time.Duration) {
	defer m.wg.Done()
	ctx, cancel := m.stopCtx()
	defer cancel()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			_ = m.flushNumeric(ctx)
		}
	}
}

func (m *Meter) drainLoop(every time.Duration) {
	defer m.wg.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
		case <-m.wake:
		}
		_ = m.drain()
	}
}

func (m *Meter) shipLoop(every time.Duration) {
	defer m.wg.Done()
	ctx, cancel := m.stopCtx()
	defer cancel()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
		}
		_ = m.ship(ctx)
	}
}
