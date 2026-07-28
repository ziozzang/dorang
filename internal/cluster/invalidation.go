package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/store"
)

// DESIGN §11.2c, risk W11 — making a revocation take effect across a cluster.
//
// The auth hot path answers from a lock-free snapshot with a TTL, so a revoked,
// pended or rotation-cut key keeps serving until the snapshot refreshes, on
// every node independently. Pending a key that keeps serving for a cache TTL
// has not stopped anything; it has started a timer.
//
// [KeyInvalidator] closes that. A control writes a durable message and applies
// it locally before returning; every other node polls the same table and drops
// the key on receipt. The TTL becomes the fallback for a node that missed the
// message rather than the mechanism, and the clustered worst case is
//
//	poll_interval + one store round trip
//
// published as a number by [KeyInvalidator.Bound], the same rule §5.6 applies to
// overshoot.

// Invalidation defaults.
const (
	// DefaultInvalidationPoll is how often a node reads the invalidation table.
	// One second: it is one indexed range scan returning nothing in the normal
	// case, and it is the dominant term in the published bound, so it is set by
	// what an operator is owed after a compromise rather than by what is
	// cheapest.
	DefaultInvalidationPoll = time.Second
	// DefaultInvalidationRetain is how long messages are kept. A node absent
	// for longer than this reloads on rejoin rather than replaying, which it
	// does at startup anyway (§11.2c rule 4).
	DefaultInvalidationRetain = time.Hour
)

// InvalidatorConfig configures a [KeyInvalidator].
type InvalidatorConfig struct {
	// Store is the shared store. Required.
	Store *store.Store
	// Auth is this node's authenticator, which the poller applies messages to.
	//
	// It may be left nil and supplied by [KeyInvalidator.Attach] instead. The
	// two are mutually referential at wiring time — the authenticator publishes
	// through the invalidator and the invalidator applies to the authenticator
	// — and one nil-able field set once beats a lazy accessor consulted on
	// every message.
	Auth *auth.Authenticator
	// NodeID names this node in log lines.
	NodeID string
	// Poll is how often to read the table. Zero means
	// [DefaultInvalidationPoll].
	Poll time.Duration
	// Retain is how long messages are kept. Zero means
	// [DefaultInvalidationRetain].
	Retain time.Duration
	// StoreLatency is the deployment's measured worst-case store round trip. It
	// is the second term of the published bound and it is a parameter rather
	// than a constant because it is a property of the deployment's database,
	// not of this code. Zero means [DefaultStoreLatency].
	StoreLatency time.Duration
	// Now overrides the clock.
	Now func() time.Time
	// Logf receives poll failures.
	Logf func(string, ...any)
}

// DefaultStoreLatency is the store round trip assumed by the published bound
// when a deployment has not measured its own.
//
// It is deliberately generous. A bound computed from an optimistic number is a
// bound that is wrong exactly when the database is slow, which is when an
// operator is most likely to be revoking something.
const DefaultStoreLatency = 250 * time.Millisecond

// KeyInvalidator is one node's half of the invalidation path: it publishes the
// controls this node applies, and it applies the controls other nodes publish.
//
// A KeyInvalidator is safe for concurrent use.
type KeyInvalidator struct {
	st     *store.Store
	nodeID string
	poll   time.Duration
	retain time.Duration
	stoLat time.Duration
	now    func() time.Time
	logf   func(string, ...any)

	authn     atomic.Pointer[auth.Authenticator]
	seq       atomic.Int64
	published atomic.Uint64
	applied   atomic.Uint64
	polls     atomic.Uint64
	pollFails atomic.Uint64

	mu      sync.Mutex
	stop    chan struct{}
	stopped chan struct{}
	started bool
}

var _ auth.Sink = (*KeyInvalidator)(nil)

