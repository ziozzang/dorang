package scenario

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/store"
)

// DESIGN §14 scenarios 4 and 5 — quota and budget.

func mustWindow(t *testing.T, s string) quota.Window {
	t.Helper()
	w, err := quota.ParseWindow(s)
	if err != nil {
		t.Fatalf("ParseWindow(%q): %v", s, err)
	}
	return w
}

func mustMeter(t *testing.T, now func() time.Time, rules ...quota.Rule) *quota.Meter {
	t.Helper()
	m, err := quota.NewMeter(quota.MeterConfig{Rules: rules, Now: now})
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	return m
}

// -----------------------------------------------------------------------------
// §14.4 — quota exhausted: the credential steps aside, traffic continues,
//         and it comes back after the window resets
// -----------------------------------------------------------------------------

func TestScenario04_ExhaustedCredentialStepsAsideAndReturns(t *testing.T) {
	const limit = 3
	group := router.Group{
		Name:  "chat",
		Class: "general",
		Deployments: []router.Deployment{{
			ID: "plan-vendor/chat", Provider: "plan-vendor", Kind: "openai",
			UpstreamModel: "zai:glm-5.1",
			Credentials: []router.Credential{
				{ID: "plan-1", CapacityGroup: "acct-1"},
				{ID: "plan-2", CapacityGroup: "acct-2"},
			},
		}},
	}

	// One clock drives the router and both meters, so a window that resets and
	// a routing decision cannot disagree about what time it is.
	clk := newClock()
	rule := quota.Rule{
		Window: mustWindow(t, "rolling:1h"), Metric: quota.MetricRequests,
		Limit: limit, OnExhaust: quota.Cooldown,
	}
	meters := router.Meters{
		"plan-1": mustMeter(t, clk.now, rule),
		"plan-2": mustMeter(t, clk.now, rule),
	}
	r := newRig(t, router.Config{Groups: []router.Group{group}}, rigOpts{clock: clk, quota: meters})

	req := router.Request{Model: "chat", Principal: "team-a"}

	// Before exhaustion the first credential in preference order serves. This
	// is the baseline the move is measured against: without it, "traffic landed
	// on plan-2" would also be satisfied by a round robin.
	for range limit {
		d := r.route(req)
		if d.Credential != "plan-1" {
			t.Fatalf("credential = %q, want plan-1 while it still has quota", d.Credential)
		}
		r.ok(d)
		meters["plan-1"].Record(r.clock.now(), quota.Usage{Requests: 1})
	}

	if dec := meters["plan-1"].Check(r.clock.now()); dec.Allow {
		t.Fatalf("plan-1 should be in cooldown after %d requests against a limit of %d", limit, limit)
	} else if dec.State != quota.StateCooldown {
		t.Fatalf("state = %v, want cooldown: only cooldown keeps serving elsewhere", dec.State)
	} else if dec.ResetAt.IsZero() {
		t.Fatal("an exhausted rolling window must say when it resets")
	}

	// Traffic continues, on the other credential, without an error.
	for range 3 {
		d := r.route(req)
		if d.Credential != "plan-2" {
			t.Fatalf("credential = %q, want plan-2: exhausted quota must move traffic, not stop it",
				d.Credential)
		}
		r.ok(d)
	}

	// And it comes back once the window resets. The clock moves; nothing sleeps.
	reset := meters["plan-1"].Check(r.clock.now()).ResetAt
	r.clock.set(reset.Add(time.Second))
	if dec := meters["plan-1"].Check(r.clock.now()); !dec.Allow {
		t.Fatalf("plan-1 did not recover after its window reset at %s: %+v", reset, dec)
	}
	d := r.route(req)
	if d.Credential != "plan-1" {
		t.Fatalf("credential = %q, want plan-1 after the reset", d.Credential)
	}
	r.ok(d)

	t.Run("both exhausted is a refusal, not a silent choice", func(t *testing.T) {
		// The inverse of "traffic continues": when there is nowhere left to go,
		// the honest outcome is an error naming the cause. A router that fell
		// back to an exhausted credential would pass every assertion above.
		for range limit {
			meters["plan-1"].Record(r.clock.now(), quota.Usage{Requests: 1})
			meters["plan-2"].Record(r.clock.now(), quota.Usage{Requests: 1})
		}
		re := r.routeErr(req)
		if re.Code != router.CodeQuotaExhausted {
			t.Fatalf("code = %q, want %q", re.Code, router.CodeQuotaExhausted)
		}
		if re.ResetAt.IsZero() {
			t.Error("a quota refusal must name when the window recovers")
		}
	})
}

// -----------------------------------------------------------------------------
// §14.5 — a budget cap is never exceeded under 100 concurrent requests
// -----------------------------------------------------------------------------

