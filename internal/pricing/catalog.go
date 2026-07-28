package pricing

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// Component indices. The order is fixed; componentInfo below is indexed by it.
const (
	cInput = iota
	cOutput
	cCacheRead
	cCacheWrite
	cReasoning
	cRequest
	cCharacters
	cSeconds
	numComponents
)

var componentInfo = [numComponents]struct {
	name string
	yaml string
	unit Unit
	// divisor is how many quanta of the measured quantity one rate covers.
	divisor uint64
	// scale is the negative power of ten the quantity is counted in.
	scale int32
}{
	cInput:      {"input", "input", UnitPerMillionTokens, 1_000_000, 0},
	cOutput:     {"output", "output", UnitPerMillionTokens, 1_000_000, 0},
	cCacheRead:  {"cache_read", "cache_read", UnitPerMillionTokens, 1_000_000, 0},
	cCacheWrite: {"cache_write", "cache_write", UnitPerMillionTokens, 1_000_000, 0},
	cReasoning:  {"reasoning", "reasoning", UnitPerMillionTokens, 1_000_000, 0},
	cRequest:    {"request", "request", UnitPerRequest, 1, 0},
	cCharacters: {"characters", "characters", UnitPerThousandCharacters, 1_000, 0},
	cSeconds:    {"seconds", "seconds", UnitPerSecond, microsPerSecond, 6},
}

// rateSet holds the rates a rule (or one of its tiers) declares.
type rateSet struct {
	set  [numComponents]bool
	atto [numComponents]u128
	text [numComponents]string
}

// inherit fills unset entries from base, so a tier need only restate what it changes.
func (r rateSet) inherit(base rateSet) rateSet {
	for i := 0; i < numComponents; i++ {
		if !r.set[i] && base.set[i] {
			r.set[i] = true
			r.atto[i] = base.atto[i]
			r.text[i] = base.text[i]
		}
	}
	return r
}

func (r rateSet) count() int {
	n := 0
	for i := 0; i < numComponents; i++ {
		if r.set[i] {
			n++
		}
	}
	return n
}

func (r rateSet) any() bool {
	for i := 0; i < numComponents; i++ {
		if r.set[i] {
			return true
		}
	}
	return false
}

type tier struct {
	bounded bool
	upTo    int64
	rates   rateSet
}

// rule is a compiled pricing rule. Everything that can be resolved at configuration load
// is resolved here: decimals are parsed, time windows are compiled, the specificity level
// is computed and the timezone is loaded.
type rule struct {
	id       string
	class    Class
	level    Level
	priority int
	order    int

	credential  string
	deployment  string
	provider    string
	model       string
	modelPrefix string
	prefixLen   int

	when *whenPred

	unit     Unit
	rates    rateSet
	tiers    []tier
	tierMode TierMode
	// maxComponents is the largest number of component lines this rule can produce.
	// It sizes the Cost.Components slice in one allocation.
	maxComponents int

	amountAtto u128
	amountText string
	period     Period
	loc        *time.Location

	op        AdjOp
	adjAmount decimal
	adjAtto   u128
	appliesTo AdjBase
}

// staticMatches tests the dimensions that are known at configuration time. The index
// already guarantees the most specific dimension matches; this confirms the rest.
func (r *rule) staticMatches(req *Request) bool {
	if r.credential != "" && r.credential != req.Credential {
		return false
	}
	if r.deployment != "" && r.deployment != req.Deployment {
		return false
	}
	if r.provider != "" && r.provider != req.Provider {
		return false
	}
	if r.model != "" && r.model != req.Model {
		return false
	}
	// Model names are opaque (§2.1): a literal prefix test on the whole name, never a
	// split on ':' or '/'.
	if r.modelPrefix != "" && !strings.HasPrefix(req.Model, r.modelPrefix) {
		return false
	}
	return true
}

// betterPrefix orders two rules inside the model-prefix level. A longer prefix is more
// specific; §8.2's tie-break (priority, then id) settles the rest, deterministically.
func betterPrefix(a, b *rule) bool {
	if a.prefixLen != b.prefixLen {
		return a.prefixLen > b.prefixLen
	}
	if a.priority != b.priority {
		return a.priority > b.priority
	}
	return a.id < b.id
}

