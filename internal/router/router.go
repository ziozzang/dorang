package router

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// QuotaSource answers whether a credential may serve a request now. It is the
// narrow view of internal/quota the router needs; the meters, the windows and
// the provider combination all live there (DESIGN §6).
type QuotaSource interface {
	// Check reports the credential's state. A credential the source has never
	// heard of must be allowed: an unmetered credential is unlimited, not
	// exhausted.
	Check(credential string, now time.Time) quota.Decision
}

// Meters adapts a set of per-credential meters to [QuotaSource].
type Meters map[string]*quota.Meter

// Check implements QuotaSource.
func (m Meters) Check(credential string, now time.Time) quota.Decision {
	if mt, ok := m[credential]; ok && mt != nil {
		return mt.Check(now)
	}
	return quota.Decision{Allow: true}
}

// Deps are the subsystems the router composes. Every one of them is optional
// except capacity and health, which are constructed with permissive defaults
// when omitted so that a notebook profile with no limits configured still runs
// the same code path as an enterprise one.
type Deps struct {
	// Capacity admits requests against every axis that constrains them (§5).
	Capacity *capacity.Broker
	// Health says which deployments are worth trying, and how fast they are.
	Health *health.Tracker
	// Prefix maps a conversation prefix to the backend that last served it
	// (§7.4b). Nil disables prefix affinity.
	Prefix *prefix.Table
	// Interner maps deployment ids to the integers the prefix table stores. It
	// MUST be the same interner that wrote the table: sharing a table without
	// sharing an interner routes on another router's ids.
	Interner *prefix.Interner
	// Pricing prices a candidate. Nil makes lowest_cost silent and leaves
	// Decision.Estimate zero.
	Pricing *pricing.Catalog
	// Catalog supplies a deployment's protocol family and declared context
	// window when configuration did not state them (§4.3).
	Catalog *catalog.Catalog
	// Quota refuses an exhausted credential. Nil allows every credential.
	Quota QuotaSource
	// Urgency scores a credential's expiring allowance for the quota_urgency
	// strategy (§7.5a(c)). Nil makes that strategy silent, which leaves the
	// rest of the chain deciding — the same treatment an unpriced candidate
	// gets from lowest_cost.
	Urgency UrgencySource
}

// UrgencySource scores how close a credential's resetting allowance is to being
// discarded. It is the narrow view of internal/quota's Ranker the router needs:
// the windows, the damping and the per-node jitter all live there.
type UrgencySource interface {
	// Urgency returns a non-negative score, higher meaning more urgent, and
	// zero for a credential with nothing resetting — which the router reads as
	// silence, not as "least urgent". occupancy is the candidate's current load
	// in [0,1] and damps the score so that every node does not converge on the
	// same credential at the window edge.
	Urgency(credential string, occupancy float64, now time.Time) float64
}

// Router resolves a client-facing model name to a deployment.
//
// A Router is immutable after [New] and safe for concurrent use. A hot reload
// builds a new one and swaps the pointer; in-flight requests keep the snapshot
// they started with (§4.1, §15.2).
type Router struct {
	cfg  Config
	deps Deps

	groups  map[string]*Group
	byID    map[string]*Deployment
	classes map[string][]*Group
	chains  map[Cause][]Target

	sticky *stickyStore
	now    func() time.Time
	rnd    func() float64
	rr     rrState
	rot    rotationState

	scratchPool sync.Pool
	evalPool    sync.Pool

	unpriced atomic.Int64
}

type scratch struct {
	cands []candidate
	deps  []*Deployment
	// creds accumulates the quota-filtered credential lists of ONE filter pass.
	// Each candidate is handed a subslice of it rather than the whole buffer,
	// because several candidates can need filtered lists in one pass and a
	// shared buffer would give every one of them the last one's contents —
	// silently reserving capacity on another deployment's axes.
	creds []capacity.Candidate
	// valid is the prefix-table predicate. It reads sc.cands rather than
	// closing over a per-request slice, so it is built once per scratch and
	// costs no allocation per request.
	valid func(uint32) bool
}

// New compiles a configuration into a Router.
//
// Compilation is where every cross-reference is resolved: an alias that names
// no group, a class member that does not exist, two deployments sharing an id.
// Those are configuration defects, and finding them here rather than on the
// first request is the difference between a startup failure and an outage.
func New(cfg Config, deps Deps) (*Router, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}
	if len(cfg.Strategy) == 0 {
		cfg.Strategy = DefaultStrategy()
	}
	if cfg.Priority.Classes == nil {
		cfg.Priority = DefaultPriority()
	}
	if cfg.Fallback.AuthCooldown == 0 {
		cfg.Fallback.AuthCooldown = DefaultAuthCooldown
	}
	if deps.Capacity == nil {
		// SweepInterval < 0 leaves no background goroutine, so a Router that
		// builds its own broker owns nothing it must later close.
		deps.Capacity = capacity.New(capacity.Config{SweepInterval: -1, Now: cfg.Now})
	}
	if deps.Health == nil {
		deps.Health = health.New(health.Options{Now: cfg.Now})
	}
	if deps.Interner == nil {
		deps.Interner = prefix.NewInterner()
	}

	r := &Router{
		cfg:     cfg,
		deps:    deps,
		groups:  make(map[string]*Group, len(cfg.Groups)),
		byID:    make(map[string]*Deployment),
		classes: make(map[string][]*Group),
		now:     cfg.Now,
		rnd:     cfg.Rand,
	}
	r.chains = cfg.Fallback.On
	if r.chains == nil {
		r.chains = DefaultChains()
	}
	if cfg.Sticky.Enabled {
		r.sticky = newStickyStore(cfg.Sticky.TTL, cfg.Now)
	}
	r.scratchPool.New = func() any {
		sc := new(scratch)
		sc.valid = func(t uint32) bool {
			for i := range sc.cands {
				if sc.cands[i].dep.interned == t {
					return true
				}
			}
			return false
		}
		return sc
	}
	if deps.Pricing != nil {
		r.evalPool.New = func() any { return deps.Pricing.NewEvaluator() }
	}

	// The Groups slice is copied wholesale so that the caller's slice may be
	// reused, and so that every *Deployment the router hands around points into
	// storage the router owns.
	groups := make([]Group, len(cfg.Groups))
	copy(groups, cfg.Groups)
	r.cfg.Groups = groups

	for gi := range groups {
		g := &groups[gi]
		if g.Name == "" {
			return nil, &Error{Status: 500, Code: "invalid_config", Message: "a model group has no name"}
		}
		if _, dup := r.groups[g.Name]; dup {
			return nil, &Error{Status: 500, Code: "invalid_config",
				Message: "duplicate model group " + g.Name}
		}
		deployments := make([]Deployment, len(g.Deployments))
		copy(deployments, g.Deployments)
		g.Deployments = deployments
		for di := range deployments {
			d := &deployments[di]
			if d.ID == "" {
				return nil, &Error{Status: 500, Code: "invalid_config",
					Message: "a deployment of " + g.Name + " has no id"}
			}
			if _, dup := r.byID[d.ID]; dup {
				return nil, &Error{Status: 500, Code: "invalid_config",
					Message: "duplicate deployment id " + d.ID}
			}
			if err := r.compile(d, g); err != nil {
				return nil, err
			}
			r.byID[d.ID] = d
		}
		r.groups[g.Name] = g
		if g.Class != "" {
			r.classes[g.Class] = append(r.classes[g.Class], g)
		}
	}

	for alias, target := range cfg.Aliases {
		if _, ok := r.groups[target]; !ok {
			return nil, &Error{Status: 500, Code: "invalid_config",
				Message: "alias " + alias + " names no model group"}
		}
		if _, clash := r.groups[alias]; clash {
			return nil, &Error{Status: 500, Code: "invalid_config",
				Message: "alias " + alias + " shadows a model group of the same name"}
		}
	}
	for class, members := range cfg.Classes {
		for _, name := range members {
			g, ok := r.groups[name]
			if !ok {
				return nil, &Error{Status: 500, Code: "invalid_config",
					Message: "class " + class + " names no model group " + name}
			}
			if !containsGroup(r.classes[class], g) {
				r.classes[class] = append(r.classes[class], g)
			}
		}
	}
	r.rr.current = make([]int64, len(r.byID))
	r.rot.cursor = make([]atomic.Uint64, len(r.byID))
	r.warmAxes()
	r.publishPrefixTTLs()
	return r, nil
}

