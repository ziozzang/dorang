package capacity

import (
	"testing"
)

// A ceiling carried by the principal itself bounds concurrency.
//
// max_parallel_requests was a column on keys, users, teams and deployments that
// reached no enforcement anywhere: internal/auth's doc comment said "it is
// enforced by internal/capacity", and the identifier MaxParallel did not occur
// in this package at all. The broker's per-principal ceiling came only from the
// static capacity.principals table in YAML, which the per-credential column
// could never reach.
func TestPrincipalMaxBoundsConcurrency(t *testing.T) {
	b := newBroker(t, Config{})

	req := Request{Provider: "p", PrincipalID: "key-1", PrincipalMax: 1}

	first, ok := b.TryAcquire(req)
	if !ok {
		t.Fatal("the first request was refused under a ceiling of 1")
	}
	if _, ok := b.TryAcquire(req); ok {
		t.Fatal("a second concurrent request was admitted under max_parallel_requests 1: " +
			"the credential's own ceiling reaches no enforcement")
	}
	first.Release()
	if second, ok := b.TryAcquire(req); !ok {
		t.Fatal("the slot was not returned on release")
	} else {
		second.Release()
	}
}

// The static table and the credential's own column combine by taking the more
// restrictive, so a per-key ceiling can tighten a deployment default and never
// loosen it.
func TestPrincipalMaxCombinesWithTheStaticTable(t *testing.T) {
	cases := []struct {
		name    string
		static  int
		carried int
		want    int
	}{
		{"only the static table", 3, 0, 3},
		{"only the credential", 0, 2, 2},
		{"the credential is tighter", 5, 2, 2},
		{"the credential is looser and does not win", 2, 5, 2},
		{"neither", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mostRestrictive(c.static, c.carried); got != c.want {
				t.Errorf("mostRestrictive(%d, %d) = %d, want %d",
					c.static, c.carried, got, c.want)
			}
		})
	}

	// And end to end: a static ceiling of 4 with a carried ceiling of 1 admits
	// one.
	b := newBroker(t, Config{Principals: map[string]int{"key-1": 4}})
	req := Request{Provider: "p", PrincipalID: "key-1", PrincipalMax: 1}
	res, ok := b.TryAcquire(req)
	if !ok {
		t.Fatal("the first request was refused")
	}
	defer res.Release()
	if _, ok := b.TryAcquire(req); ok {
		t.Fatal("the credential's tighter ceiling did not override the static table's")
	}
}

// A request that carries no ceiling behaves exactly as before.
func TestNoPrincipalMaxIsUnbounded(t *testing.T) {
	b := newBroker(t, Config{})
	req := Request{Provider: "p", PrincipalID: "key-1"}
	for i := 0; i < 32; i++ {
		if _, ok := b.TryAcquire(req); !ok {
			t.Fatalf("request %d was refused with no ceiling configured", i)
		}
	}
}
