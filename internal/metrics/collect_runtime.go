package metrics

import (
	"database/sql"
	"os"
	"runtime"
	"runtime/debug"
	rtmetrics "runtime/metrics"
	"strconv"
	"time"

	"github.com/ziozzang/dorang/internal/batch"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/shadow"
)

// ServerCollector renders the HTTP surface's own counters.
//
// It deliberately does not emit `dorang_requests_total`: DESIGN §12.3 spells
// that family with five labels and [Requests] owns it. The two agree by
// construction — internal/server increments its counter and hands the event to
// the meter in the same deferred function — and a test asserts the agreement,
// because a metric that has drifted from the thing it reports is worse than no
// metric at all.
type ServerCollector struct {
	src interface{ Stats() server.Stats }
}

// NewServerCollector builds the collector.
func NewServerCollector(s *server.Server) *ServerCollector { return &ServerCollector{src: s} }

// CollectorName implements [Collector].
func (c *ServerCollector) CollectorName() string { return "server" }

// Collect implements [Collector].
func (c *ServerCollector) Collect(w *Writer) {
	st := c.src.Stats()

	w.Metric("dorang_responses_total", Counter, "Responses by status class.")
	for class := 1; class <= 5; class++ {
		w.Label("class", statusClassLabels[class])
		w.Uint(st.ByClass[class])
	}

	w.Metric("dorang_request_bytes_total", Counter, "Request body bytes read.")
	w.Uint(st.BytesIn)

	w.Metric("dorang_response_bytes_total", Counter, "Response body bytes written.")
	w.Uint(st.BytesOut)

	w.Metric("dorang_unimplemented_total", Counter,
		"Requests answered 501 because the route is not implemented. Never a silent 404: "+
			"a 404 tells a client it asked for the wrong thing, a 501 tells it dorang has "+
			"not built this yet (COMPATIBILITY §9).")
	w.Uint(st.Unimplemented)

	w.Metric("dorang_auth_failures_total", Counter,
		"Requests refused by authentication or authorization.")
	w.Uint(st.AuthFailures)

	w.Metric("dorang_late_errors_total", Counter,
		"Errors delivered in band because the response had already started. Past the "+
			"first byte the status is 200 and cannot change (DESIGN §7.6).")
	w.Uint(st.LateErrors)

	w.Metric("dorang_handler_panics_total", Counter, "Panics recovered in a handler.")
	w.Uint(st.Panics)

	w.Metric("dorang_meter_panics_total", Counter,
		"Panics recovered in the meter; requests were unaffected.")
	w.Uint(st.MeterPanics)

	w.Metric("dorang_passthrough_requests_total", Counter,
		"Requests served by the generic passthrough engine (DESIGN §10.6).")
	w.Uint(st.Passthrough)

	w.Metric("dorang_websocket_upgrades_total", Counter, "WebSocket upgrades relayed.")
	w.Uint(st.WSUpgrades)

	w.Metric("dorang_replay_refused_total", Counter,
		"Requests marked non-replayable because the process-wide replay budget was full "+
			"(DESIGN §15.4). They cannot fall back, and they say so in a header rather "+
			"than being retained anyway.")
	w.Uint(st.ReplayRefused)

	w.Metric("dorang_body_too_large_total", Counter,
		"Requests refused for exceeding max_body_bytes.")
	w.Uint(st.BodyTooLarge)

	w.Metric("dorang_shadow_observed_total", Counter,
		"Requests captured for shadow comparison (DESIGN §14.1).")
	w.Uint(st.Observed)

	w.Metric("dorang_observer_panics_total", Counter,
		"Panics recovered in the shadow observer; requests were unaffected.")
	w.Uint(st.ObserverPanics)

	w.Metric("dorang_inflight_requests", Gauge, "Requests currently being served.")
	w.Int(st.InFlight)

	w.Metric("dorang_replay_bytes", Gauge, "Request-body bytes retained for replay.")
	w.Int(st.ReplayBytes)

	if st.ReplayLimit > 0 {
		w.Metric("dorang_replay_budget_bytes", Gauge,
			"The process-wide replay budget (DESIGN §15.4). Absent when retention is "+
				"disabled, which is not the same as a budget of zero bytes.")
		w.Int(st.ReplayLimit)

		w.Metric("dorang_replay_saturation_ratio", Gauge,
			"Retained replay bytes over the budget, in [0,1]. At 1 every further request "+
				"is non-replayable and therefore cannot fall back.")
		w.Float(clamp01(float64(st.ReplayBytes) / float64(st.ReplayLimit)))
	}

	w.Metric("dorang_ready", Gauge,
		"1 when the server accepts new work. It goes to 0 the instant a drain starts, and "+
			"also while a readiness gate is closed — a dependency this node needs is "+
			"unreachable. The two are told apart in the /health body, not here: this node "+
			"is out of rotation either way.")
	w.Bool(st.Ready)

	w.Metric("dorang_uptime_seconds", Gauge, "Seconds since start.")
	w.Float(st.Uptime.Seconds())
}

