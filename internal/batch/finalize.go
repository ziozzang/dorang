package batch

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
)

// finalize writes the output and error files and moves the batch to its terminal
// state.
//
// It is reached by four routes — a batch that finished, one that was cancelled,
// one that expired, and the sweeper picking up a batch this process never ran —
// and they differ only in the terminal status. In particular a cancelled batch
// takes exactly the same path as a completed one, because the rows that finished
// before the cancel belong to the caller and are written out. Discarding them
// would be losing work that has already been paid for.
//
// finalize is idempotent: a batch already in a terminal state is left alone.
func (s *Service) finalize(ctx context.Context, id string, final Status) error {
	s.mu.Lock()
	rec, err := s.cfg.Store.GetBatch(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if rec.Status.Terminal() {
		s.mu.Unlock()
		return nil
	}
	// The store is the authority on whether a cancel was accepted. A run that
	// drained every remaining row still ends cancelled if a cancel was recorded
	// — including one recorded by a previous process, or by another node.
	if rec.Status == StatusCancelling && final == StatusCompleted {
		final = StatusCancelled
	}
	if r := s.runs[id]; r != nil {
		// Past this point the batch is no longer cancellable, and Cancel reads
		// the same field under the same lock.
		r.phase = phaseFinal
	}
	if final == StatusCompleted {
		now := s.now()
		rec.Status = StatusFinalizing
		rec.FinalizingAt = now
		rec.UpdatedAt = now
		if err := s.cfg.Store.SaveBatch(ctx, rec); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.mu.Unlock()

	counts, err := s.countRows(ctx, id)
	if err != nil {
		return err
	}
	var outputID, errorID string
	if counts.Completed > 0 {
		outputID, err = s.writeResultFile(ctx, rec, "_output.jsonl", func(r *RowRecord) bool {
			return r.Status == RowCompleted
		})
		if err != nil {
			return err
		}
	}
	if counts.Failed > 0 {
		errorID, err = s.writeResultFile(ctx, rec, "_error.jsonl", func(r *RowRecord) bool {
			return r.Status == RowFailed
		})
		if err != nil {
			return err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err = s.cfg.Store.GetBatch(ctx, id)
	if err != nil {
		return err
	}
	if rec.Status.Terminal() {
		return nil
	}
	now := s.now()
	rec.Counts = counts
	rec.OutputFileID = outputID
	rec.ErrorFileID = errorID
	rec.Status = final
	rec.UpdatedAt = now
	switch final {
	case StatusCompleted:
		rec.CompletedAt = now
	case StatusCancelled:
		if rec.CancellingAt.IsZero() {
			rec.CancellingAt = now
		}
		rec.CancelledAt = now
	case StatusExpired:
		rec.ExpiredAt = now
	case StatusFailed:
		rec.FailedAt = now
	}
	return s.cfg.Store.SaveBatch(ctx, rec)
}

// countRows recounts from persisted row state.
//
// The running tally kept during dispatch is not used: it belongs to one process's
// view of one run, and a batch that was resumed, or that this process never
// dispatched at all, has no such view. The rows are the only tally that survives
// a restart, so they are the one that decides request_counts.
func (s *Service) countRows(ctx context.Context, id string) (RequestCounts, error) {
	var c RequestCounts
	err := s.cfg.Store.EachRow(ctx, id, func(r *RowRecord) error {
		c.Total++
		switch r.Status {
		case RowCompleted:
			c.Completed++
		case RowFailed:
			c.Failed++
		}
		return nil
	})
	return c, err
}

// writeResultFile streams the selected rows into a new file.
//
// The content is piped straight into blob storage rather than assembled in
// memory: at the 50 000-row ceiling the output of a batch is comfortably larger
// than the input, and a gateway that needs the whole thing resident to hand it
// back has a memory profile set by its largest customer.
func (s *Service) writeResultFile(ctx context.Context, rec *BatchRecord, suffix string, want func(*RowRecord) bool) (string, error) {
	id := newID("file-")
	pr, pw := io.Pipe()
	go func() {
		w := bufio.NewWriterSize(pw, 64<<10)
		enc := json.NewEncoder(w)
		// COMPATIBILITY §2.1a: Go's HTML escaping turns & into &, which no
		// other server does, and these bodies are echoes of upstream responses.
		enc.SetEscapeHTML(false)
		err := s.cfg.Store.EachRow(ctx, rec.ID, func(r *RowRecord) error {
			if !want(r) {
				return nil
			}
			return enc.Encode(outputRow(rec.ID, r))
		})
		if err == nil {
			err = w.Flush()
		}
		pw.CloseWithError(err)
	}()

	h := sha256.New()
	n, err := s.cfg.Blobs.Put(ctx, id, io.TeeReader(pr, h))
	// Unblock the writer if Put stopped reading early.
	pr.CloseWithError(err)
	if err != nil {
		_ = s.cfg.Blobs.Remove(ctx, id)
		return "", err
	}

	frec := &FileRecord{
		ID:         id,
		OwnerKeyID: rec.OwnerKeyID,
		Purpose:    PurposeBatchOutput,
		Filename:   rec.ID + suffix,
		Bytes:      n,
		SHA256:     hex.EncodeToString(h.Sum(nil)),
		StorageRef: id,
		Status:     FileProcessed,
		CreatedAt:  s.now(),
	}
	if err := s.cfg.Store.CreateFile(ctx, frec); err != nil {
		_ = s.cfg.Blobs.Remove(ctx, id)
		return "", err
	}
	return id, nil
}

// outputRow renders one finished row as a line of an output or error file.
//
// A row that received an HTTP answer carries it in response, whatever the
// status: the error file's job is to hand back what a direct call would have
// returned, so the upstream's own error envelope goes through unaltered. error
// is for the rows that never got an answer at all — a transport failure, a
// routing failure, capacity that never came free.
func outputRow(batchID string, r *RowRecord) *OutputRow {
	o := &OutputRow{ID: rowID(batchID, r.CustomID), CustomID: r.CustomID}
	if r.StatusCode > 0 {
		body := json.RawMessage(r.Result)
		if len(body) == 0 {
			body = json.RawMessage("null")
		}
		o.Response = &RowResponse{StatusCode: r.StatusCode, RequestID: r.RequestID, Body: body}
	}
	if r.Status == RowFailed && r.StatusCode == 0 {
		o.Error = &RowError{Code: r.ErrCode, Message: r.ErrMessage}
	}
	return o
}

// rowID is the per-row id on an output line. It is derived rather than random so
// that a batch finalized twice — resumed after a crash, say — produces the same
// ids, and so that a line can be matched to its row without a lookup table.
func rowID(batchID, customID string) string {
	sum := sha256.Sum256([]byte(batchID + "\x00" + customID))
	return "batch_req_" + hex.EncodeToString(sum[:12])
}
