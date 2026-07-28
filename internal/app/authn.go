package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/keyguard"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/store"
)

// authStore adapts *store.Store onto auth.Store and auth.Rehasher.
//
// internal/auth declares a one-method Store so that a cache, an HTTP delegate
// or a test double substitute for persistence without it importing the store.
// This is the real implementation of that method, plus the optional upgrade
// method that turns rehash-on-use on.
//
// auth.New enables the upgrade path only when its Store also implements
// auth.Rehasher, so an adapter that omits Rehash does not degrade the feature —
// it removes it silently, and DESIGN §2.4's "the migration completes without
// downtime" becomes a legacy window that never narrows.
type authStore struct {
	st *store.Store
	// tiers is the operator's tier configuration (DESIGN §11.6). It is applied
	// as each record is built, so the envelope the hot path enforces is already
	// narrowed to what the key's tier grants. A tier that only decided a default
	// elsewhere would be a tier the request path never sees.
	tiers *auth.TierSet
}

// LoadByLookup implements auth.Store.
//
// It resolves the index key to a (key, secret) PAIR rather than to a key. A
// rotation leaves two live index keys behind one durable id (DESIGN §11.2c) and
// both must authenticate to the same principal; a lookup that resolved only the
// current secret would refuse the caller who has not rolled yet as an unknown
// key, which is the failure a grace period exists to prevent.
func (s *authStore) LoadByLookup(ctx context.Context, lookup string) (auth.Record, error) {
	k, sec, err := s.st.ResolveKeyByLookup(ctx, lookup)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return auth.Record{}, auth.ErrNotFound
		}
		return auth.Record{}, err
	}
	return cluster.AuthRecord(k, sec, s.tiers)
}

// Rehash implements auth.Rehasher.
//
// The digest arrives already computed, because the authenticator derived it
// while verifying and the token itself must not outlive that verification. The
// store's upgrade is conditional on the row still being legacy, so a repeated
// or concurrent upgrade is a no-op rather than a race, and both packages agree
// on the digest because they are peppered from the same value (see [New]).
func (s *authStore) Rehash(ctx context.Context, keyID, lookup string, digest auth.Digest) error {
	return s.st.SetKeyDigest(ctx, keyID, lookup, digest.Hex(), store.SchemeDorangV1)
}

// authAdapter satisfies server.Authenticator with the real authenticator.
//
// The two differ in one place only: internal/server's Principal is an
// interface, and internal/auth's is a struct with a KeyID FIELD. A method and a
// field cannot share a name, so the struct is wrapped rather than embedded.
type authAdapter struct {
	a     *auth.Authenticator
	now   func() time.Time
	rates *keyRates
}

// AuthenticateHeader implements server.Authenticator.
func (ad *authAdapter) AuthenticateHeader(ctx context.Context, h http.Header) (server.Principal, error) {
	p, err := ad.a.AuthenticateHeader(ctx, h)
	if err != nil {
		return nil, authError(err)
	}
	return &principal{p: p, now: ad.now, rates: ad.rates}, nil
}

// principal is the server's view of an authenticated caller.
type principal struct {
	p   *auth.Principal
	now func() time.Time
	// rates supplies the rolling-minute counts a key's rpm_limit and tpm_limit
	// are compared against. Nil leaves both unenforced, which is what the whole
	// gateway did before it existed.
	rates *keyRates
}

// maxParallel is the key's max_parallel_requests, or 0 when it has none.
//
// It is read here rather than in internal/capacity because the broker keys its
// principal axis by api key id and takes its limits from a static configuration
// map. A per-key column cannot ride that channel: it is not in the file, it
// changes when an operator edits the key, and it belongs to the caller rather
// than to the deployment. So the limit travels with the request instead.
func (p *principal) maxParallel() int {
	if p == nil || p.p == nil || p.p.Master {
		return 0
	}
	best := int64(0)
	for _, l := range []*auth.Limits{&p.p.Key, p.p.User, p.p.Team} {
		if l == nil || l.MaxParallel == nil {
			continue
		}
		// Most restrictive wins across key, user and team (DESIGN §11.2), and a
		// configured zero really is zero: it refuses everything, which is why
		// the column is a pointer.
		if v := *l.MaxParallel; best == 0 || v < best {
			best = v
		}
	}
	if best < 0 {
		return 0
	}
	return int(best)
}

