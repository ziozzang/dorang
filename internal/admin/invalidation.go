package admin

import (
	"context"
	"net/http"
	"time"
)

// DESIGN §11.2c on the administration surface: a control that changes whether a
// key serves has to make the change TAKE EFFECT, not merely record it.
//
// # The gap this closes
//
// §11.2's hot path answers from a lock-free snapshot with a TTL. A mutation that
// only writes the row is therefore honoured, on every node independently, when
// that snapshot next refreshes — `auth.DefaultEntryTTL`, sixty seconds. §11.2c's
// answer is to PUBLISH the change: the node that applied it drops the key
// immediately and every other node drops it on receipt, which is a bound of one
// propagation interval (measured 9–15 ms on PostgreSQL, published at 270 ms)
// rather than a minute.
//
// internal/cluster has implemented that since §11.2c shipped, and its five
// controls — pend, release, revoke, rotate, cut-grace — each do the durable
// write AND the announcement. This package reached none of them: `POST
// /key/block`, the route OPERATIONS §3.1 tells an operator to use when a key
// leaks, wrote `blocked = 1` through [KeyStore] and published nothing. A leaked
// key went on serving for a minute on every node that did not handle the
// request, and the same was true of `/key/delete`, `/key/update`,
// `/key/regenerate`, `/key/rotate`, `/key/rotate/cut`, `/key/pend` and
// `/key/release`.
//
// # What is deliberately NOT published
//
// Creation. A key that has never been seen has nothing cached to drop, and the
// bound for "used before it existed, then created" is the negative-entry TTL
// (`auth.DefaultNegativeTTL`, 5 s) rather than a mistake. `/key/generate`
// therefore announces nothing, and that is a decision rather than an omission.

// InvalidationCause says why a key's cached copy has to be dropped. It travels
// with the message so that an operator reading a node's log learns which control
// fired rather than only that a cache was emptied.
//
// The spellings are the ones the invalidation table records and internal/auth
// parses. They are untyped string constants so that an implementation of
// [Invalidator] can be written — and a fake can be written in a test — without
// importing this package for a type.
const (
	// CauseRevoked: the key was blocked or deleted.
	CauseRevoked = "revoked"
	// CausePended: the key was pended (§11.6).
	CausePended = "pended"
	// CauseReleased: a pend was released. It is published for the same reason a
	// pend is — a release that waits for a TTL is an outage that outlives the
	// decision to end it.
	CauseReleased = "released"
	// CauseGraceCut: a superseded secret was cut outright, which is what
	// `/key/regenerate` and `/key/rotate/cut` do.
	CauseGraceCut = "grace_cut"
	// CauseRotated: a rotation minted a new secret and left the old one inside
	// its grace window. Nothing is refused; it is published so a node holding
	// the key's row re-reads it and learns the new secret and the old one's
	// expiry.
	CauseRotated = "rotated"
	// CauseUpdated: the key's limits, tier, ownership or block flag changed.
	CauseUpdated = "updated"
)

// Invalidator announces that a key's authorization changed, so that every node
// drops its cached copy instead of serving the old one until a TTL expires.
//
// One method, taking an id and a reason. It takes no lookup, no digest and no
// token: the durable key id is what the invalidation path is authoritative on
// (dropping by id catches secrets the receiving node learned before the message
// was sent), and an interface that accepted a credential would be an interface
// through which this package could leak one.
//
// It is called AFTER the durable write and BEFORE the response returns, so a
// caller that got a 200 has been told the fleet was told. An implementation is
// expected to apply the change to its own node before publishing it, which is
// what makes a publish failure leave one node correct rather than every node
// wrong.
//
// Optional. A deployment that wires none — a single process with no credential
// cache in front of it, or one that has decided the entry TTL is its real bound
// — keeps working, and the endpoints observe on that TTL instead. That is a
// worse guarantee and it is the one §11.2c exists to replace, so a deployment
// choosing it should say so.
type Invalidator interface {
	// InvalidateKey announces that this key's authorization changed. cause is
	// one of the [CauseRevoked] spellings.
	InvalidateKey(ctx context.Context, keyID, cause string) error
}

// CodeInvalidationFailed reports a mutation that was applied and could not be
// announced.
//
// It is its own code for the same reason [CodeAuditWriteFailed] is: the
// operator's follow-up differs from every other 500. The change is durable and
// this node has honoured it; what failed is the propagation, so the rest of the
// fleet converges on the credential cache TTL instead of within the published
// bound. Retrying the mutation does not help — it has already happened.
const CodeInvalidationFailed = "invalidation_publish_failed"

// InvalidationTimeout bounds one announcement.
//
// It exists because the announcement deliberately does NOT inherit the request's
// cancellation: see [call.invalidate]. A detached call with no deadline at all
// would be a handler a slow store can pin forever, so the detachment comes with
// a bound, and the bound is generous — an announcement that gave up early on a
// database that is merely slow would drop the fleet onto the TTL for the reason
// least worth dropping it.
const InvalidationTimeout = 10 * time.Second

// invalidate announces one applied change, or does nothing when no invalidator
// is wired.
//
// The context is DETACHED from the request. By the time this runs the durable
// change has committed, and the announcement is what makes it take effect; if it
// inherited the request's cancellation then an operator whose terminal hung up
// between the write and the publish would have blocked a leaked key everywhere
// except in the caches that are still serving it — which is precisely the defect
// this file exists to remove, reintroduced through a hang-up. The same
// reasoning, in the same words, is why internal/auth detaches its own store
// refresh.
func (c *call) invalidate(keyID, cause string) error {
	if c.a.cfg.Invalidator == nil || keyID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx()), InvalidationTimeout)
	defer cancel()
	if err := c.a.cfg.Invalidator.InvalidateKey(ctx, keyID, cause); err != nil {
		c.a.metrics.invalidationFailures.Add(1)
		f := newFault(http.StatusInternalServerError, CodeInvalidationFailed, typeAPI,
			"the change to key %q was applied and could not be announced to the other nodes; "+
				"they will honour it when their credential cache expires (auth.revocation.entry_ttl) "+
				"rather than within the published bound. Do not retry — the change is durable; "+
				"verify propagation, or SIGHUP the fleet if this was an incident response", keyID)
		f.Detail = map[string]any{"key_id": keyID, "cause": cause}
		return f
	}
	c.a.metrics.invalidations.Add(1)
	return nil
}

// applied is the closing act of every mutation on a key: announce the change,
// then write the trail.
//
// The order is the whole point and it is the opposite of the obvious one. The
// announcement is what makes the change take effect, so it goes first; the audit
// row records what happened, so it is written even when the announcement failed
// — a mutation that both took effect and left no trace is the worse of the two
// outcomes, and returning early on the announcement would produce exactly that.
// The audit fault therefore wins when both fail, because it is the one that says
// the record is incomplete.
func (c *call) applied(keyID, cause, action string, before, after any) error {
	invErr := c.invalidate(keyID, cause)
	if err := c.recordAudit(action, "key", keyID, before, after); err != nil {
		return err
	}
	return invErr
}