// publishPrefixTTLs hands the affinity table each deployment's cache lifetime.
//
// It happens here, at compile time, because the router is the only thing that
// knows both the deployment id and the interned integer the table stores against
// it — and because a hot reload builds a new router against the SAME table, so
// this is also what stops a removed deployment's lifetime outliving it.
//
// A deployment with no configured lifetime contributes nothing and takes the
// table's default. That is the difference between "inherit" and "zero", and
// collapsing the two is how a per-backend setting silently becomes a global one.
func (r *Router) publishPrefixTTLs() {
	if r.deps.Prefix == nil {
		return
	}
	var max uint32
	any := false
	for _, d := range r.byID {
		if d.PrefixTTL == 0 {
			continue
		}
		any = true
		if d.interned > max {
			max = d.interned
		}
	}
	if !any {
		r.deps.Prefix.SetTargetTTLs(nil)
		return
	}
	ttls := make([]time.Duration, max+1)
	for _, d := range r.byID {
		if d.PrefixTTL != 0 {
			ttls[d.interned] = d.PrefixTTL
		}
	}
	r.deps.Prefix.SetTargetTTLs(ttls)
}

// warmAxes commits and immediately releases one reservation per credential, so
// that every axis with a configured ceiling exists in the broker before the
// first request.
//
// It compensates for a lazy-creation artefact: internal/capacity creates a
// bucket on first commit and reports an axis it has never seen as unknown, so
// "unknown" conflates "this axis is unlimited and therefore not counted" with
// "this axis is counted but has not been used yet". least_busy reads that bit
// to decide whether it has an opinion at all, and without the warm-up it could
// only ever move traffic AWAY from a busy deployment, never TO an idle one that
// has not served a request yet — which is precisely the case a cold start is.
//
// Axes with no ceiling are excluded by the broker itself, so after this the bit
// means what least_busy needs it to mean.
func (r *Router) warmAxes() {
	for _, d := range r.byID {
		for i := range d.caps {
			res, ok := r.deps.Capacity.TryAcquire(capacity.Request{
				Provider:      d.Provider,
				Model:         d.UpstreamModel,
				ProviderGroup: d.ProviderGroup,
				Candidates:    d.caps[i : i+1],
			})
			if ok {
				res.Release()
			}
		}
	}
}

func containsGroup(gs []*Group, g *Group) bool {
	for _, x := range gs {
		if x == g {
			return true
		}
	}
	return false
}

// compile fills a deployment's derived fields once, at load, so the hot path
// reads them instead of deriving them per request.
func (r *Router) compile(d *Deployment, g *Group) error {
	d.ridx = len(r.byID)
	d.gname, d.gclass = g.Name, g.Class
	d.interned = r.deps.Interner.ID(d.ID)

	if r.deps.Catalog != nil {
		if d.Family == "" {
			if kd, ok := r.deps.Catalog.Kind(d.Kind); ok {
				d.Family = string(kd.API)
			}
		}
		if d.ContextWindow == 0 || d.MaxOutputTokens == 0 {
			// The model name goes in byte-identical and comes back
			// byte-identical: pkg/catalog never splits it (§2.1).
			mi := r.deps.Catalog.Model(d.Kind, d.UpstreamModel)
			if d.ContextWindow == 0 {
				d.ContextWindow = mi.ContextWindow
			}
			if d.MaxOutputTokens == 0 {
				d.MaxOutputTokens = mi.MaxOutputTokens
			}
		}
	}
	if d.Family == "" {
		// A deployment with no family is its own family. Falling back to a
		// shared empty family would let opaque state cross between two backends
		// that merely both failed to declare one, which is the exact silent
		// crossing §B forbids.
		d.Family = d.Kind + "/" + d.Provider
	}

	d.caps = make([]capacity.Candidate, len(d.Credentials))
	d.credIndex = make(map[string]int, len(d.Credentials))
	for i, c := range d.Credentials {
		if c.ID == "" {
			return &Error{Status: 500, Code: "invalid_config",
				Message: "a credential of deployment " + d.ID + " has no id"}
		}
		d.caps[i] = capacity.Candidate{
			ID:            c.ID,
			CapacityGroup: c.CapacityGroup,
			MaxConcurrent: c.MaxConcurrent,
			Provider:      d.Provider,
			UpstreamModel: d.UpstreamModel,
			ProviderGroup: d.ProviderGroup,
		}
		d.credIndex[c.ID] = i
	}
	return nil
}

// Groups returns the compiled group names. Diagnostics only.
func (r *Router) Groups() []string {
	out := make([]string, 0, len(r.groups))
	for n := range r.groups {
		out = append(out, n)
	}
	return out
}

// Unpriced reports how many candidates have been priced by no marginal rule.
// §8.3 requires an unpriced model to increment a counter and warn rather than
// cost zero in silence; this is that counter for the routing path.
func (r *Router) Unpriced() int64 { return r.unpriced.Load() }

// Purge drops expired session pins. It is the periodic sweep behind
// routing.sticky.purge_interval and returns how many entries it removed.
func (r *Router) Purge() int { return r.sticky.purge() }

// Resolve maps a client-facing name through the alias table to a model group
// name. A name that is not an alias is returned unchanged. Aliases do not
// chain, and nothing splits the name (§2.1, §7.2).
func (r *Router) Resolve(name string) (string, bool) {
	if t, ok := r.cfg.Aliases[name]; ok {
		name = t
	}
	_, ok := r.groups[name]
	return name, ok
}

