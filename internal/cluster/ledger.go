package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// CounterKind selects which durable counter a block is drawn from.
type CounterKind uint8

const (
	// CounterBudget draws from budget_state.spent_nano, in nano-USD.
	CounterBudget CounterKind = iota
	// CounterQuota draws from quota_buckets.value, in the metric's own units.
	CounterQuota
)

// String returns the kind's name, which is also its quota_leases.scope prefix.
func (k CounterKind) String() string {
	if k == CounterQuota {
		return "quota"
	}
	return "budget"
}

// CounterKey identifies one durable counter: a budget for a subject over a
// period, or a quota for a scope over a window.
//
// PeriodStart is part of the identity. That is what makes a counter reset in
// step on every node when a period rolls over, rather than each node deciding
// for itself when the month began.
type CounterKey struct {
	Kind CounterKind
	// Scope is the budget subject kind ("key", "team", "user", "credential",
	// "global") or the quota scope.
	Scope string
	// ID is the subject or scope id. Empty is legitimate for a global subject.
	ID string
	// Window is the budget period or the quota window.
	Window quota.Window
	// Metric is the quota metric. A budget is always [quota.MetricCostUSD].
	Metric quota.Metric
	// PeriodStart is Window.PeriodStart of the instant being accounted. Zero
	// means "derive it from now", which is what a caller normally wants.
	PeriodStart time.Time
}

// BudgetKey builds the key of a budget subject's period.
func BudgetKey(subjectKind, subjectID string, period quota.Window, now time.Time) CounterKey {
	return CounterKey{
		Kind: CounterBudget, Scope: subjectKind, ID: subjectID,
		Window: period, Metric: quota.MetricCostUSD, PeriodStart: period.PeriodStart(now),
	}
}

// QuotaKey builds the key of a quota scope's window.
func QuotaKey(scope, scopeKey string, w quota.Window, m quota.Metric, now time.Time) CounterKey {
	return CounterKey{
		Kind: CounterQuota, Scope: scope, ID: scopeKey,
		Window: w, Metric: m, PeriodStart: w.PeriodStart(now),
	}
}

func (k CounterKey) normalize(now time.Time) CounterKey {
	if k.PeriodStart.IsZero() {
		k.PeriodStart = k.Window.PeriodStart(now)
	}
	k.PeriodStart = k.PeriodStart.UTC()
	return k
}

func (k CounterKey) valid() error {
	if !k.Window.Valid() {
		return fmt.Errorf("cluster: counter key has an invalid window %q", k.Window)
	}
	if !k.Metric.Valid() {
		return fmt.Errorf("cluster: counter key has an invalid metric %v", k.Metric)
	}
	if k.Kind != CounterBudget && k.Kind != CounterQuota {
		return fmt.Errorf("cluster: counter key has an unknown kind %v", k.Kind)
	}
	return nil
}

// String is the in-memory map key. It is not stored.
func (k CounterKey) String() string {
	var b strings.Builder
	b.Grow(len(k.Scope) + len(k.ID) + 40)
	b.WriteString(k.Kind.String())
	b.WriteByte('/')
	b.WriteString(k.Scope)
	b.WriteByte('/')
	b.WriteString(k.ID)
	b.WriteByte('/')
	b.WriteString(k.leaseWindow())
	b.WriteByte('/')
	b.WriteString(k.Metric.String())
	return b.String()
}

// leaseScope is the quota_leases.scope value: the kind, so that one reclaim
// pass can route a row back to the right counter table, and so that these rows
// are distinguishable from [LeaseStore]'s.
func (k CounterKey) leaseScope() string { return k.Kind.String() + ":" + k.Scope }

// leaseWindow packs the window and its period start into quota_leases.window.
//
// The lease table has no period column, and the two together are one concept --
// which period of which window -- so they travel as one value rather than as a
// column added to a table five other things use. The encoding is
// "<window>@<unix seconds>" and is parsed back by parseLeaseWindow.
func (k CounterKey) leaseWindow() string {
	return k.Window.String() + "@" + strconv.FormatInt(k.PeriodStart.UTC().Unix(), 10)
}

func parseLeaseWindow(s string) (quota.Window, time.Time, error) {
	name, secs, ok := strings.Cut(s, "@")
	if !ok {
		return quota.Window{}, time.Time{}, fmt.Errorf("cluster: malformed lease window %q", s)
	}
	w, err := quota.ParseWindow(name)
	if err != nil {
		return quota.Window{}, time.Time{}, err
	}
	n, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return quota.Window{}, time.Time{}, fmt.Errorf("cluster: malformed lease period %q: %w", s, err)
	}
	return w, time.Unix(n, 0).UTC(), nil
}

