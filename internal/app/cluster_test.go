package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// These tests are about the assembly, not about internal/cluster. Every piece
// they exercise is already unit-tested next to its implementation and was
// already correct; what was missing for the whole life of the package is a
// caller. So each assertion here is on an effect that only a RUNNING node
// produces — a lease reclaimed from a dead peer, a swept reservation, a ceiling
// two processes respect together — and never on a job being registered, a
// coordinator being non-nil, or a field being set. Those pass whether or not
// the wiring exists, which is how the layer stayed dead through several audits.

// clusterYAML is a gateway with a concurrency ceiling to publish figures
// against and one deployment to make the assembly real.
const clusterYAML = `
version: 1
server: {listen: "127.0.0.1:0"}
cluster:
  enabled: %t
  node_id: %s
  capacity_mode: %s
  min_leasable: 16
capacity:
  global: {max_concurrency: 64}
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid"}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - {provider: p1, upstream_model: m1-upstream, credentials: [c1]}
`

// clusterTimings are test-scale versions of the §13 defaults, keeping the
// relationships the real numbers encode: the tick is many times under the node
// TTL, so a live node never lapses on one slow beat, and the lease TTL is many
// ticks, so leadership survives a slow scheduler. A test that inverted those
// would be testing a cluster nobody would deploy — and two of them, measured
// below, are what an inverted set actually does.
//
// The tick is a whole second, which looks lazy for a test and is not. One pass
// of the loop is a dozen statements, several in write transactions: heartbeat,
// campaign, ledger maintenance, then the leader's retention pass, reservation
// sweep, and lease reclaim with its table scan and its prune. SQLite has one
// writer, and under -race modernc's pure-Go driver is an order of magnitude
// slower. At a 150ms tick the writer was busy essentially all the time, the
// node's own heartbeats queued behind the node's own leader jobs, and the
// process declared ITSELF dead: measured, `Registry.Dead` returned the live
// node, the loop completed about one pass a minute, and leadership flapped.
// At 20ms it was worse — writers failed outright with SQLITE_BUSY once the
// store's five-second timeout expired.
//
// The node TTL is ten ticks and not two for the reason §13 gives, which this
// suite then rediscovered: declaring a node dead is not free, because the
// leader reclaims its leases and hands its share to somebody else. At a
// one-second TTL, contention delayed heartbeats past it, live nodes were
// reclaimed from, and two nodes admitted 104 against a shared-pg ceiling of 64.
// That is the designed consequence of an under-sized TTL rather than a defect
// in the mode, and it is worth writing down that §5.6's "maximum overshoot 0"
// is a claim about a cluster whose heartbeats hold.
//
// None of this is a property of dorang in production: §13's SQLite deployments
// are single-node, and a real cluster is on PostgreSQL, which has more than one
// writer. It is a property of testing a cluster on SQLite, and the assertions
// below avoid leaning on it — the peer that has to look dead is registered with
// a backdated heartbeat rather than by sleeping out a TTL.
var clusterTimings = Options{
	ClusterTick:     time.Second,
	ClusterNodeTTL:  10 * time.Second,
	ClusterLeaseTTL: 30 * time.Second,
}

// newClusterApp assembles a gateway on a shared database file. Several of these
// over one path are what several nodes of one cluster actually are: separate
// pools, separate caches, one shared truth.
func newClusterApp(t *testing.T, dsn, id, mode string, enabled bool) *App {
	t.Helper()
	return newClusterAppOpts(t, dsn, id, mode, enabled, nil)
}

