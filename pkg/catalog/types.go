package catalog

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// API names the wire adapter a provider kind speaks (DESIGN §4.3).
type API string

// Wire adapters.
const (
	APIOpenAIChat        API = "openai-chat"
	APIOpenAIResponses   API = "openai-responses"
	APIAnthropicMessages API = "anthropic-messages"
	APIGemini            API = "gemini"
	APICohere            API = "cohere"
	APIJina              API = "jina"
	APIEcho              API = "echo"
)

var validAPIs = map[API]bool{
	APIOpenAIChat: true, APIOpenAIResponses: true, APIAnthropicMessages: true,
	APIGemini: true, APICohere: true, APIJina: true, APIEcho: true,
}

// CacheScheme names how prompt caching is expressed on the wire for a kind.
type CacheScheme string

// Prompt-cache schemes.
const (
	CacheNone                 CacheScheme = "none"
	CacheOpenAIKey            CacheScheme = "openai_cache_key"
	CacheAnthropicControl     CacheScheme = "anthropic_cache_control"
	CacheGoogleCachedContents CacheScheme = "google_cached_contents"
	CacheOllamaKeepAlive      CacheScheme = "ollama_keep_alive"
	CacheOpenRouter           CacheScheme = "openrouter_cache"
)

var validCaches = map[CacheScheme]bool{
	CacheNone: true, CacheOpenAIKey: true, CacheAnthropicControl: true,
	CacheGoogleCachedContents: true, CacheOllamaKeepAlive: true, CacheOpenRouter: true,
}

// Category is the request family a model serves. It selects which endpoints
// (DESIGN §2.1) a model may be routed to.
type Category string

// Model categories.
const (
	CategoryChat       Category = "chat"
	CategoryCompletion Category = "completion"
	CategoryEmbedding  Category = "embedding"
	CategoryRerank     Category = "rerank"
	CategoryImage      Category = "image"
	CategoryAudio      Category = "audio"
	CategoryModeration Category = "moderation"
	CategoryOCR        Category = "ocr"
)

var validCategories = map[Category]bool{
	CategoryChat: true, CategoryCompletion: true, CategoryEmbedding: true,
	CategoryRerank: true, CategoryImage: true, CategoryAudio: true,
	CategoryModeration: true, CategoryOCR: true,
}

// ReasoningCapability is the form a model accepts its reasoning control in
// (DESIGN §10.2). It is a property of a (kind, model) pair. It is never
// inferred from the provider kind, from a sibling model, or from the shape of
// the model's name.
type ReasoningCapability string

// Reasoning capabilities. ReasoningUnknown is the default for every model that
// has not been checked against a live endpoint; the control is then omitted and
// reported in x-dorang-dropped-params rather than guessed.
const (
	ReasoningUnknown            ReasoningCapability = "unknown"
	ReasoningNone               ReasoningCapability = "none"
	ReasoningEffortScale        ReasoningCapability = "effort_scale"
	ReasoningResponsesReasoning ReasoningCapability = "responses_reasoning"
	ReasoningThinkingBudget     ReasoningCapability = "thinking_budget"
	ReasoningThinkingFlag       ReasoningCapability = "thinking_flag"
	ReasoningEnableThinking     ReasoningCapability = "enable_thinking"
)

var validReasoning = map[ReasoningCapability]bool{
	ReasoningUnknown: true, ReasoningNone: true, ReasoningEffortScale: true,
	ReasoningResponsesReasoning: true, ReasoningThinkingBudget: true,
	ReasoningThinkingFlag: true, ReasoningEnableThinking: true,
}

// Reasoning describes how one model accepts the neutral reasoning control.
//
// A zero Reasoning means unknown: Capability is the empty string, which
// [Reasoning.Effective] normalizes to ReasoningUnknown. Values returned by this
// package always carry an explicit Capability.
type Reasoning struct {
	// Capability is the accepted wire form. ReasoningUnknown means the model
	// has not been probed; callers must omit the control and report it.
	Capability ReasoningCapability

	// Levels is the declared effort level set, for ReasoningEffortScale only.
	// A caller clamps into this set and never emits a level not listed here.
	Levels []string

	// DefaultLevel is the level applied when the caller asks for reasoning
	// without naming one. Empty means the backend's own default.
	DefaultLevel string

	// MinBudgetTokens and MaxBudgetTokens bound ReasoningThinkingBudget and,
	// where declared, ReasoningEnableThinking. Zero means undeclared. The
	// per-request budget is min(table[effort], max_tokens-reserve) (DESIGN
	// §10.2 / REVIEW C4); these are bounds on that result, not a substitute.
	MinBudgetTokens int
	MaxBudgetTokens int

	// Verified is the YYYY-MM-DD date the capability was checked against a
	// live endpoint. It is empty exactly when Capability is unknown.
	Verified string

	// Note records how the capability was established, when that is not
	// obvious from the date alone.
	Note string
}

