package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// ReserveRequest asks to hold budget before spending it.
//
// The limit is passed in rather than read from the row because the hot path
// already holds the authorization snapshot the limit comes from (DESIGN 9.1),
// and re-reading it here would put a second query on the gate.
type ReserveRequest struct {
	Subject     Subject
	Period      string
	PeriodStart time.Time

	// AmountNano is the upper-bound estimate: exact input tokens priced, plus
	// output priced at max_tokens (DESIGN 6.4).
	AmountNano int64

	// LimitNano is the budget ceiling. nil means unlimited.
	LimitNano *int64

	// Until is when this hold expires if nobody settles it. Required:
	// a reservation with no expiry is the bug R1-5 is about.
	Until time.Time
}

// Reservation is a hold that must later be settled or released.
type Reservation struct {
	Subject     Subject
	Period      string
	PeriodStart time.Time
	AmountNano  int64
	Until       time.Time
}

// BudgetState is one budget_state row.
type BudgetState struct {
	Subject       Subject
	Period        string
	PeriodStart   time.Time
	SpentNano     int64
	ReservedNano  int64
	ReservedUntil time.Time
	UpdatedAt     time.Time
}

// Available reports how much of limit is still reservable.
func (b BudgetState) Available(limitNano int64) int64 {
	return limitNano - b.SpentNano - b.ReservedNano
}

const budgetKeyPredicate = `subject_kind = ? AND subject_id = ? AND period = ? AND period_start = ?`

