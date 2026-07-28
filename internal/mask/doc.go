// Package mask implements DESIGN §10.5b's reversible transform: the mechanism
// that lets an operator replace a national identity number with a placeholder
// before the text leaves their control and restore it in the answer.
//
// # The table is the dangerous object, so there isn't one that outlives a request
//
// A mask is not a redaction. It has to come back, which means holding a
// placeholder → original table, and §10.5b's first rule is that the table lives
// for one request and is never persisted — not in the ledger, the trace excerpt,
// a log line, an audit row, or a metric label. "A redaction whose key material is
// written next to the redacted text has redacted nothing."
//
// The table is not stored because it does not have to be. A placeholder is
// *derived*, not allocated:
//
//	placeholder = "[PII:" ‖ base16a(HMAC-SHA256(secret, scope ‖ salt ‖ 0x00 ‖ value)[:8]) ‖ "]"
//
// so the same text under the same scope always produces the same placeholder,
// on every node, after every restart, for ever. The reverse table is rebuilt
// from scratch on each request out of the request's own text, which works
// because the chat and messages protocols resend the whole conversation: every
// plaintext that can appear in the answer was present in the question. Nothing
// is retained, and the mapping still survives an hour — or a year.
//
// [Vault] is the exception, and it exists for one narrow case; see below.
//
// # Determinism is not a nicety — it is what keeps prefix caching alive
//
// This is the part §10.5b as first written got wrong. A per-request random
// placeholder means the same conversation masks differently on turn N and turn
// N+1, so the bytes the backend receives differ, so the KV cache never hits.
// On a self-hosted backend that is the difference between reusing a 40k-token
// prefix and recomputing it every turn — §7.4b's whole point, silently deleted
// by a feature two sections away.
//
// So the placeholder is a stable pseudonym, and the cost has to be stated
// plainly rather than implied away: **a provider can link "the same person
// appears in these requests"**. That is not a bug that could be fixed with more
// care. It is the same property as the cache hit — a backend that cannot
// recognise the repeated bytes cannot reuse the cache — and any design that
// claims both is wrong about one of them.
//
// What can be bounded is *how far* the pseudonym reaches, which is what [Scope]
// is for. The default is one conversation, where the provider already sees every
// turn together and a stable placeholder tells it nothing it did not have.
// [ScopeRequest] restores maximum unlinkability and is honest about the price:
// the caller must disable prefix affinity for that route, because a scope that
// quietly defeats another subsystem is worse than one that says so.
//
// # Placeholders
//
// A placeholder is fixed-width ASCII:
//
//	[PII:kfmadpblnbeghcoi]
//	 └┬─┘└──────┬───────┘
//	  │         └── 16 characters, 64 bits of HMAC over the value
//	  └──────────── the sentinel
//
// Three properties, each load-bearing:
//
//   - **Fixed width** ([PlaceholderLen] = 22) is what makes the streaming bound
//     provable (§10.5b rule 4, and see [Unmasker]) and what stops a length change
//     from cascading through a body.
//   - **Letters only** — the alphabet is a…p, four bits a character. A
//     placeholder therefore contains no digit and no `@`, so it cannot itself
//     match a digit-shaped or address-shaped pattern on a later pass. Masking is
//     not idempotent by accident; it is idempotent by alphabet.
//   - **Derived, not counted.** There is no sequence number, because a sequence
//     depends on what else was in the request and would make the same value mask
//     differently in a longer conversation.
//
// # Forgery, and why escaping is masking
//
// §10.5b rule 2 requires that placeholder-shaped text in the *input* be escaped
// before masking runs, so a caller cannot make the unmasker substitute for a
// string they chose. This package escapes by masking: every occurrence of the
// sentinel in the input is itself replaced with a derived placeholder that maps
// back to the literal sentinel. That is strictly better than mangling it — the
// round trip is exact, the caller gets their own text back byte for byte, and
// there is no escape syntax for an attacker to study. After the escape pass, the
// only sentinel left in the upstream text is one this filter issued.
//
// # Unmasking is not a blind replace
//
// §10.5b rule 3. Only an exact, whole, well-formed placeholder that resolves in
// *this* request is substituted. A placeholder echoed inside a longer opaque run,
// one with a character added, a truncated one, a translated one, one replayed
// from a different scope, or one invented by the model is left exactly as it is
// and counted as unresolved — an invented placeholder is a signal worth having,
// because it means the model is emitting text that looks like gateway machinery.
//
// # Failure is closed
//
// Everything else in the extension surface fails open (DESIGN §11.5): a hook that
// breaks is skipped. Masking inverts that. [Session.Mask] returns an error when
// it cannot honour the contract, and the caller must refuse the request. A filter
// that was supposed to remove an identity number and did not must not proceed,
// which is the one place in dorang where a broken extension stops traffic on
// purpose.
//
// The inversion covers a filter that never *ran* as well as one that ran and
// returned an error, and the difference is not academic: a masking pass that did
// not happen leaves the text exactly as the caller sent it, which is the outcome
// this package exists to prevent and is indistinguishable — from here — from a
// clean pass over text that contained nothing. Only the caller can tell the two
// apart, so internal/luaext refuses on behalf of a filter that was configured
// and did not run at all (a hook switched off, a plugin that registered no
// handler), rather than returning the decision a clean pass returns.
//
// # What is never printed
//
// [Session] and [Vault] have no exported fields and implement [fmt.Stringer],
// [fmt.GoStringer] and [json.Marshaler], all three redacted, because the
// realistic way key material reaches a log is `log.Printf("%v", state)` on a
// struct that happens to contain one. [Session.Stats] returns counts, and counts
// are the only thing a metric, a log line or an event may carry.
package mask
