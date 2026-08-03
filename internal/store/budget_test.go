package store

import (
	"context"
	"database/sql"
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

// TestTheReservationColumnsSurviveInertForOneRelease is the schema half of the
// same deletion, and it says the opposite of what it used to.
//
// It used to assert that migration 0006 had DROPPED both columns, because a
// column nothing writes is the same defect as a function nothing calls. That is
// still true of the code, and the code is gone — `ReserveBudget` and its three
// siblings, the leader's sweep, the `spent_nano + reserved_nano` read. What was
// wrong was dropping the STORAGE in the same release.
//
// OPERATIONS.md §9 has an operator apply migrations from one node before rolling
// the fleet, so the previous release keeps serving against the new schema for the
// whole of the roll — and the previous release names both columns, in a SELECT
// and in an INSERT. Reproduced: both fail with `no such column: reserved_nano`,
// and the budget path then fails closed with `503 budget_unavailable` on every
// budgeted request. So the drop is deferred one release, and until then the
// invariant is this pair:
//
//   - the columns are STILL THERE, so the old binary survives the roll; and
//   - nothing in this release names them, which is
//     [TestTheIntermediateReleaseNeverNamesTheRetiredColumns], a source-level
//     assertion because "no statement runs" is not a thing a query can observe.
//
// When the drop finally ships, this test is what has to be inverted with it —
// deliberately, and one release after the code stopped reading them.
func TestTheReservationColumnsSurviveInertForOneRelease(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		// Written the way THIS release writes a budget row: it names neither
		// column, so both must take their declared defaults.
		mustExec(t, s, `INSERT INTO budget_state
		    (subject_kind, subject_id, period, period_start, spent_nano, updated_at)
		  VALUES ('key', 'k', 'monthly', 0, 1, 0)`)

		// The previous release's own read, verbatim. It is the statement that
		// broke, and running it is the only assertion that settles whether the
		// old binary survives this schema.
		var counter int64
		if err := s.queryRow(context.Background(),
			`SELECT spent_nano + reserved_nano FROM budget_state`).Scan(&counter); err != nil {
			t.Fatalf("the PREVIOUS release can no longer read its budget counter: %v\n\n"+
				"Migrations are applied before the fleet is rolled (OPERATIONS.md §9), so "+
				"every node still on the old binary runs this on every budgeted request "+
				"until it is replaced. It fails CLOSED: 503 budget_unavailable.", err)
		}
		if counter != 1 {
			t.Fatalf("the previous release reads its counter as %d, want 1: the retired "+
				"columns must be INERT, not merely present — a non-zero reserved_nano "+
				"would make the old binary refuse budget this release has not spent",
				counter)
		}

		var reservedUntil sql.NullInt64
		if err := s.queryRow(context.Background(),
			`SELECT reserved_until FROM budget_state`).Scan(&reservedUntil); err != nil {
			t.Fatalf("reserved_until: %v", err)
		}
		if reservedUntil.Valid {
			t.Fatalf("reserved_until = %d, want NULL: nothing in this release writes it",
				reservedUntil.Int64)
		}
	})
}
