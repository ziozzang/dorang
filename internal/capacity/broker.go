package capacity

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"time"
)

// Errors returned by the broker.
var (
	// ErrClosed is returned by Acquire on a closed broker, and to every waiter
	// still queued when Close is called.
	ErrClosed = errors.New("capacity: broker closed")

	// ErrUnsatisfiable is returned when no candidate can ever succeed because
	// some axis it needs has an effective ceiling of zero. The only way to
	// reach it is a batch request against an axis whose interactive reserve
	// leaves batch no slots at all (DESIGN §11.1); waiting for it would block
	// forever, so it fails immediately instead.
	ErrUnsatisfiable = errors.New("capacity: request can never be satisfied")
)

// OnCapacity selects what happens when the preferred credential candidate has
// no room (DESIGN §5.3).
type OnCapacity uint8

const (
	// Wait blocks on the preferred candidate. No other candidate is tried.
	// This is the zero value.
	Wait OnCapacity = iota
	// Spill tries each remaining candidate in order before blocking.
	Spill
)

// String implements fmt.Stringer.
func (o OnCapacity) String() string {
	if o == Spill {
		return "spill"
	}
	return "wait"
}

// Defaults applied by New when the corresponding Config field is zero.
const (
	DefaultReservationTTL = 10 * time.Minute
	DefaultSweepInterval  = 30 * time.Second
	// DefaultWakeSlack is how many head-of-queue waiters beyond the number of
	// freed slots a single release may probe before giving up. It bounds the
	// wakeups per release to a constant regardless of how many waiters exist.
	DefaultWakeSlack = 8
)

const (
	// maxBlockAxes caps how many distinct axes one waiter may queue on. Only a
	// Spill request with many candidates can approach it.
	maxBlockAxes = 8
	// maxProbePerRelease is the hard ceiling on waiters examined per axis per
	// release, whatever WakeSlack says. It is what makes "bounded wakeups per
	// release" a property of the code rather than of the configuration.
	maxProbePerRelease = 16
)

// ModelLimit is a per (provider, upstream model) concurrency ceiling.
type ModelLimit struct {
	Provider string
	Model    string
	Max      int
}

// Config is the broker's static configuration. Every limit is a maximum number
// of concurrent reservations; zero or less means unlimited and is not counted.
//
// New copies the maps and slices, so the caller may reuse or mutate them
// afterwards. A Broker's configuration is immutable; hot reload replaces the
// broker rather than mutating one (DESIGN §4.1).
type Config struct {
	// Global is the process-wide ceiling.
	Global int
	// ProviderGroups maps a provider-group name to its ceiling.
	ProviderGroups map[string]int
	// CredentialGroups maps a credential-group (account) name to its ceiling.
	CredentialGroups map[string]int
	// Routes maps a provider name to that deployment's own ceiling.
	Routes map[string]int
	// Models holds per (provider, upstream model) ceilings.
	Models []ModelLimit
	// Principals maps a principal id to its ceiling. The entry "default"
	// applies to any principal without an explicit one.
	Principals map[string]int

	// InteractiveReserve is the fraction of every axis, in [0,1), that batch
	// work may not occupy (DESIGN §11.1). Values outside the range are clamped.
	InteractiveReserve float64

	// ReservationTTL is how long a reservation may live before the sweeper
	// reclaims it. Zero selects DefaultReservationTTL; a negative value
	// disables expiry entirely.
	ReservationTTL time.Duration
	// SweepInterval is how often the background sweeper runs. Zero selects
	// DefaultSweepInterval; a negative value disables the background goroutine,
	// leaving Sweep to be driven manually (which is what tests do).
	SweepInterval time.Duration

	// Now is the clock. Zero selects time.Now. It exists so tests can control
	// expiry without sleeping.
	Now func() time.Time

	// WakeSlack bounds the extra head-of-queue probes a single release may make
	// beyond the number of slots it freed. Zero selects DefaultWakeSlack.
	WakeSlack int
}

