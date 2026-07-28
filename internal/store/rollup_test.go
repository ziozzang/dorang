package store

import (
	"context"
	"testing"
	"time"
)

var rollupEpoch = time.Date(2026, 6, 15, 9, 30, 0, 0, time.UTC)

// TestRollupsMergeRatherThanAccumulateWrites is DESIGN 9.4 measured: a node
// pre-aggregates in memory and flushes once, so the number of statements
// touching a row is bounded by node count rather than request count.
func TestRollupsMergeRatherThanAccumulateWrites(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		batch := NewRollupBatch()
		const n = 500
		for i := 0; i < n; i++ {
			batch.Observe(RequestLog{
				TS:               rollupEpoch.Add(time.Duration(i) * time.Second),
				APIKeyID:         "key-1",
				TeamID:           "team-1",
				ModelGroup:       "model-a",
				Status:           200,
				PromptTokens:     2,
				CompletionTokens: 3,
				TotalTokens:      5,
				CostNano:         100,
				LatencyMS:        10,
			})
		}
		// 500 requests inside one hour collapse to one row per materialization.
		if batch.Len() != 3 {
			t.Fatalf("batch touches %d rows, want 3", batch.Len())
		}

		before := s.StatementCount()
		res, err := s.MergeRollups(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.StatementCount() - before; got > 3 {
			t.Fatalf("flush issued %d statements for 500 requests, want at most 3", got)
		}
		if res.KeyHourRows != 1 || res.ModelHourRows != 1 || res.TeamDayRows != 1 {
			t.Fatalf("result = %+v", res)
		}

		hour := hourFloor(rollupEpoch)
		got, err := s.ReadKeyHour(ctx, KeyHourKey{Hour: hour, APIKeyID: "key-1"})
		if err != nil {
			t.Fatal(err)
		}
		want := UsageDelta{
			Requests: n, PromptTokens: 2 * n, CompletionTokens: 3 * n,
			TotalTokens: 5 * n, CostNano: 100 * n, LatencyMSSum: 10 * n,
		}
		if got != want {
			t.Fatalf("usage_by_key_hour = %+v, want %+v", got, want)
		}
	})
}

// TestRollupMergeIsAdditiveAcrossFlushesAndNodes: two nodes flushing the same
// bucket must sum, because the merge is what makes per-node pre-aggregation
// correct rather than lossy.
func TestRollupMergeIsAdditiveAcrossFlushesAndNodes(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		hour := hourFloor(rollupEpoch)
		day := dayFloor(rollupEpoch)

		mk := func(requests, cost int64) RollupBatch {
			b := NewRollupBatch()
			b.KeyHour[KeyHourKey{Hour: hour, APIKeyID: "key-1"}] = UsageDelta{Requests: requests, CostNano: cost, Errors: 1}
			b.ModelHour[ModelHourKey{Hour: hour, ModelGroup: "model-a"}] = UsageDelta{Requests: requests, CostNano: cost}
			b.TeamDay[TeamDayKey{Day: day, TeamID: "team-1"}] = UsageDelta{Requests: requests, CostNano: cost}
			return b
		}

		nodeA := mk(10, 1000)
		nodeB := mk(7, 700)
		for _, b := range []RollupBatch{nodeA, nodeB} {
			if _, err := s.MergeRollups(ctx, b); err != nil {
				t.Fatal(err)
			}
		}

		key, err := s.ReadKeyHour(ctx, KeyHourKey{Hour: hour, APIKeyID: "key-1"})
		if err != nil {
			t.Fatal(err)
		}
		if key.Requests != 17 || key.CostNano != 1700 || key.Errors != 2 {
			t.Fatalf("usage_by_key_hour = %+v, want requests 17, cost 1700, errors 2", key)
		}
		model, err := s.ReadModelHour(ctx, ModelHourKey{Hour: hour, ModelGroup: "model-a"})
		if err != nil {
			t.Fatal(err)
		}
		if model.Requests != 17 {
			t.Fatalf("usage_by_model_hour requests = %d, want 17", model.Requests)
		}
		team, err := s.ReadTeamDay(ctx, TeamDayKey{Day: day, TeamID: "team-1"})
		if err != nil {
			t.Fatal(err)
		}
		if team.Requests != 17 {
			t.Fatalf("usage_by_team_day requests = %d, want 17", team.Requests)
		}
	})
}

func TestRollupBatchMergeInMemory(t *testing.T) {
	hour := hourFloor(rollupEpoch)
	a := NewRollupBatch()
	a.Observe(RequestLog{TS: rollupEpoch, APIKeyID: "k", ModelGroup: "m", TeamID: "t", Status: 200, CostNano: 5})
	b := NewRollupBatch()
	b.Observe(RequestLog{TS: rollupEpoch, APIKeyID: "k", ModelGroup: "m", TeamID: "t", Status: 500, CostNano: 7})
	a.Merge(b)

	got := a.KeyHour[KeyHourKey{Hour: hour, APIKeyID: "k"}]
	if got.Requests != 2 || got.CostNano != 12 || got.Errors != 1 {
		t.Fatalf("merged = %+v, want 2 requests, cost 12, 1 error", got)
	}
}

func TestObserveSkipsUnsetDimensions(t *testing.T) {
	b := NewRollupBatch()
	b.Observe(RequestLog{TS: rollupEpoch, ModelGroup: "m"}) // no key, no team
	if len(b.KeyHour) != 0 || len(b.TeamDay) != 0 {
		t.Fatalf("empty dimensions produced rows: %d key, %d team", len(b.KeyHour), len(b.TeamDay))
	}
	if len(b.ModelHour) != 1 {
		t.Fatalf("model dimension missing")
	}
}

func TestMergeRollupsIsWriteOrderDeterministic(t *testing.T) {
	// Two nodes with the same key set must write in the same order, or they
	// can deadlock against each other on PostgreSQL. The order is the sorted
	// primary key, and that has to hold whatever order the map iterates in.
	hour := hourFloor(rollupEpoch)
	rows := []rollupRow{
		{bucket: Micros(hour.Add(time.Hour)), dim: "b"},
		{bucket: Micros(hour), dim: "z"},
		{bucket: Micros(hour), dim: "a"},
	}
	sortRollupRows(rows)
	want := []string{"a", "z", "b"}
	for i, r := range rows {
		if r.dim != want[i] {
			t.Fatalf("row %d = %q, want %q (order: %v)", i, r.dim, want[i], rows)
		}
	}
}
