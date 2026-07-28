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

func TestDecimalSyntax(t *testing.T) {
	for _, in := range []string{"0", "5", "20.00", "0.0000025", "-10", "+1.5", "1.25e-7", "2E9"} {
		if err := checkDecimal(in); err != nil {
			t.Errorf("checkDecimal(%q): %v", in, err)
		}
	}
	for _, in := range []string{"", ".", "1.2.3", "1,5", "abc", "1e", "0x10", "5%", "1 000"} {
		if err := checkDecimal(in); err == nil {
			t.Errorf("checkDecimal(%q) accepted an invalid decimal", in)
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
