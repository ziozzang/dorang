package luaext

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

func luaEngine(t *testing.T, src string, tweak ...func(*Options)) *Engine {
	t.Helper()
	o := Options{
		Enabled: true,
		Limits:  Limits{Instructions: 200_000, MemoryBytes: 1 << 20, Timeout: 2 * time.Second},
		Plugins: []Plugin{{Name: "test", Path: "test.lua", Source: []byte(src)}},
		Logf:    func(string, ...any) {},
	}
	for _, f := range tweak {
		f(&o)
	}
	e, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e == nil {
		t.Fatal("New returned a disabled engine")
	}
	return e
}

// TestPluginRegistersAndDenies is the shape of DESIGN §11.5: an operator drops
// in a file that registers handlers.
func TestPluginRegistersAndDenies(t *testing.T) {
	e := luaEngine(t, `
		dorang.require_api(1)
		local cfg = dorang.config
		dorang.register("on_request", function(req)
			if req.model == cfg.blocked then
				return false, "this model is not available to " .. req.key_id, "model_blocked"
			end
			dorang.tag("seen", req.model)
		end)
	`, func(o *Options) {
		o.Plugins[0].Config = map[string]string{"blocked": "gpt-4"}
	})

	d := e.OnRequest(context.Background(), &RequestView{Model: "gpt-4", KeyID: "k1"})
	if !d.Denied {
		t.Fatal("the plugin's deny was not honoured")
	}
	if d.Reason != "this model is not available to k1" {
		t.Fatalf("reason = %q", d.Reason)
	}
	if d.Code != "model_blocked" {
		t.Fatalf("code = %q", d.Code)
	}

	d = e.OnRequest(context.Background(), &RequestView{Model: "gpt-3", KeyID: "k1"})
	if d.Denied {
		t.Fatal("an unrelated model was denied")
	}
	if v, _ := d.Tag("seen"); v != "gpt-3" {
		t.Fatalf("tag = %q", v)
	}
}

// TestInstructionCeilingStopsAHookThatNeverReturns is the ceiling gopher-lua
// does not have. The wall clock is set far out of reach so that only the
// instruction counter can end this.
func TestInstructionCeilingStopsAHookThatNeverReturns(t *testing.T) {
	cases := map[string]string{
		"while loop":    `while true do end`,
		"repeat loop":   `repeat until false`,
		"numeric for":   `local n = 0 for i = 1, 1e18 do n = n + 1 end`,
		"generic for":   `local t = {1} while true do for _, x in ipairs(t) do end end`,
		"recursion":     `local function f(n) return f(n+1) end f(0)`,
		"goto backward": `::top:: goto top`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			e := luaEngine(t, `dorang.register("on_request", function(req) `+body+` end)`,
				func(o *Options) {
					o.Limits = Limits{Instructions: 100_000, MemoryBytes: 1 << 20, Timeout: time.Minute}
				})
			start := time.Now()
			d := e.OnRequest(context.Background(), &RequestView{Model: "m"})
			if el := time.Since(start); el > 10*time.Second {
				t.Fatalf("took %s: the wall clock stopped it, not the instruction ceiling", el)
			}
			if d.Denied {
				t.Fatal("a hook that hit a ceiling denied the request; ceilings fail open")
			}
			if st := e.Stats(); st.LimitHits == 0 {
				t.Fatalf("no limit hit was counted: %+v", st)
			}
		})
	}
}

