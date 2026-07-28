package app

import (
	"sync"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
)

// A key's rpm_limit, tpm_limit and max_parallel_requests are carried from the
// store into auth.Limits, compared inside auth.Principal.Authorize, and — until
// this file existed — compared against nothing.
//
// auth.Access has ObservedRPM and ObservedTPM fields whose doc comment said
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
//
// # Why the window is per SUBJECT and not per key
//
// The first version of this counter was keyed by api key id alone, and
// [auth.Access] carried one observed pair for all three subjects. A team's
// rpm_limit was therefore compared against ONE KEY's count: with ten keys under
// a team whose ceiling is four, forty requests passed and none was refused,
// while ten requests on a single key refused six. That is the same
// N-multiplication as the per-key budget ceiling one file over, in the rate
// dimension. A team limit means "across the team", so the counter has to be able
// to answer for a team — which is why the windows are keyed by kind+":"+id and
// reached through [auth.RateSource].

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

// maxRateSubjects bounds how many subjects the tracker remembers at once.
//
// It is a bound and not a tuning knob. The map is keyed by api key, user or team
// id, which is attacker-influenced in exactly one direction — anyone who can
// create keys can create many — and DESIGN §9.6's first rule is that no deferred
// structure may be unbounded. Past the cap the coldest entries are dropped, which
// loses a window rather than a limit: a dropped subject's next request re-creates
// it at zero, so the failure mode is a brief under-count, never an unbounded map.
const maxRateSubjects = 100_000

// rateSubjects is how many subjects one principal has: key, user, team.
const rateSubjects = 3

// keyRates is the per-subject rolling-minute observation the authorization gate
// reads.
//
// It is sharded by subject so the gate does not serialize on one mutex. Each
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

// shardFor picks a shard by a cheap FNV-1a over the subject key.
func (r *keyRates) shardFor(k string) *rateShard {
	var h uint32 = 2166136261
	for i := 0; i < len(k); i++ {
		h ^= uint32(k[i])
		h *= 16777619
	}
	return &r.shards[h%rateShards]
}

// observation is what one subject's window held when the request arrived.
type observation struct {
	kind string
	id   string
	rpm  int64
	tpm  int64
}

// observed is the per-request answer [auth.RateSource] is served from.
//
// Three fixed slots rather than a map: a principal has exactly three subjects,
// and this is read inside the authorization gate on every request.
type observed [rateSubjects]observation

// ObservedRates implements [auth.RateSource].
//
// It answers from the snapshot taken when the request was admitted rather than
// re-reading the live window, so every subject's ceiling is judged against one
// instant. A subject this request has no id for reads as zero, which skips the
// check exactly as an unconfigured ceiling does.
func (o *observed) ObservedRates(kind, id string) (rpm, tpm int64) {
	if o == nil || id == "" {
		return 0, 0
	}
	for i := range o {
		if o[i].kind == kind && o[i].id == id {
			return o[i].rpm, o[i].tpm
		}
	}
	return 0, 0
}

// subjectsOf names the rate subjects of a principal, skipping the empty ones.
//
// A subject is named by its ID and not by whether it carries a limits envelope.
// A team's counter has to include a key that declares no team ceiling of its own,
// or the team's ceiling is per-key again the moment one key under it is
// unrestricted.
func subjectsOf(p *auth.Principal) (out [rateSubjects]observation, n int) {
	if p == nil || p.Master {
		return out, 0
	}
	for _, s := range [rateSubjects]observation{
		{kind: "key", id: p.KeyID},
		{kind: "user", id: p.UserID},
		{kind: "team", id: p.TeamID},
	} {
		if s.id == "" {
			continue
		}
		out[n] = s
		n++
	}
	return out, n
}

// observe records one admitted request against every subject of p and returns
// what each subject's window held BEFORE it — the numbers the ceilings are
// compared against.
//
// Counting at admission rather than at completion is deliberate. A ceiling
// enforced only on finished requests cannot refuse a burst: a thousand
// concurrent calls would all read a window that none of them had entered yet.
// The read and the increment happen together under the subject's shard lock, so
// two concurrent requests cannot both observe the same pre-count. The cost is
// that a request refused further down the stack still counted, which errs toward
// the limit rather than past it.
func (r *keyRates) observe(p *auth.Principal) observed {
	var out observed
	subs, n := subjectsOf(p)
	if r == nil || n == 0 {
		return out
	}
	now := r.now().Unix()
	for i := 0; i < n; i++ {
		s := subs[i]
		s.rpm, s.tpm = r.bump(s.kind+":"+s.id, now, 1, 0)
		out[i] = s
	}
	return out
}

// recordTokens adds a finished request's tokens to every named subject. The
// request itself was already counted by observe, so only the token half lands
// here — the token count does not exist until the request settles.
func (r *keyRates) recordTokens(keyID, userID, teamID string, tokens int64) {
	if r == nil || tokens <= 0 {
		return
	}
	now := r.now().Unix()
	for _, s := range [rateSubjects]observation{
		{kind: "key", id: keyID},
		{kind: "user", id: userID},
		{kind: "team", id: teamID},
	} {
		if s.id == "" {
			continue
		}
		r.bump(s.kind+":"+s.id, now, 0, tokens)
	}
}

// bump adds to one subject's window and returns what it held beforehand.
func (r *keyRates) bump(k string, now, req, tok int64) (rpm, tpm int64) {
	sh := r.shardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	w := sh.m[k]
	if w == nil {
		if len(sh.m) >= maxRateSubjects/rateShards {
			sh.evictLocked(now)
		}
		w = &rateWindow{}
		sh.m[k] = w
	}
	rpm, tpm = w.sum(now)
	w.add(now, req, tok)
	return rpm, tpm
}

// evictLocked drops every window that cannot contribute to any live sum, and
// then, if that was not enough, the coldest remaining ones.
func (sh *rateShard) evictLocked(now int64) {
	for id, w := range sh.m {
		if w.last < now-rateBuckets {
			delete(sh.m, id)
		}
	}
	// Still full: the shard is holding that many actively-rating subjects, so
	// drop the coldest half rather than growing. Approximate is fine — this is a
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
