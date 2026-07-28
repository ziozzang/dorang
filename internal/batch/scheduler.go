package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// execute is one batch's whole life in this process: validate, wait for a
// dispatch slot, run rows, write files.
func (r *run) execute() {
	s := r.svc
	defer s.wg.Done()
	defer s.retire(r)

	ctx := s.base
	rec, err := s.cfg.Store.GetBatch(ctx, r.id)
	if err != nil {
		s.logf("batch: loading %s: %v", r.id, err)
		return
	}
	if rec.Status.Terminal() {
		return
	}

	if rec.Status == StatusValidating {
		rec, err = s.prepare(ctx, rec)
		if err != nil {
			// Infrastructure, not content: leave the batch in validating so a
			// later Recover picks it up rather than failing a caller's batch
			// because a disk was briefly unreadable.
			s.logf("batch: preparing %s: %v", r.id, err)
			return
		}
		if rec.Status.Terminal() {
			return
		}
	}

	if err := s.slots.acquire(ctx, r.stop); err != nil {
		s.settle(ctx, r)
		return
	}
	defer s.slots.release()

	rec, ok := s.enterDispatch(ctx, r)
	if !ok {
		s.settle(ctx, r)
		return
	}
	if err := s.dispatch(ctx, r, rec); err != nil {
		s.logf("batch: dispatching %s: %v", r.id, err)
		s.failBatch(ctx, r.id, "dispatch_failed", err.Error())
		return
	}
	s.settle(ctx, r)
}

// retire drops the run. A batch with no run in this process is one that Recover
// may pick up again.
func (s *Service) retire(r *run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.phase = phaseDone
	if s.runs[r.id] == r {
		delete(s.runs, r.id)
	}
}

// settle writes the terminal state a stopped or finished run has earned.
//
// A shutdown deliberately writes nothing: the batch stays in_progress with its
// finished rows recorded, and the next process resumes it. Anything else is
// terminal and gets its output files, including a cancel — the rows that
// finished before the cancel are the caller's, and they are written out.
func (s *Service) settle(ctx context.Context, r *run) {
	if ctx.Err() != nil {
		return
	}
	switch r.why() {
	case reasonShutdown:
		return
	case reasonCancel:
		s.finalizeQuietly(ctx, r.id, StatusCancelled)
	case reasonExpire:
		s.finalizeQuietly(ctx, r.id, StatusExpired)
	default:
		s.finalizeQuietly(ctx, r.id, StatusCompleted)
	}
}

func (s *Service) finalizeQuietly(ctx context.Context, id string, final Status) {
	if err := s.finalize(ctx, id, final); err != nil {
		s.logf("batch: finalizing %s as %s: %v", id, final, err)
	}
}

