package scenario

import (
	"context"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/capacity"
)

// DESIGN §14 scenarios 1, 2, 3 and 17 — multi-axis capacity.
//
// Each of the first three is a statement about which axis stops the request,
// and all three are trivially satisfiable by a broker that counts nothing and
// blocks at an arbitrary number. Every one therefore carries its inverse: the
// same setup with the constraint under test removed, asserting that the request
// which blocked now succeeds. Without the inverse "the 7th waits" is also
// satisfied by "everything after the 6th waits, always".

func newBroker(t *testing.T, cfg capacity.Config) *capacity.Broker {
	t.Helper()
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = -1 // no background goroutine; scenarios drive Sweep
	}
	b := capacity.New(cfg)
	t.Cleanup(b.Close)
	return b
}

func mustAcquire(t *testing.T, b *capacity.Broker, req capacity.Request) *capacity.Reservation {
	t.Helper()
	res, ok := b.TryAcquire(req)
	if !ok {
		t.Fatalf("TryAcquire refused a request that should have been admitted; snapshot: %+v", b.Snapshot())
	}
	return res
}

func axisInUse(t *testing.T, b *capacity.Broker, a capacity.Axis, key, sub string) int {
	t.Helper()
	n, ok := b.InUse(a, key, sub)
	if !ok {
		t.Fatalf("axis %s key %q/%q is unknown to the broker", a, key, sub)
	}
	return n
}

// -----------------------------------------------------------------------------
// §14.1 — two accounts × 3 concurrent → the 7th waits; a release admits it
// -----------------------------------------------------------------------------

func TestScenario01_TwoAccountsThreeEachBlocksTheSeventh(t *testing.T) {
	const perAccount = 3

	twoAccounts := func(limit int) (*capacity.Broker, capacity.Request) {
		b := newBroker(t, capacity.Config{
			CredentialGroups: map[string]int{"acct-a": limit, "acct-b": limit},
		})
		req := capacity.Request{
			Provider: "plan-vendor", Model: "zai:glm-5.1",
			Candidates: []capacity.Candidate{
				{ID: "plan-1", CapacityGroup: "acct-a"},
				{ID: "plan-2", CapacityGroup: "acct-b"},
			},
			// Unpinned traffic spills between accounts (DESIGN §7.4a2). Without
			// Spill only the first candidate is ever tried and the scenario
			// would block at 3, not 6 — which is a different scenario.
			OnCapacity: capacity.Spill,
		}
		return b, req
	}

	b, req := twoAccounts(perAccount)

	held := make([]*capacity.Reservation, 0, 6)
	for i := range 6 {
		res := mustAcquire(t, b, req)
		held = append(held, res)
		if res.CredentialID() == "" {
			t.Fatalf("acquisition %d selected no credential", i+1)
		}
	}
	if got := axisInUse(t, b, capacity.AxisCredentialGroup, "acct-a", ""); got != perAccount {
		t.Errorf("acct-a in use = %d, want %d", got, perAccount)
	}
	if got := axisInUse(t, b, capacity.AxisCredentialGroup, "acct-b", ""); got != perAccount {
		t.Errorf("acct-b in use = %d, want %d", got, perAccount)
	}

	if !blocks(b, req) {
		t.Fatal("the 7th concurrent request was admitted; both accounts are full")
	}

	// A release admits it. The waiter is parked before anything is released, so
	// this measures the release path rather than a lucky retry.
	type grant struct {
		res *capacity.Reservation
		err error
	}
	out := make(chan grant, 1)
	go func() {
		res, err := b.Acquire(context.Background(), req)
		out <- grant{res, err}
	}()
	waitWaiting(t, b, 1)

	freed := held[0].CredentialID()
	held[0].Release()

	select {
	case g := <-out:
		if g.err != nil {
			t.Fatalf("the parked 7th request failed instead of being admitted: %v", g.err)
		}
		if g.res.CredentialID() != freed {
			t.Errorf("admitted on credential %q, but %q is the one that freed a slot",
				g.res.CredentialID(), freed)
		}
		g.res.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("a release did not admit the waiting request")
	}
	for _, r := range held[1:] {
		r.Release()
	}

	t.Run("inverse: with four per account the 7th does not wait", func(t *testing.T) {
		// If the 7th blocked here too, the assertion above would be measuring
		// an arbitrary ceiling rather than 3+3.
		b, req := twoAccounts(4)
		var held []*capacity.Reservation
		for range 7 {
			held = append(held, mustAcquire(t, b, req))
		}
		for _, r := range held {
			r.Release()
		}
	})
}

