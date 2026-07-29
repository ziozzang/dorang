// Package admin implements dorang's administration API and the embedded
// operator UI.
//
// # Two surfaces, one authorization
//
// The API has two halves. The first is the shape-compatible surface of DESIGN
// §2.3 — /key/*, /user/*, /team/*, /model/*, /model_group/info, /budget/*,
// /spend/*, /global/spend/report, /{user,team,tag}/daily/activity and
// /health/history — whose paths and field names exist so that scripts and UIs
// written against the incumbent keep working. The second is a native /admin/*
// surface for the things dorang has and the incumbent does not: credential
// health, quota snapshots, capacity occupancy, catalog provenance, the pricing
// preview of §8.4 including the notional figure of §8.5, and configuration
// reload.
//
// Both are authorized the same way: the out-of-band master credential, or a key
// whose principal reports an administrative role. Every mutation writes an
// audit row carrying the actor, the action, and the object before and after, and
// every mutation that changes whether a key SERVES also announces it — see
// [Invalidator], which is the difference between a block taking effect in
// milliseconds and taking effect when a cache expires.
//
// # Four rules that shape the code
//
//  1. **Unimplemented answers 501 with a machine-readable code, never a silent
//     404** (§0.2). The mux's fallback is 501, not 404: a caller probing for a
//     surface dorang has not filled in yet must be able to tell "not built" from
//     "wrong URL", and the code field is what tells them.
//
//  2. **Every ledger query is bounded and paginated** (§9.3). A missing or
//     inverted range is refused with an explicit code, not silently widened to a
//     default; a range past the configured maximum is refused rather than
//     capped. Capping is worse than refusing, because the caller gets a partial
//     answer that looks complete.
//
//  3. **No secret ever leaves.** A newly generated key is returned exactly once,
//     in the response to the call that created it. Nothing else can return it:
//     [Key] has no field that holds the token, its digest, or its lookup, so
//     "key/info does not leak the secret" is a property of the type rather than
//     a property of the handler that a later edit could lose. §2.4 records that
//     a foreign system's display column stored the trailing characters of the
//     key; dorang stores a non-reversible label and this package never computes
//     anything else.
//
//  4. **The notional figure is reported as missing, never as zero** (§8.5).
//     Wherever cost appears, the notional figure appears beside it, with an
//     explicit availability flag. A subscription whose notional is silently
//     zero looks infinitely efficient, which is the most flattering possible
//     answer and the least likely to be questioned.
//
// # Dependencies
//
// Everything this package consumes is an interface declared in deps.go, in the
// same spirit as internal/server and internal/batch. None of them is satisfied
// by an internal package at compile time, and that is the point: the admin
// surface, the store, the pricing engine and the capacity broker change on
// different schedules, and a compile-time edge would mean the admin API cannot
// be tested until all of them exist. The concrete wiring happens once, in
// cmd/dorang.
//
// Optional dependencies are genuinely optional. A nil [Config.Capacity] does not
// crash the capacity endpoint; it makes that endpoint answer 501 with a code
// saying which dependency is absent, which is the honest answer and the one an
// operator can act on.
//
// [Config.Invalidator] is optional in a different sense, and it is worth naming
// because the difference is not visible in a status code: its absence does not
// refuse anything, it lowers a guarantee. The mutations still apply and the
// fleet still converges — on the credential cache TTL rather than within the
// bound of §11.2c. A deployment that leaves it nil should know that is what it
// has chosen.
package admin