var statusClassLabels = [6]string{"0xx", "1xx", "2xx", "3xx", "4xx", "5xx"}

// StoreSource is the part of [store.Store] this package needs.
type StoreSource interface {
	DB() *sql.DB
	StatementCount() uint64
}

// StoreCollector renders the connection pool.
//
// `dorang_store_pool_saturation_ratio` is absent when the pool is unlimited,
// which is the default for PostgreSQL and for a file-backed SQLite. A ratio
// against an absent ceiling is not a small number, it is not a number, and
// publishing 0.0 for it would say the pool is idle at the exact moment it is
// oversubscribed.
type StoreCollector struct {
	src StoreSource
	// Driver names the dialect for the build-info-style label.
	Driver string
}

// NewStoreCollector builds the collector.
func NewStoreCollector(src StoreSource, driver string) *StoreCollector {
	return &StoreCollector{src: src, Driver: driver}
}

// CollectorName implements [Collector].
func (c *StoreCollector) CollectorName() string { return "store" }

// Collect implements [Collector].
func (c *StoreCollector) Collect(w *Writer) {
	db := c.src.DB()
	if db == nil {
		return
	}
	st := db.Stats()

	w.Metric("dorang_store_pool_open_connections", Gauge,
		"Connections open, in use and idle together.")
	w.Int(int64(st.OpenConnections))

	w.Metric("dorang_store_pool_in_use", Gauge, "Connections currently in use.")
	w.Int(int64(st.InUse))

	w.Metric("dorang_store_pool_idle", Gauge, "Connections currently idle.")
	w.Int(int64(st.Idle))

	w.Metric("dorang_store_pool_waits_total", Counter,
		"Times a caller had to wait for a connection. Nothing on the request path should "+
			"be here — DESIGN §15.5 prohibits synchronous store access on it — so a "+
			"rising value is metering, batch or administration contending, and it is the "+
			"first thing that will make a flush fall behind.")
	w.Int(st.WaitCount)

	w.Metric("dorang_store_pool_wait_seconds_total", Counter,
		"Cumulative time spent waiting for a connection.")
	w.Float(st.WaitDuration.Seconds())

	w.Metric("dorang_store_pool_closed_total", Counter,
		"Connections closed by the idle, lifetime and idle-time limits together.")
	w.Int(st.MaxIdleClosed + st.MaxIdleTimeClosed + st.MaxLifetimeClosed)

	if st.MaxOpenConnections > 0 {
		w.Metric("dorang_store_pool_max_open", Gauge,
			"The configured connection ceiling. Absent when the pool is unlimited.")
		w.Int(int64(st.MaxOpenConnections))

		w.Metric("dorang_store_pool_saturation_ratio", Gauge,
			"Connections in use over the ceiling, in [0,1]. ABSENT when the pool is "+
				"unlimited: a saturation ratio against no ceiling is not zero, it is "+
				"undefined, and reporting zero would say the pool is idle at the moment "+
				"it is oversubscribed.")
		w.Float(clamp01(float64(st.InUse) / float64(st.MaxOpenConnections)))
	}

	w.Metric("dorang_store_statements_total", Counter, "Statements executed.")
	w.Uint(c.src.StatementCount())
}

// ShadowSource is the part of [shadow.Shadower] this package needs.
type ShadowSource interface {
	Stats() shadow.Stats
}

// ShadowCollector renders the migration gate of DESIGN §14.1.
//
// It renders from [shadow.Stats] rather than delegating to the shadower's own
// exposition writer, for two reasons. The type discipline is enforced in one
// place; and the shadower's own renderer declares `dorang_shadow_cost_stops_total`
// a gauge while naming it `_total`, which is the same class of mistake as
// `kv_cache_usage_perc`. It is monotonic, so here it is a counter.
type ShadowCollector struct{ src ShadowSource }