// -----------------------------------------------------------------------------
// §14.2 — one key × 7 per model → two models reach 14; the 15th waits
// -----------------------------------------------------------------------------

func TestScenario02_PerModelLimitReachesFourteenAcrossTwoModels(t *testing.T) {
	const perModel = 7
	const m1, m2 = "gemma4:31b", "qwen3.5:397b" // opaque names; nothing splits them

	b := newBroker(t, capacity.Config{
		Models: []capacity.ModelLimit{
			{Provider: "self-hosted", Model: m1, Max: perModel},
			{Provider: "self-hosted", Model: m2, Max: perModel},
		},
	})
	// One key, no ceiling of its own: the model axis is the only constraint.
	req := func(model string) capacity.Request {
		return capacity.Request{
			Provider: "self-hosted", Model: model,
			Candidates: []capacity.Candidate{{ID: "key-1"}},
		}
	}

	var held []*capacity.Reservation
	for range perModel {
		held = append(held, mustAcquire(t, b, req(m1)))
	}
	// The eighth request overall is admitted, because it is the first on the
	// other model. This is the half of the scenario that a per-key limit of 7
	// would fail.
	for range perModel {
		held = append(held, mustAcquire(t, b, req(m2)))
	}
	if len(held) != 2*perModel {
		t.Fatalf("held %d, want %d", len(held), 2*perModel)
	}

	for _, m := range []string{m1, m2} {
		if got := axisInUse(t, b, capacity.AxisModel, "self-hosted", m); got != perModel {
			t.Errorf("model %s in use = %d, want %d", m, got, perModel)
		}
		if !blocks(b, req(m)) {
			t.Errorf("the 15th request was admitted on %s; that model is full", m)
		}
	}

	// Releasing on one model must not admit on the other: the axes are separate.
	held[0].Release()
	if blocks(b, req(m1)) {
		t.Error("a release on m1 did not admit an m1 request")
	}
	held[perModel].Release()
	for _, r := range held[1:perModel] {
		r.Release()
	}
	for _, r := range held[perModel+1:] {
		r.Release()
	}

	t.Run("inverse: the limit is per model, not per key", func(t *testing.T) {
		// A broker that counted per credential instead would block the 8th
		// request, and the scenario above would still pass its first seven
		// assertions.
		b := newBroker(t, capacity.Config{
			Models: []capacity.ModelLimit{
				{Provider: "self-hosted", Model: m1, Max: perModel},
				{Provider: "self-hosted", Model: m2, Max: perModel},
			},
		})
		req := func(model string) capacity.Request {
			return capacity.Request{Provider: "self-hosted", Model: model,
				Candidates: []capacity.Candidate{{ID: "key-1"}}}
		}
		for range perModel {
			mustAcquire(t, b, req(m1))
		}
		if blocks(b, req(m2)) {
			t.Fatal("the 8th request blocked: the ceiling is being counted on the key, not the model")
		}
	})
}

// -----------------------------------------------------------------------------
// §14.3 — both constraints → blocks at the account total, not the model limit
// -----------------------------------------------------------------------------

