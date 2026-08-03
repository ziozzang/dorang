package metrics

import (
	"sync"
	"sync/atomic"

	"github.com/ziozzang/dorang/internal/meter"
)

// OverflowSentinel is the label value every dimension of a folded series
// carries.
//
// It is internal/meter's constant rather than a second spelling of the same
// idea. A rollup row and a metric series that describe the same folded traffic
// under two different sentinels cannot be joined, and the join is the only
// reason to fold rather than drop.
const OverflowSentinel = meter.OverflowSentinel

// UnknownModelSentinel is the `model` label value for a name that is not one
// this gateway is configured to serve.
//
// It is deliberately NOT [OverflowSentinel]. The two say different things and
// an operator has to be able to tell them apart:
//
//   - `__overflow__` means a cap engaged and resolution was LOST. Something is
//     wrong — the deployment outgrew a cap, or a label value is being minted.
//   - `__unknown__` means a caller asked for a name dorang does not serve. No
//     resolution was lost, because there was never a configured series for that
//     name to lose. It is a steady-state bucket a healthy gateway may carry.
//
// It is a metrics-only constant, unlike [OverflowSentinel], because there is no
// rollup row to join it to: internal/meter records the name the caller actually
// asked for, unfolded, so the join for this bucket is "the ledger rows whose
// model_group is not in the configuration" and not a shared sentinel string.
// That is where the names went — see [Requests.SetAdmittedModels].
const UnknownModelSentinel = "__unknown__"

// Default caps. They bound memory and scrape size; every one of them is a
// number an operator can raise, and every one of them counts what it refused.
const (
	// DefaultMaxRequestSeries caps `dorang_requests_total`. Its label tuple is
	// (model, provider, credential, status, endpoint). Every one of the five is
	// now bounded by something outside the request — provider and credential by
	// the configured credential set, endpoint by the route table, status by the
	// status codes dorang answers, and `model` by the configured model set via
	// [Requests.SetAdmittedModels] — and their *product* is not. That product is
	// what this cap is for, and it is a memory budget rather than a security
	// control.
	//
	// It did not used to be. internal/server takes the model from the request
	// body (or the deployment path segment) and meters it verbatim, on every
	// request including the ones it refuses, so before admission existed any
	// holder of a valid key minted label values at will and this cap was the
	// whole of what stopped it — see [DefaultMaxModelSeries].
	DefaultMaxRequestSeries = 2048
	// DefaultMaxModelSeries is the FLOOR under the per-model families, not the
	// ceiling. Two histograms hang off each entry, so a model costs about fifty
	// series.
	//
	// [Requests.SetAdmittedModels] raises the cap to hold the whole configured
	// model set, so a deployment with three hundred model groups gets three
	// hundred entries rather than a folded dashboard. That is safe precisely
	// because admission is structural: a label value has to be a name in the
	// operator's own configuration file before it can occupy an entry, so the
	// table's size is a property of the config and not of the traffic.
	//
	// It was a ceiling, and a caller could reach it on purpose. Entries are
	// never evicted, so 128 fabricated model names spent the table for the life
	// of the process: every model FIRST SEEN afterwards — including the real
	// ones, after a config reload added one — folded into [OverflowSentinel] and
	// lost its duration, TTFT and prefix-hit series until a restart. Memory
	// stayed bounded, which is all a cap promises; resolution did not, and the
	// blinding was remotely triggerable and did not heal. Admission is what
	// closed that; this number is what is left of the memory budget.
	DefaultMaxModelSeries = 128
	// DefaultMaxFallbackSeries caps `dorang_fallback_total`.
	DefaultMaxFallbackSeries = 512
	// DefaultMaxSubstitutionSeries caps `dorang_model_substitutions_total`. Its
	// label tuple is (provider, deployment, served) and all three are bounded
	// outside the request: the first two by the configuration, and `served` by
	// pkg/catalog, because internal/app admits a returned model name only after
	// resolving it to a catalog entry — see [substKey].
	//
	// It is small on purpose. A healthy deployment contributes NO series here at
	// all: the family is empty until an upstream answers as a different model,
	// which is a condition an operator is meant to act on rather than live
	// with. A table that needs raising is itself a finding.
	DefaultMaxSubstitutionSeries = 256
	// DefaultMaxAxisSeries caps each capacity axis. The principal and key axes
	// are keyed by an api-key id and a credential id, which are rows in a
	// table rather than lines in a configuration file: this is the one axis of
	// DESIGN §5.1 with no configured bound at all.
	DefaultMaxAxisSeries = 256
	// DefaultMaxSubjectSeries caps the budget subjects of `dorang_budget_spent_ratio`.
	DefaultMaxSubjectSeries = 256
	// DefaultMaxCredentialSeries caps the per-credential quota and health
	// families.
	DefaultMaxCredentialSeries = 256
)

