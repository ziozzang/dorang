package pricing

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Evaluator prices several candidate deployments for one incoming request.
//
// Cost-based routing prices every candidate, and candidates routinely share a rule (a
// provider-level or default rule, say), so results are memoized per candidate within one
// incoming request (§8.2). An Evaluator is not safe for concurrent use; make one per
// in-flight request and Reset it if you reuse it.
type Evaluator struct {
	c *Catalog
	// memo is a small linear table, not a map: a request has a handful of candidates and
	// comparing whole Request values is cheaper than hashing one.
	memo     []memoEntry
	memoOK   bool
	adj      []*rule
	examined int
}

type memoEntry struct {
	req  Request
	cost Cost
}

// maxMemo bounds the linear scan. Beyond it the Evaluator simply stops caching.
const maxMemo = 64

// NewEvaluator returns an Evaluator with memoization enabled.
func (c *Catalog) NewEvaluator() *Evaluator {
	return &Evaluator{c: c, memo: make([]memoEntry, 0, 8), memoOK: true}
}

// Reset clears the memo so the Evaluator can serve the next incoming request.
func (e *Evaluator) Reset() {
	e.memo = e.memo[:0]
	e.adj = e.adj[:0]
	e.examined = 0
}

// Examined reports how many rules have actually been inspected. It is the direct measure
// of whether the index is working: it must stay bounded by the number of rules that can
// match a request, not by the size of the catalog.
func (e *Evaluator) Examined() int { return e.examined }

// Price prices one candidate. The returned Cost's slices are shared with the memo and
// must not be mutated by the caller.
func (e *Evaluator) Price(req Request) (Cost, error) {
	if e.memoOK {
		for i := range e.memo {
			if e.memo[i].req == req {
				return e.memo[i].cost, nil
			}
		}
	}
	cost, err := e.c.compute(&req, e, false, nil)
	if err != nil {
		return Cost{}, err
	}
	if e.memoOK && len(e.memo) < maxMemo {
		e.memo = append(e.memo, memoEntry{req: req, cost: cost})
	}
	return cost, nil
}

// Price prices one request without mutating anything: no carried remainder is consumed,
// no subscription period accumulator is advanced. This is the routing-safe entry point,
// and it may be called as often as the router likes.
func (c *Catalog) Price(req Request) (Cost, error) {
	ev := Evaluator{c: c}
	return c.compute(&req, &ev, false, nil)
}

// Settle prices one request and records it: the sub-nano rounding remainder is carried
// into the next settlement of the same bucket (Request.Settlement, defaulting to the
// credential), and the subscription period's attributed total is advanced by the share
// this row takes, so no later row attributes it a second time.
//
// Call it at most once per request, from the accounting path. Routing must call Price.
func (c *Catalog) Settle(req Request) (Cost, error) {
	ev := Evaluator{c: c}
	return c.compute(&req, &ev, true, nil)
}

// Explain prices a request and reports every rule that was considered, why it was or was
// not selected, and each component's rate, quantity and subtotal. It powers the preview
// endpoint, the admin calculator and the CLI (§8.4) — one engine, one answer. Like Price,
// it mutates nothing.
func (c *Catalog) Explain(req Request) Explanation {
	ex := Explanation{Currency: c.Currency, Request: req}
	ex.Classes = []ClassTrace{
		{Class: ClassMarginal}, {Class: ClassSubscription},
		{Class: ClassAdjustment}, {Class: ClassNotional},
	}
	ev := Evaluator{c: c}
	cost, err := c.compute(&req, &ev, false, &ex)
	if err != nil {
		ex.Err = err.Error()
		return ex
	}
	ex.Cost = cost
	if cost.Missing {
		ex.Notes = append(ex.Notes,
			"no marginal_usage rule matched: this request is unpriced, not free")
	}
	if cost.SubscriptionNano != 0 {
		ex.Notes = append(ex.Notes,
			"the subscription share is the plan cost this period has accrued since the previous "+
				"settlement, not a per-request estimate of the whole: the period's shares sum to "+
				"the plan cost and never exceed it. It is imputed for accounting; routing "+
				"compares MarginalNano only")
	}
	if cost.Floored {
		ex.Notes = append(ex.Notes,
			"the adjustments exceeded the cost they applied to: the total was clamped to zero, "+
				"because a credit may zero a request out but may not pay the caller")
	}
	if cost.NotionalMissing {
		ex.Notes = append(ex.Notes,
			"no notional_rate rule matched: the list-rate estimate is unavailable, not zero")
	} else {
		ex.Notes = append(ex.Notes, "notional cost is an estimate at list rates from "+
			ex.Notional.Source+" as of "+ex.Notional.AsOfText+
			"; it is excluded from TotalNano and never reaches billing, budget, quota or routing")
	}
	return ex
}

