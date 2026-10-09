package probe

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
)

const (
	openrouterBaseURL = "https://openrouter.ai"
	openrouterPath    = "/api/v1/credits"
)

// openrouterSource reads an OpenRouter account's prepaid credits.
//
// Measured 2026-10-09 with an ordinary inference key:
// {"data":{"total_credits":40,"total_usage":20.660322588}} — USD, as JSON
// numbers, both lifetime figures. What is left is their difference, and what it
// is a share of is everything ever credited.
type openrouterSource struct{}

func (openrouterSource) name() string { return "openrouter" }

func (openrouterSource) endpoint(base string) string {
	return baseOr(base, openrouterBaseURL) + openrouterPath
}

func (openrouterSource) accepts(string) error { return nil }

func (s openrouterSource) request(ctx context.Context, base, token, ua string) (*http.Request, error) {
	return newRequest(ctx, s.endpoint(base), ua, http.Header{"Authorization": {"Bearer " + token}})
}

func (openrouterSource) decode(body []byte) (reading, error) {
	var doc struct {
		Data *struct {
			TotalCredits *json.Number `json:"total_credits"`
			TotalUsage   *json.Number `json:"total_usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return reading{}, errors.New("response is not the expected JSON object")
	}
	if doc.Data == nil || doc.Data.TotalCredits == nil || doc.Data.TotalUsage == nil {
		return reading{}, errors.New("response carries no data.total_credits and data.total_usage")
	}
	total, err1 := numberNano(*doc.Data.TotalCredits)
	used, err2 := numberNano(*doc.Data.TotalUsage)
	if err1 != nil || err2 != nil {
		return reading{}, errors.New("credits are not amounts")
	}
	left := total - used
	return reading{balances: []Balance{{
		Currency: "USD", RemainingNano: left, TotalNano: total, Available: left > 0,
	}}}, nil
}

// numberNano reads a JSON number of currency units as nano-units, through the
// same digit parser a string amount takes. A number in exponent form is
// expanded first; it cannot carry more precision than a float64 had anyway.
func numberNano(n json.Number) (int64, error) {
	s := n.String()
	if f, err := n.Float64(); err == nil && containsExp(s) {
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, errNotAnAmount
		}
		s = formatPlain(f)
	}
	return parseNano(s)
}

func containsExp(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 'e' || s[i] == 'E' {
			return true
		}
	}
	return false
}