func TestScenario03_AccountTotalBindsBeforeTheModelLimit(t *testing.T) {
	const accountTotal, perModel = 10, 7
	const m1, m2 = "gemma4:31b", "qwen3.5:397b"

	build := func(withAccountLimit bool) (*capacity.Broker, func(string) capacity.Request) {
		cfg := capacity.Config{
			Models: []capacity.ModelLimit{
				{Provider: "plan-vendor", Model: m1, Max: perModel},
				{Provider: "plan-vendor", Model: m2, Max: perModel},
			},
		}
		if withAccountLimit {
			cfg.CredentialGroups = map[string]int{"acct": accountTotal}
		}
		b := newBroker(t, cfg)
		return b, func(model string) capacity.Request {
			return capacity.Request{
				Provider: "plan-vendor", Model: model,
				Candidates: []capacity.Candidate{{ID: "plan-1", CapacityGroup: "acct"}},
			}
		}
	}

	b, req := build(true)
	models := []string{m1, m2}
	var held []*capacity.Reservation
	for i := range accountTotal {
		held = append(held, mustAcquire(t, b, req(models[i%2])))
	}

	// The binding constraint is the account, and the proof is that BOTH model
	// axes still have room. Asserting only "the 11th blocks" would be satisfied
	// by a model limit of 5.
	for _, m := range models {
		got := axisInUse(t, b, capacity.AxisModel, "plan-vendor", m)
		want := accountTotal / 2
		if got != want {
			t.Errorf("model %s in use = %d, want %d", m, got, want)
		}
		if got >= perModel {
			t.Errorf("model %s is at its own limit (%d/%d); the scenario needs it to have room",
				m, got, perModel)
		}
	}
	if got := axisInUse(t, b, capacity.AxisCredentialGroup, "acct", ""); got != accountTotal {
		t.Fatalf("account in use = %d, want %d", got, accountTotal)
	}
	for _, m := range models {
		if !blocks(b, req(m)) {
			t.Errorf("the 11th request was admitted on %s; the account total is %d", m, accountTotal)
		}
	}
	for _, r := range held {
		r.Release()
	}

	t.Run("inverse: without the account limit the 11th is admitted", func(t *testing.T) {
		// The model axes allow 14 together, so an 11th request must succeed
		// once the account ceiling is gone. If it did not, the assertion above
		// would be about the model limit after all.
		b, req := build(false)
		for i := range 11 {
			mustAcquire(t, b, req(models[i%2]))
		}
	})
}

// -----------------------------------------------------------------------------
// §14.17 — saturation: 1000 waiters on a limit of 7
// -----------------------------------------------------------------------------

type grantOrder struct {
	idx int
	res *capacity.Reservation
	err error
}

// drainFIFO parks n waiters on a broker whose only free slot is the one this
// function releases, then hands the slot along one waiter at a time. Serializing
// the cascade is what makes "FIFO" a checkable claim: with several slots freed
// at once, several waiters wake concurrently and the order they report in is a
// property of the Go scheduler, not of the broker.
//
// It returns how many head-of-queue probes the whole drain cost.
func drainFIFO(t *testing.T, limit, n int) uint64 {
	t.Helper()
	b := newBroker(t, capacity.Config{
		Models: []capacity.ModelLimit{{Provider: "p", Model: "m", Max: limit}},
	})
	req := capacity.Request{Provider: "p", Model: "m"}

	held := make([]*capacity.Reservation, limit)
	for i := range held {
		held[i] = mustAcquire(t, b, req)
	}
	if !blocks(b, req) {
		t.Fatalf("the broker admitted request %d against a limit of %d", limit+1, limit)
	}

	out := make(chan grantOrder, n)
	hold := make(chan struct{})
	for i := range n {
		go func() {
			res, err := b.Acquire(context.Background(), req)
			out <- grantOrder{i, res, err}
			if err == nil {
				<-hold
				res.Release()
			}
		}()
		// Arrival order is established here and nowhere else.
		waitWaiting(t, b, i+1)
	}

	before := b.Snapshot().Wakeups
	held[0].Release()
	for i := range n {
		var g grantOrder
		select {
		case g = <-out:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d waiters were served: a waiter is starving", i, n)
		}
		if g.err != nil {
			t.Fatalf("waiter %d failed: %v", g.idx, g.err)
		}
		if g.idx != i {
			t.Fatalf("grant %d went to waiter %d: FIFO order was lost", i, g.idx)
		}
		hold <- struct{}{} // that waiter releases, handing the slot to the next
	}
	after := b.Snapshot().Wakeups

	for _, r := range held[1:] {
		r.Release()
	}
	if got := b.Snapshot().Waiting; got != 0 {
		t.Fatalf("waiting = %d after the drain, want 0", got)
	}
	return after - before
}

