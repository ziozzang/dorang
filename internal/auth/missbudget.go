package auth

import (
	"sync"
	"time"
)

// The rate discipline on the one lookup an unauthenticated caller can force.
//
// DESIGN §11.2c gave refusals their own short TTL, which bounds how long a
// refusal is REMEMBERED. It does not bound how many refusals can be MANUFACTURED:
// a negative entry is keyed by the index key, so a caller presenting distinct
// random keys never hits one. Every distinct unknown key was therefore one
// [Store.LoadByLookup] — a database round trip bought with a credential the
// caller invented, against the one resource every tenant shares.
//
// So the cache is only half the answer and always was. The other half is a
// budget on the lookups themselves, and its shape follows from which lookups are
// actually the attacker's:
//
//   - A lookup that FINDS a row costs nothing. It is a real credential being used
//     for the first time on this node — a newly provisioned key, or any key at
//     all on a node with no bulk snapshot — and throttling those would turn a
//     cold start into an outage. The token is taken before the call and returned
//     the moment the row comes back, so a deployment whose misses all resolve
//     never depletes the bucket however many of them there are.
//   - A lookup that finds NOTHING keeps the token. That is the half a caller with
//     no credential controls, and it is the half that is bounded.
//   - A lookup the store could not answer keeps the token too. A store that is
//     erroring is a store that needs shielding at least as much as a healthy one,
//     and refunding there would restore the unbounded path exactly when it hurts
//     most.
//
// What this deliberately does NOT do is distinguish an attacker's first miss from
// a legitimate one, because at that point in the request there is nothing to
// distinguish them BY: an unknown index key carries no identity. So when the
// bucket is empty the refusal is [ReasonUnavailable] — 503, retryable — and not
// [ReasonUnknownKey]. The gateway did not consult the store, so it does not know
// that the key is unknown, and answering 401 would be both a lie and a cached-
// looking permanent refusal for a key that may have been created a second ago.
type missBudget struct {
	mu     sync.Mutex
	tokens float64
	last   int64 // unix nanoseconds at the last refill
	rate   float64
	burst  float64
}

// newMissBudget builds the bucket. A non-positive rate returns nil, which every
// method treats as "no limit".
func newMissBudget(rate float64, burst int, now time.Time) *missBudget {
	if rate <= 0 || burst <= 0 {
		return nil
	}
	return &missBudget{
		tokens: float64(burst),
		last:   now.UnixNano(),
		rate:   rate,
		burst:  float64(burst),
	}
}

// take reserves one lookup, reporting whether the budget allowed it. A nil
// budget allows everything.
func (b *missBudget) take(now time.Time) bool {
	if b == nil {
		return true
	}
	ns := now.UnixNano()
	b.mu.Lock()
	defer b.mu.Unlock()
	if d := ns - b.last; d > 0 {
		b.tokens += (float64(d) / float64(time.Second)) * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	// The clock is only ever moved forward. A [Config.Now] that steps backwards
	// — a test fixture, a corrected wall clock — must not be able to hand out a
	// refill it has already handed out.
	if ns > b.last {
		b.last = ns
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// refund returns a token taken by a lookup that turned out to be a real
// credential. It never exceeds the burst.
func (b *missBudget) refund() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens++
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
}

// available reports the tokens currently in the bucket, for diagnostics and for
// the tests that assert the bound rather than its mechanism. A nil budget
// reports -1: "unlimited" is not a large number.
func (b *missBudget) available() float64 {
	if b == nil {
		return -1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens
}
