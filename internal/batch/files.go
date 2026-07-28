package batch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// UploadRequest is POST /v1/files.
type UploadRequest struct {
	// Filename is the client's name for the content. It is metadata only.
	Filename string
	// Purpose must be one of Config.UploadPurposes. PurposeBatch is the one
	// that gets validated.
	Purpose string
	// OwnerKeyID is the api key the file belongs to.
	OwnerKeyID string
	// Content is the bytes. It is streamed to blob storage and never held whole
	// in memory.
	Content io.Reader
	// ExpiresAfter, when set, marks the file for expiry that far in the future.
	ExpiresAfter time.Duration
}

// UploadFile stores a file, validating it when its purpose says what it is
// supposed to be.
//
// A batch input file is validated here rather than only at batch creation. The
// caller finds out that line 4 391 has a duplicate custom_id at the moment they
// can still fix it, instead of after a batch has been accepted and failed
// asynchronously. Creating a batch validates again — the endpoint constraint can
// only be checked then, and the file may have been uploaded before a deployment
// changed — so this is an early answer, not the only one.
func (s *Service) UploadFile(ctx context.Context, req UploadRequest) (*File, error) {
	if !s.purposeAllowed(req.Purpose) {
		return nil, fmt.Errorf("%w: purpose %q is not accepted for upload", ErrInvalidRequest, req.Purpose)
	}
	if req.Content == nil {
		return nil, fmt.Errorf("%w: file content is required", ErrInvalidRequest)
	}

	id := newID("file-")
	h := sha256.New()
	// One byte past the ceiling is read so that an oversized upload is detected
	// rather than silently truncated, and nothing beyond that is ever written.
	limited := io.LimitReader(req.Content, s.cfg.MaxFileBytes+1)
	n, err := s.cfg.Blobs.Put(ctx, id, io.TeeReader(limited, h))
	if err != nil {
		_ = s.cfg.Blobs.Remove(ctx, id)
		return nil, err
	}
	if n > s.cfg.MaxFileBytes {
		_ = s.cfg.Blobs.Remove(ctx, id)
		return nil, &ValidationErrors{Errs: []*ValidationError{{
			Code:    CodeFileTooLarge,
			Message: fmt.Sprintf("file is larger than the %d byte limit", s.cfg.MaxFileBytes),
		}}}
	}

	if req.Purpose == PurposeBatch {
		if err := s.validateStored(ctx, id); err != nil {
			_ = s.cfg.Blobs.Remove(ctx, id)
			return nil, err
		}
	}

	now := s.now()
	rec := &FileRecord{
		ID:         id,
		OwnerKeyID: req.OwnerKeyID,
		Purpose:    req.Purpose,
		Filename:   req.Filename,
		Bytes:      n,
		SHA256:     hex.EncodeToString(h.Sum(nil)),
		StorageRef: id,
		Status:     FileProcessed,
		CreatedAt:  now,
	}
	if req.ExpiresAfter > 0 {
		rec.ExpiresAt = now.Add(req.ExpiresAfter)
	}
	if err := s.cfg.Store.CreateFile(ctx, rec); err != nil {
		_ = s.cfg.Blobs.Remove(ctx, id)
		return nil, err
	}
	return rec.API(), nil
}

// validateStored runs the JSONL rules over content already in blob storage. No
// endpoint is enforced: at upload time the file does not yet belong to a batch,
// so a row may name any batch-capable endpoint.
func (s *Service) validateStored(ctx context.Context, ref string) error {
	rd, err := s.cfg.Blobs.Open(ctx, ref)
	if err != nil {
		return err
	}
	defer rd.Close()
	ve, err := validateInput(rd, s.validateConfig(""), nil)
	if err != nil {
		return err
	}
	if ve != nil {
		return ve
	}
	return nil
}

// RetrieveFile returns one file's metadata.
func (s *Service) RetrieveFile(ctx context.Context, id, owner string) (*File, error) {
	rec, err := s.fileFor(ctx, id, owner)
	if err != nil {
		return nil, err
	}
	return rec.API(), nil
}

// ListFiles returns a page of file metadata, newest first.
func (s *Service) ListFiles(ctx context.Context, q FileQuery) (*FileList, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	recs, more, err := s.cfg.Store.ListFiles(ctx, q)
	if err != nil {
		return nil, err
	}
	out := &FileList{Object: ObjectList, Data: make([]*File, 0, len(recs)), HasMore: more}
	for _, rec := range recs {
		out.Data = append(out.Data, rec.API())
	}
	if n := len(out.Data); n > 0 {
		out.FirstID = out.Data[0].ID
		out.LastID = out.Data[n-1].ID
	}
	return out, nil
}

// FileContent returns the bytes. The caller closes the reader.
func (s *Service) FileContent(ctx context.Context, id, owner string) (io.ReadSeekCloser, *FileRecord, error) {
	rec, err := s.fileFor(ctx, id, owner)
	if err != nil {
		return nil, nil, err
	}
	rd, err := s.cfg.Blobs.Open(ctx, rec.StorageRef)
	if err != nil {
		return nil, nil, err
	}
	return rd, rec, nil
}

// DeleteFile removes a file and its content.
//
// A file that a live batch is reading is refused. The scheduler reads rows out
// of the input file as it dispatches them, so deleting it under a running batch
// does not free storage — it breaks the batch, halfway through, in a way the
// caller did not ask for and cannot undo.
func (s *Service) DeleteFile(ctx context.Context, id, owner string) (*FileDeleted, error) {
	rec, err := s.fileFor(ctx, id, owner)
	if err != nil {
		return nil, err
	}
	active, err := s.cfg.Store.ActiveBatches(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range active {
		if b.InputFileID == rec.ID {
			return nil, fmt.Errorf("%w: file %s is the input of batch %s, which is %s",
				ErrInvalidState, rec.ID, b.ID, b.Status)
		}
	}
	if err := s.cfg.Store.DeleteFile(ctx, rec.ID); err != nil {
		return nil, err
	}
	if err := s.cfg.Blobs.Remove(ctx, rec.StorageRef); err != nil {
		// The metadata is gone, so the file is deleted as far as the caller is
		// concerned; the orphaned bytes are an operator problem, not a caller
		// error.
		s.logf("batch: removing blob for %s: %v", rec.ID, err)
	}
	return &FileDeleted{ID: rec.ID, Object: ObjectFile, Deleted: true}, nil
}

func (s *Service) fileFor(ctx context.Context, id, owner string) (*FileRecord, error) {
	rec, err := s.cfg.Store.GetFile(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ownedBy(rec.OwnerKeyID, owner) {
		return nil, fmt.Errorf("%w: file %s", ErrNotFound, id)
	}
	return rec, nil
}

func (s *Service) purposeAllowed(p string) bool {
	for _, allowed := range s.cfg.UploadPurposes {
		if allowed == p {
			return true
		}
	}
	return false
}
