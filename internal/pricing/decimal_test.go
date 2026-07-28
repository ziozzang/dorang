package pricing

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseDecimalExact(t *testing.T) {
	cases := []struct {
		in    string
		neg   bool
		units uint64
		scale uint8
	}{
		{"0", false, 0, 0},
		{"7", false, 7, 0},
		{"0.85", false, 85, 2},
		{"3.40", false, 34, 1},
		{"1.25", false, 125, 2},
		{"0.0005", false, 5, 4},
		{"20.00", false, 20, 0},
		{"-15", true, 15, 0},
		{"+0.1", false, 1, 1},
		{"0.000000000001", false, 1, 12},
		{"18446744073709551615", false, 18446744073709551615, 0},
		{"-0", false, 0, 0},
		{".5", false, 5, 1},
		{"123456789.987654321", false, 123456789987654321, 9},
	}
	for _, tc := range cases {
		d, err := parseDecimal(tc.in)
		if err != nil {
			t.Fatalf("parseDecimal(%q): %v", tc.in, err)
		}
		if d.neg != tc.neg || d.units != tc.units || d.scale != tc.scale {
			t.Fatalf("parseDecimal(%q) = {neg:%v units:%d scale:%d}, want {neg:%v units:%d scale:%d}",
				tc.in, d.neg, d.units, d.scale, tc.neg, tc.units, tc.scale)
		}
	}
	for _, bad := range []string{"", " ", "abc", "1.2.3", "1e9", "1E9", "0x10", "1 000", "--1", "-", "1.2a", "0.0000000000001", "18446744073709551616"} {
		if d, err := parseDecimal(bad); err == nil {
			t.Fatalf("parseDecimal(%q) accepted, got %+v", bad, d)
		}
	}
}

