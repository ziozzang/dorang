package metrics

import (
	"github.com/ziozzang/dorang/internal/capacity"
)

// CapacitySource is the part of [capacity.Broker] this package needs. It is an
// interface so a test can drive the collector without building a broker, and so
// internal/capacity stays unaware that metrics exist.
type CapacitySource interface {
	Snapshot() capacity.Snapshot
}

// CapacityCollector renders the multi-axis reservation state of DESIGN §5.
//
// The `key` label is the one genuinely unbounded dimension in this package. A
// route, a provider group and a credential group are lines in a configuration
// file; the principal axis is keyed by an api-key id and the key axis by a
// credential id, both of which are rows in a table that an administrator can add
// to at will. So each axis is capped separately and the tail is folded, summed
// into a single `key="__overflow__"` series rather than dropped — an axis whose
// keys have overflowed still has a true total occupancy, and the total is the
// number that says whether the ceiling is being approached.
type CapacityCollector struct {
	src CapacitySource
	max int

	inflight folder

	// Render scratch. A collector belongs to one registry and [Registry.Gather]
	// serializes rendering, so this is reused rather than reallocated on every
	// scrape.
	keys  []string
	sets  [numCapacityAxes]foldingSet
	folds [numCapacityAxes]axisFold
}

const numCapacityAxes = int(capacity.AxisKey) + 1

// NewCapacityCollector builds the collector. maxKeysPerAxis of zero uses
// [DefaultMaxAxisSeries].
func NewCapacityCollector(src CapacitySource, maxKeysPerAxis int) *CapacityCollector {
	if maxKeysPerAxis <= 0 {
		maxKeysPerAxis = DefaultMaxAxisSeries
	}
	return &CapacityCollector{
		src:      src,
		max:      maxKeysPerAxis,
		inflight: folder{family: "dorang_capacity_inflight"},
	}
}

// CollectorName implements [Collector].
func (c *CapacityCollector) CollectorName() string { return "capacity" }

func (c *CapacityCollector) folders() []*folder { return []*folder{&c.inflight} }

// axisFold accumulates the folded tail of one axis.
type axisFold struct {
	inUse, limit, waiting, soft int
	present                     bool
	unlimited                   bool
}