// NewKeyInvalidator builds one.
func NewKeyInvalidator(cfg InvalidatorConfig) (*KeyInvalidator, error) {
	if cfg.Store == nil {
		return nil, errors.New("cluster: NewKeyInvalidator needs a store")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	k := &KeyInvalidator{
		st:      cfg.Store,
		nodeID:  cfg.NodeID,
		poll:    orDuration(cfg.Poll, DefaultInvalidationPoll),
		retain:  orDuration(cfg.Retain, DefaultInvalidationRetain),
		stoLat:  orDuration(cfg.StoreLatency, DefaultStoreLatency),
		now:     now,
		logf:    logf,
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	if cfg.Auth != nil {
		k.authn.Store(cfg.Auth)
	}
	return k, nil
}

// Attach binds the authenticator this node applies messages to.
//
// ErrNoAuthenticator is what every method returns until it is called: an
// invalidation nothing applies is a row in a table, and a poller that silently
// discarded messages would leave the fleet on the TTL fallback while reporting
// that it was healthy.
func (k *KeyInvalidator) Attach(a *auth.Authenticator) { k.authn.Store(a) }

// ErrNoAuthenticator reports an invalidator that was never attached.
var ErrNoAuthenticator = errors.New(
	"cluster: the key invalidator has no authenticator; call Attach before polling, " +
		"or messages would be read and discarded while the fleet served on the TTL fallback")

// Publish implements [auth.Sink]: it appends the message to the durable table.
//
// It does NOT apply the message locally — [auth.Authenticator.Announce] has
// already done that, unconditionally and before calling here, so that a publish
// failure leaves this node correct rather than leaving every node wrong.
func (k *KeyInvalidator) Publish(ctx context.Context, inv auth.Invalidation) error {
	rec, err := k.st.PublishInvalidation(ctx, store.KeyInvalidation{
		KeyID:     inv.KeyID,
		Lookups:   inv.Lookups,
		Cause:     inv.Cause.String(),
		CreatedAt: inv.At,
	})
	if err != nil {
		return err
	}
	k.published.Add(1)
	// This node has, by construction, already applied its own message. Moving
	// the watermark past it avoids re-reading it on the next poll; a message
	// that arrives out of order simply gets applied twice, which Apply is
	// idempotent under.
	k.advance(rec.Seq)
	return nil
}

// advance moves the watermark forward, never back.
func (k *KeyInvalidator) advance(seq int64) {
	for {
		cur := k.seq.Load()
		if seq <= cur || k.seq.CompareAndSwap(cur, seq) {
			return
		}
	}
}

// Seq returns this node's watermark.
func (k *KeyInvalidator) Seq() int64 { return k.seq.Load() }

// Poll reads every message after the watermark and applies it.
//
// It is exported so a test — and an operator's debug endpoint — can drive it
// deterministically rather than waiting on a ticker.
func (k *KeyInvalidator) Poll(ctx context.Context) (int, error) {
	authn := k.authn.Load()
	if authn == nil {
		k.pollFails.Add(1)
		return 0, ErrNoAuthenticator
	}
	k.polls.Add(1)
	msgs, err := k.st.InvalidationsSince(ctx, k.seq.Load(), 0)
	if err != nil {
		k.pollFails.Add(1)
		return 0, err
	}
	n := 0
	for _, m := range msgs {
		cause, _ := auth.ParseCause(m.Cause)
		authn.Apply(auth.Invalidation{
			Seq:     m.Seq,
			KeyID:   m.KeyID,
			Lookups: m.Lookups,
			Cause:   cause,
			At:      m.CreatedAt,
		})
		k.advance(m.Seq)
		n++
	}
	k.applied.Add(uint64(n))
	return n, nil
}

// Rejoin reloads the whole credential set and starts from the current head of
// the invalidation table.
//
// §11.2c rule 4: a revocation must not be lost by a node that was down. On
// rejoin a node reloads rather than trusting a stale snapshot, and this is one
// of the few places the request path is deliberately allowed to wait.
//
// The watermark is read BEFORE the reload, not after. A message published
// between the two would otherwise be skipped — the reload might have missed it
// and the watermark would claim it was seen — and a skipped message here is a
// revoked key that keeps serving until its TTL, which is precisely the failure
// this whole path exists to remove.
func (k *KeyInvalidator) Rejoin(ctx context.Context, l auth.Loader) error {
	authn := k.authn.Load()
	if authn == nil {
		return ErrNoAuthenticator
	}
	head, err := k.st.LatestInvalidationSeq(ctx)
	if err != nil {
		return fmt.Errorf("cluster: reading the invalidation watermark on rejoin: %w", err)
	}
	if err := authn.Rejoin(ctx, l); err != nil {
		return err
	}
	k.seq.Store(head)
	// Anything published while the reload was running is picked up now rather
	// than one poll interval from now.
	_, err = k.Poll(ctx)
	return err
}

// Start runs the poll loop until [KeyInvalidator.Close].
func (k *KeyInvalidator) Start(ctx context.Context) error {
	k.mu.Lock()
	if k.started {
		k.mu.Unlock()
		return errors.New("cluster: the key invalidator is already started")
	}
	k.started = true
	k.mu.Unlock()

	go func() {
		defer close(k.stopped)
		t := time.NewTicker(k.poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-k.stop:
				return
			case <-t.C:
				if _, err := k.Poll(ctx); err != nil {
					k.logf("cluster: node %s could not read invalidations; "+
						"revocations are falling back to the auth entry TTL: %v", k.nodeID, err)
				}
			}
		}
	}()
	return nil
}

// Close stops the loop.
func (k *KeyInvalidator) Close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.started {
		return
	}
	select {
	case <-k.stop:
	default:
		close(k.stop)
		<-k.stopped
	}
	k.started = false
}

// Prune deletes messages older than the retention. It is a leader job.
func (k *KeyInvalidator) Prune(ctx context.Context) (int64, error) {
	return k.st.PruneInvalidations(ctx, k.retain)
}

// Delay returns the propagation figure this transport contributes.
//
// It is the poll interval plus one store round trip: a message is visible to a
// subscriber as soon as it is committed, and the subscriber sees it at its next
// tick. Both terms are named rather than folded together, because one is
// dorang's choice and the other is the deployment's database.
func (k *KeyInvalidator) Delay() auth.PropagationDelay {
	return auth.PropagationDelay{
		Delay:  k.poll + k.stoLat,
		Source: fmt.Sprintf("store poll %s + store round trip %s", k.poll, k.stoLat),
	}
}

// Bound returns the published worst-case revocation latency for a cluster of
// this size.
func (k *KeyInvalidator) Bound(nodes int) auth.RevocationBound {
	authn := k.authn.Load()
	if authn == nil {
		return auth.RevocationBound{}
	}
	return authn.RevocationBound(nodes, k.Delay())
}

// InvalidatorStats is what the invalidator has done. Every number that could
// hide a lost revocation is here, because a published bound nobody can check is
// not a guarantee.
type InvalidatorStats struct {
	// Seq is this node's watermark.
	Seq int64
	// Published is how many messages this node put on the bus.
	Published uint64
	// Applied is how many it read back and applied.
	Applied uint64
	// Polls and PollFailures say whether the mechanism is running at all. A
	// node whose polls are failing is a node serving on the TTL fallback, and
	// that has to be visible rather than inferred.
	Polls        uint64
	PollFailures uint64
}

// Stats returns a snapshot.
func (k *KeyInvalidator) Stats() InvalidatorStats {
	return InvalidatorStats{
		Seq:          k.seq.Load(),
		Published:    k.published.Load(),
		Applied:      k.applied.Load(),
		Polls:        k.polls.Load(),
		PollFailures: k.pollFails.Load(),
	}
}
