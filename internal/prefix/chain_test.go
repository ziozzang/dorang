package prefix

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// The whole point of the chain is that a match at depth i proves the first i
// segments are byte-identical AND in that order. Everything else in this file
// is downstream of that property, so it is tested hardest.

func TestOrderSensitivity(t *testing.T) {
	const seg = 64
	mk := func(parts ...string) []byte {
		var b bytes.Buffer
		for _, p := range parts {
			// pad each part to a full segment so parts land on segment
			// boundaries and a reorder is not masked by re-chunking
			b.WriteString(p)
			for b.Len()%seg != 0 {
				b.WriteByte('.')
			}
		}
		return b.Bytes()
	}

	base := Compute("t", "g", mk("alpha", "beta", "gamma"), seg)

	cases := []struct {
		name string
		body []byte
	}{
		{"swapped", mk("beta", "alpha", "gamma")},
		{"reversed", mk("gamma", "beta", "alpha")},
		{"inserted", mk("alpha", "delta", "beta", "gamma")},
		{"deleted", mk("alpha", "gamma")},
		{"appended", mk("alpha", "beta", "gamma", "omega")},
		{"prepended", mk("omega", "alpha", "beta", "gamma")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			other := Compute("t", "g", tc.body, seg)
			// A permutation must not reproduce the full chain.
			if len(other) == len(base) && digestsEqual(base, other) {
				t.Fatalf("%s produced an identical chain; order is not being enforced", tc.name)
			}
			// Sharper: the first differing segment must break every deeper
			// digest, never just the one where the difference occurred.
			firstDiff := -1
			for i := 0; i < len(base) && i < len(other); i++ {
				if base[i] != other[i] {
					firstDiff = i
					break
				}
			}
			if firstDiff >= 0 {
				for i := firstDiff; i < len(base) && i < len(other); i++ {
					if base[i] == other[i] {
						t.Fatalf("digest at depth %d matched after divergence at depth %d: "+
							"a later segment recovered a broken chain", i, firstDiff)
					}
				}
			}
		})
	}
}

// A permutation sweep: no rearrangement of distinct blocks may ever collide.
func TestPermutationsNeverCollide(t *testing.T) {
	const seg = 32
	blocks := [][]byte{
		bytes.Repeat([]byte("a"), seg),
		bytes.Repeat([]byte("b"), seg),
		bytes.Repeat([]byte("c"), seg),
		bytes.Repeat([]byte("d"), seg),
	}
	seen := map[Digest][]int{}
	var perm func([]int, []int)
	perm = func(cur, rest []int) {
		if len(rest) == 0 {
			var body []byte
			for _, i := range cur {
				body = append(body, blocks[i]...)
			}
			ds := Compute("t", "g", body, seg)
			final := ds[len(ds)-1]
			if prev, dup := seen[final]; dup {
				t.Fatalf("permutation %v collided with %v", cur, prev)
			}
			seen[final] = append([]int(nil), cur...)
			return
		}
		for i := range rest {
			next := append(append([]int{}, rest[:i]...), rest[i+1:]...)
			perm(append(cur, rest[i]), next)
		}
	}
	perm(nil, []int{0, 1, 2, 3})
	if len(seen) != 24 {
		t.Fatalf("expected 24 distinct permutations, got %d", len(seen))
	}
}

// Length is folded into each step precisely so two different splits of the same
// bytes cannot hash alike.
func TestLengthDisambiguatesBoundaries(t *testing.T) {
	a := Compute("t", "g", []byte("aabb"), 2) // segments "aa","bb"
	b := Compute("t", "g", []byte("aabb"), 4) // segment  "aabb"
	if a[len(a)-1] == b[len(b)-1] {
		t.Fatal("different segmentations of the same bytes produced the same digest")
	}
}

func TestGroupIsolation(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 100)
	a := Compute("t", "group-a", body, 32)
	b := Compute("t", "group-b", body, 32)
	for i := range a {
		if a[i] == b[i] {
			t.Fatalf("identical bytes in different groups shared digest at depth %d", i)
		}
	}
}

