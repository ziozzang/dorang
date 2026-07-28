package prefix

import (
	"cmp"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// entryBytes is the measured retained size of one table entry: the map key
// (16), the value struct (16), and the amortized overhead of a Go map bucket
// and its eviction-list node. Revision 1 of the design budgeted 48 bytes per
// entry from an optimistic hand count and was wrong by 2-3x, so the table is
// bounded by BYTES rather than by entry count and this number is asserted by a
// test against real retained size.
const entryBytes = 112

type entry struct {
	target   uint32 // interned deployment id — never a string, see Interner
	depth    uint8
	lastSeen int64 // unix nanos, updated on hit
	hits     uint32
}

const shardCount = 64

type shard struct {
	mu      sync.RWMutex
	entries map[Digest]entry
	// victims is the eviction scratch, reused across sweeps so that dropping
	// the coldest entries of a shard allocates nothing after the first time.
	// Eviction is approximate by design — entries are tiny, uniform, and cheap
	// to recreate, since a miss costs one routing decision.
	victims []victim
}

// victim is one candidate for eviction: a key and the instant it was last used.
type victim struct {
	d    Digest
	last int64
}

// Table maps prefix digests to the deployment that served them.
//
// It is a hint, never a source of truth: a miss costs one suboptimal routing
// decision and nothing else, so the table trades exactness for staying small
// and lock-cheap.
type Table struct {
	shards   [shardCount]shard
	maxBytes int64
	curBytes atomic.Int64
	ttl      time.Duration
	now      func() time.Time

	// perTarget is the per-deployment lifetime override, indexed by interned
	// target id. It is swapped by pointer rather than locked: the read path
	// touches it once per candidate entry, and a hot reload replaces the whole
	// slice. A nil pointer, or an id past the end, means the table default.
	perTarget atomic.Pointer[[]time.Duration]

	lookups atomic.Uint64
	hits    atomic.Uint64
	evicted atomic.Uint64
}

// neverExpires is the stored form of "this entry has no clock". It is a
// sentinel rather than a zero because zero already means "use the default"
// everywhere else in this package.
const neverExpires = time.Duration(-1)

// Options configures a Table.
type Options struct {
	// MaxBytes caps retained size. Zero means 64 MiB.
	MaxBytes int64
	// TTL expires an entry. Zero means one hour; NEGATIVE means entries never
	// expire on a clock and the byte budget alone bounds the table.
	//
	// Expiry is measured from LAST USE here, unlike session stickiness, which
	// expires from creation. The difference is deliberate: a session pin is a
	// promise about a cache that has certainly gone cold after the TTL, whereas
	// a prefix that keeps being requested keeps the upstream cache warm, so
	// refreshing on use tracks reality rather than defeating it.
	//
	// This is the table-wide default. [Table.SetTargetTTLs] overrides it per
	// deployment, which is what the setting actually models: how long the
	// BACKEND still holds those KV blocks, which differs between a hosted
	// service with a five-minute window and a self-hosted engine that evicts by
	// memory pressure and never by time.
	TTL time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// NewTable builds a Table.
func NewTable(opts Options) *Table {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 64 << 20
	}
	switch {
	case opts.TTL == 0:
		opts.TTL = time.Hour
	case opts.TTL < 0:
		opts.TTL = neverExpires
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	t := &Table{maxBytes: opts.MaxBytes, ttl: opts.TTL, now: opts.Now}
	for i := range t.shards {
		t.shards[i].entries = make(map[Digest]entry)
	}
	return t
}

func (t *Table) shardFor(d Digest) *shard {
	// The digest is already uniformly distributed, so any byte works.
	return &t.shards[d[0]%shardCount]
}

// SetTargetTTLs installs the per-deployment lifetimes, indexed by interned
// target id. A zero entry, or an id past the end of the slice, uses the table
// default; a negative entry never expires on a clock.
//
// It replaces the whole table rather than merging, so a hot reload cannot leave
// a lifetime behind for a deployment the new configuration removed. The slice is
// not copied and must not be mutated afterwards.
func (t *Table) SetTargetTTLs(ttls []time.Duration) {
	if len(ttls) == 0 {
		t.perTarget.Store(nil)
		return
	}
	t.perTarget.Store(&ttls)
}

// ttlFor is this target's lifetime, or the table default.
func (t *Table) ttlFor(target uint32) time.Duration {
	if p := t.perTarget.Load(); p != nil {
		if s := *p; int(target) < len(s) && s[target] != 0 {
			return s[target]
		}
	}
	return t.ttl
}

// liveAt reports whether an entry last used at lastSeen is still believable at
// now. An entry whose target never expires on a clock always is: the byte
// budget is what bounds it, and evict's second pass drops the coldest such
// entries exactly as it drops any other.
func (t *Table) liveAt(e entry, now int64) bool {
	ttl := t.ttlFor(e.target)
	if ttl < 0 {
		return true
	}
	return e.lastSeen >= now-int64(ttl)
}

// Lookup returns the deployment for the deepest matching prefix, searching
// deepest first so the longest common prefix wins. valid reports whether the
// candidate is still acceptable; callers pass a predicate because a target that
// has gone unhealthy or exhausted must not be resurrected by cache affinity.
func (t *Table) Lookup(digests []Digest, valid func(target uint32) bool) (target uint32, depth int, ok bool) {
	t.lookups.Add(1)
	now := t.now().UnixNano()
	for i := len(digests) - 1; i >= 0; i-- {
		d := digests[i]
		sh := t.shardFor(d)
		sh.mu.RLock()
		e, found := sh.entries[d]
		sh.mu.RUnlock()
		// The lifetime is the TARGET's, not the table's: a five-minute hosted
		// cache and a self-hosted engine that evicts by memory pressure can be
		// candidates for the same request, and one clock cannot be right for
		// both.
		if !found || !t.liveAt(e, now) {
			continue
		}
		if valid != nil && !valid(e.target) {
			continue
		}
		sh.mu.Lock()
		if cur, still := sh.entries[d]; still {
			cur.lastSeen = t.now().UnixNano()
			cur.hits++
			sh.entries[d] = cur
		}
		sh.mu.Unlock()
		t.hits.Add(1)
		return e.target, int(e.depth), true
	}
	return 0, 0, false
}

// Record binds every depth of this request's chain to the deployment that
// served it. Shallow depths matter as much as deep ones: they are what lets a
// later request that only partially matches still find a warm backend.
func (t *Table) Record(digests []Digest, target uint32) {
	now := t.now().UnixNano()
	for i, d := range digests {
		sh := t.shardFor(d)
		sh.mu.Lock()
		if _, exists := sh.entries[d]; !exists {
			t.curBytes.Add(entryBytes)
		}
		sh.entries[d] = entry{target: target, depth: uint8(i + 1), lastSeen: now}
		sh.mu.Unlock()
	}
	if t.curBytes.Load() > t.maxBytes {
		t.evict()
	}
}

// evict drops the coldest entries until the table is back under budget. It is
// approximate by design — running per shard avoids a global lock, and a
// slightly stale eviction order costs at most a few extra cache misses.
//
// It reaches the target in ONE call. That is not an optimization, it is the
// contract: [Table.Record] runs it whenever the table is over budget, so a pass
// that frees less than the deficit is re-run by the very next Record, under the
// shard lock that Lookup waits on, on the routing hot path. The earlier version
// freed one entry per shard — 64 entries — whatever the deficit was, so a table
// 5,000 entries over budget ran the whole scan 78 times per insert until it
// caught up, and never did while traffic continued.
func (t *Table) evict() {
	target := t.maxBytes - t.maxBytes/8 // reclaim to 87.5% so this is not per-insert
	now := t.now().UnixNano()

	// Pass one: expired entries only. Usually enough, and it never evicts
	// anything still useful. An entry whose target never expires on a clock is
	// never in this pass — pass two is what bounds it, which is exactly the
	// "until evicted" contract a self-hosted engine's prefix cache has.
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for d, e := range sh.entries {
			if !t.liveAt(e, now) {
				delete(sh.entries, d)
				t.curBytes.Add(-entryBytes)
				t.evicted.Add(1)
			}
		}
		sh.mu.Unlock()
		if t.curBytes.Load() <= target {
			return
		}
	}

	// Pass two: still over budget, so drop the coldest by last use. The number
	// of entries the deficit calls for is computed once and taken from the
	// shards in equal shares; a shard that cannot meet its share leaves the
	// remainder to the ones after it, and a final top-up sweep covers the case
	// where those ran out too. Both loops are bounded by shardCount, so the
	// cost of a sweep is bounded whatever the deficit is.
	need := int((t.curBytes.Load() - target + entryBytes - 1) / entryBytes)
	for i := range t.shards {
		if need <= 0 {
			return
		}
		share := (need + shardCount - i - 1) / (shardCount - i)
		need -= t.evictColdest(&t.shards[i], share)
	}
	for i := range t.shards {
		if need <= 0 {
			return
		}
		need -= t.evictColdest(&t.shards[i], need)
	}
}

// evictColdest drops the k least recently used entries of one shard and reports
// how many it dropped, which is fewer than k only when the shard held fewer.
func (t *Table) evictColdest(sh *shard, k int) int {
	if k <= 0 {
		return 0
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()

	n := len(sh.entries)
	if n == 0 {
		return 0
	}
	if k >= n {
		clear(sh.entries)
		t.curBytes.Add(-int64(n) * entryBytes)
		t.evicted.Add(uint64(n))
		return n
	}

	vs := sh.victims[:0]
	for d, e := range sh.entries {
		vs = append(vs, victim{d: d, last: e.lastSeen})
	}
	sh.victims = vs
	slices.SortFunc(vs, func(a, b victim) int { return cmp.Compare(a.last, b.last) })
	for _, v := range vs[:k] {
		delete(sh.entries, v.d)
	}
	t.curBytes.Add(-int64(k) * entryBytes)
	t.evicted.Add(uint64(k))
	return k
}

// Stats reports observability counters.
func (t *Table) Stats() (lookups, hits, evicted uint64, bytes int64) {
	return t.lookups.Load(), t.hits.Load(), t.evicted.Load(), t.curBytes.Load()
}

// Len reports the entry count across all shards. Test and metrics use only.
func (t *Table) Len() int {
	n := 0
	for i := range t.shards {
		t.shards[i].mu.RLock()
		n += len(t.shards[i].entries)
		t.shards[i].mu.RUnlock()
	}
	return n
}

// Interner maps deployment identifiers to small integers.
//
// The table stores an id rather than a string because a string header alone is
// 16 bytes before its backing array, and the same handful of deployment names
// repeat across every entry. Interning turns that into 4 bytes and one shared
// copy.
type Interner struct {
	mu   sync.RWMutex
	ids  map[string]uint32
	back []string
}

// NewInterner builds an empty Interner.
func NewInterner() *Interner {
	return &Interner{ids: make(map[string]uint32)}
}

// ID returns a stable id for name, assigning one on first sight.
func (in *Interner) ID(name string) uint32 {
	in.mu.RLock()
	id, ok := in.ids[name]
	in.mu.RUnlock()
	if ok {
		return id
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if id, ok := in.ids[name]; ok {
		return id
	}
	id = uint32(len(in.back))
	in.back = append(in.back, name)
	in.ids[name] = id
	return id
}

// Name resolves an id back to its string.
func (in *Interner) Name(id uint32) (string, bool) {
	in.mu.RLock()
	defer in.mu.RUnlock()
	if int(id) >= len(in.back) {
		return "", false
	}
	return in.back[id], true
}
