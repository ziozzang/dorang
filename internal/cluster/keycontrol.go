package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/notify"
	"github.com/ziozzang/dorang/internal/store"
)

// KeyControl is the operator-facing half of DESIGN §11.2c and §11.6: the five
// controls that change whether a key serves, each of which does the durable
// write AND publishes the invalidation that makes the change take effect.
//
// The two halves are one method each, deliberately. Every one of them is a
// control whose value is that the key stops serving NOW; a method that wrote
// the row and left the announcement to a caller would be a method whose
// contract is "half of a revocation", and half a revocation is a timer.
type KeyControl struct {
	st    *store.Store
	authn *auth.Authenticator
	notif *notify.Notifier
	now   func() time.Time
}

// NewKeyControl builds the controls.
//
// The authenticator is required: these operations are defined by their effect
// on what the gateway serves, and one that could not reach the snapshot would
// only be able to change a database.
func NewKeyControl(st *store.Store, authn *auth.Authenticator, n *notify.Notifier, now func() time.Time) (*KeyControl, error) {
	if st == nil {
		return nil, errors.New("cluster: NewKeyControl needs a store")
	}
	if authn == nil {
		return nil, errors.New("cluster: NewKeyControl needs an authenticator; a control that " +
			"cannot stop a key from serving is a control that writes a flag (DESIGN §11.2c)")
	}
	if now == nil {
		now = time.Now
	}
	return &KeyControl{st: st, authn: authn, notif: n, now: now}, nil
}

// Pend marks a key pended and stops it serving. It implements
// keyguard.Enforcer.
func (c *KeyControl) Pend(ctx context.Context, keyID, reason string) error {
	lookups, err := c.st.PendKey(ctx, keyID, reason)
	if err != nil {
		return err
	}
	return c.announce(ctx, keyID, lookups, auth.CausePended)
}

// Release clears a pend and lets the key serve again. It implements
// keyguard.Enforcer.
//
// The release publishes too. A pend that ends only when a TTL says so is a
// pend an operator cannot end, and "released in one action" means the action
// has an effect the caller can observe on their next request.
func (c *KeyControl) Release(ctx context.Context, keyID string) error {
	lookups, err := c.st.ReleaseKey(ctx, keyID)
	if err != nil {
		return err
	}
	return c.announce(ctx, keyID, lookups, auth.CauseReleased)
}

// Revoke blocks a key and stops it serving. It implements keyguard.Enforcer.
func (c *KeyControl) Revoke(ctx context.Context, keyID string) error {
	lookups, err := c.st.RevokeKey(ctx, keyID)
	if err != nil {
		return err
	}
	return c.announce(ctx, keyID, lookups, auth.CauseRevoked)
}

// Rotate mints a new secret behind an existing key id and returns what
// happened, including when the old secret expires.
//
// The plaintext is the caller's: it is passed in, hashed, and never stored or
// returned by this method. `POST /key/rotate` returns it exactly once, and the
// only way to keep that property is for the layer that persists things never to
// hold it in a struct that outlives the call.
func (c *KeyControl) Rotate(ctx context.Context, keyID string, v store.KeySecret, p store.RotationPolicy) (store.Rotation, error) {
	rot, err := c.st.RotateKey(ctx, keyID, v, p)
	if err != nil {
		return store.Rotation{}, err
	}
	// A rotation does not stop the old secret — that is the whole point of a
	// grace period — but it does change what the key's rows say, so a node
	// holding a cached copy must re-read. Any secret this rotation cut outright
	// to honour max_secrets is named explicitly.
	cause := auth.CauseRotated
	if len(rot.InvalidateLookups) > 0 {
		cause = auth.CauseGraceCut
	}
	if err := c.announce(ctx, keyID, rot.InvalidateLookups, cause); err != nil {
		return rot, err
	}
	c.notifyRotation(keyID, "rotate", []notify.Field{
		{Name: "generation", Value: fmt.Sprint(rot.New.Generation)},
		{Name: "previous_generation", Value: fmt.Sprint(rot.Previous.Generation)},
		{Name: "previous_expires_at", Value: rot.PreviousExpiresAt.UTC().Format(time.RFC3339)},
		{Name: "grace", Value: p.Grace.String()},
		{Name: "live_generations", Value: fmt.Sprint(2 - len(rot.Retired))},
	})
	return rot, nil
}

