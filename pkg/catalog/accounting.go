package catalog

import (
	"fmt"
	"strings"
)

// Accepted values of the kind-level accounting fields.
const (
	TranscriptionDurationVerboseJSON = "verbose_json"

	ReportedCostUSD      = "usd"
	ReportedCostUSDCents = "usd_cents"

	UsageAPIDeepInfra  = "deepinfra"
	UsageAPIOpenRouter = "openrouter"
)

// accounting is the resolved form of a kind's four accounting fields.
type accounting struct {
	TranscriptionDuration string
	ReportedCost          string
	ReportedCostUnit      string
	UsageAPI              string
}

// resolveAccounting validates a kind's accounting fields.
//
// Every value is a closed set or a checked path, and an unknown one is a load
// error rather than an ignored one: a reported-cost path that names nothing
// reads as "this vendor states no cost" for ever, and nothing would say so.
func resolveAccounting(name string, in *kindDoc) (accounting, []error) {
	var (
		out  accounting
		errs []error
	)
	switch v := in.TranscriptionDuration.get(); v {
	case "", TranscriptionDurationVerboseJSON:
		out.TranscriptionDuration = v
	default:
		errs = append(errs, fmt.Errorf("kinds[%q]: transcription_duration: %q is not %q",
			name, v, TranscriptionDurationVerboseJSON))
	}

	path, unit := in.ReportedCost.get(), in.ReportedCostUnit.get()
	if path != "" {
		if err := CheckCostPath(path); err != nil {
			errs = append(errs, fmt.Errorf("kinds[%q]: reported_cost: %w", name, err))
		}
		if unit == "" {
			unit = ReportedCostUSD
		}
	}
	switch {
	case unit == "":
	case path == "":
		errs = append(errs, fmt.Errorf("kinds[%q]: reported_cost_unit without reported_cost: "+
			"a unit for a figure nobody reads", name))
	case unit != ReportedCostUSD && unit != ReportedCostUSDCents:
		errs = append(errs, fmt.Errorf("kinds[%q]: reported_cost_unit: %q is not %q or %q",
			name, unit, ReportedCostUSD, ReportedCostUSDCents))
	}
	out.ReportedCost, out.ReportedCostUnit = path, unit

	switch v := in.UsageAPI.get(); v {
	case "", UsageAPIDeepInfra, UsageAPIOpenRouter:
		out.UsageAPI = v
	default:
		errs = append(errs, fmt.Errorf("kinds[%q]: usage_api: %q is not %q or %q",
			name, v, UsageAPIDeepInfra, UsageAPIOpenRouter))
	}
	return out, errs
}

// CheckCostPath validates a dotted JSON member path such as "usage.cost".
//
// Members only — no array index and no wildcard. Every vendor measured states
// the figure at a fixed member of an object, and a path language that can
// select one of several numbers is a path that can select the wrong one.
func CheckCostPath(path string) error {
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return fmt.Errorf("%q has an empty segment", path)
		}
		for _, r := range seg {
			if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				return fmt.Errorf("%q: segment %q may hold only letters, digits, '_' and '-'", path, seg)
			}
		}
	}
	return nil
}