func TestDecimalToAtto(t *testing.T) {
	cases := []struct {
		in string
		lo uint64
	}{
		{"1", 1_000_000_000_000_000_000},
		{"0.85", 850_000_000_000_000_000},
		{"0.0005", 500_000_000_000_000},
		{"0.000000000001", 1_000_000},
	}
	for _, tc := range cases {
		d, err := parseDecimal(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		a, ok := d.atto()
		if !ok {
			t.Fatalf("%q: atto overflowed", tc.in)
		}
		if a.hi != 0 || a.lo != tc.lo {
			t.Fatalf("%q: atto = {%d,%d}, want lo %d", tc.in, a.hi, a.lo, tc.lo)
		}
	}
}

func TestU128MulDiv(t *testing.T) {
	// (2^64 - 1) * (2^64 - 1) / 1 does not fit in 128 bits... it does, exactly.
	max := u64To128(^uint64(0))
	q, r, ok := max.mulDiv(^uint64(0), 1)
	if !ok || r != 0 {
		t.Fatalf("max*max/1: ok=%v r=%d", ok, r)
	}
	if q.hi != 0xFFFFFFFFFFFFFFFE || q.lo != 1 {
		t.Fatalf("max*max = {%x,%x}", q.hi, q.lo)
	}
	// The quotient overflowing 128 bits is reported, not wrapped.
	if _, _, ok := (u128{hi: ^uint64(0), lo: ^uint64(0)}).mulDiv(2, 1); ok {
		t.Fatal("expected a 128-bit overflow to be reported")
	}
	// Division by zero is reported, never a panic.
	if _, _, ok := max.mulDiv(1, 0); ok {
		t.Fatal("expected division by zero to be reported")
	}
	// Exact remainders.
	q, r, ok = u64To128(100).mulDiv(3, 7)
	if !ok || q.lo != 42 || r != 6 {
		t.Fatalf("100*3/7 = %d rem %d", q.lo, r)
	}
}

func TestRoundToNanoIsHalfToEven(t *testing.T) {
	half := int64(attoPerNano / 2)
	cases := []struct {
		atto      int64
		wantNano  int64
		wantCarry int64
	}{
		{0, 0, 0},
		{attoPerNano, 1, 0},
		{half, 0, half},                  // ties to even: 0.5 -> 0
		{3 * half, 2, -half},             // 1.5 -> 2
		{5 * half, 2, half},              // 2.5 -> 2
		{attoPerNano + 1, 1, 1},          // rounds down, remainder carried
		{attoPerNano - 1, 1, -1},         // rounds up, remainder carried
		{-half, 0, -half},                // symmetric
		{-3 * half, -2, half},            //
		{2*attoPerNano + half, 2, half},  // 2.5 -> 2
		{3*attoPerNano + half, 4, -half}, // 3.5 -> 4
	}
	for _, tc := range cases {
		v := amt{neg: tc.atto < 0}
		mag := tc.atto
		if mag < 0 {
			mag = -mag
		}
		v.m = u64To128(uint64(mag))
		nano, carry, err := roundToNano(v, 0)
		if err != nil {
			t.Fatalf("%d attos: %v", tc.atto, err)
		}
		if nano != tc.wantNano || carry != tc.wantCarry {
			t.Fatalf("%d attos -> nano %d carry %d, want nano %d carry %d",
				tc.atto, nano, carry, tc.wantNano, tc.wantCarry)
		}
		if nano*attoPerNano+carry != tc.atto {
			t.Fatalf("%d attos: nano %d and carry %d do not reconstruct the exact value",
				tc.atto, nano, carry)
		}
	}
}

// TestSubNanoPricesDoNotRoundToZero is the regression test for REVIEW.md finding 11: a
// sub-nano per-token price must not round to zero on every request forever.
func TestSubNanoPricesDoNotRoundToZero(t *testing.T) {
	c := mustCatalog(t, `
currency: USD
rules:
  - { id: micro, match: { model: m }, unit: per_1m_tokens, input: "0.0005" }
`)
	// 0.0005 per 1M tokens is 5e-10 per token: exactly half a nano.
	req := Request{Model: "m", Credential: "acct", InputTokens: 1, At: at(t, "2026-07-15T12:00:00Z")}

	const n = 2000
	var settled int64
	for i := 0; i < n; i++ {
		cost, err := c.Settle(req)
		if err != nil {
			t.Fatal(err)
		}
		var err2 error
		if settled, err2 = addNano(settled, cost.MarginalNano); err2 != nil {
			t.Fatal(err2)
		}
		// The carry alternates: 0, 1, 0, 1, ... never a systematic zero.
		if want := int64(i % 2); cost.MarginalNano != want {
			t.Fatalf("settlement %d: marginal = %d, want %d", i, cost.MarginalNano, want)
		}
	}
	if want := int64(n / 2); settled != want {
		t.Fatalf("2000 single-token requests summed to %d nano, want exactly %d", settled, want)
	}

	// Without the carry — the estimate path — each request rounds independently, which is
	// why accounting goes through Settle and routing does not.
	var estimated int64
	for i := 0; i < n; i++ {
		cost := mustPrice(t, c, req)
		estimated += cost.MarginalNano
	}
	if estimated != 0 {
		t.Fatalf("Price is expected to round each estimate independently, got %d", estimated)
	}

	// Carries are per settlement bucket, so one account cannot spend another's remainder.
	other := req
	other.Credential = "other-acct"
	first, err := c.Settle(other)
	if err != nil {
		t.Fatal(err)
	}
	if first.MarginalNano != 0 {
		t.Fatalf("a fresh bucket started with a borrowed carry: %d", first.MarginalNano)
	}
}

// TestExactDecimalArithmeticBeatsFloat64 uses values where a float64 price path is
// provably wrong: 0.07 per 1M tokens over 1e15 tokens is exactly 7e16 nano, but
// (0.07 * 1e15 / 1e6) * 1e9 in binary floating point yields 70000000000000016.
func TestExactDecimalArithmeticBeatsFloat64(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - { id: a, match: { model: big },   unit: per_1m_tokens, input: "0.07" }
  - { id: b, match: { model: small }, unit: per_1m_tokens, input: "0.19" }
`)
	now := at(t, "2026-07-15T12:00:00Z")

	cost := mustPrice(t, c, Request{Model: "big", InputTokens: 1_000_000_000_000_000, At: now})
	if want := int64(70_000_000_000_000_000); cost.MarginalNano != want {
		t.Fatalf("0.07 x 1e15 tokens = %d nano, want exactly %d (a float64 path gives %d)",
			cost.MarginalNano, want, int64((0.07*1e15/1e6)*1e9))
	}
	cost = mustPrice(t, c, Request{Model: "small", InputTokens: 123_456_789_012, At: now})
	if want := int64(23_456_789_912_280); cost.MarginalNano != want {
		t.Fatalf("0.19 x 123456789012 tokens = %d nano, want exactly %d", cost.MarginalNano, want)
	}
}

// TestTenthsAndFifthsAccumulateExactly is the 0.1 + 0.2 case: repeated accumulation of
// values with no exact binary representation must stay exact.
func TestTenthsAndFifthsAccumulateExactly(t *testing.T) {
	c := mustCatalog(t, `
rules:
  - id: tenths
    match: { model: m }
    unit: per_1m_tokens
    input:  "0.1"
    output: "0.2"
`)
	req := Request{Model: "m", Credential: "acct", InputTokens: 1_000_000, OutputTokens: 1_000_000,
		At: at(t, "2026-07-15T12:00:00Z")}
	cost := mustPrice(t, c, req)
	if want := int64(300_000_000); cost.MarginalNano != want { // 0.1 + 0.2 == 0.3, exactly
		t.Fatalf("0.1 + 0.2 = %d nano, want %d", cost.MarginalNano, want)
	}

	// A hundred settlements of 0.3 is exactly 30, with no accumulated drift.
	var total int64
	for i := 0; i < 100; i++ {
		got, err := c.Settle(req)
		if err != nil {
			t.Fatal(err)
		}
		total += got.MarginalNano
	}
	if want := int64(30_000_000_000); total != want {
		t.Fatalf("100 x 0.3 = %d nano, want %d", total, want)
	}

	// One tenth of a nano per request: 10 settlements make exactly one nano.
	c2 := mustCatalog(t, `
rules:
  - { id: tiny, match: { model: m }, unit: per_1m_tokens, input: "0.0001" }
`)
	tiny := Request{Model: "m", Credential: "acct", InputTokens: 1, At: at(t, "2026-07-15T12:00:00Z")}
	total = 0
	for i := 0; i < 10; i++ {
		got, err := c2.Settle(tiny)
		if err != nil {
			t.Fatal(err)
		}
		total += got.MarginalNano
	}
	if total != 1 {
		t.Fatalf("ten requests at 0.1 nano summed to %d, want 1", total)
	}
}

// TestNoFloatInPricePath enforces the structural rule behind §8.3: prices are parsed from
// their string form and never routed through binary floating point. float64 may appear in
// exactly two places — the Request.Seconds field and the boundary conversion in seconds.go
// — and nowhere else.
func TestNoFloatInPricePath(t *testing.T) {
	allowed := map[string]bool{"seconds.go": true, "types.go": true}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for i, raw := range strings.Split(string(src), "\n") {
			line := raw
			if k := strings.Index(line, "//"); k >= 0 { // comments may name what code may not do
				line = line[:k]
			}
			if strings.Contains(line, "float64") || strings.Contains(line, "float32") {
				if !allowed[name] {
					t.Errorf("%s:%d mentions a float type in the price path: %s", name, i+1, strings.TrimSpace(raw))
				}
			}
			if strings.Contains(line, "ParseFloat") {
				t.Errorf("%s:%d parses a number through float64: %s", name, i+1, strings.TrimSpace(raw))
			}
		}
	}
	if checked < 5 {
		t.Fatalf("only %d source files were checked; the test is not reading the package", checked)
	}
	// types.go is allowed the Request.Seconds declaration and its doc comment, no more.
	src, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	for _, raw := range strings.Split(string(src), "\n") {
		line := raw
		if k := strings.Index(line, "//"); k >= 0 {
			line = line[:k]
		}
		if strings.Contains(line, "float64") {
			code++
			if !strings.Contains(line, "Seconds float64") {
				t.Errorf("types.go declares a float outside Request.Seconds: %s", strings.TrimSpace(raw))
			}
		}
	}
	if code != 1 {
		t.Errorf("types.go has %d float64 declarations, want exactly one (Request.Seconds)", code)
	}
}

// TestYAMLPriceFieldsAreStrings guarantees yaml.v3 can never hand a price to us as a
// float64: every price-bearing field of the configuration structs is declared as a string.
func TestYAMLPriceFieldsAreStrings(t *testing.T) {
	var found int
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, path string) {
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice:
			walk(rt.Elem(), path)
			return
		case reflect.Struct:
			for i := 0; i < rt.NumField(); i++ {
				f := rt.Field(i)
				found++
				switch f.Type.Kind() {
				case reflect.Float32, reflect.Float64:
					t.Errorf("%s.%s is a float; YAML numbers must be read as strings", path, f.Name)
				case reflect.Struct, reflect.Pointer, reflect.Slice:
					walk(f.Type, path+"."+f.Name)
				}
			}
		}
	}
	walk(reflect.TypeOf(rawCatalog{}), "rawCatalog")
	if found < 20 {
		t.Fatalf("walked only %d fields; the test is not reaching the config structs", found)
	}
}
