package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/admin"
	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/store"
)

// DESIGN §11.2c, OPERATIONS §3.1 — the incident path, measured the same way
// invalidation_test.go measures the five controls.
//
// `POST /key/block` is the route an operator reaches for when a key leaks. Every
// assertion here is that the key STOPS SERVING on a node that did not handle the
// call; that a row was written, or that a function was invoked, is not asserted
// anywhere, because the defect this closes was precisely a correct row with no
// announcement behind it.

const invAdminToken = "operator-credential" // pragma: allowlist secret — test fixture

// --- the administration surface, over a real store ----------------------------

// adminAuth is an operator credential and nothing else. internal/admin's scope
// and role machinery is tested in its own package; what is needed here is a
// caller the surface admits.
type adminAuth struct{}

func (adminAuth) AuthenticateHeader(_ context.Context, h http.Header) (admin.Principal, error) {
	if strings.TrimSpace(strings.TrimPrefix(h.Get("Authorization"), "Bearer ")) != invAdminToken {
		return nil, admin.ErrUnauthenticated
	}
	return adminOperator{}, nil
}

type adminOperator struct{}

func (adminOperator) ActorKind() string       { return "master" }
func (adminOperator) ActorID() string         { return "" }
func (adminOperator) IsAdmin() bool           { return true }
func (adminOperator) AdminScope() admin.Scope { return admin.GlobalScope() }

// adminAudit counts rows. A mutation that cannot be audited is refused before it
// happens, so the surface needs one of these to mutate at all.
type adminAudit struct {
	mu sync.Mutex
	n  int
}

func (a *adminAudit) Record(context.Context, admin.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	return nil
}

func (a *adminAudit) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

// adminKeys is the administration surface's key store over a real store.
//
// It is the same adapter internal/app builds, narrowed to the operations these
// tests drive, and it is here for the reason authStore in invalidation_test.go
// is: this package's tests must not depend on that one. Only the routes below
// are implemented; everything else refuses rather than pretending, so a test
// that grew into an unimplemented route fails loudly instead of asserting
// against a stub.
type adminKeys struct{ st *store.Store }

var _ admin.KeyStore = (*adminKeys)(nil)

var errAdminKeysUnused = errors.New("cluster: this test adapter does not implement that operation")

func (a *adminKeys) GetKey(ctx context.Context, id string) (*admin.Key, error) {
	row, err := a.st.GetAPIKey(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, admin.ErrNotFound
		}
		return nil, err
	}
	return &admin.Key{
		ID: row.ID, KeyLabel: row.KeyLabel, KeyAlias: row.KeyAlias,
		UserID: row.UserID, TeamID: row.TeamID,
		Blocked: row.Blocked, ExpiresAt: row.ExpiresAt,
		Tier: row.Tier, PendedAt: row.PendedAt, PendReason: row.PendReason,
		PriorityClass: row.PriorityClass,
		HashScheme:    string(row.HashScheme), Source: row.Source,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// UpdateKey writes the fields these tests change, by reading the row and putting
// it back. The narrower shape is deliberate: the assertion is about whether a
// block PROPAGATES, and a converter that dropped a column on the way through
// would fail the test for an unrelated reason.
func (a *adminKeys) UpdateKey(ctx context.Context, k *admin.Key) error {
	row, err := a.st.GetAPIKey(ctx, k.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return admin.ErrNotFound
		}
		return err
	}
	row.Blocked = k.Blocked
	row.ExpiresAt = k.ExpiresAt
	row.UpdatedAt = k.UpdatedAt
	return a.st.UpdateAPIKey(ctx, row)
}

func (a *adminKeys) DeleteKeys(ctx context.Context, ids []string) (int, error) {
	return a.st.DeleteAPIKeys(ctx, ids)
}

func (a *adminKeys) CreateKey(context.Context, *admin.Key, admin.Verifier) error {
	return errAdminKeysUnused
}

func (a *adminKeys) ListKeys(context.Context, admin.KeyFilter) ([]*admin.Key, error) {
	return nil, errAdminKeysUnused
}

func (a *adminKeys) ReplaceVerifier(context.Context, string, admin.Verifier, string, time.Time) error {
	return errAdminKeysUnused
}

// newAdminAPI mounts the administration surface over one node's store, with the
// invalidator handed in so a test can build the wired and the unwired
// arrangement side by side.
func newAdminAPI(t *testing.T, st *store.Store, inv admin.Invalidator, now func() time.Time) (*admin.API, *adminAudit) {
	t.Helper()
	audit := &adminAudit{}
	api, err := admin.New(admin.Config{
		Auth:        adminAuth{},
		Keys:        &adminKeys{st: st},
		Audit:       audit,
		Invalidator: inv,
		Now:         now,
	})
	if err != nil {
		t.Fatalf("admin.New: %v", err)
	}
	return api, audit
}

