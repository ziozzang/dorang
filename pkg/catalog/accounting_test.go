package catalog

import "testing"

// TestAccountingIsDeclaredForTheMeasuredHosts pins what was measured on 2026-10-09:
// where each host states a request's cost, how DeepInfra is made to state a
// recording's length, and which usage API each account offers.
func TestAccountingIsDeclaredForTheMeasuredHosts(t *testing.T) {
	c := loadWith(t, "version: 1\n")
	for _, tc := range []struct {
		kind, cost, unit, duration, api string
	}{
		{"openrouter", "usage.cost", ReportedCostUSD, "", UsageAPIOpenRouter},
		{"deepinfra", "usage.estimated_cost", ReportedCostUSD, TranscriptionDurationVerboseJSON, UsageAPIDeepInfra},
		{"openai", "", "", "", ""},
	} {
		kd, ok := c.Kind(tc.kind)
		if !ok {
			t.Fatalf("kind %q is not declared", tc.kind)
		}
		if kd.ReportedCost != tc.cost || kd.ReportedCostUnit != tc.unit ||
			kd.TranscriptionDuration != tc.duration || kd.UsageAPI != tc.api {
			t.Errorf("%s: reported_cost=%q unit=%q transcription_duration=%q usage_api=%q; "+
				"want %q %q %q %q", tc.kind, kd.ReportedCost, kd.ReportedCostUnit,
				kd.TranscriptionDuration, kd.UsageAPI, tc.cost, tc.unit, tc.duration, tc.api)
		}
	}
}

func TestAccountingValuesAreClosed(t *testing.T) {
	for name, tc := range map[string]struct{ field, want string }{
		"an unknown duration mode": {"transcription_duration: srt", "transcription_duration"},
		"a path with an index":     {"reported_cost: usage.cost[0]", "reported_cost"},
		"an empty segment":         {"reported_cost: usage..cost", "empty segment"},
		"an unknown unit":          {"reported_cost: usage.cost\n    reported_cost_unit: eur", "reported_cost_unit"},
		"a unit with no path":      {"reported_cost_unit: usd", "without reported_cost"},
		"an unknown usage api":     {"usage_api: stripe", "usage_api"},
	} {
		t.Run(name, func(t *testing.T) {
			mustLoadFail(t, "version: 1\nkinds:\n  odd-host:\n    api: openai-chat\n    "+tc.field+"\n", tc.want)
		})
	}
}
