package auth

import (
	"fmt"
	"sort"
	"strings"
)

// DESIGN §11.6: tiers.
//
// A key belongs to a tier, and the tier — not the caller — decides what it may
// claim. Three things are fixed and the rest is configuration:
//
//  1. A tier is a property of the key, assigned by an operator. §10.5's rule
//     holds without exception: an operator can grant urgency, a caller cannot
//     claim it. Nothing in this file reads a request.
//  2. A tier carries defaults for budget, rate limits, model set, concurrency
//     and priority, and a per-key setting may NARROW them but never widen them.
//     [Tier.Apply] is the only way the two are combined and it is monotone in
//     one direction by construction.
//  3. Batch work ranks below every tier's interactive work, including the
//     lowest tier's. Batch has no waiting human, so latency costs it nothing,
//     and a paying customer's batch job must not delay a free user's
//     interactive request.
//
// The names `unlimited > commercial > free` are defaults. An operator defines
// their own set with their own ordering through [NewTierSet].

// TierSpacing is the gap between adjacent tiers on the canonical priority
// scale.
//
// Two, not one, for the reason §7.5 gives: vLLM's /v1/responses decrements
// priority by one after each built-in-tool round trip, so bands closer than two
// would let turn 2 of a tool loop cross into the band above the one the caller
// was granted.
const TierSpacing = 2

// Tier is one configured tier: a name, its position in the operator's ordering,
// and the ceiling it grants.
//
// Every limit is a ceiling, never a floor. A nil numeric field means the tier
// imposes no ceiling on that axis, which is what `unlimited` looks like; an
// empty Models list means the tier restricts no model.
type Tier struct {
	// Name is the configuration spelling, stored in api_keys.tier.
	Name string
	// Rank orders tiers against each other. HIGHER IS MORE PRIVILEGED, which is
	// the direction an operator writes ("unlimited > commercial > free") and the
	// opposite of the canonical priority scale — [TierSet.Canonical] converts.
	Rank int
	// PriorityClass is the class a key of this tier is scheduled in when it has
	// none of its own. Empty means the class is derived from the tier's position
	// rather than named.
	PriorityClass string

	// MaxBudgetNanoUSD is the spend ceiling the tier grants, in nano-USD.
	MaxBudgetNanoUSD *int64
	// RPMLimit, TPMLimit and MaxParallel are the rate and concurrency ceilings.
	RPMLimit    *int64
	TPMLimit    *int64
	MaxParallel *int64
	// Models is the model set the tier grants. Empty grants every model; "*"
	// grants every model explicitly.
	Models []string
}

// TierSet is an operator's complete tier configuration.
//
// It is immutable once built and safe for concurrent use. There is deliberately
// no method that adds a tier to a live set: a tier appearing at run time is a
// tier no key was assigned by an operator.
type TierSet struct {
	// asc holds the tiers in ascending privilege order. Position in this slice —
	// not Tier.Rank — is what the canonical scale is computed from, so a set
	// configured with ranks 1/5/9 and one configured with 1/2/3 schedule
	// identically.
	asc    []Tier
	byName map[string]int
	def    string
}

// ErrNoTiers reports a tier set with nothing in it.
var errNoTiers = fmt.Errorf("auth: a tier set needs at least one tier")

// NewTierSet builds a tier set from an operator's configuration.
//
// Tiers are sorted into ascending privilege order by Rank, with the name
// breaking a tie so that the result does not depend on map iteration order.
// defaultTier names the tier a key carries when its row names none; it must be
// one of the tiers, because a default that names nothing is how every
// unassigned key silently becomes unrestricted.
func NewTierSet(tiers []Tier, defaultTier string) (*TierSet, error) {
	if len(tiers) == 0 {
		return nil, errNoTiers
	}
	asc := make([]Tier, len(tiers))
	copy(asc, tiers)
	for i := range asc {
		asc[i].Name = strings.TrimSpace(asc[i].Name)
		if asc[i].Name == "" {
			return nil, fmt.Errorf("auth: tier %d has no name", i)
		}
		asc[i].Models = append([]string(nil), asc[i].Models...)
	}
	sort.SliceStable(asc, func(i, j int) bool {
		if asc[i].Rank != asc[j].Rank {
			return asc[i].Rank < asc[j].Rank
		}
		return asc[i].Name < asc[j].Name
	})
	byName := make(map[string]int, len(asc))
	for i := range asc {
		if _, dup := byName[asc[i].Name]; dup {
			return nil, fmt.Errorf("auth: tier %q is defined twice", asc[i].Name)
		}
		byName[asc[i].Name] = i
	}
	def := strings.TrimSpace(defaultTier)
	if def == "" {
		def = asc[0].Name // the least privileged tier, never the most
	}
	if _, ok := byName[def]; !ok {
		return nil, fmt.Errorf("auth: default tier %q is not one of %v", def, namesOf(asc))
	}
	return &TierSet{asc: asc, byName: byName, def: def}, nil
}

