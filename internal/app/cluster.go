package app

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// buildNode assembles this process's cluster membership (DESIGN §13).
//
// One node per process, owning one ledger. Before this existed internal/app
// built a [cluster.Ledger], a [cluster.KeyInvalidator], a [cluster.KeyControl]
// and a [cluster.KeyLoader] directly and never called [cluster.New], so the
// shipped binary had no node: nothing wrote or read `nodes`, no election ran,
// and none of the leader jobs — partition maintenance, the reservation sweep of
// §5.3 and §6.4, and lease reclaim — ever ran. Requirement R14 is "two or more
// nodes, highly available"; the lease reclaim is the part of it that costs
// money, because a node that dies holding quota leases never returns them and
// the only recovery is manual.
func (a *App) buildNode(cfg *config.Config, st *store.Store) error {
	mode, err := cluster.ParseMode(cfg.Cluster.CapacityMode)
	if err != nil {
		return fmt.Errorf("app: cluster.capacity_mode: %w", err)
	}

	block := a.opts.BudgetBlockNanoUSD
	if block <= 0 {
		block = DefaultBudgetBlockNanoUSD
	}

	node, err := cluster.New(cluster.Config{
		Enabled: cfg.Cluster.Enabled,
		NodeID:  nodeID(cfg),
		Address: cfg.Server.Listen,
		Version: Version,
		Mode:    mode,
		Store:   st,
		// Block is the ledger's draw, in nano-USD, because the counter this
		// gateway puts on the request path is a budget. LeaseBlock is left at
		// internal/cluster's default because it is denominated in the metric's
		// own units — concurrency slots — and the two are not interchangeable.
		// Handing the budget block to both would publish a §5.6 overshoot of
		// "50000000 × (nodes − 1)" against a concurrency ceiling.
		BlockSize:   block,
		MinLeasable: int64(cfg.Cluster.MinLeasable),
		LeaseTTL:    a.opts.ClusterLeaseTTL,
		Tick:        a.opts.ClusterTick,
		NodeTTL:     a.opts.ClusterNodeTTL,
		// The capacity broker's reservation sweep (§5.3). It is passed as a
		// function so that internal/cluster does not import internal/capacity
		// for one method. Without a leader running this, a panic or a leaked
		// goroutine consumes capacity until the process restarts.
		SweepCapacity: a.Broker.Sweep,
		// Retention is the zero policy — keep everything — because nothing in
		// the configuration file sets one yet. The job still runs: its other
		// half pre-creates tomorrow's partitions, and §9.5 is explicit that
		// scheduling that late fails every insert at the first midnight.
		Retention: store.RetentionPolicy{},
		Now:       a.now,
		Logf:      a.logf,
	})
	if err != nil {
		return fmt.Errorf("app: cluster node: %w", err)
	}
	a.Node = node
	a.Ledger = node.Ledger()

	// The coordinator for the configured mode. It is built from the node so
	// that the mode, the node id and the shared backend are the node's and
	// cannot disagree with the registry the accuracy figure is computed from.
	//
	// A mode this build cannot honour has already been refused by
	// internal/config; cluster.Node refuses it again, and this reports rather
	// than swallows, because a coordinator that failed to build and was
	// ignored is the shape of defect this whole change exists to remove.
	//
	// LeaseTTL is deliberately left at quota's default rather than derived from
	// anything here, and it is worth knowing what that default means before it
	// carries traffic. The shared modes hold spend as lease rows, and a row
	// that expires returns its units — so a quota key that goes uncharged for
	// quota.DefaultLeaseTTL has its counter forgotten, whatever window the key
	// names. A daily ceiling therefore resets after half a minute of quiet
	// rather than at midnight. That is a property of using a lease as a
	// counter, it predates this wiring, and fixing it means keying the TTL off
	// Key.Window inside internal/quota rather than passing a different number
	// from here. It costs nothing today because the request path does not yet
	// route quota through this coordinator; it must be closed before it does.
	//
	// The same fact has a second consequence, and this comment used to be the
	// only place it was written down: a lease that lapses under a holder still
	// using its units is what breaks the "overshoot 0" §5.6 publishes for the
	// shared modes. A qualifier on a published bound that lives in the caller's
	// wiring is a qualifier nobody reads. It is now on the figure itself
	// (cluster.Accuracy.Holds and .PerLapse), so it travels into the start-up
	// log below and into /metrics with the number it qualifies.
	coord, err := node.Coordinator(quota.CoordinatorConfig{})
	if err != nil {
		return fmt.Errorf("app: cluster.capacity_mode %q: %w", cfg.Cluster.CapacityMode, err)
	}
	a.Coordinator = coord
	return nil
}