func (c *Catalog) compute(req *Request, ev *Evaluator, settle bool, ex *Explanation) (Cost, error) {
	at := req.At
	if at.IsZero() {
		at = time.Now()
	}

	var (
		cost  Cost
		trace *ClassTrace
	)
	if ex != nil {
		trace = &ex.Classes[ClassMarginal]
	}

	// marginal_usage: the most specific rule wins.
	var marginal amt
	winner := c.idx[ClassMarginal].selectWinner(req, at, &ev.examined, trace)
	if winner == nil {
		cost.Missing = true
	} else {
		// One allocation each for the component lines and the applied-rule chain.
		if winner.maxComponents > 0 {
			cost.Components = make([]Component, 0, winner.maxComponents)
		}
		cost.AppliedRules = make([]Applied, 0, c.appliedCap)
		m, err := evalUsage(winner, req, &cost.Components)
		if err != nil {
			return Cost{}, err
		}
		marginal = m
		cost.AppliedRules = append(cost.AppliedRules, Applied{
			RuleID: winner.id, Class: ClassMarginal, Level: winner.level,
			Priority: winner.priority, Reason: ReasonMostSpecific,
		})
	}

	// fixed_subscription: the most specific rule wins, then this request takes the share
	// of the plan cost that has accrued since the previous settlement.
	if ex != nil {
		trace = &ex.Classes[ClassSubscription]
	}
	var subscription amt
	if sub := c.idx[ClassSubscription].selectWinner(req, at, &ev.examined, trace); sub != nil {
		s, err := c.subscriptionShare(sub, at, settle)
		if err != nil {
			return Cost{}, err
		}
		subscription = s
		cost.AppliedRules = append(cost.AppliedRules, Applied{
			RuleID: sub.id, Class: ClassSubscription, Level: sub.level,
			Priority: sub.priority, Reason: ReasonMostSpecific,
		})
	}

	// adjustment: every matching rule applies, in order.
	if ex != nil {
		trace = &ex.Classes[ClassAdjustment]
	}
	adjustment, err := c.applyAdjustments(req, at, ev, marginal, subscription, &cost, trace)
	if err != nil {
		return Cost{}, err
	}

	// notional_rate: what this traffic would have cost at list rates. Never billed.
	if ex != nil {
		trace = &ex.Classes[ClassNotional]
	}
	var (
		notional      amt
		notionalRule  *rule
		notionalComps []Component
	)
	cost.NotionalMissing = true
	if n := c.idx[ClassNotional].selectWinner(req, at, &ev.examined, trace); n != nil {
		var sink *[]Component
		if ex != nil {
			// Notional components stay out of Cost.Components: a caller that sums the
			// component breakdown must get the billed figure, never the estimate.
			notionalComps = make([]Component, 0, n.maxComponents)
			sink = &notionalComps
		}
		v, err := evalUsage(n, req, sink)
		if err != nil {
			return Cost{}, err
		}
		notional, notionalRule = v, n
		cost.NotionalMissing = false
		cost.AppliedRules = append(cost.AppliedRules, Applied{
			RuleID: n.id, Class: ClassNotional, Level: n.level,
			Priority: n.priority, Reason: ReasonMostSpecific,
		})
	}

	// Round once, at the end, half-to-even, carrying the sub-nano remainder.
	mNano, sNano, aNano, nNano, err := c.roundOut(req, settle, marginal, subscription, adjustment, notional)
	if err != nil {
		return Cost{}, err
	}
	total, err := addNano(mNano, sNano)
	if err != nil {
		return Cost{}, err
	}
	if total, err = addNano(total, aNano); err != nil {
		return Cost{}, err
	}
	cost.MarginalNano = mNano
	cost.SubscriptionNano = sNano
	cost.AdjustmentNano = aNano
	// TotalNano is the sum of the three billing classes. nNano is not a term and there is
	// no branch in which it becomes one (§8.5).
	cost.TotalNano = total
	if total < 0 {
		// A credit may zero a request out; it may not pay the caller. Every consumer of
		// this number treats it as an amount SPENT — the ledger row, the budget hold, the
		// quota counter — and a negative one does not merely mis-bill, it manufactures
		// quota and budget out of a catalog edit. The guard lives here, in the one place
		// all three entry points (Price, Settle, Explain) pass through, rather than at the
		// call sites: two of the three call sites checked, one did not, and that is
		// precisely the failure this shape removes.
		//
		// The adjustment absorbs the difference so that TotalNano == MarginalNano +
		// SubscriptionNano + AdjustmentNano stays exact, which is what reconciliation
		// depends on. What was clamped is reported rather than hidden.
		cost.AdjustmentNano = aNano - total
		cost.TotalNano = 0
		cost.Floored = true
	}
	cost.NotionalNano = nNano
	if ex != nil {
		ex.Notional = NotionalDetail{
			Nano: nNano, Components: notionalComps, Missing: cost.NotionalMissing,
		}
		if notionalRule != nil {
			ex.Notional.RuleID = notionalRule.id
			ex.Notional.Source = notionalRule.source
			ex.Notional.AsOf = notionalRule.asOf
			ex.Notional.AsOfText = notionalRule.asOfText
			ex.Notional.Age = at.Sub(notionalRule.asOf)
		}
	}
	return cost, nil
}

