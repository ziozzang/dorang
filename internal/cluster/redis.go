package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
)

// Script names one of the three atomic operations the shared-redis mode needs.
//
// They are named rather than passed as Lua text so that a client cannot quietly
// substitute a different script, and so that an implementation which is not
// Redis at all -- [MemRedis] -- can implement the same contract honestly
// instead of pretending to run Lua.
type Script string

// The scripts of DESIGN 5.6's "atomic Lua acquire/release with lease TTL".
const (
	// ScriptReserve grants up to ARGV[1] units against the ceiling ARGV[2],
	// refreshes the key's TTL to ARGV[3] milliseconds, and returns how many
	// units it granted. Zero means the ceiling was already reached.
	ScriptReserve Script = "reserve"
	// ScriptRelease returns ARGV[1] units, flooring the counter at zero, and
	// returns the new value.
	ScriptRelease Script = "release"
	// ScriptValue returns the counter, or zero if the key does not exist.
	ScriptValue Script = "value"
)

// The Lua source of each script, exported so that a real client is an adapter
// rather than a reimplementation.
//
// Atomicity is the whole point. Redis runs a script as one unit, so the read,
// the ceiling test and the write in LuaReserve cannot interleave with another
// node's -- which is what makes this mode exact. A client that split it into
// GET, compare, SET would reintroduce precisely the race the mode exists to
// remove, and would do it silently, since the result is correct until it is
// contended.
const (
	// LuaReserve is the acquire half.
	LuaReserve = `
local v     = tonumber(redis.call('GET', KEYS[1]) or '0')
local want  = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local ttl   = tonumber(ARGV[3])
local room  = limit - v
if room <= 0 then return 0 end
if want > room then want = room end
redis.call('SET', KEYS[1], v + want, 'PX', ttl)
return want
`
	// LuaRelease is the release half. It floors at zero: a release that arrives
	// after the key's TTL dropped the counter must not drive it negative, which
	// would hand out units nobody ever had.
	LuaRelease = `
local v = tonumber(redis.call('GET', KEYS[1]) or '0')
local n = tonumber(ARGV[1])
v = v - n
if v < 0 then v = 0 end
redis.call('SET', KEYS[1], v, 'KEEPTTL')
return v
`
	// LuaValue reads the counter.
	LuaValue = `
return tonumber(redis.call('GET', KEYS[1]) or '0')
`
)

// RedisClient is the entire surface the shared-redis mode needs: evaluate one
// of the [Script]s atomically against one key.
//
// It is this small on purpose. dorang ships shared-redis without depending on a
// Redis client library, because a dependency taken for three commands is a
// dependency taken for its whole release cadence and its whole CVE surface. A
// deployment that wants real Redis writes a dozen-line adapter over the client
// it already has; [MemRedis] is a complete implementation for a single process
// and is what the tests coordinate through.
type RedisClient interface {
	// Eval runs script against key with the given integer arguments and returns
	// its integer result. Implementations must apply the script atomically with
	// respect to every other Eval on the same key.
	Eval(ctx context.Context, script Script, key string, args ...int64) (int64, error)
}

// RedisShared adapts a [RedisClient] to [quota.SharedStore], which is what
// makes shared-redis a configuration of the same coordinator as shared-pg
// rather than a second implementation of quota accounting.
type RedisShared struct {
	client RedisClient
}

// NewRedisShared wraps a client.
func NewRedisShared(c RedisClient) (*RedisShared, error) {
	if c == nil {
		return nil, errors.New("cluster: NewRedisShared needs a client")
	}
	return &RedisShared{client: c}, nil
}

var _ quota.SharedStore = (*RedisShared)(nil)

// Reserve implements [quota.SharedStore].
func (r *RedisShared) Reserve(ctx context.Context, key string, want, limit int64, ttl time.Duration) (int64, error) {
	if want <= 0 {
		return 0, nil
	}
	ms := ttl.Milliseconds()
	if ms <= 0 {
		return 0, errors.New("cluster: Reserve needs a TTL of at least one millisecond")
	}
	n, err := r.client.Eval(ctx, ScriptReserve, key, want, limit, ms)
	if err != nil {
		return 0, fmt.Errorf("cluster: redis reserve %s: %w", key, err)
	}
	return n, nil
}