// Candidate is one credential a request may use. Together with its provider it
// forms the triple (provider, capacity group, key) that is checked and
// committed as a unit (DESIGN §5.3).
type Candidate struct {
	// ID is the credential id. It is not secret and appears in Snapshot.
	ID string
	// CapacityGroup is the credential group (account) this credential belongs
	// to. Empty means the credential-group axis does not apply.
	CapacityGroup string
	// MaxConcurrent is this credential's own ceiling. Zero or less is unlimited.
	MaxConcurrent int

	// Provider, UpstreamModel and ProviderGroup override the request-level
	// values for this candidate. They exist because a model group may span
	// providers, so a Spill can move between candidates whose provider-scoped
	// axes differ; those axes are re-derived from the candidate rather than
	// carried over (DESIGN §5.3). Leave them empty to inherit from the Request,
	// which is the common single-provider case.
	Provider      string
	UpstreamModel string
	ProviderGroup string
}

// Request is one admission attempt.
type Request struct {
	// Provider, Model and ProviderGroup are the defaults for candidates that do
	// not override them.
	Provider      string
	Model         string
	ProviderGroup string
	// PrincipalID is the api key, user or team id. Empty skips the axis.
	PrincipalID string
	// Candidates are the credentials that may serve this request. An empty
	// slice is legal and reserves only the non-credential axes.
	Candidates []Candidate
	// Preferred is the id of the preferred candidate. Empty means the first.
	Preferred string
	// OnCapacity selects Wait (preferred candidate only) or Spill.
	OnCapacity OnCapacity
	// Batch subjects this request to the interactive reserve on every axis.
	Batch bool
	// TTL overrides Config.ReservationTTL for this reservation. DESIGN §5.3
	// derives the deadline from the request's own timeout, which varies per
	// provider, so it cannot be a broker-wide constant. Zero uses the broker
	// default; a negative value gives this reservation no deadline.
	TTL time.Duration
}

// bucket is one counted axis key and its wait queue.
type bucket struct {
	limit int
	inUse int
	q     waitQueue
}

// Broker admits requests against every axis that constrains them.
// The zero value is not usable; call New.
type Broker struct {
	// Immutable after New.
	global      int
	pgroups     map[string]int
	cgroups     map[string]int
	routes      map[string]int
	models      map[modelIdent]int
	principals  map[string]int
	reserve     float64
	ttl         time.Duration
	sweepEvery  time.Duration
	now         func() time.Time
	wakeSlack   int
	hasDefaultP bool

	mu      sync.Mutex
	buckets map[axisKey]*bucket
	live    map[*Reservation]struct{}
	waiters map[*waiter]struct{}
	seq     uint64
	pass    uint64
	closed  bool

	// Instrumentation, guarded by mu.
	wakeups uint64
	grants  uint64
	expired uint64

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New builds a broker from cfg and, unless disabled, starts the expiry sweeper.
// Call Close to stop it.
func New(cfg Config) *Broker {
	b := &Broker{
		global:     cfg.Global,
		pgroups:    copyLimits(cfg.ProviderGroups),
		cgroups:    copyLimits(cfg.CredentialGroups),
		routes:     copyLimits(cfg.Routes),
		principals: copyLimits(cfg.Principals),
		models:     make(map[modelIdent]int, len(cfg.Models)),
		reserve:    cfg.InteractiveReserve,
		now:        cfg.Now,
		wakeSlack:  cfg.WakeSlack,
		buckets:    make(map[axisKey]*bucket),
		live:       make(map[*Reservation]struct{}),
		waiters:    make(map[*waiter]struct{}),
		stop:       make(chan struct{}),
	}
	for _, m := range cfg.Models {
		if m.Max > 0 {
			b.models[modelIdent{m.Provider, m.Model}] = m.Max
		}
	}
	if b.reserve < 0 {
		b.reserve = 0
	}
	if b.reserve > 1 {
		b.reserve = 1
	}
	if b.now == nil {
		b.now = time.Now
	}
	switch {
	case cfg.ReservationTTL == 0:
		b.ttl = DefaultReservationTTL
	case cfg.ReservationTTL < 0:
		b.ttl = 0 // expiry disabled
	default:
		b.ttl = cfg.ReservationTTL
	}
	switch {
	case cfg.SweepInterval == 0:
		b.sweepEvery = DefaultSweepInterval
	case cfg.SweepInterval < 0:
		b.sweepEvery = 0 // background sweeper disabled
	default:
		b.sweepEvery = cfg.SweepInterval
	}
	switch {
	case b.wakeSlack == 0:
		b.wakeSlack = DefaultWakeSlack
	case b.wakeSlack < 0:
		b.wakeSlack = 0
	}
	_, b.hasDefaultP = b.principals["default"]

	if b.ttl > 0 && b.sweepEvery > 0 {
		b.wg.Add(1)
		go b.sweepLoop()
	}
	return b
}

// copyLimits copies a limit map, dropping unlimited (<= 0) entries so lookups
// need no second test.
func copyLimits(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		if v > 0 {
			out[k] = v
		}
	}
	return out
}

