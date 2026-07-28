package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotFound is what a Store returns when no row carries the index key.
var ErrNotFound = errors.New("auth: no key with that lookup")

// ErrMasterKeyRequired is returned by New when no master credential is
// configured and the caller has not explicitly said it meant that.
//
// R1-A: the administrative credential is out-of-band, and a gateway that reads
// only an imported database loses admin authentication entirely. Losing it
// therefore has to be typed out, not defaulted into.
var ErrMasterKeyRequired = errors.New(
	"auth: no master key configured; set Config.MasterKey (server.master_key_env) " +
		"or Config.NoMasterKey to run without an administrative credential")

// Record is one stored credential row, in the shape this package needs. It is
// the narrow interface to internal/store: this package never imports it.
type Record struct {
	// Lookup is the hex index key. It may be empty on the miss path, where the
	// key being looked up is already known.
	Lookup string
	// Digest is the stored token_hash.
	Digest Digest
	// Scheme is the stored hash_scheme.
	Scheme Scheme
	// Principal is the authorization envelope of the row.
	Principal Principal
}

// Store is the one thing this package needs from persistence: resolve an index
// key to a row. It is deliberately a single method so that a cache, an HTTP
// key-management delegate (DESIGN §11.2) or a test double are all trivially
// substitutable.
type Store interface {
	// LoadByLookup returns the row whose lookup index key is the given
	// LookupHexLen-character hex string, or ErrNotFound.
	LoadByLookup(ctx context.Context, lookup string) (Record, error)
}

// Rehasher persists an upgraded digest. A Store that also implements it enables
// rehash-on-use (DESIGN §2.4): a legacy row that verifies is rewritten as
// dorang_v1 asynchronously, so the migration finishes without a flag day.
type Rehasher interface {
	// Rehash stores digest under SchemeDorangV1 for the given key id. It is
	// called off the request path.
	Rehash(ctx context.Context, keyID, lookup string, digest Digest) error
}

// Defaults applied by New for zero Config fields.
const (
	DefaultEntryTTL     = 60 * time.Second
	DefaultNegativeTTL  = 5 * time.Second
	DefaultStoreTimeout = 2 * time.Second
	DefaultRehashQueue  = 256

	// mergeThreshold is how many PROMOTABLE entries may accumulate in the
	// overlay before they are folded into the lock-free snapshot. Folding
	// copies the snapshot, so it is done in batches: a flood of unknown keys
	// must not make every miss cost O(keys).
	//
	// Counting promotable entries rather than all of them is the whole fix.
	// mergeLocked promotes only positive entries and hands the negatives back
	// as the new overlay, so a threshold on len(overlay) was satisfied
	// permanently by 64 negatives: an unknown-key flood then merged on
	// essentially every subsequent miss — roughly 8128 full snapshot copies per
	// 8192 attacker requests, each one an O(loaded keys) map allocation under
	// the exclusive lock, with every legitimate miss queued behind it. The
	// mitigation the comment above promised was the one thing that could not
	// happen.
	mergeThreshold = 64
	// maxOverlay bounds the overlay so that a flood of unknown keys cannot
	// grow memory without limit. Reaching it drops the overlay wholesale;
	// dropped entries are simply re-learned.
	maxOverlay = 8192
	// maxNegative bounds the negative entries alone.
	//
	// They need their own ceiling because they are the half an unauthenticated
	// caller controls: every distinct unknown key is one more, and they are
	// never promoted out. Reaching it drops the negatives and keeps the
	// positives, so a flood costs the attacker's own cache and not the
	// legitimate keys learned beside it.
	maxNegative = 1024
)

// Config configures an Authenticator. It holds key material and redacts itself
// under every fmt verb.
type Config struct {
	// Pepper is server.key_pepper_env's value. Required: dorang_v1 cannot
	// exist without it.
	Pepper string
	// MasterKey is the out-of-band administrative credential, compared in
	// constant time and never stored as a row (R1-A).
	MasterKey string
	// NoMasterKey acknowledges deliberately running without one.
	NoMasterKey bool
	// Legacy is the legacy_sha256 import window (DESIGN §2.4).
	Legacy LegacyPolicy
	// RehashOnUse upgrades legacy rows on successful verification.
	RehashOnUse bool
	// Store resolves index keys that are not in the snapshot. Optional: with
	// no store, only Load'ed keys and the master key authenticate.
	Store Store
	// EntryTTL is how long a row learned at runtime stays cached. Rows placed
	// by Load do not expire; they are replaced by the next Load or dropped by
	// Invalidate, because the snapshot is the authority on the hot path
	// (DESIGN §9.1).
	EntryTTL time.Duration
	// NegativeTTL is how long an unknown index key is remembered as unknown.
	NegativeTTL time.Duration
	// StoreTimeout bounds one store lookup.
	StoreTimeout time.Duration
	// RehashQueue is the depth of the asynchronous upgrade queue.
	RehashQueue int
	// Now overrides the clock, for tests.
	Now func() time.Time
	// MasterKeyID names the master principal in logs and metering.
	MasterKeyID string
}