// classIndex is the per-class rule index built at configuration load (§8.2). Each rule
// lives in exactly one bucket, chosen by its most specific match dimension, so a request
// only ever examines rules that can apply to it.
type classIndex struct {
	byCredential map[string][]*rule
	byDeployment map[string][]*rule
	byProvModel  map[provModelKey][]*rule
	byModel      map[string][]*rule
	byPrefix     map[string][]*rule // keyed by the first <=2 bytes of the prefix
	byProvider   map[string][]*rule
	defaults     []*rule
	count        int
}

type provModelKey struct{ provider, model string }

// prefixKey is the index key for a prefix or a model name: the first two bytes, or the
// whole string when it is shorter. A lookup checks this key and the one-byte key, which
// is the complete set of buckets that can hold a matching prefix rule.
func prefixKey(s string) string {
	if len(s) > 2 {
		return s[:2]
	}
	return s
}

// Catalog is a loaded, indexed price list. It is safe for concurrent use.
type Catalog struct {
	// Currency is the ISO code every amount in this catalog is denominated in.
	Currency string

	loc   *time.Location
	rules []*rule
	idx   [numClasses]classIndex

	// subs is fixed at load: one entry per fixed_subscription rule, so the map itself is
	// never written after construction and Price only takes that rule's read lock.
	subs map[string]*subState

	// carries is written only by Settle.
	carryMu sync.Mutex
	carries map[string]*carrySet
}

// carrySet holds the sub-nano remainder of each rounded field for one settlement bucket.
type carrySet struct {
	marginal     int64
	subscription int64
	adjustment   int64
}

// subState is a subscription rule's accumulator for the period in progress.
//
// Readers take no lock at all: Price loads an immutable snapshot pointer, so pricing a
// candidate that happens to sit on a subscription plan costs one atomic load rather than
// lock traffic on the routing path (§15.2). Only Settle writes, and it serializes writers
// with mu before publishing a fresh snapshot.
type subState struct {
	mu   sync.Mutex
	snap atomic.Pointer[subSnapshot]
}

type subSnapshot struct {
	periodStart time.Time
	marginal    u128      // marginal atto-units accrued in the period
	last        time.Time // instant of the most recent settlement in the period
}

// LoadCatalog reads and compiles a price catalog from disk.
func LoadCatalog(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pricing: load catalog: %w", err)
	}
	c, err := ParseCatalog(data)
	if err != nil {
		return nil, fmt.Errorf("pricing: %s: %w", path, err)
	}
	return c, nil
}

type rawCatalog struct {
	Currency string    `yaml:"currency"`
	TZ       string    `yaml:"tz"`
	Rules    []rawRule `yaml:"rules"`
}

type rawMatch struct {
	Credential  string `yaml:"credential"`
	Deployment  string `yaml:"deployment"`
	Provider    string `yaml:"provider"`
	Model       string `yaml:"model"`
	ModelPrefix string `yaml:"model_prefix"`
}

type rawWhen struct {
	TimeOfDay string   `yaml:"time_of_day"`
	TZ        string   `yaml:"tz"`
	Weekday   []string `yaml:"weekday"`
	DateRange string   `yaml:"date_range"`
}

type rawTier struct {
	UpToInputTokens *int64 `yaml:"up_to_input_tokens"`
	Input           string `yaml:"input"`
	Output          string `yaml:"output"`
	CacheRead       string `yaml:"cache_read"`
	CacheWrite      string `yaml:"cache_write"`
	Reasoning       string `yaml:"reasoning"`
	Request         string `yaml:"request"`
	Characters      string `yaml:"characters"`
	Seconds         string `yaml:"seconds"`
}

type rawRule struct {
	ID       string   `yaml:"id"`
	Class    string   `yaml:"class"`
	Match    rawMatch `yaml:"match"`
	When     *rawWhen `yaml:"when"`
	Unit     string   `yaml:"unit"`
	Priority int      `yaml:"priority"`

	Input      string `yaml:"input"`
	Output     string `yaml:"output"`
	CacheRead  string `yaml:"cache_read"`
	CacheWrite string `yaml:"cache_write"`
	Reasoning  string `yaml:"reasoning"`
	Request    string `yaml:"request"`
	Characters string `yaml:"characters"`
	Seconds    string `yaml:"seconds"`

	Tiers    []rawTier `yaml:"tiers"`
	TierMode string    `yaml:"tier_mode"`

	AmountPerPeriod string `yaml:"amount_per_period"`
	Period          string `yaml:"period"`

	Op        string `yaml:"op"`
	Amount    string `yaml:"amount"`
	AppliesTo string `yaml:"applies_to"`
	Order     int    `yaml:"order"`
}

