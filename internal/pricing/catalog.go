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
	cComputeSeconds
	cAudioSeconds
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
	// The two second axes. Their names are the whole of the convention: a rate says
	// which quantity it prices, in the one place a catalog author cannot omit it.
	cComputeSeconds: {"compute_seconds", "compute_seconds", UnitPerComputeSecond, microsPerSecond, 6},
	cAudioSeconds:   {"audio_seconds", "audio_seconds", UnitPerAudioSecond, microsPerSecond, 6},
}

// tokenComponents is the set of components that price a token count. It is what
// [noPriceReason] asks about when the vendor said it billed by duration.
var tokenComponents = [numComponents]bool{
	cInput: true, cOutput: true, cCacheRead: true, cCacheWrite: true, cReasoning: true,
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

	// Provenance, required on a notional_rate rule and rejected on every other class.
	source   string
	asOf     time.Time
	asOfText string
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
	// appliedCap sizes Cost.AppliedRules in one allocation: one marginal winner plus a
	// subscription and a notional winner if this catalog can produce them.
	appliedCap int

	// subs is fixed at load: one entry per fixed_subscription rule, so the map itself is
	// never written after construction and Price only takes that rule's read lock.
	subs map[string]*subState

	// carries is written only by Settle.
	carryMu sync.Mutex
	carries map[string]*carrySet

	// clock is what "now" means to this catalog. It is only ever consulted to decide
	// whether a settlement's instant is in the future; every other use of time comes from
	// the request. Injected so that a deployment with a controlled clock — and every test
	// in this package — settles against the same instant the rest of the process does,
	// rather than against a wall clock a fixture cannot see.
	clock func() time.Time
}

// SetClock replaces the catalog's notion of the present.
//
// It is called once, by whoever loads the catalog, before the catalog is published to
// anything that prices. Nil restores the wall clock.
func (c *Catalog) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	c.clock = now
}

func (c *Catalog) nowInstant() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// AdoptState carries the running accounting state of a previous catalog into this one.
//
// This is what makes a configuration reload not a billing event. A subscription's
// accumulator — how much of the open period's plan cost has already been attributed — lives
// with the catalog, and a reload builds a fresh one; without this, an open period restarts
// its attribution from the reload instant and attributes the remainder of the period a
// second time. Measured: twenty reloads attributed 1,030 USD of a 100 USD plan, and one
// reload nine tenths of the way through a period put 91.00 USD on a single request — which
// internal/app then reserves against THAT request's budget.
//
// The bound was not "one plan cost per deliberate change" either. A SIGHUP re-reads and
// re-applies whatever it finds, deliberately: an operator's SIGHUP is the only way to pick
// up an edited external price catalog or a rotated key_file secret, so it cannot be
// suppressed on "the main file did not change" without breaking the mechanism it is there
// for. The fix therefore belongs here — a redundant reload has to be free — and not in a
// watcher that guesses whether anything moved.
//
// State is carried per rule id. A rule that is new in this catalog starts empty, and a rule
// whose amount or period changed keeps the total it has already attributed: accrue clamps
// against the NEW plan cost, so a cheaper plan attributes nothing further for the rest of
// the period and a dearer one attributes the difference. Both directions are bounded by one
// plan cost, which is the property §8.1 exists to hold.
func (c *Catalog) AdoptState(prev *Catalog) {
	if prev == nil || prev == c {
		return
	}
	for id, st := range c.subs {
		p := prev.subs[id]
		if p == nil {
			continue
		}
		if snap := p.snap.Load(); snap != nil {
			st.snap.Store(snap)
		}
	}
	// The sub-nano rounding remainders go with it, for the same reason: §8.3 promises
	// that repeated small requests do not drift, and a remainder dropped on every reload
	// is drift with a schedule.
	prev.carryMu.Lock()
	carried := make(map[string]*carrySet, len(prev.carries))
	for k, v := range prev.carries {
		cp := *v
		carried[k] = &cp
	}
	prev.carryMu.Unlock()

	c.carryMu.Lock()
	if c.carries == nil {
		c.carries = carried
	} else {
		for k, v := range carried {
			c.carries[k] = v
		}
	}
	c.carryMu.Unlock()
}