// String redacts.
func (c Config) String() string { return "auth.Config{pepper:(redacted) master_key:(redacted)}" }

// GoString redacts %#v.
func (c Config) GoString() string { return c.String() }

// Format redacts every other verb.
func (c Config) Format(f fmt.State, verb rune) { writeRedacted(f, verb, c.String()) }

// entry is one cached row. Entries are immutable except for rehashQueued,
// which is an atomic flag, so the hot path reads them without a lock.
type entry struct {
	digest    Digest
	scheme    Scheme
	principal *Principal // nil for a negative entry
	found     bool
	expires   int64 // unix nanoseconds; 0 never expires
	// rehashQueued makes the asynchronous legacy upgrade fire once per cached
	// entry rather than once per request.
	rehashQueued atomic.Bool
}

// live reports whether the entry may still be used at now (unix nanoseconds).
func (e *entry) live(now int64) bool { return e.expires == 0 || now < e.expires }

// Stats is a snapshot of authenticator counters.
type Stats struct {
	Hits          uint64
	Misses        uint64
	StoreCalls    uint64
	Coalesced     uint64
	Rejected      uint64
	MasterHits    uint64
	RehashQueued  uint64
	RehashDropped uint64
	RehashDone    uint64
	SnapshotSize  int
	OverlaySize   int
	// Merges counts how many times the overlay has been folded into a fresh
	// snapshot. Each fold copies the whole snapshot under the exclusive lock,
	// so this is the number an unknown-key flood must not be able to drive:
	// it is the difference between a miss costing O(1) and O(loaded keys).
	Merges uint64
}

// Authenticator resolves credentials to principals.
//
// The hot path is a lock-free read of an immutable snapshot held in an
// atomic.Pointer, keyed by the raw index key so that no hex string is
// allocated. A miss falls through to a small mutex-guarded overlay and then to
// one coalesced store call. Entries learned at runtime are folded into the
// snapshot in batches.
//
// An Authenticator is safe for concurrent use.
type Authenticator struct {
	hasher  *Hasher
	store   Store
	rehash  Rehasher
	now     func() time.Time
	entTTL  time.Duration
	negTTL  time.Duration
	stoTO   time.Duration
	masterD Digest
	master  *Principal

	snap atomic.Pointer[map[Lookup]*entry]

	mu      sync.RWMutex
	overlay map[Lookup]*entry
	// positives and negatives count the two kinds of overlay entry. They are
	// maintained rather than recomputed because the merge decision is taken on
	// every insert and walking the map to answer it would reintroduce the
	// per-miss cost the batching exists to avoid.
	positives int
	negatives int
	merges    atomic.Uint64

	fl flight[Lookup, *entry]

	rehashCh   chan rehashJob
	rehashDone chan struct{}
	closeOnce  sync.Once

	hits, misses, storeCalls, coalesced, rejected, masterHits atomic.Uint64
	rehashQueued, rehashDropped, rehashOK                     atomic.Uint64
}

type rehashJob struct {
	keyID  string
	lookup Lookup
	digest Digest
}

