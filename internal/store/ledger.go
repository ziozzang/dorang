package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RequestLog is one ledger row (DESIGN 9.2). Message excerpts are deliberately
// not here: at the top of the scale range an inline excerpt column dominates
// storage and drags every ledger write down with it (R1-3). They live in
// RequestTrace.
type RequestLog struct {
	ID string
	TS time.Time

	APIKeyID string
	// SecretID names WHICH of the key's secrets authenticated this request
	// (DESIGN §11.2c). A key has one id and, during a rotation's grace period,
	// two secrets; both authenticate to the same principal, and an operator
	// needs to see whether the client actually rolled BEFORE the window closes
	// rather than finding out when it shuts. The key id alone cannot answer
	// that question.
	SecretID     string
	UserID       string
	TeamID       string
	CredentialID string
	ProviderID   string
	DeploymentID string

	ModelGroup    string
	UpstreamModel string
	Endpoint      string

	Status     int
	ErrorClass string

	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	ReasoningTokens  int64
	TotalTokens      int64

	CostNano             int64
	MarginalCostNano     int64
	SubscriptionCostNano int64
	// NotionalNano is what this request would have cost at list rate (DESIGN
	// §8.5) and NotionalKnown says whether that figure exists.
	//
	// The flag is the column, not a nicety. §8.5 rule 5: a missing list rate is
	// reported as missing and never as zero, because a subscription with no
	// notional_rate rule would otherwise read as infinitely efficient. The two
	// columns have had a schema since migration 0002 and no writer until the
	// meter carried the pair — which is why /spend/logs and the usage screen
	// both reported `unavailable` on every deployment while the pricing engine
	// computed the figure for every request and put it in a response header.
	//
	// It is never a term in CostNano. §8.5 requires that exclusion to be
	// structural, and separate columns are what makes it so.
	NotionalNano  int64
	NotionalKnown bool

	// UtilMultiplierPPM is the factor a utilization-priced rule applied to this
	// request's rate, in parts per million (1_000_000 is 1.0x), and UtilPPM the
	// occupancy it came from (DESIGN §8.6). UtilSource is where that reading came
	// from, or which refusal stood in for it.
	//
	// All three are zero and empty on an ordinary rate card, and are written as
	// SQL NULL there: a rule that does not price on utilization has no factor,
	// and a 1_000_000 would claim it had one and that the backend was measured.
	// UtilSource is the discriminator — "observed" and "no_load_header" produce
	// the same charge on an idle backend and are not the same fact about it.
	UtilMultiplierPPM int64
	UtilPPM           int64
	UtilSource        string

	LatencyMS      int64
	TTFTMS         int64
	QueueMS        int64
	CapacityWaitMS int64
	UpstreamMS     int64

	FallbackCount int
	Streamed      bool

	TraceID   string
	SessionID string
	NodeID    string
	BatchID   string
	Metadata  string

	// Tags are written to the normalized request_log_tags table, never to an
	// array column on this row (DESIGN 9.3).
	Tags []string
}

// RequestTrace is the sampled, byte-budgeted excerpt for one request
// (DESIGN 9.2, 12.2).
type RequestTrace struct {
	RequestID    string
	TS           time.Time
	TraceID      string
	Excerpt      string
	ExcerptBytes int64
	// ExcerptMode is truncated, hash, or none.
	ExcerptMode string
	Spans       string
}

// TimeRange is a half-open [Start, End) window. Every ledger query takes one.
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// validate enforces DESIGN 9.3: a ledger query without a bounded, ordered time
// range is refused. A range wider than max is refused too -- "1970 to 2100" is
// unbounded in every way that costs anything.
func (r TimeRange) validate(max time.Duration) error {
	if r.Start.IsZero() || r.End.IsZero() {
		return fmt.Errorf("%w: both Start and End are required", ErrUnboundedRange)
	}
	if !r.End.After(r.Start) {
		return fmt.Errorf("%w: End must be after Start", ErrUnboundedRange)
	}
	if max > 0 && r.End.Sub(r.Start) > max {
		return fmt.Errorf("%w: %s > %s", ErrRangeTooWide, r.End.Sub(r.Start), max)
	}
	return nil
}

// Cursor is a keyset pagination position: the (ts, id) of the last row of the
// previous page. Offsets are not offered, because an offset over a ledger is a
// scan whose cost grows with the page number.
type Cursor struct {
	TS time.Time
	ID string
}

