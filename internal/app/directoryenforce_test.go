package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// User, team and budget administration, asserted as ENFORCEMENT.
//
// Three layers disagreed before this file existed and each of them passed its
// own tests:
//
//  1. `internal/store` had no Go code for `users` or `teams`, so nothing could
//     read them.
//  2. `internal/app` wired no `Directory` and no `Budgets`, so `/user/*`,
//     `/team/*` and `/budget/*` answered 501 in the shipped binary while
//     `internal/admin`'s own tests drove complete handlers against a fake.
//  3. [cluster.AuthPrincipal] built an [auth.Principal] with `Key:` limits only,
//     so `p.User` and `p.Team` were nil on every request the gateway has ever
//     served — and every guard that reads them, in Authorize, in the
//     authenticator's kill switches, in budgetSubjectsOf and in
//     MostRestrictiveParallel, is nil-guarded. A user block was not slow. It did
//     nothing.
//
// Layer 3 is why none of these tests asserts a row. A test that read
// `users.blocked` back would have passed against the defect for the entire life
// of the schema. Every assertion here is that a REQUEST is refused or served.

// directoryYAML is a gateway that can serve a real request and price it.
//
// entry_ttl is an hour and poll is 20 ms deliberately. If a block were taking
// effect through the credential cache expiring, these tests would have to wait
// an hour to pass — which is how they tell the published invalidation apart from
// the TTL fallback it exists to replace.
const directoryYAML = `
version: 1
server: {listen: "127.0.0.1:0", env: development}
storage:
  driver: sqlite
  sqlite: {path: %q}
metering:
  flush_interval: 20ms
  spool: {dir: %q}
auth:
  revocation:
    entry_ttl: 1h
    negative_ttl: 100ms
    poll: 20ms
    store_latency: 250ms
providers:
  - {name: fake, kind: openai, base_url: "%s/v1"}
credentials:
  - {id: fake-1, provider: fake, key_env: DORANG_APP_TEST_KEY}
models:
  - name: model-x
    deployments:
      - {provider: fake, upstream_model: upstream-x, credentials: [fake-1]}
pricing:
  currency: USD
  rules:
    - id: fake-tokens
      class: marginal_usage
      match: {provider: fake}
      rates: {input: "3.00", output: "15.00"}
`

// directoryCostPerRequest is what one fixture request costs: 11 input tokens at
// $3.00/1M plus 3 output at $15.00/1M, fixed by the fake upstream's usage block.
const directoryCostPerRequest = 11*3_000 + 3*15_000

const directoryUpstreamAnswer = `{"id":"chatcmpl-fake","object":"chat.completion",` +
	`"created":1700000000,"model":"upstream-x","choices":[{"index":0,` +
	`"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`

const directoryChatBody = `{"model":"model-x","max_tokens":4,` +
	`"messages":[{"role":"user","content":"ping"}]}`

// directoryFleet is one or more gateways over one database: separate pools,
// separate credential caches, one shared truth. That is what a cluster is, and
// it is the only arrangement in which "propagation" means anything.
type directoryFleet struct {
	apps   []*App
	fronts []*httptest.Server
	dsn    string
}

func newDirectoryFleet(t *testing.T, nodes int, tune func(*Options)) *directoryFleet {
	t.Helper()
	isolateState(t)
	t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, directoryUpstreamAnswer)
	}))
	t.Cleanup(up.Close)

	f := &directoryFleet{dsn: filepath.Join(t.TempDir(), "directory.db")}
	for i := 0; i < nodes; i++ {
		// A spool per app. Two live meters appending to one segment sequence
		// behind one read cursor is the shared-spool defect in its concurrent
		// form, and it costs this suite a node that cannot make progress rather
		// than an assertion that fails.
		spool := filepath.Join(t.TempDir(), fmt.Sprintf("spool-%d", i))
		cfg, err := config.LoadBytes([]byte(fmt.Sprintf(directoryYAML, f.dsn, spool, up.URL)))
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		t.Setenv(cfg.Server.KeyPepperEnv, testPepper)
		t.Setenv(cfg.Server.MasterKeyEnv, testMasterKey)
		opts := Options{Config: cfg, Logf: t.Logf}
		if tune != nil {
			tune(&opts)
		}
		a, err := New(context.Background(), opts)
		if err != nil {
			t.Fatalf("app.New(node %d): %v", i, err)
		}
		t.Cleanup(func() { _ = a.Close(context.Background()) })
		if a.Admin == nil {
			t.Fatal("no administration surface was built: internal/admin is unmounted again")
		}
		front := httptest.NewServer(a.Server)
		t.Cleanup(front.Close)
		f.apps = append(f.apps, a)
		f.fronts = append(f.fronts, front)
	}
	return f
}