// NewShadowCollector builds the collector.
func NewShadowCollector(src ShadowSource) *ShadowCollector { return &ShadowCollector{src: src} }

// CollectorName implements [Collector].
func (c *ShadowCollector) CollectorName() string { return "shadow" }

// Collect implements [Collector].
func (c *ShadowCollector) Collect(w *Writer) {
	st := c.src.Stats()

	w.Metric("dorang_shadow_mode", Gauge,
		"1 against the configured shadow mode (DESIGN §14.1).")
	for _, m := range [...]string{"off", "compare", "mirror"} {
		w.Label("mode", m)
		w.Bool(st.Mode == m)
	}
	if st.Mode == "off" {
		// Nothing below means anything with shadowing off, and a page of zeroes
		// is exactly the "clean report" a reader must not mistake for evidence.
		return
	}

	w.Metric("dorang_shadow_sample_rate", Gauge,
		"Configured sampling fraction, in [0,1].")
	w.Float(st.SampleRate)

	w.Metric("dorang_shadow_sampled_total", Counter, "Requests admitted by the sampler.")
	w.Uint(st.Sampled)

	w.Metric("dorang_shadow_queued_total", Counter,
		"Sampled requests placed on the shadow work queue.")
	w.Uint(st.Queued)

	w.Metric("dorang_shadow_dropped_total", Counter,
		"Sampled requests dropped because the queue was full.")
	w.Uint(st.Dropped)

	w.Metric("dorang_shadow_sent_total", Counter, "Reference gateway calls attempted.")
	w.Uint(st.Sent)

	w.Metric("dorang_shadow_reference_errors_total", Counter,
		"Reference gateway calls that could not be completed.")
	w.Uint(st.ReferenceErrors)

	w.Metric("dorang_shadow_mirrored_total", Counter,
		"Reference results recorded without comparison, in mirror mode.")
	w.Uint(st.Mirrored)

	w.Metric("dorang_shadow_compared_total", Counter, "Structural comparisons run.")
	w.Uint(st.Compared)

	w.Metric("dorang_shadow_clean_total", Counter,
		"Comparisons that decided every dimension and found no difference. The only one "+
			"of the three verdicts that supports a cutover.")
	w.Uint(st.Clean)

	w.Metric("dorang_shadow_with_diffs_total", Counter,
		"Comparisons that found at least one difference.")
	w.Uint(st.WithDiffs)

	w.Metric("dorang_shadow_inconclusive_total", Counter,
		"Comparisons that could not decide a dimension. Not evidence of sameness.")
	w.Uint(st.Inconclusive)

	w.Metric("dorang_shadow_diffs_total", Counter,
		"Individual structural differences found.")
	w.Uint(st.Diffs)

	w.Metric("dorang_shadow_skipped_total", Counter,
		"Sampled requests something else then refused. Separate reasons, because each "+
			"says something different about whether the report is complete — and "+
			"`unsafe` in particular is how much of the served surface a clean report does "+
			"NOT cover, since replaying those could change state on the reference.")
	for _, p := range [...]struct {
		reason string
		n      uint64
	}{
		{"capped", st.SkippedCapped},
		{"loop", st.SkippedLoop},
		{"oversize", st.SkippedOversize},
		{"unsafe", st.SkippedUnsafe},
	} {
		w.Label("reason", p.reason)
		w.Uint(p.n)
	}

	w.Metric("dorang_shadow_unpriced_estimates_total", Counter,
		"Shadow calls charged the fallback estimate because dorang could not price the "+
			"original. A large value means the daily ceiling is being enforced against a "+
			"guess.")
	w.Uint(st.UnpricedEstimates)

	w.Metric("dorang_shadow_worker_panics_total", Counter,
		"Panics recovered in a shadow worker; requests were unaffected.")
	w.Uint(st.Panics)

	w.Metric("dorang_shadow_report_dropped_total", Counter,
		"Report records dropped because the report byte cap was reached. Non-zero means "+
			"the report is not the whole story and must not be read as one.")
	w.Uint(st.ReportDropped)

	w.Metric("dorang_shadow_report_errors_total", Counter,
		"Report records that failed to write.")
	w.Uint(st.ReportErrors)

	w.Metric("dorang_shadow_report_bytes", Gauge, "Size of the JSONL diff report.")
	w.Int(st.ReportBytes)

	w.Metric("dorang_shadow_cost_capped", Gauge,
		"1 when the daily cost ceiling has stopped shadowing for the rest of the UTC day. "+
			"An empty diff report while this is 1 is not evidence of anything.")
	w.Bool(st.CostCapped)

	w.Metric("dorang_shadow_cost_stops_total", Counter,
		"Times the daily cost ceiling has tripped since start.")
	w.Int(st.CapStops)

	w.Metric("dorang_shadow_cost_spent_nano", Gauge,
		"Reference-gateway spend committed today, in nano-USD. It is the reference "+
			"gateway's money, not dorang's.")
	w.Int(st.SpentNanoUSD)

	if st.LimitNanoUSD > 0 {
		w.Metric("dorang_shadow_cost_limit_nano", Gauge,
			"The configured daily ceiling in nano-USD. Absent when uncapped.")
		w.Int(st.LimitNanoUSD)

		w.Metric("dorang_shadow_cost_spent_ratio", Gauge,
			"Today's reference spend over the daily ceiling, in [0,1]. Absent when no "+
				"ceiling is configured.")
		w.Float(clamp01(float64(st.SpentNanoUSD) / float64(st.LimitNanoUSD)))
	}

	w.Metric("dorang_shadow_queue_depth", Gauge, "Comparisons waiting for a worker.")
	w.Int(int64(st.QueueDepth))

	w.Metric("dorang_shadow_queue_capacity", Gauge, "Shadow work queue size.")
	w.Int(int64(st.QueueCapacity))
}

