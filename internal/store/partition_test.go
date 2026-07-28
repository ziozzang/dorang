package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// midnight is deliberately the last second of a day: this test exists because
// revision 1 scheduled partition creation six milestones after the writer,
// which would have failed EVERY insert at the first midnight after metering
// shipped (DESIGN 9.5, R1-15).
var beforeMidnight = time.Date(2026, 7, 31, 23, 59, 50, 0, time.UTC)

func TestWritesCrossMidnight(t *testing.T) {
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			clk := newClock(beforeMidnight)
			s := openStore(t, b, b.env(t), func(c *Config) { c.Now = clk.Now })

			// A flush ten seconds before midnight.
			before := []RequestLog{{
				ID: "before", TS: beforeMidnight.Add(5 * time.Second),
				APIKeyID: "key-1", TeamID: "team-1", CredentialID: "cred-1",
				ModelGroup: "m", Status: 200, CostNano: 10, TraceID: "tr-before",
				Tags: []string{"midnight"},
			}}
			if err := s.InsertRequestLogs(ctx, before); err != nil {
				t.Fatalf("insert before midnight: %v", err)
			}
			if err := s.InsertRequestTraces(ctx, []RequestTrace{{
				RequestID: "before", TS: before[0].TS, Excerpt: "b",
			}}); err != nil {
				t.Fatalf("insert trace before midnight: %v", err)
			}

			// The clock rolls over. Nothing restarts, nothing re-migrates, no
			// maintenance job has run in between.
			clk.Set(beforeMidnight.Add(20 * time.Second)) // 2026-08-01 00:00:10

			after := []RequestLog{{
				ID: "after", TS: clk.Now(),
				APIKeyID: "key-1", TeamID: "team-1", CredentialID: "cred-1",
				ModelGroup: "m", Status: 200, CostNano: 20, TraceID: "tr-after",
				Tags: []string{"midnight"},
			}}
			if err := s.InsertRequestLogs(ctx, after); err != nil {
				t.Fatalf("insert after midnight: %v", err)
			}
			if err := s.InsertRequestTraces(ctx, []RequestTrace{{
				RequestID: "after", TS: after[0].TS, Excerpt: "a",
			}}); err != nil {
				t.Fatalf("insert trace after midnight: %v", err)
			}

			// Both rows are readable through a query that spans the boundary.
			r := TimeRange{Start: beforeMidnight.Add(-time.Hour), End: clk.Now().Add(time.Hour)}
			page, err := s.ListRequestsByKey(ctx, "key-1", r, Page{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Rows) != 2 {
				t.Fatalf("got %d rows across midnight, want 2", len(page.Rows))
			}
			tagged, err := s.ListRequestsByTag(ctx, "midnight", r, Page{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(tagged.Rows) != 2 {
				t.Fatalf("got %d tagged rows across midnight, want 2", len(tagged.Rows))
			}
			spend, err := s.SpendByCredential(ctx, "cred-1", r)
			if err != nil {
				t.Fatal(err)
			}
			if spend.CostNano != 30 {
				t.Fatalf("spend across midnight = %d, want 30", spend.CostNano)
			}

			if s.Partitioning() == PartitioningDaily {
				parts, err := s.ListPartitions(ctx, "request_logs")
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"request_logs_20260731", "request_logs_20260801", "request_logs_20260802"} {
					if !containsString(parts, want) {
						t.Fatalf("partition %s was not pre-created; have %v", want, parts)
					}
				}
			}
		})
	}
}

// TestWriteBeyondPreCreatedPartitionsRecovers covers the case pre-creation
// cannot: a row whose day is further out than the maintenance window, which on
// PostgreSQL is a hard insert failure unless the writer recovers from it.
func TestWriteBeyondPreCreatedPartitionsRecovers(t *testing.T) {
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			clk := newClock(beforeMidnight)
			s := openStore(t, b, b.env(t), func(c *Config) { c.Now = clk.Now })

			far := beforeMidnight.AddDate(0, 0, 30)
			row := RequestLog{ID: "far", TS: far, APIKeyID: "key-1", ModelGroup: "m", Status: 200}
			if err := s.InsertRequestLogs(ctx, []RequestLog{row}); err != nil {
				t.Fatalf("insert far beyond the pre-created window: %v", err)
			}

			r := TimeRange{Start: far.Add(-time.Hour), End: far.Add(time.Hour)}
			page, err := s.ListRequestsByKey(ctx, "key-1", r, Page{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Rows) != 1 {
				t.Fatalf("got %d rows, want 1", len(page.Rows))
			}
		})
	}
}