// Defaults for a Ledger.
const (
	// DefaultLedgerTTL is how long a block lease survives without renewal. It
	// is the window in which a dead node's unspent budget is unavailable to
	// everybody else, so it is short -- but not shorter than several ticks, or
	// a live node would keep losing blocks it is still spending.
	DefaultLedgerTTL = 60 * time.Second
	// DefaultRenewBefore is how far ahead of expiry a live node renews. It is
	// the same guard-band idea as leadership: a lease that is renewed only at
	// the moment it expires has already had a window in which it was not
	// provably held.
	DefaultRenewBefore = 20 * time.Second
)

// LedgerConfig configures [NewLedger].
type LedgerConfig struct {
	// Store is where the durable counters live. Required.
	Store *store.Store
	// NodeID names this node in its lease rows. Required.
	NodeID string
	// Block is how many units a lease draws at a time. Zero means
	// [DefaultBlockSize].
	//
	// This is the knob of DESIGN 9.6: it trades store traffic against maximum
	// overshoot. A larger block means fewer writes and a larger figure from
	// [Publish]; a block of one means a write per request and an exact
	// accounting, which is the arrangement DESIGN 9.6 exists to avoid.
	Block int64
	// TTL is the lease lifetime. Zero means [DefaultLedgerTTL].
	TTL time.Duration
	// RenewBefore is how far ahead of expiry [Ledger.Maintain] renews. Zero
	// means [DefaultRenewBefore], capped at a third of the TTL.
	RenewBefore time.Duration
	// Now overrides the clock.
	Now func() time.Time
}

// Ledger is quota and budget that survive a restart.
//
// # The gap this closes
//
// DESIGN 18, risk W9: quota and budget state was in-memory only, so a restart
// reset the windows. That is safe for concurrency -- nothing is over-granted by
// forgetting -- and wrong for accounting, because a monthly budget silently
// starts over. A lease that does not outlive its node is not a lease, and
// clustering is built on leases, so this had to close before clustering could
// mean anything.
//
// # How
//
// DESIGN 9.6 classifies budget as the one write that cannot be deferred:
// deferring the reservation means two concurrent requests both see the pre-spend
// balance, and DESIGN 6.4's whole point is that they must not. So the write is
// made cheap rather than made asynchronous:
//
//   - The hot path takes units from an in-memory block guarded by an atomic
//     compare-and-swap. No store access, no lock, no allocation.
//   - Durability comes from the lease the node holds. The block is charged to
//     the durable counter when it is drawn, before a single unit of it is
//     spent, so the counter is always at or ahead of reality.
//   - The store therefore sees one write per block of budget rather than one
//     per request.
//
// Charging the block up front is what makes the arrangement safe rather than
// merely fast: a node that dies mid-block has already had the whole block
// counted against the budget. The failure mode is a budget that under-spends
// until the lease is reclaimed, never one that overspends. The reclaim
// ([Ledger.ReclaimExpired]) returns the part the dead node had not spent, which
// is recorded in the lease row's `used` column and checkpointed whenever a
// block is drawn or renewed.
//
// # Accuracy
//
// [Publish] with [ModeLeased] gives the number, and the arithmetic is the same:
// block x (nodes - 1). The residue comes from a crash between a checkpoint and
// the spend that followed it, which makes the reclaim return slightly more than
// it should. A graceful [Ledger.Close] checkpoints and returns the exact
// remainder, so a planned restart contributes nothing at all.
//
// A Ledger is safe for concurrent use.
type Ledger struct {
	c      conn
	nodeID string
	block  int64
	ttl    time.Duration
	renew  time.Duration
	now    func() time.Time

	mu     sync.RWMutex
	blocks map[string]*ledgerBlock
	closed bool

	// draws counts store round trips taken to refill a block, so that the
	// "one write per block, not per request" claim is measurable.
	draws atomic.Int64
}

// ledgerBlock is one node's lease on one counter.
type ledgerBlock struct {
	key   CounterKey
	rowID string

	// remaining is the hot path: units drawn and not yet spent. Only ever
	// decremented by a compare-and-swap from the request path, and only ever
	// increased under mu (a refill) or by a settlement refund.
	remaining atomic.Int64
	// expiresUS is the lease expiry in unix microseconds. Read on the hot path
	// so that no unit is ever spent against a lease the authority may already
	// have reclaimed -- the rule that makes the published overshoot an upper
	// bound rather than an estimate.
	expiresUS atomic.Int64

	// limitSeen and counterAtDraw exist only to be read by an observer, and
	// they are atomics for one reason: mu below is held across a store round
	// trip, and a metrics scrape that waited behind one would be unavailable at
	// exactly the moment the store is the thing going wrong. Both are written
	// where the fields they mirror are written, under mu, and read without it.
	//
	// counterAtDraw is the durable counter immediately after this node's last
	// draw. Subtracting `remaining` from it estimates what has been spent
	// against the counter fleet-wide, on the assumption that every other node
	// has spent what it drew -- an upper bound, which is the safe direction for
	// a budget gauge to be wrong in.
	limitSeen     atomic.Int64
	counterAtDraw atomic.Int64

	// mu guards the slow path: drawing, renewing, checkpointing and closing.
	// The hot path never takes it.
	mu    sync.Mutex
	drawn int64
	// checkpointed is the consumption last written to the lease row. The gap
	// between it and the true figure is what a reclaim over-returns if this
	// node dies now, so it is tracked in order to be closed rather than left
	// to be argued about.
	checkpointed int64
}

