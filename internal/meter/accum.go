package meter

import (
	"slices"
	"sync"
	"time"
	"unsafe"
)

// bucketKey is the accumulator's internal key: the exported fixed-cardinality
// Key plus the UTC hour the event belongs to. Carrying the hour in the key
// rather than deriving it at flush time is what makes a flush window that
// straddles an hour boundary attribute exactly. It at most doubles live
// cardinality, and only for one flush interval around the boundary.
type bucketKey struct {
	Key
	hour int64 // unix seconds at the start of the UTC hour
}

// counters is one key's accumulated numbers. It lives in a shard-owned arena,
// never on the heap per-key, so admitting a new key allocates nothing.
type counters struct {
	requests int64
	errors   int64
	tokens   Tokens
	costNano int64
	// marginalNano and subscriptionNano decompose costNano by pricing class
	// (DESIGN §8.1). The rollup carries the split for the same reason the ledger
	// row does: /global/spend/report answers `marginal_spend` from it, and
	// answering it from the TOTAL reports a flat plan's share as marginal usage.
	marginalNano     int64
	subscriptionNano int64
	latencySum       int64
	ttftSum          int64
	ttftCount        int64

	// idle counts consecutive flushes in which this slot saw no event. A slot
	// is returned to the free list only after EvictAfterFlushes idle flushes,
	// so the steady-state working set stays resident and Record never has to
	// grow the map.
	idle uint8
}

func (c *counters) reset() {
	*c = counters{}
}

func (c *counters) isZero() bool {
	return c.requests == 0 && c.errors == 0 && c.tokens.IsZero() &&
		c.costNano == 0 && c.marginalNano == 0 && c.subscriptionNano == 0 &&
		c.latencySum == 0 && c.ttftSum == 0 && c.ttftCount == 0
}

func (c *counters) add(ev *Event) {
	c.requests++
	if ev.isError() {
		c.errors++
	}
	c.tokens.add(ev.Tokens)
	c.costNano += ev.CostNano
	c.marginalNano += ev.MarginalCostNano
	c.subscriptionNano += ev.SubscriptionCostNano
	c.latencySum += int64(ev.Latency)
	if ev.TTFT > 0 {
		c.ttftSum += int64(ev.TTFT)
		c.ttftCount++
	}
	c.idle = 0
}

const cacheLine = 64

// shardData is a shard's state. It is separated from shard only so that the
// padding below can be computed from its size at compile time.
type shardData struct {
	mu sync.Mutex
	// m maps key to an index into arena. The value is an int32 rather than a
	// *counters so the map holds no pointers at all: nothing for the GC to
	// scan on a structure the hot path writes to constantly.
	m     map[bucketKey]int32
	arena []counters
	free  []int32
	folds int64
}

// shard is shardData padded out to whole cache lines. Shards are chosen by a
// per-goroutine hint, so neighbouring shards are routinely written by
// different cores; without the padding they would false-share.
type shard struct {
	shardData
	_ [cacheLine - unsafe.Sizeof(shardData{})%cacheLine]byte
}

// overflowSlots is the headroom above MaxKeysPerShard reserved for the fold-to
// key. The fold-to key is admitted past the cap -- otherwise the fold would
// have nowhere to go -- but there is at most one per live hour, so a handful
// of slots bounds it.
const overflowSlots = 8

// initShard prepares s in place. Everything a shard can ever need is
// allocated here, so admitting a key on the hot path allocates nothing.
func initShard(s *shard, maxKeys int) {
	n := maxKeys + overflowSlots
	s.m = make(map[bucketKey]int32, n)
	s.arena = make([]counters, n)
	s.free = make([]int32, n)
	for i := range s.free {
		// Reversed so that the first Pop returns slot 0, which keeps arena
		// use dense and predictable in tests.
		s.free[i] = int32(n - 1 - i)
	}
}

