package pricing

import (
	"fmt"
	"time"
)

// Class separates kinds of cost that must not compete for a single winner (§8.1).
type Class uint8

const (
	// ClassMarginal is per-token, per-request, per-character or per-second cost.
	// The most specific matching rule wins.
	ClassMarginal Class = iota
	// ClassSubscription is plan cost that does not depend on this request.
	// The most specific matching rule wins, then the plan cost is amortized.
	ClassSubscription
	// ClassAdjustment is a discount, margin or tax. Every matching rule applies, in order.
	ClassAdjustment
	// ClassNotional is a pay-as-you-go list rate that is never billed (§8.5). The most
	// specific matching rule wins, exactly as for ClassMarginal, but the result lands in
	// Cost.NotionalNano and is absent from Cost.TotalNano by construction.
	ClassNotional
	numClasses
)

func (c Class) String() string {
	switch c {
	case ClassMarginal:
		return "marginal_usage"
	case ClassSubscription:
		return "fixed_subscription"
	case ClassAdjustment:
		return "adjustment"
	case ClassNotional:
		return "notional_rate"
	}
	return "unknown"
}

func parseClass(s string) (Class, error) {
	switch s {
	case "", "marginal_usage":
		return ClassMarginal, nil
	case "fixed_subscription":
		return ClassSubscription, nil
	case "adjustment":
		return ClassAdjustment, nil
	case "notional_rate":
		return ClassNotional, nil
	}
	return 0, fmt.Errorf("unknown class %q (want marginal_usage, fixed_subscription, adjustment or notional_rate)", s)
}

// Level is a rule's specificity. Higher is more specific; the order is exactly the one
// fixed by §8.2: credential > deployment > (provider, model) > model > model prefix >
// provider > default.
type Level uint8

const (
	LevelDefault Level = iota
	LevelProvider
	LevelModelPrefix
	LevelModel
	LevelProviderModel
	LevelDeployment
	LevelCredential
	numLevels
)

func (l Level) String() string {
	switch l {
	case LevelCredential:
		return "credential"
	case LevelDeployment:
		return "deployment"
	case LevelProviderModel:
		return "provider+model"
	case LevelModel:
		return "model"
	case LevelModelPrefix:
		return "model_prefix"
	case LevelProvider:
		return "provider"
	case LevelDefault:
		return "default"
	}
	return "unknown"
}

// Unit is what a marginal rate is quoted per.
type Unit uint8

const (
	UnitPerMillionTokens Unit = iota
	UnitPerRequest
	UnitPerThousandCharacters
	UnitPerSecond
	UnitSubscription
	UnitNone
)

func (u Unit) String() string {
	switch u {
	case UnitPerMillionTokens:
		return "per_1m_tokens"
	case UnitPerRequest:
		return "per_request"
	case UnitPerThousandCharacters:
		return "per_1k_characters"
	case UnitPerSecond:
		return "per_second"
	case UnitSubscription:
		return "subscription"
	}
	return "none"
}

func parseUnit(s string) (Unit, error) {
	switch s {
	case "per_1m_tokens":
		return UnitPerMillionTokens, nil
	case "per_request":
		return UnitPerRequest, nil
	case "per_1k_characters":
		return UnitPerThousandCharacters, nil
	case "per_second":
		return UnitPerSecond, nil
	case "subscription":
		return UnitSubscription, nil
	}
	return 0, fmt.Errorf("unknown unit %q", s)
}

// Period is the recurrence of a subscription charge.
type Period uint8

const (
	PeriodMonthly Period = iota
	PeriodDaily
	PeriodWeekly
	PeriodYearly
)

func (p Period) String() string {
	switch p {
	case PeriodDaily:
		return "daily"
	case PeriodWeekly:
		return "weekly"
	case PeriodMonthly:
		return "monthly"
	case PeriodYearly:
		return "yearly"
	}
	return "unknown"
}

func parsePeriod(s string) (Period, error) {
	switch s {
	case "", "monthly":
		return PeriodMonthly, nil
	case "daily":
		return PeriodDaily, nil
	case "weekly":
		return PeriodWeekly, nil
	case "yearly", "annual":
		return PeriodYearly, nil
	}
	return 0, fmt.Errorf("unknown period %q (want daily, weekly, monthly or yearly)", s)
}

// TierMode selects how size-dependent rates are applied.
type TierMode uint8

