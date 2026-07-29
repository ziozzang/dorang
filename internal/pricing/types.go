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
	// ClassSubscription is plan cost that does not depend on this request. The most
	// specific matching rule wins, then the plan cost is attributed as it accrues over
	// its period, so a period's shares sum to it and never exceed it (§8.1).
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
//
// # Two units are quoted per second and they are not interchangeable
//
// There is no `per_second`. A second of WALL TIME and a second of RECORDED AUDIO are
// two different billable quantities that share a word, and DESIGN §10.7's rule — **a
// billing unit is never converted** — is what makes them two units here rather than one
// field serving both. A GPU-second rate is a real thing and the request's own duration
// is the right input for it; a transcription vendor bills the length of the recording,
// which has nothing to do with how long dorang waited for the answer.
//
// The unit a rule declares is therefore also the statement of which quantity it prices,
// so a catalog author states the axis in the one place they cannot leave it out. A rule
// that names one axis while the request carries only the other is a NO-PRICE that says
// so ([Cost.NoPrice]) — never a silent substitution of the number that happens to be
// there. Ten minutes of audio transcribed in eight seconds is ten minutes on the
// invoice, and the defect this shape removes charged it as eight seconds.
type Unit uint8

const (
	UnitPerMillionTokens Unit = iota
	UnitPerRequest
	UnitPerThousandCharacters
	// UnitPerComputeSecond is quoted per second of WALL TIME — how long the request
	// took. It reads [Request.Seconds].
	UnitPerComputeSecond
	// UnitPerAudioSecond is quoted per second of RECORDED AUDIO — how long the media
	// the vendor billed for was. It reads [Request.AudioSeconds] and never falls back
	// to wall time.
	UnitPerAudioSecond
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
	case UnitPerComputeSecond:
		return "per_compute_second"
	case UnitPerAudioSecond:
		return "per_audio_second"
	case UnitSubscription:
		return "subscription"
	}
	return "none"
}

// errAmbiguousSecond is the load error for the spelling that did not say which second
// it priced. It is spelled out rather than folded into "unknown unit" because the
// operator who wrote it was not making a typo — they were writing the only spelling
// this catalog used to have, whose rate was applied to wall time whatever the vendor
// billed.
const errAmbiguousSecond = "unit %q does not say WHICH second it prices: use " +
	"per_compute_second for the request's own wall time (a GPU-second rate) or " +
	"per_audio_second for the length of the recording a transcription vendor bills " +
	"for. They are different quantities on different axes and dorang will not " +
	"substitute one for the other (DESIGN §10.7: a billing unit is never converted)"

func parseUnit(s string) (Unit, error) {
	switch s {
	case "per_1m_tokens":
		return UnitPerMillionTokens, nil
	case "per_request":
		return UnitPerRequest, nil
	case "per_1k_characters":
		return UnitPerThousandCharacters, nil
	case "per_compute_second":
		return UnitPerComputeSecond, nil
	case "per_audio_second":
		return UnitPerAudioSecond, nil
	case "subscription":
		return UnitSubscription, nil
	case "per_second":
		return 0, fmt.Errorf(errAmbiguousSecond, s)
	}
	return 0, fmt.Errorf("unknown unit %q", s)
}

// BilledUnit is the unit the VENDOR said it billed a request in.
//
// It is the second half of the same rule, one level up from the catalog. The audio
// surface bills in tokens or in duration, discriminated by `usage.type` on the wire
// (DESIGN §10.7), and a rule pricing the axis the vendor did NOT bill in is the same
// error as a rule reading the wrong quantity — it just makes it against a number that
// exists. Carrying what the vendor named is what lets [Catalog.Price] refuse instead of
// charging whichever figure is in the struct.
//
// Only the audio decoder states it today. Everything else leaves it unstated, which
// asserts nothing and constrains nothing.
type BilledUnit uint8

const (
	// BilledUnstated is a backend that named no unit. It is the zero value, so every
	// request built before this existed behaves exactly as it did.
	BilledUnstated BilledUnit = iota
	// BilledTokens is `usage.type: "tokens"` — the invoice is a token count.
	BilledTokens
	// BilledDuration is `usage.type: "duration"` — the invoice is a length of media.
	BilledDuration
)

