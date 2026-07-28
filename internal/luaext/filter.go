package luaext

import (
	"context"
	"errors"
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

// The transform-filter surface of DESIGN §10.5b.
//
// A filter is the one hook that may change what goes upstream, so it gets a
// different kind of view: not a flat struct of identifiers, but the request's
// *text*, segment by segment, with a way to write each segment back.
//
// The split between plugin and host is the whole design:
//
//   - The **plugin** decides what is sensitive. It walks the segments, applies
//     whatever judgement the operator's disclosure rules encode, and calls
//     dorang.mask on the text it wants removed.
//   - The **host** owns the mask table, mints the placeholders, escapes
//     placeholder-shaped input, and does all of the unmasking. Lua never sees
//     the table and never touches a response frame.
//
// That split is not tidiness. The table is the object §10.5b calls dangerous;
// handing it to untrusted code that also has a globals table which survives the
// request would be the same mistake as persisting it, one layer down.

// Masker is the host capability a filter plugin borrows for one invocation.
//
// It is an interface so that this package does not depend on internal/mask —
// which matters less for the import graph than for what it says: the mask
// implementation is replaceable, and nothing in the plugin surface knows how a
// placeholder is derived.
type Masker interface {
	// Mask replaces every configured pattern in text and returns the masked
	// text and the number of values replaced. A non-nil error fails the request
	// closed.
	Mask(text string) (string, int, error)
	// MaskValue masks one exact value the plugin identified itself, and returns
	// its placeholder. It is how an operator implements a rule no regular
	// expression can state.
	MaskValue(value string) (string, error)
}

// Doc is the request's text, as a filter sees it.
//
// A segment is one place text lives in the canonical request — a message block,
// a system prompt, a tool result, a string inside a tool call's arguments. The
// host builds the list, the plugin reads and writes segments by index, and the
// writes land back in the request that is about to be encoded.
//
// A Doc is not safe for concurrent use and belongs to one invocation.
type Doc struct {
	seg []segment
}

type segment struct {
	label string
	get   func() string
	set   func(string)
}

// NewDoc returns an empty document.
func NewDoc() *Doc { return &Doc{} }

// Add appends a segment backed by a string the caller owns.
func (d *Doc) Add(label string, p *string) {
	d.seg = append(d.seg, segment{
		label: label,
		get:   func() string { return *p },
		set:   func(s string) { *p = s },
	})
}

// AddFunc appends a segment whose read and write need more than a pointer —
// a string inside a JSON document, say.
func (d *Doc) AddFunc(label string, get func() string, set func(string)) {
	d.seg = append(d.seg, segment{label: label, get: get, set: set})
}

// Len reports how many segments the document has.
func (d *Doc) Len() int {
	if d == nil {
		return 0
	}
	return len(d.seg)
}

// Label names a segment, for a plugin that wants to treat a system prompt
// differently from a tool result.
func (d *Doc) Label(i int) string { return d.seg[i].label }

// Text reads a segment.
func (d *Doc) Text(i int) string { return d.seg[i].get() }

// SetText writes a segment back.
func (d *Doc) SetText(i int, s string) { d.seg[i].set(s) }

// FilterView is what on_filter_request sees.
//
// The identifying fields are the same shape as every other view — names and
// numbers, no credential — and the text arrives through [Doc] rather than as a
// field, so the view itself still cannot hold a body.
type FilterView struct {
	RequestID string
	Route     string
	Model     string
	KeyID     string
	KeyName   string
	UserID    string
	TeamID    string
	Priority  string
	Stream    bool

	// Doc is the text. It must not be nil.
	Doc *Doc
	// Mask is the host's masking capability, or nil when the model's filter
	// declared no patterns. A plugin that calls dorang.mask without one gets a
	// clear error rather than silent no-op masking.
	Mask Masker
	// Plugins restricts the run to the plugins the model's configuration named,
	// in the order it named them. Empty runs every registered filter, which is
	// only right for a caller with one.
	//
	// It exists because a filter is attached to a model: a plugin configured for
	// model A must not see model B's text just because both models have filters.
	Plugins []string
}

// FilterDecision is what the host does next.
type FilterDecision struct {
	tagset
	// Failed reports that a plugin did not complete. Whether that stops the
	// request is [FilterDecision.Refuse], which folds in the plugin's declared
	// fail mode.
	Failed bool
	// Refuse is set when a fail-closed plugin did not complete, or when a plugin
	// denied. DESIGN §10.5b: a filter that was supposed to remove an identity
	// number and did not must stop the request.
	Refuse bool
	// Reason explains a refusal to the caller.
	Reason string
	// Code is the machine-readable refusal code.
	Code string
	// Err is what went wrong, for the log. It is never shown to the caller.
	Err error
	// Masked counts the values the plugin masked in this request.
	Masked int
}

// FilterDenyCode is the refusal code for a filter that could not complete.
const FilterDenyCode = "filter_failed"

// EnabledFilter reports whether a filter plugin will run. Like [Engine.Enabled]
// it is the branch the hot path takes, and it allocates nothing.
//
// It is not a permission to skip. [Engine.FilterRequest] calls it and then
// refuses when it is false, because "this filter will not run" and "this request
// needs no filter" are different facts and only the caller knows the second one.
func (e *Engine) EnabledFilter() bool {
	if e == nil || e.lua == nil || !e.lua.filter {
		return false
	}
	return e.tripped.Load()&(1<<uint(HookFilterRequest)) == 0
}

// ErrFilterUnavailable reports that the filters a request was configured with
// could not be run at all: the hook has been tripped off, or this engine has no
// handler for one of the plugins the model named. It is not a plugin failure —
// nothing ran — and it fails closed for the same reason a plugin failure does.
var ErrFilterUnavailable = errors.New("luaext: the request's filters could not be run")

// FilterRequest runs the transform filters over a request.
//
// It inverts the package's usual asymmetry, and deliberately: a hook that breaks
// is skipped, but a *masking* filter that breaks refuses the request. The plugin
// declares which kind it is; `closed` is the default.
//
// # A filter that does not run is a filter that failed
//
// This used to return the zero [FilterDecision] whenever the hook was not going
// to run, and the zero value of a masking decision is *no masking and no
// refusal* — the request goes upstream carrying whatever it carried. Every way
// of not running therefore silently downgraded the one security control in the
// package into its own absence:
//
//   - The hook had been tripped off after repeated abandonment. A burst was
//     enough to arrange that, permanently, in under a second, and the trip was
//     designed for hooks whose absence is *safe*.
//   - The engine has no handler for the plugin the model named — a plugin that
//     loaded but registered nothing at on_filter_request, say. Configuration
//     checks that the *name* is declared; only this can check that it runs.
//
// So the absence is treated exactly as a failure: [applyFailMode] with
// the strictest declaration among the plugins in play, which for a filter
// defaults to closed. The one case that still returns nothing is the one that
// means nothing: a caller with no filters, on an engine with no filters.
func (e *Engine) FilterRequest(ctx context.Context, v *FilterView) FilterDecision {
	if !e.EnabledFilter() {
		var out FilterDecision
		if len(v.Plugins) == 0 && !e.hasFilters() {
			// Nothing was configured and nothing is missing: a caller with no
			// filters, on an engine with none. This is the branch the hot path
			// takes when the feature is off, and it still costs one nil check
			// and no allocation.
			return out
		}
		out.Failed = true
		out.Err = ErrFilterUnavailable
		if e != nil {
			e.skip(HookFilterRequest, "invocation", ErrFilterUnavailable)
		}
		applyFailMode(&out, e.failModeFor(v.Plugins))
		return out
	}
	if missing := e.lua.unregistered(v.Plugins); missing != "" {
		var out FilterDecision
		out.Failed = true
		out.Err = fmt.Errorf("%w: %q registers no on_filter_request", ErrFilterUnavailable, missing)
		e.skip(HookFilterRequest, missing, out.Err)
		applyFailMode(&out, e.failModeFor(v.Plugins))
		return out
	}
	j := &filterJob{e: e, v: v}
	if err := e.watch(ctx, HookFilterRequest, j.run); err != nil {
		// The job's own decision is deliberately neither read nor written here.
		// An invocation that failed by being *abandoned* is still running, and
		// still writing into it: this used to fold the failure into j.out and
		// return it, which is two goroutines and one struct. The other four
		// hooks avoid it by returning their zero decision; a filter cannot
		// return its zero decision, so it returns a fresh one.
		var out FilterDecision
		out.Failed = true
		out.Err = err
		e.skip(HookFilterRequest, "invocation", err)
		applyFailMode(&out, e.failModeFor(v.Plugins))
		return out
	}
	// Safe to read: watch returned only after the goroutine's send, which is the
	// happens-before edge.
	return j.out
}

// hasFilters reports whether any filter plugin is registered at all, tripped or
// not. It is the difference between "this engine has no filters" and "this
// engine's filters are not running".
func (e *Engine) hasFilters() bool { return e != nil && e.lua != nil && e.lua.filter }

// failModeFor is [luaRuntime.strictestFail] with an answer for the engine that
// has no runtime to ask. A filter this engine cannot see cannot have declared
// itself fail-open, and guessing open is how an identity number reaches an
// upstream, so the unknown is closed.
func (e *Engine) failModeFor(only []string) FailMode {
	if e == nil || e.lua == nil {
		return FailClosed
	}
	return e.lua.strictestFail(only)
}

// strictestFail returns the strictest fail mode among the plugins in play.
//
// When the invocation itself fails — a wall-clock breach, say — the host cannot
// tell which plugin was responsible, and guessing wrong in the permissive
// direction is how an identity number reaches an upstream. So the strictest
// declaration in the set wins, and a plugin the caller named that this engine
// has never heard of is strictest of all: it declared nothing here, and an
// undeclared filter is the default, which is closed.
func (rt *luaRuntime) strictestFail(only []string) FailMode {
	for _, name := range only {
		if rt.byName(name) == nil {
			return FailClosed
		}
	}
	for _, p := range rt.plugins {
		if !selected(only, p.name) {
			continue
		}
		if p.fail == FailClosed {
			return FailClosed
		}
	}
	return FailOpen
}

func (rt *luaRuntime) byName(name string) *loadedPlugin {
	for _, p := range rt.plugins {
		if p.name == name {
			return p
		}
	}
	return nil
}

// unregistered names a plugin the caller asked for that has no
// on_filter_request handler, or "" when every one of them has one.
//
// A plugin can load cleanly, be named correctly on a model, and register at no
// hook at all — a typo in the hook name is enough. Configuration can check that
// the plugin exists; only the runtime knows whether it registered anything, and
// before this the answer was a filter chain that ran zero handlers and reported
// a clean pass.
func (rt *luaRuntime) unregistered(only []string) string {
	for _, name := range only {
		p := rt.byName(name)
		if p == nil || !p.filter {
			return name
		}
	}
	return ""
}

// selected reports whether a plugin is in the caller's list. An empty list means
// every plugin.
func selected(only []string, name string) bool {
	if len(only) == 0 {
		return true
	}
	for _, n := range only {
		if n == name {
			return true
		}
	}
	return false
}

func applyFailMode(out *FilterDecision, mode FailMode) {
	if mode != FailClosed {
		return
	}
	out.Refuse = true
	if out.Code == "" {
		out.Code = FilterDenyCode
	}
	if out.Reason == "" {
		out.Reason = "a required request filter did not complete"
	}
}

type filterJob struct {
	e   *Engine
	v   *FilterView
	out FilterDecision
}

func (j *filterJob) run(ctx context.Context) error {
	e := j.e
	rt := e.lua
	v, err := rt.acquire()
	if err != nil {
		return err
	}
	keep := true
	defer func() {
		if keep {
			rt.release(v)
		} else {
			rt.discard(v)
		}
	}()

	var res result
	v.begin(ctx, e.limits)
	defer v.end()

	// The plugins run in the order the model listed them, not the order they
	// happened to load in: a filter chain is a sequence the operator wrote down.
	for _, hd := range orderedFilters(v.handlers[HookFilterRequest], j.v.Plugins) {
		v.inv = invocation{
			active: true, hook: HookFilterRequest, plugin: hd.plugin,
			res: &res, doc: j.v.Doc, mask: j.v.Mask,
		}
		cerr := v.call(hd.fn, j.buildView(v))
		maskErr := v.inv.maskErr
		denied, reason, code := v.inv.denied, v.inv.reason, v.inv.code

		if cerr != nil {
			cl := v.classify(ctx, cerr)
			if fatal(cl) || errors.Is(cl, context.Canceled) || errors.Is(cl, context.DeadlineExceeded) {
				keep = false
			}
			if maskErr != nil {
				// A mask that could not be completed is not a plugin bug to be
				// skipped: it is the thing the filter exists to do, not done.
				cl = maskErr
			}
			j.out.Failed = true
			j.out.Err = cl
			e.skip(HookFilterRequest, hd.plugin.name, cl)
			applyFailMode(&j.out, hd.plugin.fail)
			if j.out.Refuse {
				j.out.tagset = res.tags
				return nil
			}
			continue
		}
		if denied {
			j.out.Refuse = true
			j.out.Reason = reason
			j.out.Code = code
			if j.out.Code == "" {
				j.out.Code = FilterDenyCode
			}
			j.out.tagset = res.tags
			return nil
		}
	}
	j.out.tagset = res.tags
	return nil
}

// orderedFilters selects the handlers a model asked for, in the order it asked
// for them. With no selection the registration order stands.
func orderedFilters(all []handler, only []string) []*handler {
	out := make([]*handler, 0, len(all))
	if len(only) == 0 {
		for i := range all {
			out = append(out, &all[i])
		}
		return out
	}
	for _, name := range only {
		for i := range all {
			if all[i].plugin.name == name {
				out = append(out, &all[i])
			}
		}
	}
	return out
}

// buildView makes the Lua value one handler sees. It is built per handler, so a
// plugin that mutates the table cannot change what the next one is told.
func (j *filterJob) buildView(v *vmState) lua.LValue {
	L := v.L
	t := L.NewTable()
	t.RawSetString("request_id", lua.LString(j.v.RequestID))
	t.RawSetString("route", lua.LString(j.v.Route))
	t.RawSetString("model", lua.LString(j.v.Model))
	t.RawSetString("key_id", lua.LString(j.v.KeyID))
	t.RawSetString("key_name", lua.LString(j.v.KeyName))
	t.RawSetString("user_id", lua.LString(j.v.UserID))
	t.RawSetString("team_id", lua.LString(j.v.TeamID))
	t.RawSetString("priority", lua.LString(j.v.Priority))
	t.RawSetString("stream", lua.LBool(j.v.Stream))
	t.RawSetString("count", lua.LNumber(j.v.Doc.Len()))
	t.RawSetString("doc", v.docTable)
	return t
}
