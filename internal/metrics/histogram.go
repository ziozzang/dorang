package metrics

import (
	"sync/atomic"
	"time"
)

// DurationBounds are the gateway-overhead histogram's cumulative bounds, in
// seconds.
//
// DESIGN §15.1 states the warm-local profile as p50 249 µs and p99 2 ms. A
// histogram whose first bucket is 5 ms reports "everything is under 5 ms",
// which is true, useless, and indistinguishable from a twenty-fold regression.
// So the low end resolves microseconds and the bounds thin out only once past
// the point where the number stops being about dorang and starts being about
// the upstream.
//
// These bounds were chosen when §15.1 published a 200 µs p50, and they are left
// alone now that it is measured at 249 µs. Resolving BELOW the figure is the
// requirement, and four bounds still sit below it. The move is if anything an
// improvement: 200 µs used to be the target and is now the bound just under the
// median, so the p50 falls INSIDE (200 µs, 300 µs] instead of on its edge.
// Against a lognormal fitted to §15.1's measured 4 KiB quantiles — p50 249 µs,
// p90 324 µs, p99 429 µs — those two bounds hold 16% and 80% of the mass, and
// Prometheus' histogram_quantile recovers the median as 253 µs, 1.6% high. It
// recovers the p50 within 2% at every body size §15.1 measures.
//
// What these bounds do NOT resolve well is the upper tail of that same profile,
// and it is worth writing down because it is not what it looks like: the
// measured p90 and p99 both land in (300 µs, 500 µs], so histogram_quantile
// reports the p90 as ~399 µs against a true 324 and the p99 as ~491 against a
// true 429. That is not damage done by the p50 moving. It is the profile
// getting FASTER than the shape these bounds were drawn for — they were spread
// to carry a distribution running from 200 µs out to a 2 ms p99, and the real
// one now ends at 429 µs, so the six bounds from 750 µs to 3 ms that were meant
// to carry the tail are empty and the tail is squeezed into two. The 2 ms p99
// TARGET is still an exact bound, so whether the profile passes is still
// answered exactly; only the reported value of the tail is biased high.
var DurationBounds = []float64{
	0.00005, 0.0001, 0.00015, 0.0002, 0.0003, 0.0005, 0.00075,
	0.001, 0.0015, 0.002, 0.003, 0.005, 0.0075,
	0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
	1, 2.5, 5, 10, 30, 60, 300,
}

// TTFTBounds are the time-to-first-token bounds, in seconds.
//
// This one measures the upstream, not dorang: §15.1's "added TTFT p99 < 1 ms"
// is the gateway's contribution, which is a term of the duration histogram
// above. A real first token arrives in tens of milliseconds at best, so the
// resolution sits there — but the sub-millisecond buckets are kept, because a
// cached or refused response can return one immediately and a histogram that
// cannot represent that reports it as the same as 5 ms.
var TTFTBounds = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
	1, 2, 3, 5, 7.5, 10, 15, 20, 30, 60, 120, 300,
}

// WaitBounds are the capacity-wait bounds, in seconds.
//
// Zero wait is the overwhelmingly common case and is the one worth resolving:
// the question a capacity histogram answers is "is anything queuing at all",
// and the difference between 10 µs and 1 ms of wait is the difference between
// an uncontended acquire and a contended one.
var WaitBounds = []float64{
	0.00001, 0.0001, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1,
	0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
}

// Hist is a fixed-bucket cumulative histogram over durations.
//
// Sums are kept in nanoseconds as an integer rather than as float64 bits behind
// a compare-and-swap loop. Every histogram in this package measures a duration,
// integers add exactly, and the CAS loop would be the only unbounded operation
// on the observation path.
type Hist struct {
	bounds   []float64
	boundsNS []int64
	counts   []atomic.Uint64
	sumNS    atomic.Uint64
	count    atomic.Uint64
}

// NewHist builds a histogram over bounds, which must be sorted ascending and
// are not copied.
func NewHist(bounds []float64) *Hist {
	h := &Hist{
		bounds:   bounds,
		boundsNS: make([]int64, len(bounds)),
		counts:   make([]atomic.Uint64, len(bounds)+1),
	}
	for i, b := range bounds {
		h.boundsNS[i] = int64(b * float64(time.Second))
	}
	return h
}

// Observe records one duration.
func (h *Hist) Observe(d time.Duration) {
	ns := int64(d)
	if ns < 0 {
		ns = 0
	}
	// Linear from the low end: the bounds are dense exactly where the samples
	// are, so the common case terminates in a handful of comparisons and the
	// branch predictor learns it. A binary search over 26 elements is not
	// faster here and is harder to read.
	i := 0
	for ; i < len(h.boundsNS); i++ {
		if ns <= h.boundsNS[i] {
			break
		}
	}
	h.counts[i].Add(1)
	h.sumNS.Add(uint64(ns))
	h.count.Add(1)
}

// Count is how many samples have been recorded. Zero means the histogram has no
// opinion, and a caller that must distinguish "nothing happened" from "nothing
// is configured" checks this rather than reading a zero sum as a measurement.
func (h *Hist) Count() uint64 { return h.count.Load() }

// snapshot reads the buckets into dst, which the caller owns and reuses so that
// rendering a thousand histograms costs no allocations.
//
// The counters are individually consistent, not consistent as a set: a sample
// landing between two reads is counted in one and not the other, which is an
// error smaller than one sample in a histogram whose whole purpose is to be
// read at scrape intervals.
func (h *Hist) snapshot(dst []uint64) (counts []uint64, sum float64, total uint64) {
	if cap(dst) < len(h.counts) {
		dst = make([]uint64, len(h.counts))
	}
	counts = dst[:len(h.counts)]
	for i := range h.counts {
		counts[i] = h.counts[i].Load()
	}
	return counts, float64(h.sumNS.Load()) / float64(time.Second), h.count.Load()
}
