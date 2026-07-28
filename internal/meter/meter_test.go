package meter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// manualConfig turns off every background loop so a test drives the meter
// explicitly. Timing-dependent tests are worse than useless: they pass on a
// fast machine and flake on a loaded one.
func manualConfig(sink Sink) Config {
	return Config{
		Sink:          sink,
		FlushInterval: -1,
		DrainInterval: -1,
		ShipInterval:  -1,
		CloseTimeout:  200 * time.Millisecond,
	}
}

func sampleEvent(i int) Event {
	return Event{
		Time:         time.Unix(1700000000, 0).UTC(),
		APIKeyID:     fmt.Sprintf("key-%d", i%4),
		TeamID:       "team-a",
		ModelGroup:   fmt.Sprintf("model-%d", (i/4)%2),
		Provider:     "openai",
		CredentialID: "cred-1",
		Endpoint:     "/v1/chat/completions",
		Status:       200,
		Tokens:       Tokens{Input: 10, Output: 20, CacheRead: 3, CacheWrite: 2, Reasoning: 1},
		CostNano:     1234,
		Latency:      7 * time.Millisecond,
		TTFT:         2 * time.Millisecond,
	}
}

// ------------------------------------------------------------ numeric path

func TestNumericAccuracyUnderConcurrency(t *testing.T) {
	const (
		goroutines = 16
		perG       = 5000
	)
	sink := NewMemSink()
	m := New(manualConfig(sink))
	defer m.Close()

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				m.Record(sampleEvent(g*perG + i))
			}
		}(g)
	}
	wg.Wait()

	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	const total = goroutines * perG
	got := sink.Totals()
	want := Bucket{
		Requests:   total,
		Errors:     0,
		Tokens:     Tokens{Input: 10 * total, Output: 20 * total, CacheRead: 3 * total, CacheWrite: 2 * total, Reasoning: 1 * total},
		CostNano:   1234 * total,
		LatencySum: 7 * time.Millisecond * total,
		TTFTSum:    2 * time.Millisecond * total,
		TTFTCount:  total,
	}
	if got.Requests != want.Requests {
		t.Errorf("requests = %d, want %d", got.Requests, want.Requests)
	}
	if got.Tokens != want.Tokens {
		t.Errorf("tokens = %+v, want %+v", got.Tokens, want.Tokens)
	}
	if got.CostNano != want.CostNano {
		t.Errorf("cost = %d, want %d", got.CostNano, want.CostNano)
	}
	if got.LatencySum != want.LatencySum || got.TTFTSum != want.TTFTSum || got.TTFTCount != want.TTFTCount {
		t.Errorf("latency/ttft = %v/%v/%d, want %v/%v/%d",
			got.LatencySum, got.TTFTSum, got.TTFTCount, want.LatencySum, want.TTFTSum, want.TTFTCount)
	}
	if st := m.Stats(); st.Recorded != total {
		t.Errorf("Stats.Recorded = %d, want %d", st.Recorded, total)
	}

	// Eight distinct keys (4 api keys x 2 model groups), one hour, but a key
	// may live in more than one shard, so the bucket count is the number of
	// distinct keys after merge -- which must be exactly eight.
	keys := map[Key]bool{}
	for _, b := range sink.Buckets() {
		keys[b.Key] = true
	}
	if len(keys) != 8 {
		t.Errorf("distinct merged keys = %d, want 8", len(keys))
	}
}

func TestNumericCountsErrorsAndStatusClasses(t *testing.T) {
	sink := NewMemSink()
	m := New(manualConfig(sink))
	defer m.Close()

	statuses := []int{200, 200, 301, 404, 500, 503}
	for _, s := range statuses {
		ev := sampleEvent(0)
		ev.Status = s
		m.Record(ev)
	}
	ev := sampleEvent(0)
	ev.Status = 0
	ev.Canceled = true
	m.Record(ev)

	// A 2xx that the gateway nonetheless considers failed.
	ev = sampleEvent(0)
	ev.Status = 200
	ev.Error = true
	m.Record(ev)

	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	byClass := map[StatusClass]Bucket{}
	for _, b := range sink.Buckets() {
		acc := byClass[b.StatusClass]
		acc.addBucket(&b)
		byClass[b.StatusClass] = acc
	}
	if got := byClass[Status2xx].Requests; got != 3 {
		t.Errorf("2xx requests = %d, want 3", got)
	}
	if got := byClass[Status2xx].Errors; got != 1 {
		t.Errorf("2xx errors = %d, want 1 (the forced Error event)", got)
	}
	if got := byClass[Status4xx].Errors; got != 1 {
		t.Errorf("4xx errors = %d, want 1", got)
	}
	if got := byClass[Status5xx].Errors; got != 2 {
		t.Errorf("5xx errors = %d, want 2", got)
	}
	if got := byClass[StatusCanceled].Requests; got != 1 {
		t.Errorf("canceled requests = %d, want 1", got)
	}
	if got := byClass[StatusCanceled].Errors; got != 0 {
		t.Errorf("canceled errors = %d, want 0 (a client hang-up is not a gateway error)", got)
	}
}