// folder counts the folds one family has performed.
//
// The count is the point. A cap that silently discards resolution produces a
// dashboard that is quietly wrong and stays that way, because nothing about the
// output says a fold happened. `dorang_metrics_cardinality_folds_total{family}`
// is how the operator finds out the cap needs raising — or that something is
// putting a request id in a label, which is the failure this exists to survive.
type folder struct {
	family string
	folds  atomic.Uint64
}

func (f *folder) fold() { f.folds.Add(1) }

// Folds reports how many series this family has folded into the overflow key.
func (f *folder) Folds() uint64 { return f.folds.Load() }

// foldingSet is a bounded set of string keys used by the render-time collectors,
// which see their whole key space at once (a capacity snapshot, a credential
// list) rather than one key at a time.
//
// It is deterministic: the retained keys are the first max in the order the
// caller presents them, and every collector sorts before presenting, so the same
// state folds the same way on every scrape. A cap that keeps a different subset
// each time produces series that appear and vanish, which reads as a restart.
type foldingSet struct {
	max    int
	n      int
	f      *folder
	folded bool
}

func newFoldingSet(max int, f *folder) foldingSet {
	if max <= 0 {
		max = 1
	}
	return foldingSet{max: max, f: f}
}

// admit reports the label value to use for key: key itself while there is room,
// and [OverflowSentinel] once the cap is reached. The second return says whether
// this is the first key to be folded, so the caller can emit exactly one folded
// series and accumulate the rest into it.
func (s *foldingSet) admit(key string) (string, bool) {
	if s.n < s.max {
		s.n++
		return key, false
	}
	s.f.fold()
	if s.folded {
		return OverflowSentinel, false
	}
	s.folded = true
	return OverflowSentinel, true
}

// table is a bounded, concurrently readable map from a comparable key to a
// counter set. It backs every family this package owns rather than pulls.
//
// The lock discipline is the point (rule 4 of the package comment). A recorded
// observation takes the read lock and, in the steady state, finds its series and
// increments an atomic; [Registry.Gather] takes the same read lock. Two read
// locks never conflict, so a scrape cannot delay a request and a request cannot
// delay a scrape. The write lock is taken only when a series is seen for the
// first time, which is bounded by the cap and therefore happens a fixed number
// of times over the life of the process.
type table[K comparable, V any] struct {
	max int
	f   folder
	// init prepares a freshly created entry. It runs under the write lock, so
	// it happens at most max+1 times over the life of the process and never on
	// the path an existing series takes.
	init func(*V)

	mu sync.RWMutex
	m  map[K]*V

	// overflow is the folded series. It is created lazily so that a deployment
	// under its cap never emits it, and its presence in the scrape is itself
	// the signal that resolution was lost.
	overflow *V

	// collectMu guards the render scratch. Rendering is serialized by the
	// registry, but a Requests belonging to two registries would otherwise
	// race, and "you must only register it once" is a constraint nobody reads.
	collectMu sync.Mutex
	keys      []K
	vals      []*V
}

func newTable[K comparable, V any](family string, max int, init func(*V)) *table[K, V] {
	if max <= 0 {
		max = 1
	}
	return &table[K, V]{max: max, f: folder{family: family}, init: init, m: make(map[K]*V, 16)}
}

