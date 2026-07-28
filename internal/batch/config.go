package batch

import (
	"errors"
	"time"
)

// Defaults. Where OpenAI publishes a limit, the default is that limit, so a
// client that works against the vendor works here without discovering a
// narrower ceiling at 3 a.m.
const (
	DefaultMaxFileBytes        = 200 << 20 // 200 MiB
	DefaultMaxRows             = 50_000
	DefaultMaxRowBytes         = 16 << 20
	DefaultMaxCustomIDChars    = 256
	DefaultMaxValidationErrors = 20
	DefaultMaxMetadataKeys     = 16
	DefaultMaxMetadataKeyChars = 64
	DefaultMaxMetadataValChars = 512

	DefaultWindow    = 24 * time.Hour
	DefaultMinWindow = time.Minute
	DefaultMaxWindow = 30 * 24 * time.Hour

	DefaultRowConcurrency   = 8
	DefaultMaxActiveBatches = 4
	DefaultGroupChunk       = 16
	// DefaultGroupSegment is far smaller than prefix.DefaultBaseSegment on
	// purpose; see groupHash.
	DefaultGroupSegment = 512

	DefaultMaxAttempts = 3
	DefaultBackoffBase = 250 * time.Millisecond
	DefaultBackoffMax  = 30 * time.Second

	DefaultCountFlush    = 32
	DefaultSweepInterval = 30 * time.Second

	// DefaultPriorityClass is the class every batch row is admitted and emitted
	// at (DESIGN §7.5). Lower is more urgent on the canonical scale, and batch
	// is the least urgent class there is.
	DefaultPriorityClass = "batch"
)

// DefaultEndpoints are the endpoints a batch row may target. Chat completions,
// completions, embeddings and responses are what OpenAI's batch surface accepts;
// an operator may narrow or extend the list, but an unknown url is rejected at
// validation rather than discovered at dispatch.
func DefaultEndpoints() []string {
	return []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/embeddings",
		"/v1/responses",
	}
}

// DefaultUploadPurposes are the purposes a client may upload with.
// PurposeBatchOutput is absent deliberately: it is a purpose dorang produces,
// and letting a client claim it would let uploaded content masquerade as
// another caller's results.
func DefaultUploadPurposes() []string {
	return []string{PurposeBatch, PurposeUserData, PurposeAssistants, PurposeFineTune, PurposeVision, PurposeEvals}
}

// DefaultRetryStatus are the response statuses a row is retried on.
//
// 408, 425, 429 and the 5xx family are transient by definition. 409 is included
// because several OpenAI-compatible servers use it for "model is loading". A
// 4xx that is not on this list is the caller's own request being wrong, and
// retrying it three times only spends the caller's money three times.
func DefaultRetryStatus() []int {
	return []int{408, 409, 425, 429, 500, 502, 503, 504, 529}
}

// Config is the service's configuration and its dependencies.
//
// The dependency fields are interfaces this package declares (see ports.go), not
// concrete types from internal/router, internal/capacity or internal/store. Zero
// values in the tuning fields select the defaults above.
type Config struct {
	// Store persists batch, row and file metadata. Required.
	Store Store
	// Blobs stores file content. Required.
	Blobs Blobs
	// Executor runs one row as an ordinary request. Required.
	Executor Executor
	// Capacity admits rows against every axis. Required — a nil Reserver would
	// mean batch work bypassing DESIGN §5 entirely, so it is an error rather
	// than a permissive default.
	Capacity Reserver
	// Models resolves a client-facing model name. Required.
	Models ModelResolver
	// Clock is the only source of time. Zero selects SystemClock.
	Clock Clock
	// Logf receives operational messages. Zero discards them.
	Logf func(format string, args ...any)

	// Endpoints a batch row may target. Zero selects DefaultEndpoints.
	Endpoints []string
	// UploadPurposes a client may upload with. Zero selects DefaultUploadPurposes.
	UploadPurposes []string

	// MaxFileBytes is the upload ceiling. Content over it is refused without
	// being written, so the ceiling is also the disk an upload can consume.
	MaxFileBytes int64
	// MaxRows is the per-file request ceiling.
	MaxRows int
	// MaxRowBytes is the per-line ceiling.
	MaxRowBytes int
	// MaxCustomIDChars limits custom_id length.
	MaxCustomIDChars int
	// MaxValidationErrors caps how many failures one validation pass reports.
	MaxValidationErrors int

	// DefaultCompletionWindow, MinCompletionWindow and MaxCompletionWindow bound
	// the completion_window a caller may ask for.
	DefaultCompletionWindow time.Duration
	MinCompletionWindow     time.Duration
	MaxCompletionWindow     time.Duration

	// RowConcurrency is how many rows of one batch may be in flight at once.
	// It is an upper bound on concurrency, not a guarantee of it: capacity
	// (§5) decides how many actually run, and the interactive reserve means a
	// contended axis will admit fewer.
	RowConcurrency int
	// MaxActiveBatches is how many batches dispatch at once. Others wait in
	// arrival order.
	MaxActiveBatches int
	// GroupChunk bounds how many rows of one prefix group go to a single
	// worker before the group is split, so one enormous group cannot serialize
	// a batch.
	GroupChunk int
	// GroupSegment is the prefix-chain base segment used for grouping.
	GroupSegment int

	// MaxAttempts is the total number of tries per row, including the first.
	MaxAttempts int
	// BackoffBase and BackoffMax bound the exponential backoff between tries.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// RetryStatus overrides DefaultRetryStatus.
	RetryStatus []int

	// PriorityClass is the class rows are dispatched at. Zero selects
	// DefaultPriorityClass.
	PriorityClass string
	// EmitQueuedStatus emits the internal queued status verbatim instead of
	// rendering it as validating. See Status.Wire before turning this on.
	EmitQueuedStatus bool
	// CountFlush is how many finished rows pass between progress writes of the
	// batch record. Row state is written every row regardless; this only
	// governs how fresh request_counts looks to a poller.
	CountFlush int
	// SweepInterval is how often expired batches are reaped. Zero selects
	// DefaultSweepInterval; negative disables the background goroutine and
	// leaves Sweep to be called manually, which is what tests do (the same
	// convention internal/capacity uses).
	SweepInterval time.Duration

	// Metadata limits.
	MaxMetadataKeys     int
	MaxMetadataKeyChars int
	MaxMetadataValChars int
}

