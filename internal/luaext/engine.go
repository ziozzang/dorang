package luaext

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// Native is a Go-implemented hook.
//
// It is the documented extension point for anything the policy language cannot
// say. A Native is ordinary Go code compiled into the binary, so it is trusted
// in the way a policy program is not: the memory ceiling does not apply to it,
// and it can reach anything the process can reach. What it does *not* get is a
// richer view — it sees exactly the same secret-free structs — and it is still
// wrapped in the wall-clock watchdog and the panic guard, so a Native that
// wedges or crashes costs a skipped hook rather than a request.
//
// Only the fields for the hooks this extension implements need to be set.
type Native struct {
	// Name identifies the extension in warnings and statistics.
	Name string

	Request  func(context.Context, *RequestView, *RequestDecision)
	Route    func(context.Context, *RouteView, *RouteDecision)
	Response func(context.Context, *ResponseView, *ResponseDecision)
	// Email both filters and, for the `lua` notification driver, delivers.
	// A non-nil error is a delivery failure and is retried by internal/notify
	// under its own bounded backoff.
	Email func(context.Context, *EmailView, *EmailDecision) error
}

// Options configures [New]. It mirrors config.LuaExtension plus the natives an
// embedder registers.
type Options struct {
	// Enabled is extensions.lua.enabled. False returns a nil engine.
	Enabled bool
	// Dir holds the policy files. Empty skips file loading, which is only
	// useful alongside Native hooks.
	Dir string
	// Hooks restricts which hook points are active. Empty means all four. A
	// program for a hook that is not listed is a load error, not a silent skip.
	//
	// It does not gate [HookFilterRequest]: a filter is attached to a model, so
	// the operator has already said where it runs.
	Hooks []string
	// Limits are the three ceilings. Zero fields take package defaults.
	Limits Limits
	// Native are Go-implemented hooks.
	Native []Native
	// Plugins are the Lua plugins, named one by one. There is no directory
	// scan; see [Plugin].
	Plugins []Plugin
	// Logf receives the skip-and-warn diagnostics, rate-limited per hook.
	Logf func(string, ...any)
	// Now overrides the clock for the warning throttle.
	Now func() time.Time
}

// maxConsecutiveTimeouts is how many invocations of one hook may be abandoned
// back to back before the hook is switched off for the life of the engine.
//
// An abandoned invocation leaks the goroutine it was running on — that is the
// price of a real timeout rather than a cooperative one. Leaking a bounded
// number of them to keep serving traffic is the trade DESIGN §11.5 asks for;
// leaking one per request until the process dies is not, and would convert a
// broken extension into exactly the outage fail-open exists to prevent.
const maxConsecutiveTimeouts = 32

// maxAbandoned is how many invocations of one hook may be *outstanding* — timed
// out, no longer waited for, still running — at the same time.
//
// The trip above bounds abandonment over time and does not bound it at an
// instant, and the difference is the whole problem. An abandoned goroutine that
// is blocked costs a little memory; an abandoned goroutine that is *spinning*
// costs a core, and thirty-two of them cost thirty-two. Worse, they outlive the
// trip: the operator sees the hook switched off and the machine still pinned,
// with nothing connecting the two.
//
// So abandonment is treated as a resource with a fixed supply. Once a hook has
// this many invocations outstanding it stops being run at all — the invocation
// is refused before the goroutine is created, which is cheaper than running it
// and fails open exactly as a timeout does. A refusal counts toward the trip
// like the abandonment it stands in for, so a hook that stays wedged is switched
// off rather than refusing forever in silence.
//
// The bound this states: at most maxAbandoned goroutines per hook, so at most
// numHooks × maxAbandoned for an engine, at any instant and for its whole life.
// Nothing a plugin or a request rate can do raises it.
const maxAbandoned = 8

