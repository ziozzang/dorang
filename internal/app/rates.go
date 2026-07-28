package app

import (
	"sync"
	"time"
)

// A key's rpm_limit, tpm_limit and max_parallel_requests are carried from the
// store into auth.Limits, compared inside auth.Principal.Authorize, and — until
// this file existed — compared against nothing.
//
// auth.Access has ObservedRPM and ObservedTPM fields whose doc comment says
// "internal/quota supplies the number". Nothing supplied it. Every production
// construction of auth.Access omitted both, so the comparison was always
// `0 >= limit`, which is false for every positive ceiling. A key with
// rpm_limit: 600 was unlimited; a key with rpm_limit: 0 refused everything.
// The auth package's own tests passed because they set the fields themselves —
// the harness supplying the value the system was responsible for producing, which
// DESIGN §17.1 names as the way this defect class hides.
//
// This is the missing producer. It lives here rather than in internal/auth
// because a rolling window is state, internal/auth is a pure decision over a
// snapshot, and this package is already where the request path meets the
// principal.

// rateWindow is one subject's rolling minute, as sixty one-second buckets.
//
// A ring of stamped buckets rather than a decaying counter: a decayed estimate
// cannot answer "how many in the last sixty seconds" exactly, and a rate limit
// that is approximately right is one an operator cannot reconcile against a
// provider's own 429s.
type rateWindow struct {
	sec [rateBuckets]int64 // the unix second each bucket holds
	req [rateBuckets]int64
	tok [rateBuckets]int64
	// last is when this window was touched, for eviction.
	last int64
}

const rateBuckets = 60

// add records usage at unix second now, rolling any bucket that has aged out.
func (w *rateWindow) add(now int64, req, tok int64) {
	i := int(now % rateBuckets)
	if w.sec[i] != now {
		w.sec[i], w.req[i], w.tok[i] = now, 0, 0
	}
	w.req[i] += req
	w.tok[i] += tok
	if now > w.last {
		w.last = now
	}
}

// sum totals the buckets still inside the window ending at now.
func (w *rateWindow) sum(now int64) (req, tok int64) {
	cutoff := now - rateBuckets + 1
	for i := 0; i < rateBuckets; i++ {
		if w.sec[i] >= cutoff && w.sec[i] <= now {
			req += w.req[i]
			tok += w.tok[i]
		}
	}
	return req, tok
}

// maxRateSubjects bounds how many keys the tracker remembers at once.
//
// It is a bound and not a tuning knob. The map is keyed by api key id, which is
// attacker-influenced in exactly one direction — anyone who can create keys can
// create many — and DESIGN §9.6's first rule is that no deferred structure may
// be unbounded. Past the cap the coldest entries are dropped, which loses a
// window rather than a limit: a dropped key's next request re-creates it at zero,
// so the failure mode is a brief under-count, never an unbounded map.
const maxRateSubjects = 100_000

// keyRates is the per-key rolling-minute observation the authorization gate
// reads.
//
// It is sharded by key id so the gate does not serialize on one mutex. Each
// shard holds its own map and evicts independently, which is what keeps the cap
// enforceable without a global sweep.
type keyRates struct {
	shards [rateShards]rateShard
	now    func() time.Time
}

const rateShards = 16

type rateShard struct {
	mu sync.Mutex
	m  map[string]*rateWindow
}

func newKeyRates(now func() time.Time) *keyRates {
	if now == nil {
		now = time.Now
	}
	r := &keyRates{now: now}
	for i := range r.shards {
		r.shards[i].m = make(map[string]*rateWindow)
	}
	return r
}

// shardFor picks a shard by a cheap FNV-1a over the id.
func (r *keyRates) shardFor(id string) *rateShard {
	var h uint32 = 2166136261
	for i := 0; i < len(id); i++ {
		h ^= uint32(id[i])
		h *= 16777619
	}
	return &r.shards[h%rateShards]
}

// observe records one admitted request and returns what the window held BEFORE
// it, which is the number the ceiling is compared against.
//
// Counting at admission rather than at completion is deliberate. A ceiling
// enforced only on finished requests cannot refuse a burst: a thousand
// concurrent calls would all read a window that none of them had entered yet.
// The cost is that a request refused further down the stack still counted, which
// errs toward the limit rather than past it.
func (r *keyRates) observe(id string) (req, tok int64) {
	if r == nil || id == "" {
		return 0, 0
	}
	now := r.now().Unix()
	sh := r.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	w := sh.m[id]
	if w == nil {
		if len(sh.m) >= maxRateSubjects/rateShards {
			sh.evictLocked(now)
		}
		w = &rateWindow{}
		sh.m[id] = w
	}
	req, tok = w.sum(now)
	w.add(now, 1, 0)
	return req, tok
}

// record adds a finished request's tokens. The request itself was already
// counted by observe, so only the token half lands here.
func (r *keyRates) record(id string, tokens int64) {
	if r == nil || id == "" || tokens <= 0 {
		return
	}
	now := r.now().Unix()
	sh := r.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if w := sh.m[id]; w != nil {
		w.add(now, 0, tokens)
	}
}

// evictLocked drops every window that cannot contribute to any live sum, and
// then, if that was not enough, the coldest remaining ones.
func (sh *rateShard) evictLocked(now int64) {
	for id, w := range sh.m {
		if w.last < now-rateBuckets {
			delete(sh.m, id)
		}
	}
	// Still full: the shard is holding that many actively-rating keys, so drop
	// the coldest half rather than growing. Approximate is fine — this is a
	// bound, and a dropped window costs one under-counted minute.
	limit := maxRateSubjects / rateShards
	if len(sh.m) < limit {
		return
	}
	oldest := now
	for _, w := range sh.m {
		if w.last < oldest {
			oldest = w.last
		}
	}
	midpoint := (oldest + now) / 2
	for id, w := range sh.m {
		if w.last <= midpoint {
			delete(sh.m, id)
		}
	}
}
