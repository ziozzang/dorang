package quota

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestBudget(t *testing.T, limit int64) *Budget {
	t.Helper()
	b, err := NewBudget(BudgetConfig{
		Period:       Monthly,
		DefaultLimit: limit,
		SoftTTL:      time.Minute,
		HardTTL:      10 * time.Minute,
		Now:          func() time.Time { return base },
	})
	if err != nil {
		t.Fatalf("NewBudget: %v", err)
	}
	return b
}

var subject = Subject{Kind: "key", ID: "key-1"}

func TestEstimateIsAnUpperBound(t *testing.T) {
	// Exact input tokens, output priced at max_tokens (DESIGN §6.4).
	e := Estimate{
		InputTokens:        1000,
		MaxOutputTokens:    4096,
		InputNanoPerToken:  3000, // 3 USD per million
		OutputNanoPerToken: 15000,
		FixedNanoUSD:       0,
	}
	want := int64(1000*3000 + 4096*15000)
	if got := e.UpperBound(); got != want {
		t.Fatalf("UpperBound = %d, want %d", got, want)
	}
	// The actual output is normally far below max_tokens, which is what makes
	// the estimate an upper bound rather than a guess.
	actual := int64(1000*3000 + 200*15000)
	if actual >= want {
		t.Fatal("the estimate must dominate a realistic settlement")
	}
}

func TestReserveSettleAndRefundOfTheUnusedPart(t *testing.T) {
	b := newTestBudget(t, NanoUSD(10))
	r, err := b.Reserve(subject, NanoUSD(4), base)
	if err != nil {
		t.Fatal(err)
	}
	s := b.Snapshot(subject, base)
	if s.Reserved != NanoUSD(4) || s.Spent != 0 || s.Available != NanoUSD(6) {
		t.Fatalf("after reserving: %+v", s)
	}
	if err := b.Harden(r.ID, base); err != nil {
		t.Fatal(err)
	}
	// It actually cost 1.
	if err := b.Settle(r.ID, NanoUSD(1), base); err != nil {
		t.Fatal(err)
	}
	s = b.Snapshot(subject, base)
	if s.Reserved != 0 || s.Spent != NanoUSD(1) || s.Available != NanoUSD(9) {
		t.Fatalf("after settling: %+v", s)
	}
	if b.Outstanding() != 0 {
		t.Fatalf("%d reservations outstanding after settlement", b.Outstanding())
	}
	if err := b.Settle(r.ID, 1, base); !errors.Is(err, ErrNoReservation) {
		t.Fatalf("double settlement = %v", err)
	}
}

// TestUnexecutedRequestIsRefundedInFull is R1-6: a request may reserve at the
// gate and then be rejected while waiting for capacity, never reaching an
// upstream. Revision 1 defined settlement only for completion, so that budget
// leaked.
func TestUnexecutedRequestIsRefundedInFull(t *testing.T) {
	b := newTestBudget(t, NanoUSD(10))
	r, err := b.Reserve(subject, NanoUSD(7), base)
	if err != nil {
		t.Fatal(err)
	}
	if r.Hold != SoftHold {
		t.Fatalf("a reservation at the gate is %v, want a soft hold", r.Hold)
	}
	// Capacity is never acquired: the request is refused while queued.
	if err := b.Release(r.ID, base); err != nil {
		t.Fatal(err)
	}
	s := b.Snapshot(subject, base)
	if s.Spent != 0 || s.Reserved != 0 || s.Available != NanoUSD(10) {
		t.Fatalf("an unexecuted request cost money: %+v", s)
	}
	// The full budget is usable again.
	if _, err := b.Reserve(subject, NanoUSD(10), base); err != nil {
		t.Fatalf("the refunded budget was not reusable: %v", err)
	}
}

func TestSoftHoldBecomesHardOnlyAfterCapacity(t *testing.T) {
	b := newTestBudget(t, NanoUSD(10))
	r, err := b.Reserve(subject, NanoUSD(3), base)
	if err != nil {
		t.Fatal(err)
	}
	// A soft hold expires on the gate-to-capacity timescale.
	if want := base.Add(time.Minute); !r.ReservedUntil.Equal(want) {
		t.Fatalf("soft hold expires at %v, want %v", r.ReservedUntil, want)
	}
	if err := b.Harden(r.ID, base); err != nil {
		t.Fatal(err)
	}
	if r.Hold != HardHold {
		t.Fatalf("hold = %v after Harden", r.Hold)
	}
	// A hard hold lives on the dispatch-to-settlement timescale.
	if want := base.Add(10 * time.Minute); !r.ReservedUntil.Equal(want) {
		t.Fatalf("hard hold expires at %v, want %v", r.ReservedUntil, want)
	}
	// Both kinds count against the budget while they are held.
	if s := b.Snapshot(subject, base); s.Reserved != NanoUSD(3) {
		t.Fatalf("reserved = %v", USD(s.Reserved))
	}
	if err := b.Harden(999, base); !errors.Is(err, ErrNoReservation) {
		t.Fatalf("Harden of an unknown id = %v", err)
	}
}

