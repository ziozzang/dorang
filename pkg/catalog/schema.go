package catalog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// maxAliasHops bounds kind-alias resolution so a cycle is reported at load
// time instead of hanging a lookup.
const maxAliasHops = 8

// fileDoc is the on-disk shape of both embedded files and of any operator
// overlay. One schema for all of them: an operator overlay can carry kinds,
// prefix rules and models in a single file.
type fileDoc struct {
	Version     *int                `yaml:"version"`
	KindAliases map[string]string   `yaml:"kind_aliases"`
	Kinds       map[string]*kindDoc `yaml:"kinds"`
	PrefixRules []*prefixDoc        `yaml:"prefix_rules"`
	Models      []*modelDoc         `yaml:"models"`
}

// Pointer fields distinguish "absent" from "set to the zero value", which is
// what makes field-level overlay merging possible.
type kindDoc struct {
	API               *string `yaml:"api"`
	Cache             *string `yaml:"cache"`
	Reasoning         *string `yaml:"reasoning"`
	Category          *string `yaml:"category"`
	ContextWindow     *int    `yaml:"context_window"`
	MaxOutputTokens   *int    `yaml:"max_output_tokens"`
	SupportsTools     *bool   `yaml:"supports_tools"`
	SupportsStreaming *bool   `yaml:"supports_streaming"`
	Metrics           *string `yaml:"metrics"`
	Priority          *string `yaml:"priority"`
	Verified          *string `yaml:"verified"`
	Note              *string `yaml:"note"`
}

// prefixDoc has no reasoning field, and that is deliberate. A prefix rule
// generalizes across every present and future member of a name family; letting
// it carry a reasoning capability would reintroduce REVIEW C5 by construction.
type prefixDoc struct {
	Kind              string  `yaml:"kind"`
	Prefix            string  `yaml:"prefix"`
	Category          *string `yaml:"category"`
	ContextWindow     *int    `yaml:"context_window"`
	MaxOutputTokens   *int    `yaml:"max_output_tokens"`
	SupportsTools     *bool   `yaml:"supports_tools"`
	SupportsStreaming *bool   `yaml:"supports_streaming"`
	Note              *string `yaml:"note"`
}

type modelDoc struct {
	Kind              string        `yaml:"kind"`
	Model             string        `yaml:"model"`
	Category          *string       `yaml:"category"`
	ContextWindow     *int          `yaml:"context_window"`
	MaxOutputTokens   *int          `yaml:"max_output_tokens"`
	SupportsTools     *bool         `yaml:"supports_tools"`
	SupportsStreaming *bool         `yaml:"supports_streaming"`
	Reasoning         *reasoningDoc `yaml:"reasoning"`
	Pricing           *pricingDoc   `yaml:"pricing"`
	Verified          *string       `yaml:"verified"`
	Note              *string       `yaml:"note"`
}

type reasoningDoc struct {
	Capability      string   `yaml:"capability"`
	Levels          []string `yaml:"levels"`
	DefaultLevel    string   `yaml:"default_level"`
	MinBudgetTokens int      `yaml:"min_budget_tokens"`
	MaxBudgetTokens int      `yaml:"max_budget_tokens"`
	Verified        string   `yaml:"verified"`
	Note            string   `yaml:"note"`
}

type pricingDoc struct {
	Currency           string  `yaml:"currency"`
	InputPerMTok       Decimal `yaml:"input_per_mtok"`
	OutputPerMTok      Decimal `yaml:"output_per_mtok"`
	CachedInputPerMTok Decimal `yaml:"cached_input_per_mtok"`
	Verified           string  `yaml:"verified"`
	Note               string  `yaml:"note"`
}

// source pairs raw bytes with a name used in error messages.
type source struct {
	name string
	data []byte
}

// decode parses every YAML document in one source. Unknown fields are an
// error: an operator who misspells a key deserves to hear about it at load
// time, not to wonder why the override did nothing.
func decode(src source) ([]*fileDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src.data))
	dec.KnownFields(true)

	var docs []*fileDoc
	for {
		var doc fileDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src.name, err)
		}
		docs = append(docs, &doc)
	}
}

// builder accumulates merged documents before they are frozen into a Catalog.
type builder struct {
	aliases   map[string]string
	kinds     map[string]*kindDoc
	kindOrder []string
	rules     map[ruleKey]*prefixDoc
	ruleOrder []ruleKey
	models    map[ModelRef]*modelDoc
	modelRefs []ModelRef
	errs      []error
}

