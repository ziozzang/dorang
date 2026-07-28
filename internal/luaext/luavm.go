package luaext

import (
	"math"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// The sandbox: what a plugin can reach, and what it is charged for.
//
// The global surface is built by opening three of gopher-lua's libraries and
// then removing every name not on an allow-list, rather than by removing the
// dangerous ones. The difference matters: a deny-list is a list of things
// somebody remembered, and TestGlobalSurfaceIsExactlyTheAllowlist walks
// everything reachable from the globals table — transitively, through tables and
// metatables — and fails if a single name appears that is not written down here.
// A new gopher-lua release that adds a function to `string` cannot quietly widen
// the sandbox.
//
// Four whole libraries are never opened: io, os, debug and coroutine, plus the
// package loader. A plugin has no file system, no clock, no network, no
// subprocess and no way to introspect the host. That is also why a hook cannot
// exfiltrate a secret even if it found one — but see [RequestView] for why it
// cannot find one either.

// stringOverhead is charged on top of a string's bytes: a Go string header, the
// LString boxing, and the allocator's rounding. It is an estimate, and it is
// deliberately generous — the budget is a ceiling, not an accounting system.
const stringOverhead = 48

// valueOverheadLua is charged per value pushed onto the Lua stack by a builtin
// that can push many.
const valueOverheadLua = 16

// baseAllow is every name kept from the base library.
//
// Absent, and each for a reason:
//
//   - load, loadstring, loadfile, dofile, require, module: every one of them
//     turns text into a function, and a function that did not go through
//     [instrumentChunk] has no instruction ceiling. This is the single most
//     important removal in the file.
//   - pcall, xpcall: a ceiling a plugin can catch is not a ceiling.
//     `while true do pcall(f) end` would swallow the abort and loop until the
//     wall clock, leaking the goroutine the watchdog abandons. There is no way
//     to make a Lua error uncatchable in gopher-lua, so the catcher goes instead.
//   - getfenv, setfenv, _G: all three hand out the globals table by value, and
//     the charge functions live in it under names the lexer cannot spell. Taking
//     the names away is only half a defence; taking the table away is the other.
//   - collectgarbage: lets a plugin discard what it was charged for and start
//     again, which turns a cumulative budget into no budget.
//   - print, newproxy, _printregs: no I/O, and no undocumented corners.
var baseAllow = map[string]bool{
	"_VERSION":     true,
	"assert":       true,
	"error":        true,
	"getmetatable": true,
	"ipairs":       true,
	"next":         true,
	"pairs":        true,
	"rawequal":     true,
	"rawget":       true,
	"rawset":       true,
	"select":       true,
	"setmetatable": true,
	"tonumber":     true,
	"tostring":     true,
	"type":         true,
	"unpack":       true,
}

// stringAllow keeps the whole string library except `dump`, which serialises a
// function, and `gfind`, a 5.0 alias nobody should be writing in 2026.
var stringAllow = map[string]bool{
	"__index": true, "byte": true, "char": true, "find": true, "format": true,
	"gmatch": true, "gsub": true, "len": true, "lower": true, "match": true,
	"rep": true, "reverse": true, "sub": true, "upper": true,
}

var tableAllow = map[string]bool{
	"concat": true, "insert": true, "maxn": true, "remove": true, "sort": true,
}

// mathAllow keeps everything except the two random functions.
//
// Randomness is not a security problem here; it is a *determinism* problem, and
// determinism is what keeps a masked conversation's bytes identical from one
// turn to the next and therefore keeps the backend's prefix cache alive (§7.4b,
// and DESIGN §10.5b's derived placeholders). A filter that consulted
// math.random would produce different upstream bytes for the same conversation
// on every turn and nothing would say why. A plugin that genuinely needs a
// random-looking value should derive it, as the mask does.
var mathAllow = map[string]bool{
	"abs": true, "ceil": true, "cos": true, "cosh": true, "exp": true,
	"floor": true, "fmod": true, "frexp": true, "huge": true, "ldexp": true,
	"log": true, "log10": true, "max": true, "min": true, "modf": true,
	"pi": true, "pow": true, "sin": true, "sinh": true, "sqrt": true,
	"tan": true, "tanh": true,
}

// preFn charges for a builtin call *before* it runs.
//
// Charging afterwards would be too late for exactly the functions that need it:
// string.rep("x", 1<<31) has already allocated two gigabytes by the time a
// post-hoc accountant sees the result. So the functions whose output can exceed
// their input pre-flight their worst case against the remaining budget, and the
// budget is what bounds the single largest allocation a plugin can make.
type preFn func(v *vmState, L *lua.LState, nargs int)

// preflight lists them. Everything not here is charged for what it returned,
// which is sound because its output is bounded by its input.
var preflight = map[string]map[string]preFn{
	"string": {
		"rep":    preRep,
		"format": preFormat,
		"gsub":   preGsub,
		"byte":   preByte,
		"char":   preChar,
	},
	"table": {
		"concat": preTableConcat,
	},
	"": {
		"unpack": preUnpack,
	},
}

// newSandbox builds a fresh LState with the sandbox installed and the charge
// functions in place.
func newSandbox(v *vmState) *lua.LState {
	L := lua.NewState(lua.Options{
		SkipOpenLibs: true,
		// A bounded call stack is the recursion ceiling. Deep recursion is
		// charged like everything else, but the frames themselves are host
		// memory the budget cannot see, so they are bounded structurally.
		CallStackSize: 200,
		RegistrySize:  1024,
	})
	v.L = L

	openLib(L, "", lua.OpenBase)
	openLib(L, lua.StringLibName, lua.OpenString)
	openLib(L, lua.TabLibName, lua.OpenTable)
	openLib(L, lua.MathLibName, lua.OpenMath)

	pruneGlobals(L, baseAllow, moduleNames)
	wrapModule(v, L, lua.StringLibName, stringAllow)
	wrapModule(v, L, lua.TabLibName, tableAllow)
	wrapModule(v, L, lua.MathLibName, mathAllow)
	wrapGlobals(v, L)

	// The charge functions. Their names are one control byte and one letter, so
	// Lua's lexer cannot produce an identifier that names them: a plugin can
	// neither call them nor shadow them, and with _G, getfenv and setfenv gone
	// it cannot reach them through the globals table either.
	L.SetGlobal(gasGlobal, L.NewFunction(v.gasFn))
	L.SetGlobal(catGlobal, L.NewFunction(v.catFn))
	return L
}

func openLib(L *lua.LState, name string, fn lua.LGFunction) {
	L.Push(L.NewFunction(fn))
	L.Push(lua.LString(name))
	L.Call(1, 0)
}

// moduleNames are the library tables that survive the prune. They are named
// separately from [baseAllow] so that the base *functions* stay a closed list:
// adding a library is a decision, not a line in the same map.
var moduleNames = map[string]bool{
	lua.StringLibName: true,
	lua.TabLibName:    true,
	lua.MathLibName:   true,
}

// pruneGlobals removes every global not on an allow-list, including _G itself.
func pruneGlobals(L *lua.LState, allow ...map[string]bool) {
	keep := func(name string) bool {
		for _, m := range allow {
			if m[name] {
				return true
			}
		}
		return false
	}
	g := L.Get(lua.GlobalsIndex).(*lua.LTable)
	var drop []string
	g.ForEach(func(k, _ lua.LValue) {
		name, ok := k.(lua.LString)
		if !ok {
			drop = append(drop, "")
			return
		}
		if !keep(string(name)) {
			drop = append(drop, string(name))
		}
	})
	for _, k := range drop {
		g.RawSetString(k, lua.LNil)
	}
}

// wrapModule prunes a module table and replaces every function left in it with a
// charged wrapper.
//
// The string library's module table is also the metatable of every string, so
// replacing entries in place — rather than substituting a new table — is what
// keeps `("x"):upper()` working and charged.
func wrapModule(v *vmState, L *lua.LState, name string, allow map[string]bool) {
	modv := L.GetGlobal(name)
	mod, ok := modv.(*lua.LTable)
	if !ok {
		return
	}
	pre := preflight[name]
	var drop []string
	type repl struct {
		k string
		f *lua.LFunction
	}
	var wrap []repl
	mod.ForEach(func(k, val lua.LValue) {
		key, ok := k.(lua.LString)
		if !ok {
			return
		}
		if !allow[string(key)] {
			drop = append(drop, string(key))
			return
		}
		if fn, ok := val.(*lua.LFunction); ok {
			wrap = append(wrap, repl{string(key), fn})
		}
	})
	for _, k := range drop {
		mod.RawSetString(k, lua.LNil)
	}
	for _, r := range wrap {
		mod.RawSetString(r.k, L.NewFunction(v.charged(r.f, pre[r.k])))
	}
}

// wrapGlobals charges the base functions that can produce more than they were
// given.
func wrapGlobals(v *vmState, L *lua.LState) {
	g := L.Get(lua.GlobalsIndex).(*lua.LTable)
	pre := preflight[""]
	for name, p := range pre {
		if fn, ok := g.RawGetString(name).(*lua.LFunction); ok {
			g.RawSetString(name, L.NewFunction(v.charged(fn, p)))
		}
	}
}

// charged wraps one builtin.
func (v *vmState) charged(orig *lua.LFunction, pre preFn) lua.LGFunction {
	return func(L *lua.LState) int {
		n := L.GetTop()
		v.gas(2)
		if pre != nil {
			pre(v, L, n)
		}
		base := L.GetTop()
		L.Push(orig)
		for i := 1; i <= n; i++ {
			L.Push(L.Get(i))
		}
		L.Call(n, lua.MultRet)

		top := L.GetTop()
		for i := base + 1; i <= top; i++ {
			if s, ok := L.Get(i).(lua.LString); ok {
				v.charge(int64(len(s)) + stringOverhead)
			} else {
				v.charge(valueOverheadLua)
			}
		}
		return top - base
	}
}

// ---------------------------------------------------------------------------
// the two charge functions the instrumented code calls

// gasFn is `<gas>(n)`: the instruction ceiling.
func (v *vmState) gasFn(L *lua.LState) int {
	n, _ := L.Get(1).(lua.LNumber)
	v.gas(int64(n))
	return 0
}

func (v *vmState) gas(n int64) {
	if v.noGas {
		return
	}
	v.gasLeft -= n
	if v.gasLeft < 0 {
		v.abort(ErrInstructionLimit)
	}
}

func (v *vmState) charge(n int64) {
	if v.noMem {
		return
	}
	v.memLeft -= n
	if v.memLeft < 0 {
		v.abort(ErrMemoryLimit)
	}
}

// remaining reports the memory budget left, for the pre-flights.
func (v *vmState) remaining() int64 {
	if v.noMem {
		return math.MaxInt64
	}
	return v.memLeft
}

// catFn is `<cat>(a, b)`: the memory ceiling's most important customer.
func (v *vmState) catFn(L *lua.LState) int {
	a, b := L.Get(1), L.Get(2)
	if lua.LVCanConvToString(a) && lua.LVCanConvToString(b) {
		sa, sb := lua.LVAsString(a), lua.LVAsString(b)
		v.gas(1)
		v.charge(int64(len(sa)+len(sb)) + stringOverhead)
		L.Push(lua.LString(sa + sb))
		return 1
	}
	mm := L.GetMetaField(a, "__concat")
	if mm == lua.LNil {
		mm = L.GetMetaField(b, "__concat")
	}
	if mm == lua.LNil {
		bad := a
		if lua.LVCanConvToString(a) {
			bad = b
		}
		L.RaiseError("attempt to concatenate a %s value", bad.Type().String())
		return 0
	}
	L.Push(mm)
	L.Push(a)
	L.Push(b)
	L.Call(2, 1)
	return 1
}

// abort ends the invocation with a ceiling error.
//
// The Go-side error is recorded before the Lua error is raised, because the
// message that comes back out of PCall is a string and the caller needs to know
// *which* ceiling was hit — the difference between a limit hit and a plugin bug
// is the difference between a counter and an investigation.
func (v *vmState) abort(err error) {
	if v.aborted == nil {
		v.aborted = err
	}
	v.L.RaiseError("dorang: %s", err.Error())
}

// ---------------------------------------------------------------------------
// pre-flights

// saturate multiplies without overflowing into a negative budget.
func saturate(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

func preRep(v *vmState, L *lua.LState, n int) {
	s := lua.LVAsString(L.Get(1))
	count := int64(lua.LVAsNumber(L.Get(2)))
	v.charge(saturate(int64(len(s)), count) + stringOverhead)
}

func preChar(v *vmState, L *lua.LState, n int) {
	v.charge(int64(n) + stringOverhead)
}

func preByte(v *vmState, L *lua.LState, n int) {
	s := lua.LVAsString(L.Get(1))
	i, j := int64(1), int64(1)
	if n >= 2 {
		i = int64(lua.LVAsNumber(L.Get(2)))
	}
	if n >= 3 {
		j = int64(lua.LVAsNumber(L.Get(3)))
	} else {
		j = i
	}
	if i < 0 {
		i = int64(len(s)) + i + 1
	}
	if j < 0 {
		j = int64(len(s)) + j + 1
	}
	if j > int64(len(s)) {
		j = int64(len(s))
	}
	if j >= i {
		v.charge(saturate(j-i+1, valueOverheadLua))
	}
}

func preUnpack(v *vmState, L *lua.LState, n int) {
	t, ok := L.Get(1).(*lua.LTable)
	if !ok {
		return
	}
	size := int64(t.Len())
	if n >= 3 {
		size = int64(lua.LVAsNumber(L.Get(3))) - int64(lua.LVAsNumber(L.Get(2))) + 1
	}
	v.charge(saturate(size, valueOverheadLua))
}

// preFormat bounds string.format, whose width field is the standard way to turn
// a short format string into a long result: "%0999999999d" is one argument and a
// gigabyte of output.
func preFormat(v *vmState, L *lua.LState, n int) {
	f := lua.LVAsString(L.Get(1))
	width := maxFormatWidth(f)
	if width > maxFormatField {
		L.RaiseError("dorang: string.format field width %d exceeds the sandbox limit of %d",
			width, maxFormatField)
	}
	cost := int64(len(f)) + stringOverhead
	for i := 2; i <= n; i++ {
		cost += int64(len(lua.LVAsString(L.Get(i)))) + int64(width) + stringOverhead
	}
	v.charge(cost)
}

// maxFormatField is the widest field a plugin may ask string.format for. Real
// Lua caps a format specification at about 99 characters for the same reason.
const maxFormatField = 4096

func maxFormatWidth(f string) int {
	max := 0
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			continue
		}
		i++
		n := 0
		digits := false
		for i < len(f) {
			c := f[i]
			if c >= '0' && c <= '9' {
				if n < math.MaxInt32/10 {
					n = n*10 + int(c-'0')
				}
				digits = true
				i++
				continue
			}
			if c == '-' || c == '+' || c == ' ' || c == '#' || c == '.' {
				if c == '.' {
					if digits && n > max {
						max = n
					}
					n, digits = 0, false
				}
				i++
				continue
			}
			break
		}
		if digits && n > max {
			max = n
		}
	}
	return max
}

func preGsub(v *vmState, L *lua.LState, n int) {
	s := lua.LVAsString(L.Get(1))
	repl := L.Get(3)
	per := int64(1)
	if lua.LVCanConvToString(repl) {
		per = int64(len(lua.LVAsString(repl))) + 1
	}
	v.charge(saturate(int64(len(s))+1, per) + stringOverhead)
}

func preTableConcat(v *vmState, L *lua.LState, n int) {
	t, ok := L.Get(1).(*lua.LTable)
	if !ok {
		return
	}
	sep := int64(len(lua.LVAsString(L.Get(2))))
	size := t.Len()
	// The walk itself is work, and work is charged: a table big enough to make
	// this expensive cost that much gas to build.
	v.gas(int64(size))
	var total int64
	for i := 1; i <= size; i++ {
		total += int64(len(lua.LVAsString(t.RawGetInt(i)))) + sep
		if total > v.remaining() {
			break
		}
	}
	v.charge(total + stringOverhead)
}

// trimForLog bounds what a plugin can put into the host's log through
// dorang.log. A plugin that logs a megabyte per request is a log-volume
// incident, not an extension.
func trimForLog(s string) string {
	const max = 512
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
