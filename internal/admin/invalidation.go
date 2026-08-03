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
// # The second half: the subjects a key hangs off
//
// The sweep that closed the eight routes above enumerated KEY mutations, and
// that was the wrong noun. §11.2's envelope is three subjects — the key, its
// user, its team — and [auth.Principal] caches all three in one entry, which is
// why the hot path refuses on `p.User.Blocked` and `p.Team.Blocked` from the
// CACHED copy. Blocking a user therefore had exactly the defect blocking a key
// had, at exactly the same cost: sixty seconds on every node that did not serve
// the call, against a published bound of 270 ms. So did `/user/delete`,
// `/team/update`, `/team/delete` — and `/budget/*`, whose ceiling is
// [auth.Limits.MaxBudgetNanoUSD] on the same cached entry, which is the third
// class and the one two sweeps had now each walked past.
//
// [Invalidator] names a KEY, deliberately (see its documentation). The subject
// routes therefore resolve the subject to the keys underneath it — see
// [call.ownedKeys] — and announce one message per key. That keeps the message a
// key id, which is what the receiving node can act on without a join, and it
// keeps this package's dependency on the cluster at one method.
//
// # What is deliberately NOT published
//
// Creation, of a key. A key that has never been seen has nothing cached to
// drop, and the bound for "used before it existed, then created" is the
// negative-entry TTL (`auth.DefaultNegativeTTL`, 5 s) rather than a mistake.
// `/key/generate` therefore announces nothing, and that is a decision rather
// than an omission.
//
// Creating a USER or a TEAM is NOT the same case and does publish, for keys that
// already name the id. The asymmetry is real rather than an oversight: a key's
// id is minted by the route that creates it, so nothing can reference it first,
// whereas `/user/new` and `/team/new` both accept a caller-chosen id and a key
// may already carry it — those keys were serving with no owner's envelope and
// are now serving with one. In the normal case the enumeration finds nothing and
// nothing is published.
//
// Team MEMBERSHIP. `/team/member_add` and `/team/member_delete` write
// `team_members`, and no cached authorization decision is derived from that
// table: a key's team is `api_keys.team_id` (§9.2), the administrative scope a
// key carries is derived from the same column, and the administrative role join
// is re-read from the store on every administrative request rather than cached.
// A membership change therefore does not change whether any request is served,
// and publishing one would be a message no node could act on.
//
// Models and deployments. `/model/*` changes whether a MODEL is servable, which
// is a routing fact rather than a credential one: it reaches no entry in the
// credential cache, and [Invalidator] — one key id and a cause — has no shape
// that could carry it. The routing table is reloaded by `/admin/config/reload`,
// per node, which is what OPERATIONS tells an operator to run against the fleet.

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

// maxAnnouncedKeys bounds one subject mutation's announcement.
//
// It exists so that "block this user" cannot become an unbounded scan of the key
// table inside an HTTP handler. It is deliberately far above any real fan-out —
// a user or a team with ten thousand credentials is a deployment doing something
// this surface was not designed for — and reaching it is REPORTED rather than
// silently truncated, because an announcement that covered a prefix of the keys
// and said nothing would be the same defect this file exists to remove, wearing
// a limit.
const maxAnnouncedKeys = 10_000

// ownedKeys resolves a subject — a user, or a team — to the keys whose cached
// authorization envelope is derived from it.
//
// It is the whole of what the subject routes need that the key routes did not.
// [Invalidator] names a key id, and a node applying an invalidation drops that
// key's entries; a user's block reaches the hot path through
// [auth.Principal.User], which lives in those same entries, so dropping them is
// what makes the block take effect.
//
// It is called BEFORE the durable write, always, including on the routes where
// the ownership set cannot change. A delete has to be enumerated first — after
// it, the rows that named the subject are gone and there is nothing left to
// resolve — and having one order for every route is what stops the next handler
// from picking the wrong one. The consequence is the good one: a store that
// cannot answer refuses the mutation BEFORE it happens, rather than leaving a
// durable change nobody can announce.
//
// It returns nothing at all when no invalidator is wired: the deployment has
// accepted the entry TTL as its bound (see [Invalidator]), and a listing whose
// only consumer is an announcement that will not be sent is a query for nothing.
func (c *call) ownedKeys(f KeyFilter) ([]string, error) {
	if c.a.cfg.Invalidator == nil || c.a.cfg.Keys == nil {
		return nil, nil
	}
	if f.UserID == "" && f.TeamID == "" {
		return nil, nil
	}
	f.Limit = c.a.cfg.MaxListLimit
	f.Offset = 0
	var ids []string
	for {
		page, err := c.a.cfg.Keys.ListKeys(c.ctx(), f)
		if err != nil {
			return nil, err
		}
		for _, k := range page {
			ids = append(ids, k.ID)
		}
		if len(page) < f.Limit {
			return ids, nil
		}
		if len(ids) >= maxAnnouncedKeys {
			flt := newFault(http.StatusInternalServerError, CodeInvalidationFailed, typeAPI,
				"this subject owns more than %d keys, which is more than one change can announce; "+
					"the change has NOT been applied. Block or delete the keys directly, in batches, "+
					"so that each one is announced", maxAnnouncedKeys)
			flt.Detail = map[string]any{
				"user_id": f.UserID, "team_id": f.TeamID, "limit": maxAnnouncedKeys,
			}
			return nil, flt
		}
		f.Offset += len(page)
	}
}

// invalidateKeys announces one applied change for every key of a subject.
//
// The FIRST failure is remembered and the loop continues, for the reason
// `/key/delete`'s does: stopping half way through would leave the rest of a
// blocked user's credentials serving on the TTL because an earlier one could not
// be published, which is the worst available outcome and the one an early return
// produces by default.
func (c *call) invalidateKeys(keyIDs []string, cause string) error {
	var first error
	for _, id := range keyIDs {
		if err := c.invalidate(id, cause); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// appliedTo is [call.applied] for a mutation on a subject rather than on a key:
// announce every key underneath it, then write the one trail row that records
// what the operator actually did.
//
// Same order and the same reason. The announcement is what makes the change take
// effect so it goes first; the audit row is written even when the announcement
// failed, and its fault wins, because a mutation that took effect and left no
// record is the worse of the two outcomes.
func (c *call) appliedTo(keyIDs []string, cause, action, kind, id string, before, after any) error {
	invErr := c.invalidateKeys(keyIDs, cause)
	if err := c.recordAudit(action, kind, id, before, after); err != nil {
		return err
	}
	return invErr
}