// roundOut converts the three exact class totals to nano.
//
// Each class rounds once, from its own unrounded total, so that TotalNano is exactly the
// sum of the three reported fields and reconciliation cannot drift by a stray nano. When
// settling, each class keeps its own carried remainder, so a stream of sub-nano requests
// accumulates to the exact amount instead of rounding to zero forever.
func (c *Catalog) roundOut(req *Request, settle bool, m, s, a, n amt) (int64, int64, int64, int64, error) {
	var carry carrySet
	var slot *carrySet
	if settle {
		c.carryMu.Lock()
		defer c.carryMu.Unlock()
		bucket := settlementBucket(req)
		if c.carries == nil {
			c.carries = make(map[string]*carrySet, 4)
		}
		slot = c.carries[bucket]
		if slot == nil {
			slot = &carrySet{}
			c.carries[bucket] = slot
		}
		carry = *slot
	}
	mNano, mCarry, err := roundToNano(m, carry.marginal)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	sNano, sCarry, err := roundToNano(s, carry.subscription)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	aNano, aCarry, err := roundToNano(a, carry.adjustment)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	// The notional figure carries its own remainder, so a sub-nano list rate accumulates
	// to the right estimate instead of rounding to zero on every request.
	nNano, nCarry, err := roundToNano(n, carry.notional)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if slot != nil {
		*slot = carrySet{marginal: mCarry, subscription: sCarry, adjustment: aCarry, notional: nCarry}
	}
	return mNano, sNano, aNano, nNano, nil
}