// priorityClass is the class an operator assigned this key (§7.5). It reaches
// the router through the dispatcher; before this it was loaded from the store,
// carried on the principal, and read by nothing but the Lua hook view — so every
// request routed as the default class regardless of what its key said.
func (p *principal) priorityClass() string {
	if p == nil || p.p == nil {
		return ""
	}
	return p.p.PriorityClass
}

// KeyID implements server.Principal.
func (p *principal) KeyID() string { return p.p.KeyID }

// SecretID implements server.SecretPrincipal (DESIGN §11.2c).
//
// It is the api_key_secrets row id, never any part of the secret. It reaches
// the ledger so an operator can see which credential a client is still
// presenting during a rotation's grace period, rather than finding out when the
// window shuts.
func (p *principal) SecretID() string { return p.p.SecretID }

// Tier implements the §11.6 tier accessor. The tier comes from the stored row
// and from nowhere else: an operator grants it, a caller cannot claim it.
func (p *principal) Tier() string { return p.p.Tier }

// IsAdmin implements server.AdminPrincipal.
//
// Only the master credential qualifies. There is no per-key administrative flag
// in the store yet, and inventing one here — reading a tag, say — would make the
// administrative boundary depend on a string a key-creation API already lets
// callers set. When a real flag exists this is the one place that reads it.
func (p *principal) IsAdmin() bool { return p.p.IsMaster() }

// UserID implements server.Principal.
func (p *principal) UserID() string { return p.p.UserID }

// TeamID implements server.Principal.
func (p *principal) TeamID() string { return p.p.TeamID }

// Authorize implements server.Principal.
//
// The observed rates are supplied HERE, at the one production construction of
// auth.Access, because this is the only place that has both the principal and a
// clock. Omitting them — which is what this function did — left every positive
// rpm_limit and tpm_limit comparing a configured ceiling against a hard-coded
// zero, so the check ran on every request and could never fire.
func (p *principal) Authorize(a server.Access) error {
	ac := auth.Access{Now: p.now(), Model: a.Model, Route: a.Route}
	// The master credential is authorized unconditionally and has no stored row
	// to carry limits, so it is not counted either: counting it would let an
	// administrative probe consume a tenant's window under a shared id.
	if !p.p.IsMaster() {
		req, tok := p.rates.observe(p.p.KeyID)
		ac.ObservedRPM, ac.ObservedTPM = clampInt(req), clampInt(tok)
	}
	if err := p.p.Authorize(ac); err != nil {
		return authError(err)
	}
	return nil
}

// clampInt narrows an int64 count to the int auth.Access carries, saturating
// rather than wrapping. A wrapped count would read as a small number and turn an
// exceeded ceiling into an allowed request, which is the one direction a limit
// must never fail in.
func clampInt(v int64) int {
	const maxInt = int64(^uint(0) >> 1)
	switch {
	case v < 0:
		return 0
	case v > maxInt:
		return int(maxInt)
	}
	return int(v)
}

// AllowsModel implements server.Principal.
//
// It answers the model allow-list question alone, not the whole authorization
// question: the list filters GET /v1/models, and a caller whose key expires
// while it reads the list should get the same list, then a 401 at the gate —
// not an empty catalog that looks like a deployment with no models.
func (p *principal) AllowsModel(model string) bool {
	if p.p.Master {
		return true
	}
	for _, l := range []*auth.Limits{&p.p.Key, p.p.User, p.p.Team} {
		if l == nil {
			continue
		}
		if !modelAllowed(l.Models, model) {
			return false
		}
	}
	return true
}

