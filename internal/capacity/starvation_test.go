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

// ---------------------------------------------------------------------------
// W8: a two-axis waiter under sustained saturation of both axes
// ---------------------------------------------------------------------------

// axisStream is one single-axis workload that the *test* owns end to end.
//
// The starvation of DESIGN §5.4 correction 5 is real but it is not reproducible
// by racing goroutines: two spinning streams leave gaps in which both axes
// happen to be free at once, and the two-axis waiter gets in by luck. So the
// test does the scheduling itself. Every reservation on both axes is handed to
// the test goroutine, which releases exactly one at a time and, before each
// release, guarantees that a single-axis successor is already parked on that
// axis. The freed unit is therefore offered to a queued waiter inside Release,
// under the broker lock, and the axis is never observably free.
type axisStream struct {
	name   string
	req    Request
	key    axisKey
	out    chan *Reservation
	held   []*Reservation
	queued int
}

// TestTwoAxisWaiterServedUnderSustainedSaturation is the W8 regression.
//
// A waiter needing both the model axis and the credential-key axis is pitted
// against a stream of single-axis waiters on each, with both axes held
// saturated by construction. With soft reservations on it must be served within
// a bounded number of releases. With them off it is never served at all, which
// is the hole this test exists to prove is closed — and which is what makes the
// bounded assertion above a real assertion rather than a coincidence.
func TestTwoAxisWaiterServedUnderSustainedSaturation(t *testing.T) {
	// The bound the protocol promises is SoftReserveAfter + (axes needed)
	// releases of the blocking axes once the waiter is oldest, so 4 + 2 = 6 at
	// the shipped default. This runs at the default deliberately — the number
	// worth defending is the one that ships — and leaves headroom so a slower
	// but still bounded route is not called a failure.
	const maxReleases = 12

	for _, tc := range []struct {
		name       string
		mode       SoftReservationMode
		wantServed bool
	}{
		{"soft-reservations-on", SoftReservationsOn, true},
		{"soft-reservations-off", SoftReservationsOff, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			releases, served := runStarvationSchedule(t, tc.mode, maxReleases)
			switch {
			case tc.wantServed && !served:
				t.Fatalf("two-axis waiter starved: still queued after %d releases "+
					"with both of its axes saturated (DESIGN §5.4 correction 5, W8)",
					releases)
			case !tc.wantServed && served:
				t.Fatalf("the two-axis waiter was served after %d releases with the "+
					"guard off. Either the schedule stopped being adversarial or "+
					"starvation is now closed by some other mechanism; if the "+
					"latter, delete this branch rather than weakening it.", releases)
			case tc.wantServed:
				if releases > maxReleases {
					t.Fatalf("two-axis waiter served after %d releases, want <= %d",
						releases, maxReleases)
				}
				t.Logf("two-axis waiter served after %d releases", releases)
			default:
				t.Logf("starvation reproduced: not served after %d releases", releases)
			}
		})
	}
}