// prepare validates the input file and records the rows.
//
// A content failure fails the batch with an errors list naming every offending
// line. An infrastructure failure returns an error and changes nothing, because
// the file is probably fine and the batch should be retried, not condemned.
func (s *Service) prepare(ctx context.Context, rec *BatchRecord) (*BatchRecord, error) {
	f, err := s.cfg.Store.GetFile(ctx, rec.InputFileID)
	if err != nil {
		return s.failValidation(ctx, rec, []BatchError{{
			Code:    CodeReadFailed,
			Message: fmt.Sprintf("input file %s is not available: %v", rec.InputFileID, err),
			Param:   "input_file_id",
		}})
	}
	rd, err := s.cfg.Blobs.Open(ctx, f.StorageRef)
	if err != nil {
		return nil, err
	}
	defer rd.Close()

	now := s.now()
	rows := make([]*RowRecord, 0, 256)
	ve, err := validateInput(rd, s.validateConfig(rec.Endpoint), func(row *RowRecord) {
		row.BatchID = rec.ID
		row.CreatedAt = now
		row.UpdatedAt = now
		rows = append(rows, row)
	})
	if err != nil {
		return nil, err
	}
	if ve != nil {
		return s.failValidation(ctx, rec, ve.List())
	}
	if err := s.cfg.Store.PutRows(ctx, rows); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fresh, err := s.cfg.Store.GetBatch(ctx, rec.ID)
	if err != nil {
		return nil, err
	}
	if fresh.Status != StatusValidating {
		// Cancelled while we were validating.
		return fresh, nil
	}
	fresh.Counts.Total = len(rows)
	fresh.Status = StatusQueued
	fresh.UpdatedAt = s.now()
	if err := s.cfg.Store.SaveBatch(ctx, fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

func (s *Service) validateConfig(endpoint string) validateConfig {
	return validateConfig{
		endpoint:     endpoint,
		endpoints:    s.endpointSet(),
		maxRows:      s.cfg.MaxRows,
		maxRowBytes:  s.cfg.MaxRowBytes,
		maxCustomID:  s.cfg.MaxCustomIDChars,
		maxErrors:    s.cfg.MaxValidationErrors,
		groupSegment: s.cfg.GroupSegment,
		models:       s.cfg.Models,
	}
}

func (s *Service) failValidation(ctx context.Context, rec *BatchRecord, errs []BatchError) (*BatchRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fresh, err := s.cfg.Store.GetBatch(ctx, rec.ID)
	if err != nil {
		return nil, err
	}
	if fresh.Status.Terminal() {
		return fresh, nil
	}
	now := s.now()
	fresh.Status = StatusFailed
	fresh.Errors = errs
	fresh.FailedAt = now
	fresh.UpdatedAt = now
	if err := s.cfg.Store.SaveBatch(ctx, fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

func (s *Service) failBatch(ctx context.Context, id, code, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.cfg.Store.GetBatch(ctx, id)
	if err != nil || rec.Status.Terminal() {
		return
	}
	now := s.now()
	rec.Status = StatusFailed
	rec.Errors = append(rec.Errors, BatchError{Code: code, Message: msg})
	rec.FailedAt = now
	rec.UpdatedAt = now
	if err := s.cfg.Store.SaveBatch(ctx, rec); err != nil {
		s.logf("batch: failing %s: %v", id, err)
	}
}

// enterDispatch moves the batch to in_progress, or refuses because it was
// stopped first. The phase is written under the same lock Cancel reads it under,
// which is what makes "queued cancels immediately, dispatching cancels by
// draining" a decision rather than a race.
func (s *Service) enterDispatch(ctx context.Context, r *run) (*BatchRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.stopped() {
		return nil, false
	}
	if s.closed {
		// Close sets the flag before it halts the runs, so a run can arrive
		// here with no stop reason of its own. Without this it would fall
		// through to settle with reason none and finalize an untouched batch as
		// COMPLETED. Shutdown means "leave it for the next process".
		r.halt(reasonShutdown)
		return nil, false
	}
	rec, err := s.cfg.Store.GetBatch(ctx, r.id)
	if err != nil {
		// The store is unreachable. Do not finalize anything on a guess.
		s.logf("batch: reading %s before dispatch: %v", r.id, err)
		r.halt(reasonShutdown)
		return nil, false
	}
	if rec.Status.Terminal() || rec.Status == StatusCancelling {
		return nil, false
	}
	now := s.now()
	r.phase = phaseDispatch
	rec.Status = StatusInProgress
	if rec.InProgressAt.IsZero() {
		rec.InProgressAt = now
	}
	rec.UpdatedAt = now
	if err := s.cfg.Store.SaveBatch(ctx, rec); err != nil {
		s.logf("batch: marking %s in progress: %v", r.id, err)
		r.phase = phasePending
		r.halt(reasonShutdown)
		return nil, false
	}
	return rec, true
}

// dispatchState is the shared, lock-free part of one dispatch.
type dispatchState struct {
	next      atomic.Int64
	completed atomic.Int64
	failed    atomic.Int64
	finished  atomic.Int64

	once sync.Once
	err  atomic.Pointer[error]
}

func (d *dispatchState) fail(err error) {
	d.once.Do(func() { d.err.Store(&err) })
}

func (d *dispatchState) firstErr() error {
	if p := d.err.Load(); p != nil {
		return *p
	}
	return nil
}

// dispatch runs every row that has not already finished.
func (s *Service) dispatch(ctx context.Context, r *run, rec *BatchRecord) error {
	chunks, done, err := s.planRows(ctx, rec)
	if err != nil {
		return err
	}
	f, err := s.cfg.Store.GetFile(ctx, rec.InputFileID)
	if err != nil {
		return err
	}

	st := &dispatchState{}
	st.completed.Store(int64(done.Completed))
	st.failed.Store(int64(done.Failed))
	st.finished.Store(int64(done.Completed + done.Failed))
	if len(chunks) == 0 {
		return nil
	}

	workers := s.cfg.RowConcurrency
	if workers > len(chunks) {
		workers = len(chunks)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.worker(ctx, r, rec, f.StorageRef, chunks, st)
		}()
	}
	wg.Wait()
	s.flushCounts(ctx, rec.ID, st)
	return st.firstErr()
}

// worker drains chunks. Each worker holds its own handle on the input blob and
// reads one row at a time by offset, so a 200 MiB input file costs one row of
// memory per worker rather than 200 MiB of it.
func (s *Service) worker(ctx context.Context, r *run, rec *BatchRecord, ref string, chunks [][]*RowRecord, st *dispatchState) {
	rd, err := s.cfg.Blobs.Open(ctx, ref)
	if err != nil {
		st.fail(err)
		return
	}
	defer rd.Close()

	var buf []byte
	for {
		if r.stopped() || st.firstErr() != nil || ctx.Err() != nil {
			return
		}
		i := int(st.next.Add(1)) - 1
		if i >= len(chunks) {
			return
		}
		for _, row := range chunks[i] {
			if r.stopped() || st.firstErr() != nil || ctx.Err() != nil {
				return
			}
			if err := s.runRow(ctx, r, rec, row, rd, &buf, st); err != nil {
				st.fail(err)
				return
			}
		}
	}
}

// planRows reads persisted row state and returns the work still to do, grouped
// by prefix hash, plus the tally of what previous processes already finished.
func (s *Service) planRows(ctx context.Context, rec *BatchRecord) ([][]*RowRecord, RequestCounts, error) {
	var done RequestCounts
	pending := make([]*RowRecord, 0, 256)
	err := s.cfg.Store.EachRow(ctx, rec.ID, func(row *RowRecord) error {
		done.Total++
		switch row.Status {
		case RowCompleted:
			done.Completed++
		case RowFailed:
			done.Failed++
		}
		if !row.Status.Terminal() {
			// A finished row is never re-run — that is what makes a resumed
			// batch cost only the work that was actually lost. Its result body
			// is not needed for planning either, so it is dropped rather than
			// carried through the whole dispatch.
			row.Result = nil
			pending = append(pending, row)
		}
		return nil
	})
	if err != nil {
		return nil, done, err
	}
	return groupRows(pending, s.cfg.GroupChunk), done, nil
}

// groupRows puts rows sharing a prefix hash next to each other so that
// cache-adjacent work runs together (DESIGN §11.1, §7.4b), then splits any group
// larger than maxChunk so one enormous group cannot serialize the whole batch
// behind a single worker.
//
// Group order and within-group order are both the input file's order, so the
// plan is deterministic and a resumed batch continues in the same shape.
func groupRows(rows []*RowRecord, maxChunk int) [][]*RowRecord {
	if maxChunk < 1 {
		maxChunk = 1
	}
	order := make([]string, 0, len(rows))
	byHash := make(map[string][]*RowRecord, len(rows))
	for _, r := range rows {
		if _, ok := byHash[r.PrefixHash]; !ok {
			order = append(order, r.PrefixHash)
		}
		byHash[r.PrefixHash] = append(byHash[r.PrefixHash], r)
	}
	out := make([][]*RowRecord, 0, len(order))
	for _, h := range order {
		g := byHash[h]
		for len(g) > maxChunk {
			out = append(out, g[:maxChunk:maxChunk])
			g = g[maxChunk:]
		}
		out = append(out, g)
	}
	return out
}

// rowOutcome is what one attempt produced.
type rowOutcome struct {
	status     RowStatus
	statusCode int
	requestID  string
	body       []byte
	errCode    string
	errMessage string
}

// runRow executes one row to a terminal state, or leaves it queued.
//
// It returns an error only for conditions that make the whole batch unworkable —
// an unreadable input file, a store that will not record results. Everything
// else is a row outcome, because partial failure is the normal case.
func (s *Service) runRow(ctx context.Context, r *run, rec *BatchRecord, row *RowRecord, rd io.ReadSeeker, buf *[]byte, st *dispatchState) error {
	line, err := readRow(rd, row, buf)
	if err != nil {
		return fmt.Errorf("batch %s row %d (%s): %w", rec.ID, row.Seq, row.CustomID, err)
	}
	var in InputRow
	if err := json.Unmarshal(line, &in); err != nil {
		return s.settleRow(ctx, row, st, &rowOutcome{
			status: RowFailed, errCode: CodeInvalidJSON, errMessage: err.Error(),
		})
	}
	tgt, ok := s.cfg.Models.ResolveModel(row.Model)
	if !ok {
		return s.settleRow(ctx, row, st, &rowOutcome{
			status:  RowFailed,
			errCode: CodeModelNotFound,
			// The model resolved at validation time and does not now: a
			// deployment was removed under a running batch. That is the
			// operator's news, not the caller's fault, so it is reported
			// verbatim rather than folded into a generic upstream error.
			errMessage: fmt.Sprintf("model %q is no longer served by any deployment", row.Model),
		})
	}

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// A retry is not in flight, so a cancel does not have to wait for
			// it. The row simply stays queued.
			if r.stopped() {
				return nil
			}
			if err := s.cfg.Clock.Sleep(ctx, s.backoff(attempt)); err != nil {
				return nil
			}
			if r.stopped() {
				return nil
			}
		}
		row.Attempts = attempt + 1
		out, retryable := s.attempt(ctx, rec, row, &in, tgt, attempt)
		if out == nil {
			return nil // shutting down; the row stays queued for the next process
		}
		if retryable && attempt+1 < s.cfg.MaxAttempts {
			continue
		}
		return s.settleRow(ctx, row, st, out)
	}
}

// attempt reserves capacity, runs the row, and releases. A nil outcome means the
// context ended and nothing should be recorded.
func (s *Service) attempt(ctx context.Context, rec *BatchRecord, row *RowRecord, in *InputRow, tgt Target, attempt int) (*rowOutcome, bool) {
	resv, err := s.cfg.Capacity.Acquire(ctx, s.capacityRequest(rec, tgt))
	if err != nil {
		if ctx.Err() != nil {
			return nil, false
		}
		return &rowOutcome{
			status: RowFailed, errCode: "capacity_unavailable", errMessage: err.Error(),
		}, true
	}

	req := &ExecRequest{
		BatchID:       rec.ID,
		CustomID:      row.CustomID,
		Seq:           row.Seq,
		Endpoint:      row.URL,
		Model:         row.Model,
		Provider:      tgt.Provider,
		UpstreamModel: tgt.UpstreamModel,
		Body:          in.Body,
		PriorityClass: s.cfg.PriorityClass,
		PrincipalID:   rec.PrincipalID,
		Attempt:       attempt,
	}
	res, execErr := s.execOnce(ctx, resv, req)

	switch {
	case execErr != nil:
		if ctx.Err() != nil {
			return nil, false
		}
		return &rowOutcome{
			status: RowFailed, errCode: "upstream_error", errMessage: execErr.Error(),
		}, !IsTerminal(execErr)
	case res == nil:
		return &rowOutcome{
			status: RowFailed, errCode: "internal_error", errMessage: "executor returned no result",
		}, false
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return &rowOutcome{
			status: RowCompleted, statusCode: res.StatusCode, requestID: res.RequestID, body: res.Body,
		}, false
	default:
		// A real HTTP failure keeps the upstream's own status and body: the
		// error file is meant to carry what a direct call would have returned.
		return &rowOutcome{
			status: RowFailed, statusCode: res.StatusCode, requestID: res.RequestID, body: res.Body,
		}, s.retry[res.StatusCode]
	}
}

// execOnce runs the executor with the reservation held, and releases it however
// the call ends — including a panic, which would otherwise leak a slot on every
// axis the row was admitted on until the broker's sweeper reclaimed it.
func (s *Service) execOnce(ctx context.Context, resv Reservation, req *ExecRequest) (res *ExecResult, err error) {
	defer func() {
		resv.Release()
		if p := recover(); p != nil {
			res, err = nil, fmt.Errorf("batch: executor panicked: %v", p)
		}
	}()
	return s.cfg.Executor.Execute(ctx, req)
}

// capacityRequest builds the only capacity request this package ever makes.
//
// Batch is set here, unconditionally, and this is the only place in the package
// that writes it — there is a test that parses the package to prove it. That one
// field is the whole of DESIGN §11.1's protection: the broker applies its
// interactive reserve to every axis a batch request touches, so marking the work
// correctly protects the model axis, the credential axis, the route, the
// provider group and the global ceiling at once. Revision 1 of the design capped
// batch on the credential axis alone, under which a batch could occupy a model's
// entire limit while sitting well under its credential share and starve
// interactive traffic for that model completely.
func (s *Service) capacityRequest(rec *BatchRecord, tgt Target) CapacityRequest {
	return CapacityRequest{
		Provider:      tgt.Provider,
		UpstreamModel: tgt.UpstreamModel,
		ProviderGroup: tgt.ProviderGroup,
		PrincipalID:   rec.PrincipalID,
		Batch:         true,
	}
}

// settleRow records a row's terminal state.
func (s *Service) settleRow(ctx context.Context, row *RowRecord, st *dispatchState, out *rowOutcome) error {
	row.Status = out.status
	row.StatusCode = out.statusCode
	row.RequestID = out.requestID
	row.Result = out.body
	row.ErrCode = out.errCode
	row.ErrMessage = out.errMessage
	row.UpdatedAt = s.now()
	if err := s.saveRow(ctx, row); err != nil {
		return err
	}
	if out.status == RowCompleted {
		st.completed.Add(1)
	} else {
		st.failed.Add(1)
	}
	if n := st.finished.Add(1); s.cfg.CountFlush > 0 && n%int64(s.cfg.CountFlush) == 0 {
		s.flushCounts(ctx, row.BatchID, st)
	}
	return nil
}

// saveRow persists a finished row, retrying briefly. A row that cannot be
// recorded is worse than a row that failed: after a restart it would be executed
// and paid for a second time. If it still will not write, the batch stops.
func (s *Service) saveRow(ctx context.Context, row *RowRecord) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = s.cfg.Store.SaveRow(ctx, row); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if sleepErr := s.cfg.Clock.Sleep(ctx, time.Duration(i+1)*s.cfg.BackoffBase); sleepErr != nil {
			return err
		}
	}
	return fmt.Errorf("batch: recording row %s of %s: %w", row.CustomID, row.BatchID, err)
}