// Page requests one page of ledger rows, newest first.
type Page struct {
	// Limit is bounded by Config.MaxPageSize; zero means Config.DefaultPageSize.
	Limit int
	// After, when set, returns only rows strictly older than that position.
	After *Cursor
}

// LedgerPage is one page of results plus the cursor for the next one. Next is
// nil when the page was not full, which is the only reliable end-of-results
// signal for a table still being written to.
type LedgerPage struct {
	Rows []RequestLog
	Next *Cursor
}

// Spend summarises a credential's usage over a range (DESIGN 9.3).
type Spend struct {
	CredentialID     string
	Range            TimeRange
	Requests         int64
	Errors           int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	ReasoningTokens  int64
	TotalTokens      int64
	CostNano         int64
	MarginalCostNano int64
	SubscriptionNano int64
}

const requestLogCols = `l.ts, l.id, l.api_key_id, l.user_id, l.team_id, l.credential_id,
	l.provider_id, l.deployment_id, l.model_group, l.upstream_model, l.endpoint,
	l.status, l.error_class, l.prompt_tokens, l.completion_tokens, l.cached_tokens,
	l.reasoning_tokens, l.total_tokens, l.cost_nano, l.marginal_cost_nano,
	l.subscription_cost_nano, l.latency_ms, l.ttft_ms, l.queue_ms, l.capacity_wait_ms,
	l.upstream_ms, l.fallback_count, l.streamed, l.trace_id, l.session_id, l.node_id,
	l.batch_id, l.metadata, l.secret_id, l.util_multiplier_ppm, l.util_ppm,
	l.util_source, l.notional_nano, l.notional_known`

const requestLogInsertCols = `ts, id, api_key_id, user_id, team_id, credential_id,
	provider_id, deployment_id, model_group, upstream_model, endpoint,
	status, error_class, prompt_tokens, completion_tokens, cached_tokens,
	reasoning_tokens, total_tokens, cost_nano, marginal_cost_nano,
	subscription_cost_nano, latency_ms, ttft_ms, queue_ms, capacity_wait_ms,
	upstream_ms, fallback_count, streamed, trace_id, session_id, node_id,
	batch_id, metadata, secret_id, util_multiplier_ppm, util_ppm, util_source,
	notional_nano, notional_known`

// requestLogInsertArity is COUNTED from the column list rather than written
// beside it: a literal that has to be edited whenever a column is added is a
// bind-variable mismatch waiting for the next column, reported by the driver at
// run time on the ledger write path.
var requestLogInsertArity = strings.Count(requestLogInsertCols, ",") + 1

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// InsertRequestLogs appends ledger rows and their tags in one transaction.
//
// On a partitioned dialect a missing partition is recovered from in line: the
// partitions the batch needs are created and the insert is retried once. The
// scheduled pre-creation of EnsurePartitions is the primary mechanism; this is
// the belt to its braces, because the failure it prevents -- every insert
// failing at the first midnight after metering ships (DESIGN 9.5, R1-15) --
// is not one worth leaving to a timer.
func (s *Store) InsertRequestLogs(ctx context.Context, rows []RequestLog) error {
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		if rows[i].ID == "" {
			rows[i].ID = NewID()
		}
		if rows[i].TS.IsZero() {
			rows[i].TS = s.now()
		}
		if rows[i].Metadata == "" {
			rows[i].Metadata = "{}"
		}
		for name, v := range map[string]int64{
			"cost":         rows[i].CostNano,
			"marginal":     rows[i].MarginalCostNano,
			"subscription": rows[i].SubscriptionCostNano,
		} {
			if err := checkAmount(v, name); err != nil {
				return fmt.Errorf("request log %s: %w", rows[i].ID, err)
			}
		}
	}

	write := func() error {
		return s.withTx(ctx, func(tx *sql.Tx) error {
			for chunk := range chunks(rows, 200) {
				if err := s.insertLogChunk(ctx, tx, chunk); err != nil {
					return err
				}
			}
			return s.insertTags(ctx, tx, rows)
		})
	}

	err := write()
	if err == nil || !s.d.isMissingPartition(err) {
		return err
	}
	if _, perr := s.ensureDaysFor(ctx, rows); perr != nil {
		return errors.Join(err, perr)
	}
	return write()
}

