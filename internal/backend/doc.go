// Package backend is L5 of DESIGN §1: the provider adapters.
//
// Everything between "the router chose a deployment" and "the client's protocol
// has an answer" lives here — endpoint derivation, credential application, the
// canonical round trip through internal/wire, error normalization to
// COMPATIBILITY §11's taxonomy, the streaming relay, and the per-provider
// timeout and retry policy.
//
// The division of labour with internal/app is that app assembles and this
// package calls: app turns a configuration file into [Provider] values and a
// routing decision into a [Target], and never builds an HTTP request, spells a
// credential, or parses a provider's error envelope itself.
//
// # What an adapter is
//
// One adapter per `api` value of DESIGN §4.3 — the wire shape, not the vendor.
// Ten kinds share the openai-chat adapter; one kind (google) has its own,
// because contents[]/systemInstruction/generationConfig is a different protocol
// wearing the same words. An adapter answers four questions and nothing else:
// where does this operation go, how is the credential spelled, what bytes does
// this canonical request become, and what does the answer mean.
//
// Conversion stays N+M (§10.1): an adapter decodes an upstream answer to
// [canonical.Response] and never to the caller's protocol. The caller's
// protocol is chosen once, by [Backend.Do], from the two families a frontend
// can speak.
//
// # What this package deliberately does not take
//
// The rule is that L5 owns the call and nobody else's verdict. Concretely:
//
//   - Credentials arrive through [Credentials], a two-method interface that
//     internal/auth satisfies. This package never sees a configuration file, a
//     key store, or a token exchange.
//   - Priority arrives already direction-normalized on [Target]. This package
//     does not know dorang's priority classes and must not: the canonical value
//     and the per-engine direction are internal/router's (§7.5), and a second
//     implementation of the negation is how the two engines end up agreeing
//     when they must not.
//   - Health is NOT an input. internal/router.Report owns the health verdict
//     and already converts a 429's Retry-After into a cooldown; a backend that
//     also marked deployments unavailable would count one failure twice and
//     stand a deployment down for double the interval. The Retry-After and the
//     status come back on [Result] instead, which is the same information
//     without the second opinion.
//   - Metering arrives through [Observer], and it is called on FAILED attempts
//     only. Boxing a struct into an interface allocates, and §15.5 prohibits
//     that on the hot path; a failed request is not the hot path.
//
// # Invariants
//
//  1. Redirects are never followed (§10.6). A 30x from a provider re-issues the
//     request — with the provider credential — to a host the *upstream* chose,
//     which turns a compromised backend into a credential-exfiltration primitive
//     needing no access to dorang at all. [NewClient] returns the last response
//     instead, and [Backend.Do] turns it into a named error.
//  2. A credential never reaches an error, a log, or a header this package did
//     not put it on. Every error path here is built from a constant message plus
//     non-secret identifiers; upstream bodies are normalized by
//     [server.Normalize], which bounds the excerpt it quotes.
//  3. A stream is never buffered. The relay writes and flushes per frame; the
//     only case that decodes at all is a crossing between protocol families,
//     which is bounded by one frame.
//  4. The hot path allocates only what the wire encoders allocate. There is no
//     per-request map, no formatted string, and no interface boxing on the
//     success path.
package backend