// Release implements [quota.SharedStore].
func (r *RedisShared) Release(ctx context.Context, key string, n int64) error {
	if n <= 0 {
		return nil
	}
	if _, err := r.client.Eval(ctx, ScriptRelease, key, n); err != nil {
		return fmt.Errorf("cluster: redis release %s: %w", key, err)
	}
	return nil
}

// Value implements [quota.SharedStore].
func (r *RedisShared) Value(ctx context.Context, key string) (int64, error) {
	v, err := r.client.Eval(ctx, ScriptValue, key)
	if err != nil {
		return 0, fmt.Errorf("cluster: redis value %s: %w", key, err)
	}
	return v, nil
}

// MemRedis is a complete, in-process [RedisClient].
//
// It implements the same semantics as the Lua above -- including key expiry,
// which is not decoration: a counter that never expires is a memory leak keyed
// by every quota window that ever existed, and an implementation that skipped
// it would pass every accuracy test and fail in production a month later.
//
// It is exact and it is what the shared-redis tests coordinate through. It is
// also a legitimate production choice for a single-node deployment that wants
// the shared code path without an external service; it is not a substitute for
// Redis across nodes, because it is per process.
//
// A MemRedis is safe for concurrent use.
type MemRedis struct {
	mu   sync.Mutex
	keys map[string]*memKey
	now  func() time.Time

	// evals counts script evaluations, so a test can assert how much hot-path
	// traffic a mode actually generates rather than assume it.
	evals int64
}

type memKey struct {
	value   int64
	expires time.Time
}

// NewMemRedis builds an in-memory client.
func NewMemRedis(now func() time.Time) *MemRedis {
	if now == nil {
		now = time.Now
	}
	return &MemRedis{keys: map[string]*memKey{}, now: now}
}

// Eval implements [RedisClient].
func (m *MemRedis) Eval(_ context.Context, script Script, key string, args ...int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evals++

	now := m.now()
	e := m.entry(key, now)

	switch script {
	case ScriptReserve:
		if len(args) != 3 {
			return 0, fmt.Errorf("cluster: %s takes 3 arguments, got %d", script, len(args))
		}
		want, limit, ms := args[0], args[1], args[2]
		room := limit - e.value
		if room <= 0 {
			return 0, nil
		}
		want = minInt64(want, room)
		e.value += want
		e.expires = now.Add(time.Duration(ms) * time.Millisecond)
		return want, nil

	case ScriptRelease:
		if len(args) != 1 {
			return 0, fmt.Errorf("cluster: %s takes 1 argument, got %d", script, len(args))
		}
		// KEEPTTL: a release does not extend the key's life. Extending it would
		// let a window that nothing is using stay alive on refunds alone.
		e.value = maxInt64(e.value-args[0], 0)
		return e.value, nil

	case ScriptValue:
		return e.value, nil
	}
	return 0, fmt.Errorf("cluster: unknown script %q", script)
}

// entry fetches or creates a key, dropping it first if it has expired.
func (m *MemRedis) entry(key string, now time.Time) *memKey {
	e, ok := m.keys[key]
	if ok && !e.expires.IsZero() && !now.Before(e.expires) {
		delete(m.keys, key)
		ok = false
	}
	if !ok {
		e = &memKey{}
		m.keys[key] = e
	}
	return e
}

// SweepExpired drops expired keys and reports how many. Redis does this itself;
// doing it here keeps the in-memory client honest about the same lifecycle
// rather than accumulating every key it has ever seen.
func (m *MemRedis) SweepExpired(now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, e := range m.keys {
		if !e.expires.IsZero() && !now.Before(e.expires) {
			delete(m.keys, k)
			n++
		}
	}
	return n
}

// Evals reports how many scripts have been evaluated.
func (m *MemRedis) Evals() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.evals
}

// Len reports how many keys are resident.
func (m *MemRedis) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.keys)
}
