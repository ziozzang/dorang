package probe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/ziozzang/dorang/internal/quota"
)

const (
	deepinfraBaseURL = "https://api.deepinfra.com"
	deepinfraPath    = "/payment/checklist"
)

// deepinfraSource reads a DeepInfra account's monthly spend limit and credit.
//
// Measured 2026-10-09: GET /payment/checklist answers the account's billing
// checklist — numeric `limit` (the monthly spend limit, USD), `recent` (spent
// this month, USD), `stripe_balance` (the Stripe customer balance, USD;
// NEGATIVE is credit) and `suspended`, beside the billing address and the
// payment method. Only the four are decoded: the rest is personal data and
// never enters a snapshot, an error or a log.
//
// The spend limit is an ACCOUNT-wide figure, not an allowance of the
// credential, so its window is AlertOnly and never gates traffic.
type deepinfraSource struct{}

func (deepinfraSource) name() string { return "deepinfra" }

func (deepinfraSource) endpoint(base string) string {
	return baseOr(base, deepinfraBaseURL) + deepinfraPath
}

func (deepinfraSource) accepts(string) error { return nil }

func (s deepinfraSource) request(ctx context.Context, base, token, ua string) (*http.Request, error) {
	return newRequest(ctx, s.endpoint(base), ua, http.Header{"Authorization": {"Bearer " + token}})
}

func (deepinfraSource) decode(body []byte) (reading, error) {
	var doc struct {
		Limit         *json.Number `json:"limit"`
		Recent        *json.Number `json:"recent"`
		StripeBalance *json.Number `json:"stripe_balance"`
		Suspended     *bool        `json:"suspended"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return reading{}, errors.New("response is not the expected JSON object")
	}
	if doc.Recent == nil {
		return reading{}, errors.New("response carries no recent spend")
	}
	recent, err := numberNano(*doc.Recent)
	if err != nil {
		return reading{}, errors.New("recent spend is not an amount")
	}
	var r reading
	if doc.Limit != nil {
		if limit, err := numberNano(*doc.Limit); err == nil && limit > 0 {
			pct := float64(recent) / float64(limit) * 100
			if pct > 100 {
				pct = 100
			}
			r.windows = append(r.windows, Window{
				Label: "spend_limit:monthly", Window: quota.Monthly, Metric: quota.MetricCostUSD,
				Used: recent, Limit: limit, UsedPercent: pct, Known: true, AlertOnly: true,
			})
		}
	}
	if doc.StripeBalance != nil {
		if bal, err := numberNano(*doc.StripeBalance); err == nil && bal < 0 {
			// Credit, less what this month has used and not yet been invoiced.
			r.balances = append(r.balances, Balance{
				Currency: "USD", RemainingNano: -bal - recent,
				Available: doc.Suspended == nil || !*doc.Suspended,
			})
		}
	}
	if len(r.windows) == 0 && len(r.balances) == 0 {
		return reading{}, errors.New("response states neither a spend limit nor a credit")
	}
	return r, nil
}

func formatPlain(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
