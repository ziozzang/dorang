package capacity

// Soft reservations: the implementation of the protocol documented in doc.go.
//
// A claim is one unit of one axis key set aside for one waiter. The only state
// it needs is bucket.claimant, plus the unit it adds to bucket.inUse so that
// every other request sees it as occupancy without testing for it. Which buckets
// a waiter holds is not stored: it is a prefix of the claim candidate's axes,
// and that need set is a pure function of the request and the configuration,
// both immutable, so it is recomputed on the rare path that gives claims back.
// Everything here is mutated only under Broker.mu.

// noClaimant stands in for "this attempt is not being made on behalf of a
// queued waiter". A sentinel rather than nil, so that the per-axis claim test in
// checkLocked is a single pointer compare with no nil guard: that test sits in
// the innermost loop of every acquisition. It is never stored as a bucket's
// claimant, so it can never match one.
var noClaimant = new(waiter)

// SoftReservationMode selects the multi-axis starvation guard of DESIGN §5.4
// correction 5 (open risk W8).
//
// The guard costs at most one idled unit per axis key, and only while a
// multi-axis waiter is being escorted in, so it is on by default. It is a knob
// rather than a constant because the worst case — two axes of limit 1, where
// the claimed axis idles for a whole release interval — is a real trade, and an
// operator running only single-axis limits gains nothing from paying it.
type SoftReservationMode uint8

const (
	// SoftReservationsDefault is the zero value and selects the current
	// default, which is on.
	SoftReservationsDefault SoftReservationMode = iota
	// SoftReservationsOn guarantees that an interactive multi-axis waiter is
	// served within a bounded number of releases once it is the oldest waiter.
	SoftReservationsOn
	// SoftReservationsOff restores the pre-W8 behaviour: no axis is ever idled
	// for a waiter, and a multi-axis waiter can ping-pong indefinitely while
	// every axis it needs stays saturated.
	SoftReservationsOff
)

// String implements fmt.Stringer.
func (m SoftReservationMode) String() string {
	switch m {
	case SoftReservationsOn, SoftReservationsDefault:
		return "on"
	case SoftReservationsOff:
		return "off"
	}
	return "unknown"
}

// enabled resolves the tri-state against the default.
func (m SoftReservationMode) enabled() bool { return m != SoftReservationsOff }

// DefaultSoftReserveAfter is how many probes a waiter must fail before it
// starts setting capacity aside for itself. See Config.SoftReserveAfter.
//
// A soft reservation is a remedy for a demonstrated failure to make progress,
// and it is worth applying only once the failure has been demonstrated. Most
// blocked waiters are served on their first or second probe and never idle
// anything; the ones that keep bouncing are exactly the ones W8 is about.
//
// Measured on BenchmarkAcquireContendedMixed, which is the workload W8
// describes — a minority of two-axis requests against a majority of single-axis
// ones on each axis, both axes saturated — as ns/op against the same benchmark
// with the guard off. Two sweeps on a thermally noisy laptop, so the shape is
// the finding and the digits are not:
//
//	arm after 1 bounce   +12.6%   +10.8%
//	arm after 2 bounces   +6.0%    +7.3%
//	arm after 4 bounces   +3.1%    +7.3%   <- default
//	arm after 8 bounces   +0.8%    +2.7%
//
// A longer run pinned at the default measured +5.3% (median and best-of-ten
// agree to within a quarter of a point), and that is the number to quote.
//
// The threshold costs only a constant on the liveness bound: an oldest waiter
// is served within SoftReserveAfter + (axes it needs) releases of its blocking
// axes, so 4 + 7 = 11 in the worst case rather than 7. Lower it on a system
// whose axes release slowly, where eleven releases is a long time; raise it on
// one that is throughput-bound and whose multi-axis traffic is rare.
const DefaultSoftReserveAfter = 4

