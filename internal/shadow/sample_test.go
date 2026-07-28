package shadow

import (
	"fmt"
	"math"
	"testing"
)

func TestSamplingIsDeterministicInTheRequestID(t *testing.T) {
	// A retry carries the same id (COMPATIBILITY §7.8). If the decision were
	// random, a retried call would be sampled twice and charged twice against a
	// ceiling that is meant to bound a day of comparison.
	s := newSampler(0.37)
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("req-%d", i)
		want := s.admit(id)
		for k := 0; k < 5; k++ {
			if got := s.admit(id); got != want {
				t.Fatalf("id %q sampled %v then %v", id, want, got)
			}
		}
	}
}

func TestSamplingDistributionMatchesTheConfiguredRate(t *testing.T) {
	const n = 200000
	for _, rate := range []float64{0.01, 0.05, 0.25, 0.5} {
		s := newSampler(rate)
		hits := 0
		for i := 0; i < n; i++ {
			if s.admit(fmt.Sprintf("cid-%d-%x", i, i*2654435761)) {
				hits++
			}
		}
		got := float64(hits) / n
		// Four standard deviations of a binomial, plus a floor so a very low
		// rate is not judged by an impossibly tight band.
		sd := math.Sqrt(rate * (1 - rate) / n)
		tol := 4*sd + 0.001
		if math.Abs(got-rate) > tol {
			t.Errorf("rate %v: sampled %v (%d/%d), outside ±%v", rate, got, hits, n, tol)
		}
	}
}

func TestSamplingIsBlindToEverythingButTheID(t *testing.T) {
	// The sampler's whole interface is the id. This test exists to make the
	// property a compile-time one: if somebody adds a cost, a size or a model
	// parameter to admit, this stops building — and the moment the sampler can
	// see cost, it can prefer cheap traffic, which makes both the ceiling and
	// the coverage read better than they are.
	var _ func(string) bool = newSampler(0.5).admit
}

func TestSamplingEndpointsAreExact(t *testing.T) {
	zero, one := newSampler(0), newSampler(1)
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("x%d", i)
		if zero.admit(id) {
			t.Fatalf("rate 0 sampled %q", id)
		}
		if !one.admit(id) {
			t.Fatalf("rate 1 skipped %q", id)
		}
	}
}

func TestSamplingIsIndependentOfTheMeteringSampler(t *testing.T) {
	// Both samplers key on the request id. If they used the same hash, the
	// traced requests and the shadowed requests would be the same requests, and
	// both costs would land on one slice of traffic.
	s := newSampler(0.5)
	agree := 0
	const n = 20000
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id-%d", i)
		// The metering sampler's hash, reproduced: FNV of the bare id,
		// finalized, compared against the same threshold.
		meterPick := mix64(fnv64a("", id)) < s.threshold
		if s.admit(id) == meterPick {
			agree++
		}
	}
	// Independent decisions agree about half the time. Anything near total
	// agreement means the salts collapsed.
	frac := float64(agree) / n
	if frac > 0.55 || frac < 0.45 {
		t.Errorf("shadow and metering sampling agree %.3f of the time; want ~0.5", frac)
	}
}

// BenchmarkSample measures the one thing this package puts on the request path.
//
// It is a hash of the request id and, for the sampled fraction, a clock read
// and two atomic loads. Everything else — the copy, the queue, the reference
// call, the diff, the report — is off it.
func BenchmarkSample(b *testing.B) {
	s := NewOff()
	s.opts.Mode = ModeCompare
	s.sampler = newSampler(0.05)
	ids := make([]string, 512)
	for i := range ids {
		ids[i] = fmt.Sprintf("01h%013x", i*2654435761)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Sample(ids[i&511], nil)
	}
}