const (
	// TierThreshold prices the whole request at the rates of the tier the request's
	// input token count falls into.
	TierThreshold TierMode = iota
	// TierGraduated splits the input tokens across brackets, pricing each bracket at its
	// own input rate. Components other than input have no bracket of their own — the
	// bracket key is up_to_input_tokens — so they are priced at the reached tier's rate.
	TierGraduated
)

func (t TierMode) String() string {
	if t == TierGraduated {
		return "graduated"
	}
	return "threshold"
}

func parseTierMode(s string) (TierMode, error) {
	switch s {
	case "", "threshold":
		return TierThreshold, nil
	case "graduated":
		return TierGraduated, nil
	}
	return 0, fmt.Errorf("unknown tier_mode %q (want threshold or graduated)", s)
}

// AdjOp is how an adjustment transforms its base.
type AdjOp uint8

const (
	// AdjPercent adds base * amount / 100. A negative amount is a discount.
	AdjPercent AdjOp = iota
	// AdjMultiply scales the base by amount, contributing base*amount - base.
	AdjMultiply
	// AdjAdd contributes amount, independent of the base.
	AdjAdd
)

func (o AdjOp) String() string {
	switch o {
	case AdjPercent:
		return "percent"
	case AdjMultiply:
		return "multiply"
	case AdjAdd:
		return "add"
	}
	return "unknown"
}

func parseAdjOp(s string) (AdjOp, error) {
	switch s {
	case "", "percent":
		return AdjPercent, nil
	case "multiply":
		return AdjMultiply, nil
	case "add":
		return AdjAdd, nil
	}
	return 0, fmt.Errorf("unknown op %q (want percent, multiply or add)", s)
}

// AdjBase selects what an adjustment is computed from.
type AdjBase uint8

const (
	// BaseTotal is the running total: marginal + amortized subscription + adjustments
	// applied so far.
	BaseTotal AdjBase = iota
	// BaseMarginal is the marginal cost only.
	BaseMarginal
	// BaseSubscription is the amortized subscription cost only.
	BaseSubscription
)

func (b AdjBase) String() string {
	switch b {
	case BaseMarginal:
		return "marginal"
	case BaseSubscription:
		return "subscription"
	}
	return "total"
}

func parseAdjBase(s string) (AdjBase, error) {
	switch s {
	case "", "total":
		return BaseTotal, nil
	case "marginal":
		return BaseMarginal, nil
	case "subscription":
		return BaseSubscription, nil
	}
	return 0, fmt.Errorf("unknown applies_to %q (want total, marginal or subscription)", s)
}

// Request is one priced request's identity and measured usage.
//
// Provider, Model, Credential and Deployment are the static dimensions the rule index is
// built on. Model is opaque (§2.1): it is never split on ':' or '/' or any other
// character, and model_prefix matching is a literal prefix test on the whole name.
type Request struct {
	Provider   string
	Model      string
	Credential string
	Deployment string

	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64

	// Requests is the number of requests for per_request rules. Zero is normalized to
	// one, because Price prices one request.
	Requests   int64
	Characters int64
	// Seconds is wall time for per_second rules. It is the one float64 in the API and it
	// is a measured quantity, never a price: it is converted to exact micro-seconds at
	// the boundary (see seconds.go) and no price value ever touches binary floating point.
	Seconds float64

	// At is the instant used to evaluate time-dependent predicates and to place a
	// subscription period. Zero means "now".
	At time.Time

	// Settlement names the carry bucket used by Settle for the sub-nano rounding
	// remainder. Empty means the request's Credential, or the global bucket if that is
	// also empty. It is ignored by Price, which never mutates state.
	Settlement string
}

// Component is one priced line of a marginal rule.
type Component struct {
	Name     string
	RuleID   string
	Rate     string // the exact decimal as written in the catalog
	Unit     Unit
	Quantity int64
	// Scale is the negative power of ten Quantity is expressed in: 0 for tokens,
	// requests and characters, 6 for seconds (Quantity is then micro-seconds).
	Scale int32
	// SubtotalNano is this line rounded on its own, for display. The authoritative sum is
	// Cost.MarginalNano, which is rounded once from the unrounded total; component
	// subtotals may therefore differ from it by less than one nano each.
	SubtotalNano int64
}

// Reason records why a rule was applied.
type Reason uint8

