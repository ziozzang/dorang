package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/notify"
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

// The subject kinds a budget is recorded against in budget_state. DESIGN §6.4
// allows a budget on a credential, key, user, team or globally; all three of
// these are on the authorization snapshot the request already carries, so none
// of them costs a store lookup at the gate.
const (
	budgetKindKey  = "key"
	budgetKindUser = "user"
	budgetKindTeam = "team"
)

// budgetSubject is one ceiling and the subject that declared it.
//
// The pair is the whole point. A ceiling separated from its subject is a number
// with nowhere to be counted, and the previous arrangement — take the smallest
// ceiling across key, user and team, then count it against the KEY — is what
// let ten keys under one 100 USD team budget spend 1000 USD. Each of them was
// correctly refused at 100; there were simply ten counters.
//
// The period travels with the ceiling for the same reason internal/auth's
// Limits.BudgetPeriod does: a daily ceiling applied over a monthly window is
// thirty times too permissive, and taking the minimum limit from one subject
// and the period from another manufactures exactly that pairing.
type budgetSubject struct {
	kind   string
	id     string
	limit  int64
	window quota.Window
}

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
	// notify raises DESIGN §11.5's budget_80pct and budget_exceeded. It is nil
	// when notifications are off, and every method on a nil notifier is a
	// constant — which is what keeps the check below off the allocator.
	notify *notify.Notifier
}

// budget80Numerator and budget80Denominator are §11.5's 80% threshold as a
// fraction. It is a fraction rather than a percentage multiplication because
// limit is in nano-USD and limit*80 overflows an int64 somewhere above a
// hundred-million-dollar ceiling — which is an absurd budget, and exactly the
// kind of absurd input that should not silently invert a comparison.
const (
	budget80Numerator   = 4
	budget80Denominator = 5
)

// budgetHold is one request's holds — one per binding subject. A nil hold is
// the unbudgeted case and every method on it is a no-op, so the caller has no
// branch to forget.
type budgetHold struct {
	gate  *budgetGate
	holds []*cluster.Hold
}

// reserve takes a hold for the upper bound of what this attempt may cost.
//
// It returns a terminal *server.Error when the budget is exhausted. DESIGN §6.4
// is explicit that this is a 400 and not a 429: a 429 is a rate-limit signal,
// and emitting one here would send the request down the fallback chain to spend
// a different subject's budget on a model the caller never asked for.
func (g *budgetGate) reserve(ctx context.Context, st *dispatchState, c *call,
	dec *router.Decision, rq *server.Request) (*budgetHold, error) {

	// The nil-gate check comes before the estimate, not inside reserveFor:
	// estimate is a method on the gate and reads its clock, so an unconfigured
	// gate must not reach it. (A no-store deployment has a nil gate, and the
	// batch executor reaches this with one in tests.)
	if g == nil || g.ledger == nil {
		return nil, nil
	}
	var p *auth.Principal
	if pr, ok := rq.Principal.(*principal); ok && pr != nil {
		p = pr.p
	}
	hold, limit, err := g.reserveFor(ctx, budgetSubjectsOf(p), g.estimate(st, c, dec), rq)
	if err != nil {
		return nil, err
	}
	if hold != nil {
		rq.Result.BudgetNanoUSD = limit
	}
	return hold, nil
}

// reserveBatch is the batch path's entry to the same gate.
//
// It exists so the two call sites read alike and share every line below the
// estimate. The refusal is wrapped terminal: a budget that is spent is not a
// condition a retry improves, and internal/batch would otherwise re-dispatch
// the row through its backoff until MaxAttempts, spending the ceiling's worth
// of refusals on the store.
func (g *budgetGate) reserveBatch(ctx context.Context, st *dispatchState, c *call,
	dec *router.Decision, id *execIdentity) (*budgetHold, error) {

	if g == nil || g.ledger == nil {
		return nil, nil
	}
	if id == nil {
		return nil, errors.New("app: a batch row reached the budget gate with no identity")
	}
	hold, _, err := g.reserveFor(ctx, id.subs, g.estimate(st, c, dec), nil)
	if err != nil {
		return nil, &terminalError{msg: err.Error()}
	}
	return hold, nil
}