func namesOf(t []Tier) []string {
	out := make([]string, len(t))
	for i := range t {
		out[i] = t[i].Name
	}
	return out
}

// DefaultTiers is the set of §11.6's table: free < commercial < unlimited.
//
// It carries no numeric ceilings. A ceiling invented here would be a policy
// dorang chose on an operator's behalf and would be wrong for every deployment
// but one; what the default set fixes is the ORDERING, which is the part §11.6
// says is not negotiable.
func DefaultTiers() *TierSet {
	s, err := NewTierSet([]Tier{
		{Name: "free", Rank: 0, PriorityClass: "batch"},
		{Name: "commercial", Rank: 1, PriorityClass: "interactive"},
		{Name: "unlimited", Rank: 2, PriorityClass: "realtime"},
	}, "free")
	if err != nil {
		panic("auth: DefaultTiers is not a valid tier set: " + err.Error())
	}
	return s
}

// Len reports how many tiers are configured.
func (s *TierSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.asc)
}

// Names returns the tier names in ascending privilege order.
func (s *TierSet) Names() []string {
	if s == nil {
		return nil
	}
	return namesOf(s.asc)
}

// Default returns the tier a key with no tier of its own belongs to.
func (s *TierSet) Default() string {
	if s == nil {
		return ""
	}
	return s.def
}

// Get returns a tier by name. An unknown name is not resolved to the default:
// a row naming a tier that no longer exists is a configuration error the caller
// must see, not a key that quietly becomes free — or, far worse, unlimited.
func (s *TierSet) Get(name string) (Tier, bool) {
	if s == nil {
		return Tier{}, false
	}
	i, ok := s.byName[strings.TrimSpace(name)]
	if !ok {
		return Tier{}, false
	}
	return s.asc[i], true
}

// Resolve returns the tier for a key row's stored value, falling back to the
// default only when the row names NO tier at all.
func (s *TierSet) Resolve(name string) (Tier, error) {
	if s == nil {
		return Tier{}, errNoTiers
	}
	if strings.TrimSpace(name) == "" {
		t, _ := s.Get(s.def)
		return t, nil
	}
	t, ok := s.Get(name)
	if !ok {
		return Tier{}, fmt.Errorf("auth: no tier named %q; configured tiers are %v", name, s.Names())
	}
	return t, nil
}

// Index returns a tier's position in ascending privilege order.
func (s *TierSet) Index(name string) (int, bool) {
	if s == nil {
		return 0, false
	}
	i, ok := s.byName[strings.TrimSpace(name)]
	return i, ok
}

// Outranks reports whether tier a is more privileged than tier b. An unknown
// tier outranks nothing.
func (s *TierSet) Outranks(a, b string) bool {
	ia, ok := s.Index(a)
	if !ok {
		return false
	}
	ib, ok := s.Index(b)
	if !ok {
		return false
	}
	return ia > ib
}

// Canonical is the scheduling priority of work at a tier, on internal/router's
// canonical scale, where LOWER IS MORE URGENT.
//
// The whole of §11.6's ordering rule is in the arithmetic:
//
//	interactive(tier) = (N-1 - index) * spacing        →  0 .. (N-1)*spacing
//	batch(tier)       = N*spacing + (N-1 - index) * spacing
//
// Every batch value is at least N*spacing and every interactive value is at
// most (N-1)*spacing, so the MOST privileged tier's batch work still ranks
// below the LEAST privileged tier's interactive work — which is exactly what
// "batch sits below the free tier" means, and it holds for any N by
// construction rather than by a table somebody has to keep consistent.
//
// Batch work is still ordered among itself by tier. Yielding to every
// interactive request is the rule; being indistinguishable from every other
// batch job is not implied by it.
//
// The default three-tier set reproduces §7.5's own example exactly: free
// interactive is 4, free batch is 10, and unlimited interactive is 0.
func (s *TierSet) Canonical(tier string, batch bool) int {
	n := s.Len()
	if n == 0 {
		return 0
	}
	i, ok := s.Index(tier)
	if !ok {
		i = 0 // an unresolvable tier is scheduled as the least privileged one
	}
	v := (n - 1 - i) * TierSpacing
	if batch {
		v += n * TierSpacing
	}
	return v
}

// BatchFloor is the most urgent canonical value any batch work can take. Every
// interactive value is strictly below it. It is exported so that a scheduler
// can assert the separation rather than assume it.
func (s *TierSet) BatchFloor() int { return s.Len() * TierSpacing }

// InteractiveCeiling is the least urgent canonical value any interactive work
// can take. It is strictly below [TierSet.BatchFloor].
func (s *TierSet) InteractiveCeiling() int {
	if s.Len() == 0 {
		return 0
	}
	return (s.Len() - 1) * TierSpacing
}