func (r *Router) group(name string) (*Group, bool) {
	if t, ok := r.cfg.Aliases[name]; ok {
		name = t
	}
	g, ok := r.groups[name]
	return g, ok
}

// Route chooses where one attempt goes.
//
// The pipeline is §7.1's: resolve the alias, resolve the group, filter, order
// by the strategy chain, then try to acquire capacity walking the ranked list
// and moving to the next candidate on refusal. On success the returned decision
// holds a live capacity reservation, which [Router.Report] releases.
//
// Set [Request.Previous] to continue an existing routing session after a failed
// attempt that has already been reported. That is the fail-back path of §7.6:
// the cause recorded by Report selects the chain, the hop and wall-clock budgets
// are enforced, already-tried deployments are excluded, and a stream that has
// already sent its first byte refuses to hop at all.
func (r *Router) Route(ctx context.Context, req Request) (*Decision, error) {
	now := r.now()

	var st *sessionState
	if req.Previous != nil {
		st = req.Previous.st
	}
	if st == nil {
		st = &sessionState{started: now}
	}

	var hopCause Cause
	if req.Previous != nil {
		if err := r.canFallBack(st, now); err != nil {
			return nil, err
		}
		hopCause = st.cause
	}

	g, ok := r.group(req.Model)
	if !ok {
		return nil, &Error{Status: 404, Code: CodeModelNotFound, Attempt: st.attempts + 1,
			Message: "no model group or alias named " + req.Model, terminal: true}
	}

	chain := g.Strategy
	if len(chain) == 0 {
		chain = r.cfg.Strategy
	}

	sc := r.scratchPool.Get().(*scratch)
	defer func() {
		sc.cands = sc.cands[:0]
		sc.deps = sc.deps[:0]
		sc.creds = sc.creds[:0]
		r.scratchPool.Put(sc)
	}()

	sc.deps = r.universe(sc.deps[:0], g, st, hopCause)
	if len(sc.deps) == 0 && hopCause == CauseContextWindow {
		// §10.5a's third row: the request fits nowhere in the class. Failing is
		// the correct outcome, and the caller learns the real limit — which is
		// information they did not have and cannot get elsewhere.
		return nil, &Error{Status: 400, Code: CodeContextWindow, Cause: CauseContextWindow,
			Attempt: st.attempts + 1, terminal: true,
			Message: "no deployment of class " + g.Class + " has a context window larger than " +
				strconv.Itoa(st.lastWindow) + " tokens; dorang does not compact to make it fit"}
	}

	pin := resolvePins(req.Pins)
	key := r.stickyKeyFor(&req, g)
	sticky, hasSticky := r.sticky.get(key)

	cands, rerr := r.filter(sc, &req, st, pin, sticky, hasSticky, now)
	if rerr != nil {
		rerr.Attempt = st.attempts + 1
		if rerr.Cause == CauseNone {
			// The refusal classifies itself when it can — an exhausted quota is
			// a quota_exhausted whether it was found on hop one or hop three.
			// Only an unclassified refusal inherits the hop's cause.
			rerr.Cause = hopCause
		}
		return nil, rerr
	}

	r.score(sc, cands, chain, &req, g, pin, sticky, hasSticky, now)
	order(chain, cands)

	d, err := r.dispatch(ctx, cands, chain, &req, st, g, pin, now)
	if err != nil {
		// A refusal on a hop carries the cause that got it here, so a caller
		// reading only the error still knows what chain it was walking.
		var re *Error
		if hopCause != CauseNone && errors.As(err, &re) && re.Cause == CauseNone {
			re.Cause = hopCause
		}
		return nil, err
	}
	if hopCause != CauseNone {
		d.Reason = fallbackReason(hopCause)
	}
	st.sticky, st.stickySet = key, !key.zero()
	st.digests = req.Digests
	st.lastWindow = r.byID[d.Deployment].ContextWindow
	st.attempts = d.Attempt
	st.tried = append(st.tried, d.Deployment)
	st.reported = false
	d.st = st
	return d, nil
}

// canFallBack enforces every boundary a fail-back has (§7.6).
func (r *Router) canFallBack(st *sessionState, now time.Time) *Error {
	attempt := st.attempts + 1
	if !st.reported {
		return &Error{Status: 500, Code: "attempt_not_reported", Attempt: attempt, terminal: true,
			Message: "the previous decision was not passed to Report, so its outcome is unknown"}
	}
	if !st.failed {
		return &Error{Status: 500, Code: "attempt_succeeded", Attempt: attempt, terminal: true,
			Message: "the previous attempt succeeded; there is nothing to fall back from"}
	}
	// The streaming boundary. Once a byte has reached the client the response
	// is committed: a second attempt would duplicate output, and duplicated
	// output is worse than a visible failure.
	if st.firstByte {
		return &Error{Status: 500, Code: CodeStreamCommitted, Cause: st.cause, Attempt: attempt,
			terminal: true,
			Message:  "the stream already sent its first byte; fail-back is closed"}
	}
	if !st.cause.Chainable() {
		return &Error{Status: statusFor(st.cause), Code: CodeNotChainable, Cause: st.cause,
			Attempt: attempt, terminal: true, ResetAt: st.resetAt,
			Message: st.cause.String() + " is not a fallback condition; failing is the correct outcome"}
	}
	if len(r.chains[st.cause]) == 0 {
		return &Error{Status: statusFor(st.cause), Code: CodeNotChainable, Cause: st.cause,
			Attempt: attempt, terminal: true, ResetAt: st.resetAt,
			Message: "no fallback chain is configured for " + st.cause.String()}
	}
	if r.cfg.Fallback.MaxHops <= 0 || st.attempts > r.cfg.Fallback.MaxHops {
		return &Error{Status: statusFor(st.cause), Code: CodeHopsExhausted, Cause: st.cause,
			Attempt: attempt, terminal: true, ResetAt: st.resetAt,
			Message: "the fallback hop budget is spent"}
	}
	if b := r.cfg.Fallback.Budget; b > 0 && !now.Before(st.started.Add(b)) {
		return &Error{Status: statusFor(st.cause), Code: CodeBudgetElapsed, Cause: st.cause,
			Attempt: attempt, terminal: true, ResetAt: st.resetAt,
			Message: "the fallback wall-clock budget is spent"}
	}
	return nil
}

func statusFor(c Cause) int {
	switch c {
	case CauseRateLimit, CauseQuotaExhausted:
		return 429
	case CauseAuth:
		return 401
	case CauseBudgetExceeded:
		return 402
	case CauseContextWindow, CauseContentPolicy, CauseBadRequest:
		return 400
	case CauseTimeout:
		return 504
	}
	return 503
}