func adminPost(t *testing.T, api *admin.API, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+invAdminToken)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	return rec
}

// stillServing polls a node until the key stops serving, and reports how long it
// took. ok is false if the deadline passed first.
func stopsServingWithin(a *auth.Authenticator, tok string, now func() time.Time,
	limit time.Duration) (time.Duration, error, bool) {

	start := time.Now()
	deadline := start.Add(limit)
	for {
		err := servingAt(a, tok, now())
		if err != nil {
			return time.Since(start), err, true
		}
		if time.Now().After(deadline) {
			return time.Since(start), nil, false
		}
		time.Sleep(time.Millisecond)
	}
}

// --- the incident path --------------------------------------------------------

func TestKeyBlockStopsServingOnASecondNode(t *testing.T) {
	const nodes = 2
	eachPepperedCluster(t, nodes, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		tok := "sk-cluster-admin-block" // pragma: allowlist secret — test fixture
		k := newInvKey(t, stores[0], tok)

		// A real poll interval with the loops actually running, so the figure
		// includes the wait rather than skipping it with a direct Poll().
		poll := 20 * time.Millisecond
		ns := make([]*node, nodes)
		for i := range ns {
			ns[i] = newInvNode(t, stores[i], "node-"+string(rune('1'+i)), clk.Now, poll)
			if err := ns[i].inv.Start(ctx); err != nil {
				t.Fatal(err)
			}
		}

		// Each node is serving from its own snapshot, learned independently. The
		// entry TTL on these authenticators is an HOUR: if the block were
		// landing on the cache TTL rather than on the invalidation, this test
		// would have to wait an hour to pass, which is the point of choosing it.
		for i, n := range ns {
			if err := servingAt(n.a, tok, clk.Now()); err != nil {
				t.Fatalf("node %d was not serving before the block: %v", i, err)
			}
		}

		api, audit := newAdminAPI(t, stores[0], ns[0].ctrl, clk.Now)
		bound := ns[0].inv.Bound(nodes)

		called := time.Now()
		rec := adminPost(t, api, "/key/block", map[string]any{"key_id": k.ID})
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /key/block: status %d\nbody: %s", rec.Code, rec.Body.String())
		}
		control := time.Since(called)

		// The node that served the call is immediate, because Announce applies
		// locally before it publishes. A control whose own node has a window is
		// a control the operator cannot verify.
		if err := servingAt(ns[0].a, tok, clk.Now()); err == nil {
			t.Fatal("the node that served POST /key/block went on serving the blocked key")
		} else if !errors.Is(err, auth.ErrBlocked) {
			t.Fatalf("the publishing node refused for the wrong reason: %v", err)
		}

		prop, refusal, stopped := stopsServingWithin(ns[1].a, tok, clk.Now, 10*time.Second)
		if !stopped {
			t.Fatalf("the second node was still serving a blocked key %v after POST /key/block "+
				"returned; the mutation wrote the row and published nothing", prop)
		}
		if !errors.Is(refusal, auth.ErrBlocked) {
			// "Stopped serving" has to mean blocked. A key refused as unknown
			// would satisfy a weaker assertion and would mean something else
			// entirely went wrong.
			t.Fatalf("the second node refused for the wrong reason: %v", refusal)
		}

		t.Logf("/key/block PROPAGATION (%d nodes, poll %v): the second node stopped serving %v "+
			"after the call returned, against a published bound of %v (%s); the handler itself "+
			"took %v; TTL fallback %v",
			nodes, poll, prop, bound.Bound, bound.Formula, control, bound.Fallback)

		if prop > bound.Bound {
			t.Errorf("measured %v, which exceeds the published bound of %v (%s)",
				prop, bound.Bound, bound.Formula)
		}
		if bound.Bound >= bound.Fallback {
			t.Errorf("the published bound %v is not better than the TTL fallback %v",
				bound.Bound, bound.Fallback)
		}
		if audit.count() != 1 {
			t.Errorf("audit rows = %d, want 1: announcing must not cost the trail", audit.count())
		}
	})
}