// The gate under test is the DURABLE ledger, which is the only budget mechanism
// this build has.
//
// It used to be `quota.Budget`, an in-memory implementation of the same idea
// that no non-test caller had ever reached. DESIGN §17's W9 row already asserted
// the conclusion — "no in-memory path is kept beside it" — and that sentence was
// written about a third implementation (`store.ReserveBudget`) which was deleted
// while this one survived the same sweep. Two implementations of one thing is
// this codebase's most expensive recurring defect, and the scenario is the
// reason the second one looked alive: it was the only thing exercising it.
//
// So the scenario now drives the path a request actually takes.
// `app.budgetGate` reserves an upper bound against [cluster.Ledger] after the
// routing decision, settles it with the real cost, and releases in full anything
// that never reached an upstream; that is exactly the sequence below, at the
// hundred-way concurrency §14.5 names. What the move BUYS, beyond removing the
// duplicate, is that the invariant is now checkable against the durable counter
// rather than against a map in this process: a budget that a restart resets was
// W9's open risk, and "never exceeded" asserted over state that does not survive
// is not the property an operator was promised.

const (
	scenarioBudgetEstimate = int64(2_000_000) // 0.002 USD in nano
	scenarioBudgetActual   = int64(1_400_000)
)

// newBudgetLedger opens a store on a temporary SQLite file and builds one node's
// ledger over it. Two nodes of one cluster are two of these against the same
// path, which is what [cluster.Ledger] means by a shared counter.
func newBudgetLedger(t *testing.T, dsn, node string, clk *clock, block int64,
	ttl time.Duration) *cluster.Ledger {

	t.Helper()
	st, err := store.Open(context.Background(), store.Config{
		Driver: store.DialectSQLite, DSN: dsn, Now: clk.now,
	})
	if err != nil {
		t.Fatalf("store.Open(%s): %v", node, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	l, err := cluster.NewLedger(cluster.LedgerConfig{
		Store: st, NodeID: node, Block: block, TTL: ttl, RenewBefore: ttl / 3, Now: clk.now,
	})
	if err != nil {
		t.Fatalf("NewLedger(%s): %v", node, err)
	}
	return l
}

func TestScenario05_BudgetNeverExceededUnderConcurrency(t *testing.T) {
	const (
		concurrent = 100
		// Each request is estimated pessimistically and settles for less, which
		// is the normal shape: the estimate prices output at max_tokens.
		estimate = scenarioBudgetEstimate
		actual   = scenarioBudgetActual
	)
	// A limit that cannot fit all hundred requests, so the cap actually bites:
	// a hundred settlements of `actual` need 140% of it.
	limit := estimate * concurrent / 2
	// A block small enough that the run refills several times. A block equal to
	// the limit would prove only that one draw was correctly sized.
	block := estimate * 5

	clk := newClock()
	ctx := context.Background()
	led := newBudgetLedger(t, filepath.Join(t.TempDir(), "budget.db"), "node-a", clk, block, time.Minute)
	t.Cleanup(func() { _ = led.Close(ctx) })

	key := cluster.BudgetKey("team", "team-a", quota.Daily, clk.now())

	var (
		admitted atomic.Int64
		refused  atomic.Int64
		spent    atomic.Int64
		breach   atomic.Bool
		peak     atomic.Int64
	)

	// A watchdog samples the counter while the requests race. The invariant is
	// not merely "the final total fits" — a budget that overshoots and then
	// refunds has already let the money be spent. It reads [cluster.Ledger.Stats],
	// which is the same lock-free view §12.3's budget gauge exports and which
	// therefore cannot itself perturb what it is watching.
	stop := make(chan struct{})
	var wgWatch sync.WaitGroup
	wgWatch.Add(1)
	go func() {
		defer wgWatch.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, s := range led.Stats() {
				if s.Key.String() != key.String() {
					continue
				}
				held := s.Committed
				for {
					old := peak.Load()
					if held <= old || peak.CompareAndSwap(old, held) {
						break
					}
				}
				if held > limit {
					breach.Store(true)
				}
			}
		}
	}()

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			h, err := led.Reserve(ctx, key, limit, estimate)
			if err != nil {
				if !errors.Is(err, cluster.ErrExhausted) {
					t.Errorf("Reserve failed with an unexpected error: %v", err)
					return
				}
				// DESIGN §6.4: exceeding a budget is terminal, never a fallback
				// condition. internal/app turns this sentinel into a 400 with
				// code budget_exceeded rather than a 429 precisely so that the
				// request is not sent down the fallback chain to spend another
				// subject's budget on a model the caller never asked for.
				refused.Add(1)
				return
			}
			if err := led.Settle(h, actual); err != nil {
				t.Errorf("Settle: %v", err)
				return
			}
			admitted.Add(1)
			spent.Add(actual)
		}()
	}
	close(start)
	wg.Wait()
	close(stop)
	wgWatch.Wait()

	if breach.Load() {
		t.Errorf("the counter exceeded the limit during the run (peak %d, limit %d)", peak.Load(), limit)
	}
	if got := spent.Load(); got > limit {
		t.Errorf("settled spend %d exceeds the limit %d", got, limit)
	}

	// The durable figure, read back from the store: this is the number a process
	// starting now would see, and it is the one W9 was about.
	committed, err := led.Committed(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if committed > limit {
		t.Errorf("the durable counter holds %d against a limit of %d", committed, limit)
	}
	if committed < spent.Load() {
		// The block is charged BEFORE a unit of it is spent, so the counter is
		// always at or ahead of reality. A counter behind what was settled would
		// mean money was spent that nothing had committed — the direction this
		// arrangement exists to make impossible.
		t.Errorf("the durable counter holds %d but %d was settled against it", committed, spent.Load())
	}

	// The cap has to have bitten, or the test proved only that 100 small
	// requests fit in a large budget. It cannot not bite: a hundred settlements
	// of `actual` need 140% of the ceiling.
	if refused.Load() == 0 {
		t.Fatalf("no request was refused; the limit %d did not constrain %d requests of %d",
			limit, concurrent, estimate)
	}
	if admitted.Load()+refused.Load() != concurrent {
		t.Fatalf("admitted %d + refused %d != %d", admitted.Load(), refused.Load(), concurrent)
	}
	t.Logf("admitted %d, refused %d, settled %d of a %d limit (peak committed %d) "+
		"in %d store draws for %d reservations",
		admitted.Load(), refused.Load(), spent.Load(), limit, peak.Load(),
		led.Draws(), admitted.Load()+refused.Load())

	t.Run("inverse: the same requests fit when the limit is large enough", func(t *testing.T) {
		// If the refusals above were caused by anything other than the ceiling,
		// raising the ceiling would not remove them.
		clk := newClock()
		ctx := context.Background()
		led := newBudgetLedger(t, filepath.Join(t.TempDir(), "budget.db"), "node-a", clk,
			block, time.Minute)
		t.Cleanup(func() { _ = led.Close(ctx) })
		key := cluster.BudgetKey("team", "team-a", quota.Daily, clk.now())
		roomy := estimate * concurrent

		var ok atomic.Int64
		var wg sync.WaitGroup
		for range concurrent {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h, err := led.Reserve(ctx, key, roomy, estimate)
				if err != nil {
					return
				}
				_ = led.Settle(h, actual)
				ok.Add(1)
			}()
		}
		wg.Wait()
		if ok.Load() != concurrent {
			t.Fatalf("%d of %d requests fit a budget sized for all of them", ok.Load(), concurrent)
		}
	})

	t.Run("a hold nobody settles is reclaimed rather than locking the budget", func(t *testing.T) {
		// R1-5: without expiry, a process killed between reserve and settle locks
		// that amount forever and the budget is eventually exhausted by money
		// nobody spent.
		//
		// The durable ledger answers it one level up, which is why the assertion
		// is about a NODE rather than about a reservation: the block is charged
		// to the counter before a unit of it is spent, so a node that dies takes
		// its whole block out of circulation, and it is the lease — not the
		// individual hold — that expires and is returned by the leader.
		const ttl = 30 * time.Second
		clk := newClock()
		ctx := context.Background()
		dsn := filepath.Join(t.TempDir(), "budget.db")
		key := cluster.BudgetKey("team", "team-a", quota.Daily, clk.now())
		limit := estimate * 10

		// One block is the whole ceiling, so the node that draws it holds all of
		// the money and none of the others can have any.
		dying := newBudgetLedger(t, dsn, "node-a", clk, limit, ttl)
		leader := newBudgetLedger(t, dsn, "node-b", clk, limit, ttl)
		t.Cleanup(func() { _ = leader.Close(ctx) })

		if _, err := dying.Reserve(ctx, key, limit, estimate); err != nil {
			t.Fatalf("the first reservation was refused: %v", err)
		}
		// node-a is killed here: no Settle, no Close, no checkpoint.

		if _, err := leader.Reserve(ctx, key, limit, estimate); !errors.Is(err, cluster.ErrExhausted) {
			t.Fatalf("the budget should be held by node-a's lease, got %v", err)
		}
		// And it must stay held while the lease is live. Taking a lease whose
		// holder may still be spending is how a two-node cluster once admitted
		// 190 against a limit of 100.
		if res, err := leader.ReclaimExpired(ctx, clk.now()); err != nil {
			t.Fatal(err)
		} else if res.Leases != 0 {
			t.Fatalf("the leader reclaimed %d live leases", res.Leases)
		}

		clk.advance(ttl + time.Second)
		res, err := leader.ReclaimExpired(ctx, clk.now())
		if err != nil {
			t.Fatal(err)
		}
		if res.Leases != 1 {
			t.Fatalf("reclaim took %d leases, want the dead node's one", res.Leases)
		}
		// The whole block comes back, including the part node-a had reserved and
		// never settled. That over-return is the published overshoot of §5.6 —
		// block × (nodes − 1) — and it is the safe direction: the budget is
		// usable again, and the residue is bounded by the block rather than
		// unbounded in time.
		if res.Returned != limit {
			t.Fatalf("reclaim returned %d, want the unspent block of %d", res.Returned, limit)
		}
		if _, err := leader.Reserve(ctx, key, limit, estimate); err != nil {
			t.Fatalf("the reclaimed budget is still locked: %v", err)
		}
	})
}