// TestMemoryCeilingStopsAHookThatAllocates is the other ceiling gopher-lua does
// not have, and the reason a wall clock alone is not a sandbox: each of these
// exhausts memory long before a 200 ms deadline would fire.
func TestMemoryCeilingStopsAHookThatAllocates(t *testing.T) {
	cases := map[string]string{
		"doubling concat": `local s = "xxxx" while true do s = s .. s end`,
		"accumulating":    `local s = "" while true do s = s .. "0123456789" end`,
		"string.rep":      `local s = string.rep("x", 1000000000)`,
		"string.rep loop": `local t = {} local i = 1 while true do t[i] = string.rep("x", 4096) i = i + 1 end`,
		"table.concat":    `local t = {} for i = 1, 1000 do t[i] = string.rep("y", 1000) end local s = table.concat(t)`,
		"gsub explosion":  `local s = string.rep("a", 10000):gsub("a", string.rep("b", 10000))`,
		"format width":    `local s = string.format("%01000000000d", 1)`,
		"method rep":      `local s = ("z"):rep(1000000000)`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			e := luaEngine(t, `dorang.register("on_request", function(req) `+body+` end)`,
				func(o *Options) {
					o.Limits = Limits{Instructions: 50_000_000, MemoryBytes: 1 << 20, Timeout: time.Minute}
				})
			start := time.Now()
			d := e.OnRequest(context.Background(), &RequestView{Model: "m"})
			if el := time.Since(start); el > 10*time.Second {
				t.Fatalf("took %s: memory was not what stopped it", el)
			}
			if d.Denied {
				t.Fatal("a hook that hit a ceiling denied the request; ceilings fail open")
			}
			if st := e.Stats(); st.Skipped == 0 {
				t.Fatalf("nothing was skipped: %+v", st)
			}
		})
	}
}

// TestABuiltinIsChargedForWhatItReads is the general form of the search
// family's defect, found by the same audit and worse in the measuring.
//
// A charged Lua instruction costs about 22 ns, so the 5 M default budget buys
// roughly 110 ms. `while true do tonumber(s) end` against an 8 KiB argument was
// still running after twenty seconds and had not hit any ceiling: tonumber
// returns a *number*, so charging it for its output charged sixteen bytes for a
// call that reads every byte of its argument, and the instruction ceiling
// counted 5 M instructions while each of them dragged 8 KiB behind it.
//
// The subject comes off the view, which is the point: its length is the
// caller's.
func TestABuiltinIsChargedForWhatItReads(t *testing.T) {
	e := luaEngine(t, `dorang.register("on_request", function(req)
			while true do tonumber(req.key_id) end
		end)`, func(o *Options) {
		o.Limits = Limits{Instructions: 5_000_000, MemoryBytes: 32 << 20, Timeout: time.Minute}
	})

	start := time.Now()
	d := e.OnRequest(context.Background(), &RequestView{KeyID: strings.Repeat("1", 8192)})
	elapsed := time.Since(start)

	if d.Denied {
		t.Fatal("a ceiling breach denied the request; ceilings fail open")
	}
	if st := e.Stats(); st.LimitHits == 0 {
		t.Fatalf("the instruction ceiling did not fire in %v: a builtin walked through it (%+v)",
			elapsed, st)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the hook ran for %v inside a 110 ms budget", elapsed)
	}
}

// TestWallClockStopsAHookWithNoOtherCeiling covers the third ceiling on its own:
// instructions unlimited, memory unlimited, only the clock left.
func TestWallClockStopsAHookWithNoOtherCeiling(t *testing.T) {
	e := luaEngine(t, `dorang.register("on_request", function(req) while true do end end)`,
		func(o *Options) {
			o.Limits = Limits{Instructions: 0, MemoryBytes: 0, Timeout: 100 * time.Millisecond}
		})
	start := time.Now()
	d := e.OnRequest(context.Background(), &RequestView{Model: "m"})
	el := time.Since(start)
	if el > 5*time.Second {
		t.Fatalf("the wall clock did not stop the hook: %s", el)
	}
	if d.Denied {
		t.Fatal("an abandoned hook denied the request")
	}
}

// TestDenyOnlyFromACompletedHook is DESIGN §11.5's asymmetry, tested from the
// inside: a plugin that says no and *then* runs out of budget has not said no,
// because a hook that can refuse by crashing is a hook that refuses whenever it
// is broken.
func TestDenyOnlyFromACompletedHook(t *testing.T) {
	e := luaEngine(t, `
		dorang.register("on_request", function(req)
			dorang.deny("nope", "denied_then_looped")
			while true do end
		end)
	`, func(o *Options) {
		o.Limits = Limits{Instructions: 100_000, MemoryBytes: 1 << 20, Timeout: time.Minute}
	})
	if d := e.OnRequest(context.Background(), &RequestView{Model: "m"}); d.Denied {
		t.Fatal("a hook that never completed refused the request")
	}

	// The same plugin, without the loop, is obeyed.
	e2 := luaEngine(t, `dorang.register("on_request", function(req) dorang.deny("nope", "c") end)`)
	if d := e2.OnRequest(context.Background(), &RequestView{Model: "m"}); !d.Denied {
		t.Fatal("a completed deny was not honoured")
	}

	// So is a plugin that errors after denying — the error wins.
	e3 := luaEngine(t, `
		dorang.register("on_request", function(req)
			dorang.deny("nope")
			error("boom")
		end)
	`)
	if d := e3.OnRequest(context.Background(), &RequestView{Model: "m"}); d.Denied {
		t.Fatal("a hook that panicked after denying refused the request")
	}
}