func (s *Store) insertLogChunk(ctx context.Context, tx *sql.Tx, rows []RequestLog) error {
	var (
		b    strings.Builder
		args = make([]any, 0, len(rows)*requestLogInsertArity)
	)
	b.WriteString("INSERT INTO request_logs (" + requestLogInsertCols + ") VALUES ")
	for i := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(valuesTuple(requestLogInsertArity))
		r := &rows[i]
		args = append(args,
			Micros(r.TS), r.ID, nullStr(r.APIKeyID), nullStr(r.UserID), nullStr(r.TeamID),
			nullStr(r.CredentialID), nullStr(r.ProviderID), nullStr(r.DeploymentID),
			r.ModelGroup, r.UpstreamModel, r.Endpoint,
			r.Status, nullStr(r.ErrorClass), r.PromptTokens, r.CompletionTokens,
			r.CachedTokens, r.ReasoningTokens, r.TotalTokens,
			r.CostNano, r.MarginalCostNano, r.SubscriptionCostNano,
			r.LatencyMS, r.TTFTMS, r.QueueMS, r.CapacityWaitMS, r.UpstreamMS,
			r.FallbackCount, r.Streamed, nullStr(r.TraceID), nullStr(r.SessionID),
			nullStr(r.NodeID), nullStr(r.BatchID), r.Metadata, nullStr(r.SecretID),
			nullZeroInt(r.UtilMultiplierPPM), nullZeroInt(r.UtilPPM), nullStr(r.UtilSource),
			r.NotionalNano, r.NotionalKnown)
	}
	_, err := s.txExec(ctx, tx, b.String(), args...)
	return err
}

