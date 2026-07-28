package admin

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// The shape-compatible responses of DESIGN §2.3 exist so that scripts written
// against another gateway keep working. A script depends on *field names*, so
// renaming one is a breaking change whether or not anyone meant it to be —
// which makes the response bodies a contract, and a contract is worth pinning
// byte for byte.
//
// These files are the contract. A diff here is not a test to be fixed; it is a
// compatibility decision to be made.
func TestGoldenShapeCompatibleResponses(t *testing.T) {
	h := goldenHarness(t)
	// The id of the key minted by the first case, substituted into the later
	// paths. Object ids are minted from the same sequence audit rows are, so
	// hard-coding one here would break every time an extra audited step is
	// added to the fixture.
	var keyID string

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
		status int
	}{
		{"key_generate", http.MethodPost, "/key/generate", map[string]any{
			"key_alias": "ci", "user_id": "user-1", "team_id": "team-1",
			"models": []string{"chat"}, "max_budget": 25, "budget_duration": "monthly",
			"rpm_limit": 600, "tags": []string{"prod"},
		}, 200},
		{"key_info", http.MethodGet, "/key/info?key_id={key_id}", nil, 200},
		{"key_list", http.MethodGet, "/key/list", nil, 200},
		{"user_info", http.MethodGet, "/user/info?user_id=user-1", nil, 200},
		{"user_list", http.MethodGet, "/user/list", nil, 200},
		{"team_info", http.MethodGet, "/team/info?team_id=team-1", nil, 200},
		{"model_info", http.MethodGet, "/model/info", nil, 200},
		{"model_group_info", http.MethodGet, "/model_group/info", nil, 200},
		{"budget_info", http.MethodGet, "/budget/info?budget_id=team:team-1", nil, 200},
		{"budget_list", http.MethodGet, "/budget/list", nil, 200},
		{"spend_logs", http.MethodGet,
			"/spend/logs?start_date=2026-07-25&end_date=2026-07-28&limit=10", nil, 200},
		{"global_spend_report", http.MethodGet,
			"/global/spend/report?start_date=2026-07-25&end_date=2026-07-28&group_by=day,model", nil, 200},
		{"user_daily_activity", http.MethodGet,
			"/user/daily/activity?start_date=2026-07-25&end_date=2026-07-28", nil, 200},
		{"team_daily_activity", http.MethodGet,
			"/team/daily/activity?start_date=2026-07-25&end_date=2026-07-28", nil, 200},
		{"tag_daily_activity", http.MethodGet,
			"/tag/daily/activity?start_date=2026-07-25&end_date=2026-07-28", nil, 200},
		{"spend_calculate", http.MethodPost, "/spend/calculate", map[string]any{
			"model": "chat", "provider": "prov-a", "prompt_tokens": 1000, "completion_tokens": 100,
		}, 200},
		{"health_history", http.MethodGet,
			"/health/history?start_date=2026-07-25&end_date=2026-07-28", nil, 200},

		// The refusals are part of the contract too: a script that branches on
		// a code needs the code to keep its spelling.
		{"error_unbounded_range", http.MethodGet, "/spend/logs", nil, 400},
		{"error_not_implemented", http.MethodPost, "/organization/new", nil, 501},
		{"error_unknown_route", http.MethodPost, "/no/such/route", nil, 501},
		{"error_not_found", http.MethodGet, "/key/info?key_id=absent", nil, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(tc.method, strings.ReplaceAll(tc.path, "{key_id}", keyID), tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d\n%s", rec.Code, tc.status, rec.Body.String())
			}
			body := rec.Body.Bytes()
			if tc.name == "key_generate" {
				keyID = h.decode(rec)["token_id"].(string)
			}
			compareGolden(t, tc.name, body)
		})
	}
}

// The native surface's shape is not inherited from anyone, but the price
// preview is what the admin calculator and the CLI both read (§8.4), so its
// shape is pinned for the same reason.
func TestGoldenNativeResponses(t *testing.T) {
	h := goldenHarness(t)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"admin_pricing_preview", http.MethodPost, "/admin/pricing/preview", map[string]any{
			"model": "model-x", "provider": "plan-a", "credential": "plan-a-1",
			"prompt_tokens": 5000000,
		}},
		{"admin_credentials_health", http.MethodGet, "/admin/credentials/health", nil},
		{"admin_quota", http.MethodGet, "/admin/quota", nil},
		{"admin_capacity", http.MethodGet, "/admin/capacity", nil},
		{"admin_catalog_unverified", http.MethodGet, "/admin/catalog/unverified", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(tc.method, tc.path, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
			}
			compareGolden(t, tc.name, rec.Body.Bytes())
		})
	}
}