func TestHourBoundaryAttributedExactly(t *testing.T) {
	sink := NewMemSink()
	m := New(manualConfig(sink))
	defer m.Close()

	// One flush window straddling an hour boundary. Deriving the hour at
	// flush time would put all four events in one bucket; carrying it in the
	// key puts two in each.
	base := time.Date(2026, 7, 28, 10, 59, 59, 0, time.UTC)
	for _, off := range []time.Duration{0, 500 * time.Millisecond, time.Second, 2 * time.Second} {
		ev := sampleEvent(0)
		ev.Time = base.Add(off)
		m.Record(ev)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	byHour := map[time.Time]int64{}
	for _, b := range sink.Buckets() {
		byHour[b.HourStart] += b.Requests
	}
	h10 := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	h11 := time.Date(2026, 7, 28, 11, 0, 0, 0, time.UTC)
	if byHour[h10] != 2 || byHour[h11] != 2 {
		t.Errorf("hour split = %v, want 2 in %v and 2 in %v", byHour, h10, h11)
	}
}

func TestCardinalityCapFoldsIntoOverflow(t *testing.T) {
	sink := NewMemSink()
	cfg := manualConfig(sink)
	cfg.Shards = 2
	cfg.MaxKeysPerShard = 4
	m := New(cfg)
	defer m.Close()

	const distinct = 100
	for i := 0; i < distinct; i++ {
		ev := sampleEvent(0)
		ev.APIKeyID = fmt.Sprintf("key-%04d", i)
		m.Record(ev)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var total, overflowed int64
	sawOverflow := false
	for _, b := range sink.Buckets() {
		total += b.Requests
		if b.IsOverflow() {
			sawOverflow = true
			overflowed += b.Requests
		}
	}
	// The cap costs resolution, never counts.
	if total != distinct {
		t.Errorf("total requests after folding = %d, want %d -- the fold must not lose counts", total, distinct)
	}
	if !sawOverflow {
		t.Fatalf("no %s bucket; the cap folded silently", OverflowSentinel)
	}
	st := m.Stats()
	if st.OverflowFolds == 0 {
		t.Error("Stats.OverflowFolds = 0; the fold must be counted, not inferred")
	}
	if st.OverflowFolds != overflowed {
		t.Errorf("Stats.OverflowFolds = %d but %d requests landed in the overflow bucket", st.OverflowFolds, overflowed)
	}
	// Every shard admits at most MaxKeysPerShard real keys.
	if overflowed < distinct-int64(cfg.Shards*cfg.MaxKeysPerShard) {
		t.Errorf("overflowed = %d, want at least %d", overflowed, distinct-int64(cfg.Shards*cfg.MaxKeysPerShard))
	}
}

func TestRollupsCarryOverWhenSinkRefuses(t *testing.T) {
	sink := NewMemSink()
	boom := errors.New("store is down")
	sink.SetErrors(boom, nil)
	m := New(manualConfig(sink))
	defer m.Close()

	for i := 0; i < 50; i++ {
		m.Record(sampleEvent(i))
	}
	if err := m.Flush(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Flush error = %v, want %v", err, boom)
	}
	if st := m.Stats(); st.PendingBuckets == 0 {
		t.Fatal("nothing carried over; a refused rollup write lost data")
	}

	// More traffic while the store is still down.
	for i := 0; i < 50; i++ {
		m.Record(sampleEvent(i))
	}
	if err := m.Flush(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("second Flush error = %v, want %v", err, boom)
	}

	sink.SetErrors(nil, nil)
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush after recovery: %v", err)
	}
	if got := sink.Totals().Requests; got != 100 {
		t.Errorf("requests after recovery = %d, want 100 -- carry-over must be exact", got)
	}
	if st := m.Stats(); st.PendingBuckets != 0 {
		t.Errorf("PendingBuckets = %d after a successful flush, want 0", st.PendingBuckets)
	}
}

func TestPendingCarryOverIsBounded(t *testing.T) {
	sink := NewMemSink()
	sink.SetErrors(errors.New("down"), nil)
	cfg := manualConfig(sink)
	cfg.MaxPendingBuckets = 16
	cfg.MaxKeysPerShard = 4096
	m := New(cfg)
	defer m.Close()

	const distinct = 400
	for i := 0; i < distinct; i++ {
		ev := sampleEvent(0)
		ev.APIKeyID = fmt.Sprintf("key-%04d", i)
		m.Record(ev)
	}
	_ = m.Flush(context.Background())

	st := m.Stats()
	if st.PendingBuckets > int64(cfg.MaxPendingBuckets) {
		t.Errorf("PendingBuckets = %d, want <= %d -- a store outage must not become an OOM",
			st.PendingBuckets, cfg.MaxPendingBuckets)
	}
	if st.PendingFolds == 0 {
		t.Error("PendingFolds = 0; the carry-over cap folded silently")
	}

	sink.SetErrors(nil, nil)
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush after recovery: %v", err)
	}
	if got := sink.Totals().Requests; got != distinct {
		t.Errorf("requests = %d, want %d -- folding loses resolution, never counts", got, distinct)
	}
}

// ------------------------------------------------- the property that matters

