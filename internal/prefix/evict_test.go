package prefix

import (
	"encoding/binary"
	"testing"
	"time"
)

// digestsFor builds n distinct digests. It bypasses Compute deliberately: what
// is under test is the eviction arithmetic, and a chain long enough to overflow
// the budget by thousands of entries would otherwise take a body of gigabytes.
func digestsFor(n int) []Digest {
	out := make([]Digest, n)
	for i := range out {
		binary.LittleEndian.PutUint64(out[i][:8], uint64(i)+1)
		binary.LittleEndian.PutUint64(out[i][8:], uint64(i)*0x9e3779b97f4a7c15)
	}
	return out
}

// TestEvictReachesItsTargetInOnePass is the regression for an eviction sweep
// that freed a constant 64 entries — one per shard — however far over budget
// the table was.
//
// The constant is what made it a hot-path defect rather than a slow sweep:
// [Table.Record] runs evict whenever the table is over budget, so a sweep that
// does not reach the target is re-run by the next Record, and the next, each
// time taking every shard's write lock — the lock Lookup blocks on, on the
// routing path.
func TestEvictReachesItsTargetInOnePass(t *testing.T) {
	const budget = 1000
	now := time.Unix(1_700_000_000, 0)
	tab := NewTable(Options{
		MaxBytes: budget * entryBytes,
		// Nothing expires on a clock, so pass one frees nothing and pass two —
		// the one that was broken — is the only thing that can reach the target.
		TTL: -1,
		Now: fixedClock(&now),
	})

	// One Record, one evict: 6,000 entries against a 1,000-entry budget, a
	// deficit of 5,125 against the 87.5% target. The old sweep freed 64.
	tab.Record(digestsFor(6000), 1)

	target := int64(budget*entryBytes) - int64(budget*entryBytes)/8
	if _, _, _, bytes := tab.Stats(); bytes > target {
		t.Fatalf("one sweep left %d bytes against a %d target (%d over): evict does not "+
			"reach its target, so every Record re-runs it under the shard lock",
			bytes, target, bytes-target)
	}
	if int64(tab.Len())*entryBytes != tab.bytes() {
		t.Fatalf("byte accounting drifted: %d entries vs %d bytes", tab.Len(), tab.bytes())
	}

	// What survives must be the coldest-first choice, not an arbitrary 87.5%.
	// Every entry above was written at one instant; these arrive a minute later
	// and are therefore the warmest in the table, so no sweep may drop one while
	// a colder entry remains.
	warm := digestsFor(6200)[6000:] // 200 entries, well inside the 875 target
	now = now.Add(time.Minute)
	for _, d := range warm {
		tab.Record([]Digest{d}, 2)
	}
	for _, d := range warm {
		if _, _, ok := tab.Lookup([]Digest{d}, nil); !ok {
			t.Fatal("a recently used entry was evicted ahead of colder ones")
		}
	}
}

// TestEvictIsNotRerunPerRecord pins the consequence: once a sweep has reached
// the target, the inserts that follow do not each trigger another one. The
// eviction counter is the observable — it may only grow by about what was
// inserted, not by a multiple of it.
func TestEvictIsNotRerunPerRecord(t *testing.T) {
	const budget = 1000
	now := time.Unix(1_700_000_000, 0)
	tab := NewTable(Options{MaxBytes: budget * entryBytes, TTL: -1, Now: fixedClock(&now)})

	ds := digestsFor(4000)
	tab.Record(ds, 1) // over budget; one sweep brings it to the target

	_, _, before, _ := tab.Stats()
	const inserts = 200
	for i := 0; i < inserts; i++ {
		tab.Record(digestsFor(5000)[4000+i:4000+i+1], 1)
		now = now.Add(time.Second)
	}
	_, _, after, _ := tab.Stats()

	// Steady state: the table sits at 87.5% of budget and each insert can cost
	// at most one entry's worth of eviction, amortized over the 12.5% headroom.
	// Anything near inserts*shardCount means the sweep is running per Record.
	if got := after - before; got > inserts {
		t.Fatalf("%d entries evicted over %d inserts: the sweep is running per Record",
			got, inserts)
	}
}

// BenchmarkRecordOverBudget is the guard on the hot-path cost. Record is on the
// routing path and it is what calls evict, so a sweep that stops reaching its
// target shows up here as an order-of-magnitude regression rather than as a
// slow production node nobody attributes to eviction.
func BenchmarkRecordOverBudget(b *testing.B) {
	const budget = 2000
	now := time.Unix(1_700_000_000, 0)
	tab := NewTable(Options{MaxBytes: budget * entryBytes, TTL: -1, Now: fixedClock(&now)})

	// Fill to well over budget first, so every iteration below records into a
	// table that is already at the eviction threshold.
	ds := digestsFor(64_000)
	tab.Record(ds[:8000], 1)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tab.Record(ds[8000+i%56_000:8000+i%56_000+1], 1)
	}
}

// bytes is the retained size, for tests that assert accounting.
func (t *Table) bytes() int64 { return t.curBytes.Load() }
