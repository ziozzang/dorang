package probe

import (
	"fmt"
	"slices"
	"strings"
)

// The rule that decides what gets a prober: **a prober authenticates with the
// credential that serves the traffic, and reports what is left of that
// credential's own allowance.**
//
// It is not a convenience. DESIGN §6.2 makes a reported figure authoritative
// over local metering, so a figure about a different subject is worse than no
// figure. It excludes three families that are easy to mistake for quota
// endpoints and that between them account for most of the providers below:
//
//   - Historical spend, read with a second credential. Anthropic's and OpenAI's
//     usage and cost reports need an organization Admin key and answer "what
//     did the org spend", not "what is left of this key".
//   - Configured limits with no consumption. Anthropic's rate_limits API, xAI's
//     team model settings and GCP's QuotaInfo publish ceilings; a ceiling with
//     no usage beside it cannot say how much is left.
//   - Response headers on inference calls. anthropic-ratelimit-*,
//     x-ratelimit-* and their kin do carry remaining balances, but they arrive
//     only as a side effect of a request, so they are the request path's to
//     read, not a poller's. They are the reason several providers below need no
//     prober to be observable at all.

// Support describes what one provider publishes, including the ones that
// publish nothing. It is the table [New] consults, exported so that "there is
// no endpoint, and here is what was checked" is an answer a caller can render
// rather than a comment in a commit message.
type Support struct {
	// Provider is the canonical name.
	Provider string
	// Aliases are the other spellings [New] accepts.
	Aliases []string
	// Endpoint is what the prober reads, empty when there is none.
	Endpoint string
	// Auth says what authenticates the read.
	Auth string
	// Note is why there is no prober, or what is worth knowing about the one
	// there is.
	Note string
}

// Probed reports whether this provider has a prober.
func (s Support) Probed() bool { return s.Endpoint != "" }

// entry is one row of the table, plus the constructor when there is one.
type entry struct {
	Support
	src source
}

// table is every provider this package has examined. A provider absent from it
// yields [ErrUnknownProvider], which is a different statement from
// [ErrNoEndpoint]: one means "checked, publishes nothing", the other means
// "never looked".
var table = []entry{
	{
		Support: Support{
			Provider: "zai",
			Aliases:  []string{"z.ai", "glm", "zhipu", "zhipuai", "bigmodel"},
			Endpoint: zaiBaseURL + zaiPath,
			Auth:     "the same API key that serves inference, as a bearer token",
			Note: "Undocumented: Z.AI's developer FAQ describes no programmatic quota API. " +
				"Reports a consumed percentage per window with no absolute ceiling, so the " +
				"figure gates nothing without an Allowance and its value is the reset instant. " +
				"nextResetTime arrives as unix milliseconds from some deployments and as an " +
				"ISO-8601 string from others. TIME_LIMIT is carried unmapped because sources " +
				"disagree on whether it counts prompts or web-search and MCP calls.",
		},
		src: zaiSource{},
	},
	{
		Support: Support{
			Provider: "deepseek",
			Endpoint: deepseekBaseURL + deepseekPath,
			Auth:     "the same API key that serves inference, as a bearer token",
			Note: "Vendor-documented. Reports a prepaid balance, not a window: no ceiling, " +
				"no reset, nothing that expires, so it produces a Balance and no Window. " +
				"Amounts are decimal strings and the currency may be CNY; neither is converted.",
		},
		src: deepseekSource{},
	},
	{
		Support: Support{
			Provider: "anthropic",
			Aliases:  []string{"claude"},
			Endpoint: anthropicBaseURL + anthropicPath,
			Auth:     "the Claude Pro/Max OAuth access token the credential refreshes for itself",
			Note: "Undocumented, and OAuth only — an API key is refused rather than spent on a " +
				"401. Reports utilization 0-100 with an RFC 3339 reset for the five-hour and " +
				"seven-day subscription windows; per-model windows are carried unmapped because " +
				"a quota key has no model dimension. For an Anthropic API key there is no " +
				"endpoint: usage and cost reports need an organization Admin key and are " +
				"org-scoped and historical, the rate_limits API publishes ceilings without " +
				"consumption, and anthropic-ratelimit-* is a response header, not a poll.",
		},
		src: anthropicSource{},
	},
	{
		Support: Support{
			Provider: "openai",
			Aliases:  []string{"codex", "chatgpt", "openairesponses"},
			Note: "No endpoint a project key can read. /v1/organization/usage/* and " +
				"/v1/organization/costs need an Admin key (sk-admin-) and report org-wide " +
				"historical spend, not remaining quota. The legacy /v1/usage and " +
				"/dashboard/billing/credit_grants were dashboard endpoints, never in the API " +
				"reference, and have not served API keys for years. x-ratelimit-remaining-* is " +
				"a response header. The Codex subscription counter at " +
				"chatgpt.com/backend-api/wham/usage is undocumented and no complete payload " +
				"could be established, so it is not implemented: a partial field list is how a " +
				"prober invents a number.",
		},
	},
	{
		Support: Support{
			Provider: "xai",
			Aliases:  []string{"grok"},
			Note: "No quota endpoint on the inference host. GET /v1/api-key answers with key " +
				"metadata — id, acls, blocked and disabled flags — and no credits, usage or " +
				"limits; it is a validity check, not a quota read. Real billing lives on " +
				"management-api.x.ai behind a separate management key and a team id, which is " +
				"a different credential from the one that serves traffic.",
		},
	},
	{
		Support: Support{
			Provider: "google",
			Aliases:  []string{"gemini", "googleai", "vertex"},
			Note: "None. Gemini rate limits are scoped to a GCP project rather than to an API " +
				"key, so a key-scoped answer is not even well defined. The Cloud Quotas API " +
				"needs GCP IAM credentials and returns configured limits, not consumption; " +
				"consumption needs Cloud Monitoring. Quota details reach an API key only inside " +
				"a 429 body, which is post-hoc rather than pollable.",
		},
	},
	{
		Support: Support{
			Provider: "minimax",
			Aliases:  []string{"minimaxcn", "minimaxi"},
			Note: "Not implemented, deliberately. www.minimax.io/v1/token_plan/remains appears " +
				"in the vendor FAQ with a curl example and no response schema at all, its " +
				"sibling coding_plan/remains rejects API keys and asks for a browser cookie, " +
				"and community reports say the percentage field encodes REMAINING where every " +
				"other provider here encodes CONSUMED. Inverting that reads correctly at 50% " +
				"and backwards everywhere else, which is the exact failure §6.2 makes " +
				"expensive. This needs one empirical read with a real key before it can ship.",
		},
	},
	{
		Support: Support{
			Provider: "ollama",
			Aliases:  []string{"ollamacloud"},
			Note: "None. Ollama Cloud exposes no account endpoint and no rate-limit response " +
				"headers; inference responses carry per-request token counts only. The two " +
				"feature requests for exactly this (a /api/usage endpoint and X-Ollama-Quota-* " +
				"headers) were closed as duplicates without implementation. Limits are visible " +
				"only in the web dashboard.",
		},
	},
	{
		Support: Support{
			Provider: "qwen",
			Aliases:  []string{"dashscope", "bailian", "alibaba", "modelstudio"},
			Note: "None. No DashScope endpoint reports quota, balance or usage to a " +
				"DASHSCOPE_API_KEY; Alibaba's own rate-limit documentation directs you to the " +
				"console monitoring page. The Coding Plan FAQ states that viewing token " +
				"consumption is not supported. The Qwen Code OAuth free tier, which had a " +
				"daily request quota, was closed entirely in April 2026.",
		},
	},
}