// TestNumericSurvivesStalledSink is the test the whole two-path design exists
// to pass: with the store wedged and far more traffic than any buffer can
// hold, every numeric count survives and only trace payloads are lost.
func TestNumericSurvivesStalledSink(t *testing.T) {
	sink := NewMemSink()
	cfg := manualConfig(sink)
	cfg.TraceQueueSize = 64
	cfg.SpoolDir = t.TempDir()
	cfg.SpoolMaxBytes = 16 << 10
	cfg.SpoolSegmentBytes = 4 << 10
	m := New(cfg)
	defer m.Close()

	// Wedge the store completely -- rollups as well as traces.
	sink.Block(true)

	const n = 20000
	var wg sync.WaitGroup

	// Two concurrent background workers, on their own WaitGroup because they
	// outlive the recorders:
	//
	//   - a flusher, so the meter is repeatedly mid-flush while wedged;
	//   - a drainer, which touches only the ring and the spool. It is the
	//     proof that a wedged store costs disk rather than data: it keeps
	//     making progress with every sink call blocked.
	var bg sync.WaitGroup
	stopFlush := make(chan struct{})
	bg.Add(2)
	go func() {
		defer bg.Done()
		for {
			select {
			case <-stopFlush:
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
			_ = m.Flush(ctx)
			cancel()
		}
	}()
	go func() {
		defer bg.Done()
		for {
			select {
			case <-stopFlush:
				return
			default:
			}
			_ = m.drain()
		}
	}()

	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < n/8; i++ {
				ev := sampleEvent(i)
				ev.Trace.RequestID = fmt.Sprintf("req-%d-%d", g, i)
				ev.Trace.Excerpt = "a message excerpt that costs bytes"
				m.Record(ev)
			}
		}(g)
	}
	wg.Wait()
	close(stopFlush)
	bg.Wait()

	mid := m.Stats()
	if mid.Recorded != n {
		t.Fatalf("Stats.Recorded = %d, want %d", mid.Recorded, n)
	}
	if mid.TracesSpooled == 0 {
		t.Error("nothing reached the spool while every sink call was blocked; " +
			"the drain path is not independent of the ship path")
	}
	if mid.TracesDropped == 0 {
		t.Fatalf("no trace was dropped with the store wedged and %d events pushed through a %d-slot queue "+
			"and a %d-byte spool; the test is not exercising back-pressure", n, cfg.TraceQueueSize, cfg.SpoolMaxBytes)
	}
	if deg, reason := m.Degraded(); !deg {
		t.Errorf("Degraded() = false after %d drops; a drop must never be silent", mid.TracesDropped)
	} else {
		t.Logf("degraded as expected: %s", reason)
	}

	sink.Unblock()
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush after recovery: %v", err)
	}

	got := sink.Totals()
	if got.Requests != n {
		t.Errorf("requests = %d, want %d -- the numeric path lost %d counts", got.Requests, n, n-got.Requests)
	}
	if want := int64(10 * n); got.Tokens.Input != want {
		t.Errorf("input tokens = %d, want %d", got.Tokens.Input, want)
	}
	if want := int64(1234 * n); got.CostNano != want {
		t.Errorf("cost = %d, want %d", got.CostNano, want)
	}

	final := m.Stats()
	t.Logf("traces: recorded=%d spooled=%d flushed=%d droppedQueue=%d droppedSpool=%d",
		final.TracesRecorded, final.TracesSpooled, final.TracesFlushed,
		final.TracesDroppedQueue, final.TracesDroppedSpool)
	if final.TracesFlushed == 0 {
		t.Error("no trace reached the sink after recovery; the spool did not replay")
	}
	// Traces are droppable but never invented: what was accepted is what was
	// delivered plus what is still queued.
	if final.TracesFlushed > final.TracesRecorded {
		t.Errorf("flushed %d traces but only recorded %d", final.TracesFlushed, final.TracesRecorded)
	}
}

// -------------------------------------------------------------- trace path

func TestTraceRoundTripThroughSpool(t *testing.T) {
	sink := NewMemSink()
	cfg := manualConfig(sink)
	cfg.SpoolDir = t.TempDir()
	m := New(cfg)
	defer m.Close()

	ev := sampleEvent(0)
	ev.Status = 503
	ev.Trace = TraceInfo{
		RequestID:       "req-abc",
		TraceID:         "trace-abc",
		SpanID:          "span-1",
		ParentSpanID:    "span-0",
		UpstreamModel:   "gpt-4o-2024-08-06",
		Excerpt:         "hello, world",
		QueueWait:       time.Millisecond,
		RouteTime:       2 * time.Millisecond,
		CapacityWait:    3 * time.Millisecond,
		UpstreamConnect: 4 * time.Millisecond,
		Retries:         2,
		FallbackReason:  "upstream_5xx",
		ErrorMessage:    "service unavailable",
	}
	m.Record(ev)
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	traces := sink.Traces()
	if len(traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(traces))
	}
	tr := traces[0]
	if tr.RequestID != "req-abc" || tr.TraceID != "trace-abc" || tr.SpanID != "span-1" || tr.ParentSpanID != "span-0" {
		t.Errorf("ids not preserved: %+v", tr)
	}
	if tr.Excerpt != "hello, world" {
		t.Errorf("excerpt = %q, want %q", tr.Excerpt, "hello, world")
	}
	if tr.Status != 503 || tr.Retries != 2 || tr.FallbackReason != "upstream_5xx" || tr.ErrorMessage != "service unavailable" {
		t.Errorf("fields not preserved: %+v", tr)
	}
	if tr.QueueWait != time.Millisecond || tr.RouteTime != 2*time.Millisecond ||
		tr.CapacityWait != 3*time.Millisecond || tr.UpstreamConnect != 4*time.Millisecond {
		t.Errorf("latency breakdown not preserved: %+v", tr)
	}
	if tr.Tokens != ev.Tokens || tr.CostNano != ev.CostNano {
		t.Errorf("numbers not preserved: %+v", tr)
	}
	if !tr.Time.Equal(ev.Time) {
		t.Errorf("time = %v, want %v", tr.Time, ev.Time)
	}
}

func TestEventWithoutRequestIDIsNumericOnly(t *testing.T) {
	sink := NewMemSink()
	m := New(manualConfig(sink))
	defer m.Close()

	ev := sampleEvent(0)
	ev.Trace.Excerpt = "this has no request id and so cannot be joined to anything"
	m.Record(ev)
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := sink.Totals().Requests; got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
	if got := len(sink.Traces()); got != 0 {
		t.Errorf("traces = %d, want 0", got)
	}
	if st := m.Stats(); st.TracesDropped != 0 {
		t.Errorf("TracesDropped = %d; declining to trace an unidentified request is not a drop", st.TracesDropped)
	}
}

