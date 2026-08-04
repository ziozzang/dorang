package capacity

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"sync/atomic"
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

	// ErrQueueFull is returned when every axis the request would wait on is
	// already holding its configured `max_queue` waiters.
	//
	// It is a separate error from ErrQueueTimeout because the two say different
	// things to an operator: a full queue means the ceiling is doing its job and
	// shedding, while a timeout means the queue was admitted to and never
	// served. Collapsing them would hide which of the two knobs is the one set
	// wrong.
	ErrQueueFull = errors.New("capacity: the wait queue is full")

	// ErrQueueTimeout is returned when a waiter's queue budget elapsed before
	// capacity freed. The budget is `capacity.principals.<id>.max_queue_wait`.
	ErrQueueTimeout = errors.New("capacity: queue wait budget elapsed")
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

// QueueConfig bounds the queue in front of each axis: how many callers may wait
// (`max_queue`) and how long one may wait (`max_queue_wait`).
//
// The two are ceilings on the queue, not on the work, and they belong here
// rather than in internal/quota for the reason the concurrency ceilings do: the
// queue IS the broker's, nothing else can see its depth, and a wait that is not
// bounded where the waiting happens is not bounded at all. Before this, a
// saturated axis grew an unbounded heap of blocked callers and the 30-second
// budget every principal is defaulted to was computed and thrown away.
//
// Every map is keyed exactly as its concurrency counterpart is, so a limit and
// its queue are configured against the same name.
type QueueConfig struct {
	Global           int
	ProviderGroups   map[string]int
	CredentialGroups map[string]int
	Routes           map[string]int
	Models           []ModelLimit
	Principals       map[string]int

	// MaxWait bounds how long one of a principal's requests may sit in a queue.
	// The "default" entry applies to a principal without an explicit one, as it
	// does for the concurrency ceiling. Zero means no bound.
	MaxWait map[string]time.Duration
}

