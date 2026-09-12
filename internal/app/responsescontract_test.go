package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ziozzang/dorang/internal/config"
)

// The Responses-only contract settings are refused on a kind whose adapter
// never reads them, and accepted on the kind the catalog declares responses_only.
//
// Both halves are one test because they are one wiring: app copies the catalog
// kind's `responses_only` into the provider Spec, and NewProvider decides from
// that. Without the copy the flag is false everywhere, the settings are refused
// on the codex kind too, and the refusal test alone would still pass.
func TestTheResponsesContractSettingsFollowTheCatalogFlag(t *testing.T) {
	assemble := func(t *testing.T, kind string) error {
		t.Helper()
		isolateState(t)
		t.Setenv("DORANG_APP_TEST_KEY", testUpstreamKey)
		cfg, err := config.LoadBytes([]byte(`
version: 1
providers:
  - name: p1
    kind: ` + kind + `
    base_url: "https://example.invalid"
    params: {force_stream: true, store_false: true}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - provider: p1
        upstream_model: gpt-5.5
        credentials: [c1]
`))
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		cfg.Storage.SQLite.Path = filepath.Join(t.TempDir(), "dorang.db")
		t.Setenv(cfg.Server.KeyPepperEnv, testPepper)
		t.Setenv(cfg.Server.MasterKeyEnv, testMasterKey)
		a, err := New(context.Background(), Options{Config: cfg})
		if a != nil {
			t.Cleanup(func() { _ = a.Close(context.Background()) })
		}
		return err
	}

	t.Run("refused on a chat host", func(t *testing.T) {
		err := assemble(t, "openai")
		if err == nil {
			t.Fatal("force_stream on an openai provider assembled cleanly; the setting loads " +
				"and changes nothing")
		}
		if !strings.Contains(err.Error(), "force_stream") || !strings.Contains(err.Error(), `"p1"`) {
			t.Errorf("the refusal names neither the key nor the provider: %v", err)
		}
	})

	t.Run("accepted on the kind the catalog declares responses_only", func(t *testing.T) {
		// `codex` is the alias the catalog resolves to codex-responses, so this
		// also covers the alias path an operator's file actually takes.
		if err := assemble(t, "codex"); err != nil {
			t.Fatalf("the settings were refused on the one kind they are for, so the catalog's "+
				"responses_only never reached the provider: %v", err)
		}
	})
}

