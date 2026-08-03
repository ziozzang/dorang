package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/store"
)

// Which node served this request, asked of the ledger.
//
// `request_logs.node_id` existed, [store.Store.InsertRequestLogs] wrote it and
// [store.Store] scanned it back, and nothing between the gateway and the row
// ever set it. On the operator's two-node cluster that was 52,766 rows of NULL,
// and "which node served this" is the first question asked when one node in a
// pair misbehaves — the ledger is where it has to be answerable, because a
// response header is gone by the time anyone asks.
//
// The assertion is the ROW, not the field. A test that set storeSink.nodeID and
// then read storeSink.nodeID back would observe one value in the place that
// produced it, which DESIGN §17.1 rule 3 rejects: this drives a request through
// the assembled gateway and reads what the store holds afterwards.
func TestTheLedgerNamesTheNodeThatServed(t *testing.T) {
	const nodeYAML = wiringYAML + `
cluster:
  enabled: true
  capacity_mode: shared-pg
  min_leasable: 4
  node_id: node-under-test
`
	a := newWiringApp(t, nodeYAML, func(c *config.Config) {
		// The cluster guard refuses `local` beside cluster.enabled, and the
		// shared modes coordinate through the store this app already has.
		c.Cluster.CapacityMode = config.CapacityModeSharedPG
	})
	secret := issueKey(t, a, nil)

	// Any request that reaches metering will do; the upstream is unreachable on
	// purpose, because a FAILED request needs its node recorded just as much as
	// a served one — arguably more, since that is the row an operator reads when
	// they suspect one node of a pair.
	if w := callWith(a, secret, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("/v1/models = %d: %s", w.Code, w.Body.String())
	}
	if err := a.Meter.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	rows := ledgerRows(t, a)
	if len(rows) == 0 {
		t.Fatal("the request wrote no ledger row at all")
	}
	for _, r := range rows {
		if r.NodeID != "node-under-test" {
			t.Errorf("ledger row %s carries node_id %q, want %q — the column is written "+
				"and read but nothing populated it, so a multi-node deployment cannot "+
				"attribute a request to a process",
				r.ID, r.NodeID, "node-under-test")
		}
	}
}

// ledgerRows reads back every request log the app has written.
func ledgerRows(t *testing.T, a *App) []store.RequestLog {
	t.Helper()
	now := time.Now()
	page, err := a.Store.ListErrors(context.Background(),
		store.TimeRange{Start: now.Add(-time.Hour), End: now.Add(time.Hour)},
		store.Page{Limit: 50})
	if err != nil && !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("ListErrors: %v", err)
	}
	if len(page.Rows) > 0 {
		return page.Rows
	}
	// ListErrors only returns non-2xx rows; a served request needs the by-key
	// query, which is the one an operator uses anyway.
	keys, err := a.Store.ListAPIKeys(context.Background(), store.APIKeyFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	var out []store.RequestLog
	for _, k := range keys {
		p, err := a.Store.ListRequestsByKey(context.Background(), k.ID,
			store.TimeRange{Start: now.Add(-time.Hour), End: now.Add(time.Hour)},
			store.Page{Limit: 50})
		if err != nil {
			t.Fatalf("ListRequestsByKey: %v", err)
		}
		out = append(out, p.Rows...)
	}
	return out
}

// A shared config file cannot name two nodes, so the id comes from the process.
//
// `cluster.node_id` is a literal in a file both nodes read: writing one there
// gives them the same id, which the election refuses as a duplicate, and
// leaving it unset mints a random id per process. Random is unique — which is
// all the election needs — and useless for the other thing a node id is for,
// because "this node has been the slow one all week" cannot be said about an
// identifier that changes at every restart.
//
// The assertion is again the ledger row rather than the resolver's return
// value: what matters is that the name an operator gave one container is what
// the durable record says about the requests it served.
func TestTheNodeIDComesFromTheEnvironmentWhenTheFileIsShared(t *testing.T) {
	const sharedYAML = wiringYAML + `
cluster:
  enabled: true
  capacity_mode: shared-pg
  min_leasable: 4
  node_id: written-in-the-shared-file
  node_id_env: DORANG_TEST_NODE_ID
`
	t.Setenv("DORANG_TEST_NODE_ID", "dorang-2")
	a := newWiringApp(t, sharedYAML, func(c *config.Config) {
		c.Cluster.CapacityMode = config.CapacityModeSharedPG
	})
	secret := issueKey(t, a, nil)

	if w := callWith(a, secret, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("/v1/models = %d: %s", w.Code, w.Body.String())
	}
	if err := a.Meter.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	rows := ledgerRows(t, a)
	if len(rows) == 0 {
		t.Fatal("the request wrote no ledger row at all")
	}
	for _, r := range rows {
		switch r.NodeID {
		case "dorang-2":
			// what the container was told it is called
		case "written-in-the-shared-file":
			t.Errorf("ledger row %s carries the literal from the shared config file; "+
				"both nodes reading that file would claim the same id", r.ID)
		default:
			t.Errorf("ledger row %s carries node_id %q, want %q", r.ID, r.NodeID, "dorang-2")
		}
	}
}