type ruleKey struct {
	kind   string
	prefix string
}

func newBuilder() *builder {
	return &builder{
		aliases: map[string]string{},
		kinds:   map[string]*kindDoc{},
		rules:   map[ruleKey]*prefixDoc{},
		models:  map[ModelRef]*modelDoc{},
	}
}

func (b *builder) errf(format string, args ...any) {
	b.errs = append(b.errs, fmt.Errorf(format, args...))
}

// add folds one document into the builder. Later documents win field by field,
// so an overlay may change a single key without restating an entry.
func (b *builder) add(src string, doc *fileDoc) {
	if doc.Version != nil && *doc.Version != 1 {
		b.errf("%s: unsupported version %d, want 1", src, *doc.Version)
	}

	for alias, target := range doc.KindAliases {
		if alias == "" || target == "" {
			b.errf("%s: kind_aliases has an empty alias or target", src)
			continue
		}
		b.aliases[alias] = target
	}

	for name, in := range doc.Kinds {
		if name == "" {
			b.errf("%s: kinds has an empty name", src)
			continue
		}
		if in == nil {
			in = &kindDoc{}
		}
		cur, ok := b.kinds[name]
		if !ok {
			cur = &kindDoc{}
			b.kinds[name] = cur
			b.kindOrder = append(b.kindOrder, name)
		}
		mergeKind(cur, in)
	}

	for _, in := range doc.PrefixRules {
		if in == nil || in.Prefix == "" {
			b.errf("%s: prefix_rules entry has an empty prefix", src)
			continue
		}
		key := ruleKey{kind: in.Kind, prefix: in.Prefix}
		cur, ok := b.rules[key]
		if !ok {
			cur = &prefixDoc{Kind: in.Kind, Prefix: in.Prefix}
			b.rules[key] = cur
			b.ruleOrder = append(b.ruleOrder, key)
		}
		mergePrefix(cur, in)
	}

	for _, in := range doc.Models {
		if in == nil || in.Kind == "" || in.Model == "" {
			b.errf("%s: models entry needs both kind and model", src)
			continue
		}
		ref := ModelRef{Kind: in.Kind, Model: in.Model}
		cur, ok := b.models[ref]
		if !ok {
			cur = &modelDoc{Kind: in.Kind, Model: in.Model}
			b.models[ref] = cur
			b.modelRefs = append(b.modelRefs, ref)
		}
		mergeModel(cur, in)
	}
}

func mergeKind(dst, src *kindDoc) {
	setStr(&dst.API, src.API)
	setStr(&dst.Cache, src.Cache)
	setStr(&dst.Reasoning, src.Reasoning)
	setStr(&dst.Category, src.Category)
	setInt(&dst.ContextWindow, src.ContextWindow)
	setInt(&dst.MaxOutputTokens, src.MaxOutputTokens)
	setBool(&dst.SupportsTools, src.SupportsTools)
	setBool(&dst.SupportsStreaming, src.SupportsStreaming)
	setStr(&dst.Metrics, src.Metrics)
	setStr(&dst.Priority, src.Priority)
	setStr(&dst.Verified, src.Verified)
	setStr(&dst.Note, src.Note)
}

func mergePrefix(dst, src *prefixDoc) {
	setStr(&dst.Category, src.Category)
	setInt(&dst.ContextWindow, src.ContextWindow)
	setInt(&dst.MaxOutputTokens, src.MaxOutputTokens)
	setBool(&dst.SupportsTools, src.SupportsTools)
	setBool(&dst.SupportsStreaming, src.SupportsStreaming)
	setStr(&dst.Note, src.Note)
}

func mergeModel(dst, src *modelDoc) {
	setStr(&dst.Category, src.Category)
	setInt(&dst.ContextWindow, src.ContextWindow)
	setInt(&dst.MaxOutputTokens, src.MaxOutputTokens)
	setBool(&dst.SupportsTools, src.SupportsTools)
	setBool(&dst.SupportsStreaming, src.SupportsStreaming)
	setStr(&dst.Verified, src.Verified)
	setStr(&dst.Note, src.Note)
	// Reasoning and pricing are replaced whole. Capability, levels and the
	// verification date are one claim; merging them field by field could
	// leave a capability standing next to a date that never covered it.
	if src.Reasoning != nil {
		dst.Reasoning = src.Reasoning
	}
	if src.Pricing != nil {
		dst.Pricing = src.Pricing
	}
}

