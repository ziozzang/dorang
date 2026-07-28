package metrics

import (
	"github.com/ziozzang/dorang/internal/meter"
	"github.com/ziozzang/dorang/internal/prefix"
)

// MeterSource is the part of [meter.Meter] this package needs.
type MeterSource interface {
	Stats() meter.Stats
}

// MeterCollector renders the two metering queues of DESIGN §12.1.
//
// §12.1's whole claim is that numeric accounting is never lost and that trace
// drops are never silent. Both halves are checkable from this scrape: recorded
// is exact, and every way a trace can be lost has its own counter, separated
// into the policy exclusions (sampling, byte budget) that must not raise an
// alarm and the failures (queue full, spool full, spool error) that must.
type MeterCollector struct{ src MeterSource }

// NewMeterCollector builds the collector.
func NewMeterCollector(src MeterSource) *MeterCollector { return &MeterCollector{src: src} }

// CollectorName implements [Collector].
func (c *MeterCollector) CollectorName() string { return "meter" }

// meterReasons is every degradation reason, emitted as a state set for the same
// reason the health states are.
var meterReasons = [...]meter.Reason{
	meter.ReasonNone, meter.ReasonQueueFull, meter.ReasonSpoolFull,
	meter.ReasonSpoolError, meter.ReasonSinkError,
}

// Collect implements [Collector].
func (c *MeterCollector) Collect(w *Writer) {
	st := c.src.Stats()

	w.Metric("dorang_meter_recorded_total", Counter,
		"Events accepted by the numeric path. Exact: DESIGN §12.1 gives the numeric path "+
			"no drop at all, so cost, tokens and error counts survive any back-pressure.")
	w.Int(st.Recorded)

	w.Metric("dorang_meter_dropped_total", Counter,
		"Trace payloads lost (DESIGN §12.3). The sum of the queue and spool drops below. "+
			"Numeric accounting is unaffected — this is the droppable half of §12.1.")
	w.Int(st.TracesDropped)

	w.Metric("dorang_meter_dropped_queue_total", Counter,
		"Traces the bounded ring refused because the drainer could not keep up.")
	w.Int(st.TracesDroppedQueue)

	w.Metric("dorang_meter_dropped_spool_total", Counter,
		"Traces the durable spool refused because it hit its byte cap.")
	w.Int(st.TracesDroppedSpool)

	w.Metric("dorang_meter_traces_recorded_total", Counter,
		"Events that carried a trace payload and survived sampling and the byte budget.")
	w.Int(st.TracesRecorded)

	w.Metric("dorang_meter_traces_sampled_out_total", Counter,
		"Traces excluded by the sampler. Configured policy doing its job — this is not "+
			"a drop and does not raise dorang_metering_degraded.")
	w.Int(st.TracesSampledOut)

	w.Metric("dorang_meter_traces_budget_dropped_total", Counter,
		"Traces excluded by the daily excerpt byte budget. Policy, not failure.")
	w.Int(st.TracesBudgetDropped)

	w.Metric("dorang_meter_traces_spooled_total", Counter,
		"Traces written to durable local storage, so that a store stall costs disk "+
			"rather than data (DESIGN §12.1).")
	w.Int(st.TracesSpooled)

	w.Metric("dorang_meter_traces_flushed_total", Counter,
		"Traces the sink has accepted.")
	w.Int(st.TracesFlushed)

	w.Metric("dorang_meter_buckets_flushed_total", Counter,
		"Rollup buckets the sink has accepted.")
	w.Int(st.BucketsFlushed)

	w.Metric("dorang_meter_flush_errors_total", Counter,
		"Failed sink writes on either path.")
	w.Int(st.FlushErrors)

	w.Metric("dorang_meter_shard_collisions_total", Counter,
		"Record calls whose affinity hint landed on a shard already held. A high ratio "+
			"means the hint is degenerate on this platform, not that anything is wrong.")
	w.Int(st.ShardCollisions)

	w.Metric("dorang_meter_overflow_folds_total", Counter,
		"Events folded into the "+meter.OverflowSentinel+" rollup key because a shard was "+
			"at its cardinality cap. The same fold-and-count discipline this package "+
			"applies to labels, applied to the ledger.")
	w.Int(st.OverflowFolds)

	w.Metric("dorang_meter_pending_folds_total", Counter,
		"Carry-over buckets folded into overflow because the carry-over itself hit its cap.")
	w.Int(st.PendingFolds)

	w.Metric("dorang_meter_queue_depth", Gauge,
		"Trace payloads waiting in the in-memory ring.")
	w.Int(st.QueueDepth)

	w.Metric("dorang_meter_spool_depth", Gauge,
		"Trace records waiting on the durable local spool (DESIGN §12.3).")
	w.Int(st.SpoolDepth)

	w.Metric("dorang_meter_spool_bytes", Gauge,
		"Bytes the durable spool currently holds.")
	w.Int(st.SpoolBytes)

	w.Metric("dorang_meter_pending_buckets", Gauge,
		"Rollup buckets held as carry-over because the sink refused a write.")
	w.Int(st.PendingBuckets)

	w.Metric("dorang_meter_shards", Gauge,
		"Per-CPU numeric accumulators.")
	w.Int(int64(st.Shards))

	w.Metric("dorang_metering_degraded", Gauge,
		"1 when metering is losing or failing to ship data (DESIGN §12.1). Sampling and "+
			"the byte budget never set it: those are policy, and conflating them with "+
			"failure makes the signal useless on any deployment that samples.")
	w.Bool(st.Degraded)

	w.Metric("dorang_metering_degraded_reason", Gauge,
		"1 against the reason metering is degraded, 0 against the others.")
	for _, r := range meterReasons {
		w.Label("reason", r.String())
		w.Bool(st.Degraded && st.Reason == r)
	}
}

