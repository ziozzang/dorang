package metrics

import (
	"strings"
	"testing"
)

// The two histograms that explain a tail must actually be scraped.
//
// Heap, goroutines and a GC CYCLE COUNT were published all along; none of them
// can tell a tail from a mean. A measured p99 of 30-42 ms against a p90 of
// 2.3 ms had exactly two candidate explanations, a stop-the-world pause and
// time spent runnable but not running, and the scrape published neither.
func TestTheScrapeCanExplainATail(t *testing.T) {
	r := New(nil)
	r.Register(NewBuildCollector("v", "c", nil))
	var w Writer
	w.Reset(nil)
	for _, c := range r.Collectors() {
		c.Collect(&w)
	}
	out := string(w.Bytes())

	for _, want := range []string{
		"dorang_gc_pause_seconds_bucket",
		"dorang_gc_pause_seconds_count",
		"dorang_sched_latency_seconds_bucket",
		"dorang_sched_latency_seconds_count",
		"dorang_maxprocs",
		"dorang_os_threads",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape does not carry %s; a tail cannot be attributed from outside "+
				"the process without it", want)
		}
	}
	// The runtime publishes no sum, and an approximation from bucket midpoints
	// would be indistinguishable downstream from a measured one -- the same
	// refusal COMPATIBILITY 11.4 makes for a guessed Retry-After.
	if strings.Contains(out, "dorang_gc_pause_seconds_sum") {
		t.Error("a _sum was emitted for a histogram whose source publishes none; " +
			"rate(_sum)/rate(_count) would average the bucket layout, not the data")
	}
	// -Inf is not a legal le and makes the whole family unparseable.
	if strings.Contains(out, `le="-Inf"`) {
		t.Error(`le="-Inf" emitted; the runtime's first bucket opens there and it must be ` +
			`folded into the first finite bound`)
	}
}
