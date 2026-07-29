// Package server is dorang's HTTP surface: the listener, the route table, the
// middleware chain, the extension-header contract, the error normalizer, and
// the generic passthrough engine of DESIGN §10.6.
//
// # What this package does not know
//
// It does not know how to speak a provider's wire format, how to route a model
// name to a deployment, how to hash a credential, or how to price a token. Every
// one of those is consumed through a narrow interface declared here —
// [Authenticator], [Dispatcher], [ModelLister], [Meter] — so that the HTTP
// surface can be built, tested and benchmarked on its own, and so that wiring is
// a decision made once in cmd/dorang rather than a compile-time dependency
// baked into every handler.
//
// # Hot path
//
//   - Configuration is an immutable snapshot behind an [sync/atomic.Pointer].
//     The request path never takes a lock to read it (DESIGN §15.2.1); the only
//     mutex in the [Server] guards [Server.Reload] against a concurrent reload,
//     and TestSnapshotReadTakesNoLock holds it while a request completes.
//   - Route lookup is an exact-map hit for every T0 path and a specificity-ordered
//     scan for the rest (COMPATIBILITY §7.5). Neither allocates.
//   - The per-request struct is pooled. Nothing on the auth-and-route path
//     allocates beyond it; TestNoAllocsRoutingAndAuth asserts zero.
//   - JSON that dorang authors is written by hand-rolled appenders: no
//     reflection, no regular expressions, no fmt (DESIGN §15.5), and the
//     serializer COMPATIBILITY §2.1a fixes as normative — compact separators,
//     raw UTF-8, HTML escaping off.
//   - Response bodies stream. The relay copies through a bounded pooled buffer
//     and never holds a whole body; TestStreamRelayDoesNotBuffer moves 16 MiB
//     through it and asserts total allocation stays under a megabyte.
//
// # Connection deadlines
//
// Three, and each bounds a different client. [Options.ReadHeaderTimeout] bounds
// the one that connects and says nothing — it never reaches a handler, and what
// it exhausts is the accept queue. [Options.ReadTimeout] bounds its sibling, the
// one that sends complete headers and then dribbles a body: that request has
// been admitted, holds an in-flight slot and a handler, and is the more
// expensive of the two. [Options.IdleTimeout] bounds a keep-alive connection
// between requests.
//
// There is deliberately no write deadline. A legitimate response streams for
// minutes, and net/http measures a WriteTimeout from the start of the request,
// so it would cut exactly the traffic this gateway exists to carry. A READ
// deadline does not have that problem: net/http clears it the moment the request
// body reaches EOF, and again on Hijack, so it bounds only the half of the
// exchange that finishes long before the answer begins.
//
// # Surface
//
// The eleven T0 paths of COMPATIBILITY §0 are registered by [New]. Everything
// else answers 501 with a machine-readable code, never a silent 404
// (DESIGN §0.2, COMPATIBILITY §9).
package server
