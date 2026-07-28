package meter

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestRingFIFOAndFullness(t *testing.T) {
	r := newRing(4, 32, ExcerptText, 8)
	for i := 0; i < 4; i++ {
		tr := Trace{RequestID: fmt.Sprintf("r%d", i)}
		if !r.push(&tr, "abc") {
			t.Fatalf("push %d failed on an empty ring", i)
		}
	}
	tr := Trace{RequestID: "overflow"}
	if r.push(&tr, "abc") {
		t.Fatal("push succeeded on a full ring")
	}
	if got := r.depth(); got != 4 {
		t.Errorf("depth = %d, want 4", got)
	}

	for i := 0; i < 4; i++ {
		var out Trace
		if !r.pop(&out) {
			t.Fatalf("pop %d failed on a non-empty ring", i)
		}
		if want := fmt.Sprintf("r%d", i); out.RequestID != want {
			t.Errorf("pop %d = %s, want %s -- the ring is not FIFO", i, out.RequestID, want)
		}
		if out.Excerpt != "abc" {
			t.Errorf("excerpt = %q, want abc", out.Excerpt)
		}
	}
	var out Trace
	if r.pop(&out) {
		t.Error("pop succeeded on an empty ring")
	}

	// The ring wraps rather than filling permanently.
	for i := 0; i < 20; i++ {
		tr := Trace{RequestID: fmt.Sprintf("w%d", i)}
		if !r.push(&tr, "") {
			t.Fatalf("push %d failed after wrap", i)
		}
		if !r.pop(&out) {
			t.Fatalf("pop %d failed after wrap", i)
		}
	}
}

func TestRingReleasesReferencesOnPop(t *testing.T) {
	r := newRing(2, 32, ExcerptText, 8)
	tr := Trace{RequestID: "a", ErrorMessage: "long error message"}
	if !r.push(&tr, "x") {
		t.Fatal("push failed")
	}
	var out Trace
	r.pop(&out)
	if r.buf[0].val.RequestID != "" || r.buf[0].val.ErrorMessage != "" {
		t.Error("the slot still holds string references after pop; a drained trace stays reachable")
	}
}

func TestRingConcurrentProducersLoseNothing(t *testing.T) {
	const (
		producers = 8
		perP      = 4000
		total     = producers * perP
	)
	r := newRing(1024, 64, ExcerptText, 16)

	var mu sync.Mutex
	got := map[string]int{}
	var consumed int

	var consumers sync.WaitGroup
	stop := make(chan struct{})
	for c := 0; c < 3; c++ {
		consumers.Add(1)
		go func() {
			defer consumers.Done()
			var out Trace
			for {
				if r.pop(&out) {
					mu.Lock()
					got[out.RequestID]++
					consumed++
					mu.Unlock()
					continue
				}
				select {
				case <-stop:
					// One last sweep after the producers are done.
					for r.pop(&out) {
						mu.Lock()
						got[out.RequestID]++
						consumed++
						mu.Unlock()
					}
					return
				default:
				}
			}
		}()
	}

	var dropped int64
	var mu2 sync.Mutex
	var producersWG sync.WaitGroup
	for p := 0; p < producers; p++ {
		producersWG.Add(1)
		go func(p int) {
			defer producersWG.Done()
			var local int64
			for i := 0; i < perP; i++ {
				tr := Trace{RequestID: fmt.Sprintf("p%d-%d", p, i)}
				for !r.push(&tr, "e") {
					local++
					if local > 1e7 {
						return
					}
				}
			}
			mu2.Lock()
			dropped += local
			mu2.Unlock()
		}(p)
	}
	producersWG.Wait()
	close(stop)
	consumers.Wait()

	if consumed != total {
		t.Fatalf("consumed %d, want %d", consumed, total)
	}
	for p := 0; p < producers; p++ {
		for i := 0; i < perP; i++ {
			id := fmt.Sprintf("p%d-%d", p, i)
			if got[id] != 1 {
				t.Fatalf("%s delivered %d times, want exactly 1", id, got[id])
			}
		}
	}
}

func TestRingExcerptTruncation(t *testing.T) {
	// Arena slot of 16 bytes, configured for 8 runes.
	r := newRing(2, 16, ExcerptText, 8)
	tr := Trace{RequestID: "a"}
	r.push(&tr, strings.Repeat("z", 100))
	var out Trace
	r.pop(&out)
	if out.Excerpt != strings.Repeat("z", 8) {
		t.Errorf("excerpt = %q, want 8 z's", out.Excerpt)
	}

	// Arena of 16 bytes with 3-byte runes: 5 whole runes plus one torn byte.
	r2 := newRing(2, 16, ExcerptText, 100)
	r2.push(&tr, strings.Repeat("한", 100))
	r2.pop(&out)
	if out.Excerpt != strings.Repeat("한", 5) {
		t.Errorf("excerpt = %q (%d bytes), want 5 whole runes", out.Excerpt, len(out.Excerpt))
	}
}

func TestRingExcerptModeNoneStoresNothing(t *testing.T) {
	r := newRing(2, 0, ExcerptNone, 512)
	tr := Trace{RequestID: "a"}
	if !r.push(&tr, "secret content") {
		t.Fatal("push failed")
	}
	var out Trace
	r.pop(&out)
	if out.Excerpt != "" {
		t.Errorf("excerpt = %q, want empty under mode none", out.Excerpt)
	}
	if len(r.arena) != 0 {
		t.Errorf("arena is %d bytes under mode none, want 0 -- unused memory should not be reserved", len(r.arena))
	}
}

func TestTruncateRunes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"", 5, ""},
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"한국어", 2, "한국"},
		{"한국어", 10, "한국어"},
	} {
		if got := string(truncateRunes([]byte(tc.in), tc.n)); got != tc.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
	// A sequence cut mid-rune by the arena boundary is dropped whole.
	torn := []byte("ab\xed\x95")
	if got := string(truncateRunes(torn, 10)); got != "ab" {
		t.Errorf("truncateRunes(torn) = %q, want %q", got, "ab")
	}
}

func TestFNVDistribution(t *testing.T) {
	// The sampler's threshold comparison is on the high bits, so those have to
	// be uniform: an unmixed FNV would sample nothing at low rates.
	var buckets [16]int
	const n = 100000
	for i := 0; i < n; i++ {
		buckets[fnv64a(fmt.Sprintf("req-%08x", i))>>60]++
	}
	want := n / 16
	for i, c := range buckets {
		if c < want*8/10 || c > want*12/10 {
			t.Errorf("high-nibble bucket %d has %d of %d, want ~%d (+/-20%%)", i, c, n, want)
		}
	}
}