// BatchSource is the part of [batch.Service] this package needs.
type BatchSource interface {
	Stats() batch.Stats
}

// BatchCollector renders the backend-independent batch scheduler of DESIGN §11.1.
type BatchCollector struct{ src BatchSource }

// NewBatchCollector builds the collector.
func NewBatchCollector(src BatchSource) *BatchCollector { return &BatchCollector{src: src} }

// CollectorName implements [Collector].
func (c *BatchCollector) CollectorName() string { return "batch" }

// Collect implements [Collector].
func (c *BatchCollector) Collect(w *Writer) {
	st := c.src.Stats()

	w.Metric("dorang_batch_active", Gauge,
		"Batches with a run in this process, dispatching or waiting for a slot.")
	w.Int(int64(st.Active))

	w.Metric("dorang_batch_queue_depth", Gauge,
		"Batches blocked on the dispatch semaphore, in arrival order. A batch submitted "+
			"first is not overtaken indefinitely, which is why the queue is explicit "+
			"rather than a buffered channel.")
	w.Int(int64(st.Queued))

	w.Metric("dorang_batch_dispatch_slots", Gauge, "max_active_batches.")
	w.Int(int64(st.DispatchSlots))

	w.Metric("dorang_batch_dispatch_slots_free", Gauge, "Dispatch slots not taken.")
	w.Int(int64(st.SlotsFree))

	w.Metric("dorang_batch_rows_in_flight", Gauge,
		"Rows sent upstream and not yet settled. A row here has already been paid for, "+
			"which is why a shutdown drains rather than drops.")
	w.Int(st.RowsInFlight)

	w.Metric("dorang_batch_rows_started_total", Counter, "Rows begun in this process.")
	w.Int(st.RowsStarted)

	w.Metric("dorang_batch_rows_finished_total", Counter, "Rows settled in this process.")
	w.Int(st.RowsFinished)

	w.Metric("dorang_batch_closed", Gauge,
		"1 when the batch service has stopped accepting work.")
	w.Bool(st.Closed)
}

// BuildCollector reports what is running.
//
// It is one gauge at 1 whose labels carry the version and the commit, which is
// the conventional shape: a version is not a number to graph, it is a dimension
// to group by, and a deployment half-way through a rollout shows up as two
// series rather than as an unreadable average.
type BuildCollector struct {
	Version   string
	Commit    string
	GoVersion string
	StartedAt time.Time
	Now       func() time.Time

	// samples is reused across scrapes. runtime/metrics is read rather than
	// runtime.ReadMemStats, and the difference is the whole reason this is not
	// three lines: ReadMemStats stops the world. A scrape every fifteen seconds
	// that stops the world is a latency spike every fifteen seconds, which is
	// precisely the denial-of-service a /metrics endpoint must not be.
	samples []rtmetrics.Sample
}