func TestScenario17_SaturationBoundedWakeupsFIFONoStarvation(t *testing.T) {
	const limit = 7

	var many, few uint64
	t.Run("1000 waiters, FIFO, all served", func(t *testing.T) {
		many = drainFIFO(t, limit, 1000)
	})
	t.Run("100 waiters, FIFO, all served", func(t *testing.T) {
		few = drainFIFO(t, limit, 100)
	})

	t.Run("wakeups are bounded per release, not per waiter", func(t *testing.T) {
		// The drain performs one release per grant, so the number of releases is
		// the number of waiters. If a release woke every waiter — a broadcast
		// rather than a targeted wakeup — the per-release figure would grow with
		// the queue and the 1000-waiter run would cost roughly ten times more
		// per release than the 100-waiter one.
		perMany := float64(many) / 1000
		perFew := float64(few) / 100
		t.Logf("wakeups per release: %.2f at 1000 waiters, %.2f at 100", perMany, perFew)

		const ceiling = 1 + capacity.DefaultWakeSlack
		if perMany > ceiling {
			t.Errorf("%.2f probes per release at 1000 waiters exceeds the %d bound", perMany, ceiling)
		}
		if perFew > ceiling {
			t.Errorf("%.2f probes per release at 100 waiters exceeds the %d bound", perFew, ceiling)
		}
		// The load-bearing comparison: the cost of one release must not scale
		// with how many waiters exist.
		if perFew > 0 && perMany > 2*perFew {
			t.Errorf("wakeups per release grew from %.2f to %.2f as waiters went from 100 to 1000: "+
				"targeted wakeup has regressed toward a broadcast", perFew, perMany)
		}
	})

	t.Run("a multi-axis waiter is not overtaken by single-axis waiters", func(t *testing.T) {
		// The starvation shape DESIGN §5.4 names: a request needing two axes
		// waits for both, and a stream of requests needing only one of them
		// must not keep stepping in front of it. Effective priority is the
		// original arrival sequence, so an earlier multi-axis waiter wins.
		b := newBroker(t, capacity.Config{
			Models: []capacity.ModelLimit{
				{Provider: "p", Model: "wide", Max: 1},
				{Provider: "p", Model: "narrow", Max: 1},
			},
			CredentialGroups: map[string]int{"acct": 1},
		})
		wide := capacity.Request{Provider: "p", Model: "wide",
			Candidates: []capacity.Candidate{{ID: "k", CapacityGroup: "acct"}}}
		narrow := capacity.Request{Provider: "p", Model: "narrow",
			Candidates: []capacity.Candidate{{ID: "k", CapacityGroup: "acct"}}}

		blocker := mustAcquire(t, b, wide)

		type res struct {
			who string
			r   *capacity.Reservation
		}
		got := make(chan res, 4)
		// The two-axis waiter arrives first: it needs both the account and the
		// "wide" model.
		go func() {
			r, err := b.Acquire(context.Background(), wide)
			if err == nil {
				got <- res{"wide", r}
			}
		}()
		waitWaiting(t, b, 1)
		// Then a stream of waiters that only need the account and a model
		// nobody is holding.
		for range 3 {
			go func() {
				r, err := b.Acquire(context.Background(), narrow)
				if err == nil {
					got <- res{"narrow", r}
				}
			}()
		}
		waitWaiting(t, b, 4)

		blocker.Release()
		select {
		case g := <-got:
			if g.who != "wide" {
				t.Fatalf("the first grant went to a later single-axis waiter (%s): "+
					"the earlier multi-axis waiter was overtaken", g.who)
			}
			g.r.Release()
		case <-time.After(5 * time.Second):
			t.Fatal("nobody was served")
		}
		// Drain the rest so the broker closes clean.
		for range 3 {
			select {
			case g := <-got:
				g.r.Release()
			case <-time.After(5 * time.Second):
				t.Fatal("a single-axis waiter starved behind the multi-axis one")
			}
		}
	})
}
