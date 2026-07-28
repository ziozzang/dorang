package pricing

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// benchCandidates builds n candidate deployments the way cost-based routing sees them:
// distinct provider, model, credential and deployment per candidate.
func benchCandidates(n int) []Request {
	out := make([]Request, n)
	for i := range out {
		out[i] = Request{
			Provider:   fmt.Sprintf("prov-%d", i),
			Model:      fmt.Sprintf("model-%d:variant", i),
			Credential: fmt.Sprintf("cred-%d", i),
			Deployment: fmt.Sprintf("dep-%d", i),

			InputTokens: 12_000, OutputTokens: 800, CacheReadTokens: 4_000,
			At: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
		}
	}
	return out
}

// benchCatalog writes a catalog of n rules covering all seven specificity levels, spread
// so that different candidates win at different levels, plus a large tail of rules that
// can never match. Without the index every one of those would be evaluated per candidate.
func benchCatalog(nRules, nCandidates int, extraClasses bool) string {
	var b strings.Builder
	b.WriteString("currency: USD\nrules:\n")
	rate := func(i int) string { return fmt.Sprintf("0.%02d", i%90+5) }
	n := 0
	emit := func(id, match string, i int) {
		fmt.Fprintf(&b, "  - { id: %s, match: {%s}, unit: per_1m_tokens, input: \"%s\", output: \"%s\", cache_read: \"%s\" }\n",
			id, match, rate(i), rate(i+3), rate(i+7))
		n++
	}
	for i := 0; i < nCandidates && n < nRules-1; i++ {
		switch i % 7 {
		case 0:
			emit(fmt.Sprintf("cred-rule-%d", i), fmt.Sprintf(" credential: cred-%d ", i), i)
		case 1:
			emit(fmt.Sprintf("dep-rule-%d", i), fmt.Sprintf(" deployment: dep-%d ", i), i)
		case 2:
			emit(fmt.Sprintf("pm-rule-%d", i), fmt.Sprintf(" provider: prov-%d, model: \"model-%d:variant\" ", i, i), i)
		case 3:
			emit(fmt.Sprintf("model-rule-%d", i), fmt.Sprintf(" model: \"model-%d:variant\" ", i), i)
		case 4:
			emit(fmt.Sprintf("prefix-rule-%d", i), fmt.Sprintf(" model_prefix: \"model-%d\" ", i), i)
		case 5:
			emit(fmt.Sprintf("prov-rule-%d", i), fmt.Sprintf(" provider: prov-%d ", i), i)
		case 6:
			// left to the default rule
		}
	}
	// Filler: rules for models and providers that do not exist in this workload.
	for i := 0; n < nRules-1; i++ {
		switch i % 3 {
		case 0:
			emit(fmt.Sprintf("other-model-%d", i), fmt.Sprintf(" model: \"absent-%d:variant\" ", i), i)
		case 1:
			emit(fmt.Sprintf("other-prefix-%d", i), fmt.Sprintf(" model_prefix: \"absent-%d\" ", i), i)
		case 2:
			emit(fmt.Sprintf("other-cred-%d", i), fmt.Sprintf(" credential: absent-cred-%d ", i), i)
		}
	}
	emit("catch-all", "", 1)
	if extraClasses {
		b.WriteString("  - { id: sub-plan, class: fixed_subscription, match: { credential: cred-0 }, amount_per_period: \"20.00\" }\n")
		b.WriteString("  - { id: adj-margin, class: adjustment, match: {}, op: percent, amount: \"15\", order: 10 }\n")
		b.WriteString("  - { id: adj-tax, class: adjustment, match: {}, op: percent, amount: \"10\", order: 20 }\n")
	}
	return b.String()
}

