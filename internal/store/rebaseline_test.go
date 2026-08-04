package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The scenario that had no way out, end to end.
//
// A commit renamed and rewrote an already-applied migration — turning a column
// DROP into a no-op, because dropping columns the previous release still reads
// breaks a rolling upgrade. Correct change; and every database that had run the
// old file then refused to start, with an error that named two checksums and no
// action. The only path was hand-written SQL against `schema_migrations`.
//
// This drives the whole remedy: the refusal names the command, the report shows
// what changed, and the rebaseline lets the next migration run.
func TestADriftedMigrationHasAWayOut(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		if _, err := s.Migrate(ctx); err != nil {
			t.Fatalf("initial migrate: %v", err)
		}

		// Rewrite one applied row's checksum, which is what an edited file does.
		const forged = "0000000000000000000000000000000000000000000000000000000000000000"
		var version int64
		if err := s.queryRow(ctx,
			`SELECT version FROM schema_migrations ORDER BY version LIMIT 1`).Scan(&version); err != nil {
			t.Fatalf("read a version: %v", err)
		}
		if _, err := s.exec(ctx,
			`UPDATE schema_migrations SET checksum = ? WHERE version = ?`, forged, version); err != nil {
			t.Fatalf("forge drift: %v", err)
		}

		// 1. Migrate refuses, and the refusal names the remedy.
		_, err := s.Migrate(ctx)
		if !errors.Is(err, ErrDirtySchema) {
			t.Fatalf("Migrate = %v, want ErrDirtySchema", err)
		}
		if !strings.Contains(err.Error(), "--rebaseline") || !strings.Contains(err.Error(), "--drift") {
			t.Errorf("the refusal states a fact and no action:\n  %v", err)
		}
		if !strings.Contains(err.Error(), forged) {
			t.Errorf("the refusal does not carry the recorded checksum the remedy needs:\n  %v", err)
		}

		// 2. Drift reports it and changes nothing.
		rows, err := s.Drift(ctx)
		if err != nil {
			t.Fatalf("Drift: %v", err)
		}
		if len(rows) != 1 || rows[0].Version != version || rows[0].Recorded != forged {
			t.Fatalf("Drift = %+v, want one row for version %d recorded %s", rows, version, forged)
		}
		if rows[0].Body == "" {
			t.Error("the drift report does not carry the file, so nobody can judge whether the " +
				"schema in front of them already matches it")
		}
		if _, err := s.Migrate(ctx); !errors.Is(err, ErrDirtySchema) {
			t.Error("Drift changed something: Migrate stopped refusing")
		}

		// 3. Rebaseline, and the migration runs again.
		if err := s.Rebaseline(ctx, version, forged); err != nil {
			t.Fatalf("Rebaseline: %v", err)
		}
		if _, err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate after rebaseline: %v", err)
		}
		if rows, err := s.Drift(ctx); err != nil || len(rows) != 0 {
			t.Errorf("drift remains after rebaseline: %v %v", rows, err)
		}
	})
}

// It cannot be used without reading the report first.
//
// Re-baselining asserts "the schema in front of me is what the new file would
// have produced", and only a person can know that. A flag that accepted any
// drift would be a way to make the guard stop complaining without looking, so
// the caller must name the checksum it expects to replace — a value it can only
// have got from the report.
func TestRebaselineRefusesWithoutTheCheckedChecksum(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		if _, err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		var version int64
		if err := s.queryRow(ctx,
			`SELECT version FROM schema_migrations ORDER BY version LIMIT 1`).Scan(&version); err != nil {
			t.Fatalf("read a version: %v", err)
		}

		if err := s.Rebaseline(ctx, version, ""); err == nil {
			t.Error("rebaseline accepted an empty expectation, which is a way to silence the " +
				"guard without reading it")
		}
		if err := s.Rebaseline(ctx, version, "not-the-recorded-one"); err == nil {
			t.Error("rebaseline accepted a stale expectation; if the file moved again since the " +
				"report was read, the thing being approved is not the thing that was reviewed")
		}
	})
}

// It is not a way to skip a migration.
//
// Marking unapplied work as done is a different and far more dangerous
// operation. This updates an existing row and never inserts one, so a version
// with no row cannot be declared complete.
func TestRebaselineCannotMarkAnUnappliedMigrationAsDone(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		if _, err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		var version int64
		if err := s.queryRow(ctx,
			`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
			t.Fatalf("read the top version: %v", err)
		}
		// Delete the row so the version is "unapplied" while its file still exists.
		if _, err := s.exec(ctx, `DELETE FROM schema_migrations WHERE version = ?`, version); err != nil {
			t.Fatalf("delete: %v", err)
		}
		err := s.Rebaseline(ctx, version, "anything")
		if err == nil {
			t.Fatal("rebaseline declared an unapplied migration complete")
		}
		if !strings.Contains(err.Error(), "run the migration") {
			t.Errorf("the refusal does not tell the operator what to do instead:\n  %v", err)
		}
	})
}
