package health

import (
	"errors"
	"testing"
	"time"
)

// A half-open probe is an ordinary request, and an ordinary streaming request
// runs for minutes. While it is out every other caller used to be refused, so a
// deployment that had recovered stayed "out of service" for as long as its
// slowest answer took (gateway board #1623).
func TestALostHalfOpenProbeDoesNotBlockTheDeploymentForever(t *testing.T) {
	now, advance := clock(time.Unix(1700000000, 0))
	tr := New(Options{FailureThreshold: 1, Cooldown: 5 * time.Second, Now: now})

	tr.Report("d1", fail())
	advance(6 * time.Second)
	if !tr.Allow("d1") {
		t.Fatal("the probe was not admitted after the cooldown")
	}
	// The probe is a long stream and has not reported.
	advance(time.Second)
	if tr.Allow("d1") {
		t.Fatal("a second request ran beside a fresh probe")
	}
	advance(6 * time.Second)
	if !tr.Allow("d1") {
		t.Fatal("a probe that has been out for longer than the cooldown still blocks everyone")
	}
}

// A caller hanging up is not evidence about the deployment: three of them in a
// row used to open the circuit for every other caller.
func TestAbandonedAttemptsCountForNothing(t *testing.T) {
	now, _ := clock(time.Unix(1700000000, 0))
	tr := New(Options{FailureThreshold: 3, Cooldown: 5 * time.Second, Now: now})
	for i := 0; i < 10; i++ {
		tr.Report("d1", Outcome{Err: errors.New("context canceled"), Abandoned: true})
	}
	if s := tr.Stats("d1"); s.State != Closed || s.ConsecFails != 0 || s.Failures != 0 {
		t.Fatalf("abandoned attempts moved the circuit: %+v", s)
	}
}

func TestAnAbandonedProbeGivesItsSlotBack(t *testing.T) {
	now, advance := clock(time.Unix(1700000000, 0))
	tr := New(Options{FailureThreshold: 1, Cooldown: 5 * time.Second, Now: now})
	tr.Report("d1", fail())
	advance(6 * time.Second)
	if !tr.Allow("d1") {
		t.Fatal("no probe")
	}
	tr.Report("d1", Outcome{Err: errors.New("context canceled"), Abandoned: true})
	if !tr.Allow("d1") {
		t.Fatal("the probe slot was lost with the caller who held it")
	}
}

// Honouring Retry-After includes the answers shorter than the default cooldown.
func TestAShortRetryAfterIsHonouredBelowTheCooldown(t *testing.T) {
	now, advance := clock(time.Unix(1700000000, 0))
	tr := New(Options{Cooldown: 5 * time.Second, Now: now})
	tr.MarkUnavailable("d1", time.Second)
	if tr.Allow("d1") {
		t.Fatal("selectable at once after a 1s Retry-After")
	}
	advance(1100 * time.Millisecond)
	if !tr.Allow("d1") {
		t.Fatal("a 1s Retry-After kept the deployment out for the default 5s cooldown")
	}
}

func TestRemainingReasonAndForceProbe(t *testing.T) {
	now, advance := clock(time.Unix(1700000000, 0))
	tr := New(Options{Cooldown: 5 * time.Second, Now: now})
	tr.MarkUnavailable("d1", 20*time.Second)
	tr.SetReason("d1", "upstream_429, Retry-After 20s")
	if got := tr.Remaining("d1"); got != 20*time.Second {
		t.Fatalf("remaining %v, want 20s", got)
	}
	ds := tr.Details()
	if len(ds) != 1 || ds[0].State != Open || ds[0].Reason != "upstream_429, Retry-After 20s" {
		t.Fatalf("details %+v", ds)
	}
	if !tr.ForceProbe("d1", 2*time.Second) {
		t.Fatal("first forced probe refused")
	}
	if tr.ForceProbe("d1", 2*time.Second) {
		t.Fatal("two forced probes inside the gap")
	}
	advance(3 * time.Second)
	if !tr.ForceProbe("d1", 2*time.Second) {
		t.Fatal("forced probe refused after the gap")
	}
}
