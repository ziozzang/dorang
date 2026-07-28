package capacity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// The tests in this file are about settings that used to validate and do
// nothing: capacity.*.max_queue, capacity.principals.<id>.max_queue_wait, and
// the api key's own max_parallel_requests.
//
// Each one asserts an observable the broker did not have before — a distinct
// error, or a refusal that could only come from the new limit — rather than
// asserting that a check works when called. A test of the second kind is what
// let all three of these ship: internal/auth's rate check has always been
// correct and has never been reached.

// blockGlobal fills the global axis so that the next acquire must queue.
func blockGlobal(t *testing.T, b *Broker, n int) []*Reservation {
	t.Helper()
	var held []*Reservation
	for i := 0; i < n; i++ {
		r, ok := b.TryAcquire(Request{Provider: "p"})
		if !ok {
			t.Fatalf("filling the axis: acquire %d was refused", i)
		}
		held = append(held, r)
	}
	return held
}

// TestMaxQueueRefusesPastTheCeiling is capacity.*.max_queue. Before this the
// queue was an unbounded heap: a saturated axis grew one blocked goroutine per
// caller with nothing anywhere to stop it, and the configured ceiling was
// checked for sign at load and then discarded.
func TestMaxQueueRefusesPastTheCeiling(t *testing.T) {
	b := New(Config{
		Global:        1,
		SweepInterval: -1,
		Queues:        QueueConfig{Global: 2},
	})
	defer b.Close()

	held := blockGlobal(t, b, 1)
	defer held[0].Release()

	// Two waiters fit.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := b.Acquire(ctx, Request{Provider: "p"})
			if res != nil {
				res.Release()
			}
			_ = err
		}()
	}
	waitForWaiters(t, b, 2)

	// The third does not, and it is refused rather than queued.
	_, err := b.Acquire(context.Background(), Request{Provider: "p"})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("Acquire past max_queue = %v, want ErrQueueFull", err)
	}
	if got := b.Snapshot().QueueRefused; got != 1 {
		t.Errorf("QueueRefused = %d, want 1", got)
	}
	if got := b.Snapshot().Waiting; got != 2 {
		t.Errorf("Waiting = %d, want 2: a refused caller must leave nothing behind", got)
	}

	cancel()
	wg.Wait()
}

// TestMaxQueueIsUnboundedWhenUnset keeps the ceiling opt-in: a broker with no
// QueueConfig behaves exactly as it did.
func TestMaxQueueIsUnboundedWhenUnset(t *testing.T) {
	b := New(Config{Global: 1, SweepInterval: -1})
	defer b.Close()
	held := blockGlobal(t, b, 1)
	defer held[0].Release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _ := b.Acquire(ctx, Request{Provider: "p"})
			if res != nil {
				res.Release()
			}
		}()
	}
	waitForWaiters(t, b, 5)
	cancel()
	wg.Wait()
}

// TestMaxQueueWaitElapses is capacity.principals.<id>.max_queue_wait. Every
// principal is defaulted to 30s and the number was computed at load and thrown
// away: the broker had no wait budget, no timer, and no way to express one.
func TestMaxQueueWaitElapses(t *testing.T) {
	b := New(Config{
		Global:        1,
		SweepInterval: -1,
		Principals:    map[string]int{"default": 10},
		Queues:        QueueConfig{MaxWait: map[string]time.Duration{"k1": 30 * time.Millisecond}},
	})
	defer b.Close()

	held := blockGlobal(t, b, 1)
	defer held[0].Release()

	start := time.Now()
	_, err := b.Acquire(context.Background(), Request{Provider: "p", PrincipalID: "k1"})
	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("Acquire = %v, want ErrQueueTimeout", err)
	}
	if d := time.Since(start); d < 30*time.Millisecond {
		t.Errorf("returned after %s, before the budget elapsed", d)
	}
	if got := b.Snapshot().QueueTimeouts; got != 1 {
		t.Errorf("QueueTimeouts = %d, want 1", got)
	}
	if got := b.Snapshot().Waiting; got != 0 {
		t.Errorf("Waiting = %d after a timeout: the waiter must be dequeued", got)
	}
}

// TestMaxQueueWaitDefaultAppliesToAnUnnamedPrincipal mirrors how the
// concurrency ceiling already resolves: the "default" entry covers a principal
// with no explicit one, so an operator writes the budget once.
func TestMaxQueueWaitDefaultAppliesToAnUnnamedPrincipal(t *testing.T) {
	b := New(Config{
		Global:        1,
		SweepInterval: -1,
		Queues:        QueueConfig{MaxWait: map[string]time.Duration{"default": 20 * time.Millisecond}},
	})
	defer b.Close()
	if got := b.WaitBudget("someone-else"); got != 20*time.Millisecond {
		t.Fatalf("WaitBudget = %s, want the default entry", got)
	}

	held := blockGlobal(t, b, 1)
	defer held[0].Release()
	_, err := b.Acquire(context.Background(), Request{Provider: "p", PrincipalID: "unnamed"})
	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("Acquire = %v, want ErrQueueTimeout", err)
	}
}

