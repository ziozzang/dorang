package batch

import (
	"context"
	"fmt"
	"unicode/utf8"
)

// CreateRequest is POST /v1/batches.
type CreateRequest struct {
	// InputFileID names an uploaded file with purpose "batch".
	InputFileID string
	// Endpoint is the endpoint every row must target.
	Endpoint string
	// CompletionWindow is the wire spelling, e.g. "24h". Empty selects the
	// configured default.
	CompletionWindow string
	// Metadata is opaque caller key/value data, echoed back on the object.
	Metadata map[string]string
	// OwnerKeyID is the api key the batch belongs to. It scopes retrieval and
	// listing.
	OwnerKeyID string
	// PrincipalID is the subject the capacity principal axis is charged to —
	// the key, user or team, whichever the deployment meters on. Empty falls
	// back to OwnerKeyID.
	PrincipalID string
}

// Create accepts a batch and returns immediately.
//
// Validation of the input file happens asynchronously, which is why the returned
// object is in validating: a 200 MiB, 50 000-row file cannot be parsed inside a
// request handler without turning batch submission into a timeout. A file whose
// rows do not match the batch endpoint therefore produces a batch that reaches
// failed with an errors list, exactly as the vendor's does — not an error from
// this call. What this call does reject synchronously is what it can check
// synchronously: an unknown endpoint, a missing file, a file with the wrong
// purpose, an unusable completion window, or metadata over the limits.
func (s *Service) Create(ctx context.Context, req CreateRequest) (*Batch, error) {
	if s.isClosed() {
		return nil, ErrClosed
	}
	if !s.endpointAllowed(req.Endpoint) {
		return nil, fmt.Errorf("%w: endpoint %q is not batch-capable", ErrInvalidRequest, req.Endpoint)
	}
	window, err := parseWindow(req.CompletionWindow, s.cfg.DefaultCompletionWindow,
		s.cfg.MinCompletionWindow, s.cfg.MaxCompletionWindow)
	if err != nil {
		return nil, err
	}
	if err := s.checkMetadata(req.Metadata); err != nil {
		return nil, err
	}
	f, err := s.cfg.Store.GetFile(ctx, req.InputFileID)
	if err != nil {
		return nil, err
	}
	if !ownedBy(f.OwnerKeyID, req.OwnerKeyID) {
		return nil, fmt.Errorf("%w: file %s", ErrNotFound, req.InputFileID)
	}
	if f.Purpose != PurposeBatch {
		return nil, fmt.Errorf("%w: file %s has purpose %q, batch input requires %q",
			ErrInvalidRequest, f.ID, f.Purpose, PurposeBatch)
	}

	now := s.now()
	principal := req.PrincipalID
	if principal == "" {
		principal = req.OwnerKeyID
	}
	rec := &BatchRecord{
		ID:               newID("batch_"),
		OwnerKeyID:       req.OwnerKeyID,
		PrincipalID:      principal,
		Endpoint:         req.Endpoint,
		InputFileID:      req.InputFileID,
		CompletionWindow: window,
		Status:           StatusValidating,
		Metadata:         req.Metadata,
		CreatedAt:        now,
		UpdatedAt:        now,
		ExpiresAt:        now.Add(window),
	}
	if err := s.cfg.Store.CreateBatch(ctx, rec); err != nil {
		return nil, err
	}
	if !s.start(rec.ID, reasonNone) {
		return nil, ErrClosed
	}
	return rec.API(s.cfg.EmitQueuedStatus), nil
}

