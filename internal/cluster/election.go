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
// successor, concurrently. Three things prevent that, and all three are needed:
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
//  3. The safe window ends before the lease does, by one guard band. A
//     successor may take the lease the instant it expires, so an incumbent that
//     stopped exactly at expiry would still overlap by however long its last
//     task takes to notice cancellation. The guard band is that margin, and it
//     is why leadership is given up early rather than late.
//
// A fencing token accompanies leadership for the fourth case, the one no
// timeout can close: a write already in flight at the moment of demotion. It
// increases on every change of holder, so a superseded write is recognisable
// as such by anything that cares to check.
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

	held, fence, err := e.lock.Acquire(ctx, e.ttl)
	if err != nil {
		return e.IsLeader(), err
	}

	now := e.now()
	var notify []func()

	e.mu.Lock()
	switch {
	case !held:
		notify = append(notify, e.demoteLocked(ErrLeadershipLost)...)
	case e.leader && fence != e.fence:
		// The lease changed hands and came back. Whatever ran under the old
		// term must not continue under the new one, so this is a demotion
		// followed by a promotion, not a renewal.
		notify = append(notify, e.demoteLocked(ErrLeadershipLost)...)
		notify = append(notify, e.promoteLocked(fence, now)...)
	case e.leader:
		e.safeUntil = now.Add(e.ttl - e.guard)
	default:
		notify = append(notify, e.promoteLocked(fence, now)...)
	}
	leader := e.leader
	e.mu.Unlock()

	for _, fn := range notify {
		fn()
	}
	return leader, nil
}

// promoteLocked opens a new term. The caller holds e.mu.
func (e *Election) promoteLocked(fence uint64, now time.Time) []func() {
	ctx, cancel := context.WithCancelCause(context.Background())
	e.leader = true
	e.fence = fence
	e.term++
	e.safeUntil = now.Add(e.ttl - e.guard)
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

// Fence returns the current fencing token, or zero if this node does not lead.
func (e *Election) Fence() uint64 {
	if _, f, ok := e.Leader(); ok {
		return f
	}
	return 0
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