func TestSpoolFullDropsVisiblyAndDegrades(t *testing.T) {
	sink := NewMemSink()
	sink.SetErrors(nil, errors.New("ledger is down"))
	cfg := manualConfig(sink)
	cfg.SpoolDir = t.TempDir()
	cfg.SpoolMaxBytes = 8 << 10
	cfg.SpoolSegmentBytes = 2 << 10
	cfg.TraceQueueSize = 32
	m := New(cfg)
	defer m.Close()

	for i := 0; i < 4000; i++ {
		ev := sampleEvent(i)
		ev.Trace.RequestID = fmt.Sprintf("req-%05d", i)
		ev.Trace.Excerpt = strings.Repeat("x", 200)
		m.Record(ev)
		if i%16 == 0 {
			_ = m.drain()
		}
	}
	_ = m.drain()

	st := m.Stats()
	if st.TracesDroppedSpool == 0 && st.TracesDroppedQueue == 0 {
		t.Fatalf("no drop recorded with an %d-byte spool and 4000 traces: %+v", cfg.SpoolMaxBytes, st)
	}
	if st.TracesDroppedSpool == 0 {
		t.Errorf("TracesDroppedSpool = 0; the spool cap must be attributed, not lumped into the queue")
	}
	deg, reason := m.Degraded()
	if !deg {
		t.Fatal("Degraded() = false with a full spool")
	}
	if reason != ReasonSpoolFull && reason != ReasonQueueFull {
		t.Errorf("Reason = %v, want spool_full or trace_queue_full", reason)
	}
	if st.SpoolBytes > cfg.SpoolMaxBytes {
		t.Errorf("SpoolBytes = %d, want <= %d -- the cap is not a suggestion", st.SpoolBytes, cfg.SpoolMaxBytes)
	}
	t.Logf("%+v", st)
}

func TestSpoolReplaysUndrainedSegmentsAfterRestart(t *testing.T) {
	dir := t.TempDir()
	const n = 500

	down := NewMemSink()
	down.SetErrors(nil, errors.New("ledger is down"))
	cfg := manualConfig(down)
	cfg.SpoolDir = dir
	cfg.SpoolSegmentBytes = 4 << 10 // force several segments
	cfg.SpoolMaxBytes = 4 << 20
	m1 := New(cfg)

	for i := 0; i < n; i++ {
		ev := sampleEvent(i)
		ev.Trace.RequestID = fmt.Sprintf("req-%05d", i)
		ev.Trace.Excerpt = fmt.Sprintf("excerpt for request %d", i)
		m1.Record(ev)
	}
	_ = m1.Flush(context.Background()) // drains to disk, ship fails
	st1 := m1.Stats()
	if st1.TracesSpooled != n {
		t.Fatalf("spooled = %d, want %d", st1.TracesSpooled, n)
	}
	if st1.SpoolDepth != n {
		t.Fatalf("SpoolDepth = %d, want %d", st1.SpoolDepth, n)
	}
	if len(down.Traces()) != 0 {
		t.Fatalf("sink received traces it had refused")
	}
	_ = m1.Close() // ship still fails; the spool must still close cleanly

	// Restart against the same directory with a working sink.
	up := NewMemSink()
	cfg2 := manualConfig(up)
	cfg2.SpoolDir = dir
	cfg2.SpoolSegmentBytes = 4 << 10
	cfg2.SpoolMaxBytes = 4 << 20
	m2 := New(cfg2)
	defer m2.Close()

	if st := m2.Stats(); st.SpoolDepth != n {
		t.Errorf("SpoolDepth after restart = %d, want %d -- un-drained segments were not found", st.SpoolDepth, n)
	}
	if err := m2.Flush(context.Background()); err != nil {
		t.Fatalf("Flush after restart: %v", err)
	}

	got := up.Traces()
	if len(got) != n {
		t.Fatalf("replayed %d traces, want %d", len(got), n)
	}
	seen := map[string]bool{}
	for _, tr := range got {
		seen[tr.RequestID] = true
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("req-%05d", i)
		if !seen[id] {
			t.Fatalf("%s missing after replay", id)
		}
	}
	if got[0].Excerpt != "excerpt for request 0" {
		t.Errorf("excerpt = %q after replay", got[0].Excerpt)
	}
	if st := m2.Stats(); st.SpoolDepth != 0 {
		t.Errorf("SpoolDepth = %d after a successful ship, want 0", st.SpoolDepth)
	}
}

func TestSpoolReplayIsOrderedAndAcknowledged(t *testing.T) {
	dir := t.TempDir()
	sink := NewMemSink()
	cfg := manualConfig(sink)
	cfg.SpoolDir = dir
	cfg.SpoolBatch = 10
	cfg.SpoolSegmentBytes = 1 << 10
	m := New(cfg)
	defer m.Close()

	const n = 200
	for i := 0; i < n; i++ {
		ev := sampleEvent(i)
		ev.Trace.RequestID = fmt.Sprintf("req-%05d", i)
		m.Record(ev)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	got := sink.Traces()
	if len(got) != n {
		t.Fatalf("got %d traces, want %d", len(got), n)
	}
	for i, tr := range got {
		if want := fmt.Sprintf("req-%05d", i); tr.RequestID != want {
			t.Fatalf("trace %d = %s, want %s -- the spool must preserve order", i, tr.RequestID, want)
		}
	}
	// Everything acknowledged means everything unlinked.
	if _, bytes := m.sp.Depth(); bytes > int64(cfg.SpoolSegmentBytes) {
		t.Errorf("spool still holds %d bytes after full acknowledgement", bytes)
	}
}

func TestDegradedRecoversOnceDrainedAndQuiet(t *testing.T) {
	sink := NewMemSink()
	cfg := manualConfig(sink)
	cfg.TraceQueueSize = 4
	m := New(cfg)
	defer m.Close()

	for i := 0; i < 200; i++ {
		ev := sampleEvent(i)
		ev.Trace.RequestID = fmt.Sprintf("req-%d", i)
		m.Record(ev)
	}
	if deg, _ := m.Degraded(); !deg {
		t.Fatal("Degraded() = false after overrunning a 4-slot queue")
	}
	// One clean cycle records the drop level; the next clears.
	for i := 0; i < 3; i++ {
		if err := m.Flush(context.Background()); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}
	if deg, reason := m.Degraded(); deg {
		t.Errorf("Degraded() still true (%v) after the backlog cleared and no new drop occurred", reason)
	}
}

// -------------------------------------------------------------- sampling

func TestSamplingHonoursRate(t *testing.T) {
	const n = 20000
	for _, rate := range []float64{0.1, 0.25, 0.5} {
		t.Run(fmt.Sprintf("rate-%.2f", rate), func(t *testing.T) {
			sink := NewMemSink()
			cfg := manualConfig(sink)
			cfg.SampleRate = rate
			cfg.TraceQueueSize = 1 << 16
			m := New(cfg)
			defer m.Close()

			for i := 0; i < n; i++ {
				ev := sampleEvent(i)
				ev.Trace.RequestID = fmt.Sprintf("req-%06d", i)
				m.Record(ev)
			}
			st := m.Stats()
			if st.Recorded != n {
				t.Errorf("Recorded = %d, want %d -- sampling must not touch the numeric path", st.Recorded, n)
			}
			if st.TracesRecorded+st.TracesSampledOut != n {
				t.Errorf("admitted %d + excluded %d != %d", st.TracesRecorded, st.TracesSampledOut, n)
			}
			if st.TracesDropped != 0 {
				t.Errorf("TracesDropped = %d; sampling is policy, not loss", st.TracesDropped)
			}
			if deg, _ := m.Degraded(); deg {
				t.Error("Degraded() = true from sampling alone; a sampled deployment would report degraded forever")
			}
			got := float64(st.TracesRecorded) / n
			if got < rate-0.02 || got > rate+0.02 {
				t.Errorf("effective rate = %.4f, want %.2f +/- 0.02", got, rate)
			}
		})
	}
}

func TestSamplingIsDeterministicInRequestID(t *testing.T) {
	var s sampler
	initSampler(&s, 0.3, 0)
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("req-%d", i)
		want := s.admit(id)
		for k := 0; k < 5; k++ {
			if s.admit(id) != want {
				t.Fatalf("%s sampled inconsistently", id)
			}
		}
	}
}

