package auth

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// DESIGN §11.2c, risk W11: how fast a revocation actually takes effect.
//
// §11.2's hot path answers from a lock-free snapshot with a TTL. That is what
// makes authentication fast, and it also means a revoked, pended or
// rotation-cut key keeps serving until the snapshot refreshes — on every node
// independently. Pending a key that keeps serving for a cache TTL has not
// stopped anything; it has started a timer.
//
// So the four rules of §11.2c are implemented here rather than assumed:
//
//  1. A revocation, a pend and an early grace cut PUBLISH an invalidation, and
//     the snapshot drops the key on receipt. The TTL is the fallback for a node
//     that missed the message, not the mechanism.
//  2. Single node is immediate; clustered is bounded and the worst case is
//     published AS A NUMBER — [Authenticator.RevocationBound] — the same rule
//     §5.6 applies to overshoot.
//  3. Negative entries get a shorter TTL than serving ones, and a found row that
//     REFUSES counts as negative. Treating the two alike is what made the
//     window large.
//  4. A node that was down reloads on rejoin rather than trusting a stale
//     snapshot — [Authenticator.Rejoin].

// InvalidationCause says why a key was invalidated. It travels with the message
// so an operator reading a node's log learns which control fired, not merely
// that a cache was dropped.
type InvalidationCause uint8

const (
	// CauseUnspecified is the zero value. It invalidates, and says nothing.
	CauseUnspecified InvalidationCause = iota
	// CauseRevoked: the key was revoked or deleted.
	CauseRevoked
	// CausePended: the token guard pended the key (§11.6).
	CausePended
	// CauseReleased: an operator released a pend. It is published for the same
	// reason a pend is — a release that waits for a TTL is an outage that
	// outlives the decision to end it.
	CauseReleased
	// CauseGraceCut: a rotation's grace period was ended early, which is what a
	// suspected compromise needs (§11.2c).
	CauseGraceCut
	// CauseRotated: a rotation minted a new secret. The old one is still valid,
	// so this is not a refusal; it is published so that a node holding the key's
	// row re-reads it and learns the new secret's expiry.
	CauseRotated
	// CauseUpdated: the key's limits, tier or ownership changed.
	CauseUpdated
)

var causeNames = [...]string{"unspecified", "revoked", "pended", "released", "grace_cut", "rotated", "updated"}

// String returns the wire spelling.
func (c InvalidationCause) String() string {
	if int(c) >= len(causeNames) {
		return "unspecified"
	}
	return causeNames[c]
}

// ParseCause decodes a stored cause.
func ParseCause(s string) (InvalidationCause, bool) {
	for i, n := range causeNames {
		if n == s {
			return InvalidationCause(i), true
		}
	}
	return CauseUnspecified, false
}

// Invalidation is one published message: drop this key from the snapshot.
//
// It names the key by its DURABLE id and, where the publisher knows them, by
// the index keys of its secrets. It carries no credential and no digest — a
// lookup is a truncated digest used as an index value, which is what the
// authenticator is already keyed by.
type Invalidation struct {
	// Seq is the publisher's monotonic sequence number. A subscriber uses it as
	// a watermark; it is not required to be gap-free.
	Seq int64
	// KeyID is the api_keys row id. Dropping by id is what makes an
	// invalidation correct after a rotation the receiving node never saw: it
	// drops every secret of the key, including ones it learned before the
	// message was sent.
	KeyID string
	// Lookups are the index keys of the key's secrets, when the publisher knows
	// them. They make the drop O(1) instead of a scan; the KeyID is still
	// authoritative and is always used as well.
	Lookups []string
	// Cause is why.
	Cause InvalidationCause
	// At is when the control was applied at the publisher.
	At time.Time
}

// String renders the message for a log line. It contains no secret.
func (i Invalidation) String() string {
	return fmt.Sprintf("invalidation#%d key=%s cause=%s secrets=%d at=%s",
		i.Seq, i.KeyID, i.Cause, len(i.Lookups), i.At.UTC().Format(time.RFC3339Nano))
}

// Sink publishes an invalidation so that every other node sees it.
//
// It is one method for the same reason [Store] is: a store table, a Redis
// channel, an HTTP fan-out and a test double are all trivially substitutable,
// and this package depends on none of them.
//
// A Sink must be durable enough that rule 4 can hold — a node that was down
// must be able to catch up on rejoin — or the deployment must accept the entry
// TTL as its real bound and say so.
type Sink interface {
	// Publish records an invalidation. It is called off the request path and
	// may block for as long as its context allows.
	Publish(ctx context.Context, inv Invalidation) error
}

