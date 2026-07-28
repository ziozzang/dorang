package luaext

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// operandLen is the size of the caller-supplied strings these tests hand a hook.
// 64 KiB is a plausible chat turn, not a stunt; DESIGN §10.5b's filter exists to
// look at exactly this.
const operandLen = 64 << 10

// TestOperatorsAreChargedByOperandLength is the hole this file closes.
//
// Comparison, ordering and string-keyed indexing are single VM instructions that
// walk every byte of a string the caller chose the length of. Charged once by
// the enclosing block's tick, a hook could spend its 5 M-instruction budget on a
// three hundred times the CPU that budget is meant to buy. Measured before the
// fix, with the wall clock pushed out of reach so that only the instruction
// ceiling could end the hook:
//
//	s == s2         753 ms        t[s]            3.16 s
//	s < s2        40.07 s         t[s] = 1        7.17 s
//	rawequal(s,s2)  803 ms        empty Lua loop   128 ms
//
// Each row below is one of those measurements kept in the suite. The loop count
// is chosen so that an unpriced operator finishes it comfortably inside the
// budget: a row that stops proves the charge is being made, and a row that
// completes proves it is not.
func TestOperatorsAreChargedByOperandLength(t *testing.T) {
	const hook = `dorang.register("on_request", function(req)
		local s, s2 = req.key_id, req.path
		local t = {}
		t[s] = 1
		SETUP
		for i = 1, ITERS do
			BODY
		end
		dorang.tag("done", "1")
	end)`

	cases := []struct {
		name  string
		setup string
		body  string
		iters int
	}{
		// The three VM operators. Nothing here is a call, which is why the fix
		// had to be a rewrite and not another entry in the pre-flight table.
		{"equality/64 KiB", "", `if s == s2 then t.hit = 1 end`, 20000},
		{"inequality/64 KiB", "", `if s ~= s2 then t.hit = 1 end`, 20000},
		{"ordering/64 KiB", "", `if s < s2 then t.hit = 1 end`, 2000},
		{"ordering le/64 KiB", "", `if s <= s2 then t.hit = 1 end`, 2000},
		{"index read/64 KiB", "", `local x = t[s2]`, 20000},
		{"index write/64 KiB", "", `t[s2] = i`, 20000},
		{"table constructor key/64 KiB", "", `local u = { [s2] = 1 }`, 20000},

		// The spelled-out twins. Pricing these while the operators stayed open
		// would have bought nothing, which is why neither was done alone.
		{"rawequal/64 KiB", "", `if rawequal(s, s2) then t.hit = 1 end`, 20000},
		{"rawget/64 KiB", "", `local x = rawget(t, s2)`, 20000},
		{"rawset/64 KiB", "", `rawset(t, s2, i)`, 20000},
		{"next/64 KiB", "", `local k, v = next(t, s)`, 20000},

		// Two more reachable by a different spelling of the same work: walking a
		// table hashes every key it visits, and sorting without a comparator
		// runs gopher-lua's byte-at-a-time strCmp n·log n times.
		{"pairs over long keys", "", `for k, v in pairs(t) do end`, 20000},
		{"table.sort default comparator",
			`local a = {} for j = 1, 8 do a[j] = req.key_id end`,
			`table.sort(a)`, 200},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(hook, "SETUP", tc.setup, 1)
			src = strings.Replace(src, "BODY", tc.body, 1)
			src = strings.Replace(src, "ITERS", itoa(tc.iters), 1)
			e := luaEngine(t, src, patternLimits)

			// Two distinct allocations with the same bytes. A single string
			// compared against itself takes Go's pointer fast path and would
			// measure nothing.
			v := &RequestView{
				KeyID: strings.Repeat("a", operandLen),
				Path:  strings.Repeat("a", operandLen),
			}

			start := time.Now()
			d := e.OnRequest(context.Background(), v)
			elapsed := time.Since(start)

			if d.Denied {
				t.Fatal("a ceiling breach denied the request; ceilings fail open")
			}
			if _, completed := d.Tag("done"); completed {
				t.Fatalf("the hook ran %d of these against a %d-byte operand inside its "+
					"budget (elapsed %v): the operation is charged O(1) and costs O(length)",
					tc.iters, operandLen, elapsed)
			}
			if st := e.Stats(); st.LimitHits == 0 {
				t.Fatalf("the hook stopped without the instruction ceiling firing: %+v", st)
			}
			// Not a benchmark: a floor under "bounded". The worst pre-fix row
			// was past a minute and the cheapest was 1.7 s, so anything still
			// unpriced fails here even on a slow machine.
			if elapsed > time.Second {
				t.Fatalf("a stopped hook still took %v", elapsed)
			}
		})
	}
}

