package capacity

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ordered is a grant tagged with the arrival index of the waiter that got it.
type ordered struct {
	idx int
	res *Reservation
	err error
}

// ---------------------------------------------------------------------------
// FIFO within an axis
// ---------------------------------------------------------------------------

func TestFIFOWithinAxis(t *testing.T) {
	b := newBroker(t, Config{Models: []ModelLimit{{Provider: "p", Model: "m", Max: 1}}})
	req := Request{Provider: "p", Model: "m"}

	held := mustAcquire(t, b, req)

	const n = 50
	out := make(chan ordered, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			res, err := b.Acquire(context.Background(), req)
			out <- ordered{i, res, err}
		}()
		// Wait for this waiter to be parked before launching the next, so
		// arrival order is unambiguous.
		waitQueued(t, b, i+1)
	}

	held.Release()
	for i := 0; i < n; i++ {
		var g ordered
		select {
		case g = <-out:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d waiters served", i, n)
		}
		if g.err != nil {
			t.Fatalf("waiter %d failed: %v", g.idx, g.err)
		}
		if g.idx != i {
			t.Fatalf("grant %d went to waiter %d: FIFO order lost", i, g.idx)
		}
		g.res.Release() // hands the single slot to the next waiter
	}
	if got := b.Snapshot().Waiting; got != 0 {
		t.Fatalf("waiting = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// saturation: bounded wakeups per release
// ---------------------------------------------------------------------------

// TestSaturationBoundedWakeups is the completion gate of DESIGN §5.4: 0, 100 and
// 1000 waiters against limits of 1, 7 and 32, asserting that the work a single
// release does is bounded by a constant rather than by the number of waiters.
func TestSaturationBoundedWakeups(t *testing.T) {
	for _, limit := range []int{1, 7, 32} {
		for _, waiters := range []int{0, 100, 1000} {
			limit, waiters := limit, waiters
			t.Run("limit"+itoa(limit)+"/waiters"+itoa(waiters), func(t *testing.T) {
				saturate(t, limit, waiters)
			})
		}
	}
}

func saturate(t *testing.T, limit, waiters int) {
	t.Helper()
	b := newBroker(t, Config{Models: []ModelLimit{{Provider: "p", Model: "m", Max: limit}}})
	req := Request{Provider: "p", Model: "m"}

	held := make([]*Reservation, limit)
	for i := range held {
		held[i] = mustAcquire(t, b, req)
	}
	mustBlock(t, b, req)

	// Every waiter reports its grant, then parks until released en masse, so the
	// cascade can be driven one release at a time.
	hold := make(chan struct{})
	out := make(chan ordered, waiters)
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := b.Acquire(context.Background(), req)
			out <- ordered{i, res, err}
			if err == nil {
				<-hold
				res.Release()
			}
		}()
	}
	waitFor(t, func() bool { return b.Snapshot().Waiting == waiters }, "all waiters to park")

	// One release must probe a bounded number of head-of-queue waiters and
	// hand out exactly one grant.
	if waiters > 0 {
		before := b.wakeupCount()
		held[0].Release()
		select {
		case g := <-out:
			if g.err != nil {
				t.Fatalf("waiter failed: %v", g.err)
			}
			out <- g // put it back for the accounting below
		case <-time.After(5 * time.Second):
			t.Fatal("release admitted nobody")
		}
		delta := b.wakeupCount() - before
		maxPerRelease := uint64(1 + DefaultWakeSlack)
		if delta > maxPerRelease {
			t.Fatalf("one release probed %d waiters, want <= %d (broadcast regression)",
				delta, maxPerRelease)
		}
		if delta != 1 {
			t.Logf("one release probed %d waiters (%d waiters queued)", delta, waiters)
		}
	} else {
		held[0].Release()
	}

	// Drain everything and account for total work.
	for _, r := range held[1:] {
		r.Release()
	}
	close(hold)
	wg.Wait()

	granted := 0
	for len(out) > 0 {
		g := <-out
		if g.err != nil {
			t.Fatalf("waiter %d failed: %v", g.idx, g.err)
		}
		granted++
	}
	if granted != waiters {
		t.Fatalf("granted %d of %d waiters", granted, waiters)
	}

	// A broadcast implementation would probe O(waiters) per release and so do
	// O(waiters^2) total work. Targeted wakeup is linear with a small constant.
	total := b.wakeupCount()
	budget := uint64(4 * (waiters + limit + 1))
	if total > budget {
		t.Fatalf("total wakeups = %d for %d waiters, want <= %d", total, waiters, budget)
	}
	t.Logf("limit=%d waiters=%d total wakeups=%d (%.2f per grant)",
		limit, waiters, total, float64(total)/float64(maxInt(waiters, 1)))

	if got := b.totalQueueNodes(); got != 0 {
		t.Fatalf("queue nodes = %d after drain, want 0", got)
	}
	for _, a := range b.Snapshot().Axes {
		if a.InUse != 0 {
			t.Fatalf("axis %s in use = %d after drain, want 0", a.Key, a.InUse)
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// a multi-axis waiter is not overtaken by single-axis waiters
// ---------------------------------------------------------------------------

// TestMultiAxisWaiterKeepsItsPlace is the precise property of DESIGN §5.4: a
// waiter bounced from one axis to another carries its original arrival sequence,
// so it goes to the head of its new queue rather than to the back.
//
// Timeline:
//
//	the credential axis is full, the model axis is free
//	W (model + credential) arrives, blocks on the credential axis
//	the model axis is then taken, and N single-axis model waiters queue behind it
//	the credential axis frees: W is probed, now blocks on the model axis, and is
//	  re-queued there ahead of all N even though they were queued there first
//	the model axis frees: W wins
func TestMultiAxisWaiterKeepsItsPlace(t *testing.T) {
	b := newBroker(t, Config{Models: []ModelLimit{{Provider: "p", Model: "m", Max: 1}}})

	credOnly := Request{Provider: "p", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}}
	modelOnly := Request{Provider: "p", Model: "m"}
	both := Request{Provider: "p", Model: "m", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}}

	hCred := mustAcquire(t, b, credOnly)

	multi := make(chan ordered, 1)
	go func() {
		res, err := b.Acquire(context.Background(), both)
		multi <- ordered{-1, res, err}
	}()
	waitQueued(t, b, 1)
	if got := b.queueLen(credAxisKey("p", "c1")); got != 1 {
		t.Fatalf("multi-axis waiter not queued on the credential axis (depth %d)", got)
	}

	hModel := mustAcquire(t, b, modelOnly)

	const n = 20
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	single := make(chan ordered, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			res, err := b.Acquire(ctx, modelOnly)
			single <- ordered{i, res, err}
		}()
		waitQueued(t, b, i+2)
	}
	if got := b.queueLen(modelAxisKey("p", "m")); got != n {
		t.Fatalf("model queue depth = %d, want %d", got, n)
	}

	// The credential axis frees. W cannot be served (the model axis is taken),
	// so it moves to the model queue — and must land at its head.
	hCred.Release()
	waitFor(t, func() bool { return b.queueLen(modelAxisKey("p", "m")) == n+1 },
		"the multi-axis waiter to move to the model queue")
	select {
	case g := <-multi:
		t.Fatalf("multi-axis waiter granted too early: %+v", g)
	default:
	}
	if got := b.queueLen(credAxisKey("p", "c1")); got != 0 {
		t.Fatalf("multi-axis waiter left %d nodes on the credential axis", got)
	}

	// The model axis frees. The oldest waiter is W, not any of the N.
	hModel.Release()
	select {
	case g := <-multi:
		if g.err != nil {
			t.Fatalf("multi-axis waiter failed: %v", g.err)
		}
		defer g.res.Release()
	case g := <-single:
		t.Fatalf("single-axis waiter %d overtook the older multi-axis waiter", g.idx)
	case <-time.After(5 * time.Second):
		t.Fatal("nobody was served")
	}
	select {
	case g := <-single:
		t.Fatalf("single-axis waiter %d served on a one-slot release", g.idx)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	for i := 0; i < n; i++ {
		g := <-single
		if g.err == nil {
			g.res.Release()
		} else if !errors.Is(g.err, context.Canceled) {
			t.Fatalf("single-axis waiter %d: %v", g.idx, g.err)
		}
	}
}

// TestMultiAxisWaiterSurvivesASingleAxisStream is the liveness form: a waiter
// needing two axes must still get in while a stream of single-axis waiters
// hammers one of them.
func TestMultiAxisWaiterSurvivesASingleAxisStream(t *testing.T) {
	b := newBroker(t, Config{Models: []ModelLimit{{Provider: "p", Model: "m", Max: 4}}})

	credOnly := Request{Provider: "p", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}}
	modelOnly := Request{Provider: "p", Model: "m"}
	both := Request{Provider: "p", Model: "m", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}}

	// Saturate the model axis so the stream really contends.
	var held []*Reservation
	for i := 0; i < 4; i++ {
		held = append(held, mustAcquire(t, b, modelOnly))
	}
	hCred := mustAcquire(t, b, credOnly)

	multi := make(chan ordered, 1)
	go func() {
		res, err := b.Acquire(context.Background(), both)
		multi <- ordered{-1, res, err}
	}()
	waitQueued(t, b, 1)

	// A stream of single-axis waiters, all arriving after the multi-axis one.
	stop := make(chan struct{})
	var streamGrants atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				res, err := b.Acquire(ctx, modelOnly)
				cancel()
				if err == nil {
					streamGrants.Add(1)
					res.Release()
				}
			}
		}()
	}

	// The credential the multi-axis waiter needs becomes available.
	hCred.Release()
	releaseAll(held)

	select {
	case g := <-multi:
		if g.err != nil {
			t.Fatalf("multi-axis waiter failed: %v", g.err)
		}
		g.res.Release()
	case <-time.After(5 * time.Second):
		close(stop)
		wg.Wait()
		t.Fatalf("multi-axis waiter starved: %d single-axis grants went past it",
			streamGrants.Load())
	}
	close(stop)
	wg.Wait()
	t.Logf("multi-axis waiter admitted after %d single-axis grants", streamGrants.Load())

	waitFor(t, func() bool { return b.Snapshot().Waiting == 0 }, "stream to drain")
	for _, a := range b.Snapshot().Axes {
		if a.InUse != 0 {
			t.Fatalf("axis %s in use = %d, want 0", a.Key, a.InUse)
		}
	}
}

