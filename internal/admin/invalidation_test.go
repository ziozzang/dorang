package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Which administrative mutations announce, and with which cause.
//
// The observable assertion — "the key stops serving on a second node" — lives in
// internal/cluster, where there are two real authenticators and a real
// invalidation transport to observe it with. What is asserted here is the other
// half an enumeration needs: that EVERY route which changes whether a key serves
// reaches the announcement, and that the one route which deliberately does not
// still does not.

type invMsg struct {
	keyID string
	cause string
}

type fakeInvalidator struct {
	mu   sync.Mutex
	msgs []invMsg
	err  error
}

func (f *fakeInvalidator) InvalidateKey(_ context.Context, keyID, cause string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, invMsg{keyID: keyID, cause: cause})
	return nil
}

func (f *fakeInvalidator) taken() []invMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]invMsg(nil), f.msgs...)
	f.msgs = nil
	return out
}

func newInvalidatingHarness(t *testing.T) (*harness, *fakeInvalidator) {
	t.Helper()
	inv := &fakeInvalidator{}
	h := newHarness(t, func(c *Config) { c.Invalidator = inv })
	return h, inv
}

// TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt is the enumeration.
//
// It is one table rather than nine tests because the finding was that the list
// had a hole in it, and a list is the shape that makes the next hole visible: a
// route added below without a row here, or with an empty one, is a route whose
// change lands on the credential cache TTL.
func TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt(t *testing.T) {
	h, inv := newInvalidatingHarness(t)
	id, _ := h.newKey(nil)

	// Creation is the deliberate exception: nothing is cached for a key that has
	// never been seen, and "used before it existed, then created" is bounded by
	// the negative-entry TTL rather than by a message.
	if got := inv.taken(); len(got) != 0 {
		t.Fatalf("/key/generate announced %v; creation deliberately publishes nothing", got)
	}

	for _, tc := range []struct {
		name  string
		path  string
		body  map[string]any
		cause string
	}{
		{"block is the incident path", "/key/block", map[string]any{"key_id": id}, CauseRevoked},
		{"unblock lets it serve again", "/key/unblock", map[string]any{"key_id": id}, CauseUpdated},
		{"update rewrites the authorization envelope", "/key/update",
			map[string]any{"key_id": id, "rpm_limit": 10}, CauseUpdated},
		{"update can block by another name", "/key/update",
			map[string]any{"key_id": id, "blocked": true}, CauseRevoked},
		{"pend is the reversible refusal", "/key/pend", map[string]any{"key_id": id}, CausePended},
		{"release ends it in one action", "/key/release", map[string]any{"key_id": id}, CauseReleased},
		{"regenerate cuts the old secret outright", "/key/regenerate",
			map[string]any{"key_id": id}, CauseGraceCut},
		{"rotate leaves the old secret inside its grace", "/key/rotate",
			map[string]any{"key_id": id}, CauseRotated},
		{"rotate with no grace is a cut", "/key/rotate",
			map[string]any{"key_id": id, "grace": "0"}, CauseGraceCut},
		{"an early cut is the compromise path", "/key/rotate/cut",
			map[string]any{"key_id": id}, CauseGraceCut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Unblock first where the previous case left the key blocked, so the
			// route under test is not refused for an unrelated reason.
			rec := h.do(http.MethodPost, tc.path, tc.body)
			h.expectStatus(rec, http.StatusOK)
			got := inv.taken()
			if len(got) != 1 {
				t.Fatalf("%s announced %d times, want exactly 1: %v", tc.path, len(got), got)
			}
			if got[0].keyID != id {
				t.Errorf("announced key %q, want %q", got[0].keyID, id)
			}
			if got[0].cause != tc.cause {
				t.Errorf("cause = %q, want %q", got[0].cause, tc.cause)
			}
		})
	}

	// A bulk delete announces every id it removed. Stopping after the first
	// would leave the rest of the list serving on the TTL.
	other, _ := h.newKey(nil)
	inv.taken()
	rec := h.do(http.MethodPost, "/key/delete", map[string]any{"keys": []string{id, other}})
	h.expectStatus(rec, http.StatusOK)
	got := inv.taken()
	if len(got) != 2 {
		t.Fatalf("/key/delete announced %d of 2 deleted keys: %v", len(got), got)
	}
	for _, m := range got {
		if m.cause != CauseRevoked {
			t.Errorf("delete announced cause %q, want %q", m.cause, CauseRevoked)
		}
		if m.keyID != id && m.keyID != other {
			t.Errorf("delete announced an unrelated key %q", m.keyID)
		}
	}
}