func settlementBucket(req *Request) string {
	if req.Settlement != "" {
		return req.Settlement
	}
	return req.Credential
}

// carvedInput is the part of the inclusive input count that a more specific rate in the
// same rule already charges, and which the `input` rate must therefore not charge again.
//
// See [chargedQuantity] for the convention this implements.
func carvedInput(rates *rateSet, req *Request) int64 {
	var n int64
	if rates.set[cCacheRead] {
		n += req.CacheReadTokens
	}
	if rates.set[cCacheWrite] {
		n += req.CacheWriteTokens
	}
	return n
}

// carvedOutput is the part of the inclusive output count that the `reasoning` rate already
// charges.
func carvedOutput(rates *rateSet, req *Request) int64 {
	if rates.set[cReasoning] {
		return req.ReasoningTokens
	}
	return 0
}

// chargedQuantity is the measured quantity a component is billed for: the quantity as
// measured, less the parts that a more specific rate on the SAME rule already charges.
//
// # The convention, stated once
//
// dorang's usage counts are INCLUSIVE (§10.7): InputTokens is the whole prompt, cache reads
// and cache writes included, and OutputTokens is the whole completion, reasoning included.
// A vendor's rate card is not. Every provider dorang speaks to quotes `input` as the price
// of the prompt tokens it did NOT serve from cache, quotes a separate cached-input price for
// the ones it did, and folds reasoning into the output price unless it publishes a reasoning
// price of its own. So the two halves are in different conventions and something has to
// reconcile them.
//
// **The rate table is exclusive: a declared sub-rate carves its quantity out of its parent.**
//
//   - `cache_read` declared  → the `input` rate is charged on InputTokens - CacheReadTokens.
//   - `cache_write` declared → likewise, less CacheWriteTokens.
//   - `reasoning` declared   → the `output` rate is charged on OutputTokens - ReasoningTokens.
//   - a sub-rate NOT declared → its tokens stay with the parent and are billed at the parent
//     rate, which is exactly what a vendor with no cache discount charges.
//
// The alternative — charging `input` against the whole inclusive count and `cache_read`
// against the cached part again — is what this function replaced. Against §8.5's own example
// rates (input 0.85, output 3.40, cache_read 0.19 per 1M) and a 120/15-token request with a
// 40-token cached prefix, it billed $0.0001606 where the vendor bills $0.0001266: 27% over,
// and 5.7x over on a 90%-cached agentic workload. §10.7 warned about precisely this in prose
// — "a mis-mapped cache field does not produce a visible error, it produces a wrong invoice"
// — while the arithmetic did it anyway, which is why the rule now lives in the arithmetic
// and is asserted against a hand-computed vendor figure rather than against another function.
//
// A clamp at zero rather than an error: a negative result means the backend reported more
// cached tokens than prompt tokens, which contradicts the inclusive form every decoder
// produces. Refusing to price the request would drop the whole bill over an upstream's
// arithmetic; charging zero for the parent while its sub-rates still charge is the bounded
// answer, and the ledger's own columns show the contradiction.
func chargedQuantity(i int, req *Request, carvedIn, carvedOut int64) (int64, error) {
	q, err := quantityOf(i, req)
	if err != nil {
		return 0, err
	}
	switch i {
	case cInput:
		q -= carvedIn
	case cOutput:
		q -= carvedOut
	}
	if q < 0 {
		q = 0
	}
	return q, nil
}

// quantityOf reads the measured quantity a component is charged against.
func quantityOf(i int, req *Request) (int64, error) {
	var q int64
	switch i {
	case cInput:
		q = req.InputTokens
	case cOutput:
		q = req.OutputTokens
	case cCacheRead:
		q = req.CacheReadTokens
	case cCacheWrite:
		q = req.CacheWriteTokens
	case cReasoning:
		q = req.ReasoningTokens
	case cRequest:
		q = req.Requests
		if q == 0 {
			q = 1 // Price prices one request.
		}
	case cCharacters:
		q = req.Characters
	case cSeconds:
		return secondsToMicros(req.Seconds)
	}
	if q < 0 {
		return 0, fmt.Errorf("pricing: %s quantity is negative (%d)", componentInfo[i].name, q)
	}
	return q, nil
}

