// Package health tracks, per deployment, whether it is worth sending the next
// request to and how well it has been performing.
//
// Two jobs that are usually conflated but behave differently:
//
//   - Availability. A deployment that starts failing must stop being selected,
//     quickly, and then be probed rather than abandoned. Without this a
//     round-robin keeps sending a dead backend its full share of traffic, which
//     users experience as a gateway bug rather than a backend outage. The
//     compatibility contract records cooldown as load-bearing for exactly this
//     reason, so it is not optional or advisory here.
//
//   - Performance. Cost-blind strategies want to know which deployment answers
//     fastest, and which produces tokens fastest — those are different
//     questions. A deployment can have a low time-to-first-token and a slow
//     generation rate, or the reverse, and picking on the wrong one is how a
//     "fastest" router ends up slower than round-robin on long outputs.
//
// Both are maintained with plain atomics on a fixed-size per-deployment record,
// because this is read on the routing hot path for every candidate of every
// request.
package health

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// State is a deployment's availability.
type State uint32

const (
	// Closed is the normal state: the deployment is selectable.
	Closed State = iota
	// Open means recent failures crossed the threshold. The deployment is not
	// selectable until the cooldown elapses.
	Open
	// HalfOpen means the cooldown elapsed and exactly one probe request is
	// allowed through to decide whether to close or re-open.
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Options tunes the tracker.
//
// The defaults match the behavior a comparable deployment runs with, because a
// reliability regression is indistinguishable from a protocol break to whoever
// is using the gateway.
type Options struct {
	// FailureThreshold is the number of consecutive failures that opens the
	// circuit. Default 3.
	FailureThreshold int
	// Cooldown is how long the circuit stays open. Default 5s.
	Cooldown time.Duration
	// HalfOpenProbes is how many requests may pass while half-open. Default 1.
	HalfOpenProbes int
	// EWMAAlpha weights the newest sample. Default 0.2 — roughly a 10-sample
	// memory, responsive enough to notice a degrading backend within a handful
	// of requests without thrashing on one slow outlier.
	EWMAAlpha float64
	// Now is injectable for tests.
	Now func() time.Time
}

func (o *Options) setDefaults() {
	if o.FailureThreshold <= 0 {
		o.FailureThreshold = 3
	}
	if o.Cooldown <= 0 {
		o.Cooldown = 5 * time.Second
	}
	if o.HalfOpenProbes <= 0 {
		o.HalfOpenProbes = 1
	}
	if o.EWMAAlpha <= 0 || o.EWMAAlpha > 1 {
		o.EWMAAlpha = 0.2
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Outcome is what happened to one request.
type Outcome struct {
	// Err is nil on success.
	Err error
	// Failure marks this outcome as counting against availability. A 429 or a
	// content-policy refusal is an error to the caller but says nothing about
	// whether the deployment is alive, so the router sets this deliberately
	// rather than inferring it from Err.
	Failure bool
	// TTFT is time to first byte of the response. Zero when not measured.
	TTFT time.Duration
	// Total is the whole request duration.
	Total time.Duration
	// OutputTokens is used with Total to derive a generation rate.
	OutputTokens int64
}

type record struct {
	state        atomic.Uint32
	consecFails  atomic.Int64
	openedAtNano atomic.Int64
	probesLeft   atomic.Int64
	// rlStrikes counts consecutive rate-limit (429) stand-downs that have not yet
	// been cleared by a genuine success, and drives the exponential backoff in
	// MarkRateLimited. It is separate from consecFails because a 429 is capacity,
	// not liveness: it must not open the shared circuit, but a deployment that
	// keeps 429ing should be stood down for longer each time rather than probed
	// every base interval forever.
	rlStrikes atomic.Int64
	// lastOKNano is when the deployment last served a genuine success (0 =
	// never). The router reads it to keep traffic on the account that is
	// currently working rather than drifting back to an earlier-listed sibling.
	lastOKNano atomic.Int64

	// EWMAs are stored as float64 bits so they can be updated without a lock.
	// Contention here is benign: a lost update costs one sample of accuracy in
	// a smoothed average, which is not worth a mutex on the hot path.
	ttftBits  atomic.Uint64
	totalBits atomic.Uint64
	tpsBits   atomic.Uint64

	requests atomic.Int64
	failures atomic.Int64
	opens    atomic.Int64
}

// Tracker holds health for a set of deployments.
type Tracker struct {
	opts Options

	mu      sync.RWMutex
	records map[string]*record
}

// New builds a Tracker.
func New(opts Options) *Tracker {
	opts.setDefaults()
	return &Tracker{opts: opts, records: make(map[string]*record)}
}

func (t *Tracker) rec(id string) *record {
	t.mu.RLock()
	r, ok := t.records[id]
	t.mu.RUnlock()
	if ok {
		return r
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if r, ok := t.records[id]; ok {
		return r
	}
	r = &record{}
	t.records[id] = r
	return r
}

// Allow reports whether a request may be sent to this deployment, and
// transitions Open to HalfOpen when the cooldown has elapsed.
//
// It consumes a probe slot when half-open, so a caller that receives true must
// eventually call Report — otherwise a half-open deployment stalls with its
// probe outstanding. The cooldown re-arms on the next Allow after it expires,
// so a lost probe delays recovery rather than preventing it.
func (t *Tracker) Allow(id string) bool {
	r := t.rec(id)
	switch State(r.state.Load()) {
	case Closed:
		return true
	case HalfOpen:
		return r.probesLeft.Add(-1) >= 0
	case Open:
		openedAt := r.openedAtNano.Load()
		if t.opts.Now().UnixNano()-openedAt < int64(t.opts.Cooldown) {
			return false
		}
		// Only one goroutine wins the transition; the rest see HalfOpen and
		// compete for the probe slots below.
		if r.state.CompareAndSwap(uint32(Open), uint32(HalfOpen)) {
			r.probesLeft.Store(int64(t.opts.HalfOpenProbes))
		}
		return r.probesLeft.Add(-1) >= 0
	}
	return true
}

// Report records the outcome of a request.
func (t *Tracker) Report(id string, o Outcome) {
	r := t.rec(id)
	r.requests.Add(1)

	if o.Failure {
		r.failures.Add(1)
		n := r.consecFails.Add(1)
		switch State(r.state.Load()) {
		case HalfOpen:
			// The probe failed, so re-open for a full cooldown rather than
			// letting a stream of probes hammer a backend that is still down.
			t.open(r)
		case Closed:
			if n >= int64(t.opts.FailureThreshold) {
				t.open(r)
			}
		}
		return
	}

	if o.Err != nil {
		// An error to the caller that is NOT a liveness verdict — a 429 or a
		// content-policy refusal, which the router reports with Failure=false. It
		// says nothing about whether the deployment is alive, so it must leave the
		// circuit exactly where it is: a half-open probe that 429s is STILL failing
		// (the router will stand it down, with escalating backoff, right after this
		// call), and closing it here would both hide that and reset the backoff. It
		// carries no latency sample worth smoothing either, so it touches nothing
		// but the request count already recorded above.
		return
	}

	// A genuine success (Err == nil). This is the only outcome that proves the
	// deployment recovered, so it is the only one that resets the failure and
	// rate-limit counters and closes a half-open circuit.
	r.consecFails.Store(0)
	r.rlStrikes.Store(0)
	r.lastOKNano.Store(t.opts.Now().UnixNano())
	if State(r.state.Load()) == HalfOpen {
		r.state.Store(uint32(Closed))
	}

	if o.TTFT > 0 {
		ewmaUpdate(&r.ttftBits, float64(o.TTFT), t.opts.EWMAAlpha)
	}
	if o.Total > 0 {
		ewmaUpdate(&r.totalBits, float64(o.Total), t.opts.EWMAAlpha)
		if o.OutputTokens > 0 {
			// Generation rate excludes time to first token: that is queueing
			// and prefill, not generation, and mixing them makes a backend with
			// a long queue look like a slow generator.
			gen := o.Total - o.TTFT
			if gen > 0 {
				tps := float64(o.OutputTokens) / gen.Seconds()
				ewmaUpdate(&r.tpsBits, tps, t.opts.EWMAAlpha)
			}
		}
	}
}

func (t *Tracker) open(r *record) {
	r.state.Store(uint32(Open))
	r.openedAtNano.Store(t.opts.Now().UnixNano())
	r.probesLeft.Store(0)
	r.opens.Add(1)
}

// MarkUnavailable opens the circuit for at least d, for reasons the tracker
// cannot observe — a provider-signalled retry-after, or an exhausted quota.
func (t *Tracker) MarkUnavailable(id string, d time.Duration) {
	r := t.rec(id)
	r.state.Store(uint32(Open))
	// Allow tests `now - openedAt >= cooldown`, so a wait longer than the
	// cooldown is encoded by pushing openedAt into the FUTURE by the excess.
	// Keeping one clock and one comparison on the hot path is worth this
	// indirection; getting its sign backwards is not, which is why the test
	// asserts a retry-after strictly longer than the cooldown.
	if excess := d - t.opts.Cooldown; excess > 0 {
		r.openedAtNano.Store(t.opts.Now().Add(excess).UnixNano())
	} else {
		r.openedAtNano.Store(t.opts.Now().UnixNano())
	}
	r.probesLeft.Store(0)
	r.opens.Add(1)
}

// MarkRateLimited stands a deployment down after a rate limit (a 429) that named
// no recovery time — the only signal a headerless provider such as ollama.com
// gives — with EXPONENTIAL BACKOFF across consecutive occurrences. The first is
// base; each further one that survives a recovery probe doubles, up to max. A
// genuine success (or MarkHealthy) resets the escalation. It returns the
// stand-down actually applied, or 0 when the deployment was already open.
//
// The escalation counts a fresh down-period, not a raw 429: only a Closed->Open
// (the first 429) or a HalfOpen->Open (a recovery probe that 429'd) increments a
// strike. A 429 that arrives while the deployment is already open is part of the
// SAME down-period — either a concurrent burst that all landed before the first
// stood it down, or a late in-flight reply — so it neither escalates nor pushes
// the recovery deadline farther away. This is why Report must NOT close a
// half-open circuit on a 429: if it did, the probe's 429 would look like a fresh
// first strike and the backoff would never grow.
//
// Standing one deployment down does not touch its siblings: health is keyed by
// deployment id, and each provider account is its own deployment. So this is how
// traffic "switches" to a healthy account and stays there while the exhausted one
// keeps refusing, without abandoning it — it is probed again once the (growing)
// window elapses, and restored the instant a probe succeeds.
func (t *Tracker) MarkRateLimited(id string, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = t.opts.Cooldown
	}
	if max > 0 && max < base {
		max = base
	}
	r := t.rec(id)
	for {
		s := State(r.state.Load())
		if s == Open {
			return 0
		}
		if r.state.CompareAndSwap(uint32(s), uint32(Open)) {
			n := r.rlStrikes.Add(1)
			if n > 32 {
				// Past the cap every stand-down is max anyway; clamp so a long
				// outage cannot grow the counter without bound.
				r.rlStrikes.Store(32)
				n = 32
			}
			d := backoffDuration(base, max, n)
			// Match MarkUnavailable's encoding: Allow compares against the fixed
			// opts.Cooldown, so a longer wait is expressed by pushing openedAt into
			// the future by the excess.
			if excess := d - t.opts.Cooldown; excess > 0 {
				r.openedAtNano.Store(t.opts.Now().Add(excess).UnixNano())
			} else {
				r.openedAtNano.Store(t.opts.Now().UnixNano())
			}
			r.probesLeft.Store(0)
			r.opens.Add(1)
			return d
		}
	}
}

// backoffDuration is base doubled once per strike after the first, capped at max.
// It doubles iteratively rather than shifting so a large strike count cannot
// overflow the duration before the cap is applied.
func backoffDuration(base, max time.Duration, n int64) time.Duration {
	if n < 1 {
		n = 1
	}
	d := base
	for i := int64(1); i < n; i++ {
		d *= 2
		if d <= 0 { // overflow
			return max
		}
		if max > 0 && d >= max {
			return max
		}
	}
	if max > 0 && d > max {
		return max
	}
	return d
}

// MarkHealthy forces the circuit closed, e.g. after an operator intervention or
// a successful out-of-band probe.
func (t *Tracker) MarkHealthy(id string) {
	r := t.rec(id)
	r.state.Store(uint32(Closed))
	r.consecFails.Store(0)
	r.rlStrikes.Store(0)
	r.probesLeft.Store(0)
}

// Stats is a point-in-time view of one deployment.
type Stats struct {
	State            State
	Requests         int64
	Failures         int64
	Opens            int64
	ConsecFails      int64
	RateLimitStrikes int64
	TTFT             time.Duration
	Total            time.Duration
	TokensPerSec     float64
}

// Stats reports a deployment's counters.
func (t *Tracker) Stats(id string) Stats {
	r := t.rec(id)
	return Stats{
		State:            State(r.state.Load()),
		Requests:         r.requests.Load(),
		Failures:         r.failures.Load(),
		Opens:            r.opens.Load(),
		ConsecFails:      r.consecFails.Load(),
		RateLimitStrikes: r.rlStrikes.Load(),
		TTFT:             time.Duration(ewmaRead(&r.ttftBits)),
		Total:            time.Duration(ewmaRead(&r.totalBits)),
		TokensPerSec:     ewmaRead(&r.tpsBits),
	}
}

// LastSuccess returns when the deployment last served a genuine success, in
// Unix nanoseconds, or 0 if it never has. A 429 or any other error leaves it
// unchanged.
func (t *Tracker) LastSuccess(id string) int64 {
	return t.rec(id).lastOKNano.Load()
}

// TTFT returns the smoothed time to first token. Zero means no sample yet;
// callers treat that as "no opinion" rather than "fastest", since an unproven
// deployment must not win a latency comparison on ignorance alone.
func (t *Tracker) TTFT(id string) time.Duration {
	return time.Duration(ewmaRead(&t.rec(id).ttftBits))
}

// TokensPerSec returns the smoothed generation rate. Zero means no sample yet.
func (t *Tracker) TokensPerSec(id string) float64 {
	return ewmaRead(&t.rec(id).tpsBits)
}

// IDs lists every tracked deployment.
func (t *Tracker) IDs() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.records))
	for id := range t.records {
		out = append(out, id)
	}
	return out
}

func ewmaUpdate(bits *atomic.Uint64, sample, alpha float64) {
	for {
		old := bits.Load()
		cur := math.Float64frombits(old)
		var next float64
		if cur == 0 {
			next = sample // seed on first sample rather than decaying from zero
		} else {
			next = alpha*sample + (1-alpha)*cur
		}
		if bits.CompareAndSwap(old, math.Float64bits(next)) {
			return
		}
	}
}

func ewmaRead(bits *atomic.Uint64) float64 {
	return math.Float64frombits(bits.Load())
}
