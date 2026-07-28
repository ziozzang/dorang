package batch

import "time"

// BatchRecord is a batch as persisted. It maps onto DESIGN §9.2's `batches`
// table with two additions the table does not yet have, both load-bearing:
//
//   - Errors. A failed batch must be able to say why after a restart, and the
//     wire object has an errors list. There is no column for it.
//   - PrincipalID as distinct from OwnerKeyID, so the capacity principal axis
//     can be charged to a user or team rather than to a key.
//
// A zero time.Time means the timestamp is unset, and renders as an omitted field.
type BatchRecord struct {
	ID          string
	OwnerKeyID  string
	PrincipalID string
	Endpoint    string

	InputFileID  string
	OutputFileID string
	ErrorFileID  string

	CompletionWindow time.Duration
	Status           Status
	Counts           RequestCounts
	Errors           []BatchError
	Metadata         map[string]string

	CreatedAt    time.Time
	UpdatedAt    time.Time
	InProgressAt time.Time
	FinalizingAt time.Time
	CompletedAt  time.Time
	FailedAt     time.Time
	CancellingAt time.Time
	CancelledAt  time.Time
	ExpiredAt    time.Time
	ExpiresAt    time.Time
}

// Clone returns a deep copy. The store hands out clones so that a caller
// mutating what it read cannot reach into stored state.
func (b *BatchRecord) Clone() *BatchRecord {
	if b == nil {
		return nil
	}
	c := *b
	if b.Errors != nil {
		c.Errors = append([]BatchError(nil), b.Errors...)
	}
	if b.Metadata != nil {
		c.Metadata = make(map[string]string, len(b.Metadata))
		for k, v := range b.Metadata {
			c.Metadata[k] = v
		}
	}
	return &c
}

// API renders the record as the wire object.
//
// emitQueued selects whether the internal queued status is emitted verbatim or
// rendered as validating for strict OpenAI clients; see [Status.Wire].
func (b *BatchRecord) API(emitQueued bool) *Batch {
	st := b.Status
	if !emitQueued {
		st = st.Wire()
	}
	out := &Batch{
		ID:               b.ID,
		Object:           ObjectBatch,
		Endpoint:         b.Endpoint,
		InputFileID:      b.InputFileID,
		CompletionWindow: formatWindow(b.CompletionWindow),
		Status:           st,
		OutputFileID:     b.OutputFileID,
		ErrorFileID:      b.ErrorFileID,
		CreatedAt:        unix(b.CreatedAt),
		InProgressAt:     unix(b.InProgressAt),
		ExpiresAt:        unix(b.ExpiresAt),
		FinalizingAt:     unix(b.FinalizingAt),
		CompletedAt:      unix(b.CompletedAt),
		FailedAt:         unix(b.FailedAt),
		ExpiredAt:        unix(b.ExpiredAt),
		CancellingAt:     unix(b.CancellingAt),
		CancelledAt:      unix(b.CancelledAt),
		RequestCounts:    b.Counts,
	}
	if len(b.Errors) > 0 {
		out.Errors = &BatchErrorList{Object: ObjectList, Data: append([]BatchError(nil), b.Errors...)}
	}
	if len(b.Metadata) > 0 {
		out.Metadata = make(map[string]string, len(b.Metadata))
		for k, v := range b.Metadata {
			out.Metadata[k] = v
		}
	}
	return out
}

// RowRecord is one row of a batch as persisted. It maps onto DESIGN §9.2's
// `batch_requests` table, with three additions that the table needs:
//
//   - Offset and Length locate the row inside the input blob. Without them the
//     scheduler has to hold the entire input file in memory to dispatch from it,
//     which at the 200 MiB file ceiling is not a notebook-profile amount of
//     memory. With them a worker seeks and reads one row at a time.
//   - Result holds the response body of a finished row. The table has only
//     request_log_id, and the ledger does not store bodies — so after a restart
//     there would be nothing to write the output file from, and every finished
//     row would have to be paid for twice.
type RowRecord struct {
	BatchID  string
	CustomID string
	Seq      int

	// PrefixHash is the grouping key: hex of the shallowest prefix-chain digest
	// of the row body (DESIGN §7.4b). It is a locality hint, never a proof —
	// nothing about correctness depends on two rows sharing it.
	PrefixHash string
	// Model is the client-facing model name from the row body, opaque.
	Model string
	// URL is the row's endpoint.
	URL string

	Offset int64
	Length int

	Status     RowStatus
	Attempts   int
	StatusCode int
	RequestID  string
	Result     []byte
	ErrCode    string
	ErrMessage string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Clone returns a deep copy.
func (r *RowRecord) Clone() *RowRecord {
	if r == nil {
		return nil
	}
	c := *r
	if r.Result != nil {
		c.Result = append([]byte(nil), r.Result...)
	}
	return &c
}

// FileRecord is a file as persisted, mapping onto §9.2's `files` table.
type FileRecord struct {
	ID            string
	OwnerKeyID    string
	Purpose       string
	Filename      string
	Bytes         int64
	SHA256        string
	StorageRef    string
	Status        string
	StatusDetails string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// Clone returns a copy.
func (f *FileRecord) Clone() *FileRecord {
	if f == nil {
		return nil
	}
	c := *f
	return &c
}

// API renders the record as the wire object.
func (f *FileRecord) API() *File {
	return &File{
		ID:            f.ID,
		Object:        ObjectFile,
		Bytes:         f.Bytes,
		CreatedAt:     unix(f.CreatedAt),
		ExpiresAt:     unix(f.ExpiresAt),
		Filename:      f.Filename,
		Purpose:       f.Purpose,
		Status:        f.Status,
		StatusDetails: f.StatusDetails,
	}
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
