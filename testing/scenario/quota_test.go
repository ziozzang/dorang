package scenario

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/router"
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

func TestScenario05_BudgetNeverExceededUnderConcurrency(t *testing.T) {
	const (
		concurrent = 100
		// Each request is estimated pessimistically and settles for less, which
		// is the normal shape: the estimate prices output at max_tokens.
		estimate = int64(2_000_000) // 0.002 USD in nano
		actual   = int64(1_400_000)
	)
	// A limit that admits roughly half the requests, so the cap actually bites.
	limit := estimate * concurrent / 2

	clk := newClock()
	b, err := quota.NewBudget(quota.BudgetConfig{
		Period:       quota.Daily,
		DefaultLimit: limit,
		Now:          clk.now,
	})
	if err != nil {
		t.Fatalf("NewBudget: %v", err)
	}
	subject := quota.Subject{Kind: "team", ID: "team-a"}
	b.SetLimit(subject, limit)

	var (
		admitted atomic.Int64
		refused  atomic.Int64
		spent    atomic.Int64
		breach   atomic.Bool
		peak     atomic.Int64
	)

	// A watchdog samples the budget while the requests race. The invariant is
	// not merely "the final total fits" — a budget that overshoots and then
	// refunds has already let the money be spent.
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
			s := b.Snapshot(subject, clk.now())
			held := s.Spent + s.Reserved
			for {
				old := peak.Load()
				if held <= old || peak.CompareAndSwap(old, held) {
					break
				}
			}
			if held > s.Limit {
				breach.Store(true)
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
			now := clk.now()
			res, err := b.Reserve(subject, estimate, now)
			if err != nil {
				if !errors.Is(err, quota.ErrBudgetExceeded) {
					t.Errorf("Reserve failed with an unexpected error: %v", err)
					return
				}
				// DESIGN §6.4: exceeding a budget is terminal, never a fallback
				// condition. A caller that retried elsewhere would overspend.
				if !quota.IsTerminal(err) {
					t.Error("a budget refusal must be terminal")
				}
				refused.Add(1)
				return
			}
			// The hold is soft at the gate and hard once capacity is acquired.
			if err := b.Harden(res.ID, now); err != nil {
				t.Errorf("Harden: %v", err)
				return
			}
			if err := b.Settle(res.ID, actual, clk.now()); err != nil {
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

	snap := b.Snapshot(subject, clk.now())
	if breach.Load() {
		t.Errorf("spent+reserved exceeded the limit during the run (peak %d, limit %d)", peak.Load(), limit)
	}
	if snap.Spent > limit {
		t.Errorf("settled spend %d exceeds the limit %d", snap.Spent, limit)
	}
	if got := spent.Load(); got != snap.Spent {
		t.Errorf("the budget recorded %d but the requests settled %d", snap.Spent, got)
	}
	if snap.Reserved != 0 || b.Outstanding() != 0 {
		t.Errorf("reservations leaked: reserved=%d outstanding=%d", snap.Reserved, b.Outstanding())
	}

	// The cap has to have bitten, or the test proved only that 100 small
	// requests fit in a large budget.
	if refused.Load() == 0 {
		t.Fatalf("no request was refused; the limit %d did not constrain %d requests of %d",
			limit, concurrent, estimate)
	}
	if admitted.Load()+refused.Load() != concurrent {
		t.Fatalf("admitted %d + refused %d != %d", admitted.Load(), refused.Load(), concurrent)
	}
	t.Logf("admitted %d, refused %d, settled %d of a %d limit (peak hold %d)",
		admitted.Load(), refused.Load(), snap.Spent, limit, peak.Load())

	t.Run("inverse: the same requests fit when the limit is large enough", func(t *testing.T) {
		// If the refusals above were caused by anything other than the ceiling,
		// raising the ceiling would not remove them.
		b, err := quota.NewBudget(quota.BudgetConfig{
			Period: quota.Daily, DefaultLimit: estimate * concurrent, Now: clk.now,
		})
		if err != nil {
			t.Fatal(err)
		}
		var ok atomic.Int64
		var wg sync.WaitGroup
		for range concurrent {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := b.Reserve(subject, estimate, clk.now())
				if err != nil {
					return
				}
				_ = b.Settle(res.ID, actual, clk.now())
				ok.Add(1)
			}()
		}
		wg.Wait()
		if ok.Load() != concurrent {
			t.Fatalf("%d of %d requests fit a budget sized for all of them", ok.Load(), concurrent)
		}
	})

	t.Run("an unsettled hold is reclaimed rather than locking the budget", func(t *testing.T) {
		// R1-5: without expiry, a process killed between reserve and settle
		// locks that amount forever and the budget is eventually exhausted by
		// money nobody spent.
		clk := newClock()
		b, err := quota.NewBudget(quota.BudgetConfig{
			Period: quota.Daily, DefaultLimit: estimate, Now: clk.now,
			SoftTTL: time.Minute, HardTTL: time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Reserve(subject, estimate, clk.now()); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Reserve(subject, estimate, clk.now()); !errors.Is(err, quota.ErrBudgetExceeded) {
			t.Fatalf("the budget should be fully held, got %v", err)
		}
		clk.advance(2 * time.Minute)
		n, reclaimed := b.SweepExpired(clk.now())
		if n != 1 || reclaimed != estimate {
			t.Fatalf("sweep reclaimed %d holds worth %d, want 1 worth %d", n, reclaimed, estimate)
		}
		if _, err := b.Reserve(subject, estimate, clk.now()); err != nil {
			t.Fatalf("the reclaimed budget is still locked: %v", err)
		}
	})
}