// CutGrace ends a rotation's grace period immediately.
//
// This is what a suspected compromise needs: rotate now, cut the old secret
// immediately, keep everything else. The current secret is untouched, so the
// caller who has already rolled is not locked out by the control protecting
// them.
func (c *KeyControl) CutGrace(ctx context.Context, keyID string) ([]string, error) {
	lookups, err := c.st.EndGrace(ctx, keyID)
	if err != nil {
		return nil, err
	}
	if err := c.announce(ctx, keyID, lookups, auth.CauseGraceCut); err != nil {
		return lookups, err
	}
	c.notifyRotation(keyID, "grace_cut", []notify.Field{
		{Name: "live_generations", Value: "1"},
	})
	return lookups, nil
}

// InvalidateKey publishes the fact that a key's authorization changed, for a
// caller that has ALREADY made the durable change itself.
//
// # Why this exists next to five methods that refuse to be halved
//
// The five controls above are each one method on purpose: a method that wrote
// the row and left the announcement to its caller would have the contract "half
// of a revocation", and half a revocation is a timer. That rule is about
// splitting ONE control across two callers, and it stands.
//
// This is the other case. The administration surface (internal/admin) owns a
// credential lifecycle this type does not model — bulk delete, a partial update
// that may or may not set `blocked`, a regeneration — and writes it through its
// own narrow key-store seam, which deliberately knows nothing about
// authenticators or snapshots. Before this method existed, the consequence was
// not a shape argument: `POST /key/block`, the route OPERATIONS §3.1 gives an
// operator for a leaked key, wrote the row and published nothing, so the key
// went on serving for the credential cache TTL (60 s) on every node that did not
// handle the request — against a published bound of 270 ms and a measured 9–15
// ms.
//
// The half that must not be separated is the one the OPERATOR sees, and it is
// not: the handler does not answer until both the write and this call have
// happened, so a 200 still means "it is durable and the fleet has been told".
//
// The lookups are read here rather than being passed in, because the caller that
// needs this has already lost them — a deleted key has no secret rows left at
// all. That is safe: an [auth.Invalidation] is authoritative on the durable key
// id, and the lookups only make the drop O(1) instead of a scan. A store that
// cannot answer is therefore not a reason to skip the announcement; it is a
// reason to send the one that still works.
func (c *KeyControl) InvalidateKey(ctx context.Context, keyID, cause string) error {
	if keyID == "" {
		return errors.New("cluster: InvalidateKey needs a key id")
	}
	parsed, ok := auth.ParseCause(cause)
	if !ok {
		// An unrecognised cause still invalidates and says nothing, which is
		// exactly what auth.CauseUnspecified is for. Refusing would trade a
		// working revocation for a spelling.
		parsed = auth.CauseUnspecified
	}
	lookups, err := c.st.KeyLookups(ctx, keyID)
	if err != nil {
		lookups = nil
	}
	return c.announce(ctx, keyID, lookups, parsed)
}

