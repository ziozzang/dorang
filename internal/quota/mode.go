package quota

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode is how quota is coordinated across nodes. It is the same vocabulary
// capacity uses (DESIGN §5.6, §6.3), deliberately: quota and concurrency now
// have one accuracy vocabulary rather than two.
type Mode uint8

const (
	// ModeLocal counts per node. Exact on one node; N nodes overshoot by
	// (N-1) × limit, because each node carries the whole limit.
	ModeLocal Mode = iota
	// ModeSharedRedis coordinates through an atomic shared counter in Redis.
	// Exact, at one round trip on the hot path.
	ModeSharedRedis
	// ModeSharedPG coordinates through a shared counter in PostgreSQL. Exact,
	// at a costlier round trip.
	ModeSharedPG
	// ModeLeased leases a block from a shared authority and decrements it
	// locally. Bounded overshoot: block × (nodes − 1).
	ModeLeased
)

// String returns the configuration spelling.
func (m Mode) String() string {
	switch m {
	case ModeLocal:
		return "local"
	case ModeSharedRedis:
		return "shared-redis"
	case ModeSharedPG:
		return "shared-pg"
	case ModeLeased:
		return "leased"
	}
	return "unknown"
}

// ParseMode decodes a configured quota_mode / capacity_mode value.
func ParseMode(s string) (Mode, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "local":
		return ModeLocal, nil
	case "shared-redis":
		return ModeSharedRedis, nil
	case "shared-pg":
		return ModeSharedPG, nil
	case "leased":
		return ModeLeased, nil
	}
	return 0, fmt.Errorf("quota: unknown mode %q", s)
}

// Coordinator errors.
var (
	// ErrLocalInCluster refuses cluster.enabled with a local mode. Revision 1
	// of the design called this a recommendation; it is not (DESIGN §5.6).
	// Silently exceeding a provider's plan limit produces upstream 429s, which
	// cascade into the fallback chain and consume the capacity of unrelated
	// models — a failure that surfaces far from its cause.
	ErrLocalInCluster = errors.New(
		"quota: cluster.enabled with quota mode local is refused: " +
			"every node would carry the whole limit; use leased, shared-redis or shared-pg")
	// ErrSharedStoreRequired reports a shared or leased mode with no shared
	// store to coordinate through.
	ErrSharedStoreRequired = errors.New("quota: this mode requires a SharedStore")
	// ErrLimitTooSmall reports a limit below MinLeasable under the leased
	// mode. Single-digit limits cannot be usefully divided across nodes
	// (DESIGN §5.6).
	ErrLimitTooSmall = errors.New("quota: limit is too small to lease; use a shared mode")
)

// DefaultMinLeasable is the smallest limit the leased mode will divide.
const DefaultMinLeasable int64 = 16

// DefaultBlockSize is how much quota a node leases at a time.
const DefaultBlockSize int64 = 16

// DefaultLeaseTTL is how long a lease is valid without renewal.
const DefaultLeaseTTL = 30 * time.Second

// Key identifies one counted quota bucket across nodes.
//
// PeriodStart is part of the identity, which is what makes a shared counter
// reset in step on every node when a period rolls over.
type Key struct {
	// Scope is "credential", "key", "user", "team" or "global".
	Scope string
	// ScopeKey is the id within the scope.
	ScopeKey string
	Window   Window
	Metric   Metric
	// PeriodStart is Window.PeriodStart(now) at the time of the charge.
	PeriodStart time.Time
}

// NewKey builds a Key for now.
func NewKey(scope, scopeKey string, w Window, m Metric, now time.Time) Key {
	return Key{Scope: scope, ScopeKey: scopeKey, Window: w, Metric: m, PeriodStart: w.PeriodStart(now)}
}

// String is the shared-store key. It is stable across nodes and processes.
//
// It is built by hand rather than with fmt: a charge computes it once per
// request, and formatting through interfaces would allocate several times for
// a string this simple.
func (k Key) String() string {
	var b strings.Builder
	b.Grow(len(k.Scope) + len(k.ScopeKey) + 32)
	b.WriteString("q/")
	b.WriteString(k.Scope)
	b.WriteByte('/')
	b.WriteString(k.ScopeKey)
	b.WriteByte('/')
	b.WriteString(k.Window.String())
	b.WriteByte('/')
	b.WriteString(k.Metric.String())
	b.WriteByte('/')
	b.WriteString(strconv.FormatInt(k.PeriodStart.UTC().Unix(), 10))
	return b.String()
}

// Grant is the answer to a charge.
type Grant struct {
	// OK reports whether the units may be spent.
	OK bool
	// Remaining is how much of the limit is left after the charge, as far as
	// this coordinator can tell.
	Remaining int64
}