func (b BilledUnit) String() string {
	switch b {
	case BilledTokens:
		return "tokens"
	case BilledDuration:
		return "duration"
	}
	return "unstated"
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
	// Seconds is the request's own WALL TIME, and it prices per_compute_second rates and
	// nothing else. It is a measured quantity, never a price: it is converted to exact
	// micro-seconds at the boundary (see seconds.go) and no price value ever touches
	// binary floating point.
	//
	// It is NOT the length of anything the request carried. It used to be the only
	// per-second quantity, so a transcription vendor that bills the recording was billed
	// dorang's own latency instead: a ten-minute recording transcribed in eight seconds
	// was charged as eight seconds. That quantity is AudioSeconds, and the two never
	// substitute for one another.
	Seconds float64
	// AudioSeconds is the length of RECORDED MEDIA the vendor billed for, in seconds, and
	// it prices per_audio_second rates and nothing else.
	//
	// It is the quantity on the invoice, not necessarily the length of the file: a model
	// that bills a rounded minute for a 12.5-second clip has billed sixty seconds, and
	// that is the number a rate is applied to.
	AudioSeconds float64
	// Billed is the unit the VENDOR said it billed this request in, when it said (§10.7).
	// Unstated is the zero value and constrains nothing.
	Billed BilledUnit

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

// NoPriceReason says why a matching marginal rule did not price a request.
//
// Every value is a disagreement about the BILLING UNIT, which is the only kind of
// disagreement that cannot be resolved by arithmetic (§10.7). A token count of zero is a
// measurement and prices to zero; a duration that was never measured is not, and neither
// is a duration the vendor did not bill in.
type NoPriceReason uint8

const (
	// NoPriceNone: the rule priced the request.
	NoPriceNone NoPriceReason = iota
	// NoPriceAudioNotMeasured: the rule prices per_audio_second and the request carries
	// no recorded duration. Wall time is NOT substituted, which is the whole point: the
	// substitution charged a ten-minute recording as the eight seconds the transcription
	// took.
	NoPriceAudioNotMeasured
	// NoPriceComputeNotMeasured: the rule prices per_compute_second and the request
	// carries no wall time. This is the ordinary state of a routing QUOTE, which is
	// priced before the request runs — a quote cannot know how long an answer will take,
	// and reporting that is better than quoting zero as though the deployment were free.
	NoPriceComputeNotMeasured
	// NoPriceVendorBilledTokens: the rule prices a duration and the vendor said it
	// billed this request in tokens. The duration may well be reported; it is not what
	// is on the invoice.
	NoPriceVendorBilledTokens
	// NoPriceVendorBilledDuration: the rule prices tokens and the vendor said it billed
	// this request by duration. The token counts on such a response are usually absent
	// or synthesized, so the rule would charge a confident zero.
	NoPriceVendorBilledDuration
)

// Why renders the reason in words, for the preview endpoint and the operator's log line.
func (r NoPriceReason) Why() string {
	switch r {
	case NoPriceAudioNotMeasured:
		return "the rule prices per_audio_second and this request carries no recorded " +
			"duration; the request's wall time is a different quantity and is not " +
			"substituted for it"
	case NoPriceComputeNotMeasured:
		return "the rule prices per_compute_second and this request carries no wall time, " +
			"which is the ordinary state of a quote priced before the request runs"
	case NoPriceVendorBilledTokens:
		return "the rule prices a duration and the backend reported billing this request " +
			"in tokens (usage.type); a billing unit is never converted"
	case NoPriceVendorBilledDuration:
		return "the rule prices tokens and the backend reported billing this request by " +
			"duration (usage.type); a billing unit is never converted"
	}
	return ""
}

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
	// SubscriptionNano is this request's share of a fixed plan cost. Accounting only.
	//
	// It is the plan cost the period has accrued since the previous settlement, so the
	// shares recorded across a period sum to the plan cost and never exceed it (§8.1).
	// It is not an estimate of "the plan cost divided by the requests so far" — that
	// number, summed over N requests, reports the plan cost N-times-over-counted as
	// plan_cost x H_N, which is the defect this field's definition was changed to remove.
	SubscriptionNano int64
	// AdjustmentNano is the net effect of all adjustment rules; negative for a discount.
	AdjustmentNano int64
	// TotalNano is MarginalNano + SubscriptionNano + AdjustmentNano, exactly, and it is
	// never negative.
	//
	// The floor is a property of this type, not a rule each caller is asked to remember.
	// TotalNano is what the ledger row, the budget hold and the quota counter all read as
	// "what this request spent", and a negative one does not merely mis-bill: it gives
	// budget and quota back. An adjustment large enough to invert the sum is clamped to
	// zero and Floored says so; see [Cost.Floored].
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

	// NoPrice reports that a marginal_usage rule DID match and could not price this
	// request, because it prices a quantity the request does not carry — or one the
	// vendor did not bill in. MarginalNano is zero and no component line was emitted.
	//
	// It is a separate flag from Missing because it has a different cause and a
	// different fix: Missing means the catalog says nothing about this model, this
	// means the catalog says the wrong thing about it. It is separate from BOTH of
	// them being silent, which is what the alternative — charging the rate against
	// whatever number is in the neighbouring field — did for as long as one field
	// served two quantities.
	//
	// A caller treats it exactly as it treats Missing: record the request UNPRICED,
	// count it, and say which rule declined. It is deliberately not folded into
	// Missing, so that "no rule" and "the wrong rule" are answerable apart.
	NoPrice NoPriceReason
	// NoPriceRule is the id of the rule that could not price, and NoPriceQuantity the
	// component that could not be measured. Both are constants taken from the compiled
	// catalog, never formatted here: §15.5 forbids building a string on the hot path.
	NoPriceRule     string
	NoPriceQuantity string

	// Floored reports that the adjustments summed to less than the cost they applied to,
	// so the total was clamped to zero and AdjustmentNano reduced to match. A credit that
	// exceeds the request it credits is a catalog error — the credit outlives the request
	// it was written for, and the balance it would carry has nowhere to live in a
	// per-request price — so the caller is expected to count it and warn rather than to
	// read a zero as an ordinary free request.
	Floored bool

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
