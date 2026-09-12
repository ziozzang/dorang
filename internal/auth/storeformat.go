package auth

import (
	"fmt"
	"sort"
	"strings"
)

// A vendor CLI's token store is a file dorang did not design and does not own.
//
// The point of naming the layouts here is that an operator who is already
// signed in to a vendor's CLI does not sign in again: a credential points at the
// store that CLI already keeps, dorang reads the token out of it, and — when a
// refresh endpoint is configured — writes the successor back in the same shape.
// The alternative is a second authorization flow per account, interactive, on a
// server.
//
// What is NOT here, deliberately: the initial authorization. Acquiring a first
// refresh token is an interactive, browser-bound flow (PKCE or a device code)
// with a redirect dorang has nowhere to host. dorang refreshes credentials; it
// does not mint them. A store with no refresh token in it is reported as such
// (see [ErrNoRefreshToken]) rather than half-served.

// StoreFormat names a token-store layout.
type StoreFormat string

const (
	// FormatGeneric is the flat, standard-named store: access_token,
	// refresh_token, expires_at, account_id. It is the default, and it is what a
	// store dorang creates itself looks like.
	FormatGeneric StoreFormat = "generic"

	// FormatCodex is the OpenAI Codex CLI's own auth store.
	//
	//	{"auth_mode": "...", "tokens": {"id_token": "...", "access_token": "...",
	//	 "refresh_token": "...", "account_id": "..."}, "last_refresh": "...",
	//	 "OPENAI_API_KEY": null}
	//
	// It carries NO expiry field. The access token is a JWT, so the expiry is
	// read from its `exp` claim — without that this credential could only ever
	// be renewed by the 401 fallback, which is the mechanism §11.2b explicitly
	// says is not the mechanism.
	FormatCodex StoreFormat = "codex"

	// FormatClaude is the Claude Code CLI's credentials file, which nests
	// everything under one key and spells its fields in camel case with the
	// expiry in unix milliseconds.
	FormatClaude StoreFormat = "claude"

	// FormatGemini is the Gemini CLI's OAuth credentials file: a Google token
	// response written to disk as-is, so it is flat and standard except that the
	// expiry is `expiry_date` in unix milliseconds.
	FormatGemini StoreFormat = "gemini"
)

// storeFormats is the table. It is a table rather than a switch so that
// [StoreFormats] can list what this build understands, which is what a
// configuration error has to be able to say.
var storeFormats = map[StoreFormat]TokenFields{
	FormatGeneric: {},
	// A service-account key file: no access token to read, a non-secret key
	// id standing in as the "refresh token" so the credential knows it can
	// mint, the client email as the account. See [FormatGCPServiceAccount].
	FormatGCPServiceAccount: {
		RefreshToken: "private_key_id",
		AccountID:    "client_email",
	},
	FormatCodex: {
		AccessToken:  "tokens.access_token",  // pragma: allowlist secret — a field name
		RefreshToken: "tokens.refresh_token", // pragma: allowlist secret — a field name
		AccountID:    "tokens.account_id",
		// The same field, read a second way: there is nowhere else an expiry
		// could come from in this store.
		ExpiresAt:         "tokens.access_token",
		ExpiresAtEncoding: ExpiryJWTClaim,
	},
	FormatClaude: {
		AccessToken:  "claudeAiOauth.accessToken",  // pragma: allowlist secret — a field name
		RefreshToken: "claudeAiOauth.refreshToken", // pragma: allowlist secret — a field name
		ExpiresAt:    "claudeAiOauth.expiresAt",
		// This store carries no account identifier. The default name is left in
		// place and simply does not resolve, which is a credential with no
		// account header rather than an error.
	},
	FormatGemini: {
		AccessToken:  "access_token",
		RefreshToken: "refresh_token",
		ExpiresAt:    "expiry_date",
	},
}

// StoreFormats lists the layouts this build understands, sorted.
func StoreFormats() []string {
	out := make([]string, 0, len(storeFormats))
	for f := range storeFormats {
		out = append(out, string(f))
	}
	sort.Strings(out)
	return out
}

// ParseStoreFormat decodes a configured oauth.format value.
func ParseStoreFormat(s string) (StoreFormat, error) {
	f := StoreFormat(strings.TrimSpace(strings.ToLower(s)))
	if f == "" {
		return FormatGeneric, nil
	}
	if _, ok := storeFormats[f]; !ok {
		return "", fmt.Errorf("auth: unknown oauth token store format %q (want one of %s)",
			s, strings.Join(StoreFormats(), ", "))
	}
	return f, nil
}

// Fields returns the layout's field names, with defaults filled in.
//
// An override supplied by the operator wins over the preset, because a vendor
// that renames a key in a point release must not require a new dorang build.
func (f StoreFormat) Fields(override TokenFields) TokenFields {
	base, ok := storeFormats[f]
	if !ok {
		base = TokenFields{}
	}
	if override.AccessToken != "" {
		base.AccessToken = override.AccessToken // pragma: allowlist secret — a field name
	}
	if override.RefreshToken != "" {
		base.RefreshToken = override.RefreshToken // pragma: allowlist secret — a field name
	}
	if override.AccountID != "" {
		base.AccountID = override.AccountID
	}
	if override.ExpiresAt != "" {
		base.ExpiresAt = override.ExpiresAt
		// An operator who names the expiry field is naming a field that holds a
		// timestamp; the JWT reading belongs to the preset's own field and does
		// not follow a rename onto someone else's.
		base.ExpiresAtEncoding = override.ExpiresAtEncoding
	}
	return base.withDefaults()
}