type directoryResponse struct {
	status int
	body   string
}

// serves sends one inference request as token to node i. A 200 is the only
// thing that counts as "the key is serving": anything else is a refusal, and
// the status distinguishes which.
func (f *directoryFleet) serves(t *testing.T, i int, token string) directoryResponse {
	t.Helper()
	return f.post(t, i, f.fronts[i].URL+"/v1/chat/completions", token, directoryChatBody)
}

// admin sends one administrative call to node i with the master credential.
func (f *directoryFleet) admin(t *testing.T, i int, path, body string) directoryResponse {
	t.Helper()
	return f.post(t, i, f.fronts[i].URL+path, testMasterKey, body)
}

func (f *directoryFleet) post(t *testing.T, i int, url, token, body string) directoryResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return directoryResponse{status: resp.StatusCode, body: string(raw)}
}

// mustAdmin fails the test unless the administrative call answered 200. The
// body is included because a 501 here is the finding, not a flake.
func (f *directoryFleet) mustAdmin(t *testing.T, i int, path, body string) directoryResponse {
	t.Helper()
	r := f.admin(t, i, path, body)
	if r.status != http.StatusOK {
		t.Fatalf("POST %s: %d %s", path, r.status, r.body)
	}
	return r
}

// plantKey inserts a key owned by userID and teamID and returns its token.
func plantKey(t *testing.T, st *store.Store, token, userID, teamID string) *store.APIKey {
	t.Helper()
	k := &store.APIKey{UserID: userID, TeamID: teamID}
	if err := st.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAPIKey(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	return k
}

// ---------------------------------------------------------------------------
// A blocked user stops serving, everywhere
// ---------------------------------------------------------------------------

// Blocking a USER stops its keys serving — on the node that took the call, and
// on a node that did not, inside the published bound.
//
// This is the assertion the whole change is for, and it is deliberately not
// "the row says blocked". Before this work `POST /user/update` answered 501; had
// only the route been wired it would have answered 200, written `blocked = 1`,
// published an invalidation to the fleet, and the user's keys would have gone on
// serving forever on every node — because nothing populated
// [auth.Principal.User] for the guards to read. A row assertion passes against
// that. A request does not.
func TestBlockingAUserStopsItsKeysOnEveryNode(t *testing.T) {
	f := newDirectoryFleet(t, 2, nil)
	ctx := context.Background()

	const token = "sk-blocked-user-key" // pragma: allowlist secret — test fixture
	u := f.mustAdmin(t, 0, "/user/new", `{"user_id":"u-block","user_email":"u@example.test"}`)
	if !strings.Contains(u.body, "u-block") {
		t.Fatalf("/user/new did not report the user it created: %s", u.body)
	}
	plantKey(t, f.apps[0].Store, token, "u-block", "")

	// Both nodes are serving it, each from its own snapshot, learned
	// independently. Without this the refusal below would prove nothing.
	for i := range f.apps {
		if r := f.serves(t, i, token); r.status != http.StatusOK {
			t.Fatalf("node %d did not serve the key before the block: %d %s", i, r.status, r.body)
		}
	}

	bound := f.apps[0].Invalidator.Bound(len(f.apps))
	called := time.Now()
	f.mustAdmin(t, 0, "/user/update", `{"user_id":"u-block","blocked":true}`)
	control := time.Since(called)
	start := time.Now()

	// The node that served the call has NO window: the administration surface
	// applies the invalidation locally before it answers, so the very first
	// request after the 200 is already refused. A control whose own node has a
	// window is a control the operator cannot verify.
	r := f.serves(t, 0, token)
	if r.status == http.StatusOK {
		t.Fatal("the node that took /user/update kept serving the blocked user's key: " +
			"either the invalidation was not applied locally, or auth.Principal.User is " +
			"nil again and users.blocked reaches no decision")
	}
	if r.status != http.StatusForbidden {
		t.Errorf("a blocked user's key answered %d, want 403 (COMPATIBILITY §11.2)\n%s",
			r.status, r.body)
	}
	if !strings.Contains(r.body, "key_blocked") {
		t.Errorf("the refusal does not name the condition:\n%s", r.body)
	}

	// The other node has to learn it from the invalidation bus.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if f.serves(t, 1, token).status != http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second node was still serving a blocked user's key after %v",
				time.Since(start))
		}
		time.Sleep(time.Millisecond)
	}
	propagation := time.Since(start)

	t.Logf("USER BLOCK PROPAGATION (%d nodes, poll 20ms): window 0 on the node that served "+
		"the call; %v to the second node against a published bound of %v (%s); the control's "+
		"own durable write plus announcement took %v; TTL fallback %v",
		len(f.apps), propagation, bound.Bound, bound.Formula, control, bound.Fallback)

	if propagation > bound.Bound {
		t.Errorf("measured %v, which exceeds the published bound of %v (%s)",
			propagation, bound.Bound, bound.Formula)
	}

	// Unblocking is the same mechanism in the other direction, and it is the one
	// an operator runs during an outage they caused. It must not wait for a TTL
	// either.
	f.mustAdmin(t, 0, "/user/update", `{"user_id":"u-block","blocked":false}`)
	if r := f.serves(t, 0, token); r.status != http.StatusOK {
		t.Errorf("unblocking the user did not let its key serve again: %d %s", r.status, r.body)
	}
	unblockStart := time.Now()
	for {
		if f.serves(t, 1, token).status == http.StatusOK {
			break
		}
		if time.Since(unblockStart) > 10*time.Second {
			t.Fatalf("the second node still refused an unblocked user after %v",
				time.Since(unblockStart))
		}
		time.Sleep(time.Millisecond)
	}

	// And the durable half really happened, which is what makes the refusal a
	// policy rather than a cache artefact. This is the only row read in the
	// file, and it is checked AFTER the behaviour rather than instead of it.
	got, err := f.apps[0].Store.GetUser(ctx, "u-block")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.Blocked {
		t.Error("the user row is still blocked after /user/update cleared it")
	}
}

