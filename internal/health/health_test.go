package health

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func clock(start time.Time) (func() time.Time, func(time.Duration)) {
	var mu sync.Mutex
	now := start
	return func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}, func(d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(d)
		}
}

func fail() Outcome    { return Outcome{Err: errors.New("boom"), Failure: true} }
func succeed() Outcome { return Outcome{Total: 10 * time.Millisecond} }

// Cooldown is the load-bearing part of routing reliability: without it a dead
// backend keeps taking its full share of traffic.
func TestOpensAfterThresholdAndStopsSelection(t *testing.T) {
	now, advance := clock(time.Unix(1700000000, 0))
	tr := New(Options{FailureThreshold: 3, Cooldown: 5 * time.Second, Now: now})

	for i := 0; i < 2; i++ {
		tr.Report("d1", fail())
		if !tr.Allow("d1") {
			t.Fatalf("closed too early, after %d failures", i+1)
		}
	}
	tr.Report("d1", fail())

	if tr.Allow("d1") {
		t.Fatal("still selectable after crossing the failure threshold")
	}
	if got := tr.Stats("d1").State; got != Open {
		t.Fatalf("state %v, want open", got)
	}

	advance(4 * time.Second)
	if tr.Allow("d1") {
		t.Fatal("selectable before the cooldown elapsed")
	}
	advance(2 * time.Second)
	if !tr.Allow("d1") {
		t.Fatal("not probed after the cooldown elapsed")
	}
}

func TestHalfOpenAllowsExactlyOneProbe(t *testing.T) {
	now, advance := clock(time.Unix(1700000000, 0))
	tr := New(Options{FailureThreshold: 1, Cooldown: time.Second, HalfOpenProbes: 1, Now: now})

	tr.Report("d1", fail())
	advance(2 * time.Second)

	if !tr.Allow("d1") {
		t.Fatal("first probe refused")
	}
	if tr.Allow("d1") {
		t.Fatal("second request passed while half-open; only one probe is allowed")
	}
}

func TestHalfOpenSuccessCloses(t *testing.T) {
	now, advance := clock(time.Unix(1700000000, 0))
	tr := New(Options{FailureThreshold: 1, Cooldown: time.Second, Now: now})

	tr.Report("d1", fail())
	advance(2 * time.Second)
	tr.Allow("d1")
	tr.Report("d1", succeed())

	if got := tr.Stats("d1").State; got != Closed {
		t.Fatalf("state %v after a successful probe, want closed", got)
	}
	if !tr.Allow("d1") || !tr.Allow("d1") {
		t.Fatal("traffic not restored after recovery")
	}
}

// A failed probe must buy a full cooldown, not a stream of probes against a
// backend that is still down.
func TestHalfOpenFailureReopensForFullCooldown(t *testing.T) {
	now, advance := clock(time.Unix(1700000000, 0))
	tr := New(Options{FailureThreshold: 1, Cooldown: 5 * time.Second, Now: now})

	tr.Report("d1", fail())
	advance(6 * time.Second)
	tr.Allow("d1")
	tr.Report("d1", fail())

	if tr.Allow("d1") {
		t.Fatal("probed again immediately after the probe failed")
	}
	advance(4 * time.Second)
	if tr.Allow("d1") {
		t.Fatal("cooldown was not restarted from the probe failure")
	}
	advance(2 * time.Second)
	if !tr.Allow("d1") {
		t.Fatal("never recovered")
	}
}

// Consecutive, not cumulative: an occasional failure among successes must not
// accumulate into an outage.
func TestSuccessResetsConsecutiveFailures(t *testing.T) {
	tr := New(Options{FailureThreshold: 3})
	for i := 0; i < 10; i++ {
		tr.Report("d1", fail())
		tr.Report("d1", fail())
		tr.Report("d1", succeed())
	}
	if !tr.Allow("d1") {
		t.Fatal("opened on cumulative rather than consecutive failures")
	}
	if got := tr.Stats("d1").Failures; got != 20 {
		t.Fatalf("failure count %d, want 20 — counters must still record them", got)
	}
}

// A 429 is an error to the caller but says nothing about liveness, so the
// router decides what counts against availability.
func TestNonFailureErrorsDoNotOpen(t *testing.T) {
	tr := New(Options{FailureThreshold: 2})
	for i := 0; i < 20; i++ {
		tr.Report("d1", Outcome{Err: errors.New("429"), Failure: false})
	}
	if !tr.Allow("d1") {
		t.Fatal("rate limiting opened the circuit; it is not a liveness signal")
	}
}