// evalUsage prices every component the winning rule declares a rate for. It serves
// marginal_usage and notional_rate alike: the two classes price identically and differ only
// in where the result is reported. comps may be nil, which skips the component breakdown.
//
// The quantities are the CHARGED ones, not the measured ones: a rule that declares
// `cache_read` beside `input` charges each token once. [chargedQuantity] states the rule.
func evalUsage(r *rule, req *Request, comps *[]Component) (amt, error) {
	rates := r.rates
	if len(r.tiers) > 0 {
		// The tier is selected on the whole inclusive prompt — a bracket is about how big
		// the request is, not about how much of it is billable at the input rate.
		rates = selectTier(r, req.InputTokens).rates
	}
	carvedIn, carvedOut := carvedInput(&rates, req), carvedOutput(&rates, req)
	var total amt
	for i := 0; i < numComponents; i++ {
		if r.tierMode == TierGraduated && i == cInput && len(r.tiers) > 0 {
			sub, err := gradedInput(r, req, comps, carvedIn)
			if err != nil {
				return amt{}, err
			}
			var ok bool
			if total, ok = addAmt(total, sub); !ok {
				return amt{}, ErrOverflow
			}
			continue
		}
		if !rates.set[i] {
			continue
		}
		qty, err := chargedQuantity(i, req, carvedIn, carvedOut)
		if err != nil {
			return amt{}, err
		}
		if qty == 0 {
			continue
		}
		v, err := componentValue(rates.atto[i], qty, componentInfo[i].divisor)
		if err != nil {
			return amt{}, err
		}
		var ok bool
		if total, ok = addAmt(total, v); !ok {
			return amt{}, ErrOverflow
		}
		if err := appendComponent(comps, r, i, rates.text[i], qty, v); err != nil {
			return amt{}, err
		}
	}
	return total, nil
}

// componentValue is rate x quantity / divisor, formed at 128-bit width. Because a price
// carries at most 12 fractional digits and the largest divisor is 10^6, the atto-scaled
// rate is always divisible by the divisor and the result is exact.
func componentValue(rateAtto u128, qty int64, divisor uint64) (amt, error) {
	v, _, ok := rateAtto.mulDiv(uint64(qty), divisor)
	if !ok {
		return amt{}, ErrOverflow
	}
	return attoAmt(v), nil
}

func appendComponent(comps *[]Component, r *rule, i int, rate string, qty int64, v amt) error {
	if comps == nil {
		return nil // the hot path does not build a breakdown it will not read
	}
	nano, _, err := roundToNano(v, 0)
	if err != nil {
		return err
	}
	*comps = append(*comps, Component{
		Name: componentInfo[i].name, RuleID: r.id, Rate: rate, Unit: r.unit,
		Quantity: qty, Scale: componentInfo[i].scale, SubtotalNano: nano,
	})
	return nil
}

// selectTier returns the tier the request's input token count falls into.
func selectTier(r *rule, input int64) tier {
	for i := range r.tiers {
		if !r.tiers[i].bounded || input <= r.tiers[i].upTo {
			return r.tiers[i]
		}
	}
	return r.tiers[len(r.tiers)-1]
}

