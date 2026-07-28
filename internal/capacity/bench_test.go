package capacity

import (
	"context"
	"testing"
)

func benchConfig() Config {
	return Config{
		Global:           1 << 20,
		Routes:           map[string]int{"plan-a": 1 << 20},
		ProviderGroups:   map[string]int{"pool": 1 << 20},
		CredentialGroups: map[string]int{"acct-1": 1 << 20},
		Models:           []ModelLimit{{Provider: "plan-a", Model: "m1", Max: 1 << 20}},
		Principals:       map[string]int{"default": 1 << 20},
		SweepInterval:    -1, // no background goroutine perturbing the numbers
	}
}

func benchRequest() Request {
	return Request{
		Provider:      "plan-a",
		Model:         "m1",
		ProviderGroup: "pool",
		PrincipalID:   "user-1",
		Candidates:    []Candidate{{ID: "acct-1", CapacityGroup: "acct-1", MaxConcurrent: 1 << 20}},
	}
}

// BenchmarkAcquireRelease measures the uncontended hot path across all seven
// axes: one lock, seven map lookups, seven increments, and the mirror on release.
func BenchmarkAcquireRelease(b *testing.B) {
	br := New(benchConfig())
	defer br.Close()
	req := benchRequest()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := br.Acquire(ctx, req)
		if err != nil {
			b.Fatal(err)
		}
		res.Release()
	}
}

// BenchmarkTryAcquireRelease isolates the non-blocking path.
func BenchmarkTryAcquireRelease(b *testing.B) {
	br := New(benchConfig())
	defer br.Close()
	req := benchRequest()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, ok := br.TryAcquire(req)
		if !ok {
			b.Fatal("refused")
		}
		res.Release()
	}
}

// BenchmarkAcquireSingleAxis is the floor: one counted axis, nothing else.
func BenchmarkAcquireSingleAxis(b *testing.B) {
	br := New(Config{Global: 1 << 20, SweepInterval: -1})
	defer br.Close()
	req := Request{Provider: "plan-a"}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := br.Acquire(ctx, req)
		if err != nil {
			b.Fatal(err)
		}
		res.Release()
	}
}

// BenchmarkAcquireContended runs every goroutine through a limit of 4, so most
// acquisitions go through the wait queue and the targeted-wakeup path.
func BenchmarkAcquireContended(b *testing.B) {
	br := New(Config{
		Models:        []ModelLimit{{Provider: "plan-a", Model: "m1", Max: 4}},
		SweepInterval: -1,
	})
	defer br.Close()
	req := Request{Provider: "plan-a", Model: "m1"}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := br.Acquire(ctx, req)
			if err != nil {
				b.Fatal(err)
			}
			res.Release()
		}
	})
}

// BenchmarkAcquireContendedSpill adds candidate spilling to the contended path.
func BenchmarkAcquireContendedSpill(b *testing.B) {
	br := New(Config{
		CredentialGroups: map[string]int{"acct-1": 2, "acct-2": 2},
		Models:           []ModelLimit{{Provider: "plan-a", Model: "m1", Max: 4}},
		SweepInterval:    -1,
	})
	defer br.Close()
	req := Request{
		Provider:   "plan-a",
		Model:      "m1",
		OnCapacity: Spill,
		Candidates: []Candidate{
			{ID: "acct-1", CapacityGroup: "acct-1", MaxConcurrent: 2},
			{ID: "acct-2", CapacityGroup: "acct-2", MaxConcurrent: 2},
		},
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := br.Acquire(ctx, req)
			if err != nil {
				b.Fatal(err)
			}
			res.Release()
		}
	})
}
