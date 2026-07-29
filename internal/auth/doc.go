// Package auth authenticates a caller's credential and enforces the
// authorization fields that travel with it.
//
// # Accepting a credential
//
// Six header names are accepted and any one of them authenticates
// (COMPATIBILITY §7.3): Authorization: Bearer, API-Key, x-api-key,
// x-goog-api-key, Ocp-Apim-Subscription-Key and x-dorang-api-key. All six are
// stripped before a request is forwarded upstream — [Strip] is that function,
// and it is tested. A client credential must never reach a provider.
//
// # Two hash schemes, one lookup
//
// Keys dorang issues use dorang_v1: HMAC-SHA256(pepper, token) (DESIGN §2.4).
// legacy_sha256 — a plain, unsalted sha256 of the token — exists only as an
// import-time compatibility mode, is off by default, and requires a mandatory
// expiry date; after that date legacy verification is refused. R1-A found the
// migration argument for adopting the unsalted digest permanently did not
// survive contact with the data, so it is a migration window and nothing more.
//
// The index key is scheme-independent: lookup = hex(sha256(token))[:32]. One
// store lookup selects the row and only verification branches on the scheme,
// branchlessly, through crypto/subtle. With Config.RehashOnUse a successful
// legacy verification queues an asynchronous upgrade to dorang_v1 on a
// non-blocking channel, so the migration completes without a flag day and
// without adding latency to the request that triggered it.
//
// # The sk- gate
//
// A credential must start with "sk-" before any lookup happens. That gate is
// not cosmetic: stored rows are hex digests, and without the prefix check a
// leaked digest could be replayed as the credential it stands for.
//
// # The master credential is out-of-band
//
// The administrative credential is compared in constant time against a
// configured value and is never a stored row (R1-A). A gateway that reads only
// an imported database would otherwise lose admin authentication entirely, so
// [New] refuses to build an Authenticator with no master key unless the caller
// sets Config.NoMasterKey to say it meant it. The comparison happens before the
// sk- gate and before any store access, so no store state can withdraw it.
//
// # Authorization fields are enforced
//
// R1-A recorded that these fields must be carried or the gateway fails open:
// expiry, blocked flag, model allow-list, route allow-list, budget and spend,
// rate limits, owning team and user. [Principal.Authorize] enforces every one
// of them across the key, its user and its team, and the most restrictive
// wins (DESIGN §11.2). An expired key is refused, never resurrected.
//
// # The hot path
//
// Lookup is O(1) against a lock-free immutable snapshot held in an
// atomic.Pointer, keyed by the raw 16-byte index key so the hot path never
// allocates a hex string. A miss consults the store once — concurrent misses
// for the same key are coalesced into a single call — and lands in a small
// overlay that is merged into the snapshot in batches, so a flood of unknown
// keys cannot make each miss cost a full snapshot copy. Entries learned at
// runtime expire by TTL; the snapshot itself is replaced by [Authenticator.Load]
// or dropped by [Authenticator.Invalidate].
//
// The store lookup a miss performs is BUDGETED, because a cache alone cannot
// bound it: a negative entry is keyed by the index key, so a caller presenting
// distinct invented credentials never hits one and every request was a database
// round trip bought for free. [Config.MissRate] bounds the lookups that find
// nothing; a lookup that returns a row gives its token straight back, so first
// use of a real credential — a cold node's entire traffic — is never throttled.
// Past the budget the answer is [ReasonUnavailable], not [ReasonUnknownKey]: the
// store was not asked, so "no such key" is not something the gateway knows. See
// missbudget.go.
//
// Verification recomputes the digest on every request from a stack buffer:
// one sha256 of the token yields both the index key and the legacy digest,
// and two more compressions yield the HMAC. The uncontended path allocates
// nothing.
//
// # Provider OAuth credentials refresh themselves
//
// The other direction: where dorang authenticates to a provider by OAuth rather
// than with a static key (DESIGN §11.2b). [OAuthCredential] renews its token
// from a background loop, and the mechanics carry the weight:
//
//   - Renewal happens a refresh_margin ahead of expiry, so it never sits on a
//     request's critical path. A 401 is a second signal — [OAuthCredential.RefreshOn401]
//     is refresh-once-and-retry-once — because clock skew and server-side
//     revocation exist, but it is the fallback, not the mechanism.
//   - Refresh is single-flight per credential, through the same [flight] the
//     credential lookup uses. Some providers invalidate the previous refresh
//     token when it is used, so a stampede does not merely waste calls, it can
//     lock the account out. For the same reason the shared store is re-read
//     before an exchange: single-flight covers this process, and the vendor's
//     own CLI is another one.
//   - The store is written atomically — temporary file, fsync, rename, original
//     mode preserved — and every key dorang does not own survives the write. A
//     half-written credential file breaks the CLI too, and nobody would suspect
//     the gateway.
//   - A failed refresh marks the credential unhealthy and backs off
//     exponentially to a ceiling. It does not fail the fleet: the credential
//     steps aside as an exhausted quota does, and the token it failed to
//     replace keeps serving until it actually expires. Retrying in a tight loop
//     is how a recoverable expiry becomes a rate-limit ban.
//   - The credential id never changes, so an affinity pin survives a refresh:
//     the account is the same account and the token is an implementation detail
//     of talking to it (DESIGN §7.4a2).
//
// Tokens come from a file (the vendor CLI's own JSON store), an exec command,
// or the environment. [Refresher] is the provider-side interface; this package
// ships only [FakeRefresher], exactly as internal/quota ships only a static
// prober.
//
// # Secrets
//
// No token, pepper or master key is ever logged, formatted or returned. The
// types that hold key material — [Hasher], [Config], [Authenticator], [Token],
// [OAuthCredential] — redact themselves under every fmt verb, and no error
// message carries a credential. What leaves the OAuth subsystem is the
// credential's opaque id and its [CredentialHealth], a type with no field that
// can hold a token; anything recorded from a provider's own error has the
// credential's tokens removed from it first.
package auth