// get returns the value for k, folding into the overflow series once the table
// is at its cap. The bool reports whether the result is the folded series, so a
// caller can skip recording key-specific detail into it.
func (t *table[K, V]) get(k K) (*V, bool) {
	t.mu.RLock()
	v, ok := t.m[k]
	t.mu.RUnlock()
	if ok {
		return v, false
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if v, ok = t.m[k]; ok {
		return v, false
	}
	if len(t.m) >= t.max {
		t.f.fold()
		if t.overflow == nil {
			t.overflow = new(V)
			if t.init != nil {
				t.init(t.overflow)
			}
		}
		return t.overflow, true
	}
	v = new(V)
	if t.init != nil {
		t.init(v)
	}
	t.m[k] = v
	return v, false
}

// each calls fn for every live series in sorted key order, then for the
// overflow series if one exists.
//
// The read lock is held only long enough to copy the keys and the pointers;
// values are read outside it through their own atomics. Holding it across the
// whole render would make a large table's scrape a window during which no new
// series can be created — which is a stall on the observation path, and rule 4
// says a scrape may not cause one.
func (t *table[K, V]) each(less func(a, b K) bool, fn func(k K, v *V, overflow bool)) {
	t.collectMu.Lock()
	defer t.collectMu.Unlock()

	t.mu.RLock()
	t.keys = t.keys[:0]
	if cap(t.keys) < len(t.m) {
		t.keys = make([]K, 0, len(t.m)+8)
	}
	for k := range t.m {
		t.keys = append(t.keys, k)
	}
	vals := t.vals[:0]
	if cap(vals) < len(t.m) {
		vals = make([]*V, 0, len(t.m)+8)
	}
	over := t.overflow
	t.mu.RUnlock()

	sortSlice(t.keys, less)

	t.mu.RLock()
	for _, k := range t.keys {
		vals = append(vals, t.m[k])
	}
	t.mu.RUnlock()
	t.vals = vals

	for i, k := range t.keys {
		if vals[i] != nil {
			fn(k, vals[i], false)
		}
	}
	if over != nil {
		var zero K
		fn(zero, over, true)
	}
}

// raiseCap lifts the cap to at least n, and never lowers it.
//
// It exists so that a bound drawn from configuration can follow a reload. A cap
// fixed at construction is a cap that a legitimately grown configuration finds
// already spent, and the whole defect this table's admission rules were written
// against is "the table is spent and only a restart clears it".
//
// It never lowers because an entry that already exists cannot be withdrawn:
// lowering would leave live entries above the new cap, which is either a lie
// about the bound or a series that vanishes from the scrape. A series that
// vanishes and returns reads as a counter reset to `rate()`, which is a restart
// to every consumer downstream — see [Requests.SetAdmittedModels].
//
// It takes the write lock, which is also the lock [table.get] evaluates the cap
// under, so a concurrent first-observation sees one value or the other and
// never a torn one. It is called once per reload; nothing on the hot path
// reaches it.
func (t *table[K, V]) raiseCap(n int) {
	if n <= 0 {
		return
	}
	t.mu.Lock()
	if n > t.max {
		t.max = n
	}
	t.mu.Unlock()
}

// Len is the number of live series, excluding the overflow one.
func (t *table[K, V]) Len() int {
	t.mu.RLock()
	n := len(t.m)
	t.mu.RUnlock()
	return n
}

// sortSlice is an insertion-sort-backed sort that allocates nothing.
//
// sort.Slice takes a closure through reflect and allocates an interface header
// per call; sort.Sort needs an interface implementation per key type. The
// tables here are capped in the hundreds and are already sorted from the
// previous scrape, which is insertion sort's best case.
func sortSlice[T any](s []T, less func(a, b T) bool) {
	for i := 1; i < len(s); i++ {
		v := s[i]
		j := i - 1
		for j >= 0 && less(v, s[j]) {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = v
	}
}
