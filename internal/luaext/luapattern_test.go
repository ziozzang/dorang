package luaext

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuin/gopher-lua/pm"

	"github.com/ziozzang/dorang/internal/luaext/luapat"
)

// patternLimits are the package defaults, spelled out so these tests measure the
// ceiling an operator actually gets rather than a test-local one. The wall clock
// is pushed far out of reach: only the instruction ceiling may end these, and if
// the watchdog is what stops them the test has proved nothing.
func patternLimits(o *Options) {
	o.Limits = Limits{Instructions: 5_000_000, MemoryBytes: 32 << 20, Timeout: time.Minute}
}

// TestSearchIsChargedForItsWorkNotItsOutput is the hole this file closes.
//
// string.find returns two integers. Charged for its output it cost a flat two
// gas, and the work behind those two integers is a backtracking search whose
// cost is superlinear in the *subject* — which is the caller's, not the
// operator's. Measured against gopher-lua's matcher before the fix, with the
// pattern fixed and only the length varying:
//
//	16 chars    124 µs
//	64 chars    17.5 ms
//	128 chars   264 ms
//	256 chars   4.05 s
//
// A tenant whose text reaches a masking or policy hook could therefore buy
// seconds of CPU per request, with all three ceilings looking on. The 256-char
// row is named here so the measurement is in the suite and not only in a report.
func TestSearchIsChargedForItsWorkNotItsOutput(t *testing.T) {
	// The pattern is fixed, plausible and operator-supplied — three lazy items
	// before a literal. Only the subject's length varies, and the subject is the
	// caller's.
	const hook = `dorang.register("on_request", function(req)
		BODY
		dorang.tag("done", "1")
	end)`

	cases := []struct {
		name string
		body string
		fill string
		n    int
		// stopped says the instruction ceiling must fire on this row.
		stopped bool
	}{
		{"find/16 chars completes", `string.find(req.key_id, ".-.-.-@")`, "a", 16, false},
		{"find/128 chars", `string.find(req.key_id, ".-.-.-@")`, "a", 128, true},
		{"find/256 chars", `string.find(req.key_id, ".-.-.-@")`, "a", 256, true},
		{"find/1024 chars", `string.find(req.key_id, ".-.-.-@")`, "a", 1024, true},
		{"match/256 chars", `req.key_id:match(".-.-.-@")`, "a", 256, true},
		{"gmatch/256 chars", `for _ in req.key_id:gmatch(".-.-.-@") do end`, "a", 256, true},
		{"gsub/256 chars", `req.key_id:gsub(".-.-.-@", "x")`, "a", 256, true},
		// One quantifier is enough once the subject is long: an unanchored
		// %d+x over 64 KiB of digits measured 82 s before the fix.
		{"quadratic scan/64 KiB", `string.find(req.key_id, "%d+x")`, "1", 65536, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := luaEngine(t, strings.Replace(hook, "BODY", tc.body, 1), patternLimits)

			subject := strings.Repeat(tc.fill, tc.n)

			start := time.Now()
			d := e.OnRequest(context.Background(), &RequestView{KeyID: subject})
			elapsed := time.Since(start)

			if d.Denied {
				t.Fatal("a ceiling breach denied the request; ceilings fail open")
			}
			_, completed := d.Tag("done")
			st := e.Stats()

			if tc.stopped {
				if st.LimitHits == 0 {
					t.Fatalf("the instruction ceiling did not fire on a %d-char subject "+
						"(elapsed %v): a builtin walked through it", tc.n, elapsed)
				}
				if completed {
					t.Fatal("the hook completed after its budget was exhausted")
				}
				// The measurement stays in the suite, as a log. It is not an
				// assertion, because it cannot be one: once the ceiling has
				// fired the search ran exactly its budget in steps, and how
				// long a fixed number of steps takes is the machine's business.
				// At 1.5 s this failed whenever the suite ran beside other
				// copies of itself — a legitimate stopped search measured 1.63 s
				// — and it was reporting the load, not the pricing.
				//
				// What the row is really for is asserted two lines above and is
				// exact: LimitHits != 0 says the ceiling fired INSIDE the
				// builtin, which is the whole defect. Unpriced, the 256-char
				// search walked straight through it and took 4.05 s; the check
				// below is only a backstop against a runaway that somehow
				// counted a limit hit anyway, so it is set where no working
				// build can reach it and every broken one can.
				t.Logf("%d-char subject: stopped after %v", tc.n, elapsed)
				if elapsed > 30*time.Second {
					t.Fatalf("a stopped search still took %v", elapsed)
				}
			} else {
				if !completed {
					t.Fatalf("an affordable search was refused (elapsed %v, stats %+v)", elapsed, st)
				}
				if st.LimitHits != 0 {
					t.Fatalf("an affordable search hit a ceiling: %+v", st)
				}
			}
		})
	}
}

