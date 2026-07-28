package luaext

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
)

// APIVersion is the version of the value shape a plugin sees.
//
// DESIGN §11.5: "The value shape is versioned. A plugin written against today's
// on_request keeps working, or is told plainly that it does not." A plugin
// declares what it was written against with `dorang.require_api(n)`, and a
// mismatch is a *load* error naming both versions — not a nil field discovered
// halfway through a request.
const APIVersion = 1

// FailMode is what happens when a plugin cannot complete.
type FailMode uint8

const (
	// FailClosed refuses the request. It is the default for filters, and it is
	// DESIGN §10.5b's deliberate inversion of §11.5: "a filter that cannot
	// enrich a request should be skipped, but a filter that was supposed to
	// remove an identity number and did not must stop the request".
	FailClosed FailMode = iota
	// FailOpen skips the plugin, which is the rule for everything else.
	FailOpen
)

// String returns the configuration spelling.
func (f FailMode) String() string {
	if f == FailOpen {
		return "open"
	}
	return "closed"
}

// ParseFailMode resolves a configuration spelling.
func ParseFailMode(s string) (FailMode, bool) {
	switch s {
	case "closed", "":
		return FailClosed, true
	case "open":
		return FailOpen, true
	}
	return 0, false
}

// Plugin is one Lua plugin, named explicitly in the configuration.
//
// DESIGN §11.5: "Loading is explicit configuration, never a scan of a writable
// directory. A plugin mechanism that picks up whatever appears in a path is a
// code-execution primitive." Each plugin here was written down by an operator,
// with a path, a name and its own configuration.
type Plugin struct {
	// Name identifies the plugin in errors, counters and model filter
	// references.
	Name string
	// Path is the file to load.
	Path string
	// Source overrides Path when set. It exists for tests and for an embedder
	// that ships a plugin inside the binary; Path is then only a label.
	Source []byte
	// Config is the operator's per-plugin configuration. It reaches the plugin
	// as `dorang.config`, a table of strings, readable while the plugin loads.
	Config map[string]string
	// Fail decides what a broken filter does. It has no effect on the four
	// hooks, which are fail-open by §11.5 and not negotiable.
	Fail FailMode
}

// ErrPluginFailed reports that a fail-closed plugin did not complete. It is the
// one extension outcome that stops traffic without a policy having said no.
var ErrPluginFailed = errors.New("luaext: a fail-closed plugin did not complete")

// loadedPlugin is a compiled plugin, shared by every VM instance.
type loadedPlugin struct {
	name   string
	path   string
	proto  *lua.FunctionProto
	config map[string]string
	fail   FailMode
	// filter records that this plugin registered an on_filter_request handler.
	// A model may name a plugin that registers nothing; see
	// [luaRuntime.unregistered] for why that has to be an error rather than an
	// empty chain.
	filter bool
}

// handler is one registered Lua function.
type handler struct {
	plugin *loadedPlugin
	fn     *lua.LFunction
}

// luaRuntime owns the compiled plugins and the pool of VM instances.
//
// # Why a pool, and what it means for plugin state
//
// Creating an LState costs far more than running a small hook, so instances are
// reused. A consequence has to be stated rather than discovered: a plugin's
// globals persist across requests inside one instance, exactly as they would in
// any long-running Lua program. A plugin that stashes request text in a global
// is keeping it beyond the request.
//
// That is bounded by everything else in the sandbox — there is no I/O, so
// stashed data cannot go anywhere — and it is why the mask table is owned by the
// host and never handed to Lua, and why every capability the host lends a hook
// stops working the moment the invocation ends.
type luaRuntime struct {
	plugins []*loadedPlugin
	limits  Limits
	logf    func(string, ...any)

	pool     chan *vmState
	created  atomic.Int64
	discards atomic.Int64

	hookMask uint8
	filter   bool
}

// maxPooledStates bounds the VM instances kept warm. Beyond it, instances are
// created on demand and closed when they are done rather than kept: a burst
// costs allocation instead of a queue, and the steady-state footprint stays
// where the operator can predict it.
const maxPooledStates = 16

// loadTimeout bounds a plugin's top-level code. Loading runs plugin code — a
// chunk is a function — so it gets a budget like everything else, just a more
// generous one, because loading happens once and may legitimately build tables.
const loadTimeout = 5 * time.Second

