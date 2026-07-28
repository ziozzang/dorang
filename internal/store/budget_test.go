package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

var budgetEpoch = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

func budgetStore(t *testing.T, b backend, c *clock) *Store {
	t.Helper()
	return openStore(t, b, b.env(t), func(cfg *Config) { cfg.Now = c.Now })
}

func TestBudgetReserveSettleAndRelease(t *testing.T) {
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			clk := newClock(budgetEpoch)
			s := budgetStore(t, b, clk)

			sub := Subject{Kind: SubjectKey, ID: "k1"}
			limit := int64(10_000_000_000) // 10 units
			req := ReserveRequest{
				Subject:     sub,
				Period:      "monthly",
				PeriodStart: budgetEpoch.Truncate(24 * time.Hour),
				AmountNano:  4_000_000_000,
				LimitNano:   &limit,
				Until:       clk.Now().Add(time.Minute),
			}

			r1, err := s.ReserveBudget(ctx, req)
			if err != nil {
				t.Fatalf("first reserve: %v", err)
			}
			r2, err := s.ReserveBudget(ctx, req)
			if err != nil {
				t.Fatalf("second reserve: %v", err)
			}

			// Two 4-unit holds against a 10-unit limit; a third must fail
			// rather than overshoot.
			if _, err := s.ReserveBudget(ctx, req); !errors.Is(err, ErrBudgetExceeded) {
				t.Fatalf("third reserve: %v, want ErrBudgetExceeded", err)
			}

			st, err := s.GetBudgetState(ctx, sub, req.Period, req.PeriodStart)
			if err != nil {
				t.Fatal(err)
			}
			if st.ReservedNano != 8_000_000_000 || st.SpentNano != 0 {
				t.Fatalf("state = %+v, want reserved 8e9 spent 0", st)
			}

			// Settle one for less than it reserved; the difference comes back.
			if err := s.SettleBudget(ctx, r1, 1_500_000_000); err != nil {
				t.Fatal(err)
			}
			// Release the other in full: rejected before dispatch (R1-6).
			if err := s.ReleaseReservation(ctx, r2); err != nil {
				t.Fatal(err)
			}

			st, err = s.GetBudgetState(ctx, sub, req.Period, req.PeriodStart)
			if err != nil {
				t.Fatal(err)
			}
			if st.ReservedNano != 0 {
				t.Fatalf("reserved = %d after settle and release, want 0", st.ReservedNano)
			}
			if st.SpentNano != 1_500_000_000 {
				t.Fatalf("spent = %d, want 1.5e9", st.SpentNano)
			}
			if !st.ReservedUntil.IsZero() {
				t.Fatalf("reserved_until = %s with nothing reserved", st.ReservedUntil)
			}

			// The released capacity is reservable again.
			if _, err := s.ReserveBudget(ctx, req); err != nil {
				t.Fatalf("reserve after release: %v", err)
			}
		})
	}
}

// TestSweepExpiredReservations is R1-5 stated as a test: a process killed
// between reserve and settle must not lock that amount forever.
func TestSweepExpiredReservations(t *testing.T) {
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			clk := newClock(budgetEpoch)
			s := budgetStore(t, b, clk)

			sub := Subject{Kind: SubjectKey, ID: "k1"}
			limit := int64(10_000_000_000)
			periodStart := budgetEpoch.Truncate(24 * time.Hour)
			req := ReserveRequest{
				Subject: sub, Period: "monthly", PeriodStart: periodStart,
				AmountNano: 9_000_000_000, LimitNano: &limit,
				Until: clk.Now().Add(30 * time.Second),
			}
			if _, err := s.ReserveBudget(ctx, req); err != nil {
				t.Fatal(err)
			}

			// The process dies here: nobody settles, nobody releases.
			small := req
			small.AmountNano = 2_000_000_000
			small.Until = clk.Now().Add(30 * time.Second)
			if _, err := s.ReserveBudget(ctx, small); !errors.Is(err, ErrBudgetExceeded) {
				t.Fatalf("budget was not actually held: %v", err)
			}

			// A sweep before the hold expires must not touch it. Releasing a
			// live reservation would be far worse than releasing a dead one late.
			clk.Add(10 * time.Second)
			if n, err := s.SweepExpiredReservations(ctx, clk.Now()); err != nil || n != 0 {
				t.Fatalf("early sweep released %d rows (err %v), want 0", n, err)
			}

			clk.Add(30 * time.Second)
			n, err := s.SweepExpiredReservations(ctx, clk.Now())
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("sweep released %d rows, want 1", n)
			}

			st, err := s.GetBudgetState(ctx, sub, "monthly", periodStart)
			if err != nil {
				t.Fatal(err)
			}
			if st.ReservedNano != 0 || !st.ReservedUntil.IsZero() {
				t.Fatalf("state after sweep = %+v, want the hold cleared", st)
			}

			// The budget nobody spent is spendable again.
			small.Until = clk.Now().Add(time.Minute)
			if _, err := s.ReserveBudget(ctx, small); err != nil {
				t.Fatalf("reserve after sweep: %v", err)
			}

			// Settling a swept reservation adds the spend without driving the
			// reservation negative.
			swept := Reservation{Subject: sub, Period: "monthly", PeriodStart: periodStart, AmountNano: 9_000_000_000}
			if err := s.SettleBudget(ctx, swept, 500_000_000); err != nil {
				t.Fatal(err)
			}
			st, err = s.GetBudgetState(ctx, sub, "monthly", periodStart)
			if err != nil {
				t.Fatal(err)
			}
			if st.ReservedNano < 0 {
				t.Fatalf("reserved went negative: %d", st.ReservedNano)
			}
			if st.SpentNano != 500_000_000 {
				t.Fatalf("spent = %d, want 5e8", st.SpentNano)
			}
		})
	}
}

func TestReservationMustExpire(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		_, err := s.ReserveBudget(context.Background(), ReserveRequest{
			Subject:    Subject{Kind: SubjectTeam, ID: "t1"},
			AmountNano: 1,
			// no Until
		})
		if err == nil {
			t.Fatal("a reservation with no expiry was accepted; R1-5 requires reserved_until")
		}
	})
}

func TestBudgetWithoutALimitNeverRefuses(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		req := ReserveRequest{
			Subject:    Subject{Kind: SubjectGlobal, ID: ""},
			Period:     "none",
			AmountNano: 1_000_000_000,
			Until:      s.now().Add(time.Minute),
		}
		for i := 0; i < 3; i++ {
			if _, err := s.ReserveBudget(ctx, req); err != nil {
				t.Fatalf("unlimited reserve %d: %v", i, err)
			}
		}
		st, err := s.GetBudgetState(ctx, req.Subject, "none", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if st.ReservedNano != 3_000_000_000 {
			t.Fatalf("reserved = %d, want 3e9", st.ReservedNano)
		}
	})
}

func TestBudgetAmountsAreRangeChecked(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		_, err := s.ReserveBudget(context.Background(), ReserveRequest{
			Subject:    Subject{Kind: SubjectKey, ID: "k"},
			AmountNano: MaxAmountNano + 1,
			Until:      s.now().Add(time.Minute),
		})
		if !errors.Is(err, ErrAmountRange) {
			t.Fatalf("got %v, want ErrAmountRange", err)
		}
	})
}
