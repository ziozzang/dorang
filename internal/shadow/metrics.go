package shadow

import (
	"strconv"
	"sync/atomic"
)

// stats is the fixed-cardinality counter set. Nothing here is labelled by a
// model, a route or a key: the same rule internal/server's metrics follow, for
// the same reason.
type stats struct {
	sampled           atomic.Uint64
	queued            atomic.Uint64
	dropped           atomic.Uint64
	sent              atomic.Uint64
	refErrors         atomic.Uint64
	mirrored          atomic.Uint64
	compared          atomic.Uint64
	clean             atomic.Uint64
	withDiffs         atomic.Uint64
	inconclusive      atomic.Uint64
	diffs             atomic.Uint64
	panics            atomic.Uint64
	skippedCapped     atomic.Uint64
	skippedLoop       atomic.Uint64
	skippedOversize   atomic.Uint64
	skippedUnsafe     atomic.Uint64
	unpricedEstimates atomic.Uint64
}

// Stats is a point-in-time view, for tests and for whatever wants the numbers
// without parsing the exposition format.
type Stats struct {
	Mode       string
	SampleRate float64

	// Sampled is requests admitted by the sampler; Queued is those that made it
	// onto the work queue; Dropped is the difference the queue refused.
	Sampled uint64
	Queued  uint64
	Dropped uint64

	// Sent is reference calls attempted, ReferenceErrors those that could not
	// be completed.
	Sent            uint64
	ReferenceErrors uint64

	// Compared is comparisons run. Clean, WithDiffs and Inconclusive partition
	// it. Clean is the only one of the three that supports a cutover: a
	// comparison that could not decide a dimension is not evidence of sameness.
	Compared     uint64
	Clean        uint64
	WithDiffs    uint64
	Inconclusive uint64
	// Diffs is the total number of individual differences, which is larger than
	// WithDiffs whenever one response differed in more than one way.
	Diffs uint64

	// Mirrored is reference calls recorded without comparison, in mirror mode.
	Mirrored uint64

	// SkippedCapped, SkippedLoop and SkippedOversize are requests the sampler
	// admitted and something else refused. They are separate counters because
	// each one means something different about whether the report is complete.
	SkippedCapped   uint64
	SkippedLoop     uint64
	SkippedOversize uint64
	// SkippedUnsafe is requests that were sampled and then refused replay
	// because re-issuing them could change state on the reference — see
	// [replayable]. It is the number that says how much of the served surface
	// a clean report does *not* cover, so it is reported next to the verdict
	// rather than buried.
	SkippedUnsafe uint64
	// UnpricedEstimates is shadow calls charged the fallback estimate because
	// dorang could not price the request they copy. A large number here means
	// the daily ceiling is being enforced against a guess.
	UnpricedEstimates uint64

	Panics uint64

	// SpentNanoUSD is today's committed reference spend — settled plus
	// outstanding reservations. It is the reference gateway's money, not
	// dorang's; see the package comment.
	SpentNanoUSD int64
	// LimitNanoUSD is the configured daily ceiling.
	LimitNanoUSD int64
	// CostCapped is true when the ceiling has stopped shadowing for the rest of
	// the UTC day.
	CostCapped bool
	// CapStops is how many times the ceiling has tripped since the process
	// started.
	CapStops int64

	// ReportDropped is report records the byte cap refused, ReportErrors those
	// a write failed on, and ReportBytes the current file size. A non-zero
	// ReportDropped means the report is not the whole story and must not be
	// read as one.
	ReportDropped uint64
	ReportErrors  uint64
	ReportBytes   int64

	// QueueDepth and QueueCapacity are the live queue.
	QueueDepth    int
	QueueCapacity int
}

// Stats returns the current numbers.
func (s *Shadower) Stats() Stats {
	st := Stats{
		Mode:              s.opts.Mode.String(),
		SampleRate:        s.opts.SampleRate,
		Sampled:           s.m.sampled.Load(),
		Queued:            s.m.queued.Load(),
		Dropped:           s.m.dropped.Load(),
		Sent:              s.m.sent.Load(),
		ReferenceErrors:   s.m.refErrors.Load(),
		Compared:          s.m.compared.Load(),
		Clean:             s.m.clean.Load(),
		WithDiffs:         s.m.withDiffs.Load(),
		Inconclusive:      s.m.inconclusive.Load(),
		Diffs:             s.m.diffs.Load(),
		Mirrored:          s.m.mirrored.Load(),
		SkippedCapped:     s.m.skippedCapped.Load(),
		SkippedLoop:       s.m.skippedLoop.Load(),
		SkippedOversize:   s.m.skippedOversize.Load(),
		SkippedUnsafe:     s.m.skippedUnsafe.Load(),
		UnpricedEstimates: s.m.unpricedEstimates.Load(),
		Panics:            s.m.panics.Load(),
		LimitNanoUSD:      s.opts.MaxCostNanoUSDPerDay,
		QueueCapacity:     cap(s.q),
		QueueDepth:        len(s.q),
	}
	if s.budget != nil {
		now := s.opts.Now()
		st.SpentNanoUSD = s.budget.committed()
		st.CostCapped = s.budget.capped(now)
		st.CapStops = s.budget.stops.Load()
	}
	if s.report != nil {
		st.ReportDropped = uint64(s.report.dropped.Load())
		st.ReportErrors = uint64(s.report.errs.Load())
		st.ReportBytes = s.report.written.Load()
	}
	return st
}

