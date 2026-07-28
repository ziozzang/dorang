package scenario

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/meter"
)

// DESIGN §14 scenario 16 — metering on vs off, within the stated bound,
// measured in steady state AND at a full buffer.
//
// §15.1 spells out the denominator, and the denominator is the whole argument:
// "5%" of a no-op meter is meaningless, because a no-op returns after one
// branch. The requirement is 5% of the GATEWAY-OVERHEAD budget, so the figure
// that answers it is added-nanoseconds against the 200 µs warm-local p50.
//
// Both measurement points are required. At a full buffer metering is expected
// to be CHEAPER, because a failed ring push skips the payload copy while the
// numeric path does identical work — that asymmetry is the two-queue split of
// §12.1 behaving as designed, and a suite that measured only steady state would
// not notice it disappearing.

// warmLocalP50 is the headline gateway-overhead budget of DESIGN §15.1.
const warmLocalP50 = 200 * time.Microsecond

// meteringBudget is 5% of it — the bound the added cost of metering must stay
// under.
const meteringBudget = warmLocalP50 / 20 // 10 µs

func meteringEvent(i int) meter.Event {
	// A small fixed key set, which is what the accumulator is designed for: the
	// numeric key has no free-form dimension at all.
	keys := [...]string{"key-a", "key-b", "key-c", "key-d"}
	return meter.Event{
		APIKeyID:     keys[i&3],
		TeamID:       "team-1",
		ModelGroup:   "chat-large",
		Provider:     "self-hosted",
		CredentialID: "acct-1",
		Endpoint:     "/v1/chat/completions",
		Status:       200,
		Tokens:       meter.Tokens{Input: 812, Output: 133, CacheRead: 640},
		CostNano:     41_000,
		Latency:      420 * time.Millisecond,
		TTFT:         38 * time.Millisecond,
		Trace: meter.TraceInfo{
			RequestID: "req-0123456789abcdef",
			TraceID:   "trace-0123456789abcdef",
			Excerpt:   "user: summarise the attached document in three bullet points",
		},
	}
}

// perOp measures one Record call. It uses testing.Benchmark rather than a
// hand-rolled loop so the iteration count adapts to the machine.
func perOp(m *meter.Meter) time.Duration {
	r := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; b.Loop(); i++ {
			m.Record(meteringEvent(i))
		}
	})
	if r.N == 0 {
		return 0
	}
	return time.Duration(r.NsPerOp())
}