// A blocked TEAM refuses its keys too, and the refusal survives the key's own
// limits being untouched.
//
// It is a separate test from the user one because they are separate fields on
// separate tables reached through separate joins, and a single implementation
// that populated one and not the other would pass a test that only checked the
// other.
func TestBlockingATeamStopsItsKeys(t *testing.T) {
	f := newDirectoryFleet(t, 1, nil)

	const token = "sk-blocked-team-key" // pragma: allowlist secret — test fixture
	f.mustAdmin(t, 0, "/team/new", `{"team_id":"t-block","team_name":"Engineering"}`)
	plantKey(t, f.apps[0].Store, token, "", "t-block")

	if r := f.serves(t, 0, token); r.status != http.StatusOK {
		t.Fatalf("the key did not serve before the team was blocked: %d %s", r.status, r.body)
	}
	f.mustAdmin(t, 0, "/team/update", `{"team_id":"t-block","blocked":true}`)
	r := f.serves(t, 0, token)
	if r.status == http.StatusOK {
		t.Fatal("a blocked team's key kept serving: teams.blocked reaches no decision")
	}
	if r.status != http.StatusForbidden || !strings.Contains(r.body, "key_blocked") {
		t.Errorf("a blocked team's key answered %d\n%s", r.status, r.body)
	}
}

// ---------------------------------------------------------------------------
// A team ceiling, lowered and cleared
// ---------------------------------------------------------------------------

