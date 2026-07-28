package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// TestTwoNodesAgainstOneStore is the stress case: two real nodes, one store,
// real time, everything running at once -- ticks, campaigns, budget
// reservations, lease renewal and the leader's jobs -- with one node killed
// half way through.
//
// It exists because the deterministic tests all drive a fake clock, and a fake
// clock cannot produce the interleavings that break real coordination: a
// campaign that lands mid-reclaim, a renewal that overlaps a draw, a reserve
// that finds its block emptied between the load and the swap. Under -race this
// is where those show up.
//
// Two invariants are checked continuously rather than at the end, because both
// are about instants and a final check would miss every violation that healed:
//
//  1. Never two leaders at once.
//  2. Never more budget granted than the limit allows.
func TestTwoNodesAgainstOneStore(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test; skipped under -short")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dsn := filepath.Join(t.TempDir(), "cluster.db")
	open := func() *store.Store {
		s, err := store.Open(ctx, store.Config{Driver: store.DialectSQLite, DSN: dsn})
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	sa, sb := open(), open()

	const (
		limit    = 20 * 1_000_000_000 // 20 USD
		block    = 250_000_000        // 0.25 USD per lease
		estimate = 10_000_000         // 0.01 USD per request
		leaseTTL = 900 * time.Millisecond
		tickIvl  = 90 * time.Millisecond
		runFor   = 2500 * time.Millisecond
	)

	mk := func(s *store.Store, id string) *Node {
		n, err := New(Config{
			Enabled: true, NodeID: id, Mode: ModeSharedPG, Store: s,
			LeaseTTL: leaseTTL, Tick: tickIvl, NodeTTL: 3 * leaseTTL, BlockSize: block,
		})
		if err != nil {
			t.Fatalf("New(%s): %v", id, err)
		}
		if err := n.Register(ctx); err != nil {
			t.Fatalf("Register(%s): %v", id, err)
		}
		return n
	}
	a, b := mk(sa, "node-a"), mk(sb, "node-b")
	defer func() {
		_ = a.Close(context.Background())
		_ = b.Close(context.Background())
	}()

	key := BudgetKey("global", "", quota.Monthly, time.Now())

	var (
		wg         sync.WaitGroup
		granted    atomic.Int64
		refused    atomic.Int64
		doubleLead atomic.Int64
		tickErrs   atomic.Int64
		aStop      = make(chan struct{})
		stop       = make(chan struct{})
	)

	// Tick loops. node-a is killed part way through by closing aStop, which is
	// what a crash looks like: it simply stops running, releasing nothing.
	tickLoop := func(n *Node, halt <-chan struct{}) {
		defer wg.Done()
		tk := time.NewTicker(tickIvl)
		defer tk.Stop()
		for {
			select {
			case <-halt:
				return
			case <-stop:
				return
			case <-tk.C:
				if err := n.Tick(ctx); err != nil && !errors.Is(err, ErrClosed) {
					tickErrs.Add(1)
					t.Errorf("Tick(%s): %v", n.ID(), err)
					return
				}
			}
		}
	}
	wg.Add(2)
	go tickLoop(a, aStop)
	go tickLoop(b, stop)

	// The leadership invariant, sampled continuously.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if a.IsLeader() && b.IsLeader() {
				doubleLead.Add(1)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// Request traffic from both nodes at once.
	reserve := func(n *Node, halt <-chan struct{}) {
		defer wg.Done()
		l := n.Ledger()
		for {
			select {
			case <-halt:
				return
			case <-stop:
				return
			default:
			}
			h, err := l.Reserve(ctx, key, limit, estimate)
			switch {
			case errors.Is(err, ErrExhausted):
				refused.Add(1)
				time.Sleep(time.Millisecond)
				continue
			case errors.Is(err, ErrClosed):
				return
			case err != nil:
				t.Errorf("Reserve(%s): %v", n.ID(), err)
				return
			}
			granted.Add(estimate)
			if err := l.Settle(h, estimate); err != nil {
				t.Errorf("Settle(%s): %v", n.ID(), err)
				return
			}
		}
	}
	for i := 0; i < 3; i++ {
		wg.Add(2)
		go reserve(a, aStop)
		go reserve(b, stop)
	}

	// Kill node-a half way, so the second half of the run exercises takeover,
	// dead-node lease reclaim and a survivor spending alone.
	time.Sleep(runFor / 2)
	close(aStop)
	time.Sleep(runFor / 2)
	close(stop)
	wg.Wait()

	if n := doubleLead.Load(); n != 0 {
		t.Fatalf("both nodes believed they led at the same instant, %d times", n)
	}
	if n := tickErrs.Load(); n != 0 {
		t.Fatalf("%d ticks failed", n)
	}
	if got := granted.Load(); got > limit {
		t.Fatalf("granted %d nano against a limit of %d: the budget was exceeded", got, limit)
	}
	if refused.Load() == 0 {
		t.Fatal("nothing was ever refused; the limit was never reached and the run proved nothing")
	}

	// The survivor must take over, and the durable counter must be consistent
	// with what was granted -- at or above it, never below, since blocks are
	// charged before they are spent.
	//
	// Takeover is polled rather than sampled once. The claim is that leadership
	// moves within a bounded time, not that it has moved at some particular
	// microsecond, and asserting the latter on a loaded machine tests the
	// scheduler rather than the election.
	deadline := time.Now().Add(5 * time.Second)
	for !b.IsLeader() && time.Now().Before(deadline) {
		if err := b.Tick(ctx); err != nil {
			t.Fatalf("Tick(node-b) during takeover: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !b.IsLeader() {
		t.Fatal("the survivor never took over after the leader was killed")
	}
	if a.IsLeader() {
		t.Fatal("the killed node still believes it leads")
	}
	committed, err := b.Ledger().Committed(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if committed < granted.Load() {
		t.Fatalf("the durable counter holds %d nano but %d was granted", committed, granted.Load())
	}
	if committed > limit {
		t.Fatalf("the durable counter holds %d nano against a limit of %d", committed, limit)
	}
	t.Logf("granted %d nano, refused %d, committed %d, limit %d", granted.Load(), refused.Load(), committed, limit)
}

// TestConcurrentReservationsOnOneBlock hammers the hot path of DESIGN 9.6 --
// the atomic compare-and-swap over an in-memory block -- with far more
// goroutines than a block can serve, so the refill path is entered constantly
// and concurrently.
func TestConcurrentReservationsOnOneBlock(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *store.Store, clk *clock) {
		ctx := context.Background()
		const limit = int64(100_000)
		key := QuotaKey("key", "hot", quota.Rolling(time.Hour), quota.MetricRequests, clk.Now())

		l := newLedger(t, s, "node-a", clk, 32, time.Minute)
		t.Cleanup(func() { _ = l.Close(ctx) })

		var granted atomic.Int64
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 200; i++ {
					h, err := l.Reserve(ctx, key, limit, 1)
					if err != nil {
						t.Errorf("Reserve: %v", err)
						return
					}
					granted.Add(1)
					if err := l.Settle(h, 1); err != nil {
						t.Errorf("Settle: %v", err)
						return
					}
				}
			}()
		}
		wg.Wait()

		if got := granted.Load(); got != 1600 {
			t.Fatalf("granted %d, want 1600", got)
		}
		// Every unit granted must be accounted for, and the counter must not
		// have drifted: a lost compare-and-swap shows up here as a counter that
		// does not match the block arithmetic.
		if err := l.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		committed, err := l.Committed(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if committed < granted.Load() {
			t.Fatalf("committed %d is below the %d granted", committed, granted.Load())
		}
		if committed > granted.Load()+32 {
			t.Fatalf("committed %d is more than one block above the %d granted", committed, granted.Load())
		}
	})
}