// runStarvationSchedule drives the adversarial schedule and reports how many
// releases it took and whether the two-axis waiter was ever served.
func runStarvationSchedule(t *testing.T, mode SoftReservationMode, maxReleases int) (int, bool) {
	t.Helper()

	b := newBroker(t, Config{
		Models:           []ModelLimit{{Provider: "p", Model: "m", Max: 1}},
		SoftReservations: mode,
	})

	both := Request{
		Provider:   "p",
		Model:      "m",
		Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}},
	}
	sa := &axisStream{
		name: "model",
		req:  Request{Provider: "p", Model: "m"},
		key:  modelAxisKey("p", "m"),
		out:  make(chan *Reservation, 64),
	}
	sb := &axisStream{
		name: "key",
		req:  Request{Provider: "p", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}},
		key:  credAxisKey("p", "c1"),
		out:  make(chan *Reservation, 64),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Both axes start occupied, by the test.
	sa.held = append(sa.held, mustAcquire(t, b, sa.req))
	sb.held = append(sb.held, mustAcquire(t, b, sb.req))

	// The two-axis waiter arrives first, so it is the oldest waiter throughout
	// and aging is not what is being tested: it is never overtaken, it just
	// never wins.
	wOut := make(chan result, 1)
	launch(b, ctx, both, wOut)
	waiting := 1
	waitQueued(t, b, waiting)

	// Single-axis waiters are one-shot: they hand their reservation to the test
	// and exit, so the test — not the goroutine — decides when it comes back.
	var spawned sync.WaitGroup
	spawn := func(s *axisStream) {
		spawned.Add(1)
		go func() {
			defer spawned.Done()
			res, err := b.Acquire(ctx, s.req)
			if err == nil {
				s.out <- res
			}
		}()
		s.queued++
		waiting++
		waitQueued(t, b, waiting)
	}

	releases := 0
	var won *result

	// releaseOne gives one unit of an axis back and accounts for where it went.
	// Exactly one of three things happens, all of them settled before Release
	// returns: the unit was set aside as a soft reservation for the two-axis
	// waiter, it was granted to that waiter, or it was granted to the
	// single-axis successor parked on that axis.
	releaseOne := func(s *axisStream) {
		if won != nil || len(s.held) == 0 {
			return
		}
		r := s.held[len(s.held)-1]
		s.held = s.held[:len(s.held)-1]
		r.Release()
		releases++

		if b.softReservedOn(s.key) {
			return
		}
		select {
		case res := <-s.out:
			s.held = append(s.held, res)
			s.queued--
			waiting--
		case g := <-wOut:
			won = &g
			waiting--
		case <-time.After(5 * time.Second):
			t.Fatalf("release on the %s axis was neither granted nor set aside", s.name)
		}
	}

	for round := 0; round < maxReleases && won == nil; round++ {
		for _, s := range []*axisStream{sa, sb} {
			if won != nil {
				break
			}
			if s.queued == 0 {
				spawn(s) // never release into an empty queue
			}
			releaseOne(s)
		}
	}

	if won == nil {
		// The grant travels through the waiter's own goroutine, so give it a
		// moment to surface before calling it starvation.
		select {
		case g := <-wOut:
			won = &g
		case <-time.After(200 * time.Millisecond):
		}
	}

	served := won != nil
	if served {
		if won.err != nil {
			t.Fatalf("two-axis waiter failed: %v", won.err)
		}
		s := b.Snapshot()
		if s.SoftReservations == 0 && mode == SoftReservationsOn {
			t.Fatal("the waiter was served without a soft reservation ever being placed")
		}
		won.res.Release()
	}

	// Teardown: give everything back and prove nothing was left behind.
	cancel()
	releaseAll(sa.held)
	releaseAll(sb.held)
	spawned.Wait()
	for _, s := range []*axisStream{sa, sb} {
		for {
			select {
			case res := <-s.out:
				res.Release()
				continue
			default:
			}
			break
		}
	}
	if !served {
		if r := <-wOut; r.err == nil {
			r.res.Release()
		}
	}

	waitFor(t, func() bool { return b.Snapshot().Waiting == 0 }, "waiters to drain")
	if got := b.totalClaims(); got != 0 {
		t.Fatalf("%d soft reservations left behind: idled capacity leaked", got)
	}
	if got := b.totalQueueNodes(); got != 0 {
		t.Fatalf("queue nodes = %d after drain, want 0", got)
	}
	for _, a := range b.Snapshot().Axes {
		if a.InUse != 0 {
			t.Fatalf("axis %s in use = %d after drain, want 0", a.Key, a.InUse)
		}
	}
	return releases, served
}

// ---------------------------------------------------------------------------
// a soft reservation must not survive its holder
// ---------------------------------------------------------------------------