// A team ceiling lowered below recorded spend refuses, and clearing it lets
// traffic through again.
//
// This is how an operator ENDS a budget outage, and both halves were broken in
// different ways. The ceiling could not be set at all — `/budget/*` answered 501
// — and had it been settable, `budgetSubjectsOf` reads `p.Team`, which was nil,
// so a team ceiling would have held no budget and refused nothing.
//
// The refusal is asserted on a node with no live lease block for the team. That
// is not a convenience: DESIGN §9.6 charges a whole block before a unit of it is
// spent, so the node holding a block drawn under the OLD ceiling can serve up to
// a block more before it re-reads the limit. §5.6 publishes that as the
// overshoot; asserting the refusal on the node that already holds the block
// would be asserting the absence of a documented property.
func TestATeamCeilingBelowSpendRefusesAndClearingItServes(t *testing.T) {
	// A block far smaller than the ceiling, so the spend below is recorded
	// durably rather than sitting in one node's block.
	f := newDirectoryFleet(t, 2, func(o *Options) {
		o.BudgetBlockNanoUSD = directoryCostPerRequest
	})
	ctx := context.Background()

	const token = "sk-team-budget-key" // pragma: allowlist secret — test fixture
	f.mustAdmin(t, 0, "/team/new", `{"team_id":"t-budget","team_name":"Research"}`)
	plantKey(t, f.apps[0].Store, token, "", "t-budget")

	// A generous ceiling first, so the traffic below is admitted and counted.
	f.mustAdmin(t, 0, "/budget/new",
		`{"budget_id":"team:t-budget","max_budget":1.0,"budget_duration":"monthly"}`)

	const requests = 3
	for i := 0; i < requests; i++ {
		if r := f.serves(t, 0, token); r.status != http.StatusOK {
			t.Fatalf("request %d under a $1.00 team ceiling was refused: %d %s", i, r.status, r.body)
		}
	}

	// What the durable counter holds is what a lowered ceiling will be compared
	// against, so the new ceiling is derived from it rather than guessed.
	key := cluster.BudgetKey("team", "t-budget", quota.Monthly, time.Now())
	committed, err := f.apps[0].Ledger.Committed(ctx, key)
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if committed <= 0 {
		t.Fatalf("the team counter holds %d after %d served requests: the team ceiling took "+
			"no hold at all, so auth.Principal.Team never reached budgetSubjectsOf", committed, requests)
	}

	// /budget/info must report the ENFORCED figure. An operator deciding where
	// to put a ceiling reads this number, and a report that lagged the counter
	// would send them to a limit the gate does not agree with.
	info := f.mustAdmin(t, 0, "/budget/info", `{"budget_id":"team:t-budget"}`)
	if spent := budgetField(t, info.body, "spend"); spent <= 0 {
		t.Errorf("/budget/info reports spend %v after %d requests; the enforced counter holds %d",
			spent, requests, committed)
	}

	// Now the outage: a ceiling below what has already been spent.
	low := float64(committed/2) / 1e9
	f.mustAdmin(t, 0, "/budget/update",
		fmt.Sprintf(`{"budget_id":"team:t-budget","max_budget":%.9f}`, low))

	// Node 1 has never drawn a block for this team, so it reads the counter and
	// the new ceiling together.
	deadline := time.Now().Add(10 * time.Second)
	var refusal directoryResponse
	for {
		refusal = f.serves(t, 1, token)
		if refusal.status != http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a team ceiling below recorded spend never refused: the ceiling on a team " +
				"reaches no budget hold")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if refusal.status != http.StatusBadRequest {
		t.Errorf("an exhausted team budget answered %d, want 400 (§6.4: a 429 would send the "+
			"request down the fallback chain to spend another subject's budget)\n%s",
			refusal.status, refusal.body)
	}
	if !strings.Contains(refusal.body, "budget") {
		t.Errorf("the refusal does not say it is about a budget:\n%s", refusal.body)
	}
	if !strings.Contains(refusal.body, "team") {
		t.Errorf("the refusal does not name the TEAM as the subject that refused, which is the "+
			"one thing the operator needs to know:\n%s", refusal.body)
	}

	// And the recovery. Clearing the ceiling must let traffic through without
	// erasing what was spent under it — the spend is the ledger's, and an
	// operator restoring service is not making a billing decision.
	f.mustAdmin(t, 0, "/budget/delete", `{"budget_id":"team:t-budget"}`)
	recovered := time.Now()
	for {
		if r := f.serves(t, 1, token); r.status == http.StatusOK {
			break
		}
		if time.Since(recovered) > 10*time.Second {
			t.Fatalf("clearing the team ceiling did not restore service after %v",
				time.Since(recovered))
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Logf("TEAM BUDGET: %d nano-USD recorded, ceiling lowered to %d and refused, cleared and "+
		"restored in %v", committed, committed/2, time.Since(recovered))

	after, err := f.apps[0].Ledger.Committed(ctx, key)
	if err != nil {
		t.Fatalf("Committed after clearing: %v", err)
	}
	if after < committed {
		t.Errorf("clearing the ceiling dropped recorded spend from %d to %d; /budget/delete "+
			"removes a limit, not a ledger", committed, after)
	}
}

// budgetField reads one numeric field out of a `{"budget": {...}}` answer.
func budgetField(t *testing.T, body, field string) float64 {
	t.Helper()
	var envelope struct {
		Budget map[string]any `json:"budget"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decoding %s: %v\n%s", field, err, body)
	}
	v, ok := envelope.Budget[field]
	if !ok {
		t.Fatalf("no %q in the budget answer:\n%s", field, body)
	}
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("%q is %T, want a number:\n%s", field, v, body)
	}
	return n
}

// ---------------------------------------------------------------------------
// What answers, and what still names its gap
// ---------------------------------------------------------------------------

// The routes that answered 501 now answer, and the one that still does not says
// why.
//
// The second half matters as much as the first. `/model/*` is NOT waiting on a
// store layer — the `deployments` and `model_aliases` tables have been there
// since the first migration. It is waiting on a READER: the routing table is
// compiled from configuration by buildRouter, and nothing in the binary reads
// those tables. An adapter over them would answer 200, write the row, and route
// no traffic differently, which is the exact failure the rest of this file
// exists to remove. So it stays a 501 that names the missing dependency.
func TestDirectoryRoutesAnswerAndModelRoutesNameTheirGap(t *testing.T) {
	f := newDirectoryFleet(t, 1, nil)

	for _, tc := range []struct{ path, body string }{
		{"/user/new", `{"user_id":"u-live","user_email":"live@example.test"}`},
		{"/user/info", `{"user_id":"u-live"}`},
		{"/user/list", `{}`},
		{"/team/new", `{"team_id":"t-live","team_name":"Live"}`},
		{"/team/info", `{"team_id":"t-live"}`},
		{"/team/list", `{}`},
		{"/team/member_add", `{"team_id":"t-live","user_id":"u-live"}`},
		{"/budget/new", `{"budget_id":"team:t-live","max_budget":5.0}`},
		{"/budget/info", `{"budget_id":"team:t-live"}`},
		{"/budget/list", `{}`},
	} {
		r := f.admin(t, 0, tc.path, tc.body)
		if r.status == http.StatusNotImplemented {
			t.Errorf("POST %s still answers 501 in the shipped binary: %s", tc.path, r.body)
			continue
		}
		if r.status != http.StatusOK {
			t.Errorf("POST %s: %d %s", tc.path, r.status, r.body)
		}
	}

	// The gaps that remain, each named rather than generic.
	for _, tc := range []struct{ path, body, dependency string }{
		{"/model/new", `{"model_name":"m","litellm_params":{}}`, "model registry"},
		{"/model/info", `{}`, "model registry"},
		{"/model_group/info", `{}`, "model registry"},
	} {
		r := f.admin(t, 0, tc.path, tc.body)
		if r.status != http.StatusNotImplemented {
			t.Errorf("POST %s answered %d; if a model registry has been wired, this test and "+
				"the note in app/admin.go both have to change\n%s", tc.path, r.status, r.body)
			continue
		}
		if !strings.Contains(r.body, "dependency_not_configured") {
			t.Errorf("POST %s refuses generically rather than naming the dependency:\n%s",
				tc.path, r.body)
		}
		if !strings.Contains(r.body, tc.dependency) {
			t.Errorf("POST %s does not name %q as the missing piece:\n%s",
				tc.path, tc.dependency, r.body)
		}
	}

	// A budget on a subject this schema has no ceiling column for is refused BY
	// NAME too, rather than written into a table nothing reads.
	r := f.admin(t, 0, "/budget/new", `{"budget_id":"global:","max_budget":5.0}`)
	if r.status != http.StatusNotImplemented {
		t.Errorf("a global budget answered %d; §9.2 gives it no ceiling column\n%s",
			r.status, r.body)
	}
	if !strings.Contains(r.body, "ceiling column") {
		t.Errorf("the refusal does not say why a global budget cannot be stored:\n%s", r.body)
	}
}

// ---------------------------------------------------------------------------
// The limits that are not the block
// ---------------------------------------------------------------------------

// A team's model allow-list and its concurrency ceiling reach enforcement.
//
// They are on the same nil pointer the block was, and they fail differently, so
// a fix that populated `Blocked` alone would leave two configured limits doing
// nothing. `MaxParallel` is asserted through [principal.maxParallel] because
// that is the value internal/capacity is handed; the allow-list is asserted
// through a request, because it has an answer a client can see.
func TestATeamsAllowListAndConcurrencyCeilingReachEnforcement(t *testing.T) {
	f := newDirectoryFleet(t, 1, nil)
	ctx := context.Background()

	const token = "sk-team-limits-key" // pragma: allowlist secret — test fixture
	f.mustAdmin(t, 0, "/team/new",
		`{"team_id":"t-limits","team_name":"Limits","models":["some-other-model"],"max_parallel_requests":3}`)
	plantKey(t, f.apps[0].Store, token, "", "t-limits")

	r := f.serves(t, 0, token)
	if r.status == http.StatusOK {
		t.Fatal("a team allow-list that excludes model-x served it: teams.models reaches no decision")
	}
	if r.status != http.StatusForbidden || !strings.Contains(r.body, "model_not_allowed") {
		t.Errorf("a model outside the team's allow-list answered %d\n%s", r.status, r.body)
	}

	p, err := f.apps[0].Auth.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	pr := &principal{p: p, now: f.apps[0].now, rates: newKeyRates(f.apps[0].now)}
	if got := pr.maxParallel(); got != 3 {
		t.Errorf("the request carries a concurrency ceiling of %d to internal/capacity, want the "+
			"team's 3; auth.MostRestrictiveParallel is reading a nil Team again", got)
	}
}

// A key whose owner row was deleted still authenticates.
//
// The schema carries no foreign key on `api_keys.user_id` on purpose —
// authentication must not fail because of referential noise — so a dangling
// owner id is a state the gateway has to have an answer for. The LEFT JOIN's
// answer is "no such owner, therefore no restriction from one", which is the
// same value an unowned key gets. An INNER join here would turn a working
// credential into an unknown key, which fails closed for a reason the caller
// cannot act on and the operator did not choose.
func TestAKeyWhoseOwnerWasDeletedStillAuthenticates(t *testing.T) {
	f := newDirectoryFleet(t, 1, nil)

	const token = "sk-orphan-owner-key" // pragma: allowlist secret — test fixture
	f.mustAdmin(t, 0, "/user/new", `{"user_id":"u-gone","user_email":"gone@example.test"}`)
	plantKey(t, f.apps[0].Store, token, "u-gone", "t-never-existed")

	if r := f.serves(t, 0, token); r.status != http.StatusOK {
		t.Fatalf("a key naming a team that does not exist was refused: %d %s", r.status, r.body)
	}
	f.mustAdmin(t, 0, "/user/delete", `{"user_ids":["u-gone"]}`)
	if r := f.serves(t, 0, token); r.status != http.StatusOK {
		t.Fatalf("deleting the owning user made its key unauthenticatable rather than "+
			"unrestricted: %d %s", r.status, r.body)
	}
}