func (r rawRule) rateStrings() [numComponents]string {
	return [numComponents]string{
		cInput: r.Input, cOutput: r.Output, cCacheRead: r.CacheRead,
		cCacheWrite: r.CacheWrite, cReasoning: r.Reasoning,
		cRequest: r.Request, cCharacters: r.Characters, cSeconds: r.Seconds,
	}
}

func (t rawTier) rateStrings() [numComponents]string {
	return [numComponents]string{
		cInput: t.Input, cOutput: t.Output, cCacheRead: t.CacheRead,
		cCacheWrite: t.CacheWrite, cReasoning: t.Reasoning,
		cRequest: t.Request, cCharacters: t.Characters, cSeconds: t.Seconds,
	}
}

// ParseCatalog compiles a price catalog from YAML. Unknown fields are rejected: a typo in
// a price list must fail loudly at load, not silently price at zero.
func ParseCatalog(data []byte) (*Catalog, error) {
	var raw rawCatalog
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("pricing: parse catalog: %w", err)
	}

	c := &Catalog{Currency: raw.Currency, loc: time.UTC, subs: map[string]*subState{}}
	if c.Currency == "" {
		c.Currency = "USD"
	}
	if raw.TZ != "" {
		loc, err := time.LoadLocation(raw.TZ)
		if err != nil {
			return nil, fmt.Errorf("pricing: catalog tz: %w", err)
		}
		c.loc = loc
	}

	seen := make(map[string]bool, len(raw.Rules))
	for i := range raw.Rules {
		r, err := c.compileRule(&raw.Rules[i])
		if err != nil {
			id := raw.Rules[i].ID
			if id == "" {
				id = fmt.Sprintf("rules[%d]", i)
			}
			return nil, fmt.Errorf("pricing: rule %s: %w", id, err)
		}
		if seen[r.id] {
			return nil, fmt.Errorf("pricing: duplicate rule id %q", r.id)
		}
		seen[r.id] = true
		c.rules = append(c.rules, r)
	}
	c.buildIndex()
	return c, nil
}