// Loader reloads the full credential set. It is what [Authenticator.Rejoin]
// calls, and it is the one place the request path is deliberately allowed to
// wait: serving from a snapshot known to be stale is worse than a brief pause
// at startup (§11.2c rule 4).
type Loader interface {
	// LoadAll returns every credential row the gateway should serve.
	LoadAll(ctx context.Context) ([]Record, error)
}

// --- applying ----------------------------------------------------------------

// Apply drops a key from this node's snapshot and overlay.
//
// It is idempotent, which is what lets a subscriber replay a window of messages
// without tracking which ones it has already seen: dropping an entry that is
// already gone is a no-op, and re-reading the row is the correct behaviour in
// both cases.
//
// It reports how many cached entries were dropped.
func (a *Authenticator) Apply(inv Invalidation) int {
	a.invalidations.Add(1)
	byLookup := make(map[Lookup]bool, len(inv.Lookups))
	for _, s := range inv.Lookups {
		l, err := ParseLookup(s)
		if err != nil {
			continue // a malformed lookup drops nothing; the key id still does
		}
		byLookup[l] = true
	}
	keyID := strings.TrimSpace(inv.KeyID)
	if len(byLookup) == 0 && keyID == "" {
		return 0
	}

	drop := func(l Lookup, e *entry) bool {
		if byLookup[l] {
			return true
		}
		return keyID != "" && e != nil && e.principal != nil && e.principal.KeyID == keyID
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	n := 0
	for l, e := range a.overlay {
		if drop(l, e) {
			delete(a.overlay, l)
			n++
		}
	}
	cur := *a.snap.Load()
	var doomed []Lookup
	for l, e := range cur {
		if drop(l, e) {
			doomed = append(doomed, l)
		}
	}
	if len(doomed) == 0 {
		a.dropped.Add(uint64(n))
		return n
	}
	// The snapshot is immutable and read without a lock, so it is replaced
	// rather than mutated. One copy serves however many lookups the message
	// named, which is why the whole message is applied under one lock rather
	// than lookup by lookup.
	m := make(map[Lookup]*entry, len(cur)-len(doomed))
	for l, e := range cur {
		m[l] = e
	}
	for _, l := range doomed {
		delete(m, l)
		n++
	}
	a.snap.Store(&m)
	a.dropped.Add(uint64(n))
	return n
}

// InvalidateKey drops every cached secret of one key id.
//
// It is the local half of a revocation and takes an id, never a credential. Use
// [Authenticator.Announce] to do this AND tell the other nodes.
func (a *Authenticator) InvalidateKey(keyID string) int {
	return a.Apply(Invalidation{KeyID: keyID, Cause: CauseRevoked, At: a.now()})
}

// Announce applies an invalidation locally and publishes it.
//
// The order is deliberate and is the whole of rule 1. The local drop happens
// FIRST and unconditionally, so a publish that fails leaves this node correct
// rather than leaving every node wrong; the error is returned so the caller
// knows the fleet is only converging on the TTL. A control that published
// before dropping would have a window on the node that issued it, which is the
// node the operator is watching.
//
// With no sink configured this is a single-node deployment and the local drop
// is the entire mechanism, which is why it is not an error.
func (a *Authenticator) Announce(ctx context.Context, inv Invalidation) error {
	if inv.At.IsZero() {
		inv.At = a.now()
	}
	a.Apply(inv)
	if a.sink == nil {
		return nil
	}
	if err := a.sink.Publish(ctx, inv); err != nil {
		a.publishFails.Add(1)
		return fmt.Errorf("auth: %s was applied on this node but not published; "+
			"other nodes will converge on the entry TTL instead: %w", inv, err)
	}
	a.published.Add(1)
	return nil
}

// Refresh reloads the whole credential set WITHOUT dropping the current one
// first.
//
// It is the periodic counterpart of [Authenticator.Rejoin], and the difference
// is the whole reason both exist. Rejoin drops first, because a node that was
// down must not serve what it had. Refresh does not, because dropping the
// snapshot every interval would send every key to the store in the gap between
// the drop and the load — a stampede on a schedule, to fix nothing.
//
// Refresh is what gives a snapshot row a TTL fallback at all: rows placed by a
// bulk load do not expire (§9.1), so the interval this runs at IS
// [RevocationBound.SnapshotFallback]. A deployment that never calls it has no
// fallback for those rows, and the published figure says so rather than
// reporting the entry TTL, which does not apply to them.
func (a *Authenticator) Refresh(ctx context.Context, l Loader) error {
	if l == nil {
		return fmt.Errorf("auth: Refresh needs a loader")
	}
	recs, err := l.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("auth: credential refresh failed; the snapshot is unchanged and a "+
			"missed invalidation will not be corrected until the next one succeeds: %w", err)
	}
	if err := a.Load(recs); err != nil {
		return err
	}
	a.refreshes.Add(1)
	return nil
}