// TestCancelledWaiterGivesBackItsSoftReservation covers the lost-wakeup shape:
// a claim is capacity being kept idle on purpose, so a waiter that goes away
// while holding one must both drop it and re-offer it, or the unit sits idle
// until some unrelated release happens to touch that axis.
func TestCancelledWaiterGivesBackItsSoftReservation(t *testing.T) {
	b := newBroker(t, Config{
		Models:           []ModelLimit{{Provider: "p", Model: "m", Max: 1}},
		SoftReservations: SoftReservationsOn,
		// Arm on the first failed probe: these tests are about the mechanism,
		// not about the threshold that decides when it is worth using.
		SoftReserveAfter: 1,
	})
	modelOnly := Request{Provider: "p", Model: "m"}
	keyOnly := Request{Provider: "p", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}}
	both := Request{Provider: "p", Model: "m", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}}

	hModel := mustAcquire(t, b, modelOnly)
	hKey := mustAcquire(t, b, keyOnly)

	// The two-axis waiter is oldest; a single-axis waiter queues behind it on
	// the model axis and must inherit the unit when the claim is dropped.
	ctx, cancel := context.WithCancel(context.Background())
	multi := make(chan result, 1)
	launch(b, ctx, both, multi)
	waitQueued(t, b, 1)

	single := make(chan result, 1)
	launch(b, context.Background(), modelOnly, single)
	waitQueued(t, b, 2)

	// The model axis frees. The two-axis waiter still cannot go (the key axis
	// is held), so it sets the model unit aside rather than losing it.
	hModel.Release()
	if !b.softReservedOn(modelAxisKey("p", "m")) {
		t.Fatal("the freed model unit was not set aside for the older two-axis waiter")
	}
	select {
	case r := <-single:
		t.Fatalf("single-axis waiter took a unit reserved for an older waiter: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}

	// The two-axis waiter goes away. Its claim must come back *and* be offered
	// to the queue in the same critical section.
	cancel()
	r := <-multi
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("multi-axis waiter err = %v, want context.Canceled", r.err)
	}
	select {
	case g := <-single:
		if g.err != nil {
			t.Fatalf("single-axis waiter failed: %v", g.err)
		}
		g.res.Release()
	case <-time.After(3 * time.Second):
		t.Fatal("the dropped soft reservation was never re-offered: lost wakeup")
	}

	hKey.Release()
	if got := b.totalClaims(); got != 0 {
		t.Fatalf("claims = %d, want 0", got)
	}
	for _, a := range b.Snapshot().Axes {
		if a.InUse != 0 {
			t.Fatalf("axis %s in use = %d, want 0", a.Key, a.InUse)
		}
	}
}

// TestSoftReservationNeverOvercommits pins the counting invariant of doc.go:
// while a unit of an axis is set aside, the axis admits limit-1 others, and the
// claimant's own admission still succeeds.
func TestSoftReservationNeverOvercommits(t *testing.T) {
	const limit = 3
	b := newBroker(t, Config{
		Models:           []ModelLimit{{Provider: "p", Model: "m", Max: limit}},
		SoftReservations: SoftReservationsOn,
		// Arm on the first failed probe: these tests are about the mechanism,
		// not about the threshold that decides when it is worth using.
		SoftReserveAfter: 1,
	})
	modelOnly := Request{Provider: "p", Model: "m"}
	keyOnly := Request{Provider: "p", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}}
	both := Request{Provider: "p", Model: "m", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}}

	var held []*Reservation
	for i := 0; i < limit; i++ {
		held = append(held, mustAcquire(t, b, modelOnly))
	}
	hKey := mustAcquire(t, b, keyOnly)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	multi := make(chan result, 1)
	launch(b, ctx, both, multi)
	waitQueued(t, b, 1)

	// One model unit comes back and is set aside.
	held[0].Release()
	if !b.softReservedOn(modelAxisKey("p", "m")) {
		t.Fatal("freed unit not set aside")
	}
	// The axis is at limit-1 for everybody else, so nothing may be admitted.
	mustBlock(t, b, modelOnly)

	// A second unit comes back: now one is reserved and one is genuinely free.
	held[1].Release()
	spare, ok := b.TryAcquire(modelOnly)
	if !ok {
		t.Fatal("the unit that was not reserved must still be usable")
	}
	mustBlock(t, b, modelOnly)

	// The claimant's own axis check must pass over its own reservation.
	hKey.Release()
	select {
	case r := <-multi:
		if r.err != nil {
			t.Fatalf("claimant failed: %v", r.err)
		}
		if got := b.inUseOf(modelAxisKey("p", "m")); got != limit {
			t.Fatalf("model in use = %d, want %d (claim was not consumed by the commit)", got, limit)
		}
		if b.softReservedOn(modelAxisKey("p", "m")) {
			t.Fatal("the commit did not consume the soft reservation")
		}
		r.res.Release()
	case <-time.After(3 * time.Second):
		t.Fatal("claimant not served by its own reserved unit")
	}

	spare.Release()
	held[2].Release()
	if got := b.totalClaims(); got != 0 {
		t.Fatalf("claims = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// the total order on axes: no cycle of soft reservations
// ---------------------------------------------------------------------------

// TestSoftReservationsDoNotDeadlockOnOverlappingNeeds is the deadlock argument
// under load. Waiters need overlapping *pairs* of tight axes, which is exactly
// the shape that deadlocks if claims are taken in whatever order each waiter
// happens to find capacity in. Taking them as a prefix of one global axis order
// is what rules the cycle out; if that ever regresses, this hangs and the
// deadline fires.
func TestSoftReservationsDoNotDeadlockOnOverlappingNeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in short mode") // pragma: allowlist secret — test fixture
	}
	b := newBroker(t, Config{
		Models: []ModelLimit{
			{Provider: "p", Model: "m1", Max: 1},
			{Provider: "p", Model: "m2", Max: 1},
		},
		CredentialGroups: map[string]int{"a1": 1, "a2": 1},
		SoftReservations: SoftReservationsOn,
		// Arm on the first failed probe: these tests are about the mechanism,
		// not about the threshold that decides when it is worth using.
		SoftReserveAfter: 1,
	})

	models := []string{"m1", "m2"}
	accts := []string{"a1", "a2"}

	const workers = 16
	const iters = 400
	var wg sync.WaitGroup
	var granted, timedOut atomic.Int64
	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)*7919 + 13))
			for i := 0; i < iters; i++ {
				req := Request{
					Provider: "p",
					Model:    models[rng.Intn(len(models))],
					Candidates: []Candidate{{
						ID:            accts[rng.Intn(len(accts))],
						CapacityGroup: accts[rng.Intn(len(accts))],
						MaxConcurrent: 1,
					}},
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				res, err := b.Acquire(ctx, req)
				switch {
				case err == nil:
					granted.Add(1)
					res.Release()
				case errors.Is(err, context.DeadlineExceeded):
					timedOut.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
				cancel()
			}
		}()
	}
	wg.Wait()

	if timedOut.Load() != 0 {
		t.Fatalf("%d acquisitions timed out of %d: soft reservations livelocked or deadlocked",
			timedOut.Load(), int64(workers*iters))
	}
	t.Logf("granted=%d soft reservations placed=%d", granted.Load(), b.Snapshot().SoftReservations)

	waitFor(t, func() bool { return b.Snapshot().Waiting == 0 }, "waiters to drain")
	if got := b.totalClaims(); got != 0 {
		t.Fatalf("claims = %d after drain, want 0", got)
	}
	if got := b.totalQueueNodes(); got != 0 {
		t.Fatalf("queue nodes = %d after drain, want 0", got)
	}
	for _, a := range b.Snapshot().Axes {
		if a.InUse != 0 {
			t.Fatalf("axis %s in use = %d after drain, want 0", a.Key, a.InUse)
		}
	}
}