// slotFor returns the counters for k, admitting a new key if there is room and
// folding into the overflow key if there is not. It must be called with s.mu
// held and it allocates nothing.
func (s *shard) slotFor(k bucketKey, maxKeys int) *counters {
	if idx, ok := s.m[k]; ok {
		return &s.arena[idx]
	}
	if len(s.m) >= maxKeys && !k.IsOverflow() {
		s.folds++
		return s.slotFor(bucketKey{Key: overflowKey(), hour: k.hour}, maxKeys)
	}
	if len(s.free) == 0 {
		// Only reachable when the overflow headroom itself is exhausted, which
		// needs more distinct live hours in one flush interval than
		// overflowSlots -- backfilled or clock-skewed events, not traffic.
		// Fold rather than allocate: losing resolution is acceptable here,
		// losing the count is not. Prefer an existing overflow slot, so the
		// count lands somewhere already marked as lossy instead of inflating
		// an unrelated real key.
		s.folds++
		var any int32 = -1
		for ek, idx := range s.m {
			if ek.IsOverflow() {
				return &s.arena[idx]
			}
			any = idx
		}
		if any >= 0 {
			return &s.arena[any]
		}
		return nil
	}
	idx := s.free[len(s.free)-1]
	s.free = s.free[:len(s.free)-1]
	c := &s.arena[idx]
	c.reset()
	s.m[k] = idx
	return c
}

// drainInto merges this shard's counters into dst, zeroing what it took, and
// evicts slots idle for longer than evictAfter. It returns the number of folds
// accumulated since the last drain.
func (s *shard) drainInto(dst map[bucketKey]int, out *[]Bucket, evictAfter uint8) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	for k, idx := range s.m {
		c := &s.arena[idx]
		if c.isZero() {
			c.idle++
			if c.idle > evictAfter {
				delete(s.m, k)
				s.free = append(s.free, idx)
			}
			continue
		}
		i, ok := dst[k]
		if !ok {
			i = len(*out)
			*out = append(*out, Bucket{Key: k.Key, HourStart: time.Unix(k.hour, 0).UTC()})
			dst[k] = i
		}
		(*out)[i].addCounters(c)
		c.reset()
	}
	folds := s.folds
	s.folds = 0
	return folds
}

// mergeShards merges every shard into a fresh, sorted []Bucket, appending to
// seed (the carry-over from a refused write). scratch is a reusable index map;
// it is cleared, not reallocated.
func mergeShards(shards []shard, scratch map[bucketKey]int, seed []Bucket, evictAfter uint8) ([]Bucket, int64) {
	clear(scratch)
	out := seed
	for i := range out {
		b := &out[i]
		scratch[bucketKey{Key: b.Key, hour: b.HourStart.Unix()}] = i
	}
	var folds int64
	for i := range shards {
		folds += shards[i].drainInto(scratch, &out, evictAfter)
	}
	slices.SortFunc(out, func(a, b Bucket) int {
		if a.HourStart.Before(b.HourStart) {
			return -1
		}
		if a.HourStart.After(b.HourStart) {
			return 1
		}
		return CompareKey(a.Key, b.Key)
	})
	return out, folds
}

// foldPending caps the carry-over held when the sink refuses rollups. Past the
// cap the tail folds into one overflow bucket per hour. The alternative --
// growing without bound while a store is down -- turns a reporting outage into
// an OOM, and dropping the tail outright would make the numeric path lossy,
// which is the one thing it must never be.
func foldPending(bs []Bucket, max int) ([]Bucket, int64) {
	if len(bs) <= max {
		return bs, 0
	}
	keep := max - overflowSlots
	if keep < 1 {
		keep = 1
	}
	// bs is sorted by (hour, key); folding the tail keeps the oldest hours
	// intact, which is what a backfill wants.
	head := bs[:keep]
	tail := bs[keep:]

	byHour := make(map[int64]*Bucket, 4)
	var folds int64
	for i := range tail {
		folds++
		h := tail[i].HourStart.Unix()
		ob, ok := byHour[h]
		if !ok {
			ob = &Bucket{Key: overflowKey(), HourStart: tail[i].HourStart}
			byHour[h] = ob
		}
		ob.addBucket(&tail[i])
	}
	out := head
	for _, ob := range byHour {
		out = append(out, *ob)
	}
	slices.SortFunc(out, func(a, b Bucket) int {
		if a.HourStart.Before(b.HourStart) {
			return -1
		}
		if a.HourStart.After(b.HourStart) {
			return 1
		}
		return CompareKey(a.Key, b.Key)
	})
	return out, folds
}