// canonical folds the spellings a provider is written with. Punctuation is
// stripped so that "z.ai", "z-ai" and "zai" are one provider, which they are.
func canonical(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch r {
		case '.', '-', '_', ' ', '/':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// lookup finds a provider's row by any of its spellings.
func lookup(providerID string) (entry, bool) {
	c := canonical(providerID)
	for _, e := range table {
		if canonical(e.Provider) == c {
			return e, true
		}
		for _, a := range e.Aliases {
			if canonical(a) == c {
				return e, true
			}
		}
	}
	return entry{}, false
}

// New builds the prober for a provider.
//
// providerID is matched against every spelling in [Supported], but the prober
// keeps the id it was given: [quota.Registry] dispatches on
// quota.Credential.ProviderID, so a prober built as "glm" must answer to "glm"
// however this package spells it internally.
//
// A provider with no credential-readable endpoint returns [ErrNoEndpoint]
// carrying the reason, and one this package has not examined returns
// [ErrUnknownProvider]. Neither is a prober that returns zeroes: a probe that
// silently reports nothing is indistinguishable from a provider reporting no
// usage, and §6.2 would take the second reading.
func New(providerID string, cfg Config) (*Prober, error) {
	e, ok := lookup(providerID)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, providerID)
	}
	if e.src == nil {
		return nil, fmt.Errorf("%w: %s: %s", ErrNoEndpoint, e.Provider, e.Note)
	}
	return newProber(providerID, e.src, cfg)
}

// Supported returns the whole table, providers with a prober first.
func Supported() []Support {
	out := make([]Support, 0, len(table))
	for _, e := range table {
		s := e.Support
		s.Aliases = slices.Clone(s.Aliases)
		out = append(out, s)
	}
	slices.SortStableFunc(out, func(a, b Support) int {
		switch {
		case a.Probed() && !b.Probed():
			return -1
		case !a.Probed() && b.Probed():
			return 1
		}
		return strings.Compare(a.Provider, b.Provider)
	})
	return out
}

// Probed lists the canonical names that have a prober.
func Probed() []string {
	var out []string
	for _, e := range table {
		if e.src != nil {
			out = append(out, e.Provider)
		}
	}
	slices.Sort(out)
	return out
}

// labelOther is what an unrecognized name becomes.
const labelOther = "other"

// labelOf turns a provider's own word for something into a label, by matching
// it against the vocabulary that source is known to use.
//
// It is a whitelist rather than a filter, and the difference is the point. A
// label is the one place provider-supplied text reaches a [Snapshot], a
// snapshot is a record that outlives the read, and a snapshot must never be
// able to carry a credential (DESIGN §4.1). Filtering out punctuation does not
// achieve that — a key echoed into a type field survives a character filter
// almost intact, which is what the first version of this function did. Matching
// against a known vocabulary means nothing the provider sends can appear here
// at all.
//
// Collapsing an unrecognized name to [labelOther] costs nothing: a name this
// package does not recognize is a window it will not file under a quota key
// anyway, so the label exists only to make the omission visible.
func labelOf(vocabulary []string, raw string) string {
	t := strings.TrimSpace(raw)
	for _, v := range vocabulary {
		if strings.EqualFold(v, t) {
			return strings.ToLower(v)
		}
	}
	return labelOther
}

// currencyCode accepts an ISO 4217 code and nothing else.
//
// Three uppercase letters is the format, and holding to it exactly is what
// keeps a provider from putting arbitrary text — a key, an error page — into a
// [Balance]. An unrecognizable code becomes empty rather than a truncation of
// whatever arrived.
func currencyCode(s string) string {
	t := strings.ToUpper(strings.TrimSpace(s))
	if len(t) != 3 {
		return ""
	}
	for _, r := range t {
		if r < 'A' || r > 'Z' {
			return ""
		}
	}
	return t
}
