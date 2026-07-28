package quota

import (
	"math"
	"sync"
	"time"
)

// Expiring-quota urgency (DESIGN §7.5a(c)).
//
// A subscription window that resets is use-it-or-lose-it. An allowance 20%
// consumed with one hour left on a weekly window is about to discard 80% of
// what was already paid for, and routing that ignores this wastes the cheapest
// capacity available — invisibly, because nothing fails and only the bill
// moves.
//
//	urgency = unused_fraction ÷ remaining_fraction_of_window
//
// Both terms are in [0,1]. An allowance 20% used with 10% of its window left
// scores 0.8 ÷ 0.1 = 8.0; the same allowance at the start of its window scores
// 0.8. The value is a ranking input only: it is a comparator in the routing
// chain and can never promote a candidate that a pin, a capability filter, a
// quota or a budget has already excluded.

const (
	// DefaultJitter is how far a node perturbs its own urgency ranking, as a
	// fraction: each candidate is scaled by a deterministic factor in
	// [1-DefaultJitter/2, 1+DefaultJitter/2].
	//
	// It is deliberately small. Jitter exists to break ties between nodes that
	// computed the same urgency at the same instant, not to reorder allowances
	// that genuinely differ.
	DefaultJitter = 0.10

	// DefaultDamping is the full weight of the occupancy term: a candidate at
	// full occupancy has zero urgency, because a credential with no free
	// concurrency cannot spend its allowance however much is at stake.
	DefaultDamping = 1.0

	// minRemainingFraction floors the denominator. Without it urgency diverges
	// to +Inf in the final instants of a window, and +Inf compares equal to
	// itself across every candidate, which destroys exactly the ordering this
	// signal exists to provide.
	minRemainingFraction = 1e-3

	// MaxUrgency is the ceiling minRemainingFraction implies: a wholly unused
	// allowance in the last thousandth of its window.
	MaxUrgency = 1 / minRemainingFraction
)

// Allowance is one expiring allowance reduced to what urgency needs. It is the
// pure core of the signal: no clock, no locks, no configuration.
//
// Start and ResetAt bound the window. ResetAt is the load-bearing one — see
// [Meter.Urgency] for where it comes from and why a zero value means "no
// opinion" rather than "now".
type Allowance struct {
	// Used and Limit are the effective figures for the window, in the metric's
	// units. Used is the combined figure of DESIGN §6.2 where a provider
	// reports one.
	Used  int64
	Limit int64
	// Start is when the window began.
	Start time.Time
	// ResetAt is when the window resets and the unused remainder is discarded.
	// Zero means unknown, which is not the same as imminent.
	ResetAt time.Time
	// Resets declares that the unused remainder is actually discarded. It is a
	// declared property of the quota, never inferred from its shape: a rolling
	// balance and a resetting subscription window are indistinguishable from
	// their numbers alone, and guessing wrong spends money early for nothing.
	Resets bool
}

// Urgency returns the undamped, unjittered ratio at now.
//
// Every degenerate case resolves to zero — "no opinion" — rather than to a
// large number. That direction is deliberate: this signal moves traffic, and an
// unknown must never be able to attract it.
func (a Allowance) Urgency(now time.Time) float64 {
	// A quota that does not reset has no urgency at all. Spending a rolling
	// balance or a pay-as-you-go allowance early buys nothing and forfeits the
	// optionality of spending it later.
	if !a.Resets {
		return 0
	}
	if a.Limit <= 0 {
		return 0
	}
	// Fully consumed: nothing left to lose. The credential is also about to be
	// refused by Check, but the two facts are independent and this one is the
	// reason the urgency is zero.
	unused := float64(a.Limit-a.Used) / float64(a.Limit)
	if unused <= 0 {
		return 0
	}
	if unused > 1 {
		unused = 1 // a negative Used cannot mean more than the whole allowance
	}
	// An unknown reset instant is unknown, not immediate. A window whose phase
	// the provider never reported cannot be scored: guessing the phase would
	// spike urgency at an instant unrelated to the real reset.
	if a.ResetAt.IsZero() {
		return 0
	}
	total := a.ResetAt.Sub(a.Start)
	if total <= 0 {
		return 0 // a zero-length or inverted window carries no information
	}
	remaining := a.ResetAt.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	rf := float64(remaining) / float64(total)
	switch {
	case rf > 1:
		// now precedes the window start — clock skew against a provider-reported
		// reset. The window has not started losing anything yet.
		rf = 1
	case rf < minRemainingFraction:
		rf = minRemainingFraction
	}
	return unused / rf
}

