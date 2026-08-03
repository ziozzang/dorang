package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseByteSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "", want: 0},
		{in: "0", want: 0},
		{in: "4096", want: 4096},
		{in: "64MiB", want: 64 << 20},
		{in: "8GiB", want: 8 << 30},
		{in: "2GiB", want: 2 << 30},
		{in: "1KiB", want: 1024},
		{in: "1TiB", want: 1 << 40},
		{in: "1PiB", want: 1 << 50},
		{in: "1KB", want: 1000},
		{in: "5MB", want: 5_000_000},
		{in: "3GB", want: 3_000_000_000},
		{in: "1B", want: 1},
		{in: "512 KiB", want: 512 << 10},
		{in: "64mib", want: 64 << 20},
		{in: "64MIB", want: 64 << 20},
		{in: "1.5MiB", want: 1572864},
		{in: "-1", wantErr: true},
		{in: "-5MiB", wantErr: true},
		{in: "MiB", wantErr: true},
		{in: "64 mebibytes", wantErr: true},
		{in: "64M", wantErr: true},
		{in: "1e9", wantErr: true},
		{in: "1_000", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "99999999999999999999GiB", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseByteSize(tc.in)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("ParseByteSize(%q) = %d, want an error", tc.in, got)
		case !tc.wantErr && err != nil:
			t.Errorf("ParseByteSize(%q): %v", tc.in, err)
		case !tc.wantErr && got != tc.want:
			t.Errorf("ParseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestByteSizeString(t *testing.T) {
	for _, tc := range []struct {
		in   ByteSize
		want string
	}{
		{0, "0"},
		{4096, "4KiB"},
		{64 << 20, "64MiB"},
		{8 << 30, "8GiB"},
		{1023, "1023"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("ByteSize(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "", want: 0},
		{in: "180s", want: 180 * time.Second},
		{in: "600s", want: 600 * time.Second},
		{in: "500ms", want: 500 * time.Millisecond},
		{in: "250ms", want: 250 * time.Millisecond},
		{in: "1h", want: time.Hour},
		{in: "5m", want: 5 * time.Minute},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "200ms", want: 200 * time.Millisecond},
		{in: "600", want: 600 * time.Second}, // a bare number is seconds
		{in: "0", want: 0},
		{in: "-5s", wantErr: true},
		{in: "-1", wantErr: true},
		{in: "soon", wantErr: true},
		{in: "5 seconds", wantErr: true},
		{in: "1hour", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseDuration(tc.in)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("ParseDuration(%q) = %v, want an error", tc.in, got)
		case !tc.wantErr && err != nil:
			t.Errorf("ParseDuration(%q): %v", tc.in, err)
		case !tc.wantErr && got != tc.want:
			t.Errorf("ParseDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestDecimalSyntax pins what a decimal is here. The two exponent forms moved
// from the accepted list to the refused one, which is the defect this test used
// to hold open: internal/pricing has always refused exponent notation, so
// `dorangctl config lint` answered `ok` on a rate card the gateway then refused
// to assemble. See TestLintAndTheEngineAgreeOnADecimal for the other half.
func TestDecimalSyntax(t *testing.T) {
	for _, in := range []string{"0", "5", "20.00", "0.0000025", "-10", "+1.5",
		"2.50", "0.000000000001", "18446744073709551615"} {
		if err := checkDecimal(in); err != nil {
			t.Errorf("checkDecimal(%q): %v", in, err)
		}
	}
	for _, in := range []string{
		"", ".", "1.2.3", "1,5", "abc", "1e", "0x10", "5%", "1 000",
		// Exponent notation: legal YAML, legal in a float printer's output, and
		// not a decimal internal/pricing will parse.
		"1.25e-7", "2E9", "1e-6", "1E+3",
		// Finer than 10^-12, which internal/pricing refuses rather than truncate.
		"0.0000000000001",
		// More significant digits than the engine's significand holds.
		"18446744073709551616",
	} {
		if err := checkDecimal(in); err == nil {
			t.Errorf("checkDecimal(%q) accepted an invalid decimal", in)
		}
	}
}

// TestScaleDecimalMovesThePointExactly is the arithmetic the importer's unit
// conversion is made of: a decimal point moves, nothing rounds, and no value
// passes through a float (§8.3).
func TestScaleDecimalMovesThePointExactly(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"0.0000025", 6, "2.5"},   // $2.50 per million, written per token
		{"0.00001", 6, "10"},      // $10 per million
		{"0.00000025", 6, "0.25"}, // a cached-read rate
		{"0.000000125", 6, "0.125"},
		{"2.5e-06", 6, "2.5"},    // the same rate as a float printer writes it
		{"1.5E-6", 6, "1.5"},     // and with the other spelling of the exponent
		{"0.000125", 3, "0.125"}, // per character to per 1,000 characters
		{"0.0001", 0, "0.0001"},  // per second is already per second
		{"-0.0000025", 6, "-2.5"},
		{"0", 6, "0"},
		{"0.000000", 6, "0"},
		{"3", 3, "3000"},
		{"0.5", 0, "0.5"},
	}
	for _, tc := range cases {
		got, ok := scaleDecimal(tc.in, tc.n)
		if !ok {
			t.Errorf("scaleDecimal(%q, %d) refused a decimal", tc.in, tc.n)
			continue
		}
		if got != tc.want {
			t.Errorf("scaleDecimal(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
		if err := checkDecimal(got); err != nil {
			t.Errorf("scaleDecimal(%q, %d) = %q, which this package will not store: %v",
				tc.in, tc.n, got, err)
		}
	}
	for _, in := range []string{"", "abc", "1.2.3", "1e", "1e999999999", "--1"} {
		if got, ok := scaleDecimal(in, 6); ok {
			t.Errorf("scaleDecimal(%q, 6) = %q, want a refusal", in, got)
		}
	}
}

// TestDecimalBelowPow10 is the comparison the rate advisory is made of.
func TestDecimalBelowPow10(t *testing.T) {
	below := []string{"0.0000025", "0.000001", "0.000009", "-0.0000025", "2.5e-6"}
	for _, in := range below {
		if !decimalBelowPow10(in, -5) {
			t.Errorf("decimalBelowPow10(%q, -5) = false, want true", in)
		}
	}
	// 0.00001 is exactly the threshold and is not below it; 0.02 is the
	// cheapest real card; 0 is a free model, not a suspicious price.
	for _, in := range []string{"0.00001", "0.02", "2.50", "0", "0.000", "", "abc"} {
		if decimalBelowPow10(in, -5) {
			t.Errorf("decimalBelowPow10(%q, -5) = true, want false", in)
		}
	}
}

// TestScalarErrorsCarryTheLine checks that a bad scalar is reported where it is.
func TestScalarErrorsCarryTheLine(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "bad size",
			yaml: "routing:\n  prefix:\n    max_bytes: 64 megabytes\n",
			want: "invalid size",
		},
		{
			name: "bad duration",
			yaml: "server:\n  request_timeout: soon\n",
			want: "invalid duration",
		},
		{
			name: "bad decimal",
			yaml: "shadow:\n  mode: mirror\n  max_cost_usd_per_day: five dollars\n",
			want: "invalid decimal",
		},
		{
			name: "size must be a scalar",
			yaml: "routing:\n  prefix:\n    max_bytes: [64]\n",
			want: "size must be a scalar",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not mention %q: %v", tc.want, err)
			}
			if !strings.Contains(err.Error(), "line ") {
				t.Errorf("error does not carry a line number: %v", err)
			}
		})
	}
}

// TestSizesAndDurationsFromTheDesign parses the values the design writes.
func TestSizesAndDurationsFromTheDesign(t *testing.T) {
	c := mustLoad(t, `
server: {request_timeout: 600s, shutdown_grace: 30s}
routing:
  prefix: {chunk_bytes: 4096, max_bytes: 64MiB, ttl: 1h}
  sticky: {ttl: 1h, purge_interval: 5m}
metering:
  trace: {daily_byte_budget: 8GiB}
  spool: {max_bytes: 2GiB}
  flush_interval: 250ms
`)
	for _, tc := range []struct {
		name string
		got  any
		want any
	}{
		{"request_timeout", c.Server.RequestTimeout.Duration(), 600 * time.Second},
		{"shutdown_grace", c.Server.ShutdownGrace.Duration(), 30 * time.Second},
		{"chunk_bytes", c.Routing.Prefix.ChunkBytes.Bytes(), int64(4096)},
		{"max_bytes", c.Routing.Prefix.MaxBytes.Bytes(), int64(64 << 20)},
		{"prefix ttl", c.Routing.Prefix.TTL.Duration(), time.Hour},
		{"sticky ttl", c.Routing.Sticky.TTL.Duration(), time.Hour},
		{"purge_interval", c.Routing.Sticky.PurgeInterval.Duration(), 5 * time.Minute},
		{"daily_byte_budget", c.Metering.Trace.DailyByteBudget.Bytes(), int64(8 << 30)},
		{"spool max_bytes", c.Metering.Spool.MaxBytes.Bytes(), int64(2 << 30)},
		{"flush_interval", c.Metering.FlushInterval.Duration(), 250 * time.Millisecond},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}