// PrefixSource is the part of [prefix.Table] this package needs.
type PrefixSource interface {
	Stats() (lookups, hits, evicted uint64, bytes int64)
	Len() int
}

// PrefixCollector renders the cache-affinity table of DESIGN §7.4.
//
// It is registered only when prefix routing is configured, which is what makes
// `dorang_prefix_hit_ratio` honest: with the feature off there is no table, no
// collector and no series — rather than a ratio of zero that reads as "the
// cache never helps".
//
// The global ratio is all this source can give. The table is keyed by content
// digest and has never been told which model a lookup belonged to, so the
// per-model breakdown §12.3 asks for is counted at the request observation
// point instead; see [Requests].
type PrefixCollector struct {
	src PrefixSource
	// bytesLimit is the configured byte budget of §7.4b. Zero means unbudgeted,
	// and the saturation ratio is then omitted rather than divided by zero.
	bytesLimit int64
}

// NewPrefixCollector builds the collector. bytesLimit of zero omits the
// saturation ratio.
func NewPrefixCollector(src PrefixSource, bytesLimit int64) *PrefixCollector {
	return &PrefixCollector{src: src, bytesLimit: bytesLimit}
}

// CollectorName implements [Collector].
func (c *PrefixCollector) CollectorName() string { return "prefix" }

// Collect implements [Collector].
func (c *PrefixCollector) Collect(w *Writer) {
	lookups, hits, evicted, bytes := c.src.Stats()

	w.Metric("dorang_prefix_table_lookups_total", Counter,
		"Prefix-chain lookups against the affinity table.")
	w.Uint(lookups)

	w.Metric("dorang_prefix_table_hits_total", Counter,
		"Lookups that found a live deployment for a chain segment.")
	w.Uint(hits)

	w.Metric("dorang_prefix_table_evicted_total", Counter,
		"Entries evicted, by age or by the byte budget of DESIGN §7.4b.")
	w.Uint(evicted)

	w.Metric("dorang_prefix_table_bytes", Gauge,
		"Bytes the affinity table currently holds.")
	w.Int(bytes)

	w.Metric("dorang_prefix_table_entries", Gauge,
		"Live entries across every shard.")
	w.Int(int64(c.src.Len()))

	// Absent until there is a denominator. Zero lookups is "nothing asked",
	// which is not the same fact as "nothing hit".
	if lookups > 0 {
		w.Metric("dorang_prefix_table_hit_ratio", Gauge,
			"Fraction in [0,1] of prefix lookups that hit. Global: the table is keyed by "+
				"content digest and does not know the model. Absent until a lookup has "+
				"happened.")
		w.Float(float64(hits) / float64(lookups))
	}

	if c.bytesLimit > 0 {
		w.Metric("dorang_prefix_table_saturation_ratio", Gauge,
			"Bytes held over the configured byte budget (DESIGN §7.4b), in [0,1]. Absent "+
				"when no budget is configured — an unbudgeted table is not an empty one.")
		w.Float(clamp01(float64(bytes) / float64(c.bytesLimit)))
	}
}

func clamp01(v float64) float64 {
	switch {
	case v != v:
		return 0
	case v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}

// compile-time assertion that the real types satisfy the narrow interfaces.
var (
	_ PrefixSource = (*prefix.Table)(nil)
	_ MeterSource  = (*meter.Meter)(nil)
)