// TestOrdinaryComparisonsAndIndexingStillRun is the other half, and the half a
// blunt fix fails.
//
// It passes with the charge and without it, and its whole job is to fail a fix
// that refuses everything. Charging by operand length is only worth having if
// the plugins people actually write — field comparisons against literals, table
// lookups by short keys, loop counters — keep running inside the same budget
// they had before.
func TestOrdinaryComparisonsAndIndexingStillRun(t *testing.T) {
	e := luaEngine(t, `
		local blocked = { ["gpt-4"] = true, ["o1-preview"] = true }
		local tiers = { eng = "premium", data = "bulk", ops = "bulk" }
		dorang.register("on_request", function(req)
			local out = ""
			-- Comparison against a literal, in both orders.
			if req.model == "gpt-4" and "gpt-4" == req.model then out = out .. "exact," end
			if req.method ~= "GET" and req.body_bytes > 0 then out = out .. "write," end

			-- Lookups: constant key, dynamic short key.
			if blocked[req.model] then out = out .. "blocked," end
			out = out .. "tier=" .. tostring(tiers[req.team_id]) .. ","

			-- A loop with a dynamic bound and both kinds of index, over a table
			-- the plugin built itself and then sorted.
			local names, seen, n = {}, {}, 0
			for w in req.path:gmatch("[%w%.%-]+") do
				n = n + 1
				names[n] = w
				seen[w] = (seen[w] or 0) + 1
			end
			local i, dups = 1, 0
			while i <= n do
				if seen[names[i]] > 1 then dups = dups + 1 end
				i = i + 1
			end
			table.sort(names)
			out = out .. "parts=" .. tostring(n) .. ",dups=" .. tostring(dups) ..
				",first=" .. names[1] .. ","

			-- Comparing two long strings that a plugin would really compare:
			-- the request text against itself, once, is affordable.
			if req.key_id == req.user_id then out = out .. "same," end
			dorang.tag("out", out)
		end)`, patternLimits)

	const want = "exact,write,blocked,tier=bulk,parts=5,dups=4,first=chat,same,"
	for _, n := range []int{0, 16, 1024} {
		long := strings.Repeat("x", n)
		v := &RequestView{
			Model:     "gpt-4",
			TeamID:    "data",
			Method:    "POST",
			BodyBytes: 4096,
			Path:      "/v1/chat/completions/v1/chat",
			KeyID:     long,
			UserID:    long,
		}
		d := e.OnRequest(context.Background(), v)
		if d.Denied {
			t.Fatalf("n=%d: an ordinary hook denied the request", n)
		}
		if got, _ := d.Tag("out"); got != want {
			t.Fatalf("n=%d:\n got %q\nwant %q", n, got, want)
		}
	}
	if st := e.Stats(); st.LimitHits != 0 || st.Skipped != 0 {
		t.Fatalf("ordinary comparisons tripped a ceiling: %+v", st)
	}
}

