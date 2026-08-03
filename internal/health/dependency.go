package health

import (
	"context"
	"sync"
	"time"
)

// Defaults for the zero fields of [DependencyOptions].
const (
	// DefaultMinOutage is how long a dependency must have been failing before
	// the node stops accepting new work.
	//
	// Fifteen seconds, and the number is chosen from both directions.
	//
	// From below: a stopped PostgreSQL that comes back is not an outage this
	// node has to be drained through. Measured against a live deployment, the
	// process reconnected transparently, its restart count stayed zero, and the
	// metering buffered during the gap was written when the store returned —
	// so a restart, a failover or a connection-pool reset costs seconds and
	// costs them once. Draining a node through that is a self-inflicted
	// capacity loss, and when every node shares one store it is a self-inflicted
	// OUTAGE: all of them go unready together for a blip that none of them
	// needed to be removed for. Fifteen seconds is longer than any of those and
	// several times the two-second blip that must not move this needle.
	//
	// From above: the signal is worthless if it arrives after the incident. A
	// readiness probe is what acts on it — deploy/kubernetes.yaml polls every
	// 2s with a failure threshold of 2 — so the node leaves rotation about five
	// seconds after the verdict flips, and the whole path from "the store went
	// away" to "the balancer stopped routing here" is about twenty seconds.
	DefaultMinOutage = 15 * time.Second
	// DefaultMinFailures is how many consultations must have failed inside that
	// window. It exists so that ONE failed query spanning a long timeout cannot
	// on its own be a sustained outage: a single call that hangs for the store
	// timeout and then fails has produced one data point, not a verdict.
	DefaultMinFailures = 3
	// DefaultStaleFactor multiplies MinOutage to give the age past which the
	// newest sample stops counting as evidence.
	//
	// A verdict nobody is refreshing is not a measurement. It matters because
	// unreadiness is self-reinforcing when the samples come from traffic rather
	// than from a prober: the balancer stops routing here, the node stops
	// making the calls that would prove the store is back, and it would stay
	// out of rotation forever on evidence that has stopped being collected.
	// Going ready on stale evidence risks one more round of refused requests;
	// not going ready risks never returning at all.
	DefaultStaleFactor = 4
)