// TestOrdinaryPatternsOnOrdinaryInputStillRun is the other half, and the half a
// blunt fix fails: pricing search must not turn every hook that touches a string
// into a refusal. A closed-form bound on the pattern's *shape* does exactly
// that — anything strict enough to stop `.-.-.-@` at 256 bytes also refuses
// `^%s*(.-)%s*$`, which is how everybody writes trim.
func TestOrdinaryPatternsOnOrdinaryInputStillRun(t *testing.T) {
	e := luaEngine(t, `
		dorang.register("on_request", function(req)
			local trimmed = req.key_id:match("^%s*(.-)%s*$")
			local user, host = req.path:match("([%w%.%-]+)@([%w%.%-]+)")
			local n = select(2, req.path:gsub("%d", "#"))
			local words = 0
			for _ in req.key_id:gmatch("%a+") do words = words + 1 end
			if string.find(req.model, "^gpt%-4") then dorang.tag("family", "gpt4") end
			dorang.tag("trim", tostring(#trimmed))
			dorang.tag("mail", tostring(user) .. "@" .. tostring(host) .. "/" .. tostring(n))
			dorang.tag("words", tostring(words))
		end)`, patternLimits)

	for _, n := range []int{0, 16, 1024, 8192} {
		v := &RequestView{
			Model:  "gpt-4o",
			KeyID:  "  " + strings.Repeat("word ", n/5) + "  ",
			Path:   "/v1/chat?u=alice.smith@example.com&id=90210",
			Method: "POST",
		}
		d := e.OnRequest(context.Background(), v)
		if d.Denied {
			t.Fatalf("n=%d: an ordinary hook denied the request", n)
		}
		if got, _ := d.Tag("family"); got != "gpt4" {
			t.Fatalf("n=%d: anchored find did not run (tags %+v)", n, d.Tags())
		}
		if got, _ := d.Tag("mail"); got != "alice.smith@example.com/6" {
			t.Fatalf("n=%d: captures = %q", n, got)
		}
		if got, _ := d.Tag("words"); got != itoa(n/5) {
			t.Fatalf("n=%d: gmatch counted %q words, want %d", n, got, n/5)
		}
	}
	if st := e.Stats(); st.LimitHits != 0 || st.Skipped != 0 {
		t.Fatalf("ordinary patterns tripped a ceiling: %+v", st)
	}
}

// TestSearchStillProducesGopherLuaResults guards the thing a pricing pass could
// quietly break: the answers. The priced run must agree with the run that
// actually happens, or the ceiling is being enforced against a different call
// from the one a plugin sees.
func TestSearchStillProducesGopherLuaResults(t *testing.T) {
	e := luaEngine(t, `
		dorang.register("on_request", function(req)
			local a = req.path:find("chat", 1, true)
			local b, c = req.path:match("(%a+)/(%a+)")
			local d, n = req.path:gsub("[aeiou]", ".")
			local last
			for w in req.path:gmatch("%a+") do last = w end
			dorang.tag("t", tostring(a) .. "|" .. b .. "|" .. c .. "|" .. d .. "|" .. tostring(n) .. "|" .. last)
		end)`, patternLimits)

	d := e.OnRequest(context.Background(), &RequestView{Path: "v1/chat/completions"})
	got, _ := d.Tag("t")
	const want = "4|chat|completions|v1/ch.t/c.mpl.t..ns|5|completions"
	if got != want {
		t.Fatalf("search results changed:\n got %q\nwant %q", got, want)
	}
}