// WarnOverdue reports the keys whose current secret is older than max_age and
// sends one warning per key.
//
// §11.2c: max_age is a policy, not an execution. dorang warns and reports; it
// does not silently break a working integration on a timer. Nothing here
// rotates, pends or blocks anything, and there is deliberately no option that
// makes it do so.
func (c *KeyControl) WarnOverdue(ctx context.Context, maxAge time.Duration) (map[string]time.Duration, error) {
	overdue, err := c.st.KeysOverdueForRotation(ctx, maxAge)
	if err != nil {
		return nil, err
	}
	now := c.now()
	for id, age := range overdue {
		// Deduplicated, unlike the guard's alerts: an overdue key is overdue on
		// every sweep for as long as nobody rotates it, and a warning that
		// repeats every minute is a warning an operator filters.
		if !c.notif.Admit(notify.EventKeyRotated, notify.Subject{Kind: "key", ID: id}, now) {
			continue
		}
		c.notif.Send(notify.Notification{
			Event:   notify.EventKeyRotated,
			Subject: notify.Subject{Kind: "key", ID: id},
			At:      now,
			Fields: []notify.Field{
				{Name: "key_id", Value: id},
				{Name: "action", Value: "max_age_warning"},
				{Name: "age", Value: age.Round(time.Hour).String()},
				{Name: "max_age", Value: maxAge.String()},
			},
		})
	}
	return overdue, nil
}

// announce applies the invalidation locally and publishes it.
func (c *KeyControl) announce(ctx context.Context, keyID string, lookups []string, cause auth.InvalidationCause) error {
	return c.authn.Announce(ctx, auth.Invalidation{
		KeyID:   keyID,
		Lookups: auth.SortLookups(lookups),
		Cause:   cause,
		At:      c.now(),
	})
}

func (c *KeyControl) notifyRotation(keyID, action string, extra []notify.Field) {
	fields := append([]notify.Field{
		{Name: "key_id", Value: keyID},
		{Name: "action", Value: action},
	}, extra...)
	c.notif.Send(notify.Notification{
		Event:   notify.EventKeyRotated,
		Subject: notify.Subject{Kind: "key", ID: keyID},
		At:      c.now(),
		Fields:  fields,
	})
}

// KeyLoader reloads the whole credential set from the store. It is what a node
// uses on rejoin (§11.2c rule 4).
type KeyLoader struct {
	st *store.Store
	// Limit bounds one reload. A deployment with more keys than this loads a
	// prefix of them and serves the rest per-key from the store, which is
	// slower and correct — the alternative is a startup that reads an unbounded
	// table into memory.
	Limit int
	// Tiers is the operator's tier configuration, applied as each record is
	// built.
	Tiers *auth.TierSet
}

// NewKeyLoader builds a loader.
func NewKeyLoader(st *store.Store, limit int, tiers *auth.TierSet) *KeyLoader {
	if limit <= 0 {
		limit = DefaultLoaderLimit
	}
	return &KeyLoader{st: st, Limit: limit, Tiers: tiers}
}

// DefaultLoaderLimit bounds a snapshot reload.
const DefaultLoaderLimit = 50_000

var _ auth.Loader = (*KeyLoader)(nil)