// Effective returns Capability, normalizing the zero value to ReasoningUnknown.
func (r Reasoning) Effective() ReasoningCapability {
	if r.Capability == "" {
		return ReasoningUnknown
	}
	return r.Capability
}

// Known reports whether a concrete, verified capability is available. It is
// false for both unknown and the zero value, so the safe branch is the default.
func (r Reasoning) Known() bool {
	c := r.Effective()
	return c != ReasoningUnknown && r.Verified != ""
}

// String renders the capability in the notation used by DESIGN §10.2, for
// example "effort_scale(low,high,xhigh)".
func (r Reasoning) String() string {
	c := r.Effective()
	if c == ReasoningEffortScale && len(r.Levels) > 0 {
		return string(c) + "(" + strings.Join(r.Levels, ",") + ")"
	}
	return string(c)
}

// Decimal is an exact decimal literal kept as text. Prices are parsed as exact
// decimals and multiplied in wider precision at accounting time (REVIEW R1-11);
// binding them to float64 here would round before the arithmetic that cares.
type Decimal string

// UnmarshalYAML accepts both quoted and unquoted scalars and keeps the literal
// exactly as written.
func (d *Decimal) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("decimal must be a scalar, got %v", n.Kind)
	}
	if n.Tag == "!!null" {
		*d = ""
		return nil
	}
	*d = Decimal(n.Value)
	return nil
}

func (d Decimal) valid() bool {
	s := string(d)
	if s == "" {
		return true
	}
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return false
	}
	dot, digits := false, false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits = true
		case r == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return digits
}

// Pricing is a hint only. Authoritative pricing lives in the pricing rule set
// (DESIGN §8), which can express subscriptions and adjustments that a per-token
// pair cannot. A hint is useful for cost-aware routing before any rule matches.
type Pricing struct {
	Currency           string
	InputPerMTok       Decimal
	OutputPerMTok      Decimal
	CachedInputPerMTok Decimal
	Verified           string
	Note               string
}

// IsZero reports whether no pricing hint is present.
func (p Pricing) IsZero() bool {
	return p.Currency == "" && p.InputPerMTok == "" &&
		p.OutputPerMTok == "" && p.CachedInputPerMTok == ""
}

// KindDefaults is what a provider kind contributes before any model is known
// (DESIGN §4.3): the wire adapter, the cache scheme, and capability defaults.
type KindDefaults struct {
	// Name is the canonical kind name, after alias resolution.
	Name string

	API   API
	Cache CacheScheme

	// ReasoningHint names the reasoning shape the adapter for this kind
	// implements. It is NOT a capability claim about any model on this kind:
	// keying folding on it is exactly the defect REVIEW C5 records. Use
	// [Catalog.Reasoning] for the per-model capability.
	ReasoningHint ReasoningCapability

	// Category is the default request family for models on this kind.
	Category Category

	// ContextWindow and MaxOutputTokens are defaults in tokens. Zero means
	// undeclared, not unlimited and not zero-length: a caller must treat it
	// the same way it treats an unknown reasoning capability.
	ContextWindow   int
	MaxOutputTokens int

	SupportsTools     bool
	SupportsStreaming bool

	// Metrics and Priority record kind-specific integrations (DESIGN §4.3,
	// e.g. vllm exposes Prometheus metrics and native request priority).
	Metrics  string
	Priority string

	// Verified is the YYYY-MM-DD date this kind's shape was last checked.
	Verified string
	Note     string
}

// ModelRef names one catalog entry. Kind and Model are kept apart so no caller
// has to join or split them; a model name never has to survive a round trip
// through a delimiter.
type ModelRef struct {
	Kind  string
	Model string
}

// ModelInfo is the composition of the three layers named in DESIGN §4.3 — kind
// defaults, then the longest-matching model-name prefix rule, then the explicit
// model entry — with later layers overriding earlier ones.
type ModelInfo struct {
	// Kind is the canonical kind name after alias resolution, or the name as
	// given when it resolves to no declared kind.
	Kind string

	// Model is the requested model name, byte-identical to the argument. No
	// layer may change it; a prefix rule supplies defaults, never identity.
	Model string

	API      API
	Cache    CacheScheme
	Category Category

	// ContextWindow and MaxOutputTokens are in tokens; zero means undeclared.
	ContextWindow   int
	MaxOutputTokens int

	SupportsTools     bool
	SupportsStreaming bool

	Reasoning Reasoning
	Pricing   Pricing

	// Verified is the YYYY-MM-DD date the model's existence was confirmed
	// against the provider. It says nothing about reasoning capability, which
	// carries its own date in Reasoning.Verified.
	Verified string

	// KindKnown and ModelKnown report which layers had data. ModelKnown is
	// true only on an exact match of the whole name: a prefix rule never
	// makes an unlisted model count as listed.
	KindKnown  bool
	ModelKnown bool

	// Layers lists which of "kind", "prefix", "model" contributed, in order.
	Layers []string

	// MatchedPrefix is the prefix rule that applied, empty if none did.
	MatchedPrefix string

	Note string
}