// SubscriptionState is one subscription rule's accumulator as it crosses a PROCESS
// boundary. [Catalog.AdoptState] carries the same thing across a configuration reload,
// which is a different boundary and needs no serialization.
//
// It is the whole durable form: which period is open, and how much of that period's plan
// cost has already been attributed to rows. Attributed is atto-scaled decimal digits
// rather than a number, because a 100 USD plan is 10^20 atto and no 64-bit integer holds
// it; see [u128.text].
type SubscriptionState struct {
	// RuleID is the fixed_subscription rule this belongs to. State is carried per rule
	// id, so a catalog that no longer declares the rule simply ignores the row.
	RuleID string
	// PeriodStart is the beginning of the period the accumulator is open on, in the
	// rule's own location.
	PeriodStart time.Time
	// Attributed is the plan cost this period has already attributed, in atto-units of
	// the catalog currency, as exact decimal digits. An empty string is zero.
	Attributed string
}

// SnapshotState renders the running accumulators for a caller that will persist them.
//
// It projects each accumulator forward to `at` rather than reporting where it stands.
// That is deliberate and it is DESIGN §9.6's rule applied to a second counter: **the
// durable record is advanced before the amount it covers is spent, so it is always at or
// ahead of reality.** A process that dies between checkpoints therefore resumes having
// attributed slightly MORE than it really did — the period attributes a little less than
// the plan cost, which §8.1 allows — instead of resuming behind and attributing a slice
// of the period twice, which §8.1 forbids outright.
//
// A caller checkpointing on a timer passes now + one interval; a caller checkpointing on
// a clean shutdown passes now, because nothing is about to be lost.
//
// It mutates nothing: the in-memory accumulator keeps attributing normally inside the
// projected window, and a process that survives the interval loses nothing at all.
func (c *Catalog) SnapshotState(at time.Time) []SubscriptionState {
	if len(c.subs) == 0 {
		return nil
	}
	out := make([]SubscriptionState, 0, len(c.subs))
	for id, st := range c.subs {
		snap := st.snap.Load()
		if snap == nil {
			// Nothing has settled against this rule in this process. There is no
			// accumulator to carry, and writing a zero would pull a shared record
			// backwards on a node that has one.
			continue
		}
		attributed := snap.attributed
		if r := st.rule; r != nil && !at.Before(snap.periodStart) {
			_, end := periodBounds(r.period, snap.periodStart, r.loc)
			if ahead, err := accrue(r.amountAtto, at, snap.periodStart, end); err == nil &&
				ahead.cmp(attributed) > 0 {
				attributed = ahead
			}
		}
		out = append(out, SubscriptionState{
			RuleID:      id,
			PeriodStart: snap.periodStart,
			Attributed:  attributed.text(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuleID < out[j].RuleID })
	return out
}

// RestoreState adopts accumulators that were persisted by an earlier process.
//
// This is the process-boundary half of [Catalog.AdoptState], and without it a restart is
// a billing event in exactly the way a reload used to be: the accumulator lives in
// memory, a new process builds an empty one, and the open period attributes its whole
// elapsed share again. Measured before this existed: **180.46 USD attributed of a 100.00
// USD monthly plan across two process starts**, with the second start's whole 90.23 USD
// share landing on the first request after the restart — which internal/app reserves
// against THAT request's budget, so a brand-new key with a 1.00 USD ceiling was refused
// `400 budget_exceeded` on its first ever request.
//
// # The clock check here is the one that can disagree
//
// `now` is this process's reading of the present. `s.PeriodStart` was stamped by whatever
// process wrote the row, which may be a different process on a different host with a
// different clock — and that is what makes the comparison below a real test rather than a
// tautology. The same comparison inside [Catalog.Settle] is not: there the row's stamp and
// the catalog's clock are two readings of ONE clock, taken in order, so the stamp can
// never be the later of the two and the clamp there can never fire in a default
// deployment. An NTP step, a VM resume or a bad RTC moves both of those readings together;
// it does not move this one, because the other observation is already written down.
//
// A stored period that has not begun yet is re-dated to the open one rather than dropped.
// Dropping it would let the open period attribute its whole plan cost again on top of
// whatever the future-stamped writer already booked; re-dating keeps the attributed total
// as a bound, so the open period attributes only the remainder and the next period still
// opens at zero.
//
// It returns the number of accumulators adopted, for the caller's log line.
func (c *Catalog) RestoreState(states []SubscriptionState, now time.Time) (int, error) {
	adopted := 0
	for _, s := range states {
		st := c.subs[s.RuleID]
		if st == nil {
			// A rule the running configuration no longer declares. Its row stays in
			// the store — a rule that comes back should find its period where it
			// left it — and nothing here needs it.
			continue
		}
		attributed, err := parseU128(s.Attributed)
		if err != nil {
			return adopted, fmt.Errorf("pricing: subscription state for %q: %w", s.RuleID, err)
		}
		start := s.PeriodStart
		if start.IsZero() {
			continue
		}
		if r := st.rule; r != nil {
			// The clamp. Two observations: the stamp the stored row carries, and
			// this process's own present.
			if openStart, _ := periodBounds(r.period, now, r.loc); start.After(openStart) {
				start = openStart
			}
			// And never above one plan cost, whatever the row says: a stored value
			// larger than the plan the running configuration declares would make the
			// period attribute nothing for the rest of its life.
			if attributed.cmp(r.amountAtto) > 0 {
				attributed = r.amountAtto
			}
		}
		st.mu.Lock()
		cur := st.snap.Load()
		// A restore never pulls an accumulator backwards. A node that has already
		// settled rows in this period holds the more advanced figure, and adopting a
		// staler durable row over it would re-attribute the difference.
		if cur == nil || start.After(cur.periodStart) ||
			(start.Equal(cur.periodStart) && attributed.cmp(cur.attributed) > 0) {
			st.snap.Store(&subSnapshot{periodStart: start, attributed: attributed})
			adopted++
		}
		st.mu.Unlock()
	}
	return adopted, nil
}

// carrySet holds the sub-nano remainder of each rounded field for one settlement bucket.
type carrySet struct {
	marginal     int64
	subscription int64
	adjustment   int64
	notional     int64
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
	// rule is the compiled rule this accumulator belongs to, so that the state can be
	// projected and clamped ([Catalog.SnapshotState], [Catalog.RestoreState]) without a
	// scan over every rule in the catalog. It is written once, at compile time.
	rule *rule
}

// subSnapshot is what a period has already attributed, not what it has already used.
//
// The field that used to live here was the period's marginal-usage total, because each
// request's share was computed as plan_cost x (request_marginal / marginal_to_date) and
// then added to the ledger. Those shares are each an estimate of the SAME quantity — the
// plan cost per unit of traffic so far — so adding them counted the plan cost once per
// request: a period of N equal requests reported plan_cost x H_N, 5.19 plan costs at
// N = 100 and still climbing (§8.1).
//
// Carrying the attributed total instead makes the period's rows increments of one running
// total rather than repeated estimates of it. attributed only ever rises, never above the
// plan cost, and a request records the difference — so the rows sum to the total by
// construction and no settled row is ever restated.
// The accumulator lives for the life of this Catalog, which is the life of one loaded
// configuration, and a config reload builds a fresh Catalog. [Catalog.AdoptState] is what
// carries it across the swap, and it is not optional: without it an open period restarted
// its attribution from the reload instant and attributed the rest of the period again —
// twenty reloads put 1,030 USD on a 100 USD plan, and one reload late in a period put a
// whole 91.00 USD on a single request.
type subSnapshot struct {
	periodStart time.Time
	attributed  u128 // plan cost already attributed to this period, in atto-units
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
	ComputeSeconds  string `yaml:"compute_seconds"`
	AudioSeconds    string `yaml:"audio_seconds"`
	// Seconds exists only so the refusal can name the key. See [rawRule.Seconds].
	Seconds string `yaml:"seconds"`
}

type rawRule struct {
	ID       string   `yaml:"id"`
	Class    string   `yaml:"class"`
	Match    rawMatch `yaml:"match"`
	When     *rawWhen `yaml:"when"`
	Unit     string   `yaml:"unit"`
	Priority int      `yaml:"priority"`

	Input          string `yaml:"input"`
	Output         string `yaml:"output"`
	CacheRead      string `yaml:"cache_read"`
	CacheWrite     string `yaml:"cache_write"`
	Reasoning      string `yaml:"reasoning"`
	Request        string `yaml:"request"`
	Characters     string `yaml:"characters"`
	ComputeSeconds string `yaml:"compute_seconds"`
	AudioSeconds   string `yaml:"audio_seconds"`
	// Seconds is decoded ONLY so that the refusal below can name the key the operator
	// wrote and say what to write instead. Deleting the field would turn `seconds:`
	// into KnownFields' "field seconds not found", which reads like a typo — and it is
	// not a typo, it is the spelling this catalog used to have, whose rate was applied
	// to wall time whatever the vendor billed.
	Seconds string `yaml:"seconds"`

	Tiers    []rawTier `yaml:"tiers"`
	TierMode string    `yaml:"tier_mode"`

	AmountPerPeriod string `yaml:"amount_per_period"`
	Period          string `yaml:"period"`

	Op        string `yaml:"op"`
	Amount    string `yaml:"amount"`
	AppliesTo string `yaml:"applies_to"`
	Order     int    `yaml:"order"`

	Source string `yaml:"source"`
	AsOf   string `yaml:"as_of"`
}

func (r rawRule) rateStrings() [numComponents]string {
	return [numComponents]string{
		cInput: r.Input, cOutput: r.Output, cCacheRead: r.CacheRead,
		cCacheWrite: r.CacheWrite, cReasoning: r.Reasoning,
		cRequest: r.Request, cCharacters: r.Characters,
		cComputeSeconds: r.ComputeSeconds, cAudioSeconds: r.AudioSeconds,
	}
}

func (t rawTier) rateStrings() [numComponents]string {
	return [numComponents]string{
		cInput: t.Input, cOutput: t.Output, cCacheRead: t.CacheRead,
		cCacheWrite: t.CacheWrite, cReasoning: t.Reasoning,
		cRequest: t.Request, cCharacters: t.Characters,
		cComputeSeconds: t.ComputeSeconds, cAudioSeconds: t.AudioSeconds,
	}
}

// errAmbiguousSecondRate is the load error for the rate key that did not say which
// second it priced. It is the companion of [errAmbiguousSecond]: a catalog can name the
// axis on the unit, on the rate, or on both, and getting either one wrong is a load
// error rather than a wrong invoice.
const errAmbiguousSecondRate = "rate \"seconds\" does not say WHICH second it prices: " +
	"use \"compute_seconds\" for the request's own wall time (a GPU-second rate, unit " +
	"per_compute_second) or \"audio_seconds\" for the length of the recording a " +
	"transcription vendor bills for (unit per_audio_second). They are different " +
	"quantities on different axes and dorang will not substitute one for the other " +
	"(DESIGN §10.7: a billing unit is never converted). A ten-minute recording " +
	"transcribed in eight seconds is ten minutes on the invoice"

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

	if class != ClassNotional && (raw.Source != "" || raw.AsOf != "") {
		return nil, errors.New("source/as_of belong to a notional_rate rule")
	}
	// Before the class switch, because the ambiguous key is refused on every class and
	// not only where a rate table is legal. It is the first thing an operator upgrading
	// a catalog will hit, so it is refused once, in one place, with the whole answer.
	if raw.Seconds != "" {
		return nil, errors.New(errAmbiguousSecondRate)
	}
	for i := range raw.Tiers {
		if raw.Tiers[i].Seconds != "" {
			return nil, fmt.Errorf("tier %d: %s", i, errAmbiguousSecondRate)
		}
	}

	switch class {
	case ClassMarginal, ClassNotional:
		if err := compileUsage(raw, r); err != nil {
			return nil, err
		}
		if class == ClassNotional {
			if err := compileProvenance(raw, r); err != nil {
				return nil, err
			}
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

// compileProvenance enforces §8.5's first rule: a notional rate without a source and an
// as-of date is a guess wearing a currency symbol, so it is a load error, not a warning.
func compileProvenance(raw *rawRule, r *rule) error {
	r.source = strings.TrimSpace(raw.Source)
	if r.source == "" {
		return errors.New("source is required on a notional_rate rule: an estimate without provenance is not auditable")
	}
	r.asOfText = strings.TrimSpace(raw.AsOf)
	if r.asOfText == "" {
		return errors.New("as_of is required on a notional_rate rule: a rate with no date cannot be judged stale")
	}
	for _, layout := range [...]string{"2006-01-02", time.RFC3339} {
		t, err := time.ParseInLocation(layout, r.asOfText, r.loc)
		if err == nil {
			r.asOf = t
			return nil
		}
	}
	return fmt.Errorf("as_of: %q is not a date (want YYYY-MM-DD or RFC3339)", r.asOfText)
}

// compileUsage compiles the rate table shared by marginal_usage and notional_rate: the
// two classes price identically and differ only in where the result lands.
func compileUsage(raw *rawRule, r *rule) error {
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
	// A negative percent is a discount and a negative add is a credit; both are real
	// things and both stay legal. What is bounded is how far one may reach: a discount
	// deeper than 100% is not a discount, it is a payout expressed as one, and the
	// operator who wrote -150 meant -15 far more often than they meant "pay the caller
	// half the bill again". The size of an `add` credit cannot be bounded here — it is an
	// absolute amount and the request it applies to is not known until it arrives — so
	// that one is bounded at the total instead, which is where [Cost.Floored] clamps.
	if op == AdjPercent && d.neg && d.exceedsHundred() {
		return fmt.Errorf("amount %q: a percent discount may not exceed -100%% "+
			"(it would make the request cost less than nothing)", d.text)
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
			c.subs[r.id] = &subState{rule: r}
		}
	}
	c.appliedCap = 1
	if c.idx[ClassSubscription].count > 0 {
		c.appliedCap++
	}
	if c.idx[ClassNotional].count > 0 {
		c.appliedCap++
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