// New builds an Authenticator.
//
// It refuses a configuration with no pepper, and a configuration with no
// master credential unless NoMasterKey says so — the two ways R1-A found a
// gateway can silently lose its own security properties.
func New(cfg Config) (*Authenticator, error) {
	h, err := NewHasher(cfg.Pepper, cfg.Legacy)
	if err != nil {
		return nil, err
	}
	if cfg.MasterKey == "" && !cfg.NoMasterKey {
		return nil, ErrMasterKeyRequired
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	a := &Authenticator{
		hasher: h,
		store:  cfg.Store,
		now:    now,
		entTTL: orDuration(cfg.EntryTTL, DefaultEntryTTL),
		negTTL: orDuration(cfg.NegativeTTL, DefaultNegativeTTL),
		stoTO:  orDuration(cfg.StoreTimeout, DefaultStoreTimeout),
	}
	empty := map[Lookup]*entry{}
	a.snap.Store(&empty)

	if cfg.MasterKey != "" {
		a.masterD = sum256(cfg.MasterKey)
		id := cfg.MasterKeyID
		if id == "" {
			id = "master"
		}
		a.master = &Principal{KeyID: id, Label: "master", Master: true}
	}
	if cfg.RehashOnUse {
		if r, ok := cfg.Store.(Rehasher); ok {
			a.rehash = r
		}
	}
	if a.rehash != nil {
		depth := cfg.RehashQueue
		if depth <= 0 {
			depth = DefaultRehashQueue
		}
		a.rehashCh = make(chan rehashJob, depth)
		a.rehashDone = make(chan struct{})
		go a.rehashWorker()
	}
	return a, nil
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// String redacts.
func (a *Authenticator) String() string { return "auth.Authenticator{master:(redacted)}" }

// GoString redacts %#v.
func (a *Authenticator) GoString() string { return a.String() }

// Format redacts every other verb.
func (a *Authenticator) Format(f fmt.State, verb rune) { writeRedacted(f, verb, a.String()) }

// Load replaces the lock-free snapshot with these rows. It is how the startup
// load and a hot reload publish the credential set; it does not touch the
// store.
//
// Rows are loaded as they are, including expired ones: an expired row must
// stay present and be refused as expired (R1-A), not disappear and be refused
// as unknown, which would be indistinguishable from a deleted key.
func (a *Authenticator) Load(records []Record) error {
	m := make(map[Lookup]*entry, len(records))
	for i := range records {
		r := &records[i]
		l, err := ParseLookup(r.Lookup)
		if err != nil {
			return fmt.Errorf("auth: record %d (key %s): %w", i, r.Principal.KeyID, err)
		}
		p := r.Principal
		m[l] = &entry{digest: r.Digest, scheme: r.Scheme, principal: &p, found: true}
	}
	a.snap.Store(&m)
	a.mu.Lock()
	a.dropOverlayLocked()
	a.mu.Unlock()
	return nil
}

// Invalidate drops one index key from both the snapshot and the overlay, so
// the next request re-reads it. It takes the hex index key — never a
// credential.
func (a *Authenticator) Invalidate(lookup string) error {
	l, err := ParseLookup(lookup)
	if err != nil {
		return err
	}
	a.mu.Lock()
	if prev, ok := a.overlay[l]; ok {
		a.uncount(prev)
		delete(a.overlay, l)
	}
	cur := a.snap.Load()
	if _, ok := (*cur)[l]; ok {
		m := make(map[Lookup]*entry, len(*cur))
		for k, v := range *cur {
			if k != l {
				m[k] = v
			}
		}
		a.snap.Store(&m)
	}
	a.mu.Unlock()
	return nil
}

// InvalidateAll drops every cached row.
func (a *Authenticator) InvalidateAll() {
	a.mu.Lock()
	a.dropOverlayLocked()
	empty := map[Lookup]*entry{}
	a.snap.Store(&empty)
	a.mu.Unlock()
}

// Stats returns the counters.
func (a *Authenticator) Stats() Stats {
	a.mu.RLock()
	ov := len(a.overlay)
	a.mu.RUnlock()
	return Stats{
		Hits:          a.hits.Load(),
		Misses:        a.misses.Load(),
		StoreCalls:    a.storeCalls.Load(),
		Coalesced:     a.coalesced.Load(),
		Rejected:      a.rejected.Load(),
		MasterHits:    a.masterHits.Load(),
		RehashQueued:  a.rehashQueued.Load(),
		RehashDropped: a.rehashDropped.Load(),
		RehashDone:    a.rehashOK.Load(),
		Merges:        a.merges.Load(),
		SnapshotSize:  len(*a.snap.Load()),
		OverlaySize:   ov,
	}
}

// Close stops the rehash worker. It is idempotent.
func (a *Authenticator) Close() {
	a.closeOnce.Do(func() {
		if a.rehashCh != nil {
			close(a.rehashCh)
			<-a.rehashDone
		}
	})
}

// AuthenticateHeader extracts a credential from request headers and
// authenticates it. It does not strip the headers: stripping happens with
// [Strip] at the point a request is actually forwarded, so that an inbound
// handler cannot accidentally destroy the credential it still needs.
func (a *Authenticator) AuthenticateHeader(ctx context.Context, h http.Header) (*Principal, error) {
	token, _, ok := Extract(h)
	if !ok {
		a.rejected.Add(1)
		return nil, refuse(ReasonMissingCredential, "", "")
	}
	return a.Authenticate(ctx, token)
}

// Check authenticates a credential and authorizes the request in one call. It
// is the form the gateway's front door uses; the two halves are exported
// separately because administration needs authentication without a model or a
// route.
func (a *Authenticator) Check(ctx context.Context, token string, access Access) (*Principal, error) {
	p, err := a.Authenticate(ctx, token)
	if err != nil {
		return nil, err
	}
	if err := p.Authorize(access); err != nil {
		a.rejected.Add(1)
		return nil, err
	}
	return p, nil
}

// Authenticate resolves a credential to a principal.
//
// Order is deliberate and load-bearing:
//
//  1. The master comparison happens first, in constant time, against a
//     configured value. No store state and no prefix rule can withdraw it
//     (R1-A).
//  2. The "sk-" prefix gate happens before any lookup, so a stored digest
//     cannot be replayed as a credential.
//  3. One scheme-independent index key selects the row. Verification branches
//     on the scheme, not on the credential.
//
// The returned Principal is shared and immutable; callers must not mutate it.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		a.rejected.Add(1)
		return nil, refuse(ReasonMissingCredential, "", "")
	}

	sum, mac := a.hasher.digests(token)

	// (1) Out-of-band administrative credential. Comparing digests rather than
	// the raw strings keeps the comparison constant time and leaks no length.
	if a.master != nil && subtle.ConstantTimeCompare(sum[:], a.masterD[:]) == 1 {
		a.masterHits.Add(1)
		return a.master, nil
	}

	// (2) Prefix gate, before any lookup.
	if len(token) < len(KeyPrefix) || token[:len(KeyPrefix)] != KeyPrefix {
		a.rejected.Add(1)
		return nil, refuse(ReasonMalformed, "", "")
	}

	var l Lookup
	copy(l[:], sum[:LookupBytes])
	now := a.now()
	nowNS := now.UnixNano()

	// (3) Lock-free snapshot read.
	if e, ok := (*a.snap.Load())[l]; ok && e.live(nowNS) {
		a.hits.Add(1)
		return a.use(e, sum, mac, now)
	}

	a.mu.RLock()
	e, ok := a.overlay[l]
	a.mu.RUnlock()
	if ok && e.live(nowNS) {
		a.hits.Add(1)
		return a.use(e, sum, mac, now)
	}

	a.misses.Add(1)
	e, err := a.fetch(ctx, l)
	if err != nil {
		a.rejected.Add(1)
		return nil, err
	}
	return a.use(e, sum, mac, a.now())
}

