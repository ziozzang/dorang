package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// Config configures a [Node]. It mirrors the `cluster` block of DESIGN 4.2.
type Config struct {
	// Enabled mirrors cluster.enabled. With ModeLocal it is refused
	// ([ErrLocalInCluster]).
	Enabled bool
	// NodeID mirrors cluster.node_id. Empty means one is generated, which is
	// per process rather than per host: two processes sharing an id are one
	// node as far as every lease here is concerned.
	NodeID string
	// Address and Version are published in the registry so that a mixed
	// deployment is visible rather than inferred.
	Address string
	Version string
	// Mode mirrors cluster.capacity_mode.
	Mode Mode
	// Store is the shared store. Required.
	Store *store.Store
	// LeaseTTL is the leadership lease lifetime. Zero means
	// [DefaultLeaseTTL].
	LeaseTTL time.Duration
	// Tick is how often [Node.Start]'s loop runs. Zero means [DefaultTick].
	Tick time.Duration
	// NodeTTL is the heartbeat lapse after which a node is considered gone.
	// Zero means [DefaultNodeTTL].
	NodeTTL time.Duration
	// BlockSize is the ledger's lease size, the overshoot knob of DESIGN 9.6.
	// Zero means [DefaultBlockSize].
	//
	// It is in the counter's own units, which for the budget the gateway puts
	// on the request path means nano-USD. See [Config.LeaseBlock] for why that
	// makes it the wrong number to publish an overshoot against.
	BlockSize int64
	// LeaseBlock is how many METRIC units a leased coordinator takes at a
	// time: concurrency slots, requests, tokens. Zero means
	// [DefaultBlockSize].
	//
	// It is separate from [Config.BlockSize] because the two are denominated
	// differently and only coincide in this package's tests. internal/app
	// draws budget in nano-USD, so a single field would either publish a
	// concurrency overshoot of "50000000 x (nodes - 1)" -- a figure an
	// operator would size a cluster against -- or force the budget to take one
	// store write per sixteen nano-USD. DESIGN 5.6 requires the published
	// number to be a number, and a number in the wrong units is worse than
	// none.
	LeaseBlock int64
	// MinLeasable mirrors cluster.min_leasable. Zero means
	// [DefaultMinLeasable].
	MinLeasable int64
	// Retention is the policy the maintenance job enforces. A zero policy
	// keeps everything, which is the notebook default.
	Retention store.RetentionPolicy
	// Jobs are extra leader-only jobs, appended to the built-in ones. Rollup
	// compaction and batch assignment arrive this way, because both live in
	// packages this one must not depend on.
	Jobs []Job
	// SweepCapacity, if set, is the capacity broker's reservation sweep
	// (DESIGN 5.3). It is a function rather than an interface so that this
	// package does not import internal/capacity for one method.
	SweepCapacity func() int
	// Now overrides the clock.
	Now func() time.Time
	// Logf, if set, receives job failures and leadership transitions.
	Logf func(format string, args ...any)
}

// Node is one dorang process's membership in a cluster.
//
// It owns the registry row, the leadership lease, this node's share of every
// leased limit, and the leader-only jobs. It owns nothing on the request path:
// DESIGN 13 makes the request path stateless, and any node can serve any
// request.
//
// A Node is safe for concurrent use.
type Node struct {
	// enabled and mode are set once, at construction, and are readable but not
	// writable from outside this package. That is deliberate: the hard guard of
	// DESIGN 5.6 is only a guard if there is no later moment at which the mode
	// can become "local" again.
	enabled bool
	mode    Mode

	id         string
	address    string
	version    string
	tick       time.Duration
	nodeTTL    time.Duration
	minLease   int64
	leaseBlock int64
	startedAt  time.Time
	now        func() time.Time
	logf       func(string, ...any)

	reg    *Registry
	el     *Election
	ledger *Ledger
	leases *LeaseStore

	mu   sync.Mutex
	jobs []*jobState
	// registered records that this node has a row in the registry. Close
	// deregisters only when it does, so that a node which never joined -- the
	// unclustered gateway, which builds a Node for its ledger and never starts
	// the loop -- writes nothing on the way out.
	registered bool
	started    bool
	closed     bool
	stop       chan struct{}
	stopped    chan struct{}
}