// universe is the candidate set for this attempt, before filtering.
//
// The first attempt sees the resolved group. A fail-back hop sees whatever the
// cause's chain names at that hop, with the last entry repeating once the chain
// is shorter than the hop count — same_class is a superset of same_group, so a
// widening chain never loses a candidate the earlier target offered.
func (r *Router) universe(out []*Deployment, g *Group, st *sessionState, cause Cause) []*Deployment {
	if cause == CauseNone {
		for i := range g.Deployments {
			out = append(out, &g.Deployments[i])
		}
		return out
	}
	chain := r.chains[cause]
	hop := st.attempts - 1
	if hop >= len(chain) {
		hop = len(chain) - 1
	}
	if hop < 0 {
		hop = 0
	}
	switch chain[hop] {
	case TargetSameGroup:
		for i := range g.Deployments {
			out = append(out, &g.Deployments[i])
		}
	case TargetSameClass, TargetSameClassLarger:
		// An undeclared context window (zero) never qualifies as "larger".
		// §4.3 states the asymmetry: a window that is too large makes requests
		// fail outright with context-window fallback blind to them, while one
		// that is too small only costs an unnecessary route. Treating "we do
		// not know" as "big enough" is the expensive direction.
		larger := chain[hop] == TargetSameClassLarger
		members := r.classes[g.Class]
		if g.Class == "" {
			members = nil
			out = appendLarger(out, g.Deployments, larger, st.lastWindow)
		}
		for _, m := range members {
			out = appendLarger(out, m.Deployments, larger, st.lastWindow)
		}
	}
	return out
}

func appendLarger(out []*Deployment, ds []Deployment, larger bool, floor int) []*Deployment {
	for i := range ds {
		if larger && (ds[i].ContextWindow == 0 || ds[i].ContextWindow <= floor) {
			continue
		}
		out = append(out, &ds[i])
	}
	return out
}

// pins is the resolved constraint set for one request.
//
// The two strengths are kept in separate fields rather than in one field with a
// flag, because they are consumed at different stages: a hard pin is a FILTER
// whose failure is terminal, a soft one is only a preference the ranking reads.
// Collapsing them is how a cache hint quietly becomes a refusal.
type pins struct {
	// Hard constraints. Nothing outside them may serve the request.
	family     string
	credential string
	deployment string
	// Soft preferences. They bias ranking and the credential choice, and spill
	// freely when the preferred target is busy.
	softCredential string
	softDeployment string

	kind string
	hard bool
}

// resolvePins folds the request's pins into one constraint set. A Pinned entry
// on an axis makes that axis a filter; a Preferred entry leaves it a
// preference. A correctness constraint is never negotiated against a cache
// preference, so the two never merge.
func resolvePins(in []Pin) pins {
	var p pins
	for _, x := range in {
		if x.Strength == Pinned {
			if x.Family != "" && p.family == "" {
				p.family = x.Family
			}
			if x.Credential != "" && p.credential == "" {
				p.credential = x.Credential
			}
			if x.Deployment != "" && p.deployment == "" {
				p.deployment = x.Deployment
			}
			if !p.hard {
				p.hard, p.kind = true, x.Kind
			}
			continue
		}
		if x.Credential != "" && p.softCredential == "" {
			p.softCredential = x.Credential
		}
		if x.Deployment != "" && p.softDeployment == "" {
			p.softDeployment = x.Deployment
		}
	}
	return p
}

func (r *Router) stickyKeyFor(req *Request, g *Group) stickyKey {
	if r.sticky == nil || req.Session == "" {
		return stickyKey{}
	}
	return stickyKey{tenant: req.Tenant, group: g.Name, session: req.Session}
}

// filter removes every candidate that cannot serve this request, and explains
// the refusal when none is left.
//
// Health is NOT applied here, despite §7.1 listing it in the filter stage. A
// circuit-breaker probe is a consumable: health.Allow transitions an open
// circuit to half-open and spends the single probe slot, so evaluating it for
// candidates that are then never dispatched leaks probes and delays recovery
// exactly when recovery matters. It is applied once per candidate in the
// try-acquire walk instead, which reaches the same target set.
func (r *Router) filter(sc *scratch, req *Request, st *sessionState, p pins,
	sticky stickyEntry, hasSticky bool, now time.Time) ([]candidate, *Error) {

	need := req.Required.Structural() &^ req.AllowLossy

	var (
		union      canonical.Capability
		nFamily    int
		nCap       int
		nCtx       int
		nQuota     int
		nTried     int
		largest    int
		needed     int64
		quotaReset time.Time
		quotaCred  string
	)

	cands := sc.cands[:0]
	for _, d := range sc.deps {
		if containsString(st.tried, d.ID) {
			nTried++
			continue
		}
		if p.deployment != "" && d.ID != p.deployment {
			nFamily++
			continue
		}
		// The family pin. Crossing it cannot succeed: the receiving family
		// classifies foreign opaque state as never-retryable, so every hop
		// after this one burns to reach the identical 400 (EXTENSIONS §B.3).
		if p.family != "" && d.Family != p.family {
			nFamily++
			continue
		}
		union |= d.Capabilities
		if !d.Capabilities.Has(need) {
			nCap++
			continue
		}
		// The fit check is per deployment, because its output half is: a caller
		// that named no max_tokens still reserves whatever THIS deployment
		// generates by default, and charging that request nothing for output is
		// how a prompt at 99% of the window is admitted and then overflows on the
		// first generated token. Deployment.MaxOutputTokens is the declared
		// ceiling, and this is the only place on the routing path that reads it.
		total := req.InputTokens + outputReserve(req.MaxOutputTokens, d.MaxOutputTokens, d.ContextWindow)
		if d.ContextWindow > 0 && total > int64(d.ContextWindow) {
			nCtx++
			// The largest window that still did not fit is the real limit the
			// caller has to get under, and it is the number they cannot obtain
			// anywhere else (§10.5a). The demand recorded beside it is the one
			// measured against THAT window, so the two numbers in the refusal are
			// the two sides of one comparison rather than of two different ones.
			if d.ContextWindow > largest {
				largest, needed = d.ContextWindow, total
			}
			continue
		}

		creds, cred, ok := r.eligible(sc, d, p, sticky, hasSticky, now, &quotaReset, &quotaCred)
		if !ok {
			nQuota++
			continue
		}
		cands = append(cands, candidate{
			dep:       d,
			idx:       len(cands),
			creds:     creds,
			preferred: cred,
			pinned:    p.hard && p.credential != "",
			rr:        -1,
		})
	}
	sc.cands = cands
	if len(cands) > 0 {
		return cands, nil
	}

	// Nothing is left. The order below is the order of how specific the reason
	// is: a pin refusal says more than "no capacity", and a named construct
	// says more than "no candidate".
	if p.hard {
		e := &Error{Status: 400, Code: CodeStatePinUnroutable, Pin: p.kind, Family: p.family,
			Credential: p.credential, terminal: true,
			Message: "no deployment can accept the opaque state this conversation carries"}
		if p.credential != "" {
			e.Code = CodeCredentialPinLost
			e.Message = "the conversation is pinned to account " + p.credential +
				", which cannot serve it; another account would not be a worse choice but a wrong one"
			if !quotaReset.IsZero() && quotaCred == p.credential {
				e.Code, e.Status = CodeCredentialExhausted, 429
				e.ResetAt = quotaReset
				e.Message = "the quota of pinned account " + p.credential + " is exhausted"
			}
		}
		return nil, e
	}
	switch {
	case nCap > 0 && nFamily == 0 && nCtx == 0 && nQuota == 0:
		return nil, &Error{Status: 400, Code: CodeUnsupportedConstruct, terminal: true,
			Constructs: union.Missing(need).Names(),
			Message:    "no deployment of this model can express the request; it is not downgraded silently"}
	case nCtx > 0 && nQuota == 0 && nCap == 0:
		return nil, &Error{Status: 400, Code: CodeContextWindow, Cause: CauseContextWindow,
			Estimated: !req.InputTokensExact, EstimateMethod: req.InputTokensMethod,
			Message: sizeVerb(req.InputTokensExact) + strconv.FormatInt(needed, 10) +
				" tokens including the reserved output" +
				methodNote(req.InputTokensExact, req.InputTokensMethod) +
				" and the largest context window behind this model is " +
				strconv.Itoa(largest) + "; dorang does not compact to make it fit"}
	case nQuota > 0:
		return nil, &Error{Status: 429, Code: CodeQuotaExhausted, Cause: CauseQuotaExhausted,
			ResetAt: quotaReset, Credential: quotaCred,
			Message: "every credential of this model is out of quota"}
	case nTried > 0:
		return nil, &Error{Status: 503, Code: CodeFallbackExhausted, terminal: true,
			Message: "every candidate has already been tried"}
	}
	// 429, not 503: COMPATIBILITY §11.2's "No healthy deployment" row and the
	// reasoning printed under it. A 503 tells an SDK the gateway is down and
	// stops its retry loop; a 429 is the back-pressure this actually is.
	return nil, &Error{Status: 429, Code: CodeNoCandidate,
		Message: "no deployment is eligible for this request"}
}

