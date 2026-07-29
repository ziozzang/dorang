package cluster

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DefaultLeaseTTL is how long a leadership lease is valid without renewal.
const DefaultLeaseTTL = 15 * time.Second

// DefaultTick is how often a node heartbeats, campaigns, and runs due leader
// jobs. It is a third of the lease TTL, so two consecutive failed renewals
// still leave a whole interval before the lease could be taken elsewhere.
const DefaultTick = 5 * time.Second

// Election is one node's participation in leader election.
//
// # What leadership means here
//
// The leader owns rollup compaction, partition pre-creation and retention,
// expiry sweeps for capacity reservations (DESIGN 5.3) and budget reservations
// (DESIGN 6.4), batch assignment, and lease rebalancing (DESIGN 13). None of
// that is on the request path: the request path is stateless and any node can
// serve any request. Leadership exists so that periodic work is done once
// rather than N times, not so that requests can be routed to one node.
//
// # Stopping cleanly rather than racing
//
// The hard part is not electing a leader; it is un-electing one. A node that
// still believes it leads while another node has taken the lease will run the
// same retention pass, the same sweep and the same batch assignment as its
// successor, concurrently. Four things prevent that, and all four are needed:
//
//  1. Every promotion opens a leader-scoped context, and every demotion cancels
//     it with [ErrLeadershipLost]. A leader job takes that context, so losing
//     leadership mid-task stops the task instead of letting it finish under a
//     successor.
//
//  2. Leadership expires from this node's own clock, without any store access.
//     [Election.IsLeader] demotes as soon as the safe window has passed. A
//     process frozen by a long GC pause, a suspended container or a paused
//     debugger therefore steps down on the way back rather than resuming as a
//     second leader -- which is exactly the "old leader revives" case, and no
//     amount of store availability would catch it.
//
//  3. The safe window ends before the lease does, by one guard band, and it is
//     measured from the expiry the STORE recorded rather than from this node's
//     clock once [Lock.Acquire] returned. That distinction is the difference
//     between a guard band and a number that looks like one: safeUntil is
//     expires_at - guard by construction, so it holds for an acquire that took
//     a microsecond and for one that took longer than the lease. Deriving it
//     from the clock afterwards instead made the margin
//     `guard - acquire latency`, which goes negative exactly when the store is
//     slow -- and a store slow enough to matter is the condition under which
//     leadership changes hands in the first place. Measured, before this was
//     derived from the row: a 15s lease with a 5s guard and an 8s acquire gave
//     two nodes three full seconds of simultaneous leadership, with both
//     clocks reading identically.
//
//  4. A fencing token accompanies leadership for what durations cannot close:
//     a write already in flight at the moment of demotion, and a clock
//     disagreement wider than the guard band. It advances on every change of
//     holder, so a superseded holder is recognisable by one read of the row
//     rather than by an argument about elapsed time. [Node] checks it against
//     the store before dispatching each leader job; [Fence.In] puts the
//     assertion inside a caller's own transaction. What the token cannot reach
//     is a job that writes through some other package's transaction -- see
//     [Fence].
//
// # What none of the four reaches
//
// All four are arguments about TERMS: which holder is current, and for how long
// this one can prove it is. None of them is an argument about WHO, and there is
// a way to get two leaders that never disagrees about a term at all. Two
// processes configured with the same node id are, to the lock, one holder
// renewing: the second [Lock.Acquire] matches on node_id, succeeds, and leaves
// the token where it was, so both processes hold a token equal to the row's and
// both pass [Election.Fenced]. Every mechanism above is working, and there are
// two leaders. That is caught in the registry instead, at the moment a process
// joins -- see [ErrDuplicateNodeID].
type Election struct {
	lock   Lock
	nodeID string
	ttl    time.Duration
	guard  time.Duration
	now    func() time.Time

	onChange func(leader bool, term uint64)

	mu        sync.Mutex
	leader    bool
	fence     uint64
	term      uint64
	safeUntil time.Time
	ctx       context.Context
	cancel    context.CancelCauseFunc
	closed    bool
}

// ElectionConfig configures [NewElection].
type ElectionConfig struct {
	// Lock is the store lock leadership is decided by. Required.
	Lock Lock
	// NodeID names this node. Required.
	NodeID string
	// TTL is the lease lifetime. Zero means [DefaultLeaseTTL].
	TTL time.Duration
	// Guard is how far before the lease expires this node gives leadership up.
	// Zero means a third of the TTL. It must be smaller than the TTL, or
	// leadership would end before it began.
	//
	// What it bounds, now that the safe window is derived from the lease the
	// store granted, is the disagreement between two nodes' clocks plus the
	// time a leader task takes to notice cancellation. It does NOT bound the
	// acquire round trip; that is handled by construction. A deployment whose
	// clocks can differ by more than this has two leaders for the difference,
	// and the fencing token rather than this number is what makes that
	// survivable.
	Guard time.Duration
	// OnChange, if set, is called after every promotion and demotion, outside
	// the election's lock. It is for logging and metrics; correctness must not
	// depend on it, because it is called after the transition, not during it.
	OnChange func(leader bool, term uint64)
	// Now overrides the clock.
	Now func() time.Time
}