// ---------------------------------------------------------------------------
// the §5.7 sharding invariant
// ---------------------------------------------------------------------------

// TestSoftReservationsStayWithinOneShard asserts DESIGN §5.7 for the new state:
// a soft reservation set is taken over the axes of *one* candidate, so every
// provider-scoped key it touches belongs to a single provider. A claim set that
// spanned providers would span shards, which is what §5.7 forbids by
// construction.
func TestSoftReservationsStayWithinOneShard(t *testing.T) {
	b := newBroker(t, Config{
		Routes: map[string]int{"plan-a": 2, "cloud-a": 1},
		Models: []ModelLimit{
			{Provider: "plan-a", Model: "mx", Max: 2},
			{Provider: "cloud-a", Model: "mx:cloud", Max: 1},
		},
		CredentialGroups: map[string]int{"acct-1": 1, "acct-2": 1},
		SoftReservations: SoftReservationsOn,
		// Arm on the first failed probe: these tests are about the mechanism,
		// not about the threshold that decides when it is worth using.
		SoftReserveAfter: 1,
	})

	// A spill request whose candidates live on different providers. The claim
	// candidate is the preferred one, on plan-a.
	req := Request{
		Provider:   "plan-a",
		Model:      "mx",
		OnCapacity: Spill,
		Candidates: []Candidate{
			{ID: "acct-1", CapacityGroup: "acct-1"},
			{ID: "acct-2", CapacityGroup: "acct-2", Provider: "cloud-a", UpstreamModel: "mx:cloud"},
		},
	}
	// Occupy the credential group of the preferred candidate, so plan-a's route
	// and model axes are claimable while its credential group is not. Occupy
	// cloud-a's route so the second candidate is blocked too.
	hCred := mustAcquire(t, b, Request{
		Provider:   "plan-a",
		Candidates: []Candidate{{ID: "acct-1", CapacityGroup: "acct-1"}},
	})
	hCloud := mustAcquire(t, b, Request{
		Provider:   "cloud-a",
		Model:      "mx:cloud",
		Candidates: []Candidate{{ID: "acct-2", CapacityGroup: "acct-2"}},
	})

	// Fill plan-a's route so the waiter blocks there first, and so there is
	// something to give back that makes it probe.
	hRoute := mustAcquire(t, b, Request{Provider: "plan-a", Model: "mx"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan result, 1)
	launch(b, ctx, req, out)
	waitQueued(t, b, 1)

	// Free plan-a's route: the waiter is probed, still cannot go, and sets
	// aside what it can along its claim candidate.
	hRoute.Release()

	claimed := map[string]bool{}
	for _, a := range b.Snapshot().Axes {
		if a.SoftReserved {
			claimed[a.Key] = true
		}
	}
	if len(claimed) == 0 {
		t.Fatal("no soft reservation was placed")
	}
	for key := range claimed {
		switch key {
		case "route:plan-a", "model:plan-a|mx", "cgroup:acct-1", "key:plan-a|acct-1":
		default:
			t.Fatalf("soft reservation on %q leaves the claim candidate's shard "+
				"(DESIGN §5.7): claims = %v", key, claimed)
		}
	}
	t.Logf("claims = %v", claimed)

	hCred.Release()
	r := <-out
	if r.err != nil {
		t.Fatalf("waiter failed: %v", r.err)
	}
	if got := r.res.CredentialID(); got != "acct-1" {
		t.Fatalf("served through %q, want the claim candidate acct-1", got)
	}
	r.res.Release()
	hCloud.Release()

	if got := b.totalClaims(); got != 0 {
		t.Fatalf("claims = %d, want 0", got)
	}
	for _, a := range b.Snapshot().Axes {
		if a.InUse != 0 {
			t.Fatalf("axis %s in use = %d, want 0", a.Key, a.InUse)
		}
	}
}

// TestSoftReserveAfterArmsTheGuard checks the threshold does what it says: a
// waiter below it idles nothing, and the same waiter above it sets a unit aside.
// The threshold is the knob that buys the throughput back, so an off-by-one in
// it is an off-by-one in the cost.
func TestSoftReserveAfterArmsTheGuard(t *testing.T) {
	const after = 3
	b := newBroker(t, Config{
		Models:           []ModelLimit{{Provider: "p", Model: "m", Max: 1}},
		SoftReservations: SoftReservationsOn,
		SoftReserveAfter: after,
	})
	modelOnly := Request{Provider: "p", Model: "m"}
	keyOnly := Request{Provider: "p", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}}
	both := Request{Provider: "p", Model: "m", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}}

	hModel := mustAcquire(t, b, modelOnly)
	hKey := mustAcquire(t, b, keyOnly)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan result, 1)
	launch(b, ctx, both, out)
	waitQueued(t, b, 1)

	// Bounce the waiter between the two axes, one failed probe at a time: hand
	// back the axis it is currently queued on, which sends it to the other one,
	// then take that axis straight back so it stays saturated. The waiter starts
	// on the model axis because that is the lower of the two in axis order,
	// which is also the axis it will eventually claim.
	modelKey := modelAxisKey("p", "m")
	credKey := credAxisKey("p", "c1")
	for i := 1; i <= after; i++ {
		onModel := i%2 == 1
		if onModel {
			hModel.Release()
		} else {
			hKey.Release()
		}
		armed := b.softReservedOn(modelKey) || b.softReservedOn(credKey)
		switch {
		case i < after && armed:
			t.Fatalf("guard armed after %d failed probes, want %d", i, after)
		case i == after && !armed:
			t.Fatalf("guard did not arm after %d failed probes", after)
		case i == after:
			if !b.softReservedOn(modelKey) {
				t.Fatal("the claim was not taken on the lower axis in axis order")
			}
			continue // leave the reserved unit alone
		}
		if onModel {
			hModel = mustAcquire(t, b, modelOnly)
		} else {
			hKey = mustAcquire(t, b, keyOnly)
		}
	}
	// Armed: the model unit is set aside and nobody else may have it.
	mustBlock(t, b, modelOnly)

	hKey.Release()
	select {
	case r := <-out:
		if r.err != nil {
			t.Fatalf("waiter failed: %v", r.err)
		}
		r.res.Release()
	case <-time.After(3 * time.Second):
		t.Fatal("armed waiter was not served by its own reserved unit")
	}
	if b.softReservedOn(modelKey) || b.softReservedOn(credKey) {
		t.Fatal("soft reservation outlived its waiter")
	}
}