// Metrics implements [server.Observer]. It appends Prometheus text-exposition
// lines, in the same hand-rolled form internal/server uses.
func (s *Shadower) Metrics(b []byte) []byte {
	st := s.Stats()
	b = counter(b, "dorang_shadow_sampled_total",
		"Requests admitted by the shadow sampler.", st.Sampled)
	b = counter(b, "dorang_shadow_queued_total",
		"Sampled requests placed on the shadow work queue.", st.Queued)
	b = counter(b, "dorang_shadow_dropped_total",
		"Sampled requests dropped because the shadow queue was full.", st.Dropped)
	b = counter(b, "dorang_shadow_sent_total",
		"Reference gateway calls attempted.", st.Sent)
	b = counter(b, "dorang_shadow_reference_errors_total",
		"Reference gateway calls that could not be completed.", st.ReferenceErrors)
	b = counter(b, "dorang_shadow_mirrored_total",
		"Reference results recorded without comparison, in mirror mode.", st.Mirrored)
	b = counter(b, "dorang_shadow_compared_total",
		"Structural comparisons run.", st.Compared)
	b = counter(b, "dorang_shadow_clean_total",
		"Comparisons that decided every dimension and found no difference.", st.Clean)
	b = counter(b, "dorang_shadow_with_diffs_total",
		"Comparisons that found at least one difference.", st.WithDiffs)
	b = counter(b, "dorang_shadow_inconclusive_total",
		"Comparisons that could not decide a dimension. Not evidence of sameness.",
		st.Inconclusive)
	b = counter(b, "dorang_shadow_diffs_total",
		"Individual structural differences found.", st.Diffs)
	b = counter(b, "dorang_shadow_skipped_capped_total",
		"Requests not shadowed because the daily cost ceiling had stopped shadowing.",
		st.SkippedCapped)
	b = counter(b, "dorang_shadow_skipped_loop_total",
		"Requests not shadowed because they were themselves shadow copies.",
		st.SkippedLoop)
	b = counter(b, "dorang_shadow_skipped_oversize_total",
		"Requests not shadowed because the body did not fit the copy budget.",
		st.SkippedOversize)
	b = counter(b, "dorang_shadow_skipped_unsafe_total",
		"Requests not shadowed because replaying them could change state on the reference. "+
			"This is the served surface a clean report does not cover.",
		st.SkippedUnsafe)
	b = counter(b, "dorang_shadow_unpriced_estimates_total",
		"Shadow calls charged the fallback estimate because dorang could not price the original.",
		st.UnpricedEstimates)
	b = counter(b, "dorang_shadow_worker_panics_total",
		"Panics recovered in a shadow worker; requests were unaffected.", st.Panics)
	b = counter(b, "dorang_shadow_report_dropped_total",
		"Report records dropped because the report byte cap was reached.", st.ReportDropped)
	b = counter(b, "dorang_shadow_report_errors_total",
		"Report records that failed to write.", st.ReportErrors)

	b = gauge(b, "dorang_shadow_cost_capped",
		"1 when the daily cost ceiling has stopped shadowing for the rest of the UTC day.",
		boolGauge(st.CostCapped))
	b = gauge(b, "dorang_shadow_cost_stops_total",
		"Times the daily cost ceiling has tripped since start.", st.CapStops)
	b = gauge(b, "dorang_shadow_cost_spent_nano_usd",
		"Reference-gateway spend committed today, in nano-USD.", st.SpentNanoUSD)
	b = gauge(b, "dorang_shadow_cost_limit_nano_usd",
		"Configured daily ceiling, in nano-USD.", st.LimitNanoUSD)
	b = gauge(b, "dorang_shadow_queue_depth",
		"Comparisons waiting for a worker.", int64(st.QueueDepth))
	b = gauge(b, "dorang_shadow_queue_capacity",
		"Shadow work queue size.", int64(st.QueueCapacity))
	b = gauge(b, "dorang_shadow_report_bytes",
		"Size of the JSONL diff report.", st.ReportBytes)
	return b
}