// gradedInput splits input tokens across brackets, pricing each bracket at its own rate.
//
// carved is the cached and cache-written prefix that a `cache_read`/`cache_write` rate
// already charges ([chargedQuantity]). The brackets are still walked over the WHOLE prompt,
// because a bracket is a statement about request size; what changes is that the first
// `carved` tokens of it are not charged here. Taking them off the front rather than off the
// end is not arbitrary — a cache hit is a prefix of the prompt, so the tokens the cache
// served are literally the ones in the earliest brackets.
func gradedInput(r *rule, req *Request, comps *[]Component, carved int64) (amt, error) {
	remaining := req.InputTokens
	if remaining < 0 {
		return amt{}, fmt.Errorf("pricing: input quantity is negative (%d)", remaining)
	}
	skip := min(carved, remaining)
	var total amt
	var prev int64
	for i := range r.tiers {
		t := &r.tiers[i]
		if remaining <= 0 {
			break
		}
		span := remaining
		if t.bounded {
			span = t.upTo - prev
			if span > remaining {
				span = remaining
			}
			prev = t.upTo
		}
		if span <= 0 {
			continue
		}
		remaining -= span
		if skip > 0 {
			d := min(skip, span)
			span -= d
			skip -= d
		}
		if span <= 0 || !t.rates.set[cInput] {
			continue
		}
		v, err := componentValue(t.rates.atto[cInput], span, componentInfo[cInput].divisor)
		if err != nil {
			return amt{}, err
		}
		var ok bool
		if total, ok = addAmt(total, v); !ok {
			return amt{}, ErrOverflow
		}
		if err := appendComponent(comps, r, cInput, t.rates.text[cInput], span, v); err != nil {
			return amt{}, err
		}
	}
	return total, nil
}

// subscriptionShare attributes part of a fixed plan cost to this request (§8.1).
//
// What this request records is an INCREMENT of the period's attributed total, not an
// independent estimate of its own share. The period carries the total it has attributed so
// far; this request records the amount that brings that total up to the plan cost accrued
// by now. The period's rows therefore sum to the attributed total by construction, the
// total is bounded by the plan cost, and no settled row is ever restated.
//
// The formula this replaces charged request i a share of plan_cost x (request_marginal /
// marginal_to_date) and the ledger added those up. Each of those shares is an estimate of
// the same quantity, and estimates of one quantity must supersede one another rather than
// accumulate: summing them made a period of N equal requests report plan_cost x H_N — 519
// USD of a 100 USD plan at N = 100, 749 at N = 1000, with no bound. Every figure derived
// from it (per-key attribution, per-team chargeback, the notional-versus-actual comparison
// §8.5 exists for) was wrong by a factor that grew with traffic.
func (c *Catalog) subscriptionShare(r *rule, at time.Time, mutate bool) (amt, error) {
	st := c.subs[r.id]
	if st == nil { // not reachable for a compiled catalog; defensive.
		return amt{}, errors.New("pricing: subscription state missing")
	}
	if mutate {
		// A settlement stamped ahead of the present is clamped to it. The accumulator
		// only ever moves forward, so a row stamped in the future attributes everything
		// the plan will have accrued by that instant and leaves the real remainder of
		// the period attributing nothing — and if the stamp lands in the NEXT period it
		// moves periodStart forward too, so every subsequent row of the real period
		// takes the backfill branch below and the next period opens already depressed.
		// Measured: one such row attributed 10.00 USD of a 100.00 USD July.
		//
		// Clamping rather than refusing, because a refusal fails the whole pricing call
		// and takes the request's real marginal cost down with it — the plan share is the
		// only figure a bad clock can distort, so it is the only one adjusted.
		//
		// Only settlement is clamped. Price and Explain mutate nothing, and pricing a
		// future instant is exactly what a preview is for.
		//
		// # What this line does NOT cover, and where the cover is
		//
		// It covers a caller that stamps `At` from something other than this catalog's
		// clock: an imported or replayed row, a batch row re-priced from its recorded
		// instant, `dorangctl price --at`. It does NOT cover the gateway's own request
		// path, and the comment that used to stand here claimed it did — "one node in a
		// cluster with a clock that runs ahead is enough". internal/app gives this
		// catalog `a.now` and stamps `At: d.now()` from that SAME `a.now`, read first.
		// Two readings of one clock, in order: `at` is never the later of the two, so in
		// a default deployment this comparison cannot be true. The triggers the rule was
		// written for — an NTP step, a VM resume, a bad RTC — move both readings
		// together, which is precisely why they cannot be caught by comparing them.
		//
		// A clock check needs two OBSERVATIONS, not two readings. The second observation
		// exists at the process boundary, where the stamp was written down by whichever
		// process held the accumulator before this one: [Catalog.RestoreState] makes the
		// same comparison there, against a stamp this process's clock did not produce,
		// and that one can disagree.
		if now := c.nowInstant(); at.After(now) {
			at = now
		}
	}
	start, end := periodBounds(r.period, at, r.loc)

	if mutate {
		st.mu.Lock()
		defer st.mu.Unlock()
	}
	var attributed u128
	if cur := st.snap.Load(); cur != nil {
		switch {
		case cur.periodStart.Equal(start):
			attributed = cur.attributed
		case cur.periodStart.After(start):
			// The instant falls in a period the accumulator has already moved past
			// — a backfill, a replayed row, a clock that stepped back. That
			// period's plan cost was apportioned among the rows settled while it
			// was open, and the accumulator that would say how much of it is left
			// is gone. A late row therefore attributes nothing: with no state to
			// bound it, any positive share could take the closed period above the
			// plan cost, which is precisely the defect this shape removes. The
			// row's marginal cost is real and is priced as usual; only the plan
			// share is withheld.
			//
			// It must also not become the state of the subscription. Storing it
			// would move periodStart backwards, and the next request in the OPEN
			// period would find a period that does not match its own, read an
			// attributed total of zero, and let the open period accrue its whole
			// plan cost a second time.
			return amt{}, nil
		}
	}

	accrued, err := accrue(r.amountAtto, at, start, end)
	if err != nil {
		return amt{}, err
	}
	if accrued.cmp(attributed) <= 0 {
		// Nothing has accrued since the last row took its share: two rows at the same
		// instant, or a row that arrives out of order inside the open period. Zero,
		// never negative — a settled row is not restated, and a negative share would
		// hand budget and quota back exactly as a negative total would.
		return amt{}, nil
	}
	share, _ := accrued.sub(attributed)
	if mutate {
		st.snap.Store(&subSnapshot{periodStart: start, attributed: accrued})
	}
	return attoAmt(share), nil
}