// TestChargedOperatorsStillProduceLuaResults guards the thing an operand wrapper
// could quietly break: the answers. The wrapper must be transparent — one value
// in, the same value out — or every comparison and every lookup in every plugin
// is now against something else.
func TestChargedOperatorsStillProduceLuaResults(t *testing.T) {
	e := luaEngine(t, `
		local function join(...)
			local out = ""
			for i = 1, select("#", ...) do out = out .. tostring((select(i, ...))) .. "|" end
			return out
		end
		dorang.register("on_request", function(req)
			local a, b, m = req.key_id, req.path, req.model
			local t = { [a] = "va", [10] = "v10", x = "vx" }
			local k = "x"
			-- multiple returns truncate to one in a key and in a comparison,
			-- exactly as they do without the wrapper
			local function two() return "x", "ignored" end
			dorang.tag("t", join(
				a == b, a ~= b, a < b, a <= b, a > b, a >= b,
				m == "gpt-4", 3 < 10, "a" < "b",
				t[a], t[10], t[k], t[two()], t[m],
				#a, a == a))
		end)`, patternLimits)

	d := e.OnRequest(context.Background(), &RequestView{
		KeyID: "aaa", Path: "bbb", Model: "gpt-4",
	})
	got, _ := d.Tag("t")
	const want = "false|true|true|true|false|false|" +
		"true|true|true|" +
		"va|v10|vx|vx|nil|" +
		"3|true|"
	if got != want {
		t.Fatalf("operator results changed:\n got %q\nwant %q", got, want)
	}
}

// TestChargedKeysCompileEverywhereAKeyCanAppear is the risk specific to
// charging the key rather than the whole index expression: the wrapper turns a
// key into a call, and a key appears in places a call is not obviously legal —
// on the left of an assignment, in a multiple assignment, in a table
// constructor, as a method-call receiver, and swallowing a vararg. A shape that
// failed here would fail at plugin load, in an operator's deployment.
func TestChargedKeysCompileEverywhereAKeyCanAppear(t *testing.T) {
	e := luaEngine(t, `
		local function keys(...) return ... end
		dorang.register("on_request", function(req)
			local a, b = req.model, req.key_id
			local t, u = {}, {}

			t[a] = "one"                       -- assignment target
			t[a], u[b] = "two", "three"        -- multiple assignment target
			local v = { [a] = "four", [b .. "!"] = "five" }
			t[keys(b)] = "six"                 -- a vararg collapses to one key
			local w = t[keys(a, b)]            -- and so does a multiple return
			u[b] = u[b] .. "!"                 -- read and write in one statement
			local m = ({ [a] = "seven" })[a]:upper()
			local nested = t[v[a] ~= "four" and a or b]

			local n = 0
			repeat n = n + 1 until t[a] ~= nil or n > 2

			dorang.tag("out", w .. "/" .. u[b] .. "/" .. v[b .. "!"] .. "/" ..
				m .. "/" .. tostring(nested) .. "/" .. tostring(n))
		end)`, patternLimits)

	d := e.OnRequest(context.Background(), &RequestView{Model: "mm", KeyID: "kk"})
	if d.Denied {
		t.Fatal("the syntax hook denied the request")
	}
	got, _ := d.Tag("out")
	const want = "two/three!/five/SEVEN/six/1"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	if st := e.Stats(); st.Skipped != 0 {
		t.Fatalf("the syntax hook did not complete: %+v", st)
	}
}