// Close stops the sweeper and fails every queued waiter with ErrClosed.
// Reservations already held stay valid and may still be released. Close is
// idempotent.
func (b *Broker) Close() {
	b.stopOnce.Do(func() { close(b.stop) })

	b.mu.Lock()
	b.closed = true
	for w := range b.waiters {
		b.unqueueLocked(w)
		b.finishLocked(w, grant{err: ErrClosed}, stateFailed)
	}
	b.mu.Unlock()

	b.wg.Wait()
}

func (b *Broker) sweepLoop() {
	defer b.wg.Done()
	t := time.NewTicker(b.sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
			b.Sweep()
		}
	}
}

// ---------------------------------------------------------------------------
// Axis derivation
// ---------------------------------------------------------------------------

// needs fills out with every counted axis the given candidate requires, in the
// fixed order of the Axis constants. Axes with an unlimited limit are omitted
// entirely, so an unlimited axis costs nothing at check or commit time.
// Provider-scoped axes are derived from the candidate, falling back to the
// request (DESIGN §5.3). A nil candidate yields only the non-credential axes.
func (b *Broker) needs(req *Request, c *Candidate, out []axisNeed) []axisNeed {
	out = out[:0]

	if b.global > 0 {
		out = append(out, axisNeed{globalKey(), b.global})
	}
	if req.PrincipalID != "" {
		if l := b.principalLimit(req.PrincipalID); l > 0 {
			out = append(out, axisNeed{principalKey(req.PrincipalID), l})
		}
	}

	provider, model, pgroup := req.Provider, req.Model, req.ProviderGroup
	if c != nil {
		if c.Provider != "" {
			provider = c.Provider
		}
		if c.UpstreamModel != "" {
			model = c.UpstreamModel
		}
		if c.ProviderGroup != "" {
			pgroup = c.ProviderGroup
		}
	}

	if provider != "" {
		if l := b.routes[provider]; l > 0 {
			out = append(out, axisNeed{routeKey(provider), l})
		}
	}
	if pgroup != "" {
		if l := b.pgroups[pgroup]; l > 0 {
			out = append(out, axisNeed{pgroupKey(pgroup), l})
		}
	}
	if model != "" {
		if l := b.models[modelIdent{provider, model}]; l > 0 {
			out = append(out, axisNeed{modelAxisKey(provider, model), l})
		}
	}
	if c != nil {
		if c.CapacityGroup != "" {
			if l := b.cgroups[c.CapacityGroup]; l > 0 {
				out = append(out, axisNeed{cgroupKey(c.CapacityGroup), l})
			}
		}
		if c.ID != "" && c.MaxConcurrent > 0 {
			out = append(out, axisNeed{credAxisKey(provider, c.ID), c.MaxConcurrent})
		}
	}
	return out
}