// ---------------------------------------------------------------------------
// stress
// ---------------------------------------------------------------------------

// TestStressNeverExceedsAnyLimit is the -race workload: many goroutines
// acquiring, spilling, cancelling and releasing across every axis at once,
// with an observer asserting that no counter ever passes its ceiling.
func TestStressNeverExceedsAnyLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in short mode")
	}
	cfg := Config{
		Global:           24,
		Routes:           map[string]int{"plan-a": 16, "cloud-a": 16},
		ProviderGroups:   map[string]int{"pool": 20},
		CredentialGroups: map[string]int{"acct-1": 5, "acct-2": 5, "acct-3": 5},
		Models: []ModelLimit{
			{Provider: "plan-a", Model: "m1", Max: 7},
			{Provider: "plan-a", Model: "m2", Max: 7},
			{Provider: "cloud-a", Model: "m1", Max: 3},
		},
		Principals:         map[string]int{"default": 8, "vip": 12},
		InteractiveReserve: 0.25,
	}
	limits := map[string]int{
		"global:":            24,
		"route:plan-a":       16,
		"route:cloud-a":      16,
		"pgroup:pool":        20,
		"cgroup:acct-1":      5,
		"cgroup:acct-2":      5,
		"cgroup:acct-3":      5,
		"model:plan-a|m1":    7,
		"model:plan-a|m2":    7,
		"model:cloud-a|m1":   3,
		"principal:vip":      12,
		"principal:user-0":   8,
		"principal:user-1":   8,
		"principal:user-2":   8,
		"principal:user-3":   8,
		"key:plan-a|acct-1":  4,
		"key:plan-a|acct-2":  4,
		"key:plan-a|acct-3":  4,
		"key:cloud-a|acct-1": 4,
		"key:cloud-a|acct-2": 4,
		"key:cloud-a|acct-3": 4,
	}

	b := newBroker(t, cfg)

	stop := make(chan struct{})
	var observerErr atomic.Pointer[string]
	var obsWG sync.WaitGroup
	obsWG.Add(1)
	go func() {
		defer obsWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, a := range b.Snapshot().Axes {
				want, ok := limits[a.Key]
				if !ok {
					msg := "unknown axis " + a.Key
					observerErr.CompareAndSwap(nil, &msg)
					return
				}
				if a.InUse > want {
					msg := "axis " + a.Key + " in use " + itoa(a.InUse) + " over limit " + itoa(want)
					observerErr.CompareAndSwap(nil, &msg)
					return
				}
				if a.InUse < 0 {
					msg := "axis " + a.Key + " went negative"
					observerErr.CompareAndSwap(nil, &msg)
					return
				}
			}
		}
	}()

	const workers = 64
	const iters = 300
	var wg sync.WaitGroup
	var granted, refused, cancelled atomic.Int64
	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w) * 7919))
			for i := 0; i < iters; i++ {
				req := randomRequest(rng, w)
				switch rng.Intn(4) {
				case 0:
					if res, ok := b.TryAcquire(req); ok {
						granted.Add(1)
						res.Release()
					} else {
						refused.Add(1)
					}
				default:
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					res, err := b.Acquire(ctx, req)
					if err == nil {
						granted.Add(1)
						if rng.Intn(8) == 0 {
							// Some holders dawdle, so queues actually form.
							time.Sleep(time.Duration(rng.Intn(200)) * time.Microsecond)
						}
						res.Release()
					} else if errors.Is(err, context.DeadlineExceeded) {
						cancelled.Add(1)
					} else if !errors.Is(err, ErrUnsatisfiable) {
						t.Errorf("unexpected error: %v", err)
					}
					cancel()
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	obsWG.Wait()

	if msg := observerErr.Load(); msg != nil {
		t.Fatalf("invariant violated: %s", *msg)
	}
	t.Logf("granted=%d refused=%d timed-out=%d", granted.Load(), refused.Load(), cancelled.Load())

	// Everything must have come back.
	waitFor(t, func() bool { return b.Snapshot().Waiting == 0 }, "waiters to drain")
	s := b.Snapshot()
	if s.Reservations != 0 {
		t.Fatalf("live reservations = %d, want 0", s.Reservations)
	}
	for _, a := range s.Axes {
		if a.InUse != 0 || a.Waiting != 0 {
			t.Fatalf("axis %s = %+v, want empty", a.Key, a)
		}
	}
	if got := b.totalQueueNodes(); got != 0 {
		t.Fatalf("queue nodes = %d, want 0", got)
	}
}

func randomRequest(rng *rand.Rand, worker int) Request {
	providers := []string{"plan-a", "cloud-a"}
	models := []string{"m1", "m2"}
	accts := []string{"acct-1", "acct-2", "acct-3"}

	provider := providers[rng.Intn(len(providers))]
	principal := "vip"
	if worker%2 == 0 {
		principal = "user-" + itoa(worker%4)
	}

	cands := make([]Candidate, 0, 3)
	start := rng.Intn(len(accts))
	for i := 0; i < len(accts); i++ {
		id := accts[(start+i)%len(accts)]
		cands = append(cands, Candidate{
			ID:            id,
			CapacityGroup: id,
			MaxConcurrent: 4,
		})
	}
	// Occasionally let a candidate live on the other provider, exercising the
	// cross-provider spill path.
	if rng.Intn(3) == 0 {
		other := providers[(rng.Intn(len(providers))+1)%len(providers)]
		cands[len(cands)-1].Provider = other
		cands[len(cands)-1].UpstreamModel = "m1"
	}

	onCap := Spill
	if rng.Intn(3) == 0 {
		onCap = Wait
	}
	return Request{
		Provider:      provider,
		Model:         models[rng.Intn(len(models))],
		ProviderGroup: "pool",
		PrincipalID:   principal,
		Candidates:    cands,
		Preferred:     cands[rng.Intn(len(cands))].ID,
		OnCapacity:    onCap,
		Batch:         rng.Intn(4) == 0,
	}
}
