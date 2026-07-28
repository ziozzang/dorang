package prefix

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func fakeClock(start time.Time) (func() time.Time, func(time.Duration)) {
	var mu sync.Mutex
	now := start
	return func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}, func(d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(d)
		}
}

func TestLongestPrefixWins(t *testing.T) {
	tab := NewTable(Options{})
	const seg = 16
	shared := bytes.Repeat([]byte("s"), seg*3)

	// A served the shared head plus its own tail.
	a := Compute("g", append(append([]byte{}, shared...), bytes.Repeat([]byte("a"), seg*8)...), seg)
	tab.Record(a, 1)

	// B shares only the head.
	b := Compute("g", append(append([]byte{}, shared...), bytes.Repeat([]byte("b"), seg*8)...), seg)
	tab.Record(b, 2)

	// A's exact conversation must come back to A at full depth, not to B just
	// because B wrote the shallow entries later.
	target, depth, ok := tab.Lookup(a, nil)
	if !ok || target != 1 {
		t.Fatalf("exact match went to %d (ok=%v); want 1", target, ok)
	}
	if depth != len(a) {
		t.Fatalf("matched at depth %d, want the deepest %d", depth, len(a))
	}

	// A conversation that only shares the head must still find a warm backend
	// via a shallower entry rather than missing entirely.
	c := Compute("g", append(append([]byte{}, shared...), bytes.Repeat([]byte("c"), seg*8)...), seg)
	if _, d, ok := tab.Lookup(c, nil); !ok {
		t.Fatal("partial match found nothing; shallow entries are not being recorded")
	} else if d >= len(c) {
		t.Fatalf("partial match claimed depth %d of %d", d, len(c))
	}
}

// Cache affinity must never resurrect a target that is unhealthy or exhausted.
func TestLookupSkipsInvalidTarget(t *testing.T) {
	tab := NewTable(Options{})
	ds := Compute("g", bytes.Repeat([]byte("x"), 200), 16)
	tab.Record(ds, 7)

	if _, _, ok := tab.Lookup(ds, func(uint32) bool { return false }); ok {
		t.Fatal("returned a target the caller rejected")
	}
	if target, _, ok := tab.Lookup(ds, func(id uint32) bool { return id == 7 }); !ok || target != 7 {
		t.Fatalf("accepted target not returned: %d ok=%v", target, ok)
	}
}

func TestTTLExpiry(t *testing.T) {
	now, advance := fakeClock(time.Unix(1700000000, 0))
	tab := NewTable(Options{TTL: time.Hour, Now: now})
	ds := Compute("g", bytes.Repeat([]byte("x"), 200), 16)
	tab.Record(ds, 3)

	if _, _, ok := tab.Lookup(ds, nil); !ok {
		t.Fatal("fresh entry not found")
	}
	advance(2 * time.Hour)
	if _, _, ok := tab.Lookup(ds, nil); ok {
		t.Fatal("expired entry was returned; the upstream cache is assumed cold by then")
	}
}

// Unlike a session pin, a prefix that keeps being used keeps the upstream cache
// warm, so its TTL refreshes on use. This is a deliberate asymmetry.
func TestTTLRefreshesOnUse(t *testing.T) {
	now, advance := fakeClock(time.Unix(1700000000, 0))
	tab := NewTable(Options{TTL: time.Hour, Now: now})
	ds := Compute("g", bytes.Repeat([]byte("x"), 200), 16)
	tab.Record(ds, 3)

	for i := 0; i < 5; i++ {
		advance(50 * time.Minute)
		if _, _, ok := tab.Lookup(ds, nil); !ok {
			t.Fatalf("entry expired at step %d despite continuous use", i)
		}
	}
}

