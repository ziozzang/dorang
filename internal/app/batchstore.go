package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ziozzang/dorang/internal/batch"
	"github.com/ziozzang/dorang/internal/store"
)

// batchStore satisfies batch.Store with the real database, which is what makes
// a batch survive a restart.
//
// internal/batch declares Store and internal/store implements the tables behind
// it, and neither imports the other (DESIGN §1). This is the join, and it is a
// translation rather than a pass-through because the two sides deliberately
// speak different vocabularies: batch.Status is a typed lifecycle, the column is
// text; batch.BatchError is a shape the schema layer has no business knowing, so
// it crosses as opaque JSON.
//
// Before this existed the only Store was the in-memory one, so batch.Service's
// Recover was wired to state that could not outlive the process it was recovering
// from. A batch interrupted mid-flight did not resume slowly — it disappeared,
// and every row that had already been paid for was gone with it.
type batchStore struct{ st *store.Store }

// terminalStatuses is the batch vocabulary's terminal set, handed to the store
// so that recovery's "everything not finished" is one definition rather than
// two that drift.
var terminalStatuses = []string{
	string(batch.StatusCompleted),
	string(batch.StatusFailed),
	string(batch.StatusExpired),
	string(batch.StatusCancelled),
}

// CreateBatch implements batch.Store.
func (s *batchStore) CreateBatch(ctx context.Context, b *batch.BatchRecord) error {
	rec, err := storeBatch(b)
	if err != nil {
		return err
	}
	return batchStoreErr(s.st.CreateBatch(ctx, rec))
}

// SaveBatch implements batch.Store.
func (s *batchStore) SaveBatch(ctx context.Context, b *batch.BatchRecord) error {
	rec, err := storeBatch(b)
	if err != nil {
		return err
	}
	return batchStoreErr(s.st.SaveBatch(ctx, rec))
}

// GetBatch implements batch.Store.
func (s *batchStore) GetBatch(ctx context.Context, id string) (*batch.BatchRecord, error) {
	rec, err := s.st.GetBatch(ctx, id)
	if err != nil {
		return nil, batchStoreErr(err)
	}
	return batchRecord(rec), nil
}

// ListBatches implements batch.Store.
func (s *batchStore) ListBatches(ctx context.Context, q batch.BatchQuery) ([]*batch.BatchRecord, bool, error) {
	rows, more, err := s.st.ListBatches(ctx, store.BatchPage{
		OwnerKeyID: q.OwnerKeyID,
		After:      q.After,
		Limit:      q.Limit,
	})
	if err != nil {
		return nil, false, batchStoreErr(err)
	}
	out := make([]*batch.BatchRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, batchRecord(r))
	}
	return out, more, nil
}

// ActiveBatches implements batch.Store.
func (s *batchStore) ActiveBatches(ctx context.Context) ([]*batch.BatchRecord, error) {
	rows, err := s.st.ActiveBatches(ctx, terminalStatuses)
	if err != nil {
		return nil, batchStoreErr(err)
	}
	out := make([]*batch.BatchRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, batchRecord(r))
	}
	return out, nil
}

// PutRows implements batch.Store.
//
// The idempotency the interface requires is the store's ON CONFLICT DO NOTHING:
// a row that already exists keeps its recorded state. That is not an
// optimization — a crash during validation is recovered by validating again, and
// resetting a finished row to queued would run it, and charge for it, twice.
func (s *batchStore) PutRows(ctx context.Context, rows []*batch.RowRecord) error {
	out := make([]*store.BatchRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, storeBatchRow(r))
	}
	return batchStoreErr(s.st.PutBatchRows(ctx, out))
}

// SaveRow implements batch.Store.
func (s *batchStore) SaveRow(ctx context.Context, r *batch.RowRecord) error {
	return batchStoreErr(s.st.SaveBatchRow(ctx, storeBatchRow(r)))
}

// EachRow implements batch.Store.
func (s *batchStore) EachRow(ctx context.Context, batchID string, fn func(*batch.RowRecord) error) error {
	return batchStoreErr(s.st.EachBatchRow(ctx, batchID, func(r *store.BatchRow) error {
		return fn(batchRowRecord(r))
	}))
}

// CreateFile implements batch.Store.
func (s *batchStore) CreateFile(ctx context.Context, f *batch.FileRecord) error {
	return batchStoreErr(s.st.CreateFile(ctx, storeFile(f)))
}

// GetFile implements batch.Store.
func (s *batchStore) GetFile(ctx context.Context, id string) (*batch.FileRecord, error) {
	f, err := s.st.GetFile(ctx, id)
	if err != nil {
		return nil, batchStoreErr(err)
	}
	return fileRecord(f), nil
}

// ListFiles implements batch.Store.
func (s *batchStore) ListFiles(ctx context.Context, q batch.FileQuery) ([]*batch.FileRecord, bool, error) {
	rows, more, err := s.st.ListFiles(ctx, store.BatchPage{
		OwnerKeyID: q.OwnerKeyID,
		Purpose:    q.Purpose,
		After:      q.After,
		Limit:      q.Limit,
	})
	if err != nil {
		return nil, false, batchStoreErr(err)
	}
	out := make([]*batch.FileRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, fileRecord(r))
	}
	return out, more, nil
}

// DeleteFile implements batch.Store.
func (s *batchStore) DeleteFile(ctx context.Context, id string) error {
	return batchStoreErr(s.st.DeleteFile(ctx, id))
}

// batchStoreErr translates the store's sentinels into the batch package's, so
// the service's own error handling — and the HTTP status mapping built on it —
// works identically whichever Store is configured.
func batchStoreErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("%w: %s", batch.ErrNotFound, err)
	case errors.Is(err, store.ErrExists):
		return fmt.Errorf("%w: %s", batch.ErrExists, err)
	}
	return err
}