// Rejoin reloads the whole credential set and replaces the snapshot.
//
// §11.2c rule 4: a revocation must not be lost by a node that was down. On
// rejoin a node reloads rather than trusting a stale snapshot, and this is one
// of the few places the request path is deliberately allowed to wait — serving
// from a snapshot known to be stale is worse than a brief pause at startup.
//
// The snapshot is dropped BEFORE the load rather than replaced after it. A load
// that fails must not leave the stale set serving, which is the failure mode
// the rule exists to prevent; an empty snapshot falls through to the store per
// key, which is slower and correct.
func (a *Authenticator) Rejoin(ctx context.Context, l Loader) error {
	if l == nil {
		return fmt.Errorf("auth: Rejoin needs a loader")
	}
	a.InvalidateAll()
	recs, err := l.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("auth: rejoin reload failed; this node is serving from the store "+
			"per key rather than from a snapshot that may be stale: %w", err)
	}
	if err := a.Load(recs); err != nil {
		return err
	}
	a.rejoins.Add(1)
	return nil
}

// --- the published bound -----------------------------------------------------

// RevocationBound is the published worst-case revocation latency, in the shape
// [cluster.Accuracy] publishes overshoot: a number, the arithmetic that produced
// it, and the mechanism it comes from. "Revocation is fast" is not a
// specification.
type RevocationBound struct {
	// Topology is "single-node" or "clustered".
	Topology string
	// Nodes is how many nodes share the credential set.
	Nodes int
	// Bound is the worst case, measured FROM THE MOMENT THE CONTROL RETURNS, for
	// the key to stop serving on every node while the invalidation path is
	// working. It is the number an operator is owed.
	//
	// It is stated from the control's return rather than from its call because
	// the control's own durable write is synchronous and already visible to the
	// caller: an operator who runs `POST /key/revoke` and gets a 200 has paid
	// that cost and watched it. Folding it in would produce a figure that
	// describes the database's write latency rather than the propagation this
	// mechanism is about, and would be a different number on every deployment
	// for a reason that has nothing to do with clustering.
	Bound time.Duration
	// Fallback is the worst case for a node that MISSED the message, for a row
	// the node learned at runtime through the store: the entry TTL. It is the
	// guarantee that survives a broken bus, and it is published beside the
	// bound because a number that only holds while everything works is not a
	// bound.
	Fallback time.Duration
	// SnapshotFallback is the same figure for a row placed by a bulk load
	// ([Authenticator.Load], [Authenticator.Rejoin]).
	//
	// It is a separate number because those rows DO NOT EXPIRE — the snapshot
	// is the authority on the hot path (§9.1), which is what makes a snapshot
	// hit cost no store read at all — so the entry TTL is not their fallback
	// and reporting it as one would be false. Their fallback is the interval at
	// which the whole set is re-read ([Authenticator.Refresh]), and a
	// deployment that schedules no reload has NONE: the field is zero, the
	// invalidation path is the only mechanism, and a node whose polls are
	// failing is visible in its counters rather than covered by a timer.
	//
	// Zero therefore means "never", not "immediately". It is the one place in
	// this type where a zero is not good news, and it is named so that it
	// cannot be read as one.
	SnapshotFallback time.Duration
	// Negative is how long a refusal is remembered before it is re-checked. It
	// bounds how long a released key stays refused, so it is the reversibility
	// half of the same figure.
	Negative time.Duration
	// Formula is the arithmetic with the parameters substituted.
	Formula string
	// Why states the mechanism the number comes from.
	Why string
}

// String renders the figure for a log line or a status page.
func (b RevocationBound) String() string {
	snap := b.SnapshotFallback.String()
	if b.SnapshotFallback <= 0 {
		snap = "never (no bulk reload is scheduled)"
	}
	return fmt.Sprintf("%s (%d node(s)): revocation takes effect within %s (%s); "+
		"fallback if the invalidation is missed: %s for a row learned at runtime, %s for a row "+
		"placed by a bulk load; negative entries re-checked after %s",
		b.Topology, b.Nodes, b.Bound, b.Formula, b.Fallback, snap, b.Negative)
}

