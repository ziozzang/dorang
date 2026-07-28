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
// DESIGN §15.1's warm-local p50 is 200 µs for the whole gateway, so this must
// be a small fraction of it.
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
