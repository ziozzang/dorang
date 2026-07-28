package capacity

import (
	"context"
	"sync/atomic"
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

// ---------------------------------------------------------------------------
// the price of the W8 starvation guard
// ---------------------------------------------------------------------------

// softModes is the on/off pair every soft-reservation benchmark runs.
var softModes = []struct {
	name string
	mode SoftReservationMode
}{
	{"soft-on", SoftReservationsOn},
	{"soft-off", SoftReservationsOff},
}

// BenchmarkAcquireContendedSingleAxisSoft is the control. A single-axis waiter
// can never place a soft reservation — it fails a probe only when its one axis
// is full, and a full axis has nothing to set aside — so the two arms should be
// indistinguishable. If they are not, the guard is charging the common case for
// a problem the common case does not have.
func BenchmarkAcquireContendedSingleAxisSoft(b *testing.B) {
	for _, m := range softModes {
		m := m
		b.Run(m.name, func(b *testing.B) {
			br := New(Config{
				Models:           []ModelLimit{{Provider: "plan-a", Model: "m1", Max: 4}},
				SweepInterval:    -1,
				SoftReservations: m.mode,
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
			reportSoft(b, br)
		})
	}
}

// BenchmarkAcquireContendedTwoAxis is the worst realistic case for the guard:
// every request needs two tight axes, so every blocked waiter is a candidate to
// set one of them aside, and the axis that is set aside runs at limit-1 for as
// long as the claim stands.
func BenchmarkAcquireContendedTwoAxis(b *testing.B) {
	for _, m := range softModes {
		m := m
		b.Run(m.name, func(b *testing.B) {
			br := New(Config{
				Models:           []ModelLimit{{Provider: "plan-a", Model: "m1", Max: 4}},
				SweepInterval:    -1,
				SoftReservations: m.mode,
			})
			defer br.Close()
			req := Request{
				Provider:   "plan-a",
				Model:      "m1",
				Candidates: []Candidate{{ID: "acct-1", MaxConcurrent: 4}},
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
			reportSoft(b, br)
		})
	}
}

// BenchmarkAcquireContendedMixed is the workload W8 describes: a minority of
// two-axis requests against a majority of single-axis ones on each of the two
// axes. It is the shape in which the guard both earns its keep and costs
// something, so it is the number to quote.
func BenchmarkAcquireContendedMixed(b *testing.B) {
	for _, m := range softModes {
		m := m
		b.Run(m.name, func(b *testing.B) {
			br := New(Config{
				Models:           []ModelLimit{{Provider: "plan-a", Model: "m1", Max: 4}},
				SweepInterval:    -1,
				SoftReservations: m.mode,
			})
			defer br.Close()
			modelOnly := Request{Provider: "plan-a", Model: "m1"}
			keyOnly := Request{
				Provider:   "plan-a",
				Candidates: []Candidate{{ID: "acct-1", MaxConcurrent: 4}},
			}
			both := Request{
				Provider:   "plan-a",
				Model:      "m1",
				Candidates: []Candidate{{ID: "acct-1", MaxConcurrent: 4}},
			}
			ctx := context.Background()

			var seq atomic.Int64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				// One goroutine in four issues the two-axis request; the rest
				// keep one axis each saturated.
				kind := seq.Add(1) % 4
				req := modelOnly
				switch kind {
				case 1:
					req = keyOnly
				case 2:
					req = both
				}
				for pb.Next() {
					res, err := br.Acquire(ctx, req)
					if err != nil {
						b.Fatal(err)
					}
					res.Release()
				}
			})
			reportSoft(b, br)
		})
	}
}

// reportSoft publishes how much of the run the guard was actually responsible
// for, so a difference in ns/op can be attributed rather than guessed at.
func reportSoft(b *testing.B, br *Broker) {
	b.Helper()
	s := br.Snapshot()
	b.ReportMetric(float64(s.SoftReservations)/float64(b.N), "softres/op")
	b.ReportMetric(float64(s.Wakeups)/float64(maxU64(s.Grants, 1)), "probes/grant")
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
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