func setStr(dst **string, src *string) {
	if src != nil {
		v := *src
		*dst = &v
	}
}

func setInt(dst **int, src *int) {
	if src != nil {
		v := *src
		*dst = &v
	}
}

func setBool(dst **bool, src *bool) {
	if src != nil {
		v := *src
		*dst = &v
	}
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func intv(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func boolv(p *bool) bool {
	return p != nil && *p
}

// build validates the accumulated documents and freezes them into a Catalog.
// Every problem is collected, so one load reports every defect in an operator
// file rather than one per attempt.
func (b *builder) build() (*Catalog, error) {
	c := &Catalog{
		kinds:   make(map[string]KindDefaults, len(b.kinds)),
		aliases: make(map[string]string, len(b.aliases)),
		models:  make(map[ModelRef]modelEntry, len(b.models)),
	}

	slices.Sort(b.kindOrder)
	for _, name := range b.kindOrder {
		kd, err := resolveKind(name, b.kinds[name])
		if err != nil {
			b.errs = append(b.errs, err)
			continue
		}
		c.kinds[name] = kd
		c.kindOrder = append(c.kindOrder, name)
	}

	for alias := range b.aliases {
		if _, clash := c.kinds[alias]; clash {
			b.errf("kind_aliases: %q is also a declared kind", alias)
			continue
		}
		target, err := b.resolveAlias(alias, c.kinds)
		if err != nil {
			b.errs = append(b.errs, err)
			continue
		}
		c.aliases[alias] = target
	}

	for _, key := range b.ruleOrder {
		in := b.rules[key]
		if in.Kind != "" {
			if _, ok := c.kinds[c.canonical(in.Kind)]; !ok {
				b.errf("prefix_rules[%q]: unknown kind %q", in.Prefix, in.Kind)
				continue
			}
		}
		cat := Category(str(in.Category))
		if cat != "" && !validCategories[cat] {
			b.errf("prefix_rules[%q]: unknown category %q", in.Prefix, cat)
			continue
		}
		c.rules = append(c.rules, prefixRule{
			kind:              c.canonical(in.Kind),
			prefix:            in.Prefix,
			category:          cat,
			contextWindow:     intv(in.ContextWindow),
			maxOutputTokens:   intv(in.MaxOutputTokens),
			supportsTools:     in.SupportsTools,
			supportsStreaming: in.SupportsStreaming,
			note:              str(in.Note),
		})
	}
	// Longest prefix wins; a kind-scoped rule breaks a length tie against a
	// rule that applies to every kind; the prefix itself breaks the rest, so
	// selection is deterministic regardless of declaration order.
	slices.SortStableFunc(c.rules, func(x, y prefixRule) int {
		if d := len(y.prefix) - len(x.prefix); d != 0 {
			return d
		}
		if x.kind != y.kind {
			if x.kind == "" {
				return 1
			}
			if y.kind == "" {
				return -1
			}
		}
		return strings.Compare(x.prefix, y.prefix)
	})

	for _, ref := range b.modelRefs {
		in := b.models[ref]
		canon := ModelRef{Kind: c.canonical(ref.Kind), Model: ref.Model}
		if _, ok := c.kinds[canon.Kind]; !ok {
			b.errf("models[%q]: unknown kind %q", ref.Model, ref.Kind)
			continue
		}
		entry, err := resolveModel(canon, in)
		if err != nil {
			b.errs = append(b.errs, err)
			continue
		}
		if _, dup := c.models[canon]; !dup {
			c.modelRefs = append(c.modelRefs, canon)
		}
		c.models[canon] = entry
	}

	if len(b.errs) > 0 {
		return nil, errors.Join(b.errs...)
	}
	return c, nil
}

// resolveAlias follows an alias chain to a declared kind, refusing a cycle.
func (b *builder) resolveAlias(alias string, kinds map[string]KindDefaults) (string, error) {
	name := alias
	for hop := 0; hop < maxAliasHops; hop++ {
		target, isAlias := b.aliases[name]
		if !isAlias {
			if _, ok := kinds[name]; ok {
				return name, nil
			}
			return "", fmt.Errorf("kind_aliases[%q]: %q is not a declared kind", alias, name)
		}
		if target == alias {
			return "", fmt.Errorf("kind_aliases[%q]: alias cycle", alias)
		}
		name = target
	}
	return "", fmt.Errorf("kind_aliases[%q]: chain longer than %d hops, probably a cycle", alias, maxAliasHops)
}

func resolveKind(name string, in *kindDoc) (KindDefaults, error) {
	var errs []error

	api := API(str(in.API))
	if api == "" {
		errs = append(errs, fmt.Errorf("kinds[%q]: api is required", name))
	} else if !validAPIs[api] {
		errs = append(errs, fmt.Errorf("kinds[%q]: unknown api %q", name, api))
	}

	cache := CacheScheme(str(in.Cache))
	if cache == "" {
		cache = CacheNone
	}
	if !validCaches[cache] {
		errs = append(errs, fmt.Errorf("kinds[%q]: unknown cache scheme %q", name, cache))
	}

	hint := ReasoningCapability(str(in.Reasoning))
	if hint == "" {
		hint = ReasoningUnknown
	}
	if !validReasoning[hint] {
		errs = append(errs, fmt.Errorf("kinds[%q]: unknown reasoning shape %q", name, hint))
	}

	cat := Category(str(in.Category))
	if cat == "" {
		cat = CategoryChat
	}
	if !validCategories[cat] {
		errs = append(errs, fmt.Errorf("kinds[%q]: unknown category %q", name, cat))
	}

	if err := checkDate(str(in.Verified)); err != nil {
		errs = append(errs, fmt.Errorf("kinds[%q]: verified: %w", name, err))
	}
	if v := intv(in.ContextWindow); v < 0 {
		errs = append(errs, fmt.Errorf("kinds[%q]: context_window is negative", name))
	}
	if v := intv(in.MaxOutputTokens); v < 0 {
		errs = append(errs, fmt.Errorf("kinds[%q]: max_output_tokens is negative", name))
	}

	if len(errs) > 0 {
		return KindDefaults{}, errors.Join(errs...)
	}
	return KindDefaults{
		Name:              name,
		API:               api,
		Cache:             cache,
		ReasoningHint:     hint,
		Category:          cat,
		ContextWindow:     intv(in.ContextWindow),
		MaxOutputTokens:   intv(in.MaxOutputTokens),
		SupportsTools:     boolv(in.SupportsTools),
		SupportsStreaming: boolv(in.SupportsStreaming),
		Metrics:           str(in.Metrics),
		Priority:          str(in.Priority),
		Verified:          str(in.Verified),
		Note:              str(in.Note),
	}, nil
}

// resolveModel turns one model document into the top lookup layer. Booleans
// stay pointers so an entry can switch a kind default off, not only on; ints
// stay zero-means-undeclared.
func resolveModel(ref ModelRef, in *modelDoc) (modelEntry, error) {
	var errs []error
	where := fmt.Sprintf("models[kind=%s model=%s]", ref.Kind, ref.Model)

	cat := Category(str(in.Category))
	if cat != "" && !validCategories[cat] {
		errs = append(errs, fmt.Errorf("%s: unknown category %q", where, cat))
	}
	if err := checkDate(str(in.Verified)); err != nil {
		errs = append(errs, fmt.Errorf("%s: verified: %w", where, err))
	}
	if v := intv(in.ContextWindow); v < 0 {
		errs = append(errs, fmt.Errorf("%s: context_window is negative", where))
	}
	if v := intv(in.MaxOutputTokens); v < 0 {
		errs = append(errs, fmt.Errorf("%s: max_output_tokens is negative", where))
	}

	reasoning, err := resolveReasoning(where, in.Reasoning)
	if err != nil {
		errs = append(errs, err)
	}
	pricing, err := resolvePricing(where, in.Pricing)
	if err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return modelEntry{}, errors.Join(errs...)
	}
	return modelEntry{
		ref:               ref,
		category:          cat,
		contextWindow:     intv(in.ContextWindow),
		maxOutputTokens:   intv(in.MaxOutputTokens),
		supportsTools:     in.SupportsTools,
		supportsStreaming: in.SupportsStreaming,
		reasoning:         reasoning,
		pricing:           pricing,
		verified:          str(in.Verified),
		note:              str(in.Note),
	}, nil
}

// resolveReasoning enforces the rule the whole package exists for: a concrete
// capability requires a verification date, and an entry without one may only
// claim unknown.
func resolveReasoning(where string, in *reasoningDoc) (Reasoning, error) {
	if in == nil {
		return Reasoning{Capability: ReasoningUnknown}, nil
	}

	var errs []error
	capability := ReasoningCapability(in.Capability)
	if capability == "" {
		capability = ReasoningUnknown
	}
	if !validReasoning[capability] {
		return Reasoning{Capability: ReasoningUnknown},
			fmt.Errorf("%s: unknown reasoning capability %q", where, in.Capability)
	}

	if err := checkDate(in.Verified); err != nil {
		errs = append(errs, fmt.Errorf("%s: reasoning.verified: %w", where, err))
	}

	switch {
	case capability == ReasoningUnknown && in.Verified != "":
		errs = append(errs, fmt.Errorf(
			"%s: reasoning capability is unknown but carries a verified date; "+
				"a date records what was checked, and nothing was", where))
	case capability != ReasoningUnknown && in.Verified == "":
		errs = append(errs, fmt.Errorf(
			"%s: reasoning capability %q needs a verified date; an entry "+
				"without one may only claim %q", where, capability, ReasoningUnknown))
	}

	switch capability {
	case ReasoningEffortScale:
		if len(in.Levels) == 0 {
			errs = append(errs, fmt.Errorf(
				"%s: %q needs a non-empty levels list; folding clamps into it "+
					"and must never emit a level the model does not declare",
				where, ReasoningEffortScale))
		}
		if in.DefaultLevel != "" && !slices.Contains(in.Levels, in.DefaultLevel) {
			errs = append(errs, fmt.Errorf(
				"%s: default_level %q is not in levels %v", where, in.DefaultLevel, in.Levels))
		}
	default:
		if len(in.Levels) > 0 {
			errs = append(errs, fmt.Errorf(
				"%s: levels are only meaningful for %q, not %q",
				where, ReasoningEffortScale, capability))
		}
		if in.DefaultLevel != "" {
			errs = append(errs, fmt.Errorf(
				"%s: default_level is only meaningful for %q, not %q",
				where, ReasoningEffortScale, capability))
		}
	}

	if in.MinBudgetTokens < 0 || in.MaxBudgetTokens < 0 {
		errs = append(errs, fmt.Errorf("%s: reasoning budget bounds are negative", where))
	}
	if in.MaxBudgetTokens > 0 && in.MinBudgetTokens > in.MaxBudgetTokens {
		errs = append(errs, fmt.Errorf("%s: min_budget_tokens exceeds max_budget_tokens", where))
	}

	if len(errs) > 0 {
		return Reasoning{Capability: ReasoningUnknown}, errors.Join(errs...)
	}
	return Reasoning{
		Capability:      capability,
		Levels:          slices.Clone(in.Levels),
		DefaultLevel:    in.DefaultLevel,
		MinBudgetTokens: in.MinBudgetTokens,
		MaxBudgetTokens: in.MaxBudgetTokens,
		Verified:        in.Verified,
		Note:            in.Note,
	}, nil
}

func resolvePricing(where string, in *pricingDoc) (Pricing, error) {
	if in == nil {
		return Pricing{}, nil
	}
	var errs []error
	for name, d := range map[string]Decimal{
		"input_per_mtok":        in.InputPerMTok,
		"output_per_mtok":       in.OutputPerMTok,
		"cached_input_per_mtok": in.CachedInputPerMTok,
	} {
		if !d.valid() {
			errs = append(errs, fmt.Errorf("%s: pricing.%s: %q is not a decimal", where, name, d))
		}
	}
	if err := checkDate(in.Verified); err != nil {
		errs = append(errs, fmt.Errorf("%s: pricing.verified: %w", where, err))
	}
	if len(errs) > 0 {
		return Pricing{}, errors.Join(errs...)
	}
	return Pricing{
		Currency:           in.Currency,
		InputPerMTok:       in.InputPerMTok,
		OutputPerMTok:      in.OutputPerMTok,
		CachedInputPerMTok: in.CachedInputPerMTok,
		Verified:           in.Verified,
		Note:               in.Note,
	}, nil
}

func checkDate(s string) error {
	if s == "" {
		return nil
	}
	if _, err := time.Parse(time.DateOnly, s); err != nil {
		return fmt.Errorf("%q is not a YYYY-MM-DD date", s)
	}
	return nil
}