// Coordinator admits or refuses quota spend across nodes.
//
// Every implementation reports its maximum possible overshoot as a number.
// "Approximately accurate" is not an acceptable specification (DESIGN §5.6).
type Coordinator interface {
	// Mode returns the coordination mode.
	Mode() Mode

	// MaxOvershoot returns the largest number of metric units that all nodes
	// together can spend beyond limit. It returns an error when this mode
	// cannot coordinate that limit at all.
	MaxOvershoot(limit int64, nodes int) (int64, error)

	// Charge asks to spend n units against limit for k. All or nothing.
	Charge(ctx context.Context, k Key, limit, n int64, now time.Time) (Grant, error)

	// Refund returns units that were charged but not spent.
	Refund(ctx context.Context, k Key, n int64, now time.Time) error

	// Used reports the coordinator's view of the units spent for k.
	Used(ctx context.Context, k Key, now time.Time) (int64, error)

	// Close releases anything held on behalf of this node, including any
	// unspent lease.
	Close() error
}

// SharedStore is the atomic counter the shared and leased modes coordinate
// through. Redis (Lua) and PostgreSQL (advisory lock or upsert) implement it;
// [MemShared] implements it in memory.
//
// Reserve must be atomic: the whole point of the shared modes is that two
// nodes cannot both see room for the last unit.
type SharedStore interface {
	// Reserve grants up to want units against limit for key, and returns how
	// many were granted — zero when the limit is already reached. ttl is how
	// long the counter survives without further activity.
	Reserve(ctx context.Context, key string, want, limit int64, ttl time.Duration) (int64, error)
	// Release returns units to the counter.
	Release(ctx context.Context, key string, n int64) error
	// Value reports the counter.
	Value(ctx context.Context, key string) (int64, error)
}

// CoordinatorConfig configures [NewCoordinator].
type CoordinatorConfig struct {
	// Mode selects the coordination mode.
	Mode Mode
	// Clustered mirrors cluster.enabled. With ModeLocal it is refused.
	Clustered bool
	// Nodes is how many nodes are expected to share a limit. It is used to
	// report overshoot, not to enforce anything.
	Nodes int
	// NodeID names this node in lease keys.
	NodeID string
	// Shared is the coordination backend for the shared and leased modes.
	Shared SharedStore
	// BlockSize is how much a leased node takes at a time.
	BlockSize int64
	// MinLeasable is the smallest limit the leased mode will divide.
	MinLeasable int64
	// LeaseTTL is how long a lease is valid.
	LeaseTTL time.Duration
	// Now overrides the clock.
	Now func() time.Time
}

