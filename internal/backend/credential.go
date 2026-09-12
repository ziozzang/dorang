package backend

import (
	"errors"
	"net/http"
)

// DefaultAnthropicVersion is the anthropic-version header sent upstream when a
// deployment does not carry one. The header is mandatory on that surface and a
// request without it is refused, so it has a default rather than being omitted.
const DefaultAnthropicVersion = "2023-06-01"

// Applier sets a credential's own headers on an outbound request.
//
// It is the narrow view of an OAuth credential this package needs, and
// *auth.OAuthCredential satisfies it as written (DESIGN §11.2b). Nothing here
// refreshes a token, reads a token store, or learns what a token is: an
// expiring credential renews itself on a background loop, off the request path,
// and this call reads whatever is current without blocking.
type Applier interface {
	Apply(http.Header) error
}

// Credentials resolves a credential id to what authenticates the request.
//
// It is the auth seam. This package never sees a configuration file, a key
// store or an exchange — only a secret it applies once and never records.
type Credentials interface {
	// Credential returns the static secret for id, or an Applier when the
	// credential authenticates by OAuth.
	//
	// Both empty is not an error: an unauthenticated deployment is ordinary
	// (a vLLM on a private network is the common case), and manufacturing a
	// 401 for one would refuse a request the upstream would have served.
	Credential(id string) (secret string, oauth Applier)
}

// errCredentialUnavailable is what an OAuth applier's failure becomes.
//
// The applier's own error is deliberately NOT wrapped. DESIGN §11.2b: that is
// provider code, its errors can carry token material, and "tokens are secrets"
// cannot bind a refresher this package did not write. The credential id is
// opaque and safe; the message it failed with is not, so it does not travel.
var errCredentialUnavailable = errors.New("backend: the credential is not usable")

// ApplyCredential puts a provider credential on an outbound request in the
// spelling that wire shape expects.
//
// The client's own credential never travels upstream — internal/auth strips
// every accepted header at the gate — so this is the only thing that
// authenticates dorang to a backend.
//
// [Backend.Do] calls it for the requests it makes. It is exported for the one
// outbound path that is not a [Backend.Do] — the §10.6 passthrough relay, which
// forwards a caller's own bytes to a provider and needs the credential spelled
// the same way, from the same table, by the same code.
func (p *Provider) ApplyCredential(secret string, oauth Applier, h http.Header) error {
	// Family headers that are not the credential itself go on regardless of how
	// the credential is spelled: anthropic-version is mandatory on that surface
	// whether the key is static or an OAuth token.
	return applyWith(p.ad, secret, oauth, h)
}

// applyWith is [Provider.ApplyCredential] for the adapter one exchange chose.
func applyWith(ad adapter, secret string, oauth Applier, h http.Header) error {
	ad.headers(h)

	if oauth != nil {
		if err := oauth.Apply(h); err != nil {
			return errCredentialUnavailable
		}
		return nil
	}
	if secret == "" {
		return nil
	}
	ad.credential(secret, h)
	return nil
}

// bearer is the credential spelling of every OpenAI-shaped family and of the
// two non-chat vendors.
func bearer(secret string, h http.Header) { h.Set("Authorization", "Bearer "+secret) }