// The table is bounded by bytes, not entries, because per-entry size was
// underestimated 2-3x in an earlier design and one request writes an entry at
// every depth.
func TestByteBudgetEnforced(t *testing.T) {
	const budget = 64 * entryBytes
	tab := NewTable(Options{MaxBytes: budget})

	for i := 0; i < 500; i++ {
		body := append(bytes.Repeat([]byte("p"), 64), []byte(fmt.Sprint(i))...)
		tab.Record(Compute("g", body, 16), uint32(i))
	}

	_, _, evicted, bytesUsed := tab.Stats()
	if evicted == 0 {
		t.Fatal("nothing was evicted despite far exceeding the budget")
	}
	// Eviction reclaims to 87.5% and runs per-insert, so allow headroom for one
	// batch of inserts between sweeps.
	if bytesUsed > budget*3 {
		t.Fatalf("retained %d bytes against a %d budget", bytesUsed, budget)
	}
	if int64(tab.Len())*entryBytes != bytesUsed {
		t.Fatalf("byte accounting drifted: %d entries vs %d bytes", tab.Len(), bytesUsed)
	}
}

// entryBytes is a budget input, so it must not silently understate reality.
func TestEntryBytesIsNotOptimistic(t *testing.T) {
	var e entry
	structSize := unsafe.Sizeof(e)
	keySize := unsafe.Sizeof(Digest{})
	floor := int(structSize + keySize)
	if entryBytes < floor {
		t.Fatalf("entryBytes=%d is below the bare key+value floor of %d", entryBytes, floor)
	}
	// Go map buckets, overflow chaining, and load factor all add real overhead
	// on top of key+value. Refusing anything under 2x that floor is what stops
	// this constant from drifting back toward the optimistic hand count.
	if entryBytes < floor*2 {
		t.Fatalf("entryBytes=%d ignores map overhead; want at least %d", entryBytes, floor*2)
	}
	t.Logf("key %d + value %d = %d floor; budgeting %d", keySize, structSize, floor, entryBytes)
}

func TestConcurrentUse(t *testing.T) {
	tab := NewTable(Options{MaxBytes: 4 << 20})
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				body := []byte(fmt.Sprintf("conversation-%d-%d", g%4, i%20))
				ds := Compute("g", body, 16)
				if _, _, ok := tab.Lookup(ds, nil); !ok {
					tab.Record(ds, uint32(g))
				}
			}
		}(g)
	}
	wg.Wait()
	lookups, hits, _, _ := tab.Stats()
	if lookups == 0 {
		t.Fatal("no lookups recorded")
	}
	if hits == 0 {
		t.Fatal("no hits across repeated conversations; affinity is not working")
	}
	t.Logf("%d lookups, %d hits (%.0f%%)", lookups, hits, 100*float64(hits)/float64(lookups))
}

func TestInterner(t *testing.T) {
	in := NewInterner()
	// Opaque model-shaped names, including every colon form in real use.
	names := []string{"gemma4:31b", "zai:glm-5.1", "deepseek-v4-flash:cloud", "vendor/model-1.5"}
	ids := make([]uint32, len(names))
	for i, n := range names {
		ids[i] = in.ID(n)
	}
	for i, n := range names {
		if got := in.ID(n); got != ids[i] {
			t.Fatalf("%q was reassigned %d -> %d", n, ids[i], got)
		}
		if back, ok := in.Name(ids[i]); !ok || back != n {
			t.Fatalf("id %d resolved to %q, want %q", ids[i], back, n)
		}
	}
	if _, ok := in.Name(9999); ok {
		t.Fatal("unknown id resolved")
	}
}

func TestInternerConcurrent(t *testing.T) {
	in := NewInterner()
	var wg sync.WaitGroup
	seen := make([]uint32, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seen[i] = in.ID("same-deployment")
		}(i)
	}
	wg.Wait()
	for i, id := range seen {
		if id != seen[0] {
			t.Fatalf("goroutine %d got id %d, want %d — interning is not stable", i, id, seen[0])
		}
	}
}

func BenchmarkTableLookupHit(b *testing.B) {
	tab := NewTable(Options{MaxBytes: 64 << 20})
	ds := Compute("g", bytes.Repeat([]byte("x"), 64<<10), DefaultBaseSegment)
	tab.Record(ds, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tab.Lookup(ds, nil)
	}
}

func BenchmarkTableLookupMiss(b *testing.B) {
	tab := NewTable(Options{MaxBytes: 64 << 20})
	ds := Compute("g", bytes.Repeat([]byte("y"), 64<<10), DefaultBaseSegment)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tab.Lookup(ds, nil)
	}
}
