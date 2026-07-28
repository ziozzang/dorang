package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"
)

// UsageDelta is the fixed-cardinality counter set a node accumulates in memory
// before flushing (DESIGN 12.1: numeric accounting is never lost, and it is
// merged rather than written per request).
type UsageDelta struct {
	Requests         int64
	Errors           int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	ReasoningTokens  int64
	TotalTokens      int64
	CostNano         int64
	LatencyMSSum     int64
}

// Add folds o into d.
func (d *UsageDelta) Add(o UsageDelta) {
	d.Requests += o.Requests
	d.Errors += o.Errors
	d.PromptTokens += o.PromptTokens
	d.CompletionTokens += o.CompletionTokens
	d.CachedTokens += o.CachedTokens
	d.ReasoningTokens += o.ReasoningTokens
	d.TotalTokens += o.TotalTokens
	d.CostNano += o.CostNano
	d.LatencyMSSum += o.LatencyMSSum
}

// Rollup keys. Time components are bucket starts in UTC; Observe floors them.
type (
	// KeyHourKey identifies a usage_by_key_hour row.
	KeyHourKey struct {
		Hour     time.Time
		APIKeyID string
	}
	// ModelHourKey identifies a usage_by_model_hour row.
	ModelHourKey struct {
		Hour       time.Time
		ModelGroup string
	}
	// TeamDayKey identifies a usage_by_team_day row.
	TeamDayKey struct {
		Day    time.Time
		TeamID string
	}
)

// RollupBatch is one node's pre-aggregated flush.
//
// DESIGN 9.4: a single full-cube hourly key would bloat permanently and create
// hot-row contention when one key dominates a bucket. Instead there are a few
// purpose-built materializations, and each node pre-aggregates in memory and
// merges once per flush -- so the number of writes touching a given row is
// bounded by node count, not request count.
type RollupBatch struct {
	KeyHour   map[KeyHourKey]UsageDelta
	ModelHour map[ModelHourKey]UsageDelta
	TeamDay   map[TeamDayKey]UsageDelta
}

// NewRollupBatch returns an empty batch ready to Observe into.
func NewRollupBatch() RollupBatch {
	return RollupBatch{
		KeyHour:   map[KeyHourKey]UsageDelta{},
		ModelHour: map[ModelHourKey]UsageDelta{},
		TeamDay:   map[TeamDayKey]UsageDelta{},
	}
}

// Len reports how many rows the batch will touch.
func (b RollupBatch) Len() int { return len(b.KeyHour) + len(b.ModelHour) + len(b.TeamDay) }

// Observe folds one completed request into the batch. This is the in-memory
// half of the merge: a thousand requests against one key in one hour become
// one row update, not a thousand.
//
// It is not safe for concurrent use -- a batch belongs to the goroutine
// draining a node's accumulators, and merging across goroutines is Merge's job.
func (b RollupBatch) Observe(r RequestLog) {
	d := UsageDelta{
		Requests:         1,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		CachedTokens:     r.CachedTokens,
		ReasoningTokens:  r.ReasoningTokens,
		TotalTokens:      r.TotalTokens,
		CostNano:         r.CostNano,
		LatencyMSSum:     r.LatencyMS,
	}
	if r.Status >= 400 {
		d.Errors = 1
	}
	hour := hourFloor(r.TS)
	if r.APIKeyID != "" {
		k := KeyHourKey{Hour: hour, APIKeyID: r.APIKeyID}
		e := b.KeyHour[k]
		e.Add(d)
		b.KeyHour[k] = e
	}
	if r.ModelGroup != "" {
		k := ModelHourKey{Hour: hour, ModelGroup: r.ModelGroup}
		e := b.ModelHour[k]
		e.Add(d)
		b.ModelHour[k] = e
	}
	if r.TeamID != "" {
		k := TeamDayKey{Day: dayFloor(r.TS), TeamID: r.TeamID}
		e := b.TeamDay[k]
		e.Add(d)
		b.TeamDay[k] = e
	}
}

// Merge folds another batch into b, for combining per-CPU accumulators before
// the single flush.
func (b RollupBatch) Merge(o RollupBatch) {
	for k, v := range o.KeyHour {
		e := b.KeyHour[k]
		e.Add(v)
		b.KeyHour[k] = e
	}
	for k, v := range o.ModelHour {
		e := b.ModelHour[k]
		e.Add(v)
		b.ModelHour[k] = e
	}
	for k, v := range o.TeamDay {
		e := b.TeamDay[k]
		e.Add(v)
		b.TeamDay[k] = e
	}
}

// RollupResult counts rows written per materialization.
type RollupResult struct {
	KeyHourRows   int
	ModelHourRows int
	TeamDayRows   int
}