// NewLedger builds a ledger for one node.
func NewLedger(cfg LedgerConfig) (*Ledger, error) {
	if cfg.Store == nil {
		return nil, errors.New("cluster: NewLedger needs a store")
	}
	if cfg.NodeID == "" {
		return nil, errors.New("cluster: NewLedger needs a node id")
	}
	ttl := orDuration(cfg.TTL, DefaultLedgerTTL)
	renew := orDuration(cfg.RenewBefore, DefaultRenewBefore)
	if renew >= ttl {
		renew = ttl / 3
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Ledger{
		c:      newConn(cfg.Store),
		nodeID: cfg.NodeID,
		block:  orInt64(cfg.Block, DefaultBlockSize),
		ttl:    ttl,
		renew:  renew,
		now:    now,
		blocks: map[string]*ledgerBlock{},
	}, nil
}

// BlockSize reports the lease size, which is the overshoot knob.
func (l *Ledger) BlockSize() int64 { return l.block }

// Draws reports how many store round trips this ledger has made to refill a
// block. Compared against the number of reservations, it is the measured
// "write per block, not per request" of DESIGN 9.6.
func (l *Ledger) Draws() int64 { return l.draws.Load() }

// Hold is units taken from a block and not yet settled.
//
// It is a soft hold in DESIGN 6.4's sense at the gate and a hard one after
// capacity is acquired, but the distinction lives in the caller: from the
// ledger's point of view both are units removed from a block, and both are
// returned in full by [Ledger.Release].
type Hold struct {
	Key    CounterKey
	Amount int64

	block *ledgerBlock
	done  atomic.Bool
}

// Consumed estimates what has been spent against this hold's counter, and the
// ceiling it is measured against.
//
// It is the same upper-bound arithmetic [Ledger.Stats] reports — the durable
// counter at this node's last draw, less what the node has not yet spent — but
// for one counter and without the lock, so it is two atomic loads and no
// allocation. That matters because the caller is the request path: DESIGN
// §11.5's budget_80pct threshold is crossed while serving a request, and
// discovering it must not cost a store read.
//
// Both figures are zero for a hold taken against no limit.
func (h *Hold) Consumed() (spent, limit int64) {
	if h == nil || h.block == nil {
		return 0, 0
	}
	rem := h.block.remaining.Load()
	spent = h.block.counterAtDraw.Load() - rem
	if spent < 0 {
		spent = 0
	}
	return spent, h.block.limitSeen.Load()
}

// Reserve takes amount units against limit.
//
// limit is passed in rather than read from the store because the hot path
// already holds the authorization snapshot the limit comes from (DESIGN 9.1),
// and re-reading it here would put a query on the gate. A limit of zero or less
// means unlimited: units are still counted, because the accounting is the point
// even when nothing is capped, but nothing is refused.
//
// It returns [ErrExhausted] when the limit has no room left. For a budget that
// is DESIGN 6.4's terminal outcome and must surface as a 400, not a 429: a 429
// would send the request down the fallback chain and spend a different
// subject's budget on a model the caller never asked for.
func (l *Ledger) Reserve(ctx context.Context, key CounterKey, limit, amount int64) (*Hold, error) {
	if amount < 0 {
		return nil, fmt.Errorf("cluster: negative reservation %d", amount)
	}
	now := l.now()
	key = key.normalize(now)
	if err := key.valid(); err != nil {
		return nil, err
	}

	b, err := l.blockFor(key)
	if err != nil {
		return nil, err
	}
	// The ceiling is a per-call argument, so the block never learns it except
	// here. Stored only when it changes, so the steady state is one atomic load
	// rather than a store.
	if b.limitSeen.Load() != limit {
		b.limitSeen.Store(limit)
	}
	if amount == 0 {
		return &Hold{Key: key, Amount: 0, block: b}, nil
	}

	// Hot path: an atomic compare-and-swap against the block this node already
	// holds. This is the arrangement DESIGN 9.6 asks for, and it is the whole
	// reason a synchronous budget reservation is affordable.
	if b.take(amount, now) {
		return &Hold{Key: key, Amount: amount, block: b}, nil
	}
	// Slow path. It draws and takes in one critical section rather than
	// drawing and then re-entering the hot path, because the two-step version
	// has no progress guarantee: under contention a caller that has just paid
	// for a store round trip loses the units to callers that have not, and
	// eventually reports the limit exhausted when the limit is nowhere near
	// exhausted. Reporting contention as exhaustion is not a cosmetic error --
	// for a budget, exhaustion is terminal and surfaces as a 400.
	if err := l.drawAndTake(ctx, b, limit, amount, now); err != nil {
		return nil, err
	}
	return &Hold{Key: key, Amount: amount, block: b}, nil
}

// Settle records what a request actually cost and returns the difference.
//
// The estimate is an upper bound (DESIGN 6.4), so actual is normally smaller
// and the remainder goes back to the block -- in memory, with no store write,
// because the block was already charged durably when it was drawn. Settlement
// is safe to be late for exactly that reason: the correction only ever releases
// units.
//
// An actual above the estimate is recorded rather than clamped. The units were
// spent, and hiding that would make the ledger lie; it is also why the estimate
// is deliberately pessimistic.
func (l *Ledger) Settle(h *Hold, actual int64) error {
	if h == nil {
		return errors.New("cluster: Settle needs a hold")
	}
	if actual < 0 {
		return fmt.Errorf("cluster: negative settlement %d", actual)
	}
	if !h.done.CompareAndSwap(false, true) {
		return nil
	}
	if refund := h.Amount - actual; refund != 0 && h.block != nil {
		h.block.remaining.Add(refund)
	}
	return nil
}

// Release returns a hold in full.
//
// This is the path for everything that fails before dispatch: a request that
// reserved at the gate and was then refused while waiting for capacity never
// reached an upstream and must cost nothing (DESIGN 6.4, R1-20).
func (l *Ledger) Release(h *Hold) error { return l.Settle(h, 0) }

// blockFor returns this node's block for a key, creating an empty one if it has
// none. An empty block simply fails its first take and falls through to draw.
func (l *Ledger) blockFor(key CounterKey) (*ledgerBlock, error) {
	id := key.String()

	l.mu.RLock()
	b, ok := l.blocks[id]
	closed := l.closed
	l.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}
	if ok {
		return b, nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrClosed
	}
	if b, ok = l.blocks[id]; ok {
		return b, nil
	}
	b = &ledgerBlock{key: key, rowID: rowID(key.leaseScope(), key.ID, key.leaseWindow(), key.Metric.String(), l.nodeID)}
	l.blocks[id] = b
	return b, nil
}