// liveNodes caches the registry count that DESIGN §5.6's published overshoot is
// computed against.
//
// It is a cache and not a query because /metrics must not take a store round
// trip per scrape, and it is refreshed on the node's own heartbeat interval —
// the same tick that keeps this node's row alive — so the staleness is bounded
// by the interval that already decides whether a peer is considered live at
// all. Zero means "not clustered": one node, no query, no refresh.
type liveNodes struct{ n atomic.Int64 }

func (l *liveNodes) get() int {
	if v := l.n.Load(); v > 0 {
		return int(v)
	}
	return 1
}

func (l *liveNodes) set(n int) {
	if n > 0 {
		l.n.Store(int64(n))
	}
}

// joinCluster registers this node and starts its loop, and reports whether it
// did.
//
// This is where `cluster.enabled` is honoured. DESIGN §0.2 promises a single
// binary with no required dependencies, and a notebook must not pay for a layer
// about coordinating with peers it does not have: with clustering off nothing
// here starts a goroutine, writes a registry row, campaigns for a leadership
// lease, or adds a query to any path. The cost of the node layer to a
// single-node gateway is the [cluster.New] call above and one branch here.
//
// A failed join is FATAL, and that is the whole of the second return value.
// This used to log the error and report false, and a logged error returned as a
// boolean is how [cluster.ErrDuplicateNodeID] — a condition internal/cluster
// detects, names, demotes for and exports [cluster.Node.Conflict] to escalate —
// reached production as a line in a log file. See [App.startBackground] for what
// the two failure modes cost.
func (a *App) joinCluster(ctx context.Context) (bool, error) {
	if a.Node == nil || !a.Node.Enabled() {
		return false, nil
	}
	if err := a.Node.Start(ctx); err != nil {
		return false, a.joinError(err)
	}
	a.refreshAccuracy(ctx)

	// §5.6 requires the maximum overshoot to be published as a number. It is
	// published continuously through dorang_coordination_max_overshoot; it is
	// logged once here as well, because an operator sizing a fleet reads the
	// number at start-up and the log is what survives into an incident review.
	// A mode that cannot serve this deployment's ceilings says so in the same
	// place rather than being discovered by its absence from the scrape.
	limit := widestCapacityLimit(a.cfg.Load())
	acc, err := a.Node.Accuracy(ctx, limit)
	if err != nil {
		a.logf("app: cluster: no published accuracy for capacity_mode %s against a limit of %d: %v",
			a.Node.Mode(), limit, err)
		return true, nil
	}
	a.logf("app: cluster: node %s, %s", a.Node.ID(), acc)
	return true, nil
}

// joinError turns a refused join into a start-up failure, and says which of the
// two it is.
//
// Both are fatal and they are fatal for different reasons, so they are two
// messages rather than one:
//
//   - A duplicate id is a deployment mistake with a cost DESIGN §13 and
//     [cluster.ErrDuplicateNodeID] set out in full. Refusing to LEAD is not
//     enough, which is the whole point of escalating it here: the node id also
//     keys this process's row in `nodes`, its share of every leased limit in
//     `quota_leases`, and the budget draw in [App.Ledger] — which this assembly
//     puts on the request path. A process that started and merely declined to
//     lead would go on drawing budget from the incumbent's lease block and
//     taking its slice of every ceiling, under an identity the cluster
//     attributes to somebody else.
//   - Anything else — a store that would not answer the registry write, a node
//     already started — leaves the node NEVER joined, because
//     [cluster.Node.Start] is called exactly once and its loop is what would
//     retry. `cluster.enabled: true` would then be a gateway that runs no leader
//     job (no lease reclaim, no reservation sweep, no partition maintenance) and
//     publishes §5.6's overshoot against a node count of one — "exact" — from a
//     node that is in a cluster and is not counting itself in it. That is the
//     one answer 5.6 exists to prevent, and an operator sizes against it.
//
// Refusing to start is also the loud failure DESIGN §9.2 asks for here: a
// process that will not come up halts a rollout and shows up in every
// orchestrator's own alerting, and an orchestrator restarting it IS the retry.
func (a *App) joinError(err error) error {
	if errors.Is(err, cluster.ErrDuplicateNodeID) {
		return fmt.Errorf("app: cluster.node_id %q is already in use by a running process; "+
			"this one refuses to start rather than draw budget and quota under an identity "+
			"the cluster attributes to somebody else (DESIGN §13): %w", a.Node.ID(), err)
	}
	return fmt.Errorf("app: cluster.enabled is true and node %q could not join, so it would "+
		"run no leader job and publish an overshoot computed from a node count of one; "+
		"refusing to start: %w", a.Node.ID(), err)
}

