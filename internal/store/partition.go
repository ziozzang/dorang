package store

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// RetentionPolicy is how long each class of row is kept. A zero duration means
// "keep forever" for that class.
//
// The classes are separate because DESIGN 9.1 makes them separate: the ledger
// and the rollups have different retention, and the excerpt store has a
// shorter one still, since at enterprise volume excerpts at 100% sampling are
// larger than the ledger they annotate (DESIGN 9.2).
type RetentionPolicy struct {
	RequestLogs   time.Duration
	RequestTraces time.Duration
	Responses     time.Duration
	Rollups       time.Duration
	AuditLogs     time.Duration

	// DeleteBatch bounds one DELETE on an unpartitioned dialect, so retention
	// never takes a long write lock on a notebook. Zero means 5000.
	DeleteBatch int
}

// RetentionMechanism names how retention was actually enforced. It is reported
// rather than assumed because the two mechanisms have very different costs and
// a caller scheduling maintenance deserves to know which one it is paying for.
type RetentionMechanism string

// Retention mechanisms.
const (
	// MechanismDropPartition drops whole day partitions. O(1) per day.
	MechanismDropPartition RetentionMechanism = "drop-partition"
	// MechanismDelete deletes rows in bounded batches. O(rows).
	MechanismDelete RetentionMechanism = "delete"
)

// MaintenanceReport is what one Maintain call did.
type MaintenanceReport struct {
	Dialect      Dialect
	Partitioning Partitioning
	Mechanism    RetentionMechanism

	// PartitionsCreated and PartitionsDropped are empty on an unpartitioned
	// dialect.
	PartitionsCreated []string
	PartitionsDropped []string

	// RowsDeleted is per table, and empty on a partitioned dialect except for
	// the tables that are not partitioned (responses_store, rollups, audit).
	RowsDeleted map[string]int64
}

// EnsurePartitions pre-creates ledger partitions for today and the next
// `ahead` days (plus yesterday, for late-arriving spooled rows), and returns
// the partitions it created.
//
// DESIGN 9.5 requires at least two days of headroom, and requires this to ship
// with the writer rather than with clustering: the failure it prevents is every
// insert failing at the first midnight after metering ships. `ahead` below 2 is
// raised to 2.
//
// Returns ErrNoPartitioning on a dialect without partitions. That is a
// deliberate error rather than a silent success: a caller who needs to know
// which retention mechanism applies must not be able to mistake one for the
// other. Callers that simply want maintenance done should call Maintain.
func (s *Store) EnsurePartitions(ctx context.Context, now time.Time, ahead int) ([]string, error) {
	if s.Partitioning() == PartitioningNone {
		return nil, ErrNoPartitioning
	}
	if ahead < 2 {
		ahead = 2
	}
	start := dayFloor(now).AddDate(0, 0, -1)
	days := make([]time.Time, 0, ahead+2)
	for i := 0; i <= ahead+1; i++ {
		days = append(days, start.AddDate(0, 0, i))
	}
	return s.d.ensureDays(ctx, s, days)
}

// EnsureDays creates the partitions covering the given instants' UTC days.
// It is what the writer calls when an insert lands on a day nobody
// pre-created. On an unpartitioned dialect it succeeds with nothing to do,
// because there is nothing a writer needs it to have done.
func (s *Store) EnsureDays(ctx context.Context, at []time.Time) ([]string, error) {
	if s.Partitioning() == PartitioningNone {
		return nil, nil
	}
	return s.d.ensureDays(ctx, s, uniqueDays(at))
}