// take is the hot path.
func (b *ledgerBlock) take(amount int64, now time.Time) bool {
	if now.UnixMicro() >= b.expiresUS.Load() {
		// The authority may already have reclaimed this lease. Spending against
		// it would be spending units that have been returned to somebody else,
		// which is the one thing the published overshoot figure assumes cannot
		// happen.
		return false
	}
	for {
		r := b.remaining.Load()
		if r < amount {
			return false
		}
		if b.remaining.CompareAndSwap(r, r-amount) {
			return true
		}
	}
}

// used reports what this block has consumed. It is a snapshot: a spend landing
// between the load and the caller's use of the value is not reflected, which is
// the residue the published overshoot figure accounts for.
func (b *ledgerBlock) used() int64 {
	u := b.drawn - b.remaining.Load()
	if u < 0 {
		return 0
	}
	return minInt64(u, b.drawn)
}

// drawAndTake refills a block from the durable counter and reserves amount from
// it in the same critical section: one transaction, one write, per block of
// units rather than per request.
//
// The custody step is what makes it correct under contention. Before touching
// the store it swaps the block's remainder out into a local, so hot-path takers
// see an empty block for the duration of the round trip and cannot spend the
// units this caller is about to pay for. Without it there is no progress
// guarantee: eight goroutines against one block will hand the drawn units to
// whoever happens to be scheduled next, and the drawer eventually reports an
// exhausted limit that is not exhausted at all.
//
// Everything the block holds is left untouched until the transaction has
// committed, because [conn.withTx] retries a SQLite writer that lost the race
// for the lock -- and a closure that mutated the block would apply its mutation
// once per attempt.
func (l *Ledger) drawAndTake(ctx context.Context, b *ledgerBlock, limit, amount int64, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Somebody else may have refilled while this goroutine waited for the lock.
	if b.take(amount, l.now()) {
		return nil
	}
	// Close may have run while this goroutine waited. Drawing now would write a
	// fresh lease row for a ledger that has already returned everything and has
	// nothing left that will ever renew it.
	l.mu.RLock()
	closed := l.closed
	l.mu.RUnlock()
	if closed {
		return ErrClosed
	}

	held := maxInt64(b.remaining.Swap(0), 0)
	nowUS := store.Micros(now)
	expUS := store.Micros(now.Add(l.ttl))
	key := b.key

	var (
		grant   int64
		reset   bool
		counter int64
	)
	err := l.c.withTx(ctx, func(ctx context.Context, t tx) error {
		grant, reset, counter = 0, false, 0
		if err := t.lock(ctx, "ledger/"+key.String()); err != nil {
			return err
		}

		// Check for this node's own lease row first. If it is gone, the leader
		// has reclaimed it and returned its unspent part to the counter, so
		// this node's local view of what it holds is void and must restart at
		// zero. Carrying it forward would spend units the reclaim gave back.
		//
		// Only the row's existence matters, not its contents: this node is the
		// only writer of its own lease, so its own accounting is the fresher
		// copy of everything except whether the row is still there.
		var present int64
		err := t.queryRow(ctx, `SELECT 1 FROM quota_leases WHERE id = ?`, b.rowID).Scan(&present)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			reset = true
		case err != nil:
			return err
		}

		drawn, have := b.drawn, held
		if reset {
			drawn, have = 0, 0
		}
		need := amount - have // > 0: the initial take already failed

		value, err := l.readCounter(ctx, t, key)
		if err != nil {
			return err
		}
		counter = value
		want := maxInt64(need, l.block)
		grant = want
		if limit > 0 {
			room := limit - value
			if room < need {
				return fmt.Errorf("%w: %s: limit %d, committed %d, needed %d more",
					ErrExhausted, key, limit, value, need)
			}
			grant = minInt64(want, room)
		}
		if grant < need {
			return fmt.Errorf("%w: %s", ErrExhausted, key)
		}

		// Charge the whole block before a unit of it is spent. This ordering is
		// the safety property: a crash now costs an unspent block, never an
		// overspent budget.
		if err := l.addCounter(ctx, t, key, grant, now); err != nil {
			return err
		}

		// Checkpoint what this block has consumed in the same statement that
		// records the new units, so the reclaim of a crashed node returns the
		// unspent part and not the whole lease.
		spent := maxInt64(drawn-have, 0)
		_, err = t.exec(ctx, `
			INSERT INTO quota_leases
			    (id, node_id, scope, scope_key, "window", metric, amount, used, acquired_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
			    amount     = quota_leases.amount + excluded.amount,
			    used       = excluded.used,
			    expires_at = excluded.expires_at`,
			b.rowID, l.nodeID, key.leaseScope(), key.ID, key.leaseWindow(), key.Metric.String(),
			grant, spent, nowUS, expUS)
		return err
	})
	if err != nil {
		// Give custody back. The units were never spent and the transaction
		// changed nothing, so keeping them would lose them until the lease
		// expired.
		b.remaining.Add(held)
		return err
	}

	if reset {
		b.drawn, b.checkpointed, held = 0, 0, 0
	}
	b.checkpointed = maxInt64(b.drawn-held, 0)
	b.drawn += grant
	// held + grant - amount is non-negative: grant is at least amount - held.
	b.remaining.Add(held + grant - amount)
	b.expiresUS.Store(expUS)
	b.counterAtDraw.Store(counter + grant)
	l.draws.Add(1)
	return nil
}

