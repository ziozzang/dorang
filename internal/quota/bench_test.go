package quota

import (
	"context"
	"testing"
	"time"
)

// BenchmarkMeterCheck is the admission path: one pass over the rules, each a
// running total rather than a sum over the ring.
func BenchmarkMeterCheck(b *testing.B) {
	m, err := NewMeter(MeterConfig{Rules: []Rule{
		{Window: Rolling(5 * time.Hour), Metric: MetricCostUSD, Limit: NanoUSD(1e6)},
		{Window: Rolling(time.Hour), Metric: MetricRequests, Limit: 1 << 40},
		{Window: Daily, Metric: MetricTokensTotal, Limit: 1 << 40},
	}, Now: func() time.Time { return base }})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if d := m.Check(base); !d.Allow {
			b.Fatal("refused")
		}
	}
}

func BenchmarkMeterRecord(b *testing.B) {
	m, err := NewMeter(MeterConfig{Rules: []Rule{
		{Window: Rolling(5 * time.Hour), Metric: MetricCostUSD, Limit: NanoUSD(1e12)},
		{Window: Monthly, Metric: MetricTokensTotal, Limit: 1 << 60},
	}, Now: func() time.Time { return base }})
	if err != nil {
		b.Fatal(err)
	}
	u := Usage{CostNanoUSD: 1200, TokensInput: 100, TokensOutput: 50, Requests: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		m.Record(base, u)
	}
}

// BenchmarkMeterQueryLongWindow shows that query cost does not grow with the
// window: a 5-hour ring (300 buckets) and a 24-hour one (1440) cost the same,
// because the total is maintained rather than summed.
func BenchmarkMeterQueryLongWindow(b *testing.B) {
	for _, w := range []Window{Rolling(time.Hour), Rolling(5 * time.Hour), Rolling(24 * time.Hour)} {
		b.Run(w.String(), func(b *testing.B) {
			m, err := NewMeter(MeterConfig{
				Rules: []Rule{{Window: w, Metric: MetricRequests, Limit: 1 << 40}},
				Now:   func() time.Time { return base },
			})
			if err != nil {
				b.Fatal(err)
			}
			// Fill the whole ring so a naive implementation would have to walk it.
			for i := range w.buckets() {
				m.Record(base.Add(time.Duration(i)*time.Minute), Usage{Requests: 1})
			}
			now := base.Add(time.Duration(w.buckets()) * time.Minute)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.Check(now)
			}
		})
	}
}

// BenchmarkMeterUrgency is the ranking path: it runs once per candidate per
// request, so it holds the meter's lock for a handful of divisions and a hash
// over two short ids, and allocates nothing.
func BenchmarkMeterUrgency(b *testing.B) {
	m, err := NewMeter(MeterConfig{Rules: []Rule{
		{Window: Weekly, Metric: MetricTokensTotal, Limit: 1 << 40, Resets: true},
		{Window: Rolling(5 * time.Hour), Metric: MetricCostUSD, Limit: NanoUSD(1e6)},
		{Window: Daily, Metric: MetricRequests, Limit: 1 << 30, Resets: true},
	}, Now: func() time.Time { return base }})
	if err != nil {
		b.Fatal(err)
	}
	m.Record(base, Usage{TokensInput: 1 << 20, Requests: 1, CostNanoUSD: NanoUSD(1)})
	in := UrgencyInput{Subject: "cred-bench", NodeID: "node-bench", Occupancy: 0.25}
	now := base.Add(time.Hour)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if m.Urgency(now, in) <= 0 {
			b.Fatal("no urgency")
		}
	}
}

func BenchmarkBudgetReserveSettle(b *testing.B) {
	bg, err := NewBudget(BudgetConfig{
		Period: Monthly, DefaultLimit: 1 << 60, Now: func() time.Time { return base },
	})
	if err != nil {
		b.Fatal(err)
	}
	s := Subject{Kind: "key", ID: "k"}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		r, err := bg.Reserve(s, 1000, base)
		if err != nil {
			b.Fatal(err)
		}
		if err := bg.Harden(r.ID, base); err != nil {
			b.Fatal(err)
		}
		if err := bg.Settle(r.ID, 500, base); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCoordinatorCharge(b *testing.B) {
	shared := NewMemShared(func() time.Time { return base })
	cfgs := []CoordinatorConfig{
		{Mode: ModeLocal, Now: func() time.Time { return base }},
		{Mode: ModeSharedRedis, Clustered: true, Nodes: 2, Shared: shared, Now: func() time.Time { return base }},
		{Mode: ModeLeased, Clustered: true, Nodes: 2, Shared: shared, BlockSize: 4096,
			LeaseTTL: time.Hour, Now: func() time.Time { return base }},
	}
	for _, cfg := range cfgs {
		b.Run(cfg.Mode.String(), func(b *testing.B) {
			co, err := NewCoordinator(cfg)
			if err != nil {
				b.Fatal(err)
			}
			defer co.Close()
			k := NewKey("credential", "bench-"+cfg.Mode.String(), Rolling(time.Hour), MetricRequests, base)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := co.Charge(ctx, k, int64(b.N)+1<<40, 1, base); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
