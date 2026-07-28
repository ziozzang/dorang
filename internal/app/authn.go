package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/store"
	"github.com/ziozzang/dorang/pkg/catalog"
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
}

// LoadByLookup implements auth.Store.
func (s *authStore) LoadByLookup(ctx context.Context, lookup string) (auth.Record, error) {
	k, err := s.st.GetAPIKeyByLookup(ctx, lookup)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return auth.Record{}, auth.ErrNotFound
		}
		return auth.Record{}, err
	}
	return recordFromAPIKey(k)
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

// recordFromAPIKey converts a stored row into the authenticator's view of it.
//
// Every authorization field of DESIGN §2.4 is carried across. A field that is
// dropped here is a field the gateway fails open on, which is exactly what
// R1-A recorded, so the conversion is exhaustive rather than convenient.
func recordFromAPIKey(k *store.APIKey) (auth.Record, error) {
	digest, err := auth.ParseDigest(k.TokenHash)
	if err != nil {
		return auth.Record{}, err
	}
	scheme, err := auth.ParseScheme(string(k.HashScheme))
	if err != nil {
		return auth.Record{}, err
	}
	return auth.Record{
		Lookup: k.Lookup,
		Digest: digest,
		Scheme: scheme,
		Principal: auth.Principal{
			KeyID:         k.ID,
			Label:         k.KeyLabel,
			UserID:        k.UserID,
			TeamID:        k.TeamID,
			PriorityClass: k.PriorityClass,
			Tags:          k.Tags,
			Key: auth.Limits{
				Blocked:          k.Blocked,
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
			},
		},
	}, nil
}

// authAdapter satisfies server.Authenticator with the real authenticator.
//
// The two differ in one place only: internal/server's Principal is an
// interface, and internal/auth's is a struct with a KeyID FIELD. A method and a
// field cannot share a name, so the struct is wrapped rather than embedded.
type authAdapter struct {
	a     *auth.Authenticator
	now   func() time.Time
	rates *rateMeter
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
//
// One is built per request, which is what lets counted below be a plain field:
// it records that THIS request has already been counted against the rate
// meters, so a handler that authorizes fifty batch rows does not count fifty
// requests.
type principal struct {
	p       *auth.Principal
	now     func() time.Time
	rates   *rateMeter
	counted bool
}

// KeyID implements server.Principal.
func (p *principal) KeyID() string { return p.p.KeyID }

// UserID implements server.Principal.
func (p *principal) UserID() string { return p.p.UserID }

// TeamID implements server.Principal.
func (p *principal) TeamID() string { return p.p.TeamID }

// IsMaster reports the out-of-band administrative credential.
//
// It exists so that principalID can give a master-credential upload a real
// owner. The master has no api_keys row, so its KeyID is "" — and an object
// recorded with an empty owner used to be readable, usable and deletable by
// every key in the deployment (batch.ownedBy).
func (p *principal) IsMaster() bool { return p.p != nil && p.p.Master }

// Authorize implements server.Principal.
//
// The observed rates come from the meter rather than being left at zero, which
// is the whole of DESIGN §11.2's rate half. Reading before recording is
// deliberate: the ceiling is "requests already counted this minute", so the
// request being authorized is not one of them, and a limit of 1 admits one
// request rather than none.
func (p *principal) Authorize(a server.Access) error {
	err := p.p.Authorize(auth.Access{
		Now: p.now(), Model: a.Model, Route: a.Route, Rates: p.rates,
	})
	if err != nil {
		return authError(err)
	}
	if !p.counted {
		p.counted = true
		p.rates.record(p.p, 1, 0)
	}
	return nil
}

// recordTokens adds a finished request's token count to the rate meters. It is
// called once, at settlement, because that is when the number exists.
func (p *principal) recordTokens(n int64) {
	if p == nil || n <= 0 {
		return
	}
	p.rates.record(p.p, 0, n)
}

// MaxParallel implements the ceiling the dispatcher reads for
// capacity.Request.PrincipalMax. The most restrictive of key, user and team
// wins, matching every other limit on this type.
func (p *principal) MaxParallel() int {
	if p == nil || p.p == nil || p.p.Master {
		return 0
	}
	return auth.MostRestrictiveParallel(&p.p.Key, p.p.User, p.p.Team)
}

// ModelsRestricted implements server.ModelRestricted.
//
// It answers "is there a list at all", which is what a route that could not
// determine the model has to know before it decides whether that is fine or
// fatal. A key with no allow-list anywhere is unrestricted and a passthrough
// body dorang cannot parse costs it nothing.
func (p *principal) ModelsRestricted() bool {
	if p == nil || p.p == nil || p.p.Master {
		return false
	}
	for _, l := range []*auth.Limits{&p.p.Key, p.p.User, p.p.Team} {
		if l == nil {
			continue
		}
		for _, m := range l.Models {
			if m == "*" {
				continue
			}
			if m != "" {
				return true
			}
		}
	}
	return false
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

// applyCredential puts a provider credential on an outbound request in the
// spelling that provider kind expects.
//
// The client's own credential never travels upstream — internal/auth strips all
// six accepted headers — so this is the only thing that authenticates dorang to
// a backend.
func applyCredential(api catalog.API, secret string, h http.Header) {
	if secret == "" {
		return
	}
	switch api {
	case catalog.APIAnthropicMessages:
		h.Set("x-api-key", secret)
		if h.Get("anthropic-version") == "" {
			h.Set("anthropic-version", DefaultAnthropicVersion)
		}
	default:
		h.Set("Authorization", "Bearer "+secret)
	}
}

// DefaultAnthropicVersion is the anthropic-version header sent upstream when a
// deployment does not carry one. The header is mandatory on that surface and a
// request without it is refused, so it has a default rather than being omitted.
const DefaultAnthropicVersion = "2023-06-01"
