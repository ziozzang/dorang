package router

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/health"
	"github.com/ziozzang/dorang/internal/prefix"
	"github.com/ziozzang/dorang/internal/pricing"
)

// benchCatalog prices all ten candidates so lowest_cost has a real opinion on
// every one of them, which is the expensive case: a cost-based router prices
// every candidate on every request (§8.2).
func benchCatalog(n int) string {
	var b strings.Builder
	b.WriteString("currency: USD\nrules:\n")
	for i := 0; i < n; i++ {
		b.WriteString("  - { id: r")
		b.WriteString(itoa(i))
		b.WriteString(", match: { deployment: d")
		b.WriteString(itoa(i))
		b.WriteString(" }, unit: per_1m_tokens, input: \"0.")
		b.WriteString(itoa(10 + i))
		b.WriteString("\", output: \"1.")
		b.WriteString(itoa(10 + i))
		b.WriteString("\" }\n")
	}
	return b.String()
}

// benchRouter builds the §15.1 warm-local shape: ten candidates, prefix
// affinity on, capacity counted on every axis, prices loaded, and the default
// tie-break chain of §4.2.
func benchRouter(b *testing.B, n int, chain []Strategy) (*Router, *capacity.Broker) {
	b.Helper()
	g := Group{Name: "model-x", Class: "chat-large", Strategy: chain}
	var models []capacity.ModelLimit
	for i := 0; i < n; i++ {
		id := "d" + itoa(i)
		g.Deployments = append(g.Deployments, Deployment{
			ID: id, Provider: "prov" + itoa(i), Kind: "vllm",
			UpstreamModel: "qwen3.5:397b", ContextWindow: 128000, Weight: i%3 + 1,
			Credentials: []Credential{
				{ID: id + "-a", CapacityGroup: "acct" + itoa(i), MaxConcurrent: 64},
				{ID: id + "-b", CapacityGroup: "acct" + itoa(i), MaxConcurrent: 64},
			},
		})
		models = append(models, capacity.ModelLimit{
			Provider: "prov" + itoa(i), Model: "qwen3.5:397b", Max: 128})
	}

	broker := capacity.New(capacity.Config{SweepInterval: -1, Models: models})
	b.Cleanup(broker.Close)

	cat, err := pricing.ParseCatalog([]byte(benchCatalog(n)))
	if err != nil {
		b.Fatalf("pricing: %v", err)
	}
	r, err := New(Config{
		Groups:     []Group{g},
		Aliases:    map[string]string{"model-large": "model-x"},
		Sticky:     StickyConfig{Enabled: true, TTL: time.Hour},
		Prefix:     PrefixConfig{Enabled: true},
		Fallback:   FallbackConfig{On: DefaultChains(), MaxHops: 3},
		OnCapacity: capacity.Spill,
	}, Deps{
		Capacity: broker,
		Health:   health.New(health.Options{}),
		Prefix:   prefix.NewTable(prefix.Options{TTL: time.Hour}),
		Pricing:  cat,
	})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	return r, broker
}

// BenchmarkRoute measures the routing decision itself with ten candidates.
// DESIGN §15.1's warm-local p50 is 480 µs for the whole gateway, measured, so
// this must be a small fraction of it — and is: testing/perf found that turning
// routing's inputs on and off (prefix affinity, a hundred pricing rules, a
// second deployment) does not move the assembled gateway's p50 outside noise.
func BenchmarkRoute(b *testing.B) {
	r, _ := benchRouter(b, 10, DefaultStrategy())
	ctx := context.Background()
	digests := prefixFor("model-x", strings.Repeat("conversation bytes ", 200))
	req := Request{
		Model: "model-large", Principal: "key-1", Tenant: "t", Session: "s",
		Digests: digests, PriorityClass: "interactive",
		InputTokens: 4096, MaxOutputTokens: 1024,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, err := r.Route(ctx, req)
		if err != nil {
			b.Fatalf("Route: %v", err)
		}
		d.Reservation.Release()
	}
}

// BenchmarkRouteAndReport is the whole loop: the decision plus the feedback
// that closes it, which is what a request actually costs.
func BenchmarkRouteAndReport(b *testing.B) {
	r, _ := benchRouter(b, 10, DefaultStrategy())
	ctx := context.Background()
	digests := prefixFor("model-x", strings.Repeat("conversation bytes ", 200))
	req := Request{
		Model: "model-large", Principal: "key-1", Tenant: "t", Session: "s",
		Digests: digests, PriorityClass: "interactive",
		InputTokens: 4096, MaxOutputTokens: 1024,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, err := r.Route(ctx, req)
		if err != nil {
			b.Fatalf("Route: %v", err)
		}
		r.Report(d, Outcome{TTFT: time.Millisecond, Total: 5 * time.Millisecond, OutputTokens: 64})
	}
}

