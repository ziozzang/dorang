package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SubjectKind names what a budget attaches to. DESIGN 6.4: budgets attach to a
// credential, key, user, team, or globally.
type SubjectKind string

// Budget subjects.
const (
	SubjectKey        SubjectKind = "key"
	SubjectUser       SubjectKind = "user"
	SubjectTeam       SubjectKind = "team"
	SubjectCredential SubjectKind = "credential"
	SubjectGlobal     SubjectKind = "global"
)

// Subject identifies one budgeted entity.
type Subject struct {
	Kind SubjectKind
	ID   string
}

// BudgetState is one budget_state row: the durable spend counter for a
// subject's period.
//
// # Why there is no reserved_nano beside spent_nano
//
// There was, and the pair was a second reservation mechanism (ReserveBudget /
// SettleBudget / ReleaseReservation / SweepExpiredReservations) that this
// package shipped, tested, and never had a caller for. The one that runs is
// [github.com/ziozzang/dorang/internal/cluster.Ledger]: DESIGN 9.6 makes budget
// the one write that cannot be deferred and then makes it cheap by holding
// units in a per-node lease block guarded by an atomic, so the store sees a
// write per BLOCK rather than a write per request. Reserving here would have
// been the write-per-request arrangement 9.6 exists to avoid.
//
// The safety net the reserved_until column carried — a process killed between
// reserve and settle must not lock that amount forever — is carried by the
// block lease instead. A block is charged to spent_nano in full when it is
// drawn, its `quota_leases` row has an expires_at, and the leader's
// lease-reclaim pass returns the unspent part of every lease whose TTL has
// passed. That is the same pattern with the same failure mode, and it covers
// the crash the reservation columns covered, plus the one they did not: a node
// that dies holding a block it had already been charged for.
//
// Migration 0006 dropped both columns. See DESIGN 6.4 and 9.6.
type BudgetState struct {
	Subject     Subject
	Period      string
	PeriodStart time.Time
	SpentNano   int64
	UpdatedAt   time.Time
}

// Available reports how much of limit is still spendable.
func (b BudgetState) Available(limitNano int64) int64 {
	return limitNano - b.SpentNano
}

const budgetKeyPredicate = `subject_kind = ? AND subject_id = ? AND period = ? AND period_start = ?`

// GetBudgetState reads one budget row.
//
// The counter it reads runs up to one lease block AHEAD of what has actually
// been spent, by design: a node charges the whole block before it hands out a
// unit of it, which is what makes a crash under-spend rather than overspend
// (DESIGN 9.6). A report that must agree with the ledger reads the rollups,
// not this.
func (s *Store) GetBudgetState(ctx context.Context, sub Subject, period string, periodStart time.Time) (BudgetState, error) {
	if period == "" {
		period = "none"
	}
	var (
		b     BudgetState
		start int64
		upd   int64
	)
	err := s.queryRow(ctx, `
		SELECT subject_kind, subject_id, period, period_start, spent_nano, updated_at
		  FROM budget_state WHERE `+budgetKeyPredicate,
		string(sub.Kind), sub.ID, period, Micros(periodStart)).
		Scan((*string)(&b.Subject.Kind), &b.Subject.ID, &b.Period, &start,
			&b.SpentNano, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return BudgetState{}, ErrNotFound
	}
	if err != nil {
		return BudgetState{}, err
	}
	b.PeriodStart = TimeAt(start)
	b.UpdatedAt = TimeAt(upd)
	return b, nil
}