// MergeRollups applies a merged in-memory batch to the rollup tables, adding
// each delta to whatever is already there.
//
// Rows are written in sorted primary-key order. That is not cosmetic: two
// nodes flushing overlapping key sets in different orders can deadlock against
// each other on PostgreSQL, and a total order on the keys removes the cycle by
// construction. Map iteration order would reintroduce it.
//
// Each key appears once per statement, because the input is a map -- which is
// also what PostgreSQL requires, since ON CONFLICT DO UPDATE refuses to affect
// the same row twice in one command.
func (s *Store) MergeRollups(ctx context.Context, b RollupBatch) (RollupResult, error) {
	var out RollupResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := Micros(s.now())

		keyRows := make([]rollupRow, 0, len(b.KeyHour))
		for k, v := range b.KeyHour {
			keyRows = append(keyRows, rollupRow{bucket: Micros(k.Hour), dim: k.APIKeyID, d: v})
		}
		if err := s.mergeRollupTable(ctx, tx, "usage_by_key_hour", "api_key_id", keyRows, now); err != nil {
			return err
		}
		out.KeyHourRows = len(keyRows)

		modelRows := make([]rollupRow, 0, len(b.ModelHour))
		for k, v := range b.ModelHour {
			modelRows = append(modelRows, rollupRow{bucket: Micros(k.Hour), dim: k.ModelGroup, d: v})
		}
		if err := s.mergeRollupTable(ctx, tx, "usage_by_model_hour", "model_group", modelRows, now); err != nil {
			return err
		}
		out.ModelHourRows = len(modelRows)

		teamRows := make([]rollupRow, 0, len(b.TeamDay))
		for k, v := range b.TeamDay {
			teamRows = append(teamRows, rollupRow{bucket: Micros(k.Day), dim: k.TeamID, d: v})
		}
		if err := s.mergeRollupTable(ctx, tx, "usage_by_team_day", "team_id", teamRows, now); err != nil {
			return err
		}
		out.TeamDayRows = len(teamRows)
		return nil
	})
	if err != nil {
		return RollupResult{}, err
	}
	return out, nil
}

type rollupRow struct {
	bucket int64
	dim    string
	d      UsageDelta
}

// sortRollupRows imposes the total order on the primary key that makes the
// flush deadlock-free between nodes.
func sortRollupRows(rows []rollupRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].bucket != rows[j].bucket {
			return rows[i].bucket < rows[j].bucket
		}
		return rows[i].dim < rows[j].dim
	})
}

const rollupCounters = `requests, errors, prompt_tokens, completion_tokens, cached_tokens,
	reasoning_tokens, total_tokens, cost_nano, latency_ms_sum`

func (s *Store) mergeRollupTable(ctx context.Context, tx *sql.Tx, table, dimCol string, rows []rollupRow, now int64) error {
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		if err := checkAmount(rows[i].d.CostNano, table+" cost"); err != nil {
			return err
		}
	}
	sortRollupRows(rows)

	// 12 bound parameters per row; chunked well under SQLite's per-statement
	// parameter ceiling.
	for chunk := range chunks(rows, 500) {
		var b strings.Builder
		b.WriteString("INSERT INTO " + table + " (bucket_start, " + dimCol + ", " + rollupCounters + ", updated_at) VALUES ")
		args := make([]any, 0, len(chunk)*12)
		for i := range chunk {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(valuesTuple(12))
			r := &chunk[i]
			args = append(args, r.bucket, r.dim,
				r.d.Requests, r.d.Errors, r.d.PromptTokens, r.d.CompletionTokens,
				r.d.CachedTokens, r.d.ReasoningTokens, r.d.TotalTokens,
				r.d.CostNano, r.d.LatencyMSSum, now)
		}
		b.WriteString(`
			ON CONFLICT (bucket_start, ` + dimCol + `) DO UPDATE SET
			  requests          = ` + table + `.requests          + excluded.requests,
			  errors            = ` + table + `.errors            + excluded.errors,
			  prompt_tokens     = ` + table + `.prompt_tokens     + excluded.prompt_tokens,
			  completion_tokens = ` + table + `.completion_tokens + excluded.completion_tokens,
			  cached_tokens     = ` + table + `.cached_tokens     + excluded.cached_tokens,
			  reasoning_tokens  = ` + table + `.reasoning_tokens  + excluded.reasoning_tokens,
			  total_tokens      = ` + table + `.total_tokens      + excluded.total_tokens,
			  cost_nano         = ` + table + `.cost_nano         + excluded.cost_nano,
			  latency_ms_sum    = ` + table + `.latency_ms_sum    + excluded.latency_ms_sum,
			  updated_at        = excluded.updated_at`)
		if _, err := s.txExec(ctx, tx, b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// ReadKeyHour reads one usage_by_key_hour row.
func (s *Store) ReadKeyHour(ctx context.Context, k KeyHourKey) (UsageDelta, error) {
	return s.readRollup(ctx, "usage_by_key_hour", "api_key_id", Micros(k.Hour), k.APIKeyID)
}

// ReadModelHour reads one usage_by_model_hour row.
func (s *Store) ReadModelHour(ctx context.Context, k ModelHourKey) (UsageDelta, error) {
	return s.readRollup(ctx, "usage_by_model_hour", "model_group", Micros(k.Hour), k.ModelGroup)
}

// ReadTeamDay reads one usage_by_team_day row.
func (s *Store) ReadTeamDay(ctx context.Context, k TeamDayKey) (UsageDelta, error) {
	return s.readRollup(ctx, "usage_by_team_day", "team_id", Micros(k.Day), k.TeamID)
}

func (s *Store) readRollup(ctx context.Context, table, dimCol string, bucket int64, dim string) (UsageDelta, error) {
	var d UsageDelta
	err := s.queryRow(ctx,
		"SELECT "+rollupCounters+" FROM "+table+" WHERE bucket_start = ? AND "+dimCol+" = ?",
		bucket, dim).
		Scan(&d.Requests, &d.Errors, &d.PromptTokens, &d.CompletionTokens, &d.CachedTokens,
			&d.ReasoningTokens, &d.TotalTokens, &d.CostNano, &d.LatencyMSSum)
	if err == sql.ErrNoRows {
		return UsageDelta{}, ErrNotFound
	}
	if err != nil {
		return UsageDelta{}, err
	}
	return d, nil
}