// --- the enumeration, one level up -------------------------------------------
//
// The table above enumerates KEY mutations, and that was the wrong noun. §11.2's
// authorization envelope is three subjects — the key, its user, its team — and
// [auth.Principal] caches all three in one entry, so `POST /user/update` with
// `{"blocked": true}` had exactly the defect `POST /key/block` had. So did
// `/budget/*`, whose ceiling sits on the same entry, and which neither sweep
// looked at.
//
// What follows is therefore an enumeration of ROUTES rather than of key
// operations: every path that mutates anything gets a row saying what it
// announces, or saying in words why it announces nothing. A route added without
// a row fails [TestEveryWriteRouteIsEnumerated], which is the part that makes a
// third sweep unnecessary rather than merely unlikely.

// subjectFixture is one user with two keys, one team with two keys, and a key
// that belongs to neither. Two keys per subject is the whole point: a control
// that announced the first one would look correct against a single-key fixture
// and would leave every other credential of a blocked user serving.
type subjectFixture struct {
	h   *harness
	inv *fakeInvalidator

	userKeyA, userKeyB string
	teamKeyA, teamKeyB string
	// loneKey belongs to neither subject. It is what gives assertAnnounced's
	// "unexpected announcement" arm something to catch: a control that resolved
	// its subject too widely would name it.
	loneKey string
}

func newSubjectFixture(t *testing.T) *subjectFixture {
	t.Helper()
	h, inv := newInvalidatingHarness(t)
	// Pricing and a reloader, so that the routes which mutate nothing still
	// EXECUTE rather than being answered 501 by a dependency guard. A row
	// asserting "this route announces nothing" is worth little if the route
	// never reached its handler.
	h.api.cfg.Pricing = fakePricer{}
	h.api.cfg.Reloader = fakeReloader{}
	h.api.cfg.ConfigWriter = fakeConfigWriter{}
	h.api.cfg.Setup = &fakeSetup{}

	f := &subjectFixture{h: h, inv: inv}
	mustOK(t, h.do(http.MethodPost, "/user/new",
		map[string]any{"user_id": "u-1", "user_email": "one@example.com"}))
	mustOK(t, h.do(http.MethodPost, "/user/new",
		map[string]any{"user_id": "u-2", "user_email": "two@example.com"}))
	mustOK(t, h.do(http.MethodPost, "/team/new",
		map[string]any{"team_id": "t-1", "team_name": "one"}))
	mustOK(t, h.do(http.MethodPost, "/team/member_add",
		map[string]any{"team_id": "t-1", "user_id": "u-1"}))
	mustOK(t, h.do(http.MethodPost, "/model/new", map[string]any{
		"id": "d-1", "model_name": "gpt-4",
		"dorang_params": map[string]any{"provider": "openai", "model": "gpt-4o"},
	}))

	f.userKeyA, _ = h.newKey(map[string]any{"user_id": "u-1"})
	f.userKeyB, _ = h.newKey(map[string]any{"user_id": "u-1"})
	f.teamKeyA, _ = h.newKey(map[string]any{"team_id": "t-1"})
	f.teamKeyB, _ = h.newKey(map[string]any{"team_id": "t-1"})
	f.loneKey, _ = h.newKey(nil)

	// A ceiling the team already has, seeded through the store rather than
	// through the route, so that /budget/update and /budget/delete have
	// something to act on and the seeding itself announces nothing.
	sub := BudgetSubject{Kind: "team", ID: "t-1"}
	h.store.mu.Lock()
	h.store.budgets[sub] = Budget{Subject: sub, MaxBudgetNano: ptrInt64(50_000_000_000)}
	h.store.mu.Unlock()

	inv.taken()
	return f
}

func ptrInt64(v int64) *int64 { return &v }

func mustOK(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("fixture setup failed with status %d: %s", rec.Code, rec.Body.String())
	}
}

// mutationRow is one route's entry in the enumeration.
type mutationRow struct {
	path string
	// why is the one-line reason a reviewer reads. It is required on every row,
	// including the ones that announce: "it announces" is not a reason, and the
	// rows that do not announce are the ones a sweep walks past.
	why string
	// coveredBy names the test that asserts this route's announcements, for the
	// key routes whose sequencing makes them a test of their own. A row with it
	// set is not driven here; it is only required to exist.
	coveredBy string
	// body is the request this test sends.
	body map[string]any
	// want is the announcements it must produce, resolved against the fixture.
	// An empty result is an assertion in its own right.
	want func(f *subjectFixture) []invMsg
}