// Retrieve returns one batch. owner, when non-empty, scopes the lookup to that
// api key: a batch belonging to someone else answers ErrNotFound rather than
// confirming that the id exists.
func (s *Service) Retrieve(ctx context.Context, id, owner string) (*Batch, error) {
	rec, err := s.cfg.Store.GetBatch(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ownedBy(rec.OwnerKeyID, owner) {
		return nil, fmt.Errorf("%w: batch %s", ErrNotFound, id)
	}
	return rec.API(s.cfg.EmitQueuedStatus), nil
}

// List returns a page of batches, newest first.
func (s *Service) List(ctx context.Context, q BatchQuery) (*BatchList, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	recs, more, err := s.cfg.Store.ListBatches(ctx, q)
	if err != nil {
		return nil, err
	}
	out := &BatchList{Object: ObjectList, Data: make([]*Batch, 0, len(recs)), HasMore: more}
	for _, rec := range recs {
		out.Data = append(out.Data, rec.API(s.cfg.EmitQueuedStatus))
	}
	if n := len(out.Data); n > 0 {
		out.FirstID = out.Data[0].ID
		out.LastID = out.Data[n-1].ID
	}
	return out, nil
}

// Cancel stops a batch.
//
// Two behaviours, decided atomically against the batch's own progress:
//
//   - A batch that has not started dispatching reaches cancelled before this
//     call returns. Nothing ran, so there is nothing to drain.
//   - A batch that is dispatching goes to cancelling, stops taking new rows, and
//     reaches cancelled once the rows already in flight have finished. Their
//     results are written to the output and error files. A cancel that threw
//     away work the caller had already paid for would be a data-loss bug, so
//     in-flight rows are never aborted and finished rows are never dropped.
//
// Cancelling a batch that is already writing its output files, or that has
// reached a terminal state, is ErrInvalidState.
func (s *Service) Cancel(ctx context.Context, id, owner string) (*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.cfg.Store.GetBatch(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ownedBy(rec.OwnerKeyID, owner) {
		return nil, fmt.Errorf("%w: batch %s", ErrNotFound, id)
	}
	if rec.Status.Terminal() {
		return nil, fmt.Errorf("%w: batch %s is already %s", ErrInvalidState, id, rec.Status)
	}
	if rec.Status == StatusFinalizing {
		return nil, fmt.Errorf("%w: batch %s is writing its output files", ErrInvalidState, id)
	}

	r := s.runs[id]
	if r != nil && r.phase >= phaseFinal {
		return nil, fmt.Errorf("%w: batch %s is writing its output files", ErrInvalidState, id)
	}
	// Dispatching means rows may be in flight; anything earlier means none can
	// be, and the decision cannot change under us because the run enters
	// phaseDispatch under this same lock.
	dispatching := r != nil && r.phase == phaseDispatch
	if r != nil {
		r.halt(reasonCancel)
	}

	now := s.now()
	if rec.Status != StatusCancelling {
		rec.CancellingAt = now
	}
	if dispatching {
		rec.Status = StatusCancelling
	} else {
		rec.Status = StatusCancelled
		rec.CancelledAt = now
	}
	rec.UpdatedAt = now
	if err := s.cfg.Store.SaveBatch(ctx, rec); err != nil {
		return nil, err
	}
	return rec.API(s.cfg.EmitQueuedStatus), nil
}

func (s *Service) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Service) endpointAllowed(url string) bool {
	for _, e := range s.cfg.Endpoints {
		if e == url {
			return true
		}
	}
	return false
}

func (s *Service) endpointSet() map[string]bool {
	m := make(map[string]bool, len(s.cfg.Endpoints))
	for _, e := range s.cfg.Endpoints {
		m[e] = true
	}
	return m
}

func (s *Service) checkMetadata(md map[string]string) error {
	if len(md) > s.cfg.MaxMetadataKeys {
		return fmt.Errorf("%w: metadata has %d keys, limit is %d",
			ErrInvalidRequest, len(md), s.cfg.MaxMetadataKeys)
	}
	for k, v := range md {
		if utf8.RuneCountInString(k) > s.cfg.MaxMetadataKeyChars {
			return fmt.Errorf("%w: metadata key %q is longer than %d characters",
				ErrInvalidRequest, k, s.cfg.MaxMetadataKeyChars)
		}
		if utf8.RuneCountInString(v) > s.cfg.MaxMetadataValChars {
			return fmt.Errorf("%w: metadata value for %q is longer than %d characters",
				ErrInvalidRequest, k, s.cfg.MaxMetadataValChars)
		}
	}
	return nil
}

// ownedBy reports whether a caller scoped to owner may see a record owned by
// recordOwner. An empty owner is an administrative caller and sees everything;
// an unowned record is visible to anyone who can name it.
func ownedBy(recordOwner, owner string) bool {
	return owner == "" || recordOwner == "" || recordOwner == owner
}