// TestSweepReclaimsAnExpiredReservation is R1-5: without it, a process killed
// between reserve and settle locks that amount forever and the budget is
// eventually exhausted by money nobody spent.
func TestSweepReclaimsAnExpiredReservation(t *testing.T) {
	b := newTestBudget(t, NanoUSD(10))
	dead, err := b.Reserve(subject, NanoUSD(6), base) // the process dies here
	if err != nil {
		t.Fatal(err)
	}
	live, err := b.Reserve(subject, NanoUSD(2), base)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Harden(live.ID, base); err != nil {
		t.Fatal(err)
	}

	// Nothing to reclaim while both are inside their expiry.
	if n, _ := b.SweepExpired(base.Add(30 * time.Second)); n != 0 {
		t.Fatalf("swept %d live reservations", n)
	}
	// After the soft TTL the dead one is reclaimed and the hard one is not.
	n, reclaimed := b.SweepExpired(base.Add(2 * time.Minute))
	if n != 1 || reclaimed != NanoUSD(6) {
		t.Fatalf("swept %d reservations worth %v USD, want 1 worth 6", n, USD(reclaimed))
	}
	s := b.Snapshot(subject, base.Add(2*time.Minute))
	if s.Reserved != NanoUSD(2) || s.Available != NanoUSD(8) {
		t.Fatalf("after the sweep: %+v", s)
	}
	if err := b.Settle(dead.ID, NanoUSD(6), base); !errors.Is(err, ErrNoReservation) {
		t.Fatalf("a swept reservation was still settleable: %v", err)
	}
	// The hard hold goes eventually too.
	if n, _ := b.SweepExpired(base.Add(time.Hour)); n != 1 {
		t.Fatalf("the hard hold was never swept (%d)", n)
	}
	if b.Outstanding() != 0 {
		t.Fatal("reservations survived the sweep")
	}
}

// TestBudgetIsNeverExceededUnderConcurrentReservations is the reason
// reservation exists at all: a check-then-charge would let every one of these
// through.
func TestBudgetIsNeverExceededUnderConcurrentReservations(t *testing.T) {
	const (
		limit  = 100
		each   = 3
		actors = 100
	)
	b := newTestBudget(t, limit)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ok      int
		refused int
		ids     []uint64
	)
	for range actors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := b.Reserve(subject, each, base)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !errors.Is(err, ErrBudgetExceeded) {
					t.Errorf("unexpected error: %v", err)
				}
				refused++
				return
			}
			ok++
			ids = append(ids, r.ID)
		}()
	}
	wg.Wait()

	if ok != limit/each {
		t.Fatalf("%d reservations of %d succeeded against a limit of %d, want %d",
			ok, each, limit, limit/each)
	}
	if refused != actors-ok {
		t.Fatalf("%d refused, %d granted, %d actors", refused, ok, actors)
	}
	s := b.Snapshot(subject, base)
	if s.Reserved > limit {
		t.Fatalf("reserved %d beyond a limit of %d", s.Reserved, limit)
	}

	// Settling every one of them at full price still cannot exceed the limit.
	for _, id := range ids {
		if err := b.Settle(id, each, base); err != nil {
			t.Fatal(err)
		}
	}
	if s := b.Snapshot(subject, base); s.Spent > limit {
		t.Fatalf("spent %d beyond a limit of %d", s.Spent, limit)
	}
}

func TestBudgetExceededIsTerminalNotAFallbackCondition(t *testing.T) {
	b := newTestBudget(t, NanoUSD(1))
	if _, err := b.Reserve(subject, NanoUSD(2), base); err == nil {
		t.Fatal("a reservation past the limit succeeded")
	} else {
		if !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("err = %v, want ErrBudgetExceeded", err)
		}
		if !IsTerminal(err) {
			t.Fatal("budget exceeded must be terminal: failing is the correct outcome")
		}
		var be *BudgetError
		if !errors.As(err, &be) || be.Subject != subject || be.Requested != NanoUSD(2) {
			t.Fatalf("error carries no usable detail: %+v", be)
		}
		if msg := err.Error(); !strings.Contains(msg, "budget exceeded") {
			t.Fatalf("message = %q", msg)
		}
	}
	// A rate-limit-shaped error is not terminal, so the two cannot be confused.
	if IsTerminal(errors.New("some other failure")) {
		t.Fatal("an unrelated error was reported as terminal")
	}
}

