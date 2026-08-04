package capacity

import (
	"testing"
)

// Two brokers sharing a ceiling admit the ceiling, not twice it.
//
// This is the defect, reproduced in a unit test. Measured first against a real
// two-node cluster and an upstream that counted its own concurrency: a
// credential ceiling of 2 admitted FOUR at once, exactly two per node, twice in
// a row, with `capacity_leases` holding only the leadership row.
//
// The assertion is the SUM across brokers, because that is what the provider
// sees. Asserting each broker's own count would pass throughout the defect —
// each was always correct about itself, and being correct about itself is
// precisely the problem.
func TestTwoNodesShareACeilingRatherThanEachTakingIt(t *testing.T) {
	const ceiling = 4

	newBroker := func(nodes int) *Broker {
		b := New(Config{Global: ceiling})
		b.SetShare(nodes)
		return b
	}

	// One node: the whole ceiling, which is today's behaviour and must not move.
	solo := newBroker(1)
	if got := admitAll(solo, ceiling*3); got != ceiling {
		t.Errorf("one node admitted %d against a ceiling of %d", got, ceiling)
	}

	// Two nodes: each takes half, so the two together admit the ceiling.
	a, b := newBroker(2), newBroker(2)
	total := admitAll(a, ceiling*3) + admitAll(b, ceiling*3)
	if total != ceiling {
		t.Errorf("two nodes admitted %d against a configured ceiling of %d; the provider "+
			"sees the sum, and %d is what it saw before the ceilings were divided",
			total, ceiling, ceiling*2)
	}

	// Four nodes over a ceiling of four: one each.
	var sum int
	for i := 0; i < 4; i++ {
		sum += admitAll(newBroker(4), ceiling*3)
	}
	if sum != ceiling {
		t.Errorf("four nodes admitted %d against a ceiling of %d", sum, ceiling)
	}
}

// A ceiling smaller than the cluster keeps one slot per node, and says so.
//
// Dividing 1 by 3 is 0, and a node with a ceiling of 0 can never serve that
// axis — a cluster of zeroes serves nothing, which is a worse failure than a
// bounded overshoot. So each node keeps 1, the cluster can exceed the ceiling
// by (nodes - ceiling), and [ShareOvershoot] publishes that rather than leaving
// it to be found in production.
func TestACeilingBelowTheNodeCountOvershootsAndPublishesIt(t *testing.T) {
	const ceiling = 1
	var sum int
	for i := 0; i < 3; i++ {
		b := New(Config{Global: ceiling})
		b.SetShare(3)
		sum += admitAll(b, 5)
	}
	if sum != 3 {
		t.Fatalf("three nodes over a ceiling of 1 admitted %d, want 3 (one each)", sum)
	}
	if got := ShareOvershoot(ceiling, 3); got != 2 {
		t.Errorf("ShareOvershoot(1, 3) = %d, want 2 — the figure a cluster can exceed by "+
			"must be published, not discovered", got)
	}
	// And it is zero wherever the division is clean, which is every ordinary
	// configuration.
	for _, tc := range []struct{ limit, nodes int }{{16, 2}, {4, 4}, {64, 3}, {8, 1}} {
		if got := ShareOvershoot(tc.limit, tc.nodes); got != 0 {
			t.Errorf("ShareOvershoot(%d, %d) = %d, want 0", tc.limit, tc.nodes, got)
		}
	}
}

// An unclustered broker is untouched.
//
// Every existing test in this package was written against a broker that carries
// the whole ceiling, and a divisor of 0 or 1 must mean exactly that — the
// change has to be invisible to a single node.
func TestASingleNodeKeepsTheWholeCeiling(t *testing.T) {
	for _, share := range []int{0, 1} {
		b := New(Config{Global: 6})
		b.SetShare(share)
		if got := b.Share(); got != 1 {
			t.Errorf("SetShare(%d) left Share() = %d, want 1", share, got)
		}
		if got := admitAll(b, 20); got != 6 {
			t.Errorf("SetShare(%d): admitted %d against a ceiling of 6", share, got)
		}
	}
}

// admitAll takes reservations until the broker refuses, and returns how many it
// granted. Nothing is released, so the count is the ceiling in force.
func admitAll(b *Broker, attempts int) int {
	n := 0
	for i := 0; i < attempts; i++ {
		res, ok := b.TryAcquire(Request{})
		if !ok {
			break
		}
		_ = res
		n++
	}
	return n
}
