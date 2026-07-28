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
		rec, err := AuthRecord(rows[i].Key, rows[i].Secret, l.Tiers)
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
func AuthRecord(k *store.APIKey, sec *store.KeySecret, tiers *auth.TierSet) (auth.Record, error) {
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
			return auth.Record{}, terr
		}
		tierName = t.Name
		limits = t.Apply(limits)
		class = t.NarrowClass(class, tiers.Classes())
	}
	// The secret's expiry is the LATER of nothing and its own; the key's expiry
	// is separate and already on the limits. Keeping them apart is what lets a
	// caller be told "use the secret from the last rotation" instead of "your
	// key expired".
	secretExpiry := sec.ExpiresAt
	if !sec.RevokedAt.IsZero() && (secretExpiry.IsZero() || sec.RevokedAt.Before(secretExpiry)) {
		secretExpiry = sec.RevokedAt
	}
	return auth.Record{
		Lookup: sec.Lookup,
		Digest: digest,
		Scheme: scheme,
		Principal: auth.Principal{
			KeyID:            k.ID,
			SecretID:         sec.ID,
			SecretGeneration: int(sec.Generation),
			SecretExpiresAt:  secretExpiry,
			Tier:             tierName,
			Label:            k.KeyLabel,
			UserID:           k.UserID,
			TeamID:           k.TeamID,
			PriorityClass:    class,
			Tags:             k.Tags,
			Key:              limits,
		},
	}, nil
}
