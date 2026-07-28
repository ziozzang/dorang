package shadow

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Record is one line of the JSONL report.
//
// It is written for a comparison that found a difference and for one that could
// not decide a dimension — and for nothing else. That asymmetry is what makes
// the completion criterion work: an empty file means every comparison that ran
// decided every dimension and found nothing, which is a statement worth basing
// a production cutover on. A file that also contained a line per clean
// comparison would be a log, and nobody reads a log to decide a cutover.
//
// The counts of clean comparisons are in [Stats], where a number belongs.
type Record struct {
	Time      string `json:"time"`
	RequestID string `json:"request_id"`
	Mode      string `json:"mode"`
	Route     string `json:"route,omitempty"`
	Method    string `json:"method,omitempty"`
	Path      string `json:"path,omitempty"`
	Model     string `json:"model,omitempty"`
	Stream    bool   `json:"stream,omitempty"`

	DorangStatus    int `json:"dorang_status"`
	ReferenceStatus int `json:"reference_status"`

	// ReferenceError is the transport failure, when the reference could not be
	// reached at all. A record with it set and no diffs is not a clean
	// comparison; it is a comparison that did not happen.
	ReferenceError string `json:"reference_error,omitempty"`

	Diffs        []Diff         `json:"diffs,omitempty"`
	Inconclusive []Inconclusive `json:"inconclusive,omitempty"`

	DorangMS    int64 `json:"dorang_ms"`
	ReferenceMS int64 `json:"reference_ms"`
	// CostNanoUSD is what this shadow call was charged against the daily
	// ceiling. It is the reference gateway's spend, not dorang's — see the
	// package comment on accounting.
	CostNanoUSD int64 `json:"cost_nano_usd"`
	// CostEstimated says the charge is the fallback estimate rather than a
	// price. It is on the record and not only in a counter so that a reader
	// working out why the ceiling tripped can see which rows were guesses.
	CostEstimated bool `json:"cost_estimated,omitempty"`
}

// reporter writes the JSONL report under a byte cap.
//
// The cap is §9.6 rule 1 applied to a file: a report that grows without bound
// converts a diff nobody is watching into a full disk. Reaching it drops
// records and counts them, and the count is in [Stats] and on the metrics
// endpoint — the drop is never silent, because a truncated report read as an
// empty one is the worst outcome this package has.
type reporter struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer
	max    int64

	written atomic.Int64
	dropped atomic.Int64
	errs    atomic.Int64
}

func newReporter(path string, w io.Writer, max int64) (*reporter, error) {
	r := &reporter{w: w, max: max}
	if w != nil || path == "" {
		return r, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil {
		r.written.Store(st.Size())
	}
	r.w, r.closer = f, f
	return r, nil
}

// write appends one record.
func (r *reporter) write(rec *Record) {
	if r == nil || r.w == nil {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		r.errs.Add(1)
		return
	}
	b = append(b, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.max > 0 && r.written.Load()+int64(len(b)) > r.max {
		r.dropped.Add(1)
		return
	}
	n, err := r.w.Write(b)
	r.written.Add(int64(n))
	if err != nil {
		r.errs.Add(1)
	}
}

func (r *reporter) close() error {
	if r == nil || r.closer == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.closer.Close()
	r.closer = nil
	r.w = nil
	return err
}

// rfc3339 renders a timestamp for the report. The report is read by people and
// by jq, so it is a string rather than an epoch.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