func (b *Broker) principalLimit(id string) int {
	if l, ok := b.principals[id]; ok {
		return l
	}
	if b.hasDefaultP {
		return b.principals["default"]
	}
	return 0
}

// effLimit applies the interactive reserve. Batch work sees a lower ceiling on
// every axis, not just the credential one (DESIGN §11.1).
//
// The epsilon absorbs binary floating point error: 10 * (1 - 0.3) evaluates to
// 6.999999999999999, and flooring that to 6 would silently reserve more than
// was configured.
func (b *Broker) effLimit(limit int, batch bool) int {
	if !batch || b.reserve <= 0 {
		return limit
	}
	if b.reserve >= 1 {
		return 0
	}
	return int(math.Floor(float64(limit)*(1-b.reserve) + 1e-9))
}

// ---------------------------------------------------------------------------
// Acquisition
// ---------------------------------------------------------------------------

// checkLocked reports whether every axis in needs has room. On failure it
// returns the bucket that blocked (nil when the block is permanent) and whether
// the block is permanent: a permanent block means the effective ceiling is zero,
// so no release can ever help.
//
// A non-permanent block always has a bucket, because a bucket exists as soon as
// anything has committed on that key, and a positive ceiling can only be reached
// by something having committed.
func (b *Broker) checkLocked(needs []axisNeed, batch bool) (blocking *bucket, ok bool, permanent bool) {
	for _, n := range needs {
		eff := b.effLimit(n.limit, batch)
		if eff <= 0 {
			return nil, false, true
		}
		bk := b.buckets[n.key]
		if bk != nil && bk.inUse >= eff {
			return bk, false, false
		}
	}
	return nil, true, false
}

// commitLocked increments every axis at once and returns the reservation.
// It is only ever reached from a checkLocked that passed, in the same critical
// section, which is what makes acquisition all-or-nothing.
func (b *Broker) commitLocked(needs []axisNeed, credID string, ttl time.Duration) *Reservation {
	r := &Reservation{b: b, credID: credID}
	r.held = r.heldBuf[:0]
	for _, n := range needs {
		bk := b.buckets[n.key]
		if bk == nil {
			bk = &bucket{limit: n.limit}
			b.buckets[n.key] = bk
		}
		bk.inUse++
		r.held = append(r.held, bk)
	}
	if ttl == 0 {
		ttl = b.ttl
	}
	if ttl > 0 {
		r.deadline = b.now().Add(ttl)
	}
	b.live[r] = struct{}{}
	return r
}

// candidateAt returns the i'th candidate in attempt order: the preferred one
// first, then the rest in declaration order. A nil result with no candidates
// configured is the implicit "no credential" candidate.
func candidateAt(req *Request, i int) *Candidate {
	n := len(req.Candidates)
	if n == 0 {
		return nil
	}
	pref := 0
	if req.Preferred != "" {
		for j := range req.Candidates {
			if req.Candidates[j].ID == req.Preferred {
				pref = j
				break
			}
		}
	}
	switch {
	case i == 0:
		return &req.Candidates[pref]
	case i <= pref:
		// Still inside the prefix that precedes the preferred entry.
		return &req.Candidates[i-1]
	case i < n:
		return &req.Candidates[i]
	}
	return nil
}

// candidateCount is how many candidates will actually be tried. With Wait, only
// the preferred one is ever tried (DESIGN §5.3).
func candidateCount(req *Request) int {
	if req.OnCapacity == Wait {
		return 1
	}
	if n := len(req.Candidates); n > 0 {
		return n
	}
	return 1
}

