package quota

import (
	"fmt"
	"time"
)

// counter accumulates one metric over one window.
//
// A rolling window is a ring of one-minute buckets with a running total
// (DESIGN §6.1). Maintenance is O(1) per record and query is O(1): advancing
// the clock subtracts only the buckets that fall out, which is amortized O(1)
// per elapsed minute and never a sum over the ring. A gap longer than the
// window clears the ring in one step.
//
// A calendar window needs no ring: one accumulator and a period start, reset
// when the boundary is crossed.
//
// counter is not safe for concurrent use; its owner holds the lock.
type counter struct {
	w Window

	// rolling state
	buckets []int64
	head    int   // index of the bucket for headMin
	headMin int64 // minute index of the newest bucket
	total   int64

	// calendar state
	periodStart time.Time
	value       int64
}

// newCounter builds a counter for a window.
func newCounter(w Window, now time.Time) (*counter, error) {
	if !w.Valid() {
		return nil, fmt.Errorf("quota: invalid window %q", w)
	}
	c := &counter{w: w}
	if w.Kind() == KindRolling {
		n := w.buckets()
		if n > maxBuckets {
			return nil, fmt.Errorf(
				"quota: rolling window %s needs %d minute buckets (max %d); use a calendar window",
				w, n, maxBuckets)
		}
		c.buckets = make([]int64, n)
		c.headMin = minuteOf(now)
		return c, nil
	}
	c.periodStart = w.PeriodStart(now)
	return c, nil
}

// minuteOf is the absolute minute index of an instant.
func minuteOf(t time.Time) int64 { return t.Unix() / 60 }

// advance moves the counter's notion of now forward, discarding what has
// aged out.
func (c *counter) advance(now time.Time) {
	if c.w.Kind() != KindRolling {
		if start := c.w.PeriodStart(now); start.After(c.periodStart) {
			c.periodStart, c.value = start, 0
		}
		return
	}
	m := minuteOf(now)
	if m <= c.headMin {
		return // same minute, or a late record: it lands in the current bucket
	}
	n := int64(len(c.buckets))
	if m-c.headMin >= n {
		// The whole window has elapsed. Clearing beats subtracting n buckets.
		clear(c.buckets)
		c.total, c.head, c.headMin = 0, 0, m
		return
	}
	for i := c.headMin + 1; i <= m; i++ {
		c.head = (c.head + 1) % len(c.buckets)
		c.total -= c.buckets[c.head]
		c.buckets[c.head] = 0
	}
	c.headMin = m
}

// add records v units at now.
func (c *counter) add(now time.Time, v int64) {
	c.advance(now)
	if c.w.Kind() != KindRolling {
		c.value += v
		return
	}
	c.buckets[c.head] += v
	c.total += v
}

// sum returns the units inside the window at now.
func (c *counter) sum(now time.Time) int64 {
	c.advance(now)
	if c.w.Kind() != KindRolling {
		return c.value
	}
	return c.total
}

// resetAt returns the earliest instant at which the counter drops below limit.
//
// For a calendar window that is the period boundary. For a rolling window it is
// the minute the oldest buckets holding the excess age out, which is computed
// by walking from the oldest bucket forward — O(buckets), but only ever on the
// exhaustion path, never on the hot path.
func (c *counter) resetAt(now time.Time, limit int64) time.Time {
	c.advance(now)
	if c.w.Kind() != KindRolling {
		return c.w.PeriodEnd(now)
	}
	need := c.total - limit + 1 // shed this much to be strictly below the limit
	if need <= 0 {
		return now
	}
	n := len(c.buckets)
	var shed int64
	for i := 1; i <= n; i++ {
		idx := (c.head + i) % n // oldest first
		shed += c.buckets[idx]
		if shed >= need {
			// That bucket covers minute headMin-(n-i); it leaves the window
			// one minute after the window's length has passed over it.
			bucketMin := c.headMin - int64(n-i)
			return time.Unix((bucketMin+int64(n))*60, 0).UTC()
		}
	}
	return now.Add(c.w.Duration())
}

// reset clears the counter.
func (c *counter) reset(now time.Time) {
	if c.w.Kind() == KindRolling {
		clear(c.buckets)
		c.total, c.head, c.headMin = 0, 0, minuteOf(now)
		return
	}
	c.periodStart, c.value = c.w.PeriodStart(now), 0
}
