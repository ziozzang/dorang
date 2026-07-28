// Package luaext is dorang's extension-hook surface: the four hook points of
// DESIGN §11.5 — on_request, on_route, on_response, on_email — each run under
// an instruction, memory and wall-clock ceiling, disabled by default, and
// unable to see a secret.
//
// # There is no Lua interpreter here, and that is a decision
//
// §11.5 names Lua. This package does not embed a Lua VM. The reasoning, so a
// reader can disagree with it on the merits rather than guess:
//
//   - A Lua VM is a required module dependency, and dorang's dependency budget
//     is deliberate (§0.2). Every existing dependency is either the storage
//     driver or the YAML parser, and the SQLite driver was chosen pure-Go for
//     the same reason. "The gateway needs a scripting language to boot" is a
//     larger claim than the feature earns.
//   - The ceilings §11.5 promises are the hard part, and a general-purpose Lua
//     VM makes two of the three harder, not easier. gopher-lua — the only
//     serious pure-Go option — offers cooperative context cancellation but no
//     per-state instruction counter and no per-state memory accounting, so
//     "instruction and memory ceilings" would have to be re-implemented on top
//     of it or quietly withdrawn.
//   - What the hooks are actually for is small. on_request is a policy
//     predicate ("deny this key on that model"). on_route is the same predicate
//     with the chosen deployment in view. on_response is an annotation.
//     on_email is a filter. Not one of them needs closures, coroutines, string
//     building, or a standard library.
//
// So this package ships the hook contract with two implementations behind it:
//
//  1. A tiny, total policy language (see [Program], files `*.policy`) that
//     covers the config-driven cases. It has no loops, no recursion, no
//     function calls, no I/O, and no way to build a string, so a program
//     terminates by construction and cannot allocate without bound. The
//     ceilings are still enforced — as defence in depth, not as the only thing
//     standing between an extension and the gateway.
//  2. [Native] hooks: Go functions registered by an embedder. This is the
//     documented escape hatch for anything the policy language cannot say, and
//     it inherits the same wall-clock watchdog, the same panic containment, the
//     same fail-open rule and the same secret-free views.
//
// # What is not delivered
//
//   - Lua. There is no interpreter, and a `.lua` file under extensions.lua.dir
//     is a **load error** ([ErrLuaSource]), never a file that is silently
//     ignored. The configuration surface does not accept Lua that never runs.
//   - Re-routing from on_route. The hook sees the chosen deployment and may
//     refuse it; it cannot ask for a different one. See [RouteDecision] for the
//     reason, which is a property of internal/router's fail-back machinery
//     rather than of this package.
//   - A memory ceiling over a [Native] hook. A Native is compiled-in Go code;
//     only wall-clock and panic containment apply to it. The memory ceiling is
//     enforced against policy programs, where it means peak live operand and
//     tag bytes.
//
// # Disabled is free
//
// A nil *[Engine] is the disabled engine and every method on it is a constant.
// [New] returns (nil, nil) when extensions.lua.enabled is false, so a caller
// holds a typed nil and the hot path costs one nil check:
//
//	if eng.Enabled(luaext.HookRequest) {
//	    v := luaext.RequestView{...}
//	    if d := eng.OnRequest(ctx, &v); d.Denied { ... }
//	}
//
// TestDisabledEngineIsFree asserts zero allocations for that branch, and
// TestEnabledEngineDoesNotAllocateWhenHookAbsent asserts the same for an engine
// that is on but has nothing registered for the hook being asked about.
//
// # Fail-open, except a deny
//
// §11.5's asymmetry is the whole safety argument and it is implemented
// literally: a hook that exceeds a ceiling, panics, or is cancelled is skipped
// and warned, and the request proceeds as if the hook were not configured. The
// one thing that is honoured is an explicit deny from a hook that *completed*.
// A broken extension cannot take the gateway down; a policy that says no is
// obeyed.
//
// The wall-clock ceiling is enforced by the caller, not by the hook: every
// invocation runs on its own goroutine and the caller stops waiting at the
// deadline whether or not the hook noticed. A hook that ignores its context
// therefore delays nothing. Repeatedly abandoning a hook trips it off entirely
// (see [Engine.Tripped]), because a wedged extension that leaks one goroutine
// per request is an outage with a delay fuse.
//
// # Secrets
//
// The views are flat structs of identifiers and numbers. There is no field on
// any of them that can hold a credential, a bearer token, a request or response
// body, or a header — so a hook cannot read a secret and, having no I/O, could
// not send one anywhere if it had. TestNoSecretIsReachableFromAnyView asserts
// the structural half by walking the view types, and TestHookCannotSeeCredential
// asserts the behavioural half with a real credential in flight.
package luaext
