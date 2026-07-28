package probe

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// A reset instant arrives in whichever shape the provider felt like. Every one
// that is not unambiguous decodes to the zero value, because §7.5a(c) reads a
// zero as "no opinion" and a wrong one as a real deadline.
func TestResetTimeDecoding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		json string
		want time.Time
	}{
		{"unix milliseconds", `1777819631597`, time.UnixMilli(1777819631597).UTC()},
		{"unix seconds", `1777819631`, time.Unix(1777819631, 0).UTC()},
		{"iso 8601 with offset", `"2026-04-11T07:00:00.528743+00:00"`,
			time.Date(2026, 4, 11, 7, 0, 0, 528743000, time.UTC)},
		{"iso 8601 with Z", `"2026-04-11T07:00:00Z"`, time.Date(2026, 4, 11, 7, 0, 0, 0, time.UTC)},
		{"iso 8601 with a real offset", `"2026-04-11T15:00:00+08:00"`,
			time.Date(2026, 4, 11, 7, 0, 0, 0, time.UTC)},
		{"stringified unix milliseconds", `"1777819631597"`, time.UnixMilli(1777819631597).UTC()},

		// Everything below is unknown rather than guessed.
		{"null", `null`, time.Time{}},
		{"zero", `0`, time.Time{}},
		{"negative", `-1777819631`, time.Time{}},
		{"empty string", `""`, time.Time{}},
		{"prose", `"soon"`, time.Time{}},
		{"an object", `{"at":1}`, time.Time{}},
		// No zone named: these providers publish from UTC+8, and being eight
		// hours wrong about a five-hour window inverts the signal.
		{"no zone", `"2026-04-11T07:00:00"`, time.Time{}},
		{"space separated, no zone", `"2026-04-11 07:00:00"`, time.Time{}},
		// A unit confusion, not an instant in the year 5138.
		{"microseconds", `1777819631597000`, time.Time{}},
		{"implausibly old", `100000`, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got resetTime
			if err := json.Unmarshal([]byte(tc.json), &got); err != nil {
				t.Fatalf("Unmarshal(%s): %v", tc.json, err)
			}
			if !got.Time.Equal(tc.want) {
				t.Errorf("= %v, want %v", got.Time, tc.want)
			}
		})
	}
}

// A malformed reset must not discard the percentage beside it: they are
// independent facts and only one of them is broken.
func TestResetTimeFailureDoesNotFailTheEnclosingObject(t *testing.T) {
	t.Parallel()
	var l zaiLimit
	if err := json.Unmarshal([]byte(
		`{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":42,"nextResetTime":"whenever"}`), &l); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !l.NextResetTime.Time.IsZero() {
		t.Error("an unparseable reset produced an instant")
	}
	if v, ok := l.Percentage.Value, l.Percentage.Set; !ok || v != 42 {
		t.Errorf("percentage = %v/%v, want 42", v, ok)
	}
}