// defaultOutputShare bounds how much of a window an UNREQUESTED generation may
// reserve: at most one quarter of it.
//
// The bound exists because the catalogued ceilings are enormous relative to the
// windows they sit in — 200,000 context against 100,000 max output is an
// ordinary reasoning-model entry, and 1,000,000 against 128,000 an ordinary
// frontier one. Reserving the whole ceiling for a caller who asked for nothing
// would refuse a 150,000-token prompt on a 200,000-token model, which is a
// request that routes and completes today. That refusal would be the same class
// of defect this fit check exists to fix, pointed the other way.
const defaultOutputShare = 4

// outputReserve is how much of a window this attempt has to leave for the
// answer.
//
// max_tokens is optional across the OpenAI family, so "the caller named none"
// is the common case and not an edge one. Reserving zero for it makes the fit
// check a statement about the prompt alone, which is not the question the window
// asks: it has to hold the prompt AND the generation, and a prompt admitted at
// 99% of the window overflows on the first token generated.
//
// A caller who named a ceiling gets exactly it — that is a number they chose and
// dorang does not second-guess it. A caller who named none gets the deployment's
// declared ceiling, bounded by [defaultOutputShare]. An undeclared ceiling
// reserves nothing, because inventing a number would refuse requests against a
// limit the operator never set.
func outputReserve(asked int64, declared, window int) int64 {
	if asked > 0 {
		return asked
	}
	if declared <= 0 {
		return 0
	}
	if limit := int64(window) / defaultOutputShare; window > 0 && int64(declared) > limit {
		return limit
	}
	return int64(declared)
}

// sizeVerb opens the context-window refusal by saying whether the number that
// follows was measured or guessed.
//
// It is a table rather than a format verb (§15.5), and it exists because a
// client told "needs 900,000 tokens" for a photograph has no way to tell that
// from a measurement — and therefore no way to know that the right response is
// to report it rather than to shrink the conversation.
func sizeVerb(exact bool) string {
	if exact {
		return "the request needs "
	}
	return "the request is estimated at "
}

// methodNote names the rule the size came from, so that "estimated" is
// actionable rather than merely honest: a caller who knows the count came from
// the byte fallback knows it can be off by a lot, and one who sees the
// structural count knows their images were counted as images.
func methodNote(exact bool, method string) string {
	if exact || method == "" {
		return ""
	}
	return " (estimate method: " + method + ")"
}

// eligible returns the credentials of a deployment that may serve, the
// preferred one among them, and whether any remain.
func (r *Router) eligible(sc *scratch, d *Deployment, p pins, sticky stickyEntry, hasSticky bool,
	now time.Time, reset *time.Time, resetCred *string) ([]capacity.Candidate, string, bool) {

	// A credential pin narrows the pool to one account before quota is even
	// consulted, because the alternative accounts are not alternatives.
	//
	// The one-element slice is cut from THIS deployment's own precomputed
	// array, never from a buffer shared with the other candidates: a shared
	// one-element buffer would hand every pinned candidate the last one's
	// provider and model, and the reservation would land on the wrong axes.
	if p.credential != "" {
		i, ok := d.credIndex[p.credential]
		if !ok {
			return nil, "", false
		}
		if !r.quotaOK(p.credential, now, reset, resetCred) {
			return nil, "", false
		}
		return d.caps[i : i+1], p.credential, true
	}

	all := true
	for _, c := range d.Credentials {
		if !r.quotaOK(c.ID, now, reset, resetCred) {
			all = false
			break
		}
	}
	var creds []capacity.Candidate
	if all {
		creds = d.caps // the common case allocates nothing
	} else {
		start := len(sc.creds)
		for i, c := range d.Credentials {
			if r.quotaOK(c.ID, now, reset, resetCred) {
				sc.creds = append(sc.creds, d.caps[i])
			}
		}
		creds = sc.creds[start:len(sc.creds):len(sc.creds)]
	}
	if len(creds) == 0 && len(d.Credentials) > 0 {
		return nil, "", false
	}

	// The preferred credential is a hint to internal/capacity, which tries it
	// first and then spills. A soft pin outranks a session pin here because it
	// came from this request rather than from a memory of an earlier one.
	pref := ""
	if hasSticky && sticky.deployment == d.ID {
		if _, ok := d.credIndex[sticky.credential]; ok {
			pref = sticky.credential
		}
	}
	if p.softCredential != "" {
		if _, ok := d.credIndex[p.softCredential]; ok {
			pref = p.softCredential
		}
	}
	// key_rotation.strategy, last: a pin and a sticky entry are statements about
	// THIS conversation and outrank a statement about load. Before this the
	// strategy was validated against four names and then every one of them
	// behaved as failover, because nothing outside internal/config ever read it.
	if pref == "" && r.cfg.Rotation != "" && r.cfg.Rotation != RotationFailover {
		pref = r.rotate(d, creds)
	}
	return creds, pref, true
}

func (r *Router) quotaOK(cred string, now time.Time, reset *time.Time, resetCred *string) bool {
	if r.deps.Quota == nil {
		return true
	}
	dec := r.deps.Quota.Check(cred, now)
	if dec.Allow {
		return true
	}
	if reset.IsZero() || (!dec.ResetAt.IsZero() && dec.ResetAt.Before(*reset)) {
		*reset = dec.ResetAt
		*resetCred = cred
	}
	return false
}

