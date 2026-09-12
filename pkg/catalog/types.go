package catalog

import (
	"fmt"
	"slices"
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

// ProbeResult records what a live endpoint said when it was asked about a
// model and the answer was not the model answering.
//
// `verified:` covers exactly one outcome — asked, and it answered as itself.
// Everything else collapsed into "no date", which made a model nobody had ever
// asked about indistinguishable from one that had been asked and had given a
// definite, useful answer. These are the definite answers.
type ProbeResult string

// Probe results.
const (
	// ProbeNone is the zero value: no probe is recorded.
	ProbeNone ProbeResult = ""

	// ProbeDenied means the endpoint refused because the credential's plan is
	// not entitled to the model — not because the model does not exist. The
	// two are different facts and only the error text separates them: a
	// retirement says so, with a date and a reference ("kimi-k2.5 was retired
	// at 2026-07-31"), while an entitlement refusal talks about eligibility.
	// The model is real, its context window is real, and another account's
	// plan can reach it. Dating such an entry would be false; deleting it
	// would throw away knowledge nothing else in the deployment records.
	//
	// Entitlement is a property of (account, model), not of the model. The
	// note carries the verbatim refusal, because that text is the only
	// evidence that this is entitlement and not absence.
	ProbeDenied ProbeResult = "denied"

	// ProbeSubstituted means the request succeeded and a DIFFERENT model
	// answered: the endpoint accepted the name and served something else,
	// naming it in the response. This is the outcome a listing can never
	// reveal and the one that looks most like success — a 200 with a body.
	// It matters because every number the catalog holds for the requested
	// name (context window, max output, price) then describes a model the
	// caller is not talking to.
	//
	// Served names what answered. Nothing in dorang rewrites the request to
	// it: recording that the endpoint substitutes is not permission to
	// substitute.
	ProbeSubstituted ProbeResult = "substituted"

	// ProbeCitationOnly is a KIND-level result: every model entry under this
	// kind was transcribed from a third-party catalog and not one has been
	// checked against the endpoint, because no credential for this route was
	// available where this catalog is maintained.
	//
	// It is deliberately a statement about the data rather than about the
	// reader: an operator holding a key for the provider is not being told
	// they lack one, and their probe results overlay this. It exists so that
	// "nobody could look" is distinguishable from "nobody has looked yet",
	// which is the difference between a boundary and a backlog.
	ProbeCitationOnly ProbeResult = "citation_only"
)

// probeResultLevel records where each result may legally appear. Entitlement is
// per (account, model) and substitution is per model name, so both belong on an
// entry; the absence of any credential is a property of the route, so it
// belongs on the kind and would be 195 identical copies on the entries.
var probeResultLevel = map[ProbeResult]string{
	ProbeDenied:       LayerModel,
	ProbeSubstituted:  LayerModel,
	ProbeCitationOnly: LayerKind,
}

// Probe is one recorded answer from a live endpoint that did not establish the
// model. It is the third state the two-state `verified:`/absent split could not
// express (DESIGN §10.2).
type Probe struct {
	// Result is what happened. ProbeNone means no probe is recorded, which is
	// not the same as a probe that found nothing.
	Result ProbeResult

	// Date is the YYYY-MM-DD the probe was made. It is required with any
	// non-empty Result, for the same reason `verified:` is a date rather than
	// a flag: the answer is about a moment, and an entitlement can be bought.
	Date string

	// Served is the model id that answered, for ProbeSubstituted only. It is
	// the whole content of that finding.
	Served string

	// Note carries the evidence, verbatim where there is any. It is required
	// for ProbeDenied because the refusal text is the only thing that
	// separates "not entitled" from "does not exist".
	Note string
}

// IsZero reports whether no probe is recorded.
func (p Probe) IsZero() bool { return p.Result == ProbeNone }

// String renders one probe as a report line, for example
// "substituted(served=glm-5.2) 2026-08-03".
func (p Probe) String() string {
	if p.IsZero() {
		return ""
	}
	var b strings.Builder
	b.WriteString(string(p.Result))
	if p.Served != "" {
		b.WriteString("(served=")
		b.WriteString(p.Served)
		b.WriteString(")")
	}
	if p.Date != "" {
		b.WriteString(" ")
		b.WriteString(p.Date)
	}
	return b.String()
}

// Verification is the answer to "what happened the last time anybody asked the
// endpoint about this model?". It is derived, never stored: one entry cannot
// carry two answers to that question, and the loader refuses data that tries.
//
// The point of the enumeration is what the ABSENCE of a date means. Before it,
// undated covered four unrelated situations at once, and an operator reading
// the catalog could not tell a model that had been checked and refused from one
// nobody had ever typed.
type Verification string

// Verification states.
const (
	// VerificationVerified: asked on a date, and it answered as itself.
	VerificationVerified Verification = "verified"

	// VerificationDenied: asked, and refused on entitlement. The model exists.
	VerificationDenied Verification = "denied"

	// VerificationSubstituted: asked, and a different model answered.
	VerificationSubstituted Verification = "substituted"

	// VerificationCitationOnly: never asked, and nothing here could ask —
	// the kind has no credential where this catalog is maintained. The
	// boundary of what this build can establish, stated rather than implied.
	VerificationCitationOnly Verification = "citation_only"

	// VerificationUnchecked: nobody has asked, and nothing says why. This is
	// a backlog item, and after the states above it is a much smaller and
	// much more actionable set than "everything undated" was.
	VerificationUnchecked Verification = "unchecked"
)

// Established reports whether the endpoint answered as this model. It is false
// for every other state including substituted, where a request succeeded: a 200
// from a different model establishes that model, not this one.
func (v Verification) Established() bool { return v == VerificationVerified }

// Asked reports whether anybody has put this entry to a live endpoint. It
// separates the two states that carry a finding from the two that carry none,
// which is the split `verified:` alone could not express.
func (v Verification) Asked() bool {
	switch v {
	case VerificationVerified, VerificationDenied, VerificationSubstituted:
		return true
	}
	return false
}

// verificationOrder is the reporting order: what was established, then what was
// asked and answered otherwise, then what was not asked. Alphabetical would put
// the backlog above the findings.
var verificationOrder = []Verification{
	VerificationVerified,
	VerificationDenied,
	VerificationSubstituted,
	VerificationCitationOnly,
	VerificationUnchecked,
}

// VerificationStates lists every state in reporting order.
func VerificationStates() []Verification { return slices.Clone(verificationOrder) }

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

	// BaseURL is the canonical endpoint this kind's adapter addresses. It is
	// a DEFAULT for provider configuration, not an identity: a deployment
	// names its own base URL and that wins. It is recorded because the same
	// vendor often exposes several routes with different limits and even
	// different wire shapes, and a kind that names its route is one an
	// operator can recognise.
	BaseURL string

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

	// ResponsesOnly records that the HOST behind this kind serves `/responses`
	// and no other route, so a chat-completions address answers 403 or 404
	// rather than a completion.
	//
	// It is a fact about the host, not about the wire shape: `api:
	// openai-responses` says what the request looks like, and most hosts
	// declaring it serve `/chat/completions` too. It lives on the kind so that
	// the adapter choice and the start-up validation of the provider settings
	// that only mean something on such a host read ONE declaration — a table
	// in the backend and a check in app that each had their own list would be
	// two lists, and two lists disagree.
	//
	// Measured 2026-08-06 against the ChatGPT Codex surface: `POST
	// /backend-api/codex/chat/completions` answers 403 and `POST
	// /backend-api/codex/responses` answers 400 for a body problem.
	ResponsesOnly bool

	// Surfaces names the chat-shaped routes the HOST serves natively beyond
	// the one `API` implies: any of "chat", "messages", "responses". A caller
	// speaking one of them is sent to that route in its own family's shape,
	// with no conversion and therefore no conversion loss, instead of being
	// translated to the kind's primary surface (docs/SURFACES.md). A surface
	// equal to the primary is redundant and ignored; a name outside the three
	// is refused at load. Measured, like ResponsesOnly: Ollama Cloud serves all
	// three and takes the same bearer credential on each.
	Surfaces []string

	// Metrics and Priority record kind-specific integrations (DESIGN §4.3,
	// e.g. vllm exposes Prometheus metrics and native request priority).
	Metrics  string
	Priority string

	// Verified is the YYYY-MM-DD date this kind's shape was last checked
	// against the live endpoint, which requires a credential for it.
	Verified string

	// Probe records why this kind's entries carry no dates, when the reason is
	// known. The only kind-level result is [ProbeCitationOnly]. It is mutually
	// exclusive with Verified: a kind whose shape was confirmed against the
	// endpoint was confirmed with a credential.
	Probe Probe

	Note string
}

