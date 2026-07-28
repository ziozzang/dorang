package meter

import (
	"math"
	"sync/atomic"
)

// sampler decides which trace payloads are admitted. It enforces the two
// controls DESIGN 12.2 requires -- a rate and a daily byte budget -- and both
// have to hold, because a rate alone does not bound bytes when excerpts vary
// in size, and a budget alone spends the whole day's allowance in the first
// minute of a traffic spike.
type sampler struct {
	rate      float64
	threshold uint64
	budget    int64

	day  atomic.Int64
	used atomic.Int64
}

func initSampler(s *sampler, rate float64, budget int64) {
	s.rate = rate
	// math.MaxUint64 is not exactly representable as a float64; scaling by it
	// and converting back can overflow, so scale by 2^64 in float space and
	// clamp. The imprecision is a few parts in 2^53 of the sampling rate.
	switch {
	case rate <= 0:
		s.threshold = 0
	case rate >= 1:
		s.threshold = math.MaxUint64
	default:
		s.threshold = uint64(rate * 18446744073709551616.0)
	}
	s.budget = budget
	s.day.Store(math.MinInt64)
}

// admit reports whether the trace for id is sampled in. Sampling is
// deterministic in the request id rather than random, so a request is either
// traced by every node that touches it or by none: a randomly sampled fleet
// produces fragments of traces, which is worse than fewer whole ones.
func (s *sampler) admit(id string) bool {
	switch s.threshold {
	case 0:
		return false
	case math.MaxUint64:
		return true
	}
	return fnv64a(id) < s.threshold
}

// charge accounts n estimated bytes against the UTC-day budget and reports
// whether they fit. ts is the event's unix time, so the hot path does not need
// a clock read.
func (s *sampler) charge(ts int64, n int64) bool {
	if s.budget <= 0 {
		return true
	}
	day := ts / 86400
	if cur := s.day.Load(); cur != day {
		// A concurrent rollover can let two goroutines both reset, costing at
		// most one in-flight charge. This is a budget, not accounting; the
		// numeric path is where exactness is required.
		if s.day.CompareAndSwap(cur, day) {
			s.used.Store(0)
		}
	}
	if s.used.Add(n) <= s.budget {
		return true
	}
	// Refund what was not admitted. Without this the counter runs away with
	// every rejected trace, so a day's traffic past the budget would leave
	// used at some number with no relation to bytes actually stored -- and a
	// budget that cannot be read back is not enforced, only asserted.
	s.used.Add(-n)
	return false
}

// refund returns bytes charged for a trace that never made it into the queue.
// Without it a burst that overruns the ring would spend the day's allowance on
// traces nobody ever stored, so the budget would throttle the wrong traffic.
func (s *sampler) refund(n int64) {
	if s.budget > 0 {
		s.used.Add(-n)
	}
}

// bytesUsed reports the current day's charged bytes, for tests and metrics.
func (s *sampler) bytesUsed() int64 { return s.used.Load() }

const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// fnv64a hashes a string without allocating. hash/fnv would require a
// []byte(s) conversion, which allocates, and this runs on the hot path.
func fnv64a(s string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	// FNV's low bits are well mixed but its high bits are not, and the
	// threshold comparison is on the high bits. One splitmix64 finalizer
	// makes the distribution uniform enough that a rate of 0.01 really is
	// one in a hundred.
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// traceOverheadBytes is the fixed part of a spooled trace: framing, the
// numeric fields, and the varint lengths. It is an estimate used only for the
// byte budget, which is a policy control and not an accounting record.
const traceOverheadBytes = 160

func estimateTraceBytes(t *TraceInfo, excerptLen int) int64 {
	n := traceOverheadBytes + excerptLen +
		len(t.RequestID) + len(t.TraceID) + len(t.SpanID) + len(t.ParentSpanID) +
		len(t.UpstreamModel) + len(t.FallbackReason) + len(t.ErrorMessage)
	return int64(n)
}
