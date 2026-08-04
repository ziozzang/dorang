package cluster

import (
	"strings"
	"testing"
)

// Every published accuracy says what it does not cover.
//
// `shared-pg` and `shared-redis` publish "max overshoot 0", and that figure
// goes into the start-up log an operator reads before sizing a fleet. It is
// true of QUOTA — windowed spend, coordinated through the lease table — and
// says nothing about concurrency, which internal/capacity enforces per node in
// every mode.
//
// Measured 2026-08-04 on an isolated two-node cluster against an upstream that
// counted its own concurrency: a credential ceiling of 2 admitted FOUR at once,
// exactly two per node, reproduced twice, with `capacity_leases` holding only
// the leadership row.
//
// [Accuracy]'s own doc comment is the argument for this test: "A number true
// only sometimes, published as though it were true always, is worse than no
// number: it is the one an operator sizes a fleet against." The qualifier
// existed — in this package's doc comment, which is not where anybody reads a
// bound.
func TestEveryPublishedAccuracyNamesWhatItDoesNotCover(t *testing.T) {
	for _, m := range Modes() {
		p := Params{Limit: 64, Nodes: 3, Clustered: m != ModeLocal}
		acc, err := Publish(m, p)
		if err != nil {
			continue // a mode this build refuses says so elsewhere
		}
		if acc.Covers == "" {
			t.Errorf("%s publishes overshoot %d and does not say what it is about",
				m, acc.MaxOvershoot)
		}
		if acc.Excludes == "" {
			t.Errorf("%s publishes overshoot %d and does not say what it EXCLUDES; "+
				"concurrency ceilings are per node in every mode and an operator reading "+
				"this figure will size a plan against it", m, acc.MaxOvershoot)
		}
		if !strings.Contains(strings.ToLower(acc.Excludes), "concurren") {
			t.Errorf("%s's Excludes does not name concurrency: %q", m, acc.Excludes)
		}
		// The rendered form is what reaches the start-up log and /metrics. A
		// qualifier the renderer drops is a qualifier nobody reads — the exact
		// failure Accuracy.Holds was added to fix.
		s := acc.String()
		if !strings.Contains(s, "DOES NOT COVER") {
			t.Errorf("%s renders without its exclusion:\n  %s", m, s)
		}
	}
}

// The refusal for `local` stops recommending a remedy that is not one.
//
// The message told an operator to use "leased" or "shared-pg" because node-local
// counting means the provider sees N-fold. Both of those coordinate quota and
// neither reaches internal/capacity, so for a concurrency ceiling all three
// modes behave identically — a guard that refuses the obviously wrong value and
// points at two that do the same thing.
func TestTheLocalRefusalDoesNotPromiseWhatTheModesCannotDo(t *testing.T) {
	msg := ErrLocalInCluster.Error()
	if !strings.Contains(msg, "max_concurrency") {
		t.Error("the refusal recommends shared-pg/leased without saying that neither " +
			"bounds concurrency; an operator following it gets the same N-fold overshoot " +
			"the message warned them about")
	}
	if !strings.Contains(msg, "per node") {
		t.Errorf("the refusal does not state that concurrency is counted per node:\n  %s", msg)
	}
}