// Collect implements [Collector].
func (c *CapacityCollector) Collect(w *Writer) {
	snap := c.src.Snapshot()

	// Snapshot returns the axes already sorted by (axis, key), which is what
	// makes the fold deterministic: the same occupancy folds the same way on
	// every scrape, so a series does not appear and vanish between two of them.
	for i := range c.sets {
		c.sets[i] = newFoldingSet(c.max, &c.inflight)
		c.folds[i] = axisFold{}
	}
	// keys[i] is the label the i-th axis state renders under, resolved once so
	// that the four families below agree on which states were folded.
	if cap(c.keys) < len(snap.Axes) {
		c.keys = make([]string, len(snap.Axes)+16)
	}
	keys := c.keys[:len(snap.Axes)]
	folds := &c.folds
	for i, a := range snap.Axes {
		ai := int(a.Axis)
		if ai < 0 || ai >= numCapacityAxes {
			keys[i] = a.Key
			continue
		}
		key, _ := c.sets[ai].admit(a.Key)
		keys[i] = key
		if key == OverflowSentinel {
			f := &folds[ai]
			f.present = true
			f.inUse += a.InUse
			f.waiting += a.Waiting
			if a.SoftReserved {
				f.soft++
			}
			// An unlimited axis (limit <= 0 is "not counted at all" in
			// internal/capacity) cannot be summed into a ceiling: adding a
			// finite limit to an absent one would invent a ceiling that does
			// not exist and make the fold look closer to full than it is.
			if a.Limit > 0 {
				f.limit += a.Limit
			} else {
				f.unlimited = true
			}
		}
	}

	w.Metric("dorang_capacity_inflight", Gauge,
		"Committed concurrency reservations on one axis key (DESIGN §5.1). A unit held "+
			"idle by a soft reservation is reported separately and is not counted here.")
	c.eachAxis(w, snap, keys, func(a capacity.AxisState) int64 { return int64(a.InUse) },
		func(f *axisFold) (int64, bool) { return int64(f.inUse), true })

	w.Metric("dorang_capacity_limit", Gauge,
		"Configured ceiling on one axis key. An axis with no ceiling is absent rather "+
			"than reported as zero: internal/capacity does not count an unlimited axis "+
			"at all, and a limit of 0 would read as 'admit nothing'.")
	c.eachAxis(w, snap, keys,
		func(a capacity.AxisState) int64 {
			if a.Limit <= 0 {
				return -1
			}
			return int64(a.Limit)
		},
		func(f *axisFold) (int64, bool) {
			if f.unlimited || f.limit <= 0 {
				return 0, false
			}
			return int64(f.limit), true
		})

	w.Metric("dorang_capacity_waiting", Gauge,
		"Blocked acquirers queued on one axis key (DESIGN §5.4).")
	c.eachAxis(w, snap, keys, func(a capacity.AxisState) int64 { return int64(a.Waiting) },
		func(f *axisFold) (int64, bool) { return int64(f.waiting), true })

	w.Metric("dorang_capacity_soft_reserved", Gauge,
		"1 when one unit of this axis key is held idle for a named multi-axis waiter "+
			"(DESIGN §5.4 correction 5). Summed over an axis it is the live throughput "+
			"cost of the starvation guard, in slots.")
	c.eachAxis(w, snap, keys,
		func(a capacity.AxisState) int64 {
			if a.SoftReserved {
				return 1
			}
			return 0
		},
		func(f *axisFold) (int64, bool) { return int64(f.soft), true })

	// The measured half of DESIGN §5.6's published-overshoot requirement. On a
	// correct single-node broker this is identically zero; a non-zero value
	// means a ceiling was exceeded on this node, which is a different fault
	// from the coordination overshoot a clustered mode publishes.
	var over int64
	for _, a := range snap.Axes {
		if a.Limit > 0 && a.InUse > a.Limit {
			if d := int64(a.InUse - a.Limit); d > over {
				over = d
			}
		}
	}
	w.Metric("dorang_capacity_overshoot_measured", Gauge,
		"Largest observed excess of committed reservations over a configured ceiling on "+
			"this node. The measured counterpart of dorang_coordination_max_overshoot, "+
			"which is the published bound (DESIGN §5.6).")
	w.Int(over)

	w.Metric("dorang_capacity_reservations", Gauge,
		"Live, unreleased reservations across every axis.")
	w.Int(int64(snap.Reservations))

	w.Metric("dorang_capacity_waiters", Gauge,
		"Distinct blocked Acquire calls. Not the sum of dorang_capacity_waiting: one "+
			"spilling waiter sits in several queues at once.")
	w.Int(int64(snap.Waiting))

	w.Metric("dorang_capacity_soft_reserved_keys", Gauge,
		"Axis keys currently holding a unit idle for a waiter.")
	w.Int(int64(snap.SoftReserved))

	w.Metric("dorang_capacity_grants_total", Counter, "Successful acquisitions.")
	w.Uint(snap.Grants)

	w.Metric("dorang_capacity_wakeups_total", Counter,
		"Waiters a release has probed. It grows by at most a constant per release; if it "+
			"starts tracking the number of waiters, targeted wakeup has regressed to a "+
			"broadcast (DESIGN §5.4).")
	w.Uint(snap.Wakeups)

	w.Metric("dorang_capacity_expired_total", Counter,
		"Reservations the sweeper reclaimed because their deadline passed.")
	w.Uint(snap.Expired)

	w.Metric("dorang_capacity_soft_reservations_total", Counter,
		"Soft reservations placed. Zero on a workload of single-axis requests, which "+
			"never need one.")
	w.Uint(snap.SoftReservations)
}

// eachAxis renders one family over every axis state, then the folded tail.
func (c *CapacityCollector) eachAxis(w *Writer, snap capacity.Snapshot, keys []string,
	val func(capacity.AxisState) int64, foldVal func(*axisFold) (int64, bool)) {

	for i, a := range snap.Axes {
		if keys[i] == OverflowSentinel {
			continue
		}
		v := val(a)
		if v < 0 {
			continue // the collector's "absent" signal
		}
		w.Label("axis", a.Axis.String())
		w.Label("key", a.Key)
		w.Int(v)
	}
	for _, ax := range capacityAxisOrder {
		f := &c.folds[int(ax)]
		if !f.present {
			continue
		}
		v, ok := foldVal(f)
		if !ok {
			continue
		}
		w.Label("axis", ax.String())
		w.Label("key", OverflowSentinel)
		w.Int(v)
	}
}

// capacityAxisOrder is DESIGN §5.7's check order, which is also the order
// Snapshot sorts by, so the folded series appear in the same place every time.
var capacityAxisOrder = []capacity.Axis{
	capacity.AxisGlobal, capacity.AxisPrincipal, capacity.AxisRoute,
	capacity.AxisProviderGroup, capacity.AxisModel, capacity.AxisCredentialGroup,
	capacity.AxisKey,
}
