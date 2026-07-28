package scenario

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/store"
)

// DESIGN §14 scenario 18 — midnight rollover with metering active, no failed
// writes.
//
// Two failures hide here and they are not the same:
//
//  1. On a partitioned dialect, every insert fails at the first midnight after
//     metering ships unless partitions were pre-created — DESIGN §9.5, R1-15.
//     SQLite has no partitions, so the assertion there is that the absence is
//     REPORTED rather than silently succeeding: a caller who needs to know
//     which retention mechanism applies must not be able to mistake one for the
//     other.
//
//  2. The rollup hour must be part of the accumulator key, not derived at flush
//     time. A flush window that straddles midnight otherwise attributes the
//     whole window to the hour the flush happened in, which is a quiet
//     accounting error rather than an outage.

// meterSink is the wiring between internal/meter and internal/store. Neither
// package imports the other (DESIGN §9.1: the hot path does not touch the
// store, so the store must not be reachable from it), so the adapter lives with
// the caller — here, and in cmd/dorang.
type meterSink struct {
	s *store.Store

	rollupCalls atomic.Int64
	rollupRows  atomic.Int64
	traceRows   atomic.Int64
	failures    atomic.Int64
}

func (m *meterSink) WriteRollups(ctx context.Context, buckets []meter.Bucket) error {
	b := store.NewRollupBatch()
	for _, bk := range buckets {
		// The bucket is ALREADY aggregated, so the delta is copied rather than
		// folded one request at a time: Observe would count each bucket as a
		// single request and quietly divide every rate by its true count.
		d := store.UsageDelta{
			Requests:         bk.Requests,
			Errors:           bk.Errors,
			PromptTokens:     bk.Tokens.Input,
			CompletionTokens: bk.Tokens.Output,
			CachedTokens:     bk.Tokens.CacheRead,
			ReasoningTokens:  bk.Tokens.Reasoning,
			TotalTokens:      bk.Tokens.Input + bk.Tokens.Output,
			CostNano:         bk.CostNano,
			LatencyMSSum:     bk.LatencySum.Milliseconds(),
		}
		if bk.APIKeyID != "" {
			k := store.KeyHourKey{Hour: bk.HourStart, APIKeyID: bk.APIKeyID}
			cur := b.KeyHour[k]
			cur.Add(d)
			b.KeyHour[k] = cur
		}
		if bk.ModelGroup != "" {
			k := store.ModelHourKey{Hour: bk.HourStart, ModelGroup: bk.ModelGroup}
			cur := b.ModelHour[k]
			cur.Add(d)
			b.ModelHour[k] = cur
		}
		if bk.TeamID != "" {
			k := store.TeamDayKey{Day: bk.HourStart.UTC().Truncate(24 * time.Hour), TeamID: bk.TeamID}
			cur := b.TeamDay[k]
			cur.Add(d)
			b.TeamDay[k] = cur
		}
	}
	m.rollupCalls.Add(1)
	m.rollupRows.Add(int64(b.Len()))
	if _, err := m.s.MergeRollups(ctx, b); err != nil {
		m.failures.Add(1)
		return err
	}
	return nil
}

func (m *meterSink) WriteTraces(ctx context.Context, traces []meter.Trace) error {
	rows := make([]store.RequestTrace, 0, len(traces))
	for _, tr := range traces {
		rows = append(rows, store.RequestTrace{
			RequestID: tr.RequestID, TS: tr.Time, TraceID: tr.TraceID,
			Excerpt: tr.Excerpt, ExcerptBytes: int64(len(tr.Excerpt)), ExcerptMode: "truncated",
		})
	}
	m.traceRows.Add(int64(len(rows)))
	if err := m.s.InsertRequestTraces(ctx, rows); err != nil {
		m.failures.Add(1)
		return err
	}
	return nil
}

