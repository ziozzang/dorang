package luaext

import (
	"context"
	"errors"
	"strings"
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
				// The pre-fix 256-char measurement was 4.05 s. This is not a
				// benchmark, only a floor under "bounded": if the search were
				// still unpriced this could not pass.
				if elapsed > 1500*time.Millisecond {
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
				budget := int64(1 << 40)
				got, gerr := luapat.Find(p, []byte(s), 0, limit, &budget)
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
				if budget >= 1<<40 {
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
	budget := int64(1000)
	mds, err := luapat.Find(".-.-.-@", []byte(strings.Repeat("a", 256)), 0, 1, &budget)
	if !errors.Is(err, luapat.ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
	if mds != nil {
		t.Fatalf("a stopped match returned %d matches; a partial answer is a wrong answer", len(mds))
	}
	if budget >= 0 {
		t.Fatalf("budget = %d, want it spent", budget)
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