// UrgencyInput carries the two things urgency needs that this package must not
// reach for itself.
//
// Occupancy comes from internal/capacity and is passed in rather than imported:
// quota depends on nothing, and a quota package that reached into the
// concurrency broker would make the two subsystems one.
type UrgencyInput struct {
	// Subject is the opaque id of what is being ranked — normally a credential
	// id. It is one half of the jitter key and is never interpreted.
	Subject string
	// NodeID identifies this node. It is the other half of the jitter key.
	NodeID string
	// Occupancy is the subject's current concurrency occupancy in [0,1]: the
	// same least_busy term the router already has. Values outside the range are
	// clamped.
	Occupancy float64
	// Jitter overrides DefaultJitter. A negative value disables jitter, which
	// is what a test that needs an exact ratio asks for.
	Jitter float64
	// Damping overrides DefaultDamping, in [0,1]. A negative value disables
	// damping.
	Damping float64
}

// jitter returns the configured amplitude.
func (in UrgencyInput) jitter() float64 {
	if in.Jitter == 0 {
		return DefaultJitter
	}
	if in.Jitter < 0 {
		return 0
	}
	return in.Jitter
}

// damping returns the configured occupancy weight.
func (in UrgencyInput) damping() float64 {
	if in.Damping == 0 {
		return DefaultDamping
	}
	if in.Damping < 0 {
		return 0
	}
	if in.Damping > 1 {
		return 1
	}
	return in.Damping
}

// Shape applies the two stampede guards to a raw urgency: damping by occupancy
// and a deterministic per-node perturbation.
//
// Both exist for the same reason. At a window edge every node computes the same
// urgency from the same figures at the same moment and converges on one
// credential, whose concurrency limit then becomes the whole fleet's
// bottleneck. Damping is the closed loop — as the chosen credential fills, its
// urgency falls below its neighbours' and traffic redistributes — and jitter is
// the open-loop half that separates nodes which have not yet observed each
// other's effect on occupancy.
//
// The jitter key is (node, subject), not the node alone. A factor that depended
// only on the node would scale every candidate that node ranks by the same
// amount and therefore could not change any node's ordering — it would be
// arithmetic with no effect. See the note in the package documentation.
func Shape(raw float64, in UrgencyInput) float64 {
	if raw <= 0 {
		return 0
	}
	occ := in.Occupancy
	switch {
	case math.IsNaN(occ) || occ < 0:
		occ = 0
	case occ > 1:
		occ = 1
	}
	v := raw * (1 - in.damping()*occ)
	if v <= 0 {
		return 0
	}
	if amp := in.jitter(); amp > 0 {
		v *= 1 + amp*(unitHash(in.NodeID, in.Subject)-0.5)
	}
	if v > MaxUrgency {
		v = MaxUrgency
	}
	return v
}