// PropagationDelay is the publisher-to-subscriber delay a deployment's
// invalidation transport contributes, which is the only term in the clustered
// bound this package cannot compute for itself.
//
// For the store-polled transport it is the poll interval plus one store round
// trip. A transport with a push channel sets it to that channel's delivery
// bound. Zero means "not measured", and the bound then reports the entry TTL,
// because an unmeasured delay is not a small one.
type PropagationDelay struct {
	// Delay is the transport's worst-case delivery time.
	Delay time.Duration
	// Source names the transport, for the published formula.
	Source string
}

// RevocationBound returns the published worst case for this authenticator.
//
// nodes below 2 is the single-node topology, where the answer is exact: there
// is one snapshot, [Authenticator.Announce] drops the key before it returns,
// and no message has to travel. It is 0 rather than "immediate" because 0 is a
// number and "immediate" is a claim.
//
// Clustered, the bound is the transport's delivery time. It is NOT the entry
// TTL: the TTL is what a node falls back to when it misses the message, and it
// is reported separately so that neither figure has to stand in for the other.
func (a *Authenticator) RevocationBound(nodes int, p PropagationDelay) RevocationBound {
	if nodes < 1 {
		nodes = 1
	}
	b := RevocationBound{
		Nodes:            nodes,
		Fallback:         a.entTTL,
		SnapshotFallback: a.reloadEvery,
		Negative:         a.negTTL,
	}
	if nodes == 1 || a.sink == nil {
		b.Topology = "single-node"
		b.Nodes = 1
		b.Bound = 0
		b.Formula = "0"
		b.Why = "there is one snapshot and Announce drops the key from it before it returns, " +
			"so the control and the effect are the same operation"
		return b
	}
	b.Topology = "clustered"
	switch {
	case p.Delay <= 0:
		b.Bound = a.entTTL
		b.Formula = fmt.Sprintf("entry_ttl = %s (no propagation delay was measured)", a.entTTL)
		b.Why = "the invalidation transport did not state a delivery bound, so the only figure " +
			"that can be honoured is the TTL fallback. An unmeasured delay is not a small one"
	default:
		b.Bound = p.Delay
		src := p.Source
		if src == "" {
			src = "the invalidation transport"
		}
		b.Formula = fmt.Sprintf("propagation(%s) = %s", src, p.Delay)
		b.Why = "every node drops the key on receipt of the published invalidation; the TTL is " +
			"the fallback for a node that missed the message, not the mechanism"
	}
	return b
}

// --- an in-process transport -------------------------------------------------

// MemBus is a complete, in-process [Sink] that fans an invalidation out to a
// set of authenticators.
//
// It ships for the same reason internal/cluster ships MemRedis and
// internal/quota ships a static prober: the mechanism has to be exercisable by
// `go test` with no external service, and a single-process deployment that
// wants the shared code path is a legitimate use of it. It is not a substitute
// for a durable transport across nodes — it holds no history, so rule 4's
// "a node that was down reloads on rejoin" is [Authenticator.Rejoin]'s job and
// not this type's.
//
// A MemBus is safe for concurrent use.
type MemBus struct {
	mu   sync.Mutex
	subs []*Authenticator
	seq  int64
	log  []Invalidation
}

// NewMemBus builds an in-process bus.
func NewMemBus() *MemBus { return &MemBus{} }

// Subscribe attaches an authenticator. A publisher is normally also a
// subscriber; applying its own message twice is harmless because Apply is
// idempotent.
func (b *MemBus) Subscribe(a ...*Authenticator) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, a...)
}

// Publish implements [Sink]. Delivery is synchronous, which makes the bound it
// contributes exactly zero and makes a test that measures the bound measure the
// mechanism rather than a scheduler.
func (b *MemBus) Publish(_ context.Context, inv Invalidation) error {
	b.mu.Lock()
	b.seq++
	inv.Seq = b.seq
	subs := make([]*Authenticator, len(b.subs))
	copy(subs, b.subs)
	b.log = append(b.log, inv)
	b.mu.Unlock()

	for _, s := range subs {
		s.Apply(inv)
	}
	return nil
}

// Published returns every message the bus has carried, oldest first.
func (b *MemBus) Published() []Invalidation {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Invalidation, len(b.log))
	copy(out, b.log)
	return out
}

// Delay implements the propagation figure for this transport.
func (b *MemBus) Delay() PropagationDelay {
	return PropagationDelay{Delay: 0, Source: "in-process bus"}
}

// --- helpers -----------------------------------------------------------------

// SortLookups returns a deterministic copy of a lookup list. Publishing them in
// a stable order keeps an audit trail and a golden test reproducible.
func SortLookups(l []string) []string {
	out := append([]string(nil), l...)
	sort.Strings(out)
	return out
}