const (
	// ReasonMostSpecific: the highest-specificity time-eligible rule of its class won.
	ReasonMostSpecific Reason = iota
	// ReasonAllApply: adjustments do not compete; every matching rule applies in order.
	ReasonAllApply
)

// Applied is one rule that contributed to a Cost.
//
// The reason is carried as enumerated data, not as a formatted string, because Price runs
// on the hot path and §15.5 forbids formatted string construction there. Why renders it
// for the preview endpoint and the admin calculator.
type Applied struct {
	RuleID   string
	Class    Class
	Level    Level
	Priority int
	Order    int
	Reason   Reason
}

// Why renders the selection reason in words. It allocates, and is meant for §8.4
// rendering, not for the hot path.
func (a Applied) Why() string {
	switch a.Reason {
	case ReasonAllApply:
		return fmt.Sprintf("every matching %s rule applies; this one matched at the %s level and ran at order %d",
			a.Class, a.Level, a.Order)
	default:
		return fmt.Sprintf("most specific %s rule that matched: %s level, priority %d",
			a.Class, a.Level, a.Priority)
	}
}

// Cost is the outcome of pricing one request. All amounts are in nano-units of the
// catalog currency (1e-9).
type Cost struct {
	// MarginalNano is what this request costs at the margin. Routing uses this, and only
	// this: a sunk subscription cost must not make a saturated plan look cheap.
	MarginalNano int64
	// SubscriptionNano is the amortized share of a fixed plan cost. Accounting only.
	SubscriptionNano int64
	// AdjustmentNano is the net effect of all adjustment rules; negative for a discount.
	AdjustmentNano int64
	// TotalNano is MarginalNano + SubscriptionNano + AdjustmentNano, exactly.
	//
	// NotionalNano is deliberately not a term here. It is not omitted by convention that
	// a later edit could forget: the sum is formed from the three billing classes and
	// there is no code path that adds a notional amount to it (§8.5).
	TotalNano int64

	// NotionalNano is what this traffic would have cost at pay-as-you-go list rates.
	// It is an estimate for traceability and prediction, and it must never reach billing,
	// budget, quota or routing. Routing compares MarginalNano (§8.1).
	NotionalNano int64

	Components   []Component
	AppliedRules []Applied

	// Missing reports that no marginal_usage rule matched. The cost is zero, and the
	// caller is expected to increment a counter and warn (§8.3): silent zero-cost
	// accounting is the failure mode this package exists to avoid.
	Missing bool

	// NotionalMissing reports that no notional_rate rule matched, so NotionalNano is
	// unavailable rather than zero (§8.5). It is a separate flag from Missing because the
	// two have different consequences: Missing means the request cannot be billed, this
	// means it cannot be estimated. A zero here would make a subscription look infinitely
	// efficient, which is the most flattering answer and the least likely to be
	// questioned, so the caller is expected to increment its own counter and warn.
	NotionalMissing bool
}

// Considered is one rule that was examined for a class, and what became of it.
type Considered struct {
	RuleID   string
	Level    Level
	Priority int
	Order    int
	Eligible bool
	Selected bool
	Reason   string
}

// ClassTrace is the evaluation of one class.
type ClassTrace struct {
	Class      Class
	Considered []Considered
}

// NotionalDetail is the audit trail of a notional figure (§8.5). An estimate that cannot
// be traced to a source and a date is a guess wearing a currency symbol, so the rule id,
// the operator-declared source and the as-of date travel with the number.
type NotionalDetail struct {
	RuleID string
	Source string
	// AsOf is the date the operator recorded the rate on.
	AsOf time.Time
	// AsOfText is the as_of value exactly as written in the catalog.
	AsOfText string
	// Age is how stale the rate was at the priced instant.
	Age time.Duration

	Nano       int64
	Components []Component
	// Missing reports that no notional rule matched; Nano is then unavailable, not zero.
	Missing bool
}

// Explanation powers the preview endpoint, the admin calculator and the CLI (§8.4): one
// engine, one answer. It reports the applied rule chain per class, each component's rate
// and quantity, the subtotal and the final amount, and why each rule was selected.
type Explanation struct {
	Currency string
	Request  Request
	Cost     Cost
	Classes  []ClassTrace
	// Notional carries the provenance of the notional figure, so an estimate can be
	// audited rather than merely displayed (§8.5).
	Notional NotionalDetail
	Notes    []string
	Err      string
}
