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
// credential), and the request's marginal cost is added to the subscription period
// accumulator that amortization divides by.
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
			"the subscription share is imputed for accounting; routing compares MarginalNano only")
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

	// fixed_subscription: the most specific rule wins, then it is amortized.
	if ex != nil {
		trace = &ex.Classes[ClassSubscription]
	}
	var subscription amt
	if sub := c.idx[ClassSubscription].selectWinner(req, at, &ev.examined, trace); sub != nil {
		s, err := c.subscriptionShare(sub, marginal, at, settle)
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
func evalUsage(r *rule, req *Request, comps *[]Component) (amt, error) {
	rates := r.rates
	if len(r.tiers) > 0 {
		rates = selectTier(r, req.InputTokens).rates
	}
	var total amt
	for i := 0; i < numComponents; i++ {
		if r.tierMode == TierGraduated && i == cInput && len(r.tiers) > 0 {
			sub, err := gradedInput(r, req, comps)
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
		qty, err := quantityOf(i, req)
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
func gradedInput(r *rule, req *Request, comps *[]Component) (amt, error) {
	remaining := req.InputTokens
	if remaining < 0 {
		return amt{}, fmt.Errorf("pricing: input quantity is negative (%d)", remaining)
	}
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
		if !t.rates.set[cInput] {
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

// subscriptionShare amortizes a plan cost onto this request, using the one formula §8.1
// fixes: period_cost x (request_marginal / period_marginal_to_date), falling back to the
// elapsed fraction of the period when the period's marginal is zero.
//
// The denominator includes this request, so the share is defined for the first request of
// a period (it takes the whole plan cost) and falls as the period accumulates usage. That
// is inherent to attributing a fixed cost before the period ends, and it is exactly why
// this number is accounting-only and never reaches routing.
func (c *Catalog) subscriptionShare(r *rule, marginal amt, at time.Time, mutate bool) (amt, error) {
	st := c.subs[r.id]
	if st == nil { // not reachable for a compiled catalog; defensive.
		return amt{}, errors.New("pricing: subscription state missing")
	}
	start, end := periodBounds(r.period, at, r.loc)

	if mutate {
		st.mu.Lock()
		defer st.mu.Unlock()
	}
	var (
		toDate u128
		last   time.Time
	)
	if cur := st.snap.Load(); cur != nil && cur.periodStart.Equal(start) {
		toDate, last = cur.marginal, cur.last
	}

	share, err := amortize(r.amountAtto, marginal.m, toDate, at, start, end, last)
	if err != nil {
		return amt{}, err
	}

	if mutate {
		sum, ok := toDate.add(marginal.m)
		if !ok {
			return amt{}, ErrOverflow
		}
		st.snap.Store(&subSnapshot{periodStart: start, marginal: sum, last: at})
	}
	return attoAmt(share), nil
}

func amortize(amount, reqMarginal, toDate u128, at, start, end, last time.Time) (u128, error) {
	if amount.isZero() {
		return u128{}, nil
	}
	total, ok := toDate.add(reqMarginal)
	if !ok {
		return u128{}, ErrOverflow
	}
	if total.isZero() {
		// No marginal usage to apportion by: fall back to the elapsed fraction of the
		// period since the last settlement, which sums to the plan cost over the period.
		base := last
		if base.Before(start) {
			base = start
		}
		elapsed := at.Sub(base)
		if elapsed < 0 {
			elapsed = 0
		}
		span := end.Sub(start)
		if span <= 0 {
			return u128{}, nil
		}
		if elapsed > span {
			elapsed = span
		}
		v, _, ok := amount.mulDiv(uint64(elapsed), uint64(span))
		if !ok {
			return u128{}, ErrOverflow
		}
		return v, nil
	}
	// Reduce the ratio to 64 bits, keeping 63 significant bits of the denominator. The
	// ratio of two accumulated amounts is not an exact decimal in general; this is the
	// one place the result is an approximation, it is bounded at 1 part in 2^62, and it
	// is accounting-only.
	shift := 0
	if bl := total.bitLen(); bl > 63 {
		shift = bl - 63
	}
	num := reqMarginal.shr(uint(shift))
	den := total.shr(uint(shift))
	if den.hi != 0 || den.isZero() {
		return u128{}, ErrOverflow
	}
	if num.hi != 0 {
		return u128{}, ErrOverflow
	}
	v, _, ok := amount.mulDiv(num.lo, den.lo)
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