// BenchmarkPrice is the §8.2 workload: 100 rules, 10 candidate deployments, one Price
// call per candidate. It also asserts the point of the index — the number of rules a
// request actually examines must stay small however large the catalog is.
func BenchmarkPrice(b *testing.B) {
	c, err := ParseCatalog([]byte(benchCatalog(100, 10, false)))
	if err != nil {
		b.Fatal(err)
	}
	if got := len(c.Rules()); got != 100 {
		b.Fatalf("catalog has %d rules, want 100", got)
	}
	cands := benchCandidates(10)

	// Assert the index is doing the work before measuring: pricing every one of the ten
	// candidates against a hundred rules must not examine a hundred rules.
	ev := c.NewEvaluator()
	for _, req := range cands {
		if _, err := ev.Price(req); err != nil {
			b.Fatal(err)
		}
	}
	const maxExamined = 3 // per candidate
	if ev.Examined() > len(cands)*maxExamined {
		b.Fatalf("examined %d rules for %d candidates against a 100-rule catalog: "+
			"the index is not being used", ev.Examined(), len(cands))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Price(cands[i%len(cands)]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPriceAllClasses adds a subscription rule and two adjustments, which is the
// full composition path of §8.1.
func BenchmarkPriceAllClasses(b *testing.B) {
	c, err := ParseCatalog([]byte(benchCatalog(100, 10, true)))
	if err != nil {
		b.Fatal(err)
	}
	cands := benchCandidates(10)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Price(cands[i%len(cands)]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRouteTenCandidates is what cost-based routing actually does: price every
// candidate for one incoming request, through one memoizing Evaluator.
func BenchmarkRouteTenCandidates(b *testing.B) {
	c, err := ParseCatalog([]byte(benchCatalog(100, 10, false)))
	if err != nil {
		b.Fatal(err)
	}
	cands := benchCandidates(10)
	ev := c.NewEvaluator()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev.Reset()
		best := int64(1 << 62)
		for j := range cands {
			cost, err := ev.Price(cands[j])
			if err != nil {
				b.Fatal(err)
			}
			if cost.MarginalNano < best {
				best = cost.MarginalNano
			}
		}
	}
}

// TestIndexKeepsWorkIndependentOfCatalogSize is the deterministic form of the benchmark's
// assertion: growing the catalog tenfold must not grow the work one request does.
func TestIndexKeepsWorkIndependentOfCatalogSize(t *testing.T) {
	cands := benchCandidates(10)
	var counts []int
	for _, n := range []int{100, 1000} {
		c := mustCatalog(t, benchCatalog(n, 10, true))
		if got := len(c.Rules()); got != n+3 {
			t.Fatalf("catalog has %d rules, want %d", got, n+3)
		}
		ev := c.NewEvaluator()
		for _, req := range cands {
			if _, err := ev.Price(req); err != nil {
				t.Fatal(err)
			}
		}
		counts = append(counts, ev.Examined())
	}
	if counts[0] != counts[1] {
		t.Fatalf("examined %d rules with 100 rules but %d with 1000: the index is not keying "+
			"on the static dimensions", counts[0], counts[1])
	}
	if perCandidate := counts[0] / len(cands); perCandidate > 6 {
		t.Fatalf("examined %d rules per candidate, want a handful", perCandidate)
	}
}

func TestEvaluatorMemoizesPerCandidate(t *testing.T) {
	c := mustCatalog(t, benchCatalog(100, 10, false))
	cands := benchCandidates(10)
	ev := c.NewEvaluator()

	first, err := ev.Price(cands[0])
	if err != nil {
		t.Fatal(err)
	}
	afterFirst := ev.Examined()
	for i := 0; i < 20; i++ {
		again, err := ev.Price(cands[0])
		if err != nil {
			t.Fatal(err)
		}
		if !sameAmounts(again, first) {
			t.Fatalf("memoized result differs: %+v then %+v", first, again)
		}
	}
	if ev.Examined() != afterFirst {
		t.Fatalf("repricing the same candidate examined more rules (%d then %d): "+
			"the per-request memo is not working", afterFirst, ev.Examined())
	}
	ev.Reset()
	if ev.Examined() != 0 {
		t.Fatal("Reset must clear the counter")
	}
	if _, err := ev.Price(cands[0]); err != nil {
		t.Fatal(err)
	}
	if ev.Examined() != afterFirst {
		t.Fatalf("after Reset the work should be done again: %d, want %d", ev.Examined(), afterFirst)
	}
}