// tryLocked runs the whole acquisition of DESIGN §5.3 under the broker lock: it
// checks every axis of every eligible candidate and, for the first candidate
// that passes, commits all of its axes at once. On failure nothing anywhere was
// incremented and it returns the axes to wait on, deduplicated in attempt
// order, plus whether every candidate was permanently blocked.
func (b *Broker) tryLocked(req *Request, blockBuf []*bucket) (*Reservation, []*bucket, bool) {
	var needBuf [numAxes]axisNeed
	needs := needBuf[:0]

	blocking := blockBuf[:0]
	permanent := true
	n := candidateCount(req)

	for i := 0; i < n; i++ {
		c := candidateAt(req, i)
		if c == nil && len(req.Candidates) > 0 {
			break
		}
		needs = b.needs(req, c, needs)
		blk, ok, perm := b.checkLocked(needs, req.Batch)
		if ok {
			id := ""
			if c != nil {
				id = c.ID
			}
			return b.commitLocked(needs, id, req.TTL), nil, false
		}
		if perm {
			// No release can ever unblock this candidate, so queuing on it
			// would be pure waste.
			continue
		}
		permanent = false
		if len(blocking) < maxBlockAxes && !containsBucket(blocking, blk) {
			blocking = append(blocking, blk)
		}
	}
	return nil, blocking, permanent
}

func containsBucket(s []*bucket, bk *bucket) bool {
	for _, v := range s {
		if v == bk {
			return true
		}
	}
	return false
}

// TryAcquire reserves every axis the request needs or returns false. It never
// blocks and never enqueues.
func (b *Broker) TryAcquire(req Request) (*Reservation, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, false
	}
	var buf [maxBlockAxes]*bucket
	res, _, _ := b.tryLocked(&req, buf[:])
	if res == nil {
		return nil, false
	}
	b.grants++
	return res, true
}

