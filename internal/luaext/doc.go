// Package luaext is dorang's extension surface: the hook points of DESIGN §11.5
// plus §10.5b's transform filter, each run under an instruction, memory and
// wall-clock ceiling, disabled by default, and unable to see a secret.
//
// # Three ways to write an extension, one contract
//
//  1. [Plugin] — a Lua file, named in the configuration. This is the mechanism
//     §11.5 asks for: "an operator drops in a file that registers handlers,
//     rather than the gateway shipping every policy anyone might want."
//  2. [Program] — a tiny total policy language (files `*.policy`) with no loops,
//     no calls and no way to build a string, so a program terminates by
//     construction. It covers the config-driven predicates — "deny this key on
//     that model" — at a fraction of a VM's cost, and it stays because for those
//     cases it is a better answer than Lua, not a substitute for it.
//  3. [Native] — a Go function registered by an embedder. Compiled-in code for
//     anything the other two cannot say.
//
// All three see the same secret-free views, run under the same watchdog and the
// same panic guard, and obey the same fail-open rule.
//
// # The VM, and the two ceilings that had to be built
//
// The interpreter is gopher-lua (github.com/yuin/gopher-lua), pure Go. §11.5
// makes that non-negotiable — "a cgo interpreter would break the static binary
// that makes the notebook tier's 'zero required dependencies' literal rather
// than aspirational" — and it costs about 1.8 MB of binary.
//
// gopher-lua gives one of the three ceilings and not the other two: a context
// check runs between VM instructions, so a wall clock can stop a hook, but there
// is no per-state instruction counter and no per-state allocation accounting.
// Both are built here, because a wall clock alone is not a sandbox — a hook
// allocating in a tight loop exhausts the process long before a 200 ms deadline
// fires, and the process holds provider credentials.
//
//   - **Instructions.** Source is parsed, the AST is rewritten to charge a
//     budget at every function body, every loop body and every backward `goto`,
//     and the rewritten AST is compiled. Those are the only three ways *Lua
//     source* can execute an unbounded number of instructions; everything else
//     is straight-line code whose length is fixed at load. See luagas.go.
//   - **Instructions, the other half.** Counting instructions bounds how many
//     builtins a plugin may call and says nothing about what one call does. A
//     builtin is host code the counter cannot see into, so any builtin whose
//     work is not bounded by its result is charged for that work before it runs:
//     the search family (find, match, gmatch, gsub), whose backtracking is
//     superlinear in a subject the *caller* supplies, and tonumber, which reads
//     every byte of its argument to return a number. See luapattern.go.
//   - **Memory.** Every allocation a plugin can cause is either O(1) per charge
//     — hence bounded by the instruction ceiling — or is charged explicitly
//     before it happens. String concatenation is rewritten into a charged host
//     call, and the library functions whose output can exceed their input
//     pre-flight their worst case. See luavm.go.
//
// # Residual exposure, stated rather than implied
//
//   - The memory budget is **cumulative, not live**. Bytes charged are never
//     refunded, because a host cannot see when Lua's collector frees a string.
//     Cumulative allocation bounds peak live memory from above, so the ceiling
//     holds; a long-running hook that allocates and releases repeatedly will hit
//     it earlier than its true footprint deserves.
//
//   - The ceilings do not apply to a [Native], which is ordinary Go code with
//     the whole process in reach. Only the wall clock and the panic guard do.
//
//   - A hook that ignores its context is **abandoned, not killed**. Go cannot
//     kill a goroutine. The request proceeds on time; the goroutine runs until it
//     notices the cancelled context or exhausts a ceiling, and repeated
//     abandonment trips the hook off entirely ([Engine.Tripped]).
//
//     What is bounded is *how many* — at most eight per hook, forty for an
//     engine, at any instant and for its whole life; past that, invocations are
//     refused before a goroutine exists to abandon ([Engine.Abandoned],
//     [ErrAbandonBacklog]). That bound is what makes the leak affordable: the
//     trip alone bounded abandonment over time and not at an instant, so a hook
//     stuck in one uninterruptible builtin could pin a core per request and keep
//     them all after being switched off. A Lua hook now also exits on its own,
//     because there is no builtin left that can outrun its ceilings; a [Native]
//     that blocks forever holds one of the eight forever.
//
//   - **String comparison and string-keyed indexing are charged O(1) and cost
//     O(len).** `a == b`, `a < b` and `t[k]` are VM operators rather than
//     builtins, so they are charged once by the enclosing block's tick while Go
//     compares or hashes every byte. Measured against 64 KiB operands and the 5 M
//     default budget: `a == b` 1.8 s, `t[k]` 1.9 s, `a < b` past a minute, where
//     the same budget of ordinary Lua is 110 ms. The fix is the one already used
//     for `..` — rewrite the expression into a charged host call — and it is not
//     done here because it puts a host call on every comparison and every dynamic
//     index in every plugin and has to carry __eq, __lt and __index with it. Only
//     a plugin handed a long string can reach it, which today means §10.5b's
//     document text and the request path.
//
//   - Plugin globals **persist across requests** inside one pooled VM instance,
//     exactly as in any long-running Lua program. Nothing can leave — there is
//     no I/O — but a plugin that stashes request text in a global has kept it.
//     Every capability the host lends is revoked when the invocation ends, which
//     is why the mask table is the host's and never Lua's.
//
// # The sandbox is a whitelist, and there is a test that proves it
//
// io, os, debug, coroutine and the package loader are never opened. Of what is
// opened, every name not on an allow-list is removed — including `_G`,
// `getfenv`, `setfenv`, `load`, `loadstring`, `dofile`, `require`, `pcall` and
// `xpcall`. The first three would hand out the globals table, where the charge
// functions live; the next four turn text into uninstrumented code; the last two
// would let a plugin catch its own ceiling, and a ceiling that can be caught is
// not a ceiling.
//
// TestGlobalSurfaceIsExactlyTheAllowlist walks everything reachable and fails on
// a single name that is not written down, so a gopher-lua upgrade cannot widen
// the sandbox quietly.
//
// # Disabled is free
//
// A nil *[Engine] is the disabled engine and every method on it is a constant.
// [New] returns (nil, nil) when nothing is configured, so a caller holds a typed
// nil and the hot path costs one nil check:
//
//	if eng.Enabled(luaext.HookRequest) {
//	    v := luaext.RequestView{...}
//	    if d := eng.OnRequest(ctx, &v); d.Denied { ... }
//	}
//
// TestDisabledEngineIsFree asserts zero allocations for that branch, and
// TestLuaHookOffCostsNothing asserts the same for an engine that is on but has
// nothing registered at the hook being asked about.
//
// # Fail-open, except a deny — and except a mask
//
// §11.5's asymmetry is the whole safety argument and it is implemented
// literally: a hook that exceeds a ceiling, panics, or is cancelled is skipped
// and warned, and the request proceeds as if the hook were not configured. The
// one thing honoured is an explicit deny from a hook that *completed*.
//
// [Engine.FilterRequest] inverts it, and §10.5b says why: "a filter that cannot
// enrich a request should be skipped, but a filter that was supposed to remove
// an identity number and did not must stop the request." A filter plugin
// declares which kind it is; [FailClosed] is the default.
//
// # Secrets
//
// The views are flat structs of identifiers and numbers. No field on any of them
// can hold a credential, a bearer token, a request or response body, or a header
// — so a hook cannot read a secret and, having no I/O, could not send one
// anywhere if it had. TestNoSecretIsReachableFromAnyView asserts the structural
// half by walking the view types, TestHookCannotSeeCredential asserts the
// behavioural half with a real credential in flight, and
// TestNoSecretIsReachableFromLua does it again from inside the VM with a plugin
// that goes looking.
//
// [Doc] is the one exception to "no bodies", and it is the point of §10.5b: a
// filter sees the request's text because rewriting it is what a filter is for.
// It sees text only — never a header, never a credential, never the mask table.
package luaext
