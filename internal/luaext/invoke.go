package luaext

import (
	"context"
	"errors"

	lua "github.com/yuin/gopher-lua"
)

// The four hook entry points.
//
// Each has the same shape and the same guarantees:
//
//   - Disabled costs one nil check and no allocation.
//   - The invocation runs on a watchdog goroutine and the caller stops waiting
//     at the wall-clock ceiling.
//   - Anything that goes wrong — a ceiling, a panic, a cancelled request —
//     returns the zero decision, which permits. That is DESIGN §11.5's
//     fail-open.
//   - A deny is only ever returned by a hook that ran to completion inside its
//     ceilings. That is §11.5's fail-closed exception, and it is the reason the
//     job writes into its own decision that is copied out only on success:
//     a hook that panics halfway through setting Denied must not be able to
//     refuse a request by crashing.

// DefaultDenyCode is the refusal code used when a hook denies without naming
// one.
const DefaultDenyCode = "extension_denied"

// OnRequest runs the on_request hook.
func (e *Engine) OnRequest(ctx context.Context, v *RequestView) RequestDecision {
	if !e.Enabled(HookRequest) {
		return RequestDecision{}
	}
	j := &requestJob{e: e, v: v}
	if err := e.watch(ctx, HookRequest, j.run); err != nil {
		e.skip(HookRequest, "invocation", err)
		return RequestDecision{}
	}
	if j.out.Denied {
		e.st.denies.Add(1)
		if j.out.Code == "" {
			j.out.Code = DefaultDenyCode
		}
		if j.out.Reason == "" {
			j.out.Reason = "refused by a gateway extension"
		}
	}
	return j.out
}

type requestJob struct {
	e   *Engine
	v   *RequestView
	out RequestDecision
}

