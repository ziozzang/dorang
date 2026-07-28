package metrics

import (
	"slices"
	"time"

	"github.com/ziozzang/dorang/internal/cluster"
)

// LedgerSource is the part of [cluster.Ledger] this package needs.
type LedgerSource interface {
	Stats() []cluster.LedgerStat
	BlockSize() int64
	Draws() int64
}

// BudgetCollector renders the durable budget and quota counters of DESIGN §9.6.
//
// `dorang_budget_spent_ratio` is this node's reading, and the HELP text says so.
// The exact figure lives in the store and reading it is a round trip, which
// rule 4 forbids a scrape from taking; the reading here is the durable counter
// as of this node's last block draw, less what this node still holds unspent.
// It errs high — it assumes every other node has spent what it drew — which is
// the correct direction for a gauge whose purpose is to warn.
type BudgetCollector struct {
	src LedgerSource
	now func() time.Time
	max int
	f   folder
}

// NewBudgetCollector builds the collector.
func NewBudgetCollector(src LedgerSource, now func() time.Time, maxSubjects int) *BudgetCollector {
	if now == nil {
		now = time.Now
	}
	if maxSubjects <= 0 {
		maxSubjects = DefaultMaxSubjectSeries
	}
	return &BudgetCollector{src: src, now: now, max: maxSubjects,
		f: folder{family: "dorang_budget_spent_ratio"}}
}

// CollectorName implements [Collector].
func (c *BudgetCollector) CollectorName() string { return "budget" }

func (c *BudgetCollector) folders() []*folder { return []*folder{&c.f} }

// Collect implements [Collector].
func (c *BudgetCollector) Collect(w *Writer) {
	stats := c.src.Stats()
	now := c.now()

	// A spent ratio does not add up across subjects, so the tail past the cap
	// is dropped and counted rather than summed.
	if len(stats) > c.max {
		for range stats[c.max:] {
			c.f.fold()
		}
		stats = stats[:c.max]
	}

	w.Metric("dorang_budget_spent_ratio", Gauge,
		"Budget consumed over the subject's ceiling, in [0,1] (DESIGN §6.4). This node's "+
			"reading: the durable counter as of its last block draw, less what it still "+
			"holds unspent. A subject with no ceiling is absent — there is no ratio to a "+
			"budget nobody set.")
	for _, s := range stats {
		if s.Key.Kind != cluster.CounterBudget || s.Limit <= 0 {
			continue
		}
		w.Label("subject", subjectLabel(s.Key))
		w.Label("period", s.Key.Window.String())
		w.Float(clamp01(float64(s.Committed) / float64(s.Limit)))
	}

	w.Metric("dorang_budget_spent_nano", Gauge,
		"Budget consumed, in nano-USD, as this node knows it.")
	for _, s := range stats {
		if s.Key.Kind != cluster.CounterBudget {
			continue
		}
		w.Label("subject", subjectLabel(s.Key))
		w.Label("period", s.Key.Window.String())
		w.Int(s.Committed)
	}

	w.Metric("dorang_budget_limit_nano", Gauge,
		"The subject's ceiling in nano-USD. Absent when unbudgeted.")
	for _, s := range stats {
		if s.Key.Kind != cluster.CounterBudget || s.Limit <= 0 {
			continue
		}
		w.Label("subject", subjectLabel(s.Key))
		w.Label("period", s.Key.Window.String())
		w.Int(s.Limit)
	}

	w.Metric("dorang_budget_lease_remaining_nano", Gauge,
		"Units this node has drawn from the durable counter and not yet spent. This is "+
			"the budget that would be stranded if the node died now, and it is bounded "+
			"by the block size (DESIGN §9.6).")
	for _, s := range stats {
		if s.Key.Kind != cluster.CounterBudget {
			continue
		}
		w.Label("subject", subjectLabel(s.Key))
		w.Label("period", s.Key.Window.String())
		w.Int(s.Remaining)
	}

	w.Metric("dorang_ledger_lease_expires_seconds", Gauge,
		"Seconds until this node's lease on a counter lapses. Absent when it holds none.")
	for _, s := range stats {
		if s.ExpiresAt.IsZero() {
			continue
		}
		w.Label("counter", s.Key.Kind.String())
		w.Label("subject", subjectLabel(s.Key))
		w.Label("period", s.Key.Window.String())
		w.Float(s.ExpiresAt.Sub(now).Seconds())
	}

	w.Metric("dorang_ledger_block_size", Gauge,
		"Units a node draws at a time. It is the knob of DESIGN §9.6: larger means fewer "+
			"store writes and a larger published overshoot, and it is the multiplicand of "+
			"dorang_coordination_max_overshoot under the leased mode.")
	w.Int(c.src.BlockSize())

	w.Metric("dorang_ledger_draws_total", Counter,
		"Store round trips taken to refill a block. Divided into the request count it is "+
			"the measured 'one write per block, not per request' of DESIGN §9.6.")
	w.Int(c.src.Draws())
}

func subjectLabel(k cluster.CounterKey) string {
	if k.ID == "" {
		return k.Scope
	}
	return k.Scope + ":" + k.ID
}