// accrue returns how much of a period's plan cost exists to be attributed at instant at.
//
// A fixed plan buys a PERIOD, so the fraction of the period that has passed is the
// fraction of the plan cost that has been incurred. That denominator is known in advance,
// which is what makes the result bounded. Usage-to-date cannot serve as one: a period's
// total usage is unknowable until the period closes, every mid-period estimate of it is
// too small, and dividing by a denominator that is too small is what over-attributed every
// request and produced the harmonic sum. The exact usage-weighted apportionment is still
// available — it is a rollup over the ledger's marginal column once the period has closed
// — but a per-request column settled in real time cannot wait for it.
//
// The clamp on elapsed is where "a period never attributes more than the plan cost" is
// enforced. Price, Settle and Explain all reach the subscription figure through this one
// function, so there is no call site left to forget it: the same shape as the floor under
// TotalNano, which caught a third caller nobody had named.
func accrue(amount u128, at, start, end time.Time) (u128, error) {
	span := end.Sub(start)
	if amount.isZero() || span <= 0 {
		return u128{}, nil
	}
	elapsed := at.Sub(start)
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed >= span {
		return amount, nil // the cap: a whole plan cost, and never more than one
	}
	v, _, ok := amount.mulDiv(uint64(elapsed), uint64(span))
	if !ok {
		return u128{}, ErrOverflow
	}
	return v, nil
}

