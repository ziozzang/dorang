package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/batch"
	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// ---------------------------------------------------------------------------
// [CRITICAL] a batch row is a paid model call and is authorized like one
// ---------------------------------------------------------------------------

// A batch row naming a model the owning key may not use is refused, and never
// reaches an upstream.
//
// This is the last line of the critical finding. The create body names no
// model, so the gate had nothing to check; the batch scheduler resolved each
// row's model against the process-wide target table with no reference to the
// owner; and the executor dispatched. A key restricted to gpt-4o-mini could
// upload a JSONL file naming claude-opus-4 on every line and have all of them
// run on the operator's credential.
//
// The executor is the choke point every dispatched row must pass, which is why
// the check lives here as well as at upload: an upload validated under one
// allow-list can be dispatched hours later, by a different process, after the
// key's limits have changed.
func TestBatchExecutorRefusesAModelTheOwnerMayNotUse(t *testing.T) {
	reached := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer up.Close()

	exec := newTestBatchExecutor(t, up.URL, &auth.Principal{
		KeyID: "key-1",
		Key:   auth.Limits{Models: []string{"gpt-4o-mini"}},
	}, nil)

	_, err := exec.Execute(context.Background(), &batch.ExecRequest{
		BatchID: "batch-1", CustomID: "row-0", OwnerKeyID: "key-1",
		Endpoint: "/v1/chat/completions", Model: "claude-opus-4",
		Provider: "p1", UpstreamModel: "up-1",
		Body: []byte(`{"model":"claude-opus-4","messages":[]}`),
	})
	if err == nil {
		t.Fatal("a batch row dispatched a model the owning key's allow-list refuses")
	}
	if reached {
		t.Error("the row reached the provider: the operator's credential was spent")
	}
	if !batch.IsTerminal(err) {
		t.Error("the refusal is retryable, so the scheduler will re-attempt it until MaxAttempts")
	}
	if !strings.Contains(err.Error(), "claude-opus-4") {
		t.Errorf("the refusal does not name the model: %v", err)
	}
}