// Engine holds the compiled programs and registered natives for all four hooks.
//
// A nil *Engine is the disabled engine: every method is a constant and costs
// one nil check. That is how "disabled by default on the hot path" is
// implemented — not as a flag inside a live object, but as the absence of one.
type Engine struct {
	// mask is immutable after New: bit h is set when hook h has at least one
	// unit. Configuration reloads build a new Engine and swap the pointer, in
	// the same style as the routing snapshot (§15.2.1), so the read path takes
	// no lock.
	mask   uint8
	limits Limits

	progs   [numHooks][]*Program
	natives [numHooks][]Native
	lua     *luaRuntime

	logf func(string, ...any)
	now  func() time.Time

	tripped   atomic.Uint32
	consecTMO [numHooks]atomic.Int64
	lastWarn  [numHooks]atomic.Int64
	// abandoned counts invocations of each hook that timed out and are still
	// running. See [maxAbandoned].
	abandoned [numHooks]atomic.Int64

	st stats
}

type stats struct {
	invocations atomic.Uint64
	denies      atomic.Uint64
	skipped     atomic.Uint64
	panics      atomic.Uint64
	timeouts    atomic.Uint64
	limitHits   atomic.Uint64
	tripped     atomic.Uint64
	refused     atomic.Uint64
}

// Stats is a snapshot of what the hooks have done.
type Stats struct {
	Invocations uint64
	Denies      uint64
	Skipped     uint64
	Panics      uint64
	Timeouts    uint64
	LimitHits   uint64
	Tripped     uint64
	// Refused counts invocations that were not run because the hook already
	// had [maxAbandoned] outstanding. It is distinct from Timeouts because it
	// is the *cheap* failure — nothing was started — and an operator watching
	// it climb is watching a hook that is wedged rather than merely slow.
	Refused uint64
}

// Stats returns a snapshot. It is safe on a nil engine.
func (e *Engine) Stats() Stats {
	if e == nil {
		return Stats{}
	}
	return Stats{
		Invocations: e.st.invocations.Load(),
		Denies:      e.st.denies.Load(),
		Skipped:     e.st.skipped.Load(),
		Panics:      e.st.panics.Load(),
		Timeouts:    e.st.timeouts.Load(),
		LimitHits:   e.st.limitHits.Load(),
		Tripped:     e.st.tripped.Load(),
		Refused:     e.st.refused.Load(),
	}
}

// Abandoned reports how many invocations of h timed out and are still running.
// It is bounded by [maxAbandoned]; an operator seeing it pinned there is seeing
// a hook that never returns.
func (e *Engine) Abandoned(h Hook) int {
	if e == nil || int(h) >= numHooks {
		return 0
	}
	return int(e.abandoned[h].Load())
}

// New builds an engine.
//
// It returns (nil, nil) when Enabled is false. That is deliberate: the caller
// stores a typed nil and the hot path costs a nil check, rather than a live
// object whose methods each test a flag.
func New(opts Options) (*Engine, error) {
	if !opts.Enabled {
		return nil, nil
	}
	opts.Limits.setDefaults()

	e := &Engine{
		limits: opts.Limits,
		logf:   opts.Logf,
		now:    opts.Now,
	}
	if e.now == nil {
		e.now = time.Now
	}

	allowed, err := hookSet(opts.Hooks)
	if err != nil {
		return nil, err
	}

	if opts.Dir != "" {
		progs, err := LoadDir(opts.Dir, allowed)
		if err != nil {
			return nil, err
		}
		for _, p := range progs {
			e.progs[p.Hook] = append(e.progs[p.Hook], p)
		}
	}

	for _, n := range opts.Native {
		if n.Name == "" {
			return nil, errors.New("luaext: a Native hook needs a Name")
		}
		for h, fn := range map[Hook]bool{
			HookRequest:  n.Request != nil,
			HookRoute:    n.Route != nil,
			HookResponse: n.Response != nil,
			HookEmail:    n.Email != nil,
		} {
			if !fn {
				continue
			}
			if allowed&(1<<uint(h)) == 0 {
				return nil, fmt.Errorf("luaext: native %q implements %s, which extensions.lua.hooks does not list",
					n.Name, h)
			}
			e.natives[h] = append(e.natives[h], n)
		}
	}

	if len(opts.Plugins) > 0 {
		rt, err := newLuaRuntime(opts.Plugins, e.limits, e.logf)
		if err != nil {
			return nil, err
		}
		for h := 0; h < numHooks; h++ {
			if rt.hookMask&(1<<uint(h)) == 0 {
				continue
			}
			if allowed&(1<<uint(h)) == 0 {
				return nil, fmt.Errorf("luaext: a plugin registers at %s, which extensions.lua.hooks does not list",
					Hook(h))
			}
		}
		e.lua = rt
	}

	for h := 0; h < numHooks; h++ {
		if len(e.progs[h]) > 0 || len(e.natives[h]) > 0 {
			e.mask |= 1 << uint(h)
		}
	}
	if e.lua != nil {
		e.mask |= e.lua.hookMask
	}
	return e, nil
}