// LedgerStat is one live block, as this node knows it.
//
// Everything here is this node's view. The durable counter is read only when a
// block is drawn, so Committed is exact at that instant and drifts by whatever
// other nodes spend afterwards; the drift is bounded by the same block size
// DESIGN §5.6 publishes as the overshoot. Reading the true figure needs a store
// round trip, which is not something a scrape may do.
type LedgerStat struct {
	Key CounterKey
	// Limit is the ceiling last reserved against, 0 when none was given.
	Limit int64
	// Committed estimates what has been spent against the counter fleet-wide.
	Committed int64
	// Remaining is what this node still holds unspent from its lease.
	Remaining int64
	// ExpiresAt is when this node's lease lapses. Zero means it holds none.
	ExpiresAt time.Time
}

// Stats reports every live block without touching the store.
//
// It takes only the read lock the hot path takes -- two read locks never
// conflict -- and the per-block mutex, which is held across a store round trip,
// is deliberately not taken. DESIGN §12.3's budget gauge is worth nothing if
// reading it blocks behind the write it is meant to warn about.
func (l *Ledger) Stats() []LedgerStat {
	l.mu.RLock()
	out := make([]LedgerStat, 0, len(l.blocks))
	for _, b := range l.blocks {
		rem := b.remaining.Load()
		st := LedgerStat{
			Key:       b.key,
			Limit:     b.limitSeen.Load(),
			Committed: maxInt64(b.counterAtDraw.Load()-rem, 0),
			Remaining: maxInt64(rem, 0),
		}
		if us := b.expiresUS.Load(); us > 0 {
			st.ExpiresAt = time.UnixMicro(us).UTC()
		}
		out = append(out, st)
	}
	l.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Key.String() < out[j].Key.String() })
	return out
}