// A row naming an allowed model still runs.
func TestBatchExecutorRunsAnAllowedModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()

	exec := newTestBatchExecutor(t, up.URL, &auth.Principal{
		KeyID: "key-1",
		Key:   auth.Limits{Models: []string{"m1"}},
	}, nil)

	res, err := exec.Execute(context.Background(), &batch.ExecRequest{
		BatchID: "batch-1", CustomID: "row-0", OwnerKeyID: "key-1",
		Endpoint: "/v1/chat/completions", Model: "m1",
		Provider: "p1", UpstreamModel: "up-1",
		Body: []byte(`{"model":"m1","messages":[]}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
}

// A batch owned by a blocked credential stops dispatching.
//
// Without this, submitting a large batch and then having the key blocked is a
// way to keep spending after revocation — which matters more than usual here,
// because there is no API path to revoke a key at all (see docs/SECURITY-REVIEW.md).
func TestBatchExecutorRefusesABlockedOwner(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a blocked credential's batch row reached the provider")
	}))
	defer up.Close()

	exec := newTestBatchExecutor(t, up.URL, &auth.Principal{
		KeyID: "key-1",
		Key:   auth.Limits{Blocked: true},
	}, nil)

	_, err := exec.Execute(context.Background(), &batch.ExecRequest{
		BatchID: "b", CustomID: "r", OwnerKeyID: "key-1",
		Endpoint: "/v1/chat/completions", Model: "m1",
		Provider: "p1", UpstreamModel: "up-1",
		Body: []byte(`{"model":"m1","messages":[]}`),
	})
	if err == nil {
		t.Fatal("a blocked credential's batch kept running")
	}
}

// A batch row that names an owner the store does not know is refused rather
// than dispatched as nobody.
func TestBatchExecutorRefusesAnUnknownOwner(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a row with no resolvable owner reached the provider")
	}))
	defer up.Close()

	exec := newTestBatchExecutor(t, up.URL, &auth.Principal{KeyID: "key-1"}, nil)

	for _, owner := range []string{"", "key-that-does-not-exist"} {
		_, err := exec.Execute(context.Background(), &batch.ExecRequest{
			BatchID: "b", CustomID: "r", OwnerKeyID: owner,
			Endpoint: "/v1/chat/completions", Model: "m1",
			Provider: "p1", UpstreamModel: "up-1",
			Body: []byte(`{"model":"m1","messages":[]}`),
		})
		if err == nil {
			t.Errorf("owner %q: a row with no resolvable owner was dispatched", owner)
		}
	}
}

// Batch spend reaches the budget ceiling.
//
// budgetGate.reserve had exactly one caller, dispatcher.Dispatch, and the batch
// executor dispatched directly — so a batch was unbudgeted spend on the
// operator's provider credential, at whatever rate the scheduler could hold
// capacity for.
func TestBatchExecutionIsBudgeted(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":1000}}`))
	}))
	defer up.Close()

	gate := newTestBudgetGate(t)
	// One US dollar, and a row priced far above it.
	owner := &auth.Principal{
		KeyID: "key-1",
		Key: auth.Limits{
			MaxBudgetNanoUSD: auth.Limit(1_000_000_000),
			BudgetPeriod:     "monthly",
		},
	}
	exec := newTestBatchExecutor(t, up.URL, owner, gate)

	run := func() error {
		_, err := exec.Execute(context.Background(), &batch.ExecRequest{
			BatchID: "b", CustomID: "r", OwnerKeyID: "key-1",
			Endpoint: "/v1/chat/completions", Model: "m1",
			Provider: "p1", UpstreamModel: "up-1",
			Body: []byte(`{"model":"m1","messages":[],"max_tokens":1000000}`),
		})
		return err
	}

	// Rows run until the ceiling is reached, and then stop. Without the gate
	// they never stop.
	var lastErr error
	for i := 0; i < 200; i++ {
		if lastErr = run(); lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatalf("200 batch rows ran against a 1 USD budget without ever being refused "+
			"(%d upstream calls): batch spend reaches no ceiling", calls)
	}
	if !strings.Contains(lastErr.Error(), "budget") {
		t.Errorf("the refusal is not a budget refusal: %v", lastErr)
	}
}

// ---------------------------------------------------------------------------
// [HIGH] a team budget is a team's budget, not a budget per key
// ---------------------------------------------------------------------------