// NewElection builds an election participant.
func NewElection(cfg ElectionConfig) (*Election, error) {
	if cfg.Lock == nil {
		return nil, errors.New("cluster: NewElection needs a Lock")
	}
	if cfg.NodeID == "" {
		return nil, errors.New("cluster: NewElection needs a node id")
	}
	ttl := orDuration(cfg.TTL, DefaultLeaseTTL)
	guard := cfg.Guard
	if guard <= 0 {
		guard = ttl / 3
	}
	if guard >= ttl {
		return nil, errors.New("cluster: election guard must be shorter than the lease TTL")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Election{
		lock:     cfg.Lock,
		nodeID:   cfg.NodeID,
		ttl:      ttl,
		guard:    guard,
		now:      now,
		onChange: cfg.OnChange,
	}, nil
}

// NodeID returns this participant's node id.
func (e *Election) NodeID() string { return e.nodeID }

// TTL returns the lease lifetime.
func (e *Election) TTL() time.Duration { return e.ttl }

// Campaign performs one election step: take the lease, or renew it if this node
// already holds it.
//
// It reports whether this node is the leader afterwards. A campaign that loses
// is not an error -- for all but one node, losing is the normal outcome.
//
// A store error is returned but does not by itself demote: a single failed
// renewal is far more likely to be a slow write than a lost lease, and demoting
// on it would make leadership flap under exactly the load that makes leadership
// worth having. What does demote is the passage of time, which the next call
// (or any [Election.IsLeader]) observes -- so a store that stays unreachable
// still costs this node its leadership within one guard band of the TTL, with
// no store access required to notice.
func (e *Election) Campaign(ctx context.Context) (bool, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return false, ErrClosed
	}
	e.mu.Unlock()

	// Expire first. Campaigning while stale would let this node renew a lease
	// it can no longer prove it held continuously.
	e.IsLeader()

	held, fence, expires, err := e.lock.Acquire(ctx, e.ttl)
	if err != nil {
		return e.IsLeader(), err
	}

	// The safe window is the granted lease less the guard band, and is computed
	// from nothing else. Adding the TTL to the clock here instead would place
	// it one round trip after the expiry the row holds, which is the arithmetic
	// that produced two leaders with no clock skew at all.
	now := e.now()
	safe := expires.Add(-e.guard)
	var notify []func()

	e.mu.Lock()
	switch {
	case !held:
		notify = append(notify, e.demoteLocked(ErrLeadershipLost)...)
	case expires.IsZero() || !now.Before(safe):
		// The lock was taken, but the round trip consumed the whole safe window
		// -- or the lock cannot say when its lease ends. Either way there is no
		// interval left in which this node can prove it leads, so it does not
		// claim to. The lease still stands and this node renews it on the next
		// campaign; what it must not do is act on a window that has already
		// closed. Refusing leadership is the conservative outcome: the cluster
		// goes without a leader for a tick, which costs a deferred sweep, and
		// the alternative costs a second leader.
		notify = append(notify, e.demoteLocked(ErrLeadershipLost)...)
	case e.leader && fence != e.fence:
		// The lease changed hands and came back. Whatever ran under the old
		// term must not continue under the new one, so this is a demotion
		// followed by a promotion, not a renewal.
		notify = append(notify, e.demoteLocked(ErrLeadershipLost)...)
		notify = append(notify, e.promoteLocked(fence, safe)...)
	case e.leader:
		e.safeUntil = safe
	default:
		notify = append(notify, e.promoteLocked(fence, safe)...)
	}
	leader := e.leader
	e.mu.Unlock()

	for _, fn := range notify {
		fn()
	}
	return leader, nil
}

// promoteLocked opens a new term, safe until the instant the caller derived
// from the granted lease. The caller holds e.mu.
func (e *Election) promoteLocked(fence uint64, safeUntil time.Time) []func() {
	ctx, cancel := context.WithCancelCause(context.Background())
	e.leader = true
	e.fence = fence
	e.term++
	e.safeUntil = safeUntil
	e.ctx, e.cancel = ctx, cancel
	term := e.term
	if e.onChange == nil {
		return nil
	}
	return []func(){func() { e.onChange(true, term) }}
}

// demoteLocked ends the current term and cancels everything running under it.
// The caller holds e.mu.
func (e *Election) demoteLocked(cause error) []func() {
	if !e.leader {
		return nil
	}
	e.leader = false
	e.safeUntil = time.Time{}
	if e.cancel != nil {
		e.cancel(cause)
	}
	term := e.term
	if e.onChange == nil {
		return nil
	}
	return []func(){func() { e.onChange(false, term) }}
}

// IsLeader reports whether this node currently leads, demoting it first if its
// safe window has passed.
//
// This is the check that does not depend on the store being reachable, on the
// campaign loop still running, or on this process having been scheduled at all
// since the last renewal. A node that was stopped and resumed calls this and
// discovers it is no longer the leader, which is the whole of DESIGN 13's
// requirement that a revived leader stop cleanly.
func (e *Election) IsLeader() bool {
	e.mu.Lock()
	notify := e.expireLocked(e.now())
	leader := e.leader
	e.mu.Unlock()
	for _, fn := range notify {
		fn()
	}
	return leader
}

