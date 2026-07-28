package batch

import "encoding/json"

// Object type strings, on the wire exactly as OpenAI spells them.
const (
	ObjectBatch = "batch"
	ObjectFile  = "file"
	ObjectList  = "list"
)

// Status is a batch's lifecycle state (DESIGN §11.1).
//
// One value needs care. §11.1's state machine names a queued state, and this
// package implements it — a batch that has been validated but has not been given
// a dispatch slot is genuinely waiting, and calling that "in progress" would be a
// lie to anyone reading the ledger. But "queued" is NOT in OpenAI's batch status
// enum, and a strict client that parses status into a closed set rejects it. So
// queued is the internal truth and [Status.Wire] renders it as validating, which
// is the OpenAI state that means the same thing to a caller: accepted, not
// started. [Config.EmitQueuedStatus] opts into emitting the true value for
// callers that prefer accuracy over strict compatibility.
type Status string

// The batch lifecycle.
const (
	StatusValidating Status = "validating"
	StatusQueued     Status = "queued" // dorang-internal; see Status.Wire
	StatusInProgress Status = "in_progress"
	StatusFinalizing Status = "finalizing"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusExpired    Status = "expired"
	StatusCancelling Status = "cancelling"
	StatusCancelled  Status = "cancelled"
)

// Terminal reports whether no further transition is possible.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusExpired, StatusCancelled:
		return true
	}
	return false
}

// Wire returns the status as an OpenAI client must see it.
func (s Status) Wire() Status {
	if s == StatusQueued {
		return StatusValidating
	}
	return s
}

// RowStatus is one row's state. A row is queued until it reaches a terminal
// state; there is deliberately no persisted running state, because a row
// interrupted mid-flight is simply re-executed and the write it would have cost
// is one per row per batch at every scale.
type RowStatus string

// Row states.
const (
	RowQueued    RowStatus = "queued"
	RowCompleted RowStatus = "completed"
	RowFailed    RowStatus = "failed"
)

// Terminal reports whether the row will not be executed again.
func (s RowStatus) Terminal() bool { return s == RowCompleted || s == RowFailed }

// RequestCounts is the batch's row tally.
//
// Completed plus Failed equals Total only for a batch that ran to completion. A
// cancelled or expired batch reports fewer, because the rows that never ran are
// neither completed nor failed — which is the honest answer and the one OpenAI
// gives.
type RequestCounts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// Batch is the wire object returned by the batch endpoints.
//
// Timestamps are Unix seconds and omitted when unset, per COMPATIBILITY §2.1's
// rule that an absent field is omitted rather than emitted as null.
type Batch struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	Endpoint         string            `json:"endpoint"`
	Errors           *BatchErrorList   `json:"errors,omitempty"`
	InputFileID      string            `json:"input_file_id"`
	CompletionWindow string            `json:"completion_window"`
	Status           Status            `json:"status"`
	OutputFileID     string            `json:"output_file_id,omitempty"`
	ErrorFileID      string            `json:"error_file_id,omitempty"`
	CreatedAt        int64             `json:"created_at"`
	InProgressAt     int64             `json:"in_progress_at,omitempty"`
	ExpiresAt        int64             `json:"expires_at,omitempty"`
	FinalizingAt     int64             `json:"finalizing_at,omitempty"`
	CompletedAt      int64             `json:"completed_at,omitempty"`
	FailedAt         int64             `json:"failed_at,omitempty"`
	ExpiredAt        int64             `json:"expired_at,omitempty"`
	CancellingAt     int64             `json:"cancelling_at,omitempty"`
	CancelledAt      int64             `json:"cancelled_at,omitempty"`
	RequestCounts    RequestCounts     `json:"request_counts"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// BatchErrorList is the errors envelope on a failed batch.
type BatchErrorList struct {
	Object string       `json:"object"`
	Data   []BatchError `json:"data"`
}

// BatchError is one validation failure. Line is 1-based and names the offending
// line of the input file; it is omitted when the failure is not line-scoped.
type BatchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
	Line    int    `json:"line,omitempty"`
}

// BatchList is the list envelope for GET /v1/batches.
type BatchList struct {
	Object  string   `json:"object"`
	Data    []*Batch `json:"data"`
	FirstID string   `json:"first_id,omitempty"`
	LastID  string   `json:"last_id,omitempty"`
	HasMore bool     `json:"has_more"`
}

// File statuses. The field is deprecated on the OpenAI surface but still emitted,
// and clients still read it.
const (
	FileUploaded  = "uploaded"
	FileProcessed = "processed"
	FileError     = "error"
)

// Purposes. PurposeBatchOutput is produced by dorang and may not be uploaded:
// a client that could upload a file claiming to be batch output could hand
// another caller's results back to itself through a batch id it guessed.
const (
	PurposeBatch       = "batch"
	PurposeBatchOutput = "batch_output"
	PurposeUserData    = "user_data"
	PurposeAssistants  = "assistants"
	PurposeFineTune    = "fine-tune"
	PurposeVision      = "vision"
	PurposeEvals       = "evals"
)

// File is the wire object returned by the files endpoints.
type File struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Bytes         int64  `json:"bytes"`
	CreatedAt     int64  `json:"created_at"`
	ExpiresAt     int64  `json:"expires_at,omitempty"`
	Filename      string `json:"filename"`
	Purpose       string `json:"purpose"`
	Status        string `json:"status"`
	StatusDetails string `json:"status_details,omitempty"`
}

// FileList is the list envelope for GET /v1/files.
type FileList struct {
	Object  string  `json:"object"`
	Data    []*File `json:"data"`
	FirstID string  `json:"first_id,omitempty"`
	LastID  string  `json:"last_id,omitempty"`
	HasMore bool    `json:"has_more"`
}

// FileDeleted is the wire object returned by DELETE /v1/files/{id}.
type FileDeleted struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

// InputRow is one line of a batch input file.
type InputRow struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

// OutputRow is one line of an output or error file.
//
// Response and Error are pointers without omitempty on purpose: OpenAI emits
// both keys on every line, with the unused one explicitly null, and clients
// index them unconditionally. COMPATIBILITY §2.1's omit-null rule is scoped to
// chat-completion chunks and does not reach here.
type OutputRow struct {
	ID       string       `json:"id"`
	CustomID string       `json:"custom_id"`
	Response *RowResponse `json:"response"`
	Error    *RowError    `json:"error"`
}

// RowResponse is the upstream answer to one row. Body is the response as the
// client would have received it from a direct call.
type RowResponse struct {
	StatusCode int             `json:"status_code"`
	RequestID  string          `json:"request_id,omitempty"`
	Body       json.RawMessage `json:"body"`
}

// RowError is a row failure that produced no HTTP response at all — a transport
// failure, a routing failure, or a capacity refusal. A row that received a real
// error *status* carries that status and body in Response instead, because the
// caller wants the upstream's own error envelope.
type RowError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