func newLuaRuntime(plugins []Plugin, limits Limits, logf func(string, ...any)) (*luaRuntime, error) {
	rt := &luaRuntime{
		limits: limits,
		logf:   logf,
		pool:   make(chan *vmState, maxPooledStates),
	}
	seen := make(map[string]bool, len(plugins))
	for i := range plugins {
		p := &plugins[i]
		if p.Name == "" {
			return nil, fmt.Errorf("luaext: plugins[%d]: needs a name", i)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("luaext: plugins[%d]: %q is declared twice", i, p.Name)
		}
		seen[p.Name] = true

		src := p.Source
		if src == nil {
			if p.Path == "" {
				return nil, fmt.Errorf("luaext: plugin %q: needs a path", p.Name)
			}
			b, err := os.ReadFile(p.Path)
			if err != nil {
				return nil, fmt.Errorf("luaext: plugin %q: %w", p.Name, err)
			}
			src = b
		}
		proto, err := compileLua(p.Name, p.Path, src)
		if err != nil {
			return nil, err
		}
		rt.plugins = append(rt.plugins, &loadedPlugin{
			name: p.Name, path: p.Path, proto: proto, config: p.Config, fail: p.Fail,
		})
	}

	// Building one instance now is what turns "this plugin is broken" into a
	// startup error rather than a surprise on the first request that happens to
	// reach the hook.
	v, err := rt.newState()
	if err != nil {
		return nil, err
	}
	for h := 0; h < numHooks; h++ {
		if len(v.handlers[h]) > 0 {
			rt.hookMask |= 1 << uint(h)
		}
	}
	for _, hd := range v.handlers[HookFilterRequest] {
		hd.plugin.filter = true
	}
	rt.filter = len(v.handlers[HookFilterRequest]) > 0
	rt.release(v)
	return rt, nil
}

// compileLua parses, instruments and compiles one plugin.
func compileLua(name, path string, src []byte) (*lua.FunctionProto, error) {
	label := path
	if label == "" {
		label = name
	}
	stmts, err := parse.Parse(strings.NewReader(string(src)), label)
	if err != nil {
		return nil, fmt.Errorf("luaext: plugin %q: %w", name, err)
	}
	proto, err := lua.Compile(instrumentChunk(stmts), label)
	if err != nil {
		return nil, fmt.Errorf("luaext: plugin %q: %w", name, err)
	}
	return proto, nil
}

// newState builds a VM instance and runs every plugin's top-level code in it.
func (rt *luaRuntime) newState() (*vmState, error) {
	v := &vmState{rt: rt}
	newSandbox(v)
	installDorang(v)

	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()

	for _, p := range rt.plugins {
		v.loading = p
		// The plugin's own configuration, readable while it loads. It is
		// replaced for each plugin and cleared afterwards, so a plugin that
		// reads it from inside a hook gets nil rather than someone else's.
		cfg := v.L.NewTable()
		for k, val := range p.config {
			cfg.RawSetString(k, lua.LString(val))
		}
		v.dorang.RawSetString("config", cfg)
		v.begin(ctx, Limits{
			// Loading gets a larger budget than a hook: it runs once per VM
			// instance and a plugin may build lookup tables. It is still
			// bounded, because "once" is once per instance, not once ever.
			Instructions: rt.limits.Instructions * 10,
			MemoryBytes:  rt.limits.MemoryBytes * 4,
		})
		fn := v.L.NewFunctionFromProto(p.proto)
		v.L.Push(fn)
		err := v.L.PCall(0, 0, nil)
		v.end()
		v.loading = nil
		if err != nil {
			v.L.Close()
			return nil, fmt.Errorf("luaext: plugin %q: %w", p.name, v.classify(ctx, err))
		}
	}
	// `dorang.config` belongs to the plugin that was loading. Clearing it means
	// a plugin that reads it from inside a hook gets nil rather than whichever
	// plugin happened to load last.
	v.dorang.RawSetString("config", lua.LNil)
	rt.created.Add(1)
	return v, nil
}

func (rt *luaRuntime) acquire() (*vmState, error) {
	select {
	case v := <-rt.pool:
		return v, nil
	default:
	}
	return rt.newState()
}

func (rt *luaRuntime) release(v *vmState) {
	v.inv = invocation{}
	select {
	case rt.pool <- v:
	default:
		rt.discards.Add(1)
		v.L.Close()
	}
}

// discard drops an instance without returning it to the pool. It is used after a
// ceiling breach or a cancellation, where the instance is not known to be in a
// state worth reusing.
func (rt *luaRuntime) discard(v *vmState) {
	rt.discards.Add(1)
	v.L.Close()
}

// vmState is one VM instance: an LState, its budget, and its bindings for the
// invocation currently running on it.
type vmState struct {
	L  *lua.LState
	rt *luaRuntime

	gasLeft int64
	memLeft int64
	noGas   bool
	noMem   bool
	aborted error
	// done is the invocation context's cancellation channel. gopher-lua checks
	// the context itself between VM instructions; this is the same signal in the
	// form a *host* call can check, and it is what stops a priced pattern match
	// from running on after the request has gone.
	done <-chan struct{}

	dorang   *lua.LTable
	docTable *lua.LTable
	handlers [numHooks][]handler
	loading  *loadedPlugin

	inv invocation
}