// goldenHarness builds a deterministic deployment: a fixed clock, sequential
// ids, and a fixed token generator, so that the only thing a golden file can
// record is the response shape.
func goldenHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, withReporters)

	h.expectStatus(h.do(http.MethodPost, "/user/new", map[string]any{
		"user_id": "user-1", "user_email": "ada@example.test", "user_name": "Ada",
		"user_role": "admin", "max_budget": 100,
	}), http.StatusOK)
	h.expectStatus(h.do(http.MethodPost, "/team/new", map[string]any{
		"team_id": "team-1", "team_name": "platform", "max_budget": 500,
		"budget_duration": "monthly",
	}), http.StatusOK)
	h.expectStatus(h.do(http.MethodPost, "/team/member_add", map[string]any{
		"team_id": "team-1", "user_id": "user-1", "role": "admin",
	}), http.StatusOK)
	h.expectStatus(h.do(http.MethodPost, "/model/new", map[string]any{
		"id": "dep-1", "model_name": "chat",
		"dorang_params": map[string]any{
			"provider": "prov-a", "model": "vendor/model-x:2026-07",
			"weight": 3, "rpm": 600, "credential_ids": []string{"cred-a"},
		},
	}), http.StatusOK)
	h.store.aliases = []Alias{{Alias: "chat-latest", ModelGroup: "chat"}}
	h.expectStatus(h.do(http.MethodPost, "/budget/new", map[string]any{
		"subject_kind": "team", "subject_id": "team-1",
		"max_budget": 500, "budget_duration": "monthly",
	}), http.StatusOK)

	day := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	h.store.addLog(LogRow{
		ID: "req-1", TS: day, APIKeyID: "key-1", UserID: "user-1", TeamID: "team-1",
		CredentialID: "cred-a", ProviderID: "prov-a", DeploymentID: "dep-1",
		ModelGroup: "chat", UpstreamModel: "vendor/model-x:2026-07",
		Endpoint: "/v1/chat/completions", Status: 200,
		PromptTokens: 1000, CompletionTokens: 200, CachedTokens: 100, TotalTokens: 1200,
		CostNano: 660_000_000, SubscriptionCostNano: 660_000_000,
		NotionalNano: 1_190_000_000, NotionalKnown: true,
		LatencyMS: 250, TTFTMS: 90, QueueMS: 3, Streamed: true,
		TraceID: "trace-1", Tags: []string{"prod"},
	})
	h.store.addLog(LogRow{
		ID: "req-2", TS: day.Add(time.Hour), APIKeyID: "key-1", UserID: "user-1",
		TeamID: "team-1", ProviderID: "prov-a", ModelGroup: "chat", Status: 429,
		ErrorClass: "rate_limit", PromptTokens: 20, TotalTokens: 20,
		NotionalKnown: true, Tags: []string{"prod"},
	})

	hist, _ := h.api.cfg.Health.(*MemoryHealthHistory)
	hist.Record(HealthEvent{TS: day.Add(30 * time.Minute), SubjectKind: "credential",
		Subject: "cred-b", State: "unavailable", Reason: "consecutive failures", Status: 503})
	hist.Record(HealthEvent{TS: day.Add(35 * time.Minute), SubjectKind: "credential",
		Subject: "cred-b", State: "healthy", Reason: "probe succeeded", LatencyMS: 120})

	return h
}

func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, got, "", "  "); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, got)
	}
	pretty.WriteByte('\n')

	path := filepath.Join("testdata", name+".json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no golden file (run `go test ./internal/admin -update`): %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(pretty.Bytes())) {
		t.Errorf("response shape changed.\n--- want (%s)\n%s\n--- got\n%s",
			path, want, pretty.String())
	}
}

// A golden file must never contain a live-looking credential. The generator is
// deterministic in tests, but the check exists so that a future change to the
// fixture cannot quietly commit one.
func TestGoldenFilesCarryNoRealSecret(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Skip("no golden files yet")
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("testdata", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			continue
		}
		if v, ok := doc["key"].(string); ok && !bytes.Contains([]byte(v), []byte("test-token")) {
			t.Errorf("%s carries a key that is not an obvious test fixture: %q", e.Name(), v)
		}
	}
}
