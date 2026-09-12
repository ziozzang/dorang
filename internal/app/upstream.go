package app

import (
	"fmt"

	"github.com/ziozzang/dorang/internal/backend"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// upstreamTable is the provider and credential lookup the dispatcher hands to
// internal/backend. It is immutable; a hot reload builds a new one and swaps the
// pointer, so an in-flight request keeps the snapshot it started with.
//
// The split of responsibility is DESIGN §1's L4/L5 line: this package turns a
// configuration file into [backend.Provider] values and a routing decision into
// a [backend.Target], and never builds an HTTP request, spells a credential, or
// parses a provider's error envelope itself.
type upstreamTable struct {
	providers map[string]*backend.Provider
	creds     map[string]*credential
}

// newUpstreamTable resolves providers[] and the credential set.
//
// A provider with no base URL falls back to the base URL its kind declares in
// the catalog (DESIGN §4.3): the kind's URL is a DEFAULT for configuration, and
// a deployment that does not override it is asking for exactly that default.
// A provider that ends up with no URL at all is a start-up failure, because the
// alternative is a 502 on the first request that names it.
func newUpstreamTable(cfg *config.Config, cat *catalog.Catalog) (*upstreamTable, error) {
	t := &upstreamTable{
		providers: make(map[string]*backend.Provider, len(cfg.Providers)),
		creds:     collectCredentials(cfg),
	}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		base := p.BaseURL
		if base == "" {
			if kd, ok := cat.Kind(p.Kind); ok {
				base = kd.BaseURL
			}
		}
		prov, err := backend.NewProvider(backend.Spec{
			Name:    p.Name,
			Kind:    p.Kind,
			API:     apiFor(cat, p.Kind),
			BaseURL: base,
			// Whether the host serves /responses alone is the catalog's claim
			// about the kind, and it decides the adapter. Passing it is what
			// makes the two settings below refusable on any other host.
			ResponsesOnly: responsesOnlyFor(cat, p.Kind),
			// A Responses-only host's contract, stated by the deployment.
			ResponsesForceStream: p.Params.ForceStream,
			ResponsesStoreFalse:  p.Params.StoreFalse,
			Timeout:              p.Timeout.Duration(),
			// providers[].retry, which is a different thing from the fallback
			// chain of §7.6 and is documented as such on [backend.Policy].
			Retry: backend.Policy{
				MaxAttempts: p.Retry.MaxAttempts,
				Backoff:     p.Retry.Backoff,
				Base:        p.Retry.Base.Duration(),
			},
			// providers[].params.drop. Without this line the whole mechanism —
			// the validated list, the refusals, the neutral-request removal and
			// the x-dorang-dropped-params report — is live and no configuration
			// file can reach it (DESIGN §17.1). backend.NewProvider validates
			// the list against this provider's wire shape, so an undroppable
			// name is a START-UP error naming the provider rather than a 400
			// from the upstream on the first request.
			DropParams: p.Params.Drop,
		})
		if err != nil {
			return nil, fmt.Errorf("app: provider %q (kind %q): %w", p.Name, p.Kind, err)
		}
		t.providers[p.Name] = prov
	}
	return t, nil
}

func (t *upstreamTable) provider(name string) (*backend.Provider, bool) {
	u, ok := t.providers[name]
	return u, ok
}

func (t *upstreamTable) secret(credentialID string) string {
	if c, ok := t.creds[credentialID]; ok {
		return c.secret
	}
	return ""
}

// responsesOnlyFor reads the catalog's declaration that a kind's host serves
// `/responses` and nothing else. An unknown kind is not one: the default is the
// chat address, which is the one most hosts serve.
func responsesOnlyFor(cat *catalog.Catalog, kind string) bool {
	kd, ok := cat.Kind(kind)
	return ok && kd.ResponsesOnly
}
