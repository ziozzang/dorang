package router

import (
	"math"
	"slices"
	"sync"
	"time"

	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/pricing"
)

// candidate is one deployment under consideration for one attempt. Every field
// a strategy might read is computed once, before ordering, so a comparator that
// runs O(n log n) times never re-queries a lock-taking source.
type candidate struct {
	dep *Deployment
	idx int // stable order across the whole candidate set: the final tie-break

	creds     []capacity.Candidate
	preferred string // preferred credential, from a pin or a sticky entry
	pinned    bool   // a credential pin: spill is forbidden for this candidate

	cost      pricing.Cost
	costCred  string
	costNano  int64
	costKnown bool
	busy      int
	busyKnown bool
	ttft      time.Duration
	tps       float64

	sticky      bool
	prefixHit   bool
	prefixDepth int

	rr int
	wr float64
}

// cmp is a comparator's answer: negative if a wins, positive if b wins, and
// exactly zero for NO OPINION.
//
// Zero really means silence, not equality. That distinction is what keeps an
// unproven deployment from winning on ignorance: a backend with no latency
// sample does not compare as "fast", it declines to compare at all, and the
// next strategy in the chain decides. Reading a missing sample as zero — which
// is what internal/health literally returns — would make every cold backend the
// fastest and the cheapest thing in the group.
func compare(s Strategy, a, b *candidate) int {
	switch s {
	case StrategyPrefixSticky:
		return boolFirst(a.prefixHit, b.prefixHit)
	case StrategySticky:
		return boolFirst(a.sticky, b.sticky)
	case StrategyLowestCost:
		// Marginal cost only (§8.1): a sunk subscription must not make a
		// saturated plan look cheap. An unpriced candidate has no opinion
		// rather than a cost of zero, or the cheapest deployment in every group
		// would be the one nobody has written a price rule for.
		if !a.costKnown || !b.costKnown {
			return 0
		}
		return cmpInt64(a.costNano, b.costNano)
	case StrategyLeastBusy:
		if !a.busyKnown || !b.busyKnown {
			return 0
		}
		return cmpInt(a.busy, b.busy)
	case StrategyLowestLatency:
		if a.ttft <= 0 || b.ttft <= 0 {
			return 0
		}
		return cmpInt64(int64(a.ttft), int64(b.ttft))
	case StrategyHighestTPS:
		if a.tps <= 0 || b.tps <= 0 {
			return 0
		}
		return -cmpFloat(a.tps, b.tps)
	case StrategyPriority:
		// Deployment priority uses the canonical scale: lower is preferred.
		return cmpInt(a.dep.Priority, b.dep.Priority)
	case StrategyRoundRobin:
		return cmpInt(a.rr, b.rr)
	case StrategyWeightedRandom:
		return cmpFloat(a.wr, b.wr)
	}
	return 0
}

// chainCompare walks the tie-break chain and returns the first opinion, plus
// the strategy that held it.
func chainCompare(chain []Strategy, a, b *candidate) (int, Strategy) {
	for _, s := range chain {
		if c := compare(s, a, b); c != 0 {
			return c, s
		}
	}
	return 0, ""
}

func boolFirst(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	default:
		return 1
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// order sorts candidates by the chain, falling back to configuration order.
// The sort is stable and the final tie-break is deterministic, so the same
// inputs always produce the same order — a router whose answer depends on map
// iteration is untestable and unexplainable.
//
// slices.SortStableFunc rather than sort.SliceStable: the latter sorts through
// reflection, which §15.5 prohibits on this path.
func order(chain []Strategy, cands []candidate) {
	if len(cands) < 2 {
		return
	}
	slices.SortStableFunc(cands, func(a, b candidate) int {
		if c, _ := chainCompare(chain, &a, &b); c != 0 {
			return c
		}
		return cmpInt(a.idx, b.idx)
	})
}

// decidedBy names the strategy that separated the winner from the runner-up.
// It is what the decision's Reason reports, and it is deliberately the FIRST
// strategy with an opinion rather than the last: in [prefix_sticky, lowest_cost]
// a prefix hit that also happens to be cheapest was chosen because it was warm.
func decidedBy(chain []Strategy, cands []candidate) (Strategy, bool) {
	if len(cands) < 2 {
		return "", false
	}
	_, s := chainCompare(chain, &cands[0], &cands[1])
	return s, s != ""
}

// rrState is smooth weighted round-robin state, keyed by router-wide
// deployment index.
//
// Smooth WRR rather than a modulo counter because a modulo counter with weights
// emits a run of the heavy target and then a run of the light one, which is a
// worse arrival pattern for a prefix cache than interleaving them.
type rrState struct {
	mu      sync.Mutex
	current []int64
}

// assignRR fills each candidate's rr rank with a weighted round-robin position
// and advances the state by exactly one selection.
//
// The full ordering is computed on a copy, and only the first step is
// committed. Committing all n steps would return the state to where it started
// and produce the same order on every request, which is a round-robin that does
// not rotate.
func (r *Router) assignRR(cands []candidate) {
	n := len(cands)
	if n == 0 {
		return
	}
	var stack [32]int64
	var cur []int64
	if n <= len(stack) {
		cur = stack[:n]
	} else {
		cur = make([]int64, n)
	}

	r.rr.mu.Lock()
	var total int64
	for i := range cands {
		cur[i] = r.rr.current[cands[i].dep.ridx]
		cands[i].rr = -1
		total += weightOf(cands[i].dep)
	}

	for rank := 0; rank < n; rank++ {
		best := -1
		for i := range cands {
			if cands[i].rr >= 0 {
				continue // already ranked
			}
			cur[i] += weightOf(cands[i].dep)
			if best < 0 || cur[i] > cur[best] {
				best = i
			}
		}
		if best < 0 {
			break
		}
		cur[best] -= total
		cands[best].rr = rank
		if rank == 0 {
			// Commit exactly this step to the shared state.
			for i := range cands {
				r.rr.current[cands[i].dep.ridx] += weightOf(cands[i].dep)
			}
			r.rr.current[cands[best].dep.ridx] -= total
		}
	}
	r.rr.mu.Unlock()
}

func weightOf(d *Deployment) int64 {
	if d.Weight <= 0 {
		return 1
	}
	return int64(d.Weight)
}

// assignWeightedRandom gives each candidate a sort key drawn so that ordering
// ascending by the key is a weighted random permutation: key = -ln(u)/weight is
// an exponential race, and the probability that a candidate comes first is its
// share of total weight.
func assignWeightedRandom(cands []candidate, rnd func() float64) {
	for i := range cands {
		u := rnd()
		if u <= 0 {
			u = math.SmallestNonzeroFloat64
		}
		cands[i].wr = -math.Log(u) / float64(weightOf(cands[i].dep))
	}
}

// chainUses reports whether a strategy appears in the chain, so the expensive
// inputs (pricing every candidate, querying the capacity broker per candidate)
// are computed only when something will read them.
func chainUses(chain []Strategy, s Strategy) bool {
	for _, c := range chain {
		if c == s {
			return true
		}
	}
	return false
}
