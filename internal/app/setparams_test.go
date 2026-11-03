package app

import (
	"encoding/json"
	"github.com/ziozzang/dorang/internal/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Exercise the assembled gateway, not only a helper: privacy policy must be
// present on the upstream HTTP request even when the client tries to weaken it.
func TestProviderSetPolicySurvivesClientOverride(t *testing.T) {
	for _, field := range []string{"require_no_training", "zdr"} {
		t.Run(field, func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				var policy map[string]bool
				if err := json.Unmarshal(body["provider"], &policy); err != nil {
					t.Error(err)
				}
				if len(policy) != 1 || !policy[field] {
					t.Errorf("operator policy missing: %s", body["provider"])
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"test","object":"chat.completion","model":"m1-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
			}))
			defer up.Close()
			a := newWiringApp(t, strings.Replace(wiringYAML, "https://example.invalid", up.URL, 1), func(c *config.Config) {
				c.Providers[0].Params.Set = map[string]any{"provider": map[string]any{field: true}}
			})
			for _, extra := range []string{"", `,"provider":{"zdr":false,"require_no_training":false,"order":["untrusted"]}`} {
				w := callWith(a, testMasterKey, "POST", "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"OK"}]`+extra+`}`)
				if w.Code != 200 {
					t.Fatalf("status %d", w.Code)
				}
			}
			if calls != 2 {
				t.Errorf("upstream calls %d", calls)
			}
		})
	}
}