func storeBatch(b *batch.BatchRecord) (*store.Batch, error) {
	var errs []byte
	if len(b.Errors) > 0 {
		enc, err := json.Marshal(b.Errors)
		if err != nil {
			return nil, err
		}
		errs = enc
	}
	return &store.Batch{
		ID:                 b.ID,
		OwnerKeyID:         b.OwnerKeyID,
		PrincipalID:        b.PrincipalID,
		Endpoint:           b.Endpoint,
		InputFileID:        b.InputFileID,
		OutputFileID:       b.OutputFileID,
		ErrorFileID:        b.ErrorFileID,
		CompletionWindowMS: b.CompletionWindow.Milliseconds(),
		Status:             string(b.Status),
		Total:              int64(b.Counts.Total),
		Completed:          int64(b.Counts.Completed),
		Failed:             int64(b.Counts.Failed),
		Errors:             errs,
		Metadata:           b.Metadata,
		CreatedAt:          b.CreatedAt,
		UpdatedAt:          b.UpdatedAt,
		InProgressAt:       b.InProgressAt,
		FinalizingAt:       b.FinalizingAt,
		CompletedAt:        b.CompletedAt,
		FailedAt:           b.FailedAt,
		CancellingAt:       b.CancellingAt,
		CancelledAt:        b.CancelledAt,
		ExpiredAt:          b.ExpiredAt,
		ExpiresAt:          b.ExpiresAt,
	}, nil
}

func batchRecord(b *store.Batch) *batch.BatchRecord {
	rec := &batch.BatchRecord{
		ID:               b.ID,
		OwnerKeyID:       b.OwnerKeyID,
		PrincipalID:      b.PrincipalID,
		Endpoint:         b.Endpoint,
		InputFileID:      b.InputFileID,
		OutputFileID:     b.OutputFileID,
		ErrorFileID:      b.ErrorFileID,
		CompletionWindow: time.Duration(b.CompletionWindowMS) * time.Millisecond,
		Status:           batch.Status(b.Status),
		Counts: batch.RequestCounts{
			Total:     int(b.Total),
			Completed: int(b.Completed),
			Failed:    int(b.Failed),
		},
		Metadata:     b.Metadata,
		CreatedAt:    b.CreatedAt,
		UpdatedAt:    b.UpdatedAt,
		InProgressAt: b.InProgressAt,
		FinalizingAt: b.FinalizingAt,
		CompletedAt:  b.CompletedAt,
		FailedAt:     b.FailedAt,
		CancellingAt: b.CancellingAt,
		CancelledAt:  b.CancelledAt,
		ExpiredAt:    b.ExpiredAt,
		ExpiresAt:    b.ExpiresAt,
	}
	if len(b.Errors) > 0 {
		// A stored error list that no longer parses is dropped rather than
		// failing the read: losing the explanation is bad, losing the batch is
		// worse.
		_ = json.Unmarshal(b.Errors, &rec.Errors)
	}
	return rec
}

func storeBatchRow(r *batch.RowRecord) *store.BatchRow {
	return &store.BatchRow{
		BatchID:      r.BatchID,
		CustomID:     r.CustomID,
		Seq:          int64(r.Seq),
		PrefixHash:   r.PrefixHash,
		ModelGroup:   r.Model,
		URL:          r.URL,
		InputOffset:  r.Offset,
		InputLength:  int64(r.Length),
		Status:       string(r.Status),
		Attempts:     int64(r.Attempts),
		StatusCode:   int64(r.StatusCode),
		RequestLogID: r.RequestID,
		ResponseBody: r.Result,
		ErrorCode:    r.ErrCode,
		ErrorMessage: r.ErrMessage,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
	}
}

func batchRowRecord(r *store.BatchRow) *batch.RowRecord {
	return &batch.RowRecord{
		BatchID:    r.BatchID,
		CustomID:   r.CustomID,
		Seq:        int(r.Seq),
		PrefixHash: r.PrefixHash,
		Model:      r.ModelGroup,
		URL:        r.URL,
		Offset:     r.InputOffset,
		Length:     int(r.InputLength),
		Status:     batch.RowStatus(r.Status),
		Attempts:   int(r.Attempts),
		StatusCode: int(r.StatusCode),
		RequestID:  r.RequestLogID,
		Result:     r.ResponseBody,
		ErrCode:    r.ErrorCode,
		ErrMessage: r.ErrorMessage,
		CreatedAt:  r.CreatedAt,
		UpdatedAt:  r.UpdatedAt,
	}
}

func storeFile(f *batch.FileRecord) *store.File {
	return &store.File{
		ID:            f.ID,
		OwnerKeyID:    f.OwnerKeyID,
		Purpose:       f.Purpose,
		Filename:      f.Filename,
		Bytes:         f.Bytes,
		SHA256:        f.SHA256,
		StorageRef:    f.StorageRef,
		Status:        f.Status,
		StatusDetails: f.StatusDetails,
		CreatedAt:     f.CreatedAt,
		ExpiresAt:     f.ExpiresAt,
	}
}

func fileRecord(f *store.File) *batch.FileRecord {
	return &batch.FileRecord{
		ID:            f.ID,
		OwnerKeyID:    f.OwnerKeyID,
		Purpose:       f.Purpose,
		Filename:      f.Filename,
		Bytes:         f.Bytes,
		SHA256:        f.SHA256,
		StorageRef:    f.StorageRef,
		Status:        f.Status,
		StatusDetails: f.StatusDetails,
		CreatedAt:     f.CreatedAt,
		ExpiresAt:     f.ExpiresAt,
	}
}
