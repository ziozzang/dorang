package catalog

import (
	_ "embed"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
)

//go:embed provider_defaults.yaml
var embeddedProviderDefaults []byte

//go:embed model_catalog.yaml
var embeddedModelCatalog []byte

// prefixRule is the middle layer of a model lookup: capability defaults keyed
// by a literal prefix of the model name. It is unexported because it is not a
// lookup surface — a caller asks for a model and gets a composed answer.
type prefixRule struct {
	kind              string // "" applies to every kind
	prefix            string
	category          Category
	contextWindow     int
	maxOutputTokens   int
	supportsTools     *bool
	supportsStreaming *bool
	note              string
}

// modelEntry is the top layer of a lookup: what is declared for exactly one
// (kind, model). Booleans are pointers so an entry can switch a kind default
// off as well as on; integers use zero to mean undeclared.
type modelEntry struct {
	ref               ModelRef
	category          Category
	contextWindow     int
	maxOutputTokens   int
	supportsTools     *bool
	supportsStreaming *bool
	reasoning         Reasoning
	pricing           Pricing
	verified          string
	note              string
}

// Catalog is an immutable snapshot of provider-kind defaults and model
// entries. It is safe for concurrent use; nothing mutates after construction.
type Catalog struct {
	kinds     map[string]KindDefaults
	kindOrder []string
	aliases   map[string]string // alias -> canonical kind, fully resolved
	rules     []prefixRule      // longest prefix first
	models    map[ModelRef]modelEntry
	modelRefs []ModelRef // declaration order
}

var (
	defaultOnce sync.Once
	defaultCat  *Catalog
)

// Default returns the catalog built from the embedded data files.
//
// The returned value is shared and immutable. Embedded data that does not
// satisfy the package's own rules is a build-time defect, not a runtime
// condition, so it panics rather than returning an error nobody could act on.
func Default() *Catalog {
	defaultOnce.Do(func() {
		c, err := buildFrom(embeddedSources())
		if err != nil {
			panic(fmt.Sprintf("catalog: embedded data is invalid: %v", err))
		}
		defaultCat = c
	})
	return defaultCat
}

// Load returns the embedded catalog with operator files overlaid in order.
//
// An overlay merges field by field, so changing one key does not require
// restating an entry, and later files win. Overlays may add kinds, aliases,
// prefix rules and models, and may correct any field of an existing entry.
// They cannot remove an embedded entry: a catalog that can be silently emptied
// is a catalog whose absences mean nothing. Load with no paths is Default's
// data in an independent snapshot.
//
// The same validation applies to operator data as to embedded data, including
// the rule that a concrete reasoning capability must carry the date it was
// verified against a live endpoint.
func Load(paths ...string) (*Catalog, error) {
	srcs := embeddedSources()
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		srcs = append(srcs, source{name: p, data: data})
	}
	return buildFrom(srcs)
}

func embeddedSources() []source {
	return []source{
		{name: "provider_defaults.yaml", data: embeddedProviderDefaults},
		{name: "model_catalog.yaml", data: embeddedModelCatalog},
	}
}

func buildFrom(srcs []source) (*Catalog, error) {
	b := newBuilder()
	for _, src := range srcs {
		docs, err := decode(src)
		if err != nil {
			return nil, err
		}
		for _, doc := range docs {
			b.add(src.name, doc)
		}
	}
	c, err := b.build()
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	return c, nil
}

// canonical resolves a kind alias to the declared kind name. An unknown name
// is returned unchanged, so callers can report it as given.
func (c *Catalog) canonical(kind string) string {
	if target, ok := c.aliases[kind]; ok {
		return target
	}
	return kind
}

// Kind returns the defaults for a provider kind, resolving kind aliases.
func (c *Catalog) Kind(name string) (KindDefaults, bool) {
	kd, ok := c.kinds[c.canonical(name)]
	return kd, ok
}

// Kinds lists every declared kind name, sorted. Aliases are not included.
func (c *Catalog) Kinds() []string {
	return slices.Clone(c.kindOrder)
}

// KindAliases returns a copy of the alias table, each alias mapped to the
// declared kind it ultimately resolves to.
func (c *Catalog) KindAliases() map[string]string {
	out := make(map[string]string, len(c.aliases))
	for k, v := range c.aliases {
		out[k] = v
	}
	return out
}

// Models lists every model entry in declaration order.
func (c *Catalog) Models() []ModelRef {
	return slices.Clone(c.modelRefs)
}

