package capacity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newBroker builds a broker with expiry and the background sweeper off by
// default, so tests are deterministic. Pass an explicit positive
// ReservationTTL/SweepInterval to opt back in.
func newBroker(t *testing.T, cfg Config) *Broker {
	t.Helper()
	if cfg.ReservationTTL == 0 {
		cfg.ReservationTTL = -1
	}
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = -1
	}
	b := New(cfg)
	t.Cleanup(b.Close)
	return b
}

type result struct {
	res *Reservation
	err error
}

// launch starts a blocking Acquire and reports its outcome on out.
func launch(b *Broker, ctx context.Context, req Request, out chan<- result) {
	go func() {
		res, err := b.Acquire(ctx, req)
		out <- result{res, err}
	}()
}

// waitFor polls cond until it holds or the test fails.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Microsecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitQueued blocks until exactly n Acquire calls are parked in the broker.
func waitQueued(t *testing.T, b *Broker, n int) {
	t.Helper()
	waitFor(t, func() bool { return b.Snapshot().Waiting == n }, "waiters to reach "+itoa(n))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// mustAcquire acquires or fails the test.
func mustAcquire(t *testing.T, b *Broker, req Request) *Reservation {
	t.Helper()
	res, ok := b.TryAcquire(req)
	if !ok {
		t.Fatalf("TryAcquire unexpectedly failed: %+v", req)
	}
	return res
}

// mustBlock asserts the request cannot be admitted right now.
func mustBlock(t *testing.T, b *Broker, req Request) {
	t.Helper()
	if res, ok := b.TryAcquire(req); ok {
		res.Release()
		t.Fatalf("TryAcquire unexpectedly succeeded: %+v", req)
	}
}

// releaseAll releases a slice of reservations.
func releaseAll(rs []*Reservation) {
	for _, r := range rs {
		r.Release()
	}
}

// ---------------------------------------------------------------------------
// scenario: two credential groups of three
// ---------------------------------------------------------------------------

func TestTwoCredentialGroupsOfThree(t *testing.T) {
	b := newBroker(t, Config{
		CredentialGroups: map[string]int{"acct-1": 3, "acct-2": 3},
	})

	req := Request{
		Provider:   "cloud-a",
		Model:      "model-x",
		OnCapacity: Spill,
		Candidates: []Candidate{
			{ID: "acct-1", CapacityGroup: "acct-1"},
			{ID: "acct-2", CapacityGroup: "acct-2"},
		},
	}

	var held []*Reservation
	byCred := map[string]int{}
	for i := 0; i < 6; i++ {
		r := mustAcquire(t, b, req)
		held = append(held, r)
		byCred[r.CredentialID()]++
	}
	if byCred["acct-1"] != 3 || byCred["acct-2"] != 3 {
		t.Fatalf("expected 3 per credential group, got %v", byCred)
	}

	// The seventh has nowhere to go.
	mustBlock(t, b, req)

	// Three waiters queue up; a single release must admit exactly one.
	out := make(chan result, 3)
	for i := 0; i < 3; i++ {
		launch(b, context.Background(), req, out)
		waitQueued(t, b, i+1)
	}

	held[0].Release() // frees one slot in acct-1

	var first result
	select {
	case first = <-out:
	case <-time.After(3 * time.Second):
		t.Fatal("release did not admit a waiter")
	}
	if first.err != nil {
		t.Fatalf("waiter failed: %v", first.err)
	}
	if got := first.res.CredentialID(); got != "acct-1" {
		t.Fatalf("expected the freed group acct-1, got %q", got)
	}

	select {
	case extra := <-out:
		t.Fatalf("one release admitted more than one waiter: %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
	if got := b.Snapshot().Waiting; got != 2 {
		t.Fatalf("waiting = %d, want 2", got)
	}

	first.res.Release()
	releaseAll(held[1:])
	for i := 0; i < 2; i++ {
		r := <-out
		if r.err != nil {
			t.Fatalf("waiter failed: %v", r.err)
		}
		r.res.Release()
	}
	if got := b.Snapshot().Reservations; got != 0 {
		t.Fatalf("live reservations = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// scenario: one credential, per (provider, model) limit of 7 across two models
// ---------------------------------------------------------------------------

func TestPerModelLimitAcrossTwoModels(t *testing.T) {
	b := newBroker(t, Config{
		Models: []ModelLimit{
			{Provider: "plan-a", Model: "model-x", Max: 7},
			{Provider: "plan-a", Model: "model-y", Max: 7},
		},
	})

	req := func(model string) Request {
		return Request{
			Provider:   "plan-a",
			Model:      model,
			Candidates: []Candidate{{ID: "plan-a-1", CapacityGroup: "plan-a-1"}},
		}
	}

	var held []*Reservation
	for i := 0; i < 7; i++ {
		held = append(held, mustAcquire(t, b, req("model-x")))
		held = append(held, mustAcquire(t, b, req("model-y")))
	}
	if len(held) != 14 {
		t.Fatalf("held %d, want 14", len(held))
	}

	// The fifteenth on either model is over that model's limit.
	mustBlock(t, b, req("model-x"))
	mustBlock(t, b, req("model-y"))

	held[0].Release() // a model-x slot
	mustBlock(t, b, req("model-y"))
	r := mustAcquire(t, b, req("model-x"))
	r.Release()

	releaseAll(held[1:])
}

// ---------------------------------------------------------------------------
// scenario: credential total 20 plus per-model 7 — blocks at the credential total
// ---------------------------------------------------------------------------

func TestCredentialTotalCapsBeforeModelLimits(t *testing.T) {
	b := newBroker(t, Config{
		CredentialGroups: map[string]int{"acct-1": 20},
		Models: []ModelLimit{
			{Provider: "plan-a", Model: "m1", Max: 7},
			{Provider: "plan-a", Model: "m2", Max: 7},
			{Provider: "plan-a", Model: "m3", Max: 7},
		},
	})

	req := func(model string) Request {
		return Request{
			Provider:   "plan-a",
			Model:      model,
			Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
		}
	}

	var held []*Reservation
	for _, m := range []string{"m1", "m2"} {
		for i := 0; i < 7; i++ {
			held = append(held, mustAcquire(t, b, req(m)))
		}
	}
	for i := 0; i < 6; i++ {
		held = append(held, mustAcquire(t, b, req("m3")))
	}
	if len(held) != 20 {
		t.Fatalf("held %d, want 20", len(held))
	}

	// m3 still has a model slot free (6 of 7) but the account is full at 20.
	if got := b.inUseOf(modelAxisKey("plan-a", "m3")); got != 6 {
		t.Fatalf("model m3 in use = %d, want 6", got)
	}
	if got := b.inUseOf(cgroupKey("acct-1")); got != 20 {
		t.Fatalf("credential group in use = %d, want 20", got)
	}
	mustBlock(t, b, req("m3"))

	// Freeing an m1 slot frees the account, so m3 can proceed.
	held[0].Release()
	r := mustAcquire(t, b, req("m3"))
	if got := b.inUseOf(modelAxisKey("plan-a", "m3")); got != 7 {
		t.Fatalf("model m3 in use = %d, want 7", got)
	}
	// Now m3 is at its own limit even though the account has room.
	held[0] = r
	held[1].Release()
	held[1] = nil
	mustBlock(t, b, req("m3"))
	// m1 has room on both its own axis and the account's.
	r2 := mustAcquire(t, b, req("m1"))
	r2.Release()

	for _, h := range held {
		h.Release()
	}
}

// ---------------------------------------------------------------------------
// all-or-nothing
// ---------------------------------------------------------------------------

func TestAllOrNothingNoCounterMoves(t *testing.T) {
	// The credential group fills before the model does, so the failing check
	// happens after the model axis has already been found to have room. That is
	// the case a non-atomic implementation would get wrong.
	b := newBroker(t, Config{
		Global:           100,
		Routes:           map[string]int{"plan-a": 100},
		ProviderGroups:   map[string]int{"pool": 100},
		Principals:       map[string]int{"default": 100},
		CredentialGroups: map[string]int{"acct-1": 1},
		Models:           []ModelLimit{{Provider: "plan-a", Model: "m1", Max: 5}},
	})

	req := Request{
		Provider:      "plan-a",
		Model:         "m1",
		ProviderGroup: "pool",
		PrincipalID:   "user-1",
		Candidates:    []Candidate{{ID: "acct-1", CapacityGroup: "acct-1", MaxConcurrent: 5}},
	}

	held := mustAcquire(t, b, req)
	before := b.Snapshot()

	for i := 0; i < 20; i++ {
		mustBlock(t, b, req)
	}

	after := b.Snapshot()
	if len(before.Axes) != len(after.Axes) {
		t.Fatalf("axis set changed: %v -> %v", before.Axes, after.Axes)
	}
	for i := range before.Axes {
		if before.Axes[i] != after.Axes[i] {
			t.Fatalf("axis %s moved: %+v -> %+v", before.Axes[i].Key, before.Axes[i], after.Axes[i])
		}
	}
	// Every axis must still show exactly the one held reservation.
	for _, a := range after.Axes {
		if a.InUse != 1 {
			t.Fatalf("axis %s in use = %d, want 1 (failed attempt left residue)", a.Key, a.InUse)
		}
	}

	held.Release()
	for _, a := range b.Snapshot().Axes {
		if a.InUse != 0 {
			t.Fatalf("axis %s in use = %d after release, want 0", a.Key, a.InUse)
		}
	}
}

func TestAllOrNothingWhenFirstAxisBlocks(t *testing.T) {
	// Mirror image: the global axis (checked first) is what blocks, so nothing
	// further along may be touched either.
	b := newBroker(t, Config{
		Global:           1,
		CredentialGroups: map[string]int{"acct-1": 5},
	})
	req := Request{
		Provider:   "plan-a",
		Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1", MaxConcurrent: 5}},
	}
	held := mustAcquire(t, b, req)
	mustBlock(t, b, req)

	if got := b.inUseOf(cgroupKey("acct-1")); got != 1 {
		t.Fatalf("credential group in use = %d, want 1", got)
	}
	if got := b.inUseOf(credAxisKey("plan-a", "acct-1")); got != 1 {
		t.Fatalf("key axis in use = %d, want 1", got)
	}
	held.Release()
}

// ---------------------------------------------------------------------------
// spill vs wait
// ---------------------------------------------------------------------------

func TestSpillMovesToNextCandidateWaitDoesNot(t *testing.T) {
	b := newBroker(t, Config{
		CredentialGroups: map[string]int{"acct-1": 1, "acct-2": 1},
	})
	cands := []Candidate{
		{ID: "acct-1", CapacityGroup: "acct-1"},
		{ID: "acct-2", CapacityGroup: "acct-2"},
	}
	base := Request{Provider: "cloud-a", Candidates: cands, Preferred: "acct-1"}

	spill := base
	spill.OnCapacity = Spill
	wait := base
	wait.OnCapacity = Wait

	first := mustAcquire(t, b, spill)
	if first.CredentialID() != "acct-1" {
		t.Fatalf("preferred candidate not chosen first: %q", first.CredentialID())
	}

	// Wait never looks past the preferred candidate, even though acct-2 is free.
	mustBlock(t, b, wait)

	second := mustAcquire(t, b, spill)
	if second.CredentialID() != "acct-2" {
		t.Fatalf("spill did not move to the next candidate: %q", second.CredentialID())
	}

	mustBlock(t, b, spill)

	// A Wait waiter is woken only by its own preferred candidate.
	out := make(chan result, 1)
	launch(b, context.Background(), wait, out)
	waitQueued(t, b, 1)
	if got := b.queueLen(cgroupKey("acct-2")); got != 0 {
		t.Fatalf("Wait waiter queued on a non-preferred axis (acct-2 depth %d)", got)
	}
	if got := b.queueLen(cgroupKey("acct-1")); got != 1 {
		t.Fatalf("Wait waiter not queued on its preferred axis (acct-1 depth %d)", got)
	}

	second.Release() // frees acct-2, which the Wait waiter must ignore
	select {
	case r := <-out:
		t.Fatalf("Wait waiter woken by a non-preferred candidate: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}

	first.Release() // frees acct-1
	r := <-out
	if r.err != nil {
		t.Fatalf("waiter failed: %v", r.err)
	}
	if r.res.CredentialID() != "acct-1" {
		t.Fatalf("Wait waiter got %q, want acct-1", r.res.CredentialID())
	}
	r.res.Release()
}

func TestSpillWaiterQueuesOnEveryBlockingAxis(t *testing.T) {
	b := newBroker(t, Config{
		CredentialGroups: map[string]int{"acct-1": 1, "acct-2": 1},
	})
	req := Request{
		Provider:   "cloud-a",
		OnCapacity: Spill,
		Candidates: []Candidate{
			{ID: "acct-1", CapacityGroup: "acct-1"},
			{ID: "acct-2", CapacityGroup: "acct-2"},
		},
	}
	r1 := mustAcquire(t, b, req)
	r2 := mustAcquire(t, b, req)

	out := make(chan result, 1)
	launch(b, context.Background(), req, out)
	waitQueued(t, b, 1)

	if b.queueLen(cgroupKey("acct-1")) != 1 || b.queueLen(cgroupKey("acct-2")) != 1 {
		t.Fatal("spill waiter must be queued on every candidate's blocking axis")
	}

	// Releasing the *second* candidate must be enough to wake it.
	r2.Release()
	r := <-out
	if r.err != nil {
		t.Fatalf("waiter failed: %v", r.err)
	}
	if r.res.CredentialID() != "acct-2" {
		t.Fatalf("got %q, want acct-2", r.res.CredentialID())
	}
	if n := b.totalQueueNodes(); n != 0 {
		t.Fatalf("granted waiter left %d queue nodes behind", n)
	}
	r.res.Release()
	r1.Release()
}

// ---------------------------------------------------------------------------
// candidate atomicity across providers (DESIGN §5.3)
// ---------------------------------------------------------------------------

func TestSpillAcrossProvidersRederivesProviderScopedAxes(t *testing.T) {
	b := newBroker(t, Config{
		Routes: map[string]int{"plan-a": 1, "cloud-a": 1},
		Models: []ModelLimit{
			{Provider: "plan-a", Model: "model-x", Max: 1},
			{Provider: "cloud-a", Model: "model-x:cloud", Max: 1},
		},
	})
	req := Request{
		Provider:   "plan-a",
		Model:      "model-x",
		OnCapacity: Spill,
		Candidates: []Candidate{
			{ID: "plan-a-1", CapacityGroup: "plan-a-1"},
			{ID: "acct-1", CapacityGroup: "acct-1", Provider: "cloud-a", UpstreamModel: "model-x:cloud"},
		},
	}

	r1 := mustAcquire(t, b, req)
	if r1.CredentialID() != "plan-a-1" {
		t.Fatalf("first candidate not chosen: %q", r1.CredentialID())
	}
	r2 := mustAcquire(t, b, req)
	if r2.CredentialID() != "acct-1" {
		t.Fatalf("spill did not cross providers: %q", r2.CredentialID())
	}

	// The second reservation must count against cloud-a's axes, not plan-a's.
	if got := b.inUseOf(routeKey("cloud-a")); got != 1 {
		t.Fatalf("cloud-a route in use = %d, want 1", got)
	}
	if got := b.inUseOf(routeKey("plan-a")); got != 1 {
		t.Fatalf("plan-a route in use = %d, want 1", got)
	}
	if got := b.inUseOf(modelAxisKey("cloud-a", "model-x:cloud")); got != 1 {
		t.Fatalf("cloud-a model in use = %d, want 1", got)
	}
	if got := b.inUseOf(modelAxisKey("plan-a", "model-x")); got != 1 {
		t.Fatalf("plan-a model in use = %d, want 1", got)
	}

	mustBlock(t, b, req)
	r1.Release()
	r2.Release()
	for _, a := range b.Snapshot().Axes {
		if a.InUse != 0 {
			t.Fatalf("axis %s in use = %d, want 0", a.Key, a.InUse)
		}
	}
}

// ---------------------------------------------------------------------------
// cancellation
// ---------------------------------------------------------------------------

func TestCancellationLeavesQueuesClean(t *testing.T) {
	b := newBroker(t, Config{
		CredentialGroups: map[string]int{"acct-1": 1, "acct-2": 1},
	})
	req := Request{
		Provider:   "cloud-a",
		OnCapacity: Spill,
		Candidates: []Candidate{
			{ID: "acct-1", CapacityGroup: "acct-1"},
			{ID: "acct-2", CapacityGroup: "acct-2"},
		},
	}
	r1 := mustAcquire(t, b, req)
	r2 := mustAcquire(t, b, req)

	const n = 12
	ctxs := make([]context.CancelFunc, n)
	out := make(chan result, n)
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		ctxs[i] = cancel
		launch(b, ctx, req, out)
		waitQueued(t, b, i+1)
	}
	// Each spill waiter sits in both credential-group queues.
	if got := b.totalQueueNodes(); got != 2*n {
		t.Fatalf("queue nodes = %d, want %d", got, 2*n)
	}

	// Cancel out of order to exercise removal from the middle of the heap.
	for _, i := range []int{5, 0, 11, 3, 7, 1, 2, 4, 6, 8, 9, 10} {
		ctxs[i]()
	}
	for i := 0; i < n; i++ {
		r := <-out
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("waiter %d: err = %v, want context.Canceled", i, r.err)
		}
		if r.res != nil {
			t.Fatal("cancelled waiter must not receive a reservation")
		}
	}

	waitFor(t, func() bool { return b.liveWaiters() == 0 }, "waiters to drain")
	if got := b.totalQueueNodes(); got != 0 {
		t.Fatalf("queue nodes = %d after cancellation, want 0", got)
	}
	if got := b.Snapshot().Waiting; got != 0 {
		t.Fatalf("Snapshot().Waiting = %d, want 0", got)
	}

	r1.Release()
	r2.Release()
	s := b.Snapshot()
	if s.Reservations != 0 {
		t.Fatalf("live reservations = %d, want 0", s.Reservations)
	}
	for _, a := range s.Axes {
		if a.InUse != 0 || a.Waiting != 0 {
			t.Fatalf("axis %s = %+v, want empty", a.Key, a)
		}
	}
}

func TestCancellationRacingGrantDoesNotLeak(t *testing.T) {
	b := newBroker(t, Config{CredentialGroups: map[string]int{"acct-1": 1}})
	req := Request{
		Provider:   "cloud-a",
		Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
	}

	for i := 0; i < 300; i++ {
		held := mustAcquire(t, b, req)
		ctx, cancel := context.WithCancel(context.Background())
		out := make(chan result, 1)
		launch(b, ctx, req, out)
		waitQueued(t, b, 1)

		// Release and cancel at the same time: whoever wins, the slot must come
		// back and no reservation may be stranded.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); held.Release() }()
		go func() { defer wg.Done(); cancel() }()
		wg.Wait()

		r := <-out
		if r.err == nil {
			r.res.Release()
		}
		waitFor(t, func() bool { return b.inUseOf(cgroupKey("acct-1")) == 0 },
			"capacity to return after race")
		if n := b.totalQueueNodes(); n != 0 {
			t.Fatalf("iteration %d left %d queue nodes", i, n)
		}
	}
}

// ---------------------------------------------------------------------------
// expiry
// ---------------------------------------------------------------------------

type fakeClock struct{ ns atomic.Int64 }

func (c *fakeClock) now() time.Time      { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) add(d time.Duration) { c.ns.Add(int64(d)) }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ns.Store(time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func TestSweeperReclaimsExpiredReservation(t *testing.T) {
	clk := newFakeClock()
	b := newBroker(t, Config{
		CredentialGroups: map[string]int{"acct-1": 1},
		ReservationTTL:   30 * time.Second,
		Now:              clk.now,
	})
	req := Request{
		Provider:   "cloud-a",
		Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
	}

	leaked := mustAcquire(t, b, req)
	if leaked.Deadline().IsZero() {
		t.Fatal("reservation carries no deadline")
	}
	mustBlock(t, b, req)

	if n := b.Sweep(); n != 0 {
		t.Fatalf("swept %d before the deadline, want 0", n)
	}

	// Pretend the holder panicked and never released.
	clk.add(31 * time.Second)
	if n := b.Sweep(); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if got := b.inUseOf(cgroupKey("acct-1")); got != 0 {
		t.Fatalf("in use = %d after sweep, want 0", got)
	}
	if got := b.Snapshot().Expired; got != 1 {
		t.Fatalf("Expired = %d, want 1", got)
	}

	next := mustAcquire(t, b, req)

	// The stale handle must not double-release the new holder's slot.
	leaked.Release()
	if got := b.inUseOf(cgroupKey("acct-1")); got != 1 {
		t.Fatalf("in use = %d after releasing a swept reservation, want 1", got)
	}
	next.Release()
}

func TestSweeperWakesAWaiter(t *testing.T) {
	clk := newFakeClock()
	b := newBroker(t, Config{
		CredentialGroups: map[string]int{"acct-1": 1},
		ReservationTTL:   30 * time.Second,
		Now:              clk.now,
	})
	req := Request{
		Provider:   "cloud-a",
		Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
	}
	_ = mustAcquire(t, b, req) // leaked on purpose

	out := make(chan result, 1)
	launch(b, context.Background(), req, out)
	waitQueued(t, b, 1)

	clk.add(time.Minute)
	if n := b.Sweep(); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	select {
	case r := <-out:
		if r.err != nil {
			t.Fatalf("waiter failed: %v", r.err)
		}
		r.res.Release()
	case <-time.After(3 * time.Second):
		t.Fatal("sweeper did not wake the waiter")
	}
}

func TestBackgroundSweeperRuns(t *testing.T) {
	clk := newFakeClock()
	b := New(Config{
		CredentialGroups: map[string]int{"acct-1": 1},
		ReservationTTL:   time.Second,
		SweepInterval:    time.Millisecond,
		Now:              clk.now,
	})
	defer b.Close()

	req := Request{
		Provider:   "cloud-a",
		Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
	}
	_ = mustAcquire(t, b, req)
	clk.add(2 * time.Second)
	waitFor(t, func() bool { return b.inUseOf(cgroupKey("acct-1")) == 0 },
		"background sweeper to reclaim")
}

func TestExpiryDisabled(t *testing.T) {
	clk := newFakeClock()
	b := newBroker(t, Config{
		CredentialGroups: map[string]int{"acct-1": 1},
		ReservationTTL:   -1,
		Now:              clk.now,
	})
	r := mustAcquire(t, b, Request{
		Provider:   "cloud-a",
		Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
	})
	if !r.Deadline().IsZero() {
		t.Fatal("expiry disabled but a deadline was set")
	}
	clk.add(365 * 24 * time.Hour)
	if n := b.Sweep(); n != 0 {
		t.Fatalf("swept %d with expiry disabled, want 0", n)
	}
	r.Release()
}

// ---------------------------------------------------------------------------
// release semantics
// ---------------------------------------------------------------------------

func TestReleaseIsIdempotent(t *testing.T) {
	b := newBroker(t, Config{
		Global:           2,
		CredentialGroups: map[string]int{"acct-1": 2},
	})
	req := Request{
		Provider:   "cloud-a",
		Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1", MaxConcurrent: 2}},
	}
	r1 := mustAcquire(t, b, req)
	r2 := mustAcquire(t, b, req)
	mustBlock(t, b, req)

	for i := 0; i < 5; i++ {
		r1.Release()
	}
	if got := b.inUseOf(globalKey()); got != 1 {
		t.Fatalf("global in use = %d after repeated release, want 1", got)
	}

	r3 := mustAcquire(t, b, req)
	mustBlock(t, b, req)

	r2.Release()
	r3.Release()
	for _, a := range b.Snapshot().Axes {
		if a.InUse != 0 {
			t.Fatalf("axis %s in use = %d, want 0", a.Key, a.InUse)
		}
	}

	var nilRes *Reservation
	nilRes.Release() // must not panic
	if nilRes.CredentialID() != "" || !nilRes.Deadline().IsZero() {
		t.Fatal("nil reservation accessors misbehaved")
	}
}

func TestConcurrentReleaseIsIdempotent(t *testing.T) {
	b := newBroker(t, Config{Global: 4})
	req := Request{Provider: "cloud-a"}
	r := mustAcquire(t, b, req)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Release() }()
	}
	wg.Wait()
	if got := b.inUseOf(globalKey()); got != 0 {
		t.Fatalf("global in use = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// interactive reserve (DESIGN §11.1)
// ---------------------------------------------------------------------------

func TestInteractiveReserveAppliesToModelAxis(t *testing.T) {
	b := newBroker(t, Config{
		// Deliberately no credential-axis limit: revision 1 reserved only there,
		// so batch could take the model's entire limit.
		Models:             []ModelLimit{{Provider: "plan-a", Model: "m1", Max: 10}},
		InteractiveReserve: 0.3,
	})
	mk := func(batch bool) Request {
		return Request{
			Provider:   "plan-a",
			Model:      "m1",
			Batch:      batch,
			Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
		}
	}

	var batchHeld []*Reservation
	for i := 0; i < 7; i++ { // floor(10 * 0.7)
		batchHeld = append(batchHeld, mustAcquire(t, b, mk(true)))
	}
	mustBlock(t, b, mk(true))

	// Interactive traffic still has its reserved 3.
	var interactiveHeld []*Reservation
	for i := 0; i < 3; i++ {
		interactiveHeld = append(interactiveHeld, mustAcquire(t, b, mk(false)))
	}
	mustBlock(t, b, mk(false))

	releaseAll(batchHeld)
	releaseAll(interactiveHeld)
}

func TestInteractiveReserveDoesNotRoundAwayASlot(t *testing.T) {
	// 10 * (1 - 0.3) is 6.999999999999999 in binary floating point. Flooring it
	// naively would reserve 4 rather than the configured 3.
	b := newBroker(t, Config{
		Models:             []ModelLimit{{Provider: "p", Model: "m", Max: 10}},
		InteractiveReserve: 0.3,
	})
	if got := b.effLimit(10, true); got != 7 {
		t.Fatalf("effLimit(10, batch) = %d, want 7", got)
	}
	if got := b.effLimit(7, true); got != 4 {
		t.Fatalf("effLimit(7, batch) = %d, want 4", got)
	}
	if got := b.effLimit(10, false); got != 10 {
		t.Fatalf("effLimit(10, interactive) = %d, want 10", got)
	}
}

func TestBatchWaiterDoesNotHeadOfLineBlockInteractive(t *testing.T) {
	b := newBroker(t, Config{
		Models:             []ModelLimit{{Provider: "plan-a", Model: "m1", Max: 10}},
		InteractiveReserve: 0.3,
	})
	mk := func(batch bool) Request {
		return Request{Provider: "plan-a", Model: "m1", Batch: batch}
	}

	var held []*Reservation
	for i := 0; i < 7; i++ {
		held = append(held, mustAcquire(t, b, mk(true)))
	}
	for i := 0; i < 3; i++ {
		held = append(held, mustAcquire(t, b, mk(false)))
	}

	batchOut := make(chan result, 1)
	launch(b, context.Background(), mk(true), batchOut)
	waitQueued(t, b, 1)

	interactiveOut := make(chan result, 1)
	launch(b, context.Background(), mk(false), interactiveOut)
	waitQueued(t, b, 2)

	// One slot comes back: in use drops to 9. Batch is still over its reduced
	// ceiling of 7, but interactive is under 10 and must not wait behind it.
	held[9].Release()

	select {
	case r := <-interactiveOut:
		if r.err != nil {
			t.Fatalf("interactive waiter failed: %v", r.err)
		}
		defer r.res.Release()
	case <-time.After(3 * time.Second):
		t.Fatal("interactive waiter starved behind a blocked batch waiter")
	}
	select {
	case r := <-batchOut:
		t.Fatalf("batch waiter admitted over its reserve: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}

	// Draining below the batch ceiling finally admits it.
	for i := 0; i < 4; i++ {
		held[i].Release()
	}
	select {
	case r := <-batchOut:
		if r.err != nil {
			t.Fatalf("batch waiter failed: %v", r.err)
		}
		r.res.Release()
	case <-time.After(3 * time.Second):
		t.Fatal("batch waiter never admitted")
	}
	for i := 4; i < 9; i++ {
		held[i].Release()
	}
}

func TestBatchWithNoReservedSlotsFailsFast(t *testing.T) {
	// floor(1 * (1 - 0.5)) == 0: no release can ever admit this batch request,
	// so blocking forever would be wrong.
	b := newBroker(t, Config{
		Models:             []ModelLimit{{Provider: "p", Model: "m", Max: 1}},
		InteractiveReserve: 0.5,
	})
	req := Request{Provider: "p", Model: "m", Batch: true}

	start := time.Now()
	_, err := b.Acquire(context.Background(), req)
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("err = %v, want ErrUnsatisfiable", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Acquire blocked for %v before failing", elapsed)
	}
	if _, ok := b.TryAcquire(req); ok {
		t.Fatal("TryAcquire admitted a permanently blocked batch request")
	}
	// Interactive is unaffected.
	r := mustAcquire(t, b, Request{Provider: "p", Model: "m"})
	r.Release()
}

// ---------------------------------------------------------------------------
// unlimited axes, principals, global
// ---------------------------------------------------------------------------

func TestUnlimitedAxesAreNotCounted(t *testing.T) {
	b := newBroker(t, Config{
		Global:           0, // unlimited
		CredentialGroups: map[string]int{"acct-1": 0},
		Routes:           map[string]int{"p": -5},
	})
	req := Request{
		Provider:   "p",
		Model:      "m",
		Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1", MaxConcurrent: 0}},
	}
	for i := 0; i < 1000; i++ {
		if _, ok := b.TryAcquire(req); !ok {
			t.Fatalf("unlimited broker refused at %d", i)
		}
	}
	if got := len(b.Snapshot().Axes); got != 0 {
		t.Fatalf("unlimited axes created %d buckets, want 0", got)
	}
}

func TestPrincipalDefaultAndExplicit(t *testing.T) {
	b := newBroker(t, Config{
		Principals: map[string]int{"default": 2, "vip": 4},
	})
	mk := func(p string) Request { return Request{Provider: "p", PrincipalID: p} }

	var held []*Reservation
	for i := 0; i < 2; i++ {
		held = append(held, mustAcquire(t, b, mk("someone")))
	}
	mustBlock(t, b, mk("someone"))
	// A different principal has its own counter.
	for i := 0; i < 4; i++ {
		held = append(held, mustAcquire(t, b, mk("vip")))
	}
	mustBlock(t, b, mk("vip"))
	// Another default principal is independent again.
	held = append(held, mustAcquire(t, b, mk("other")))
	releaseAll(held)
}

func TestNoPrincipalIDSkipsAxis(t *testing.T) {
	b := newBroker(t, Config{Principals: map[string]int{"default": 1}})
	req := Request{Provider: "p"}
	var held []*Reservation
	for i := 0; i < 5; i++ {
		held = append(held, mustAcquire(t, b, req))
	}
	if got := b.inUseOf(principalKey("")); got != 0 {
		t.Fatalf("empty principal was counted: %d", got)
	}
	releaseAll(held)
}

func TestRequestWithoutCandidates(t *testing.T) {
	b := newBroker(t, Config{Models: []ModelLimit{{Provider: "p", Model: "m", Max: 2}}})
	req := Request{Provider: "p", Model: "m"}
	r1 := mustAcquire(t, b, req)
	r2 := mustAcquire(t, b, req)
	if r1.CredentialID() != "" {
		t.Fatalf("credential id = %q, want empty", r1.CredentialID())
	}
	mustBlock(t, b, req)
	r1.Release()
	r2.Release()
}

// ---------------------------------------------------------------------------
// lifecycle
// ---------------------------------------------------------------------------

func TestCloseFailsWaiters(t *testing.T) {
	b := New(Config{
		CredentialGroups: map[string]int{"acct-1": 1},
		ReservationTTL:   -1,
		SweepInterval:    -1,
	})
	req := Request{
		Provider:   "cloud-a",
		Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
	}
	held := mustAcquire(t, b, req)

	out := make(chan result, 3)
	for i := 0; i < 3; i++ {
		launch(b, context.Background(), req, out)
		waitQueued(t, b, i+1)
	}

	b.Close()
	b.Close() // idempotent

	for i := 0; i < 3; i++ {
		r := <-out
		if !errors.Is(r.err, ErrClosed) {
			t.Fatalf("waiter %d: err = %v, want ErrClosed", i, r.err)
		}
	}
	if got := b.totalQueueNodes(); got != 0 {
		t.Fatalf("queue nodes = %d after Close, want 0", got)
	}
	if _, err := b.Acquire(context.Background(), req); !errors.Is(err, ErrClosed) {
		t.Fatalf("Acquire after Close: err = %v, want ErrClosed", err)
	}
	if _, ok := b.TryAcquire(req); ok {
		t.Fatal("TryAcquire succeeded after Close")
	}
	// Reservations taken before Close remain releasable.
	held.Release()
	if got := b.inUseOf(cgroupKey("acct-1")); got != 0 {
		t.Fatalf("in use = %d, want 0", got)
	}
}

func TestAcquireWithDoneContextFailsImmediately(t *testing.T) {
	b := newBroker(t, Config{Global: 4})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Acquire(ctx, Request{Provider: "p"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := b.inUseOf(globalKey()); got != 0 {
		t.Fatalf("global in use = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// snapshot
// ---------------------------------------------------------------------------

func TestSnapshotShape(t *testing.T) {
	b := newBroker(t, Config{
		Global:           4,
		Routes:           map[string]int{"plan-a": 3},
		ProviderGroups:   map[string]int{"pool": 3},
		CredentialGroups: map[string]int{"acct-1": 2},
		Models:           []ModelLimit{{Provider: "plan-a", Model: "m1", Max: 2}},
		Principals:       map[string]int{"default": 2},
	})
	req := Request{
		Provider:      "plan-a",
		Model:         "m1",
		ProviderGroup: "pool",
		PrincipalID:   "user-1",
		Candidates:    []Candidate{{ID: "acct-1", CapacityGroup: "acct-1", MaxConcurrent: 2}},
	}
	r := mustAcquire(t, b, req)

	s := b.Snapshot()
	if len(s.Axes) != 7 {
		t.Fatalf("got %d axes, want all 7: %+v", len(s.Axes), s.Axes)
	}
	want := map[string]int{
		"global:":           4,
		"principal:user-1":  2,
		"route:plan-a":      3,
		"pgroup:pool":       3,
		"model:plan-a|m1":   2,
		"cgroup:acct-1":     2,
		"key:plan-a|acct-1": 2,
	}
	for _, a := range s.Axes {
		limit, ok := want[a.Key]
		if !ok {
			t.Fatalf("unexpected axis %q", a.Key)
		}
		if a.Limit != limit {
			t.Fatalf("axis %q limit = %d, want %d", a.Key, a.Limit, limit)
		}
		if a.InUse != 1 {
			t.Fatalf("axis %q in use = %d, want 1", a.Key, a.InUse)
		}
		delete(want, a.Key)
	}
	if len(want) != 0 {
		t.Fatalf("missing axes: %v", want)
	}
	// Sorted by (axis, key), and no separator byte ever leaks into the render.
	for i := 1; i < len(s.Axes); i++ {
		if s.Axes[i-1].Axis > s.Axes[i].Axis {
			t.Fatal("axes not sorted")
		}
	}
	for _, a := range s.Axes {
		for _, c := range a.Key {
			if c == 0 {
				t.Fatalf("axis key %q contains a NUL byte", a.Key)
			}
		}
	}
	if s.Reservations != 1 || s.Grants != 1 {
		t.Fatalf("snapshot counters = %+v", s)
	}
	r.Release()
}

func TestAxisStringNames(t *testing.T) {
	want := []string{"global", "principal", "route", "pgroup", "model", "cgroup", "key"}
	for i, name := range want {
		if got := Axis(i).String(); got != name {
			t.Fatalf("Axis(%d) = %q, want %q", i, got, name)
		}
	}
	if got := Axis(99).String(); got != "unknown" {
		t.Fatalf("Axis(99) = %q", got)
	}
	if Wait.String() != "wait" || Spill.String() != "spill" {
		t.Fatal("OnCapacity names wrong")
	}
}

func TestConfigMapsAreCopied(t *testing.T) {
	groups := map[string]int{"acct-1": 1}
	b := newBroker(t, Config{CredentialGroups: groups})
	groups["acct-1"] = 100 // must not affect the broker

	req := Request{
		Provider:   "p",
		Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
	}
	r := mustAcquire(t, b, req)
	mustBlock(t, b, req)
	r.Release()
}

func TestPerRequestTTLOverridesBrokerDefault(t *testing.T) {
	clk := newFakeClock()
	b := newBroker(t, Config{
		CredentialGroups: map[string]int{"acct-1": 3},
		ReservationTTL:   time.Hour,
		Now:              clk.now,
	})
	mk := func(ttl time.Duration) Request {
		return Request{
			Provider:   "cloud-a",
			Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
			TTL:        ttl,
		}
	}

	short := mustAcquire(t, b, mk(10*time.Second)) // shorter than the broker default
	long := mustAcquire(t, b, mk(0))               // broker default
	forever := mustAcquire(t, b, mk(-1))           // opted out of expiry

	if !forever.Deadline().IsZero() {
		t.Fatal("negative TTL still produced a deadline")
	}
	if !short.Deadline().Before(long.Deadline()) {
		t.Fatalf("per-request TTL ignored: %v vs %v", short.Deadline(), long.Deadline())
	}

	clk.add(11 * time.Second)
	if n := b.Sweep(); n != 1 {
		t.Fatalf("swept %d, want just the short-TTL reservation", n)
	}
	if got := b.inUseOf(cgroupKey("acct-1")); got != 2 {
		t.Fatalf("in use = %d, want 2", got)
	}

	clk.add(2 * time.Hour)
	if n := b.Sweep(); n != 1 {
		t.Fatalf("swept %d, want just the default-TTL reservation", n)
	}
	if got := b.inUseOf(cgroupKey("acct-1")); got != 1 {
		t.Fatalf("in use = %d, want 1 (the never-expiring reservation)", got)
	}
	forever.Release()
	short.Release()
	long.Release()
}
