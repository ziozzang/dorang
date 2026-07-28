package batch

import (
	"context"
	"testing"
	"time"
)

// interactiveBound is the latency an interactive request for a saturated model
// must be admitted within while a batch is running against that same model.
// A free slot is held for it on every axis, so the honest expectation is
// "immediately"; the bound is generous enough not to be a CI timing test.
const interactiveBound = 2 * time.Second

// TestModelAxisContention is the test DESIGN §11.1 asks for by name.
//
// Revision 1 of the design capped batch at a share of *credential* concurrency.
// Under that rule a batch can occupy a model's entire limit while sitting well
// under its credential share, and interactive traffic for that model is starved
// completely — with every credential-axis assertion still passing. So the axis
// under test here is the model axis, and the subtests show what the
// credential-axis version of this test would have concluded.
//
// The arrangement is deliberately lopsided: the model axis admits 4, the
// credential axis 64. A batch is only ever blocked by the model.
func TestModelAxisContention(t *testing.T) {
	t.Run("reserve on every axis protects the contended model", func(t *testing.T) {
		c := newFakeCapacity(0.25) // every axis
		res := runContention(t, c)

		if !res.sameModelOK {
			t.Fatalf("interactive traffic for the contended model was starved (waited %s)", res.sameModelWait)
		}
		if res.sameModelWait > interactiveBound {
			t.Errorf("interactive request took %s, bound is %s", res.sameModelWait, interactiveBound)
		}
		// floor(4 * (1 - 0.25)) = 3 slots for batch; the fourth is reserved.
		if got := res.batchPeak; got != 3 {
			t.Errorf("batch peaked at %d of the model's 4 slots, want 3", got)
		}
	})

	t.Run("credential-only reserve starves the model — revision 1's defect", func(t *testing.T) {
		c := newFakeCapacity(0.25, axisCred)
		res := runContention(t, c)

		if res.sameModelOK {
			t.Fatal("expected the model axis to be starved when only the credential axis is reserved; " +
				"if this now passes, the fake no longer reproduces the defect and the test above proves nothing")
		}
		// And here is why revision 1's own test passed: the credential axis is
		// wide open the whole time, so an assertion made on that axis — or on
		// any other model sharing the credential — sees no contention at all.
		if !res.otherModelOK {
			t.Error("the credential axis was contended too; the demonstration needs it not to be")
		}
		if got := res.batchPeak; got != 4 {
			t.Errorf("batch took %d of the model's 4 slots, want all 4 (that is the defect)", got)
		}
	})

	t.Run("work not marked as batch defeats the reserve entirely", func(t *testing.T) {
		c := newFakeCapacity(0.25)
		c.stripBatch = true // the flag is set, but something downstream drops it
		res := runContention(t, c)

		if res.sameModelOK {
			t.Fatal("expected starvation when batch work is not marked as batch")
		}
		if got := res.batchPeak; got != 4 {
			t.Errorf("unmarked batch took %d of 4 slots, want all 4", got)
		}
	})
}

type contentionResult struct {
	sameModelOK   bool
	sameModelWait time.Duration
	otherModelOK  bool
	batchPeak     int
}