// use verifies a credential against a cached row and returns its principal.
func (a *Authenticator) use(e *entry, sum, mac Digest, now time.Time) (*Principal, error) {
	if !e.found {
		a.rejected.Add(1)
		return nil, refuse(ReasonUnknownKey, "", "")
	}
	if err := a.hasher.verify(e.scheme, e.digest, sum, mac, now); err != nil {
		a.rejected.Add(1)
		return nil, err
	}
	p := e.principal
	// Two kill switches are enforced here as well as in Authorize, so a caller
	// that forgets to authorize still cannot use a revoked credential.
	if p.Key.Blocked || (p.User != nil && p.User.Blocked) || (p.Team != nil && p.Team.Blocked) {
		a.rejected.Add(1)
		return nil, refuse(ReasonBlocked, "key", "")
	}
	if p.Key.Expired(now) || (p.User != nil && p.User.Expired(now)) || (p.Team != nil && p.Team.Expired(now)) {
		a.rejected.Add(1)
		return nil, refuse(ReasonExpired, "key", "")
	}
	if e.scheme == SchemeLegacySHA256 && a.rehash != nil {
		a.scheduleRehash(e, p.KeyID, mac, sum)
	}
	return p, nil
}

// scheduleRehash queues the asynchronous upgrade of a legacy row. The send is
// non-blocking and the digest is already computed, so a request that triggers
// an upgrade pays nothing beyond a channel send that may fail.
func (a *Authenticator) scheduleRehash(e *entry, keyID string, mac, sum Digest) {
	if !e.rehashQueued.CompareAndSwap(false, true) {
		return
	}
	var l Lookup
	copy(l[:], sum[:LookupBytes])
	select {
	case a.rehashCh <- rehashJob{keyID: keyID, lookup: l, digest: mac}:
		a.rehashQueued.Add(1)
	default:
		// The queue is full. Dropping is correct: the upgrade is an
		// optimization, and the next request will re-arm it.
		e.rehashQueued.Store(false)
		a.rehashDropped.Add(1)
	}
}

// rehashWorker performs queued upgrades off the request path.
func (a *Authenticator) rehashWorker() {
	defer close(a.rehashDone)
	for job := range a.rehashCh {
		ctx, cancel := context.WithTimeout(context.Background(), a.stoTO)
		err := a.rehash.Rehash(ctx, job.keyID, job.lookup.Hex(), job.digest)
		cancel()
		if err != nil {
			continue
		}
		a.rehashOK.Add(1)
		// The row is dorang_v1 now. Drop the cached copy so the next request
		// reloads it under the new scheme rather than re-verifying legacy
		// until the TTL runs out.
		_ = a.Invalidate(job.lookup.Hex())
	}
}

