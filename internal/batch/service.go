package batch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"
)

// Service implements the Batch and Files surface.
//
// # Locking
//
// s.mu serializes every read-modify-write of a batch *record* and every
// lifecycle decision that races with one — which is what makes "cancel while
// queued is immediate" and "cancel while dispatching drains" two branches of one
// atomic decision rather than a race. It is never held across row execution, and
// row records are written without it, so the hot path does not touch it.
//
// The store is the source of truth. Nothing about a batch is cached in memory
// except the run's lifecycle phase, which by construction cannot be persisted:
// it describes this process's relationship to the batch, not the batch.
type Service struct {
	cfg   Config
	retry map[int]bool

	mu     sync.Mutex
	runs   map[string]*run
	closed bool

	slots *fifoSem
	wg    sync.WaitGroup

	base   context.Context
	cancel context.CancelFunc

	sweepStop chan struct{}
	sweepDone chan struct{}

	// Observability. DESIGN §12.3 asks for queue depth and rows in flight, and
	// the store cannot answer either: a row that has been sent upstream and not
	// yet settled has no persisted state that distinguishes it from one that
	// has not started, and "how many batches are waiting for a dispatch slot"
	// is a fact about this process rather than about the batch.
	rowsStarted  atomic.Int64
	rowsFinished atomic.Int64
	rowsInFlight atomic.Int64
}

// Stats is a point-in-time view of the scheduler, for metrics and tests.
type Stats struct {
	// Active is batches with a run in this process, whether dispatching or
	// still waiting for a slot.
	Active int
	// DispatchSlots is max_active_batches and SlotsFree is what is left of it.
	DispatchSlots int
	SlotsFree     int
	// Queued is runs blocked on the dispatch semaphore, in arrival order.
	Queued int
	// RowsInFlight is rows sent upstream and not yet settled.
	RowsInFlight int64
	// RowsStarted and RowsFinished are cumulative.
	RowsStarted  int64
	RowsFinished int64
	// Closed reports that the service has stopped accepting work.
	Closed bool
}

// Stats reports the scheduler's live state.
//
// It takes s.mu, which is the same mutex a batch record's read-modify-write
// takes — but never the row execution path, which is explicitly documented as
// not touching it. So a scrape can contend with a batch being created or
// cancelled, and never with a row being run.
func (s *Service) Stats() Stats {
	s.mu.Lock()
	active, closed := len(s.runs), s.closed
	s.mu.Unlock()

	free, queued, slots := s.slots.stats()
	return Stats{
		Active:        active,
		DispatchSlots: slots,
		SlotsFree:     free,
		Queued:        queued,
		RowsInFlight:  s.rowsInFlight.Load(),
		RowsStarted:   s.rowsStarted.Load(),
		RowsFinished:  s.rowsFinished.Load(),
		Closed:        closed,
	}
}

// New builds a service. It does not read the store; call [Service.Recover] to
// pick up batches left running by a previous process.
func New(cfg Config) (*Service, error) {
	if err := cfg.fill(); err != nil {
		return nil, err
	}
	s := &Service{
		cfg:   cfg,
		retry: make(map[int]bool, len(cfg.RetryStatus)),
		runs:  make(map[string]*run),
		slots: newFIFOSem(cfg.MaxActiveBatches),
	}
	for _, code := range cfg.RetryStatus {
		s.retry[code] = true
	}
	s.base, s.cancel = context.WithCancel(context.Background())
	if cfg.SweepInterval > 0 {
		s.sweepStop = make(chan struct{})
		s.sweepDone = make(chan struct{})
		go s.sweepLoop(cfg.SweepInterval)
	}
	return s, nil
}

// Close stops dispatching new rows, waits for in-flight rows to finish, and
// returns.
//
// Draining is the point. A row that has already been sent upstream has already
// been paid for, so Close records its result rather than dropping it, and a
// batch left in progress is resumed by the next process's [Service.Recover]
// rather than restarted. If ctx ends before the drain finishes, in-flight rows
// are aborted and ctx's error is returned; that is the hard-stop path, and it
// costs at most one re-execution per in-flight row after the restart.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	runs := make([]*run, 0, len(s.runs))
	for _, r := range s.runs {
		runs = append(runs, r)
	}
	s.mu.Unlock()

	if s.sweepStop != nil {
		close(s.sweepStop)
		<-s.sweepDone
	}
	for _, r := range runs {
		r.halt(reasonShutdown)
	}
	s.slots.close()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
		s.cancel()
		<-done
	}
	s.cancel()
	return err
}

// Recover picks up every batch left in a non-terminal state by a previous
// process and resumes it. Rows that already finished are not run again; a batch
// left cancelling drains straight to cancelled.
//
// It is safe to call once, at startup. Calling it twice is harmless — a batch
// that already has a run in this process is skipped.
func (s *Service) Recover(ctx context.Context) (int, error) {
	recs, err := s.cfg.Store.ActiveBatches(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rec := range recs {
		if rec.Status.Terminal() {
			continue
		}
		reason := reasonNone
		if rec.Status == StatusCancelling {
			// The cancel was accepted before the process ended. Honour it:
			// dispatch nothing, drain nothing, write the files for whatever
			// finished, and reach cancelled.
			reason = reasonCancel
		}
		if s.start(rec.ID, reason) {
			n++
		}
	}
	return n, nil
}

// Sweep expires batches whose completion window has passed and returns how many
// it acted on. The background sweeper calls it; tests call it directly, the same
// convention internal/capacity uses for reservation expiry.
func (s *Service) Sweep(ctx context.Context) int {
	recs, err := s.cfg.Store.ActiveBatches(ctx)
	if err != nil {
		s.logf("batch: sweep: %v", err)
		return 0
	}
	now := s.cfg.Clock.Now()
	n := 0
	for _, rec := range recs {
		if rec.ExpiresAt.IsZero() || now.Before(rec.ExpiresAt) {
			continue
		}
		s.mu.Lock()
		r := s.runs[rec.ID]
		s.mu.Unlock()
		if r != nil {
			r.halt(reasonExpire)
			n++
			continue
		}
		// No run in this process — expire it in place, preserving whatever
		// finished before the window ran out.
		if err := s.finalize(ctx, rec.ID, StatusExpired); err != nil {
			s.logf("batch: expiring %s: %v", rec.ID, err)
			continue
		}
		n++
	}
	return n
}

func (s *Service) sweepLoop(every time.Duration) {
	defer close(s.sweepDone)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Sweep(s.base)
		case <-s.sweepStop:
			return
		}
	}
}

