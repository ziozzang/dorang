package luaext

import (
	"context"
	"strings"
	"testing"
	"time"
)

// What these benchmarks are for.
//
// Charging the VM's O(length) operators — string comparison and string-keyed
// indexing — means putting a host call somewhere in every plugin that writes
// one. That is a cost paid by every hook on every request, against an exposure
// only a long caller-supplied string can reach. The trade cannot be argued from
// first principles, so it is measured here: [BenchmarkFilterPIIMask] runs the
// shipped filter, which is the hook that actually sees request text, and the
// loop benchmarks price a single charged site so the per-site cost is a number
// rather than an impression.

// benchLimits are the shipped defaults, so a benchmark measures what an operator
// gets. The wall clock is pushed out of reach: a benchmark that is really
// measuring the watchdog is measuring nothing.
func benchLimits(o *Options) {
	o.Limits = Limits{Instructions: 5_000_000, MemoryBytes: 32 << 20, Timeout: time.Minute}
}

// benchMasker is a mask that costs about what a real one costs on text with
// nothing to mask: one scan for a fixed marker. It is deliberately cheap, so
// that the Lua-side cost this file exists to measure is not buried under the
// host's.
type benchMasker struct{}

func (benchMasker) Mask(s string) (string, int, error) {
	if i := strings.Index(s, "900101-"); i >= 0 {
		return s[:i] + "[[krrn:0001]]" + s[i+14:], 1, nil
	}
	return s, 0, nil
}

func (benchMasker) MaskValue(s string) (string, error) { return "[[v:0001]]", nil }

// benchDoc builds a request shaped like a chat completion: one operator-written
// system prompt and a handful of caller-written turns, at sizes a real
// conversation reaches.
func benchDoc() (*Doc, []*string) {
	texts := []*string{
		ptr("You are a careful assistant. " + strings.Repeat("Follow the operator's rules. ", 20)),
		ptr("Hello, I need help with my account. " + strings.Repeat("Here is some more context. ", 60)),
		ptr("Sure — my registration number is 900101-1234567 and my order is 44812."),
		ptr(strings.Repeat("The previous answer was not quite right, let me explain again. ", 120)),
	}
	d := NewDoc()
	d.Add("system", texts[0])
	d.Add("message[0]", texts[1])
	d.Add("message[1]", texts[2])
	d.Add("message[2]", texts[3])
	return d, texts
}

func ptr(s string) *string { return &s }

// piiMaskSource is the shipped plugin, read from deploy rather than copied, so
// this measures the file an operator actually installs.
func piiMaskSource(b *testing.B) []byte {
	b.Helper()
	src, err := readShippedFilter()
	if err != nil {
		b.Fatalf("the shipped filter is the subject of this benchmark: %v", err)
	}
	return src
}

// BenchmarkFilterPIIMask is the measurement the operator-charge trade turns on:
// the cost of one request through DESIGN §10.5b's masking filter, which is the
// hook that exists to look at request text.
func BenchmarkFilterPIIMask(b *testing.B) {
	e, err := New(Options{
		Enabled: true,
		Limits:  Limits{Instructions: 5_000_000, MemoryBytes: 32 << 20, Timeout: time.Minute},
		Plugins: []Plugin{{
			Name:   "pii_mask",
			Path:   "pii_mask.lua",
			Source: piiMaskSource(b),
			Config: map[string]string{"skip_system": "true", "min_length": "8"},
		}},
		Logf: func(string, ...any) {},
	})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doc, _ := benchDoc()
		d := e.FilterRequest(ctx, &FilterView{
			Model: "gpt-4o", KeyID: "k-1", Doc: doc, Mask: benchMasker{},
		})
		if d.Refuse {
			b.Fatalf("the filter refused: %+v", d)
		}
	}
}

// BenchmarkOnRequestPolicy is an ordinary policy hook: field comparisons against
// literals and constant-keyed lookups, which is what nearly every plugin is made
// of. Nothing here is charged by operand length, and this benchmark is what says
// so.
func BenchmarkOnRequestPolicy(b *testing.B) {
	e := benchEngine(b, `
		local blocked = { ["gpt-4"] = true, ["o1-preview"] = true }
		local tiers = { eng = "premium", data = "bulk" }
		dorang.register("on_request", function(req)
			if blocked[req.model] and req.team_id ~= "eng" then
				return false, "not available", "model_blocked"
			end
			local tier = tiers[req.team_id]
			if tier ~= nil then dorang.tag("tier", tier) end
			if req.body_bytes > 1048576 then dorang.tag("large", "1") end
			if req.method == "POST" and req.path ~= "/v1/models" then
				dorang.tag("billable", "1")
			end
		end)`)
	v := &RequestView{
		Model: "gpt-4o", TeamID: "data", KeyID: "k-1",
		Method: "POST", Path: "/v1/chat/completions", BodyBytes: 4096,
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if d := e.OnRequest(ctx, v); d.Denied {
			b.Fatal("denied")
		}
	}
}

// BenchmarkComparisonLoopDynamic prices the charged site itself: a comparison
// whose operands are both dynamic, which is the only shape the rewrite touches.
// One thousand of them per invocation, so the per-comparison cost is the
// reported figure divided by a thousand.
func BenchmarkComparisonLoopDynamic(b *testing.B) {
	benchLoop(b, `local a, c = req.model, req.key_id
		local hits = 0
		for i = 1, 1000 do
			if a == c then hits = hits + 1 end
		end`)
}

// BenchmarkComparisonLoopLiteral is the same loop against a literal, which the
// rewrite leaves alone because the literal's length bounds the work at compile
// time. The gap between this and the one above is what the charge costs.
func BenchmarkComparisonLoopLiteral(b *testing.B) {
	benchLoop(b, `local a = req.model
		local hits = 0
		for i = 1, 1000 do
			if a == "gpt-4" then hits = hits + 1 end
		end`)
}

// BenchmarkIndexLoopDynamic prices a dynamic string-keyed lookup.
func BenchmarkIndexLoopDynamic(b *testing.B) {
	benchLoop(b, `local t = { ["gpt-4o"] = 1 }
		local k = req.model
		local hits = 0
		for i = 1, 1000 do
			if t[k] then hits = hits + 1 end
		end`)
}

// BenchmarkIndexLoopConstant is the same lookup with a constant key, which the
// rewrite leaves alone.
func BenchmarkIndexLoopConstant(b *testing.B) {
	benchLoop(b, `local t = { ["gpt-4o"] = 1 }
		local hits = 0
		for i = 1, 1000 do
			if t["gpt-4o"] then hits = hits + 1 end
		end`)
}

// BenchmarkNumericLoop is the floor: a loop of the same length doing arithmetic
// only. Everything above is measured against it.
func BenchmarkNumericLoop(b *testing.B) {
	benchLoop(b, `local hits = 0
		for i = 1, 1000 do
			hits = hits + 1
		end`)
}

func benchLoop(b *testing.B, body string) {
	e := benchEngine(b, "dorang.register(\"on_request\", function(req)\n"+body+"\nend)")
	v := &RequestView{Model: "gpt-4o", KeyID: "k-1"}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if d := e.OnRequest(ctx, v); d.Denied {
			b.Fatal("denied")
		}
	}
}

func benchEngine(b *testing.B, src string) *Engine {
	b.Helper()
	o := Options{
		Enabled: true,
		Plugins: []Plugin{{Name: "bench", Path: "bench.lua", Source: []byte(src)}},
		Logf:    func(string, ...any) {},
	}
	benchLimits(&o)
	e, err := New(o)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	return e
}