// TestMaxQueueWaitDoesNotApplyToBatch. Batch is the work that is supposed to
// wait: §11.1 gives it a lower ceiling on every axis so it yields to interactive
// traffic. Applying the thirty-second budget every principal is defaulted to
// would turn ordinary contention into failed rows, which is the opposite of what
// a batch queue is for.
func TestMaxQueueWaitDoesNotApplyToBatch(t *testing.T) {
	b := New(Config{
		Global:        1,
		SweepInterval: -1,
		Queues:        QueueConfig{MaxWait: map[string]time.Duration{"default": 20 * time.Millisecond}},
	})
	defer b.Close()

	held := blockGlobal(t, b, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		res, err := b.Acquire(ctx, Request{Provider: "p", PrincipalID: "k1", Batch: true})
		if res != nil {
			res.Release()
		}
		done <- err
	}()
	waitForWaiters(t, b, 1)

	// Well past the budget an interactive request would have been given.
	select {
	case err := <-done:
		t.Fatalf("a batch waiter was timed out by max_queue_wait: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	held[0].Release()
	if err := <-done; err != nil {
		t.Fatalf("the batch waiter was not served: %v", err)
	}
	if got := b.Snapshot().QueueTimeouts; got != 0 {
		t.Errorf("QueueTimeouts = %d for a batch waiter, want 0", got)
	}
}

// TestMaxQueueWaitGrantRacingTheTimerKeepsTheReservation: a grant that lands
// while the timer fires must not be dropped on the floor. Discarding it would
// leak a slot until the sweeper reclaimed it, and refuse a caller who had in
// fact been served.
func TestMaxQueueWaitGrantWinsARace(t *testing.T) {
	b := New(Config{
		Global:        1,
		SweepInterval: -1,
		Queues:        QueueConfig{MaxWait: map[string]time.Duration{"k1": 50 * time.Millisecond}},
	})
	defer b.Close()

	held := blockGlobal(t, b, 1)
	done := make(chan *Reservation, 1)
	go func() {
		res, err := b.Acquire(context.Background(), Request{Provider: "p", PrincipalID: "k1"})
		if err != nil {
			done <- nil
			return
		}
		done <- res
	}()
	waitForWaiters(t, b, 1)
	held[0].Release()

	res := <-done
	if res == nil {
		t.Fatal("the waiter was served and then told it had timed out")
	}
	res.Release()
	if got := b.Snapshot().Reservations; got != 0 {
		t.Errorf("Reservations = %d after release, want 0", got)
	}
}

// TestPrincipalMaxTightensTheAxis is the api key's max_parallel_requests. It was
// carried from the store into auth.Limits under a comment claiming
// "enforced by internal/capacity" — a package in which the identifier did not
// appear at all.
func TestPrincipalMaxTightensTheAxis(t *testing.T) {
	b := New(Config{SweepInterval: -1, Principals: map[string]int{"default": 8}})
	defer b.Close()

	req := Request{Provider: "p", PrincipalID: "k1", PrincipalMax: 1}
	first, ok := b.TryAcquire(req)
	if !ok {
		t.Fatal("the first request was refused")
	}
	if _, ok := b.TryAcquire(req); ok {
		t.Fatal("a second concurrent request was admitted past max_parallel_requests: 1")
	}
	first.Release()
	second, ok := b.TryAcquire(req)
	if !ok {
		t.Fatal("the slot was not returned")
	}
	second.Release()
}

// TestPrincipalMaxNeverWidensTheConfiguredCeiling: a key issued a generous
// max_parallel_requests must not escape a tighter capacity.principals entry. The
// stricter of the two wins, in both directions.
func TestPrincipalMaxNeverWidens(t *testing.T) {
	b := New(Config{SweepInterval: -1, Principals: map[string]int{"k1": 1}})
	defer b.Close()

	req := Request{Provider: "p", PrincipalID: "k1", PrincipalMax: 100}
	first, ok := b.TryAcquire(req)
	if !ok {
		t.Fatal("the first request was refused")
	}
	defer first.Release()
	if _, ok := b.TryAcquire(req); ok {
		t.Fatal("a key's own ceiling widened the configured principal ceiling")
	}
}

// TestLeastUsedKeyPrefersTheIdlestAccount backs key_rotation.strategy:
// least_used, which was validated against four names and then behaved as
// failover for all four.
func TestLeastUsedKeyPrefersTheIdlestAccount(t *testing.T) {
	b := New(Config{SweepInterval: -1})
	defer b.Close()

	cands := []Candidate{
		{ID: "acct-1", MaxConcurrent: 4},
		{ID: "acct-2", MaxConcurrent: 4},
	}
	// Nothing outstanding: configuration order is the tie-break.
	if got := b.LeastUsedKey("p", cands); got != "acct-1" {
		t.Errorf("LeastUsedKey on an idle pool = %q, want acct-1", got)
	}
	r, ok := b.TryAcquire(Request{Provider: "p", Candidates: cands[:1], Preferred: "acct-1"})
	if !ok {
		t.Fatal("acquire on acct-1 was refused")
	}
	defer r.Release()
	if got := b.LeastUsedKey("p", cands); got != "acct-2" {
		t.Errorf("LeastUsedKey = %q, want acct-2 once acct-1 holds one", got)
	}
}

// waitForWaiters blocks until the broker reports n queued callers.
func waitForWaiters(t *testing.T, b *Broker, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b.Snapshot().Waiting >= n {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatalf("timed out waiting for %d queued callers, have %d", n, b.Snapshot().Waiting)
}
