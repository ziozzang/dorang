package admin

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
)

// DESIGN §11.2c, OPERATIONS §3.1 — `POST /user/update` measured the way
// `POST /key/block` is measured in internal/cluster.
//
// The assertion is that the user's key STOPS SERVING on a node that did not
// handle the call, within the published bound. That a row was written, or that
// an interface method was invoked, is asserted nowhere here: the defect this
// closes was a correct row with no announcement behind it, and every weaker
// assertion passes against that defect.
//
// # Why the two nodes are built here rather than in internal/cluster
//
// internal/cluster measures the KEY controls, and its harness reaches the store
// through the adapter that has them. The subject half needs something that
// package does not have: an [auth.Principal] whose User and Team envelopes are
// populated, which is where `blocked` on a user reaches the hot path
// ([auth.Authenticator.use] refuses on `p.User.Blocked`). [auth.Store] is one
// method, so the whole two-node arrangement is a shared row set, two snapshots
// and a message table, and building it beside the handlers under test keeps the
// measurement in the package whose defect it is.

// --- the shared credential rows ----------------------------------------------

// credStore resolves an index key to the three-subject envelope internal/auth
// caches. Both nodes read the same one, which is what makes them a cluster
// rather than two unrelated processes.
//
// It is the piece that carries the USER and TEAM limits onto the principal.
// Without them a user's block is a column no request path reads, and the
// measurement below would be measuring nothing.
type credStore struct {
	st *fakeStore
	h  *auth.Hasher
}

var _ auth.Store = (*credStore)(nil)

func (s *credStore) LoadByLookup(_ context.Context, lookup string) (auth.Record, error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()

	var (
		id string
		v  Verifier
	)
	for keyID, ver := range s.st.verifiers {
		if ver.Lookup == lookup {
			id, v = keyID, ver
			break
		}
	}
	if id == "" {
		return auth.Record{}, auth.ErrNotFound
	}
	k := s.st.keys[id]
	if k == nil {
		return auth.Record{}, auth.ErrNotFound
	}
	digest, err := auth.ParseDigest(v.TokenHash)
	if err != nil {
		return auth.Record{}, err
	}
	scheme, err := auth.ParseScheme(v.HashScheme)
	if err != nil {
		return auth.Record{}, err
	}
	p := auth.Principal{
		KeyID:    k.ID,
		SecretID: k.ID + ".1",
		UserID:   k.UserID,
		TeamID:   k.TeamID,
		Key: auth.Limits{
			Blocked:          k.Blocked,
			ExpiresAt:        k.ExpiresAt,
			Models:           k.Models,
			AllowedRoutes:    k.AllowedRoutes,
			MaxBudgetNanoUSD: k.MaxBudgetNano,
			SpentNanoUSD:     k.SpendNano,
			BudgetPeriod:     k.BudgetPeriod,
			BudgetResetAt:    k.BudgetResetAt,
			RPMLimit:         k.RPMLimit,
			TPMLimit:         k.TPMLimit,
		},
	}
	if u := s.st.users[k.UserID]; u != nil {
		p.User = &auth.Limits{
			Blocked:          u.Blocked,
			Models:           u.Models,
			MaxBudgetNanoUSD: u.MaxBudgetNano,
			SpentNanoUSD:     u.SpendNano,
			BudgetPeriod:     u.BudgetPeriod,
			BudgetResetAt:    u.BudgetResetAt,
			RPMLimit:         u.RPMLimit,
			TPMLimit:         u.TPMLimit,
		}
	}
	if tm := s.st.teams[k.TeamID]; tm != nil {
		p.Team = &auth.Limits{
			Blocked:          tm.Blocked,
			Models:           tm.Models,
			MaxBudgetNanoUSD: tm.MaxBudgetNano,
			SpentNanoUSD:     tm.SpendNano,
			BudgetPeriod:     tm.BudgetPeriod,
			BudgetResetAt:    tm.BudgetResetAt,
			RPMLimit:         tm.RPMLimit,
			TPMLimit:         tm.TPMLimit,
			MaxParallel:      tm.MaxParallel,
		}
	}
	return auth.Record{Lookup: v.Lookup, Digest: digest, Scheme: scheme, Principal: p}, nil
}