// periodBounds returns the half-open period containing at, in the rule's location.
func periodBounds(p Period, at time.Time, loc *time.Location) (time.Time, time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	l := at.In(loc)
	y, m, d := l.Date()
	switch p {
	case PeriodDaily:
		s := time.Date(y, m, d, 0, 0, 0, 0, loc)
		return s, time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	case PeriodWeekly:
		back := (int(l.Weekday()) + 6) % 7 // weeks start on Monday
		s := time.Date(y, m, d-back, 0, 0, 0, 0, loc)
		return s, time.Date(y, m, d-back+7, 0, 0, 0, 0, loc)
	case PeriodYearly:
		s := time.Date(y, time.January, 1, 0, 0, 0, 0, loc)
		return s, time.Date(y+1, time.January, 1, 0, 0, 0, 0, loc)
	default:
		s := time.Date(y, m, 1, 0, 0, 0, 0, loc)
		return s, time.Date(y, m+1, 1, 0, 0, 0, 0, loc)
	}
}

// applyAdjustments runs every matching adjustment rule in order over the running total.
func (c *Catalog) applyAdjustments(req *Request, at time.Time, ev *Evaluator,
	marginal, subscription amt, cost *Cost, tr *ClassTrace) (amt, error) {

	if c.idx[ClassAdjustment].count == 0 {
		return amt{}, nil
	}
	ev.adj = c.idx[ClassAdjustment].collectAll(ev.adj[:0], req, at, &ev.examined, tr)
	if len(ev.adj) == 0 {
		return amt{}, nil
	}
	rules := ev.adj
	// slices, not sort: sort.Slice goes through reflect.Swapper, and §15.5 forbids
	// reflection on the hot path.
	slices.SortStableFunc(rules, func(a, b *rule) int {
		if a.order != b.order {
			return a.order - b.order
		}
		if a.priority != b.priority {
			return b.priority - a.priority
		}
		return strings.Compare(a.id, b.id)
	})

	running, ok := addAmt(marginal, subscription)
	if !ok {
		return amt{}, ErrOverflow
	}
	var netAdj amt
	for _, r := range rules {
		base := running
		switch r.appliesTo {
		case BaseMarginal:
			base = marginal
		case BaseSubscription:
			base = subscription
		}
		delta, err := adjustmentDelta(r, base)
		if err != nil {
			return amt{}, err
		}
		if running, ok = addAmt(running, delta); !ok {
			return amt{}, ErrOverflow
		}
		if netAdj, ok = addAmt(netAdj, delta); !ok {
			return amt{}, ErrOverflow
		}
		cost.AppliedRules = append(cost.AppliedRules, Applied{
			RuleID: r.id, Class: ClassAdjustment, Level: r.level,
			Priority: r.priority, Order: r.order, Reason: ReasonAllApply,
		})
	}
	return netAdj, nil
}

func adjustmentDelta(r *rule, base amt) (amt, error) {
	switch r.op {
	case AdjAdd:
		return amt{neg: r.adjAmount.neg && !r.adjAtto.isZero(), m: r.adjAtto}, nil
	case AdjMultiply:
		scaled, ok := mulDivAmt(base, r.adjAmount.units, pow10[r.adjAmount.scale])
		if !ok {
			return amt{}, ErrOverflow
		}
		delta, ok := addAmt(scaled, base.negate())
		if !ok {
			return amt{}, ErrOverflow
		}
		return delta, nil
	default: // AdjPercent
		den, ok := mul64(100, pow10[r.adjAmount.scale])
		if !ok {
			return amt{}, ErrOverflow
		}
		delta, ok2 := mulDivAmt(base, r.adjAmount.units, den)
		if !ok2 {
			return amt{}, ErrOverflow
		}
		if r.adjAmount.neg {
			delta = delta.negate()
		}
		return delta, nil
	}
}

func mul64(a, b uint64) (uint64, bool) {
	p, _, ok := u64To128(a).mulDiv(b, 1)
	if !ok || p.hi != 0 {
		return 0, false
	}
	return p.lo, true
}
