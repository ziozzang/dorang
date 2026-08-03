package app

import (
	"context"
	"time"

	"github.com/ziozzang/dorang/internal/keyguard"
	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/store"
)

// meterAdapter satisfies server.Meter with the real metering pipeline.
//
// The two event shapes map field for field. They did not always: the server's
// event carried only a key id, so meter.Event.TeamID was always empty and
// usage_by_team_day (DESIGN §9.4) was a materialization nothing ever wrote.
// The team and the user cannot be recovered downstream either — that would be a
// store lookup on the metering path — so server.Principal carries them and this
// adapter copies them across.
type meterAdapter struct {
	m   *meter.Meter
	now func() time.Time
	// record feeds the same event into the Prometheus surface of DESIGN §12.3.
	//
	// It hangs off the meter adapter rather than off a second hook in
	// internal/server because this is already the observation point: one place
	// where a finished request is described in full, reached exactly once per
	// request, after the client's last byte. A second hook would be a second
	// chance to disagree with this one.
	record func(*server.Event)
	// guard is DESIGN §11.6's token guard. It is fed here rather than from the
	// request path for the same reason the meter is: this is the one place a
	// finished request is described in full, and recording usage is a bounded
	// map write, not a decision. The decision is a periodic sweep.
	//
	// Nil is the disabled guard and costs one nil check.
	guard *keyguard.Guard
}

// Record implements server.Meter. It never blocks and never fails the request:
// internal/meter's Record is a bounded, non-blocking enqueue.
func (a *meterAdapter) Record(ev server.Event) {
	if a.record != nil {
		a.record(&ev)
	}
	r := &ev.Result
	if tokens := totalTokens(r.Tokens); tokens > 0 && ev.KeyID != "" {
		// The guard watches TOKENS, not requests: §11.6's trigger is a token
		// rate against a token baseline, and a key that doubles its request
		// count while halving its context has not departed from anything.
		//
		// It counts them through totalTokens and not through a sum written out
		// here. This line read Input+Output+Reasoning — a THIRD answer to "how
		// many tokens was that", seventy lines above the one C1 corrected, and
		// it reported 128 for the request whose own response body said 120.
		// Reasoning is already inside Output (§10.7), so adding it again
		// inflates both the observed rate and the baseline it is compared
		// against, and a key whose reasoning share merely CHANGES drifts
		// against a baseline built under a different mix.
		_ = a.guard.Observe(context.Background(), ev.KeyID, tokens, a.now())
	}
	a.m.Record(meter.Event{
		Time:         a.now(),
		APIKeyID:     ev.KeyID,
		SecretID:     ev.SecretID,
		UserID:       ev.UserID,
		TeamID:       ev.TeamID,
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
		CostNano:             r.CostNanoUSD,
		MarginalCostNano:     r.MarginalNanoUSD,
		SubscriptionCostNano: r.SubscriptionNanoUSD,
		// The list-rate equivalent and which of §8.5's three states this request
		// is in. It was computed for every priced request and published in
		// x-dorang-notional-usd, and it stopped here — so `notional_nano`, which
		// has had a column on the ledger row and on all three rollups since
		// migration 0002, was written by nothing, and every window of
		// /global/spend/report and of the operator's usage screen reported it as
		// unavailable.
		NotionalCostNano: r.NotionalNanoUSD,
		NotionalKnown:    r.NotionalPriced,
		NotionalMissing:  r.NotionalMissing,
		// The utilization disclosure, carried on every priced request whose rule
		// declares a factor — the fallbacks included. A row that records the
		// factor only when the price moved cannot answer "was this request
		// charged at a premium, and if not, why not", which is the question an
		// invoice dispute is.
		UtilMultiplierPPM: r.UtilizationMultiplierPPM,
		UtilPPM:           int64(r.UtilizationPPM),
		UtilSource:        r.UtilizationSource,
		Latency:           time.Duration(ev.DurationNS),
		TTFT:              time.Duration(r.TTFTNS),
		Trace: meter.TraceInfo{
			RequestID:     ev.RequestID,
			UpstreamModel: r.UpstreamModel,
			// The SAME field x-dorang-deployment is stamped from
			// (server.stampHeaders reads r.Deployment), so the ledger row and
			// the response header cannot disagree about which deployment served
			// the request. They used to: the header carried it and the column
			// was empty on every row.
			DeploymentID:   r.Deployment,
			Streamed:       ev.Streamed,
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
	// nodeID stamps every ledger row with the process that served it.
	//
	// It lives on the sink rather than on meter.Trace because the node is a
	// property of THIS PROCESS and not of a request: every trace this sink ever
	// writes came from here, so carrying it per-trace would widen the spool
	// record by a string that is the same on every row.
	//
	// The column existed, the store wrote it and read it back, and nothing ever
	// set it — 52,766 rows of NULL on a two-node cluster, where "which node
	// served this" is the first question an operator asks and the ledger could
	// not answer it (DESIGN §17.1).
	nodeID string
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
			MarginalNano:     bk.MarginalCostNano,
			SubscriptionNano: bk.SubscriptionCostNano,
			// The list-rate sum and the two counts that say whether it is whole.
			// See [store.UsageDelta.NotionalKnown]: a sum with holes in it is
			// reported as unavailable, never as a total.
			NotionalNano:     bk.NotionalCostNano,
			NotionalRequests: bk.NotionalRequests,
			NotionalMissing:  bk.NotionalMissingCount,
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
			NodeID:           s.nodeID,
			APIKeyID:         t.APIKeyID,
			SecretID:         t.SecretID,
			UserID:           t.UserID,
			TeamID:           t.TeamID,
			CredentialID:     t.CredentialID,
			ProviderID:       t.Provider,
			DeploymentID:     t.DeploymentID,
			ModelGroup:       t.ModelGroup,
			UpstreamModel:    t.UpstreamModel,
			Endpoint:         t.Endpoint,
			Status:           t.Status,
			Streamed:         t.Streamed,
			PromptTokens:     t.Tokens.Input,
			CompletionTokens: t.Tokens.Output,
			CachedTokens:     t.Tokens.CacheRead,
			ReasoningTokens:  t.Tokens.Reasoning,
			TotalTokens:      t.Tokens.Total(),
			CostNano:         t.CostNano,
			// The decomposition as pricing computed it, not the total copied
			// twice. `MarginalCostNano: t.CostNano` is the line that gave
			// `subscription_cost_nano` a column, a reader in /spend/logs and no
			// producer, and it filed a flat plan's share under `marginal_spend`
			// — which DESIGN §8.1 forbids by name, because routing compares the
			// marginal figure and a sunk plan cost must not enter it.
			MarginalCostNano:     t.MarginalCostNano,
			SubscriptionCostNano: t.SubscriptionCostNano,
			// Per row the answer is a boolean: this request had a list rate or
			// it did not. §8.5 rule 5 forbids reporting the second as a zero,
			// which is what the column pair exists for.
			NotionalNano:      t.NotionalCostNano,
			NotionalKnown:     t.NotionalKnown,
			UtilMultiplierPPM: t.UtilMultiplierPPM,
			UtilPPM:           t.UtilPPM,
			UtilSource:        t.UtilSource,
			LatencyMS:         t.Latency.Milliseconds(),
			TTFTMS:            t.TTFT.Milliseconds(),
			QueueMS:           t.QueueWait.Milliseconds(),
			CapacityWaitMS:    t.CapacityWait.Milliseconds(),
			UpstreamMS:        t.UpstreamConnect.Milliseconds(),
			FallbackCount:     t.Retries,
			TraceID:           t.TraceID,
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
