package batch

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

// This file declares every interface the package consumes. None of them is
// satisfied by an internal package at compile time on purpose: the wiring layer
// adapts internal/router, internal/capacity and internal/store onto these, and
// this package is testable without any of the three. The shapes are chosen to
// map onto those packages without translation loss, not to be minimal for its
// own sake.

// Executor runs one batch row as an ordinary request.
//
// The contract that matters: a returned error means no HTTP answer was produced
// at all. A non-2xx answer is NOT an error — it is a [*ExecResult] with that
// status, because the caller's error file must carry the upstream's own error
// envelope, not dorang's paraphrase of it.
type Executor interface {
	Execute(ctx context.Context, req *ExecRequest) (*ExecResult, error)
}

// ExecRequest is one row on its way to a backend.
//
// Note what is absent: the prefix digests this package computed for grouping.
// They are seeded and segmented for grouping, not for routing (see
// [Config.GroupSegment]), so handing them to the router would invite it to reuse
// a chain that does not answer its question. The router computes its own.
type ExecRequest struct {
	// BatchID and CustomID identify the row for logging and for the ledger's
	// batch_id column.
	BatchID  string
	CustomID string
	// Seq is the row's 0-based position in the input file.
	Seq int
	// Endpoint is the row's url, e.g. "/v1/chat/completions".
	Endpoint string
	// Model is the client-facing model name, verbatim from the row body. It is
	// an opaque string and must not be split on any character (DESIGN §2.1).
	Model string
	// Provider and UpstreamModel are what Model resolved to, carried so the
	// executor does not resolve twice and so the capacity reservation and the
	// dispatch cannot disagree about the target.
	Provider      string
	UpstreamModel string
	// Body is the row's request body, verbatim.
	Body json.RawMessage
	// PriorityClass is the class this row is admitted and emitted at, always
	// Config.PriorityClass ("batch" by default, DESIGN §7.5).
	PriorityClass string
	// PrincipalID is the owning api key, user or team.
	PrincipalID string
	// Attempt is 0 on the first try and increments per retry.
	Attempt int
}

// ExecResult is one row's answer.
type ExecResult struct {
	// StatusCode is the HTTP status the caller would have seen.
	StatusCode int
	// Body is the response body verbatim, and lands in the output file as-is.
	Body json.RawMessage
	// RequestID is dorang's own request id for the row, so a line in the output
	// file can be joined against the ledger.
	RequestID string
}

// Reserver admits a row against every capacity axis that constrains it.
//
// This is internal/capacity's Acquire narrowed to what a batch row needs. The
// error return covers both "cannot ever succeed" (an axis whose interactive
// reserve leaves batch nothing) and "context ended"; the scheduler treats the
// former as a terminal row failure rather than retrying it forever.
type Reserver interface {
	Acquire(ctx context.Context, req CapacityRequest) (Reservation, error)
}

// Reservation is a held slot on every axis the request needed. Release is
// idempotent and never blocks.
type Reservation interface {
	Release()
}

// CapacityRequest is one admission attempt for one row.
//
// Batch is the field this whole package exists to set correctly. See
// Service.capacityRequest, which is the only place it is ever written.
type CapacityRequest struct {
	Provider      string
	UpstreamModel string
	ProviderGroup string
	PrincipalID   string
	// Batch subjects the request to the interactive reserve on EVERY axis
	// (DESIGN §11.1). Always true for work this package dispatches.
	Batch bool
}

// ModelResolver maps a client-facing model name onto the provider and upstream
// model it will be served by.
//
// Validation calls it to reject a row naming a model no deployment serves, and
// the scheduler calls it to build the capacity request. Implementations must
// treat the name as opaque (DESIGN §2.1): no splitting on ':' or '/', ever.
type ModelResolver interface {
	ResolveModel(name string) (Target, bool)
}

// Target is what a model name resolves to.
type Target struct {
	Provider      string
	UpstreamModel string
	ProviderGroup string
}

