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
	Hooks []string
	// Limits are the three ceilings. Zero fields take package defaults.
	Limits Limits
	// Native are Go-implemented hooks.
	Native []Native
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

	logf func(string, ...any)
	now  func() time.Time

	tripped   atomic.Uint32
	consecTMO [numHooks]atomic.Int64
	lastWarn  [numHooks]atomic.Int64

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
	}
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

	for h := 0; h < numHooks; h++ {
		if len(e.progs[h]) > 0 || len(e.natives[h]) > 0 {
			e.mask |= 1 << uint(h)
		}
	}
	return e, nil
}

// hookSet turns the configured hook names into a bitmask. An empty list means
// all four, which matches internal/config's default.
func hookSet(names []string) (uint8, error) {
	if len(names) == 0 {
		return (1 << numHooks) - 1, nil
	}
	var m uint8
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
// by the ceiling and no more. The goroutine is left running — there is no way
// to kill a goroutine in Go — which is why repeated abandonment trips the hook
// off entirely.
func (e *Engine) watch(ctx context.Context, h Hook, fn func(context.Context) error) error {
	e.st.invocations.Add(1)

	if e.limits.Timeout <= 0 {
		return guard(h, "invocation", func() error { return fn(ctx) })
	}

	cctx, cancel := context.WithTimeout(ctx, e.limits.Timeout)
	defer cancel()

	// Buffered so an abandoned hook's send never blocks and the goroutine can
	// still finish and be collected.
	done := make(chan error, 1)
	go func() {
		done <- guard(h, "invocation", func() error { return fn(cctx) })
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
			return err
		}
		e.st.timeouts.Add(1)
		if e.consecTMO[h].Add(1) >= maxConsecutiveTimeouts {
			e.trip(h)
		}
		return ErrTimeout
	}
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