func TestUnlimitedBudgetNeverRefuses(t *testing.T) {
	b := newTestBudget(t, 0) // zero means unlimited
	r, err := b.Reserve(subject, NanoUSD(1_000_000), base)
	if err != nil {
		t.Fatalf("an unlimited budget refused a reservation: %v", err)
	}
	if err := b.Settle(r.ID, NanoUSD(1_000_000), base); err != nil {
		t.Fatal(err)
	}
	if s := b.Snapshot(subject, base); s.Limit != 0 || s.Spent != NanoUSD(1_000_000) {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestPerSubjectLimits(t *testing.T) {
	b := newTestBudget(t, NanoUSD(100))
	small := Subject{Kind: "team", ID: "t1"}
	b.SetLimit(small, NanoUSD(5))

	if _, err := b.Reserve(small, NanoUSD(6), base); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("the team's own limit was not applied: %v", err)
	}
	if _, err := b.Reserve(subject, NanoUSD(50), base); err != nil {
		t.Fatalf("the default limit was not applied to another subject: %v", err)
	}
	if got := b.Limit(small); got != NanoUSD(5) {
		t.Fatalf("Limit = %v", USD(got))
	}
	if Global.String() != "global" || subject.String() != "key:key-1" {
		t.Fatalf("subject rendering: %q %q", Global, subject)
	}
}

func TestPeriodRollover(t *testing.T) {
	b := newTestBudget(t, NanoUSD(10))
	r, err := b.Reserve(subject, NanoUSD(8), base)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(r.ID, NanoUSD(8), base); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve(subject, NanoUSD(5), base); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("the period's spend was not counted: %v", err)
	}
	// Next month the spend is gone.
	next := Monthly.PeriodEnd(base)
	if _, err := b.Reserve(subject, NanoUSD(5), next); err != nil {
		t.Fatalf("the budget did not roll over: %v", err)
	}
	s := b.Snapshot(subject, next)
	if s.Spent != 0 || s.PeriodStart != Monthly.PeriodStart(next) {
		t.Fatalf("after rollover: %+v", s)
	}

	// An outstanding reservation survives a rollover: it is money the previous
	// period committed and has not accounted for yet.
	held, err := b.Reserve(subject, NanoUSD(1), next)
	if err != nil {
		t.Fatal(err)
	}
	after := Monthly.PeriodEnd(next)
	if s := b.Snapshot(subject, after); s.Reserved != NanoUSD(6) {
		t.Fatalf("reservations did not survive the rollover: %+v", s)
	}
	if err := b.Settle(held.ID, NanoUSD(1), after); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetRejectsNonsense(t *testing.T) {
	if _, err := NewBudget(BudgetConfig{Period: Window{}}); err == nil {
		t.Fatal("NewBudget accepted an invalid period")
	}
	b := newTestBudget(t, NanoUSD(10))
	if _, err := b.Reserve(subject, -1, base); err == nil {
		t.Fatal("Reserve accepted a negative amount")
	}
	r, _ := b.Reserve(subject, NanoUSD(1), base)
	if err := b.Settle(r.ID, -1, base); err == nil {
		t.Fatal("Settle accepted a negative amount")
	}
	if err := b.Release(999, base); !errors.Is(err, ErrNoReservation) {
		t.Fatalf("Release of an unknown id = %v", err)
	}
}

// TestSettlementAboveTheEstimateIsRecorded: the estimate is an upper bound, so
// this should not happen — but if it does, the money was spent and the ledger
// must say so rather than quietly losing it.
func TestSettlementAboveTheEstimateIsRecorded(t *testing.T) {
	b := newTestBudget(t, NanoUSD(10))
	r, err := b.Reserve(subject, NanoUSD(2), base)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(r.ID, NanoUSD(3), base); err != nil {
		t.Fatal(err)
	}
	if s := b.Snapshot(subject, base); s.Spent != NanoUSD(3) {
		t.Fatalf("spent = %v USD, want the amount actually spent", USD(s.Spent))
	}
}
