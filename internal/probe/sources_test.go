package probe

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
)

// The decoders are pure, so every payload shape a provider has been observed to
// return is a table entry with no HTTP in it.

func TestZAIDecode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
		check   func(t *testing.T, r reading)
	}{
		{
			name: "the short observed shape, unit without number",
			body: `{"code":0,"success":true,"data":{"limits":[
			  {"type":"TOKENS_LIMIT","unit":3,"percentage":16,"nextResetTime":1777819631597}]}}`,
			check: func(t *testing.T, r reading) {
				// A missing count means one of the unit.
				if got := r.windows[0].Window; got != quota.Rolling(time.Hour) {
					t.Errorf("window = %v, want 1h", got)
				}
				if !r.windows[0].Mapped {
					t.Error("a token window with a known length was not filed")
				}
			},
		},
		{
			name: "minutes",
			body: `{"success":true,"data":{"limits":[
			  {"type":"TOKENS_LIMIT","unit":5,"number":90,"percentage":1}]}}`,
			check: func(t *testing.T, r reading) {
				if got := r.windows[0].Window; got != quota.Rolling(90*time.Minute) {
					t.Errorf("window = %v, want 90m", got)
				}
				if !r.windows[0].ResetAt.IsZero() {
					t.Error("a missing reset produced an instant")
				}
			},
		},
		{
			name: "an unrecognized unit leaves the length unknown",
			body: `{"success":true,"data":{"limits":[
			  {"type":"TOKENS_LIMIT","unit":7,"number":5,"percentage":50,"nextResetTime":1777819631597}]}}`,
			check: func(t *testing.T, r reading) {
				w := r.windows[0]
				if w.Mapped {
					t.Error("a window of unknown length was filed under a key")
				}
				if w.Label != "tokens_limit" {
					t.Errorf("label = %q, want the unqualified type", w.Label)
				}
				// The percentage and the reset are still true.
				if !w.Known || w.UsedPercent != 50 || w.ResetAt.IsZero() {
					t.Errorf("usable figures were discarded: %+v", w)
				}
			},
		},
		{
			name: "a missing unit leaves the length unknown",
			body: `{"success":true,"data":{"limits":[{"type":"TOKENS_LIMIT","percentage":50}]}}`,
			check: func(t *testing.T, r reading) {
				if r.windows[0].Mapped {
					t.Error("a window with no unit was filed under a key")
				}
			},
		},
		{
			name: "a percentage over 100 clamps rather than overflowing",
			body: `{"success":true,"data":{"limits":[
			  {"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":137}]}}`,
			check: func(t *testing.T, r reading) {
				if !r.windows[0].Known || r.windows[0].UsedPercent != 100 {
					t.Errorf("percentage = %v", r.windows[0].UsedPercent)
				}
			},
		},
		{
			name: "a negative percentage is unknown, not zero usage",
			body: `{"success":true,"data":{"limits":[
			  {"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":-4,"nextResetTime":1777819631597}]}}`,
			check: func(t *testing.T, r reading) {
				if r.windows[0].Known {
					t.Error("a negative percentage was read as a usage figure")
				}
				if !r.windows[0].Mapped || r.windows[0].ResetAt.IsZero() {
					t.Error("the reset instant was discarded with the bad percentage")
				}
			},
		},
		{
			name: "a missing percentage is unknown",
			body: `{"success":true,"data":{"limits":[
			  {"type":"TOKENS_LIMIT","unit":3,"number":5,"nextResetTime":1777819631597}]}}`,
			check: func(t *testing.T, r reading) {
				if r.windows[0].Known {
					t.Error("an absent percentage was read as zero usage")
				}
			},
		},
		{
			name: "an absurd window length is refused",
			body: `{"success":true,"data":{"limits":[
			  {"type":"TOKENS_LIMIT","unit":1,"number":9999,"percentage":5}]}}`,
			check: func(t *testing.T, r reading) {
				if r.windows[0].Mapped {
					t.Error("a 27-year window was filed under a key")
				}
			},
		},
		{
			name: "a stringified numeric field still decodes",
			body: `{"success":true,"data":{"limits":[
			  {"type":"TOKENS_LIMIT","unit":"3","number":"5","percentage":"37"}]}}`,
			check: func(t *testing.T, r reading) {
				w := r.windows[0]
				if !w.Mapped || w.Window != quota.Rolling(5*time.Hour) || w.UsedPercent != 37 {
					t.Errorf("window = %+v", w)
				}
			},
		},
		{name: "not JSON", body: `<html>502 Bad Gateway</html>`, wantErr: true},
		{name: "no data object", body: `{"code":401,"msg":"unauthorized"}`, wantErr: true},
		{name: "the provider reported failure", body: `{"code":500,"success":false,"data":{"limits":[]}}`, wantErr: true},
		{
			name:  "no limits at all",
			body:  `{"code":200,"success":true,"data":{"limits":[]}}`,
			check: func(t *testing.T, r reading) { assertEmpty(t, r) },
		},
		{
			name:  "limits is null",
			body:  `{"code":200,"success":true,"data":{}}`,
			check: func(t *testing.T, r reading) { assertEmpty(t, r) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := zaiSource{}.decode([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("decode error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && tc.check != nil {
				tc.check(t, r)
			}
		})
	}
}

func assertEmpty(t *testing.T, r reading) {
	t.Helper()
	if len(r.windows) != 0 || len(r.balances) != 0 {
		t.Errorf("figures were invented from an empty payload: %+v", r)
	}
}

// A prober whose decoder produced nothing reports a failed read rather than a
// success with no figures — adopting an empty result would refresh the
// staleness of numbers that were never re-read.
func TestEmptyReadingIsAFailedRead(t *testing.T) {
	t.Parallel()
	s := serve(t, jsonHandler(200, `{"code":200,"success":true,"data":{"limits":[]}}`))
	p := newProbe(t, "zai", s.URL)
	_, err := p.Read(context.Background(), testCredential("zai"))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("error = %v, want ErrMalformed", err)
	}
}

