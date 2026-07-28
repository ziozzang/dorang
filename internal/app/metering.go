package app

import (
	"context"
	"time"

	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/store"
)

// meterAdapter satisfies server.Meter with the real metering pipeline.
//
// The two event shapes are close but not identical, and where they differ the
// server's is the poorer one: it has no team id, because the HTTP surface never
// learns one — internal/server.Principal exposes only KeyID. A team-scoped
// rollup therefore stays empty on this path. Carrying the team would mean
// widening server.Principal, which is a change to that package's contract and
// not something the wiring layer gets to decide.
type meterAdapter struct {
	m   *meter.Meter
	now func() time.Time
}

// Record implements server.Meter. It never blocks and never fails the request:
// internal/meter's Record is a bounded, non-blocking enqueue.
func (a *meterAdapter) Record(ev server.Event) {
	r := &ev.Result
	a.m.Record(meter.Event{
		Time:         a.now(),
		APIKeyID:     ev.KeyID,
		ModelGroup:   ev.Model,
		Provider:     r.Provider,
		CredentialID: r.Credential,
		Endpoint:     ev.Route,
		Status:       ev.Status,
		Tokens: meter.Tokens{
			Input:      r.Tokens.Input,
			Output:     r.Tokens.Output,
			CacheRead:  r.Tokens.CacheRead,
			CacheWrite: r.Tokens.CacheWrite,
			Reasoning:  r.Tokens.Reasoning,
		},
		CostNano: r.CostNanoUSD,
		Latency:  time.Duration(ev.DurationNS),
		TTFT:     time.Duration(r.TTFTNS),
		Trace: meter.TraceInfo{
			RequestID:      ev.RequestID,
			UpstreamModel:  r.UpstreamModel,
			QueueWait:      time.Duration(r.QueueNS),
			CapacityWait:   time.Duration(r.QueueNS),
			Retries:        max(r.Attempt-1, 0),
			FallbackReason: r.FallbackFrom,
		},
	})
}

// storeSink satisfies meter.Sink with the real store.
//
// internal/meter declares Sink and internal/store implements the operations
// behind it, and neither imports the other (DESIGN §9.1): the hot path does not
// touch the store, so the store must not be reachable from it either. This is
// the join.
//
// Both methods are idempotent as the interface requires: rollups merge on
// (key, hour) and ledger rows are keyed by request id.
type storeSink struct {
	st *store.Store
}

// WriteRollups implements meter.Sink.
func (s *storeSink) WriteRollups(ctx context.Context, buckets []meter.Bucket) error {
	if len(buckets) == 0 {
		return nil
	}
	b := store.NewRollupBatch()
	for i := range buckets {
		bk := &buckets[i]
		d := store.UsageDelta{
			Requests:         bk.Requests,
			Errors:           bk.Errors,
			PromptTokens:     bk.Tokens.Input,
			CompletionTokens: bk.Tokens.Output,
			CachedTokens:     bk.Tokens.CacheRead,
			ReasoningTokens:  bk.Tokens.Reasoning,
			TotalTokens:      bk.Tokens.Total(),
			CostNano:         bk.CostNano,
			LatencyMSSum:     bk.LatencySum.Milliseconds(),
		}
		// The bucket's hour is already the accumulator key, so it is used as
		// given rather than re-floored: a flush straddling an hour boundary
		// carries two buckets and attributing both to "now" would move one.
		if bk.APIKeyID != "" {
			k := store.KeyHourKey{Hour: bk.HourStart, APIKeyID: bk.APIKeyID}
			e := b.KeyHour[k]
			e.Add(d)
			b.KeyHour[k] = e
		}
		if bk.ModelGroup != "" {
			k := store.ModelHourKey{Hour: bk.HourStart, ModelGroup: bk.ModelGroup}
			e := b.ModelHour[k]
			e.Add(d)
			b.ModelHour[k] = e
		}
		if bk.TeamID != "" {
			k := store.TeamDayKey{Day: bk.HourStart.UTC().Truncate(24 * time.Hour), TeamID: bk.TeamID}
			e := b.TeamDay[k]
			e.Add(d)
			b.TeamDay[k] = e
		}
	}
	if b.Len() == 0 {
		return nil
	}
	_, err := s.st.MergeRollups(ctx, b)
	return err
}

// WriteTraces implements meter.Sink.
//
// One trace becomes one ledger row plus, when there is an excerpt to keep, one
// trace row. The ledger row goes first: the trace references it.
func (s *storeSink) WriteTraces(ctx context.Context, traces []meter.Trace) error {
	if len(traces) == 0 {
		return nil
	}
	logs := make([]store.RequestLog, 0, len(traces))
	rows := make([]store.RequestTrace, 0, len(traces))
	for i := range traces {
		t := &traces[i]
		logs = append(logs, store.RequestLog{
			ID:               t.RequestID,
			TS:               t.Time,
			APIKeyID:         t.APIKeyID,
			TeamID:           t.TeamID,
			CredentialID:     t.CredentialID,
			ProviderID:       t.Provider,
			ModelGroup:       t.ModelGroup,
			UpstreamModel:    t.UpstreamModel,
			Endpoint:         t.Endpoint,
			Status:           t.Status,
			PromptTokens:     t.Tokens.Input,
			CompletionTokens: t.Tokens.Output,
			CachedTokens:     t.Tokens.CacheRead,
			ReasoningTokens:  t.Tokens.Reasoning,
			TotalTokens:      t.Tokens.Total(),
			CostNano:         t.CostNano,
			MarginalCostNano: t.CostNano,
			LatencyMS:        t.Latency.Milliseconds(),
			TTFTMS:           t.TTFT.Milliseconds(),
			QueueMS:          t.QueueWait.Milliseconds(),
			CapacityWaitMS:   t.CapacityWait.Milliseconds(),
			UpstreamMS:       t.UpstreamConnect.Milliseconds(),
			FallbackCount:    t.Retries,
			TraceID:          t.TraceID,
		})
		if t.Excerpt != "" {
			rows = append(rows, store.RequestTrace{
				RequestID:    t.RequestID,
				TS:           t.Time,
				TraceID:      t.TraceID,
				Excerpt:      t.Excerpt,
				ExcerptBytes: int64(len(t.Excerpt)),
			})
		}
	}
	if err := s.st.InsertRequestLogs(ctx, logs); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	return s.st.InsertRequestTraces(ctx, rows)
}
