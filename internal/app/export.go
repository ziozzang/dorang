package app

import (
	"context"
	"strings"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/store"
)

// This file is the surface cmd/dorangctl uses. It exists so the CLI answers
// with the SAME construction a running server would use rather than a second
// one that happens to agree today — DESIGN §8.4's "the admin UI calculator and
// the CLI use the same engine, so there is one answer, not three" applies to
// every one of these, not only to pricing.

// Pricing compiles the price catalog a gateway started on this configuration
// would price against: the catalog file and the rules kept in the main file,
// spliced exactly as the server splices them.
func Pricing(cfg *config.Config) (*pricing.Catalog, error) { return buildPricing(cfg) }

// OpenStore opens the store this configuration names, applying migrations
// unless skipMigrate says otherwise.
//
// It resolves the key pepper the same way the server does, which matters: a CLI
// that issued keys under a different pepper would issue keys the server cannot
// verify.
func OpenStore(ctx context.Context, cfg *config.Config, skipMigrate bool) (*store.Store, error) {
	pepper, _, err := resolvePepper(cfg)
	if err != nil {
		return nil, err
	}
	sc, err := storeConfig(cfg, pepper)
	if err != nil {
		return nil, err
	}
	sc.SkipMigrate = skipMigrate
	return store.Open(ctx, sc)
}

// PriceTarget is where a client-facing model name would actually be served, as
// the pricing engine needs to see it (DESIGN §8.2's specificity ladder is keyed
// on all four dimensions).
type PriceTarget struct {
	Group         string
	Provider      string
	UpstreamModel string
	Deployment    string
	Credential    string
	Kind          string
}

// ResolveTarget maps a client-facing name onto its first deployment.
//
// It resolves an alias but never splits a name (§2.1). "First" is the same
// choice the batch resolver makes and for the same reason: a price preview
// answers for one target, and the first one in configuration order is the one an
// operator reading the file would expect.
func ResolveTarget(cfg *config.Config, name string) (PriceTarget, bool) {
	m, ok := cfg.ResolveModel(name)
	if !ok || len(m.Deployments) == 0 {
		return PriceTarget{}, false
	}
	d := &m.Deployments[0]
	t := PriceTarget{
		Group:         m.Name,
		Provider:      d.Provider,
		UpstreamModel: d.UpstreamModel,
		Deployment:    deploymentID(m.Name, d.Provider, d.UpstreamModel),
	}
	if len(d.Credentials) > 0 {
		t.Credential = d.Credentials[0]
	}
	if p, ok := cfg.Provider(d.Provider); ok {
		t.Kind = p.Kind
	}
	return t, true
}

// CheckConfig loads a configuration for VALIDATION rather than for running.
//
// It differs from config.Load in exactly one way: a secret that cannot be READ
// on this machine is returned as a warning instead of an error. A configuration
// is routinely checked from a laptop or a CI runner that does not hold the
// deployment's key material, and refusing to check the other four hundred lines
// because of that helps nobody.
//
// Everything else stays an error, including the three configurations the design
// refuses to start on — clustering with local capacity accounting (§5.6), an
// inline literal secret outside development (§4.1), and legacy key hashing with
// no expiry date (§2.4). An inline literal in particular is a WRONG REFERENCE,
// not a missing one, so it is never softened here.
//
// The returned *config.Config is nil when only warnings prevented a full load.
func CheckConfig(path string) (*config.Config, []string, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil, nil
	}
	problems := config.Problems(err)
	if len(problems) == 0 {
		return nil, nil, err
	}
	warnings := make([]string, 0, len(problems))
	for _, p := range problems {
		if !UnreadableSecret(p.Path) {
			return nil, nil, err
		}
		warnings = append(warnings, p.Error())
	}
	return nil, warnings, nil
}

// UnreadableSecret reports whether a validation problem is "the secret is not
// on this machine" rather than "the reference is wrong".
func UnreadableSecret(path string) bool {
	return strings.HasSuffix(path, ".key_env") || strings.HasSuffix(path, ".key_file")
}
