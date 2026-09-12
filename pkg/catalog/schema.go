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

// field is one optional scalar of a document, carrying whether it was stated
// and which layer stated it.
//
// The pointer-to-scalar this replaces could distinguish "absent" from "set to
// the zero value", which is what makes field-level overlay merging possible,
// but it could not say *who* set it. Provenance has to be per field, because
// merging is per field: an entry's context window and its max output routinely
// come from different files.
type field[T any] struct {
	set    bool
	val    T
	origin fieldSource
}

// UnmarshalYAML records the value and marks the field stated. An explicit YAML
// null is treated as absent, the same as omitting the key: removing an
// inherited value is `merge: replace`, and having two spellings for it would
// make the file harder to read, not easier.
func (f *field[T]) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag == "!!null" {
		return nil
	}
	var v T
	if err := n.Decode(&v); err != nil {
		return err
	}
	f.set, f.val = true, v
	return nil
}

// get returns the value, or T's zero value when the field was never stated.
func (f field[T]) get() T { return f.val }

// or returns the value, or def when the field was never stated.
func (f field[T]) or(def T) T {
	if f.set {
		return f.val
	}
	return def
}

// ptr returns a copy of the value, or nil when the field was never stated. It
// is how a boolean survives into the lookup layers: a model must be able to
// switch a kind default off, not only on.
func (f field[T]) ptr() *T {
	if !f.set {
		return nil
	}
	v := f.val
	return &v
}

// mergeField overlays one field, recording from as the winning layer. An
// unstated source field leaves the destination — and its provenance — alone.
func mergeField[T any](dst *field[T], src field[T], from fieldSource) {
	if !src.set {
		return
	}
	dst.set = true
	dst.val = src.val
	dst.origin = from
}

// mergeMode controls how one document's entry combines with what earlier
// documents contributed for the same entry.
type mergeMode string

const (
	// mergeInherit is "not stated": take the document's default.
	mergeInherit mergeMode = ""

	// mergeMerge overlays field by field. It is the default, everywhere, and
	// it is why an overlay can correct one key without restating an entry.
	mergeMerge mergeMode = "merge"

	// mergeReplace discards every inherited field first, so the entry is
	// exactly what this document states. It is the only way to REMOVE an
	// inherited value rather than change it — a wrong context window on an
	// embedded entry cannot be merged away, because "absent" and "unchanged"
	// are the same input under merge.
	mergeReplace mergeMode = "replace"
)

func parseMerge(p *string) (mergeMode, bool) {
	if p == nil {
		return mergeInherit, true
	}
	switch m := mergeMode(*p); m {
	case mergeMerge, mergeReplace:
		return m, true
	}
	return mergeInherit, false
}

// fileDoc is the on-disk shape of both embedded files and of any operator
// overlay. One schema for all of them: an operator overlay can carry kinds,
// prefix rules and models in a single file, which is what lets a deployment
// keep one file per provider.
type fileDoc struct {
	Version *int `yaml:"version"`

	// Merge sets the default merge mode for every entry in this document.
	// A file that fully describes its providers says `merge: replace` once at
	// the top instead of on every entry.
	Merge *string `yaml:"merge"`

	KindAliases map[string]string   `yaml:"kind_aliases"`
	Kinds       map[string]*kindDoc `yaml:"kinds"`
	PrefixRules []*prefixDoc        `yaml:"prefix_rules"`
	Models      []*modelDoc         `yaml:"models"`
}

