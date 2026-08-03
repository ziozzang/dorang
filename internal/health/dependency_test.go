package health

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Every threshold here is about elapsed wall time, so the tests move the
// package's existing hand-driven clock rather than sleeping through it.
func depClock() (func() time.Time, func(time.Duration)) {
	return clock(time.Unix(1_800_000_000, 0))
}

func newDep(t *testing.T, now func() time.Time) *Dependency {
	t.Helper()
	return NewDependency(DependencyOptions{
		Name:        "store",
		MinOutage:   15 * time.Second,
		MinFailures: 3,
		Now:         now,
	})
}

func mustReady(t *testing.T, d *Dependency, why string) {
	t.Helper()
	if ok, reason := d.ReadyForWork(); !ok {
		t.Fatalf("%s: the node went unready (%s)", why, reason)
	}
}

func mustUnready(t *testing.T, d *Dependency, why string) {
	t.Helper()
	ok, reason := d.ReadyForWork()
	if ok {
		t.Fatalf("%s: the node is still accepting new work", why)
	}
	if reason == "" {
		t.Errorf("%s: unready with no reason", why)
	}
}

// TestFreshDependencyIsReady: nothing has been observed, so there is nothing to
// refuse on. A gate that starts closed would make every cold start fail its
// first readiness probe.
func TestFreshDependencyIsReady(t *testing.T) {
	now, _ := depClock()
	mustReady(t, newDep(t, now), "a dependency that has never been consulted")
}

// TestABlipDoesNotDrainTheNode is the half that stops this becoming a hair
// trigger, and it is the half that matters more.
//
// The store this models went away and came back on its own: the process
// reconnected transparently, its restart count stayed zero, and the metering
// buffered during the gap was written when the store returned. Draining a node
// through that costs capacity for nothing — and when every node shares one
// store, it drains the whole fleet at once for a two-second interruption none
// of them needed to be removed for.
func TestABlipDoesNotDrainTheNode(t *testing.T) {
	now, advance := depClock()
	d := newDep(t, now)

	// A busy node during a two-second interruption: far more than the three
	// failures the count threshold asks for, inside far less than the fifteen
	// seconds the duration threshold asks for.
	for i := 0; i < 200; i++ {
		d.Fail()
		advance(10 * time.Millisecond)
	}
	mustReady(t, d, "200 failures inside two seconds")

	if st := d.Stats(); st.Outages != 0 {
		t.Errorf("a blip was counted as %d outage(s)", st.Outages)
	}

	// And the store comes back.
	d.OK()
	mustReady(t, d, "after the store answered again")
}

// TestOneSlowFailureIsNotAnOutage is the other end of the same rule. A single
// call that hangs for the store timeout and then fails has produced one data
// point, not a verdict — the count threshold is what stops a lone long timeout
// satisfying the duration threshold by itself.
func TestOneSlowFailureIsNotAnOutage(t *testing.T) {
	now, advance := depClock()
	d := newDep(t, now)

	d.Fail()
	advance(time.Minute)
	mustReady(t, d, "one failure, however long ago")

	d.Fail()
	advance(time.Minute)
	mustReady(t, d, "two failures")
}

// TestSustainedOutageStopsNewWork is the defect: with the store stopped,
// readiness kept answering ready while every caller whose key this node had not
// recently seen got a 503 from a balancer that still believed in it.
func TestSustainedOutageStopsNewWork(t *testing.T) {
	now, advance := depClock()
	d := newDep(t, now)

	// A prober at a five-second cadence, which is what the wiring is expected
	// to look like.
	d.Fail() // t+0
	advance(5 * time.Second)
	d.Fail() // t+5
	advance(5 * time.Second)
	d.Fail() // t+10 — three failures, but only ten seconds of them
	mustReady(t, d, "ten seconds of failures")

	advance(5 * time.Second)
	d.Fail() // t+15 — four failures spanning fifteen seconds
	mustUnready(t, d, "fifteen seconds of unbroken failures")

	if st := d.Stats(); st.Outages != 1 || st.Failures != 4 || st.Outage != 15*time.Second {
		t.Errorf("Stats = %+v, want 1 outage of 4 failures over 15s", st)
	}
}

// TestRecoveryIsImmediateAndNeedsNoRestart is the other half of the fix, and it
// is why the gate is a gate and not a liveness signal. The dependency came back
// by itself; the node has to come back with it, in one probe interval, without
// being restarted, redeployed or told anything.
func TestRecoveryIsImmediateAndNeedsNoRestart(t *testing.T) {
	now, advance := depClock()
	d := newDep(t, now)

	for i := 0; i < 4; i++ {
		d.Fail()
		advance(5 * time.Second)
	}
	mustUnready(t, d, "a sustained outage")

	// One successful consultation. Not three, and not fifteen seconds of them:
	// a node that can serve must serve, and a hysteresis on the way back is
	// capacity withheld from a store that is already answering.
	d.OK()
	mustReady(t, d, "the first success after the outage")

	// And the run really is cleared rather than merely masked: a fresh outage
	// has to earn the verdict again from zero.
	d.Fail()
	d.Fail()
	advance(time.Minute)
	mustReady(t, d, "two failures after a recovery")
}