// TestBudgetedMatcherMatchesUpstream is the test that keeps [luapat] a *copy*.
// The only intended difference from gopher-lua's matcher is that it counts; if a
// re-copy after an upgrade ever changes an answer, this says so.
func TestBudgetedMatcherMatchesUpstream(t *testing.T) {
	patterns := []string{
		"", "a", "%a+", "^%s*(.-)%s*$", "(%a+)/(%a+)", "[%w%.%-]+@[%w%.%-]+%.%w+",
		"%d+x", ".-.-@", "()aa()", "%bxy", "(a)%1", "^abc$", "[^%s]+", "a*b-c?",
		"%u%l+", "x$", "%%", "[%]]", "(.)(.)",
	}
	subjects := []string{
		"", "a", "aaaa", "  hello world  ", "v1/chat/completions",
		"alice.smith@example.com", "12345x", "xaaay", "aa", "ABCdef", "%", "]",
		strings.Repeat("ab", 40) + "@",
	}
	for _, p := range patterns {
		for _, s := range subjects {
			for _, limit := range []int{1, -1, 3} {
				run := luapat.Run{Budget: 1 << 40}
				got, gerr := luapat.Find(p, []byte(s), 0, limit, &run)
				want, werr := pm.Find(p, []byte(s), 0, limit)
				if (gerr == nil) != (werr == nil) {
					t.Fatalf("Find(%q, %q, %d): err %v, upstream %v", p, s, limit, gerr, werr)
				}
				if gerr != nil {
					continue
				}
				if len(got) != len(want) {
					t.Fatalf("Find(%q, %q, %d): %d matches, upstream %d", p, s, limit, len(got), len(want))
				}
				for i := range got {
					if got[i].CaptureLength() != want[i].CaptureLength() {
						t.Fatalf("Find(%q, %q, %d)[%d]: capture count differs", p, s, limit, i)
					}
					for c := 0; c < got[i].CaptureLength(); c++ {
						if got[i].Capture(c) != want[i].Capture(c) ||
							got[i].IsPosCapture(c) != want[i].IsPosCapture(c) {
							t.Fatalf("Find(%q, %q, %d)[%d]: capture %d differs", p, s, limit, i, c)
						}
					}
				}
				if run.Budget >= 1<<40 {
					t.Fatalf("Find(%q, %q, %d) charged nothing", p, s, limit)
				}
			}
		}
	}
}

