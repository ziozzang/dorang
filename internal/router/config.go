package router

import (
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/capacity"
)

// Strategy is one ordering rule (DESIGN §7.3). A list of them composes as a
// tie-break chain, evaluated in order: [prefix_sticky, lowest_cost, least_busy]
// means "prefer the warm backend; among equally warm ones prefer the cheapest;
// among equally cheap ones prefer the idlest".
type Strategy string

// The strategies of DESIGN §7.3, spelled exactly as internal/config validates
// them.
const (
	StrategyRoundRobin     Strategy = "round_robin"
	StrategyLeastBusy      Strategy = "least_busy"
	StrategyLowestCost     Strategy = "lowest_cost"
	StrategyLowestLatency  Strategy = "lowest_latency"
	StrategyHighestTPS     Strategy = "highest_tps"
	StrategySticky         Strategy = "sticky"
	StrategyPrefixSticky   Strategy = "prefix_sticky"
	StrategyPriority       Strategy = "priority"
	StrategyWeightedRandom Strategy = "weighted_random"
	// StrategyQuotaUrgency prefers the credential whose resetting allowance is
	// closest to being discarded (§7.5a(c)): unused_fraction ÷
	// remaining_fraction_of_window, ranked descending.
	//
	// It is not a performance signal. A subscription window that resets is
	// use-it-or-lose-it, so routing that ignores it systematically wastes the
	// cheapest capacity available and does so invisibly — nothing fails, the
	// bill is simply higher than it needed to be. §7.5a constrains it twice:
	// it composes AFTER lowest_cost by default, because preferring an expiring
	// allowance is right only when the alternative is also already paid for;
	// and it is damped by occupancy and jittered per node, or every node
	// converges on the same credential at the window edge and turns its
	// concurrency limit into a fleet-wide bottleneck.
	StrategyQuotaUrgency Strategy = "quota_urgency"
)

var allStrategies = [...]Strategy{
	StrategyRoundRobin, StrategyLeastBusy, StrategyLowestCost,
	StrategyLowestLatency, StrategyHighestTPS, StrategySticky,
	StrategyPrefixSticky, StrategyPriority, StrategyWeightedRandom,
	StrategyQuotaUrgency,
}

// ParseStrategy decodes a configured strategy name.
func ParseStrategy(s string) (Strategy, bool) {
	for _, k := range allStrategies {
		if Strategy(s) == k {
			return k, true
		}
	}
	return "", false
}

// Credential is one authenticating identity a deployment may use. It is the
// unit quotas, concurrency and — for stateful conversations — conversation
// state all attach to (DESIGN §3, §7.4a2).
//
// Only the id appears here. No key material ever reaches the router.
type Credential struct {
	// ID is the credential id. It is not secret; it appears in decisions,
	// metrics and errors.
	ID string
	// CapacityGroup is the account this credential belongs to, for the
	// credential-group axis of §5.1. Empty means the axis does not apply.
	CapacityGroup string
	// MaxConcurrent is this credential's own ceiling. Zero or less is
	// unlimited.
	MaxConcurrent int
}

// Deployment is one routing candidate: a provider, a credential pool and an
// upstream model name (DESIGN §3).
type Deployment struct {
	// ID uniquely names this candidate. It keys health, prefix affinity and
	// circuit state, so it must be stable across a hot reload.
	ID string
	// Provider is the configured provider name. Provider identity comes only
	// from configuration, never from parsing a model string (§2.1).
	Provider string
	// ProviderGroup is the provider-group capacity axis this deployment sits
	// in. Empty means the axis does not apply.
	ProviderGroup string
	// Kind is the provider kind ("vllm", "sglang", "anthropic", …). It selects
	// the priority emit rule and, through pkg/catalog, the protocol family and
	// the model's declared context window.
	Kind string
	// UpstreamModel is the real model id sent upstream (§7.2). It is opaque:
	// nothing splits it.
	UpstreamModel string
	// Credentials are the accounts that may serve this deployment, in
	// preference order.
	Credentials []Credential
	// Weight biases round_robin and weighted_random. Zero is treated as one.
	Weight int
	// Priority orders the `priority` strategy. Lower is preferred, matching the
	// canonical scale of §7.5.
	Priority int
	// Family is the protocol/model family opaque state is scoped to
	// (EXTENSIONS §B). Left empty, it is derived from the kind's wire adapter
	// through pkg/catalog.
	Family string
	// Capabilities is what this deployment can express (§10.1). A request whose
	// required structural set is not covered is filtered out before ranking.
	Capabilities canonical.Capability
	// ContextWindow is the real window in tokens. Zero means UNDECLARED, which
	// is not zero and not unlimited (§4.3): an undeclared window neither
	// excludes a deployment nor qualifies it as "larger" for the
	// same_class_larger chain. Left zero, it is filled from pkg/catalog.
	ContextWindow int
	// MaxOutputTokens is the declared output ceiling. Zero means undeclared.
	MaxOutputTokens int
	// PriorityVerified records that the operator has declared the engine flag
	// that makes priority actually take effect (VLLM.md §1.2, SGLANG.md §3.4).
	// Both engines accept a priority and ignore it silently by default, and no
	// response reveals that, so an unverified emission is flagged on the
	// decision rather than assumed to work.
	PriorityVerified bool
	// Timeout is this deployment's request timeout. It bounds the capacity
	// reservation, because §5.3 derives the deadline from the request's own
	// timeout rather than from a broker-wide constant.
	Timeout time.Duration
	// PrefixTTL is how long cache affinity to THIS deployment stays believable
	// (§7.4b). Zero inherits the table default; negative means the backend's
	// prefix cache has no clock and the entry lives until the table's byte
	// budget evicts it.
	//
	// It is per deployment because it models the backend's cache, not dorang's
	// table: a hosted service holds a prefix for a vendor-set window of minutes
	// while a self-hosted engine holds blocks until memory pressure evicts them,
	// and one number cannot be right for both.
	PrefixTTL time.Duration

	// compiled at New.
	caps      []capacity.Candidate
	credIndex map[string]int
	interned  uint32
	// ridx is a router-wide deployment index. Round-robin state is keyed on it
	// rather than on a position within one group, because a same_class hop
	// produces a candidate set drawn from several groups at once.
	ridx   int
	gname  string
	gclass string
}