// unitHash maps a (node, subject) pair to a deterministic value in [0,1).
//
// Deterministic is a requirement, not a convenience: a random perturbation
// would make the ranking untestable and would also re-roll on every request,
// which turns a stable preference into flapping. FNV-1a over the two strings,
// finished with a bit mixer so that ids differing in one character do not land
// next to each other.
func unitHash(node, subject string) float64 {
	const (
		fnvOffset = 14695981039346656037
		fnvPrime  = 1099511628211
	)
	h := uint64(fnvOffset)
	for i := range len(node) {
		h = (h ^ uint64(node[i])) * fnvPrime
	}
	h = (h ^ 0xff) * fnvPrime // separator: ("ab","c") must not equal ("a","bc")
	for i := range len(subject) {
		h = (h ^ uint64(subject[i])) * fnvPrime
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return float64(h>>11) / (1 << 53)
}

// Allowance projects one rule onto the urgency inputs at now.
//
// The reset instant is where the care is. A provider-reported reset wins
// whenever there is one, because the provider's own window is what actually
// discards the remainder — exactly the precedence [Meter.Check] already applies
// when it reports when a cooled-down credential recovers.
//
// Without a provider report the answer depends on the window kind, and this is
// the part §7.5a(c) does not address:
//
//   - A calendar window (daily, weekly, monthly) has a boundary dorang itself
//     defines, in UTC. It is known.
//   - A rolling window has no reset at all — it is a ring that always ends now
//     — so declaring resets: true on one asserts that the provider tumbles a
//     window of that length without saying when. The phase is unknown, and
//     [Window.PeriodStart]'s epoch-aligned grid is an arbitrary guess rather
//     than knowledge. The allowance is returned with a zero ResetAt, which
//     scores zero.
//
// ok is false when the rule is not in this meter.
func (m *Meter) Allowance(now time.Time, r Rule) (Allowance, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, rr := range m.rules {
		if rr.Window == r.Window && rr.Metric == r.Metric {
			return m.allowanceLocked(rr, i, now), true
		}
	}
	return Allowance{}, false
}

// allowanceLocked builds one rule's allowance. The caller holds m.mu.
func (m *Meter) allowanceLocked(r Rule, i int, now time.Time) Allowance {
	used, _, _ := m.effectiveUsed(r, i, now)
	a := Allowance{Used: used, Limit: r.Limit, Resets: r.Resets}
	if !r.Resets {
		return a
	}

	length := r.Window.Duration()
	if r.Window.Kind() != KindRolling {
		a.Start, a.ResetAt = r.Window.periodBounds(now)
		length = a.ResetAt.Sub(a.Start)
	}
	if m.tracker != nil {
		if pr, ok := m.tracker.ResetAt(r.Window, r.Metric); ok && pr.After(now) {
			a.ResetAt = pr
			a.Start = pr.Add(-length)
		}
	}
	return a
}

// Urgency returns the meter's expiring-quota urgency at now, damped and
// jittered (DESIGN §7.5a(c)).
//
// A meter carries several rules and they do not all expire together, so the
// answer is the largest urgency among the rules that reset. §7.5a(c) specifies
// the signal for a single allowance and does not say how several compose; the
// maximum is what "how much paid-for capacity is about to be discarded" means,
// and the alternative reading — the minimum, on the grounds that the tightest
// rule bounds what can still be spent — describes admission, which is
// [Meter.Check]'s job, not ranking's.
//
// A meter that is standing aside scores zero whatever its allowances say: an
// allowance that cannot be spent before it expires has nothing at stake. This
// is a floor, not admission control; the router still filters on Check.
func (m *Meter) Urgency(now time.Time, in UrgencyInput) float64 {
	return Shape(m.rawUrgency(now), in)
}

// rawUrgency is the undamped, unjittered value.
func (m *Meter) rawUrgency(now time.Time) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Read the state without lapsing a finished cooldown: a query must not
	// mutate the meter, and Check is where a cooldown ends.
	if m.state == StateDisabled || (m.state == StateCooldown && now.Before(m.until)) {
		return 0
	}
	best := 0.0
	for i, r := range m.rules {
		if !r.Resets {
			continue
		}
		if u := m.allowanceLocked(r, i, now).Urgency(now); u > best {
			best = u
		}
	}
	return best
}

// RankerConfig configures a [Ranker].
type RankerConfig struct {
	// NodeID identifies this node. Two nodes with the same id jitter
	// identically, which is the one configuration mistake that reintroduces
	// the stampede this term exists to prevent.
	NodeID string
	// Jitter and Damping override the defaults; see [UrgencyInput].
	Jitter  float64
	Damping float64
}

// Ranker answers Urgency for a set of metered subjects.
//
// It is the shape routing wants — Urgency(subject) — with the node identity and
// the shaping constants fixed once instead of being repeated at every call. A
// subject the ranker has never heard of scores zero: an unmetered credential
// has no expiring allowance, so it has nothing at stake.
//
// A Ranker is safe for concurrent use.
type Ranker struct {
	cfg RankerConfig

	mu     sync.RWMutex
	meters map[string]*Meter
}

// NewRanker builds a ranker.
func NewRanker(cfg RankerConfig) *Ranker {
	return &Ranker{cfg: cfg, meters: map[string]*Meter{}}
}

// Track registers a subject's meter. A nil meter untracks the subject.
func (r *Ranker) Track(subject string, m *Meter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m == nil {
		delete(r.meters, subject)
		return
	}
	r.meters[subject] = m
}

// Untrack forgets a subject.
func (r *Ranker) Untrack(subject string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.meters, subject)
}

// Urgency ranks one subject at now, given its current occupancy in [0,1].
func (r *Ranker) Urgency(subject string, occupancy float64, now time.Time) float64 {
	r.mu.RLock()
	m := r.meters[subject]
	r.mu.RUnlock()
	if m == nil {
		return 0
	}
	return m.Urgency(now, UrgencyInput{
		Subject:   subject,
		NodeID:    r.cfg.NodeID,
		Occupancy: occupancy,
		Jitter:    r.cfg.Jitter,
		Damping:   r.cfg.Damping,
	})
}
