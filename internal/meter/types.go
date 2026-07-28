package meter

import (
	"cmp"
	"time"
)

// OverflowSentinel is the value every dimension of the fold-to key carries
// when a shard has run out of cardinality budget. It is deliberately ugly and
// deliberately not a valid identifier: a rollup row containing it means
// "resolution was lost here", and Stats.OverflowFolds says how much.
const OverflowSentinel = "__overflow__"

// StatusClass folds an HTTP status into the small fixed set the rollup key
// carries. Carrying the class rather than the code is one of the two things
// that keep the numeric key fixed-cardinality (DESIGN 12.1); the other is that
// the key has no free-form dimension at all.
type StatusClass uint8

const (
	// StatusUnknown is a request with no status: it never reached a decision.
	StatusUnknown StatusClass = iota
	Status2xx
	Status3xx
	Status4xx
	Status5xx
	// StatusCanceled is a client disconnect or a canceled context. It is not
	// an upstream failure and is counted apart from 4xx and 5xx.
	StatusCanceled
	numStatusClasses
)

var statusClassNames = [numStatusClasses]string{
	StatusUnknown:  "unknown",
	Status2xx:      "2xx",
	Status3xx:      "3xx",
	Status4xx:      "4xx",
	Status5xx:      "5xx",
	StatusCanceled: "canceled",
}

func (s StatusClass) String() string {
	if int(s) < len(statusClassNames) {
		return statusClassNames[s]
	}
	return "unknown"
}

// ClassifyStatus folds an HTTP status code into a StatusClass. A code below
// 200 (including zero) is StatusUnknown.
func ClassifyStatus(code int) StatusClass {
	switch {
	case code >= 500:
		return Status5xx
	case code >= 400:
		return Status4xx
	case code >= 300:
		return Status3xx
	case code >= 200:
		return Status2xx
	}
	return StatusUnknown
}

// Key is the fixed-cardinality rollup key of DESIGN 12.1. Every field is an
// identifier drawn from configuration or from a bounded enumeration; none of
// them is caller-controlled free text, which is what makes the accumulator
// boundable at all.
type Key struct {
	APIKeyID     string
	TeamID       string
	ModelGroup   string
	Provider     string
	CredentialID string
	Endpoint     string
	StatusClass  StatusClass
}

// IsOverflow reports whether k is the fold-to key.
func (k Key) IsOverflow() bool { return k.APIKeyID == OverflowSentinel }

func overflowKey() Key {
	return Key{
		APIKeyID:     OverflowSentinel,
		TeamID:       OverflowSentinel,
		ModelGroup:   OverflowSentinel,
		Provider:     OverflowSentinel,
		CredentialID: OverflowSentinel,
		Endpoint:     OverflowSentinel,
		StatusClass:  StatusUnknown,
	}
}

// CompareKey orders keys so that flushed buckets reach the sink in a stable
// order. Ordering is not semantically required; it makes writes predictable
// and tests deterministic.
func CompareKey(a, b Key) int {
	if c := cmp.Compare(a.APIKeyID, b.APIKeyID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.TeamID, b.TeamID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.ModelGroup, b.ModelGroup); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Provider, b.Provider); c != 0 {
		return c
	}
	if c := cmp.Compare(a.CredentialID, b.CredentialID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Endpoint, b.Endpoint); c != 0 {
		return c
	}
	return cmp.Compare(a.StatusClass, b.StatusClass)
}

// Tokens is the token breakdown carried by both paths.
type Tokens struct {
	Input     int64
	Output    int64
	CacheRead int64
	// CacheWrite is cache-creation input, priced separately from CacheRead
	// by every provider that offers it.
	CacheWrite int64
	Reasoning  int64
}

// Total is the sum of every token dimension.
func (t Tokens) Total() int64 {
	return t.Input + t.Output + t.CacheRead + t.CacheWrite + t.Reasoning
}

// IsZero reports whether no token was counted.
func (t Tokens) IsZero() bool { return t == Tokens{} }

func (t *Tokens) add(o Tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.Reasoning += o.Reasoning
}