// Acquire reserves every axis the request needs, blocking until it can do so or
// ctx is done. It never holds a slot on one axis while waiting for another, so
// there is nothing to deadlock on.
//
// A cancelled context leaves no trace: the waiter is removed from every queue it
// sits in, and a reservation granted concurrently with cancellation is released
// rather than leaked.
func (b *Broker) Acquire(ctx context.Context, req Request) (*Reservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrClosed
	}
	var buf [maxBlockAxes]*bucket
	res, blocking, permanent := b.tryLocked(&req, buf[:])
	if res != nil {
		b.grants++
		b.mu.Unlock()
		return res, nil
	}
	if permanent || len(blocking) == 0 {
		b.mu.Unlock()
		return nil, ErrUnsatisfiable
	}

	b.seq++
	w := &waiter{
		seq:      b.seq,
		enqueued: b.now(),
		req:      req,
		ch:       make(chan grant, 1),
	}
	// The waiter's request outlives this call frame's exclusive view of the
	// candidate slice only if the caller shares it, but the copy costs nothing
	// on the slow path.
	w.req.Candidates = append([]Candidate(nil), req.Candidates...)
	b.waiters[w] = struct{}{}
	b.enqueueLocked(w, blocking)
	b.mu.Unlock()

	select {
	case g := <-w.ch:
		return g.res, g.err

	case <-ctx.Done():
		b.mu.Lock()
		if w.state == stateWaiting {
			b.unqueueLocked(w)
			w.state = stateCancelled
			delete(b.waiters, w)
			b.mu.Unlock()
			return nil, ctx.Err()
		}
		b.mu.Unlock()
		// A grant raced with cancellation. It is already in the channel; take
		// it and give the capacity straight back so nothing leaks.
		g := <-w.ch
		if g.res != nil {
			g.res.Release()
		}
		return nil, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Queue membership
// ---------------------------------------------------------------------------

func (b *Broker) enqueueLocked(w *waiter, axes []*bucket) {
	for _, bk := range axes {
		nd := &qnode{w: w, bk: bk, idx: -1}
		bk.q.push(nd)
		w.at = append(w.at, nd)
	}
}

func (b *Broker) unqueueLocked(w *waiter) {
	for _, nd := range w.at {
		nd.bk.q.remove(nd)
	}
	w.at = w.at[:0]
}

// finishLocked delivers a waiter's single result. It is a no-op if the waiter
// already finished, which is what makes the grant/cancel race safe.
func (b *Broker) finishLocked(w *waiter, g grant, st waiterState) bool {
	if w.state != stateWaiting {
		return false
	}
	w.state = st
	delete(b.waiters, w)
	w.ch <- g // buffered with room for exactly this send, never blocks
	return true
}

// ---------------------------------------------------------------------------
// Release and targeted wakeup
// ---------------------------------------------------------------------------

// releaseLocked gives back every axis of one reservation and then serves the
// queues of exactly those axes.
//
// Every decrement happens before any wakeup. Serving a queue between decrements
// would test a waiter against a state where some of the freed slots have not
// been returned yet, and re-enqueue it for no reason.
func (b *Broker) releaseLocked(held []*bucket) {
	for _, bk := range held {
		if bk.inUse > 0 {
			bk.inUse--
		}
	}
	b.pass++
	pass := b.pass
	for _, bk := range held {
		b.serveLocked(bk, 1, pass)
	}
}

// serveLocked hands out at most freed reservations to the head of one axis's
// queue (DESIGN §5.4).
//
// The number of waiters examined is bounded by a constant, so the cost of a
// release does not grow with the number of waiters: this is the whole
// difference from a broadcast. Each waiter is probed at most once per release
// pass. A waiter that still cannot be satisfied is removed from every queue and
// re-enqueued on whatever now blocks it, keeping its original arrival sequence,
// so it stays ahead of everything that arrived after it.
func (b *Broker) serveLocked(bk *bucket, freed int, pass uint64) {
	if bk.q.Len() == 0 {
		return
	}
	budget := freed + b.wakeSlack
	if budget > maxProbePerRelease {
		budget = maxProbePerRelease
	}

	// Waiters that cannot be served now but are still queued on this axis are
	// parked for the duration of the pass so the next-oldest waiter gets a
	// turn, then restored with their sequence intact. Without this, a batch
	// waiter held below its reduced ceiling would head-of-line block the
	// interactive waiters the reserve exists to protect (DESIGN §11.1).
	var parkArr [maxProbePerRelease]*qnode
	park := parkArr[:0]

	granted := 0
	var buf [maxBlockAxes]*bucket

	for granted < freed && budget > 0 {
		w := bk.q.head()
		if w == nil {
			break
		}
		if w.pass == pass {
			// Already probed by this release; nothing has improved since.
			nd := w.nodeFor(bk)
			if nd == nil || len(park) == cap(park) {
				break
			}
			bk.q.remove(nd)
			park = append(park, nd)
			continue
		}
		w.pass = pass
		budget--
		b.wakeups++

		res, blocking, permanent := b.tryLocked(&w.req, buf[:])
		b.unqueueLocked(w)
		switch {
		case res != nil:
			b.finishLocked(w, grant{res: res}, stateGranted)
			b.grants++
			granted++
		case permanent:
			b.finishLocked(w, grant{err: ErrUnsatisfiable}, stateFailed)
		default:
			b.enqueueLocked(w, blocking)
			if nd := w.nodeFor(bk); nd != nil && len(park) < cap(park) {
				bk.q.remove(nd)
				park = append(park, nd)
			}
		}
	}
	for _, nd := range park {
		bk.q.push(nd)
	}
}

// Reservation is a committed set of axis slots. Release returns them.
// A Reservation is safe for concurrent use.
type Reservation struct {
	b        *Broker
	credID   string
	deadline time.Time
	// held points straight at the buckets this reservation incremented, so
	// releasing needs no map lookup. heldBuf backs it inline: a reservation
	// touches at most one bucket per axis, so it never grows.
	held    []*bucket
	heldBuf [numAxes]*bucket

	// released is guarded by Broker.mu.
	released bool
}

// CredentialID is the id of the credential this reservation selected. It is
// empty when the request carried no candidates.
func (r *Reservation) CredentialID() string {
	if r == nil {
		return ""
	}
	return r.credID
}

// Deadline is when the sweeper will reclaim this reservation. It is the zero
// time when expiry is disabled.
func (r *Reservation) Deadline() time.Time {
	if r == nil {
		return time.Time{}
	}
	return r.deadline
}

// Release returns every slot and wakes whatever the freed capacity can serve.
// It is idempotent, safe on a nil receiver, and safe after the sweeper has
// already reclaimed the reservation.
func (r *Reservation) Release() {
	if r == nil {
		return
	}
	b := r.b
	b.mu.Lock()
	if r.released {
		b.mu.Unlock()
		return
	}
	r.released = true
	delete(b.live, r)
	b.releaseLocked(r.held)
	b.mu.Unlock()
}

// Sweep reclaims every reservation whose deadline has passed and returns how
// many it reclaimed. The background sweeper calls it on Config.SweepInterval;
// it is exported so a caller, or a test with an injected clock, can drive it
// directly.
func (b *Broker) Sweep() int {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()

	var doomed []*Reservation
	for r := range b.live {
		if !r.deadline.IsZero() && now.After(r.deadline) {
			doomed = append(doomed, r)
		}
	}
	for _, r := range doomed {
		r.released = true
		delete(b.live, r)
		b.expired++
		b.releaseLocked(r.held)
	}
	return len(doomed)
}

// ---------------------------------------------------------------------------
// Observability
// ---------------------------------------------------------------------------

// AxisState is one axis key's occupancy.
type AxisState struct {
	Axis Axis
	// Key is the axis key; compound axes render as "provider|model" and
	// "provider|credential-id". It contains provider names, model names and
	// credential *ids* only: no key material ever reaches the broker, so none
	// can appear here.
	Key string
	// InUse is the number of committed reservations on this key.
	InUse int
	// Limit is the configured ceiling, before any interactive reserve.
	Limit int
	// Waiting is the queue depth on this key.
	Waiting int
}

// Snapshot is a consistent view of the broker, safe to publish as metrics.
type Snapshot struct {
	// Axes is sorted by (axis, key) so output is stable.
	Axes []AxisState
	// Waiting is the number of distinct blocked Acquire calls. It is not the
	// sum of AxisState.Waiting, because one Spill waiter may sit in several
	// queues at once.
	Waiting int
	// Reservations is the number of live, unreleased reservations.
	Reservations int
	// Wakeups is the cumulative number of waiters a release has probed. It
	// grows by at most a constant per release; if it starts tracking the number
	// of waiters, targeted wakeup has regressed to a broadcast.
	Wakeups uint64
	// Grants is the cumulative number of successful acquisitions.
	Grants uint64
	// Expired is the cumulative number of reservations the sweeper reclaimed.
	Expired uint64
}

// Snapshot returns per-axis occupancy. It allocates and is not for the hot path.
func (b *Broker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()

	s := Snapshot{
		Axes:         make([]AxisState, 0, len(b.buckets)),
		Waiting:      len(b.waiters),
		Reservations: len(b.live),
		Wakeups:      b.wakeups,
		Grants:       b.grants,
		Expired:      b.expired,
	}
	for k, bk := range b.buckets {
		s.Axes = append(s.Axes, AxisState{
			Axis:    k.axis,
			Key:     k.String(),
			InUse:   bk.inUse,
			Limit:   bk.limit,
			Waiting: bk.q.Len(),
		})
	}
	sort.Slice(s.Axes, func(i, j int) bool {
		if s.Axes[i].Axis != s.Axes[j].Axis {
			return s.Axes[i].Axis < s.Axes[j].Axis
		}
		return s.Axes[i].Key < s.Axes[j].Key
	})
	return s
}

// InUse returns the committed count on one axis key and whether that key is
// known to the broker. It is a cheap targeted alternative to Snapshot.
//
// Axes keyed by a single value (global, principal, route, pgroup, cgroup) take
// an empty sub. The model axis takes (provider, upstream model) and the key axis
// takes (provider, credential id).
func (b *Broker) InUse(axis Axis, key, sub string) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bk := b.buckets[axisKey{axis, key, sub}]
	if bk == nil {
		return 0, false
	}
	return bk.inUse, true
}
