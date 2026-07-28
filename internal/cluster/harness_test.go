package cluster

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/store"
)

// The whole suite runs against SQLite with no build tag and no external
// service, which is what makes the accuracy claims of DESIGN 5.6 checkable by
// `go test` rather than by a CI job that happens to have a database. The
// PostgreSQL backend appends itself in pg_test.go under the `integration` tag,
// and every test below then runs a second time against it -- the assertions do
// not change, because the guarantees do not.

type backend struct {
	name    string
	dialect store.Dialect
	// env returns a DSN for a fresh, isolated database. Several stores are
	// opened against it, because a cluster is several processes sharing one.
	env func(t *testing.T) string
}

var backends = []backend{{
	name:    "sqlite",
	dialect: store.DialectSQLite,
	env: func(t *testing.T) string {
		return filepath.Join(t.TempDir(), "dorang.db")
	},
}}

// clock is an injectable, race-safe time source. Every timing assertion in this
// package is made against it rather than against a sleep: a leadership handover
// tested with sleeps is a test that fails on a loaded machine and proves
// nothing on an idle one.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

var epoch = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

func newClock(t time.Time) *clock { return &clock{t: t} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openStore(t *testing.T, b backend, dsn string, now func() time.Time) *store.Store {
	t.Helper()
	cfg := store.Config{Driver: b.dialect, DSN: dsn}
	if now != nil {
		cfg.Now = now
	}
	s, err := store.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", b.name, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// eachBackend runs fn against every configured backend with one fresh store.
func eachBackend(t *testing.T, fn func(t *testing.T, s *store.Store, clk *clock)) {
	t.Helper()
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			clk := newClock(epoch)
			fn(t, openStore(t, b, b.env(t), clk.Now), clk)
		})
	}
}

// eachCluster runs fn with n independent store handles over one database, which
// is what n nodes of one cluster actually look like: separate pools, separate
// caches, one shared truth.
func eachCluster(t *testing.T, n int, fn func(t *testing.T, stores []*store.Store, clk *clock)) {
	t.Helper()
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			clk := newClock(epoch)
			dsn := b.env(t)
			stores := make([]*store.Store, n)
			for i := range stores {
				stores[i] = openStore(t, b, dsn, clk.Now)
			}
			fn(t, stores, clk)
		})
	}
}

// testNode builds a node with test-scale timings.
func testNode(t *testing.T, s *store.Store, id string, clk *clock, mutate func(*Config)) *Node {
	t.Helper()
	cfg := Config{
		Enabled:   true,
		NodeID:    id,
		Address:   "127.0.0.1:0",
		Version:   "test",
		Mode:      ModeSharedPG,
		Store:     s,
		LeaseTTL:  6 * time.Second,
		Tick:      time.Second,
		NodeTTL:   6 * time.Second,
		BlockSize: 16,
		Now:       clk.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	n, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%s): %v", id, err)
	}
	if err := n.Register(context.Background()); err != nil {
		t.Fatalf("Register(%s): %v", id, err)
	}
	return n
}

// tick advances every node once, in the given order, and fails on error.
func tick(t *testing.T, nodes ...*Node) {
	t.Helper()
	for _, n := range nodes {
		if err := n.Tick(context.Background()); err != nil {
			t.Fatalf("Tick(%s): %v", n.ID(), err)
		}
	}
}

// leaders returns the nodes that currently believe they lead.
func leaders(nodes ...*Node) []string {
	var out []string
	for _, n := range nodes {
		if n.IsLeader() {
			out = append(out, n.ID())
		}
	}
	return out
}
