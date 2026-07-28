package admin

import (
	"net/http"
	"testing"
	"time"
)

// The three defects the security review said would go live the moment this
// package was mounted. Each test below fails against the code as it stood.

// ---------------------------------------------------------------------------
// 1. read-before-auth
// ---------------------------------------------------------------------------

// Nothing about the route table is answered before the caller is authenticated.
//
// Route dispatch, OPTIONS handling and the 405/501 answers all used to precede
// authenticate(), so an unauthenticated caller could map the surface: which
// administrative routes this build serves, which methods each accepts, and —
// from the difference between the generic 501 and a stub's specific code —
// which concepts exist. The refusal is now the same 401 for every path.
func TestTheRouteTableIsNotReadableBeforeAuthentication(t *testing.T) {
	h := newHarness(t)

	// A registered path, an unregistered one, a stubbed one, and a registered
	// path with a method it does not answer. Before the fix these produced a
	// 401 only for the first: 501/501-with-a-specific-code/405-with-an-Allow.
	cases := []struct{ method, path string }{
		{http.MethodPost, "/key/list"},
		{http.MethodPost, "/nope"},
		{http.MethodPost, "/organization/new"},
		{http.MethodPut, "/key/list"},
		{http.MethodOptions, "/key/generate"},
	}
	for _, c := range cases {
		rec := h.do(c.method, c.path, nil, asToken(""))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d to an unauthenticated caller: %s",
				c.method, c.path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Allow"); got != "" {
			t.Errorf("%s %s leaked an Allow header to an unauthenticated caller: %q",
				c.method, c.path, got)
		}
	}

	// The same probes, authenticated, still answer what they always did: the
	// fix is an ordering change and not a removal of the §0.2 contract.
	h.expectFault(h.do(http.MethodPost, "/nope", nil), http.StatusNotImplemented, CodeNotImplemented)
	h.expectFault(h.do(http.MethodPost, "/organization/new", nil),
		http.StatusNotImplemented, "organizations_unsupported")
	if rec := h.do(http.MethodPut, "/key/list", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("an authenticated caller should still get 405: %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// 2. no team-scoped admin
// ---------------------------------------------------------------------------

// An administrative key on one team cannot act on another.
//
// Authorization used to be one global bit — the master credential or any user
// with an administrative role — so `admin_viewer` on team A administered team
// B, and every key in the deployment, and every budget.
func TestATeamAdminCannotReachAnotherTeam(t *testing.T) {
	h := newHarness(t)

	// Two keys, one per team, both created by the operator.
	idA, _ := h.newKey(map[string]any{"team_id": "team-a", "key_alias": "a"})
	idB, _ := h.newKey(map[string]any{"team_id": "team-b", "key_alias": "b"})

	asTeamA := asToken(teamAToken)

	// Its own key: visible and mutable.
	h.expectStatus(h.do(http.MethodPost, "/key/info", map[string]any{"key_id": idA}, asTeamA),
		http.StatusOK)
	h.expectStatus(h.do(http.MethodPost, "/key/block", map[string]any{"key_id": idA}, asTeamA),
		http.StatusOK)

	// The other team's key: indistinguishable from a key that does not exist.
	// Not 403 — a 403 here would confirm the id, which turns every key id into
	// an existence oracle for anyone holding any administrative key.
	for _, path := range []string{"/key/info", "/key/block", "/key/unblock", "/key/regenerate"} {
		rec := h.do(http.MethodPost, path, map[string]any{"key_id": idB}, asTeamA)
		h.expectFault(rec, http.StatusNotFound, CodeNotFound)
	}
	rec := h.do(http.MethodPost, "/key/update",
		map[string]any{"key_id": idB, "key_alias": "stolen"}, asTeamA)
	h.expectFault(rec, http.StatusNotFound, CodeNotFound)

	// And the other team's key really was not touched.
	body := h.expectStatus(h.do(http.MethodPost, "/key/info", map[string]any{"key_id": idB}), http.StatusOK)
	key := body["key"].(map[string]any)
	if key["key_alias"] != "b" {
		t.Errorf("team-a's administrator changed team-b's key: %v", key["key_alias"])
	}
	if key["blocked"] == true {
		t.Error("team-a's administrator blocked team-b's key")
	}
}

// A team administrator cannot escalate by writing through the scope check.
//
// Two ways to try it, both refused: mint a key onto another team, or move an
// existing key of its own onto another team (or off every team, which is the
// global scope).
func TestATeamAdminCannotMintOrMoveKeysAcrossTeams(t *testing.T) {
	h := newHarness(t)
	asTeamA := asToken(teamAToken)

	rec := h.do(http.MethodPost, "/key/generate", map[string]any{"team_id": "team-b"}, asTeamA)
	h.expectFault(rec, http.StatusForbidden, CodeOutOfScope)

	// A key with no team at all is a deployment-wide key, and under this model
	// an administrative one would be a GLOBAL administrator. Minting one from a
	// team scope is the escalation.
	rec = h.do(http.MethodPost, "/key/generate", map[string]any{"key_alias": "no-team"}, asTeamA)
	h.expectFault(rec, http.StatusForbidden, CodeOutOfScope)

	id, _ := h.newKey(map[string]any{"team_id": "team-a"})
	rec = h.do(http.MethodPost, "/key/update",
		map[string]any{"key_id": id, "team_id": "team-b"}, asTeamA)
	h.expectFault(rec, http.StatusForbidden, CodeOutOfScope)
}

// Deployment-wide administration is refused rather than filtered.
//
// There is no team-scoped view of "reload the process's configuration" or "the
// deployment table", so a scoped administrator is told so instead of being
// served a filtered fiction.
func TestDeploymentWideEndpointsRefuseAScopedAdmin(t *testing.T) {
	h := newHarness(t)
	asTeamA := asToken(teamAToken)
	for _, path := range []string{
		"/model/info", "/model_group/info", "/admin/status", "/admin/capacity",
		"/admin/config/reload", "/health/history", "/global/spend/report",
		"/user/new", "/team/new",
	} {
		rec := h.do(http.MethodPost, path, map[string]any{}, asTeamA)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s answered %d to a team-scoped administrator, want 403: %s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// 3. request-supplied id filters
// ---------------------------------------------------------------------------

// A listing filter cannot widen the caller's scope.
//
// keyList read `team_id` off the query string and handed it to the store, so
// naming a team was the same as being entitled to it. The filter now narrows
// within the scope and never outside it.
func TestListingFiltersCannotWidenTheScope(t *testing.T) {
	h := newHarness(t)
	h.newKey(map[string]any{"team_id": "team-a", "key_alias": "a1"})
	h.newKey(map[string]any{"team_id": "team-a", "key_alias": "a2"})
	h.newKey(map[string]any{"team_id": "team-b", "key_alias": "b1"})
	h.newKey(map[string]any{"key_alias": "no-team"})

	asTeamA := asToken(teamAToken)

	// No filter: its own team only, not the whole deployment.
	body := h.expectStatus(h.do(http.MethodGet, "/key/list", nil, asTeamA), http.StatusOK)
	keys := body["keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("an unfiltered listing returned %d keys, want the 2 on team-a: %s",
			len(keys), body)
	}
	for _, k := range keys {
		if got := k.(map[string]any)["team_id"]; got != "team-a" {
			t.Errorf("listing returned a key on %v", got)
		}
	}

	// Naming another team is refused, not silently rewritten: an empty result
	// would be indistinguishable from a team with no keys.
	rec := h.do(http.MethodGet, "/key/list?team_id=team-b", nil, asTeamA)
	h.expectFault(rec, http.StatusForbidden, CodeOutOfScope)

	// The operator still sees everything.
	body = h.expectStatus(h.do(http.MethodGet, "/key/list", nil), http.StatusOK)
	if n := len(body["keys"].([]any)); n != 4 {
		t.Errorf("the operator's listing returned %d keys, want 4", n)
	}
}

// A ledger filter cannot reach another tenant's request log.
//
// /spend/logs took key_id, team_id, user_id, trace_id and tag straight off the
// request. A trace id in particular travels in support tickets, so "I have an
// id" was the whole of the authorization.
func TestSpendLogsCannotReachAnotherTeam(t *testing.T) {
	h := newHarness(t)
	start := fixedNow().Add(-time.Hour)
	h.store.addLog(LogRow{ID: "r1", TS: fixedNow().Add(-time.Minute),
		APIKeyID: "k-a", TeamID: "team-a", TraceID: "trace-a"})
	h.store.addLog(LogRow{ID: "r2", TS: fixedNow().Add(-time.Minute),
		APIKeyID: "k-b", TeamID: "team-b", TraceID: "trace-b"})

	rng := map[string]any{
		"start_date": start.Format(time.RFC3339),
		"end_date":   fixedNow().Add(time.Minute).Format(time.RFC3339),
	}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range rng {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	asTeamA := asToken(teamAToken)

	// Its own rows.
	body := h.expectStatus(h.do(http.MethodPost, "/spend/logs", with(nil), asTeamA), http.StatusOK)
	logs := body["logs"].([]any)
	if len(logs) != 1 || logs[0].(map[string]any)["request_id"] != "r1" {
		t.Fatalf("an unfiltered query returned %v, want only team-a's row", logs)
	}

	// Naming the other team is refused.
	rec := h.do(http.MethodPost, "/spend/logs", with(map[string]any{"team_id": "team-b"}), asTeamA)
	h.expectFault(rec, http.StatusForbidden, CodeOutOfScope)

	// Naming the other team's KEY or TRACE reaches the store, and the row is
	// dropped on the way out. This is the case a store-side filter alone would
	// have missed: the query is legitimate, the rows are not the caller's.
	for _, extra := range []map[string]any{
		{"key_id": "k-b"},
		{"trace_id": "trace-b"},
	} {
		body := h.expectStatus(h.do(http.MethodPost, "/spend/logs", with(extra), asTeamA), http.StatusOK)
		if n := len(body["logs"].([]any)); n != 0 {
			t.Errorf("%v returned %d of another team's ledger rows", extra, n)
		}
	}

	// The operator sees both.
	body = h.expectStatus(h.do(http.MethodPost, "/spend/logs", with(nil)), http.StatusOK)
	if n := len(body["logs"].([]any)); n != 2 {
		t.Errorf("the operator's query returned %d rows, want 2", n)
	}
}

// A budget subject is checked against the scope, whichever form names it.
func TestBudgetSubjectsAreScoped(t *testing.T) {
	h := newHarness(t)
	idA, _ := h.newKey(map[string]any{"team_id": "team-a"})
	idB, _ := h.newKey(map[string]any{"team_id": "team-b"})
	asTeamA := asToken(teamAToken)

	ok := h.do(http.MethodPost, "/budget/new", map[string]any{
		"subject_kind": "team", "subject_id": "team-a", "max_budget": 10,
	}, asTeamA)
	h.expectStatus(ok, http.StatusOK)

	for _, spec := range []map[string]any{
		{"subject_kind": "team", "subject_id": "team-b", "max_budget": 10},
		{"subject_kind": "key", "subject_id": idB, "max_budget": 10},
		{"subject_kind": "global", "max_budget": 10},
		{"subject_kind": "credential", "subject_id": "openai-1", "max_budget": 10},
		// The compact form has to be checked too: it is a second spelling of
		// the same parameter, and a check on one spelling is not a check.
		{"budget_id": "team:team-b", "max_budget": 10},
	} {
		rec := h.do(http.MethodPost, "/budget/new", spec, asTeamA)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%v answered %d, want 403: %s", spec, rec.Code, rec.Body.String())
		}
	}

	// Its own key's budget is fine.
	h.expectStatus(h.do(http.MethodPost, "/budget/new", map[string]any{
		"subject_kind": "key", "subject_id": idA, "max_budget": 10,
	}, asTeamA), http.StatusOK)
}

// The zero Scope admits nothing.
//
// This is the property the whole design rests on: an adapter that forgets to
// answer, or a Principal built from a path nobody thought about, produces a
// surface that refuses rather than one that permits.
func TestTheZeroScopeAdmitsNothing(t *testing.T) {
	var s Scope
	if s.AllowsTeam("") || s.AllowsTeam("anything") {
		t.Fatal("the zero Scope admits a team")
	}
	if !s.Empty() {
		t.Error("the zero Scope should report itself empty")
	}
	// An empty team id is not covered by a team scope either. Keys, users and
	// budgets with no team are the deployment-wide ones, and reading "" as
	// "everyone's" is the batch.ownedBy defect in a different package.
	if TeamScope("team-a").AllowsTeam("") {
		t.Error("a team scope admits an unowned subject")
	}
	if !GlobalScope().AllowsTeam("") || !GlobalScope().AllowsTeam("team-z") {
		t.Error("the global scope should admit everything")
	}
}
