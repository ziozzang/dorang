package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// benchAuth returns an authenticator whose snapshot already holds the token,
// which is the steady state: the hot path never touches the store.
func benchAuth(b *testing.B, scheme Scheme, withRehasher bool) (*Authenticator, *fakeStore) {
	b.Helper()
	legacy := LegacyPolicy{Enabled: true, Until: testNow.Add(365 * 24 * time.Hour)}
	h, err := NewHasher(testPepper, legacy)
	if err != nil {
		b.Fatal(err)
	}
	d, err := h.Hash(scheme, testToken)
	if err != nil {
		b.Fatal(err)
	}
	store := newFakeStore()
	cfg := Config{
		Pepper: testPepper, MasterKey: testMaster, Legacy: legacy,
		Now: func() time.Time { return testNow },
	}
	if withRehasher {
		cfg.Store = store
		cfg.RehashOnUse = true
	}
	a, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(a.Close)
	rec := Record{Lookup: LookupKey(testToken), Digest: d, Scheme: scheme,
		Principal: Principal{KeyID: "bench", Key: Limits{Models: []string{"model-x"}}}}
	store.put(rec)
	if err := a.Load([]Record{rec}); err != nil {
		b.Fatal(err)
	}
	return a, store
}

// BenchmarkAuthenticateCached is the hot path: sha256 of the credential, one
// map lookup against the lock-free snapshot, and a constant-time digest
// comparison. It must not allocate.
func BenchmarkAuthenticateCached(b *testing.B) {
	a, _ := benchAuth(b, SchemeDorangV1, false)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := a.Authenticate(ctx, testToken); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCheckCached adds authorization of a model and a route.
func BenchmarkCheckCached(b *testing.B) {
	a, _ := benchAuth(b, SchemeDorangV1, false)
	ctx := context.Background()
	access := Access{Now: testNow, Model: "model-x", Route: "/v1/chat/completions"}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := a.Check(ctx, testToken, access); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAuthenticateCachedParallel measures contention: the snapshot read is
// lock-free, so throughput should scale with cores.
func BenchmarkAuthenticateCachedParallel(b *testing.B) {
	a, _ := benchAuth(b, SchemeDorangV1, false)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := a.Authenticate(ctx, testToken); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkAuthenticateLegacy is the same path against a legacy row.
func BenchmarkAuthenticateLegacy(b *testing.B) {
	a, _ := benchAuth(b, SchemeLegacySHA256, false)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := a.Authenticate(ctx, testToken); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAuthenticateLegacyRehashOnUse is the pair to compare against
// BenchmarkAuthenticateLegacy: the asynchronous upgrade must not show up in the
// request that triggers it. Only the first iteration enqueues; the rest see the
// entry already flagged.
func BenchmarkAuthenticateLegacyRehashOnUse(b *testing.B) {
	a, _ := benchAuth(b, SchemeLegacySHA256, true)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := a.Authenticate(ctx, testToken); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHasherDigests(b *testing.B) {
	h, err := NewHasher(testPepper, LegacyPolicy{})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		h.digests(testToken)
	}
}

func BenchmarkExtract(b *testing.B) {
	hdr := http.Header{}
	hdr.Set(HeaderAuthorization, "Bearer "+testToken)
	hdr.Set("Content-Type", "application/json")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, ok := Extract(hdr); !ok {
			b.Fatal("no credential")
		}
	}
}

func BenchmarkStrip(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		hdr := http.Header{}
		for _, n := range Headers() {
			hdr.Set(n, testToken)
		}
		hdr.Set("Content-Type", "application/json")
		b.StartTimer()
		Strip(hdr)
	}
}

// TestHotPathIsAllocationFree turns the benchmark's allocation claim into an
// assertion, so a change that starts allocating on the hot path fails the test
// suite rather than quietly costing throughput (DESIGN §15.5).
func TestHotPathIsAllocationFree(t *testing.T) {
	legacy := LegacyPolicy{Enabled: true, Until: testNow.Add(time.Hour)}
	h := testHasher(t, legacy)
	a := newAuth(t, Config{Legacy: legacy})
	if err := a.Load([]Record{
		record(t, h, testToken, SchemeDorangV1, Principal{
			KeyID: "k", Key: Limits{Models: []string{"model-x"}, AllowedRoutes: []string{"/v1/chat/completions"}},
		}),
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	access := Access{Now: testNow, Model: "model-x", Route: "/v1/chat/completions"}

	cases := []struct {
		name string
		fn   func()
	}{
		{"digests", func() { h.digests(testToken) }},
		{"lookup", func() { h.Lookup(testToken) }},
		{"authenticate", func() {
			if _, err := a.Authenticate(ctx, testToken); err != nil {
				t.Fatal(err)
			}
		}},
		{"check", func() {
			if _, err := a.Check(ctx, testToken, access); err != nil {
				t.Fatal(err)
			}
		}},
		{"master", func() {
			if _, err := a.Authenticate(ctx, testMaster); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		if n := testing.AllocsPerRun(200, c.fn); n != 0 {
			t.Errorf("%s allocates %.1f times per call, want 0", c.name, n)
		}
	}
}

// TestVerifyTimingDoesNotDependOnWhereTheDigestDiffers is a coarse smoke test.
// The real guarantee is structural — crypto/subtle for both the selection and
// the comparison, and both digests computed unconditionally, which
// TestVerifyComputesBothDigests covers — but a comparison that bailed out on
// the first differing byte would show up here as a large asymmetry.
func TestVerifyTimingDoesNotDependOnWhereTheDigestDiffers(t *testing.T) {
	if testing.Short() {
		t.Skip("timing smoke test")
	}
	h := testHasher(t, LegacyPolicy{})
	good, _ := h.Hash(SchemeDorangV1, testToken)
	early, late := good, good
	early[0] ^= 0xff
	late[len(late)-1] ^= 0xff

	measure := func(d Digest) time.Duration {
		const n = 20000
		start := time.Now()
		for range n {
			_ = h.VerifyToken(SchemeDorangV1, d, testToken, testNow)
		}
		return time.Since(start) / n
	}
	// Warm up, then take the best of three to damp scheduler noise.
	measure(early)
	best := func(d Digest) time.Duration {
		b := measure(d)
		for range 2 {
			if v := measure(d); v < b {
				b = v
			}
		}
		return b
	}
	e, l := best(early), best(late)
	t.Logf("first-byte difference %v, last-byte difference %v", e, l)
	ratio := float64(e) / float64(l)
	if ratio < 0.25 || ratio > 4 {
		t.Fatalf("verification time depends on where the digest differs: %v vs %v", e, l)
	}
}