// Maintain renews the leases that are approaching expiry and checkpoints the
// ones whose consumption has moved. It is per-node work, not leader work: a
// node renews its own leases, and [Node.Tick] calls this.
//
// Renewing ahead of expiry is what keeps the hot path's expiry check from ever
// firing on a healthy node. When it does fire, the node has stopped ticking --
// which is exactly the case the check exists for.
//
// Checkpointing on the tick rather than only on a refill is what makes the
// residue small in practice: after a crash, a reclaim returns more than it
// should by exactly the consumption since the last checkpoint. Bounding that by
// a tick instead of by a block does not change the published figure -- a node
// that never ticked at all is still bounded only by the block -- but it is the
// difference between a bound and a typical case, and the write costs one UPDATE
// per active counter per tick, which is not per request.
func (l *Ledger) Maintain(ctx context.Context) error {
	now := l.now()
	l.mu.RLock()
	blocks := make([]*ledgerBlock, 0, len(l.blocks))
	for _, b := range l.blocks {
		blocks = append(blocks, b)
	}
	closed := l.closed
	l.mu.RUnlock()
	if closed {
		return ErrClosed
	}

	var firstErr error
	for _, b := range blocks {
		exp := store.TimeAt(b.expiresUS.Load())
		if exp.IsZero() {
			continue
		}
		renew := !now.Add(l.renew).Before(exp)
		if err := l.refreshBlock(ctx, b, now, renew); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// refreshBlock checkpoints one lease's consumption and, when renew is set,
// extends its expiry. It draws no new units, so it cannot fail on an exhausted
// limit.
func (l *Ledger) refreshBlock(ctx context.Context, b *ledgerBlock, now time.Time, renew bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	spent := b.used()
	if !renew && spent == b.checkpointed {
		return nil
	}
	expUS := store.Micros(now.Add(l.ttl))
	q := `UPDATE quota_leases SET used = ? WHERE id = ? AND node_id = ?`
	args := []any{spent, b.rowID, l.nodeID}
	if renew {
		q = `UPDATE quota_leases SET used = ?, expires_at = ? WHERE id = ? AND node_id = ?`
		args = []any{spent, expUS, b.rowID, l.nodeID}
	}
	res, err := l.c.exec(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// The row is gone: this node's lease was reclaimed while it was not
		// looking. Drop the local block rather than keep spending against units
		// that have been given back.
		b.drawn = 0
		b.remaining.Store(0)
		b.checkpointed = 0
		b.expiresUS.Store(0)
		return nil
	}
	b.checkpointed = spent
	if renew {
		b.expiresUS.Store(expUS)
	}
	return nil
}

// Committed reports the durable counter: what a process starting now would see.
// It includes every block this and every other node has drawn, spent or not.
func (l *Ledger) Committed(ctx context.Context, key CounterKey) (int64, error) {
	key = key.normalize(l.now())
	var v int64
	err := l.c.withTx(ctx, func(ctx context.Context, t tx) error {
		var err error
		v, err = l.readCounter(ctx, t, key)
		return err
	})
	return v, err
}

// Available reports how much of limit this node can still spend: what the
// durable counter leaves, plus the part of this node's own block it has drawn
// and not yet spent.
func (l *Ledger) Available(ctx context.Context, key CounterKey, limit int64) (int64, error) {
	v, err := l.Committed(ctx, key)
	if err != nil {
		return 0, err
	}
	key = key.normalize(l.now())
	local := int64(0)
	l.mu.RLock()
	if b, ok := l.blocks[key.String()]; ok {
		local = maxInt64(b.remaining.Load(), 0)
	}
	l.mu.RUnlock()
	return maxInt64(limit-v, 0) + local, nil
}

// Checkpoint writes every block's consumption to its lease row without
// renewing, whether or not it has moved. It is what a caller runs before a
// risky operation, and it is the difference between a reclaim that returns the
// unspent part and one that returns the whole lease.
func (l *Ledger) Checkpoint(ctx context.Context) error {
	l.mu.RLock()
	blocks := make([]*ledgerBlock, 0, len(l.blocks))
	for _, b := range l.blocks {
		blocks = append(blocks, b)
	}
	l.mu.RUnlock()

	now := l.now()
	var firstErr error
	for _, b := range blocks {
		b.mu.Lock()
		b.checkpointed = -1 // force the write even if nothing has moved
		b.mu.Unlock()
		if err := l.refreshBlock(ctx, b, now, false); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close returns every unspent unit and drops this node's lease rows.
//
// This is the draining path of DESIGN 13, and it is what makes a planned
// restart exact: the counter ends up holding precisely what was spent, so the
// next process reads the true figure rather than a conservative one. A crash
// skips this, which is what [Ledger.ReclaimExpired] is for -- and the
// difference between the two is the whole of the published overshoot.
func (l *Ledger) Close(ctx context.Context) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	blocks := make([]*ledgerBlock, 0, len(l.blocks))
	for _, b := range l.blocks {
		blocks = append(blocks, b)
	}
	l.blocks = map[string]*ledgerBlock{}
	l.mu.Unlock()

	now := l.now()
	var firstErr error
	for _, b := range blocks {
		if err := l.returnBlock(ctx, b, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (l *Ledger) returnBlock(ctx context.Context, b *ledgerBlock, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Stop the hot path before touching the store: a unit taken after the
	// remainder is computed would be a unit spent from a block that no longer
	// exists.
	b.expiresUS.Store(0)
	unspent := maxInt64(b.remaining.Swap(0), 0)
	b.drawn = 0
	if unspent == 0 {
		_, err := l.c.exec(ctx, `DELETE FROM quota_leases WHERE id = ? AND node_id = ?`, b.rowID, l.nodeID)
		return err
	}
	return l.c.withTx(ctx, func(ctx context.Context, t tx) error {
		if err := t.lock(ctx, "ledger/"+b.key.String()); err != nil {
			return err
		}
		res, err := t.exec(ctx, `DELETE FROM quota_leases WHERE id = ? AND node_id = ?`, b.rowID, l.nodeID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Already reclaimed; the units are back in the counter and
			// returning them again would create budget out of nothing.
			return nil
		}
		return l.addCounter(ctx, t, b.key, -unspent, now)
	})
}

// ReclaimResult is what one reclaim pass did.
type ReclaimResult struct {
	// Leases is how many lease rows were reclaimed.
	Leases int
	// Returned is how many units were returned to the durable counters.
	Returned int64
}

// ReclaimExpired returns the unspent part of every block lease whose TTL has
// passed, and deletes the rows.
//
// This is leader work (DESIGN 13): a lease that outlives its node has to be
// reclaimed by something, and a lease that is never reclaimed is a budget that
// shrinks by one block every time a node dies. It is the same safety net
// DESIGN 5.3 gives capacity reservations and DESIGN 6.4 gives budget
// reservations; all three are the same pattern and now have the same net.
func (l *Ledger) ReclaimExpired(ctx context.Context, now time.Time) (ReclaimResult, error) {
	// The same expiry predicate goes on the delete, not only on the select.
	// Between the two, the holder may have renewed -- it is alive after all,
	// just slow -- and deleting a renewed lease would return units its holder
	// is still spending against, which is the one case the published overshoot
	// figure assumes cannot happen.
	cutoff := store.Micros(now)
	return l.reclaim(ctx, now,
		`SELECT id, scope, scope_key, "window", metric, amount, used FROM quota_leases
		  WHERE scope <> ? AND expires_at <= ?`,
		[]any{leaseScopeShared, cutoff},
		` AND expires_at <= ?`, []any{cutoff})
}

// ReclaimNode returns the unspent blocks of a node whose heartbeat has lapsed,
// whether or not its leases have expired.
//
// Waiting for a TTL that the dead node's own writes were extending would keep
// its budget out of circulation for as long as it was alive, and a node that is
// gone is not coming back to return it.
func (l *Ledger) ReclaimNode(ctx context.Context, nodeID string, now time.Time) (ReclaimResult, error) {
	if nodeID == "" || nodeID == l.nodeID {
		// Reclaiming this node's own leases would pull the block out from under
		// its own hot path. A node releases its own leases through Close.
		return ReclaimResult{}, nil
	}
	// The delete is guarded by node_id rather than by expiry: the node is gone,
	// so the row cannot legitimately have been renewed, but it could have been
	// taken over by a restarted node of the same name -- in which case the
	// guard still holds and the reclaim is correct either way.
	return l.reclaim(ctx, now,
		`SELECT id, scope, scope_key, "window", metric, amount, used FROM quota_leases
		  WHERE scope <> ? AND node_id = ?`,
		[]any{leaseScopeShared, nodeID},
		` AND node_id = ?`, []any{nodeID})
}

type reclaimable struct {
	id      string
	key     CounterKey
	unspent int64
}

func (l *Ledger) reclaim(ctx context.Context, now time.Time, q string, args []any,
	guard string, guardArgs []any) (ReclaimResult, error) {
	rows, err := l.c.query(ctx, q, args...)
	if err != nil {
		return ReclaimResult{}, err
	}
	var todo []reclaimable
	for rows.Next() {
		var (
			id, scope, scopeKey, window, metric string
			amount, used                        int64
		)
		if err := rows.Scan(&id, &scope, &scopeKey, &window, &metric, &amount, &used); err != nil {
			rows.Close()
			return ReclaimResult{}, err
		}
		key, err := decodeLeaseKey(scope, scopeKey, window, metric)
		if err != nil {
			// A row this package did not write, or wrote in an older shape.
			// Leaving it alone is the safe answer: guessing which counter it
			// belongs to would move somebody's money.
			continue
		}
		todo = append(todo, reclaimable{id: id, key: key, unspent: maxInt64(amount-used, 0)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ReclaimResult{}, err
	}

	var out ReclaimResult
	for _, r := range todo {
		err := l.c.withTx(ctx, func(ctx context.Context, t tx) error {
			if err := t.lock(ctx, "ledger/"+r.key.String()); err != nil {
				return err
			}
			// Delete first and act only if this pass is the one that removed
			// the row. Two leaders overlapping across a handover would
			// otherwise both return the same units.
			res, err := t.exec(ctx, `DELETE FROM quota_leases WHERE id = ?`+guard,
				append([]any{r.id}, guardArgs...)...)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
			out.Leases++
			out.Returned += r.unspent
			if r.unspent == 0 {
				return nil
			}
			return l.addCounter(ctx, t, r.key, -r.unspent, now)
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func decodeLeaseKey(scope, scopeKey, window, metric string) (CounterKey, error) {
	kindName, rest, ok := strings.Cut(scope, ":")
	if !ok {
		return CounterKey{}, fmt.Errorf("cluster: lease scope %q has no kind", scope)
	}
	var kind CounterKind
	switch kindName {
	case "budget":
		kind = CounterBudget
	case "quota":
		kind = CounterQuota
	default:
		return CounterKey{}, fmt.Errorf("cluster: unknown lease kind %q", kindName)
	}
	w, start, err := parseLeaseWindow(window)
	if err != nil {
		return CounterKey{}, err
	}
	m, err := quota.ParseMetric(metric)
	if err != nil {
		return CounterKey{}, err
	}
	return CounterKey{Kind: kind, Scope: rest, ID: scopeKey, Window: w, Metric: m, PeriodStart: start}, nil
}

// ---------------------------------------------------------------------------
// The two durable counters
// ---------------------------------------------------------------------------

// readCounter returns the committed value of a counter.
//
// For a budget it is spent plus reserved: internal/store's synchronous
// ReserveBudget writes reserved_nano, and a block drawn without counting it
// would let the two mechanisms hand out the same money.
func (l *Ledger) readCounter(ctx context.Context, t tx, k CounterKey) (int64, error) {
	var v int64
	var err error
	if k.Kind == CounterBudget {
		err = t.queryRow(ctx, `
			SELECT spent_nano + reserved_nano FROM budget_state
			 WHERE subject_kind = ? AND subject_id = ? AND period = ? AND period_start = ?`,
			k.Scope, k.ID, k.Window.String(), store.Micros(k.PeriodStart)).Scan(&v)
	} else {
		err = t.queryRow(ctx, `
			SELECT value FROM quota_buckets
			 WHERE scope = ? AND scope_key = ? AND "window" = ? AND metric = ? AND bucket_start = ?`,
			k.Scope, k.ID, k.Window.String(), k.Metric.String(), store.Micros(k.PeriodStart)).Scan(&v)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// addCounter applies a delta, flooring the stored value at zero. A negative
// delta is a reclaim or a graceful return; flooring stops a double return from
// creating budget that never existed.
func (l *Ledger) addCounter(ctx context.Context, t tx, k CounterKey, delta int64, now time.Time) error {
	if delta == 0 {
		return nil
	}
	g := greatest(l.c.dia)
	seed := maxInt64(delta, 0)
	nowUS := store.Micros(now)

	if k.Kind == CounterBudget {
		_, err := t.exec(ctx, `
			INSERT INTO budget_state
			    (subject_kind, subject_id, period, period_start, spent_nano, reserved_nano, reserved_until, updated_at)
			VALUES (?, ?, ?, ?, ?, 0, NULL, ?)
			ON CONFLICT (subject_kind, subject_id, period, period_start) DO UPDATE SET
			    spent_nano = `+g+`(budget_state.spent_nano + ?, 0),
			    updated_at = ?`,
			k.Scope, k.ID, k.Window.String(), store.Micros(k.PeriodStart), seed, nowUS,
			delta, nowUS)
		return err
	}
	_, err := t.exec(ctx, `
		INSERT INTO quota_buckets (scope, scope_key, "window", metric, bucket_start, value, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (scope, scope_key, "window", metric, bucket_start) DO UPDATE SET
		    value      = `+g+`(quota_buckets.value + ?, 0),
		    updated_at = ?`,
		k.Scope, k.ID, k.Window.String(), k.Metric.String(), store.Micros(k.PeriodStart), seed, nowUS,
		delta, nowUS)
	return err
}
