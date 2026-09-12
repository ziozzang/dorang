package backend

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// Engine names a self-hosted inference server whose behaviour differs from the
// protocol it claims to speak (DESIGN §4.4).
//
// The two engines here are one class, not two adapters: both serve
// openai-chat, both accept the same JSON, and both return 200 whether or not
// they honoured it. What differs is documented per engine in VLLM.md and
// SGLANG.md, and applied in selfhosted.go.
type Engine uint8

// The self-hosted engines dorang normalizes.
const (
	// EngineNone is every provider that is not a self-hosted engine dorang has
	// a profile for. It gets no normalization, because a normalization applied
	// to a provider whose behaviour was never checked is a guess.
	EngineNone Engine = iota
	EngineVLLM
	EngineSGLang
)

// String names the engine as an operator spells its kind.
func (e Engine) String() string {
	switch e {
	case EngineVLLM:
		return "vllm"
	case EngineSGLang:
		return "sglang"
	default:
		return ""
	}
}

// SelfHosted reports whether this engine has an operator profile — which is the
// condition for applying any of the normalizations in selfhosted.go.
func (e Engine) SelfHosted() bool { return e == EngineVLLM || e == EngineSGLang }

// EngineForKind maps a provider kind onto its engine profile. The kind must
// already be alias-resolved; this function does not consult the catalog.
func EngineForKind(kind string) Engine {
	switch kind {
	case "vllm":
		return EngineVLLM
	case "sglang":
		return EngineSGLang
	}
	return EngineNone
}

// Spec is one provider as configuration describes it. [NewProvider] resolves it
// into the immutable form the call path uses.
type Spec struct {
	// Name is the provider's configured id, used in errors and metering.
	Name string
	// Kind is the alias-resolved provider kind (DESIGN §4.3). It selects the
	// engine profile; it does not select the adapter, because two kinds can
	// share one wire shape and one kind can be overridden per deployment.
	Kind string
	// API is the wire adapter, already resolved from kind or from an explicit
	// per-deployment override.
	API catalog.API
	// BaseURL is the endpoint root. Empty is an error: a provider with no URL
	// is a 502 on the first request that names it.
	BaseURL string
	// Timeout bounds one upstream attempt. Zero means the request context's own
	// deadline is the only bound.
	Timeout time.Duration
	// Retry is the in-provider retry policy (§7.6 owns the cross-deployment
	// one). The zero value means one attempt.
	Retry Policy
	// DropParams are the parameters this upstream rejects, named by the
	// operator: `providers[].params.drop` (DESIGN §10.3).
	//
	// It is per PROVIDER rather than per deployment because the fact it records
	// is a fact about the endpoint — an old vLLM build, a vendor shim, a proxy
	// that validates strictly — and every model behind one base URL shares it.
	//
	// The list is applied to the neutral request before the encoder runs and
	// every removal is reported in x-dorang-dropped-params, which is the whole
	// difference between this and the operator editing their clients.
	DropParams []string
	// ResponsesOnly says the host behind Kind serves `/responses` and nothing
	// else. It is copied from the catalog kind by whoever builds the Spec, so
	// the adapter choice and the check on the two settings below read the
	// catalog's declaration rather than a list of their own.
	ResponsesOnly bool
	// Surfaces are the chat-shaped routes the host serves natively beyond its
	// primary one ([catalog.KindDefaults.Surfaces]): a caller speaking one of
	// them is sent there in its own family's shape.
	Surfaces []string
	// ResponsesForceStream and ResponsesStoreFalse state a Responses-only host's
	// contract once, instead of every caller meeting it as a 400. Refused by
	// [NewProvider] unless ResponsesOnly: nothing else reads them.
	ResponsesForceStream bool
	ResponsesStoreFalse  bool
}

// ErrNoBaseURL is returned for a provider with no endpoint at all.
var ErrNoBaseURL = errors.New("backend: provider has no base_url and its kind declares none")