// claimableLocked reports whether w may take a soft reservation on the bucket
// holding n.
//
// The ceiling used is the raw limit, not the batch-adjusted one, and that is
// correct for every waiter that reaches here: extendClaimsLocked turns batch
// work away whenever a reserve is configured, and with no reserve the two
// ceilings are the same. It is what makes inUse <= limit — the invariant that
// makes a claim a guarantee — hold against interactive traffic too.
func (b *Broker) claimableLocked(bk *bucket, n axisNeed, w *waiter) bool {
	if n.limit <= 0 {
		return false // unlimited axes are not counted and need no claim
	}
	if bk == nil {
		return true // nothing has ever committed here: wide open
	}
	if bk.claimant != nil {
		return bk.claimant == w
	}
	// inUse counts committed reservations and any claim, so this is the whole
	// occupancy test.
	if bk.inUse >= n.limit {
		return false // no free unit to set aside
	}
	// Never take a unit out from under a waiter that has been queued here
	// longer: a claim must not become a way to overtake, which is the property
	// aging exists to provide.
	if h := bk.q.head(); h != nil && h != w && h.seq < w.seq {
		return false
	}
	return true
}

// extendClaimsLocked grows w's claim prefix as far as it currently reaches and
// reports whether it claimed the bucket `on`, which is the bucket whose release
// triggered the probe.
//
// Claims are taken over the axes of the claim candidate — attempt-order
// candidate zero, the only candidate an OnCapacity == Wait request will ever
// use — in axis order, stopping at the first axis that cannot be claimed. The
// prefix discipline is what makes the wait-for graph acyclic; see doc.go.
//
// It must be called with w already removed from every queue, so that w's own
// queue membership cannot make its axes look contested to itself.
func (b *Broker) extendClaimsLocked(w *waiter, on *bucket) bool {
	// Batch work is excluded only because the interactive reserve gives it a
	// lower ceiling than the traffic it would be setting a unit aside against,
	// which a claim cannot bridge (doc.go). With no reserve configured there is
	// no such gap and batch is entitled to the guarantee like anything else.
	if !b.soft || w.bounces < int32(b.softAfter) || (w.req.Batch && b.reserve > 0) {
		return false
	}
	c, ok := claimCandidate(&w.req)
	if !ok {
		return false
	}

	var needBuf [numAxes]axisNeed
	needs := b.needs(&w.req, c, needBuf[:0])

	claimedOn := false
	for _, n := range needs {
		bk := b.buckets[n.key]
		if bk != nil && bk.claimant == w {
			continue // already ours; the prefix runs on
		}
		if !b.claimableLocked(bk, n, w) {
			break
		}
		if bk == nil {
			bk = &bucket{limit: n.limit}
			b.buckets[n.key] = bk
		}
		bk.claimant = w
		bk.inUse++ // the unit is occupied from everyone else's point of view
		w.claimed = true
		b.softPlaced++
		if bk == on {
			claimedOn = true
		}
	}
	return claimedOn
}

// claimCandidate is the candidate a waiter takes its soft reservations over:
// attempt-order candidate zero, which under OnCapacity == Wait is the only
// candidate it will ever use. It is a pure function of the request, which is
// immutable once a waiter exists, so the claim set can be recomputed instead of
// stored.
func claimCandidate(req *Request) (*Candidate, bool) {
	c := candidateAt(req, 0)
	if c == nil && len(req.Candidates) > 0 {
		return nil, false
	}
	return c, true
}

// releaseClaimsLocked gives back every claim w still holds and re-offers the
// freed capacity to the queues of those axes.
//
// The re-offer is not optional. A claim is capacity that is being kept idle on
// purpose; if a cancelled waiter simply dropped it, the unit would stay idle
// until some unrelated release happened to serve that bucket, which is a lost
// wakeup with no upper bound on its duration.
//
// It only queues the buckets. Serving them is the caller's job, through
// drainPendingLocked, because this runs from inside serveLocked as often as not.
func (b *Broker) releaseClaimsLocked(w *waiter) {
	if !w.claimed {
		return
	}
	w.claimed = false
	c, ok := claimCandidate(&w.req)
	if !ok {
		return
	}
	var needBuf [numAxes]axisNeed
	needs := b.needs(&w.req, c, needBuf[:0])

	for _, n := range needs {
		bk := b.buckets[n.key]
		if bk == nil || bk.claimant != w {
			continue // never claimed, or already consumed by a commit
		}
		bk.claimant = nil
		bk.inUse-- // the unit stops being occupied on w's behalf
		b.pending = append(b.pending, bk)
	}
}

// softReservedLocked is the number of outstanding claims. It is O(buckets) and
// exists for Snapshot only.
func (b *Broker) softReservedLocked() int {
	n := 0
	for _, bk := range b.buckets {
		if bk.claimant != nil {
			n++
		}
	}
	return n
}