// realHasher replaces [fakeHasher] with internal/auth's own, so that a verifier
// this surface stored is one the authenticators below can actually verify.
type realHasher struct{ h *auth.Hasher }

func (r realHasher) Lookup(token string) string { return r.h.Lookup(token).Hex() }

func (r realHasher) Hash(token string) (string, string, error) {
	d, err := r.h.Hash(auth.SchemeDorangV1, token)
	if err != nil {
		return "", "", err
	}
	return d.Hex(), auth.SchemeDorangV1.String(), nil
}

func (r realHasher) Label(token string) string { return "dk-" + r.h.Lookup(token).Hex()[:8] }

// --- the message table --------------------------------------------------------

// invBus is the durable invalidation table of §11.2c: one node appends, every
// node polls. It stands in for the store-backed transport internal/cluster
// implements, and it has the property the published bound depends on — a
// message is visible to a subscriber only on its next poll.
type invBus struct {
	mu   sync.Mutex
	msgs []auth.Invalidation
}

var _ auth.Sink = (*invBus)(nil)

func (b *invBus) Publish(_ context.Context, inv auth.Invalidation) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, inv)
	return nil
}

func (b *invBus) since(n int) []auth.Invalidation {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n >= len(b.msgs) {
		return nil
	}
	return append([]auth.Invalidation(nil), b.msgs[n:]...)
}

// authNode is one node: a snapshot of its own and a poller that applies what the
// other node published.
type authNode struct {
	a    *auth.Authenticator
	stop chan struct{}
	done chan struct{}
}

func (n *authNode) close() {
	close(n.stop)
	<-n.done
}