// TestBudgetedMatcherStopsRatherThanTruncates: a budget breach must not look
// like "no match". A truncated answer is a wrong answer, and a masking filter
// that believes there was nothing to mask is the failure §10.5b exists to stop.
func TestBudgetedMatcherStopsRatherThanTruncates(t *testing.T) {
	run := luapat.Run{Budget: 1000}
	mds, err := luapat.Find(".-.-.-@", []byte(strings.Repeat("a", 256)), 0, 1, &run)
	if !errors.Is(err, luapat.ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
	if mds != nil {
		t.Fatalf("a stopped match returned %d matches; a partial answer is a wrong answer", len(mds))
	}
	if run.Budget >= 0 {
		t.Fatalf("budget = %d, want it spent", run.Budget)
	}
}

// --- the stack is a resource too -------------------------------------------

// TestAGreedyPatternCannotEatTheStack is the ceiling the step counter did not
// give.
//
// The matcher is recursive: one Go frame per branch explored, so `(.*)` costs a
// frame per character it consumes and the *caller* chooses how many by choosing
// how long the text is. Charging steps bounded the CPU and watched the stack
// grow — 800 KiB of caller text through the filter's own view cost 800 K frames
// and **3 072 MB** of goroutine stack across eight concurrent requests, with
// limitHits = 0 and no ceiling firing. Go kills the process at a 1 GB stack, so
// a large enough segment was a crash rather than a refusal.
//
// The shape is the reachable one: FilterView.Doc is the caller's own message
// text, and the pattern is a plausible operator-written one.
func TestAGreedyPatternCannotEatTheStack(t *testing.T) {
	e := luaEngine(t, `
		dorang.register("on_filter_request", function(f)
			string.find(f.doc.text(1), "^(.*)=(.*)$")
		end)`, patternLimits)

	const concurrency = 8
	subject := strings.Repeat("a", 800<<10)

	stop := make(chan struct{})
	peak := make(chan uint64, 1)
	go func() {
		var m runtime.MemStats
		var max uint64
		for {
			runtime.ReadMemStats(&m)
			if m.StackInuse > max {
				max = m.StackInuse
			}
			select {
			case <-stop:
				peak <- max
				return
			default:
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var base runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&base)

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := subject
			doc := NewDoc()
			doc.Add("message[0]", &s)
			d := e.FilterRequest(context.Background(), &FilterView{Doc: doc, Mask: staticMasker{}})
			if !d.Refuse {
				t.Errorf("a match that cannot be afforded was served: %+v", d)
			}
			if !errors.Is(d.Err, ErrPatternTooDeep) {
				t.Errorf("err = %v, want the stack ceiling", d.Err)
			}
		}()
	}
	wg.Wait()
	close(stop)

	// The bound: [luapat.MaxDepth] frames of about 480 bytes each, per match,
	// and one match at a time per goroutine. The slack is for the harness's own
	// stacks and for Go's stack doubling, and it is still two orders of
	// magnitude below what this used to reach.
	const perMatch = luapat.MaxDepth * 512
	limit := uint64(concurrency*perMatch) + base.StackInuse + (4 << 20)
	if got := <-peak; got > limit {
		t.Fatalf("peak goroutine stack %d MB (baseline %d MB), want at most %d MB: "+
			"%d concurrent matches over %d KiB are not bounded by the depth ceiling",
			got>>20, base.StackInuse>>20, limit>>20, concurrency, len(subject)>>10)
	}
	if e.Stats().LimitHits != concurrency {
		t.Fatalf("limitHits = %d, want %d: a ceiling that fires must be counted as one",
			e.Stats().LimitHits, concurrency)
	}
}

// TestAPricedMatchStopsWhenTheRequestDoes: the priced run takes the invocation's
// context, so the wall clock still bounds it.
//
// Without it the pricing run was the one thing in the package the watchdog could
// not reach: it spent its whole step budget on a goroutine nobody was waiting
// for, which is how a burst of large requests turned into a backlog of abandoned
// goroutines and — through the trip — into a filter that was switched off.
func TestAPricedMatchStopsWhenTheRequestDoes(t *testing.T) {
	// A long scan with no deep backtracking: every start position costs the
	// pattern's length and recurses two frames, so neither the step budget nor
	// the depth ceiling ends this — 4 MiB of subject against a 30-byte literal
	// is 130 M steps and 1.1 s, and the budget covers all of it. The only thing
	// that can stop it is the wall clock, and the wall clock could not reach
	// inside a host call.
	const wall = 20 * time.Millisecond
	e := luaEngine(t, `
		dorang.register("on_filter_request", function(f)
			string.find(f.doc.text(1), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaab")
		end)`, func(o *Options) {
		o.Limits = Limits{Instructions: 500_000_000, MemoryBytes: 64 << 20, Timeout: wall}
	})

	base := settledGoroutines(t)
	s := strings.Repeat("a", 4<<20)
	doc := NewDoc()
	doc.Add("message[0]", &s)

	start := time.Now()
	d := e.FilterRequest(context.Background(), &FilterView{Doc: doc, Mask: staticMasker{}})
	elapsed := time.Since(start)

	if !d.Refuse {
		t.Fatalf("a filter that did not complete must refuse: %+v", d)
	}
	if elapsed > time.Second {
		t.Fatalf("the request waited %v for a %v ceiling", elapsed, wall)
	}
	// The property, and the one a caller-side stopwatch cannot see: the match
	// ended itself, so there is no abandoned goroutine matching on after the
	// request. Without the context it ran for 1.1 s on a core nobody was
	// waiting for — and that is what a burst turned into a trip.
	//
	// Asserted as "how long until the goroutine is gone", not as "which of the
	// two counters it landed in". Those are different questions, and only the
	// first one is about this code. Whether the match's return beats
	// [abandonGrace] is decided by whether the scheduler runs its goroutine in
	// the 5 ms after the deadline, so `Timeouts != 0` was a coin the machine
	// tossed — it failed roughly one full-suite run in three. Both faces are
	// correct behaviour: a match that observed its cancellation and was booked
	// as abandoned because it was slow to be scheduled has still not outlived
	// the request, which is the whole claim.
	//
	// The bound below is what the two answers actually differ by. A match that
	// takes its context is finished within the deadline plus a scheduling
	// hiccup; one that ignores it has 1.1 s of scanning left to do and cannot
	// possibly be gone. Half a second sits between them with an order of
	// magnitude either way.
	outstanding := func() int { return e.Abandoned(HookFilterRequest) }
	for deadline := time.Now().Add(500 * time.Millisecond); outstanding() > 0; {
		if time.Now().After(deadline) {
			t.Fatalf("%d invocations still outstanding half a second after a %v ceiling: "+
				"the priced run ignores its context and is still matching", outstanding(), wall)
		}
		time.Sleep(time.Millisecond)
	}
	if n, ok := waitForGoroutines(base+2, time.Second); !ok {
		t.Fatalf("goroutines settled at %d, baseline %d: a match is still running", n, base)
	}
}

// TestGsubIsPricedWhicheverWayItIsSpelled is the A/B that found the hole.
//
// Identical output, two spellings: `gsub(s, "a", "y")` and the same call with a
// function returning "y". The string form was refused by the memory charge in
// 48 µs when the replacement was long; the function form was charged **one byte
// per position** — `per = 1` — so the memory bound never fired, and it ran the
// full wall clock with limitHits = 0. The pricing covered the pattern scan and
// missed both the replacement callback and the rebuild underneath it, which
// gopher-lua does once per match over the whole subject.
func TestGsubIsPricedWhicheverWayItIsSpelled(t *testing.T) {
	subject := strings.Repeat("ab", 200<<10) // 400 KiB, 200 K matches

	run := func(t *testing.T, src string) (time.Duration, FilterDecision, Stats) {
		t.Helper()
		e := luaEngine(t, src, patternLimits)
		s := subject
		doc := NewDoc()
		doc.Add("message[0]", &s)
		start := time.Now()
		d := e.FilterRequest(context.Background(), &FilterView{Doc: doc, Mask: staticMasker{}})
		return time.Since(start), d, e.Stats()
	}

	const str = `dorang.register("on_filter_request", function(f)
		f.doc.set_text(1, string.gsub(f.doc.text(1), "a", "y"))
	end)`
	const fn = `dorang.register("on_filter_request", function(f)
		f.doc.set_text(1, string.gsub(f.doc.text(1), "a", function() return "y" end))
	end)`

	strTime, strDec, strStats := run(t, str)
	fnTime, fnDec, fnStats := run(t, fn)

	for _, tc := range []struct {
		name  string
		took  time.Duration
		dec   FilterDecision
		stats Stats
	}{{"string", strTime, strDec, strStats}, {"function", fnTime, fnDec, fnStats}} {
		if !tc.dec.Refuse {
			t.Fatalf("%s: a gsub that rebuilds 400 KiB two hundred thousand times was served: %+v",
				tc.name, tc.dec)
		}
		if tc.stats.LimitHits == 0 {
			t.Fatalf("%s: refused with no ceiling counted (%+v); the wall clock is not a price",
				tc.name, tc.stats)
		}
		if tc.stats.Timeouts != 0 {
			t.Fatalf("%s: the wall clock stopped it, not a ceiling: %+v", tc.name, tc.stats)
		}
	}
	// Comparable, not identical: the function form cannot be refused before the
	// scan that counts its matches, because the length of what a callback
	// returns does not exist until it returns. Within a small multiple is the
	// property; a thousandfold was the defect.
	if fnTime > 4*strTime+50*time.Millisecond {
		t.Fatalf("the function form took %v against the string form's %v", fnTime, strTime)
	}
}

// TestGsubStillProducesGopherLuaResults guards what pricing a call must never
// do: change its answer.
//
// The function form is now called through a wrapper that charges what the
// callback returns, so every part of gsub's contract that runs through that
// wrapper is checked here — captures as arguments, a `nil` return meaning "keep
// the original", the replacement count, and the table form beside both.
func TestGsubStillProducesGopherLuaResults(t *testing.T) {
	e := luaEngine(t, `
		dorang.register("on_request", function(req)
			local s = req.path
			local a, na = s:gsub("(%a+)/(%a+)", "%2-%1")
			local b, nb = s:gsub("(%a+)/(%a+)", function(x, y) return y .. "-" .. x end)
			local c, nc = s:gsub("%a+", function(w) if w == "chat" then return "CHAT" end end)
			local d, nd = s:gsub("%a+", {chat = "CHAT", v1 = false})
			dorang.tag("s", a .. "|" .. tostring(na))
			dorang.tag("f", b .. "|" .. tostring(nb))
			dorang.tag("n", c .. "|" .. tostring(nc))
			dorang.tag("t", d .. "|" .. tostring(nd))
		end)`, patternLimits)

	d := e.OnRequest(context.Background(), &RequestView{Path: "v1/chat/completions"})
	str, _ := d.Tag("s")
	fn, _ := d.Tag("f")
	if str != fn {
		t.Fatalf("the two spellings of one gsub disagree:\n string %q\n func   %q", str, fn)
	}
	if str != "v1/completions-chat|1" {
		t.Fatalf("gsub with captures = %q", str)
	}
	if got, _ := d.Tag("n"); got != "v1/CHAT/completions|3" {
		t.Fatalf("a callback returning nil must keep the original: %q", got)
	}
	if got, _ := d.Tag("t"); got != "v1/CHAT/completions|3" {
		t.Fatalf("table replacement = %q", got)
	}
	if st := e.Stats(); st.Skipped != 0 || st.LimitHits != 0 {
		t.Fatalf("an affordable gsub hit a ceiling: %+v", st)
	}
}

// TestGsubWithALongReplacementIsRefusedBeforeItRuns keeps the cheap end of the
// same charge: when the replacement's length *is* known, the memory bound still
// fires before any matching happens.
func TestGsubWithALongReplacementIsRefusedBeforeItRuns(t *testing.T) {
	e := luaEngine(t, `
		local wide = string.rep("y", 100)
		dorang.register("on_filter_request", function(f)
			f.doc.set_text(1, string.gsub(f.doc.text(1), "a", wide))
		end)`, patternLimits)

	s := strings.Repeat("ab", 200<<10)
	doc := NewDoc()
	doc.Add("message[0]", &s)
	start := time.Now()
	d := e.FilterRequest(context.Background(), &FilterView{Doc: doc, Mask: staticMasker{}})
	if !d.Refuse || !errors.Is(d.Err, ErrMemoryLimit) {
		t.Fatalf("decision = %+v, want a memory refusal", d)
	}
	if took := time.Since(start); took > 20*time.Millisecond {
		t.Fatalf("a bound that is known before the call took %v to apply", took)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
