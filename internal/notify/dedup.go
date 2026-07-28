package notify

import (
	"sync"
	"sync/atomic"
	"time"
)

// The deduper.
//
// It is on the request path — budget_80pct is evaluated per request, which is
// exactly why it needs deduplicating — so it has to be cheap and allocate
// nothing. It is a sharded map from (event, subject) to the period bucket that
// subject was last alerted in. Admission is one hash, one lock of a 1/64th
// shard, and one map operation.
//
// A map that only grows is a leak with a schedule (§9.6 rule 1), so each shard
// has an entry cap. Past it, entries older than one period are swept; if the
// sweep does not free enough, entries are evicted and the eviction is counted.
// An evicted subject can alert twice in a period, which is the right way round:
// losing an alert is worse than repeating one.

const (
	dedupShards      = 64
	dedupShardEntrie = 2048
)

type dedupKey struct {
	ev   Event
	kind string
	id   string
}

type dedupShard struct {
	mu sync.Mutex
	m  map[dedupKey]int64
}

type deduper struct {
	shards     [dedupShards]dedupShard
	periods    [numEvents]time.Duration
	cap        int
	evictions  atomic.Uint64
	suppressed atomic.Uint64
}

func newDeduper(periods [numEvents]time.Duration) *deduper {
	d := &deduper{periods: periods, cap: dedupShardEntrie}
	for i := range d.shards {
		d.shards[i].m = make(map[dedupKey]int64)
	}
	return d
}

// admit reserves the alert slot for (ev, subj) in the period containing at.
//
// It returns true for the first caller in a period and false for every one
// after it. A zero period disables deduplication for that event, which is the
// right answer for an event that is already unique per subject — key_created
// happens once per key by construction, and suppressing it would suppress it
// forever.
func (d *deduper) admit(ev Event, subj Subject, at time.Time) bool {
	period := d.periods[ev]
	if period <= 0 {
		return true
	}
	bucket := at.UnixNano() / int64(period)
	k := dedupKey{ev: ev, kind: subj.Kind, id: subj.ID}
	sh := &d.shards[shardIndex(k)]

	sh.mu.Lock()
	if last, ok := sh.m[k]; ok && last == bucket {
		sh.mu.Unlock()
		d.suppressed.Add(1)
		return false
	}
	sh.m[k] = bucket
	if len(sh.m) > d.cap {
		d.evictLocked(sh, bucket)
	}
	sh.mu.Unlock()
	return true
}

// evictLocked frees room in a full shard. It sweeps stale buckets first,
// because a subject that has not alerted in a period does not need remembering.
func (d *deduper) evictLocked(sh *dedupShard, bucket int64) {
	for k, b := range sh.m {
		if b < bucket {
			delete(sh.m, k)
		}
	}
	for k := range sh.m {
		if len(sh.m) <= d.cap {
			break
		}
		delete(sh.m, k)
		d.evictions.Add(1)
	}
}

// shardIndex hashes the key with FNV-1a, written out rather than taken from
// hash/fnv so that hashing a key allocates nothing.
func shardIndex(k dedupKey) uint64 {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	h ^= uint64(k.ev)
	h *= prime
	for i := 0; i < len(k.kind); i++ {
		h ^= uint64(k.kind[i])
		h *= prime
	}
	h ^= '\x00'
	h *= prime
	for i := 0; i < len(k.id); i++ {
		h ^= uint64(k.id[i])
		h *= prime
	}
	return h % dedupShards
}

// defaultPeriods are the per-event deduplication windows.
//
// The three that repeat per request or per probe get a window; the three that
// are already unique per subject get none. credential_unhealthy sits in
// between: a flapping credential re-opens its circuit repeatedly, and an
// operator wants to know it is flapping, not to receive one message per flap.
func defaultPeriods(base time.Duration) [numEvents]time.Duration {
	if base <= 0 {
		base = time.Hour
	}
	var p [numEvents]time.Duration
	p[EventKeyCreated] = 0
	p[EventBudget80] = base
	p[EventBudgetExceeded] = base
	p[EventQuotaExhausted] = base / 4
	p[EventCredentialUnhealthy] = base / 4
	p[EventBatchCompleted] = 0
	p[EventInvite] = 0
	return p
}