func (j *requestJob) run(ctx context.Context) error {
	e := j.e
	b := newBudget(e.limits)

	var res result
	stop, err := e.runPrograms(HookRequest, j.v, &b, &res)
	if err != nil {
		return err
	}
	j.out.tagset = res.tags
	if stop {
		if res.denied {
			j.out.Denied = true
			j.out.Reason = res.reason
			return nil
		}
		// An explicit allow ends evaluation, natives included: a rule that says
		// "this key is exempt" has to be able to mean it.
		return nil
	}

	denied, reason, code, err := e.luaHookRun(ctx, HookRequest, &res,
		func(L *lua.LState) lua.LValue { return requestTable(L, j.v) })
	if err != nil {
		return err
	}
	j.out.tagset = res.tags
	if denied {
		j.out.Denied = true
		j.out.Reason = reason
		j.out.Code = code
		return nil
	}

	for i := range e.natives[HookRequest] {
		n := &e.natives[HookRequest][i]
		if n.Request == nil {
			continue
		}
		scratch := j.out
		if err := guard(HookRequest, n.Name, func() error {
			n.Request(ctx, j.v, &scratch)
			return nil
		}); err != nil {
			e.skip(HookRequest, n.Name, err)
			continue
		}
		j.out = scratch
		if j.out.Denied {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// OnRoute runs the on_route hook.
func (e *Engine) OnRoute(ctx context.Context, v *RouteView) RouteDecision {
	if !e.Enabled(HookRoute) {
		return RouteDecision{}
	}
	j := &routeJob{e: e, v: v}
	if err := e.watch(ctx, HookRoute, j.run); err != nil {
		e.skip(HookRoute, "invocation", err)
		return RouteDecision{}
	}
	if j.out.Denied {
		e.st.denies.Add(1)
		if j.out.Code == "" {
			j.out.Code = DefaultDenyCode
		}
		if j.out.Reason == "" {
			j.out.Reason = "refused by a gateway extension"
		}
	}
	return j.out
}

type routeJob struct {
	e   *Engine
	v   *RouteView
	out RouteDecision
}

func (j *routeJob) run(ctx context.Context) error {
	e := j.e
	b := newBudget(e.limits)

	var res result
	stop, err := e.runPrograms(HookRoute, j.v, &b, &res)
	if err != nil {
		return err
	}
	j.out.tagset = res.tags
	if stop {
		if res.denied {
			j.out.Denied = true
			j.out.Reason = res.reason
		}
		return nil
	}

	denied, reason, code, err := e.luaHookRun(ctx, HookRoute, &res,
		func(L *lua.LState) lua.LValue { return routeTable(L, j.v) })
	if err != nil {
		return err
	}
	j.out.tagset = res.tags
	if denied {
		j.out.Denied = true
		j.out.Reason = reason
		j.out.Code = code
		return nil
	}

	for i := range e.natives[HookRoute] {
		n := &e.natives[HookRoute][i]
		if n.Route == nil {
			continue
		}
		scratch := j.out
		if err := guard(HookRoute, n.Name, func() error {
			n.Route(ctx, j.v, &scratch)
			return nil
		}); err != nil {
			e.skip(HookRoute, n.Name, err)
			continue
		}
		j.out = scratch
		if j.out.Denied {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// OnResponse runs the on_response hook. It observes and annotates; the answer
// has already gone to the client.
func (e *Engine) OnResponse(ctx context.Context, v *ResponseView) ResponseDecision {
	if !e.Enabled(HookResponse) {
		return ResponseDecision{}
	}
	j := &responseJob{e: e, v: v}
	if err := e.watch(ctx, HookResponse, j.run); err != nil {
		e.skip(HookResponse, "invocation", err)
		return ResponseDecision{}
	}
	return j.out
}

type responseJob struct {
	e   *Engine
	v   *ResponseView
	out ResponseDecision
}

func (j *responseJob) run(ctx context.Context) error {
	e := j.e
	b := newBudget(e.limits)

	var res result
	if _, err := e.runPrograms(HookResponse, j.v, &b, &res); err != nil {
		return err
	}
	if _, _, _, err := e.luaHookRun(ctx, HookResponse, &res,
		func(L *lua.LState) lua.LValue { return responseTable(L, j.v) }); err != nil {
		return err
	}
	j.out.tagset = res.tags

	for i := range e.natives[HookResponse] {
		n := &e.natives[HookResponse][i]
		if n.Response == nil {
			continue
		}
		scratch := j.out
		if err := guard(HookResponse, n.Name, func() error {
			n.Response(ctx, j.v, &scratch)
			return nil
		}); err != nil {
			e.skip(HookResponse, n.Name, err)
			continue
		}
		j.out = scratch
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// OnEmail runs the on_email hook.
//
// It has a second job the other three do not: for the `lua` notification driver
// a [Native] with an Email function is the transport, so a non-nil error here is
// a delivery failure that internal/notify retries under its own backoff. A
// suppressed message returns Denied with no error.
func (e *Engine) OnEmail(ctx context.Context, v *EmailView) (EmailDecision, error) {
	if !e.Enabled(HookEmail) {
		return EmailDecision{}, nil
	}
	j := &emailJob{e: e, v: v}
	if err := e.watch(ctx, HookEmail, j.run); err != nil {
		if fatal(err) || isPanic(err) || errors.Is(err, ErrTimeout) || errors.Is(err, ErrAbandonBacklog) {
			e.skip(HookEmail, "invocation", err)
			// Fail open: a broken filter must not silently swallow an alert.
			// A backlog refusal is the same failure as the timeout it stands in
			// for, and a notification driver must not retry it as a delivery
			// error — the hook is wedged, and retrying is how that becomes a
			// queue rather than a counter.
			return EmailDecision{}, nil
		}
		return EmailDecision{}, err
	}
	if j.deliverErr != nil {
		return j.out, j.deliverErr
	}
	return j.out, nil
}

// HasEmailTransport reports whether a Native can actually deliver a message.
//
// The `lua` driver of DESIGN §11.5 delivers through the on_email hook. A policy
// program can only suppress, so an engine with programs but no Native email
// function is a filter with no transport — internal/notify has to be able to
// say so out loud rather than count deliveries that never happened.
func (e *Engine) HasEmailTransport() bool {
	if !e.Enabled(HookEmail) {
		return false
	}
	for i := range e.natives[HookEmail] {
		if e.natives[HookEmail][i].Email != nil {
			return true
		}
	}
	return false
}

type emailJob struct {
	e          *Engine
	v          *EmailView
	out        EmailDecision
	deliverErr error
}

func (j *emailJob) run(ctx context.Context) error {
	e := j.e
	b := newBudget(e.limits)

	var res result
	stop, err := e.runPrograms(HookEmail, j.v, &b, &res)
	if err != nil {
		return err
	}
	j.out.tagset = res.tags
	if stop && res.denied {
		j.out.Denied = true
		j.out.Reason = res.reason
		return nil
	}

	denied, reason, _, err := e.luaHookRun(ctx, HookEmail, &res,
		func(L *lua.LState) lua.LValue { return emailTable(L, j.v) })
	if err != nil {
		return err
	}
	j.out.tagset = res.tags
	if denied {
		j.out.Denied = true
		j.out.Reason = reason
		return nil
	}

	for i := range e.natives[HookEmail] {
		n := &e.natives[HookEmail][i]
		if n.Email == nil {
			continue
		}
		scratch := j.out
		var derr error
		if err := guard(HookEmail, n.Name, func() error {
			derr = n.Email(ctx, j.v, &scratch)
			return nil
		}); err != nil {
			e.skip(HookEmail, n.Name, err)
			continue
		}
		j.out = scratch
		if derr != nil {
			j.deliverErr = derr
			return nil
		}
		if j.out.Denied {
			return nil
		}
	}
	return nil
}

func isPanic(err error) bool {
	_, ok := err.(*PanicError)
	return ok
}