func TestDeepSeekDecode(t *testing.T) {
	t.Parallel()
	const ok = `{"is_available":true,"balance_infos":[
	  {"currency":"USD","total_balance":"1.85","granted_balance":"0.00","topped_up_balance":"1.85"}]}`

	t.Run("the documented shape", func(t *testing.T) {
		t.Parallel()
		r, err := deepseekSource{}.decode([]byte(ok))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(r.windows) != 0 {
			t.Error("a balance produced a window; there is no ceiling and no reset to make one from")
		}
		if len(r.balances) != 1 {
			t.Fatalf("balances = %d", len(r.balances))
		}
		b := r.balances[0]
		if b.Currency != "USD" || b.RemainingNano != 1_850_000_000 || !b.Available {
			t.Errorf("balance = %+v", b)
		}
	})

	t.Run("two currencies, unavailable", func(t *testing.T) {
		t.Parallel()
		r, err := deepseekSource{}.decode([]byte(`{"is_available":false,"balance_infos":[
		  {"currency":"CNY","total_balance":"110.00"},{"currency":"USD","total_balance":"0.00"}]}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(r.balances) != 2 {
			t.Fatalf("balances = %d", len(r.balances))
		}
		// CNY is reported as CNY. An exchange rate this package invented would
		// be a made-up number in a field a caller reads as measured.
		if r.balances[0].Currency != "CNY" || r.balances[0].RemainingNano != 110_000_000_000 {
			t.Errorf("balance = %+v", r.balances[0])
		}
		for _, b := range r.balances {
			if b.Available {
				t.Error("is_available:false was not carried")
			}
		}
	})

	t.Run("an unparseable amount is dropped, not read as empty", func(t *testing.T) {
		t.Parallel()
		r, err := deepseekSource{}.decode([]byte(
			`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"unknown"}]}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(r.balances) != 0 {
			t.Errorf("an unparseable amount produced a balance: %+v", r.balances)
		}
	})

	for _, tc := range []struct{ name, body string }{
		{"not JSON", `<html>502</html>`},
		{"no balance_infos", `{"is_available":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := (deepseekSource{}).decode([]byte(tc.body)); err == nil {
				t.Error("want an error")
			}
		})
	}
}

// The Anthropic payload as the endpoint is reported to return it, including the
// nulls a plan without a per-model window produces.
const anthropicOK = `{
  "five_hour": {"utilization": 33.0, "resets_at": "2026-04-11T07:00:00.528743+00:00"},
  "seven_day": {"utilization": 13.0, "resets_at": "2026-04-17T00:59:59.951713+00:00"},
  "seven_day_opus": null,
  "seven_day_sonnet": {"utilization": 1.0, "resets_at": "2026-04-16T03:00:00.951719+00:00"},
  "extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null}
}`

func TestAnthropicDecode(t *testing.T) {
	t.Parallel()
	r, err := anthropicSource{}.decode([]byte(anthropicOK))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	byLabel := map[string]Window{}
	for _, w := range r.windows {
		byLabel[w.Label] = w
	}
	if len(byLabel) != 3 {
		t.Fatalf("windows = %v; the nulls should have produced nothing", byLabel)
	}

	fh := byLabel[anthropicFiveHour]
	if !fh.Mapped || fh.Window != quota.Rolling(5*time.Hour) || fh.Metric != quota.MetricTokensTotal {
		t.Errorf("five_hour = %+v", fh)
	}
	if !fh.Known || fh.UsedPercent != 33 {
		t.Errorf("five_hour utilization = %v", fh.UsedPercent)
	}
	want := time.Date(2026, 4, 11, 7, 0, 0, 528743000, time.UTC)
	if !fh.ResetAt.Equal(want) {
		t.Errorf("five_hour reset = %v, want %v", fh.ResetAt, want)
	}
	if fh.Used != 0 || fh.Limit != 0 {
		t.Error("a utilization percentage was turned into an absolute figure")
	}

	sd := byLabel[anthropicSevenDay]
	if !sd.Mapped || sd.Window != quota.Rolling(7*24*time.Hour) {
		t.Errorf("seven_day = %+v", sd)
	}

	// A per-model window is carried but not filed: a quota key has no model
	// dimension, so filing it would report one model's usage as the plan's.
	sm := byLabel["seven_day_sonnet"]
	if sm.Mapped {
		t.Error("a per-model window was filed under the subscription's key")
	}
	if !sm.Known || sm.UsedPercent != 1 {
		t.Errorf("seven_day_sonnet figures were discarded: %+v", sm)
	}
}

func TestAnthropicDecodeIsOrderStable(t *testing.T) {
	t.Parallel()
	first, err := anthropicSource{}.decode([]byte(anthropicOK))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for range 20 {
		got, err := anthropicSource{}.decode([]byte(anthropicOK))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		for i := range got.windows {
			if got.windows[i].Label != first.windows[i].Label {
				t.Fatalf("window order depends on map iteration: %v vs %v",
					got.windows[i].Label, first.windows[i].Label)
			}
		}
	}
}

func TestAnthropicDecodeUnexpectedPayloads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
		wantLen int
	}{
		{name: "not JSON", body: `<html>502</html>`, wantErr: true},
		{name: "a JSON array", body: `[1,2,3]`, wantErr: true},
		{name: "an empty object", body: `{}`, wantLen: 0},
		{name: "an error envelope", body: `{"error":{"type":"x","message":"y"}}`, wantLen: 0},
		{name: "scalars", body: `{"five_hour": 33}`, wantLen: 0},
		{name: "a new key is carried, not dropped", body: `{"nine_day":{"utilization":5}}`, wantLen: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := anthropicSource{}.decode([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && len(r.windows) != tc.wantLen {
				t.Errorf("windows = %d, want %d: %+v", len(r.windows), tc.wantLen, r.windows)
			}
			if tc.name == "a new key is carried, not dropped" && err == nil {
				if r.windows[0].Mapped || r.windows[0].Label != labelOther {
					t.Errorf("an unknown key was filed or named: %+v", r.windows[0])
				}
			}
		})
	}
}

// An Anthropic API key cannot read this endpoint. Refusing beats spending a
// request per poll to be told 401, which is how a prober gets an account
// rate-limited.
func TestAnthropicRefusesAnAPIKeyWithoutAsking(t *testing.T) {
	t.Parallel()
	var called bool
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		jsonHandler(200, anthropicOK)(w, r)
	})
	p := newProbe(t, "anthropic", s.URL)
	cred := quota.NewCredential("c", "anthropic", "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAA") // pragma: allowlist secret — test fixture
	_, err := p.Read(context.Background(), cred)
	if !errors.Is(err, ErrWrongCredentialKind) {
		t.Fatalf("error = %v, want ErrWrongCredentialKind", err)
	}
	if called {
		t.Error("a request was spent on a credential the endpoint cannot accept")
	}
	if strings.Contains(err.Error(), "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAA") { // pragma: allowlist secret — test fixture
		t.Errorf("the refusal quotes the credential: %v", err)
	}
}

func TestAnthropicAcceptsAnOAuthToken(t *testing.T) {
	t.Parallel()
	var gotAuth, gotBeta string
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta = r.Header.Get("Authorization"), r.Header.Get("Anthropic-Beta")
		jsonHandler(200, anthropicOK)(w, r)
	})
	p := newProbe(t, "anthropic", s.URL)
	cred := quota.NewCredential("c", "anthropic", "sk-ant-oat01-BBBBBBBBBBBBBBBBBBBB") // pragma: allowlist secret — test fixture
	snap, err := p.Read(context.Background(), cred)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if gotAuth != "Bearer sk-ant-oat01-BBBBBBBBBBBBBBBBBBBB" { // pragma: allowlist secret — test fixture
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotBeta != anthropicOAuthBeta {
		t.Errorf("anthropic-beta = %q", gotBeta)
	}
	if len(snap.ProbeResult().Windows) != 2 {
		t.Errorf("quota windows = %d, want the five-hour and seven-day ones",
			len(snap.ProbeResult().Windows))
	}
}

func TestDeepSeekEndToEnd(t *testing.T) {
	t.Parallel()
	s := serve(t, jsonHandler(200,
		`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"1.85"}]}`))
	p := newProbe(t, "deepseek", s.URL)
	snap, err := p.Read(context.Background(), testCredential("deepseek"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snap.Balances) != 1 {
		t.Fatalf("balances = %d", len(snap.Balances))
	}
	// Nothing reaches quota: a balance is not a window, so §6.2 has nothing to
	// combine and §7.5a(c) has nothing to expire.
	if got := snap.ProbeResult(); len(got.Windows) != 0 {
		t.Errorf("a balance produced quota windows: %+v", got.Windows)
	}
}
