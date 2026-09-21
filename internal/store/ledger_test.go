package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

var ledgerEpoch = time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)

func seededLedger(t *testing.T, s *Store) TimeRange {
	t.Helper()
	seedLedger(t, s, ledgerEpoch, 48*time.Hour, 600)
	analyze(t, s)
	return TimeRange{Start: ledgerEpoch.Add(-time.Hour), End: ledgerEpoch.Add(49 * time.Hour)}
}

// TestQuerySetReturnsCorrectRowsAndUsesItsIndex walks the six queries of
// DESIGN 9.3, checking both halves of the claim: the right rows come back, and
// the planner reaches them through the index that exists for them.
func TestQuerySetReturnsCorrectRowsAndUsesItsIndex(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		r := seededLedger(t, s)

		t.Run("recent requests for a key", func(t *testing.T) {
			page, err := s.ListRequestsByKey(ctx, "key-3", r, Page{Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Rows) == 0 {
				t.Fatal("no rows")
			}
			assertDescending(t, page.Rows)
			for _, row := range page.Rows {
				if row.APIKeyID != "key-3" {
					t.Fatalf("row for %s leaked into key-3's results", row.APIKeyID)
				}
			}
			q, args, err := s.buildLedgerQuery(ledgerSpec{where: "l.api_key_id = ?", args: []any{"key-3"}}, r, Page{Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			assertIndexed(t, s, q, args, "request_logs_key_ts_idx")
		})

		t.Run("recent requests for a user", func(t *testing.T) {
			page, err := s.ListRequestsByUser(ctx, "user-3", r, Page{Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Rows) == 0 {
				t.Fatal("no rows")
			}
			assertDescending(t, page.Rows)
			for _, row := range page.Rows {
				if row.UserID != "user-3" {
					t.Fatalf("user filter leaked %s", row.UserID)
				}
			}
			q, args, err := s.buildLedgerQuery(ledgerSpec{where: "l.user_id = ?", args: []any{"user-3"}}, r, Page{Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			assertIndexed(t, s, q, args, "request_logs_user_ts_idx")
		})

		t.Run("recent requests for a team", func(t *testing.T) {
			page, err := s.ListRequestsByTeam(ctx, "team-2", r, Page{Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Rows) == 0 {
				t.Fatal("no rows")
			}
			for _, row := range page.Rows {
				if row.TeamID != "team-2" {
					t.Fatalf("row for %s leaked into team-2's results", row.TeamID)
				}
			}
			q, args, _ := s.buildLedgerQuery(ledgerSpec{where: "l.team_id = ?", args: []any{"team-2"}}, r, Page{Limit: 50})
			assertIndexed(t, s, q, args, "request_logs_team_ts_idx")
		})

		t.Run("by trace id", func(t *testing.T) {
			page, err := s.ListRequestsByTrace(ctx, "trace-42", r, Page{})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Rows) != 1 {
				t.Fatalf("got %d rows for trace-42, want 1", len(page.Rows))
			}
			if page.Rows[0].TraceID != "trace-42" {
				t.Fatalf("got trace %q", page.Rows[0].TraceID)
			}
			q, args, _ := s.buildLedgerQuery(ledgerSpec{where: "l.trace_id = ?", args: []any{"trace-42"}}, r, Page{})
			assertIndexed(t, s, q, args, "request_logs_trace_ts_idx")
		})

		t.Run("spend for a credential over a range", func(t *testing.T) {
			spend, err := s.SpendByCredential(ctx, "cred-4", r)
			if err != nil {
				t.Fatal(err)
			}
			if spend.Requests == 0 {
				t.Fatal("no requests counted")
			}
			// Cross-check against the rows themselves.
			var want Spend
			for i := 0; i < 600; i++ {
				if i%seedCreds != 4 {
					continue
				}
				want.Requests++
				want.CostNano += int64(1000 + i)
				want.TotalTokens += int64(20 + i)
				if i%seedErrorEvery == 0 {
					want.Errors++
				}
			}
			if spend.Requests != want.Requests || spend.CostNano != want.CostNano ||
				spend.TotalTokens != want.TotalTokens || spend.Errors != want.Errors {
				t.Fatalf("spend = %+v, want requests=%d cost=%d tokens=%d errors=%d",
					spend, want.Requests, want.CostNano, want.TotalTokens, want.Errors)
			}
			const q = `SELECT COUNT(*), COALESCE(SUM(l.cost_nano), 0) FROM request_logs l
				WHERE l.credential_id = ? AND l.ts >= ? AND l.ts < ?`
			assertIndexed(t, s, q, []any{"cred-4", Micros(r.Start), Micros(r.End)}, "request_logs_cred_ts_idx")
		})

		t.Run("errors over a range", func(t *testing.T) {
			page, err := s.ListErrors(ctx, r, Page{Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			wantErrors := 0
			for i := 0; i < 600; i++ {
				if i%seedErrorEvery == 0 {
					wantErrors++
				}
			}
			if len(page.Rows) != wantErrors {
				t.Fatalf("got %d error rows, want %d", len(page.Rows), wantErrors)
			}
			for _, row := range page.Rows {
				if row.Status < 400 {
					t.Fatalf("status %d is not an error", row.Status)
				}
			}
			q, args, _ := s.buildLedgerQuery(ledgerSpec{where: "l.status >= 400"}, r, Page{Limit: 100})
			assertIndexed(t, s, q, args, "request_logs_errors_idx")
		})

		t.Run("by tag", func(t *testing.T) {
			page, err := s.ListRequestsByTag(ctx, "prod", r, Page{Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			for i := 0; i < 600; i++ {
				if i%seedProdEvery == 0 {
					want++
				}
			}
			if len(page.Rows) != want {
				t.Fatalf("got %d tagged rows, want %d", len(page.Rows), want)
			}
			q, args, _ := s.buildLedgerQuery(ledgerSpec{
				join:          "JOIN request_log_tags t ON t.ts = l.ts AND t.request_id = l.id",
				where:         "t.tag = ?",
				args:          []any{"prod"},
				extraRangeCol: "t.ts",
			}, r, Page{Limit: 100})
			assertIndexed(t, s, q, args, "request_log_tags_tag_ts_idx")

			// The point of the normalized table: no query touches an array
			// column, so no query can degrade into scanning one.
			if _, err := s.query(ctx, "SELECT tags FROM request_logs WHERE 1 = 0"); err == nil {
				t.Fatal("request_logs has a tags column; DESIGN 9.3 requires a normalized table instead")
			}
		})
	})
}

// TestUnboundedLedgerQueryIsRefused is the explicit refusal of DESIGN 9.3.
// Every ledger entry point must reject an unbounded range, because one that
// answers instead is the slow path everything else was designed to avoid.
func TestUnboundedLedgerQueryIsRefused(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seedLedger(t, s, ledgerEpoch, time.Hour, 5)

		bad := []struct {
			name string
			r    TimeRange
			want error
		}{
			{"zero range", TimeRange{}, ErrUnboundedRange},
			{"no start", TimeRange{End: ledgerEpoch}, ErrUnboundedRange},
			{"no end", TimeRange{Start: ledgerEpoch}, ErrUnboundedRange},
			{"reversed", TimeRange{Start: ledgerEpoch, End: ledgerEpoch.Add(-time.Hour)}, ErrUnboundedRange},
			{"empty", TimeRange{Start: ledgerEpoch, End: ledgerEpoch}, ErrUnboundedRange},
			{"too wide", TimeRange{Start: ledgerEpoch, End: ledgerEpoch.AddDate(5, 0, 0)}, ErrRangeTooWide},
		}

		calls := map[string]func(TimeRange) error{
			"ListRequestsByKey":   func(r TimeRange) error { _, e := s.ListRequestsByKey(ctx, "key-1", r, Page{}); return e },
			"ListRequestsByTeam":  func(r TimeRange) error { _, e := s.ListRequestsByTeam(ctx, "team-1", r, Page{}); return e },
			"ListRequestsByTrace": func(r TimeRange) error { _, e := s.ListRequestsByTrace(ctx, "trace-1", r, Page{}); return e },
			"ListRequestsByTag":   func(r TimeRange) error { _, e := s.ListRequestsByTag(ctx, "prod", r, Page{}); return e },
			"ListErrors":          func(r TimeRange) error { _, e := s.ListErrors(ctx, r, Page{}); return e },
			"SpendByCredential":   func(r TimeRange) error { _, e := s.SpendByCredential(ctx, "cred-1", r); return e },
		}

		for name, call := range calls {
			for _, tc := range bad {
				if err := call(tc.r); !errors.Is(err, tc.want) {
					t.Errorf("%s with %s: got %v, want %v", name, tc.name, err, tc.want)
				}
			}
		}
	})
}

func TestLedgerPaginationCoversEveryRowExactlyOnce(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		const rows = 900
		seedLedger(t, s, ledgerEpoch, 6*time.Hour, rows)
		r := TimeRange{Start: ledgerEpoch.Add(-time.Hour), End: ledgerEpoch.Add(7 * time.Hour)}

		seen := map[string]int{}
		page := Page{Limit: 4}
		pages := 0
		for {
			res, err := s.ListRequestsByTeam(ctx, "team-1", r, page)
			if err != nil {
				t.Fatal(err)
			}
			assertDescending(t, res.Rows)
			for _, row := range res.Rows {
				seen[row.ID]++
			}
			pages++
			if res.Next == nil || pages > 100 {
				break
			}
			page.After = res.Next
		}

		want := 0
		for i := 0; i < rows; i++ {
			if i%seedTeams == 1 {
				want++
			}
		}
		if len(seen) != want {
			t.Fatalf("paged over %d distinct rows, want %d", len(seen), want)
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("row %s returned %d times", id, n)
			}
		}
		if pages < 2 {
			t.Fatalf("only %d page(s); pagination was not exercised", pages)
		}
	})
}

func TestPageLimitIsBounded(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		r := TimeRange{Start: ledgerEpoch, End: ledgerEpoch.Add(time.Hour)}
		if _, err := s.ListErrors(ctx, r, Page{Limit: DefaultMaxPageSize + 1}); err == nil {
			t.Fatal("an oversized page limit was accepted")
		}
		if _, err := s.ListErrors(ctx, r, Page{Limit: -1}); err == nil {
			t.Fatal("a negative page limit was accepted")
		}
	})
}

func TestTracesAreSeparateFromTheLedger(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		rows := seedLedger(t, s, ledgerEpoch, time.Hour, 3)

		// R1-3: excerpts are not a ledger column.
		if _, err := s.query(ctx, "SELECT excerpt FROM request_logs WHERE 1 = 0"); err == nil {
			t.Fatal("request_logs carries an excerpt column; DESIGN 9.2 puts excerpts in request_traces")
		}

		err := s.InsertRequestTraces(ctx, []RequestTrace{{
			RequestID: rows[0].ID,
			TS:        rows[0].TS,
			TraceID:   rows[0].TraceID,
			Excerpt:   "hello",
		}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRequestTrace(ctx, rows[0].TS, rows[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Excerpt != "hello" || got.ExcerptBytes != 5 || got.ExcerptMode != "truncated" {
			t.Fatalf("trace = %+v", got)
		}
		if _, err := s.GetRequestTrace(ctx, rows[1].TS, rows[1].ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing trace: %v, want ErrNotFound", err)
		}
	})
}

func TestCostAmountsAreRangeChecked(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		err := s.InsertRequestLogs(context.Background(), []RequestLog{{
			TS:       ledgerEpoch,
			CostNano: MaxAmountNano + 1,
		}})
		if !errors.Is(err, ErrAmountRange) {
			t.Fatalf("insert with an out-of-range cost: %v, want ErrAmountRange", err)
		}
	})
}

func assertDescending(t *testing.T, rows []RequestLog) {
	t.Helper()
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1], rows[i]
		if a.TS.Before(b.TS) || (a.TS.Equal(b.TS) && a.ID < b.ID) {
			t.Fatalf("rows are not newest-first at %d: %s/%s then %s/%s",
				i, a.TS, a.ID, b.TS, b.ID)
		}
	}
}