// empty reports whether nothing here constrains anything, which lets the hot
// path skip the whole mechanism with one boolean.
func (q QueueConfig) empty() bool {
	return q.Global <= 0 && len(q.ProviderGroups) == 0 && len(q.CredentialGroups) == 0 &&
		len(q.Routes) == 0 && len(q.Models) == 0 && len(q.Principals) == 0 && len(q.MaxWait) == 0
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

	// Queues bounds the waiting, as opposed to the running. It is optional and
	// entirely additive: a zero QueueConfig is the unbounded, never-timing-out
	// behaviour the broker had before.
	Queues QueueConfig

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

	// SoftReservations selects the multi-axis starvation guard (DESIGN §5.4
	// correction 5, which closed risk W8). The zero value is on; see
	// SoftReservationMode and the "Soft reservations" section of the package
	// documentation.
	//
	// NOT REACHABLE FROM CONFIGURATION. §18's W8 row promises "on by default,
	// with an off switch", and the switch is this field and SoftReserveAfter —
	// which no YAML key sets and nothing in internal/app or internal/config
	// writes. So the default holds and the escape hatch does not exist: an
	// operator who hits a pathology the guard causes has no way to turn it off
	// short of a rebuild. Recorded in docs/CONFIG.md §8.1 rather than left as a
	// promise the wiring does not keep.
	SoftReservations SoftReservationMode
	// SoftReserveAfter is how many probes a waiter must fail before the guard
	// arms for it. Zero selects DefaultSoftReserveAfter, which documents the
	// measured throughput cost of each setting; values below one are clamped to
	// one, which arms on the first failed probe. It trades a constant on the
	// rescue latency against the throughput the guard idles.
	SoftReserveAfter int
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
	// PrincipalMax is this caller's OWN concurrency ceiling
	// (`max_parallel_requests` on the api key), 0 for none.
	//
	// It travels with the request because it is not in the configuration file:
	// Config.Principals is a static map an operator would have to populate with
	// literal key ids, while this comes from the key's own row and changes when
	// the key is edited. The stricter of the two applies, so a configured
	// principal ceiling still bounds a key that was issued a looser one.
	PrincipalMax int
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

// queueLimits is the compiled form of [QueueConfig]'s depth ceilings, in the
// same shape the concurrency limits take so that [Broker.needs] can fill both in
// one pass.
type queueLimits struct {
	global      int
	pgroups     map[string]int
	cgroups     map[string]int
	routes      map[string]int
	models      map[modelIdent]int
	principals  map[string]int
	hasDefaultP bool
}

func (q *queueLimits) principal(id string) int {
	if l, ok := q.principals[id]; ok {
		return l
	}
	if q.hasDefaultP {
		return q.principals["default"]
	}
	return 0
}

// bucket is one counted axis key and its wait queue.
type bucket struct {
	limit int
	// queueMax is the configured depth ceiling for this key, 0 for unbounded.
	// It is stamped when the bucket is created and refreshed whenever a need
	// carries one, because a bucket can be created by an axis that was checked
	// before the queue ceiling was known.
	queueMax int
	// inUse counts committed reservations *plus* the soft reservation, if any.
	// Counting the claim as occupancy is what keeps it off the hot path: a
	// request that is not the claimant needs no extra test at all, because the
	// unit set aside is already subtracted from what it can see. Only the
	// claimant subtracts it back out, and only a waiter can be a claimant.
	inUse int
	q     waitQueue

	// claimant holds the soft reservation on this key, if any. At most one
	// exists at a time, which is what bounds the idled capacity to one unit per
	// axis key. See doc.go, "Soft reservations".
	claimant *waiter
}

// committed is inUse minus the unit held idle by a soft reservation: the number
// of reservations actually outstanding, which is what observability reports.
func (bk *bucket) committed() int {
	if bk.claimant != nil {
		return bk.inUse - 1
	}
	return bk.inUse
}

// Broker admits requests against every axis that constrains them.
// The zero value is not usable; call New.
type Broker struct {
	// share is how many nodes divide every configured ceiling; 1 unclustered.
	//
	// Atomic and not under b.mu on purpose: it is read once per needs() call on
	// the hot path and written from the cluster heartbeat, so putting it under
	// the broker lock would make a background tick contend with every acquire.
	// A read that crosses a write sees the old or the new divisor and both are
	// valid ceilings; there is no torn state to protect.
	share atomic.Int64

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
	soft        bool
	softAfter   int

	// queues mirrors the limit maps above for queue depth, and waits holds the
	// per-principal wait budget. bounded is the one test the hot path makes.
	queues      queueLimits
	waits       map[string]time.Duration
	hasDefaultW bool
	bounded     bool

	mu      sync.Mutex
	buckets map[axisKey]*bucket
	live    map[*Reservation]struct{}
	waiters map[*waiter]struct{}
	seq     uint64
	pass    uint64
	closed  bool

	// Instrumentation, guarded by mu.
	wakeups       uint64
	grants        uint64
	expired       uint64
	softPlaced    uint64
	queueRefused  uint64
	queueTimeouts uint64

	// pending is the work list of buckets a dropped soft reservation has freed
	// and that still have to be offered to their queues. It turns what would be
	// recursion — serving a bucket finishes a waiter, which drops a claim, which
	// frees a bucket that must itself be served — into a flat loop
	// (drainPendingLocked). It is guarded by mu and reused, so a release
	// allocates nothing, and it stays empty on a broker that never claims.
	pending []*bucket

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
		soft:       cfg.SoftReservations.enabled(),
		softAfter:  cfg.SoftReserveAfter,
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
	b.queues = queueLimits{
		global:     cfg.Queues.Global,
		pgroups:    copyLimits(cfg.Queues.ProviderGroups),
		cgroups:    copyLimits(cfg.Queues.CredentialGroups),
		routes:     copyLimits(cfg.Queues.Routes),
		principals: copyLimits(cfg.Queues.Principals),
		models:     make(map[modelIdent]int, len(cfg.Queues.Models)),
	}
	for _, m := range cfg.Queues.Models {
		if m.Max > 0 {
			b.queues.models[modelIdent{m.Provider, m.Model}] = m.Max
		}
	}
	_, b.queues.hasDefaultP = b.queues.principals["default"]
	b.waits = make(map[string]time.Duration, len(cfg.Queues.MaxWait))
	for k, v := range cfg.Queues.MaxWait {
		if v > 0 {
			b.waits[k] = v
		}
	}
	_, b.hasDefaultW = b.waits["default"]
	b.bounded = !cfg.Queues.empty()
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
	switch {
	case b.softAfter == 0:
		b.softAfter = DefaultSoftReserveAfter
	case b.softAfter < 1:
		b.softAfter = 1 // a claim can only be placed on a failed probe anyway
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
	// Every waiter has been failed, so every queue is empty and the units their
	// soft reservations were holding — already given back by finishLocked — have
	// nobody to be offered to.
	b.pending = b.pending[:0]
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
	// Every queue lookup below is skipped entirely on a broker that configures
	// no queue ceilings, which is the common case and the one the benchmark
	// measures.
	q := b.bounded

	if b.global > 0 {
		n := axisNeed{key: globalKey(), limit: b.global}
		if q {
			n.queue = b.queues.global
		}
		out = append(out, n)
	}
	if req.PrincipalID != "" {
		if l := strictest(b.principalLimit(req.PrincipalID), req.PrincipalMax); l > 0 {
			n := axisNeed{key: principalKey(req.PrincipalID), limit: l}
			if q {
				n.queue = b.queues.principal(req.PrincipalID)
			}
			out = append(out, n)
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
			n := axisNeed{key: routeKey(provider), limit: l}
			if q {
				n.queue = b.queues.routes[provider]
			}
			out = append(out, n)
		}
	}
	if pgroup != "" {
		if l := b.pgroups[pgroup]; l > 0 {
			n := axisNeed{key: pgroupKey(pgroup), limit: l}
			if q {
				n.queue = b.queues.pgroups[pgroup]
			}
			out = append(out, n)
		}
	}
	if model != "" {
		if l := b.models[modelIdent{provider, model}]; l > 0 {
			n := axisNeed{key: modelAxisKey(provider, model), limit: l}
			if q {
				n.queue = b.queues.models[modelIdent{provider, model}]
			}
			out = append(out, n)
		}
	}
	if c != nil {
		if c.CapacityGroup != "" {
			if l := b.cgroups[c.CapacityGroup]; l > 0 {
				n := axisNeed{key: cgroupKey(c.CapacityGroup), limit: l}
				if q {
					n.queue = b.queues.cgroups[c.CapacityGroup]
				}
				out = append(out, n)
			}
		}
		if c.ID != "" && c.MaxConcurrent > 0 {
			out = append(out, axisNeed{key: credAxisKey(provider, c.ID), limit: c.MaxConcurrent})
		}
	}
	return b.divideByShare(out)
}

// divideByShare turns each configured ceiling into THIS NODE'S share of it.
//
// A ceiling in the configuration is a statement about the whole deployment —
// "this plan allows 16 in flight" — and this broker counts only its own
// process. So with N nodes the provider saw N x every ceiling: measured on an
// isolated two-node cluster against an upstream counting its own concurrency, a
// credential ceiling of 2 admitted FOUR at once, exactly two per node.
//
// Dividing is exact in the direction that matters. Each node admits at most
// limit/N, so the cluster admits at most N x (limit/N) <= limit: the provider
// NEVER sees more than the operator configured. What it costs is the opposite
// error — an idle node's share is not lent to a busy one, so a cluster can
// refuse while capacity exists elsewhere. That is a throughput cost, and the
// alternative is a store round trip inside the lock that serialises every
// acquire on every axis, which is a latency cost on every request including the
// ones that would never have contended.
//
// A ceiling SMALLER than the node count cannot be divided. Each node keeps 1
// rather than 0, because 0 means that node can never serve that axis at all and
// a cluster of zeroes serves nothing. That is the one case where the cluster
// can exceed the configured ceiling, it is bounded by (nodes - limit), and
// [Broker.ShareOvershoot] publishes it rather than leaving it to be discovered.
func (b *Broker) divideByShare(out []axisNeed) []axisNeed {
	n := int(b.share.Load())
	if n <= 1 {
		return out
	}
	for i := range out {
		if out[i].limit <= 0 {
			continue
		}
		if l := out[i].limit / n; l > 0 {
			out[i].limit = l
		} else {
			out[i].limit = 1
		}
	}
	return out
}

// SetShare tells the broker how many nodes divide every configured ceiling.
//
// It is a whole number rather than a fraction because the ceilings are, and it
// is set from the live node registry rather than from configuration: a cluster
// that loses a node should not keep reserving its share forever, and one that
// gains a node must narrow BEFORE that node starts serving.
//
// Zero and one both mean "this process is the whole deployment", which is the
// unclustered default and the behaviour every existing test was written
// against.
func (b *Broker) SetShare(nodes int) {
	if nodes < 1 {
		nodes = 1
	}
	b.share.Store(int64(nodes))
}

// Share reports the divisor in force, which is 1 until one is set.
//
// Never 0. The field's zero value and "this process is the whole deployment"
// are the same state, and reporting 0 would put a divisor nobody could divide
// by into a log line — the value is read back by the cluster heartbeat to
// decide whether the count changed.
func (b *Broker) Share() int {
	if n := int(b.share.Load()); n > 1 {
		return n
	}
	return 1
}

// ShareOvershoot reports how far a cluster of this many nodes can exceed the
// given configured ceiling, which is zero unless the ceiling is smaller than
// the node count.
//
// It exists so the figure is published rather than discovered. DESIGN §5.6
// requires every coordination mode to publish its maximum overshoot as a
// number, and "0 except for ceilings below the node count, where it is
// nodes - ceiling" is that number for this mechanism.
func ShareOvershoot(limit, nodes int) int {
	if nodes <= 1 || limit <= 0 || limit >= nodes {
		return 0
	}
	return nodes - limit
}

// WaitBudget reports how long this principal may sit in a queue before
// [Broker.Acquire] gives up with [ErrQueueTimeout], or zero when it may wait
// indefinitely. It is `capacity.principals.<id>.max_queue_wait`.
//
// It is exported because the caller has to decide whether to wait at all before
// it calls Acquire — an unpinned request spills or falls back instead of
// queueing (§7.4a2, §7.6) — and that decision needs the number.
func (b *Broker) WaitBudget(principal string) time.Duration { return b.waitBudget(principal) }

// waitBudget is how long this principal may sit in a queue, 0 for no bound.
func (b *Broker) waitBudget(principal string) time.Duration {
	if len(b.waits) == 0 {
		return 0
	}
	if d, ok := b.waits[principal]; ok {
		return d
	}
	if b.hasDefaultW {
		return b.waits["default"]
	}
	return 0
}

// strictest is the tighter of two ceilings, where 0 means "no ceiling" and
// therefore always loses.
//
// A bucket's limit is stamped when it is first created, so two requests with
// different per-key ceilings on the same principal id cannot disagree in
// practice: the principal id IS the key id, so every request on that axis
// carries the same key's number. The general form is written anyway, because the
// axis is documented as "api key, user or team id" and a future user-scoped
// principal would break the assumption silently.
func strictest(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case b < a:
		return b
	}
	return a
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

// checkLocked reports whether every axis in needs has room for w, which is nil
// for a request that is not (yet) a waiter. On failure it returns the bucket
// that blocked (nil when the block is permanent) and whether the block is
// permanent: a permanent block means the effective ceiling is zero, so no
// release can ever help.
//
// A soft reservation is already part of bk.inUse, so everyone but its holder
// sees it as occupancy for free. The holder subtracts it back out, which is
// exactly what makes a claim a guarantee rather than a hint: inUse <= limit is
// maintained by every path, so a claimant's own check inUse-1 < limit cannot
// fail.
//
// The claim test only runs on a bucket that is otherwise full, so the fast path
// — where nothing is full — reaches it never, and w is a sentinel rather than
// nil so that it is one pointer compare when it does run.
//
// The interactive reserve is not handled here at all; tryLocked routes batch
// work under a reserve to checkReservedLocked instead. This function is the
// innermost loop of every acquisition and has to stay inlinable into tryLocked:
// before soft reservations it fitted the budget with a single node to spare, and
// losing the inline costs about 7% of an uncontended acquire. Splitting the
// reserve out pays for the claim test with room over, and it takes the reserve's
// floating-point work off the interactive path, which never needed it. For
// interactive work the effective ceiling is always the raw limit, and needs only
// ever contains axes with a positive one, so this path also cannot report a
// permanent block.
//
// A non-permanent block always has a bucket, because a bucket exists as soon as
// anything has committed on or claimed that key, and a positive ceiling can only
// be reached by something having committed.
func (b *Broker) checkLocked(needs []axisNeed, w *waiter) (blocking *bucket, ok bool, permanent bool) {
	for _, n := range needs {
		bk := b.buckets[n.key]
		if bk == nil || bk.inUse < n.limit {
			continue
		}
		if bk.claimant != w || bk.inUse-1 >= n.limit {
			return bk, false, false
		}
	}
	return nil, true, false
}

// checkReservedLocked is checkLocked for batch work under an interactive
// reserve, where the effective ceiling is below the configured one and can even
// be zero, which is the only way a request becomes permanently unsatisfiable.
//
// It carries no claim test because batch work under a reserve is exactly the
// work that never holds a soft reservation (see doc.go). It still sees other
// waiters' claims, because a claim is counted as occupancy — which is the whole
// effect batch needs from one.
func (b *Broker) checkReservedLocked(needs []axisNeed) (blocking *bucket, ok bool, permanent bool) {
	for _, n := range needs {
		eff := b.effLimit(n.limit, true)
		if eff <= 0 {
			return nil, false, true
		}
		if bk := b.buckets[n.key]; bk != nil && bk.inUse >= eff {
			return bk, false, false
		}
	}
	return nil, true, false
}

// commitLocked increments every axis at once and returns the reservation.
// It is only ever reached from a checkLocked that passed, in the same critical
// section, which is what makes acquisition all-or-nothing.
//
// A soft reservation held by w on an axis it commits is consumed by the commit:
// the unit that was already counted as occupancy on w's behalf simply becomes
// the unit w occupies, so inUse does not move for that axis.
func (b *Broker) commitLocked(needs []axisNeed, credID string, ttl time.Duration, w *waiter) *Reservation {
	r := &Reservation{b: b, credID: credID}
	r.held = r.heldBuf[:0]
	for _, n := range needs {
		bk := b.buckets[n.key]
		if bk == nil {
			bk = &bucket{limit: n.limit, queueMax: n.queue}
			b.buckets[n.key] = bk
		}
		if bk.claimant == w {
			bk.claimant = nil // the claimed unit is already counted
		} else {
			bk.inUse++
		}
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
//
// w is the waiter this attempt is made on behalf of, or noClaimant for a fresh
// Acquire/TryAcquire. It only affects how soft reservations are counted.
func (b *Broker) tryLocked(req *Request, blockBuf []*bucket, w *waiter) (*Reservation, []*bucket, bool) {
	var needBuf [numAxes]axisNeed
	needs := needBuf[:0]

	blocking := blockBuf[:0]
	permanent := true
	n := candidateCount(req)
	// Only batch work under a non-zero reserve sees an effective ceiling below
	// the configured one, and it is the only work that can be permanently
	// blocked. Deciding once here keeps that whole case out of checkLocked.
	reserved := req.Batch && b.reserve > 0

	for i := 0; i < n; i++ {
		c := candidateAt(req, i)
		if c == nil && len(req.Candidates) > 0 {
			break
		}
		needs = b.needs(req, c, needs)
		var (
			blk  *bucket
			ok   bool
			perm bool
		)
		if reserved {
			blk, ok, perm = b.checkReservedLocked(needs)
		} else {
			blk, ok, perm = b.checkLocked(needs, w)
		}
		if ok {
			id := ""
			if c != nil {
				id = c.ID
			}
			return b.commitLocked(needs, id, req.TTL, w), nil, false
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
	res, _, _ := b.tryLocked(&req, buf[:], noClaimant)
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
	res, blocking, permanent := b.tryLocked(&req, buf[:], noClaimant)
	if res != nil {
		b.grants++
		b.mu.Unlock()
		return res, nil
	}
	if permanent || len(blocking) == 0 {
		b.mu.Unlock()
		return nil, ErrUnsatisfiable
	}
	// max_queue is checked BEFORE the waiter exists, so a refused request has
	// left nothing behind to unwind. A ceiling is per axis and every axis the
	// request would block on has to admit it: a waiter sits in all of them at
	// once, so being over the ceiling on any one of them is being over it.
	if bk := b.queueFullLocked(blocking); bk != nil {
		b.mu.Unlock()
		return nil, ErrQueueFull
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

	// max_queue_wait. The budget is the principal's, and it bounds the WAIT
	// rather than the request: a caller that has already been queued for its
	// whole budget is told so, instead of holding a connection open until the
	// request timeout that the operator set for a generation, not for a queue.
	//
	// It does not apply to batch. Batch is the work that is SUPPOSED to wait —
	// §11.1 gives it a lower ceiling on every axis precisely so that it yields
	// to interactive traffic — and a thirty-second budget, which is the default
	// every principal carries, would turn ordinary contention into failed rows.
	// A batch row's bound is its own context, which the scheduler owns.
	var timeout <-chan time.Time
	if budget := b.waitBudget(req.PrincipalID); budget > 0 && !req.Batch {
		t := time.NewTimer(budget)
		defer t.Stop()
		timeout = t.C
	}

	select {
	case g := <-w.ch:
		return g.res, g.err

	case <-timeout:
		b.mu.Lock()
		if w.state == stateWaiting {
			b.unqueueLocked(w)
			w.state = stateCancelled
			delete(b.waiters, w)
			b.queueTimeouts++
			// A soft reservation this waiter held is capacity kept idle for it.
			// Give it back before dropping the lock, exactly as cancellation
			// does — a timeout that leaked one would idle a slot per expiry.
			b.releaseClaimsLocked(w)
			b.drainPendingLocked()
			b.mu.Unlock()
			return nil, ErrQueueTimeout
		}
		b.mu.Unlock()
		// A grant raced with the timer. It is already in the channel; the
		// request won, so take it rather than discarding capacity nobody else
		// can be given without a second release.
		g := <-w.ch
		return g.res, g.err

	case <-ctx.Done():
		b.mu.Lock()
		if w.state == stateWaiting {
			b.unqueueLocked(w)
			w.state = stateCancelled
			delete(b.waiters, w)
			// Any soft reservation this waiter held is capacity that was being
			// kept idle for it. Give it back and offer it to the queues before
			// dropping the lock, or it idles until an unrelated release.
			b.releaseClaimsLocked(w)
			b.drainPendingLocked()
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

// queueFullLocked reports the first axis whose queue is already at its
// configured depth, or nil when every axis can take one more waiter.
//
// The count is the heap length, which is the number of waiters currently sitting
// in that queue — not the number of requests blocked overall, since one waiter
// on a Spill request sits in several queues at once. That is the right unit: the
// ceiling exists to bound what one saturated axis makes the process hold.
//
// It is checked on ADMISSION only. A waiter that is re-enqueued onto a newly
// blocking axis (serveLocked) is not re-tested, and may therefore push that
// axis one over its ceiling. That is deliberate: `max_queue` bounds how many
// callers may START waiting on an axis, and turning an already-admitted waiter
// away because the axis it moved to is busy would fail a request that has
// already waited — the one caller with the strongest claim to be served — while
// freeing nothing, since it was occupying a queue slot elsewhere a moment
// earlier. The process-wide bound is unaffected: waiters are only ever created
// by an admission that passed this test.
func (b *Broker) queueFullLocked(blocking []*bucket) *bucket {
	if !b.bounded {
		return nil
	}
	for _, bk := range blocking {
		if bk.queueMax > 0 && bk.q.Len() >= bk.queueMax {
			b.queueRefused++
			return bk
		}
	}
	return nil
}

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
//
// It is the single choke point where a waiter leaves the wait state through the
// broker, so it is also where soft reservations are given back. On a grant the
// claims covering the axes just committed are already gone (commitLocked
// consumed them); what can remain is the prefix of a Spill waiter that was
// served through a different candidate.
func (b *Broker) finishLocked(w *waiter, g grant, st waiterState) bool {
	if w.state != stateWaiting {
		return false
	}
	w.state = st
	delete(b.waiters, w)
	w.ch <- g // buffered with room for exactly this send, never blocks
	b.releaseClaimsLocked(w)
	return true
}

// ---------------------------------------------------------------------------
// Release and targeted wakeup
// ---------------------------------------------------------------------------

// releaseLocked gives back every axis of one reservation and then serves the
// queues of exactly those axes, plus whatever that knocks loose (drainLocked).
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
	b.drainPendingLocked()
}

// drainPendingLocked serves whatever giving a soft reservation back knocked
// loose, and is the only way drainLocked is ever entered.
//
// Every path that can drop a claim — a grant that orphaned one, a cancellation,
// a close — only appends the affected buckets to b.pending, and every top-level
// entry point ends here. That is what keeps the recursion out without a
// re-entrancy flag, and it is why the common release, on a broker where nothing
// is claimed, pays exactly one length test for the whole mechanism.
func (b *Broker) drainPendingLocked() {
	if len(b.pending) > 0 {
		b.drainLocked()
	}
}

// drainLocked serves every bucket on the work list, and everything that becomes
// servable as a knock-on effect, iteratively.
//
// The knock-on case is a soft reservation orphaned by a grant or dropped by a
// cancellation: the unit it was keeping idle is real capacity, so the bucket has
// to be re-offered to its queue rather than left idle until some unrelated
// release happens to touch it.
//
// Each round of the loop gets its own pass number. Reusing one would let a
// waiter already probed this release miss capacity that appeared afterwards,
// which is a lost wakeup. Rounds terminate because every round after the first
// is caused by a waiter leaving the wait state, and a waiter does that once.
func (b *Broker) drainLocked() {
	for len(b.pending) > 0 && !b.closed {
		n := len(b.pending)
		b.pass++
		pass := b.pass
		for i := 0; i < n; i++ {
			b.serveLocked(b.pending[i], 1, pass)
		}
		// Drop the round just served, keeping anything it appended. The copy
		// overlaps in place, so no allocation happens after the first release.
		b.pending = append(b.pending[:0], b.pending[n:]...)
	}
	b.pending = b.pending[:0]
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
	if bk.q.Len() == 0 || b.closed {
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

		res, blocking, permanent := b.tryLocked(&w.req, buf[:], w)
		b.unqueueLocked(w)
		switch {
		case res != nil:
			b.finishLocked(w, grant{res: res}, stateGranted)
			b.grants++
			granted++
		case permanent:
			b.finishLocked(w, grant{err: ErrUnsatisfiable}, stateFailed)
		default:
			// The waiter still cannot be served: it reached the head of a queue
			// and found another axis it needs full. Once it has done that often
			// enough to be ping-ponging rather than merely queued, set aside
			// what it can hold, so that the next axis it waits for is the last
			// one it has to win (DESIGN §5.4 correction 5). This is the only
			// place a soft reservation is ever placed: w is at the head of this
			// queue, so a claim can never be a way to overtake.
			w.bounces++
			if b.extendClaimsLocked(w, bk) {
				// The unit this release freed on *this* axis is now set aside
				// for w. It is spent: nobody behind w in this queue can have
				// it, so there is nothing left to serve.
				granted++
			}
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
	// QueueLimit is the configured max_queue for this key, 0 when unbounded.
	QueueLimit int
	// SoftReserved is set when one unit of this key is being held idle for a
	// named multi-axis waiter (DESIGN §5.4 correction 5). Every other request
	// sees Limit-1 while it is set. It is never set for more than one waiter at
	// a time, which is what bounds the cost to one unit per key.
	SoftReserved bool
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
	// SoftReserved is the number of axis keys currently holding one unit idle
	// for a waiter. It is the live throughput cost of the starvation guard,
	// expressed in slots.
	SoftReserved int
	// SoftReservations is the cumulative number of soft reservations placed.
	// Zero on a workload of single-axis requests, which never need one.
	SoftReservations uint64
	// QueueRefused is the cumulative number of acquisitions refused because an
	// axis was already holding its configured `max_queue` waiters.
	QueueRefused uint64
	// QueueTimeouts is the cumulative number of waiters whose
	// `max_queue_wait` budget elapsed before capacity freed. A non-zero value
	// here with QueueRefused at zero says the queue is deep enough and slow,
	// which is a different remedy from a queue that is too shallow.
	QueueTimeouts uint64
}

// Snapshot returns per-axis occupancy. It allocates and is not for the hot path.
func (b *Broker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()

	s := Snapshot{
		Axes:             make([]AxisState, 0, len(b.buckets)),
		Waiting:          len(b.waiters),
		Reservations:     len(b.live),
		Wakeups:          b.wakeups,
		Grants:           b.grants,
		Expired:          b.expired,
		SoftReserved:     b.softReservedLocked(),
		SoftReservations: b.softPlaced,
		QueueRefused:     b.queueRefused,
		QueueTimeouts:    b.queueTimeouts,
	}
	for k, bk := range b.buckets {
		s.Axes = append(s.Axes, AxisState{
			Axis:         k.axis,
			Key:          k.String(),
			InUse:        bk.committed(),
			Limit:        bk.limit,
			Waiting:      bk.q.Len(),
			QueueLimit:   bk.queueMax,
			SoftReserved: bk.claimant != nil,
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

// LeastUsedKey returns the id of the candidate with the fewest reservations
// outstanding on its own key axis, or "" when cands is empty.
//
// It exists as one call rather than a loop over [Broker.InUse] because it runs
// on the routing hot path: N candidates would otherwise be N mutex round trips
// per routing decision, which would make `key_rotation.strategy: least_used` —
// the default whenever a rotation pool is configured — the most expensive thing
// in the request path.
//
// A credential the broker has never seen counts as zero in use, which is
// correct here and would not be for [Broker.busy]: an account with no
// reservations really is the least used one, whereas a deployment with no
// CONFIGURED ceiling has no occupancy to compare at all.
func (b *Broker) LeastUsedKey(provider string, cands []Candidate) string {
	if len(cands) == 0 {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	best, bestN := "", 0
	for i := range cands {
		id := cands[i].ID
		if id == "" {
			continue
		}
		n := 0
		if bk := b.buckets[credAxisKey(provider, id)]; bk != nil {
			n = bk.committed()
		}
		// Strictly less, so ties keep configuration order — the tie-break the
		// rest of the router already uses, and the only one that is stable.
		if best == "" || n < bestN {
			best, bestN = id, n
		}
	}
	return best
}

// InUse returns the committed count on one axis key and whether that key is
// known to the broker. It is a cheap targeted alternative to Snapshot. A unit
// held idle by a soft reservation is not committed and is not counted here.
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
	return bk.committed(), true
}