// One team ceiling binds every key under it, once.
//
// budget() took the MINIMUM ceiling across key, user and team — the right
// direction — and then reserved it against the KEY's durable counter. Ten keys
// under a 100 USD team budget therefore got ten independent 100 USD counters
// and the team spent 1000. Each key was correctly refused at 100; there were
// simply ten of them.
func TestTeamBudgetIsNotMultipliedByTheNumberOfKeys(t *testing.T) {
	gate := newTestBudgetGate(t)
	const teamCeiling = 1_000_000_000 // 1 USD
	const perRequest = 100_000_000    // 0.10 USD

	spend := func(keyID string) error {
		p := &auth.Principal{
			KeyID: keyID, TeamID: "team-a",
			Team: &auth.Limits{
				MaxBudgetNanoUSD: auth.Limit(teamCeiling),
				BudgetPeriod:     "monthly",
			},
		}
		hold, _, err := gate.reserveFor(context.Background(), budgetSubjectsOf(p), perRequest, nil)
		if err != nil {
			return err
		}
		hold.settle(perRequest)
		return nil
	}

	// Ten different keys, all under the one team. Ten requests each: a hundred
	// requests at 0.10 USD is 10 USD against a 1 USD team ceiling.
	refused := 0
	for i := 0; i < 100; i++ {
		if err := spend("key-" + string(rune('a'+i%10))); err != nil {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("ten keys spent ten times the team ceiling and none was refused: " +
			"the team budget is being enforced per key")
	}
}

// Each subject keeps its own period.
//
// budget() took the minimum LIMIT across subjects but the FIRST NON-EMPTY
// period, so a key with a monthly 10 USD budget under a team with a daily 1 USD
// budget produced "1 USD, monthly" — the daily ceiling applied over a monthly
// window, which is the exact hazard internal/auth documents.
func TestEachBudgetSubjectKeepsItsOwnPeriod(t *testing.T) {
	p := &auth.Principal{
		KeyID: "key-1", TeamID: "team-a",
		Key: auth.Limits{
			MaxBudgetNanoUSD: auth.Limit(10_000_000_000), BudgetPeriod: "monthly",
		},
		Team: &auth.Limits{
			MaxBudgetNanoUSD: auth.Limit(1_000_000_000), BudgetPeriod: "daily",
		},
	}
	subs := budgetSubjectsOf(p)
	if len(subs) != 2 {
		t.Fatalf("subjects = %d, want one per declaring subject", len(subs))
	}
	byKind := map[string]budgetSubject{}
	for _, s := range subs {
		byKind[s.kind] = s
	}
	key, team := byKind["key"], byKind["team"]

	if key.window != quota.Monthly {
		t.Errorf("the key's 10 USD monthly ceiling landed in a %s window", key.window)
	}
	if team.window == quota.Monthly {
		t.Error("the team's DAILY 1 USD ceiling is being applied over a monthly window: " +
			"a limit was carried without its period")
	}
	if key.id != "key-1" || team.id != "team-a" {
		t.Errorf("subjects are not counted against themselves: key=%q team=%q", key.id, team.id)
	}
}

// A subject that declares no ceiling contributes no hold, and a principal with
// no ceilings anywhere is unbudgeted.
func TestBudgetSubjectsOnlyCoverDeclaredCeilings(t *testing.T) {
	if subs := budgetSubjectsOf(&auth.Principal{KeyID: "k"}); len(subs) != 0 {
		t.Errorf("an unbudgeted principal produced %d subjects", len(subs))
	}
	if subs := budgetSubjectsOf(&auth.Principal{KeyID: "k", Master: true}); len(subs) != 0 {
		t.Error("the master credential was given a budget")
	}
}

// ---------------------------------------------------------------------------
// [HIGH] rpm_limit, tpm_limit and max_parallel_requests enforce something
// ---------------------------------------------------------------------------

// The observed request rate reaches the check.
//
// internal/auth's rate branch reads Access.ObservedRPM, and neither production
// constructor ever assigned it — so every comparison was `0 >= limit`, false
// for every positive limit. The check was unit-tested by a test that set the
// observed value itself, which is why nothing noticed.
func TestRPMLimitIsEnforced(t *testing.T) {
	now := time.Now()
	p := &principal{
		p: &auth.Principal{
			KeyID: "key-1",
			Key:   auth.Limits{RPMLimit: auth.Limit(2)},
		},
		now:   func() time.Time { return now },
		rates: newRateMeter(func() time.Time { return now }),
	}
	// Each Authorize is a separate HTTP request, so each gets its own
	// per-request principal sharing the one meter.
	fresh := func() *principal {
		return &principal{p: p.p, now: p.now, rates: p.rates}
	}

	if err := fresh().Authorize(server.Access{Route: "/v1/chat/completions"}); err != nil {
		t.Fatalf("request 1 refused: %v", err)
	}
	if err := fresh().Authorize(server.Access{Route: "/v1/chat/completions"}); err != nil {
		t.Fatalf("request 2 refused: %v", err)
	}
	if err := fresh().Authorize(server.Access{Route: "/v1/chat/completions"}); err == nil {
		t.Fatal("a key with rpm_limit 2 served a third request in the same minute: " +
			"the observed rate never reaches the check")
	}
}

// The observed token rate reaches the check, and is fed from settlement.
func TestTPMLimitIsEnforced(t *testing.T) {
	now := time.Now()
	rates := newRateMeter(func() time.Time { return now })
	ap := &auth.Principal{
		KeyID: "key-1",
		Key:   auth.Limits{TPMLimit: auth.Limit(100)},
	}
	fresh := func() *principal {
		return &principal{p: ap, now: func() time.Time { return now }, rates: rates}
	}

	first := fresh()
	if err := first.Authorize(server.Access{}); err != nil {
		t.Fatalf("the first request was refused: %v", err)
	}
	// Settlement is where the token count exists.
	first.recordTokens(150)

	if err := fresh().Authorize(server.Access{}); err == nil {
		t.Fatal("a key with tpm_limit 100 was admitted after spending 150 tokens " +
			"in the same minute")
	}
}

// A team's rate ceiling counts every key under it.
//
// Access carried ONE observed number for three subjects, so a team limit was
// being compared against one key's count — a team ceiling enforced per key, the
// same shape as the budget defect.
func TestTeamRPMCountsEveryKeyUnderTheTeam(t *testing.T) {
	now := time.Now()
	rates := newRateMeter(func() time.Time { return now })
	team := &auth.Limits{RPMLimit: auth.Limit(2)}

	admit := func(keyID string) error {
		return (&principal{
			p: &auth.Principal{
				KeyID: keyID, TeamID: "team-a", Team: team,
			},
			now:   func() time.Time { return now },
			rates: rates,
		}).Authorize(server.Access{})
	}

	if err := admit("key-1"); err != nil {
		t.Fatalf("key-1 refused: %v", err)
	}
	if err := admit("key-2"); err != nil {
		t.Fatalf("key-2 refused: %v", err)
	}
	if err := admit("key-3"); err == nil {
		t.Fatal("three different keys under one team with rpm_limit 2 were all admitted: " +
			"the team ceiling is being counted per key")
	}
}

// The rate window rolls, so a ceiling is per minute rather than forever.
func TestRateWindowRolls(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	rates := newRateMeter(clock)
	ap := &auth.Principal{KeyID: "key-1", Key: auth.Limits{RPMLimit: auth.Limit(1)}}

	if err := (&principal{p: ap, now: clock, rates: rates}).Authorize(server.Access{}); err != nil {
		t.Fatalf("the first request was refused: %v", err)
	}
	if err := (&principal{p: ap, now: clock, rates: rates}).Authorize(server.Access{}); err == nil {
		t.Fatal("the ceiling did not bind within the minute")
	}
	now = now.Add(90 * time.Second)
	if err := (&principal{p: ap, now: clock, rates: rates}).Authorize(server.Access{}); err != nil {
		t.Fatalf("the ceiling outlived its minute: %v", err)
	}
}

// max_parallel_requests reaches the concurrency broker.
//
// The column was stored, imported and administered, and the identifier
// MaxParallel did not occur in internal/capacity at all — the doc comment
// claiming capacity enforced it was simply false. The broker's per-principal
// ceiling came only from static YAML.
func TestMaxParallelReachesThePrincipal(t *testing.T) {
	p := &principal{p: &auth.Principal{
		KeyID: "key-1",
		Key:   auth.Limits{MaxParallel: auth.Limit(4)},
		Team:  &auth.Limits{MaxParallel: auth.Limit(2)},
	}}
	if got := p.MaxParallel(); got != 2 {
		t.Errorf("MaxParallel = %d, want the most restrictive across subjects (2)", got)
	}
	if got := principalMaxParallel(p); got != 2 {
		t.Errorf("the dispatcher reads %d, want 2", got)
	}
	if got := principalMaxParallel(nil); got != 0 {
		t.Errorf("a nil principal declares %d, want no ceiling", got)
	}
}

// ---------------------------------------------------------------------------
// [HIGH] cache affinity is tenant-scoped in the shipped binary
// ---------------------------------------------------------------------------

// The routing request carries a tenant.
//
// router.Request.Tenant leads the session pin (§7.4a) and now the prefix chain
// (§7.4b). It was assigned in exactly one place in the whole tree —
// testing/scenario/harness.go — so in the shipped binary the leading component
// of both keys was always "": two tenants presenting the same session id shared
// a pin, and one tenant's prefix entry answered another's lookup.
func TestRouterRequestCarriesTheTenant(t *testing.T) {
	cases := []struct {
		name string
		p    server.Principal
		want string
	}{
		{"team wins", &principal{p: &auth.Principal{KeyID: "k", UserID: "u", TeamID: "t"}}, "team:t"},
		{"user next", &principal{p: &auth.Principal{KeyID: "k", UserID: "u"}}, "user:u"},
		{"key last", &principal{p: &auth.Principal{KeyID: "k"}}, "key:k"},
		{"no principal", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tenantOf(c.p); got != c.want {
				t.Errorf("tenantOf = %q, want %q", got, c.want)
			}
		})
	}

	// And it actually lands on the request the router is asked.
	d := newDispatcher(http.DefaultClient, t.Logf, time.Now)
	d.swap(&dispatchState{})
	rq := newDecodeRequest(t, `{"model":"m1","messages":[]}`,
		&principal{p: &auth.Principal{KeyID: "key-1", TeamID: "team-a"}})

	c, err := d.decode(d.state(), rq)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.rreq.Tenant != "team:team-a" {
		t.Errorf("router.Request.Tenant = %q: the field is populated only by the test "+
			"harness, so session pins and prefix entries are shared across tenants",
			c.rreq.Tenant)
	}
}

