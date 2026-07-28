package backend

import (
	"errors"
	"net/url"
	"strings"
	"time"

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
}

// ErrNoBaseURL is returned for a provider with no endpoint at all.
var ErrNoBaseURL = errors.New("backend: provider has no base_url and its kind declares none")

// Provider is one configured upstream, resolved: everything the backend half of
// a request needs, in a value that never changes after a reload builds it.
type Provider struct {
	name    string
	kind    string
	api     catalog.API
	base    string
	timeout time.Duration
	retry   Policy
	engine  Engine
	ad      adapter
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
	ad, err := adapterFor(api, s.Kind)
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
	}, nil
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