// newClusterAppOpts is newClusterApp with the node's timings tunable, for the
// one test whose whole method is to make the tick absurd.
func newClusterAppOpts(t *testing.T, dsn, id, mode string, enabled bool, tune func(*Options)) *App {
	t.Helper()
	a, err := New(context.Background(), clusterOptions(t, dsn, id, mode, enabled, tune))
	if err != nil {
		t.Fatalf("app.New(%s): %v", id, err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a
}

// clusterOptions is everything newClusterAppOpts does except calling [New].
//
// It is split out for the tests whose subject is a gateway that must NOT come
// up: a helper that fails the test on a construction error cannot express "this
// one is supposed to be refused", and asserting the refusal is the whole point
// of those tests.
func clusterOptions(t *testing.T, dsn, id, mode string, enabled bool, tune func(*Options)) Options {
	t.Helper()
	isolateState(t)
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
	cfg, err := config.LoadBytes([]byte(fmt.Sprintf(clusterYAML, enabled, id, mode)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Storage.SQLite.Path = dsn
	// The metering spool, pointed somewhere this app owns.
	//
	// [isolateState] above already gives this TEST its own, and that is what
	// makes it safe. This line makes it per APP, which matters here and nowhere
	// else in the package: several of these tests assemble two gateways at once,
	// because two nodes of one cluster is what they are about, and two live
	// meters appending to one segment sequence behind one read cursor is the
	// shared-spool defect in its concurrent form rather than its sequential one.
	//
	// What it costs when it is missing was measured on the node loop, which is
	// the first thing in the package that depended on making timely progress: a
	// gateway that inherits a backlog ships it into its own store as one
	// enormous multi-row insert, SQLite has a single writer, and the loop's next
	// heartbeat waits out the store's five-second busy timeout and fails. These
	// tests passed in eleven seconds alone and could not elect a leader in
	// thirty when run after the rest of the package.
	cfg.Metering.Spool.Dir = filepath.Join(t.TempDir(), "spool")
	t.Setenv(cfg.Server.KeyPepperEnv, testPepper)
	t.Setenv(cfg.Server.MasterKeyEnv, testMasterKey)

	opts := clusterTimings
	opts.Config = cfg
	// The node's tick errors go to Logf and nowhere else. Without this a loop
	// that fails every pass — a busy store, a refused campaign — is
	// indistinguishable from a loop that is not running, which is the exact
	// ambiguity these tests exist to remove. Guarded, because t.Logf after the
	// test function returns is a panic and the loop outlives the body by however
	// long Close takes.
	var logMu sync.Mutex
	done := false
	t.Cleanup(func() { logMu.Lock(); done = true; logMu.Unlock() })
	opts.Logf = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		if !done {
			t.Logf(format, args...)
		}
	}
	if tune != nil {
		tune(&opts)
	}
	return opts
}

func sharedDSN(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "cluster.db")
}

// waitForNode waits on a condition the node answers from MEMORY: leadership,
// the cached live-node count. Nothing here touches the store, so it is polled
// tightly.
func waitForNode(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitForStore waits on a condition that has to be READ from the store, and
// polls slowly on purpose.
//
// The package's waitFor polls every 200 microseconds, which is right for an
// in-memory condition and actively harmful here. SQLite has one writer, and the
// node loop needs it: at 200µs a poll that opens a read transaction — or worse,
// one that scrapes /metrics, since that is an HTTP request the meter records
// and eventually writes — puts thousands of operations a second between the
// loop and the write that would satisfy the condition. Measured under -race,
// that harness starved the loop badly enough that a single node could not win
// an uncontested election in thirty seconds.
//
// So: the harness observes at a rate that leaves the system under test able to
// run. Anything that can be answered from memory uses waitForNode instead, and
// the scrape is read once at the end rather than in the loop.
func waitForStore(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestLeaderJobsRunInTheAssembledGateway is the one that matters.
//
// Before the node was wired, `cluster.enabled: true` bought the capacity mode
// and nothing else: no registration, no election, and therefore none of the
// leader jobs. The consequence that costs money is lease reclaim. A node that
// crashes holding quota leases never returns them, the quota is leaked
// permanently, and the only recovery is manual — DESIGN §13 lists lease
// rebalancing as leader work precisely so that this cannot happen.
//
// The assertion is on an effect the app package cannot produce by itself: the
// reclaimed budget was drawn by a DIFFERENT node's ledger and is returned by
// the leader walking Registry.Dead. A harness substituting it would be
// substituting the value under test.
//
// There used to be a second assertion here, on the budget-reservation half of
// the sweep job: the test planted a row through store.ReserveBudget with an
// expiry already in the past and waited for the leader to clear it. It proved
// nothing about the gateway. ReserveBudget had no caller outside tests, so the
// state it planted was one no running gateway could reach — the harness was
// producing the value the system under test is responsible for producing, which
// is exactly §17.1 rule 1. The mechanism is deleted; the capability it claimed
// ("a process killed mid-request must not lock budget forever") is what the
// remaining assertion covers, through the lease the dead peer left behind.
func TestLeaderJobsRunInTheAssembledGateway(t *testing.T) {
	ctx := context.Background()
	dsn := sharedDSN(t)
	a := newClusterApp(t, dsn, "node-live", "leased", true)

	// A peer that draws a block and then dies. It is a real cluster.Ledger over
	// the same database under its own node id, which is what a second process
	// is; nothing here fabricates a lease row.
	const (
		block  = 1_000
		spend  = 100
		limit  = 1_000_000
		ghost  = "node-ghost"
		budget = "team-eng"
	)
	//
	// Its clock is set an hour back, so both of the things a dead peer stops
	// doing have already stopped: its heartbeat lapsed AND its block lease
	// expired. The second is not decoration. The leader reclaims a departed
	// node's blocks on the LEASE and not on the heartbeat, because a lapsed
	// heartbeat is a declaration — one slow store write produces it on a node
	// that is serving fine — while a lapsed lease is the same instant the
	// holder's own hot path stops spending. A ghost whose lease was still live
	// would be asserting that the leader takes units out from under a peer that
	// can still hand them out, which is how a two-node cluster admitted 190
	// against a limit of 100.
	stale := func() time.Time { return time.Now().Add(-time.Hour) }
	ghostLedger, err := cluster.NewLedger(cluster.LedgerConfig{
		Store: a.Store, NodeID: ghost, Block: block, Now: stale,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := cluster.BudgetKey(string(store.SubjectTeam), budget, quota.Monthly, time.Now())
	hold, err := ghostLedger.Reserve(ctx, key, limit, spend)
	if err != nil {
		t.Fatalf("the dead peer could not draw a block: %v", err)
	}
	if err := ghostLedger.Settle(hold, spend); err != nil {
		t.Fatal(err)
	}
	// Checkpoint what it spent. The `used` column is what the reclaim reads to
	// tell the unspent part of a block from the spent part, and it is written
	// on a draw, a renewal or a tick — none of which this peer reaches again,
	// because it is about to die. Without this the reclaim is still correct but
	// coarser: the whole block comes back and the peer's spend is forgotten,
	// which is the safe direction (§9.6 under-spends, never overspends) and a
	// weaker thing to assert. Checkpointing first is what makes the assertion
	// below about the ARITHMETIC of the reclaim rather than about it having
	// happened at all.
	if err := ghostLedger.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	// The whole block is charged to the durable counter before a unit of it is
	// spent — that is what makes a crash under-spend rather than overspend
	// (§9.6). So the counter reads the block, not the spend.
	committed, err := a.Ledger.Committed(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if committed != block {
		t.Fatalf("the peer's block charged %d to the durable counter, want %d", committed, block)
	}
	// Register the peer, then abandon it: no Close, no further heartbeat. This
	// is a crash, and it is the only way to reach the state the reclaim exists
	// for. Registration goes through the real Registry so the row's shape is
	// the one the leader will read.
	//
	// It registers on the same backdated clock, so the row lands with a
	// heartbeat that has already lapsed. That is the harness substituting a
	// DEPENDENCY — a peer that last beat an hour ago — and not a value the
	// gateway is responsible for producing: whether that peer counts as dead,
	// whether its leases are this leader's to reclaim, and how much comes back
	// are all still decided by the code under test (§17.1). Sleeping out the
	// TTL instead would assert exactly the same thing and take ten seconds to
	// do it.
	ghostReg, err := cluster.NewRegistry(a.Store, ghost, clusterTimings.ClusterNodeTTL, stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := ghostReg.Register(ctx, cluster.NodeInfo{ID: ghost}); err != nil {
		t.Fatal(err)
	}
	// It really does look dead to the gateway — otherwise the wait below would
	// be measuring the registry rather than the reclaim.
	dead, err := a.Node.Registry().Dead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 || dead[0].ID != ghost {
		t.Fatalf("the gateway sees %v as dead, want exactly the abandoned peer", dead)
	}

	// Nothing below drives a tick. The gateway's own loop has to do it, which
	// is the whole point: reverting the Node.Start call in joinCluster leaves
	// every assertion from here on unmet.
	waitForNode(t, "the only node to take leadership", func() bool { return a.Node.IsLeader() })

	waitForStore(t, "the leader to reclaim the dead peer's unspent budget", func() bool {
		v, err := a.Ledger.Committed(ctx, key)
		return err == nil && v == spend
	})
	// Exactly the unspent part came back: the peer's 100 is still charged and
	// the 900 it never spent is available again. Returning the whole block
	// would lose the spend; returning nothing is the leak this job exists to
	// prevent.
	if v, err := a.Ledger.Committed(ctx, key); err != nil || v != spend {
		t.Fatalf("after reclaim the durable counter reads %d (err %v), want the peer's "+
			"spend of %d", v, err, spend)
	}
	// The reclaim is attributed, not blind: the peer's row is what the leader
	// walked to find the lease, so it must still have been there when the
	// reclaim ran. Pruning happens on a much longer grace (§13).
	//
	// Waited for rather than read once, because the counter trails the work.
	// jobState.run calls the job body and increments Runs only after it
	// returns, so between the ledger write this test has already observed and
	// the statistic that describes it there is a window where the reclaim has
	// happened and nothing has counted it — and under load the read lands
	// there. The claim is unchanged: if the count never arrives, the budget
	// came back some other way.
	waitForNode(t, "the lease-reclaim job to be counted as having run "+
		"(the lease was reclaimed, so if this never arrives something else returned it)",
		func() bool { return a.Node.JobStats()["lease-reclaim"].Runs > 0 })
}

// TestSingleNodeJoinsNothing holds DESIGN §0.2's promise against the layer most
// likely to break it: a package whose entire subject is coordinating with peers
// a notebook does not have.
//
// "Costs nothing" is stated as four measurements, not as a claim. A gateway with
// `cluster.enabled: false`, serving and then left alone:
//
//  1. never becomes leader. It is the only node, so a loop that ran even once
//     would have campaigned and won;
//  2. runs no leader job, ever — the sweep and the reclaim are due on every
//     tick, so a single tick would show up here;
//  3. writes no row to `nodes` and holds no leadership lease;
//  4. issues no store statements at the cluster tick rate.
//
// The first two are the sharp ones: they observe the loop directly instead of
// inferring it, and they hold with no timing assumption at all. The fourth is
// what catches a loop that polls something these three do not read, and its
// method is to make the tick absurd — two milliseconds. That costs nothing when
// the wiring is right, because nothing runs at all; if it is wrong the loop
// beats five hundred times a second and the count is off by three orders of
// magnitude. A margin that large needs no calibration against an unrelated
// background job's interval, which is what a comparative threshold would.
func TestSingleNodeJoinsNothing(t *testing.T) {
	ctx := context.Background()
	a := newClusterAppOpts(t, sharedDSN(t), "node-solo", "local", false, func(o *Options) {
		o.ClusterTick = 2 * time.Millisecond
		o.ClusterNodeTTL = 20 * time.Millisecond
	})

	if a.Node == nil {
		t.Fatal("an unclustered gateway has no node, so nothing owns the durable ledger")
	}
	if a.Node.Enabled() {
		t.Fatal("cluster.enabled is false and the node reports itself clustered")
	}

	// Serve some traffic first: an idle process is not the claim, a working one
	// is.
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		a.Server.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health/readiness", nil))
	}

	const window = 2 * time.Second
	before := a.Store.StatementCount()
	time.Sleep(window)
	solo := a.Store.StatementCount() - before

	// A loop at this tick would run a thousand times in the window, and every
	// pass heartbeats, campaigns and runs two due leader jobs. The ceiling is
	// the tick count itself, which even a loop that did ONE statement a pass
	// would exceed.
	if ticks := uint64(window / (2 * time.Millisecond)); solo >= ticks {
		t.Errorf("an unclustered gateway issued %d store statements in %v at a 2ms "+
			"cluster tick, which is %d ticks. The node loop is running: "+
			"cluster.enabled: false must cost no goroutine and no query", solo, window, ticks)
	}

	if a.Node.IsLeader() {
		t.Error("an unclustered gateway campaigned and won; nothing should have campaigned")
	}
	for name, st := range a.Node.JobStats() {
		if st.Runs != 0 {
			t.Errorf("leader job %q ran %d times on an unclustered gateway", name, st.Runs)
		}
	}

	nodes, err := a.Node.Registry().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Errorf("an unclustered gateway registered %d node(s): %v", len(nodes), nodes)
	}
	holder, _, _, err := a.Node.Election().Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if holder != "" {
		t.Errorf("an unclustered gateway campaigned and %q holds the leadership lease", holder)
	}
}

// TestAppLedgerIsTheNodesLedger: one owner, not two.
//
// internal/app used to build its own cluster.Ledger. Adding a node without
// taking its ledger would have left two of them under one node id — two
// in-memory block caches over the same rows, each drawing blocks the other did
// not know about and each returning on close what the other had already
// returned. The identity is asserted rather than the behaviour because the
// behaviour is a double-spend that only appears under a race.
func TestAppLedgerIsTheNodesLedger(t *testing.T) {
	a := newClusterApp(t, sharedDSN(t), "node-solo", "local", false)
	if a.Ledger != a.Node.Ledger() {
		t.Error("App.Ledger is not the node's ledger, so two ledgers share one node id")
	}
}

// TestTwoNodesShareOneCeiling is what `cluster.capacity_mode` is FOR.
//
// The coordinator is built from configuration and owned by the node, so the
// mode, the node id and the shared backend cannot disagree with the registry.
// The observable is that two SEPARATE PROCESSES see one counter: a local
// coordinator — which is what `capacity_mode` selected before this was wired,
// and what a config silently ignored would leave you with — gives each of them
// its own, and no assertion inside one process can tell the two apart.
//
// It deliberately does not re-run internal/cluster's concurrent overshoot
// suite. That suite proves the accuracy claim under load with deterministic
// ticks; this proves the wiring, and doing it in three charges rather than a
// hundred keeps two real node loops from saturating one SQLite file, which is a
// deployment shape §13 does not ship and which measures contention rather than
// coordination.
func TestTwoNodesShareOneCeiling(t *testing.T) {
	ctx := context.Background()
	dsn := sharedDSN(t)
	a := newClusterApp(t, dsn, "node-a", "shared-pg", true)
	b := newClusterApp(t, dsn, "node-b", "shared-pg", true)

	if a.Coordinator == nil || b.Coordinator == nil {
		t.Fatal("cluster.capacity_mode selected no coordinator")
	}
	for _, c := range []quota.Coordinator{a.Coordinator, b.Coordinator} {
		if c.Mode() != cluster.ModeSharedPG {
			t.Fatalf("capacity_mode shared-pg built a %s coordinator", c.Mode())
		}
	}

	const limit = 64
	now := time.Now()
	key := quota.NewKey("credential", "c1", quota.Daily, quota.MetricRequests, now)

	// Node A takes half the ceiling.
	if g, err := a.Coordinator.Charge(ctx, key, limit, limit/2, now); err != nil || !g.OK {
		t.Fatalf("node A could not take half the ceiling: %v %+v", err, g)
	}
	// Node B must SEE it. This is the whole test: a local coordinator reports 0
	// here, because the units were spent in another process, and no assertion
	// made inside one process can tell a local coordinator from a shared one.
	used, err := b.Coordinator.Used(ctx, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if used != limit/2 {
		t.Fatalf("node B sees %d units spent against the shared ceiling, node A spent %d. "+
			"capacity_mode selected a coordinator that counts only its own traffic", used, limit/2)
	}
	// And symmetrically, so that this is one counter rather than a one-way
	// report: B spends the rest and A sees the ceiling full.
	if g, err := b.Coordinator.Charge(ctx, key, limit, limit/2, now); err != nil || !g.OK {
		t.Fatalf("node B could not take the remaining half: %v %+v", err, g)
	}
	if used, err = a.Coordinator.Used(ctx, key, now); err != nil {
		t.Fatal(err)
	}
	if used != limit {
		t.Errorf("node A sees %d of the ceiling spent, want the full %d after both "+
			"nodes charged half each", used, limit)
	}

	// What is deliberately NOT asserted here: that one more unit is refused.
	// The shared coordinator holds spend as lease rows with a TTL
	// (quota.DefaultLeaseTTL, 30s), and a row that expires returns its units —
	// so on a slow enough run the next charge is admitted and the assertion
	// fails for a reason that has nothing to do with this wiring. See the note
	// on that TTL in internal/app/cluster.go. The exactness of the shared modes
	// under concurrent load is internal/cluster's overshoot suite, which drives
	// ticks deterministically and can hold the property still; what belongs
	// here is that the app's coordinator is the shared one at all.
}

// TestPublishedOvershootCountsLiveNodes: DESIGN §5.6 requires the maximum
// overshoot to be published as a number, and every one of those numbers is
// `something × (nodes − 1)`.
//
// With the node count hardcoded to 1 they are all exactly 0 — "exact" — which
// is the one answer a multi-node cluster cannot give. That is not a
// conservative error. An operator sizes a fleet against the published bound, so
// a bound that is wrong in the multi-node case is worse than no bound at all,
// because no bound does not get acted on.
//
// The second node is a real gateway, not a registry row this test wrote: the
// count has to come from the registry the cluster keeps.
func TestPublishedOvershootCountsLiveNodes(t *testing.T) {
	dsn := sharedDSN(t)
	a := newClusterApp(t, dsn, "node-a", "leased", true)

	// One node: nothing to overshoot by, and the figure says so. No wait is
	// needed — a lone node is what the gateway publishes from the moment it
	// starts, and the point of the second half is that this number MOVES.
	waitForNode(t, "the first node to count itself", func() bool { return a.nodes.get() == 1 })
	if got := scrapeInt(t, a, "dorang_cluster_expected_nodes", ""); got != 1 {
		t.Fatalf("one node publishes a count of %d", got)
	}
	if got := scrapeInt(t, a, "dorang_coordination_max_overshoot", `mode="leased"`); got != 0 {
		t.Errorf("one node publishes an overshoot of %d, want 0", got)
	}

	// A second node joins. It is a real cluster.Node over the same database,
	// registered through the same call the gateway makes; it simply does not
	// run a loop, because a second loop against one SQLite file would measure
	// write-lock contention and this test is about arithmetic. What is
	// substituted is the peer — a dependency of the count — and not the count,
	// which the gateway still has to read for itself.
	peer, err := cluster.New(cluster.Config{
		Enabled: true, NodeID: "node-b", Mode: cluster.ModeLeased, Store: a.Store,
		NodeTTL: clusterTimings.ClusterNodeTTL, Tick: clusterTimings.ClusterTick,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close(context.Background()) })
	if err := peer.Register(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The wait is on the gateway's cached count, which it refreshes from the
	// registry on its own tick; the SCRAPE is then read once, below. Polling
	// the scrape instead would put an HTTP request and a metering write between
	// the node and every refresh it was waiting for.
	waitForNode(t, "the gateway to notice the second node", func() bool { return a.nodes.get() == 2 })
	if got := scrapeInt(t, a, "dorang_cluster_expected_nodes", ""); got != 2 {
		t.Fatalf("the gateway counted 2 live nodes and published %d", got)
	}

	// block × (nodes − 1), with the node's own lease block. It must be
	// positive: that is the whole difference between a published bound and a
	// published zero.
	want := int64(cluster.DefaultBlockSize)
	if got := scrapeInt(t, a, "dorang_coordination_max_overshoot", `mode="leased"`); got != want {
		t.Errorf("two nodes publish a leased overshoot of %d, want block x (nodes-1) = %d",
			got, want)
	}
	// The local mode is refused under cluster.enabled, so it is absent from the
	// published set entirely; shared-pg stays exact however many nodes there
	// are, which is what distinguishes "exact" from "one node".
	if got := scrapeInt(t, a, "dorang_coordination_max_overshoot", `mode="shared-pg"`); got != 0 {
		t.Errorf("shared-pg publishes %d with two nodes, want 0", got)
	}
}

// TestScrapeDoesNotOfferAModeTheLoaderRefuses.
//
// Publishing every mode rather than only the configured one exists so an
// operator can see the cost of a different choice before making it. That makes
// the comparison set advice, and advice that fails on the next config load is
// the worst kind: shared-redis reads "exact, one round trip to Redis" — better
// than the shared-pg row right above it — and then `capacity_mode: shared-redis`
// is rejected, because this build ships the protocol and no client.
func TestScrapeDoesNotOfferAModeTheLoaderRefuses(t *testing.T) {
	a := newClusterApp(t, sharedDSN(t), "node-a", "leased", true)

	if got := scrapeInt(t, a, "dorang_coordination_mode_refused", `mode="shared-redis"`); got != 1 {
		t.Errorf("dorang_coordination_mode_refused{mode=\"shared-redis\"} = %d, want 1: "+
			"the scrape offers a mode internal/config rejects", got)
	}
	// Refused, not merely absent from the overshoot family: an operator reading
	// the two families together must be told it cannot be chosen, rather than
	// left to infer it from a missing row.
	if got := scrapeInt(t, a, "dorang_coordination_max_overshoot", `mode="shared-redis"`); got != -1 {
		t.Errorf("shared-redis still publishes an overshoot of %d while being refused", got)
	}
	// The mode that IS available must still be offered, or this test would pass
	// on a scrape that refused everything.
	if got := scrapeInt(t, a, "dorang_coordination_mode_refused", `mode="shared-pg"`); got != 0 {
		t.Errorf("shared-pg is refused too; then nothing is being compared")
	}

	// And the loader really does reject it, so the two agree. This is the half
	// that keeps the metric honest rather than merely consistent with itself.
	_, err := config.LoadBytes([]byte(fmt.Sprintf(clusterYAML, true, "node-x", "shared-redis")))
	if err == nil {
		t.Error("capacity_mode: shared-redis loaded, so the refused gauge is wrong")
	}
}

// TestClusteredScrapePublishesLeadership: the leadership gauges were never
// emitted by the real binary, because nothing set Leader or Term. An operator
// asking which node leads had no answer from the scrape.
func TestClusteredScrapePublishesLeadership(t *testing.T) {
	a := newClusterApp(t, sharedDSN(t), "node-a", "leased", true)
	waitForNode(t, "the only node to take leadership", func() bool { return a.Node.IsLeader() })
	if got := scrapeInt(t, a, "dorang_cluster_is_leader", ""); got != 1 {
		t.Errorf("the node holds leadership and the scrape reports %d", got)
	}
	if got := scrapeInt(t, a, "dorang_cluster_term", ""); got < 1 {
		t.Errorf("dorang_cluster_term is %d, want the term of the election that was won", got)
	}
}

// readiness is what a load balancer polls, answered by the assembled gateway.
//
// It goes through the real HTTP surface rather than reading a flag, because the
// claim being made in these tests is about what a balancer is told — and a
// boolean read off the App would be satisfied by a field nothing serves.
func readiness(t *testing.T, a *App) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	a.Server.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health/readiness", nil))
	return w.Code, w.Body.String()
}

// TestDuplicateNodeIDRefusesToStart: the second process does not come up.
//
// internal/cluster has refused the duplicate id since it was written, and
// exported [cluster.Node.Conflict] so that the assembly could escalate it.
// Nothing did: internal/app logged Node.Start's error and returned a boolean,
// so the second process never led — which closes the leader-jobs-run-twice
// harm — and KEPT SERVING. That is the part a boolean cannot express and this
// test is about. App.Ledger is the node's ledger and App.Coordinator is built
// with the node's id, so a process that started and merely declined to lead
// draws budget from the incumbent's lease block and takes its share of every
// leased limit, under an identity the cluster attributes to somebody else.
//
// So the assertions are on the PROCESS: New returns an error, the error names
// the condition, no App comes back to serve anything, and the incumbent still
// holds everything it held. Asserting a log line or a false return would pass
// against the defect.
func TestDuplicateNodeIDRefusesToStart(t *testing.T) {
	ctx := context.Background()
	dsn := sharedDSN(t)
	const id = "node-copied-id"

	a := newClusterApp(t, dsn, id, "shared-pg", true)
	waitForNode(t, "the incumbent to take leadership", func() bool { return a.Node.IsLeader() })

	// The incumbent takes half a shared ceiling. This is not scene-setting: the
	// refused process's shutdown path releases by node id, and under a copied id
	// that is the incumbent's row. A refusal that emptied the running node's
	// share on its way out would be a worse defect than the one being closed.
	const limit = 64
	now := time.Now()
	key := quota.NewKey("credential", "c1", quota.Daily, quota.MetricRequests, now)
	if g, err := a.Coordinator.Charge(ctx, key, limit, limit/2, now); err != nil || !g.OK {
		t.Fatalf("the incumbent could not take half the ceiling: %v %+v", err, g)
	}

	// The second process: same node_id, same database. Two pods of one
	// deployment whose cluster.node_id was written into the config file.
	b, err := New(ctx, clusterOptions(t, dsn, id, "shared-pg", true, nil))
	if err == nil {
		_ = b.Close(ctx)
		t.Fatal("a second process started under a node id another process is already " +
			"running: it is now drawing budget and quota under the incumbent's identity")
	}
	if b != nil {
		t.Errorf("app.New returned an error AND a gateway; the caller has something to serve with")
	}
	if !errors.Is(err, cluster.ErrDuplicateNodeID) {
		t.Fatalf("the refusal is %v, which does not identify itself as a duplicate node id; "+
			"an operator cannot tell it from a store outage", err)
	}
	if !strings.Contains(err.Error(), id) {
		t.Errorf("the refusal does not name the id at fault (%q): %v", id, err)
	}

	// The incumbent is untouched by the refusal, on all three of the things the
	// id keys: its registry row, its leadership, and its share of the ceiling.
	nodes, err := a.Node.Registry().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].ID != id {
		t.Errorf("the registry holds %v after the refusal, want just the incumbent", nodes)
	}
	if !a.Node.IsLeader() {
		t.Error("the refused process took leadership away from the incumbent")
	}
	used, err := a.Coordinator.Used(ctx, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if used != limit/2 {
		t.Errorf("the incumbent's share of the ceiling reads %d after the refusal, want %d: "+
			"the process that was refused released the leases of the process that holds the id",
			used, limit/2)
	}
}

// TestSupersededNodeReportsNotReady is the half start-up cannot reach.
//
// A process frozen past the node TTL — a long GC pause, a suspended container, a
// paused debugger — is indistinguishable from a dead one, so a successor may
// legitimately adopt its row while it is away. It learns that from its own
// heartbeat, internal/cluster demotes it, and it never leads again. joinCluster
// runs once and never re-checks, so nothing else in the process changes: it goes
// on serving under a contested identity indefinitely.
//
// Readiness is the signal, because readiness is the one a load balancer polls.
// The harness substitutes the DEPENDENCY — a successor that took the row over,
// registered through the same [cluster.Registry] the gateway uses, on a clock an
// hour ahead so that the frozen node's heartbeat has lapsed from its point of
// view. Whether that supersedes this node, whether this node then stops leading,
// and whether it stops being routed to are all still decided by the code under
// test.
func TestSupersededNodeReportsNotReady(t *testing.T) {
	ctx := context.Background()
	const id = "node-frozen"
	a := newClusterApp(t, sharedDSN(t), id, "leased", true)
	waitForNode(t, "the node to take leadership", func() bool { return a.Node.IsLeader() })

	if code, body := readiness(t, a); code != http.StatusOK {
		t.Fatalf("readiness before the supersession is %d: %s", code, body)
	}

	ahead := func() time.Time { return time.Now().Add(time.Hour) }
	succ, err := cluster.NewRegistry(a.Store, id, clusterTimings.ClusterNodeTTL, ahead)
	if err != nil {
		t.Fatal(err)
	}
	if err := succ.Register(ctx, cluster.NodeInfo{ID: id}); err != nil {
		t.Fatalf("the successor could not adopt the lapsed row: %v", err)
	}

	waitForStore(t, "readiness to go false on the superseded node", func() bool {
		code, _ := readiness(t, a)
		return code == http.StatusServiceUnavailable
	})

	// Not-ready is the observable; these say it is not-ready for the right
	// reason, so that a probe failing for any other cause could not satisfy the
	// wait above.
	if a.Node.Conflict() == nil {
		t.Error("readiness went false but the node reports no conflict")
	}
	if a.Node.IsLeader() {
		t.Error("a superseded node still claims leadership")
	}
	_, body := readiness(t, a)
	if !strings.Contains(body, `"disqualified":true`) {
		t.Errorf("the readiness body does not say why the node is out of rotation, so an "+
			"operator cannot tell it from a shutdown: %s", body)
	}
}

// TestClusterJoinFailureIsFatal covers the OTHER thing joinCluster swallowed.
//
// [cluster.Node.Start] fails for more than a duplicate id: the registry write is
// the first thing it does, and a store that cannot answer it fails the join.
// Start is called exactly once and its loop is what would retry, so the error
// that was logged and dropped left `cluster.enabled: true` running with no
// registry row, no election, no leader job — no lease reclaim, no reservation
// sweep — and publishing DESIGN §5.6's overshoot against a node count of one,
// which reads "exact", from a node that is in a cluster and not counting itself
// in it.
//
// The store here is opened without migrations, so the `nodes` table the join
// writes to does not exist. That is a real store failure rather than a fake: the
// registry statement is the one that fails, and it fails inside Node.Start
// exactly where a connection refused would.
func TestClusterJoinFailureIsFatal(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx, clusterOptions(t, sharedDSN(t), "node-unmigrated", "leased", true,
		func(o *Options) { o.SkipMigrate = true }))
	if err == nil {
		_ = a.Close(ctx)
		t.Fatal("a gateway with cluster.enabled: true started without joining the cluster; " +
			"it runs no leader job and publishes an overshoot of 0 from inside a cluster")
	}
	if a != nil {
		t.Errorf("app.New returned an error AND a gateway")
	}
	if errors.Is(err, cluster.ErrDuplicateNodeID) {
		t.Errorf("a store failure was reported as a duplicate node id: %v", err)
	}
	if !strings.Contains(err.Error(), "node-unmigrated") {
		t.Errorf("the refusal does not name the node that could not join: %v", err)
	}
}

// TestUnclusteredGatewayIgnoresTheJoin holds DESIGN §0.2 against the change
// above: making a refused join fatal must not make a notebook's start-up depend
// on a cluster it does not have. `cluster.enabled: false` never calls Start, so
// there is no failure to be fatal about — and the same unmigrated store that
// refuses the clustered gateway above must not refuse this one.
func TestUnclusteredGatewayIgnoresTheJoin(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx, clusterOptions(t, sharedDSN(t), "node-solo", "local", false,
		func(o *Options) { o.SkipMigrate = true }))
	if err != nil {
		t.Fatalf("an unclustered gateway refused to start over a cluster join it never makes: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })
	if code, body := readiness(t, a); code != http.StatusOK {
		t.Errorf("an unclustered gateway is not ready: %d %s", code, body)
	}
}

// scrapeInt reads one sample out of the assembled scrape. label is a substring
// match on the series' label set, or "" for an unlabelled family. It returns -1
// when the series is absent, which is a distinct answer from 0 — several of the
// assertions above turn on exactly that difference.
func scrapeInt(t *testing.T, a *App, family, label string) int64 {
	t.Helper()
	w := httptest.NewRecorder()
	a.Server.ServeHTTP(w, adminRequest(httptest.NewRequest(http.MethodGet, "/metrics", nil)))
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics answered %d", w.Code)
	}
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, family) || strings.HasPrefix(line, "#") {
			continue
		}
		rest := line[len(family):]
		if label != "" && !strings.Contains(rest, label) {
			continue
		}
		if label == "" && strings.HasPrefix(rest, "{") {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		var v int64
		if _, err := fmt.Sscanf(fields[len(fields)-1], "%d", &v); err != nil {
			continue
		}
		return v
	}
	return -1
}