func TestMarkUnavailableHonoursLongerWait(t *testing.T) {
	now, advance := clock(time.Unix(1700000000, 0))
	tr := New(Options{Cooldown: 5 * time.Second, Now: now})

	tr.MarkUnavailable("d1", time.Minute)
	advance(30 * time.Second)
	if tr.Allow("d1") {
		t.Fatal("recovered after 30s despite a 60s retry-after")
	}
	advance(31 * time.Second)
	if !tr.Allow("d1") {
		t.Fatal("never recovered after the retry-after elapsed")
	}
}

func TestMarkHealthy(t *testing.T) {
	tr := New(Options{FailureThreshold: 1})
	tr.Report("d1", fail())
	if tr.Allow("d1") {
		t.Fatal("expected open")
	}
	tr.MarkHealthy("d1")
	if !tr.Allow("d1") {
		t.Fatal("MarkHealthy did not restore selection")
	}
}

// An unproven deployment must not win a latency comparison on ignorance alone.
func TestNoSampleMeansNoOpinion(t *testing.T) {
	tr := New(Options{})
	if got := tr.TTFT("never-used"); got != 0 {
		t.Fatalf("TTFT %v for an unused deployment, want zero", got)
	}
	if got := tr.TokensPerSec("never-used"); got != 0 {
		t.Fatalf("TPS %v for an unused deployment, want zero", got)
	}
}

func TestEWMASeedsThenSmooths(t *testing.T) {
	tr := New(Options{EWMAAlpha: 0.5})
	tr.Report("d1", Outcome{TTFT: 100 * time.Millisecond, Total: 200 * time.Millisecond})
	if got := tr.TTFT("d1"); got != 100*time.Millisecond {
		t.Fatalf("first sample %v, want it seeded exactly at 100ms", got)
	}
	tr.Report("d1", Outcome{TTFT: 300 * time.Millisecond, Total: 400 * time.Millisecond})
	if got := tr.TTFT("d1"); got != 200*time.Millisecond {
		t.Fatalf("smoothed %v, want 200ms at alpha 0.5", got)
	}
}

// Generation rate must exclude time to first token, or a backend with a long
// queue looks like a slow generator and gets penalised for the wrong thing.
func TestTokensPerSecExcludesTTFT(t *testing.T) {
	tr := New(Options{EWMAAlpha: 1})

	// Same generation phase (1s for 100 tokens), very different queueing.
	tr.Report("fast-queue", Outcome{TTFT: 10 * time.Millisecond, Total: 1010 * time.Millisecond, OutputTokens: 100})
	tr.Report("slow-queue", Outcome{TTFT: 2 * time.Second, Total: 3 * time.Second, OutputTokens: 100})

	a, b := tr.TokensPerSec("fast-queue"), tr.TokensPerSec("slow-queue")
	if diff := a - b; diff > 1 || diff < -1 {
		t.Fatalf("generation rates differ (%.2f vs %.2f) though only queueing differed", a, b)
	}
	// And TTFT still distinguishes them, since that is the other question.
	if tr.TTFT("fast-queue") >= tr.TTFT("slow-queue") {
		t.Fatal("TTFT failed to separate a queued backend from a prompt one")
	}
}

func TestConcurrentReportAndAllow(t *testing.T) {
	tr := New(Options{FailureThreshold: 5, Cooldown: time.Millisecond})
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				id := "d" + string(rune('0'+g%4))
				tr.Allow(id)
				if i%7 == 0 {
					tr.Report(id, fail())
				} else {
					tr.Report(id, Outcome{TTFT: time.Millisecond, Total: 10 * time.Millisecond, OutputTokens: 50})
				}
			}
		}(g)
	}
	wg.Wait()

	total := int64(0)
	for _, id := range tr.IDs() {
		total += tr.Stats(id).Requests
	}
	if want := int64(32 * 500); total != want {
		t.Fatalf("recorded %d requests, want %d — counters lost updates", total, want)
	}
}

func BenchmarkAllow(b *testing.B) {
	tr := New(Options{})
	tr.Report("d1", succeed())
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tr.Allow("d1")
		}
	})
}

func BenchmarkReport(b *testing.B) {
	tr := New(Options{})
	o := Outcome{TTFT: time.Millisecond, Total: 10 * time.Millisecond, OutputTokens: 100}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tr.Report("d1", o)
		}
	})
}
