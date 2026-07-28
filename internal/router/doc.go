// Package router turns a client-facing model name into a deployment that will
// serve the request, and closes the loop when the attempt finishes.
//
// It is the integration point: every other package states a constraint, and
// this is where they meet. internal/capacity says how many requests may run at
// once, internal/health says which deployments are worth trying, internal/prefix
// says which one probably still holds the conversation, internal/pricing says
// what each one costs, pkg/catalog says what each one can express, and
// internal/quota says whether the money and the window allow it at all.
//
// # Pipeline (DESIGN §7.1)
//
//	resolve alias → resolve group → filter (pin, capability, context, quota)
//	  → order by strategy → try acquire (next candidate on failure) → return
//
// [Router.Route] runs that pipeline and returns a [Decision]. The caller
// dispatches, then calls [Router.Report], which feeds health, prefix affinity
// and session stickiness, releases the capacity reservation, and records what
// happened so that a following [Router.Route] with [Request.Previous] set can
// continue the same routing session as a fallback hop (§7.6).
//
// # Six things that are easy to get backwards
//
// Priority direction is per engine. The canonical scale is lower-is-more-urgent
// (§7.5). vLLM schedules the lowest value first and takes the scale unchanged;
// SGLang schedules the HIGHEST first and the adapter must negate. Both engines
// accept the same JSON and both return 200 either way, so a single shared
// constant is an inversion rather than a degradation, and nothing in the
// response reveals it. See priority.go and VLLM.md §1.2 / SGLANG.md §3.
//
// Opaque state is a hard pin, not a preference (EXTENSIONS §B.3, DESIGN §7.4a2).
// A conversation carrying an integrity-protected reasoning block, a server-side
// response handle or a vendor compaction cursor is pinned to the family — and
// often to the individual credential — that minted it. The receiving family
// classifies foreign state as never-retryable, so a fallback across that
// boundary burns every remaining hop to arrive at the identical 400, slower.
// Such a request fails immediately with a reason naming the pin.
//
// Capability filters before it ranks (§10.1). A request using a construct only
// one protocol family can express is routed there when such a deployment
// exists, and fails with a named 400 when none does. It is never silently
// downgraded, because a 200 that dropped a PDF is worse than an error.
//
// Cost-based ordering reads [pricing.Cost.MarginalNano] only (§8.1). A sunk
// subscription must not make a saturated plan look cheap. A model no rule
// prices has no opinion in the ordering rather than a cost of zero — otherwise
// the cheapest deployment is always the one nobody has priced.
//
// least_busy, lowest_latency and highest_tps treat "no samples" as no opinion
// rather than as best. internal/health returns zero for an unproven deployment
// and internal/capacity reports an unknown axis, and in both cases the honest
// reading is silence, not victory.
//
// Model names are opaque (§2.1). Nothing here splits one on ':' or '/' or any
// other character. "gemma4:31b", "zai:glm-5.1" and "deepseek-v4-flash:cloud"
// are single names, compared whole.
//
// # Two clocks that deliberately disagree
//
// Session stickiness expires from CREATION (§7.4a): the premise is that the
// upstream cache is gone once the TTL elapses, so refreshing on use would
// defeat the point. Prefix affinity expires from LAST USE (§7.4b): a prefix
// that keeps being requested keeps the upstream cache warm, so refreshing
// tracks reality. The asymmetry is intentional and both directions are tested.
//
// # Hot path
//
// Route allocates a Decision, a reservation, and whatever pricing returns.
// Everything else — the candidate scratch, the credential lists, the reason
// strings — is precomputed at New or drawn from a pool. There is no reflection,
// no regular expression and no formatted string construction on this path
// (§15.5); "prefix_hit:depth=3" comes out of a table, not a Sprintf.
package router