func TestSamplingHonoursDailyByteBudget(t *testing.T) {
	sink := NewMemSink()
	cfg := manualConfig(sink)
	cfg.TraceQueueSize = 1 << 14
	cfg.DailyByteBudget = 64 << 10
	m := New(cfg)
	defer m.Close()

	const n = 4000
	day := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		ev := sampleEvent(i)
		ev.Time = day
		ev.Trace.RequestID = fmt.Sprintf("req-%06d", i)
		ev.Trace.Excerpt = strings.Repeat("x", 300)
		m.Record(ev)
	}
	st := m.Stats()
	if st.Recorded != n {
		t.Errorf("Recorded = %d, want %d", st.Recorded, n)
	}
	if st.TracesBudgetDropped == 0 {
		t.Fatalf("nothing excluded by a %d-byte budget over %d traces of ~460 bytes", cfg.DailyByteBudget, n)
	}
	if st.TracesRecorded == 0 {
		t.Fatal("the budget excluded everything, including the first trace")
	}
	if deg, _ := m.Degraded(); deg {
		t.Error("Degraded() = true from the byte budget; that is policy working, not failure")
	}
	if st.TracesDropped != 0 {
		t.Errorf("TracesDropped = %d; a budget exclusion is not a drop", st.TracesDropped)
	}
	// The budget is enforced, not hoped for (DESIGN 9.2).
	if used := m.smpl.bytesUsed(); used > cfg.DailyByteBudget+2000 {
		// used may overshoot by the size of concurrent in-flight charges only.
		t.Errorf("charged %d bytes against a %d-byte budget", used, cfg.DailyByteBudget)
	}

	// The next UTC day starts a fresh allowance.
	before := m.Stats().TracesRecorded
	ev := sampleEvent(0)
	ev.Time = day.Add(24 * time.Hour)
	ev.Trace.RequestID = "req-tomorrow"
	ev.Trace.Excerpt = "small"
	m.Record(ev)
	if m.Stats().TracesRecorded != before+1 {
		t.Error("the byte budget did not reset at the UTC day boundary")
	}
}

func TestSampleRateZeroAndOne(t *testing.T) {
	for _, tc := range []struct {
		rate float64
		want int64
	}{{0.0000001, 0}, {1, 100}} {
		sink := NewMemSink()
		cfg := manualConfig(sink)
		cfg.SampleRate = tc.rate
		m := New(cfg)
		for i := 0; i < 100; i++ {
			ev := sampleEvent(i)
			ev.Trace.RequestID = fmt.Sprintf("req-%d", i)
			m.Record(ev)
		}
		if got := m.Stats().TracesRecorded; got != tc.want {
			t.Errorf("rate %v admitted %d traces, want %d", tc.rate, got, tc.want)
		}
		if got := m.Stats().Recorded; got != 100 {
			t.Errorf("rate %v: numeric Recorded = %d, want 100", tc.rate, got)
		}
		_ = m.Close()
	}
}

// -------------------------------------------------------------- excerpts

func TestExcerptModes(t *testing.T) {
	long := strings.Repeat("a", 2000)
	for _, tc := range []struct {
		mode  ExcerptMode
		check func(t *testing.T, got string)
	}{
		{ExcerptText, func(t *testing.T, got string) {
			if len(got) != 512 {
				t.Errorf("excerpt length = %d, want 512", len(got))
			}
		}},
		{ExcerptNone, func(t *testing.T, got string) {
			if got != "" {
				t.Errorf("excerpt = %q, want empty under mode none", got)
			}
		}},
		{ExcerptHash, func(t *testing.T, got string) {
			if !strings.HasPrefix(got, "sha256:") || len(got) != 7+64 {
				t.Errorf("excerpt = %q, want a sha256: digest", got)
			}
		}},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			sink := NewMemSink()
			cfg := manualConfig(sink)
			cfg.ExcerptMode = tc.mode
			m := New(cfg)
			defer m.Close()

			ev := sampleEvent(0)
			ev.Trace.RequestID = "req-1"
			ev.Trace.Excerpt = long
			m.Record(ev)
			if err := m.Flush(context.Background()); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			traces := sink.Traces()
			if len(traces) != 1 {
				t.Fatalf("got %d traces, want 1", len(traces))
			}
			tc.check(t, traces[0].Excerpt)
		})
	}
}

