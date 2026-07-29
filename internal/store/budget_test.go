package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

var budgetEpoch = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// TestBudgetStateIsSpendOnly is the deletion of the reservation mechanism,
// stated as a test.
//
// This file used to hold five of them — reserve, settle, release, the expiry
// sweep, the mandatory `Until` — over `ReserveBudget` and the `reserved_nano` /
// `reserved_until` columns. Every one passed and none of the code they covered
// had a caller: the budget path that runs is internal/cluster's lease block
// (DESIGN §9.6), which charges a whole block to `spent_nano` before handing out
// a unit of it and holds the units in memory behind an atomic.
//
// So what is asserted here is what the store's budget row IS now: a spend
// counter, with no second place for money to be held. The reservation half is
// covered where it actually lives, in internal/cluster:
// TestReleaseRefundsInFull for the refund, TestLeaseOutlivingItsNodeIsReclaimed
// and TestLeaderJobsReclaimBothKindsOfAbandonedHold for the expiry net that
// replaced SweepExpiredReservations.
func TestBudgetStateIsSpendOnly(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		sub := Subject{Kind: SubjectKey, ID: "k1"}
		periodStart := budgetEpoch.Truncate(24 * time.Hour)

		if _, err := s.GetBudgetState(ctx, sub, "monthly", periodStart); err != ErrNotFound {
			t.Fatalf("an unwritten budget reads %v, want ErrNotFound", err)
		}

		// The only writer of this row in the tree is Ledger.addCounter, and it
		// writes spent_nano. Nothing in this package holds money.
		mustExec(t, s, `INSERT INTO budget_state
		    (subject_kind, subject_id, period, period_start, spent_nano, updated_at)
		  VALUES (?, ?, ?, ?, ?, ?)`,
			string(sub.Kind), sub.ID, "monthly", Micros(periodStart),
			int64(1_500_000_000), Micros(s.now()))

		st, err := s.GetBudgetState(ctx, sub, "monthly", periodStart)
		if err != nil {
			t.Fatal(err)
		}
		if st.SpentNano != 1_500_000_000 {
			t.Fatalf("spent = %d, want 1.5e9", st.SpentNano)
		}
		if !st.PeriodStart.Equal(periodStart) {
			t.Fatalf("period start = %s, want %s", st.PeriodStart, periodStart)
		}
		// Available is spend against the ceiling and nothing else. When it
		// subtracted a reserved figure as well, the term was structurally
		// always zero.
		if got := st.Available(10_000_000_000); got != 8_500_000_000 {
			t.Fatalf("available = %d, want 8.5e9", got)
		}
	})
}

// TestBudgetStateHasNoReservationColumns is the schema half of the same
// deletion, and it is the assertion migration 0006 exists to make true.
//
// A column nothing writes is the same defect as a function nothing calls, in a
// medium with no compiler to notice it. This fails if either column comes back.
//
// The ErrNoRows case is the whole difficulty and is why it is spelled out. A
// SELECT of a column that does not exist fails to PREPARE, with the column
// named; a SELECT of a column that does exist against an empty table succeeds
// and returns no rows. Both are non-nil errors, so "err != nil" passes either
// way — which is what the first version of this test asserted, and reverting
// migration 0006 left it green. Distinguishing them is the test.
func TestBudgetStateHasNoReservationColumns(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		// A row, so that a surviving column answers with a value rather than
		// with ErrNoRows and the two cases cannot be confused at all.
		mustExec(t, s, `INSERT INTO budget_state
		    (subject_kind, subject_id, period, period_start, spent_nano, updated_at)
		  VALUES ('key', 'k', 'monthly', 0, 1, 0)`)

		for _, col := range []string{"reserved_nano", "reserved_until"} {
			var v any
			err := s.queryRow(context.Background(),
				`SELECT `+col+` FROM budget_state`).Scan(&v)
			if err == nil || errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("budget_state still has %s (err %v): the reservation mechanism "+
					"was deleted and its storage must go with it", col, err)
			}
			if !strings.Contains(strings.ToLower(err.Error()), col) {
				t.Fatalf("selecting %s failed for some reason other than the column "+
					"being absent: %v", col, err)
			}
		}
	})
}
