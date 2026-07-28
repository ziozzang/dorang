package prefix

import (
	"testing"
	"time"
)

// The affinity TTL used to be one number for the whole fleet, and the thing it
// models is not fleet-wide: it is how long the BACKEND still holds the KV blocks
// for a prefix. That is roughly five minutes for OpenAI's automatic caching,
// five minutes on Anthropic's default tier and an hour on its extended one, and
// for vLLM and SGLang it is not a duration at all — blocks live until LRU
// eviction under memory pressure.
//
// One hour was therefore wrong in both directions at once: too long for a hosted
// backend, where it pins a conversation to a node that no longer holds the
// prefix and costs load balance for nothing; too short for a self-hosted one,
// where it discards hits that were still there.

func fixedClock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

// TestPerTargetTTLExpiresIndependently: two backends, two lifetimes, one table.
func TestPerTargetTTLExpiresIndependently(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tab := NewTable(Options{TTL: time.Hour, Now: fixedClock(&now)})
	const hosted, selfHosted = uint32(1), uint32(2)
	tab.SetTargetTTLs([]time.Duration{hosted: 5 * time.Minute, selfHosted: time.Hour})

	hostedChain := Compute("m", []byte("a hosted conversation prefix"), 8)
	selfChain := Compute("m", []byte("a self-hosted conversation prefix"), 8)
	tab.Record(hostedChain, hosted)
	tab.Record(selfChain, selfHosted)

	now = now.Add(10 * time.Minute)

	if _, _, ok := tab.Lookup(hostedChain, nil); ok {
		t.Error("a five-minute cache still matched ten minutes later")
	}
	if _, _, ok := tab.Lookup(selfChain, nil); !ok {
		t.Error("an hour-long cache was expired by another backend's five minutes")
	}
}

// TestUntilEvictedNeverExpiresOnAClock is the self-hosted case: vLLM and SGLang
// have no cache clock, so an affinity entry there must survive any amount of
// wall time and be bounded only by the table's byte budget.
func TestUntilEvictedNeverExpiresOnAClock(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tab := NewTable(Options{TTL: time.Minute, Now: fixedClock(&now)})
	const engine = uint32(3)
	tab.SetTargetTTLs([]time.Duration{engine: -1})

	chain := Compute("m", []byte("a long-lived prefix on a self-hosted engine"), 8)
	tab.Record(chain, engine)

	now = now.Add(30 * 24 * time.Hour)
	if _, _, ok := tab.Lookup(chain, nil); !ok {
		t.Fatal("until_evicted expired on a clock")
	}
}

// TestUntilEvictedIsStillBoundedByBytes is the other half, and the one worth
// checking before promising the setting: "no clock" must not mean "no bound".
// The byte budget's second eviction pass drops the coldest entries whether or
// not they have a lifetime, so the table stays bounded.
func TestUntilEvictedIsStillBoundedByBytes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// Room for a few hundred entries, so the budget is reached long before the
	// loop ends.
	const budget = 200 * entryBytes
	tab := NewTable(Options{MaxBytes: budget, TTL: time.Hour, Now: fixedClock(&now)})
	const engine = uint32(1)
	tab.SetTargetTTLs([]time.Duration{engine: -1})

	for i := 0; i < 5000; i++ {
		tab.Record(Compute("m", []byte("prefix "+string(rune('A'+i%26))+string(rune(i))), 4), engine)
		now = now.Add(time.Second)
	}
	if _, _, _, bytes := tab.Stats(); bytes > budget {
		t.Fatalf("table holds %d bytes against a %d budget: until_evicted is unbounded", bytes, budget)
	}
	if _, _, evicted, _ := tab.Stats(); evicted == 0 {
		t.Fatal("nothing was ever evicted, so the budget was never reached and this proves nothing")
	}
}

// TestTargetTTLsAreReplacedWholesale: a hot reload must not leave a lifetime
// behind for a deployment the new configuration removed.
func TestTargetTTLsAreReplacedWholesale(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tab := NewTable(Options{TTL: 10 * time.Minute, Now: fixedClock(&now)})
	tab.SetTargetTTLs([]time.Duration{1: -1})

	chain := Compute("m", []byte("prefix"), 8)
	tab.Record(chain, 1)
	now = now.Add(time.Hour)
	if _, _, ok := tab.Lookup(chain, nil); !ok {
		t.Fatal("until_evicted did not apply")
	}

	// The deployment is gone from the new configuration; the table default is
	// what governs now.
	tab.SetTargetTTLs(nil)
	tab.Record(chain, 1)
	now = now.Add(time.Hour)
	if _, _, ok := tab.Lookup(chain, nil); ok {
		t.Fatal("a removed deployment's lifetime outlived the reload")
	}
}

// TestTableTTLNegativeMeansNoClock covers the whole table taking the
// self-hosted default, which is what routing.prefix.ttl: until_evicted does.
func TestTableTTLNegativeMeansNoClock(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tab := NewTable(Options{TTL: -1, Now: fixedClock(&now)})
	chain := Compute("m", []byte("prefix"), 8)
	tab.Record(chain, 7)
	now = now.Add(365 * 24 * time.Hour)
	if _, _, ok := tab.Lookup(chain, nil); !ok {
		t.Fatal("a table-wide until_evicted expired on a clock")
	}
}