func (s *Store) insertTags(ctx context.Context, tx *sql.Tx, rows []RequestLog) error {
	type tagRow struct {
		ts  int64
		id  string
		tag string
	}
	var tags []tagRow
	for i := range rows {
		for _, t := range rows[i].Tags {
			if t == "" {
				continue
			}
			tags = append(tags, tagRow{Micros(rows[i].TS), rows[i].ID, t})
		}
	}
	if len(tags) == 0 {
		return nil
	}
	for chunk := range chunks(tags, 500) {
		var b strings.Builder
		b.WriteString("INSERT INTO request_log_tags (ts, request_id, tag) VALUES ")
		args := make([]any, 0, len(chunk)*3)
		for i, t := range chunk {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(valuesTuple(3))
			args = append(args, t.ts, t.id, t.tag)
		}
		if _, err := s.txExec(ctx, tx, b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// InsertRequestTraces appends sampled excerpts. Traces are droppable by design
// (DESIGN 12.1) but never silently: a caller that drops counts the drop.
func (s *Store) InsertRequestTraces(ctx context.Context, traces []RequestTrace) error {
	if len(traces) == 0 {
		return nil
	}
	write := func() error {
		return s.withTx(ctx, func(tx *sql.Tx) error {
			for chunk := range chunks(traces, 200) {
				var b strings.Builder
				b.WriteString("INSERT INTO request_traces (ts, request_id, trace_id, excerpt, excerpt_bytes, excerpt_mode, spans) VALUES ")
				args := make([]any, 0, len(chunk)*7)
				for i := range chunk {
					if i > 0 {
						b.WriteString(", ")
					}
					b.WriteString(valuesTuple(7))
					t := &chunk[i]
					mode := t.ExcerptMode
					if mode == "" {
						mode = "truncated"
					}
					spans := t.Spans
					if spans == "" {
						spans = "[]"
					}
					bytes := t.ExcerptBytes
					if bytes == 0 {
						bytes = int64(len(t.Excerpt))
					}
					args = append(args, Micros(t.TS), t.RequestID, nullStr(t.TraceID),
						t.Excerpt, bytes, mode, spans)
				}
				if _, err := s.txExec(ctx, tx, b.String(), args...); err != nil {
					return err
				}
			}
			return nil
		})
	}
	err := write()
	if err == nil || !s.d.isMissingPartition(err) {
		return err
	}
	days := make([]time.Time, 0, len(traces))
	for i := range traces {
		days = append(days, traces[i].TS)
	}
	if _, perr := s.EnsureDays(ctx, days); perr != nil {
		return errors.Join(err, perr)
	}
	return write()
}

func (s *Store) ensureDaysFor(ctx context.Context, rows []RequestLog) ([]string, error) {
	days := make([]time.Time, 0, len(rows))
	for i := range rows {
		days = append(days, rows[i].TS)
	}
	return s.EnsureDays(ctx, days)
}

// ---------------------------------------------------------------------------
// The query set (DESIGN 9.3). Six queries, six indexes, nothing speculative.
// ---------------------------------------------------------------------------

// ListRequestsByKey returns recent requests for an API key.
// Index: request_logs (api_key_id, ts DESC, id DESC).
func (s *Store) ListRequestsByKey(ctx context.Context, apiKeyID string, r TimeRange, p Page) (LedgerPage, error) {
	q, args, err := s.buildLedgerQuery(ledgerSpec{where: "l.api_key_id = ?", args: []any{apiKeyID}}, r, p)
	if err != nil {
		return LedgerPage{}, err
	}
	return s.runLedgerQuery(ctx, q, args, p)
}

// ListRequestsByTeam returns recent requests for a team.
// Index: request_logs (team_id, ts DESC, id DESC).
func (s *Store) ListRequestsByTeam(ctx context.Context, teamID string, r TimeRange, p Page) (LedgerPage, error) {
	q, args, err := s.buildLedgerQuery(ledgerSpec{where: "l.team_id = ?", args: []any{teamID}}, r, p)
	if err != nil {
		return LedgerPage{}, err
	}
	return s.runLedgerQuery(ctx, q, args, p)
}

// ListRequestsByTrace returns the requests carrying a trace id.
// Index: request_logs (trace_id, ts DESC, id DESC).
//
// The time range is required here as it is everywhere else. On a partitioned
// ledger a bare trace_id lookup would have to visit every partition's index;
// the range is what makes it a bounded lookup rather than a fan-out over the
// whole retention period.
func (s *Store) ListRequestsByTrace(ctx context.Context, traceID string, r TimeRange, p Page) (LedgerPage, error) {
	q, args, err := s.buildLedgerQuery(ledgerSpec{where: "l.trace_id = ?", args: []any{traceID}}, r, p)
	if err != nil {
		return LedgerPage{}, err
	}
	return s.runLedgerQuery(ctx, q, args, p)
}

// ListErrors returns failed requests over a range.
// Index: partial index on request_logs (ts DESC, id DESC) WHERE status >= 400.
//
// The predicate is written literally so that it matches the index predicate;
// parameterising the threshold would make the partial index unusable.
func (s *Store) ListErrors(ctx context.Context, r TimeRange, p Page) (LedgerPage, error) {
	q, args, err := s.buildLedgerQuery(ledgerSpec{where: "l.status >= 400"}, r, p)
	if err != nil {
		return LedgerPage{}, err
	}
	return s.runLedgerQuery(ctx, q, args, p)
}

// ListRequestsByTag returns requests carrying a tag, through the normalized
// request_log_tags table.
// Index: request_log_tags (tag, ts DESC, request_id DESC), then the ledger's
// own (ts, id) primary key for the join.
//
// This is the query DESIGN 9.3 singles out: an array column would make it a
// scan of every row in the range.
func (s *Store) ListRequestsByTag(ctx context.Context, tag string, r TimeRange, p Page) (LedgerPage, error) {
	q, args, err := s.buildLedgerQuery(ledgerSpec{
		join:  "JOIN request_log_tags t ON t.ts = l.ts AND t.request_id = l.id",
		where: "t.tag = ?",
		args:  []any{tag},
		// Bound the tag side of the join too, so both partitioned relations
		// prune instead of only the one the range was written against.
		extraRangeCol: "t.ts",
	}, r, p)
	if err != nil {
		return LedgerPage{}, err
	}
	return s.runLedgerQuery(ctx, q, args, p)
}

// SpendByCredential aggregates a credential's usage over a range.
// Index: request_logs (credential_id, ts DESC, id DESC).
func (s *Store) SpendByCredential(ctx context.Context, credentialID string, r TimeRange) (Spend, error) {
	if err := r.validate(s.cfg.MaxTimeRange); err != nil {
		return Spend{}, err
	}
	const q = `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN l.status >= 400 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(l.prompt_tokens), 0), COALESCE(SUM(l.completion_tokens), 0),
		       COALESCE(SUM(l.cached_tokens), 0), COALESCE(SUM(l.reasoning_tokens), 0),
		       COALESCE(SUM(l.total_tokens), 0),
		       COALESCE(SUM(l.cost_nano), 0), COALESCE(SUM(l.marginal_cost_nano), 0),
		       COALESCE(SUM(l.subscription_cost_nano), 0)
		  FROM request_logs l
		 WHERE l.credential_id = ? AND l.ts >= ? AND l.ts < ?`
	out := Spend{CredentialID: credentialID, Range: r}
	err := s.queryRow(ctx, q, credentialID, Micros(r.Start), Micros(r.End)).Scan(
		&out.Requests, &out.Errors,
		&out.PromptTokens, &out.CompletionTokens, &out.CachedTokens,
		&out.ReasoningTokens, &out.TotalTokens,
		&out.CostNano, &out.MarginalCostNano, &out.SubscriptionNano)
	if err != nil {
		return Spend{}, err
	}
	return out, nil
}

// GetRequestTrace fetches the excerpt for a ledger row already in hand.
func (s *Store) GetRequestTrace(ctx context.Context, ts time.Time, requestID string) (RequestTrace, error) {
	var (
		t       RequestTrace
		traceID sql.NullString
		us      int64
	)
	err := s.queryRow(ctx, `
		SELECT ts, request_id, trace_id, excerpt, excerpt_bytes, excerpt_mode, spans
		  FROM request_traces WHERE ts = ? AND request_id = ?`,
		Micros(ts), requestID).
		Scan(&us, &t.RequestID, &traceID, &t.Excerpt, &t.ExcerptBytes, &t.ExcerptMode, &t.Spans)
	if errors.Is(err, sql.ErrNoRows) {
		return RequestTrace{}, ErrNotFound
	}
	if err != nil {
		return RequestTrace{}, err
	}
	t.TS = TimeAt(us)
	t.TraceID = str(traceID)
	return t, nil
}

// ---------------------------------------------------------------------------
// Query construction
// ---------------------------------------------------------------------------

type ledgerSpec struct {
	join          string
	where         string
	args          []any
	extraRangeCol string
	// orderCol is the (ts, id) pair the ORDER BY and cursor use. Empty means
	// the ledger's own columns.
	orderTS string
	orderID string
}

// buildLedgerQuery is separate from execution so that tests can assert on the
// SQL and run EXPLAIN against exactly what the store runs.
func (s *Store) buildLedgerQuery(spec ledgerSpec, r TimeRange, p Page) (string, []any, error) {
	if err := r.validate(s.cfg.MaxTimeRange); err != nil {
		return "", nil, err
	}
	limit, err := s.pageLimit(p)
	if err != nil {
		return "", nil, err
	}

	tsCol, idCol := spec.orderTS, spec.orderID
	if tsCol == "" {
		tsCol, idCol = "l.ts", "l.id"
	}
	if spec.join != "" && spec.orderTS == "" {
		// Order on the driving relation so the tag index supplies the order
		// and the join is a lookup, not a sort.
		tsCol, idCol = "t.ts", "t.request_id"
	}

	var b strings.Builder
	args := make([]any, 0, len(spec.args)+5)

	b.WriteString("SELECT " + requestLogCols + "\n  FROM request_logs l\n")
	if spec.join != "" {
		b.WriteString("  " + spec.join + "\n")
	}
	b.WriteString(" WHERE ")
	if spec.where != "" {
		b.WriteString(spec.where)
		b.WriteString(" AND ")
		args = append(args, spec.args...)
	}
	b.WriteString("l.ts >= ? AND l.ts < ?")
	args = append(args, Micros(r.Start), Micros(r.End))
	if spec.extraRangeCol != "" {
		b.WriteString(" AND " + spec.extraRangeCol + " >= ? AND " + spec.extraRangeCol + " < ?")
		args = append(args, Micros(r.Start), Micros(r.End))
	}
	if p.After != nil {
		b.WriteString(" AND (" + tsCol + ", " + idCol + ") < (?, ?)")
		args = append(args, Micros(p.After.TS), p.After.ID)
	}
	b.WriteString("\n ORDER BY " + tsCol + " DESC, " + idCol + " DESC\n LIMIT ?")
	args = append(args, limit)
	return b.String(), args, nil
}

func (s *Store) pageLimit(p Page) (int, error) {
	switch {
	case p.Limit < 0:
		return 0, fmt.Errorf("store: negative page limit %d", p.Limit)
	case p.Limit == 0:
		return s.cfg.DefaultPageSize, nil
	case p.Limit > s.cfg.MaxPageSize:
		return 0, fmt.Errorf("store: page limit %d exceeds maximum %d", p.Limit, s.cfg.MaxPageSize)
	default:
		return p.Limit, nil
	}
}

func (s *Store) runLedgerQuery(ctx context.Context, q string, args []any, p Page) (LedgerPage, error) {
	limit, err := s.pageLimit(p)
	if err != nil {
		return LedgerPage{}, err
	}
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return LedgerPage{}, err
	}
	defer rows.Close()

	out := LedgerPage{}
	for rows.Next() {
		r, err := scanRequestLog(rows)
		if err != nil {
			return LedgerPage{}, err
		}
		out.Rows = append(out.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return LedgerPage{}, err
	}
	if len(out.Rows) == limit && limit > 0 {
		last := out.Rows[len(out.Rows)-1]
		out.Next = &Cursor{TS: last.TS, ID: last.ID}
	}
	return out, nil
}

func scanRequestLog(rows *sql.Rows) (RequestLog, error) {
	var (
		r                                             RequestLog
		us                                            int64
		keyID, userID, teamID, credID, provID, deplID sql.NullString
		errClass, traceID, sessionID, nodeID, batchID sql.NullString
		secretID, utilSource                          sql.NullString
		utilMul, utilPPM                              sql.NullInt64
	)
	err := rows.Scan(&us, &r.ID, &keyID, &userID, &teamID, &credID, &provID, &deplID,
		&r.ModelGroup, &r.UpstreamModel, &r.Endpoint, &r.Status, &errClass,
		&r.PromptTokens, &r.CompletionTokens, &r.CachedTokens, &r.ReasoningTokens,
		&r.TotalTokens, &r.CostNano, &r.MarginalCostNano, &r.SubscriptionCostNano,
		&r.LatencyMS, &r.TTFTMS, &r.QueueMS, &r.CapacityWaitMS, &r.UpstreamMS,
		&r.FallbackCount, &r.Streamed, &traceID, &sessionID, &nodeID, &batchID, &r.Metadata,
		&secretID, &utilMul, &utilPPM, &utilSource, &r.NotionalNano, &r.NotionalKnown)
	if err != nil {
		return RequestLog{}, err
	}
	r.TS = TimeAt(us)
	r.APIKeyID = str(keyID)
	r.UserID = str(userID)
	r.TeamID = str(teamID)
	r.CredentialID = str(credID)
	r.ProviderID = str(provID)
	r.DeploymentID = str(deplID)
	r.ErrorClass = str(errClass)
	r.TraceID = str(traceID)
	r.SessionID = str(sessionID)
	r.NodeID = str(nodeID)
	r.BatchID = str(batchID)
	r.SecretID = str(secretID)
	r.UtilMultiplierPPM = utilMul.Int64
	r.UtilPPM = utilPPM.Int64
	r.UtilSource = str(utilSource)
	return r, nil
}

// valuesTuple renders "(?, ?, ...)" with n placeholders.
func valuesTuple(n int) string {
	var b strings.Builder
	b.Grow(2*n + 2)
	b.WriteByte('(')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('?')
	}
	b.WriteByte(')')
	return b.String()
}

// chunks yields fixed-size slices of s. Multi-row inserts are chunked because
// SQLite caps bound parameters per statement and a metering flush can be large.
func chunks[T any](s []T, n int) func(func([]T) bool) {
	return func(yield func([]T) bool) {
		for i := 0; i < len(s); i += n {
			j := min(i+n, len(s))
			if !yield(s[i:j]) {
				return
			}
		}
	}
}

// uniqueDays reduces a set of timestamps to their distinct UTC days, sorted.
func uniqueDays(ts []time.Time) []time.Time {
	seen := map[int64]time.Time{}
	for _, t := range ts {
		d := dayFloor(t)
		seen[d.Unix()] = d
	}
	out := make([]time.Time, 0, len(seen))
	for _, d := range seen {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}