// TraceInfo is the droppable half of an Event: the per-request payload that
// becomes a row in request_traces. It is separate from the numeric fields
// because it is variable-length and therefore not boundable, which is exactly
// why it may be dropped and the numeric half may not.
//
// A zero RequestID means "no trace payload"; the numeric half is still
// recorded. Excerpt is copied into the meter's own storage on Record, so the
// caller may reuse its buffer immediately.
type TraceInfo struct {
	RequestID    string
	TraceID      string
	SpanID       string
	ParentSpanID string

	// UpstreamModel is the provider-side name actually sent, which may differ
	// from ModelGroup after aliasing (DESIGN 7.2).
	UpstreamModel string

	// Excerpt is the message content excerpt. It is truncated to
	// Config.ExcerptChars runes and subject to Config.ExcerptMode; under
	// ExcerptHash only its digest is kept, under ExcerptNone nothing is.
	Excerpt string

	// Latency breakdown of DESIGN 12.2. Latency and TTFT live on Event
	// because they are also numeric-path values; these are trace-only.
	QueueWait       time.Duration
	RouteTime       time.Duration
	CapacityWait    time.Duration
	UpstreamConnect time.Duration

	Retries        int
	FallbackReason string
	ErrorMessage   string
}

// Event is one completed request, handed to Record on the hot path. It is
// passed by value and copied into the accumulator; Record retains no pointer
// into the caller's memory.
type Event struct {
	// Time is when the request completed. Zero means "now" and costs a clock
	// read; supplying it is cheaper and more accurate.
	Time time.Time

	APIKeyID string
	// SecretID names which of the key's secrets authenticated (DESIGN §11.2c).
	// Like UserID it is carried on the trace path only: it belongs in the
	// ledger, and adding it to [Key] would multiply rollup cardinality across a
	// rotation for a materialization nothing queries by secret.
	SecretID string
	// UserID is the owning user. It is carried on the trace path only: the
	// ledger has a user_id column (DESIGN §9.2) and an index on it, while
	// §9.4's rollups are keyed by key, model and team. Adding it to [Key] would
	// multiply rollup cardinality for a materialization nothing queries.
	UserID       string
	TeamID       string
	ModelGroup   string
	Provider     string
	CredentialID string
	Endpoint     string

	// Status is the HTTP status returned to the client, folded to a
	// StatusClass for the key. Canceled takes precedence over Status.
	Status   int
	Canceled bool
	// Error forces the event to count as an error even on a 2xx status, for
	// failures that were recovered before the client saw them.
	Error bool

	Tokens   Tokens
	CostNano int64
	Latency  time.Duration
	TTFT     time.Duration

	Trace TraceInfo
}

// key derives the fixed-cardinality rollup key from the event.
func (e *Event) key() Key {
	sc := ClassifyStatus(e.Status)
	if e.Canceled {
		sc = StatusCanceled
	}
	return Key{
		APIKeyID:     e.APIKeyID,
		TeamID:       e.TeamID,
		ModelGroup:   e.ModelGroup,
		Provider:     e.Provider,
		CredentialID: e.CredentialID,
		Endpoint:     e.Endpoint,
		StatusClass:  sc,
	}
}

func (e *Event) isError() bool { return e.Error || e.Status >= 400 }

// Bucket is one merged rollup row: a Key, the UTC hour it belongs to, and the
// counters accumulated for it. It is the unit Sink.WriteRollups accepts, and
// it maps onto the purpose-built materializations of DESIGN 9.4 without the
// writer having to re-aggregate.
type Bucket struct {
	Key
	// HourStart is the beginning of the UTC hour this bucket covers. The hour
	// is part of the accumulator key, not derived at flush time, so a flush
	// window straddling an hour boundary still attributes exactly.
	HourStart time.Time

	Requests int64
	Errors   int64
	Tokens   Tokens
	CostNano int64

	// LatencySum and TTFTSum are sums, not averages; the store divides by
	// Requests and TTFTCount respectively. Summing keeps merges associative.
	LatencySum time.Duration
	TTFTSum    time.Duration
	// TTFTCount is the number of requests that reported a TTFT, which is
	// fewer than Requests for non-streaming calls.
	TTFTCount int64
}

func (b *Bucket) addCounters(c *counters) {
	b.Requests += c.requests
	b.Errors += c.errors
	b.Tokens.add(c.tokens)
	b.CostNano += c.costNano
	b.LatencySum += time.Duration(c.latencySum)
	b.TTFTSum += time.Duration(c.ttftSum)
	b.TTFTCount += c.ttftCount
}

func (b *Bucket) addBucket(o *Bucket) {
	b.Requests += o.Requests
	b.Errors += o.Errors
	b.Tokens.add(o.Tokens)
	b.CostNano += o.CostNano
	b.LatencySum += o.LatencySum
	b.TTFTSum += o.TTFTSum
	b.TTFTCount += o.TTFTCount
}

