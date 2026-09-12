package app

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/config"
)

// buildOAuth turns every `auth: oauth` credential into a live credential that
// refreshes itself (DESIGN §11.2b).
//
// Until this function existed the whole subsystem was unreachable from a running
// binary: internal/auth implemented the credential, internal/backend declared the
// seam an *auth.OAuthCredential already satisfies, and there was no way to
// DECLARE one — internal/config had no oauth expression at all, and the
// dispatcher's Credential returned nil for OAuth with a comment saying so. That
// is DESIGN §17.1's defect class, and the two halves have to land together or
// the configuration guard in internal/config fails, which is deliberate.
//
// It returns nil when no credential authenticates by OAuth. A nil manager is the
// absence of the subsystem rather than an empty one: no background loop runs, no
// metric family is published, and the dispatcher's lookup is one nil check.
func buildOAuth(cfg *config.Config, now func() time.Time) (*auth.OAuthManager, error) {
	var m *auth.OAuthManager
	for i := range cfg.Credentials {
		cr := &cfg.Credentials[i]
		if !cr.IsOAuth() || cr.OAuth == nil {
			continue
		}
		c, err := newOAuthCredential(cr, now)
		if err != nil {
			return nil, err
		}
		if m == nil {
			m = auth.NewOAuthManager()
		}
		if err := m.Add(c); err != nil {
			return nil, fmt.Errorf("app: credential %q: %w", cr.ID, err)
		}
		// Read the store once, here, rather than leaving it to the background
		// loop's first pass. The loop starts after the HTTP surface does, and a
		// request that arrives in between would find a credential with no token
		// and be refused — a start-up race whose symptom is a 502 on the first
		// request and nothing at all a minute later.
		//
		// The error is deliberately dropped: an unreadable store is recorded in
		// the credential's health, and refusing to start the gateway because one
		// account's file is missing would take every other account down with it.
		_ = c.Reload()
	}
	return m, nil
}

// newOAuthCredential builds one credential from its configured block.
func newOAuthCredential(cr *config.Credential, now func() time.Time) (*auth.OAuthCredential, error) {
	o := cr.OAuth
	src, err := auth.ParseTokenSource(o.Source)
	if err != nil {
		return nil, fmt.Errorf("app: credential %q: %w", cr.ID, err)
	}
	format, err := auth.ParseStoreFormat(o.Format)
	if err != nil {
		return nil, fmt.Errorf("app: credential %q: %w", cr.ID, err)
	}
	ac := auth.OAuthConfig{
		ID:          cr.ID,
		Provider:    cr.Provider,
		Source:      src,
		Path:        o.Path,
		Command:     o.Command,
		EnvVar:      o.EnvVar,
		StoreFormat: format,
		Fields: auth.TokenFields{
			AccessToken:  o.AccessTokenField,  // pragma: allowlist secret — a field name
			RefreshToken: o.RefreshTokenField, // pragma: allowlist secret — a field name
			ExpiresAt:    o.ExpiresAtField,
			AccountID:    o.AccountIDField,
		},
		AccountHeader: o.AccountHeader,
		RefreshMargin: o.RefreshMargin.Duration(),
		PollInterval:  o.PollInterval.Duration(),
		ExecTimeout:   o.ExecTimeout.Duration(),
		Now:           now,
	}

	refresher, err := oauthRefresher(cr, now)
	if err != nil {
		return nil, err
	}
	c, err := auth.NewOAuthCredential(ac, refresher)
	if err != nil {
		return nil, fmt.Errorf("app: credential %q: %w", cr.ID, err)
	}
	return c, nil
}

// oauthRefresher builds the token exchange, or nil when none is configured.
//
// Nil is a supported deployment and the safest one: dorang reads the store the
// vendor's CLI keeps current, adopts whatever it finds there, and never writes.
// A refresh token is spent only when an operator has said where to spend it.
func oauthRefresher(cr *config.Credential, now func() time.Time) (auth.Refresher, error) {
	r := cr.OAuth.Refresh
	if cr.OAuth.Format == string(auth.FormatGCPServiceAccount) {
		// Minted from the key, not refreshed from a token: the file IS the
		// credential. token_url, when set, overrides the file's token_uri
		// (a test's fake; production leaves it to the file).
		sa, err := auth.NewServiceAccountRefresher(auth.ServiceAccountConfig{
			Path:     cr.OAuth.Path,
			Scope:    r.Scope,
			TokenURL: r.TokenURL,
			Timeout:  r.Timeout.Duration(),
			Now:      now,
		})
		if err != nil {
			return nil, fmt.Errorf("app: credential %q: %w", cr.ID, err)
		}
		return sa, nil
	}
	if r.TokenURL == "" {
		return nil, nil
	}
	enc, err := auth.ParseRefreshEncoding(r.Encoding)
	if err != nil {
		return nil, fmt.Errorf("app: credential %q: %w", cr.ID, err)
	}
	// An unresolved reference yields no value, and Value's second result is the
	// only way to tell "no secret configured" from "a secret that resolved to
	// the empty string" — the second being a public client, which is ordinary.
	secret, _ := r.ClientSecret.Value()
	hr, err := auth.NewHTTPRefresher(auth.RefreshConfig{
		TokenURL:     r.TokenURL,
		ClientID:     r.ClientID,
		ClientSecret: secret,
		Scope:        r.Scope,
		Encoding:     enc,
		Timeout:      r.Timeout.Duration(),
		Now:          now,
	})
	if err != nil {
		return nil, fmt.Errorf("app: credential %q: %w", cr.ID, err)
	}
	return hr, nil
}

// oauthCredentialSet renders the declared OAuth credentials as a comparable
// string, for [checkOAuthUnchanged].
//
// The token store's PATH is part of it and the token is not: a path is a
// reference and a token is a secret, and the whole of §11.2b rests on that
// distinction holding in every direction, including this one.
func oauthCredentialSet(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	var rows []string
	for i := range cfg.Credentials {
		cr := &cfg.Credentials[i]
		if !cr.IsOAuth() || cr.OAuth == nil {
			continue
		}
		o := cr.OAuth
		rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s",
			cr.ID, cr.Provider, o.Source, o.Path, strings.Join(o.Command, " "),
			o.EnvVar, o.Format, o.Refresh.TokenURL))
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

// checkOAuthUnchanged refuses a reload that changes the OAuth credential set.
//
// §4.1 says every section hot-reloads. This one cannot, for the reason the
// shadow section cannot: an OAuth credential is per-process state, not a
// snapshot. It holds the token in memory, the consecutive-failure count that
// paces its backoff, and one background loop that was started once. Rebuilding
// it on SIGHUP would discard all three — re-reading the store, re-arming the
// backoff a failing account had already been put behind, and leaving the old
// loop running against a credential nothing points at any more.
//
// A change is refused rather than half-applied. Everything else in the file
// still reloads; this returns an error naming the section, which is what an
// operator can act on.
func checkOAuthUnchanged(old, next *config.Config) error {
	before, after := oauthCredentialSet(old), oauthCredentialSet(next)
	if before == after {
		return nil
	}
	return fmt.Errorf("app: the OAuth credential set cannot be changed by a reload: " +
		"each credential holds a token, a backoff and a background loop that were " +
		"established when the process started, and rebuilding them would re-read every " +
		"store and re-arm the backoff of any account that is already failing. " +
		"Restart to apply it")
}