// Maintain runs one storage maintenance pass: pre-create partitions where the
// dialect has them, then enforce retention by the mechanism the dialect
// supports. In a cluster this is leader-only work (DESIGN 9.5); clustering
// restricts the job, it does not introduce it.
func (s *Store) Maintain(ctx context.Context, now time.Time, ret RetentionPolicy) (MaintenanceReport, error) {
	rep := MaintenanceReport{
		Dialect:      s.cfg.Driver,
		Partitioning: s.Partitioning(),
		RowsDeleted:  map[string]int64{},
	}
	batch := ret.DeleteBatch
	if batch <= 0 {
		batch = 5000
	}

	if rep.Partitioning == PartitioningDaily {
		rep.Mechanism = MechanismDropPartition
		created, err := s.EnsurePartitions(ctx, now, s.cfg.PartitionAhead)
		if err != nil {
			return rep, err
		}
		rep.PartitionsCreated = created

		// request_log_tags is dropped with request_logs: it is keyed by the
		// same ts and pruning them apart would leave tags pointing at rows
		// that no longer exist.
		if ret.RequestLogs > 0 {
			cutoff := now.Add(-ret.RequestLogs)
			for _, tbl := range []string{"request_logs", "request_log_tags"} {
				dropped, err := s.d.dropPartitionsBefore(ctx, s, tbl, cutoff)
				rep.PartitionsDropped = append(rep.PartitionsDropped, dropped...)
				if err != nil {
					return rep, err
				}
			}
		}
		if ret.RequestTraces > 0 {
			dropped, err := s.d.dropPartitionsBefore(ctx, s, "request_traces", now.Add(-ret.RequestTraces))
			rep.PartitionsDropped = append(rep.PartitionsDropped, dropped...)
			if err != nil {
				return rep, err
			}
		}
	} else {
		rep.Mechanism = MechanismDelete
		if ret.RequestLogs > 0 {
			cutoff := now.Add(-ret.RequestLogs)
			n, err := s.deleteLedgerBefore(ctx, "request_log_tags", "request_id", cutoff, batch)
			rep.RowsDeleted["request_log_tags"] = n
			if err != nil {
				return rep, err
			}
			n, err = s.deleteLedgerBefore(ctx, "request_logs", "id", cutoff, batch)
			rep.RowsDeleted["request_logs"] = n
			if err != nil {
				return rep, err
			}
		}
		if ret.RequestTraces > 0 {
			n, err := s.deleteLedgerBefore(ctx, "request_traces", "request_id", now.Add(-ret.RequestTraces), batch)
			rep.RowsDeleted["request_traces"] = n
			if err != nil {
				return rep, err
			}
		}
	}

	// These are unpartitioned on both dialects, so they are always deleted.
	if ret.Responses > 0 {
		n, err := s.deleteBatched(ctx,
			`DELETE FROM responses_store WHERE response_id IN (
			   SELECT response_id FROM responses_store WHERE expires_at < ? ORDER BY expires_at LIMIT ?)`,
			Micros(now.Add(-ret.Responses)), batch)
		rep.RowsDeleted["responses_store"] = n
		if err != nil {
			return rep, err
		}
	}
	if ret.Rollups > 0 {
		cutoff := Micros(now.Add(-ret.Rollups))
		for _, tbl := range []string{"usage_by_key_hour", "usage_by_model_hour", "usage_by_team_day"} {
			res, err := s.exec(ctx, "DELETE FROM "+tbl+" WHERE bucket_start < ?", cutoff)
			if err != nil {
				return rep, err
			}
			n, _ := res.RowsAffected()
			rep.RowsDeleted[tbl] = n
		}
	}
	if ret.AuditLogs > 0 {
		n, err := s.deleteBatched(ctx,
			`DELETE FROM audit_logs WHERE id IN (
			   SELECT id FROM audit_logs WHERE ts < ? ORDER BY ts LIMIT ?)`,
			Micros(now.Add(-ret.AuditLogs)), batch)
		rep.RowsDeleted["audit_logs"] = n
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// deleteLedgerBefore removes ledger rows older than cutoff in bounded batches.
// The row-value IN (SELECT ...) form is used rather than DELETE ... LIMIT,
// which SQLite only offers in a non-default build.
func (s *Store) deleteLedgerBefore(ctx context.Context, table, idCol string, cutoff time.Time, batch int) (int64, error) {
	q := fmt.Sprintf(
		`DELETE FROM %s WHERE (ts, %s) IN (SELECT ts, %s FROM %s WHERE ts < ? ORDER BY ts LIMIT ?)`,
		table, idCol, idCol, table)
	return s.deleteBatched(ctx, q, Micros(cutoff), batch)
}

func (s *Store) deleteBatched(ctx context.Context, q string, cutoff int64, batch int) (int64, error) {
	var total int64
	for {
		res, err := s.exec(ctx, q, cutoff, batch)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(batch) {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// ListPartitions returns the current partition names of a ledger table,
// oldest first. Returns ErrNoPartitioning on an unpartitioned dialect.
func (s *Store) ListPartitions(ctx context.Context, table string) ([]string, error) {
	if s.Partitioning() == PartitioningNone {
		return nil, ErrNoPartitioning
	}
	if !slices.Contains(partitionedTables, table) {
		return nil, fmt.Errorf("%w: %q is not a partitioned ledger table", ErrBadIdentifier, table)
	}
	rows, err := s.query(ctx,
		`SELECT c.relname FROM pg_inherits i
		   JOIN pg_class c ON c.oid = i.inhrelid
		  WHERE i.inhparent = to_regclass(?)
		  ORDER BY c.relname`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
