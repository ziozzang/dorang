package store

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
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
	// MarginalNano and SubscriptionNano decompose CostNano by pricing class
	// (DESIGN §8.1). They are two columns and not one derived from the other:
	// the classes compose as marginal + subscription, THEN adjustments in
	// order, so neither can be recovered from the total once a discount rule
	// exists.
	MarginalNano     int64
	SubscriptionNano int64
	// NotionalNano is the list-rate equivalent of this bucket's traffic (DESIGN
	// §8.5). NotionalRequests counts the requests that contributed to it and
	// NotionalMissing the ones that reached pricing with no notional rule.
	//
	// Counts rather than a flag, because a bucket is many requests and "known"
	// is not a property that survives being ORed. They are sums, which is what
	// keeps [UsageDelta.NotionalKnown] associative across merges, across nodes
	// and across a GROUP BY.
	NotionalNano     int64
	NotionalRequests int64
	NotionalMissing  int64
	LatencyMSSum     int64
}

// NotionalKnown reports whether NotionalNano is the whole list-rate figure for
// this delta rather than a sum with holes in it (DESIGN §8.5 rule 5: a missing
// list rate is reported as missing, never as a flattering zero).
//
// Both clauses are load-bearing. A missing count above zero means the sum is
// short by an unknown amount. A notional count of zero means nothing
// contributed to it at all — an empty window, a window of refusals, or a bucket
// written before the counts existed — and "0" would be a claim about a list
// rate nobody computed.
func (d UsageDelta) NotionalKnown() bool {
	return d.NotionalRequests > 0 && d.NotionalMissing == 0
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
	d.MarginalNano += o.MarginalNano
	d.SubscriptionNano += o.SubscriptionNano
	d.NotionalNano += o.NotionalNano
	d.NotionalRequests += o.NotionalRequests
	d.NotionalMissing += o.NotionalMissing
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
		// The decomposition travels with the total here too, or an aggregate
		// built from ledger rows would disagree with the rows it was built from.
		MarginalNano:     r.MarginalCostNano,
		SubscriptionNano: r.SubscriptionCostNano,
		NotionalNano:     r.NotionalNano,
		LatencyMSSum:     r.LatencyMS,
	}
	if r.NotionalKnown {
		d.NotionalRequests = 1
	} else {
		// One row with no list rate makes the bucket's notional sum an
		// understatement, and the count is how the reader finds out.
		//
		// A ledger row carries one flag and therefore cannot distinguish "no
		// notional rule matched" from "this request never reached pricing at
		// all", which the metering path can. Folding both into missing is the
		// conservative direction: it reports unavailable where the truth might
		// have been a complete total, and never a total that is quietly short.
		d.NotionalMissing = 1
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
	reasoning_tokens, total_tokens, cost_nano, marginal_nano, subscription_nano,
	notional_nano, notional_requests, notional_missing, latency_ms_sum`

// rollupArity is the number of bind variables one merged row takes: the bucket
// key, the dimension value, every counter rollupCounters names, and updated_at.
//
// It is COUNTED from that list rather than written beside it. The number was a
// literal 14 in two places, and a literal that must be edited whenever a column
// is added is a bind-variable mismatch waiting for the next column — reported
// by the driver, at run time, on the flush path.
var rollupArity = strings.Count(rollupCounters, ",") + 1 + 3

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

	// rollupArity bound parameters per row; chunked well under SQLite's
	// per-statement parameter ceiling.
	for chunk := range chunks(rows, 500) {
		var b strings.Builder
		b.WriteString("INSERT INTO " + table + " (bucket_start, " + dimCol + ", " + rollupCounters + ", updated_at) VALUES ")
		args := make([]any, 0, len(chunk)*rollupArity)
		for i := range chunk {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(valuesTuple(rollupArity))
			r := &chunk[i]
			args = append(args, r.bucket, r.dim,
				r.d.Requests, r.d.Errors, r.d.PromptTokens, r.d.CompletionTokens,
				r.d.CachedTokens, r.d.ReasoningTokens, r.d.TotalTokens,
				r.d.CostNano, r.d.MarginalNano, r.d.SubscriptionNano,
				r.d.NotionalNano, r.d.NotionalRequests, r.d.NotionalMissing,
				r.d.LatencyMSSum, now)
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
			  marginal_nano     = ` + table + `.marginal_nano     + excluded.marginal_nano,
			  subscription_nano = ` + table + `.subscription_nano + excluded.subscription_nano,
			  notional_nano     = ` + table + `.notional_nano     + excluded.notional_nano,
			  notional_requests = ` + table + `.notional_requests + excluded.notional_requests,
			  notional_missing  = ` + table + `.notional_missing  + excluded.notional_missing,
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

// KeySpendRange sums one or more keys' cost over a bounded window, from the
// per-key rollup of DESIGN §9.4.
//
// It exists because `api_keys.spend_nano` has no writer on the request path.
// Budget enforcement lives in the durable counter (`budget_state`), and that
// counter is DRAWN rather than spent — a node charges a whole block before it
// spends a unit of it, so it runs up to one block ahead of reality and is the
// wrong number to print next to a ceiling on an operator screen. This
// materialization is the one that agrees with the ledger row for row, which is
// why /global/spend/report answers from it too.
//
// One statement for the whole page. The index added beside this query is
// (api_key_id, bucket_start), so a key's window is a seek and a walk rather
// than the scan the (bucket_start, api_key_id) primary key would make of it.
//
// Unlike [Store.ReadRollupRange] this does not enforce MaxTimeRange: the window
// is not a caller's search, it is one subject's budget period, and refusing to
// report a yearly budget's spend because a year is wider than the ledger search
// cap would refuse the only question the endpoint is for.
func (s *Store) KeySpendRange(ctx context.Context, ids []string, r TimeRange) (map[string]int64, error) {
	if len(ids) == 0 {
		return map[string]int64{}, nil
	}
	if !r.End.After(r.Start) {
		return nil, ErrUnboundedRange
	}
	out := make(map[string]int64, len(ids))
	// Chunked for the same reason every other IN list here is: a page of keys is
	// bounded by the administration surface's MaxListLimit, and a bind-variable
	// ceiling is a thing a driver has and a caller does not know about.
	for chunk := range chunks(ids, 500) {
		q := `SELECT api_key_id, SUM(cost_nano) FROM usage_by_key_hour
		       WHERE bucket_start >= ? AND bucket_start < ? AND api_key_id IN (` +
			placeholders(len(chunk)) + `) GROUP BY api_key_id`
		args := make([]any, 0, len(chunk)+2)
		args = append(args, Micros(r.Start), Micros(r.End))
		for _, id := range chunk {
			args = append(args, id)
		}
		if err := s.scanKeySpend(ctx, q, args, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) scanKeySpend(ctx context.Context, q string, args []any, out map[string]int64) error {
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id   string
			cost sql.NullInt64
		)
		if err := rows.Scan(&id, &cost); err != nil {
			return err
		}
		out[id] = cost.Int64
	}
	return rows.Err()
}

// RollupDim names one of the three materializations DESIGN §9.4 keeps.
//
// There is deliberately no fourth: the design refuses a full cube, so a report
// can be grouped by the dimension its materialization is keyed on and by time,
// and by nothing else. A query for a combination nobody materialized is refused
// by name rather than answered with a table scan (§9.3).
type RollupDim uint8

const (
	// RollupKey reads usage_by_key_hour.
	RollupKey RollupDim = iota
	// RollupModel reads usage_by_model_hour.
	RollupModel
	// RollupTeam reads usage_by_team_day.
	RollupTeam
)

func (d RollupDim) table() (table, col string, bucketWidth int64) {
	switch d {
	case RollupKey:
		return "usage_by_key_hour", "api_key_id", int64(time.Hour / time.Microsecond)
	case RollupTeam:
		return "usage_by_team_day", "team_id", int64(24 * time.Hour / time.Microsecond)
	default:
		return "usage_by_model_hour", "model_group", int64(time.Hour / time.Microsecond)
	}
}

// RollupQuery asks for aggregated usage over a bounded window.
//
// ByDay and ByDim select the grouping. Neither means "one row for the whole
// range"; both means one row per (day, dimension value).
type RollupQuery struct {
	Dim   RollupDim
	Range TimeRange
	ByDay bool
	ByDim bool
	// Limit caps the returned rows. The total is computed by its own query and
	// is therefore the total over the RANGE, not over the page — a truncated
	// sum presented as a total is a wrong number wearing an authoritative name.
	Limit int
}

// RollupRow is one aggregated bucket.
type RollupRow struct {
	// Day is the UTC day the bucket falls in, zero when the query did not group
	// by time.
	Day time.Time
	// Dim is the dimension's value, empty when the query did not group by it.
	Dim   string
	Usage UsageDelta
}

// ReadRollupRange aggregates one materialization over a bounded range.
//
// This is the query DESIGN §9.4's rollups were built to answer and that
// /global/spend/report had no implementation for: the counters were correct and
// unreadable through the administration surface, so an operator could see one
// hour of one key and could not see a week of anything.
//
// It is an index range scan, not a table scan: every rollup table is keyed
// (bucket_start, <dimension>), so a bounded window on bucket_start is a seek
// plus a walk. That is the whole reason the grouping offered is exactly the
// grouping the materializations are keyed on.
func (s *Store) ReadRollupRange(ctx context.Context, q RollupQuery) ([]RollupRow, UsageDelta, error) {
	if err := q.Range.validate(s.cfg.MaxTimeRange); err != nil {
		return nil, UsageDelta{}, err
	}
	table, dimCol, width := q.Dim.table()
	start, end := Micros(q.Range.Start), Micros(q.Range.End)

	total, err := s.rollupTotal(ctx, table, start, end)
	if err != nil {
		return nil, UsageDelta{}, err
	}

	// Neither grouping asked for: the total is the whole answer, and running a
	// second identical query to produce one row of it would be waste.
	if !q.ByDay && !q.ByDim {
		return nil, total, nil
	}

	var group, sel []string
	if q.ByDay {
		// The day floor is arithmetic on the bucket key rather than a date
		// function, so one statement serves both dialects and stays an
		// expression over an indexed integer. Unix micro-seconds put midnight
		// UTC at an exact multiple of a day, which is what makes this exact.
		day := "(bucket_start - (bucket_start % " + strconv.FormatInt(width*bucketsPerDay(width), 10) + "))"
		sel = append(sel, day+" AS day_start")
		group = append(group, day)
	} else {
		sel = append(sel, "0 AS day_start")
	}
	if q.ByDim {
		sel = append(sel, dimCol)
		group = append(group, dimCol)
	} else {
		sel = append(sel, "'' AS dim_value")
	}
	sel = append(sel, sumCols...)

	qs := "SELECT " + strings.Join(sel, ", ") +
		" FROM " + table +
		" WHERE bucket_start >= ? AND bucket_start < ?" +
		" GROUP BY " + strings.Join(group, ", ") +
		// Oldest day first, and within a day the largest spender first: a
		// report that is truncated by Limit should lose the rows an operator
		// was least likely to be asking about.
		" ORDER BY day_start ASC, SUM(cost_nano) DESC, 2 ASC"
	args := []any{start, end}
	if q.Limit > 0 {
		qs += " LIMIT ?"
		args = append(args, q.Limit)
	}

	rows, err := s.query(ctx, qs, args...)
	if err != nil {
		return nil, UsageDelta{}, err
	}
	defer rows.Close()

	var out []RollupRow
	for rows.Next() {
		var (
			day int64
			dim string
			r   RollupRow
		)
		if err := rows.Scan(&day, &dim, &r.Usage.Requests, &r.Usage.Errors,
			&r.Usage.PromptTokens, &r.Usage.CompletionTokens, &r.Usage.CachedTokens,
			&r.Usage.ReasoningTokens, &r.Usage.TotalTokens, &r.Usage.CostNano,
			&r.Usage.MarginalNano, &r.Usage.SubscriptionNano,
			&r.Usage.NotionalNano, &r.Usage.NotionalRequests, &r.Usage.NotionalMissing,
			&r.Usage.LatencyMSSum); err != nil {
			return nil, UsageDelta{}, err
		}
		if q.ByDay {
			r.Day = time.UnixMicro(day).UTC()
		}
		if q.ByDim {
			r.Dim = dim
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, UsageDelta{}, err
	}
	return out, total, nil
}

// bucketsPerDay is how many buckets of the given width make a day, so the day
// floor is exact for an hourly table and a no-op for a daily one.
func bucketsPerDay(width int64) int64 {
	return int64(24*time.Hour/time.Microsecond) / width
}

// sumCols are the counter aggregates, in the order readRollup scans them.
var sumCols = []string{
	"COALESCE(SUM(requests),0)", "COALESCE(SUM(errors),0)",
	"COALESCE(SUM(prompt_tokens),0)", "COALESCE(SUM(completion_tokens),0)",
	"COALESCE(SUM(cached_tokens),0)", "COALESCE(SUM(reasoning_tokens),0)",
	"COALESCE(SUM(total_tokens),0)", "COALESCE(SUM(cost_nano),0)",
	"COALESCE(SUM(marginal_nano),0)", "COALESCE(SUM(subscription_nano),0)",
	"COALESCE(SUM(notional_nano),0)", "COALESCE(SUM(notional_requests),0)",
	"COALESCE(SUM(notional_missing),0)",
	"COALESCE(SUM(latency_ms_sum),0)",
}

func (s *Store) rollupTotal(ctx context.Context, table string, start, end int64) (UsageDelta, error) {
	var d UsageDelta
	err := s.queryRow(ctx,
		"SELECT "+strings.Join(sumCols, ", ")+" FROM "+table+
			" WHERE bucket_start >= ? AND bucket_start < ?", start, end).
		Scan(&d.Requests, &d.Errors, &d.PromptTokens, &d.CompletionTokens, &d.CachedTokens,
			&d.ReasoningTokens, &d.TotalTokens, &d.CostNano,
			&d.MarginalNano, &d.SubscriptionNano,
			&d.NotionalNano, &d.NotionalRequests, &d.NotionalMissing, &d.LatencyMSSum)
	if err != nil && err != sql.ErrNoRows {
		return UsageDelta{}, err
	}
	return d, nil
}

func (s *Store) readRollup(ctx context.Context, table, dimCol string, bucket int64, dim string) (UsageDelta, error) {
	var d UsageDelta
	err := s.queryRow(ctx,
		"SELECT "+rollupCounters+" FROM "+table+" WHERE bucket_start = ? AND "+dimCol+" = ?",
		bucket, dim).
		Scan(&d.Requests, &d.Errors, &d.PromptTokens, &d.CompletionTokens, &d.CachedTokens,
			&d.ReasoningTokens, &d.TotalTokens, &d.CostNano,
			&d.MarginalNano, &d.SubscriptionNano,
			&d.NotionalNano, &d.NotionalRequests, &d.NotionalMissing, &d.LatencyMSSum)
	if err == sql.ErrNoRows {
		return UsageDelta{}, ErrNotFound
	}
	if err != nil {
		return UsageDelta{}, err
	}
	return d, nil
}