// fetch resolves one index key through the store, coalescing concurrent misses.
func (a *Authenticator) fetch(ctx context.Context, l Lookup) (*entry, error) {
	if a.store == nil {
		return nil, refuse(ReasonUnknownKey, "", "")
	}
	if err := ctx.Err(); err != nil {
		return nil, refuse(ReasonUnavailable, "", "request cancelled")
	}
	e, err, shared := a.fl.do(l, func() (*entry, error) {
		// The leader's work is detached from its caller's context: a follower's
		// result must not depend on whether an unrelated caller cancelled.
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.stoTO)
		defer cancel()
		a.storeCalls.Add(1)
		rec, err := a.store.LoadByLookup(c, l.Hex())
		switch {
		case errors.Is(err, ErrNotFound):
			ne := &entry{expires: a.now().Add(a.negTTL).UnixNano()}
			a.insert(l, ne)
			return ne, nil
		case err != nil:
			return nil, refuse(ReasonUnavailable, "", "")
		}
		p := rec.Principal
		ne := &entry{
			digest:    rec.Digest,
			scheme:    rec.Scheme,
			principal: &p,
			found:     true,
			expires:   a.now().Add(a.entTTL).UnixNano(),
		}
		a.insert(l, ne)
		return ne, nil
	})
	if shared {
		a.coalesced.Add(1)
	}
	if errors.Is(err, errFlightAborted) {
		return nil, refuse(ReasonUnavailable, "", "lookup did not complete")
	}
	return e, err
}

// insert publishes a runtime-learned entry. Entries accumulate in the overlay
// and are folded into the lock-free snapshot in batches, so a burst of misses
// costs O(1) each rather than O(keys) each.
func (a *Authenticator) insert(l Lookup, e *entry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.overlay == nil {
		a.overlay = make(map[Lookup]*entry, mergeThreshold)
	}
	if prev, ok := a.overlay[l]; ok {
		// Replacing an entry must not double-count its kind, or the counters
		// drift away from the map they describe and the thresholds stop
		// meaning anything.
		a.uncount(prev)
	}
	a.overlay[l] = e
	if e.found {
		a.positives++
	} else {
		a.negatives++
	}

	switch {
	case len(a.overlay) >= maxOverlay:
		// Bounded: an unknown-key flood cannot grow this without limit.
		a.dropOverlayLocked()
	case a.negatives >= maxNegative:
		// The half a caller with no credential controls, dropped on its own.
		a.dropNegativesLocked()
	case a.positives >= mergeThreshold:
		// Merge on what a merge can actually PROMOTE. Negatives are never
		// promoted, so counting them here would ask for a snapshot copy that
		// moves nothing — which is exactly what an unknown-key flood used to
		// get, once per request.
		a.mergeLocked()
	}
}

// uncount removes an entry from the kind counters.
func (a *Authenticator) uncount(e *entry) {
	if e.found {
		a.positives--
	} else {
		a.negatives--
	}
}

// dropOverlayLocked discards the whole overlay.
func (a *Authenticator) dropOverlayLocked() {
	a.overlay = nil
	a.positives, a.negatives = 0, 0
}

// dropNegativesLocked discards the negative entries and keeps the positives.
//
// A negative entry is a short-lived "this key does not exist" and re-learning
// one costs the store query it would have cost anyway. A positive entry is a
// real credential that a legitimate caller is using right now, and throwing it
// away because an attacker flooded the same map is the amplification the cap
// exists to prevent.
func (a *Authenticator) dropNegativesLocked() {
	for k, v := range a.overlay {
		if !v.found {
			delete(a.overlay, k)
		}
	}
	a.negatives = 0
}

// mergeLocked folds the overlay into a fresh snapshot. Only positive entries
// are promoted: negative entries are short-lived by design and belong in the
// bounded overlay, not in the long-lived snapshot.
func (a *Authenticator) mergeLocked() {
	cur := *a.snap.Load()
	a.merges.Add(1)
	m := make(map[Lookup]*entry, len(cur)+a.positives)
	for k, v := range cur {
		m[k] = v
	}
	keep := make(map[Lookup]*entry, a.negatives)
	for k, v := range a.overlay {
		if v.found {
			m[k] = v
		} else {
			keep[k] = v
		}
	}
	a.snap.Store(&m)
	a.overlay = keep
	a.positives = 0
	// negatives is unchanged: they are exactly what was kept.
}