// Trace is one per-request trace payload as it reaches the sink. It carries
// the dimensions too, so the ledger writer does not have to join against a
// rollup to know what the request was.
type Trace struct {
	RequestID    string
	TraceID      string
	SpanID       string
	ParentSpanID string
	Time         time.Time

	APIKeyID string
	// SecretID names WHICH of the key's secrets authenticated the request
	// (DESIGN §11.2c). During a rotation's grace period a key has two, both
	// authenticate, and the whole value of the grace period is that an operator
	// can see whether the client actually rolled BEFORE the window closes
	// rather than finding out when it shuts. The key id alone cannot say that.
	SecretID      string
	UserID        string
	TeamID        string
	ModelGroup    string
	Provider      string
	CredentialID  string
	Endpoint      string
	UpstreamModel string

	Status int

	// Excerpt is the truncated message excerpt, or under ExcerptHash the hex
	// SHA-256 of it prefixed with "sha256:", or empty under ExcerptNone.
	Excerpt string

	Latency         time.Duration
	TTFT            time.Duration
	QueueWait       time.Duration
	RouteTime       time.Duration
	CapacityWait    time.Duration
	UpstreamConnect time.Duration

	Tokens   Tokens
	CostNano int64

	Retries        int
	FallbackReason string
	ErrorMessage   string
}

// Reason says why the meter is degraded. It is reported through Degraded and
// Stats so that metering_degraded (DESIGN 12.3) has a cause attached rather
// than being a bare boolean.
type Reason uint8

const (
	// ReasonNone means not degraded.
	ReasonNone Reason = iota
	// ReasonQueueFull means the in-memory trace ring was full when a trace
	// arrived: the drainer could not keep up, or the spool behind it is full.
	ReasonQueueFull
	// ReasonSpoolFull means the durable spool hit its size cap. The sink has
	// been unable to accept traces for long enough to fill the disk budget.
	ReasonSpoolFull
	// ReasonSpoolError means the spool could not be read or written. Traces
	// are being dropped because durability itself failed.
	ReasonSpoolError
	// ReasonSinkError means the sink rejected a write. Traces are still
	// durable in the spool; this is a warning, not a loss.
	ReasonSinkError
	numReasons
)

var reasonNames = [numReasons]string{
	ReasonNone:       "none",
	ReasonQueueFull:  "trace_queue_full",
	ReasonSpoolFull:  "spool_full",
	ReasonSpoolError: "spool_error",
	ReasonSinkError:  "sink_error",
}

func (r Reason) String() string {
	if int(r) < len(reasonNames) {
		return reasonNames[r]
	}
	return "unknown"
}

// Stats is a point-in-time snapshot of the meter's own accounting. Everything
// that can be lost is counted here, which is what "the drop is visible" means
// in practice.
type Stats struct {
	// Recorded is the number of events accepted by the numeric path. It is
	// exact: the numeric path has no drop.
	Recorded int64

	// TracesRecorded is the number of events that carried a trace payload and
	// survived sampling and the byte budget.
	TracesRecorded int64
	// TracesSampledOut and TracesBudgetDropped are policy exclusions, not
	// failures, and do not raise Degraded.
	TracesSampledOut    int64
	TracesBudgetDropped int64

	// TracesDroppedQueue and TracesDroppedSpool are failures. Their sum is
	// TracesDropped, and any non-zero increment raises Degraded.
	TracesDroppedQueue int64
	TracesDroppedSpool int64
	TracesDropped      int64

	// TracesSpooled is the number of traces written to durable storage;
	// TracesFlushed the number the sink has accepted.
	TracesSpooled int64
	TracesFlushed int64

	// BucketsFlushed is the number of rollup buckets the sink has accepted.
	BucketsFlushed int64
	// PendingBuckets is the carry-over held because the sink refused a write.
	PendingBuckets int64
	// PendingFolds counts carry-over buckets folded into overflow because the
	// carry-over itself hit its cap.
	PendingFolds int64

	// OverflowFolds is the number of events folded into OverflowSentinel
	// because a shard was at its cardinality cap.
	OverflowFolds int64

	// QueueDepth, SpoolDepth and SpoolBytes are the trace path's backlog.
	QueueDepth int64
	SpoolDepth int64
	SpoolBytes int64

	// FlushErrors counts failed sink writes on either path.
	FlushErrors int64
	// ShardCollisions counts Record calls whose affinity hint landed on a
	// shard that was already held. A high ratio means the hint is degenerate
	// on this platform, not that anything is wrong.
	ShardCollisions int64

	Shards int

	Degraded bool
	Reason   Reason
}
