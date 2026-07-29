package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SubscriptionState is one `fixed_subscription` rule's period accumulator, as
// it is written down between processes.
//
// The value type is deliberately dumb: an id, an instant and an opaque exact
// amount. What the amount MEANS — how much of the open period's plan cost has
// already been attributed to ledger rows — belongs to internal/pricing, and
// this package does not price anything. Attributed is atto-scaled decimal
// digits rather than a number because a 100 USD plan is 10^20 atto, which no
// 64-bit integer holds; see the migration for why it is not a float.
type SubscriptionState struct {
	RuleID      string
	PeriodStart time.Time
	Attributed  string
	UpdatedAt   time.Time
}

// LoadSubscriptionState reads every accumulator, in rule id order.
//
// The table has one row per subscription rule and a deployment has a handful,
// so it is read whole: there is no query here that a filter would make cheaper,
// and the caller — a process starting up, or its periodic checkpoint — wants
// all of them.
func (s *Store) LoadSubscriptionState(ctx context.Context) ([]SubscriptionState, error) {
	rows, err := s.query(ctx, `
		SELECT rule_id, period_start, attributed_atto, updated_at
		  FROM subscription_state ORDER BY rule_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SubscriptionState
	for rows.Next() {
		var (
			st         SubscriptionState
			start, upd int64
			attributed string
		)
		if err := rows.Scan(&st.RuleID, &start, &attributed, &upd); err != nil {
			return nil, err
		}
		st.PeriodStart = TimeAt(start)
		st.Attributed = attributed
		st.UpdatedAt = TimeAt(upd)
		out = append(out, st)
	}
	return out, rows.Err()
}

// SaveSubscriptionState merges accumulators into the table, monotonically.
//
// Monotone, and merged in Go rather than in SQL, because the ordering is on a
// pair: a LATER period replaces the row outright, the SAME period keeps the
// larger attributed total, and an EARLIER period is dropped. That is three
// cases over two columns, one of which is an arbitrary-precision decimal held
// as text — expressible as a CASE expression in neither dialect without
// arithmetic this schema deliberately does not have.
//
// It runs in one transaction so the read and the write cannot straddle another
// node's. Two nodes checkpointing at once therefore converge on the high-water
// mark; the loser of a race is corrected on its own next tick, and neither can
// pull the shared figure backwards, which is the only direction that would
// re-attribute a slice of a period twice.
//
// It returns how many rows it actually changed, so a caller can log a
// checkpoint that did nothing as the no-op it is.
func (s *Store) SaveSubscriptionState(ctx context.Context, states []SubscriptionState, now time.Time) (int, error) {
	if len(states) == 0 {
		return 0, nil
	}
	written := 0
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		written = 0
		for _, st := range states {
			if st.RuleID == "" || st.PeriodStart.IsZero() {
				continue
			}
			var (
				curStart      int64
				curAttributed string
			)
			err := tx.QueryRowContext(ctx, s.rebind(
				`SELECT period_start, attributed_atto FROM subscription_state WHERE rule_id = ?`),
				st.RuleID).Scan(&curStart, &curAttributed)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				if _, err := s.txExec(ctx, tx,
					`INSERT INTO subscription_state (rule_id, period_start, attributed_atto, updated_at)
					 VALUES (?, ?, ?, ?)`,
					st.RuleID, Micros(st.PeriodStart), attributedText(st.Attributed), Micros(now)); err != nil {
					return err
				}
				written++
				continue
			case err != nil:
				return err
			}
			incoming := Micros(st.PeriodStart)
			switch {
			case incoming < curStart:
				// A node still settling a period this table has moved past. Its
				// figure bounds nothing that is still open.
				continue
			case incoming == curStart &&
				compareAttributed(attributedText(st.Attributed), curAttributed) <= 0:
				continue
			}
			if _, err := s.txExec(ctx, tx,
				`UPDATE subscription_state
				    SET period_start = ?, attributed_atto = ?, updated_at = ?
				  WHERE rule_id = ?`,
				incoming, attributedText(st.Attributed), Micros(now), st.RuleID); err != nil {
				return err
			}
			written++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// attributedText normalizes the stored form: no sign, no separators, no empty
// string. An accumulator is a magnitude and only ever rises.
func attributedText(s string) string {
	s = trimLeadingZeros(s)
	if s == "" {
		return "0"
	}
	return s
}

func trimLeadingZeros(s string) string {
	i := 0
	for i < len(s)-1 && s[i] == '0' {
		i++
	}
	return s[i:]
}

// compareAttributed orders two decimal magnitudes without parsing them into a
// number this package has no type for. Longer is larger once the leading zeros
// are gone; equal lengths compare byte for byte, which for digits is the same
// as comparing values.
func compareAttributed(a, b string) int {
	a, b = trimLeadingZeros(a), trimLeadingZeros(b)
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
