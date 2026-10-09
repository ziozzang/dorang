package app

import "testing"

// TestPlanExhaustedRecognizesAPlanNotARateLimit: the alert is for an allowance
// that is used up, and an ordinary per-minute rate limit is not one.
func TestPlanExhaustedRecognizesAPlanNotARateLimit(t *testing.T) {
	for msg, want := range map[string]bool{
		// Ollama Cloud, measured 2026-10-09.
		"You reached your Pro 5-hour limit. Max is $100/month for $300 of usage, with no 5-hour or weekly caps: https://ollama.com/settings/billing": true,
		"You exceeded your current quota, please check your plan and billing details.":                                                               true,
		"Usage limit reached for the coding plan": true,
		"insufficient_quota":                      true,
		"Insufficient credits":                    true,
		"Rate limit reached for gpt-4o in organization org-x on tokens per min (TPM)": false,
		"Number of request tokens has exceeded your per-minute rate limit":            false,
		"Too many requests": false,
	} {
		if got := planExhausted.MatchString(msg); got != want {
			t.Errorf("%q: %v, want %v", msg, got, want)
		}
	}
}

func TestDecimalNano(t *testing.T) {
	for s, want := range map[string]int64{"2": 2_000_000_000, "0.5": 500_000_000, "1.0000000019": 1_000_000_001} {
		if got, err := decimalNano(s); err != nil || got != want {
			t.Errorf("decimalNano(%q) = %d, %v", s, got, err)
		}
	}
}