func TestScenario18_MidnightRolloverWithMeteringActive(t *testing.T) {
	// 23:58:00 UTC, so that a single flush window genuinely straddles midnight.
	clk := newClock()
	clk.set(time.Date(2026, 7, 28, 23, 58, 0, 0, time.UTC))

	s := openStore(t, clk.now, store.LegacyAuth{})
	sink := &meterSink{s: s}

	m := meter.New(meter.Config{
		Sink: sink,
		Now:  clk.now,
		// The background loops are disabled so the scenario drives the flush
		// itself: a timer would make "the window straddled midnight" a race.
		FlushInterval: -1,
		DrainInterval: -1,
		ShipInterval:  -1,
	})
	t.Cleanup(func() { _ = m.Close() })

	ev := func(status int) meter.Event {
		return meter.Event{
			Time:         clk.now(),
			APIKeyID:     "key-1",
			TeamID:       "team-1",
			ModelGroup:   "chat-large",
			Provider:     "vendor",
			CredentialID: "acct-1",
			Endpoint:     "/v1/chat/completions",
			Status:       status,
			Tokens:       meter.Tokens{Input: 100, Output: 20},
			Latency:      25 * time.Millisecond,
		}
	}

	// Before midnight.
	for range 3 {
		m.Record(ev(200))
	}
	beforeHour := clk.now().UTC().Truncate(time.Hour)
	beforeDay := clk.now().UTC().Truncate(24 * time.Hour)

	// Cross it. The clock moves; nothing sleeps.
	clk.advance(4 * time.Minute) // 00:02:00 the next day
	for range 5 {
		m.Record(ev(200))
	}
	afterHour := clk.now().UTC().Truncate(time.Hour)
	afterDay := clk.now().UTC().Truncate(24 * time.Hour)
	if beforeDay.Equal(afterDay) {
		t.Fatalf("setup: the clock did not cross midnight (%s -> %s)", beforeDay, afterDay)
	}

	// ONE flush, after midnight, covering both sides of it.
	ctx := context.Background()
	if err := m.Flush(ctx); err != nil {
		t.Fatalf("flush across midnight failed: %v", err)
	}
	if got := sink.failures.Load(); got != 0 {
		t.Fatalf("%d writes failed across the rollover", got)
	}
	if st := m.Stats(); st.FlushErrors != 0 {
		t.Fatalf("the meter recorded %d flush errors", st.FlushErrors)
	}
	if st := m.Stats(); st.Recorded != 8 {
		t.Fatalf("recorded = %d, want 8: the numeric path has no drop", st.Recorded)
	}
	if got := sink.rollupCalls.Load(); got != 1 {
		t.Fatalf("the sink was called %d times, want 1: the scenario needs ONE flush "+
			"spanning the boundary", got)
	}
	// Two hours × (key-hour + model-hour) plus two team-days: six rows, and the
	// count is what proves the two sides did not fold into one bucket.
	if got := sink.rollupRows.Load(); got != 6 {
		t.Fatalf("the flush produced %d rollup rows, want 6 (2 key-hours, 2 model-hours, "+
			"2 team-days)", got)
	}

	// The load-bearing assertion: the two sides of midnight landed in DIFFERENT
	// hour buckets. A meter that derived the hour at flush time would put all
	// eight requests in the 00:00 hour, and every "requests per hour" report
	// would show a nightly spike that never happened.
	before, err := s.ReadKeyHour(ctx, store.KeyHourKey{Hour: beforeHour, APIKeyID: "key-1"})
	if err != nil {
		t.Fatalf("reading the pre-midnight hour: %v", err)
	}
	after, err := s.ReadKeyHour(ctx, store.KeyHourKey{Hour: afterHour, APIKeyID: "key-1"})
	if err != nil {
		t.Fatalf("reading the post-midnight hour: %v", err)
	}
	if before.Requests != 3 {
		t.Errorf("the hour before midnight recorded %d requests, want 3", before.Requests)
	}
	if after.Requests != 5 {
		t.Errorf("the hour after midnight recorded %d requests, want 5", after.Requests)
	}

	// And the day boundary, which is the one a team budget is charged against.
	dayBefore, err := s.ReadTeamDay(ctx, store.TeamDayKey{Day: beforeDay, TeamID: "team-1"})
	if err != nil {
		t.Fatal(err)
	}
	dayAfter, err := s.ReadTeamDay(ctx, store.TeamDayKey{Day: afterDay, TeamID: "team-1"})
	if err != nil {
		t.Fatal(err)
	}
	if dayBefore.Requests != 3 || dayAfter.Requests != 5 {
		t.Errorf("team-day split = %d/%d, want 3/5: a day boundary was crossed inside one flush",
			dayBefore.Requests, dayAfter.Requests)
	}

	t.Run("ledger writes succeed on both sides of the boundary", func(t *testing.T) {
		rows := []store.RequestLog{
			{ID: store.NewID(), TS: beforeHour.Add(58 * time.Minute), APIKeyID: "key-1",
				TeamID: "team-1", ModelGroup: "chat-large", Status: 200, Tags: []string{"pre"}},
			{ID: store.NewID(), TS: afterHour.Add(2 * time.Minute), APIKeyID: "key-1",
				TeamID: "team-1", ModelGroup: "chat-large", Status: 200, Tags: []string{"post"}},
		}
		if err := s.InsertRequestLogs(ctx, rows); err != nil {
			t.Fatalf("a ledger write spanning midnight failed: %v", err)
		}
		page, err := s.ListRequestsByKey(ctx,
			"key-1",
			store.TimeRange{Start: beforeDay, End: afterDay.Add(24 * time.Hour)},
			store.Page{Limit: 10})
		if err != nil {
			t.Fatalf("ledger query: %v", err)
		}
		if len(page.Rows) != 2 {
			t.Fatalf("read back %d rows, want 2", len(page.Rows))
		}
	})

	t.Run("an unpartitioned dialect reports the absence rather than succeeding quietly", func(t *testing.T) {
		// DESIGN §9.5 makes this a deliberate error: a caller scheduling
		// maintenance must not be able to mistake DELETE-based retention for
		// DROP PARTITION.
		if got := s.Partitioning(); got != store.PartitioningNone {
			t.Fatalf("SQLite reports partitioning %q", got)
		}
		if _, err := s.EnsurePartitions(ctx, clk.now(), 3); !errors.Is(err, store.ErrNoPartitioning) {
			t.Fatalf("EnsurePartitions returned %v, want ErrNoPartitioning", err)
		}
		// EnsureDays is the writer's in-line recovery and must succeed with
		// nothing to do, because there is nothing a writer needs it to have
		// done.
		if _, err := s.EnsureDays(ctx, []time.Time{beforeDay, afterDay}); err != nil {
			t.Fatalf("EnsureDays must be a no-op here, got %v", err)
		}
		rep, err := s.Maintain(ctx, clk.now(), store.RetentionPolicy{RequestLogs: 90 * 24 * time.Hour})
		if err != nil {
			t.Fatalf("Maintain: %v", err)
		}
		if rep.Mechanism != store.MechanismDelete {
			t.Errorf("mechanism = %q, want %q on an unpartitioned dialect",
				rep.Mechanism, store.MechanismDelete)
		}
	})

	t.Run("postgres: partitions exist across the boundary before the writer needs them", func(t *testing.T) {
		// The half SQLite cannot answer. It runs when DORANG_TEST_PG names a
		// database and is skipped otherwise, rather than being silently absent.
		dsn := os.Getenv("DORANG_TEST_PG")
		if dsn == "" {
			t.Skip("DORANG_TEST_PG is not set; the partitioned half of scenario 18 is not exercised")
		}
		pg, err := store.Open(ctx, store.Config{
			Driver: store.DialectPostgres, DSN: dsn, Now: clk.now,
			Pepper: []byte("scenario-pepper-not-a-real-secret"),
		})
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		defer pg.Close()
		if pg.Partitioning() != store.PartitioningDaily {
			t.Fatalf("partitioning = %q", pg.Partitioning())
		}
		if _, err := pg.EnsurePartitions(ctx, clk.now(), 3); err != nil {
			t.Fatalf("EnsurePartitions: %v", err)
		}
		row := store.RequestLog{ID: store.NewID(), TS: afterHour.Add(3 * time.Minute),
			APIKeyID: "key-1", TeamID: "team-1", ModelGroup: "chat-large", Status: 200}
		if err := pg.InsertRequestLogs(ctx, []store.RequestLog{row}); err != nil {
			t.Fatalf("the first insert after midnight failed: %v", err)
		}
	})
}