// ClusterCollector renders the coordination accuracy of DESIGN §5.6 and §6.3.
//
// The published maximum overshoot is the headline: "Every mode publishes its
// maximum possible overshoot as a number. 'Approximately accurate' is not an
// acceptable specification." A figure that lives only in a design document is
// not published; this is where it becomes a number an operator can alert on,
// next to `dorang_capacity_overshoot_measured`, which is what was actually
// observed.
type ClusterCollector struct {
	// Enabled mirrors cluster.enabled.
	Enabled bool
	// NodeID names this node.
	NodeID string
	// Mode is the configured coordination mode.
	Mode cluster.Mode
	// Params describe the deployment the published figures are about.
	//
	// Params.Nodes is the fallback, used when Nodes is nil. It is a
	// configured or assumed count; every overshoot figure here is
	// proportional to it, so a wrong one is a wrong bound rather than a
	// missing one.
	Params cluster.Params
	// Nodes reports how many nodes are alive right now. Nil keeps
	// Params.Nodes, which is what an unclustered process publishes because
	// there it is not an assumption — there is one node.
	//
	// It exists because every figure below is `something × (nodes − 1)`: at a
	// hardcoded 1 they are all exactly 0, and a bound of "exact" published by
	// a four-node cluster is not a conservative error, it is the wrong number
	// in the direction an operator sizes against.
	Nodes func() int
	// Unavailable are modes this deployment cannot switch to, whatever their
	// arithmetic says, with the reason. They are published as refused rather
	// than dropped, because the comparison set exists so that the cost of a
	// different choice is visible without making it — and a mode listed as
	// exact and cheap that the loader will reject is the most expensive kind
	// of visible.
	//
	// cluster.PublishAll deliberately does not know about this. It answers for
	// the arithmetic of a mode, which is a property of the mode; whether this
	// build can select it from a configuration file is a property of the
	// build, and a Go embedder that supplies its own client is not subject to
	// it.
	Unavailable map[cluster.Mode]error
	// Leader reports whether this node currently holds leadership; nil when
	// there is no election (the unclustered case), in which case the gauge is
	// absent rather than 0 — an unclustered process is not "not the leader".
	Leader func() bool
	// Term reports the election term; nil omits it.
	Term func() uint64
}

// params is the deployment description this scrape publishes against, with the
// live node count substituted when one is available.
func (c *ClusterCollector) params() cluster.Params {
	p := c.Params
	if c.Nodes != nil {
		if n := c.Nodes(); n > 0 {
			p.Nodes = n
		}
	}
	return p
}

// CollectorName implements [Collector].
func (c *ClusterCollector) CollectorName() string { return "cluster" }

// Collect implements [Collector].
func (c *ClusterCollector) Collect(w *Writer) {
	p := c.params()

	w.Metric("dorang_cluster_enabled", Gauge,
		"1 when this process runs as part of a cluster (DESIGN §13).")
	w.Bool(c.Enabled)

	w.Metric("dorang_cluster_expected_nodes", Gauge,
		"Nodes the published overshoot figures are computed for. In a cluster this is "+
			"the live registry count, so the figures below describe the deployment that "+
			"exists rather than the one the configuration imagined.")
	w.Int(int64(max(p.Nodes, 1)))

	if c.Leader != nil {
		w.Metric("dorang_cluster_is_leader", Gauge,
			"1 when this node holds leadership. Absent when there is no election at all, "+
				"which is not the same fact as losing one.")
		w.Bool(c.Leader())
	}
	if c.Term != nil {
		w.Metric("dorang_cluster_term", Gauge, "Current election term.")
		w.Uint(c.Term())
	}

	w.Metric("dorang_coordination_mode", Gauge,
		"1 against the configured capacity and quota coordination mode (DESIGN §5.6). "+
			"Quota and concurrency share one accuracy vocabulary (§6.3), so there is one "+
			"mode and not two.")
	for _, m := range cluster.Modes() {
		w.Label("mode", m.String())
		w.Bool(m == c.Mode)
	}

	// Every mode, not only the configured one. An operator choosing between
	// them is entitled to see what each would cost before switching, and a mode
	// that cannot serve this deployment says so by being absent from the
	// published set and present in the refused one.
	ok, refused := cluster.PublishAll(p)
	for m, err := range c.Unavailable {
		if _, already := refused[m]; already {
			continue
		}
		refused[m] = err
		ok = slices.DeleteFunc(ok, func(a cluster.Accuracy) bool { return a.Mode == m })
	}

	w.Metric("dorang_coordination_max_overshoot", Gauge,
		"Maximum units all nodes together can admit beyond a ceiling, per mode "+
			"(DESIGN §5.6). Zero means exact. Published for every mode that can serve "+
			"this deployment, so the cost of a different choice is visible without "+
			"making it. The measured counterpart is dorang_capacity_overshoot_measured.")
	for _, a := range ok {
		w.Label("mode", a.Mode.String())
		w.Label("configured", boolLabel(a.Mode == c.Mode))
		w.Int(a.MaxOvershoot)
	}

	w.Metric("dorang_coordination_mode_refused", Gauge,
		"1 for a mode that cannot coordinate this deployment's limits at all — `local` "+
			"under cluster.enabled, or `leased` below cluster.min_leasable. DESIGN §5.6 "+
			"makes these refusals rather than warnings: silently exceeding a plan limit "+
			"produces upstream 429s that cascade into the fallback chain and surface far "+
			"from their cause.")
	for _, m := range cluster.Modes() {
		_, bad := refused[m]
		w.Label("mode", m.String())
		w.Bool(bad)
	}

	w.Metric("dorang_coordination_limit", Gauge,
		"The ceiling the published overshoot figures are computed against.")
	w.Int(p.Limit)
}

func boolLabel(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

var _ LedgerSource = (*cluster.Ledger)(nil)