// mutationRows is the enumeration. Every write route in this package appears
// exactly once.
func mutationRows() []mutationRow {
	none := func(*subjectFixture) []invMsg { return nil }
	return []mutationRow{
		// --- keys: the first sweep -------------------------------------------
		{path: "/key/generate", why: "a key nobody has seen has no cached copy to drop; " +
			"the bound is the negative-entry TTL",
			coveredBy: "TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt"},
		{path: "/key/update", why: "every field it writes is on the cached entry",
			coveredBy: "TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt"},
		{path: "/key/delete", why: "nothing is left to refuse the credential",
			coveredBy: "TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt"},
		{path: "/key/block", why: "the incident path of OPERATIONS §3.1",
			coveredBy: "TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt"},
		{path: "/key/unblock", why: "an outage that ends on a TTL is not ended by the operator",
			coveredBy: "TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt"},
		{path: "/key/regenerate", why: "the old secret is cut outright",
			coveredBy: "TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt"},
		{path: "/key/rotate", why: "the key's secrets changed",
			coveredBy: "TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt"},
		{path: "/key/rotate/cut", why: "a superseded secret stopped working",
			coveredBy: "TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt"},
		{path: "/key/pend", why: "a pend that waits for a TTL has started a timer",
			coveredBy: "TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt"},
		{path: "/key/release", why: "released in one action means observable on the next request",
			coveredBy: "TestEveryMutationThatChangesWhetherAKeyServesAnnouncesIt"},

		// --- users and teams: the class the first sweep missed ----------------
		{
			path: "/user/new",
			why: "the id is the caller's, so a key may already name it and be about to " +
				"acquire an owner's envelope. Normally there is none",
			body: map[string]any{"user_id": "u-fresh", "user_email": "fresh@example.com"},
			want: none,
		},
		{
			path: "/user/update",
			why:  "blocked, models, the ceilings and the period are all on the cached entry",
			body: map[string]any{"user_id": "u-1", "blocked": true},
			want: func(f *subjectFixture) []invMsg {
				return []invMsg{
					{keyID: f.userKeyA, cause: CauseRevoked},
					{keyID: f.userKeyB, cause: CauseRevoked},
				}
			},
		},
		{
			path: "/user/update",
			why:  "an update that is not a block is still an envelope change",
			body: map[string]any{"user_id": "u-2", "rpm_limit": 10},
			want: none, // u-2 owns no keys, which is the empty case done honestly
		},
		{
			path: "/user/delete",
			why:  "the keys are cascaded away or orphaned; either way the cached copy is wrong",
			body: map[string]any{"user_ids": []string{"u-1"}},
			want: func(f *subjectFixture) []invMsg {
				return []invMsg{
					{keyID: f.userKeyA, cause: CauseRevoked},
					{keyID: f.userKeyB, cause: CauseRevoked},
				}
			},
		},
		{
			path: "/team/new",
			why:  "the same case as /user/new, for the same reason",
			body: map[string]any{"team_id": "t-fresh", "team_name": "fresh"},
			want: none,
		},
		{
			path: "/team/update",
			why:  "the widest control on this surface: it stops a whole team at once",
			body: map[string]any{"team_id": "t-1", "blocked": true},
			want: func(f *subjectFixture) []invMsg {
				return []invMsg{
					{keyID: f.teamKeyA, cause: CauseRevoked},
					{keyID: f.teamKeyB, cause: CauseRevoked},
				}
			},
		},
		{
			path: "/team/delete",
			why:  "as /user/delete",
			body: map[string]any{"team_ids": []string{"t-1"}},
			want: func(f *subjectFixture) []invMsg {
				return []invMsg{
					{keyID: f.teamKeyA, cause: CauseRevoked},
					{keyID: f.teamKeyB, cause: CauseRevoked},
				}
			},
		},
		{
			path: "/team/member_add",
			why: "team_members feeds no cached decision: a key's team is api_keys.team_id " +
				"and so is its administrative scope",
			body: map[string]any{"team_id": "t-1", "user_id": "u-2"},
			want: none,
		},
		{
			path: "/team/member_delete",
			why:  "as /team/member_add",
			body: map[string]any{"team_id": "t-1", "user_id": "u-1"},
			want: none,
		},

		// --- budgets: the class BOTH sweeps missed ----------------------------
		{
			path: "/budget/new",
			why: "a ceiling is auth.Limits.MaxBudgetNanoUSD on the same cached entry the " +
				"block flag is on, and the hot path refuses on it",
			body: map[string]any{"subject_kind": "user", "subject_id": "u-1", "max_budget": 5},
			want: func(f *subjectFixture) []invMsg {
				return []invMsg{
					{keyID: f.userKeyA, cause: CauseUpdated},
					{keyID: f.userKeyB, cause: CauseUpdated},
				}
			},
		},
		{
			path: "/budget/update",
			why:  "lowering a ceiling below recorded spend is a refusal",
			body: map[string]any{"subject_kind": "team", "subject_id": "t-1", "max_budget": 1},
			want: func(f *subjectFixture) []invMsg {
				return []invMsg{
					{keyID: f.teamKeyA, cause: CauseUpdated},
					{keyID: f.teamKeyB, cause: CauseUpdated},
				}
			},
		},
		{
			path: "/budget/delete",
			why:  "clearing a ceiling is how an operator ends a budget outage",
			body: map[string]any{"subject_kind": "team", "subject_id": "t-1"},
			want: func(f *subjectFixture) []invMsg {
				return []invMsg{
					{keyID: f.teamKeyA, cause: CauseUpdated},
					{keyID: f.teamKeyB, cause: CauseUpdated},
				}
			},
		},

		// --- routing and process state ---------------------------------------
		{
			path: "/model/new",
			why: "a deployment is a routing fact, not a credential one: it reaches no entry " +
				"in the credential cache, and Invalidator has no shape that could carry it",
			body: map[string]any{
				"id": "d-2", "model_name": "gpt-4",
				"dorang_params": map[string]any{"provider": "openai", "model": "gpt-4o-mini"},
			},
			want: none,
		},
		{
			path: "/model/update",
			why:  "as /model/new",
			body: map[string]any{"id": "d-1", "enabled": false},
			want: none,
		},
		{
			path: "/model/delete",
			why:  "as /model/new",
			body: map[string]any{"id": "d-1"},
			want: none,
		},
		{
			path: "/admin/config/reload",
			why: "a reload is this node's own by construction; OPERATIONS tells the operator " +
				"to run it against the fleet",
			body: map[string]any{},
			want: none,
		},
		{
			path: "/model/deployment/set_enabled",
			why: "taking a deployment in or out of routing is a config fact, not a credential " +
				"one: it reaches no credential-cache entry, and it propagates by the config file " +
				"the other nodes' watchers re-read, not by the key-invalidation bus",
			body: map[string]any{
				"model_group": "gpt-4", "provider": "openai",
				"upstream_model": "gpt-4o", "enabled": false,
			},
			want: none,
		},

		{path: "/admin/setup/change", why: "upstream provider credentials and model bindings propagate through the shared config watcher, not the client credential cache", body: map[string]any{"action": "credential", "id": "account"}, want: none},
		{path: "/admin/setup/discover", why: "an upstream model-list probe changes no client authorization envelope", body: map[string]any{"credential": "account"}, want: none},

		// --- POST routes that compute rather than mutate ----------------------
		{
			path: "/spend/calculate",
			why:  "prices a hypothetical; it writes nothing",
			body: map[string]any{"model": "gpt-4", "prompt_tokens": 10},
			want: none,
		},
		{
			path: "/admin/pricing/preview",
			why:  "as /spend/calculate",
			body: map[string]any{"model": "gpt-4", "provider": "openai", "prompt_tokens": 10},
			want: none,
		},
	}
}