// TestCloseReleasesSoftReservations covers the other way a waiter leaves the
// wait state. Close fails every waiter, so every unit being kept idle for one
// has to come back — and it must come back without Close trying to serve the
// queues it is in the middle of tearing down.
func TestCloseReleasesSoftReservations(t *testing.T) {
	b := New(Config{
		Models:           []ModelLimit{{Provider: "p", Model: "m", Max: 1}},
		SweepInterval:    -1,
		ReservationTTL:   -1,
		SoftReservations: SoftReservationsOn,
		SoftReserveAfter: 1,
	})
	modelOnly := Request{Provider: "p", Model: "m"}
	keyOnly := Request{Provider: "p", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}}
	both := Request{Provider: "p", Model: "m", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 1}}}

	hModel := mustAcquire(t, b, modelOnly)
	hKey := mustAcquire(t, b, keyOnly)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan result, 1)
	launch(b, ctx, both, out)
	waitQueued(t, b, 1)

	hModel.Release()
	if !b.softReservedOn(modelAxisKey("p", "m")) {
		t.Fatal("no soft reservation to test with")
	}

	b.Close()
	r := <-out
	if !errors.Is(r.err, ErrClosed) {
		t.Fatalf("waiter err = %v, want ErrClosed", r.err)
	}
	if got := b.totalClaims(); got != 0 {
		t.Fatalf("claims = %d after Close, want 0", got)
	}
	hKey.Release() // reservations held across Close stay releasable
}