// Health implements [server.Observer].
//
// The cost ceiling appears here as well as in metrics because "is the cutover
// gate still running" is a question asked of a health endpoint, and a ceiling
// that silently stopped shadowing three days ago would otherwise be discovered
// by someone reading an empty diff report as proof of readiness.
//
// It never makes the gateway unhealthy. A stopped shadow is not a serving
// failure, and taking a pod out of rotation over one would turn a diagnostic
// into an outage.
func (s *Shadower) Health(b []byte) []byte {
	st := s.Stats()
	b = append(b, `{"mode":"`...)
	b = append(b, st.Mode...)
	b = append(b, `","sample_rate":`...)
	b = strconv.AppendFloat(b, st.SampleRate, 'g', -1, 64)
	b = append(b, `,"cost_capped":`...)
	b = strconv.AppendBool(b, st.CostCapped)
	b = append(b, `,"spent_usd":"`...)
	b = appendNanoUSD(b, st.SpentNanoUSD)
	b = append(b, `","limit_usd":"`...)
	b = appendNanoUSD(b, st.LimitNanoUSD)
	b = append(b, `","sampled":`...)
	b = strconv.AppendUint(b, st.Sampled, 10)
	b = append(b, `,"compared":`...)
	b = strconv.AppendUint(b, st.Compared, 10)
	b = append(b, `,"clean":`...)
	b = strconv.AppendUint(b, st.Clean, 10)
	b = append(b, `,"with_diffs":`...)
	b = strconv.AppendUint(b, st.WithDiffs, 10)
	b = append(b, `,"inconclusive":`...)
	b = strconv.AppendUint(b, st.Inconclusive, 10)
	b = append(b, `,"queue_dropped":`...)
	b = strconv.AppendUint(b, st.Dropped, 10)
	b = append(b, `,"reference_errors":`...)
	b = strconv.AppendUint(b, st.ReferenceErrors, 10)
	b = append(b, `,"report_dropped":`...)
	b = strconv.AppendUint(b, st.ReportDropped, 10)
	// Next to the verdict, not buried: a clean verdict covers the compared
	// surface, and this is how much of the served surface was never compared.
	b = append(b, `,"skipped_unsafe":`...)
	b = strconv.AppendUint(b, st.SkippedUnsafe, 10)
	// The one line an operator actually needs: whether an empty report means
	// anything yet.
	b = append(b, `,"gate":"`...)
	b = append(b, gateVerdict(st)...)
	return append(b, `"}`...)
}

// gateVerdict renders §14.1's completion criterion as a word.
//
// "clean" is reserved for a run with comparisons, no differences, no
// inconclusive comparisons, no dropped work and no dropped report records —
// because every one of those makes an empty report mean less than it appears
// to.
func gateVerdict(st Stats) string {
	switch {
	case st.Mode == "off":
		return "off"
	case st.CostCapped:
		return "stopped_cost_capped"
	case st.Compared == 0 && st.Mirrored == 0:
		return "no_data"
	case st.Mode == "mirror":
		return "mirroring_only"
	case st.WithDiffs > 0:
		return "diffs"
	case st.Inconclusive > 0 || st.Dropped > 0 || st.ReportDropped > 0 ||
		st.ReferenceErrors > 0 || st.SkippedOversize > 0:
		return "incomplete"
	case st.Clean > 0:
		return "clean"
	}
	return "no_data"
}

func boolGauge(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// counter and gauge render the Prometheus text exposition format. Hand-rolled
// to match internal/server, which hand-rolls it so that the notebook profile
// has no required dependencies (DESIGN §0.2).
func counter(b []byte, name, help string, v uint64) []byte {
	b = appendMetricHeader(b, name, help, "counter")
	b = strconv.AppendUint(b, v, 10)
	return append(b, '\n')
}

func gauge(b []byte, name, help string, v int64) []byte {
	b = appendMetricHeader(b, name, help, "gauge")
	b = strconv.AppendInt(b, v, 10)
	return append(b, '\n')
}

func appendMetricHeader(b []byte, name, help, typ string) []byte {
	b = append(b, "# HELP "...)
	b = append(b, name...)
	b = append(b, ' ')
	b = append(b, help...)
	b = append(b, "\n# TYPE "...)
	b = append(b, name...)
	b = append(b, ' ')
	b = append(b, typ...)
	b = append(b, '\n')
	b = append(b, name...)
	return append(b, ' ')
}

// appendNanoUSD renders nano-USD as a plain decimal, never through a float
// (DESIGN §8.3, §15.5).
func appendNanoUSD(dst []byte, nano int64) []byte {
	if nano < 0 {
		dst = append(dst, '-')
		if nano == -1<<63 {
			nano = 1<<63 - 1
		} else {
			nano = -nano
		}
	}
	dst = strconv.AppendInt(dst, nano/1e9, 10)
	frac := nano % 1e9
	if frac == 0 {
		return dst
	}
	var d [9]byte
	for i := 8; i >= 0; i-- {
		d[i] = byte('0' + frac%10)
		frac /= 10
	}
	n := 9
	for n > 0 && d[n-1] == '0' {
		n--
	}
	dst = append(dst, '.')
	return append(dst, d[:n]...)
}