// A common prefix must match to exactly the depth it is actually shared, and no
// further. This is what "longest common prefix wins" rests on.
func TestSharedPrefixMatchesToDivergence(t *testing.T) {
	const seg = 16
	shared := bytes.Repeat([]byte("s"), seg*3)
	a := Compute("t", "g", append(append([]byte{}, shared...), bytes.Repeat([]byte("a"), seg*4)...), seg)
	b := Compute("t", "g", append(append([]byte{}, shared...), bytes.Repeat([]byte("b"), seg*4)...), seg)

	matched := 0
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			break
		}
		matched++
	}
	if matched == 0 {
		t.Fatal("no shared depth at all despite an identical head")
	}
	if matched == len(a) {
		t.Fatal("chains matched fully despite diverging bodies")
	}
	t.Logf("shared to depth %d of %d", matched, len(a))
}

// Segment sizes double, so entry count is logarithmic rather than linear. A
// fixed-size chunking would put ~4000 entries in the table for a 16 MiB body.
func TestDepthIsLogarithmic(t *testing.T) {
	for _, size := range []int{4 << 10, 1 << 20, 16 << 20} {
		ds := Compute("t", "g", make([]byte, size), DefaultBaseSegment)
		linear := size / DefaultBaseSegment
		if len(ds) > 24 {
			t.Fatalf("size %d produced %d digests, above the depth guard", size, len(ds))
		}
		t.Logf("size %-9d depth %2d (fixed chunking would be %d)", size, len(ds), linear)
		if size > DefaultBaseSegment*4 && len(ds) >= linear {
			t.Fatalf("size %d: depth %d is not sub-linear vs %d", size, len(ds), linear)
		}
	}
}

// Streaming in arbitrary pieces must equal the whole-buffer result, because the
// hot path feeds bytes as they arrive off the wire.
func TestStreamingMatchesWholeBuffer(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	body := make([]byte, 300000)
	rng.Read(body)
	want := Compute("t", "g", body, DefaultBaseSegment)

	for _, chunk := range []int{1, 7, 4095, 4096, 4097, 65536} {
		c := NewChain("t", "g", DefaultBaseSegment)
		for off := 0; off < len(body); off += chunk {
			end := off + chunk
			if end > len(body) {
				end = len(body)
			}
			if _, err := c.Write(body[off:end]); err != nil {
				t.Fatal(err)
			}
		}
		got := c.Seal()
		if !digestsEqual(want, got) {
			t.Fatalf("chunk size %d diverged from whole-buffer result", chunk)
		}
	}
}

// A short conversation still gets a digest — those benefit most from landing on
// a warm backend, so producing nothing would defeat the feature.
func TestShortBodyStillProducesDigest(t *testing.T) {
	ds := Compute("t", "g", []byte("hi"), DefaultBaseSegment)
	if len(ds) != 1 {
		t.Fatalf("want 1 digest for a short body, got %d", len(ds))
	}
	if ds[0] == (Digest{}) {
		t.Fatal("digest is zero")
	}
}

func TestEmptyBodyProducesNoDigest(t *testing.T) {
	if ds := Compute("t", "g", nil, DefaultBaseSegment); len(ds) != 0 {
		t.Fatalf("want 0 digests for an empty body, got %d", len(ds))
	}
}

func TestSealIsIdempotent(t *testing.T) {
	c := NewChain("t", "g", 16)
	_, _ = c.Write(bytes.Repeat([]byte("z"), 100))
	first := append([]Digest{}, c.Seal()...)
	second := c.Seal()
	if !digestsEqual(first, second) {
		t.Fatal("second Seal changed the result")
	}
	// Writing after sealing must not corrupt what was already reported.
	_, _ = c.Write([]byte("more"))
	if !digestsEqual(first, c.Seal()) {
		t.Fatal("write after seal mutated the chain")
	}
}

func TestDepthGuard(t *testing.T) {
	c := NewChain("t", "g", 1)
	_, _ = c.Write(bytes.Repeat([]byte("q"), 1<<20))
	if got := len(c.Seal()); got > MaxDepth {
		t.Fatalf("depth %d exceeded the guard of %d", got, MaxDepth)
	}
}

func digestsEqual(a, b []Digest) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func BenchmarkChain(b *testing.B) {
	for _, size := range []int{4 << 10, 32 << 10, 400 << 10, 16 << 20} {
		body := make([]byte, size)
		rand.New(rand.NewSource(1)).Read(body)
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Compute("t", "group", body, DefaultBaseSegment)
			}
		})
	}
}