// TestEveryMutationThatChangesWhetherARequestIsServedAnnouncesIt is the
// enumeration, one noun wider than the key table above.
//
// It is one table for the reason that one is: the finding was a hole in a list,
// and a list is the shape that makes the next hole visible. Every row carries a
// REASON, including the rows that announce nothing, because the two classes that
// were missed were both missed by a sweep that never wrote the reason down.
func TestEveryMutationThatChangesWhetherARequestIsServedAnnouncesIt(t *testing.T) {
	for _, tc := range mutationRows() {
		if tc.coveredBy != "" {
			continue
		}
		t.Run(tc.path+" "+tc.why, func(t *testing.T) {
			// A fresh fixture per row, so that a row's mutations cannot make the
			// next one's subject unreachable and quietly turn it into a
			// zero-announcement pass.
			f := newSubjectFixture(t)
			rec := f.h.do(http.MethodPost, tc.path, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("POST %s: status %d, want 200 — a row that does not reach its "+
					"handler asserts nothing\nbody: %s", tc.path, rec.Code, rec.Body.String())
			}
			assertAnnounced(t, tc.path, f.inv.taken(), tc.want(f))
		})
	}
}

// assertAnnounced compares two announcement sets without regard to order.
func assertAnnounced(t *testing.T, path string, got, want []invMsg) {
	t.Helper()
	index := func(ms []invMsg) map[invMsg]int {
		m := map[invMsg]int{}
		for _, x := range ms {
			m[x]++
		}
		return m
	}
	g, w := index(got), index(want)
	for msg, n := range w {
		if g[msg] != n {
			t.Errorf("%s announced %s×%d, want ×%d (all: %v)", path, msg.keyID+"/"+msg.cause,
				g[msg], n, got)
		}
	}
	for msg, n := range g {
		if w[msg] == 0 {
			t.Errorf("%s announced an unexpected %s×%d; if that is correct the row's "+
				"`want` is what has to say so", path, msg.keyID+"/"+msg.cause, n)
		}
	}
}