// NewCoordinator builds the coordinator for a mode.
func NewCoordinator(cfg CoordinatorConfig) (Coordinator, error) {
	if cfg.Clustered && cfg.Mode == ModeLocal {
		return nil, ErrLocalInCluster
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	nodes := cfg.Nodes
	if nodes < 1 {
		nodes = 1
	}
	switch cfg.Mode {
	case ModeLocal:
		return &localCoordinator{nodes: nodes, counters: map[string]*counter{}, now: now}, nil
	case ModeSharedRedis, ModeSharedPG:
		if cfg.Shared == nil {
			return nil, fmt.Errorf("%w: mode %s", ErrSharedStoreRequired, cfg.Mode)
		}
		return &sharedCoordinator{mode: cfg.Mode, shared: cfg.Shared, ttl: orDur(cfg.LeaseTTL, DefaultLeaseTTL)}, nil
	case ModeLeased:
		if cfg.Shared == nil {
			return nil, fmt.Errorf("%w: mode %s", ErrSharedStoreRequired, cfg.Mode)
		}
		return &leasedCoordinator{
			shared:      cfg.Shared,
			nodeID:      cfg.NodeID,
			nodes:       nodes,
			block:       orInt(cfg.BlockSize, DefaultBlockSize),
			minLeasable: orInt(cfg.MinLeasable, DefaultMinLeasable),
			ttl:         orDur(cfg.LeaseTTL, DefaultLeaseTTL),
			leases:      map[string]*lease{},
			now:         now,
		}, nil
	}
	return nil, fmt.Errorf("quota: unknown mode %v", cfg.Mode)
}

func orDur(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func orInt(v, def int64) int64 {
	if v <= 0 {
		return def
	}
	return v
}

// ---------------------------------------------------------------- local ---

// localCoordinator counts on this node only.
type localCoordinator struct {
	mu       sync.Mutex
	counters map[string]*counter
	nodes    int
	now      func() time.Time
}

func (c *localCoordinator) Mode() Mode { return ModeLocal }

// MaxOvershoot is (nodes − 1) × limit: every node carries the whole limit, so
// N nodes admit N × limit in the worst case.
func (c *localCoordinator) MaxOvershoot(limit int64, nodes int) (int64, error) {
	if nodes < 1 {
		nodes = c.nodes
	}
	if nodes <= 1 {
		return 0, nil
	}
	return limit * int64(nodes-1), nil
}

func (c *localCoordinator) counterFor(k Key, now time.Time) (*counter, error) {
	id := k.String()
	if ct, ok := c.counters[id]; ok {
		return ct, nil
	}
	ct, err := newCounter(k.Window, now)
	if err != nil {
		return nil, err
	}
	c.counters[id] = ct
	return ct, nil
}

func (c *localCoordinator) Charge(_ context.Context, k Key, limit, n int64, now time.Time) (Grant, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ct, err := c.counterFor(k, now)
	if err != nil {
		return Grant{}, err
	}
	used := ct.sum(now)
	if used+n > limit {
		return Grant{OK: false, Remaining: max64(limit-used, 0)}, nil
	}
	ct.add(now, n)
	return Grant{OK: true, Remaining: limit - used - n}, nil
}

func (c *localCoordinator) Refund(_ context.Context, k Key, n int64, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ct, err := c.counterFor(k, now)
	if err != nil {
		return err
	}
	ct.add(now, -n)
	return nil
}

func (c *localCoordinator) Used(_ context.Context, k Key, now time.Time) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ct, err := c.counterFor(k, now)
	if err != nil {
		return 0, err
	}
	return ct.sum(now), nil
}

func (c *localCoordinator) Close() error { return nil }

// --------------------------------------------------------------- shared ---

// sharedCoordinator charges the shared counter directly: one round trip, no
// overshoot. The Redis and PostgreSQL modes differ only in which SharedStore
// they are given, which is why they share this implementation.
type sharedCoordinator struct {
	mode   Mode
	shared SharedStore
	ttl    time.Duration
}

func (c *sharedCoordinator) Mode() Mode { return c.mode }

// MaxOvershoot is zero: every charge is atomic at the shared authority.
func (c *sharedCoordinator) MaxOvershoot(int64, int) (int64, error) { return 0, nil }

func (c *sharedCoordinator) Charge(ctx context.Context, k Key, limit, n int64, _ time.Time) (Grant, error) {
	id := k.String()
	got, err := c.shared.Reserve(ctx, id, n, limit, c.ttl)
	if err != nil {
		return Grant{}, err
	}
	if got < n {
		// Partial grants are useless for an all-or-nothing charge; give them
		// back rather than leaving them stranded.
		if got > 0 {
			_ = c.shared.Release(ctx, id, got)
		}
		return Grant{OK: false}, nil
	}
	v, err := c.shared.Value(ctx, id)
	if err != nil {
		return Grant{OK: true}, nil
	}
	return Grant{OK: true, Remaining: max64(limit-v, 0)}, nil
}

func (c *sharedCoordinator) Refund(ctx context.Context, k Key, n int64, _ time.Time) error {
	return c.shared.Release(ctx, k.String(), n)
}

func (c *sharedCoordinator) Used(ctx context.Context, k Key, _ time.Time) (int64, error) {
	return c.shared.Value(ctx, k.String())
}

func (c *sharedCoordinator) Close() error { return nil }

// --------------------------------------------------------------- leased ---

type lease struct {
	remaining int64
	expires   time.Time
}

// leasedCoordinator takes a block of quota from the shared authority and
// decrements it locally, so the hot path is local after the first charge of a
// block.
type leasedCoordinator struct {
	mu          sync.Mutex
	shared      SharedStore
	leases      map[string]*lease
	nodeID      string
	nodes       int
	block       int64
	minLeasable int64
	ttl         time.Duration
	now         func() time.Time
	closed      bool
}

func (c *leasedCoordinator) Mode() Mode { return ModeLeased }

// MaxOvershoot is block × (nodes − 1).
//
// Leases are taken from the shared counter, so the units handed out never
// exceed the limit while every lease is live. Overshoot comes from expiry: a
// lease that outlives its TTL is reclaimed by the authority, and the units its
// holder already spent are no longer reflected anywhere. At most one expired
// block per other node can be in that state at once, which is where the number
// comes from. It is also why small limits are refused: with a limit of 5 and a
// block of 16, one node would hold everything.
//
// This implementation never spends against an expired lease, so the real
// overshoot is at or below the number reported here — which is what "maximum
// possible" has to mean for the number to be worth publishing.
func (c *leasedCoordinator) MaxOvershoot(limit int64, nodes int) (int64, error) {
	if limit < c.minLeasable {
		return 0, fmt.Errorf("%w: limit %d is below min_leasable %d", ErrLimitTooSmall, limit, c.minLeasable)
	}
	if nodes < 1 {
		nodes = c.nodes
	}
	if nodes <= 1 {
		return 0, nil
	}
	return c.block * int64(nodes-1), nil
}

func (c *leasedCoordinator) Charge(ctx context.Context, k Key, limit, n int64, now time.Time) (Grant, error) {
	if limit < c.minLeasable {
		return Grant{}, fmt.Errorf("%w: limit %d is below min_leasable %d", ErrLimitTooSmall, limit, c.minLeasable)
	}
	if n <= 0 {
		return Grant{OK: true}, nil
	}
	id := k.String()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Grant{}, errors.New("quota: coordinator is closed")
	}
	l := c.leases[id]
	if l != nil && !now.Before(l.expires) {
		// The authority may already have reclaimed this lease; do not spend
		// against it and do not release it either, or the units would be
		// returned twice.
		delete(c.leases, id)
		l = nil
	}
	if l == nil {
		l = &lease{}
		c.leases[id] = l
	}
	if l.remaining < n {
		want := c.block
		if n > want {
			want = n
		}
		got, err := c.shared.Reserve(ctx, id, want, limit, c.ttl)
		if err != nil {
			return Grant{}, err
		}
		l.remaining += got
		if got > 0 {
			l.expires = now.Add(c.ttl)
		}
	}
	if l.remaining < n {
		return Grant{OK: false, Remaining: l.remaining}, nil
	}
	l.remaining -= n
	return Grant{OK: true, Remaining: l.remaining}, nil
}