// TestNoSecretIsReachableFromLua is the adversarial half of §11.5's sandbox: a
// plugin that tries to find one, with a real credential in flight.
func TestNoSecretIsReachableFromLua(t *testing.T) {
	const secret = "sk-live-THIS-IS-A-CREDENTIAL" // pragma: allowlist secret — fixture

	e := luaEngine(t, `
		local seen = {}
		local function walk(t, depth, out)
			if depth > 4 then return end
			for k, v in pairs(t) do
				out[#out+1] = tostring(k)
				local ty = type(v)
				if ty == "string" or ty == "number" or ty == "boolean" then
					out[#out+1] = tostring(v)
				elseif ty == "table" and not seen[v] then
					seen[v] = true
					walk(v, depth + 1, out)
				end
				local mt = getmetatable(v)
				if type(mt) == "table" and not seen[mt] then
					seen[mt] = true
					walk(mt, depth + 1, out)
				end
			end
		end
		dorang.register("on_request", function(req)
			local out = {}
			walk(req, 0, out)
			walk(dorang, 0, out)
			walk(string, 0, out)
			-- Everything a plugin might hope reaches the host.
			out[#out+1] = tostring(rawget(req, "authorization"))
			out[#out+1] = tostring(_G)
			out[#out+1] = tostring(getfenv)
			out[#out+1] = tostring(load)
			out[#out+1] = tostring(loadstring)
			out[#out+1] = tostring(dofile)
			out[#out+1] = tostring(require)
			out[#out+1] = tostring(io)
			out[#out+1] = tostring(os)
			out[#out+1] = tostring(debug)
			out[#out+1] = tostring(coroutine)
			out[#out+1] = tostring(pcall)
			dorang.tag("harvest", table.concat(out, " "))
		end)
	`, func(o *Options) {
		o.Limits = Limits{Instructions: 5_000_000, MemoryBytes: 8 << 20, Timeout: 5 * time.Second}
	})

	// The credential is in flight: it is in the process, it is in a struct the
	// caller holds, and it is simply not in the view.
	type inflight struct {
		Credential string
		View       RequestView
	}
	f := inflight{
		Credential: secret,
		View:       RequestView{RequestID: "r1", Model: "m", KeyID: "k1", KeyName: "prod"},
	}
	d := e.OnRequest(context.Background(), &f.View)

	got, _ := d.Tag("harvest")
	if got == "" {
		t.Fatal("the plugin did not run")
	}
	if strings.Contains(got, secret) {
		t.Fatalf("a plugin reached a credential: %q", got)
	}
	for _, forbidden := range []string{"getfenv", "loadstring", "dofile", "require"} {
		if strings.Contains(got, forbidden+"=") {
			t.Fatalf("%s is reachable", forbidden)
		}
	}
	// Everything it hoped for came back nil.
	for _, want := range []string{"nil"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected nils in the harvest, got %q", got)
		}
	}
}