func TestRetentionMechanismIsExplicit(t *testing.T) {
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			clk := newClock(beforeMidnight)
			s := openStore(t, b, b.env(t), func(c *Config) { c.Now = clk.Now })

			// Seven days of history, one row per day.
			var rows []RequestLog
			for i := 0; i < 7; i++ {
				ts := dayFloor(beforeMidnight).AddDate(0, 0, -i).Add(12 * time.Hour)
				rows = append(rows, RequestLog{
					ID: "r" + itoa(i), TS: ts, APIKeyID: "key-1", ModelGroup: "m", Status: 200,
					Tags: []string{"keepme"},
				})
			}
			if err := s.InsertRequestLogs(ctx, rows); err != nil {
				t.Fatal(err)
			}

			rep, err := s.Maintain(ctx, clk.Now(), RetentionPolicy{
				RequestLogs:   3 * 24 * time.Hour,
				RequestTraces: 2 * 24 * time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Dialect != s.Driver() {
				t.Fatalf("report dialect = %s", rep.Dialect)
			}

			switch s.Partitioning() {
			case PartitioningDaily:
				if rep.Mechanism != MechanismDropPartition {
					t.Fatalf("mechanism = %s, want %s", rep.Mechanism, MechanismDropPartition)
				}
				if len(rep.PartitionsDropped) == 0 {
					t.Fatal("no partitions dropped")
				}
				// Maintenance leaves the future covered, whether it had to
				// create anything on this pass or Open already had.
				parts, err := s.ListPartitions(ctx, "request_logs")
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i <= 2; i++ {
					want := partitionName("request_logs", dayFloor(clk.Now()).AddDate(0, 0, i))
					if !containsString(parts, want) {
						t.Fatalf("after maintenance, %s is missing: %v", want, parts)
					}
				}
			case PartitioningNone:
				if rep.Mechanism != MechanismDelete {
					t.Fatalf("mechanism = %s, want %s", rep.Mechanism, MechanismDelete)
				}
				if rep.RowsDeleted["request_logs"] == 0 {
					t.Fatalf("nothing deleted: %+v", rep.RowsDeleted)
				}
				if len(rep.PartitionsDropped) != 0 {
					t.Fatal("an unpartitioned store reported dropped partitions")
				}
			}

			// Whatever the mechanism, the outcome is the same: rows inside the
			// retention window survive and rows outside it do not.
			r := TimeRange{
				Start: dayFloor(beforeMidnight).AddDate(0, 0, -30),
				End:   beforeMidnight.Add(time.Hour),
			}
			page, err := s.ListRequestsByKey(ctx, "key-1", r, Page{Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			cutoff := clk.Now().Add(-3 * 24 * time.Hour)
			for _, row := range page.Rows {
				if row.TS.Before(dayFloor(cutoff)) {
					t.Fatalf("row from %s survived a 3-day retention (cutoff day %s)", row.TS, dayFloor(cutoff))
				}
			}
			if len(page.Rows) == 0 {
				t.Fatal("retention removed everything")
			}
		})
	}
}

func TestPartitionNameRoundTrip(t *testing.T) {
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	name := partitionName("request_logs", day)
	if name != "request_logs_20260102" {
		t.Fatalf("name = %q", name)
	}
	got, ok := partitionDay("request_logs", name)
	if !ok || !got.Equal(day) {
		t.Fatalf("partitionDay = %s, %v", got, ok)
	}
	// Anything that is not exactly parent_YYYYMMDD is left alone rather than
	// dropped: a retention job must never guess about a table it does not own.
	for _, bad := range []string{"request_logs", "request_logs_x", "request_logs_2026010", "other_20260102"} {
		if _, ok := partitionDay("request_logs", bad); ok {
			t.Fatalf("partitionDay accepted %q", bad)
		}
	}
}

func TestListPartitionsRejectsUnknownTables(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		_, err := s.ListPartitions(context.Background(), "api_keys")
		if s.Partitioning() == PartitioningNone {
			if !errors.Is(err, ErrNoPartitioning) {
				t.Fatalf("got %v, want ErrNoPartitioning", err)
			}
			return
		}
		if !errors.Is(err, ErrBadIdentifier) {
			t.Fatalf("got %v, want ErrBadIdentifier", err)
		}
	})
}

func TestValidIdent(t *testing.T) {
	good := []string{"LiteLLM_VerificationToken", "a", "t1", "_x"} // pragma: allowlist secret — test fixture
	bad := []string{"", "1t", "a b", `a"b`, "a;drop", "a-b", strings.Repeat("x", 64)}
	for _, s := range good {
		if !validIdent(s) {
			t.Errorf("validIdent(%q) = false", s)
		}
	}
	for _, s := range bad {
		if validIdent(s) {
			t.Errorf("validIdent(%q) = true", s)
		}
	}
}

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
