package admin

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"
)

// GET|POST /health/history
//
// The one shape-compatible path with no table behind it. §9.2 gives
// credential_state, which is a *current* state — health, unavailable_until,
// consecutive_failures — and keeps no transitions, so a process that has been
// restarted knows nothing about yesterday. That is recorded as design feedback
// rather than papered over: the endpoint answers from whatever recorder the
// process has, and answers 501 with a code when it has none.
//
// What it must never do is return an empty list when nothing is recording,
// because an empty history reads as "nothing ever failed", which is the most
// reassuring possible answer and the least likely to be checked.
func (c *call) healthHistory() error {
	if c.a.cfg.Health == nil {
		f := dependencyOff("health history recorder", "/health/history")
		f.Detail = map[string]any{
			"dependency": "health history recorder",
			"reason": "DESIGN §9.2 stores credential_state, which is a current state and not a " +
				"transition log; without a recorder this process cannot answer for the past, and " +
				"an empty list would read as 'nothing ever failed'",
		}
		return f
	}
	var body struct {
		rangeSpec
		Subject string `json:"subject"`
		Limit   int    `json:"limit"`
	}
	if err := decodeOptionalBody(c.w, c.r, &body); err != nil {
		return err
	}
	// A history query is a ledger query and takes the same bounded range. §9.3
	// makes the point about trace-id lookup specifically: exempting one read
	// from the rule contradicts the rule.
	rng, err := c.resolveRange(body.rangeSpec)
	if err != nil {
		return err
	}
	limit, err := c.pageLimit(body.Limit)
	if err != nil {
		return err
	}
	events, err := c.a.cfg.Health.History(c.ctx(), HealthQuery{
		Range:   rng,
		Subject: firstNonEmpty(body.Subject, queryString(c.r, "subject", "credential_id", "deployment_id")),
		Limit:   limit,
	})
	if err != nil {
		return c.ledgerError(err)
	}
	type eventView struct {
		Timestamp   Stamp  `json:"timestamp"`
		SubjectKind string `json:"subject_kind"`
		Subject     string `json:"subject"`
		State       string `json:"state"`
		Reason      string `json:"reason,omitempty"`
		Status      int    `json:"status,omitempty"`
		LatencyMS   int64  `json:"latency_ms,omitempty"`
	}
	out := make([]eventView, 0, len(events))
	for _, e := range events {
		out = append(out, eventView{
			Timestamp:   Stamp(e.TS),
			SubjectKind: e.SubjectKind,
			Subject:     e.Subject,
			State:       e.State,
			Reason:      e.Reason,
			Status:      e.Status,
			LatencyMS:   e.LatencyMS,
		})
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"history":    out,
		"count":      len(out),
		"start_date": Stamp(rng.Start),
		"end_date":   Stamp(rng.End),
		"has_more":   len(out) >= limit,
	})
	return nil
}

// ---------------------------------------------------------------------------
// An in-process recorder
// ---------------------------------------------------------------------------

// DefaultHealthHistorySize is the number of transitions [MemoryHealthHistory]
// keeps. Transitions are rare — a healthy fleet produces none — so a few
// thousand is days of history at a few hundred bytes each.
const DefaultHealthHistorySize = 4096

// MemoryHealthHistory is a bounded in-process recorder for /health/history.
//
// It exists because §9.2 has no table for transitions and inventing one is a
// schema change this package must not make. What it can honestly offer is the
// history since this process started, which is what an operator debugging a
// live incident is asking about anyway — and it says so, rather than implying
// it knows more.
//
// It is a ring: the oldest event is dropped when it is full, so a flapping
// deployment cannot grow the process's memory. Safe for concurrent use.
type MemoryHealthHistory struct {
	mu     sync.RWMutex
	events []HealthEvent
	next   int
	full   bool
	// since is when recording began. A query whose range starts before it is
	// answered, and the answer is honestly incomplete rather than silently so.
	since time.Time
}

// NewMemoryHealthHistory returns a recorder holding at most size events. A
// size of zero means [DefaultHealthHistorySize].
func NewMemoryHealthHistory(size int, now time.Time) *MemoryHealthHistory {
	if size <= 0 {
		size = DefaultHealthHistorySize
	}
	return &MemoryHealthHistory{events: make([]HealthEvent, size), since: now}
}

// Record appends one transition.
func (m *MemoryHealthHistory) Record(e HealthEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events[m.next] = e
	m.next = (m.next + 1) % len(m.events)
	if m.next == 0 {
		m.full = true
	}
}

// Since reports when this recorder started, so a caller can tell "no failures"
// from "no memory of failures".
func (m *MemoryHealthHistory) Since() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.since
}

// History implements [HealthHistory]. Newest first, like every other ledger
// read in this package.
func (m *MemoryHealthHistory) History(_ context.Context, q HealthQuery) ([]HealthEvent, error) {
	if q.Range.Start.IsZero() || q.Range.End.IsZero() || !q.Range.End.After(q.Range.Start) {
		return nil, ErrUnboundedRange
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	n := m.next
	if m.full {
		n = len(m.events)
	}
	out := make([]HealthEvent, 0, min(limit, n))
	for i := 0; i < n; i++ {
		var e HealthEvent
		if m.full {
			e = m.events[(m.next+i)%len(m.events)]
		} else {
			e = m.events[i]
		}
		if e.TS.Before(q.Range.Start) || !e.TS.Before(q.Range.End) {
			continue
		}
		if q.Subject != "" && e.Subject != q.Subject {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