// TestKeyBlockWithoutAnInvalidatorObservesOnTheCacheTTL is the inverse, and it
// is what makes the test above an assertion about the fix rather than about the
// database.
//
// With no invalidator wired the surface writes exactly the same row — the block
// is durable, and a node that reads the store afresh honours it — and the second
// node keeps serving from its snapshot. That was the shipped behaviour of
// `POST /key/block` on every clustered deployment: 60 seconds of a leaked key
// serving, against a published bound of 270 ms.
func TestKeyBlockWithoutAnInvalidatorObservesOnTheCacheTTL(t *testing.T) {
	const nodes = 2
	eachPepperedCluster(t, nodes, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		tok := "sk-cluster-admin-unwired" // pragma: allowlist secret — test fixture
		k := newInvKey(t, stores[0], tok)

		poll := 20 * time.Millisecond
		ns := make([]*node, nodes)
		for i := range ns {
			ns[i] = newInvNode(t, stores[i], "node-"+string(rune('1'+i)), clk.Now, poll)
			if err := ns[i].inv.Start(ctx); err != nil {
				t.Fatal(err)
			}
		}
		for i, n := range ns {
			if err := servingAt(n.a, tok, clk.Now()); err != nil {
				t.Fatalf("node %d was not serving before the block: %v", i, err)
			}
		}

		api, _ := newAdminAPI(t, stores[0], nil, clk.Now)
		if rec := adminPost(t, api, "/key/block", map[string]any{"key_id": k.ID}); rec.Code != http.StatusOK {
			t.Fatalf("POST /key/block: status %d\nbody: %s", rec.Code, rec.Body.String())
		}

		// The row is durable: a reader that does not hold a snapshot sees it.
		row, err := stores[1].GetAPIKey(ctx, k.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !row.Blocked {
			t.Fatal("the block was not written; this test is measuring the wrong thing")
		}

		// And the second node serves it anyway, for as long as its cache lasts.
		// Twenty poll intervals is not a bound — it is enough to show that no
		// message is coming, which is what the ONE HOUR entry TTL on these
		// authenticators makes unambiguous.
		if _, _, stopped := stopsServingWithin(ns[1].a, tok, clk.Now, 20*poll); stopped {
			t.Fatal("the second node stopped serving without an invalidator; " +
				"something other than the announcement is closing this window, " +
				"and the measurement above is not measuring the fix")
		}
		t.Logf("without an invalidator the second node still serves the blocked key after %v; "+
			"its bound is the entry TTL (%v)", 20*poll, ns[1].a.EntryTTL())
	})
}

// TestKeyDeleteStopsServingOnASecondNode covers the mutation whose lookups are
// GONE by the time anything can announce them.
//
// A deleted key is the one case where the invalidation cannot name a single
// secret: the rows that held them are removed. It is also the case where the TTL
// fallback is at its worst, because nothing is left to refuse the credential —
// a node holding a cached copy simply keeps authenticating it.
func TestKeyDeleteStopsServingOnASecondNode(t *testing.T) {
	const nodes = 2
	eachPepperedCluster(t, nodes, func(t *testing.T, stores []*store.Store, clk *clock) {
		ctx := context.Background()
		tok := "sk-cluster-admin-delete" // pragma: allowlist secret — test fixture
		k := newInvKey(t, stores[0], tok)

		poll := 20 * time.Millisecond
		ns := make([]*node, nodes)
		for i := range ns {
			ns[i] = newInvNode(t, stores[i], "node-"+string(rune('1'+i)), clk.Now, poll)
			if err := ns[i].inv.Start(ctx); err != nil {
				t.Fatal(err)
			}
		}
		for i, n := range ns {
			if err := servingAt(n.a, tok, clk.Now()); err != nil {
				t.Fatalf("node %d was not serving before the delete: %v", i, err)
			}
		}

		api, _ := newAdminAPI(t, stores[0], ns[0].ctrl, clk.Now)
		bound := ns[0].inv.Bound(nodes)
		if rec := adminPost(t, api, "/key/delete",
			map[string]any{"keys": []string{k.ID}}); rec.Code != http.StatusOK {
			t.Fatalf("POST /key/delete: status %d\nbody: %s", rec.Code, rec.Body.String())
		}

		prop, refusal, stopped := stopsServingWithin(ns[1].a, tok, clk.Now, 10*time.Second)
		if !stopped {
			t.Fatalf("the second node was still serving a DELETED key %v after the call returned", prop)
		}
		if !errors.Is(refusal, auth.ErrUnknownKey) {
			t.Fatalf("a deleted key was refused for the wrong reason: %v", refusal)
		}
		t.Logf("/key/delete PROPAGATION (%d nodes, poll %v): %v against a published bound of %v",
			nodes, poll, prop, bound.Bound)
		if prop > bound.Bound {
			t.Errorf("measured %v, which exceeds the published bound of %v (%s)",
				prop, bound.Bound, bound.Formula)
		}
	})
}