// Provider is one configured upstream, resolved: everything the backend half of
// a request needs, in a value that never changes after a reload builds it.
type Provider struct {
	name string
	kind string
	api  catalog.API
	base string
	// ResponsesForceStream and ResponsesStoreFalse are the contract of a
	// Responses-only host, stated by the deployment instead of discovered by
	// every caller as a 400.
	//
	// They are exported because [responsesAdapter] reads them and this struct's
	// other fields are not, which is deliberate: nothing else in this package
	// needs them, and the two are provider POLICY rather than provider identity.
	// [NewProvider] refuses them on any provider whose host is not
	// Responses-only, because only [responsesAdapter] reads them and a setting
	// that loads and changes nothing is the CONFIG §23.2 defect.
	ResponsesForceStream bool
	ResponsesStoreFalse  bool

	timeout time.Duration
	retry   Policy
	engine  Engine
	ad      adapter
	// native holds one adapter per surface the host serves natively beyond the
	// primary, keyed by the caller family it serves. [Provider.pick] reads it.
	native map[catalog.API]adapter
	drop   []string
}

// NewProvider resolves a spec.
//
// It fails rather than defaulting when the endpoint is missing or unparseable,
// because both alternatives — a request to a wrong host, or a 502 the operator
// only sees in production — are worse than a start-up error.
func NewProvider(s Spec) (*Provider, error) {
	base := trimBase(s.BaseURL)
	if base == "" {
		return nil, ErrNoBaseURL
	}
	if _, err := url.Parse(base); err != nil {
		return nil, err
	}
	api := s.API
	if api == "" {
		api = catalog.APIOpenAIChat
	}
	ad, err := adapterFor(api, s.Kind, s.ResponsesOnly)
	if err != nil {
		return nil, err
	}
	if err := checkResponsesContract(s); err != nil {
		return nil, err
	}
	native, err := nativeAdapters(api, ad, s.Surfaces)
	if err != nil {
		return nil, err
	}
	drop, err := resolveDropParams(api, s.DropParams)
	if err != nil {
		return nil, err
	}
	return &Provider{
		name:    s.Name,
		kind:    s.Kind,
		api:     api,
		base:    base,
		timeout: s.Timeout,
		retry:   s.Retry.withDefaults(),
		engine:  EngineForKind(s.Kind),
		ad:      ad,
		native:  native,
		drop:    drop,

		ResponsesForceStream: s.ResponsesForceStream,
		ResponsesStoreFalse:  s.ResponsesStoreFalse,
	}, nil
}

// nativeAdapters builds the per-surface adapters a host's declared surfaces
// call for.
//
// A surface is served in ITS family's shape but with THIS host's credential:
// Ollama Cloud's /v1/messages takes `Authorization: Bearer`, the same as its
// chat route, and answers 401 to the x-api-key the Anthropic family spells.
// So the messages adapter here is the Anthropic encoder and decoder wrapped
// around the primary adapter's credential. The Responses surface on a host
// that serves both routes is addressed under the versioned base, unlike the
// Responses-only host, which serves it at the bare base.
func nativeAdapters(api catalog.API, primary adapter, surfaces []string) (map[catalog.API]adapter, error) {
	var out map[catalog.API]adapter
	add := func(k catalog.API, a adapter) {
		if out == nil {
			out = map[catalog.API]adapter{}
		}
		out[k] = a
	}
	for _, s := range surfaces {
		switch s {
		case catalog.SurfaceChat:
			if api != catalog.APIOpenAIChat && api != catalog.APIOpenAIResponses {
				return nil, fmt.Errorf("backend: surface %q is not served natively from a %s host in this build", s, api)
			}
		case catalog.SurfaceMessages:
			if api == catalog.APIAnthropicMessages {
				continue // the primary surface
			}
			add(catalog.APIAnthropicMessages, nativeMessagesAdapter{primary: primary})
		case catalog.SurfaceResponses:
			if api != catalog.APIOpenAIChat && api != catalog.APIOpenAIResponses {
				return nil, fmt.Errorf("backend: surface %q is not served natively from a %s host in this build", s, api)
			}
			add(catalog.APIOpenAIResponses, responsesAdapter{versioned: true})
		default:
			return nil, fmt.Errorf("backend: surface %q is not one of chat, messages, responses", s)
		}
	}
	return out, nil
}

// pick chooses the adapter for one call: the native adapter for the caller's
// own surface when the host serves it, the primary otherwise. The API it
// returns is the family the request is encoded against, which is what the
// §10.1 gate and the encoder read their capability set from.
func (p *Provider) pick(c *Call) (adapter, catalog.API) {
	if c != nil && p.native != nil {
		switch {
		case c.Op == OpResponses:
			if a, ok := p.native[catalog.APIOpenAIResponses]; ok {
				return a, catalog.APIOpenAIResponses
			}
		case c.Op == OpChat && c.ClientAPI == catalog.APIAnthropicMessages:
			if a, ok := p.native[catalog.APIAnthropicMessages]; ok {
				return a, catalog.APIAnthropicMessages
			}
		}
	}
	return p.ad, p.api
}