// Store persists batch, row and file metadata. It is the reason a restart
// resumes rather than starting over.
//
// Every method must be safe for concurrent use. Records handed to the store are
// owned by the caller afterwards, and records returned by it are owned by the
// caller — an implementation that returns a pointer into its own state will make
// a caller's mutation visible to the next reader, which is a class of bug the
// in-memory implementation here deliberately cannot have.
type Store interface {
	// CreateBatch inserts a new batch. It must fail if the id already exists.
	CreateBatch(ctx context.Context, b *BatchRecord) error
	// SaveBatch overwrites an existing batch.
	SaveBatch(ctx context.Context, b *BatchRecord) error
	// GetBatch returns one batch, or a nil record with ErrNotFound.
	GetBatch(ctx context.Context, id string) (*BatchRecord, error)
	// ListBatches returns batches newest first. It returns at most q.Limit
	// records and reports whether more exist after them.
	ListBatches(ctx context.Context, q BatchQuery) ([]*BatchRecord, bool, error)
	// ActiveBatches returns every batch in a non-terminal state, oldest first.
	// It is what Recover reads.
	ActiveBatches(ctx context.Context) ([]*BatchRecord, error)

	// PutRows inserts a batch's rows. It is idempotent by (batch id, custom id):
	// a row that already exists keeps its recorded state and is NOT reset, so a
	// crash during validation can be recovered by simply validating again.
	PutRows(ctx context.Context, rows []*RowRecord) error
	// SaveRow overwrites one row.
	SaveRow(ctx context.Context, r *RowRecord) error
	// EachRow calls fn for every row of a batch in seq order, stopping at the
	// first error fn returns. It streams because a batch holds up to
	// Config.MaxRows rows and their result bodies.
	EachRow(ctx context.Context, batchID string, fn func(*RowRecord) error) error

	// CreateFile inserts a file record.
	CreateFile(ctx context.Context, f *FileRecord) error
	// GetFile returns one file record, or ErrNotFound.
	GetFile(ctx context.Context, id string) (*FileRecord, error)
	// ListFiles returns file records newest first.
	ListFiles(ctx context.Context, q FileQuery) ([]*FileRecord, bool, error)
	// DeleteFile removes a file record.
	DeleteFile(ctx context.Context, id string) error
}

// BatchQuery selects a page of batches.
type BatchQuery struct {
	// OwnerKeyID scopes the listing to one principal. Empty lists everything,
	// which only an administrative caller may ask for.
	OwnerKeyID string
	// After is a cursor: only batches older than this id are returned.
	After string
	// Limit is the page size. Zero means the store's default.
	Limit int
}

// FileQuery selects a page of files.
type FileQuery struct {
	OwnerKeyID string
	Purpose    string
	After      string
	Limit      int
}

// Blobs stores file content. The metadata lives in the [Store]; this holds
// bytes and nothing else, so an operator can point it at a disk, an object
// store, or a shared volume without touching the schema.
type Blobs interface {
	// Put writes r under ref, replacing whatever was there, and returns the
	// number of bytes written. It must be atomic against a concurrent Open: a
	// reader sees either the old content or the new one, never a partial write.
	Put(ctx context.Context, ref string, r io.Reader) (int64, error)
	// Open returns the content. The seeker is required: the scheduler reads one
	// row at a time by offset rather than holding a whole input file in memory.
	Open(ctx context.Context, ref string) (io.ReadSeekCloser, error)
	// Remove deletes the content. Removing what is not there is not an error.
	Remove(ctx context.Context, ref string) error
}

// Clock is the package's only source of time, so tests do not sleep.
//
// Sleep returns ctx.Err() if the context ends first. It is used for retry
// backoff and nothing else; expiry is driven by Now and by [Service.Sweep],
// following the same convention internal/capacity uses for its reservation
// sweeper.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// Sleep blocks for d or until ctx ends.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