// TestUnreadyDoesNotLatchWhenNobodyIsSampling: unreadiness is self-reinforcing
// when the samples come from the request path. The balancer stops routing here,
// the node stops making the calls that would prove the store is back, and it
// would sit out of rotation forever on evidence that stopped being collected.
func TestUnreadyDoesNotLatchWhenNobodyIsSampling(t *testing.T) {
	now, advance := depClock()
	d := NewDependency(DependencyOptions{
		Name:        "store",
		MinOutage:   15 * time.Second,
		MinFailures: 3,
		StaleAfter:  time.Minute,
		Now:         now,
	})

	for i := 0; i < 4; i++ {
		d.Fail()
		advance(5 * time.Second)
	}
	mustUnready(t, d, "a sustained outage")

	advance(2 * time.Minute)
	mustReady(t, d, "two minutes with no sample at all")

	// A failure arriving after the silence starts a new run rather than
	// resuming the old one: two blips an hour apart are not one outage.
	d.Fail()
	mustReady(t, d, "the first failure of a new run")
}

// TestObserveMapsErrorsToTheTwoOutcomes covers the convenience the wiring will
// actually call, since a prober has an error in hand rather than a verdict.
func TestObserveMapsErrorsToTheTwoOutcomes(t *testing.T) {
	now, advance := depClock()
	d := newDep(t, now)
	boom := errors.New("dial tcp: connection refused")

	for i := 0; i < 4; i++ {
		d.Observe(boom)
		advance(5 * time.Second)
	}
	mustUnready(t, d, "four observed errors over fifteen seconds")
	d.Observe(nil)
	mustReady(t, d, "one observed success")
}

// TestDependencyIsConcurrencySafe: ReadyForWork is answered from a health
// handler while a prober writes, so the two run at once by construction.
func TestDependencyIsConcurrencySafe(t *testing.T) {
	d := NewDependency(DependencyOptions{Name: "store", MinOutage: time.Millisecond, MinFailures: 1})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				switch {
				case i%3 == 0:
					d.Fail()
				case i%3 == 1:
					d.OK()
				default:
					d.ReadyForWork()
					d.Stats()
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestDependencyDefaults pins the numbers the wiring inherits, because they are
// the whole judgement: the drain threshold is not a tuning parameter an
// operator is expected to discover.
func TestDependencyDefaults(t *testing.T) {
	now, advance := depClock()
	d := NewDependency(DependencyOptions{Name: "store", Now: now})

	for i := 0; i < DefaultMinFailures; i++ {
		d.Fail()
		advance(DefaultMinOutage / DefaultMinFailures)
	}
	mustReady(t, d, "the count threshold reached before the duration one")

	d.Fail()
	mustUnready(t, d, "the default thresholds both reached")

	// The stale window follows MinOutage rather than being set apart, so a
	// caller that changes one does not silently keep the other's default.
	advance(DefaultStaleFactor*DefaultMinOutage + time.Second)
	mustReady(t, d, "past the derived stale window")
}

// TestWatchProbesUntilCancelled covers the loop the wiring runs, including the
// property that makes recovery work: it keeps probing while the node is
// unready, so the node learns the dependency is back without anyone routing
// traffic here to find out.
func TestWatchProbesUntilCancelled(t *testing.T) {
	d := NewDependency(DependencyOptions{Name: "store", MinOutage: time.Millisecond, MinFailures: 2})

	var mu sync.Mutex
	down := true
	calls := 0
	probe := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if down {
			return errors.New("dial tcp: connection refused")
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); d.Watch(ctx, time.Millisecond, probe) }()

	waitFor(t, "the node to stop accepting new work", func() bool {
		ok, _ := d.ReadyForWork()
		return !ok
	})

	// The dependency comes back with nobody routing here. The prober is the
	// only thing that can notice, and it has to.
	mu.Lock()
	down = false
	mu.Unlock()
	waitFor(t, "the node to return to rotation on its own", func() bool {
		ok, _ := d.ReadyForWork()
		return ok
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return when its context was cancelled")
	}

	// A cancelled probe must not be recorded as a failure: the last thing in
	// the counters at shutdown would look like an outage that never happened.
	if ok, _ := d.ReadyForWork(); !ok {
		t.Error("shutdown left the dependency looking unreachable")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls == 0 {
		t.Error("Watch never probed")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