type kindDoc struct {
	Merge             *string       `yaml:"merge"`
	API               field[string] `yaml:"api"`
	BaseURL           field[string] `yaml:"base_url"`
	Cache             field[string] `yaml:"cache"`
	Reasoning         field[string] `yaml:"reasoning"`
	Category          field[string] `yaml:"category"`
	ContextWindow     field[int]    `yaml:"context_window"`
	MaxOutputTokens   field[int]    `yaml:"max_output_tokens"`
	SupportsTools     field[bool]   `yaml:"supports_tools"`
	SupportsStreaming field[bool]   `yaml:"supports_streaming"`
	ResponsesOnly     field[bool]   `yaml:"responses_only"`
	Metrics           field[string] `yaml:"metrics"`
	Priority          field[string] `yaml:"priority"`
	Verified          field[string] `yaml:"verified"`
	Probe             *probeDoc     `yaml:"probe"`
	Note              field[string] `yaml:"note"`

	// Provenance for the block that merges whole rather than field-wise.
	probeFrom fieldSource
}

// prefixDoc has no reasoning field, and that is deliberate. A prefix rule
// generalizes across every present and future member of a name family; letting
// it carry a reasoning capability would reintroduce REVIEW C5 by construction.
type prefixDoc struct {
	Kind              string        `yaml:"kind"`
	Prefix            string        `yaml:"prefix"`
	Merge             *string       `yaml:"merge"`
	Category          field[string] `yaml:"category"`
	ContextWindow     field[int]    `yaml:"context_window"`
	MaxOutputTokens   field[int]    `yaml:"max_output_tokens"`
	SupportsTools     field[bool]   `yaml:"supports_tools"`
	SupportsStreaming field[bool]   `yaml:"supports_streaming"`
	Note              field[string] `yaml:"note"`
}

type modelDoc struct {
	Kind              string        `yaml:"kind"`
	Model             string        `yaml:"model"`
	Merge             *string       `yaml:"merge"`
	Category          field[string] `yaml:"category"`
	ContextWindow     field[int]    `yaml:"context_window"`
	MaxOutputTokens   field[int]    `yaml:"max_output_tokens"`
	SupportsTools     field[bool]   `yaml:"supports_tools"`
	SupportsStreaming field[bool]   `yaml:"supports_streaming"`
	Reasoning         *reasoningDoc `yaml:"reasoning"`
	Pricing           *pricingDoc   `yaml:"pricing"`
	Verified          field[string] `yaml:"verified"`
	Probe             *probeDoc     `yaml:"probe"`
	Note              field[string] `yaml:"note"`

	// Provenance for the blocks that merge whole rather than field-wise.
	// Unexported, so they are neither read from nor accepted in YAML.
	reasoningFrom fieldSource
	pricingFrom   fieldSource
	probeFrom     fieldSource
}