// TestChargedOperatorsCarryTheMetamethods is the risk an operand wrapper was
// chosen to avoid, asserted rather than assumed.
//
// Replacing `==`, `<`, `<=` and `[]` with host calls would have meant
// reimplementing four metamethod dispatches — and gopher-lua exports no entry
// point for `<=` at all, so one of them would have been a guess. Charging an
// operand leaves every dispatch in the VM. This is what says so, including the
// Lua 5.1 corner where `a <= b` falls back to `not (b < a)`.
func TestChargedOperatorsCarryTheMetamethods(t *testing.T) {
	e := luaEngine(t, `
		dorang.register("on_request", function(req)
			local calls = {}
			local mt = {}
			mt.__eq    = function(x, y) calls[#calls+1] = "eq"  return x.v == y.v end
			mt.__lt    = function(x, y) calls[#calls+1] = "lt"  return x.v < y.v end
			mt.__index = function(x, k) calls[#calls+1] = "idx" return "via:" .. tostring(k) end
			mt.__newindex = function(x, k, val) calls[#calls+1] = "new" rawset(x, "seen", k) end

			local a = setmetatable({ v = 1 }, mt)
			local b = setmetatable({ v = 1 }, mt)
			local c = setmetatable({ v = 2 }, mt)
			local key = req.key_id

			local eq  = (a == b)
			local lt  = (a < c)
			-- No __le on the metatable, so 5.1 falls back to not (c < a).
			local le  = (a <= c)
			local got = a[key]
			a[key] = "written"

			dorang.tag("out", tostring(eq) .. "|" .. tostring(lt) .. "|" .. tostring(le) ..
				"|" .. tostring(got) .. "|" .. tostring(rawget(a, "seen")))
			dorang.tag("calls", table.concat(calls, ","))
		end)`, patternLimits)

	d := e.OnRequest(context.Background(), &RequestView{KeyID: "k"})
	if d.Denied {
		t.Fatal("the metamethod hook denied the request")
	}
	for _, want := range []struct{ k, v string }{
		{"out", "true|true|true|via:k|k"},
		{"calls", "eq,lt,lt,idx,new"},
	} {
		if got, _ := d.Tag(want.k); got != want.v {
			t.Fatalf("tag %q = %q, want %q (tags %+v)", want.k, got, want.v, d.Tags())
		}
	}
	if st := e.Stats(); st.Skipped != 0 || st.LimitHits != 0 {
		t.Fatalf("the metamethod hook did not complete cleanly: %+v", st)
	}
}

// TestMetamethodComparisonIsChargedByItsOwnBody: a plugin can move the work into
// a metamethod, and that is fine — a metamethod is Lua, so its body is
// instrumented and the comparison inside it is charged like any other. This
// asserts the loop stops rather than trusting that it does.
func TestMetamethodComparisonIsChargedByItsOwnBody(t *testing.T) {
	e := luaEngine(t, `
		dorang.register("on_request", function(req)
			local mt = { __lt = function(x, y) return x.s < y.s end }
			local a = setmetatable({ s = req.key_id }, mt)
			local b = setmetatable({ s = req.path }, mt)
			for i = 1, 20000 do
				if a < b then break end
			end
			dorang.tag("done", "1")
		end)`, patternLimits)

	start := time.Now()
	d := e.OnRequest(context.Background(), &RequestView{
		KeyID: strings.Repeat("a", operandLen),
		Path:  strings.Repeat("a", operandLen),
	})
	elapsed := time.Since(start)

	if _, completed := d.Tag("done"); completed {
		t.Fatalf("a comparison hidden in __lt ran %d times against %d-byte operands (elapsed %v)",
			20000, operandLen, elapsed)
	}
	if st := e.Stats(); st.LimitHits == 0 {
		t.Fatalf("the hook stopped without the instruction ceiling firing: %+v", st)
	}
	if elapsed > time.Second {
		t.Fatalf("a stopped hook still took %v", elapsed)
	}
}