// invocation is what the host lends a hook for the duration of one call. Every
// field is cleared when the call ends, which is what makes a capability a plugin
// stashed in a global useless afterwards.
type invocation struct {
	active  bool
	hook    Hook
	plugin  *loadedPlugin
	res     *result
	doc     *Doc
	mask    Masker
	maskErr error
	denied  bool
	reason  string
	code    string
}

// begin resets the budget for one invocation.
func (v *vmState) begin(ctx context.Context, l Limits) {
	v.gasLeft = l.Instructions
	v.memLeft = l.MemoryBytes
	v.noGas = l.Instructions <= 0
	v.noMem = l.MemoryBytes <= 0
	v.aborted = nil
	v.done = nil
	if ctx != nil {
		v.done = ctx.Done()
		v.L.SetContext(ctx)
	}
}

func (v *vmState) end() {
	v.L.RemoveContext()
	v.done = nil
	v.inv = invocation{}
}

// classify turns a gopher-lua error into the host's vocabulary.
//
// It matters because the three outcomes are handled differently: a ceiling ends
// the whole invocation, a cancellation is the request going away, and anything
// else is one plugin's bug and the next plugin still runs.
func (v *vmState) classify(ctx context.Context, err error) error {
	if v.aborted != nil {
		return v.aborted
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// call runs one handler and reads its answer.
//
// The protocol is small on purpose: return nothing to allow, or `false, reason,
// code` to refuse. A plugin that wants to be explicit can call dorang.deny.
func (v *vmState) call(fn *lua.LFunction, arg lua.LValue) error {
	err := v.L.CallByParam(lua.P{Fn: fn, NRet: 3, Protect: true}, arg)
	if err != nil {
		return err
	}
	r1, r2, r3 := v.L.Get(-3), v.L.Get(-2), v.L.Get(-1)
	v.L.Pop(3)
	if r1 == lua.LFalse {
		v.inv.denied = true
		if s, ok := r2.(lua.LString); ok && v.inv.reason == "" {
			v.inv.reason = trimForLog(string(s))
		}
		if s, ok := r3.(lua.LString); ok && v.inv.code == "" {
			v.inv.code = trimForLog(string(s))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// the dorang table

func installDorang(v *vmState) {
	L := v.L
	d := L.NewTable()
	v.dorang = d
	L.SetGlobal("dorang", d)

	d.RawSetString("api", lua.LNumber(APIVersion))
	d.RawSetString("require_api", L.NewFunction(v.requireAPI))
	d.RawSetString("register", L.NewFunction(v.register))
	d.RawSetString("tag", L.NewFunction(v.tag))
	d.RawSetString("deny", L.NewFunction(v.deny))
	d.RawSetString("log", L.NewFunction(v.log))
	d.RawSetString("mask", L.NewFunction(v.maskText))
	d.RawSetString("mask_value", L.NewFunction(v.maskValue))

	doc := L.NewTable()
	v.docTable = doc
	doc.RawSetString("count", L.NewFunction(v.docCount))
	doc.RawSetString("label", L.NewFunction(v.docLabel))
	doc.RawSetString("text", L.NewFunction(v.docText))
	doc.RawSetString("set_text", L.NewFunction(v.docSetText))
}

func (v *vmState) requireAPI(L *lua.LState) int {
	want := int(lua.LVAsNumber(L.Get(1)))
	if want != APIVersion {
		L.RaiseError("this plugin was written for dorang plugin API %d; this build offers %d. "+
			"The value shape changed, so it is being told plainly rather than run against fields it does not expect",
			want, APIVersion)
	}
	return 0
}

func (v *vmState) register(L *lua.LState) int {
	if v.loading == nil {
		L.RaiseError("dorang.register may only be called while a plugin loads")
	}
	name := lua.LVAsString(L.Get(1))
	h, ok := ParseHook(name)
	if !ok {
		L.RaiseError("%q is not a hook point; want one of %s", name, strings.Join(hookNames[:], ", "))
	}
	fn, ok := L.Get(2).(*lua.LFunction)
	if !ok {
		L.RaiseError("dorang.register(%q, ...) wants a function", name)
	}
	v.handlers[h] = append(v.handlers[h], handler{plugin: v.loading, fn: fn})
	return 0
}

func (v *vmState) tag(L *lua.LState) int {
	if v.inv.res == nil {
		L.RaiseError("dorang.tag is only available inside a hook")
	}
	v.inv.res.tags.set(trimForLog(lua.LVAsString(L.Get(1))), trimForLog(lua.LVAsString(L.Get(2))))
	return 0
}

func (v *vmState) deny(L *lua.LState) int {
	if !v.inv.active {
		L.RaiseError("dorang.deny is only available inside a hook")
	}
	v.inv.denied = true
	if s := lua.LVAsString(L.Get(1)); s != "" && v.inv.reason == "" {
		v.inv.reason = trimForLog(s)
	}
	if s := lua.LVAsString(L.Get(2)); s != "" && v.inv.code == "" {
		v.inv.code = trimForLog(s)
	}
	return 0
}

func (v *vmState) log(L *lua.LState) int {
	if v.rt.logf == nil {
		return 0
	}
	name := "plugin"
	switch {
	case v.inv.plugin != nil:
		name = v.inv.plugin.name
	case v.loading != nil:
		name = v.loading.name
	}
	v.rt.logf("luaext: %s: %s", name, trimForLog(lua.LVAsString(L.Get(1))))
	return 0
}

func (v *vmState) maskText(L *lua.LState) int {
	if v.inv.mask == nil {
		L.RaiseError("dorang.mask is only available inside on_filter_request, and only when a mask is configured")
	}
	in := lua.LVAsString(L.Get(1))
	out, n, err := v.inv.mask.Mask(in)
	if err != nil {
		// The plugin is not allowed to swallow this. Recording it on the
		// invocation means the host refuses the request even if the plugin
		// carries on as though nothing happened.
		v.inv.maskErr = err
		L.RaiseError("dorang.mask: %s", err.Error())
	}
	v.charge(int64(len(out)) + stringOverhead)
	L.Push(lua.LString(out))
	L.Push(lua.LNumber(n))
	return 2
}

func (v *vmState) maskValue(L *lua.LState) int {
	if v.inv.mask == nil {
		L.RaiseError("dorang.mask_value is only available inside on_filter_request, and only when a mask is configured")
	}
	in := lua.LVAsString(L.Get(1))
	if in == "" {
		L.RaiseError("dorang.mask_value wants a non-empty string")
	}
	ph, err := v.inv.mask.MaskValue(in)
	if err != nil {
		v.inv.maskErr = err
		L.RaiseError("dorang.mask_value: %s", err.Error())
	}
	v.charge(int64(len(ph)) + stringOverhead)
	L.Push(lua.LString(ph))
	return 1
}

func (v *vmState) docCount(L *lua.LState) int {
	if v.inv.doc == nil {
		L.RaiseError("the document is only available inside on_filter_request")
	}
	L.Push(lua.LNumber(v.inv.doc.Len()))
	return 1
}

func (v *vmState) docIndex(L *lua.LState) int {
	i := int(lua.LVAsNumber(L.Get(1)))
	if v.inv.doc == nil {
		L.RaiseError("the document is only available inside on_filter_request")
	}
	if i < 1 || i > v.inv.doc.Len() {
		L.RaiseError("segment %d is out of range (the document has %d)", i, v.inv.doc.Len())
	}
	return i - 1
}

func (v *vmState) docLabel(L *lua.LState) int {
	i := v.docIndex(L)
	L.Push(lua.LString(v.inv.doc.Label(i)))
	return 1
}

func (v *vmState) docText(L *lua.LState) int {
	i := v.docIndex(L)
	s := v.inv.doc.Text(i)
	v.charge(int64(len(s)) + stringOverhead)
	L.Push(lua.LString(s))
	return 1
}

func (v *vmState) docSetText(L *lua.LState) int {
	i := v.docIndex(L)
	s := lua.LVAsString(L.Get(2))
	v.inv.doc.SetText(i, s)
	return 0
}

// ---------------------------------------------------------------------------
// dispatch

// luaHookRun runs every Lua handler registered at h.
//
// It returns a fatal error when a ceiling was hit or the request was cancelled —
// both of which end the invocation and fail open — and skips a plugin that
// merely errored, so one broken plugin does not silence the next one.
func (e *Engine) luaHookRun(ctx context.Context, h Hook, res *result,
	build func(*lua.LState) lua.LValue) (denied bool, reason, code string, err error) {

	rt := e.lua
	if rt == nil || rt.hookMask&(1<<uint(h)) == 0 {
		return false, "", "", nil
	}
	v, err := rt.acquire()
	if err != nil {
		return false, "", "", err
	}
	keep := true
	defer func() {
		if keep {
			rt.release(v)
		} else {
			rt.discard(v)
		}
	}()

	v.begin(ctx, e.limits)
	defer v.end()

	for i := range v.handlers[h] {
		hd := &v.handlers[h][i]
		v.inv = invocation{active: true, hook: h, plugin: hd.plugin, res: res}
		cerr := v.call(hd.fn, build(v.L))
		if cerr != nil {
			cl := v.classify(ctx, cerr)
			if fatal(cl) || errors.Is(cl, context.Canceled) || errors.Is(cl, context.DeadlineExceeded) {
				keep = false
				return false, "", "", cl
			}
			e.skip(h, hd.plugin.name, cl)
			continue
		}
		if v.inv.denied {
			return true, v.inv.reason, v.inv.code, nil
		}
	}
	return false, "", "", nil
}
