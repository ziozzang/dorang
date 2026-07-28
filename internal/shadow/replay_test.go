package shadow

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/server"
)

func TestReplayIsDenyByDefault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		family server.Family
		path   string
		want   bool
	}{
		// The inference surface the comparison exists for.
		{"chat completions", "POST", server.FamilyOpenAIChat, "/v1/chat/completions", true},
		{"embeddings", "POST", server.FamilyOpenAIEmbeddings, "/v1/embeddings", true},
		{"messages", "POST", server.FamilyAnthropicMessages, "/v1/messages", true},
		{"count tokens", "POST", server.FamilyAnthropicCountTokens, "/v1/messages/count_tokens", true},

		// Read-only routes, including three of the eleven T0 paths.
		{"models", "GET", server.FamilyModels, "/v1/models", true},
		{"health", "GET", server.FamilyHealth, "/health/readiness", true},
		{"health preflight", "OPTIONS", server.FamilyHealth, "/health/readiness", true},
		{"unknown route", "GET", server.FamilyNone, "/v1/files", true},

		// The scrape has no counterpart on the reference.
		{"metrics", "GET", server.FamilyMetrics, "/metrics", false},

		// Everything that could change state on the incumbent.
		{"key delete", "DELETE", server.FamilyNone, "/key/delete", false},
		{"key update", "POST", server.FamilyNone, "/key/update", false},
		{"batch create", "POST", server.FamilyNone, "/v1/batches", false},
		{"file upload", "PUT", server.FamilyNone, "/v1/files", false},
		{"patch", "PATCH", server.FamilyNone, "/team/member_update", false},
		// A provider-native passthrough POST can carry a deletion, and the
		// generic engine never parses the body well enough to tell.
		{"passthrough post", "POST", server.FamilyPassthrough, "/anthropic/v1/messages", false},
	} {
		if got := replayable(tc.method, tc.family, tc.path); got != tc.want {
			t.Errorf("%s: replayable(%s %s) = %v, want %v", tc.name, tc.method, tc.path, got, tc.want)
		}
	}
}

func TestUnsafeRequestsAreNotSentAndAreCounted(t *testing.T) {
	var hits int
	var mu sync.Mutex
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(ref.Close)

	s := newTestShadower(t, ref.URL, &syncWriter{}, nil)

	del := observation("delete-1", 200, `{"deleted":true}`)
	del.Method = http.MethodDelete
	del.Path = "/key/delete"
	del.Family = server.FamilyNone
	s.Observe(del)

	st := s.Stats()
	if st.SkippedUnsafe != 1 {
		t.Fatalf("SkippedUnsafe = %d, want 1", st.SkippedUnsafe)
	}
	if st.Queued != 0 {
		t.Fatal("a DELETE must never be replayed against the reference")
	}
	if st.SpentNanoUSD != 0 {
		t.Errorf("a refused replay must not reserve budget, spent = %d", st.SpentNanoUSD)
	}
	// Give a worker a chance to be wrong.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("the reference received %d calls it must never have seen", hits)
	}

	// And the number is where an operator will see it.
	if !strings.Contains(string(s.Metrics(nil)), "dorang_shadow_skipped_unsafe_total 1") {
		t.Error("the skipped count is not on the metrics endpoint")
	}
	if !strings.Contains(string(s.Health(nil)), `"skipped_unsafe":1`) { // pragma: allowlist secret — test fixture
		t.Error("the skipped count is not in the health body, next to the verdict")
	}
}

func TestSettlementOverTheCeilingStopsShadowing(t *testing.T) {
	// The estimate is dorang's own price for the same request; the reference
	// may simply be dearer. The ceiling has to hold anyway.
	now := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	b := newDayBudget(100, now)
	if !b.reserve(now, 50) {
		t.Fatal("reservation should fit")
	}
	b.settle(now, 50, 150) // the reference charged three times the estimate
	if !b.capped(now) {
		t.Fatal("settling past the ceiling must stop shadowing, not merely be recorded")
	}
}