// Model composes what is known about one model from three layers, later
// overriding earlier (DESIGN §4.3):
//
//  1. the kind's defaults,
//  2. the longest model-name prefix rule that literally prefixes the name,
//  3. the explicit entry for exactly this (kind, model).
//
// The model name is opaque (DESIGN §2.1, REVIEW C3). It is never split, and
// the prefix test in layer 2 supplies capability defaults only: it cannot make
// an unlisted name count as a listed model, cannot change the returned name,
// and cannot supply a reasoning capability. ModelInfo.Model always equals the
// model argument byte for byte.
//
// An unknown kind or model is not an error. The result reports which layers
// applied through KindKnown, ModelKnown and Layers, with undeclared numeric
// capabilities left at zero and reasoning left unknown.
func (c *Catalog) Model(kind, model string) ModelInfo {
	canonKind := c.canonical(kind)

	out := ModelInfo{
		Kind:      canonKind,
		Model:     model,
		Reasoning: Reasoning{Capability: ReasoningUnknown},
	}

	if kd, ok := c.kinds[canonKind]; ok {
		out.KindKnown = true
		out.API = kd.API
		out.Cache = kd.Cache
		out.Category = kd.Category
		out.ContextWindow = kd.ContextWindow
		out.MaxOutputTokens = kd.MaxOutputTokens
		out.SupportsTools = kd.SupportsTools
		out.SupportsStreaming = kd.SupportsStreaming
		out.Layers = append(out.Layers, "kind")
	}

	if rule, ok := c.matchPrefix(canonKind, model); ok {
		out.MatchedPrefix = rule.prefix
		if rule.category != "" {
			out.Category = rule.category
		}
		if rule.contextWindow > 0 {
			out.ContextWindow = rule.contextWindow
		}
		if rule.maxOutputTokens > 0 {
			out.MaxOutputTokens = rule.maxOutputTokens
		}
		if rule.supportsTools != nil {
			out.SupportsTools = *rule.supportsTools
		}
		if rule.supportsStreaming != nil {
			out.SupportsStreaming = *rule.supportsStreaming
		}
		if rule.note != "" {
			out.Note = rule.note
		}
		out.Layers = append(out.Layers, "prefix")
	}

	// Exact match on the whole name. Nothing here is prefix-based: identity
	// is identity.
	if entry, ok := c.models[ModelRef{Kind: canonKind, Model: model}]; ok {
		out.ModelKnown = true
		out.Verified = entry.verified
		if entry.category != "" {
			out.Category = entry.category
		}
		if entry.contextWindow > 0 {
			out.ContextWindow = entry.contextWindow
		}
		if entry.maxOutputTokens > 0 {
			out.MaxOutputTokens = entry.maxOutputTokens
		}
		if entry.supportsTools != nil {
			out.SupportsTools = *entry.supportsTools
		}
		if entry.supportsStreaming != nil {
			out.SupportsStreaming = *entry.supportsStreaming
		}
		out.Reasoning = entry.reasoning
		out.Reasoning.Levels = slices.Clone(entry.reasoning.Levels)
		out.Pricing = entry.pricing
		if entry.note != "" {
			out.Note = entry.note
		}
		out.Layers = append(out.Layers, "model")
	}

	return out
}

// matchPrefix returns the longest rule whose prefix literally prefixes the
// model name, preferring a kind-scoped rule over a kind-agnostic one of the
// same length. Rules are pre-sorted, so the first match is the answer.
func (c *Catalog) matchPrefix(kind, model string) (prefixRule, bool) {
	for _, r := range c.rules {
		if r.kind != "" && r.kind != kind {
			continue
		}
		if strings.HasPrefix(model, r.prefix) {
			return r, true
		}
	}
	return prefixRule{}, false
}

// Reasoning returns the reasoning control the model accepts.
//
// It is keyed by (kind, model), never by kind alone (REVIEW C5), and comes
// only from an explicit, dated model entry. Everything else — an unlisted
// model, a listed model that was never probed, an unknown kind — returns
// ReasoningUnknown. A caller that gets unknown must omit the control and
// report it in x-dorang-dropped-params (DESIGN §10.2), not guess a shape from
// the kind, from a sibling model, or from the model's name.
func (c *Catalog) Reasoning(kind, model string) Reasoning {
	return c.Model(kind, model).Reasoning
}

// UnverifiedModels lists every catalog entry whose reasoning capability is
// still unknown, in declaration order, as human-readable report lines of the
// form "kind=<kind> model=<model>".
//
// The lines are labelled rather than joined by a delimiter on purpose: a model
// name may contain any character, so a joined identifier would invite exactly
// the splitting REVIEW C3 forbids. They are for an operator to read, not for a
// program to parse; use Models and Reasoning for that.
//
// A non-empty result is the expected state. Filling an entry in means probing
// the live endpoint and recording the date, and this list is how an operator
// sees what is left to do.
func (c *Catalog) UnverifiedModels() []string {
	var out []string
	for _, ref := range c.modelRefs {
		if c.models[ref].reasoning.Known() {
			continue
		}
		out = append(out, fmt.Sprintf("kind=%s model=%s", ref.Kind, ref.Model))
	}
	return out
}
