package app

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/shadow"
)

// buildShadow translates the file's shadow: block into shadow.Options and
// builds the observer (DESIGN §14.1).
//
// It returns (nil, nil) when the mode is off, which is what wires nothing at
// all: internal/server reads one nil pointer per request and skips the whole
// mechanism, so a deployment that is not migrating pays for none of it.
func buildShadow(cfg *config.Config, logf func(string, ...any)) (*shadow.Shadower, error) {
	sc := cfg.Shadow
	mode, err := shadow.ParseMode(sc.Mode)
	if err != nil {
		return nil, err
	}
	if mode == shadow.ModeOff {
		return nil, nil
	}

	key, err := shadowKey(sc.Reference.APIKeyEnv)
	if err != nil {
		return nil, err
	}
	limit, err := usdToNano("shadow.max_cost_usd_per_day", sc.MaxCostUSDPerDay)
	if err != nil {
		return nil, err
	}
	est, err := usdToNano("shadow.unpriced_estimate_usd", sc.UnpricedEstimateUSD)
	if err != nil {
		return nil, err
	}

	s, err := shadow.New(shadow.Options{
		Mode:             mode,
		ReferenceURL:     sc.Reference.URL,
		ReferenceKey:     key,
		ReferenceTimeout: sc.Reference.Timeout.Duration(),
		SampleRate:       sc.SampleRate,
		Structural:       sc.Compare.IsStructural(),
		IgnoreFields:     sc.Compare.IgnoreFields,

		MaxCostNanoUSDPerDay:    limit,
		UnpricedEstimateNanoUSD: est,

		QueueSize:       sc.QueueSize,
		Workers:         sc.Workers,
		MaxCaptureBytes: int(sc.Capture.HeadBytes.Bytes() + sc.Capture.TailBytes.Bytes()),

		ReportPath:     config.ExpandPath(sc.Report.Path),
		ReportMaxBytes: sc.Report.MaxBytes.Bytes(),

		Logf: logf,
	})
	if err != nil {
		return nil, fmt.Errorf("app: shadow: %w", err)
	}
	return s, nil
}

// shadowKey resolves the reference gateway's credential.
//
// §14.1 spells this `api_key_env`, so an environment variable is the only
// source the file can express — unlike every other credential in the file,
// which takes key_env / key_file / key_ref (§4.1). A deployment that keeps its
// secrets in a file or a vault therefore cannot configure a shadow reference,
// and this function is where that limitation would be lifted.
func shadowKey(env string) (string, error) {
	if env == "" {
		// A reference gateway that needs no credential is legal — a local
		// instance behind a network boundary, for example — so this is not an
		// error.
		return "", nil
	}
	v, ok := os.LookupEnv(env)
	if !ok || v == "" {
		return "", fmt.Errorf("app: shadow.reference.api_key_env: %s is not set", env)
	}
	return v, nil
}

// usdToNano converts a decimal USD literal to nano-USD without a float
// (DESIGN §8.3).
func usdToNano(path string, d config.Decimal) (int64, error) {
	if d.IsZero() {
		return 0, nil
	}
	n, ok := parseUSDNano(string(d))
	if !ok {
		return 0, fmt.Errorf("app: %s: %q is not a decimal amount", path, string(d))
	}
	return n, nil
}

// parseUSDNano reads a plain decimal into nano-USD. Exponent notation is
// rejected: a cost ceiling written as 5e-3 is a typo waiting to be misread, and
// the config validator already accepts the plain form.
func parseUSDNano(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "eE") {
		return 0, false
	}
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		s = s[1:]
	}
	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" && fracPart == "" {
		return 0, false
	}
	var whole int64
	for i := 0; i < len(intPart); i++ {
		c := intPart[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if whole > (1<<62)/10 {
			return 0, false
		}
		whole = whole*10 + int64(c-'0')
	}
	var frac int64
	for i := 0; i < 9; i++ {
		frac *= 10
		if i < len(fracPart) {
			c := fracPart[i]
			if c < '0' || c > '9' {
				return 0, false
			}
			frac += int64(c - '0')
		}
	}
	for i := 9; i < len(fracPart); i++ {
		if c := fracPart[i]; c < '0' || c > '9' {
			return 0, false
		}
	}
	const nano = 1_000_000_000
	if whole > (1<<62)/nano {
		return 0, false
	}
	n := whole*nano + frac
	if neg {
		n = -n
	}
	return n, true
}

// errShadowReloadUnsupported explains why a running shadow is not swapped.
//
// The daily cost ceiling, the sampled set and the report are all state that a
// swap would reset: a reload every hour would rearm the ceiling every hour, so
// "five dollars a day" would become five dollars an hour. §4.1 says every
// section hot-reloads; this is the one place that is not free, and it is
// refused loudly rather than implemented wrongly.
var errShadowReloadUnsupported = errors.New(
	"app: shadow cannot be reloaded in place: the daily cost ceiling, the sampled " +
		"set and the report are per-process state, and swapping them would rearm " +
		"the ceiling on every reload. Restart to change the shadow section")

// checkShadowUnchanged refuses a reload that changes the shadow section.
func checkShadowUnchanged(old, next *config.Config) error {
	if old == nil {
		return nil
	}
	a, b := old.Shadow, next.Shadow
	same := a.Mode == b.Mode &&
		a.Reference == b.Reference &&
		a.SampleRate == b.SampleRate &&
		a.Compare.IsStructural() == b.Compare.IsStructural() &&
		a.Compare.Semantic == b.Compare.Semantic &&
		equalStrings(a.Compare.IgnoreFields, b.Compare.IgnoreFields) &&
		a.MaxCostUSDPerDay == b.MaxCostUSDPerDay &&
		a.UnpricedEstimateUSD == b.UnpricedEstimateUSD &&
		a.QueueSize == b.QueueSize && a.Workers == b.Workers &&
		a.Capture == b.Capture && a.Report == b.Report
	if !same {
		return errShadowReloadUnsupported
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