// NewBuildCollector fills the Go version and start time.
//
// An empty version or commit falls back to the module's own build information
// rather than to the string "unknown". The Makefile does not stamp a version,
// and a build_info that says "unknown" on every deployment is a label nobody
// can group by — which is the only thing the metric is for.
func NewBuildCollector(version, commit string, now func() time.Time) *BuildCollector {
	if now == nil {
		now = time.Now
	}
	if version == "" || commit == "" {
		v, c := buildInfo()
		if version == "" {
			version = v
		}
		if commit == "" {
			commit = c
		}
	}
	if version == "" {
		version = "unknown"
	}
	if commit == "" {
		commit = "unknown"
	}
	return &BuildCollector{
		Version: version, Commit: commit, GoVersion: runtime.Version(),
		StartedAt: now(), Now: now,
		samples: []rtmetrics.Sample{
			{Name: "/memory/classes/total:bytes"},
			{Name: "/memory/classes/heap/objects:bytes"},
			{Name: "/gc/cycles/total:gc-cycles"},
			{Name: "/sched/goroutines:goroutines"},
			// The two histograms, and the reason they were added: a p99 of
			// 30–42 ms against a p90 of 2.3 ms was measured at concurrency 64
			// and could not be attributed to anything, because the only two
			// candidates — a stop-the-world pause and time spent runnable but
			// not running — were the two things this scrape did not publish.
			// Heap, goroutines and GC COUNT were already here; neither of them
			// can tell a tail from a mean.
			{Name: "/gc/pauses:seconds"},
			{Name: "/sched/latencies:seconds"},
		},
	}
}

// buildInfo reads the module version and the VCS revision the toolchain
// embeds. Both are empty for a `go run` of an untagged working tree, which is
// honest: there is nothing to report.
func buildInfo() (version, commit string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ""
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.modified":
			if s.Value == "true" && commit != "" {
				commit += "-dirty"
			}
		}
	}
	return version, commit
}

// CollectorName implements [Collector].
func (c *BuildCollector) CollectorName() string { return "build" }

// Collect implements [Collector].
func (c *BuildCollector) Collect(w *Writer) {
	w.Metric("dorang_build_info", Gauge,
		"Always 1. The version and the commit are labels because a build is a dimension "+
			"to group by, not a number to graph: a half-finished rollout shows up as two "+
			"series rather than as an average of two versions.")
	w.Label("version", c.Version)
	w.Label("commit", c.Commit)
	w.Label("go_version", c.GoVersion)
	w.Int(1)

	w.Metric("dorang_process_start_time_seconds", Gauge,
		"Unix time the process started.")
	w.Float(float64(c.StartedAt.UnixNano()) / 1e9)

	rtmetrics.Read(c.samples)

	w.Metric("dorang_memory_bytes", Gauge,
		"Bytes obtained from the OS and not returned. DESIGN §15.1 budgets under 100 MB "+
			"idle on the notebook profile and under 300 MB for a thousand concurrent "+
			"streams, replay budget included.")
	w.Uint(uintValue(c.samples[0]))

	w.Metric("dorang_memory_heap_bytes", Gauge, "Live heap object bytes.")
	w.Uint(uintValue(c.samples[1]))

	w.Metric("dorang_gc_cycles_total", Counter, "Completed garbage collections.")
	w.Uint(uintValue(c.samples[2]))

	w.Metric("dorang_goroutines", Gauge, "Goroutines currently running.")
	w.Uint(uintValue(c.samples[3]))

	w.Metric("dorang_gc_pause_seconds", Histogram,
		"Stop-the-world GC pause durations. Every request in flight during a pause pays it, so "+
			"this is the first thing to read when a p99 stands far above a p90 that looks fine. "+
			"`dorang_gc_cycles_total` counts pauses and cannot distinguish a hundred short ones "+
			"from one long one.")
	writeRuntimeHist(w, c.samples[4])

	w.Metric("dorang_sched_latency_seconds", Histogram,
		"Time goroutines spent runnable but not running. This is oversubscription rather than "+
			"work: with more requests in flight than GOMAXPROCS, a request can be finished in CPU "+
			"terms and still be waiting for a thread to run on. Read beside "+
			"`dorang_gc_pause_seconds` — between them they account for a tail the handler's own "+
			"duration cannot explain.")
	writeRuntimeHist(w, c.samples[5])

	w.Metric("dorang_maxprocs", Gauge,
		"GOMAXPROCS. `dorang_sched_latency_seconds` rises when offered concurrency exceeds it, "+
			"which is the difference between a gateway that is slow and one that is merely "+
			"oversubscribed.")
	w.Int(int64(runtime.GOMAXPROCS(0)))

	w.Metric("dorang_os_threads", Gauge,
		"OS threads the runtime has created. A thread is created when every existing one is "+
			"blocked in a syscall, so a count far above GOMAXPROCS means blocking calls rather "+
			"than computation.")
	n, _ := runtime.ThreadCreateProfile(nil)
	w.Int(int64(n))

	if fds, ok := openFDs(); ok {
		w.Metric("dorang_open_file_descriptors", Gauge,
			"Descriptors the process holds. Every upstream connection, store connection and "+
				"accepted request holds at least one, so an unbounded rise is the shape of a "+
				"connection that is never closed. Absent where the count cannot be read, "+
				"because zero would say the process holds none.")
		w.Int(int64(fds))
	}
}

