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
// Verification recomputes the digest on every request from a stack buffer:
// one sha256 of the token yields both the index key and the legacy digest,
// and two more compressions yield the HMAC. The uncontended path allocates
// nothing.
//
// # Secrets
//
// No token, pepper or master key is ever logged, formatted or returned. The
// types that hold key material — [Hasher], [Config], [Authenticator] — redact
// themselves under every fmt verb, and no error message carries a credential.
package auth