// noteConflict takes this process out of the load balancer's rotation once its
// node id turns out to belong to somebody else.
//
// It is the half of DESIGN §13's identity check that start-up cannot reach.
// [App.joinError] covers the process that arrives second; this covers the
// process that was already running, was frozen past the node TTL — a long GC
// pause, a suspended container, a paused debugger — and was legitimately
// superseded. It learns that from its own heartbeat row, is demoted by
// internal/cluster, and then never leads again. Nothing re-checks the join, so
// without this it would keep serving under a contested identity forever.
//
// Readiness is the right signal and the only one that is. The registry row says
// what the cluster believes and nothing routes on it (see cluster.Node.Close);
// what a load balancer polls is /health/readiness, and "this node will never
// lead again and is still taking traffic under an id another process owns" is
// exactly what it should be told. The process is deliberately NOT killed: it is
// serving requests correctly, and an operator draining it is a better ending
// than an abrupt exit mid-stream.
//
// [server.Server.StartDrain] is the lever, because it flips readiness and
// touches nothing else — the listener stays open, the route table is untouched,
// and requests in flight are served in full. What it does not give is a distinct
// word for the state, so /health reports `draining` with the reason beside it in
// the `cluster` object [clusterHealth] contributes.
func (a *App) noteConflict() {
	if a.Node == nil {
		return
	}
	err := a.Node.Conflict()
	if err == nil || !a.disqualified.CompareAndSwap(false, true) {
		return
	}
	a.logf("app: cluster: node %s has been disqualified and will never lead again; "+
		"readiness is now false so the load balancer stops routing here. This process is "+
		"still serving in-flight traffic under an identity another process owns — drain it "+
		"and give every node a distinct cluster.node_id (DESIGN §13): %v", a.Node.ID(), err)
	if a.Server != nil {
		a.Server.StartDrain()
	}
}

// Disqualified reports whether this node's identity turned out to belong to
// another process. It is what /health answers from, and it is one-way: DESIGN
// §13 makes a duplicated id a deployment mistake rather than a transient.
func (a *App) Disqualified() bool { return a.disqualified.Load() }

// clusterTick is the node loop's interval, which is also how often the live
// node count is re-read. One knob: a deployment that beats faster because it
// wants a dead peer detected sooner wants its published bound to follow at the
// same speed, and two independent intervals would be two numbers to reason
// about for one question.
func (a *App) clusterTick() time.Duration {
	if a.opts.ClusterTick > 0 {
		return a.opts.ClusterTick
	}
	return cluster.DefaultTick
}

// refreshAccuracy re-reads how many nodes are alive.
//
// The figures of §5.6 are all `something × (nodes − 1)`, so a count that is
// never re-read publishes 0 — "exact" — from every node of a cluster that is
// not. That is the wrong number in the direction an operator sizes against,
// which is worse than no number at all.
func (a *App) refreshAccuracy(ctx context.Context) {
	if a.Node == nil || !a.Node.Enabled() {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := a.Node.Registry().Count(cctx)
	if err != nil {
		// The previous count stands. A published bound computed from the last
		// known peer count is a stale bound; one computed from a failed read is
		// no bound at all.
		a.logf("app: cluster: live node count: %v", err)
		return
	}
	a.nodes.set(n)
}