// writeRuntimeHist renders a runtime/metrics histogram in the cumulative bucket
// form, and DELIBERATELY EMITS NO `_sum`.
//
// The runtime does not publish one. It could be approximated from bucket
// midpoints, and that is exactly the move this codebase refused for
// `Retry-After` in COMPATIBILITY §11.4: a guessed value is worse than an absent
// one, because nothing downstream can tell it from a measured one. Anyone
// computing `rate(_sum)/rate(_count)` would get an average of the bucket
// layout rather than of the data. `histogram_quantile()` needs only the buckets
// and is unaffected.
//
// The runtime's first bucket opens at -Inf, which is not a legal `le` and would
// make the family unparseable; its count is folded into the first finite bound,
// where it belongs — a pause cannot be negative.
func writeRuntimeHist(w *Writer, s rtmetrics.Sample) {
	if s.Value.Kind() != rtmetrics.KindFloat64Histogram {
		// This Go release does not publish that name. Nothing is written, and
		// [Writer.Metric] defers its HELP/TYPE until the first sample, so the
		// family is ABSENT rather than flat at zero — "the runtime does not
		// report this" and "the value is zero" are different facts.
		return
	}
	h := s.Value.Float64Histogram()
	if h == nil || len(h.Buckets) < 2 {
		return
	}
	var scratch [40]byte
	var cum uint64
	for i, count := range h.Counts {
		cum += count
		hi := h.Buckets[i+1]
		if hi > maxFloat {
			continue // the +Inf bucket is written once, after the loop
		}
		le := append(scratch[:0], "le=\""...)
		le = appendFloat(le, hi)
		le = append(le, '"')
		w.sampleName("_bucket", le)
		w.b = strconv.AppendUint(w.b, cum, 10)
		w.b = append(w.b, '\n')
	}
	w.sampleName("_bucket", append(scratch[:0], `le="+Inf"`...))
	w.b = strconv.AppendUint(w.b, cum, 10)
	w.b = append(w.b, '\n')

	w.sampleName("_count", nil)
	w.b = strconv.AppendUint(w.b, cum, 10)
	w.b = append(w.b, '\n')
	w.lbl = w.lbl[:0]
}

// openFDs counts the process's open descriptors, reporting false where that
// cannot be known rather than guessing.
func openFDs() (int, bool) {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	// ReadDir held a descriptor of its own while listing, so the entry for it is
	// in the result and is closed by the time this returns. Subtracting it keeps
	// the number from depending on how it was measured.
	if n := len(ents) - 1; n > 0 {
		return n, true
	}
	return 0, true
}

// uintValue reads a runtime/metrics sample, tolerating a name this Go release
// does not know rather than reporting a zero for it.
func uintValue(s rtmetrics.Sample) uint64 {
	if s.Value.Kind() != rtmetrics.KindUint64 {
		return 0
	}
	return s.Value.Uint64()
}

var (
	_ BatchSource  = (*batch.Service)(nil)
	_ ShadowSource = (*shadow.Shadower)(nil)
)