// runContention saturates the model axis of (p1, up-1) with batch rows, then
// tries to get interactive requests through — one for that same model, one for a
// different model on the same credential.
func runContention(t *testing.T, c *fakeCapacity) contentionResult {
	t.Helper()
	c.limit(axisModel, "p1\x00up-1", 4)
	c.limit(axisModel, "p1\x00up-2", 4)
	c.limit(axisCred, "p1", 64)
	c.limit(axisGlobal, "", 64)

	h := newHarness(t, func(cfg *Config) {
		cfg.Capacity = c
		cfg.RowConcurrency = 8 // more workers than the axis can ever admit
	})
	g := newGate()
	defer g.openAll()
	h.exec.fn = g.handler

	fid := h.upload(jsonlFile(200, "m1"))
	b := h.create(fid)

	// Wait until the batch has taken everything it is entitled to and stopped
	// climbing. Whatever it settles at is the occupancy the interactive request
	// has to get through.
	waitFor(t, 10*time.Second, func() bool {
		s, _ := g.counts()
		return s >= 3
	})
	stable := 0
	last := -1
	waitFor(t, 10*time.Second, func() bool {
		n := c.inUseOf(axisModel, "p1\x00up-1")
		if n == last {
			stable++
		} else {
			stable, last = 0, n
		}
		return stable >= 20
	})

	var res contentionResult
	res.batchPeak = c.peakOf(axisModel, "p1\x00up-1")

	// Every admission the scheduler made must have been marked as batch; that
	// single flag is the whole mechanism. Checked before the probes below,
	// which are deliberately not batch.
	if batch, plain := c.counts(); plain != 0 {
		t.Errorf("the scheduler made %d unmarked admissions out of %d", plain, batch+plain)
	}

	// The interactive request. It is NOT marked as batch, so it may use the
	// reserved part of every axis.
	res.sameModelOK, res.sameModelWait = tryInteractive(c, CapacityRequest{
		Provider: "p1", UpstreamModel: "up-1", ProviderGroup: "g1", PrincipalID: "someone",
	})
	// A different model on the same credential. This is what a credential-axis
	// test would have measured.
	res.otherModelOK, _ = tryInteractive(c, CapacityRequest{
		Provider: "p1", UpstreamModel: "up-2", ProviderGroup: "g1", PrincipalID: "someone",
	})

	g.openAll()
	h.await(b.ID, StatusCompleted)
	return res
}

// tryInteractive measures how long the contended model makes an interactive
// request wait, and whether it is admitted at all.
//
// The budget is [interactiveBound] and not something tighter on purpose. The
// caller distinguishes two outcomes — "starved", which is a fatal defect, and
// "admitted but slow", which is a latency number — and a ceiling below the
// bound collapses them: every admission between the ceiling and the bound gets
// reported as starvation on a machine that was merely busy. The deadline
// enforced here has to be the deadline the assertions are written against.
func tryInteractive(c *fakeCapacity, req CapacityRequest) (bool, time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), interactiveBound)
	defer cancel()
	start := time.Now()
	resv, err := c.Acquire(ctx, req)
	took := time.Since(start)
	if err != nil {
		return false, took
	}
	resv.Release()
	return true, took
}

// The batch class reaches the executor, so the backend's own scheduler can act
// on it too (DESIGN §7.5).
func TestRowsCarryBatchPriorityClass(t *testing.T) {
	h := newHarness(t, nil)
	fid := h.upload(jsonlFile(5, "m1"))
	b := h.create(fid)
	h.await(b.ID, StatusCompleted)

	h.exec.mu.Lock()
	defer h.exec.mu.Unlock()
	if got := h.exec.classes[DefaultPriorityClass]; got != 5 {
		t.Errorf("%d of 5 rows carried the %q class: %v", got, DefaultPriorityClass, h.exec.classes)
	}
}

// An axis whose reserve leaves batch nothing at all fails its rows quickly
// instead of blocking forever. It is the one capacity condition that can never
// resolve, and a batch waiting on it would look identical to a hung gateway.
func TestUnsatisfiableAxisFailsRowsRatherThanHanging(t *testing.T) {
	c := newFakeCapacity(0.5)
	c.limit(axisModel, "p1\x00up-1", 1) // floor(1 * 0.5) = 0 for batch
	h := newHarness(t, func(cfg *Config) {
		cfg.Capacity = c
		cfg.MaxAttempts = 2
	})

	fid := h.upload(jsonlFile(4, "m1"))
	b := h.create(fid)
	final := h.await(b.ID, StatusCompleted, StatusFailed)

	if final.Status != StatusCompleted {
		t.Fatalf("status = %s, want completed with failed rows", final.Status)
	}
	if final.RequestCounts.Failed != 4 {
		t.Fatalf("counts = %+v, want 4 failed", final.RequestCounts)
	}
	rows := h.lines(final.ErrorFileID)
	if len(rows) != 4 || rows[0].Error == nil || rows[0].Error.Code != "capacity_unavailable" {
		t.Fatalf("error file = %+v", rows)
	}
	if h.exec.totalCalls() != 0 {
		t.Errorf("executor was called %d times for rows that were never admitted", h.exec.totalCalls())
	}
}
