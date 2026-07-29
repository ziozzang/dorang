//go:build integration

package main

// Two dorang processes, one PostgreSQL, no injected clocks.
//
// Every claim internal/cluster makes about a fleet has until now been tested
// with several *store handles* inside one process and a fake clock shared
// between them. That arrangement can show the SQL is right. It cannot show that
// two operating-system processes -- separate connection pools, separate
// caches, separate wall clocks, separate schedulers, and a real network round
// trip between each of them and the database -- agree about who leads, how much
// of a ceiling is left, or whose leases are reclaimable.
//
// These tests run the real binary through the same re-exec harness the
// single-node process tests use (see start() in main_test.go). Nothing is
// mocked at the process boundary and nothing is mocked at the clock: a bound
// stated in seconds is measured in seconds.
//
// They are behind the `integration` tag and skip without DORANG_TEST_PG. See
// docs/OPERATIONS.md 8.5 for the command, and 10.1 for the numbers they measure.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

const testPepper = "two-node-pepper" // pragma: allowlist secret — test fixture

// pgSchema gives the calling test its own schema in the shared test database,
// so two tests can run against one PostgreSQL without seeing each other's
// nodes, leases or budgets.
func pgSchema(t *testing.T) string {
	t.Helper()
	base := os.Getenv("DORANG_TEST_PG")
	if base == "" {
		t.Skip("DORANG_TEST_PG is not set; skipping the two-node PostgreSQL tests")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()

	schema := "n_" + store.NewID()[:16]
	if _, err := admin.ExecContext(context.Background(), `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", base)
		if err != nil {
			return
		}
		defer db.Close()
		_, _ = db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse DORANG_TEST_PG: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// nodeStore opens a store handle for the TEST's own use: seeding keys, reading
// the tables the nodes write. It is a third participant on purpose -- an
// assertion made through a node's own handle would be asking the node about
// itself.
func nodeStore(t *testing.T, dsn string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), store.Config{
		Driver: store.DialectPostgres,
		DSN:    dsn,
		Pepper: []byte(testPepper),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// nodeConfig renders a clustered, PostgreSQL-backed configuration for one node.
//
// preStop is server.pre_stop_delay and is a parameter rather than a constant
// because it is the one drain knob whose correct value depends on the caller's
// balancer, not on dorang: DESIGN 13 requires
//
//	pre_stop_delay >= probe period x failure threshold + probe timeout
//	                  + endpoint-withdrawal propagation
//
// A test that hard-coded 0 here would be measuring a misconfiguration, and one
// that hard-coded 10s would be sleeping through every drain.
func nodeConfig(t *testing.T, dir, nodeID, upstreamURL, preStop string, extra ...string) string {
	t.Helper()
	src := fmt.Sprintf(`version: 1
server:
  listen: 127.0.0.1:0
  env: development
  shutdown_grace: 10s
  pre_stop_delay: %s
%s
storage:
  driver: postgres
  postgres:
    url_env: DORANG_TEST_NODE_DSN
    max_conns: 8
cluster:
  enabled: true
  node_id: %s
  capacity_mode: shared-pg
metering:
  flush_interval: 20ms
  spool:
    dir: %s/spool-%s
providers:
  - name: fake
    kind: openai
    base_url: %s/v1
    max_concurrency: 16
    capacity_group: fake-pool
credentials:
  - id: fake-1
    provider: fake
    key: upstream-secret
    capacity_group: fake-account
capacity:
  provider_groups:
    fake-pool: {max_concurrency: 32}
  credential_groups:
    fake-account: {max_concurrency: 32}
  models:
    - {provider: fake, model: upstream-x, max_concurrency: 32}
  global: {max_concurrency: 64}
classes:
  chat: [model-x]
models:
  - name: model-x
    class: chat
    strategy: [least_busy]
    deployments:
      - provider: fake
        upstream_model: upstream-x
        credentials: [fake-1]
pricing:
  currency: USD
  rules:
    - id: fake-tokens
      class: marginal_usage
      match: {provider: fake}
      rates: {input: "3.00", output: "15.00"}
`, preStop, strings.Join(extra, ""), nodeID, dir, nodeID, upstreamURL)

	path := filepath.Join(dir, "node-"+nodeID+".yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// startNode launches one gateway process against the shared database. The
// tests that never drain use noDrain, so they do not wait for a delay whose
// only purpose is to cover a balancer's detection window.
const (
	noDrain    = "0s"
	probeDrain = "3s"
)

func startNode(t *testing.T, dir, nodeID, upstreamURL, dsn, preStop string, env ...string) *child {
	t.Helper()
	return startNodeCfg(t, dir, nodeID, upstreamURL, dsn, preStop, "", env...)
}

// startNodeCfg is startNode with an extra top-level configuration block, for
// the tests that need to set a knob the published bound is stated in terms of.
func startNodeCfg(t *testing.T, dir, nodeID, upstreamURL, dsn, preStop, extra string, env ...string) *child {
	t.Helper()
	path := nodeConfig(t, dir, nodeID, upstreamURL, preStop, extra)
	e := append([]string{
		"DORANG_TEST_NODE_DSN=" + dsn,
		"DORANG_KEY_PEPPER=" + testPepper,
		"HOME=" + dir,
	}, env...)
	return start(t, []string{"--config", path}, e...)
}

// leaderRows reads who the cluster's own tables say is leading. It is read from
// a third connection, not from either node's memory: DESIGN 13's guarantee is
// about the fleet, and a node's opinion of itself is not evidence about it.
func leaderRows(t *testing.T, s *store.Store) (holder string, expires time.Time) {
	t.Helper()
	row := s.DB().QueryRowContext(context.Background(),
		`SELECT node_id, expires_at FROM capacity_leases WHERE axis = 'leader'`)
	var exp int64
	switch err := row.Scan(&holder, &exp); {
	case err == sql.ErrNoRows:
		return "", time.Time{}
	case err != nil:
		t.Fatalf("read leader lease: %v", err)
	}
	return holder, store.TimeAt(exp)
}

func liveNodes(t *testing.T, s *store.Store) []string {
	t.Helper()
	rows, err := s.DB().QueryContext(context.Background(),
		`SELECT node_id FROM nodes ORDER BY node_id`)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// eventually polls until fn is true or the deadline passes, returning how long
// it took. Every timing figure this file reports comes from here, so a bound is
// measured rather than slept through.
func eventually(t *testing.T, within time.Duration, what string, fn func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < within {
		if fn() {
			return time.Since(start)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %s", what, within)
	return 0
}

const chatBody = `{"model":"model-x","max_tokens":4,"messages":[{"role":"user","content":"ping"}]}`

// costPerRequest is fixed by the fake upstream's usage block: 11 input tokens
// at $3.00/1M plus 3 output at $15.00/1M.
const costPerRequest = 11*3_000 + 3*15_000

// seedBudgetedKey issues a key with a monthly ceiling and returns its token.
func seedBudgetedKey(t *testing.T, s *store.Store, alias string, limitNano int64) (token, id string) {
	t.Helper()
	token = "sk-" + alias + "-" + store.NewID()[:12]
	k := &store.APIKey{KeyAlias: alias, MaxBudgetNano: &limitNano, BudgetPeriod: "monthly"}
	if err := s.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAPIKey(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	return token, k.ID
}

// ---------------------------------------------------------------------------
// DESIGN 13: exactly one leader, and leader jobs run once across the fleet
// ---------------------------------------------------------------------------

// TestTwoProcessesElectExactlyOneLeader is DESIGN 13's central claim asserted
// across a process boundary.
//
// The observable is the lease row, not either node's IsLeader(): two processes
// can both believe they lead and the row is the only place that disagreement
// shows up. It is read from a third connection for the same reason.
func TestTwoProcessesElectExactlyOneLeader(t *testing.T) {
	dsn := pgSchema(t)
	dir := t.TempDir()
	up, _ := fakeUpstream(t, 0)
	s := nodeStore(t, dsn)

	a := startNode(t, dir, "a", up.URL, dsn, noDrain)
	b := startNode(t, dir, "b", up.URL, dsn, noDrain)

	// Both processes must join. A node that failed to register would make the
	// leader assertion below trivially true.
	eventually(t, 60*time.Second, "both nodes register", func() bool {
		return len(liveNodes(t, s)) == 2
	})

	took := eventually(t, 60*time.Second, "a leader is elected", func() bool {
		h, _ := leaderRows(t, s)
		return h != ""
	})
	holder, _ := leaderRows(t, s)
	t.Logf("MEASURED: a leader was elected %s after both processes joined; holder = %s", took, holder)

	if holder != "a" && holder != "b" {
		t.Fatalf("the leader lease is held by %q, which is neither running node", holder)
	}

	// One lease row, one holder, held continuously. Sampling matters: a second
	// leader that existed only between two campaigns would leave the final read
	// looking correct.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := s.DB().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM capacity_leases WHERE axis = 'leader'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("%d leader lease rows exist; DESIGN 13 allows one", n)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// Both processes must still be serving: leadership is not a routing
	// decision, and a node that lost the election still answers requests.
	for _, c := range []*child{a, b} {
		r := get(t, c.url("/health/readiness"), "")
		if r.status != http.StatusOK {
			t.Errorf("node at %s is not ready: %d %s", c.addr, r.status, r.body)
		}
	}
}

// TestLeaderJobsRunOnceAcrossTheFleet asserts the EFFECT of leader-only work
// rather than the flag that gates it.
//
// Partition pre-creation is the job to test this with, because its effect is a
// row in the catalog that either exists once or does not: two leaders running
// it concurrently is exactly the CREATE TABLE collision DESIGN 9.5 exists to
// prevent, and a collision is a job failure rather than a duplicate row.
func TestLeaderJobsRunOnceAcrossTheFleet(t *testing.T) {
	dsn := pgSchema(t)
	dir := t.TempDir()
	up, _ := fakeUpstream(t, 0)
	s := nodeStore(t, dsn)

	startNode(t, dir, "a", up.URL, dsn, noDrain)
	startNode(t, dir, "b", up.URL, dsn, noDrain)

	eventually(t, 60*time.Second, "both nodes register", func() bool {
		return len(liveNodes(t, s)) == 2
	})
	eventually(t, 60*time.Second, "a leader is elected", func() bool {
		h, _ := leaderRows(t, s)
		return h != ""
	})

	// Partitions must exist and must be a coherent set: PartitionAhead days
	// forward with no gaps. A second leader creating them concurrently either
	// errors (and the day is missing) or is serialized by the partition
	// advisory lock (and it is not).
	parts, err := s.ListPartitions(context.Background(), "request_logs")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) < 3 {
		t.Fatalf("only %d ledger partitions exist after two nodes started: %v", len(parts), parts)
	}
	seen := map[string]bool{}
	for _, p := range parts {
		if seen[p] {
			t.Fatalf("partition %s is listed twice", p)
		}
		seen[p] = true
	}

	// Every partition must be attached and writable. An orphan created by a
	// losing leader would be a table that no insert ever routes to.
	if err := s.InsertRequestLogs(context.Background(), []store.RequestLog{{
		ID: store.NewID(), TS: time.Now().UTC(), APIKeyID: "k", ModelGroup: "m", Metadata: "{}",
	}}); err != nil {
		t.Fatalf("a write after two nodes maintained partitions failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DESIGN 13: a hard-killed node's leases are reclaimed, within the bound
// ---------------------------------------------------------------------------

// TestKilledNodeLeasesAreReclaimed kills a node with SIGKILL -- no drain, no
// deregistration, no lease release -- and measures how long the fleet takes to
// get its units back.
//
// SIGKILL rather than SIGTERM is the point. A drained node returns everything
// on the way out, which tests the drain and not the reclaim. What an operator
// actually loses a node to is an OOM kill or a hypervisor, and the recovery
// path for that is the lease TTL plus the leader's sweep.
func TestKilledNodeLeasesAreReclaimed(t *testing.T) {
	dsn := pgSchema(t)
	dir := t.TempDir()
	up, _ := fakeUpstream(t, 0)
	s := nodeStore(t, dsn)

	a := startNode(t, dir, "a", up.URL, dsn, noDrain)
	b := startNode(t, dir, "b", up.URL, dsn, noDrain)

	eventually(t, 60*time.Second, "both nodes register", func() bool {
		return len(liveNodes(t, s)) == 2
	})

	// Give the doomed node a budget block to hold, so it dies holding
	// something. Its block is charged in budget_state before a unit is handed
	// out (DESIGN 9.6), which is the state a killed process actually leaves.
	token, _ := seedBudgetedKey(t, s, "killed", 100*costPerRequest)
	served := 0
	for i := 0; i < 3; i++ {
		if r := post(t, a.url("/v1/chat/completions"), token, chatBody); r.status == http.StatusOK {
			served++
		}
	}
	if served == 0 {
		t.Fatal("node a served nothing, so it holds no lease to reclaim")
	}

	held := func(node string) int64 {
		var n int64
		if err := s.DB().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM quota_leases WHERE node_id = $1`, node).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if held("a") == 0 {
		t.Fatal("node a holds no quota_leases row after serving; nothing to reclaim")
	}

	// Hard kill. Nothing runs on the way out.
	if err := a.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = a.cmd.Wait()
	killed := time.Now()

	// The bound is NOT the node TTL, and getting that wrong is how an operator
	// sizes a fleet against a number twice too small. The registry declares the
	// node dead after cluster.NodeTTL (30s), but the reclaim then refuses to
	// take a lease that has not expired -- deliberately, because a lapsed
	// heartbeat is a declaration and a lapsed lease is a fact, and reclaiming a
	// live node's block is precisely the defect that admitted 190 against 100.
	// So the units come back one LEDGER lease TTL after the dead node's last
	// renewal, plus however many leader ticks pass before a sweep lands after
	// that instant:
	//
	//	DefaultLedgerTTL (60s) + up to 2 x DefaultTick (5s) = 70s
	//
	// Two ticks and not one, measured: the sweep is scheduled on the leader's
	// own tick phase, which is unrelated to when the dead node's lease happens
	// to expire, so a sweep that lands just before the expiry costs a whole
	// further period. Runs of this test have produced 65.2s and 70.2s. A bound
	// stated as one tick would be a bound that is wrong one time in two, which
	// is the failure mode this whole file exists to find.
	bound := cluster.DefaultLedgerTTL + 3*cluster.DefaultTick
	took := eventually(t, 3*time.Minute, "the killed node's leases are reclaimed", func() bool {
		return held("a") == 0
	})
	t.Logf("MEASURED: a SIGKILLed node's quota leases were reclaimed %s after the kill "+
		"(ledger lease TTL %s + up to two leader ticks of %s = %s)",
		took.Round(100*time.Millisecond), cluster.DefaultLedgerTTL, cluster.DefaultTick,
		cluster.DefaultLedgerTTL+2*cluster.DefaultTick)
	if took > bound {
		t.Errorf("reclaim took %s, beyond %s (ledger lease TTL + two leader ticks, "+
			"with a third tick of slack)", took, bound)
	}
	_ = killed

	// And the survivor is still serving, which is what R14 means by highly
	// available: the fleet lost a node, not its ability to answer.
	if r := post(t, b.url("/v1/chat/completions"), token, chatBody); r.status != http.StatusOK {
		t.Fatalf("the surviving node answered %d after its peer was killed:\n%s", r.status, r.body)
	}
}

// ---------------------------------------------------------------------------
// DESIGN 5.6: the published overshoot, across two processes
// ---------------------------------------------------------------------------

// TestBudgetCeilingHoldsAcrossTwoProcesses is DESIGN 5.6's `shared-pg` figure
// measured rather than asserted.
//
// The published number is 0: all nodes together must not admit one unit beyond
// the ceiling while every holder's lease is live. The observable is a REFUSED
// REQUEST at an HTTP surface, on both nodes, not a field on a coordinator --
// two processes that agreed about a counter and still both served the last
// request would pass a field assertion and fail the deployment.
//
// Both nodes are driven at once and from many goroutines, because the failure
// this guards against is a race and a serial test would never open the window.
func TestBudgetCeilingHoldsAcrossTwoProcesses(t *testing.T) {
	dsn := pgSchema(t)
	dir := t.TempDir()
	up, _ := fakeUpstream(t, 0)
	s := nodeStore(t, dsn)

	a := startNode(t, dir, "a", up.URL, dsn, noDrain)
	b := startNode(t, dir, "b", up.URL, dsn, noDrain)
	eventually(t, 60*time.Second, "both nodes register", func() bool {
		return len(liveNodes(t, s)) == 2
	})

	// A ceiling of exactly 40 requests' worth. Small enough that both nodes are
	// contending for the last blocks, large enough that the first request is
	// not the refusal.
	const requests = 40
	limit := int64(requests) * costPerRequest
	token, keyID := seedBudgetedKey(t, s, "shared", limit)

	pub, err := cluster.Publish(cluster.ModeSharedPG, cluster.Params{
		Limit: limit, Nodes: 2, Clustered: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var served, refused atomic.Int64
	var wg sync.WaitGroup
	for _, c := range []*child{a, b} {
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(c *child) {
				defer wg.Done()
				for j := 0; j < requests; j++ {
					r, err := postErr(c.url("/v1/chat/completions"), token, chatBody, 60*time.Second)
					if err != nil {
						t.Errorf("post: %v", err)
						return
					}
					switch r.status {
					case http.StatusOK:
						served.Add(1)
					case http.StatusBadRequest:
						refused.Add(1)
					default:
						t.Errorf("unexpected status %d:\n%s", r.status, r.body)
						return
					}
				}
			}(c)
		}
	}
	wg.Wait()

	if refused.Load() == 0 {
		t.Fatalf("two nodes served all %d attempts against a ceiling of %d requests; "+
			"the ceiling is not shared", served.Load(), requests)
	}
	if served.Load() == 0 {
		t.Fatal("nothing was served, so the ceiling proves nothing")
	}

	spentNano := served.Load() * costPerRequest
	overshootNano := spentNano - limit
	if overshootNano < 0 {
		overshootNano = 0
	}
	t.Logf("MEASURED: shared-pg over two processes admitted %d requests against a ceiling of %d "+
		"(%d nano-USD spent against a %d nano-USD limit); overshoot %d nano-USD, published %d",
		served.Load(), requests, spentNano, limit, overshootNano, pub.MaxOvershoot)

	if overshootNano > pub.MaxOvershoot {
		t.Errorf("two processes exceeded the ceiling by %d nano-USD (%d requests); "+
			"DESIGN 5.6 publishes %d for shared-pg",
			overshootNano, overshootNano/costPerRequest, pub.MaxOvershoot)
	}

	// The durable counter must agree with what was served. A ledger that let
	// requests through without charging them would pass the count above and
	// still be a budget that is not enforced.
	key := cluster.BudgetKey("key", keyID, quota.Monthly, time.Now())
	l, err := cluster.NewLedger(cluster.LedgerConfig{Store: s, NodeID: "observer"})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := l.Committed(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if committed < spentNano {
		t.Errorf("the durable counter holds %d nano-USD after %d requests spent %d; "+
			"spend was served without being charged", committed, served.Load(), spentNano)
	}
}

// ---------------------------------------------------------------------------
// DESIGN 13 / 10: a rolling restart of two nodes loses no request
// ---------------------------------------------------------------------------

// TestRollingRestartOfTwoNodesLosesNoRequest restarts both nodes one at a time
// while a client drives them through a balancer that honours readiness, and
// requires every request to be answered.
//
// This is what two nodes are FOR. DESIGN 13's drain table exists so that a
// rolling restart is invisible to a client, and OPERATIONS is explicit that a
// single node cannot do it. The claim has never been checked with two.
//
// The "balancer" is deliberately readiness-aware and deliberately simple: it is
// what a real one does (stop routing to a node whose readiness probe fails) and
// nothing more. A test that kept routing to a draining node would be testing
// the absence of a balancer.
func TestRollingRestartOfTwoNodesLosesNoRequest(t *testing.T) {
	dsn := pgSchema(t)
	dir := t.TempDir()
	up, _ := fakeUpstream(t, 0)
	s := nodeStore(t, dsn)

	nodes := map[string]*child{
		"a": startNode(t, dir, "a", up.URL, dsn, probeDrain),
		"b": startNode(t, dir, "b", up.URL, dsn, probeDrain),
	}
	eventually(t, 60*time.Second, "both nodes register", func() bool {
		return len(liveNodes(t, s)) == 2
	})

	// A generous ceiling: this test is about availability, not about budgets.
	token, _ := seedBudgetedKey(t, s, "rolling", 1_000_000*costPerRequest)

	var mu sync.Mutex
	ready := map[string]bool{"a": true, "b": true}
	addr := map[string]string{"a": nodes["a"].addr, "b": nodes["b"].addr}

	// The balancer polls readiness the way a real one does, and takes a node
	// out on the first failure.
	stopProbe := make(chan struct{})
	var probeWG sync.WaitGroup
	probeWG.Add(1)
	go func() {
		defer probeWG.Done()
		c := &http.Client{Timeout: time.Second}
		for {
			select {
			case <-stopProbe:
				return
			case <-time.After(100 * time.Millisecond):
			}
			for _, id := range []string{"a", "b"} {
				mu.Lock()
				a := addr[id]
				mu.Unlock()
				ok := false
				if a != "" {
					if resp, err := c.Get("http://" + a + "/health/readiness"); err == nil {
						ok = resp.StatusCode == http.StatusOK
						resp.Body.Close()
					}
				}
				mu.Lock()
				ready[id] = ok
				mu.Unlock()
			}
		}
	}()

	var sent, ok, lost atomic.Int64
	var failures []string
	var failMu sync.Mutex
	stopLoad := make(chan struct{})
	var loadWG sync.WaitGroup
	for i := 0; i < 4; i++ {
		loadWG.Add(1)
		go func() {
			defer loadWG.Done()
			for {
				select {
				case <-stopLoad:
					return
				default:
				}
				// Pick a node the balancer believes is ready.
				mu.Lock()
				var target string
				for _, id := range []string{"a", "b"} {
					if ready[id] && addr[id] != "" {
						target = addr[id]
						break
					}
				}
				mu.Unlock()
				if target == "" {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				sent.Add(1)
				r, err := postErr("http://"+target+"/v1/chat/completions", token, chatBody, 30*time.Second)
				switch {
				case err != nil:
					lost.Add(1)
					failMu.Lock()
					if len(failures) < 10 {
						failures = append(failures, "transport: "+err.Error())
					}
					failMu.Unlock()
				case r.status == http.StatusOK:
					ok.Add(1)
				default:
					lost.Add(1)
					failMu.Lock()
					if len(failures) < 10 {
						failures = append(failures, fmt.Sprintf("status %d: %s", r.status, r.body))
					}
					failMu.Unlock()
				}
			}
		}()
	}

	// Let the load settle before touching anything.
	time.Sleep(2 * time.Second)
	before := ok.Load()
	if before == 0 {
		t.Fatal("no request succeeded before the roll; the fixture is broken")
	}

	// Roll each node: SIGTERM, wait for exit, start a replacement under the
	// same id, wait for it to be ready again.
	for _, id := range []string{"a", "b"} {
		c := nodes[id]
		if code := c.signalAndWait(t, syscall.SIGTERM, 60*time.Second); code != 0 {
			t.Errorf("node %s exited %d on SIGTERM\nstderr:\n%s", id, code, c.err())
		}
		mu.Lock()
		ready[id] = false
		addr[id] = ""
		mu.Unlock()

		next := startNode(t, dir, id, up.URL, dsn, probeDrain)
		nodes[id] = next
		mu.Lock()
		addr[id] = next.addr
		mu.Unlock()
		eventually(t, 60*time.Second, "node "+id+" is ready again", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return ready[id]
		})
		// Let the restarted node take real traffic before rolling the other.
		time.Sleep(1500 * time.Millisecond)
	}

	close(stopLoad)
	loadWG.Wait()
	close(stopProbe)
	probeWG.Wait()

	t.Logf("MEASURED: rolling restart of two nodes: %d requests sent, %d answered, %d lost",
		sent.Load(), ok.Load(), lost.Load())
	if lost.Load() != 0 {
		failMu.Lock()
		detail := strings.Join(failures, "\n  ")
		failMu.Unlock()
		t.Fatalf("a rolling restart of two nodes lost %d of %d requests:\n  %s",
			lost.Load(), sent.Load(), detail)
	}
	if ok.Load() < before*2 {
		t.Fatalf("only %d requests were answered in total against %d before the roll; "+
			"the load stopped rather than continuing through it", ok.Load(), before)
	}

	// The fleet must be back to two registered nodes, not four rows of history.
	live := liveNodes(t, s)
	if len(live) != 2 {
		t.Fatalf("after rolling both nodes the registry holds %v, want two", live)
	}
	holder, _ := leaderRows(t, s)
	if holder == "" {
		t.Fatal("no node holds the leader lease after the roll")
	}
}


// ---------------------------------------------------------------------------
// DESIGN 11.2c: revocation propagates within the published bound
// ---------------------------------------------------------------------------

// TestRevocationPropagatesAcrossTwoProcessesOnPostgres measures §11.2c's
// clustered bound on the store a cluster actually runs.
//
// The published figure is `poll + store_latency`, and the measurement DESIGN
// records against it -- ~21 ms to the last of four nodes -- was taken on
// SQLite, in ONE process, with four store handles that share a file. That is
// not the deployment the number is published for. On PostgreSQL the subscriber
// is a separate process on a separate connection, the write it is waiting for
// went through a real network round trip, and it becomes visible under
// PostgreSQL's own snapshot rules rather than under a shared page cache. None
// of that is guaranteed to land inside the same bound, and nothing has checked.
//
// The mechanism under test is the durable invalidation bus (§11.2c): the
// control applies locally and appends a row to `key_invalidations`, and every
// other node polls that table. The publish here is [store.Store.PublishInvalidation],
// which is exactly what [cluster.KeyInvalidator.Publish] calls.
//
// The observable is a REFUSED REQUEST at node b's HTTP surface. A watermark
// that advanced without the request path noticing would pass a field assertion
// and leave a revoked key serving traffic.
func TestRevocationPropagatesAcrossTwoProcessesOnPostgres(t *testing.T) {
	dsn := pgSchema(t)
	dir := t.TempDir()
	up, _ := fakeUpstream(t, 0)
	s := nodeStore(t, dsn)

	// poll and store_latency are named explicitly so the bound this is measured
	// against is the same 270 ms DESIGN 11.2c's SQLite figure was measured
	// against, rather than the 1.25 s default. Comparing a PostgreSQL
	// measurement to a different bound would not be a comparison.
	//
	// entry_ttl is long on purpose: it is the FALLBACK, and leaving it short
	// would let a cache expiry masquerade as a propagated invalidation. If the
	// bus does not work, this test must wait 10 minutes and fail, not pass in
	// 60 seconds for the wrong reason.
	const revocation = `auth:
  revocation:
    poll: 20ms
    store_latency: 250ms
    entry_ttl: 600s
    negative_ttl: 1s
`
	const publishedBound = 20*time.Millisecond + 250*time.Millisecond

	a := startNodeCfg(t, dir, "a", up.URL, dsn, noDrain, revocation)
	b := startNodeCfg(t, dir, "b", up.URL, dsn, noDrain, revocation)
	eventually(t, 60*time.Second, "both nodes register", func() bool {
		return len(liveNodes(t, s)) == 2
	})

	token, keyID := seedBudgetedKey(t, s, "revoked", 1_000_000*costPerRequest)

	// Both nodes must serve it first, and must have CACHED it -- otherwise the
	// refusal below would prove only that a node re-read a blocked row, which
	// is the fallback and not the mechanism.
	for _, c := range []*child{a, b} {
		for i := 0; i < 2; i++ {
			if r := post(t, c.url("/v1/chat/completions"), token, chatBody); r.status != http.StatusOK {
				t.Fatalf("node at %s refused the key before it was revoked: %d\n%s",
					c.addr, r.status, r.body)
			}
		}
	}

	// The durable half of the control: the key stops being valid in the store.
	ctx := context.Background()
	lookups, err := s.RevokeKey(ctx, keyID)
	if err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	// The published half: append to the bus. The bound is stated from the
	// moment the control returns, so the clock starts here.
	if _, err := s.PublishInvalidation(ctx, store.KeyInvalidation{
		KeyID:   keyID,
		Lookups: lookups,
		Cause:   "revoked",
	}); err != nil {
		t.Fatalf("PublishInvalidation: %v", err)
	}
	published := time.Now()

	// Both nodes are subscribers here: neither ran the control, so both have to
	// learn it from the table. Measure the LAST one, which is what the bound is
	// about.
	var last time.Duration
	for _, c := range []*child{a, b} {
		var took time.Duration
		deadline := time.Now().Add(10 * time.Minute)
		for time.Now().Before(deadline) {
			r, err := postErr(c.url("/v1/chat/completions"), token, chatBody, 30*time.Second)
			if err != nil {
				t.Fatalf("post to %s: %v", c.addr, err)
			}
			if r.status != http.StatusOK {
				took = time.Since(published)
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if took == 0 {
			t.Fatalf("node at %s still served a revoked key 10 minutes after the "+
				"invalidation was published; the bus is not reaching the request path", c.addr)
		}
		if took > last {
			last = took
		}
	}

	t.Logf("MEASURED: on PostgreSQL, two processes, poll 20ms + store_latency 250ms: "+
		"the last node refused a revoked key %s after the invalidation was published, "+
		"against a published %s (DESIGN 11.2c records ~21ms for four nodes on SQLite)",
		last.Round(time.Millisecond), publishedBound)
	if last > publishedBound {
		t.Errorf("revocation reached the last node in %s, beyond the published bound of %s "+
			"(DESIGN 11.2c: poll + store_latency)", last, publishedBound)
	}
}