// Group is the set of deployments behind one client-facing name (DESIGN §3).
type Group struct {
	// Name is the client-facing model name. It is opaque and compared whole.
	Name string
	// Class is the model class, the scope fail-back may delegate within.
	Class string
	// Strategy is this group's tie-break chain. Empty uses Config.Strategy.
	Strategy []Strategy
	// Deployments are the candidates, in configuration order. That order is the
	// final tie-break, so it is deterministic rather than map-random.
	Deployments []Deployment
}

// StickyConfig configures session stickiness (DESIGN §7.4a).
type StickyConfig struct {
	// Enabled turns stickiness on. A pin is only ever created for a request
	// that names a session.
	Enabled bool
	// TTL is how long a pin lives FROM CREATION. It is deliberately not
	// refreshed on use: the premise is that the upstream cache is gone after
	// the TTL, and refreshing would defeat the point.
	TTL time.Duration
}

// PrefixConfig configures prefix affinity (DESIGN §7.4b).
type PrefixConfig struct {
	// Enabled turns prefix affinity on.
	Enabled bool
}

// FallbackConfig is the fail-back policy of DESIGN §7.6.
type FallbackConfig struct {
	// On maps a cause to its chain. A nil map uses DefaultChains. A cause
	// present with an empty chain never falls back.
	On map[Cause][]Target
	// MaxHops bounds the number of FALLBACK attempts after the first dispatch,
	// so a request makes at most MaxHops+1 attempts. Zero disables fallback
	// entirely; a negative value is treated as zero.
	MaxHops int
	// Budget is the wall-clock ceiling on a whole routing session, measured
	// from the first Route call. Zero means no wall-clock bound.
	Budget time.Duration
	// AuthCooldown is how long a credential is taken out of service after an
	// authentication failure. Zero selects DefaultAuthCooldown.
	AuthCooldown time.Duration
	// RateLimitCooldown is used when a 429 carries no Retry-After. Zero leaves
	// the deployment selectable, which is what the health tracker's own
	// consecutive-failure logic already handles.
	RateLimitCooldown time.Duration
}

// DefaultAuthCooldown is how long a credential that failed authentication stays
// out of service. §7.6 marks such a credential exhausted; it recovers only when
// an operator or a rotation replaces it, so this is long rather than eager.
const DefaultAuthCooldown = 15 * time.Minute

// DefaultChains is the fail-back table of DESIGN §7.6, verbatim. budget_exceeded
// and auth are present with empty chains, so "the cause is absent" and "the
// cause never falls back" are different states rather than the same silence.
func DefaultChains() map[Cause][]Target {
	return map[Cause][]Target{
		CauseRateLimit:      {TargetSameGroup, TargetSameClass},
		CauseQuotaExhausted: {TargetSameGroup, TargetSameClass},
		CauseContextWindow:  {TargetSameClassLarger},
		CauseContentPolicy:  {TargetSameClass},
		CauseUpstream5xx:    {TargetSameGroup, TargetSameClass},
		CauseTimeout:        {TargetSameGroup},
		CauseBudgetExceeded: {},
		CauseAuth:           {},
	}
}

// Config is the router's static configuration. It is immutable once [New]
// returns: a hot reload builds a new Router and swaps the pointer (§4.1, §15.2).
type Config struct {
	// Groups are the model groups.
	Groups []Group
	// Aliases maps a virtual name to a group name. Aliases do not chain.
	Aliases map[string]string
	// Classes maps a class to its member group names. It is optional: a class
	// is also formed implicitly by every group that names it in Group.Class,
	// and the two are unioned.
	Classes map[string][]string
	// Strategy is the default tie-break chain for groups that name none.
	// Empty selects DefaultStrategy.
	Strategy []Strategy
	// Rotation is how a credential is chosen among a deployment's pool
	// (`key_rotation.strategy`). Empty means [RotationFailover].
	Rotation Rotation

	Sticky   StickyConfig
	Prefix   PrefixConfig
	Fallback FallbackConfig
	Priority PriorityConfig

	// OnCapacity is the spill policy for UNPINNED requests (§7.4a2). It never
	// applies to a pinned one: a pinned conversation that spills to another
	// account is not a worse choice, it is a wrong one.
	OnCapacity capacity.OnCapacity
	// PinnedWait bounds how long a pinned request waits for its own credential
	// before failing with a reason naming the pin. Zero fails immediately
	// rather than blocking.
	PinnedWait time.Duration

	// Now is the clock. Zero selects time.Now.
	Now func() time.Time
	// Rand returns a uniform value in [0,1). Zero selects the runtime source.
	// It exists so weighted_random is reproducible in a test.
	Rand func() float64
}

// DefaultStrategy is the chain used when neither the group nor the config names
// one. It is the §4.2 example's chain, which is also the sensible default:
// stay warm, then stay cheap, then stay idle.
func DefaultStrategy() []Strategy {
	return []Strategy{StrategyPrefixSticky, StrategyLowestCost, StrategyLeastBusy}
}