// ModelRef names one catalog entry. Kind and Model are kept apart so no caller
// has to join or split them; a model name never has to survive a round trip
// through a delimiter.
type ModelRef struct {
	Kind  string
	Model string
}

// OriginKind classifies where a value came from, coarsely enough to answer
// "is this dorang's opinion or mine?" without reading a path.
type OriginKind string

// Origins. OriginDefault is the package's own fallback — the value no file
// stated — and is deliberately distinguishable from a file that happens to
// state the same thing.
const (
	OriginNone     OriginKind = ""
	OriginDefault  OriginKind = "default"
	OriginEmbedded OriginKind = "embedded"
	OriginFile     OriginKind = "file"
	OriginEnv      OriginKind = "env"
)

// Catalog field names, in their YAML spelling. [Catalog.Explain] and
// [Catalog.ExplainKind] report origins keyed by these, so an operator reads the
// name they would type into a file.
const (
	FieldAPI               = "api"
	FieldBaseURL           = "base_url"
	FieldCache             = "cache"
	FieldReasoningHint     = "reasoning" // kind-level shape hint, not a claim
	FieldCategory          = "category"
	FieldContextWindow     = "context_window"
	FieldMaxOutputTokens   = "max_output_tokens"
	FieldSupportsTools     = "supports_tools"
	FieldSupportsStreaming = "supports_streaming"
	FieldResponsesOnly     = "responses_only"
	FieldSurfaces          = "surfaces"
	FieldMetrics           = "metrics"
	FieldPriority          = "priority"
	FieldReasoning         = "reasoning"
	FieldPricing           = "pricing"
	FieldVerified          = "verified"
	FieldProbe             = "probe"
	FieldNote              = "note"
)