// flushCounts publishes progress so a poller sees a batch advancing. It is not
// the authority on the final numbers — finalize recounts from the rows, which is
// the only source that survives a restart.
func (s *Service) flushCounts(ctx context.Context, id string, st *dispatchState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.cfg.Store.GetBatch(ctx, id)
	if err != nil || rec.Status.Terminal() {
		return
	}
	rec.Counts.Completed = int(st.completed.Load())
	rec.Counts.Failed = int(st.failed.Load())
	rec.UpdatedAt = s.now()
	if err := s.cfg.Store.SaveBatch(ctx, rec); err != nil {
		s.logf("batch: progress for %s: %v", id, err)
	}
}

// backoff is exponential with full jitter. Jitter matters more here than in an
// interactive path: a thousand rows failing against the same upstream retry in
// lockstep without it, and the retry storm is indistinguishable from the
// original outage.
func (s *Service) backoff(attempt int) time.Duration {
	d := s.cfg.BackoffBase << uint(attempt-1)
	if d <= 0 || d > s.cfg.BackoffMax {
		d = s.cfg.BackoffMax
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)) + int64(d)/2)
}

// readRow reads one row out of the input blob by offset.
func readRow(rd io.ReadSeeker, row *RowRecord, buf *[]byte) ([]byte, error) {
	if row.Length <= 0 {
		return nil, fmt.Errorf("row has no recorded length")
	}
	if cap(*buf) < row.Length {
		*buf = make([]byte, row.Length)
	}
	b := (*buf)[:row.Length]
	if _, err := rd.Seek(row.Offset, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(rd, b); err != nil {
		return nil, err
	}
	return b, nil
}