func (s *Service) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
	}
}

func (s *Service) now() time.Time { return s.cfg.Clock.Now() }

// start creates and launches a run unless the service is closed or the batch
// already has one. It reports whether a run was launched.
func (s *Service) start(id string, reason stopReason) bool {
	s.mu.Lock()
	if s.closed || s.runs[id] != nil {
		s.mu.Unlock()
		return false
	}
	r := &run{svc: s, id: id, stop: make(chan struct{})}
	if reason != reasonNone {
		r.reason.Store(int32(reason))
		close(r.stop)
	}
	s.runs[id] = r
	s.wg.Add(1)
	s.mu.Unlock()
	go r.execute()
	return true
}

// ---------------------------------------------------------------------------
// run — one batch's lifecycle in this process
// ---------------------------------------------------------------------------

type phase int32

const (
	phasePending  phase = iota // created; validating or waiting for a slot
	phaseDispatch              // rows are being dispatched
	phaseFinal                 // writing output files; no longer cancellable
	phaseDone
)

type stopReason int32

const (
	reasonNone stopReason = iota
	reasonCancel
	reasonExpire
	reasonShutdown
)

// run is one batch being worked on by this process.
type run struct {
	svc *Service
	id  string

	// phase is guarded by svc.mu, because the cancel decision depends on it.
	phase phase

	reason   atomic.Int32
	stop     chan struct{}
	stopOnce sync.Once
}

// halt stops dispatch. The first reason wins: a cancel that arrives before a
// shutdown still ends as cancelled, and a shutdown that arrives first leaves the
// cancel to be honoured by the next process.
func (r *run) halt(reason stopReason) {
	r.reason.CompareAndSwap(int32(reasonNone), int32(reason))
	r.stopOnce.Do(func() { close(r.stop) })
}

func (r *run) stopped() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

func (r *run) why() stopReason { return stopReason(r.reason.Load()) }

// ---------------------------------------------------------------------------
// fifoSem — arrival-ordered slots for dispatching batches
// ---------------------------------------------------------------------------

// fifoSem is a semaphore that hands slots out in arrival order.
//
// A buffered channel would be simpler and would wake an arbitrary waiter, which
// means a batch submitted first can be overtaken indefinitely by later ones.
// Batches are long-lived enough that this is visible to a user, so the queue is
// explicit.
type fifoSem struct {
	mu     sync.Mutex
	free   int
	q      []chan struct{}
	closed bool
	slots  int
}

func newFIFOSem(n int) *fifoSem {
	if n < 1 {
		n = 1
	}
	return &fifoSem{free: n, slots: n}
}

// stats reports the semaphore's occupancy. The critical section is three field
// reads, and the only other writers are slot acquisition and release, neither
// of which happens per row.
func (f *fifoSem) stats() (free, queued, slots int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.free, len(f.q), f.slots
}

// acquire waits for a slot, for ctx to end, or for stop to be closed.
func (f *fifoSem) acquire(ctx context.Context, stop <-chan struct{}) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return ErrClosed
	}
	if f.free > 0 {
		f.free--
		f.mu.Unlock()
		return nil
	}
	ch := make(chan struct{}, 1)
	f.q = append(f.q, ch)
	f.mu.Unlock()

	select {
	case _, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		return nil
	case <-ctx.Done():
		return f.abandon(ch, ctx.Err())
	case <-stop:
		return f.abandon(ch, ErrClosed)
	}
}

// abandon removes a waiter that gave up, returning the slot if it was granted
// in the same instant.
func (f *fifoSem) abandon(ch chan struct{}, cause error) error {
	f.mu.Lock()
	for i, c := range f.q {
		if c == ch {
			f.q = append(f.q[:i], f.q[i+1:]...)
			f.mu.Unlock()
			return cause
		}
	}
	f.mu.Unlock()
	// Not queued any more: either granted or closed. Drain and hand back.
	if _, ok := <-ch; ok {
		f.release()
	}
	return cause
}

func (f *fifoSem) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.q) > 0 {
		ch := f.q[0]
		f.q = f.q[1:]
		ch <- struct{}{}
		return
	}
	f.free++
}

func (f *fifoSem) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	for _, ch := range f.q {
		close(ch)
	}
	f.q = nil
}

// ---------------------------------------------------------------------------
// ids
// ---------------------------------------------------------------------------

// newID returns an opaque identifier with the given prefix. The separator
// matches the vendor's spelling per object kind — "batch_abc", "file-abc" — so
// that a client regexp written against one works against the other.
func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot fail on any supported platform; if it somehow
		// does, a time-derived id is still unique enough to be safe here.
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(now >> (i % 8 * 8))
		}
	}
	return prefix + hex.EncodeToString(b[:])
}