// nativeMessagesAdapter is the Anthropic surface on a host whose credential
// is spelled some other way.
type nativeMessagesAdapter struct {
	anthropicAdapter
	primary adapter
}

func (a nativeMessagesAdapter) credential(secret string, h http.Header) {
	a.primary.credential(secret, h)
}

// checkResponsesContract refuses the Responses-only settings on a host that is
// not one.
//
// `force_stream` and `store_false` are read by [responsesAdapter] and by
// nothing else. On any other provider they would load, appear in the operator's
// file as though they took effect, and change no byte on the wire — CONFIG
// §23.2's rule is that such a key is refused at start-up naming what it would
// have needed. The same disposition [resolveDropParams] takes for an
// undroppable name.
func checkResponsesContract(s Spec) error {
	if s.ResponsesOnly {
		return nil
	}
	var set []string
	if s.ResponsesForceStream {
		set = append(set, "force_stream")
	}
	if s.ResponsesStoreFalse {
		set = append(set, "store_false")
	}
	if len(set) == 0 {
		return nil
	}
	return fmt.Errorf("backend: params.%s is set, but kind %q is not a Responses-only host "+
		"(catalog responses_only: false) and the chat adapter it uses never reads the setting — "+
		"it would load and change nothing. Remove it, or declare the kind responses_only in "+
		"the catalog if its host really serves /responses alone",
		strings.Join(set, " and params."), s.Kind)
}

// requiredOutputCeiling names the wire shapes whose request is INVALID without
// an output ceiling.
//
// Anthropic's messages API is the one dorang speaks: max_tokens is required,
// and internal/wire/anthropic fills it from the model catalog when the caller
// named none (DESIGN §10.7's first trap). So a provider of that shape whose
// operator asked to drop max_tokens gets it back from the catalog on the very
// next line — the setting would load, look applied, and change nothing, which
// is the disposition this whole mechanism exists to stop being.
func requiredOutputCeiling(api catalog.API) bool {
	return api == catalog.APIAnthropicMessages
}

// outputCeilingNames are the three spellings of the one field.
var outputCeilingNames = map[string]bool{
	"max_tokens": true, "max_completion_tokens": true, "max_output_tokens": true,
}

// resolveDropParams validates an operator's drop list against the wire shape
// this provider actually speaks.
//
// It refuses at construction rather than per request, for the reason
// [NewProvider] refuses a missing base URL: a start-up failure an operator reads
// is better than a behaviour they never see.
func resolveDropParams(api catalog.API, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if err := canonical.CheckDropParam(n); err != nil {
			return nil, fmt.Errorf("backend: params.drop: %w", err)
		}
		if outputCeilingNames[n] && requiredOutputCeiling(api) {
			return nil, fmt.Errorf("backend: params.drop: %q cannot be dropped for a %s provider: "+
				"the field is REQUIRED by that wire shape and is refilled from the model catalog, "+
				"so the drop would take effect on nothing. Cap the ceiling with "+
				"models[].deployments[].max_output_tokens instead", n, api)
		}
		out = append(out, n)
	}
	return out, nil
}

// Name is the provider's configured id.
func (p *Provider) Name() string { return p.name }

// Kind is the alias-resolved provider kind.
func (p *Provider) Kind() string { return p.kind }

// API is the wire adapter this provider speaks.
func (p *Provider) API() catalog.API { return p.api }

// BaseURL is the endpoint root, without a trailing slash.
func (p *Provider) BaseURL() string { return p.base }

// Engine is the self-hosted profile, [EngineNone] for a hosted provider.
func (p *Provider) Engine() Engine { return p.engine }

// Timeout bounds one attempt; zero means unbounded by the provider.
func (p *Provider) Timeout() time.Duration { return p.timeout }

// Retry is the in-provider retry policy.
func (p *Provider) Retry() Policy { return p.retry }

// trimBase removes trailing slashes so that joining a path never doubles one.
func trimBase(s string) string { return strings.TrimRight(s, "/") }
