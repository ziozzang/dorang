package store

import (
	"context"
	"testing"
	"time"
)

// TestReportedCostRoundTripsAndAggregates: a vendor that stated zero and one that
// stated nothing are different rows, on the way back out and in the sums
// `dorangctl spend reconcile` reads.
func TestReportedCostRoundTripsAndAggregates(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
		row := func(id, model string, cost int64, stated bool, reported int64) RequestLog {
			return RequestLog{
				TS: at, ID: id, CredentialID: "deepinfra-1", ProviderID: "deepinfra",
				ModelGroup: model, UpstreamModel: model, Endpoint: "/v1/chat/completions",
				Status: 200, CostNano: cost, ReportedCost: stated, ReportedCostNano: reported,
			}
		}
		rows := []RequestLog{
			row("r1", "llama", 300, true, 340),
			row("r2", "llama", 300, true, 0), // stated free
			row("r3", "llama", 300, false, 0),
			row("r4", "whisper", 13347, false, 0),
		}
		if err := s.InsertRequestLogs(ctx, rows); err != nil {
			t.Fatal(err)
		}
		r := TimeRange{Start: at.Add(-time.Hour), End: at.Add(time.Hour)}

		got := map[string]RequestLog{}
		for _, id := range []string{"r1", "r2", "r3"} {
			lp, err := s.runLedgerQuery(ctx,
				"SELECT "+requestLogCols+" FROM request_logs l WHERE l.id = ?", []any{id}, Page{})
			if err != nil || len(lp.Rows) != 1 {
				t.Fatalf("read %s: %v (%d rows)", id, err, len(lp.Rows))
			}
			got[id] = lp.Rows[0]
		}
		if g := got["r1"]; !g.ReportedCost || g.ReportedCostNano != 340 {
			t.Errorf("r1 = %d, %v; want 340 stated", g.ReportedCostNano, g.ReportedCost)
		}
		if g := got["r2"]; !g.ReportedCost || g.ReportedCostNano != 0 {
			t.Errorf("r2 = %d, %v; want a stated zero", g.ReportedCostNano, g.ReportedCost)
		}
		if g := got["r3"]; g.ReportedCost {
			t.Errorf("r3 stated nothing and reads back as stated %d", g.ReportedCostNano)
		}

		sums, err := s.SpendByCredentialModel(ctx, "deepinfra-1", r)
		if err != nil {
			t.Fatal(err)
		}
		want := []ModelSpend{
			{UpstreamModel: "llama", Requests: 3, CostNano: 900, ReportedNano: 340, ReportedRequests: 2},
			{UpstreamModel: "whisper", Requests: 1, CostNano: 13347},
		}
		if len(sums) != len(want) {
			t.Fatalf("sums = %+v, want %+v", sums, want)
		}
		for i := range want {
			if sums[i] != want[i] {
				t.Errorf("sums[%d] = %+v, want %+v", i, sums[i], want[i])
			}
		}
		if _, err := s.SpendByCredentialModel(ctx, "deepinfra-1", TimeRange{}); err == nil {
			t.Error("an unbounded range was accepted")
		}
	})
}