func TestExcerptNeverTearsARune(t *testing.T) {
	// 2000 runes at 3 bytes each is 6000 bytes; the arena holds 2048, which
	// lands mid-sequence. The excerpt handed to the sink must still decode.
	sink := NewMemSink()
	cfg := manualConfig(sink)
	cfg.ExcerptChars = 2000
	m := New(cfg)
	defer m.Close()

	ev := sampleEvent(0)
	ev.Trace.RequestID = "req-1"
	ev.Trace.Excerpt = strings.Repeat("한", 2000)
	m.Record(ev)
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	got := sink.Traces()[0].Excerpt
	if len(got)%3 != 0 {
		t.Fatalf("excerpt is %d bytes, not a whole number of 3-byte runes", len(got))
	}
	for _, r := range got {
		if r != '한' {
			t.Fatalf("excerpt contains %q; a rune was torn by the arena boundary", r)
		}
	}
	if len(got) > maxExcerptBytes {
		t.Errorf("excerpt is %d bytes, above the %d-byte arena slot", len(got), maxExcerptBytes)
	}
}

func TestExcerptIsCopiedNotRetained(t *testing.T) {
	// Record must not pin the caller's buffer: a 512-byte substring of a
	// 400 KiB body would otherwise keep the whole body alive while queued.
	sink := NewMemSink()
	cfg := manualConfig(sink)
	m := New(cfg)
	defer m.Close()

	body := []byte(strings.Repeat("A", 4096))
	ev := sampleEvent(0)
	ev.Trace.RequestID = "req-1"
	ev.Trace.Excerpt = string(body[:100])
	m.Record(ev)
	for i := range body {
		body[i] = 'Z'
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := sink.Traces()[0].Excerpt; got != strings.Repeat("A", 100) {
		t.Errorf("excerpt = %q...; Record kept a reference into the caller's buffer", got[:min(20, len(got))])
	}
}

// ------------------------------------------------------------- lifecycle

func TestNilAndOffMeterAreSafe(t *testing.T) {
	var nilM *Meter
	nilM.Record(sampleEvent(0))
	if err := nilM.Flush(context.Background()); err != nil {
		t.Errorf("nil Flush: %v", err)
	}
	if deg, _ := nilM.Degraded(); deg {
		t.Error("nil Degraded() = true")
	}
	if st := nilM.Stats(); st != (Stats{}) {
		t.Errorf("nil Stats() = %+v, want zero", st)
	}
	if err := nilM.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}

	off := Off()
	for i := 0; i < 100; i++ {
		off.Record(sampleEvent(i))
	}
	if st := off.Stats(); st.Recorded != 0 {
		t.Errorf("Off().Stats().Recorded = %d, want 0", st.Recorded)
	}
	if err := off.Close(); err != nil {
		t.Errorf("Off().Close: %v", err)
	}
}

func TestCloseIsIdempotentAndDrainsToDisk(t *testing.T) {
	dir := t.TempDir()
	sink := NewMemSink()
	sink.SetErrors(nil, errors.New("down"))
	cfg := manualConfig(sink)
	cfg.SpoolDir = dir
	m := New(cfg)

	for i := 0; i < 100; i++ {
		ev := sampleEvent(i)
		ev.Trace.RequestID = fmt.Sprintf("req-%d", i)
		m.Record(ev)
	}
	// Close without any prior Flush: the ring must still reach disk.
	first := m.Close()
	if second := m.Close(); !errors.Is(second, first) && !errors.Is(first, second) {
		t.Errorf("Close is not idempotent: first = %v, second = %v", first, second)
	}

	up := NewMemSink()
	cfg2 := manualConfig(up)
	cfg2.SpoolDir = dir
	m2 := New(cfg2)
	defer m2.Close()
	if st := m2.Stats(); st.SpoolDepth != 100 {
		t.Fatalf("SpoolDepth after restart = %d, want 100 -- Close did not drain the ring", st.SpoolDepth)
	}
	if err := m2.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(up.Traces()); got != 100 {
		t.Errorf("replayed %d traces, want 100", got)
	}
}