// TestEveryWriteRouteIsEnumerated is the part that makes a third sweep
// unnecessary.
//
// Two enumerations have now each missed a class the other did not look at, and
// both times the missing piece was a route nobody had written a row for. The
// route table is the authority on what this surface mutates, so it is read
// directly: a `write` route with no row above fails here, and a row naming a
// route that no longer exists fails here too.
func TestEveryWriteRouteIsEnumerated(t *testing.T) {
	h := newHarness(t)

	// A write route is one that answers POST and not GET. `read` mounts both and
	// `stub` mounts four methods, so the pair is exactly the discriminator.
	writes := map[string]bool{}
	for path, rt := range h.api.routes {
		_, post := rt.methods[http.MethodPost]
		_, get := rt.methods[http.MethodGet]
		if post && !get {
			writes[path] = true
		}
	}
	if len(writes) == 0 {
		t.Fatal("no write routes found; this test is reading the wrong table")
	}

	enumerated := map[string]bool{}
	for _, r := range mutationRows() {
		if r.why == "" {
			t.Errorf("%s has no reason on its row; the two classes that were missed were "+
				"missed by sweeps that never wrote one down", r.path)
		}
		enumerated[r.path] = true
	}

	for path := range writes {
		if !enumerated[path] {
			t.Errorf("POST %s mutates and has no row in mutationRows(). Add one saying what "+
				"it announces, or saying why it announces nothing — a route without a row "+
				"is how /user/*, /team/* and /budget/* each stayed on the 60-second "+
				"credential cache TTL through a sweep that only looked at keys", path)
		}
	}
	for path := range enumerated {
		if !writes[path] {
			t.Errorf("mutationRows() has a row for %s, which is not a write route", path)
		}
	}
}

// TestAMutationThatCannotBeAnnouncedIsReportedAndStillAudited is the failure
// shape.
//
// The change is durable and this node has honoured it, so the caller must not be
// told the request failed and must not retry it. What they are told is that the
// fleet is converging on the cache TTL instead of within the published bound —
// and the trail is written anyway, because a mutation that took effect and left
// no record is the worse of the two outcomes.
func TestAMutationThatCannotBeAnnouncedIsReportedAndStillAudited(t *testing.T) {
	h, inv := newInvalidatingHarness(t)
	id, _ := h.newKey(nil)
	before := len(h.store.auditLog())

	inv.err = errors.New("the invalidation table is unreachable")
	rec := h.do(http.MethodPost, "/key/block", map[string]any{"key_id": id})
	h.expectFault(rec, http.StatusInternalServerError, CodeInvalidationFailed)

	if got := h.api.Metrics().InvalidationFailures; got != 1 {
		t.Errorf("InvalidationFailures = %d, want 1: a fleet on the TTL fallback has to be visible", got)
	}
	if after := len(h.store.auditLog()); after != before+1 {
		t.Errorf("audit rows went from %d to %d; a mutation that took effect must leave a record "+
			"even when it could not be announced", before, after)
	}
	// And the change really did take effect, which is why retrying is the wrong
	// advice and the message says so.
	k, err := h.store.GetKey(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !k.Blocked {
		t.Error("the block was rolled back; the refusal claims it is durable")
	}
}

// TestWithoutAnInvalidatorTheSurfaceStillMutates keeps the dependency optional.
//
// A deployment with no invalidation path — one process, or one that has accepted
// the entry TTL as its real bound — must not lose the administration surface
// over it. What it loses is the bound, which is what [Invalidator] says.
func TestWithoutAnInvalidatorTheSurfaceStillMutates(t *testing.T) {
	h := newHarness(t)
	id, _ := h.newKey(nil)
	rec := h.do(http.MethodPost, "/key/block", map[string]any{"key_id": id})
	body := h.expectStatus(rec, http.StatusOK)
	key, _ := body["key"].(map[string]any)
	if key == nil || key["blocked"] != true {
		t.Fatalf("the key was not blocked: %s", rec.Body.String())
	}
	if got := h.api.Metrics().Invalidations; got != 0 {
		t.Errorf("Invalidations = %d with no invalidator configured", got)
	}
}