// Layer names, matching ModelInfo.Layers.
const (
	LayerKind   = "kind"
	LayerPrefix = "prefix"
	LayerModel  = "model"
)

// fieldSource is the internal record of which loaded layer last wrote a field.
type fieldSource struct {
	kind   OriginKind
	source string
}

// FieldOrigin says where one resolved field's value came from: which of the
// three composition layers won it, and which file wrote it.
//
// An operator whose context window is wrong needs to know which file to edit,
// and an operator who wrote an overlay needs to know whether it applied. Both
// are guesses without this.
type FieldOrigin struct {
	// Field is the YAML spelling of the field, e.g. "context_window".
	Field string

	// Layer is "kind", "prefix" or "model" — which composition layer supplied
	// the winning value. Empty when no layer did.
	Layer string

	// Origin classifies Source: embedded data, an operator file, a file named
	// by the environment, or the package's own default. OriginNone means the
	// field is undeclared everywhere and holds its zero value.
	Origin OriginKind

	// Source is the file the value came from: an embedded file name, or the
	// path exactly as it was given to the loader. Empty for OriginDefault and
	// OriginNone.
	Source string

	// Value is the resolved value rendered for display. It is for reading, not
	// for parsing; use the ModelInfo field for the typed value.
	Value string
}

// Declared reports whether any file or package default supplied this field. A
// false result means the ModelInfo field holds its zero value because nothing
// said otherwise — undeclared, which is not the same as zero (DESIGN §4.3).
func (o FieldOrigin) Declared() bool { return o.Origin != OriginNone }

// String renders one origin as a single report line.
func (o FieldOrigin) String() string {
	var b strings.Builder
	b.WriteString(o.Field)
	b.WriteString("=")
	if o.Value == "" {
		b.WriteString(`""`)
	} else {
		b.WriteString(o.Value)
	}
	if !o.Declared() {
		b.WriteString(" (undeclared)")
		return b.String()
	}
	b.WriteString(" from ")
	b.WriteString(string(o.Origin))
	if o.Source != "" {
		b.WriteString(" ")
		b.WriteString(o.Source)
	}
	if o.Layer != "" {
		b.WriteString(" layer=")
		b.WriteString(o.Layer)
	}
	return b.String()
}

// Severity separates a catalog that is wrong from one that is merely
// suspicious. An error means the data contradicts itself or the schema; a
// warning means it is shaped like a mistake that has bitten before.
type Severity string

// Severities.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Problem is one finding from [Catalog.Validate] or [LintFiles]. Findings are
// values, not errors, because the point is to report every problem in one pass
// rather than stop at the first.
type Problem struct {
	Severity Severity

	// Source is the file the problem is attributable to, empty when it cannot
	// be pinned to one (a contradiction between two files, for instance).
	Source string

	// Kind, Model and Field locate the problem. Any may be empty.
	Kind  string
	Model string
	Field string

	Message string
}

// String renders one problem as a report line. Kind and model are labelled
// rather than joined, because a model name may contain any character and a
// joined identifier would invite the splitting REVIEW C3 forbids.
func (p Problem) String() string {
	var b strings.Builder
	b.WriteString(string(p.Severity))
	b.WriteString(": ")
	if p.Source != "" {
		b.WriteString(p.Source)
		b.WriteString(": ")
	}
	if p.Kind != "" {
		b.WriteString("kind=")
		b.WriteString(p.Kind)
		b.WriteString(" ")
	}
	if p.Model != "" {
		b.WriteString("model=")
		b.WriteString(p.Model)
		b.WriteString(" ")
	}
	if p.Field != "" {
		b.WriteString(p.Field)
		b.WriteString(": ")
	}
	b.WriteString(p.Message)
	return b.String()
}

// HasErrors reports whether any problem in the list is error severity. A
// warning-only result is a catalog that loads.
func HasErrors(ps []Problem) bool {
	for _, p := range ps {
		if p.Severity == SeverityError {
			return true
		}
	}
	return false
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

	// Probe is the recorded answer from a live endpoint that did NOT establish
	// this model — an entitlement refusal, or a different model answering. It
	// is mutually exclusive with Verified, because both answer the same
	// question and a stale date beside a fresh refusal is exactly the
	// ambiguity Probe exists to remove.
	//
	// Use [Catalog.Verification] rather than reading this and Verified in
	// turn; it collapses both, plus the kind's own probe, into one answer.
	Probe Probe

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

// The chat-shaped surfaces a kind may declare in `surfaces`.
const (
	SurfaceChat      = "chat"
	SurfaceMessages  = "messages"
	SurfaceResponses = "responses"
)