// Classes renders the tier set as internal/router's class table, so that the
// priority configuration a backend is driven from is derived from the tiers
// rather than maintained beside them.
//
// Each tier contributes two classes: "<tier>" and "<tier>:batch".
func (s *TierSet) Classes() map[string]int {
	if s == nil {
		return nil
	}
	out := make(map[string]int, 2*len(s.asc))
	for _, t := range s.asc {
		out[t.Name] = s.Canonical(t.Name, false)
		out[t.Name+":batch"] = s.Canonical(t.Name, true)
	}
	return out
}

// --- narrowing ---------------------------------------------------------------

// Apply folds a tier's defaults onto a key's own limits and returns the
// effective envelope.
//
// Every axis narrows and none widens:
//
//   - A numeric ceiling: nil on the key means "inherit the tier"; nil on the
//     tier means "the tier imposes none". Where both are set the SMALLER wins,
//     so a key configured with a larger number than its tier gets the tier's.
//   - The model set: empty means "no restriction from this side". Where both
//     restrict, the effective set is the intersection, which can legitimately be
//     empty — a key restricted to a model its tier does not grant can use
//     nothing, and that is the honest answer rather than a silent grant.
//   - Blocked, expiry and the route allow-list belong to the key alone; a tier
//     has no view of them and Apply does not touch them.
//
// Apply is idempotent: applying a tier to an envelope it has already narrowed
// changes nothing.
func (t Tier) Apply(l Limits) Limits {
	l.MaxBudgetNanoUSD = narrowInt(t.MaxBudgetNanoUSD, l.MaxBudgetNanoUSD)
	l.RPMLimit = narrowInt(t.RPMLimit, l.RPMLimit)
	l.TPMLimit = narrowInt(t.TPMLimit, l.TPMLimit)
	l.MaxParallel = narrowInt(t.MaxParallel, l.MaxParallel)
	l.Models = narrowModels(t.Models, l.Models)
	return l
}

// narrowInt returns the more restrictive of a tier ceiling and a key ceiling.
//
// The pointer distinction is load-bearing and is the same one §2.4 records: nil
// is "no ceiling configured" and 0 is "a ceiling of zero, allow nothing". A
// tier ceiling of zero therefore narrows a key to nothing, which is a usable
// way to configure a suspended tier, and flattening the two into a bare 0 is
// how a configured ceiling quietly stops existing.
func narrowInt(tier, key *int64) *int64 {
	switch {
	case tier == nil && key == nil:
		return nil
	case tier == nil:
		v := *key
		return &v
	case key == nil:
		v := *tier
		return &v
	}
	v := *tier
	if *key < v {
		v = *key
	}
	return &v
}

// narrowModels intersects two allow-lists under the convention that an empty
// list restricts nothing and "*" restricts nothing explicitly.
func narrowModels(tier, key []string) []string {
	tw, ta := wildcard(tier)
	kw, ka := wildcard(key)
	switch {
	case ta && ka:
		return nil
	case ta:
		return append([]string(nil), kw...)
	case ka:
		return append([]string(nil), tw...)
	}
	allowed := make(map[string]bool, len(tw))
	for _, m := range tw {
		allowed[m] = true
	}
	// The intersection is built in the KEY's order so that an operator reading
	// the effective set back sees the order they wrote.
	out := make([]string, 0, len(kw))
	for _, m := range kw {
		if allowed[m] {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		// Not nil: nil means "unrestricted", and an empty intersection is the
		// opposite of that. The sentinel is a list that admits nothing.
		return []string{denyAll}
	}
	return out
}

// denyAll is the model entry that matches nothing. It cannot collide with a
// real model name because a model name is a name and this is not one.
const denyAll = "\x00none"

// wildcard splits a list into its real entries and whether it grants
// everything.
func wildcard(list []string) ([]string, bool) {
	if len(list) == 0 {
		return nil, true
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		if e == "*" {
			return nil, true
		}
		out = append(out, e)
	}
	return out, false
}

// NarrowClass returns the priority class a key of this tier is actually
// scheduled in.
//
// The tier's class is a ceiling. A key may name a LESS urgent class than its
// tier — a customer who wants their reporting job out of the way of their
// interactive traffic is expressing something real — and may not name a more
// urgent one. A key naming a class the set does not know is scheduled in its
// tier's class, because the alternative is scheduling it at a value nobody
// chose.
//
// classes is internal/router's canonical table (lower is more urgent).
func (t Tier) NarrowClass(keyClass string, classes map[string]int) string {
	keyClass = strings.TrimSpace(keyClass)
	if keyClass == "" || t.PriorityClass == "" || keyClass == t.PriorityClass {
		if t.PriorityClass != "" {
			return t.PriorityClass
		}
		return keyClass
	}
	kv, ok := classes[keyClass]
	if !ok {
		return t.PriorityClass
	}
	tv, ok := classes[t.PriorityClass]
	if !ok {
		return t.PriorityClass
	}
	if kv > tv { // larger canonical value is LESS urgent: a narrowing
		return keyClass
	}
	return t.PriorityClass
}