// TestAnOperatorLoopExitsWhenItsDeadlinePasses bounds what the charge does not
// reach.
//
// One VM instruction can probe up to gopher-lua's MaxTableGetLoop of a hundred
// tables, because `t[k]` walks an `__index` chain the plugin built. That is an
// O(1)-per-charge operation with a constant of a hundred, not an O(caller's
// length) one — no string a caller sends changes the depth — but a hook that
// spends its whole budget on chained lookups still measured 3.6 s against a
// 130 ms baseline, so the bound has to come from somewhere.
//
// It comes from the wall clock, and this is the difference between an operator
// and a builtin: gopher-lua checks the context *between instructions*, so a loop
// of them is interruptible where one `string.find` was not. The hook is stopped
// at its deadline and the goroutine notices within one instruction, which is
// what this asserts — the request returns on time and the goroutine goes away
// rather than pinning a core after it.
//
// It used to assert one *timeout* here. It no longer is one: a hook that notices
// its cancellation finishes inside [abandonGrace] and is never abandoned, so
// there is no leaked goroutine to count and nothing for the trip to switch off.
// That is the point of the loop being interruptible, and counting it as an
// abandonment counted a leak that did not happen.
func TestAnOperatorLoopExitsWhenItsDeadlinePasses(t *testing.T) {
	e := luaEngine(t, `
		dorang.register("on_request", function(req)
			local cur = { hit = 1 }
			for j = 1, 90 do cur = setmetatable({ z = 1 }, { __index = cur }) end
			local k = req.model
			while true do
				local x = cur[k]
			end
		end)`, func(o *Options) {
		o.Limits = Limits{Instructions: 5_000_000, MemoryBytes: 32 << 20, Timeout: 50 * time.Millisecond}
	})

	start := time.Now()
	d := e.OnRequest(context.Background(), &RequestView{Model: "gpt-4o"})
	elapsed := time.Since(start)

	if d.Denied {
		t.Fatal("a timed-out hook denied the request; ceilings fail open")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("the request waited %v for a hook with a 50 ms ceiling", elapsed)
	}
	if st := e.Stats(); st.Timeouts != 0 || st.Skipped != 1 {
		t.Fatalf("stats = %+v, want one skipped invocation and no abandonment: the loop is "+
			"interruptible, so it ended itself", st)
	}

	// The goroutine is never killed. What must be true is that it notices: a
	// hook still spinning after its request is gone is the cost this bound
	// exists to cap.
	deadline := time.Now().Add(5 * time.Second)
	for e.Abandoned(HookRequest) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the abandoned goroutine was still running 5 s after its 50 ms deadline: " +
				"a loop of VM operators is supposed to see the cancelled context between instructions")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestFilterPluginPaysNothingForTheCharge is the cost side of the trade, kept as
// a property rather than only as a benchmark number.
//
// The rewrite skips any site where one operand's cost is already fixed at load,
// and the claim that this covers real plugins is only worth as much as the
// plugins it was checked against. The shipped masking filter — the hook that
// actually runs on request text — comes out with no charged site at all, so it
// pays nothing for a bound that protects it.
func TestFilterPluginPaysNothingForTheCharge(t *testing.T) {
	src, err := readShippedFilter()
	if err != nil {
		t.Fatalf("the shipped filter is the subject of this test: %v", err)
	}
	if chargesAnOperand(t, src) {
		t.Fatal("the shipped masking filter now has a charged site; it had none. " +
			"Either the plugin changed or the rewrite got broader — both are worth knowing")
	}

	// A plugin that does compare by a caller's string gets one, so the check
	// above is measuring something rather than always passing.
	const compares = `dorang.register("on_request", function(req)
		if req.key_id == req.user_id then dorang.tag("same", "1") end
	end)`
	if !chargesAnOperand(t, []byte(compares)) {
		t.Fatal("a hook comparing two dynamic strings has no charged site")
	}
}

// readShippedFilter reads deploy/plugins/pii_mask.lua — the file an operator
// installs, rather than a copy of it that could drift.
func readShippedFilter() ([]byte, error) {
	return os.ReadFile(filepath.Join("..", "..", "deploy", "plugins", "pii_mask.lua"))
}

// chargesAnOperand reports whether the instrumented plugin reaches the charge
// function anywhere.
//
// It asks the compiled prototypes rather than the syntax tree: the charge is a
// call to a global, so its name is a string constant in every function that
// contains one, and nothing else in the sandbox can produce that name.
func chargesAnOperand(t *testing.T, src []byte) bool {
	t.Helper()
	proto, err := compileLua("probe", "probe.lua", src)
	if err != nil {
		t.Fatalf("compiling the plugin: %v", err)
	}
	var walk func(*lua.FunctionProto) bool
	walk = func(p *lua.FunctionProto) bool {
		for _, k := range p.Constants {
			if s, ok := k.(lua.LString); ok && string(s) == sizeGlobal {
				return true
			}
		}
		for _, sub := range p.FunctionPrototypes {
			if walk(sub) {
				return true
			}
		}
		return false
	}
	return walk(proto)
}
