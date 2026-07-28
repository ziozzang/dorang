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
// # Surface
//
// The eleven T0 paths of COMPATIBILITY §0 are registered by [New]. Everything
// else answers 501 with a machine-readable code, never a silent 404
// (DESIGN §0.2, COMPATIBILITY §9).
package server
