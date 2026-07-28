package probe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// DeepSeek publishes a credential's prepaid balance, authenticated with the
// same key that serves inference. It is the only provider here whose quota
// endpoint is documented by the vendor.
//
// What it reports is a balance, and a balance is not a window. There is no
// ceiling for a proportion to be of, no reset, and nothing that expires — so it
// produces no [Window] at all rather than a window with invented bounds. Two
// consequences, both correct rather than limitations:
//
//   - §6.2's max() has nothing to combine. "How much is left" and "how much has
//     been used within a window" are different questions, and local metering
//     answers the second on its own.
//   - §7.5a(c) scores it zero, by constraint 1: a balance that rolls over loses
//     nothing by being spent later, so spending it early only forfeits the
//     option of spending it later.
//
// The amounts are decimal strings and the currency may be CNY. Neither is
// converted: a rate this package invented would be a made-up number in a field
// a caller would read as measured.
const (
	deepseekBaseURL = "https://api.deepseek.com"
	deepseekPath    = "/user/balance"
)

type deepseekSource struct{}

func (deepseekSource) name() string { return "deepseek" }

func (deepseekSource) endpoint(base string) string {
	return baseOr(base, deepseekBaseURL) + deepseekPath
}

func (deepseekSource) accepts(string) error { return nil }

func (s deepseekSource) request(ctx context.Context, base, token, ua string) (*http.Request, error) {
	return newRequest(ctx, s.endpoint(base), ua, http.Header{
		"Authorization": {"Bearer " + token},
	})
}

type deepseekBalance struct {
	IsAvailable  *bool `json:"is_available"`
	BalanceInfos []struct {
		Currency        string `json:"currency"`
		TotalBalance    string `json:"total_balance"`
		GrantedBalance  string `json:"granted_balance"`
		ToppedUpBalance string `json:"topped_up_balance"`
	} `json:"balance_infos"`
}

func (deepseekSource) decode(body []byte) (reading, error) {
	var b deepseekBalance
	if err := json.Unmarshal(body, &b); err != nil {
		return reading{}, errors.New("response is not the expected JSON object")
	}
	if b.BalanceInfos == nil {
		return reading{}, errors.New("response carries no balance_infos array")
	}
	available := b.IsAvailable != nil && *b.IsAvailable

	var r reading
	for _, info := range b.BalanceInfos {
		n, err := parseNano(info.TotalBalance)
		if err != nil {
			// An amount that did not parse is unknown. Reporting zero would say
			// the account is empty, which is the reading that stops traffic.
			continue
		}
		r.balances = append(r.balances, Balance{
			Currency:      currencyCode(info.Currency),
			RemainingNano: n,
			Available:     available,
		})
	}
	return r, nil
}