func TestScenario16_MeteringOnVsOffWithinBound(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: the metering gate is a measurement, not a unit test")
	}

	off := meter.Off()
	t.Cleanup(func() { _ = off.Close() })
	offCost := perOp(off)

	// ---- steady state -------------------------------------------------------
	//
	// The ring is drained and the spool is shipped, so the trace path is doing
	// its full job rather than failing fast.
	steadyMeter := meter.New(meter.Config{
		Sink:          meter.NopSink(),
		SpoolDir:      t.TempDir(),
		FlushInterval: 50 * time.Millisecond,
		DrainInterval: time.Millisecond,
		ShipInterval:  5 * time.Millisecond,
	})
	t.Cleanup(func() { _ = steadyMeter.Close() })
	steadyCost := perOp(steadyMeter)

	// ---- a full buffer ------------------------------------------------------
	//
	// The ring is tiny and nothing drains it, so every trace push fails. This is
	// the degraded state, and it must be both cheap and VISIBLE.
	fullMeter := meter.New(meter.Config{
		Sink:           meter.NopSink(),
		SpoolDir:       t.TempDir(),
		TraceQueueSize: 2,
		FlushInterval:  -1,
		DrainInterval:  -1,
		ShipInterval:   -1,
	})
	t.Cleanup(func() { _ = fullMeter.Close() })
	fullCost := perOp(fullMeter)

	steadyAdded := steadyCost - offCost
	fullAdded := fullCost - offCost

	t.Logf("metering off:          %v/op", offCost)
	t.Logf("metering on (steady):  %v/op  (+%v, %.4f%% of the %v budget)",
		steadyCost, steadyAdded, 100*float64(steadyAdded)/float64(warmLocalP50), warmLocalP50)
	t.Logf("metering on (full):    %v/op  (+%v, %.4f%% of the %v budget)",
		fullCost, fullAdded, 100*float64(fullAdded)/float64(warmLocalP50), warmLocalP50)
	t.Logf("bound: +%v (5%% of the gateway-overhead budget)", meteringBudget)

	if steadyAdded > meteringBudget {
		t.Errorf("steady state adds %v per request, over the %v bound", steadyAdded, meteringBudget)
	}
	if fullAdded > meteringBudget {
		t.Errorf("a full buffer adds %v per request, over the %v bound", fullAdded, meteringBudget)
	}
	if fullAdded > steadyAdded {
		// Not a failure — it is a wall-clock measurement and the two are within
		// noise of each other on a loaded machine — but the design predicts the
		// opposite, and a persistent inversion means the failed push is no
		// longer skipping the payload copy.
		t.Logf("note: a full buffer measured DEARER than steady state (+%v vs +%v); "+
			"§15.1 predicts the opposite, because a failed ring push skips the payload copy",
			fullAdded, steadyAdded)
	}

	t.Run("inverse: the cheap number is not cheap because nothing was recorded", func(t *testing.T) {
		// A meter that dropped everything would beat every bound above. The
		// numeric path has no drop at all, and the trace path's drops are
		// counted and raise Degraded.
		if st := off.Stats(); st.Recorded != 0 {
			t.Errorf("meter.Off() recorded %d events; the baseline is not a baseline", st.Recorded)
		}

		m := meter.New(meter.Config{
			Sink: meter.NopSink(), SpoolDir: t.TempDir(),
			FlushInterval: -1, DrainInterval: -1, ShipInterval: -1,
		})
		defer func() { _ = m.Close() }()
		const n = 5000
		for i := range n {
			m.Record(meteringEvent(i))
		}
		if st := m.Stats(); st.Recorded != n {
			t.Fatalf("recorded %d of %d events: the numeric path has no drop", st.Recorded, n)
		}
	})

	t.Run("a full buffer drops traces, and the drop is visible", func(t *testing.T) {
		// "Never silently" is the load-bearing half. A meter that dropped
		// traces without raising Degraded would be indistinguishable from one
		// that had no traffic.
		m := meter.New(meter.Config{
			Sink: meter.NopSink(), SpoolDir: t.TempDir(),
			TraceQueueSize: 2,
			FlushInterval:  -1, DrainInterval: -1, ShipInterval: -1,
		})
		defer func() { _ = m.Close() }()
		for i := range 2000 {
			m.Record(meteringEvent(i))
		}
		st := m.Stats()
		if st.TracesDropped == 0 {
			t.Fatal("a 2-slot ring under 2000 events dropped nothing; the full-buffer arm is not full")
		}
		degraded, reason := m.Degraded()
		if !degraded {
			t.Fatalf("%d traces were dropped without raising Degraded", st.TracesDropped)
		}
		if reason == 0 {
			t.Error("Degraded must carry a reason")
		}
		// And the numeric side is untouched by it: this is the whole point of
		// the two-queue split.
		if st.Recorded != 2000 {
			t.Fatalf("the numeric path lost %d events while the trace path was full",
				2000-st.Recorded)
		}
	})

	t.Run("the numeric path survives a sink that refuses every write", func(t *testing.T) {
		// A store outage must cost latency in the reporting surface and never a
		// counter: refused rollups are carried over and merged into the next
		// flush.
		bad := &refusingSink{}
		m := meter.New(meter.Config{
			Sink: bad, SpoolDir: t.TempDir(),
			FlushInterval: -1, DrainInterval: -1, ShipInterval: -1,
		})
		defer func() { _ = m.Close() }()
		for i := range 400 {
			m.Record(meteringEvent(i))
		}
		_ = m.Flush(context.Background())
		st := m.Stats()
		if st.PendingBuckets == 0 {
			t.Fatal("a refused flush discarded the buckets instead of carrying them over")
		}
		if st.Recorded != 400 {
			t.Fatalf("recorded = %d, want 400", st.Recorded)
		}
		// Once the sink recovers the carry-over drains.
		bad.ok.Store(true)
		if err := m.Flush(context.Background()); err != nil {
			t.Fatalf("flush after recovery: %v", err)
		}
		if got := m.Stats().PendingBuckets; got != 0 {
			t.Errorf("%d buckets are still pending after the sink recovered", got)
		}
	})
}

// refusingSink refuses every write until ok is set, standing in for a store
// outage. DESIGN §12.1: a refused rollup is carried over and retried, never
// discarded — a store outage costs latency in the reporting surface and never a
// counter.
type refusingSink struct{ ok atomic.Bool }

func (s *refusingSink) WriteRollups(context.Context, []meter.Bucket) error {
	if s.ok.Load() {
		return nil
	}
	return errSinkDown
}

func (s *refusingSink) WriteTraces(context.Context, []meter.Trace) error {
	if s.ok.Load() {
		return nil
	}
	return errSinkDown
}

var errSinkDown = errors.New("scenario: the sink is refusing writes")