// BenchmarkRouteNoAffinity isolates the ranking from the affinity lookups, so a
// regression can be attributed to one or the other.
func BenchmarkRouteNoAffinity(b *testing.B) {
	r, _ := benchRouter(b, 10, []Strategy{StrategyLowestCost, StrategyLeastBusy})
	ctx := context.Background()
	req := Request{Model: "model-x", Principal: "key-1",
		InputTokens: 4096, MaxOutputTokens: 1024}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, err := r.Route(ctx, req)
		if err != nil {
			b.Fatalf("Route: %v", err)
		}
		d.Reservation.Release()
	}
}

// BenchmarkRouteRoundRobin is the cheapest realistic chain, for the floor.
func BenchmarkRouteRoundRobin(b *testing.B) {
	r, _ := benchRouter(b, 10, []Strategy{StrategyRoundRobin})
	ctx := context.Background()
	req := Request{Model: "model-x", Principal: "key-1"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, err := r.Route(ctx, req)
		if err != nil {
			b.Fatalf("Route: %v", err)
		}
		d.Reservation.Release()
	}
}

// BenchmarkRouteParallel checks the decision path scales across cores, since
// the router is on every request of every connection.
func BenchmarkRouteParallel(b *testing.B) {
	r, _ := benchRouter(b, 10, DefaultStrategy())
	digests := prefixFor("model-x", strings.Repeat("conversation bytes ", 200))

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		req := Request{Model: "model-x", Principal: "key-1", Digests: digests,
			InputTokens: 4096, MaxOutputTokens: 1024}
		for pb.Next() {
			d, err := r.Route(ctx, req)
			if err != nil {
				b.Fatalf("Route: %v", err)
			}
			d.Reservation.Release()
		}
	})
}

// BenchmarkRouteParallelByCandidates is the shape of the routing decision's
// cost rather than its value: what one costs on every core as the GROUP grows.
//
// Measured on a 16-core machine: 1.4 µs/op at one candidate, 1.9 µs at four,
// 3.8 µs at sixteen. The growth is superlinear in the last step, and the reason
// is the capacity broker's single mutex: `least_busy` — which is in the default
// chain — asks [capacity.Broker.InUse] once per candidate, so a group of N
// takes N turns at a lock that TryAcquire and Release are also queuing for.
//
// # Batching it was tried and is slower
//
// The obvious repair is to answer all N questions under one acquisition, and it
// was written, measured and reverted. Two shapes were tried:
//
//   - Both axes for every candidate in one list: 3.8 -> 7.0 µs/op at N=16. It
//     doubles the map lookups performed while the lock is HELD, and on a
//     saturated mutex hold time is what decides throughput, not turn count.
//   - The preferred axis first and the fallback only for what missed, so the
//     lookup count is unchanged: 4.8 -> 6.1 µs/op at N=16 (GOMAXPROCS=4,
//     minimum of six interleaved runs). The staging is the cost — the query
//     carries strings, so writing it into a reused heap slice is a GC write
//     barrier per candidate. CPU profiles confirm it: the batched call uses
//     HALF the CPU of the loop it replaced and still finishes fewer operations
//     per second.
//
// So the per-candidate acquisition stays, and this benchmark records the shape
// rather than guarding a fix. On the assembled gateway it is not the ceiling:
// throughput was flat at 19-20k req/s from one deployment to eight, because a
// real request spends ~400 µs elsewhere and touches this lock about 1% of the
// time, where the microbenchmark touches it continuously.
func BenchmarkRouteParallelByCandidates(b *testing.B) {
	for _, n := range []int{1, 4, 16} {
		b.Run("n="+itoa(n), func(b *testing.B) {
			r, _ := benchRouter(b, n, DefaultStrategy())
			digests := prefixFor("model-x", strings.Repeat("conversation bytes ", 200))
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				ctx := context.Background()
				req := Request{Model: "model-x", Principal: "key-1", Digests: digests,
					InputTokens: 4096, MaxOutputTokens: 1024}
				for pb.Next() {
					d, err := r.Route(ctx, req)
					if err != nil {
						b.Fatalf("Route: %v", err)
					}
					r.Report(d, Outcome{TTFT: time.Millisecond, Total: 5 * time.Millisecond})
				}
			})
		})
	}
}
