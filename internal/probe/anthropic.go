package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
)

// Anthropic publishes a Claude Pro/Max subscription's own utilization to the
// OAuth credential that serves it. That last clause is the whole reason this
// one is here and the API-key path is not.
//
// The rule this package applies is that a prober authenticates with the
// credential that serves the traffic. Anthropic's documented usage and cost
// reports need an organization Admin key — a second credential that serves no
// requests, reporting org-wide historical spend rather than what is left of the
// credential dorang is about to route to. Reading it would answer a different
// question about a different subject, so [New] returns [ErrNoEndpoint] for an
// Anthropic API key and this source is reached only by an OAuth credential.
//
// The endpoint is undocumented, and the token it needs is the OAuth access
// token a credential refreshes for itself (DESIGN §11.2b) — which is why [Auth]
// resolves the secret at probe time rather than capturing it once. It is also
// reported to answer 429 readily, so the shared backoff is not decoration here.
const (
	anthropicBaseURL = "https://api.anthropic.com"
	anthropicPath    = "/api/oauth/usage"
	// anthropicOAuthBeta is the beta flag the endpoint requires.
	anthropicOAuthBeta = "oauth-2025-04-20"
	// anthropicAPIKeyPrefix marks a plain API key. Such a key cannot read this
	// endpoint, and spending a request per poll to be told 401 is how a prober
	// gets an account rate-limited — the failure it exists to prevent.
	anthropicAPIKeyPrefix = "sk-ant-api"
)

type anthropicSource struct{}

func (anthropicSource) name() string { return "anthropic" }

func (anthropicSource) endpoint(base string) string {
	return baseOr(base, anthropicBaseURL) + anthropicPath
}

func (anthropicSource) accepts(secret string) error {
	if strings.HasPrefix(strings.TrimSpace(secret), anthropicAPIKeyPrefix) {
		return fmt.Errorf("%w: %s: this endpoint reads a Claude subscription and needs "+
			"an OAuth access token, not an API key", ErrWrongCredentialKind, "anthropic")
	}
	return nil
}

func (s anthropicSource) request(ctx context.Context, base, token, ua string) (*http.Request, error) {
	return newRequest(ctx, s.endpoint(base), ua, http.Header{
		"Authorization":  {"Bearer " + token},
		"Anthropic-Beta": {anthropicOAuthBeta},
	})
}

// anthropicWindow is one utilization entry. utilization is consumed, 0–100;
// resets_at is RFC 3339 with a zone. Both are nullable — a plan that has no
// per-model window reports null for it rather than omitting the key.
type anthropicWindow struct {
	Utilization number    `json:"utilization"`
	ResetsAt    resetTime `json:"resets_at"`
}

// The two windows whose scope is the whole subscription, and therefore the two
// that a credential-level quota rule can describe.
const (
	anthropicFiveHour = "five_hour"
	anthropicSevenDay = "seven_day"
)

// anthropicKeys is the vocabulary a label may be built from; see [labelOf]. The
// per-model and extra-usage entries are listed so that they are named rather
// than collapsed, even though none of them is filed under a quota key.
var anthropicKeys = []string{
	anthropicFiveHour, anthropicSevenDay,
	"seven_day_opus", "seven_day_sonnet", "seven_day_haiku", "extra_usage",
}

func (anthropicSource) decode(body []byte) (reading, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return reading{}, errors.New("response is not the expected JSON object")
	}

	// Iterating the payload rather than a fixed struct means a plan that grows
	// a new per-model window reports it as unmapped instead of vanishing.
	// Sorted, so that a snapshot does not depend on map order.
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var r reading
	for _, k := range keys {
		var aw anthropicWindow
		if err := json.Unmarshal(raw[k], &aw); err != nil {
			continue // not a window object: a scalar or a differently shaped field
		}
		if !aw.Utilization.Set && aw.ResetsAt.Time.IsZero() {
			continue // nothing usable, including the explicit nulls
		}
		w := Window{Label: labelOf(anthropicKeys, k), ResetAt: aw.ResetsAt.Time}
		if aw.Utilization.Set {
			w.UsedPercent, w.Known = usedPercent(aw.Utilization.Value)
		}
		switch k {
		case anthropicFiveHour:
			w.Window, w.Metric, w.Mapped = quota.Rolling(5*time.Hour), quota.MetricTokensTotal, true
		case anthropicSevenDay:
			w.Window, w.Metric, w.Mapped = quota.Rolling(7*24*time.Hour), quota.MetricTokensTotal, true
		default:
			// seven_day_opus, seven_day_sonnet and anything like them are
			// scoped to a model, and a quota key has no model dimension: filing
			// a per-model figure under the subscription's seven-day rule would
			// report one model's consumption as the plan's. extra_usage is
			// billed credits, not an allowance. Both are carried unmapped, with
			// their label and reset intact.
		}
		r.windows = append(r.windows, w)
	}
	return r, nil
}