func (c *Config) fill() error {
	switch {
	case c.Store == nil:
		return errors.New("batch: Config.Store is required")
	case c.Blobs == nil:
		return errors.New("batch: Config.Blobs is required")
	case c.Executor == nil:
		return errors.New("batch: Config.Executor is required")
	case c.Capacity == nil:
		return errors.New("batch: Config.Capacity is required")
	case c.Models == nil:
		return errors.New("batch: Config.Models is required")
	}
	if c.Clock == nil {
		c.Clock = SystemClock{}
	}
	if len(c.Endpoints) == 0 {
		c.Endpoints = DefaultEndpoints()
	}
	if len(c.UploadPurposes) == 0 {
		c.UploadPurposes = DefaultUploadPurposes()
	}
	if len(c.RetryStatus) == 0 {
		c.RetryStatus = DefaultRetryStatus()
	}
	setInt64(&c.MaxFileBytes, DefaultMaxFileBytes)
	setInt(&c.MaxRows, DefaultMaxRows)
	setInt(&c.MaxRowBytes, DefaultMaxRowBytes)
	setInt(&c.MaxCustomIDChars, DefaultMaxCustomIDChars)
	setInt(&c.MaxValidationErrors, DefaultMaxValidationErrors)
	setDur(&c.DefaultCompletionWindow, DefaultWindow)
	setDur(&c.MinCompletionWindow, DefaultMinWindow)
	setDur(&c.MaxCompletionWindow, DefaultMaxWindow)
	setInt(&c.RowConcurrency, DefaultRowConcurrency)
	setInt(&c.MaxActiveBatches, DefaultMaxActiveBatches)
	setInt(&c.GroupChunk, DefaultGroupChunk)
	setInt(&c.GroupSegment, DefaultGroupSegment)
	setInt(&c.MaxAttempts, DefaultMaxAttempts)
	setDur(&c.BackoffBase, DefaultBackoffBase)
	setDur(&c.BackoffMax, DefaultBackoffMax)
	setInt(&c.CountFlush, DefaultCountFlush)
	setInt(&c.MaxMetadataKeys, DefaultMaxMetadataKeys)
	setInt(&c.MaxMetadataKeyChars, DefaultMaxMetadataKeyChars)
	setInt(&c.MaxMetadataValChars, DefaultMaxMetadataValChars)
	if c.SweepInterval == 0 {
		c.SweepInterval = DefaultSweepInterval
	}
	if c.PriorityClass == "" {
		c.PriorityClass = DefaultPriorityClass
	}
	if c.MaxCompletionWindow < c.MinCompletionWindow {
		return errors.New("batch: MaxCompletionWindow is below MinCompletionWindow")
	}
	return nil
}

func setInt(p *int, def int) {
	if *p <= 0 {
		*p = def
	}
}

func setInt64(p *int64, def int64) {
	if *p <= 0 {
		*p = def
	}
}

func setDur(p *time.Duration, def time.Duration) {
	if *p <= 0 {
		*p = def
	}
}
