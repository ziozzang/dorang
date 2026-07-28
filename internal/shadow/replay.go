package shadow

import (
	"net/http"

	"github.com/ziozzang/dorang/internal/server"
)

// replayable reports whether a captured request may be re-issued against the
// reference gateway.
//
// §14.1 does not raise this, and it is the most dangerous thing it leaves out.
// "Send the same request to a reference gateway" is harmless for a completion —
// it costs money, which is what the daily ceiling is for — and is not harmless
// for anything that changes state. A gateway's surface is mostly control plane
// (COMPATIBILITY §0: 80% of it), and a shadow copy of `DELETE /key/…` deletes a
// key on the incumbent. Read literally, §14.1 says to do exactly that.
//
// So the rule is deny by default:
//
//   - GET, HEAD and OPTIONS replay. They are safe by definition, and they cover
//     GET /v1/models and the health probes — three of the eleven T0 paths, and
//     ones a client really does break on.
//   - POST replays only for the inference families. Those are the routes the
//     comparison exists for, their only side effect is spend, and spend is
//     already bounded.
//   - Nothing else replays: not PUT, PATCH or DELETE, and not POST to a
//     passthrough, batch or administrative route. A provider-native passthrough
//     prefix can carry a file deletion or a batch cancellation, and the engine
//     does not parse the body well enough to tell which (DESIGN §10.6).
//
// The cost of the rule is that the compared surface is smaller than the served
// surface, and an empty report therefore proves less than it appears to. That
// is why the refusals are counted and reported next to the verdict instead of
// being invisible — see [Stats.SkippedUnsafe].
func replayable(method string, family server.Family, path string) bool {
	// dorang's own scrape is not part of the compatibility surface and has no
	// counterpart on the reference. Comparing two Prometheus dumps produces an
	// inconclusive record per probe and nothing else.
	if path == "/metrics" {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	case http.MethodPost:
		switch family {
		case server.FamilyOpenAIChat, server.FamilyOpenAIEmbeddings,
			server.FamilyAnthropicMessages, server.FamilyAnthropicCountTokens:
			return true
		}
	}
	return false
}