func (e *Election) expireLocked(now time.Time) []func() {
	if !e.leader || now.Before(e.safeUntil) {
		return nil
	}
	return e.demoteLocked(ErrLeadershipLost)
}

// Leader returns the leader-scoped context, this term's fencing token, and
// whether this node leads.
//
// The context is cancelled with cause [ErrLeadershipLost] the moment leadership
// ends. A leader-only task must take it and honour it; a task that ignores it
// is a task that will one day run twice at once.
func (e *Election) Leader() (context.Context, uint64, bool) {
	e.mu.Lock()
	notify := e.expireLocked(e.now())
	ctx, fence, leader := e.ctx, e.fence, e.leader
	e.mu.Unlock()
	for _, fn := range notify {
		fn()
	}
	if !leader {
		return nil, 0, false
	}
	return ctx, fence, true
}

// Term counts promotions. It changes on every promotion and never otherwise, so
// a task can record the term it started under and compare.
func (e *Election) Term() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.term
}

// Fence returns the precondition a leader-owned write carries under the
// current term, or nil if this node does not lead. [Fence.Token] on the result
// is the raw token and is safe on nil.
func (e *Election) Fence() *Fence {
	_, f, ok := e.Leader()
	if !ok {
		return nil
	}
	return e.lock.Fence(f)
}

// SafeUntil reports the instant this node's leadership stops being provable. It
// is the granted lease less the guard band, so it is always at least one guard
// band before the lease a successor tests against -- whatever the acquire cost.
// Zero means this node does not lead.
func (e *Election) SafeUntil() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.leader {
		return time.Time{}
	}
	return e.safeUntil
}

// Fenced returns the leader-scoped context and this term's fencing token,
// having first asserted the token against the STORE rather than against this
// node's memory.
//
// It is what a caller uses before doing leader work, and it differs from
// [Election.Leader] in the one way that matters when clocks disagree: Leader
// answers from a safe window this node computed, and Fenced answers from the
// row every node shares. A node that has been superseded is demoted here -- the
// leader-scoped context is cancelled with [ErrLeadershipLost], so work already
// running under it stops too -- and gets [ErrLeadershipLost] back.
//
// It costs one store round trip. That is why it guards a due job rather than
// every tick: leadership is renewed on the campaign, and this is the second
// opinion taken at the moment work is about to happen.
func (e *Election) Fenced(ctx context.Context) (context.Context, *Fence, error) {
	lctx, token, ok := e.Leader()
	if !ok {
		return nil, nil, ErrNotLeader
	}
	f := e.lock.Fence(token)
	if err := f.Check(ctx); err != nil {
		if errors.Is(err, ErrLeadershipLost) {
			e.mu.Lock()
			notify := e.demoteLocked(err)
			e.mu.Unlock()
			for _, fn := range notify {
				fn()
			}
		}
		return nil, nil, err
	}
	return lctx, f, nil
}

// Resign gives leadership up deliberately: the leader-scoped context is
// cancelled first, so leader tasks stop before the lease is handed over, and
// only then is the lease released.
//
// The order matters. Releasing first would open the window this whole type
// exists to close -- a successor elected while this node's tasks are still
// running.
func (e *Election) Resign(ctx context.Context) error {
	e.mu.Lock()
	notify := e.demoteLocked(ErrLeadershipLost)
	e.mu.Unlock()
	for _, fn := range notify {
		fn()
	}
	return e.lock.Release(ctx)
}

// Disqualify ends this node's participation permanently: it stops leading now,
// and every later campaign is refused with [ErrClosed].
//
// It differs from [Election.Resign] in the one way that matters when the reason
// is [ErrDuplicateNodeID]: the lease is NOT released. Release is scoped by node
// id, and under a duplicated id that is precisely the other process's lease as
// well -- so a node that discovered it shares an identity and then released
// "its" lease would take leadership away from the process that legitimately
// holds it, on the way out. Letting the lease lapse on its TTL is slower and is
// the only ending that cannot hurt somebody else.
//
// It is one-way. A node whose id turned out to belong to another process does
// not get it back by waiting: the condition is a deployment mistake, not a
// transient, and a node that resumed campaigning on the next pass would be two
// leaders again one tick later.
func (e *Election) Disqualify(cause error) {
	e.mu.Lock()
	e.closed = true
	notify := e.demoteLocked(cause)
	e.mu.Unlock()
	for _, fn := range notify {
		fn()
	}
}

// Close resigns and refuses further campaigns.
func (e *Election) Close(ctx context.Context) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.mu.Unlock()
	return e.Resign(ctx)
}

// Observe reports who the cluster currently believes the leader is, read from
// the store rather than from this node's memory. It is for diagnostics and for
// tests; nothing in this package routes on it.
func (e *Election) Observe(ctx context.Context) (nodeID string, expires time.Time, fence uint64, err error) {
	return e.lock.Holder(ctx)
}