// LoadAll implements [auth.Loader]. It returns one record per SECRET, not one
// per key: two secrets in a rotation grace period are two index keys and both
// must authenticate to the same principal.
func (l *KeyLoader) LoadAll(ctx context.Context) ([]auth.Record, error) {
	rows, err := l.st.ListKeyRecords(ctx, l.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]auth.Record, 0, len(rows))
	for i := range rows {
		rec, err := AuthRecord(rows[i].Key, rows[i].Secret, rows[i].Owners, l.Tiers)
		if err != nil {
			// One unreadable row must not cost the whole snapshot. It is
			// skipped, which leaves that key to the per-key store path, where
			// it will fail the same way and be reported against the request
			// that asked for it rather than against every key.
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// AuthRecord converts a stored key and one of its secrets into the shape
// internal/auth caches.
//
// It lives here rather than in internal/store because internal/auth
// deliberately does not import internal/store and the dependency must not be
// created in the other direction either: the narrow [auth.Record] is the seam,
// and this is the one adapter across it.
//
// Every authorization field of §2.4 is carried, because a field dropped here is
// a field the gateway fails open on. The tier is resolved through the operator's
// configured set and is applied to the key's own limits, so the envelope the
// hot path enforces is already narrowed — a tier that only decided a default
// somewhere else would be a tier the request path never sees.
func AuthRecord(k *store.APIKey, sec *store.KeySecret, own store.Owners,
	tiers *auth.TierSet) (auth.Record, error) {

	if k == nil || sec == nil {
		return auth.Record{}, errors.New("cluster: AuthRecord needs a key and a secret")
	}
	digest, err := auth.ParseDigest(sec.TokenHash)
	if err != nil {
		return auth.Record{}, err
	}
	scheme, err := auth.ParseScheme(string(sec.HashScheme))
	if err != nil {
		return auth.Record{}, err
	}
	p, err := AuthPrincipal(k, own, tiers)
	if err != nil {
		return auth.Record{}, err
	}
	// The secret's expiry is the LATER of nothing and its own; the key's expiry
	// is separate and already on the limits. Keeping them apart is what lets a
	// caller be told "use the secret from the last rotation" instead of "your
	// key expired".
	secretExpiry := sec.ExpiresAt
	if !sec.RevokedAt.IsZero() && (secretExpiry.IsZero() || sec.RevokedAt.Before(secretExpiry)) {
		secretExpiry = sec.RevokedAt
	}
	p.SecretID = sec.ID
	p.SecretGeneration = int(sec.Generation)
	p.SecretExpiresAt = secretExpiry
	return auth.Record{
		Lookup:    sec.Lookup,
		Digest:    digest,
		Scheme:    scheme,
		Principal: p,
	}, nil
}

// AuthPrincipal is the KEY half of [AuthRecord]: the authorization envelope a
// stored row carries, with no secret attached.
//
// It exists because the batch path has to recover a row's owner from an api key
// id alone, hours after the request that created the batch is gone and with no
// secret in hand. Building a second, batch-shaped view of a credential's limits
// there would be a second place for a field to go missing — which is exactly
// what R1-A recorded — so both callers derive from this one conversion and a new
// authorization column has one place to be added.
//
// Every authorization field of §2.4 is carried, because a field dropped here is
// a field the gateway fails open on. The tier is resolved through the operator's
// configured set and is applied to the key's own limits, so the envelope the hot
// path enforces is already narrowed — a tier that only decided a default
// somewhere else would be a tier the request path never sees.
//
// # The owners, and why they are an argument
//
// DESIGN §11.2's envelope is THREE subjects — the key, its user, its team — and
// the most restrictive wins. [auth.Principal] has carried all three since it was
// written and [auth.Principal.Authorize] enforces all three; this function
// populated the first and left the other two nil. Every guard downstream is
// nil-guarded, so nothing broke and nothing was enforced: `users.blocked` and
// `teams.blocked` refused nothing, a team's `max_budget_nano` bound nothing, and
// a team's rate and concurrency ceilings were read from a nil pointer that is
// treated as "declares no limit". That is the shape §17.1 calls this codebase's
// dominant failure — a control that exists and is not reached — and it was not
// a latency problem: a user block took effect at no latency, ever.
//
// The owners are therefore a REQUIRED argument rather than an optional lookup
// this function could do for itself. It has no store handle and must not grow
// one — it is called from the bulk loader, from the credential miss path and
// from the batch executor, and a per-call fetch inside it would put two store
// round trips on each of them. Making it an argument means every construction
// site has to answer "what are this key's owners?", which is exactly the
// question that went unasked; a zero [store.Owners] is a legitimate answer
// ("this key is unowned"), and it is the one an honest caller supplies.
//
// # What the tier does and does not narrow
//
// The tier is applied to the KEY's limits only. A tier is granted to a
// credential (§11.6), not to a person or an organization, and applying it to all
// three envelopes would narrow one grant three times — a tier capping requests
// per minute at 60 would produce a key, a user and a team each independently
// capped at 60, which is not what "the most restrictive wins" means. The user's
// and the team's limits are their own, and Authorize already takes the strictest
// across all three.
func AuthPrincipal(k *store.APIKey, own store.Owners, tiers *auth.TierSet) (auth.Principal, error) {
	if k == nil {
		return auth.Principal{}, errors.New("cluster: AuthPrincipal needs a key")
	}
	limits := auth.Limits{
		Blocked:          k.Blocked,
		Pended:           k.Pended(),
		ExpiresAt:        k.ExpiresAt,
		Models:           k.Models,
		AllowedRoutes:    k.AllowedRoutes,
		MaxBudgetNanoUSD: k.MaxBudgetNano,
		SpentNanoUSD:     k.SpendNano,
		BudgetPeriod:     k.BudgetPeriod,
		BudgetResetAt:    k.BudgetResetAt,
		RPMLimit:         k.RPMLimit,
		TPMLimit:         k.TPMLimit,
		MaxParallel:      k.MaxParallel,
	}
	class := k.PriorityClass
	tierName := k.Tier
	if tiers != nil {
		t, terr := tiers.Resolve(k.Tier)
		if terr != nil {
			// A row naming a tier the configuration no longer has is a
			// configuration error, and it is reported rather than resolved to
			// the default — resolving it would silently move a key to a tier
			// nobody assigned, in whichever direction the default happens to
			// lie.
			return auth.Principal{}, terr
		}
		tierName = t.Name
		limits = t.Apply(limits)
		class = t.NarrowClass(class, tiers.Classes())
	}
	return auth.Principal{
		KeyID:         k.ID,
		Tier:          tierName,
		Label:         k.KeyLabel,
		UserID:        k.UserID,
		TeamID:        k.TeamID,
		PriorityClass: class,
		Tags:          k.Tags,
		Key:           limits,
		User:          userLimits(own.User),
		Team:          teamLimits(own.Team),
	}, nil
}

// userLimits is the authorization envelope of a `users` row, or nil when the key
// has no owning user.
//
// nil and "a user that declares nothing" are deliberately different values even
// though [auth.Principal.Authorize] treats them alike today: nil says the join
// found no row, and a present envelope full of zeroes says the row exists and
// restricts nothing. The distinction is what makes [auth.Principal.RequireOwner]
// mean something, and it is what a future check on ownership would read.
//
// Neither Pended nor MaxParallel is set: `users` has no pend column (§11.6's
// pend is the token guard's judgement about a CREDENTIAL) and no
// max_parallel column. Writing zero values for them would be indistinguishable
// from a limit the operator set, and only `teams` has the concurrency column.
func userLimits(u *store.User) *auth.Limits {
	if u == nil {
		return nil
	}
	return &auth.Limits{
		Blocked:          u.Blocked,
		Models:           u.Models,
		MaxBudgetNanoUSD: u.MaxBudgetNano,
		SpentNanoUSD:     u.SpendNano,
		BudgetPeriod:     u.BudgetPeriod,
		BudgetResetAt:    u.BudgetResetAt,
		RPMLimit:         u.RPMLimit,
		TPMLimit:         u.TPMLimit,
	}
}

// teamLimits is [userLimits] for a `teams` row. It carries max_parallel, which
// the team table has and the users table does not — internal/capacity narrows a
// request to the smallest ceiling declared across the three subjects, and a team
// concurrency cap that never reached it was a configured limit with no effect.
func teamLimits(t *store.Team) *auth.Limits {
	if t == nil {
		return nil
	}
	return &auth.Limits{
		Blocked:          t.Blocked,
		Models:           t.Models,
		MaxBudgetNanoUSD: t.MaxBudgetNano,
		SpentNanoUSD:     t.SpendNano,
		BudgetPeriod:     t.BudgetPeriod,
		BudgetResetAt:    t.BudgetResetAt,
		RPMLimit:         t.RPMLimit,
		TPMLimit:         t.TPMLimit,
		MaxParallel:      t.MaxParallel,
	}
}