// hookSet turns the configured hook names into a bitmask. An empty list means
// all of them, which matches internal/config's default.
//
// HookFilterRequest is always set: extensions.lua.hooks lists the notification
// and policy hooks, and a filter is declared where it is used, on the model.
func hookSet(names []string) (uint8, error) {
	if len(names) == 0 {
		return (1 << numHooks) - 1, nil
	}
	m := uint8(1) << uint(HookFilterRequest)
	for _, n := range names {
		h, ok := ParseHook(n)
		if !ok {
			return 0, fmt.Errorf("luaext: %q is not a hook point; want one of %v", n, hookNames)
		}
		m |= 1 << uint(h)
	}
	return m, nil
}

// Enabled reports whether hook h will run. It is the branch the hot path takes
// when hooks are off, and it allocates nothing.
func (e *Engine) Enabled(h Hook) bool {
	if e == nil {
		return false
	}
	if e.mask&(1<<uint(h)) == 0 {
		return false
	}
	return e.tripped.Load()&(1<<uint(h)) == 0
}

// Tripped reports whether a hook was switched off after repeated abandonment.
func (e *Engine) Tripped(h Hook) bool {
	if e == nil {
		return false
	}
	return e.tripped.Load()&(1<<uint(h)) != 0
}

// Units reports how many programs and natives are registered for a hook.
func (e *Engine) Units(h Hook) (programs, natives int) {
	if e == nil || int(h) >= numHooks {
		return 0, 0
	}
	return len(e.progs[h]), len(e.natives[h])
}

// Limits returns the configured ceilings.
func (e *Engine) Limits() Limits {
	if e == nil {
		return Limits{}
	}
	return e.limits
}

// ---------------------------------------------------------------------------
// invocation

// watch runs fn under the wall-clock ceiling.
//
// The ceiling is enforced on this side of the call, not inside it: fn runs on
// its own goroutine and this function stops waiting when the deadline passes,
// whether or not fn noticed. A hook that ignores its context delays the request
// by the ceiling and no more. The goroutine is left running — there is no way to
// kill a goroutine in Go — so what the host can promise is not that it stops but
// that there are never more than [maxAbandoned] of them per hook: past that,
// invocations are refused before a goroutine exists to abandon.
func (e *Engine) watch(ctx context.Context, h Hook, fn func(context.Context) error) error {
	e.st.invocations.Add(1)

	if e.limits.Timeout <= 0 {
		return guard(h, "invocation", func() error { return fn(ctx) })
	}

	if e.abandoned[h].Load() >= maxAbandoned {
		// The hook already owns every goroutine it is allowed to lose. Starting
		// another would be the leak this bound exists to refuse, and the answer
		// would be thrown away at the deadline anyway.
		e.st.refused.Add(1)
		if err := ctx.Err(); err != nil {
			// Same exemption as the timeout path below: a request that is
			// already going away must not be able to trip an extension off,
			// or a disconnect storm becomes a permanent outage of the hook.
			return err
		}
		if e.consecTMO[h].Add(1) >= maxConsecutiveTimeouts {
			e.trip(h)
		}
		return ErrAbandonBacklog
	}

	cctx, cancel := context.WithTimeout(ctx, e.limits.Timeout)
	defer cancel()

	run := &inflight{e: e, h: h}
	// Buffered so an abandoned hook's send never blocks and the goroutine can
	// still finish and be collected.
	done := make(chan error, 1)
	go func() {
		err := guard(h, "invocation", func() error { return fn(cctx) })
		run.finish()
		done <- err
	}()

	select {
	case err := <-done:
		e.consecTMO[h].Store(0)
		return err
	case <-cctx.Done():
		if err := ctx.Err(); err != nil {
			// The *request* is going away, not the hook. Counting this would
			// trip a perfectly good extension off during a client disconnect
			// storm, which is the opposite of what the trip is for.
			//
			// The goroutine is still abandoned in the sense that nobody is
			// waiting for it, so it is registered as one: a disconnect storm
			// against a wedged hook leaks exactly as fast as anything else
			// does, which is to say not at all past the bound.
			run.abandon()
			return err
		}
		run.abandon()
		e.st.timeouts.Add(1)
		if e.consecTMO[h].Add(1) >= maxConsecutiveTimeouts {
			e.trip(h)
		}
		return ErrTimeout
	}
}