func TestUsedPercent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      float64
		want    float64
		wantOK  bool
		comment string
	}{
		{name: "zero", in: 0, want: 0, wantOK: true},
		{name: "ordinary", in: 37.5, want: 37.5, wantOK: true},
		{name: "full", in: 100, want: 100, wantOK: true},
		{name: "over full clamps to full", in: 105, want: 100, wantOK: true,
			comment: "over-consumption is real; clamping up is the pessimistic direction"},
		{name: "negative is unknown", in: -1, wantOK: false,
			comment: "not zero usage: the field is not what the decoder thinks it is"},
		{name: "NaN is unknown", in: math.NaN(), wantOK: false},
		{name: "Inf is unknown", in: math.Inf(1), wantOK: false},
		// The reading this deliberately does not make: 0.5 could be half a
		// percent or half the allowance, and rescaling costs an order of
		// magnitude in the direction that keeps routing into a spent account.
		{name: "a fraction is taken at face value", in: 0.5, want: 0.5, wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := usedPercent(tc.in)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Errorf("usedPercent(%v) = %v,%v want %v,%v", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestParseNano(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "110.00", want: 110_000_000_000},
		{in: "1.85", want: 1_850_000_000},
		{in: "0", want: 0},
		{in: "0.000000001", want: 1},
		{in: "-2.5", want: -2_500_000_000},
		{in: "+3", want: 3_000_000_000},
		{in: ".5", want: 500_000_000},
		{in: " 7.25 ", want: 7_250_000_000},
		// Truncated, never rounded up: a balance is not made larger by
		// reporting it.
		{in: "0.9999999999", want: 999_999_999},
		{in: "", wantErr: true},
		{in: "many", wantErr: true},
		{in: "1.2.3", wantErr: true},
		{in: "1e9", wantErr: true},
		{in: "99999999999999999999", wantErr: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseNano(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseNano(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("parseNano(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestNumberDecoding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		json    string
		want    float64
		wantSet bool
	}{
		{json: `5`, want: 5, wantSet: true},
		{json: `12.5`, want: 12.5, wantSet: true},
		{json: `"7"`, want: 7, wantSet: true},
		{json: `null`, wantSet: false},
		{json: `"abc"`, wantSet: false},
		{json: `{}`, wantSet: false},
		{json: `[]`, wantSet: false},
	} {
		t.Run(tc.json, func(t *testing.T) {
			t.Parallel()
			var n number
			if err := json.Unmarshal([]byte(tc.json), &n); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if n.Set != tc.wantSet || (n.Set && n.Value != tc.want) {
				t.Errorf("= %v,%v want %v,%v", n.Value, n.Set, tc.want, tc.wantSet)
			}
		})
	}
}

func TestNumberInt(t *testing.T) {
	t.Parallel()
	if v, ok := (number{Value: 5, Set: true}).Int(); !ok || v != 5 {
		t.Errorf("Int() = %v,%v", v, ok)
	}
	if _, ok := (number{Value: 5.5, Set: true}).Int(); ok {
		t.Error("a fractional value passed as an integer")
	}
	if _, ok := (number{}).Int(); ok {
		t.Error("an unset value passed as an integer")
	}
	if _, ok := (number{Value: math.NaN(), Set: true}).Int(); ok {
		t.Error("NaN passed as an integer")
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{in: "", want: 0},
		{in: "60", want: time.Minute},
		{in: "0", want: 0},
		{in: "-5", want: 0},
		{in: "Tue, 28 Jul 2026 12:05:00 GMT", want: 5 * time.Minute},
		{in: "Tue, 28 Jul 2026 11:55:00 GMT", want: 0},
		{in: "later", want: 0},
		// Capped: a header bug must not silence a credential for a day.
		{in: "86400", want: maxRetryAfter},
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := parseRetryAfter(tc.in, now); got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestLabelOfIsAWhitelist(t *testing.T) {
	t.Parallel()
	vocab := []string{"TOKENS_LIMIT", "TIME_LIMIT"}
	if got := labelOf(vocab, "tokens_limit"); got != "tokens_limit" {
		t.Errorf("= %q", got)
	}
	if got := labelOf(vocab, " TIME_LIMIT "); got != "time_limit" {
		t.Errorf("= %q", got)
	}
	for _, in := range []string{"", "NEW_LIMIT", testSecret, "<script>", "tokens_limit_x"} {
		if got := labelOf(vocab, in); got != labelOther {
			t.Errorf("labelOf(%q) = %q, want %q", in, got, labelOther)
		}
	}
}

func TestCurrencyCode(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"USD": "USD", "cny": "CNY", " eur ": "EUR",
		"": "", "US": "", "USDT": "", "U5D": "", testSecret: "",
	} {
		if got := currencyCode(in); got != want {
			t.Errorf("currencyCode(%q) = %q, want %q", in, got, want)
		}
	}
}
