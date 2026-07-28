package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/capacity"
)

// TestConcurrentRoutingUnderFlappingHealth is the concurrency gate. It routes
// from many goroutines across two groups of one class while a separate
// goroutine opens and closes circuits underneath them, and while a third
// expires session pins by moving the clock.
//
// What it asserts, beyond -race finding nothing:
//   - every decision that was handed out is internally consistent;
//   - no reservation leaks, whatever mixture of success, failure and refusal
//     the run produced;
//   - a refusal is always a *router.Error with a code, never a bare panic or a
//     nil decision with a nil error.
func TestConcurrentRoutingUnderFlappingHealth(t *testing.T) {
	cfg := Config{
		Groups: []Group{
			{Name: "g", Class: "c",
				Strategy: []Strategy{StrategyPrefixSticky, StrategySticky, StrategyLeastBusy,
					StrategyRoundRobin},
				Deployments: []Deployment{
					dep("g1", "p1", "vllm", "qwen3.5:397b", "k1", "k2"),
					dep("g2", "p2", "sglang", "qwen3.5:397b", "k3"),
					dep("g3", "p3", "openai", "zai:glm-5.1", "k4"),
				}},
			{Name: "h", Class: "c", Deployments: []Deployment{
				dep("h1", "p4", "anthropic", "claude-y", "k5"),
			}},
		},
		Sticky:     StickyConfig{Enabled: true, TTL: 250 * time.Millisecond},
		Prefix:     PrefixConfig{Enabled: true},
		Fallback:   FallbackConfig{On: DefaultChains(), MaxHops: 2, Budget: time.Minute},
		OnCapacity: capacity.Spill,
	}
	cc := capacity.Config{
		SweepInterval: -1,
		Models: []capacity.ModelLimit{
			{Provider: "p1", Model: "qwen3.5:397b", Max: 4},
			{Provider: "p2", Model: "qwen3.5:397b", Max: 4},
			{Provider: "p3", Model: "zai:glm-5.1", Max: 4},
			{Provider: "p4", Model: "claude-y", Max: 4},
		},
	}
	h := newHarness(t, cfg, harnessOpts{capacity: cc, prefixOn: true, prefixTL: time.Second,
		pricing: planAndTokens})

	const workers = 24
	const iters = 300

	var stop atomic.Bool
	var bg, wg sync.WaitGroup

	// Health flapping, continuously, on every deployment.
	bg.Add(1)
	go func() {
		defer bg.Done()
		ids := []string{"g1", "g2", "g3", "h1"}
		for i := 0; !stop.Load(); i++ {
			id := ids[i%len(ids)]
			if i%2 == 0 {
				h.health.MarkUnavailable(id, 5*time.Millisecond)
			} else {
				h.health.MarkHealthy(id)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// The clock moves, so session pins expire mid-flight.
	bg.Add(1)
	go func() {
		defer bg.Done()
		for !stop.Load() {
			h.clock.advance(20 * time.Millisecond)
			time.Sleep(time.Millisecond)
		}
	}()

	// The prefix table is swept from another goroutine at the same time.
	bg.Add(1)
	go func() {
		defer bg.Done()
		for !stop.Load() {
			h.r.Purge()
			time.Sleep(2 * time.Millisecond)
		}
	}()

	var routed, refused atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			model := []string{"g", "h"}[w%2]
			body := "conversation-" + itoa(w%7)
			for i := 0; i < iters; i++ {
				req := Request{
					Model:         model,
					Principal:     "key-" + itoa(w%3),
					Tenant:        "tenant-" + itoa(w%2),
					Session:       "sess-" + itoa(w),
					Digests:       prefixFor(model, body),
					PriorityClass: []string{"realtime", "interactive", "batch"}[i%3],
					InputTokens:   1000,
				}
				d, err := h.r.Route(context.Background(), req)
				if err != nil {
					refused.Add(1)
					var re *Error
					if !errors.As(err, &re) || re.Code == "" {
						t.Errorf("a refusal must be a coded *router.Error: %v", err)
					}
					continue
				}
				routed.Add(1)
				if d.Deployment == "" || d.Provider == "" || d.UpstreamModel == "" {
					t.Errorf("incomplete decision: %+v", d)
					h.r.Report(d, Outcome{})
					continue
				}
				if d.Reason == "" {
					t.Errorf("every decision must carry a reason: %+v", d)
				}
				switch i % 5 {
				case 0:
					h.r.Report(d, Outcome{Err: errFake, Status: 500, Total: time.Millisecond})
					// One fail-back hop, which may itself be refused.
					if d2, err2 := h.r.Route(context.Background(), Request{
						Model: model, Principal: req.Principal, Previous: d,
					}); err2 == nil {
						h.r.Report(d2, Outcome{Total: time.Millisecond})
					}
				case 1:
					h.r.Report(d, Outcome{Err: errFake, Status: 429})
				default:
					h.r.Report(d, Outcome{TTFT: time.Millisecond, Total: 3 * time.Millisecond,
						OutputTokens: 32})
				}
			}
		}(w)
	}

	wg.Wait()
	stop.Store(true)
	bg.Wait()

	if routed.Load() == 0 {
		t.Fatal("the run routed nothing; the test proved only that refusals are cheap")
	}
	if snap := h.broker.Snapshot(); snap.Reservations != 0 {
		t.Fatalf("Report must release every reservation: %d still live", snap.Reservations)
	}
	if snap := h.broker.Snapshot(); snap.Waiting != 0 {
		t.Fatalf("no waiter should remain: %d", snap.Waiting)
	}
	t.Logf("routed=%d refused=%d", routed.Load(), refused.Load())
}

// TestReportReleasesTheReservationExactlyOnce: releasing is idempotent, and a
// caller that reports twice must not return capacity twice.
func TestReportReleasesTheReservationExactlyOnce(t *testing.T) {
	cfg := Config{Groups: []Group{{Name: "m", Deployments: []Deployment{
		{ID: "d1", Provider: "p", Kind: "openai", UpstreamModel: "m",
			Credentials: []Credential{{ID: "k", MaxConcurrent: 1}}},
	}}}}
	h := newHarness(t, cfg, harnessOpts{})

	d := h.route(Request{Model: "m"})
	h.ok(d)
	h.ok(d) // a double report must be harmless

	if snap := h.broker.Snapshot(); snap.Reservations != 0 {
		t.Fatalf("want 0 live reservations, got %d", snap.Reservations)
	}
	// The single slot is available again, and only once.
	a := h.route(Request{Model: "m"})
	if _, err := h.r.Route(context.Background(), Request{Model: "m"}); err == nil {
		t.Fatal("a double release would have handed out a second slot on a limit of one")
	}
	h.ok(a)
}
