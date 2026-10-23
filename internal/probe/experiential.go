package probe

import (
	"context"
	"net/http"
)

// Experiential publishes the same credits schema as OpenRouter. Reuse its
// strict decoder; only the identity and endpoint differ. The inference host
// does not determine the quota host.
// https://platform.experientiallabs.ai/docs/cost-api (2026-10-23).
type experientialSource struct{ openrouterSource }

func (experientialSource) name() string { return "experiential" }
func (experientialSource) endpoint(base string) string {
	return baseOr(base, "https://api.experientiallabs.ai") + "/api/v1/credits"
}
func (s experientialSource) request(ctx context.Context, base, token, ua string) (*http.Request, error) {
	return newRequest(ctx, s.endpoint(base), ua, http.Header{"Authorization": {"Bearer " + token}})
}
