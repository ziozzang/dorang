package catalog

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	origins           map[string]fieldSource
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
	probe             Probe
	note              string
	origins           map[string]fieldSource
}

// Catalog is an immutable snapshot of provider-kind defaults and model
// entries. It is safe for concurrent use; nothing mutates after construction.
type Catalog struct {
	kinds       map[string]KindDefaults
	kindOrder   []string
	kindOrigins map[string]map[string]fieldSource

	aliases      map[string]string // alias -> canonical kind, fully resolved
	aliasOrigins map[string]fieldSource

	rules     []prefixRule // longest prefix first
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

// EnvCatalogPath names the environment variable holding a PATH-style list of
// catalog files and directories, separated by [os.PathListSeparator] (':' on
// Unix). It is applied as the LAST overlay layer, after everything the
// configuration named, so an operator can correct a running deployment without
// editing its configuration file.
const EnvCatalogPath = "DORANG_CATALOG_PATH"

// Loader describes the layers to compose into a Catalog.
//
// The embedded data is always the base layer. It is a default, not a ceiling:
// every later layer may correct it, and may introduce provider kinds, aliases,
// prefix rules and models that dorang has never heard of. An operator can run
// against a provider this build has no knowledge of using configuration alone.
type Loader struct {
	// Paths are files or directories, applied in order after the embedded
	// data. A directory contributes its *.yaml and *.yml entries, sorted by
	// name, non-recursively — so a deployment can keep one file per provider
	// and rely on the ordering being the one it sees in a listing.
	Paths []string

	// EnvVar names an environment variable holding a PATH-style list of files
	// and directories, applied after Paths. Empty disables the layer;
	// [Load] sets it to [EnvCatalogPath].
	EnvVar string
}

// Load composes the catalog described by l.
//
// Layers compose field by field, later winning, so an overlay changes one key
// without restating an entry. An entry may instead say `merge: replace` to
// discard everything inherited for it, which is the only way to REMOVE a wrong
// inherited value rather than change it.
//
// No layer can remove an entry outright: a catalog that can be silently
// emptied is a catalog whose absences mean nothing. Correct entries; do not
// delete them.
//
// The same validation applies to operator data as to embedded data, including
// the rule that a concrete reasoning capability must carry the date it was
// verified against a live endpoint. [Catalog.Explain] reports which layer each
// resolved field came from.
func (l Loader) Load() (*Catalog, error) {
	srcs := embeddedSources()

	for _, p := range l.Paths {
		got, err := expand(p, OriginFile)
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		srcs = append(srcs, got...)
	}

	if l.EnvVar != "" {
		for _, p := range splitEnvPaths(os.Getenv(l.EnvVar)) {
			got, err := expand(p, OriginEnv)
			if err != nil {
				return nil, fmt.Errorf("catalog: %s: %w", l.EnvVar, err)
			}
			srcs = append(srcs, got...)
		}
	}

	return buildFrom(srcs)
}

// Load returns the embedded catalog with operator layers overlaid in order:
// each element of paths — a file or a directory — and then whatever
// [EnvCatalogPath] names.
//
// Load with no paths and no environment variable set is [Default]'s data in an
// independent snapshot. To load without consulting the environment, use
// Loader{Paths: paths}.Load().
func Load(paths ...string) (*Catalog, error) {
	return Loader{Paths: paths, EnvVar: EnvCatalogPath}.Load()
}

// EnvPaths returns the entries of [EnvCatalogPath], in order, with empty
// segments dropped. It is exported so a `dorangctl lint` can check exactly
// what a `dorangctl serve` would load:
//
//	catalog.LintFiles(append(configured, catalog.EnvPaths()...)...)
func EnvPaths() []string {
	return splitEnvPaths(os.Getenv(EnvCatalogPath))
}

// splitEnvPaths splits a PATH-style list, dropping empty segments so that a
// trailing or doubled separator is harmless rather than an error.
func splitEnvPaths(v string) []string {
	var out []string
	for _, p := range filepath.SplitList(v) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// catalogFileExts are the extensions a directory layer picks up.
var catalogFileExts = []string{".yaml", ".yml"}

// expand turns one path into the sources it contributes: a file is itself, a
// directory is its *.yaml and *.yml entries sorted by name.
//
// A path that does not exist is an error, including one named by the
// environment. PATH-like variables conventionally skip missing entries, but
// this package exists so that "did my overlay apply?" has an answer, and a
// silently skipped layer is exactly that question with no answer.
func expand(path string, origin OriginKind) ([]source, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return []source{{name: path, origin: origin, data: data}}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if slices.Contains(catalogFileExts, ext) {
			names = append(names, e.Name())
		}
	}
	// os.ReadDir already sorts, but the contract is worth being explicit
	// about: an operator reading a directory listing must be able to predict
	// which file wins.
	slices.Sort(names)

	out := make([]source, 0, len(names))
	for _, n := range names {
		full := filepath.Join(path, n)
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		out = append(out, source{name: full, origin: origin, data: data})
	}
	return out, nil
}

func embeddedSources() []source {
	return []source{
		{name: "provider_defaults.yaml", origin: OriginEmbedded, data: embeddedProviderDefaults},
		{name: "model_catalog.yaml", origin: OriginEmbedded, data: embeddedModelCatalog},
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
			b.add(src, doc)
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
	info, _ := c.compose(kind, model, false)
	return info
}

// compose is Model, optionally recording where each winning field came from.
// One implementation, so Explain can never drift from the answer it explains.
func (c *Catalog) compose(kind, model string, trace bool) (ModelInfo, map[string]FieldOrigin) {
	canonKind := c.canonical(kind)

	out := ModelInfo{
		Kind:      canonKind,
		Model:     model,
		Reasoning: Reasoning{Capability: ReasoningUnknown},
	}

	var org map[string]FieldOrigin
	if trace {
		org = make(map[string]FieldOrigin, len(modelInfoFields))
	}
	// note records a field's winning layer and source; a no-op when untraced.
	note := func(field, layer string, origins map[string]fieldSource) {
		if !trace {
			return
		}
		fs := origins[field]
		org[field] = FieldOrigin{Field: field, Layer: layer, Origin: fs.kind, Source: fs.source}
	}

	if kd, ok := c.kinds[canonKind]; ok {
		ko := c.kindOrigins[canonKind]
		out.KindKnown = true
		out.API = kd.API
		out.Cache = kd.Cache
		out.Category = kd.Category
		out.ContextWindow = kd.ContextWindow
		out.MaxOutputTokens = kd.MaxOutputTokens
		out.SupportsTools = kd.SupportsTools
		out.SupportsStreaming = kd.SupportsStreaming
		out.Layers = append(out.Layers, LayerKind)

		note(FieldAPI, LayerKind, ko)
		note(FieldCache, LayerKind, ko)
		note(FieldCategory, LayerKind, ko)
		if kd.ContextWindow > 0 {
			note(FieldContextWindow, LayerKind, ko)
		}
		if kd.MaxOutputTokens > 0 {
			note(FieldMaxOutputTokens, LayerKind, ko)
		}
		note(FieldSupportsTools, LayerKind, ko)
		note(FieldSupportsStreaming, LayerKind, ko)
	}

	if rule, ok := c.matchPrefix(canonKind, model); ok {
		out.MatchedPrefix = rule.prefix
		if rule.category != "" {
			out.Category = rule.category
			note(FieldCategory, LayerPrefix, rule.origins)
		}
		if rule.contextWindow > 0 {
			out.ContextWindow = rule.contextWindow
			note(FieldContextWindow, LayerPrefix, rule.origins)
		}
		if rule.maxOutputTokens > 0 {
			out.MaxOutputTokens = rule.maxOutputTokens
			note(FieldMaxOutputTokens, LayerPrefix, rule.origins)
		}
		if rule.supportsTools != nil {
			out.SupportsTools = *rule.supportsTools
			note(FieldSupportsTools, LayerPrefix, rule.origins)
		}
		if rule.supportsStreaming != nil {
			out.SupportsStreaming = *rule.supportsStreaming
			note(FieldSupportsStreaming, LayerPrefix, rule.origins)
		}
		if rule.note != "" {
			out.Note = rule.note
			note(FieldNote, LayerPrefix, rule.origins)
		}
		out.Layers = append(out.Layers, LayerPrefix)
	}

	// Exact match on the whole name. Nothing here is prefix-based: identity
	// is identity.
	if entry, ok := c.models[ModelRef{Kind: canonKind, Model: model}]; ok {
		mo := entry.origins
		out.ModelKnown = true
		out.Verified = entry.verified
		out.Probe = entry.probe
		note(FieldVerified, LayerModel, mo)
		if !entry.probe.IsZero() {
			note(FieldProbe, LayerModel, mo)
		}
		if entry.category != "" {
			out.Category = entry.category
			note(FieldCategory, LayerModel, mo)
		}
		if entry.contextWindow > 0 {
			out.ContextWindow = entry.contextWindow
			note(FieldContextWindow, LayerModel, mo)
		}
		if entry.maxOutputTokens > 0 {
			out.MaxOutputTokens = entry.maxOutputTokens
			note(FieldMaxOutputTokens, LayerModel, mo)
		}
		if entry.supportsTools != nil {
			out.SupportsTools = *entry.supportsTools
			note(FieldSupportsTools, LayerModel, mo)
		}
		if entry.supportsStreaming != nil {
			out.SupportsStreaming = *entry.supportsStreaming
			note(FieldSupportsStreaming, LayerModel, mo)
		}
		out.Reasoning = entry.reasoning
		out.Reasoning.Levels = slices.Clone(entry.reasoning.Levels)
		out.Pricing = entry.pricing
		if entry.reasoning.Effective() != ReasoningUnknown {
			note(FieldReasoning, LayerModel, mo)
		}
		if !entry.pricing.IsZero() {
			note(FieldPricing, LayerModel, mo)
		}
		if entry.note != "" {
			out.Note = entry.note
			note(FieldNote, LayerModel, mo)
		}
		out.Layers = append(out.Layers, LayerModel)
	}

	return out, org
}

// modelInfoFields is the order Explain reports in: the shape of a lookup
// answer, not alphabetical, so the reasoning claim and the numbers that bound
// it stay next to each other.
var modelInfoFields = []string{
	FieldAPI,
	FieldCache,
	FieldCategory,
	FieldContextWindow,
	FieldMaxOutputTokens,
	FieldSupportsTools,
	FieldSupportsStreaming,
	FieldReasoning,
	FieldPricing,
	FieldVerified,
	FieldProbe,
	FieldNote,
}

// Explain reports, for every field of [Catalog.Model]'s answer, which layer
// supplied the winning value and which file wrote it.
//
// An operator debugging a wrong context window needs to know which file to
// edit, and an operator who has just written an overlay needs to know whether
// it applied at all. Both are guesswork otherwise — the composition is three
// layers deep across an arbitrary number of files, and an overlay that missed
// (wrong kind spelling, wrong model name by one byte) looks exactly like an
// overlay that applied and agreed.
//
// Every field is reported, in a fixed order. A field nothing declared comes
// back with Origin [OriginNone] and [FieldOrigin.Declared] false, which is how
// "undeclared" is told apart from "declared as zero" (DESIGN §4.3).
func (c *Catalog) Explain(kind, model string) []FieldOrigin {
	info, org := c.compose(kind, model, true)

	out := make([]FieldOrigin, 0, len(modelInfoFields))
	for _, f := range modelInfoFields {
		o := org[f]
		o.Field = f
		o.Value = renderModelField(info, f)
		out = append(out, o)
	}
	return out
}

// ExplainKind reports where each field of a provider kind's defaults came
// from, resolving kind aliases. The bool is false for an undeclared kind.
func (c *Catalog) ExplainKind(name string) ([]FieldOrigin, bool) {
	canon := c.canonical(name)
	kd, ok := c.kinds[canon]
	if !ok {
		return nil, false
	}
	origins := c.kindOrigins[canon]

	fields := []string{
		FieldAPI, FieldBaseURL, FieldCache, FieldReasoningHint, FieldCategory,
		FieldContextWindow, FieldMaxOutputTokens,
		FieldSupportsTools, FieldSupportsStreaming, FieldResponsesOnly, FieldSurfaces,
		FieldMetrics, FieldPriority, FieldVerified, FieldProbe, FieldNote,
	}
	out := make([]FieldOrigin, 0, len(fields))
	for _, f := range fields {
		fs := origins[f]
		out = append(out, FieldOrigin{
			Field:  f,
			Layer:  LayerKind,
			Origin: fs.kind,
			Source: fs.source,
			Value:  renderKindField(kd, f),
		})
	}
	return out, true
}

// AliasOrigin reports which file declared a kind alias, and the kind it
// resolves to. The bool is false when name is not an alias.
func (c *Catalog) AliasOrigin(name string) (target string, origin FieldOrigin, ok bool) {
	target, ok = c.aliases[name]
	if !ok {
		return "", FieldOrigin{}, false
	}
	fs := c.aliasOrigins[name]
	return target, FieldOrigin{
		Field:  "kind_aliases",
		Origin: fs.kind,
		Source: fs.source,
		Value:  target,
	}, true
}

func renderModelField(info ModelInfo, f string) string {
	switch f {
	case FieldAPI:
		return string(info.API)
	case FieldCache:
		return string(info.Cache)
	case FieldCategory:
		return string(info.Category)
	case FieldContextWindow:
		return strconv.Itoa(info.ContextWindow)
	case FieldMaxOutputTokens:
		return strconv.Itoa(info.MaxOutputTokens)
	case FieldSupportsTools:
		return strconv.FormatBool(info.SupportsTools)
	case FieldSupportsStreaming:
		return strconv.FormatBool(info.SupportsStreaming)
	case FieldReasoning:
		return info.Reasoning.String()
	case FieldPricing:
		return renderPricing(info.Pricing)
	case FieldVerified:
		return info.Verified
	case FieldProbe:
		return info.Probe.String()
	case FieldNote:
		return info.Note
	}
	return ""
}

func renderKindField(kd KindDefaults, f string) string {
	switch f {
	case FieldAPI:
		return string(kd.API)
	case FieldBaseURL:
		return kd.BaseURL
	case FieldCache:
		return string(kd.Cache)
	case FieldReasoningHint:
		return string(kd.ReasoningHint)
	case FieldCategory:
		return string(kd.Category)
	case FieldContextWindow:
		return strconv.Itoa(kd.ContextWindow)
	case FieldMaxOutputTokens:
		return strconv.Itoa(kd.MaxOutputTokens)
	case FieldSupportsTools:
		return strconv.FormatBool(kd.SupportsTools)
	case FieldSupportsStreaming:
		return strconv.FormatBool(kd.SupportsStreaming)
	case FieldResponsesOnly:
		return strconv.FormatBool(kd.ResponsesOnly)
	case FieldSurfaces:
		return strings.Join(kd.Surfaces, ",")
	case FieldMetrics:
		return kd.Metrics
	case FieldPriority:
		return kd.Priority
	case FieldVerified:
		return kd.Verified
	case FieldProbe:
		return kd.Probe.String()
	case FieldNote:
		return kd.Note
	}
	return ""
}

func renderPricing(p Pricing) string {
	if p.IsZero() {
		return ""
	}
	var b strings.Builder
	if p.Currency != "" {
		b.WriteString(p.Currency)
		b.WriteString(" ")
	}
	b.WriteString("in=")
	b.WriteString(string(p.InputPerMTok))
	b.WriteString(" out=")
	b.WriteString(string(p.OutputPerMTok))
	if p.CachedInputPerMTok != "" {
		b.WriteString(" cached=")
		b.WriteString(string(p.CachedInputPerMTok))
	}
	return b.String()
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

// Verification answers "what happened the last time anybody asked the endpoint
// about this model?" — the question `verified:` alone could only half answer.
//
// A date meant asked-and-answered. Its absence meant four unrelated things at
// once: nobody asked; somebody asked and the plan was not entitled; somebody
// asked and a different model answered; nobody here could ask at all. They call
// for opposite actions, and only the last two are gaps. This collapses the
// entry's date, the entry's probe and the kind's probe into one answer, in that
// order of specificity.
//
// An unknown kind or model is [VerificationUnchecked]: nobody has asked about a
// model the catalog does not list, which is true and is the safe reading.
func (c *Catalog) Verification(kind, model string) Verification {
	canon := c.canonical(kind)
	if entry, ok := c.models[ModelRef{Kind: canon, Model: model}]; ok {
		if entry.verified != "" {
			return VerificationVerified
		}
		switch entry.probe.Result {
		case ProbeDenied:
			return VerificationDenied
		case ProbeSubstituted:
			return VerificationSubstituted
		}
	}
	// The kind speaks only where the entry is silent: an entry that was
	// probed was probed, whatever the route's general state.
	if kd, ok := c.kinds[canon]; ok && kd.Probe.Result == ProbeCitationOnly {
		return VerificationCitationOnly
	}
	return VerificationUnchecked
}

// ModelsByVerification groups every catalog entry by [Catalog.Verification],
// each group in declaration order.
//
// It is what a report is built from. Every state is present as a key, including
// the empty ones, so a caller iterating [VerificationStates] renders a stable
// table rather than a table whose rows appear and vanish with the data.
func (c *Catalog) ModelsByVerification() map[Verification][]ModelRef {
	out := make(map[Verification][]ModelRef, len(verificationOrder))
	for _, v := range verificationOrder {
		out[v] = nil
	}
	for _, ref := range c.modelRefs {
		v := c.Verification(ref.Kind, ref.Model)
		out[v] = append(out[v], ref)
	}
	return out
}

// Probe returns the recorded live answer for one model that did not establish
// it, or the zero Probe when there is none. It resolves kind aliases.
//
// A caller wanting to know the state should use [Catalog.Verification]; this is
// for reporting the evidence behind it — the refusal text, the substituted id.
func (c *Catalog) Probe(kind, model string) Probe {
	return c.Model(kind, model).Probe
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