// score computes every input the strategy chain will read, once per candidate.
// Sources that take a lock or allocate are consulted only when a strategy in
// the chain actually reads them.
func (r *Router) score(sc *scratch, cands []candidate, chain []Strategy, req *Request, g *Group,
	pin pins, sticky stickyEntry, hasSticky bool, now time.Time) {

	if len(cands) == 0 {
		return
	}
	if chainUses(chain, StrategyPrefixSticky) && r.deps.Prefix != nil && len(req.Digests) > 0 {
		target, depth, ok := r.deps.Prefix.Lookup(req.Digests, sc.valid)
		if ok {
			for i := range cands {
				if cands[i].dep.interned == target {
					cands[i].prefixHit = true
					cands[i].prefixDepth = depth
					break
				}
			}
		}
	}
	// A session pin binds a deployment and a credential together, because
	// §7.4a2 makes the account the unit conversation state attaches to. A soft
	// deployment pin from this request is the same kind of preference.
	if hasSticky || pin.softDeployment != "" {
		for i := range cands {
			cands[i].sticky = cands[i].dep.ID == sticky.deployment ||
				cands[i].dep.ID == pin.softDeployment
		}
	}
	if chainUses(chain, StrategyLowestCost) && r.deps.Pricing != nil {
		ev := r.evalPool.Get().(*pricing.Evaluator)
		ev.Reset()
		for i := range cands {
			pr := r.priceRequest(&cands[i], req, now)
			cost, err := ev.Price(pr)
			if err != nil || cost.Missing {
				if err == nil {
					r.unpriced.Add(1)
				}
				continue
			}
			cands[i].cost, cands[i].costCred = cost, pr.Credential
			cands[i].costNano = cost.MarginalNano
			cands[i].costKnown = true
		}
		r.evalPool.Put(ev)
	}
	if chainUses(chain, StrategyLeastBusy) {
		for i := range cands {
			cands[i].busy, cands[i].busyKnown = r.busy(cands[i].dep)
		}
	}
	if chainUses(chain, StrategyLowestLatency) {
		for i := range cands {
			cands[i].ttft = r.deps.Health.TTFT(cands[i].dep.ID)
		}
	}
	if chainUses(chain, StrategyHighestTPS) {
		for i := range cands {
			cands[i].tps = r.deps.Health.TokensPerSec(cands[i].dep.ID)
		}
	}
	if chainUses(chain, StrategyQuotaUrgency) && r.deps.Urgency != nil {
		for i := range cands {
			c := &cands[i]
			// The signal is per CREDENTIAL and a candidate is a deployment, so
			// it is scored against the credential that would actually serve —
			// the same one lowest_cost prices, for the same reason: scoring a
			// candidate on an account it will not use is scoring the wrong
			// thing.
			cred := c.preferred
			if cred == "" && len(c.creds) > 0 {
				cred = c.creds[0].ID
			}
			if cred == "" {
				continue
			}
			// Occupancy damps the score (§7.5a(c) constraint 3). It is the
			// least_busy term, computed here rather than reused, because a
			// chain can name quota_urgency without naming least_busy.
			occ := 0.0
			busy, known := c.busy, c.busyKnown
			if !known {
				busy, known = r.busy(c.dep)
			}
			if known && busy > 0 {
				occ = occupancy(busy, c.creds)
			}
			if u := r.deps.Urgency.Urgency(cred, occ, now); u > 0 {
				c.urgency, c.urgencyKnown = u, true
			}
		}
	}
	if chainUses(chain, StrategyRoundRobin) {
		r.assignRR(cands)
	}
	if chainUses(chain, StrategyWeightedRandom) {
		assignWeightedRandom(cands, r.rnd)
	}
}

// occupancy renders a busy count as the [0,1] fraction the urgency damper
// wants. The denominator is the pool's own ceiling; with no ceiling configured
// there is no fraction to compute and the damper is left off rather than fed a
// number invented from nothing.
func occupancy(busy int, creds []capacity.Candidate) float64 {
	limit := 0
	for i := range creds {
		if creds[i].MaxConcurrent > limit {
			limit = creds[i].MaxConcurrent
		}
	}
	if limit <= 0 {
		return 0
	}
	if busy >= limit {
		return 1
	}
	return float64(busy) / float64(limit)
}

// busy reads a deployment's occupancy from the capacity broker.
//
// An axis with no configured ceiling is not counted at all, so the broker
// reports it as unknown. Reading that as "zero in use" would make every
// unlimited deployment permanently the idlest thing in the group — a backend
// winning least_busy on the strength of nobody measuring it.
func (r *Router) busy(d *Deployment) (int, bool) {
	if n, ok := r.deps.Capacity.InUse(capacity.AxisModel, d.Provider, d.UpstreamModel); ok {
		return n, true
	}
	if n, ok := r.deps.Capacity.InUse(capacity.AxisRoute, d.Provider, ""); ok {
		return n, true
	}
	return 0, false
}

func (r *Router) priceRequest(c *candidate, req *Request, now time.Time) pricing.Request {
	cred := c.preferred
	if cred == "" && len(c.creds) > 0 {
		cred = c.creds[0].ID
	}
	return pricing.Request{
		Provider:     c.dep.Provider,
		Model:        c.dep.UpstreamModel,
		Credential:   cred,
		Deployment:   c.dep.ID,
		InputTokens:  req.InputTokens,
		OutputTokens: req.MaxOutputTokens,
		Requests:     1,
		At:           now,
	}
}

// dispatch walks the ranked candidates and takes the first reservation it can.
func (r *Router) dispatch(ctx context.Context, cands []candidate, chain []Strategy,
	req *Request, st *sessionState, g *Group, p pins, now time.Time) (*Decision, error) {

	var lastErr error
	var unhealthy, saturated int
	for i := range cands {
		c := &cands[i]
		// Health is consulted here rather than in the filter, and exactly once
		// per candidate, because Allow spends a half-open probe.
		if !r.deps.Health.Allow(c.dep.ID) {
			unhealthy++
			continue
		}
		res, err := r.acquire(ctx, req, c)
		if err != nil {
			lastErr = err
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				break
			}
			continue
		}
		if res == nil {
			saturated++
			continue
		}
		return r.decide(c, chain, cands, req, st, g, p, res, now), nil
	}

	// The refusal distinguishes "everything is busy" from "everything is down",
	// because those call for opposite responses from whoever reads it.
	if p.hard && p.credential != "" {
		e := &Error{Status: 503, Code: CodeCredentialSaturated, Attempt: st.attempts + 1,
			Credential: p.credential, Pin: p.kind, terminal: true,
			Message: "the account this conversation is pinned to has no room, and spilling to " +
				"another account would break the conversation rather than slow it"}
		if saturated == 0 && unhealthy > 0 {
			e.Code = CodeCredentialPinLost
			e.Message = "every deployment that can serve the pinned account " + p.credential +
				" is out of service; another account would be a wrong choice, not a slower one"
		}
		return nil, e
	}
	// Both spellings are 429 by §11.2 — "Capacity wait timed out" and "No
	// healthy deployment" are the same row's two neighbours, and both are
	// conditions a client should back off from rather than give up on.
	e := &Error{Status: 429, Code: CodeNoCapacity, Attempt: st.attempts + 1,
		Message: "no candidate of this model had room"}
	if saturated == 0 && unhealthy > 0 {
		e.Code = CodeNoCandidate
		e.Message = "every candidate of this model is out of service"
	}
	if unhealthy == 0 && saturated == 0 && lastErr != nil {
		e.Message = lastErr.Error()
	}
	return nil, e
}