func TestSoftReservationModeNames(t *testing.T) {
	if got := SoftReservationsDefault.String(); got != "on" {
		t.Fatalf("default mode = %q, want the shipped default %q", got, "on")
	}
	if got := SoftReservationsOn.String(); got != "on" {
		t.Fatalf("on = %q", got)
	}
	if got := SoftReservationsOff.String(); got != "off" {
		t.Fatalf("off = %q", got)
	}
	if got := SoftReservationMode(99).String(); got != "unknown" {
		t.Fatalf("SoftReservationMode(99) = %q", got)
	}
	if !SoftReservationsDefault.enabled() || !SoftReservationsOn.enabled() ||
		SoftReservationsOff.enabled() {
		t.Fatal("mode resolution wrong")
	}
}

// TestBatchWaitersNeverSoftReserve pins the exclusion argued in doc.go: a batch
// claim cannot be honoured against interactive traffic entitled to sit above
// the batch ceiling, so batch never places one.
func TestBatchWaitersNeverSoftReserve(t *testing.T) {
	b := newBroker(t, Config{
		Models:             []ModelLimit{{Provider: "p", Model: "m", Max: 10}},
		InteractiveReserve: 0.3, // batch ceiling 7
		SoftReservations:   SoftReservationsOn,
		SoftReserveAfter:   1,
	})
	// The key axis has limit 2, so the batch ceiling on it is floor(2*0.7) == 1:
	// one interactive holder blocks batch there while leaving room for
	// interactive traffic, which is the situation the exclusion is about.
	batchBoth := Request{
		Provider:   "p",
		Model:      "m",
		Batch:      true,
		Candidates: []Candidate{{ID: "c1", MaxConcurrent: 2}},
	}
	keyOnly := Request{Provider: "p", Candidates: []Candidate{{ID: "c1", MaxConcurrent: 2}}}
	modelOnly := Request{Provider: "p", Model: "m"}

	hKey := mustAcquire(t, b, keyOnly)
	var held []*Reservation
	for i := 0; i < 7; i++ {
		held = append(held, mustAcquire(t, b, modelOnly))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan result, 1)
	launch(b, ctx, batchBoth, out)
	waitQueued(t, b, 1)

	held[0].Release() // model drops to 6: under the batch ceiling
	if b.softReservedOn(modelAxisKey("p", "m")) {
		t.Fatal("a batch waiter placed a soft reservation; see doc.go for why it must not")
	}
	if got := b.Snapshot().SoftReservations; got != 0 {
		t.Fatalf("soft reservations placed = %d, want 0", got)
	}

	hKey.Release()
	select {
	case r := <-out:
		if r.err != nil {
			t.Fatalf("batch waiter failed: %v", r.err)
		}
		r.res.Release()
	case <-time.After(3 * time.Second):
		t.Fatal("batch waiter never admitted")
	}
	releaseAll(held[1:])
}