// inflight is the handshake between a watchdog that has stopped waiting and the
// goroutine it stopped waiting for.
//
// Exactly one of the two wins, and the counter is only touched when abandon
// wins, so a hook that returns a microsecond after its deadline costs nothing.
// abandon increments *before* claiming the state, so finish can never observe
// the claim without also observing the increment and can never drive the count
// below zero.
type inflight struct {
	e     *Engine
	h     Hook
	state atomic.Uint32 // 0 running, 1 finished, 2 abandoned
}

func (r *inflight) abandon() {
	r.e.abandoned[r.h].Add(1)
	if r.state.CompareAndSwap(0, 2) {
		return
	}
	r.e.abandoned[r.h].Add(-1)
}

func (r *inflight) finish() {
	if r.state.CompareAndSwap(0, 1) {
		return
	}
	r.e.abandoned[r.h].Add(-1)
}

// trip switches a hook off for the life of the engine.
func (e *Engine) trip(h Hook) {
	bit := uint32(1) << uint(h)
	for {
		old := e.tripped.Load()
		if old&bit != 0 {
			return
		}
		if e.tripped.CompareAndSwap(old, old|bit) {
			e.st.tripped.Add(1)
			if e.logf != nil {
				e.logf("luaext: %s: switched off after %d consecutive abandoned invocations; "+
					"the extension is not being waited for and will not be run again until "+
					"the configuration is reloaded", h, maxConsecutiveTimeouts)
			}
			return
		}
	}
}

// guard contains a panic. A hook that crashes must not crash the gateway, so
// the recovered value becomes an error that fails open like any other.
func guard(h Hook, unit string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Hook: h, Unit: unit, Value: r, Stack: debug.Stack()}
		}
	}()
	return fn()
}

// fatal reports whether an error ends the whole invocation rather than one
// unit. The ceilings are shared across units, so exhausting one ends everything;
// a panic is one unit's problem.
func fatal(err error) bool {
	return errors.Is(err, ErrInstructionLimit) || errors.Is(err, ErrMemoryLimit)
}

// skip counts and warns about a failed hook. Every path through it fails open.
func (e *Engine) skip(h Hook, unit string, err error) {
	e.st.skipped.Add(1)
	var pe *PanicError
	switch {
	case errors.As(err, &pe):
		e.st.panics.Add(1)
	case fatal(err):
		e.st.limitHits.Add(1)
	}
	if e.logf == nil {
		return
	}
	// One warning per hook per second. A hook that fails on every request would
	// otherwise turn a broken extension into a log-volume incident.
	now := e.now().UnixNano()
	last := e.lastWarn[h].Load()
	if now-last < int64(time.Second) {
		return
	}
	if !e.lastWarn[h].CompareAndSwap(last, now) {
		return
	}
	if pe != nil {
		e.logf("luaext: %s: %s panicked and was skipped: %v\n%s", h, unit, pe.Value, pe.Stack)
		return
	}
	e.logf("luaext: %s: %s skipped: %v", h, unit, err)
}

// runPrograms evaluates every program registered at h against v.
//
// A program that trips a ceiling ends the invocation (the budget is shared and
// now spent). A program that panics — which the language should make
// impossible, so this is defence in depth — is skipped and the next one runs.
func (e *Engine) runPrograms(h Hook, v viewer, b *budget, out *result) (stop bool, fatalErr error) {
	for _, p := range e.progs[h] {
		var res result
		res.tags = out.tags
		err := guard(h, p.Name, func() error {
			s, err := p.run(v, b, &res)
			stop = s
			return err
		})
		if err != nil {
			e.skip(h, p.Name, err)
			if fatal(err) {
				return false, err
			}
			stop = false
			continue
		}
		*out = res
		if stop {
			return true, nil
		}
	}
	return false, nil
}