func (r *Router) acquire(ctx context.Context, req *Request, c *candidate) (*capacity.Reservation, error) {
	cr := capacity.Request{
		Provider:      c.dep.Provider,
		Model:         c.dep.UpstreamModel,
		ProviderGroup: c.dep.ProviderGroup,
		PrincipalID:   req.Principal,
		PrincipalMax:  req.PrincipalMax,
		Candidates:    c.creds,
		Preferred:     c.preferred,
		OnCapacity:    r.cfg.OnCapacity,
		Batch:         req.Batch,
		TTL:           c.dep.Timeout,
	}
	if c.pinned {
		// Spill must not apply to a pinned request (§7.4a2). The candidate list
		// already holds only the pinned account; Wait states the intent so a
		// future change to that list cannot quietly re-enable spilling.
		cr.OnCapacity = capacity.Wait
		// A pinned request is the one that genuinely has to wait: it may not
		// spill to another account, so the alternative to waiting is failing.
		// The budget comes from the principal's max_queue_wait when the router
		// has no tighter one of its own, which is what gives that setting a
		// consumer on the interactive path. An UNPINNED request still never
		// queues — it spills or falls back (§7.4a2, §7.6), and queueing it
		// instead would trade a fast hop onto a healthy backend for a slow wait
		// on a saturated one.
		wait := r.cfg.PinnedWait
		if wait <= 0 {
			wait = r.deps.Capacity.WaitBudget(req.Principal)
		}
		if wait > 0 {
			wctx, cancel := context.WithTimeout(ctx, wait)
			defer cancel()
			res, err := r.deps.Capacity.Acquire(wctx, cr)
			if err != nil {
				switch {
				case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil,
					errors.Is(err, capacity.ErrQueueTimeout),
					errors.Is(err, capacity.ErrQueueFull):
					// Waited out the budget, or was never admitted to the queue.
					// Both are refusals of this candidate, not of the request.
					return nil, nil
				}
				return nil, err
			}
			return res, nil
		}
	}
	res, ok := r.deps.Capacity.TryAcquire(cr)
	if !ok {
		return nil, nil
	}
	return res, nil
}

func (r *Router) decide(c *candidate, chain []Strategy, cands []candidate, req *Request,
	st *sessionState, g *Group, p pins, res *capacity.Reservation, now time.Time) *Decision {

	d := &Decision{
		Deployment:    c.dep.ID,
		Provider:      c.dep.Provider,
		Credential:    res.CredentialID(),
		UpstreamModel: c.dep.UpstreamModel,
		Reservation:   res,
		Attempt:       st.attempts + 1,
		Group:         c.dep.gname,
		Class:         c.dep.gclass,
		Kind:          c.dep.Kind,
		Family:        c.dep.Family,
		Capabilities:  c.dep.Capabilities,
		PrefixDepth:   c.prefixDepth,
		Stream:        req.Stream,
		interned:      c.dep.interned,
	}
	if d.Credential == "" {
		d.Credential = c.preferred
	}
	if p.hard {
		d.PinnedTo = p.kind
	}

	// Priority, normalized for THIS engine. The canonical value is kept beside
	// it so a hop onto a different engine re-normalizes from the class rather
	// than negating an already-negated number.
	d.CanonicalPriority = r.cfg.Priority.CanonicalFor(req.Principal, req.PriorityClass, req.PriorityHint)
	d.PriorityHintDropped = req.PriorityHint != nil && !r.cfg.Priority.GrantsHint(req.Principal)
	if v, field, ok := r.cfg.Priority.Wire(c.dep.Kind, d.CanonicalPriority); ok {
		d.Priority, d.PriorityField = v, field
		d.PriorityUnverified = !c.dep.PriorityVerified
	} else {
		d.Priority = d.CanonicalPriority
	}
	d.PriorityTier = r.cfg.Priority.Tier(c.dep.Kind, req.PriorityClass)

	// Droppable losses are reported, never fatal: the request still means what
	// it meant (§10.1).
	//
	// Suppression is the other term, and it is not a capability gap — see
	// [Deployment.Suppressed]. A self-hosted engine's wire shape carries
	// service_tier perfectly well and §4.4 clears the field anyway, so the
	// caller's value goes nowhere and nothing said so. It is reported here, on
	// the same header the ignored priority hint rides, and for the identical
	// reason §10.3 gives: silently discarding something a caller sent leaves them
	// believing it took effect.
	//
	// The intersection with Required is what keeps this from becoming noise:
	// Required raises CapServiceTier only for a tier that SELECTS something.
	// `auto` delegates the band to the provider, which is exactly what not
	// sending the field does, so there is nothing to report — and SDKs fill that
	// value whether or not the application asked for it.
	//
	// §10.5's tier fold is the other way a caller's service_tier fails to reach
	// the wire, and it is settled by the dispatcher rather than here: deciding it
	// needs the VALUE the caller wrote, and a decision that reported an override
	// whenever dorang chose a tier would fire on requests where dorang chose the
	// same one.
	d.Dropped = c.dep.Capabilities.Missing(req.Required).Droppable() |
		c.dep.Suppressed&req.Required

	// Estimate is priced for the credential that was actually acquired, which
	// is not necessarily the one ranking priced. When it is, the ranking's
	// answer is reused rather than recomputed: Price is pure, so the two calls
	// would agree, and one of them would be wasted work on the hot path.
	switch {
	case c.costKnown && c.costCred == d.Credential:
		d.Estimate = c.cost
	case r.deps.Pricing != nil:
		pr := r.priceRequest(c, req, now)
		pr.Credential = d.Credential
		if cost, err := r.deps.Pricing.Price(pr); err == nil {
			d.Estimate = cost
			if cost.Missing {
				r.unpriced.Add(1)
			}
		}
	}

	switch {
	case c.prefixHit:
		d.Reason = prefixHitReason(c.prefixDepth)
	case c.sticky:
		d.Reason = ReasonSticky
	case p.hard:
		d.Reason = ReasonPinned
	case len(cands) == 1:
		d.Reason = ReasonOnlyCandidate
	default:
		if s, ok := decidedBy(chain, cands); ok {
			d.Reason = string(s)
		} else {
			d.Reason = ReasonConfigOrder
		}
	}
	return d
}