// probeDoc is the on-disk shape of a recorded live answer that did not
// establish the model. One shape at both levels, so there is one vocabulary,
// one validator and one rendering; which results are legal is decided by the
// level, not by the shape.
type probeDoc struct {
	Result string `yaml:"result"`
	Date   string `yaml:"date"`
	Served string `yaml:"served"`
	Note   string `yaml:"note"`
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

// source pairs raw bytes with the name and origin used in error messages and
// in provenance.
type source struct {
	name   string
	origin OriginKind
	data   []byte
}

func (s source) from() fieldSource {
	return fieldSource{kind: s.origin, source: s.name}
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
	aliases      map[string]string
	aliasOrigins map[string]fieldSource
	kinds        map[string]*kindDoc
	kindOrder    []string
	rules        map[ruleKey]*prefixDoc
	ruleOrder    []ruleKey
	models       map[ModelRef]*modelDoc
	modelRefs    []ModelRef
	errs         []error
}

type ruleKey struct {
	kind   string
	prefix string
}

func newBuilder() *builder {
	return &builder{
		aliases:      map[string]string{},
		aliasOrigins: map[string]fieldSource{},
		kinds:        map[string]*kindDoc{},
		rules:        map[ruleKey]*prefixDoc{},
		models:       map[ModelRef]*modelDoc{},
	}
}

func (b *builder) errf(format string, args ...any) {
	b.errs = append(b.errs, fmt.Errorf(format, args...))
}

// add folds one document into the builder. Later documents win field by field
// under the default merge mode, so an overlay may change a single key without
// restating an entry; `merge: replace` drops the inherited fields first.
//
// Every entry kind here is additive: a document may introduce a provider kind,
// an alias, a prefix rule or a model that no earlier document mentioned. The
// embedded data is a starting point, not a closed world.
func (b *builder) add(src source, doc *fileDoc) {
	name := src.name
	from := src.from()

	if doc.Version != nil && *doc.Version != 1 {
		b.errf("%s: unsupported version %d, want 1", name, *doc.Version)
	}

	docMode, ok := parseMerge(doc.Merge)
	if !ok {
		b.errf("%s: merge must be %q or %q, got %q", name, mergeMerge, mergeReplace, *doc.Merge)
	}
	if docMode == mergeInherit {
		docMode = mergeMerge
	}

	// entryMode resolves an entry's own merge key against the document default.
	entryMode := func(p *string, where string) mergeMode {
		m, ok := parseMerge(p)
		if !ok {
			b.errf("%s: %s: merge must be %q or %q, got %q", name, where, mergeMerge, mergeReplace, *p)
			return docMode
		}
		if m == mergeInherit {
			return docMode
		}
		return m
	}

	for alias, target := range doc.KindAliases {
		if alias == "" || target == "" {
			b.errf("%s: kind_aliases has an empty alias or target", name)
			continue
		}
		b.aliases[alias] = target
		b.aliasOrigins[alias] = from
	}

	for kindName, in := range doc.Kinds {
		if kindName == "" {
			b.errf("%s: kinds has an empty name", name)
			continue
		}
		if in == nil {
			in = &kindDoc{}
		}
		mode := entryMode(in.Merge, fmt.Sprintf("kinds[%q]", kindName))
		cur, ok := b.kinds[kindName]
		if !ok {
			cur = &kindDoc{}
			b.kinds[kindName] = cur
			b.kindOrder = append(b.kindOrder, kindName)
		} else if mode == mergeReplace {
			cur = &kindDoc{}
			b.kinds[kindName] = cur
		}
		mergeKind(cur, in, from)
	}

	for _, in := range doc.PrefixRules {
		if in == nil || in.Prefix == "" {
			b.errf("%s: prefix_rules entry has an empty prefix", name)
			continue
		}
		mode := entryMode(in.Merge, fmt.Sprintf("prefix_rules[%q]", in.Prefix))
		key := ruleKey{kind: in.Kind, prefix: in.Prefix}
		cur, ok := b.rules[key]
		if !ok {
			cur = &prefixDoc{Kind: in.Kind, Prefix: in.Prefix}
			b.rules[key] = cur
			b.ruleOrder = append(b.ruleOrder, key)
		} else if mode == mergeReplace {
			cur = &prefixDoc{Kind: in.Kind, Prefix: in.Prefix}
			b.rules[key] = cur
		}
		mergePrefix(cur, in, from)
	}

	for _, in := range doc.Models {
		if in == nil || in.Kind == "" || in.Model == "" {
			b.errf("%s: models entry needs both kind and model", name)
			continue
		}
		mode := entryMode(in.Merge, fmt.Sprintf("models[kind=%s model=%s]", in.Kind, in.Model))
		ref := ModelRef{Kind: in.Kind, Model: in.Model}
		cur, ok := b.models[ref]
		if !ok {
			cur = &modelDoc{Kind: in.Kind, Model: in.Model}
			b.models[ref] = cur
			b.modelRefs = append(b.modelRefs, ref)
		} else if mode == mergeReplace {
			cur = &modelDoc{Kind: in.Kind, Model: in.Model}
			b.models[ref] = cur
		}
		mergeModel(cur, in, from)
	}
}

func mergeKind(dst, src *kindDoc, from fieldSource) {
	mergeField(&dst.API, src.API, from)
	mergeField(&dst.BaseURL, src.BaseURL, from)
	mergeField(&dst.Cache, src.Cache, from)
	mergeField(&dst.Reasoning, src.Reasoning, from)
	mergeField(&dst.Category, src.Category, from)
	mergeField(&dst.ContextWindow, src.ContextWindow, from)
	mergeField(&dst.MaxOutputTokens, src.MaxOutputTokens, from)
	mergeField(&dst.SupportsTools, src.SupportsTools, from)
	mergeField(&dst.SupportsStreaming, src.SupportsStreaming, from)
	mergeField(&dst.ResponsesOnly, src.ResponsesOnly, from)
	mergeField(&dst.Metrics, src.Metrics, from)
	mergeField(&dst.Priority, src.Priority, from)
	mergeField(&dst.Verified, src.Verified, from)
	mergeField(&dst.Note, src.Note, from)
	supersede(&dst.Verified, &dst.Probe, &dst.probeFrom, src.Verified, src.Probe, from)
}

// supersede keeps `verified:` and `probe:` as ONE slot across layers.
//
// They are two answers to one question — what happened the last time an
// endpoint was asked — and within a document stating both is a load error. But
// an overlay is how a probe result reaches the catalog, and under field-wise
// merge an overlay that dates an entry the embedded data marked `denied` would
// inherit the denial and collide with itself. Making the later layer replace
// the earlier answer is the same rule the single-document check enforces,
// applied across layers: the newest answer is the answer.
//
// `merge: replace` is the alternative and it is the wrong tool here — it would
// also discard the context window and the citation that came with the entry,
// which the probe said nothing about.
func supersede(dstVerified *field[string], dstProbe **probeDoc, dstProbeFrom *fieldSource,
	srcVerified field[string], srcProbe *probeDoc, from fieldSource) {
	switch {
	case srcProbe != nil:
		*dstProbe = srcProbe
		*dstProbeFrom = from
		if srcVerified.set {
			// Both in one document: leave them both standing so the resolver
			// reports the contradiction rather than silently picking one.
			return
		}
		*dstVerified = field[string]{}
	case srcVerified.set:
		*dstProbe = nil
		*dstProbeFrom = fieldSource{}
	}
}

func mergePrefix(dst, src *prefixDoc, from fieldSource) {
	mergeField(&dst.Category, src.Category, from)
	mergeField(&dst.ContextWindow, src.ContextWindow, from)
	mergeField(&dst.MaxOutputTokens, src.MaxOutputTokens, from)
	mergeField(&dst.SupportsTools, src.SupportsTools, from)
	mergeField(&dst.SupportsStreaming, src.SupportsStreaming, from)
	mergeField(&dst.Note, src.Note, from)
}

func mergeModel(dst, src *modelDoc, from fieldSource) {
	mergeField(&dst.Category, src.Category, from)
	mergeField(&dst.ContextWindow, src.ContextWindow, from)
	mergeField(&dst.MaxOutputTokens, src.MaxOutputTokens, from)
	mergeField(&dst.SupportsTools, src.SupportsTools, from)
	mergeField(&dst.SupportsStreaming, src.SupportsStreaming, from)
	mergeField(&dst.Verified, src.Verified, from)
	mergeField(&dst.Note, src.Note, from)
	// Reasoning and pricing are replaced whole. Capability, levels and the
	// verification date are one claim; merging them field by field could
	// leave a capability standing next to a date that never covered it.
	if src.Reasoning != nil {
		dst.Reasoning = src.Reasoning
		dst.reasoningFrom = from
	}
	if src.Pricing != nil {
		dst.Pricing = src.Pricing
		dst.pricingFrom = from
	}
	// A probe is one answer from one moment: result, date and evidence are a
	// single claim, and merging them field-wise could leave a fresh date on a
	// stale result. It shares its slot with `verified:`.
	supersede(&dst.Verified, &dst.Probe, &dst.probeFrom, src.Verified, src.Probe, from)
}

// build validates the accumulated documents and freezes them into a Catalog.
// Every problem is collected, so one load reports every defect in an operator
// file rather than one per attempt.
func (b *builder) build() (*Catalog, error) {
	c := &Catalog{
		kinds:        make(map[string]KindDefaults, len(b.kinds)),
		kindOrigins:  make(map[string]map[string]fieldSource, len(b.kinds)),
		aliases:      make(map[string]string, len(b.aliases)),
		aliasOrigins: make(map[string]fieldSource, len(b.aliases)),
		models:       make(map[ModelRef]modelEntry, len(b.models)),
	}

	slices.Sort(b.kindOrder)
	for _, name := range b.kindOrder {
		kd, origins, err := resolveKind(name, b.kinds[name])
		if err != nil {
			b.errs = append(b.errs, err)
			continue
		}
		c.kinds[name] = kd
		c.kindOrigins[name] = origins
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
		c.aliasOrigins[alias] = b.aliasOrigins[alias]
	}

	for _, key := range b.ruleOrder {
		in := b.rules[key]
		if in.Kind != "" {
			if _, ok := c.kinds[c.canonical(in.Kind)]; !ok {
				b.errf("prefix_rules[%q]: unknown kind %q", in.Prefix, in.Kind)
				continue
			}
		}
		cat := Category(in.Category.get())
		if cat != "" && !validCategories[cat] {
			b.errf("prefix_rules[%q]: unknown category %q", in.Prefix, cat)
			continue
		}
		c.rules = append(c.rules, prefixRule{
			kind:              c.canonical(in.Kind),
			prefix:            in.Prefix,
			category:          cat,
			contextWindow:     in.ContextWindow.get(),
			maxOutputTokens:   in.MaxOutputTokens.get(),
			supportsTools:     in.SupportsTools.ptr(),
			supportsStreaming: in.SupportsStreaming.ptr(),
			note:              in.Note.get(),
			origins: map[string]fieldSource{
				FieldCategory:          in.Category.origin,
				FieldContextWindow:     in.ContextWindow.origin,
				FieldMaxOutputTokens:   in.MaxOutputTokens.origin,
				FieldSupportsTools:     in.SupportsTools.origin,
				FieldSupportsStreaming: in.SupportsStreaming.origin,
				FieldNote:              in.Note.origin,
			},
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

// packageDefault marks a value this package supplied because no file did. It
// is distinguishable from a file that states the same value on purpose: an
// operator reading Explain must be able to tell "dorang assumed chat" from
// "my file says chat".
var packageDefault = fieldSource{kind: OriginDefault}

func resolveKind(name string, in *kindDoc) (KindDefaults, map[string]fieldSource, error) {
	var errs []error

	api := API(in.API.get())
	if api == "" {
		errs = append(errs, fmt.Errorf("kinds[%q]: api is required", name))
	} else if !validAPIs[api] {
		errs = append(errs, fmt.Errorf("kinds[%q]: unknown api %q", name, api))
	}

	cacheOrigin := in.Cache.origin
	cache := CacheScheme(in.Cache.get())
	if cache == "" {
		cache, cacheOrigin = CacheNone, packageDefault
	}
	if !validCaches[cache] {
		errs = append(errs, fmt.Errorf("kinds[%q]: unknown cache scheme %q", name, cache))
	}

	hintOrigin := in.Reasoning.origin
	hint := ReasoningCapability(in.Reasoning.get())
	if hint == "" {
		hint, hintOrigin = ReasoningUnknown, packageDefault
	}
	if !validReasoning[hint] {
		errs = append(errs, fmt.Errorf("kinds[%q]: unknown reasoning shape %q", name, hint))
	}

	catOrigin := in.Category.origin
	cat := Category(in.Category.get())
	if cat == "" {
		cat, catOrigin = CategoryChat, packageDefault
	}
	if !validCategories[cat] {
		errs = append(errs, fmt.Errorf("kinds[%q]: unknown category %q", name, cat))
	}

	if err := checkDate(in.Verified.get()); err != nil {
		errs = append(errs, fmt.Errorf("kinds[%q]: verified: %w", name, err))
	}
	probe, err := resolveProbe(fmt.Sprintf("kinds[%q]", name), LayerKind, in.Verified.get(), in.Probe)
	if err != nil {
		errs = append(errs, err)
	}
	if v := in.ContextWindow.get(); v < 0 {
		errs = append(errs, fmt.Errorf("kinds[%q]: context_window is negative", name))
	}
	if v := in.MaxOutputTokens.get(); v < 0 {
		errs = append(errs, fmt.Errorf("kinds[%q]: max_output_tokens is negative", name))
	}

	if len(errs) > 0 {
		return KindDefaults{}, nil, errors.Join(errs...)
	}
	origins := map[string]fieldSource{
		FieldAPI:               in.API.origin,
		FieldBaseURL:           in.BaseURL.origin,
		FieldCache:             cacheOrigin,
		FieldReasoningHint:     hintOrigin,
		FieldCategory:          catOrigin,
		FieldContextWindow:     in.ContextWindow.origin,
		FieldMaxOutputTokens:   in.MaxOutputTokens.origin,
		FieldSupportsTools:     in.SupportsTools.origin,
		FieldSupportsStreaming: in.SupportsStreaming.origin,
		FieldResponsesOnly:     in.ResponsesOnly.origin,
		FieldMetrics:           in.Metrics.origin,
		FieldPriority:          in.Priority.origin,
		FieldVerified:          in.Verified.origin,
		FieldProbe:             in.probeFrom,
		FieldNote:              in.Note.origin,
	}
	return KindDefaults{
		Name:              name,
		API:               api,
		BaseURL:           in.BaseURL.get(),
		Cache:             cache,
		ReasoningHint:     hint,
		Category:          cat,
		ContextWindow:     in.ContextWindow.get(),
		MaxOutputTokens:   in.MaxOutputTokens.get(),
		SupportsTools:     in.SupportsTools.get(),
		SupportsStreaming: in.SupportsStreaming.get(),
		ResponsesOnly:     in.ResponsesOnly.get(),
		Metrics:           in.Metrics.get(),
		Priority:          in.Priority.get(),
		Verified:          in.Verified.get(),
		Probe:             probe,
		Note:              in.Note.get(),
	}, origins, nil
}

// resolveProbe enforces the rules that give a recorded probe its meaning.
//
// Every one of them exists so that the ABSENCE of a date says something
// definite. A probe without a date is a claim about no moment; a result at the
// wrong level is a claim about the wrong subject; a denial without its refusal
// text cannot be told from a model that simply is not there, which is the
// distinction the whole state exists to draw. And a probe beside `verified:` is
// two answers to one question, so the loader refuses it rather than picking.
func resolveProbe(where, level, verified string, in *probeDoc) (Probe, error) {
	if in == nil {
		return Probe{}, nil
	}
	var errs []error

	result := ProbeResult(in.Result)
	wantLevel, known := probeResultLevel[result]
	switch {
	case result == ProbeNone:
		errs = append(errs, fmt.Errorf("%s: probe.result is required; an empty "+
			"probe records nothing and is not the same as no probe", where))
	case !known:
		errs = append(errs, fmt.Errorf("%s: unknown probe result %q", where, in.Result))
	case wantLevel != level:
		errs = append(errs, fmt.Errorf(
			"%s: probe result %q belongs on the %s, not the %s: %s",
			where, result, wantLevel, level, probeLevelReason(result)))
	}

	if err := checkDate(in.Date); err != nil {
		errs = append(errs, fmt.Errorf("%s: probe.date: %w", where, err))
	} else if in.Date == "" {
		errs = append(errs, fmt.Errorf(
			"%s: probe needs a date; it records what an endpoint said at a "+
				"moment, and an entitlement can be bought the next day", where))
	}
	if verified != "" {
		errs = append(errs, fmt.Errorf(
			"%s: carries both verified: and probe:; they answer the same "+
				"question and only one can be the last answer. A probe that "+
				"supersedes a date replaces it", where))
	}

	if result == ProbeSubstituted && in.Served == "" {
		errs = append(errs, fmt.Errorf(
			"%s: probe result %q needs probe.served, the model id that "+
				"answered; without it the finding has no content",
			where, ProbeSubstituted))
	}
	if result != ProbeSubstituted && in.Served != "" {
		errs = append(errs, fmt.Errorf(
			"%s: probe.served is only meaningful for %q, not %q",
			where, ProbeSubstituted, result))
	}
	if result == ProbeDenied && strings.TrimSpace(in.Note) == "" {
		errs = append(errs, fmt.Errorf(
			"%s: probe result %q needs a note carrying the refusal verbatim; "+
				"that text is the only evidence separating an entitlement "+
				"refusal from a model that does not exist", where, ProbeDenied))
	}

	if len(errs) > 0 {
		return Probe{}, errors.Join(errs...)
	}
	return Probe{Result: result, Date: in.Date, Served: in.Served, Note: in.Note}, nil
}

// probeLevelReason explains a level mismatch in terms of what the fact is about,
// so the message teaches the rule instead of restating it.
func probeLevelReason(r ProbeResult) string {
	switch r {
	case ProbeDenied:
		return "entitlement is a property of (account, model), and one plan reaches some models on a route and not others"
	case ProbeSubstituted:
		return "substitution is a property of one model name on one route"
	case ProbeCitationOnly:
		return "a missing credential is a property of the route, and stating it per entry would be one identical copy per model"
	}
	return ""
}

// resolveModel turns one model document into the top lookup layer. Booleans
// stay pointers so an entry can switch a kind default off, not only on; ints
// stay zero-means-undeclared.
func resolveModel(ref ModelRef, in *modelDoc) (modelEntry, error) {
	var errs []error
	where := fmt.Sprintf("models[kind=%s model=%s]", ref.Kind, ref.Model)

	cat := Category(in.Category.get())
	if cat != "" && !validCategories[cat] {
		errs = append(errs, fmt.Errorf("%s: unknown category %q", where, cat))
	}
	if err := checkDate(in.Verified.get()); err != nil {
		errs = append(errs, fmt.Errorf("%s: verified: %w", where, err))
	}
	if v := in.ContextWindow.get(); v < 0 {
		errs = append(errs, fmt.Errorf("%s: context_window is negative", where))
	}
	if v := in.MaxOutputTokens.get(); v < 0 {
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
	probe, err := resolveProbe(where, LayerModel, in.Verified.get(), in.Probe)
	if err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return modelEntry{}, errors.Join(errs...)
	}
	return modelEntry{
		ref:               ref,
		category:          cat,
		contextWindow:     in.ContextWindow.get(),
		maxOutputTokens:   in.MaxOutputTokens.get(),
		supportsTools:     in.SupportsTools.ptr(),
		supportsStreaming: in.SupportsStreaming.ptr(),
		reasoning:         reasoning,
		pricing:           pricing,
		verified:          in.Verified.get(),
		probe:             probe,
		note:              in.Note.get(),
		origins: map[string]fieldSource{
			FieldCategory:          in.Category.origin,
			FieldContextWindow:     in.ContextWindow.origin,
			FieldMaxOutputTokens:   in.MaxOutputTokens.origin,
			FieldSupportsTools:     in.SupportsTools.origin,
			FieldSupportsStreaming: in.SupportsStreaming.origin,
			FieldReasoning:         in.reasoningFrom,
			FieldPricing:           in.pricingFrom,
			FieldVerified:          in.Verified.origin,
			FieldProbe:             in.probeFrom,
			FieldNote:              in.Note.origin,
		},
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