func (c *Catalog) compileRule(raw *rawRule) (*rule, error) {
	if strings.TrimSpace(raw.ID) == "" {
		return nil, errors.New("id is required")
	}
	class, err := parseClass(raw.Class)
	if err != nil {
		return nil, err
	}
	r := &rule{
		id:          raw.ID,
		class:       class,
		priority:    raw.Priority,
		order:       raw.Order,
		credential:  raw.Match.Credential,
		deployment:  raw.Match.Deployment,
		provider:    raw.Match.Provider,
		model:       raw.Match.Model,
		modelPrefix: raw.Match.ModelPrefix,
		prefixLen:   len(raw.Match.ModelPrefix),
		loc:         c.loc,
	}
	r.level = specificity(r)

	if raw.When != nil {
		w, err := compileWhen(raw.When, c.loc)
		if err != nil {
			return nil, err
		}
		r.when = w
		if w.loc != nil {
			r.loc = w.loc
		}
	}

	switch class {
	case ClassMarginal:
		if err := compileMarginal(raw, r); err != nil {
			return nil, err
		}
	case ClassSubscription:
		if err := compileSubscription(raw, r); err != nil {
			return nil, err
		}
	case ClassAdjustment:
		if err := compileAdjustment(raw, r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func compileMarginal(raw *rawRule, r *rule) error {
	if raw.AmountPerPeriod != "" || raw.Period != "" {
		return errors.New("amount_per_period/period belong to a fixed_subscription rule")
	}
	if raw.Op != "" || raw.Amount != "" || raw.AppliesTo != "" {
		return errors.New("op/amount/applies_to belong to an adjustment rule")
	}
	unit := UnitPerMillionTokens
	if raw.Unit != "" {
		u, err := parseUnit(raw.Unit)
		if err != nil {
			return err
		}
		unit = u
	}
	if unit == UnitSubscription {
		return errors.New("unit subscription requires class fixed_subscription")
	}
	r.unit = unit

	rates, err := compileRates(raw.rateStrings(), unit)
	if err != nil {
		return err
	}
	r.rates = rates

	mode, err := parseTierMode(raw.TierMode)
	if err != nil {
		return err
	}
	r.tierMode = mode
	if len(raw.Tiers) == 0 {
		if raw.TierMode != "" {
			return errors.New("tier_mode set but no tiers declared")
		}
		if !rates.any() {
			return errors.New("no rates declared")
		}
		r.maxComponents = rates.count()
		return nil
	}

	var prev int64 = -1
	for i := range raw.Tiers {
		t := &raw.Tiers[i]
		tr, err := compileRates(t.rateStrings(), unit)
		if err != nil {
			return fmt.Errorf("tier %d: %w", i, err)
		}
		ct := tier{rates: tr.inherit(rates)}
		if t.UpToInputTokens != nil {
			if *t.UpToInputTokens <= prev {
				return fmt.Errorf("tier %d: up_to_input_tokens must increase", i)
			}
			if i == len(raw.Tiers)-1 {
				return fmt.Errorf("tier %d: the last tier must be unbounded (up_to_input_tokens: null)", i)
			}
			ct.bounded = true
			ct.upTo = *t.UpToInputTokens
			prev = *t.UpToInputTokens
		} else if i != len(raw.Tiers)-1 {
			return fmt.Errorf("tier %d: only the last tier may be unbounded", i)
		}
		if !ct.rates.any() {
			return fmt.Errorf("tier %d: no rates declared", i)
		}
		r.tiers = append(r.tiers, ct)
	}
	if r.tierMode == TierGraduated && !r.tiers[0].rates.set[cInput] {
		return errors.New("graduated tiering requires an input rate in every tier")
	}
	r.maxComponents = tierComponentBound(r)
	return nil
}

// tierComponentBound is the largest number of component lines a marginal rule can emit.
func tierComponentBound(r *rule) int {
	if len(r.tiers) == 0 {
		return r.rates.count()
	}
	best := 0
	for i := range r.tiers {
		n := r.tiers[i].rates.count()
		if r.tierMode == TierGraduated {
			// Every bracket up to this one can contribute its own input line.
			n += i
		}
		if n > best {
			best = n
		}
	}
	return best
}

func compileRates(in [numComponents]string, unit Unit) (rateSet, error) {
	var rs rateSet
	for i := 0; i < numComponents; i++ {
		if in[i] == "" {
			continue
		}
		info := componentInfo[i]
		if info.unit != unit {
			return rs, fmt.Errorf("rate %q does not belong to unit %s (it is a %s rate)",
				info.yaml, unit, info.unit)
		}
		d, err := parseDecimal(in[i])
		if err != nil {
			return rs, fmt.Errorf("rate %q: %w", info.yaml, err)
		}
		if d.neg {
			return rs, fmt.Errorf("rate %q: a marginal rate may not be negative", info.yaml)
		}
		a, ok := d.atto()
		if !ok {
			return rs, fmt.Errorf("rate %q: %w", info.yaml, ErrOverflow)
		}
		rs.set[i], rs.atto[i], rs.text[i] = true, a, d.text
	}
	return rs, nil
}

func compileSubscription(raw *rawRule, r *rule) error {
	if raw.Unit != "" {
		u, err := parseUnit(raw.Unit)
		if err != nil {
			return err
		}
		if u != UnitSubscription {
			return fmt.Errorf("a fixed_subscription rule must use unit subscription, not %s", u)
		}
	}
	if len(raw.Tiers) > 0 || raw.TierMode != "" {
		return errors.New("tiers do not apply to a fixed_subscription rule")
	}
	if raw.Op != "" || raw.Amount != "" || raw.AppliesTo != "" {
		return errors.New("op/amount/applies_to belong to an adjustment rule")
	}
	if rs := raw.rateStrings(); rs != [numComponents]string{} {
		return errors.New("a fixed_subscription rule prices no components; use amount_per_period")
	}
	r.unit = UnitSubscription
	if raw.AmountPerPeriod == "" {
		return errors.New("amount_per_period is required")
	}
	d, err := parseDecimal(raw.AmountPerPeriod)
	if err != nil {
		return fmt.Errorf("amount_per_period: %w", err)
	}
	if d.neg {
		return errors.New("amount_per_period may not be negative")
	}
	a, ok := d.atto()
	if !ok {
		return fmt.Errorf("amount_per_period: %w", ErrOverflow)
	}
	r.amountAtto, r.amountText = a, d.text
	p, err := parsePeriod(raw.Period)
	if err != nil {
		return err
	}
	r.period = p
	return nil
}

func compileAdjustment(raw *rawRule, r *rule) error {
	if raw.Unit != "" {
		return errors.New("unit does not apply to an adjustment rule")
	}
	if len(raw.Tiers) > 0 || raw.TierMode != "" {
		return errors.New("tiers do not apply to an adjustment rule")
	}
	if raw.AmountPerPeriod != "" || raw.Period != "" {
		return errors.New("amount_per_period/period belong to a fixed_subscription rule")
	}
	if rs := raw.rateStrings(); rs != [numComponents]string{} {
		return errors.New("an adjustment rule prices no components; use op and amount")
	}
	r.unit = UnitNone
	op, err := parseAdjOp(raw.Op)
	if err != nil {
		return err
	}
	r.op = op
	base, err := parseAdjBase(raw.AppliesTo)
	if err != nil {
		return err
	}
	r.appliesTo = base
	if raw.Amount == "" {
		return errors.New("amount is required")
	}
	d, err := parseDecimal(raw.Amount)
	if err != nil {
		return fmt.Errorf("amount: %w", err)
	}
	r.adjAmount = d
	if op == AdjAdd {
		a, ok := d.atto()
		if !ok {
			return fmt.Errorf("amount: %w", ErrOverflow)
		}
		r.adjAtto = a
	}
	if op == AdjMultiply && d.neg {
		return errors.New("a multiply adjustment may not use a negative factor")
	}
	return nil
}

// specificity assigns the level from the most specific dimension the rule constrains,
// in the order fixed by §8.2.
func specificity(r *rule) Level {
	switch {
	case r.credential != "":
		return LevelCredential
	case r.deployment != "":
		return LevelDeployment
	case r.provider != "" && r.model != "":
		return LevelProviderModel
	case r.model != "":
		return LevelModel
	case r.modelPrefix != "":
		return LevelModelPrefix
	case r.provider != "":
		return LevelProvider
	}
	return LevelDefault
}

func (c *Catalog) buildIndex() {
	for cl := range c.idx {
		c.idx[cl] = classIndex{
			byCredential: map[string][]*rule{},
			byDeployment: map[string][]*rule{},
			byProvModel:  map[provModelKey][]*rule{},
			byModel:      map[string][]*rule{},
			byPrefix:     map[string][]*rule{},
			byProvider:   map[string][]*rule{},
		}
	}
	for _, r := range c.rules {
		ix := &c.idx[r.class]
		ix.count++
		switch r.level {
		case LevelCredential:
			ix.byCredential[r.credential] = append(ix.byCredential[r.credential], r)
		case LevelDeployment:
			ix.byDeployment[r.deployment] = append(ix.byDeployment[r.deployment], r)
		case LevelProviderModel:
			k := provModelKey{r.provider, r.model}
			ix.byProvModel[k] = append(ix.byProvModel[k], r)
		case LevelModel:
			ix.byModel[r.model] = append(ix.byModel[r.model], r)
		case LevelModelPrefix:
			k := prefixKey(r.modelPrefix)
			ix.byPrefix[k] = append(ix.byPrefix[k], r)
		case LevelProvider:
			ix.byProvider[r.provider] = append(ix.byProvider[r.provider], r)
		default:
			ix.defaults = append(ix.defaults, r)
		}
		if r.class == ClassSubscription {
			c.subs[r.id] = &subState{}
		}
	}
	for cl := range c.idx {
		ix := &c.idx[cl]
		for _, m := range []map[string][]*rule{ix.byCredential, ix.byDeployment, ix.byModel, ix.byProvider} {
			for _, rs := range m {
				sortBucket(rs)
			}
		}
		for _, rs := range ix.byProvModel {
			sortBucket(rs)
		}
		for _, rs := range ix.byPrefix {
			sort.Slice(rs, func(i, j int) bool { return betterPrefix(rs[i], rs[j]) })
		}
		sortBucket(ix.defaults)
	}
}

// sortBucket puts a bucket in selection order: higher priority first, then rule id.
// Every bucket holds exactly one specificity level, so this is the whole tie-break.
func sortBucket(rs []*rule) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].priority != rs[j].priority {
			return rs[i].priority > rs[j].priority
		}
		return rs[i].id < rs[j].id
	})
}

// Rules returns the compiled rule ids in catalog order. It exists for diagnostics.
func (c *Catalog) Rules() []string {
	out := make([]string, len(c.rules))
	for i, r := range c.rules {
		out[i] = r.id
	}
	return out
}