// Report closes the loop on one attempt.
//
// It releases the capacity reservation, feeds internal/health, records or
// discards the prefix and session affinity, and stores the classified cause so
// that a following Route with Request.Previous set can continue the session as
// a fail-back hop.
//
// Every decision must be reported exactly once, including a successful one:
// the reservation is held until it is.
func (r *Router) Report(d *Decision, o Outcome) {
	if d == nil {
		return
	}
	d.Reservation.Release()

	cause := o.Cause
	if cause == CauseNone && o.Err != nil {
		cause = Classify(o.Status, o.Err)
		if cause == CauseNone {
			cause = unclassifiedCause(o.Status)
		}
	}
	ok := cause == CauseNone && o.Err == nil

	// Failure is set deliberately, never inferred from Err (see
	// [countsAgainstAvailability]). An error to the CALLER and a failure of the
	// DEPLOYMENT are different facts, and only the second one belongs here.
	failure := countsAgainstAvailability(cause)
	r.deps.Health.Report(d.Deployment, health.Outcome{
		Err:          o.Err,
		Failure:      failure,
		TTFT:         o.TTFT,
		Total:        o.Total,
		OutputTokens: o.OutputTokens,
	})

	switch cause {
	case CauseAuth:
		// §7.6: the credential is marked exhausted. It does not recover on a
		// cooldown the way a 5xx does — a wrong key stays wrong.
		r.deps.Health.MarkUnavailable(d.Deployment, r.cfg.Fallback.AuthCooldown)
	case CauseQuotaExhausted:
		if w := o.RetryAfter; w > 0 {
			r.deps.Health.MarkUnavailable(d.Deployment, w)
		} else if !o.ResetAt.IsZero() {
			if w := o.ResetAt.Sub(r.now()); w > 0 {
				r.deps.Health.MarkUnavailable(d.Deployment, w)
			}
		}
	case CauseRateLimit:
		if o.RetryAfter > 0 {
			r.deps.Health.MarkUnavailable(d.Deployment, o.RetryAfter)
		} else if r.cfg.Fallback.RateLimitCooldown > 0 {
			r.deps.Health.MarkUnavailable(d.Deployment, r.cfg.Fallback.RateLimitCooldown)
		}
	}

	st := d.st
	if st != nil {
		st.reported = true
		st.failed = !ok
		st.cause = cause
		st.firstByte = st.firstByte || o.FirstByteSent
		st.lastErr = o.Err
		if !o.ResetAt.IsZero() {
			st.resetAt = o.ResetAt
		}
	}

	if ok {
		// Prefix affinity is recorded at EVERY checkpoint of the chain, not
		// only the deepest: the shallow entries are what lets a later request
		// that shares only the system prompt still find a warm backend (§7.4b).
		if r.deps.Prefix != nil && st != nil && len(st.digests) > 0 {
			r.deps.Prefix.Record(st.digests, d.interned)
		}
		if st != nil && st.stickySet {
			r.sticky.put(st.sticky, d.Deployment, d.Credential)
		}
		return
	}

	// An unhealthy or exhausted target discards the pin immediately (§7.4a):
	// leaving it in place would route every remaining turn of the session at a
	// backend that has just proved it cannot serve them.
	if st != nil && st.stickySet && discardsPin(cause) {
		r.sticky.discard(st.sticky)
	}
}

// unclassifiedCause settles a failure neither the caller nor [Classify] could
// name. It is the last word: nothing further is known about this attempt.
//
// A 4xx is the caller's request being refused. Every 4xx that means something
// else is already named by Classify, so what arrives here is a 400, a 422, a
// 404 — a body, a field or a route the caller got wrong. The deployment
// examined it and said no, quickly and correctly, which is a backend WORKING.
//
// Defaulting it to CauseUpstream5xx, as this once did, made every such refusal
// a liveness failure. internal/health opens a circuit after three consecutive
// failures and an open circuit removes the deployment from selection for EVERY
// tenant, so three malformed requests from any caller holding any key took a
// healthy deployment away from everybody else — a cross-tenant denial of
// service costing an attacker three requests. It also gave the request an
// upstream_5xx fallback chain, so ONE bad body toured the group and then the
// whole class, opening a circuit at each stop.
//
// Anything that is not a 4xx keeps the old default. A failure with no status at
// all, or with a 2xx status and an error, is dorang unable to complete an
// exchange it started: no response arrived, or the response could not be read,
// converted or relayed. That is the deployment's side of the wire, and there is
// nobody else to attribute it to.
func unclassifiedCause(status int) Cause {
	if status >= 400 && status < 500 {
		return CauseBadRequest
	}
	return CauseUpstream5xx
}

// countsAgainstAvailability reports whether a cause is evidence about the
// DEPLOYMENT rather than about the request, the caller or the credential.
//
// This is the whole of the router's half of the bargain internal/health
// describes: that package tracks liveness and leaves the verdict here, because
// only the router knows what the status meant. An open circuit is shared — it
// removes the deployment from selection for every tenant at once — so the bar
// for counting something is that the deployment itself is at fault.
//
// It is an allow-list over the vocabulary rather than a list of exclusions, so
// that a cause added later counts for nothing until somebody argues it onto the
// list. The safe direction is leaving a broken backend in service one request
// longer, not standing a working one down.
func countsAgainstAvailability(c Cause) bool {
	switch c {
	case CauseUpstream5xx, CauseTimeout:
		// The deployment failed on its own account, or did not answer at all.
		// This is the condition availability tracking exists for, and the only
		// one: consecutive occurrences open the circuit, the cooldown elapses,
		// and one probe decides whether it recovered.
		return true

	case CauseBadRequest, CauseContextWindow, CauseContentPolicy:
		// The REQUEST was refused. All three are the backend reading the body
		// and correctly declining it — a fast, accurate 4xx is a healthy
		// deployment, not a failing one. They are also the three a caller can
		// produce at will, which is what makes counting them a denial of service
		// against every other tenant of the same deployment rather than a
		// tuning mistake.
		return false

	case CauseRateLimit, CauseQuotaExhausted:
		// Capacity, not liveness — and already acted on above:
		// MarkUnavailable stands the deployment down for exactly the
		// Retry-After the provider named. Counting it here as well would stack
		// a second, unrelated cooldown on top of the one that was asked for.
		return false

	case CauseAuth:
		// A dead credential, not a dead backend. Also already acted on:
		// MarkUnavailable holds it out for AuthCooldown, which is long because
		// a wrong key does not fix itself the way an overloaded backend does.
		return false

	case CauseBudgetExceeded:
		// dorang's own refusal. The upstream was never called and has nothing
		// to answer for.
		return false
	}
	return false
}

func discardsPin(c Cause) bool {
	switch c {
	case CauseUpstream5xx, CauseTimeout, CauseAuth, CauseQuotaExhausted, CauseRateLimit:
		return true
	}
	return false
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