// ReserveBudget holds AmountNano against a budget, atomically, so concurrent
// requests cannot overshoot (DESIGN 6.4).
//
// Returns ErrBudgetExceeded when spent + reserved + amount would pass the
// limit. That is the correct outcome, not a fallback condition: DESIGN 7.6
// gives budget_exceeded an empty fallback list on purpose.
func (s *Store) ReserveBudget(ctx context.Context, req ReserveRequest) (Reservation, error) {
	if req.Subject.Kind == "" {
		return Reservation{}, errors.New("store: reservation needs a subject kind")
	}
	if req.AmountNano < 0 {
		return Reservation{}, fmt.Errorf("store: negative reservation %d", req.AmountNano)
	}
	if err := checkAmount(req.AmountNano, "reservation"); err != nil {
		return Reservation{}, err
	}
	if req.Until.IsZero() {
		// Refusing here is the whole point of R1-5. A hold with no expiry is
		// indistinguishable from a leak the moment the process dies.
		return Reservation{}, errors.New("store: reservation requires a non-zero Until")
	}
	if req.Period == "" {
		req.Period = "none"
	}
	if req.LimitNano != nil && req.AmountNano > *req.LimitNano {
		return Reservation{}, fmt.Errorf("%w: %d > limit %d", ErrBudgetExceeded, req.AmountNano, *req.LimitNano)
	}

	now := Micros(s.now())
	until := Micros(req.Until)
	args := []any{
		string(req.Subject.Kind), req.Subject.ID, req.Period, Micros(req.PeriodStart),
		req.AmountNano, until, now,
		// DO UPDATE
		req.AmountNano, until, until, now,
	}

	q := `
		INSERT INTO budget_state
		    (subject_kind, subject_id, period, period_start, spent_nano, reserved_nano, reserved_until, updated_at)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?)
		ON CONFLICT (subject_kind, subject_id, period, period_start) DO UPDATE SET
		    reserved_nano  = budget_state.reserved_nano + ?,
		    reserved_until = CASE
		                       WHEN budget_state.reserved_until IS NULL THEN ?
		                       WHEN budget_state.reserved_until < ? THEN excluded.reserved_until
		                       ELSE budget_state.reserved_until
		                     END,
		    updated_at     = ?`
	if req.LimitNano != nil {
		q += `
		WHERE budget_state.spent_nano + budget_state.reserved_nano + ? <= ?`
		args = append(args, req.AmountNano, *req.LimitNano)
	}

	res, err := s.exec(ctx, q, args...)
	if err != nil {
		return Reservation{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Reservation{}, err
	}
	if n == 0 {
		return Reservation{}, ErrBudgetExceeded
	}
	return Reservation{
		Subject:     req.Subject,
		Period:      req.Period,
		PeriodStart: req.PeriodStart,
		AmountNano:  req.AmountNano,
		Until:       req.Until,
	}, nil
}

// SettleBudget converts a hold into spend. actualNano is what the request
// actually cost; the difference between it and the reservation is released.
func (s *Store) SettleBudget(ctx context.Context, r Reservation, actualNano int64) error {
	if err := checkAmount(actualNano, "settlement"); err != nil {
		return err
	}
	return s.adjustReservation(ctx, r, actualNano)
}

// ReleaseReservation refunds a hold in full.
//
// DESIGN 6.4 / R1-6: a request may reserve at the gate and then be rejected
// while waiting for capacity, never reaching an upstream. Anything that fails
// before dispatch releases the whole amount, so the hold is soft until capacity
// is acquired and hard only afterwards.
func (s *Store) ReleaseReservation(ctx context.Context, r Reservation) error {
	return s.adjustReservation(ctx, r, 0)
}

func (s *Store) adjustReservation(ctx context.Context, r Reservation, spendNano int64) error {
	// reserved_nano is floored at zero: a sweep may already have cleared this
	// hold, and settling afterwards must add the spend without driving the
	// reservation negative.
	const q = `
		UPDATE budget_state
		   SET reserved_nano  = CASE WHEN reserved_nano > ? THEN reserved_nano - ? ELSE 0 END,
		       spent_nano     = spent_nano + ?,
		       reserved_until = CASE WHEN reserved_nano > ? THEN reserved_until ELSE NULL END,
		       updated_at     = ?
		 WHERE ` + budgetKeyPredicate
	res, err := s.exec(ctx, q,
		r.AmountNano, r.AmountNano, spendNano, r.AmountNano, Micros(s.now()),
		string(r.Subject.Kind), r.Subject.ID, r.Period, Micros(r.PeriodStart))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SweepExpiredReservations releases holds whose reserved_until has passed and
// reports how many rows it cleared.
//
// Without this, a process killed between reserve and settle locks that amount
// forever, and reserved-but-never-settled amounts eventually exhaust a budget
// that nothing actually spent (DESIGN 6.4, R1-5). It is the same safety net
// capacity has in DESIGN 5.3, and the leader runs it on the same schedule.
//
// budget_state carries one reserved_until for the whole row, which is the
// design's shape. Concurrent holds therefore share the latest expiry: the
// sweep fires only once every outstanding hold on that row has expired. That
// direction is the safe one -- it can be late, never early, so a live
// reservation is never released out from under a request in flight.
func (s *Store) SweepExpiredReservations(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.exec(ctx, `
		UPDATE budget_state
		   SET reserved_nano = 0, reserved_until = NULL, updated_at = ?
		 WHERE reserved_nano > 0 AND reserved_until IS NOT NULL AND reserved_until <= ?`,
		Micros(now), Micros(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetBudgetState reads one budget row.
func (s *Store) GetBudgetState(ctx context.Context, sub Subject, period string, periodStart time.Time) (BudgetState, error) {
	if period == "" {
		period = "none"
	}
	var (
		b     BudgetState
		start int64
		until sql.NullInt64
		upd   int64
	)
	err := s.queryRow(ctx, `
		SELECT subject_kind, subject_id, period, period_start, spent_nano, reserved_nano, reserved_until, updated_at
		  FROM budget_state WHERE `+budgetKeyPredicate,
		string(sub.Kind), sub.ID, period, Micros(periodStart)).
		Scan((*string)(&b.Subject.Kind), &b.Subject.ID, &b.Period, &start,
			&b.SpentNano, &b.ReservedNano, &until, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return BudgetState{}, ErrNotFound
	}
	if err != nil {
		return BudgetState{}, err
	}
	b.PeriodStart = TimeAt(start)
	b.ReservedUntil = TimeAt(nullInt(until))
	b.UpdatedAt = TimeAt(upd)
	return b, nil
}