// newAuthNode builds a node and starts its poller.
//
// The entry TTL is an HOUR. If a mutation were landing on the credential cache
// rather than on the announcement, every test below would have to wait an hour
// to pass, which is the point of choosing it.
func newAuthNode(t *testing.T, cs *credStore, bus *invBus, sink auth.Sink, poll time.Duration) *authNode {
	t.Helper()
	a, err := auth.New(auth.Config{
		Pepper:      "test-pepper",
		NoMasterKey: true,
		Store:       cs,
		Sink:        sink,
		EntryTTL:    time.Hour,
		NegativeTTL: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	n := &authNode{a: a, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(n.done)
		seen := 0
		tick := time.NewTicker(poll)
		defer tick.Stop()
		for {
			select {
			case <-n.stop:
				return
			case <-tick.C:
				msgs := bus.since(seen)
				seen += len(msgs)
				for _, m := range msgs {
					a.Apply(m)
				}
			}
		}
	}()
	t.Cleanup(n.close)
	return n
}

// announcer is the [Invalidator] internal/cluster's KeyControl implements,
// narrowed to what this package calls. It applies locally and publishes, in that
// order, which is the whole of §11.2c rule 1.
type announcer struct {
	a  *auth.Authenticator
	st *fakeStore
}

var _ Invalidator = (*announcer)(nil)

func (x *announcer) InvalidateKey(ctx context.Context, keyID, cause string) error {
	c, ok := auth.ParseCause(cause)
	if !ok {
		c = auth.CauseUnspecified
	}
	// The lookups are read here rather than passed in, exactly as KeyControl
	// does it: the key id is authoritative and the lookups only make the drop
	// O(1). A deleted key has none left and is still dropped.
	x.st.mu.Lock()
	var lookups []string
	if v, ok := x.st.verifiers[keyID]; ok && v.Lookup != "" {
		lookups = []string{v.Lookup}
	}
	x.st.mu.Unlock()
	return x.a.Announce(ctx, auth.Invalidation{
		KeyID: keyID, Lookups: lookups, Cause: c, At: time.Now(),
	})
}

// --- the arrangement -----------------------------------------------------------

// writeThroughBudgets is [fakeStore]'s budget store with the one fidelity these
// tests need: dorang has no reusable named-budget object, so §9.2 carries the
// ceiling ON THE SUBJECT ROW (`api_keys.max_budget_nano` and its siblings). The
// plain fake keeps a separate map, which is enough for the endpoint's own tests
// and would make the measurement below assert nothing — the authenticator reads
// the subject row.
type writeThroughBudgets struct{ st *fakeStore }

var _ BudgetStore = (*writeThroughBudgets)(nil)

func (w *writeThroughBudgets) SetBudget(ctx context.Context, b Budget) error {
	if err := w.st.SetBudget(ctx, b); err != nil {
		return err
	}
	w.apply(b.Subject, b.MaxBudgetNano)
	return nil
}

func (w *writeThroughBudgets) ClearBudget(ctx context.Context, s BudgetSubject) error {
	if err := w.st.ClearBudget(ctx, s); err != nil {
		return err
	}
	w.apply(s, nil)
	return nil
}

func (w *writeThroughBudgets) apply(s BudgetSubject, max *int64) {
	w.st.mu.Lock()
	defer w.st.mu.Unlock()
	switch s.Kind {
	case "key":
		if k := w.st.keys[s.ID]; k != nil {
			k.MaxBudgetNano = max
		}
	case "user":
		if u := w.st.users[s.ID]; u != nil {
			u.MaxBudgetNano = max
		}
	case "team":
		if t := w.st.teams[s.ID]; t != nil {
			t.MaxBudgetNano = max
		}
	}
}

func (w *writeThroughBudgets) GetBudget(ctx context.Context, s BudgetSubject) (Budget, error) {
	return w.st.GetBudget(ctx, s)
}

func (w *writeThroughBudgets) ListBudgets(ctx context.Context, o ListOptions) ([]Budget, error) {
	return w.st.ListBudgets(ctx, o)
}

// clusterPoll is the invalidation poll interval these tests run at, and
// clusterStoreLatency the store round trip the published bound assumes. Together
// they are the 270 ms OPERATIONS publishes.
const (
	clusterPoll          = 20 * time.Millisecond
	clusterStoreLatency  = 250 * time.Millisecond
	clusterNodes         = 2
	clusterObserveWindow = 10 * time.Second
)

type twoNodes struct {
	h    *harness
	bus  *invBus
	up   *authNode // the node that serves the administrative call
	down *authNode // the node that only ever receives the message
}

// newTwoNodes builds the administration surface over node one's store, with node
// two serving the same rows from a snapshot it learned independently.
//
// wired chooses whether an invalidator is configured at all, which is what makes
// the measurement an assertion about the fix rather than about the fake.
func newTwoNodes(t *testing.T, wired bool) *twoNodes {
	t.Helper()
	hasher, err := auth.NewHasher("test-pepper", auth.LegacyPolicy{})
	if err != nil {
		t.Fatalf("auth.NewHasher: %v", err)
	}
	bus := &invBus{}
	tn := &twoNodes{bus: bus}

	h := newHarness(t, func(c *Config) {
		c.Hasher = realHasher{h: hasher}
		c.Budgets = &writeThroughBudgets{st: c.Keys.(*fakeStore)}
	})
	tn.h = h
	cs := &credStore{st: h.store, h: hasher}

	// Only the publishing node has a sink. The other one polls, which is the
	// asymmetry the bound is about.
	tn.up = newAuthNode(t, cs, bus, bus, clusterPoll)
	tn.down = newAuthNode(t, cs, bus, nil, clusterPoll)

	if wired {
		h.api.cfg.Invalidator = &announcer{a: tn.up.a, st: h.store}
	}
	return tn
}

// bound is the figure OPERATIONS publishes for this arrangement.
func (tn *twoNodes) bound() auth.RevocationBound {
	return tn.up.a.RevocationBound(clusterNodes, auth.PropagationDelay{
		Delay:  clusterPoll + clusterStoreLatency,
		Source: "the invalidation table, polled",
	})
}

// serving reports whether a node would still serve a request presenting the
// token.
//
// Authenticate AND Authorize, because "whether a request is served" is decided
// by both and the three classes of mutation land on different ones: a block is
// refused in the authenticator's own kill switches, a ceiling only in
// [auth.Principal.Authorize]. A helper that asked one of them would report a
// budget change as no change at all.
func serving(n *authNode, token string) error {
	p, err := n.a.Authenticate(context.Background(), token)
	if err != nil {
		return err
	}
	return p.Authorize(auth.Access{Now: time.Now()})
}

// principalOn returns what a node currently resolves the token to, or nil when
// it refuses it outright.
func principalOn(n *authNode, token string) *auth.Principal {
	p, err := n.a.Authenticate(context.Background(), token)
	if err != nil {
		return nil
	}
	return p
}

// stopsServingWithin polls a node until the token stops authenticating and
// reports how long it took. ok is false if the deadline passed first.
func stopsServingWithin(n *authNode, token string, limit time.Duration) (time.Duration, error, bool) {
	start := time.Now()
	deadline := start.Add(limit)
	for {
		if err := serving(n, token); err != nil {
			return time.Since(start), err, true
		}
		if time.Now().After(deadline) {
			return time.Since(start), nil, false
		}
		time.Sleep(time.Millisecond)
	}
}

// warm makes both nodes learn the credential independently, so that each is
// answering from its own snapshot when the control fires.
func (tn *twoNodes) warm(t *testing.T, token string) {
	t.Helper()
	for i, n := range []*authNode{tn.up, tn.down} {
		if err := serving(n, token); err != nil {
			t.Fatalf("node %d was not serving before the control: %v", i+1, err)
		}
	}
}

// --- the measurement -----------------------------------------------------------

// TestUserBlockStopsServingOnASecondNode is the observable for the finding.
//
// `POST /user/update` with `{"blocked": true}` is what an operator runs when a
// person leaves, and it is the control with the widest blast radius on this
// surface: it stops every credential the person holds at once. It wrote the row
// and published nothing, so on every node that did not serve the call the keys
// went on authenticating for the credential cache TTL — sixty seconds, against a
// published bound of 270 ms, and sixty times slower than blocking one of the
// same person's keys through `/key/block`.
func TestUserBlockStopsServingOnASecondNode(t *testing.T) {
	tn := newTwoNodes(t, true)
	h := tn.h

	rec := h.do(http.MethodPost, "/user/new",
		map[string]any{"user_id": "u-leaver", "user_email": "leaver@example.com"})
	h.expectStatus(rec, http.StatusOK)
	_, token := h.newKey(map[string]any{"user_id": "u-leaver"})
	tn.warm(t, token)

	bound := tn.bound()
	called := time.Now()
	rec = h.do(http.MethodPost, "/user/update",
		map[string]any{"user_id": "u-leaver", "blocked": true})
	h.expectStatus(rec, http.StatusOK)
	control := time.Since(called)

	// The node that served the call is immediate, because Announce applies
	// locally before it publishes. A control whose own node has a window is a
	// control the operator cannot verify.
	if err := serving(tn.up, token); err == nil {
		t.Fatal("the node that served POST /user/update went on serving the blocked user's key")
	} else if !errors.Is(err, auth.ErrBlocked) {
		t.Fatalf("the publishing node refused for the wrong reason: %v", err)
	}

	prop, refusal, stopped := stopsServingWithin(tn.down, token, clusterObserveWindow)
	if !stopped {
		t.Fatalf("the second node was still serving a BLOCKED USER's key %v after "+
			"POST /user/update returned; the mutation wrote the row and published nothing", prop)
	}
	if !errors.Is(refusal, auth.ErrBlocked) {
		// "Stopped serving" has to mean blocked. A key refused as unknown would
		// satisfy a weaker assertion and would mean something else went wrong.
		t.Fatalf("the second node refused for the wrong reason: %v", refusal)
	}

	t.Logf("/user/update {blocked:true} PROPAGATION (%d nodes, poll %v): the second node stopped "+
		"serving %v after the call returned, against a published bound of %v (%s); the handler "+
		"itself took %v; TTL fallback %v",
		clusterNodes, clusterPoll, prop, bound.Bound, bound.Formula, control, bound.Fallback)

	if prop > bound.Bound {
		t.Errorf("measured %v, which exceeds the published bound of %v (%s)",
			prop, bound.Bound, bound.Formula)
	}
	if bound.Bound >= bound.Fallback {
		t.Errorf("the published bound %v is not better than the TTL fallback %v",
			bound.Bound, bound.Fallback)
	}
}

// TestUserBlockWithoutAnInvalidatorObservesOnTheCacheTTL is the inverse, and it
// is what makes the test above an assertion about the fix rather than about the
// fake.
//
// With no invalidator wired the surface writes exactly the same row — the block
// is durable, and a node reading the store afresh honours it — and the second
// node keeps serving from its snapshot. That was the shipped behaviour of every
// `/user/*` and `/team/*` mutation on every clustered deployment.
func TestUserBlockWithoutAnInvalidatorObservesOnTheCacheTTL(t *testing.T) {
	tn := newTwoNodes(t, false)
	h := tn.h

	rec := h.do(http.MethodPost, "/user/new",
		map[string]any{"user_id": "u-leaver", "user_email": "leaver@example.com"})
	h.expectStatus(rec, http.StatusOK)
	_, token := h.newKey(map[string]any{"user_id": "u-leaver"})
	tn.warm(t, token)

	rec = h.do(http.MethodPost, "/user/update",
		map[string]any{"user_id": "u-leaver", "blocked": true})
	h.expectStatus(rec, http.StatusOK)

	// The row is durable: a reader with no snapshot sees it.
	u, err := h.store.GetUser(context.Background(), "u-leaver")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Blocked {
		t.Fatal("the block was not written; this test is measuring the wrong thing")
	}

	// And the second node serves the key anyway, for as long as its cache lasts.
	// Twenty poll intervals is not a bound — it is enough to show that no message
	// is coming, which is what the ONE HOUR entry TTL makes unambiguous.
	if _, _, stopped := stopsServingWithin(tn.down, token, 20*clusterPoll); stopped {
		t.Fatal("the second node stopped serving without an invalidator; something other " +
			"than the announcement is closing this window, and the measurement above is " +
			"not measuring the fix")
	}
	t.Logf("without an invalidator the second node still serves the blocked user's key after %v; "+
		"its bound is the entry TTL (%v)", 20*clusterPoll, tn.down.a.EntryTTL())
}

// TestTeamBlockStopsServingOnASecondNode is the same control one level up, and
// it is the one an operator reaches for to stop a whole team at once.
func TestTeamBlockStopsServingOnASecondNode(t *testing.T) {
	tn := newTwoNodes(t, true)
	h := tn.h

	rec := h.do(http.MethodPost, "/team/new",
		map[string]any{"team_id": "t-eng", "team_name": "engineering"})
	h.expectStatus(rec, http.StatusOK)
	_, one := h.newKey(map[string]any{"team_id": "t-eng"})
	_, two := h.newKey(map[string]any{"team_id": "t-eng"})
	tn.warm(t, one)
	tn.warm(t, two)

	bound := tn.bound()
	rec = h.do(http.MethodPost, "/team/update",
		map[string]any{"team_id": "t-eng", "blocked": true})
	h.expectStatus(rec, http.StatusOK)

	// EVERY key on the team, not the first one. A control that announced one of
	// them would look correct in a one-key test and leave the rest serving.
	for i, token := range []string{one, two} {
		prop, refusal, stopped := stopsServingWithin(tn.down, token, clusterObserveWindow)
		if !stopped {
			t.Fatalf("key %d of a BLOCKED TEAM was still serving on the second node %v "+
				"after POST /team/update returned", i+1, prop)
		}
		if !errors.Is(refusal, auth.ErrBlocked) {
			t.Fatalf("key %d was refused for the wrong reason: %v", i+1, refusal)
		}
		t.Logf("/team/update {blocked:true} PROPAGATION for key %d: %v against a published "+
			"bound of %v", i+1, prop, bound.Bound)
		if prop > bound.Bound {
			t.Errorf("key %d measured %v, which exceeds the published bound of %v (%s)",
				i+1, prop, bound.Bound, bound.Formula)
		}
	}
}

// TestUserDeleteStopsServingOnASecondNode covers the mutation whose subject is
// GONE by the time anything could enumerate it.
//
// Deleting the user is the other thing an operator does when a person leaves,
// and it is the case where the enumeration has to happen BEFORE the durable
// write: afterwards there is nothing left to resolve the keys from.
func TestUserDeleteStopsServingOnASecondNode(t *testing.T) {
	tn := newTwoNodes(t, true)
	h := tn.h

	rec := h.do(http.MethodPost, "/user/new",
		map[string]any{"user_id": "u-gone", "user_email": "gone@example.com"})
	h.expectStatus(rec, http.StatusOK)
	_, token := h.newKey(map[string]any{"user_id": "u-gone"})
	tn.warm(t, token)

	if p := principalOn(tn.down, token); p == nil || p.User == nil {
		t.Fatal("the second node was not carrying the user's envelope before the delete; " +
			"this test would then be measuring nothing")
	}

	bound := tn.bound()
	rec = h.do(http.MethodPost, "/user/delete", map[string]any{"user_ids": []string{"u-gone"}})
	h.expectStatus(rec, http.StatusOK)

	// The observable is that the second node stops serving the credential UNDER
	// THE DELETED USER'S ENVELOPE, which covers both things a store may do with
	// the keys: cascade them away, and the credential stops authenticating; or
	// orphan them, and it authenticates with no owner. Asserting only the first
	// would pass or fail on the store's foreign keys rather than on this
	// surface's announcement — and the failure that matters is the same either
	// way, a node still answering from a principal that names a user who no
	// longer exists.
	start := time.Now()
	deadline := start.Add(clusterObserveWindow)
	var prop time.Duration
	for {
		p := principalOn(tn.down, token)
		if p == nil || p.User == nil {
			prop = time.Since(start)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second node was still serving a DELETED user's envelope %v after "+
				"the call returned; the enumeration ran after the delete, or not at all",
				time.Since(start))
		}
		time.Sleep(time.Millisecond)
	}
	t.Logf("/user/delete PROPAGATION: %v against a published bound of %v", prop, bound.Bound)
	if prop > bound.Bound {
		t.Errorf("measured %v, which exceeds the published bound of %v (%s)",
			prop, bound.Bound, bound.Formula)
	}
}

// TestLoweringATeamBudgetStopsServingOnASecondNode is the third class, the one
// two enumerations had each walked past.
//
// A ceiling is not bookkeeping: [auth.Limits.MaxBudgetNanoUSD] is on the same
// cached entry the block flag is on, and the hot path refuses a subject whose
// recorded spend has already reached it. `POST /budget/update` that lowers a
// team's ceiling below what it has spent is therefore a refusal, and it landed
// on the fleet a credential cache TTL later.
func TestLoweringATeamBudgetStopsServingOnASecondNode(t *testing.T) {
	tn := newTwoNodes(t, true)
	h := tn.h

	rec := h.do(http.MethodPost, "/team/new",
		map[string]any{"team_id": "t-spend", "team_name": "spenders"})
	h.expectStatus(rec, http.StatusOK)
	_, token := h.newKey(map[string]any{"team_id": "t-spend"})

	// The team has already spent five dollars, under a generous ceiling.
	h.store.mu.Lock()
	h.store.teams["t-spend"].SpendNano = 5_000_000_000
	h.store.teams["t-spend"].MaxBudgetNano = nanoOf(100)
	h.store.mu.Unlock()
	tn.warm(t, token)

	bound := tn.bound()
	rec = h.do(http.MethodPost, "/budget/new", map[string]any{
		"subject_kind": "team", "subject_id": "t-spend", "max_budget": 1,
	})
	h.expectStatus(rec, http.StatusOK)

	prop, refusal, stopped := stopsServingWithin(tn.down, token, clusterObserveWindow)
	if stopped && !errors.Is(refusal, auth.ErrBudgetExceeded) {
		t.Fatalf("the second node refused for the wrong reason: %v", refusal)
	}
	if !stopped {
		t.Fatalf("the second node was still serving over a LOWERED team ceiling %v after "+
			"POST /budget/new returned", prop)
	}
	t.Logf("/budget/new PROPAGATION: %v against a published bound of %v", prop, bound.Bound)
	if prop > bound.Bound {
		t.Errorf("measured %v, which exceeds the published bound of %v (%s)",
			prop, bound.Bound, bound.Formula)
	}
}

// nanoOf is a ceiling in nano-USD, as a pointer, because nil ("no ceiling") and
// 0 ("a ceiling of zero") are different answers.
func nanoOf(usd int64) *int64 {
	n := usd * 1_000_000_000
	return &n
}