// DependencyOptions tunes a [Dependency].
type DependencyOptions struct {
	// Name is the fixed key this dependency reports under, such as "store".
	// Required: an unnamed dependency cannot be reported.
	Name string
	// MinOutage is how long the unbroken run of failures must span before the
	// dependency reports itself unable to serve new work. Zero uses
	// [DefaultMinOutage].
	MinOutage time.Duration
	// MinFailures is how many failures that run must contain. Zero uses
	// [DefaultMinFailures].
	MinFailures int
	// StaleAfter is how old the newest sample may be and still count. Zero uses
	// MinOutage x [DefaultStaleFactor].
	StaleAfter time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

func (o *DependencyOptions) setDefaults() {
	if o.MinOutage <= 0 {
		o.MinOutage = DefaultMinOutage
	}
	if o.MinFailures <= 0 {
		o.MinFailures = DefaultMinFailures
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = time.Duration(DefaultStaleFactor) * o.MinOutage
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Dependency answers one question about a backing service the whole process
// shares: has it been unable to answer for long enough that this node should
// stop being sent new work?
//
// It is the other half of what [Tracker] does. The tracker decides whether a
// DEPLOYMENT is worth the next request; this decides whether the NODE is, and
// the two are different because a node that cannot reach its credential store
// has nothing wrong with any of its upstreams. Readiness is the signal that
// decides whether new work should arrive (DESIGN §13), and a node whose store
// is unreachable is exactly a node that cannot take any: cached principals keep
// serving, so the failure is invisible in the aggregate, while every caller
// whose key this node has not recently seen gets a 503 from a balancer that
// still believes in it.
//
// The judgement is deliberately asymmetric.
//
//   - Slow to refuse. A failure has to be sustained: [DependencyOptions.MinOutage]
//     of wall time AND [DependencyOptions.MinFailures] consultations, with no
//     success in between. "A query failed" is not "cannot serve new work", and
//     the difference is the whole point — a store that comes back on its own is
//     the common case and draining through it costs capacity for nothing.
//   - Instant to return. One success clears the verdict. The recovery this is
//     modelled on needed no restart and no intervention: the process reconnected,
//     served, and flushed what it had buffered. Readiness must not be the thing
//     that keeps a node out of rotation after it can serve again.
//
// Samples must keep arriving. A caller that probes on a fixed cadence satisfies
// that by construction; a caller that samples from the request path does not,
// once the balancer has drained the node — which is what
// [DependencyOptions.StaleAfter] is for.
//
// A Dependency is safe for concurrent use. It performs no I/O and never blocks
// on anything but its own mutex, so [Dependency.ReadyForWork] is safe to call
// from a health handler.
type Dependency struct {
	opts DependencyOptions

	mu sync.Mutex
	// The current unbroken run of failures. fails is zero when there is none.
	fails int
	first time.Time
	last  time.Time
	// outages counts how many times the verdict has been reached, for the
	// operator's benefit: a store that trips this once a day is a different
	// problem from one that trips it once a quarter.
	outages uint64
	// unready latches the current verdict so outages is incremented once per
	// outage rather than once per sample taken while it holds.
	unready bool
}

// NewDependency builds a Dependency. Name is required.
func NewDependency(opts DependencyOptions) *Dependency {
	opts.setDefaults()
	return &Dependency{opts: opts}
}

// GateName is the fixed key this dependency reports under.
func (d *Dependency) GateName() string { return d.opts.Name }

// Fail records one consultation the dependency could not answer.
//
// It means "the store was asked and did not answer" — a dial failure, a
// timeout, a dead connection. It does NOT mean the answer was unwelcome: a
// lookup that correctly returns "no such key", and a refusal the gateway made
// without consulting the store at all, are both successful consultations as far
// as this is concerned. Counting those here would let an unknown-key flood, or
// any burst of ordinary 401s, drain the fleet from outside.
func (d *Dependency) Fail() {
	now := d.opts.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	// A failure that arrives long after the previous one is not more evidence
	// of the same outage; it starts a new run. Without this, two unrelated
	// blips an hour apart would add up to a verdict neither of them earned.
	if d.fails > 0 && now.Sub(d.last) > d.opts.StaleAfter {
		d.reset()
	}
	if d.fails == 0 {
		d.first = now
	}
	d.fails++
	d.last = now
	if !d.unready && d.verdictLocked(now) {
		d.unready = true
		d.outages++
	}
}

// OK records one consultation the dependency answered, and clears any verdict.
func (d *Dependency) OK() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reset()
}

// Observe is Fail or OK depending on err, for a caller that has one in hand.
func (d *Dependency) Observe(err error) {
	if err != nil {
		d.Fail()
		return
	}
	d.OK()
}

// Watch probes the dependency on a fixed cadence until ctx is done, feeding
// each outcome to [Dependency.Observe]. It blocks; run it in a goroutine.
//
// A prober rather than the request path, and the difference is the reason this
// exists as a loop rather than as advice. Samples taken from traffic stop
// arriving exactly when they are most needed: the balancer acts on the verdict,
// the node stops making the calls that would prove the dependency is back, and
// the node is left deciding its own readiness from evidence nobody is
// collecting. A prober keeps sampling whether or not anyone is routing here,
// which is what makes recovery a property of the mechanism rather than a hope.
//
// probe is what an unseen credential would have to do — one real call against
// the dependency, not a liveness ping. A connection pool that dials happily
// while the query it is asked for fails is a store that cannot authenticate
// anyone, and a ping would call it healthy.
//
// Each probe is bounded by every, because a probe that cannot answer inside its
// own cadence has failed for this purpose whatever it eventually returns —
// and because an unbounded probe would let one hung call stall the loop that is
// supposed to be watching for hung calls.
func (d *Dependency) Watch(ctx context.Context, every time.Duration, probe func(context.Context) error) {
	if every <= 0 {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		// Probe first, then wait: a node whose dependency is already gone should
		// not spend a whole tick believing otherwise.
		c, cancel := context.WithTimeout(ctx, every)
		err := probe(c)
		cancel()
		if ctx.Err() != nil {
			// The process is shutting down. A cancelled probe is not evidence
			// about the dependency, and recording it would leave the last thing
			// in the counters looking like an outage.
			return
		}
		d.Observe(err)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *Dependency) reset() {
	d.fails = 0
	d.first = time.Time{}
	d.last = time.Time{}
	d.unready = false
}

// ReadyForWork reports whether this node may be sent new work, and a fixed
// reason when it may not.
func (d *Dependency) ReadyForWork() (bool, string) {
	now := d.opts.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.verdictLocked(now) {
		d.unready = false
		return true, ""
	}
	return false, "unreachable"
}

// verdictLocked reports whether the current run of failures is a sustained
// outage. d.mu must be held.
func (d *Dependency) verdictLocked(now time.Time) bool {
	switch {
	case d.fails < d.opts.MinFailures:
		return false
	case d.last.Sub(d.first) < d.opts.MinOutage:
		return false
	case now.Sub(d.last) > d.opts.StaleAfter:
		// Nobody is sampling any more. See DefaultStaleFactor.
		return false
	}
	return true
}

// DependencyStats is a point-in-time view, for metrics and for an operator
// asking how bad it is rather than whether it is bad.
type DependencyStats struct {
	// Name is the dependency's fixed key.
	Name string
	// Ready is the current verdict.
	Ready bool
	// Failures is the length of the current unbroken run, zero when there is
	// none.
	Failures int
	// Outage is how long that run has been going, zero when there is none.
	Outage time.Duration
	// Outages counts how many times the verdict has been reached since start.
	Outages uint64
}

// Stats reports the dependency's counters.
func (d *Dependency) Stats() DependencyStats {
	now := d.opts.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	st := DependencyStats{
		Name:     d.opts.Name,
		Ready:    !d.verdictLocked(now),
		Failures: d.fails,
		Outages:  d.outages,
	}
	if d.fails > 0 {
		st.Outage = d.last.Sub(d.first)
	}
	return st
}
