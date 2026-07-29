package meter

import (
	"fmt"
	"sync"
	"testing"
)

// An event recorded after Close must not vanish.
//
// Before this, it did — and it did so in the worst possible shape. Record added
// the event to a shard, incremented `recorded`, and returned. The shards are
// merged by a flush, Close performs the last flush there will ever be, so the
// count was accepted into a structure nothing would read again. Stats then said
// the event was recorded and the sink never saw it: the numeric path's "no drop
// at all" was false, and metering_degraded — the ONE signal that exists so a
// loss is never silent — reported healthy.
//
// The observable is the operator's: the drop reaches the same Degraded signal a
// dropped trace does, with its own reason, and it is counted.
func TestRecordAfterCloseIsRefusedAndVisible(t *testing.T) {
	sink := NewMemSink()
	m := New(manualConfig(sink))

	m.Record(sampleEvent(0))
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before := m.Stats()
	if before.RecordsRefusedClosed != 0 {
		t.Fatalf("RecordsRefusedClosed = %d before any post-close record", before.RecordsRefusedClosed)
	}
	if deg, _ := m.Degraded(); deg {
		t.Fatalf("the meter is degraded after a clean close: %v", before.Reason)
	}

	const late = 3
	for i := 0; i < late; i++ {
		m.Record(sampleEvent(100 + i))
	}

	st := m.Stats()
	if st.Recorded != before.Recorded {
		t.Errorf("Recorded rose from %d to %d across %d post-close events: they were "+
			"counted as recorded and then discarded, which is the defect",
			before.Recorded, st.Recorded, late)
	}
	if st.RecordsRefusedClosed != late {
		t.Errorf("RecordsRefusedClosed = %d, want %d", st.RecordsRefusedClosed, late)
	}
	if !st.Degraded {
		t.Error("metering_degraded is false after events were lost past Close: this is " +
			"the one signal that exists so a drop is never silent, and it could not " +
			"see the drop")
	}
	if st.Reason != ReasonClosed {
		t.Errorf("degraded reason = %v, want %v", st.Reason, ReasonClosed)
	}
	if got := ReasonClosed.String(); got != "meter_closed" {
		t.Errorf("ReasonClosed spells itself %q", got)
	}

	// And the sink really did not get them, which is what makes the counter a
	// report of a loss rather than a report of a detour.
	if got := sink.Totals().Requests; got != before.Recorded {
		t.Errorf("the sink received %d requests, want %d", got, before.Recorded)
	}
}

// The traces on a post-close event are refused with it. A trace that reached
// the ring after the last drain would be lost in the same silence.
func TestRecordAfterCloseDoesNotEnqueueATrace(t *testing.T) {
	sink := NewMemSink()
	m := New(manualConfig(sink))
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ev := sampleEvent(1)
	ev.Trace.RequestID = "req-after-close"
	m.Record(ev)

	st := m.Stats()
	if st.QueueDepth != 0 {
		t.Errorf("QueueDepth = %d after a post-close record: the trace is sitting in a "+
			"ring nothing will drain", st.QueueDepth)
	}
	if st.TracesRecorded != 0 {
		t.Errorf("TracesRecorded = %d, want 0", st.TracesRecorded)
	}
	if st.RecordsRefusedClosed != 1 {
		t.Errorf("RecordsRefusedClosed = %d, want 1", st.RecordsRefusedClosed)
	}
}

// The window this is actually about: Close racing the callers still finishing.
//
// Every event is either recorded and shipped, or refused and counted. Nothing
// falls between the two — which is the property that was missing, and the one
// a race detector run is worth having on.
func TestCloseRacingRecordLosesNothingUncounted(t *testing.T) {
	const (
		writers = 8
		each    = 200
	)
	sink := NewMemSink()
	m := New(manualConfig(sink))

	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < each; i++ {
				ev := sampleEvent(i)
				ev.Trace.RequestID = fmt.Sprintf("req-%d-%d", w, i)
				m.Record(ev)
			}
		}(w)
	}
	close(start)
	// Close concurrently with the writers, which is the shutdown race itself.
	closeErr := m.Close()
	wg.Wait()
	if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	// Every event landed on exactly one side of the close.
	st := m.Stats()
	total := int64(writers * each)
	if st.Recorded+st.RecordsRefusedClosed != total {
		t.Errorf("recorded %d + refused %d = %d, want %d: some events were neither "+
			"accepted nor counted as lost",
			st.Recorded, st.RecordsRefusedClosed, st.Recorded+st.RecordsRefusedClosed, total)
	}
	if st.RecordsRefusedClosed > 0 && !st.Degraded {
		t.Errorf("%d events were refused past Close and Degraded is false",
			st.RecordsRefusedClosed)
	}
	// A meter that accepted an event it could not flush would show it here: the
	// sink's total is what actually reached durable accounting.
	if got := sink.Totals().Requests; int64(got) > st.Recorded {
		t.Errorf("the sink holds %d requests but only %d were recorded", got, st.Recorded)
	}
}