// codexHost is a fake enforcing the Codex contract: /responses only, store
// false and stream true or a 400, and an event stream answering "yes". It
// records every body it received, parsed.
func codexHost(t *testing.T) (*httptest.Server, func() []codexSeen) {
	t.Helper()
	var mu sync.Mutex
	var got []codexSeen
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		got = append(got, codexSeen{r.URL.Path, body})
		mu.Unlock()
		if r.URL.Path != "/responses" {
			http.Error(w, `{"detail":"no such route"}`, http.StatusForbidden)
			return
		}
		if s, ok := body["store"].(bool); !ok || s {
			http.Error(w, `{"detail":"Store must be set to false"}`, http.StatusBadRequest)
			return
		}
		if s, _ := body["stream"].(bool); !s {
			http.Error(w, `{"detail":"Stream must be set to true"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"gpt-5.5\"}}\n\n" +
			"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"yes\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"))
	}))
	t.Cleanup(up.Close)
	return up, func() []codexSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]codexSeen(nil), got...)
	}
}

type codexSeen struct {
	path string
	body map[string]any
}

// codexYAML is one codex provider at the given host, with the contract stated.
func codexYAML(url string) string {
	return `
version: 1
providers:
  - name: p1
    kind: codex
    base_url: "` + url + `"
    params: {force_stream: true, store_false: true, drop: [max_tokens]}
credentials:
  - {id: c1, provider: p1, key_env: DORANG_APP_TEST_KEY}
models:
  - name: m1
    deployments:
      - provider: p1
        upstream_model: gpt-5.5
        credentials: [c1]
`
}

// The contract reaches the wire from an operator's file.
//
// The backend tests hand NewProvider a Spec directly, so they prove the adapter
// and not the copy from `providers[].params` into it: a build that dropped the
// `store_false` line in app/upstream.go would pass every one of them and send
// `store` unset to a host that refuses exactly that. This drives an
// app-assembled provider against a host enforcing the Codex contract and reads
// the bytes it received.
func TestTheContractSettingsReachTheWireFromTheFile(t *testing.T) {
	up, seen := codexHost(t)
	a := newWiringApp(t, codexYAML(up.URL), nil, func(o *Options) { o.Upstream = up.Client() })
	key := issueKey(t, a, nil)
	w := callWith(a, key, http.MethodPost, "/v1/chat/completions",
		`{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":"go"}]}`)
	got := seen()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the host saw %+v\n%s", w.Code, got, w.Body.String())
	}
	if len(got) != 1 || got[0].path != "/responses" {
		t.Fatalf("host saw %+v; want exactly one request at /responses", got)
	}
	if v, ok := got[0].body["store"].(bool); !ok || v {
		t.Errorf("store=%v on the wire; params.store_false did not reach the adapter", got[0].body["store"])
	}
	if v, _ := got[0].body["stream"].(bool); !v {
		t.Errorf("stream=%v on the wire; params.force_stream did not reach the adapter", got[0].body["stream"])
	}
	if !strings.Contains(w.Body.String(), `"content":"yes"`) {
		t.Errorf("the collected answer did not reach the caller:\n%s", w.Body.String())
	}
}

// A dorang continuation id is resolved here and never sent upstream.
//
// `previous_response_id` names an exchange dorang stored. The history it
// stands for is replayed into the request; the id itself means nothing to any
// upstream, and a host that resolves references would 404 on it — or, worse,
// chain a second time on top of the replay. The Responses encoder is the one
// that carries the field to the wire, so the host here speaks that shape: a
// chat-shaped host never sends it and would prove nothing.
func TestALocalContinuationIDIsNotSentUpstream(t *testing.T) {
	up, seen := codexHost(t)
	a := newWiringApp(t, codexYAML(up.URL), nil, func(o *Options) { o.Upstream = up.Client() })
	key := issueKey(t, a, nil)

	first := callWith(a, key, http.MethodPost, "/v1/responses", `{"model":"m1","input":"first"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d\n%s", first.Code, first.Body.String())
	}
	var made struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &made); err != nil || made.ID == "" {
		t.Fatalf("no id on the first answer: %v\n%s", err, first.Body.String())
	}

	second := callWith(a, key, http.MethodPost, "/v1/responses",
		`{"model":"m1","input":"second","previous_response_id":"`+made.ID+`"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second: %d\n%s", second.Code, second.Body.String())
	}
	got := seen()
	if len(got) != 2 {
		t.Fatalf("host saw %d requests, want 2", len(got))
	}
	body := got[1].body
	if v, ok := body["previous_response_id"]; ok && v != nil {
		t.Errorf("dorang's own id reached the upstream: %v", v)
	}
	raw, _ := json.Marshal(body["input"])
	if !strings.Contains(string(raw), "yes") || !strings.Contains(string(raw), "first") {
		t.Errorf("the stored turn was not replayed into the request: %s", raw)
	}
	if !strings.Contains(second.Body.String(), made.ID) {
		t.Errorf("the caller's reference was not echoed on the answer:\n%s", second.Body.String())
	}
	// The stored row still records which exchange this one continued: the
	// reference was removed from the UPSTREAM request, not from dorang's own
	// record of the conversation.
	var second2 struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(second.Body.Bytes(), &second2)
	row, err := a.Store.GetStoredResponse(context.Background(), second2.ID, "key-"+key)
	if err != nil {
		t.Fatalf("the second exchange was not stored: %v", err)
	}
	if row.PreviousResponseID != made.ID {
		t.Errorf("stored previous_response_id = %q, want %q: the parent reference was lost with the "+
			"upstream field", row.PreviousResponseID, made.ID)
	}
}
