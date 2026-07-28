package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
)

// DefaultBudgetBlockNanoUSD is how much budget a node draws from the durable
// counter at a time: 0.05 USD.
//
// This is the knob of DESIGN §9.6 — lease size trades store traffic against the
// published overshoot of §5.6 (block × (nodes−1)). At the few hundred micro-USD
// a typical chat request costs, a five-cent block is on the order of a hundred
// requests per store write, which is the same order as the 400-requests-to-5-
// writes W9 was measured at. A block of one would be a write per request, which
// is the arrangement §9.6 exists to avoid; a block of a whole budget would put
// the entire ceiling at risk on one node's crash.
const DefaultBudgetBlockNanoUSD int64 = 50_000_000

// budgetSubjectKind is what a key-scoped budget is recorded against in
// budget_state. DESIGN §6.4 allows a budget on a credential, key, user, team or
// globally; the key is the one the gate can name without a store lookup,
// because it is on the authorization snapshot the request already carries.
const budgetSubjectKind = "key"

// budgetGate holds money before it is spent, durably.
//
// # Why this is not internal/quota's Budget
//
// quota.Budget is exact, cheap and entirely in memory — and a process restart
// resets it. That is safe for concurrency (nothing is over-granted by
// forgetting) and wrong for accounting: a monthly budget silently starts over,
// which is DESIGN risk W9. internal/cluster closed the mechanism; this is the
// call site W9's last line asks for.
//
// It is not a trade of speed for durability either, which is why no in-memory
// path is kept alongside it. quota.Budget serializes every reservation on one
// mutex; cluster.Ledger takes a read lock and an atomic compare-and-swap on a
// block this node already holds, and touches the store once per BLOCK rather
// than once per request. The durable path is the cheaper one under concurrency,
// so "keep the in-memory one for the notebook tier" would be keeping a slower
// implementation for its lack of a feature.
//
// # Where the hold is taken
//
// DESIGN §6.4 describes a soft hold at the gate that hardens once capacity is
// acquired. Here there is one hold, taken immediately after the routing decision
// and before the upstream call. The reason is the estimate: §6.4 defines it as
// "exact input tokens priced, plus output priced at max_tokens", and a price
// requires a provider and an upstream model — neither of which exists until
// routing has chosen a deployment. Reserving earlier would mean reserving
// against a price nobody has yet quoted.
//
// What the soft hold buys is therefore preserved in the one place it matters:
// a request that fails before reaching an upstream releases in full (R1-20), and
// concurrent requests still cannot both see the pre-spend balance, because the
// reservation happens before either of them sends anything.
type budgetGate struct {
	ledger *cluster.Ledger
	now    func() time.Time
}

// budgetHold is one request's hold. A nil hold is the unbudgeted case and every
// method on it is a no-op, so the caller has no branch to forget.
type budgetHold struct {
	gate *budgetGate
	hold *cluster.Hold
}

// reserve takes a hold for the upper bound of what this attempt may cost.
//
// It returns a terminal *server.Error when the budget is exhausted. DESIGN §6.4
// is explicit that this is a 400 and not a 429: a 429 is a rate-limit signal,
// and emitting one here would send the request down the fallback chain to spend
// a different subject's budget on a model the caller never asked for.
func (g *budgetGate) reserve(ctx context.Context, st *dispatchState, c *call,
	dec *router.Decision, rq *server.Request) (*budgetHold, error) {

	if g == nil || g.ledger == nil {
		return nil, nil
	}
	p, ok := rq.Principal.(*principal)
	if !ok || p == nil {
		// No principal means a public route; there is no subject to charge.
		return nil, nil
	}
	limit, window, ok := p.budget()
	if !ok {
		return nil, nil
	}

	amount := g.estimate(st, c, dec)
	hold, err := g.ledger.Reserve(ctx,
		cluster.BudgetKey(budgetSubjectKind, p.KeyID(), window, g.now()), limit, amount)
	if err != nil {
		if errors.Is(err, cluster.ErrExhausted) {
			return nil, server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
				"the budget for this credential is exhausted for the current "+
					window.String()+" period").WithCode("budget_exceeded")
		}
		return nil, server.NewError(http.StatusServiceUnavailable, server.TypeAPIError,
			"the budget could not be reserved: "+err.Error()).WithCode("budget_unavailable")
	}
	rq.Result.BudgetNanoUSD = limit
	return &budgetHold{gate: g, hold: hold}, nil
}

// estimate is DESIGN §6.4's deliberately pessimistic upper bound: exact input
// tokens priced, plus output priced at max_tokens.
//
// An over-estimate is refunded at settlement; an under-estimate has already let
// the budget overshoot, so where the two are not symmetric this errs high. A
// request that names no max_tokens is priced at the deployment's catalogued
// ceiling rather than at zero, because zero would make an unbounded generation
// free to reserve.
func (g *budgetGate) estimate(st *dispatchState, c *call, dec *router.Decision) int64 {
	if st == nil || st.pricing == nil {
		return 0
	}
	out := maxOutputTokens(c.creq)
	if out <= 0 && st.catalog != nil {
		out = int64(st.catalog.Model(dec.Kind, dec.UpstreamModel).MaxOutputTokens)
	}
	cost, err := st.pricing.Settle(pricing.Request{
		Provider:     dec.Provider,
		Model:        dec.UpstreamModel,
		Credential:   dec.Credential,
		Deployment:   dec.Deployment,
		InputTokens:  c.rreq.InputTokens,
		OutputTokens: out,
		Requests:     1,
		At:           g.now(),
	})
	if err != nil || cost.Missing || cost.TotalNano < 0 {
		// An unpriced model reserves nothing. It is already reported as
		// unpriced at settlement (§8.3), and refusing traffic on a price the
		// deployment never configured would be a worse answer than counting it
		// at zero.
		return 0
	}
	return cost.TotalNano
}

// settle records what the request actually cost and returns the difference.
func (h *budgetHold) settle(actual int64) {
	if h == nil || h.hold == nil {
		return
	}
	_ = h.gate.ledger.Settle(h.hold, actual)
}

// release returns the hold in full. It is the path for everything that never
// reached an upstream (§6.4, R1-20).
func (h *budgetHold) release() {
	if h == nil || h.hold == nil {
		return
	}
	_ = h.gate.ledger.Release(h.hold)
}

// budget reads the calling credential's ceiling and its period.
//
// The most restrictive of key, user and team wins (DESIGN §11.2). Only the key
// is charged, because it is the only subject the durable counter can be keyed by
// without a second lookup — but a user or team ceiling lower than the key's
// still binds the request, which is the direction that cannot fail open.
func (p *principal) budget() (limitNano int64, window quota.Window, ok bool) {
	if p == nil || p.p == nil || p.p.Master {
		return 0, quota.Window{}, false
	}
	var (
		limit  int64
		period string
	)
	for _, l := range []*auth.Limits{&p.p.Key, p.p.User, p.p.Team} {
		if l == nil || l.MaxBudgetNanoUSD == nil {
			continue
		}
		if v := *l.MaxBudgetNanoUSD; !ok || v < limit {
			limit, ok = v, true
		}
		if period == "" {
			period = l.BudgetPeriod
		}
	}
	if !ok {
		return 0, quota.Window{}, false
	}
	w, err := quota.ParseWindow(period)
	if err != nil || !w.Valid() {
		// §6.4's own example is a monthly budget, and a period that does not
		// parse must not become "no budget at all".
		w = quota.Monthly
	}
	return limit, w, true
}