// reserveFor takes one hold per binding subject for an amount already
// estimated.
//
// This is the whole budget gate. Both callers reach it — the interactive
// dispatcher and the batch executor — because a batch row that reserved
// nothing was not "unbudgeted", it was unpriced spend on the operator's
// credential, and giving the two paths separate reservation code is how they
// came to disagree in the first place.
//
// Every subject that declares a ceiling gets its own hold against its own
// durable counter, with its own limit and its own window. Any one of them
// refusing refuses the request, and the ones already taken are released before
// the error returns — a partial reservation would leak budget on every refusal.
//
// The returned limit is the smallest ceiling among the subjects, which is what
// the client-facing header reports: it is the number that actually bounds this
// request.
//
// §11.5's budget_80pct and budget_exceeded are raised per subject, because the
// subject is what the operator is being told about: a team crossing 80% and a
// key crossing 80% are different facts and the notifier deduplicates on the
// subject, not on the request.
func (g *budgetGate) reserveFor(ctx context.Context, subs []budgetSubject,
	amount int64, rq *server.Request) (*budgetHold, int64, error) {

	if g == nil || g.ledger == nil || len(subs) == 0 {
		return nil, 0, nil
	}
	h := &budgetHold{gate: g, holds: make([]*cluster.Hold, 0, len(subs))}
	minLimit := int64(-1)
	now := g.now()
	for _, s := range subs {
		subject := notify.Subject{Kind: s.kind, ID: s.id}
		hold, err := g.ledger.Reserve(ctx,
			cluster.BudgetKey(s.kind, s.id, s.window, now), s.limit, amount)
		if err != nil {
			h.release()
			if errors.Is(err, cluster.ErrExhausted) {
				g.raise(notify.EventBudgetExceeded, subject, now, s.limit, s.limit, s.window, rq)
				return nil, 0, server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
					"the "+s.kind+" budget for this credential is exhausted for the current "+
						s.window.String()+" period").WithCode("budget_exceeded")
			}
			return nil, 0, server.NewError(http.StatusServiceUnavailable, server.TypeAPIError,
				"the budget could not be reserved: "+err.Error()).WithCode("budget_unavailable")
		}
		h.holds = append(h.holds, hold)
		if minLimit < 0 || s.limit < minLimit {
			minLimit = s.limit
		}

		// budget_80pct (§11.5). This is discovered on the request path and must
		// not be *sent* from it: Admit is a sharded map lookup that allocates
		// nothing and returns true at most once per subject per period, and
		// everything after it — rendering, connecting, retrying — happens on a
		// worker.
		//
		// Without the deduplication this fires on every request past the
		// threshold, which is the difference between an alert and a filter rule.
		if spent, seen := hold.Consumed(); seen > 0 && spent >= seen/budget80Denominator*budget80Numerator {
			g.raise(notify.EventBudget80, subject, now, spent, seen, s.window, nil)
		}
	}
	return h, minLimit, nil
}

// raise queues a budget notification, once per subject per period.
//
// The fields are built only after Admit says yes, which is the whole point of
// the two-step API: a budget that sits at 81% for an hour costs one allocation,
// not one per request.
func (g *budgetGate) raise(ev notify.Event, subject notify.Subject, now time.Time,
	spent, limit int64, window quota.Window, rq *server.Request) {

	if !g.notify.Admit(ev, subject, now) {
		return
	}
	fields := []notify.Field{
		{Name: "subject", Value: subject.String()},
		{Name: "period", Value: window.String()},
		{Name: "limit_usd", Value: usdString(limit)},
		{Name: "spent_usd", Value: usdString(spent)},
	}
	if ev == notify.EventBudget80 && limit > 0 {
		fields = append(fields, notify.Field{Name: "percent",
			Value: strconv.FormatInt(spent*100/limit, 10)})
	}
	if rq != nil {
		fields = append(fields, notify.Field{Name: "request_id", Value: rq.ID})
	}
	g.notify.Send(notify.Notification{
		Event: ev, Subject: subject, Fields: fields, At: now,
	})
}

// usdString renders nano-USD as dollars. It is off the request path — it runs
// once per subject per period, behind Admit — so §15.5's ban on formatted
// string construction does not reach it.
func usdString(nano int64) string {
	return strconv.FormatFloat(quota.USD(nano), 'f', 6, 64)
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

// settle records what the request actually cost, against every subject that
// held budget for it.
func (h *budgetHold) settle(actual int64) {
	if h == nil {
		return
	}
	for _, hold := range h.holds {
		_ = h.gate.ledger.Settle(hold, actual)
	}
}

// release returns every hold in full. It is the path for everything that never
// reached an upstream (§6.4, R1-20), and for a partial reservation that has to
// be undone because a later subject refused.
func (h *budgetHold) release() {
	if h == nil {
		return
	}
	for _, hold := range h.holds {
		_ = h.gate.ledger.Release(hold)
	}
	h.holds = nil
}

// budgetSubjectsOf lists every ceiling that binds this principal, one per
// subject that declares one.
//
// DESIGN §11.2's "the most restrictive wins" is satisfied by holding against
// all of them rather than by picking the smallest: a team ceiling and a key
// ceiling are different counters over different populations, and the smallest
// NUMBER counted against the key is not the team's ceiling — it is the team's
// ceiling granted separately to every key under it.
//
// Each subject keeps its own period. A key with a monthly 10 USD budget under a
// team with a daily 1 USD budget now yields two holds — 10 USD monthly on the
// key, 1 USD daily on the team — instead of one 1 USD ceiling applied over a
// month.
func budgetSubjectsOf(p *auth.Principal) []budgetSubject {
	if p == nil || p.Master {
		return nil
	}
	var out []budgetSubject
	add := func(kind, id string, l *auth.Limits) {
		if l == nil || l.MaxBudgetNanoUSD == nil || id == "" {
			return
		}
		w, err := quota.ParseWindow(l.BudgetPeriod)
		if err != nil || !w.Valid() {
			// §6.4's own example is a monthly budget, and a period that does
			// not parse must not become "no budget at all".
			w = quota.Monthly
		}
		out = append(out, budgetSubject{
			kind: kind, id: id, limit: *l.MaxBudgetNanoUSD, window: w,
		})
	}
	add(budgetKindKey, p.KeyID, &p.Key)
	add(budgetKindUser, p.UserID, p.User)
	add(budgetKindTeam, p.TeamID, p.Team)
	return out
}
