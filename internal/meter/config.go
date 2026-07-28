package meter

import (
	"errors"
	"math/bits"
	"runtime"
	"time"
)

// ExcerptMode is the DESIGN 12.2 excerpt policy: a truncated excerpt by
// default, with none and hash also available.
type ExcerptMode uint8

const (
	// ExcerptText stores a truncated excerpt. This is the default.
	ExcerptText ExcerptMode = iota
	// ExcerptNone stores nothing. The trace row still exists.
	ExcerptNone
	// ExcerptHash stores "sha256:" plus the hex digest of the truncated
	// excerpt. The digest is computed as the trace leaves the in-memory ring,
	// so content reaches neither the spool nor the store -- only the process's
	// own memory, and only until the next drain. Note that it digests what was
	// kept after truncation, not the whole message, so two messages sharing a
	// prefix longer than the excerpt collide by design.
	ExcerptHash
)

func (m ExcerptMode) String() string {
	switch m {
	case ExcerptNone:
		return "none"
	case ExcerptHash:
		return "hash"
	default:
		return "excerpt"
	}
}

// ParseExcerptMode maps the configuration spelling onto an ExcerptMode.
func ParseExcerptMode(s string) (ExcerptMode, error) {
	switch s {
	case "", "excerpt", "text":
		return ExcerptText, nil
	case "none":
		return ExcerptNone, nil
	case "hash":
		return ExcerptHash, nil
	}
	return 0, errors.New("meter: unknown excerpt mode " + s + " (want excerpt, none or hash)")
}

// Defaults. Every one of these is clamped rather than validated, because New
// has no error return: a nonsensical value becomes the default and the meter
// still runs. Metering must never be the reason a gateway refuses to start.
const (
	DefaultMaxKeysPerShard   = 1024
	DefaultMaxPendingBuckets = 16384
	DefaultFlushInterval     = 10 * time.Second
	DefaultEvictAfterFlushes = 6

	DefaultTraceQueueSize = 2048
	DefaultDrainInterval  = 25 * time.Millisecond
	DefaultShipInterval   = 250 * time.Millisecond
	DefaultSpoolBatch     = 256

	DefaultExcerptChars = 512
	DefaultSampleRate   = 1.0

	DefaultSpoolMaxBytes     = 256 << 20
	DefaultSpoolSegmentBytes = 8 << 20

	DefaultCloseTimeout = 5 * time.Second

	// maxExcerptBytes caps the per-slot excerpt arena. The ring preallocates
	// TraceQueueSize * excerptBytes, so this bounds idle RSS contribution
	// (DESIGN 15.1 targets < 100 MB idle on the notebook profile).
	maxExcerptBytes = 2048
	maxShards       = 256
	minShards       = 2
)

// Config configures a Meter. The zero value is usable: it yields a meter with
// a nop sink, an in-memory spool, and the defaults above.
type Config struct {
	// Sink receives merged rollups and spooled traces. Nil means a nop sink,
	// which is useful for benchmarking the producer alone.
	Sink Sink

	// Now is the clock, injectable for tests. Nil means time.Now.
	Now func() time.Time

	// Shards is the number of numeric accumulator shards. It is rounded up to
	// a power of two and clamped to [minShards, maxShards]. Zero means
	// runtime.NumCPU rounded up to a power of two.
	//
	// Total distinct keys the accumulator can hold is Shards *
	// MaxKeysPerShard, because a key may appear in every shard: the affinity
	// hint spreads by goroutine, not by key. Spreading by key would bound
	// total cardinality but would reintroduce the hot-row contention DESIGN
	// 9.4 exists to avoid, so the duplication is the deliberate trade.
	Shards int

	// MaxKeysPerShard caps distinct keys per shard. Past the cap, events fold
	// into OverflowSentinel and Stats.OverflowFolds counts the folds.
	MaxKeysPerShard int

	// MaxPendingBuckets caps the carry-over held when the sink refuses
	// rollups. Past it the tail folds into per-hour overflow buckets.
	MaxPendingBuckets int

	// FlushInterval is how often the numeric path merges shards and writes
	// rollups. Zero means DefaultFlushInterval; negative disables the
	// background loop, leaving Flush to the caller.
	FlushInterval time.Duration

	// EvictAfterFlushes is how many consecutive idle flushes a key survives
	// before its accumulator slot is returned to the free list. Larger keeps
	// the steady-state working set resident, which is what keeps Record
	// allocation-free.
	EvictAfterFlushes int

	// TraceQueueSize is the bounded in-memory trace ring, rounded up to a
	// power of two. When it is full, traces drop and Degraded rises.
	TraceQueueSize int

	// DrainInterval is how often the ring is emptied into the spool.
	// ShipInterval is how often the spool is offered to the sink. Negative
	// disables the respective background loop.
	DrainInterval time.Duration
	ShipInterval  time.Duration

	// SpoolBatch is the maximum number of traces handed to Sink.WriteTraces
	// at once.
	SpoolBatch int

	// SampleRate in [0,1] selects trace payloads. Sampling is deterministic in
	// the request id, so a given request is either traced everywhere or
	// nowhere rather than half-traced across a fleet.
	SampleRate float64

	// DailyByteBudget caps the estimated trace bytes admitted per UTC day.
	// Zero or negative means unlimited. Exceeding it is policy, not failure:
	// it counts into Stats.TracesBudgetDropped and does not raise Degraded.
	DailyByteBudget int64

	ExcerptMode ExcerptMode
	// ExcerptChars is the excerpt truncation length in runes.
	ExcerptChars int

	// SpoolDir is the durable spool directory. Empty means an in-memory spool
	// bounded by SpoolMaxBytes -- correct, but it forfeits the whole point of
	// the spool, so production configurations must set it.
	//
	// A directory belongs to exactly one Meter. Two processes pointed at the
	// same directory will interleave segments and corrupt each other's cursor;
	// on a multi-node deployment give each node its own path.
	SpoolDir string
	// SpoolMaxBytes caps the spool's total size. SpoolSegmentBytes is the
	// per-segment roll size.
	SpoolMaxBytes     int64
	SpoolSegmentBytes int64
	// SpoolSync fsyncs after every append batch. Off by default: the spool
	// protects against a stalled store, which is common, at the cost of a
	// window against machine loss, which is not.
	SpoolSync bool

	// CloseTimeout bounds the final flush attempt in Close. The spool is
	// always closed cleanly regardless.
	CloseTimeout time.Duration
}

