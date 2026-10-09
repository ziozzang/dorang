package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// alert_state holds internal/alerts' conditions. Every write that decides
// whether to announce is a conditional statement on one row, so the row is the
// claim: of any number of nodes and restarts that observe one crossing, exactly
// one wins the right to announce it.

// AlertRaise records that key is low and claims its announcement when that is
// news: the condition is new, it had recovered, or it was last announced at or
// before remindBefore. announce is false when another observation already
// claimed it. pending is the transports the announcement is still owed to,
// comma-separated; payload is what a retry re-renders it from.
func (s *Store) AlertRaise(ctx context.Context, key, event string, now, remindBefore time.Time,
	pending, payload string) (announce bool, seq int64, err error) {

	at := Micros(now)
	res, err := s.exec(ctx, `
		INSERT INTO alert_state (alert_key, event, low, since, last_sent, seq, pending, payload, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)
		ON CONFLICT (alert_key) DO NOTHING`,
		key, event, true, at, at, pending, payload, at)
	if err != nil {
		return false, 0, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, 1, nil
	}
	err = s.queryRow(ctx, `
		UPDATE alert_state
		   SET since = CASE WHEN low = ? THEN since ELSE ? END,
		       low = ?, last_sent = ?, seq = seq + 1, pending = ?, payload = ?, updated_at = ?
		 WHERE alert_key = ? AND (low = ? OR last_sent IS NULL OR last_sent <= ?)
		RETURNING seq`,
		true, at, true, at, pending, payload, at,
		key, false, Micros(remindBefore)).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return true, seq, nil
}

// AlertClear records that key is no longer low and reports whether it was.
func (s *Store) AlertClear(ctx context.Context, key string, now time.Time) (bool, error) {
	res, err := s.exec(ctx, `
		UPDATE alert_state SET low = ?, pending = '', updated_at = ?
		 WHERE alert_key = ? AND low = ?`,
		false, Micros(now), key, true)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// AlertDelivered removes driver from the transports announcement seq of key
// is still owed to. An announcement that has since been superseded is left
// alone.
func (s *Store) AlertDelivered(ctx context.Context, key string, seq int64, driver string, now time.Time) error {
	for attempt := 0; attempt < 4; attempt++ {
		var pending string
		err := s.queryRow(ctx, `SELECT pending FROM alert_state WHERE alert_key = ? AND seq = ?`,
			key, seq).Scan(&pending)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		next := removeItem(pending, driver)
		if next == pending {
			return nil
		}
		res, err := s.exec(ctx, `
			UPDATE alert_state SET pending = ?, updated_at = ?
			 WHERE alert_key = ? AND seq = ? AND pending = ?`,
			next, Micros(now), key, seq, pending)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return nil
		}
	}
	return errors.New("store: alert delivery record kept changing under contention")
}

// AlertPending is an announcement some transport has not delivered.
type AlertPending struct {
	Key      string
	Event    string
	Seq      int64
	Pending  []string
	Payload  string
	LastSent time.Time
}

// AlertsPending lists low conditions whose announcement is still owed to a
// transport and was last attempted at or before before.
func (s *Store) AlertsPending(ctx context.Context, before time.Time, limit int) ([]AlertPending, error) {
	rows, err := s.query(ctx, `
		SELECT alert_key, event, seq, pending, payload, last_sent
		  FROM alert_state
		 WHERE pending <> '' AND low = ? AND last_sent <= ?
		 ORDER BY last_sent
		 LIMIT ?`, true, Micros(before), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertPending
	for rows.Next() {
		var (
			p       AlertPending
			pending string
			last    int64
		)
		if err := rows.Scan(&p.Key, &p.Event, &p.Seq, &pending, &p.Payload, &last); err != nil {
			return nil, err
		}
		p.Pending = strings.Split(pending, ",")
		p.LastSent = TimeAt(last)
		out = append(out, p)
	}
	return out, rows.Err()
}

// AlertRetryClaim claims the retry of announcement seq of key, which was last
// attempted at last, so that one node retries it, and returns the transports
// still owed it as of the claim — a delivery recorded since the listing is not
// sent again. ok is false when nothing is owed any more or another node won.
func (s *Store) AlertRetryClaim(ctx context.Context, key string, seq int64, last, now time.Time) ([]string, bool, error) {
	var pending string
	err := s.queryRow(ctx, `
		UPDATE alert_state SET last_sent = ?, updated_at = ?
		 WHERE alert_key = ? AND seq = ? AND last_sent = ? AND pending <> '' AND low = ?
		RETURNING pending`,
		Micros(now), Micros(now), key, seq, Micros(last), true).Scan(&pending)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return strings.Split(pending, ","), true, nil
}

// AlertActive is a condition the store holds as low.
type AlertActive struct {
	Key      string
	LastSent time.Time
}

// AlertsActive lists the conditions that are low, so that a process that has
// just started knows them: a recovery it observes must clear them.
func (s *Store) AlertsActive(ctx context.Context, limit int) ([]AlertActive, error) {
	rows, err := s.query(ctx, `
		SELECT alert_key, last_sent FROM alert_state WHERE low = ? ORDER BY alert_key LIMIT ?`,
		true, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertActive
	for rows.Next() {
		var (
			a    AlertActive
			last sql.NullInt64
		)
		if err := rows.Scan(&a.Key, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			a.LastSent = TimeAt(last.Int64)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func removeItem(list, item string) string {
	var keep []string
	for _, v := range strings.Split(list, ",") {
		if v != "" && v != item {
			keep = append(keep, v)
		}
	}
	return strings.Join(keep, ",")
}