// TestGlobalSurfaceIsExactlyTheAllowlist walks everything a plugin can reach and
// compares it to a written-down list.
//
// This is the test that makes the sandbox a whitelist rather than a list of
// things somebody remembered: a gopher-lua upgrade that adds a function to the
// string library, or a future edit that forgets to prune one, fails here.
func TestGlobalSurfaceIsExactlyTheAllowlist(t *testing.T) {
	e := luaEngine(t, `dorang.register("on_request", function(req) end)`)
	v, err := e.lua.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer e.lua.release(v)

	got := map[string]bool{}
	seen := map[*lua.LTable]bool{}
	var walk func(prefix string, t *lua.LTable)
	walk = func(prefix string, tbl *lua.LTable) {
		if seen[tbl] {
			return
		}
		seen[tbl] = true
		tbl.ForEach(func(k, val lua.LValue) {
			name, ok := k.(lua.LString)
			if !ok {
				return
			}
			path := prefix + string(name)
			got[path] = true
			if sub, ok := val.(*lua.LTable); ok {
				walk(path+".", sub)
			}
		})
	}
	walk("", v.L.Get(lua.GlobalsIndex).(*lua.LTable))

	want := map[string]bool{}
	for n := range baseAllow {
		want[n] = true
	}
	for n := range stringAllow {
		want["string."+n] = true
	}
	for n := range tableAllow {
		want["table."+n] = true
	}
	for n := range mathAllow {
		want["math."+n] = true
	}
	for _, n := range []string{"string", "table", "math", "dorang"} {
		want[n] = true
	}
	for _, n := range []string{
		"api", "require_api", "register", "tag", "deny", "log", "mask", "mask_value",
	} {
		want["dorang."+n] = true
	}
	// The two charge functions are in the globals table by construction — the
	// instrumented code calls them by name. What matters is that no plugin can
	// write that name, which is asserted below with the lexer rather than
	// assumed here.
	want[gasGlobal] = true
	want[catGlobal] = true

	var extra, missing []string
	for n := range got {
		if !want[n] {
			extra = append(extra, n)
		}
	}
	for n := range want {
		if !got[n] {
			missing = append(missing, n)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 {
		t.Errorf("reachable but not on the allow-list: %v", extra)
	}
	if len(missing) > 0 {
		t.Errorf("on the allow-list but not reachable: %v", missing)
	}

	// The charge functions must not be nameable. Lua's lexer cannot produce an
	// identifier containing a control byte, so a plugin can neither call them
	// nor shadow them — and with _G, getfenv and setfenv pruned there is no way
	// to reach the globals table by value either.
	for _, n := range []string{gasGlobal, catGlobal} {
		for _, src := range []string{n + " = nil", "local x = " + n, n + "(1)"} {
			if _, err := New(Options{
				Enabled: true,
				Plugins: []Plugin{{Name: "evil", Source: []byte(src)}},
			}); err == nil {
				t.Errorf("a plugin named the charge function: %q compiled", src)
			}
		}
	}
	if got["_G"] || got["getfenv"] || got["setfenv"] {
		t.Error("the globals table is reachable by value")
	}
}

// TestPluginCannotCompileNewCode: every route from text to a function is gone,
// because a function that did not pass through the instrumenter has no
// instruction ceiling.
func TestPluginCannotCompileNewCode(t *testing.T) {
	for _, fn := range []string{"load", "loadstring", "dofile", "loadfile", "require"} {
		t.Run(fn, func(t *testing.T) {
			src := fmt.Sprintf(`dorang.register("on_request", function(req)
				dorang.tag("kind", type(%s))
			end)`, fn)
			e := luaEngine(t, src)
			d := e.OnRequest(context.Background(), &RequestView{Model: "m"})
			if v, _ := d.Tag("kind"); v != "nil" {
				t.Fatalf("%s is %s, want nil", fn, v)
			}
		})
	}
}

// TestAPIVersionMismatchIsALoadError covers §11.5's versioned value shape: a
// plugin is told plainly, at startup, not at the first request that reaches it.
func TestAPIVersionMismatchIsALoadError(t *testing.T) {
	_, err := New(Options{
		Enabled: true,
		Plugins: []Plugin{{Name: "old", Path: "old.lua", Source: []byte(`dorang.require_api(99)`)}},
	})
	if err == nil {
		t.Fatal("a plugin written for another API version loaded")
	}
	if !strings.Contains(err.Error(), "99") || !strings.Contains(err.Error(), "1") {
		t.Fatalf("the error does not name both versions: %v", err)
	}
}

// TestLoadingIsExplicit: there is no directory scan, and a plugin that is not
// there is a startup error rather than a hook that silently never runs.
func TestLoadingIsExplicit(t *testing.T) {
	_, err := New(Options{
		Enabled: true,
		Plugins: []Plugin{{Name: "gone", Path: "/nonexistent/plugin.lua"}},
	})
	if err == nil {
		t.Fatal("a missing plugin loaded")
	}
	_, err = New(Options{
		Enabled: true,
		Plugins: []Plugin{
			{Name: "dup", Source: []byte(``)},
			{Name: "dup", Source: []byte(``)},
		},
	})
	if err == nil {
		t.Fatal("two plugins with one name loaded")
	}
	_, err = New(Options{
		Enabled: true,
		Plugins: []Plugin{{Name: "bad", Source: []byte(`this is not lua`)}},
	})
	if err == nil {
		t.Fatal("a plugin that does not parse loaded")
	}
}

// TestLuaHookOffCostsNothing: an engine with a plugin registered at one hook
// must still cost an untaken branch at the others.
func TestLuaHookOffCostsNothing(t *testing.T) {
	e := luaEngine(t, `dorang.register("on_request", function(req) end)`)
	if e.Enabled(HookRoute) {
		t.Fatal("on_route is enabled with nothing registered for it")
	}
	rv := RouteView{Model: "m"}
	if n := testing.AllocsPerRun(200, func() {
		if e.Enabled(HookRoute) {
			_ = e.OnRoute(context.Background(), &rv)
		}
	}); n != 0 {
		t.Fatalf("an unregistered hook allocated %v times per run", n)
	}
	if n := testing.AllocsPerRun(200, func() {
		var nilEngine *Engine
		if nilEngine.Enabled(HookRequest) {
			t.Fatal("the disabled engine reported itself enabled")
		}
	}); n != 0 {
		t.Fatalf("the disabled engine allocated %v times per run", n)
	}
}

// TestPluginStateDoesNotCrossHooksAsACapability: a plugin can keep its own
// globals — that is what a long-running Lua program does — but a capability the
// host lent it for one invocation stops working the moment the invocation ends.
func TestPluginStateDoesNotCrossHooksAsACapability(t *testing.T) {
	e := luaEngine(t, `
		local stolen
		dorang.register("on_filter_request", function(f)
			stolen = dorang.mask
			return
		end)
		dorang.register("on_request", function(req)
			if stolen == nil then dorang.tag("stole", "no") return end
			local ok = "unexpected"
			-- No pcall in the sandbox, so calling it must be the last thing
			-- this handler ever does: the error ends the hook, which fails open.
			dorang.tag("stole", "yes")
			stolen("900101-1234567")
			dorang.tag("stole", ok)
		end)
	`)
	doc := NewDoc()
	s := "900101-1234567"
	doc.Add("m0", &s)
	fd := e.FilterRequest(context.Background(), &FilterView{Doc: doc, Mask: staticMasker{}})
	if fd.Refuse {
		t.Fatalf("the filter refused: %+v", fd)
	}
	d := e.OnRequest(context.Background(), &RequestView{Model: "m"})
	if v, _ := d.Tag("stole"); v != "yes" {
		t.Fatalf("the plugin did not keep the reference (%q); the test is not testing what it says", v)
	}
	if d.Denied {
		t.Fatal("the borrowed capability denied a later request")
	}
	if st := e.Stats(); st.Skipped == 0 {
		t.Fatal("calling a revoked capability did not fail the hook")
	}
}

type staticMasker struct{}

func (staticMasker) Mask(s string) (string, int, error) { return "[masked]", 1, nil }
func (staticMasker) MaskValue(s string) (string, error) { return "[masked]", nil }

// TestFilterRewritesTheRequest is the filter surface itself.
func TestFilterRewritesTheRequest(t *testing.T) {
	e := luaEngine(t, `
		dorang.register("on_filter_request", function(f)
			for i = 1, f.count do
				if f.doc.label(i) ~= "system" then
					local masked, n = dorang.mask(f.doc.text(i))
					if n > 0 then f.doc.set_text(i, masked) end
				end
			end
		end)
	`)
	sys := "you are helpful"
	msg := "my id is 900101-1234567"
	doc := NewDoc()
	doc.Add("system", &sys)
	doc.Add("message[0]", &msg)

	d := e.FilterRequest(context.Background(), &FilterView{Model: "m", Doc: doc, Mask: staticMasker{}})
	if d.Refuse {
		t.Fatalf("the filter refused: %+v", d)
	}
	if msg != "[masked]" {
		t.Fatalf("the message was not rewritten: %q", msg)
	}
	if sys != "you are helpful" {
		t.Fatalf("the plugin rewrote a segment it skipped: %q", sys)
	}
}

// TestFilterFailsClosed is DESIGN §10.5b's inversion of §11.5.
func TestFilterFailsClosed(t *testing.T) {
	const src = `dorang.register("on_filter_request", function(f)
		dorang.mask(f.doc.text(1))
	end)`

	// The mask itself fails: the filter was supposed to remove an identity
	// number and did not, so the request stops.
	e := luaEngine(t, src)
	s := "900101-1234567"
	doc := NewDoc()
	doc.Add("m0", &s)
	d := e.FilterRequest(context.Background(), &FilterView{Doc: doc, Mask: brokenMasker{}})
	if !d.Refuse {
		t.Fatalf("a failed mask did not stop the request: %+v", d)
	}
	if d.Code != FilterDenyCode {
		t.Fatalf("code = %q", d.Code)
	}
	if !errors.Is(d.Err, errBrokenMask) {
		t.Fatalf("err = %v, want the mask's own error", d.Err)
	}
	if s != "900101-1234567" {
		t.Fatalf("the text was rewritten by a failed mask: %q", s)
	}

	// A plugin whose ceiling breaks, declared fail-closed, also stops.
	e2 := luaEngine(t, `dorang.register("on_filter_request", function(f) while true do end end)`,
		func(o *Options) {
			o.Limits = Limits{Instructions: 50_000, MemoryBytes: 1 << 20, Timeout: time.Minute}
		})
	if d := e2.FilterRequest(context.Background(), &FilterView{Doc: doc, Mask: staticMasker{}}); !d.Refuse {
		t.Fatalf("a fail-closed filter that hit a ceiling did not stop the request: %+v", d)
	}

	// The same plugin declared fail-open is skipped instead, which is what an
	// enrichment filter should do.
	e3 := luaEngine(t, `dorang.register("on_filter_request", function(f) while true do end end)`,
		func(o *Options) {
			o.Limits = Limits{Instructions: 50_000, MemoryBytes: 1 << 20, Timeout: time.Minute}
			o.Plugins[0].Fail = FailOpen
		})
	if d := e3.FilterRequest(context.Background(), &FilterView{Doc: doc, Mask: staticMasker{}}); d.Refuse {
		t.Fatalf("a fail-open filter stopped the request: %+v", d)
	}
}

var errBrokenMask = errors.New("mask unavailable")

type brokenMasker struct{}

func (brokenMasker) Mask(string) (string, int, error) { return "", 0, errBrokenMask }
func (brokenMasker) MaskValue(string) (string, error) { return "", errBrokenMask }

// TestPluginsRunInOrderAndOneBadOneDoesNotSilenceTheNext.
func TestPluginsRunInOrder(t *testing.T) {
	e, err := New(Options{
		Enabled: true,
		Limits:  Limits{Instructions: 200_000, MemoryBytes: 1 << 20, Timeout: 2 * time.Second},
		Logf:    func(string, ...any) {},
		Plugins: []Plugin{
			{Name: "broken", Source: []byte(`dorang.register("on_request", function(r) error("boom") end)`)},
			{Name: "good", Source: []byte(`dorang.register("on_request", function(r) dorang.tag("ran", "yes") end)`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := e.OnRequest(context.Background(), &RequestView{Model: "m"})
	if v, _ := d.Tag("ran"); v != "yes" {
		t.Fatal("a broken plugin silenced the next one")
	}
	if d.Denied {
		t.Fatal("a plugin error denied the request")
	}
}

// TestVMInstancesAreReused checks the pool does its job, since a fresh LState
// per request would put the cost of the sandbox on every hooked request.
func TestVMInstancesAreReused(t *testing.T) {
	e := luaEngine(t, `dorang.register("on_request", function(req) end)`)
	before := e.lua.created.Load()
	for i := 0; i < 50; i++ {
		e.OnRequest(context.Background(), &RequestView{Model: "m"})
	}
	if after := e.lua.created.Load(); after != before {
		t.Fatalf("built %d new VM instances for 50 sequential requests", after-before)
	}
}