// New builds a node.
//
// It refuses cluster.enabled with capacity_mode "local" ([ErrLocalInCluster]).
// internal/config refuses the same combination at load; this is the second
// place, because a guard with one entry point is a guard that a second entry
// point walks around, and DESIGN 5.6 makes this a refusal rather than a warning
// for a reason that is easy to miss: the symptom appears in an unrelated model.
func New(cfg Config) (*Node, error) {
	if err := Guard(cfg.Enabled, cfg.Mode); err != nil {
		return nil, err
	}
	if cfg.Store == nil {
		return nil, errors.New("cluster: New needs a store")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	id := cfg.NodeID
	if id == "" {
		id = "node-" + store.NewID()[:12]
	}

	reg, err := NewRegistry(cfg.Store, id, cfg.NodeTTL, now)
	if err != nil {
		return nil, err
	}
	lock, err := NewLock(cfg.Store, LeaderLockName, id, now)
	if err != nil {
		return nil, err
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	el, err := NewElection(ElectionConfig{
		Lock: lock, NodeID: id, TTL: cfg.LeaseTTL, Now: now,
		OnChange: func(leader bool, term uint64) {
			if leader {
				logf("cluster: node %s became leader (term %d)", id, term)
				return
			}
			logf("cluster: node %s lost leadership (term %d)", id, term)
		},
	})
	if err != nil {
		return nil, err
	}
	ledger, err := NewLedger(LedgerConfig{
		Store: cfg.Store, NodeID: id, Block: cfg.BlockSize, Now: now,
	})
	if err != nil {
		return nil, err
	}
	leases, err := NewLeaseStore(cfg.Store, id, now)
	if err != nil {
		return nil, err
	}

	n := &Node{
		enabled:    cfg.Enabled,
		mode:       cfg.Mode,
		id:         id,
		address:    cfg.Address,
		version:    cfg.Version,
		tick:       orDuration(cfg.Tick, DefaultTick),
		nodeTTL:    reg.NodeTTL(),
		minLease:   orInt64(cfg.MinLeasable, DefaultMinLeasable),
		leaseBlock: orInt64(cfg.LeaseBlock, DefaultBlockSize),
		startedAt:  now(),
		now:        now,
		logf:       logf,
		reg:        reg,
		el:         el,
		ledger:     ledger,
		leases:     leases,
		stop:       make(chan struct{}),
		stopped:    make(chan struct{}),
	}

	every := n.tick
	n.addJobs(
		MaintenanceJob(cfg.Store, cfg.Retention, maxDuration(every, time.Minute), now),
		ReservationSweepJob(cfg.Store, cfg.SweepCapacity, every, now),
		LeaseReclaimJob(reg, ledger, leases, every, now),
	)
	n.addJobs(cfg.Jobs...)
	return n, nil
}

// Guard is the hard guard of DESIGN 5.6, exported so that every entry point can
// state it rather than reimplement it.
//
// cluster.enabled with capacity_mode "local" refuses to start. Node-local
// counting is exact on one node only, so with N nodes every ceiling is counted
// N times over. The upstream 429s that follow cascade into the fallback chain
// and consume the capacity of unrelated models in the same class, which is why
// the design promoted this from a recommendation to a refusal: the failure
// surfaces far from its cause.
func Guard(enabled bool, mode Mode) error {
	if enabled && mode == ModeLocal {
		return ErrLocalInCluster
	}
	return nil
}

// ID returns this node's identity.
func (n *Node) ID() string { return n.id }

// Enabled reports whether clustering is on.
func (n *Node) Enabled() bool { return n.enabled }

// Mode reports the capacity mode. There is deliberately no setter: the guard in
// [New] is only a guard if the mode cannot become "local" afterwards.
func (n *Node) Mode() Mode { return n.mode }

// Registry returns the node registry.
func (n *Node) Registry() *Registry { return n.reg }

// Election returns this node's election participant.
func (n *Node) Election() *Election { return n.el }

// Ledger returns the durable quota and budget ledger.
func (n *Node) Ledger() *Ledger { return n.ledger }

// Leases returns the shared lease table, which is the shared-pg backend.
func (n *Node) Leases() *LeaseStore { return n.leases }

// IsLeader reports whether this node currently leads.
func (n *Node) IsLeader() bool { return n.el.IsLeader() }

// Accuracy returns the published maximum overshoot for this node's mode,
// against a limit and the number of nodes currently alive.
//
// It reads the live node count rather than a configured one, so the figure
// describes the cluster that exists. DESIGN 5.6 requires the number to be
// published; publishing one computed from a node count nobody has checked would
// satisfy the letter of that and nothing else.
func (n *Node) Accuracy(ctx context.Context, limit int64) (Accuracy, error) {
	p, err := n.AccuracyParams(ctx, limit)
	if err != nil {
		return Accuracy{}, err
	}
	return Publish(n.mode, p)
}

// AccuracyParams returns the deployment description a published figure is
// about, with the live node count read from the registry.
//
// [Node.Accuracy] answers for this node's own mode. A caller that publishes
// every mode -- which DESIGN 5.6 asks for, so that the cost of a different
// choice is visible without making it -- needs the parameters rather than one
// mode's answer, and must not be tempted to assemble them itself from a node
// count nobody checked.
func (n *Node) AccuracyParams(ctx context.Context, limit int64) (Params, error) {
	nodes, err := n.reg.Count(ctx)
	if err != nil {
		return Params{}, err
	}
	return Params{
		Limit: limit, Nodes: nodes, Block: n.leaseBlock,
		MinLeasable: n.minLease, Clustered: n.enabled,
	}, nil
}

// Coordinator builds a quota coordinator for this node's mode.
//
// Every field that could weaken the guard is overridden from the node rather
// than taken from the caller: the mode, whether the deployment is clustered,
// the node id, and the shared backend. A caller cannot ask for local
// coordination in a cluster by handing over a config that says so.
//
// The shared backend defaults to this node's lease table for shared-pg and
// leased, and DELIBERATELY NOT for shared-redis ([ErrNoRedisClient]). The two
// modes are the same coordinator over different stores, so substituting the SQL
// table for a missing Redis client would produce a working coordinator that
// reports Mode() == "shared-redis" and publishes "one round trip to Redis per
// acquire" while touching no Redis at all. An operator reads that figure to
// choose between the modes; a mode that quietly is not the mode it says it is
// makes the choice meaningless. A caller with a real client passes
// [NewRedisShared] in cfg.Shared and the mode is honoured.
func (n *Node) Coordinator(cfg quota.CoordinatorConfig) (quota.Coordinator, error) {
	if err := Guard(n.enabled, n.mode); err != nil {
		return nil, err
	}
	cfg.Mode = n.mode
	cfg.Clustered = n.enabled
	cfg.NodeID = n.id
	if cfg.Now == nil {
		cfg.Now = n.now
	}
	if cfg.MinLeasable <= 0 {
		cfg.MinLeasable = n.minLease
	}
	if cfg.BlockSize <= 0 {
		cfg.BlockSize = n.leaseBlock
	}
	if cfg.Shared == nil {
		switch n.mode {
		case ModeSharedPG, ModeLeased:
			cfg.Shared = n.leases
		case ModeSharedRedis:
			return nil, ErrNoRedisClient
		}
	}
	return quota.NewCoordinator(cfg)
}

// Register records this node and beats once.
func (n *Node) Register(ctx context.Context) error {
	err := n.reg.Register(ctx, NodeInfo{
		ID: n.id, Address: n.address, Version: n.version, StartedAt: n.startedAt,
	})
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.registered = true
	n.mu.Unlock()
	return nil
}

// Tick is one pass of the node loop: heartbeat, campaign, renew this node's
// leases, and -- if this node leads -- run the leader jobs that are due.
//
// It is exported so that a caller can drive the loop deterministically, which
// is what the tests do. Nothing in it blocks for longer than the store takes.
func (n *Node) Tick(ctx context.Context) error {
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	if closed {
		return ErrClosed
	}

	var errs []error

	// Heartbeat before campaigning. A node that cannot record itself alive has
	// no business claiming leadership, and re-registering here is what lets a
	// node whose row was pruned rejoin instead of quietly ceasing to exist.
	if err := n.reg.Heartbeat(ctx, n.el.IsLeader()); err != nil {
		if errors.Is(err, ErrNotRegistered) {
			if rerr := n.Register(ctx); rerr != nil {
				errs = append(errs, rerr)
			}
		} else {
			errs = append(errs, err)
		}
	}

	leader, err := n.el.Campaign(ctx)
	if err != nil {
		errs = append(errs, err)
	}

	if err := n.ledger.Maintain(ctx); err != nil && !errors.Is(err, ErrClosed) {
		errs = append(errs, err)
	}

	if leader {
		if err := n.runDueJobs(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// runDueJobs runs the leader jobs whose interval has elapsed.
//
// Every job runs under the leader-scoped context, so a demotion part way
// through a retention pass or a batch assignment cancels it rather than letting
// it race the successor that is already starting the same work. The leadership
// check is repeated between jobs for the same reason: a pass with five jobs
// must not finish all five on the strength of a check made before the first.
func (n *Node) runDueJobs(ctx context.Context) error {
	n.mu.Lock()
	jobs := make([]*jobState, len(n.jobs))
	copy(jobs, n.jobs)
	n.mu.Unlock()

	now := n.now()
	var errs []error
	for _, j := range jobs {
		lctx, _, ok := n.el.Leader()
		if !ok {
			break
		}
		if !j.due(now) {
			continue
		}
		// The leader context ends the job; the caller's context bounds it. Both
		// matter: one is demotion, the other is shutdown.
		runCtx, cancel := mergeCancel(ctx, lctx)
		err := j.run(runCtx, now)
		cancel()
		if err != nil {
			if errors.Is(err, context.Canceled) && !n.el.IsLeader() {
				// Demoted mid-task. Stopping is the correct outcome, not a
				// failure to report.
				break
			}
			n.logf("cluster: leader job %s failed: %v", j.job.Name, err)
			errs = append(errs, fmt.Errorf("cluster: job %s: %w", j.job.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Start registers the node and runs [Node.Tick] on an interval until
// [Node.Close]. Calling it twice, or after Close, is refused rather than
// quietly starting a second loop that would heartbeat and campaign as the same
// node.
func (n *Node) Start(ctx context.Context) error {
	n.mu.Lock()
	switch {
	case n.closed:
		n.mu.Unlock()
		return ErrClosed
	case n.started:
		n.mu.Unlock()
		return errors.New("cluster: node is already started")
	}
	n.started = true
	n.mu.Unlock()

	if err := n.Register(ctx); err != nil {
		return err
	}
	go n.loop(ctx)
	return nil
}

func (n *Node) loop(ctx context.Context) {
	defer close(n.stopped)
	t := time.NewTicker(n.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.stop:
			return
		case <-t.C:
			if err := n.Tick(ctx); err != nil {
				n.logf("cluster: tick: %v", err)
			}
		}
	}
}

// Close drains this node: it stops the loop, resigns leadership so a successor
// can take over immediately rather than after a lease TTL, returns every leased
// unit, and removes the registry row.
//
// The order is the draining order of DESIGN 13, and each step depends on the
// one before it. Resigning after returning the leases would leave a window in
// which this node still leads with nothing to lead with.
func (n *Node) Close(ctx context.Context) error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	started := n.started
	registered := n.registered
	close(n.stop)
	n.mu.Unlock()

	// Wait for the loop to stop before releasing anything. A tick that is
	// already in flight would otherwise renew the lease this call is about to
	// release, and the node would leave holding a lease nobody is renewing --
	// which is exactly the state the TTL exists to recover from, entered on
	// purpose. The wait is bounded: Tick returns ErrClosed immediately once
	// closed is set, so the loop reaches its next select at once.
	if started {
		<-n.stopped
	}

	var errs []error
	if err := n.el.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := n.ledger.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := n.leases.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	// Deregistration is LAST, and not beside the readiness flip at the top of
	// the drain. That looks wrong -- the node advertises itself as live for the
	// whole drain -- so here is why it is not, because the next reader will ask.
	//
	// Nothing routes off this table. DESIGN 13 makes the request path stateless
	// and any node able to serve any request; what stops traffic arriving is the
	// balancer reading /health/readiness, which went false at the start of the
	// drain. The registry's readers are all coordination:
	//
	//   - [LeaseReclaimJob] walks [Registry.Dead] to reclaim the leases of a
	//     node that stopped beating. The ROW is the handle for that. Deleting it
	//     first would mean a drain interrupted between here and the top -- a
	//     SIGKILL after the grace, a machine that goes away -- leaves leases the
	//     leader can no longer attribute, recoverable only on their own TTL
	//     through [Ledger.ReclaimExpired] rather than promptly.
	//   - [Node.Accuracy] divides a limit by [Registry.Count], the number of
	//     LIVE nodes, to publish the overshoot of DESIGN 5.6. A node that
	//     deregistered while still holding its leases and still serving makes
	//     every peer size its share as if it were gone, on top of a share it has
	//     not yet returned. The published figure would then understate the real
	//     overshoot, which is the one number 5.6 exists to keep honest.
	//
	// Both arguments say the same thing: readiness is the signal to the
	// balancer and the registry row is the signal to the cluster, they answer
	// different questions, and they correctly stop being true at different
	// moments. The row goes away only once this node holds nothing.
	//
	// A node that never registered has no row, and issuing the DELETE anyway
	// would put a write into the shutdown of every unclustered gateway --
	// which builds a Node to own its ledger and never joins anything. The
	// guard is on having registered rather than on having started, because
	// [Node.Register] is callable on its own and a row placed that way still
	// has to be taken away.
	if registered {
		if err := n.reg.Deregister(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// addJobs appends jobs, skipping the ones a caller left empty.
func (n *Node) addJobs(jobs ...Job) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, j := range jobs {
		if j.Run == nil || j.Name == "" {
			continue
		}
		if j.Every <= 0 {
			j.Every = n.tick
		}
		n.jobs = append(n.jobs, &jobState{job: j})
	}
}

// JobStats reports how many times each leader job has run and failed.
func (n *Node) JobStats() map[string]JobStat {
	n.mu.Lock()
	jobs := make([]*jobState, len(n.jobs))
	copy(jobs, n.jobs)
	n.mu.Unlock()

	out := make(map[string]JobStat, len(jobs))
	for _, j := range jobs {
		out[j.job.Name] = j.stat()
	}
	return out
}

// mergeCancel returns a context cancelled when either parent is.
//
// context.WithCancel takes one parent, and a leader job genuinely has two: the
// process is shutting down, or this node is no longer the leader. Treating
// either as decisive is the point -- a job that honoured only shutdown would
// keep running under a successor.
func mergeCancel(a, b context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(a)
	stop := make(chan struct{})
	go func() {
		select {
		case <-b.Done():
			cancel(context.Cause(b))
		case <-ctx.Done():
		case <-stop:
		}
	}()
	return ctx, func() {
		close(stop)
		cancel(context.Canceled)
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