// Refund returns units to this node's lease. They were never spent, and the
// lease still holds them at the authority.
func (c *leasedCoordinator) Refund(_ context.Context, k Key, n int64, now time.Time) error {
	id := k.String()
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.leases[id]
	if l == nil || !now.Before(l.expires) {
		return nil // the lease is gone; the authority will reclaim the block
	}
	l.remaining += n
	return nil
}

func (c *leasedCoordinator) Used(ctx context.Context, k Key, _ time.Time) (int64, error) {
	v, err := c.shared.Value(ctx, k.String())
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if l := c.leases[k.String()]; l != nil {
		// Units leased but not yet spent are not used.
		v -= l.remaining
	}
	return max64(v, 0), nil
}

// LeaseRemaining reports the unspent part of this node's lease.
func (c *leasedCoordinator) LeaseRemaining(k Key) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l := c.leases[k.String()]; l != nil {
		return l.remaining
	}
	return 0
}

// Close returns every unspent lease to the authority.
func (c *leasedCoordinator) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	now := c.now()
	ctx := context.Background()
	var firstErr error
	for id, l := range c.leases {
		if l.remaining > 0 && now.Before(l.expires) {
			if err := c.shared.Release(ctx, id, l.remaining); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		delete(c.leases, id)
	}
	return firstErr
}

// ------------------------------------------------------------ MemShared ---

// MemShared is an in-memory SharedStore. It is exact and is what the tests
// coordinate through; a single-process deployment can also use it to run the
// shared or leased paths without a Redis.
type MemShared struct {
	mu sync.Mutex
	v  map[string]*memEntry
	// Now overrides the clock.
	Now func() time.Time
}

type memEntry struct {
	value   int64
	expires time.Time
}

// NewMemShared builds an in-memory shared store.
func NewMemShared(now func() time.Time) *MemShared {
	if now == nil {
		now = time.Now
	}
	return &MemShared{v: map[string]*memEntry{}, Now: now}
}

// Reserve implements SharedStore.
func (m *MemShared) Reserve(_ context.Context, key string, want, limit int64, ttl time.Duration) (int64, error) {
	if want <= 0 {
		return 0, nil
	}
	now := m.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entry(key, now)
	room := limit - e.value
	if room <= 0 {
		return 0, nil
	}
	grant := want
	if grant > room {
		grant = room
	}
	e.value += grant
	e.expires = now.Add(ttl)
	return grant, nil
}

// Release implements SharedStore.
func (m *MemShared) Release(_ context.Context, key string, n int64) error {
	if n <= 0 {
		return nil
	}
	now := m.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entry(key, now)
	e.value = max64(e.value-n, 0)
	return nil
}

// Value implements SharedStore.
func (m *MemShared) Value(_ context.Context, key string) (int64, error) {
	now := m.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entry(key, now).value, nil
}

// SweepExpired drops counters no node has touched within their TTL. A real
// backend does this with key expiry; doing it here keeps the in-memory store
// honest about the same lifecycle.
func (m *MemShared) SweepExpired(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, e := range m.v {
		if !e.expires.IsZero() && !now.Before(e.expires) {
			delete(m.v, k)
			n++
		}
	}
	return n
}

// entry fetches or creates a counter, dropping it first if it has expired.
func (m *MemShared) entry(key string, now time.Time) *memEntry {
	e, ok := m.v[key]
	if ok && !e.expires.IsZero() && !now.Before(e.expires) {
		delete(m.v, key)
		ok = false
	}
	if !ok {
		e = &memEntry{}
		m.v[key] = e
	}
	return e
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
