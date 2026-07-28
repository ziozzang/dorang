package keyguard

import (
	"context"
	"sync"
	"time"
)

// History is the usage record the guard reasons over.
//
// It is an interface because where the numbers come from is a deployment
// decision and the guard's arithmetic is not. A single node counts in memory; a
// cluster counts in the ledger, so that the baseline is the fleet's traffic and
// not one node's share of it.
type History interface {
	// Add records n tokens used by keyID at t. It is called off the request
	// path, from the metering queue.
	Add(ctx context.Context, keyID string, n int64, t time.Time) error
	// Window returns the tokens keyID used in [from, to).
	Window(ctx context.Context, keyID string, from, to time.Time) (int64, error)
	// Since returns the instant this key's history begins — the first sample
	// ever recorded for it. It is what rule 3 is decided on: a key with less
	// history than Config.MinHistory has no baseline and can only be alerted
	// about, never acted on.
	//
	// A key with no history at all returns the zero time.
	Since(ctx context.Context, keyID string) (time.Time, error)
	// Keys returns the key ids with any history, which is what a sweep
	// iterates.
	Keys(ctx context.Context) ([]string, error)
}

// MemHistory is a bounded, bucketed, in-process [History].
//
// Samples are accumulated into fixed-width buckets rather than kept
// individually: a guard that stored every request would cost more than the
// traffic it watches, and a rate over a window does not need per-request
// resolution. Buckets older than the retention are dropped on write, so the
// footprint is O(keys x retention/bucket) and not O(requests).
//
// It is exact for one node. In a cluster each node sees only the requests it
// served, so both the observed rate and the baseline are that node's share —
// which is self-consistent (the ratio is roughly preserved) but makes the
// ABSOLUTE condition, the one that stops the guard firing on quiet keys, a
// fraction of the fleet's real number. That is why [Config.MinAbsolute] is
// documented as a per-History figure, and why a clustered deployment wants a
// store-backed History instead. Saying so is better than shipping a number that
// silently means something different on node four.
//
// A MemHistory is safe for concurrent use.
type MemHistory struct {
	bucket    time.Duration
	retention time.Duration
	maxKeys   int

	mu   sync.Mutex
	keys map[string]*keyHistory
}

type keyHistory struct {
	// buckets maps a bucket start (unix nanoseconds) to the tokens in it.
	buckets map[int64]int64
	first   time.Time
	last    time.Time
}

// MemHistory defaults.
const (
	// DefaultBucket is the resolution of the in-memory history. Five minutes
	// over a seven-day retention is 2016 buckets per key, which is small enough
	// to keep and fine enough that an hour's rate is not dominated by a
	// bucket's edge.
	DefaultBucket = 5 * time.Minute
	// DefaultRetention matches §11.6's baseline_window default.
	DefaultRetention = 7 * 24 * time.Hour
	// DefaultMaxKeys bounds the table. Reaching it evicts the least recently
	// used key, which loses that key's baseline and makes it a new key again —
	// which means it can only be alerted about, never pended. Losing history in
	// the safe direction is the only acceptable way to lose it.
	DefaultMaxKeys = 100_000
)

// NewMemHistory builds an in-memory history. Zero values take the defaults.
func NewMemHistory(bucket, retention time.Duration, maxKeys int) *MemHistory {
	if bucket <= 0 {
		bucket = DefaultBucket
	}
	if retention <= 0 {
		retention = DefaultRetention
	}
	if maxKeys <= 0 {
		maxKeys = DefaultMaxKeys
	}
	return &MemHistory{
		bucket:    bucket,
		retention: retention,
		maxKeys:   maxKeys,
		keys:      map[string]*keyHistory{},
	}
}

func (h *MemHistory) bucketOf(t time.Time) int64 {
	n := t.UnixNano()
	b := int64(h.bucket)
	return n - ((n%b)+b)%b // floor division, correct for pre-epoch times too
}

// Add implements [History].
func (h *MemHistory) Add(_ context.Context, keyID string, n int64, t time.Time) error {
	if keyID == "" || n <= 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	k, ok := h.keys[keyID]
	if !ok {
		h.evictLocked()
		k = &keyHistory{buckets: map[int64]int64{}, first: t}
		h.keys[keyID] = k
	}
	if t.Before(k.first) {
		k.first = t
	}
	if t.After(k.last) {
		k.last = t
	}
	k.buckets[h.bucketOf(t)] += n

	cutoff := t.Add(-h.retention).UnixNano()
	for b := range k.buckets {
		if b < cutoff {
			delete(k.buckets, b)
		}
	}
	// first cannot pretend to be older than what is still held. A key whose
	// early buckets aged out has less usable history than its first sample
	// suggests, and rule 3 must be decided on the history that EXISTS.
	if k.first.UnixNano() < cutoff {
		k.first = time.Unix(0, cutoff)
	}
	return nil
}

// evictLocked drops the least recently used key when the table is full.
func (h *MemHistory) evictLocked() {
	if len(h.keys) < h.maxKeys {
		return
	}
	var (
		victim string
		oldest time.Time
	)
	for id, k := range h.keys {
		if victim == "" || k.last.Before(oldest) {
			victim, oldest = id, k.last
		}
	}
	delete(h.keys, victim)
}

// Window implements [History]. Buckets are counted whole when their start falls
// inside [from, to), which is the same convention every window in this codebase
// uses and keeps a rate comparable with itself across calls.
func (h *MemHistory) Window(_ context.Context, keyID string, from, to time.Time) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k, ok := h.keys[keyID]
	if !ok {
		return 0, nil
	}
	lo, hi := from.UnixNano(), to.UnixNano()
	var total int64
	for b, v := range k.buckets {
		if b >= lo && b < hi {
			total += v
		}
	}
	return total, nil
}

// Since implements [History].
func (h *MemHistory) Since(_ context.Context, keyID string) (time.Time, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k, ok := h.keys[keyID]
	if !ok {
		return time.Time{}, nil
	}
	return k.first, nil
}

// Keys implements [History].
func (h *MemHistory) Keys(_ context.Context) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.keys))
	for id := range h.keys {
		out = append(out, id)
	}
	return out, nil
}

// Len reports how many keys are tracked.
func (h *MemHistory) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.keys)
}