// ---------------------------------------------------------------------------
// [HIGH] the upstream cannot OOM the gateway with one answer
// ---------------------------------------------------------------------------

// A non-streaming answer past the ceiling is refused rather than buffered.
//
// The error path four lines above was bounded at 1 MiB and the success path was
// a bare io.ReadAll. convertResponse then unmarshals and re-marshals the
// buffer, so peak resident memory was three to four times the body, per
// concurrent request, driven entirely from the far side of the trust boundary.
func TestOversizedUpstreamResponseIsRefused(t *testing.T) {
	const limit = 64 << 10
	body := strings.Repeat("x", limit*4)

	// The reader counts. Asserting only that the call FAILS would pass against
	// an implementation that reads the whole body and then measures it, which
	// is not the property: the finding was a remote OOM, so the bytes must not
	// be read, not merely not returned.
	src := &countingReader{r: strings.NewReader(body)}
	got, err := readUpstreamBody(src, limit)
	if err == nil {
		t.Fatalf("a %d byte answer was buffered whole under a %d byte ceiling (%d read)",
			len(body), limit, len(got))
	}
	if err != errUpstreamTooLarge {
		t.Fatalf("err = %v, want errUpstreamTooLarge", err)
	}
	if src.n > limit+1 {
		t.Fatalf("read %d bytes under a %d byte ceiling: the read is unbounded, "+
			"the refusal merely happens afterwards", src.n, limit)
	}

	// A body at exactly the ceiling is fine; the bound is not off by one.
	exact := strings.Repeat("y", limit)
	if got, err := readUpstreamBody(strings.NewReader(exact), limit); err != nil {
		t.Fatalf("a body of exactly the ceiling was refused: %v", err)
	} else if len(got) != limit {
		t.Fatalf("read %d bytes, want %d", len(got), limit)
	}

	// And a zero ceiling means the default, not "no limit".
	if _, err := readUpstreamBody(strings.NewReader("small"), 0); err != nil {
		t.Fatalf("a small body was refused under the default ceiling: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fixedOwnerLoader resolves every key id to one principal.
type fixedOwnerLoader struct{ p *auth.Principal }

func (l *fixedOwnerLoader) principalByKeyID(_ context.Context, id string) (*auth.Principal, error) {
	if l.p == nil || l.p.KeyID != id {
		return nil, errOwnerGone
	}
	return l.p, nil
}

// newTestBatchExecutor builds an executor pointed at a fake upstream, owned by
// one principal.
func newTestBatchExecutor(t *testing.T, baseURL string, owner *auth.Principal,
	gate *budgetGate) *batchExecutor {
	t.Helper()

	table := &upstreamTable{
		providers: map[string]*upstream{
			"p1": {name: "p1", baseURL: baseURL, api: catalog.APIOpenAIChat, kind: "openai"},
		},
		creds: map[string]*credential{
			"cred-a": {id: "cred-a", provider: "p1", secret: "provider-secret-value"}, // pragma: allowlist secret — fixture
		},
	}
	d := newDispatcher(http.DefaultClient, t.Logf, time.Now)
	d.swap(&dispatchState{
		upstreams: table,
		budget:    gate,
		pricing:   testPricing(t),
	})
	return &batchExecutor{
		d:      d,
		owners: newOwnerResolver(&fixedOwnerLoader{p: owner}, time.Now),
	}
}

// testPricing is a catalog that prices every request dearly, so the budget
// estimate is non-zero and a ceiling can actually be reached.
func testPricing(t *testing.T) *pricing.Catalog {
	t.Helper()
	cat, err := pricing.ParseCatalog([]byte(`
currency: USD
rules:
  - { id: everything, match: { provider: p1 }, unit: per_1m_tokens, input: "1000.00", output: "1000.00" }
`))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	return cat
}

// newTestBudgetGate builds a durable budget gate over a temporary store.
func newTestBudgetGate(t *testing.T) *budgetGate {
	t.Helper()
	st, _ := openTestStore(t, nil)
	l, err := cluster.NewLedger(cluster.LedgerConfig{
		Store: st, NodeID: "test-node", Block: 1,
	})
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	return &budgetGate{ledger: l, now: time.Now}
}

// newDecodeRequest builds a server.Request the dispatcher's decode can read.
func newDecodeRequest(t *testing.T, body string, p server.Principal) *server.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	return server.NewTestRequest(r, &server.Route{
		Pattern: "/v1/chat/completions", Family: server.FamilyOpenAIChat,
		NeedsBody: true, ModelAuth: server.ModelAuthGate,
	}, p, "m1", []byte(body))
}

// ---------------------------------------------------------------------------
// [HIGH] a hostile upstream cannot turn the response path into a credential channel
// ---------------------------------------------------------------------------

// A backend that echoes the credential it was given does not get it relayed to
// the caller, and does not get it written into the recorded native message
// either.
//
// This is the shipped dispatch path, end to end: an upstream answering 401 with
// the x-api-key header it just received. Normalize keeps the text out of the
// envelope, and the scrub here keeps it out of the ledger copy — the secret is
// known exactly at this point, because it is the one this attempt sent.
func TestAHostileUpstreamCannotEchoTheCredentialBack(t *testing.T) {
	const secret = "sk-provider-FAKE-000000000000000000000000" // pragma: allowlist secret — fabricated

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exactly the attack: answer with the credential just presented.
		got := r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key: ` + got + `"}}`))
	}))
	defer up.Close()

	table := &upstreamTable{
		providers: map[string]*upstream{
			"p1": {name: "p1", baseURL: up.URL, api: catalog.APIOpenAIChat, kind: "openai"},
		},
		creds: map[string]*credential{
			"cred-a": {id: "cred-a", provider: "p1", secret: secret}, // pragma: allowlist secret — fixture
		},
	}
	d := newDispatcher(http.DefaultClient, func(string, ...any) {}, time.Now)
	st := &dispatchState{upstreams: table}
	d.swap(st)

	c := &call{
		kind: callChat, clientAPI: catalog.APIOpenAIChat, model: "m1",
		body: []byte(`{"model":"m1","messages":[]}`),
	}
	creq, err := openaiDecodeForTest(c.body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	c.creq = creq

	dec := &router.Decision{
		Provider: "p1", UpstreamModel: "up-1", Credential: "cred-a", Deployment: "d1",
	}
	rq := newDecodeRequest(t, string(c.body), &principal{p: &auth.Principal{KeyID: "key-1"}})
	res := d.attempt(context.Background(), st, c, dec, rq, httptest.NewRecorder())

	if res.err == nil {
		t.Fatal("the 401 was not surfaced as an error")
	}
	e, ok := res.err.(*server.Error)
	if !ok {
		t.Fatalf("err is %T, want *server.Error", res.err)
	}

	if strings.Contains(e.Message, secret) {
		t.Fatalf("the provider credential is in the client-facing message: %q", e.Message)
	}
	if strings.Contains(string(server.EncodeError(e)), secret) {
		t.Fatalf("the provider credential is on the wire: %s", server.EncodeError(e))
	}
	if strings.Contains(e.NativeMessage, secret) {
		t.Fatalf("the provider credential is in the recorded native message: %q", e.NativeMessage)
	}
	if e.NativeMessage == "" {
		t.Error("the upstream's message was discarded entirely rather than recorded scrubbed")
	}
}