// modelAllowed applies one subject's allow-list. An empty list allows
// everything, and "*" does too. Names are compared WHOLE: a model name is
// opaque and nothing splits it (DESIGN §2.1).
func modelAllowed(list []string, model string) bool {
	if len(list) == 0 {
		return true
	}
	for _, m := range list {
		if m == "*" || m == model {
			return true
		}
	}
	return false
}

// authError renders an authentication or authorization failure as the server's
// error type, so its status, code and envelope reach the client verbatim
// instead of collapsing into a bare 401.
func authError(err error) error {
	var ae *auth.Error
	if errors.As(err, &ae) {
		status := ae.Status()
		return server.NewError(status, server.TypeForStatus(status), ae.Error()).
			WithCode(ae.Code())
	}
	return err
}

// How a provider credential is spelled on an outbound request lives in
// internal/backend, with the adapter that decides it: [backend.Provider.ApplyCredential].
// There is no second spelling here, because the two would only ever be found to
// disagree by an upstream answering 401.

// --- tiers, rotation and the guard (DESIGN §11.6, §11.2c) --------------------

// tierSet builds the operator's tier set from configuration.
//
// An empty `auth.tiers` takes the built-in `free < commercial < unlimited`.
// What is configuration is the set and the ordering; what is not is that a tier
// belongs to the key and is assigned by an operator — there is no path from a
// request to any of this (§10.5, §11.6).
//
// Position in the list IS the ordering, most privileged last, so an operator
// expresses "unlimited outranks commercial" by writing it in that order rather
// than by maintaining a number beside it.
func tierSet(cfg *config.Config) (*auth.TierSet, error) {
	if len(cfg.Auth.Tiers) == 0 {
		return auth.DefaultTiers(), nil
	}
	tiers := make([]auth.Tier, 0, len(cfg.Auth.Tiers))
	for i, t := range cfg.Auth.Tiers {
		tier := auth.Tier{
			Name:          t.Name,
			Rank:          i,
			PriorityClass: t.PriorityClass,
			RPMLimit:      t.RPMLimit,
			TPMLimit:      t.TPMLimit,
			MaxParallel:   t.MaxParallel,
			Models:        t.Models,
		}
		if t.MaxBudget != nil {
			n, err := usdToNano(fmt.Sprintf("auth.tiers[%d].max_budget", i), *t.MaxBudget)
			if err != nil {
				return nil, err
			}
			// A pointer, because nil is "the tier imposes no ceiling" and 0 is
			// "a ceiling of zero: allow nothing". Flattening them is how a
			// configured ceiling quietly stops existing (§2.4).
			tier.MaxBudgetNanoUSD = &n
		}
		tiers = append(tiers, tier)
	}
	return auth.NewTierSet(tiers, cfg.Auth.DefaultTier)
}

// rotationPolicy renders `auth.rotation` (§11.2c).
func rotationPolicy(cfg *config.Config) store.RotationPolicy {
	return store.RotationPolicy{
		Grace:      time.Duration(cfg.Auth.Rotation.Grace),
		MaxSecrets: cfg.Auth.Rotation.MaxSecrets,
		MaxAge:     time.Duration(cfg.Auth.Rotation.MaxAge),
	}
}

// guardConfig renders `token_guard` (§11.6). A disabled guard is the zero
// value, and [keyguard.New] answers it with a typed nil.
func guardConfig(cfg *config.Config, now func() time.Time) (keyguard.Config, error) {
	g := cfg.TokenGuard
	action, err := keyguard.ParseAction(g.Action)
	if err != nil {
		return keyguard.Config{}, err
	}
	return keyguard.Config{
		Enabled:        g.Enabled,
		BaselineWindow: time.Duration(g.BaselineWindow),
		Window:         time.Duration(g.Window),
		Factor:         g.Trigger.Factor,
		MinAbsolute:    g.Trigger.MinAbsolute,
		MinHistory:     time.Duration(g.MinHistory),
		Action:         action,
		Cooldown:       time.Duration(g.Cooldown),
		Now:            now,
	}, nil
}