// withDefaults returns cfg with every field clamped into range.
func (c Config) withDefaults() Config {
	if c.Sink == nil {
		c.Sink = nopSink{}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Shards <= 0 {
		c.Shards = runtime.NumCPU()
	}
	c.Shards = clampPow2(c.Shards, minShards, maxShards)

	if c.MaxKeysPerShard <= 0 {
		c.MaxKeysPerShard = DefaultMaxKeysPerShard
	}
	if c.MaxPendingBuckets <= 0 {
		c.MaxPendingBuckets = DefaultMaxPendingBuckets
	}
	if c.FlushInterval == 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	if c.EvictAfterFlushes <= 0 {
		c.EvictAfterFlushes = DefaultEvictAfterFlushes
	}
	if c.EvictAfterFlushes > 250 {
		c.EvictAfterFlushes = 250
	}

	if c.TraceQueueSize <= 0 {
		c.TraceQueueSize = DefaultTraceQueueSize
	}
	c.TraceQueueSize = clampPow2(c.TraceQueueSize, 2, 1<<20)

	if c.DrainInterval == 0 {
		c.DrainInterval = DefaultDrainInterval
	}
	if c.ShipInterval == 0 {
		c.ShipInterval = DefaultShipInterval
	}
	if c.SpoolBatch <= 0 {
		c.SpoolBatch = DefaultSpoolBatch
	}

	if c.SampleRate == 0 {
		c.SampleRate = DefaultSampleRate
	}
	if c.SampleRate < 0 {
		c.SampleRate = 0
	}
	if c.SampleRate > 1 {
		c.SampleRate = 1
	}

	if c.ExcerptChars <= 0 {
		c.ExcerptChars = DefaultExcerptChars
	}
	if c.ExcerptChars > maxExcerptBytes {
		// The per-slot arena cannot hold more than maxExcerptBytes, so a
		// larger setting would silently deliver less than it asked for.
		// Clamping makes the configured number the one that actually holds.
		c.ExcerptChars = maxExcerptBytes
	}
	if c.ExcerptMode != ExcerptText && c.ExcerptMode != ExcerptNone && c.ExcerptMode != ExcerptHash {
		c.ExcerptMode = ExcerptText
	}

	if c.SpoolMaxBytes <= 0 {
		c.SpoolMaxBytes = DefaultSpoolMaxBytes
	}
	if c.SpoolSegmentBytes <= 0 {
		c.SpoolSegmentBytes = DefaultSpoolSegmentBytes
	}
	if c.SpoolSegmentBytes > c.SpoolMaxBytes {
		c.SpoolSegmentBytes = c.SpoolMaxBytes
	}
	if c.CloseTimeout <= 0 {
		c.CloseTimeout = DefaultCloseTimeout
	}
	return c
}

// excerptBytes is the per-slot arena size: enough for ExcerptChars runes of
// worst-case UTF-8, capped so the ring stays a bounded allocation.
func (c Config) excerptBytes() int {
	if c.ExcerptMode == ExcerptNone {
		return 0
	}
	n := c.ExcerptChars * 4
	if n > maxExcerptBytes {
		n = maxExcerptBytes
	}
	if n < 16 {
		n = 16
	}
	return n
}

func clampPow2(n, lo, hi int) int {
	if n < lo {
		n = lo
	}
	if n > hi {
		n = hi
	}
	if n&(n-1) != 0 {
		n = 1 << bits.Len(uint(n-1))
	}
	if n > hi {
		n = hi
	}
	return n
}