// A transport failure does not map the operator's internal network for the
// caller.
//
// The passthrough engine refuses to relay this text, with a comment saying why;
// the dispatch path put the whole url.Error in the client's 502, so a caller
// learned internal hostnames, ports and resolved addresses by sending a request
// to a deployment that happened to be down.
func TestUnreachableUpstreamDoesNotNameInternalHosts(t *testing.T) {
	table := &upstreamTable{
		providers: map[string]*upstream{
			// A host that does not resolve, on a port nobody serves.
			"p1": {name: "p1", baseURL: "http://vllm-a.internal.invalid:8000",
				api: catalog.APIOpenAIChat, kind: "openai"},
		},
		creds: map[string]*credential{"cred-a": {id: "cred-a", provider: "p1"}},
	}
	d := newDispatcher(http.DefaultClient, func(string, ...any) {}, time.Now)
	st := &dispatchState{upstreams: table}
	d.swap(st)

	c := &call{kind: callChat, clientAPI: catalog.APIOpenAIChat, model: "m1",
		body: []byte(`{"model":"m1","messages":[]}`)}
	creq, err := openaiDecodeForTest(c.body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	c.creq = creq
	dec := &router.Decision{Provider: "p1", UpstreamModel: "up-1", Credential: "cred-a"}
	rq := newDecodeRequest(t, string(c.body), &principal{p: &auth.Principal{KeyID: "key-1"}})

	res := d.attempt(context.Background(), st, c, dec, rq, httptest.NewRecorder())
	if res.err == nil {
		t.Fatal("an unreachable upstream produced no error")
	}
	msg := res.err.Error()
	for _, leak := range []string{"vllm-a.internal.invalid", "8000", "dial tcp"} {
		if strings.Contains(msg, leak) {
			t.Errorf("the 502 body names the operator's internal network (%q): %s", leak, msg)
		}
	}
}

// openaiDecodeForTest decodes a chat body into the neutral request, so a test
// can build a *call the dispatcher would have built.
func openaiDecodeForTest(body []byte) (*canonical.Request, error) {
	return openai.DecodeRequest(body)
}

// countingReader records how many bytes a reader was actually asked for. It is
// how a "bounded read" test tells a bound from a post-hoc length check.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// The master credential owns what it creates.
//
// The write half of batch.ownedBy's finding. The master credential has no
// api_keys row, so its KeyID is "" — and principalID wrote that empty string
// into every file and batch it created. batch.ownedBy read an empty owner as
// "everyone's", so a master-credential upload was readable, usable and
// deletable by every `sk-` key in the deployment. Fixing only the read side
// would have left the write side producing rows that need a special rule
// somewhere else forever.
func TestTheMasterCredentialOwnsWhatItCreates(t *testing.T) {
	master := &principal{p: &auth.Principal{Master: true}}
	rq := &server.Request{Principal: master}
	if got := principalID(rq); got != MasterOwnerID {
		t.Fatalf("principalID for the master credential = %q, want %q: an object it "+
			"creates is unowned, and batch.ownedBy used to read unowned as public",
			got, MasterOwnerID)
	}
	// The reserved id cannot be a key id: key ids carry no colon.
	if !strings.Contains(MasterOwnerID, ":") {
		t.Error("the master owner id must be unforgeable as a key id")
	}
	// An ordinary key is unchanged.
	ord := &principal{p: &auth.Principal{KeyID: "key-7"}}
	if got := principalID(&server.Request{Principal: ord}); got != "key-7" {
		t.Errorf("principalID for an ordinary key = %q, want key-7", got)
	}
	// No principal is still "", which creates nothing.
	if got := principalID(&server.Request{}); got != "" {
		t.Errorf("principalID with no principal = %q, want empty", got)
	}
}