func TestBackgroundLoopsFlushWithoutBeingAsked(t *testing.T) {
	sink := NewMemSink()
	m := New(Config{
		Sink:          sink,
		FlushInterval: 5 * time.Millisecond,
		DrainInterval: 5 * time.Millisecond,
		ShipInterval:  5 * time.Millisecond,
		CloseTimeout:  time.Second,
	})
	defer m.Close()

	for i := 0; i < 500; i++ {
		ev := sampleEvent(i)
		ev.Trace.RequestID = fmt.Sprintf("req-%d", i)
		m.Record(ev)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sink.Totals().Requests == 500 && len(sink.Traces()) == 500 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("background loops did not flush: requests=%d traces=%d",
		sink.Totals().Requests, len(sink.Traces()))
}

func TestZeroConfigIsUsable(t *testing.T) {
	m := New(Config{})
	defer m.Close()
	m.Record(sampleEvent(0))
	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	st := m.Stats()
	if st.Recorded != 1 {
		t.Errorf("Recorded = %d, want 1", st.Recorded)
	}
	if st.Shards < minShards {
		t.Errorf("Shards = %d, want at least %d", st.Shards, minShards)
	}
	if deg, _ := m.Degraded(); deg {
		t.Error("a fresh meter reports degraded")
	}
}

func TestConfigClamping(t *testing.T) {
	c := Config{Shards: 3, SampleRate: 9, TraceQueueSize: 5, ExcerptChars: -1}.withDefaults()
	if c.Shards != 4 {
		t.Errorf("Shards = %d, want 4 (next power of two)", c.Shards)
	}
	if c.SampleRate != 1 {
		t.Errorf("SampleRate = %v, want 1", c.SampleRate)
	}
	if c.TraceQueueSize != 8 {
		t.Errorf("TraceQueueSize = %d, want 8", c.TraceQueueSize)
	}
	if c.ExcerptChars != DefaultExcerptChars {
		t.Errorf("ExcerptChars = %d, want %d", c.ExcerptChars, DefaultExcerptChars)
	}
	if c.Sink == nil || c.Now == nil {
		t.Error("Sink and Now must be defaulted, not left nil")
	}
	big := Config{Shards: 1 << 20}.withDefaults()
	if big.Shards != maxShards {
		t.Errorf("Shards = %d, want the %d cap", big.Shards, maxShards)
	}
}

func TestParseExcerptMode(t *testing.T) {
	for in, want := range map[string]ExcerptMode{
		"": ExcerptText, "excerpt": ExcerptText, "text": ExcerptText,
		"none": ExcerptNone, "hash": ExcerptHash,
	} {
		got, err := ParseExcerptMode(in)
		if err != nil || got != want {
			t.Errorf("ParseExcerptMode(%q) = %v, %v; want %v, nil", in, got, err, want)
		}
	}
	if _, err := ParseExcerptMode("full"); err == nil {
		t.Error("ParseExcerptMode(\"full\") should be an error, not a silent default")
	}
}

func TestClassifyStatus(t *testing.T) {
	for code, want := range map[int]StatusClass{
		0: StatusUnknown, 100: StatusUnknown, 200: Status2xx, 204: Status2xx,
		301: Status3xx, 404: Status4xx, 429: Status4xx, 500: Status5xx, 503: Status5xx,
	} {
		if got := ClassifyStatus(code); got != want {
			t.Errorf("ClassifyStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

// ------------------------------------------------------- the alloc contract

// TestRecordZeroAlloc holds Record to the claim DESIGN 15.2 makes for the hot
// path. It is the proof, not the restatement: if the accumulator ever starts
// allocating per key, or the trace ring per payload, this fails.
func TestRecordZeroAlloc(t *testing.T) {
	m := New(Config{FlushInterval: -1, DrainInterval: -1, ShipInterval: -1, TraceQueueSize: 1 << 16})
	defer m.Close()

	numeric := sampleEvent(0)
	// Warm the working set: admitting a key for the first time takes a slot
	// from a preallocated free list, but the map insert itself is what we
	// want out of the measurement.
	for i := 0; i < 64; i++ {
		m.Record(sampleEvent(i))
	}
	if n := testing.AllocsPerRun(1000, func() { m.Record(numeric) }); n != 0 {
		t.Errorf("Record (numeric path) allocates %v times per call, want 0", n)
	}

	traced := sampleEvent(0)
	traced.Trace = TraceInfo{
		RequestID:      "req-steady",
		TraceID:        "trace-steady",
		SpanID:         "span-1",
		UpstreamModel:  "gpt-4o",
		Excerpt:        strings.Repeat("x", 512),
		QueueWait:      time.Millisecond,
		Retries:        1,
		FallbackReason: "none",
	}
	for i := 0; i < 64; i++ {
		m.Record(traced)
	}
	if n := testing.AllocsPerRun(1000, func() { m.Record(traced) }); n != 0 {
		t.Errorf("Record (with trace payload) allocates %v times per call, want 0", n)
	}

	// A disabled meter must be free too, or "metering off" is not a baseline.
	off := Off()
	if n := testing.AllocsPerRun(1000, func() { off.Record(traced) }); n != 0 {
		t.Errorf("Off().Record allocates %v times per call, want 0", n)
	}
}

func TestConcurrentRecordFlushAndStats(t *testing.T) {
	sink := NewMemSink()
	cfg := manualConfig(sink)
	cfg.SpoolDir = t.TempDir()
	m := New(cfg)
	defer m.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 3000; i++ {
				ev := sampleEvent(i)
				ev.Trace.RequestID = fmt.Sprintf("r-%d-%d", g, i)
				ev.Trace.Excerpt = "excerpt"
				m.Record(ev)
			}
		}(g)
	}
	var readers sync.WaitGroup
	for k := 0; k < 3; k++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = m.Flush(context.Background())
				_ = m.Stats()
				_, _ = m.Degraded()
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()

	if err := m.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got, want := sink.Totals().Requests, int64(8*3000); got != want {
		t.Errorf("requests = %d, want %d", got, want)
	}
}

func TestStatsAndDegradedDoNotBlockOnAStalledSink(t *testing.T) {
	sink := NewMemSink()
	m := New(Config{
		Sink:          sink,
		FlushInterval: time.Millisecond,
		DrainInterval: time.Millisecond,
		ShipInterval:  time.Millisecond,
		CloseTimeout:  100 * time.Millisecond,
	})
	sink.Block(true)
	for i := 0; i < 2000; i++ {
		ev := sampleEvent(i)
		ev.Trace.RequestID = fmt.Sprintf("req-%d", i)
		m.Record(ev)
	}
	// Give the background loops time to park inside the wedged sink.
	time.Sleep(50 * time.Millisecond)

	// The health and metrics surface must answer while the store is down --
	// that is the moment it is needed.
	done := make(chan Stats, 1)
	go func() {
		_, _ = m.Degraded()
		done <- m.Stats()
	}()
	select {
	case st := <-done:
		t.Logf("Stats while wedged: recorded=%d dropped=%d degraded=%v", st.Recorded, st.TracesDropped, st.Degraded)
	case <-time.After(5 * time.Second):
		t.Fatal("Stats() blocked while the sink was stalled")
	}

	// Close must not wait out the sink either.
	start := time.Now()
	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case <-closed:
		t.Logf("Close returned in %v with the sink still wedged", time.Since(start))
	case <-time.After(10 * time.Second):
		sink.Unblock()
		t.Fatal("Close() blocked on a stalled sink instead of bounding itself by CloseTimeout")
	}
	sink.Unblock()
}

func TestShardHintSpreadsAcrossGoroutines(t *testing.T) {
	// Correctness never depends on the hint -- lockShard verifies with
	// TryLock -- but if it ever became degenerate, every Record would fan out
	// and the uncontended fast path would be gone. This is the regression
	// guard for that, e.g. against a change in how the runtime lays out
	// goroutine stacks.
	const (
		goroutines = 64
		mask       = 15
	)
	var mu sync.Mutex
	seen := map[uint64]int{}
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := stackHint() & mask
			mu.Lock()
			seen[h]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	t.Logf("%d goroutines landed on %d of %d shards: %v", goroutines, len(seen), mask+1, seen)
	if len(seen) < 4 {
		t.Errorf("the goroutine-affinity hint put %d goroutines on only %d shards; "+
			"shard selection has degenerated to a fan-out on every call", goroutines, len(seen))
	}
}

// TestRecordStaysZeroAllocAcrossFlushes guards the reason the accumulator
// resets counters in place instead of clearing its map: if a flush dropped the
// key set, the first Record for each key afterwards would have to grow the map
// again, and the zero-allocation claim would hold only between flushes.
func TestRecordStaysZeroAllocAcrossFlushes(t *testing.T) {
	m := New(Config{FlushInterval: -1, DrainInterval: -1, ShipInterval: -1})
	defer m.Close()

	ev := sampleEvent(0)
	ev.Trace.RequestID = "req-steady"
	ev.Trace.Excerpt = strings.Repeat("x", 512)
	for i := 0; i < 64; i++ {
		m.Record(sampleEvent(i))
	}
	for round := 0; round < 4; round++ {
		if err := m.flushNumeric(context.Background()); err != nil {
			t.Fatalf("round %d: flush: %v", round, err)
		}
		m.Record(ev) // re-admit after the flush
		if n := testing.AllocsPerRun(500, func() { m.Record(ev) }); n != 0 {
			t.Errorf("round %d after a flush: Record allocates %v times per call, want 0", round, n)
		}
	}
}

// TestCountersSurviveCloseWithADeadSink is DESIGN 12.1's whole reason for
// splitting the two paths: the trace payload may be dropped, the counters may
// not. The trace path made good on that with a durable spool; the numeric path
// carried its refused rollups in a slice, so a shutdown against an unreachable
// store lost 100% of them — the case the promise exists for.
//
// The sink is dead for the entire life of the first meter, including its final
// flush. What is asserted is not that a code path ran but that the counters are
// still there afterwards: a second meter over the same directory replays them
// and a working sink receives every request that was recorded.
func TestCountersSurviveCloseWithADeadSink(t *testing.T) {
	dir := t.TempDir()
	const requests = 250

	dead := NewMemSink()
	dead.SetErrors(errors.New("store is down"), errors.New("store is down"))
	cfg := manualConfig(dead)
	cfg.SpoolDir = dir
	m := New(cfg)

	for i := 0; i < requests; i++ {
		m.Record(sampleEvent(i))
	}
	// Close makes the final attempt; the sink refuses it, as it has all along.
	_ = m.Close()
	if got := dead.Totals().Requests; got != 0 {
		t.Fatalf("the dead sink accepted %d requests; it was supposed to refuse everything", got)
	}

	// A new process over the same spool directory, with a store that answers.
	up := NewMemSink()
	cfg2 := manualConfig(up)
	cfg2.SpoolDir = dir
	m2 := New(cfg2)
	defer m2.Close()

	if st := m2.Stats(); st.PendingBuckets == 0 {
		t.Fatal("nothing was replayed: every counter recorded before the shutdown is gone")
	}
	if err := m2.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	tot := up.Totals()
	if tot.Requests != requests {
		t.Fatalf("requests recovered = %d, want %d", tot.Requests, requests)
	}
	if want := int64(requests) * sampleEvent(0).CostNano; tot.CostNano != want {
		t.Errorf("cost recovered = %d, want %d", tot.CostNano, want)
	}
	if want := int64(requests) * sampleEvent(0).Tokens.Total(); tot.Tokens.Total() != want {
		t.Errorf("tokens recovered = %d, want %d", tot.Tokens.Total(), want)
	}
	if st := m2.Stats(); st.PendingBuckets != 0 {
		t.Errorf("PendingBuckets = %d after a successful flush, want 0", st.PendingBuckets)
	}

	// A third start must find nothing left owed: the file is removed once the
	// counters have actually landed, or every restart re-reports them.
	m3 := New(cfg2)
	defer m3.Close()
	if st := m3.Stats(); st.PendingBuckets != 0 {
		t.Errorf("PendingBuckets = %d on a start after a clean flush, want 0", st.PendingBuckets)
	}
}

// The carry-over survives the round trip exactly, dimension by dimension. A
// bucket that comes back with the right request count and the wrong hour, or
// the wrong status class, is a silently wrong rollup row.
func TestCarryOverRoundTripsEveryField(t *testing.T) {
	dir := t.TempDir()
	want := []Bucket{{
		Key: Key{APIKeyID: "key-1", TeamID: "team-a", ModelGroup: "gpt", Provider: "openai",
			CredentialID: "cred-1", Endpoint: "/v1/chat/completions", StatusClass: Status5xx},
		HourStart:  time.Unix(1700000000-1700000000%3600, 0).UTC(),
		Requests:   7,
		Errors:     3,
		Tokens:     Tokens{Input: 11, Output: 22, CacheRead: 33, CacheWrite: 44, Reasoning: 55},
		CostNano:   987654321,
		LatencySum: 9 * time.Second,
		TTFTSum:    3 * time.Second,
		TTFTCount:  5,
	}, {
		Key:       Key{APIKeyID: OverflowSentinel},
		HourStart: time.Unix(1700003600, 0).UTC(),
		Requests:  1,
	}}
	if err := writeCarry(dir, want); err != nil {
		t.Fatalf("writeCarry: %v", err)
	}
	got, err := readCarry(dir)
	if err != nil {
		t.Fatalf("readCarry: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d buckets, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bucket %d round-tripped as %+v, want %+v", i, got[i], want[i])
		}
	}

	// A truncated tail yields everything before it rather than nothing.
	path := filepath.Join(dir, carryName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := os.WriteFile(path, raw[:len(raw)-3], 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err = readCarry(dir)
	if err != nil {
		t.Fatalf("readCarry after truncation: %v", err)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("a torn tail lost the intact records before it: got %+v", got)
	}
}
