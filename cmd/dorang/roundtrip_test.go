package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/store"
)

// TestRoundTripThroughTheAssembledStack is the assembly's real test: a request
// enters the HTTP surface, passes the authenticator, is converted by the wire
// adapter, routed, admitted by the capacity broker, sent to a fake upstream,
// converted back, priced and metered — with every one of those being the real
// implementation and only the provider being fake.
//
// It asserts three things that each fail differently when the wiring is wrong:
// the response body byte for byte, a ledger row, and a non-zero cost.
func TestRoundTripThroughTheAssembledStack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "round-trip-pepper")

	var upstreamRequests int
	var sawAuth, sawModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		sawAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe)
		sawModel = probe.Model
		// A client credential must never reach a provider (COMPATIBILITY §7.3).
		for _, h := range []string{"X-Api-Key", "Api-Key", "X-Dorang-Api-Key"} {
			if r.Header.Get(h) != "" {
				t.Errorf("client header %s reached the upstream", h)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamAnswer)
	}))
	defer up.Close()

	cfg := loadRoundTripConfig(t, dir, up.URL)
	ctx := context.Background()
	a, err := app.New(ctx, app.Options{Config: cfg, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)

	token := issueKey(t, ctx, a.Store)
	front := httptest.NewServer(a.Server)
	defer front.Close()

	resp, err := postErr(front.URL+"/v1/chat/completions", token, chatRequest, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.status, resp.body)
	}

	// 1. The upstream saw the deployment's real model id and dorang's own
	//    provider credential, not the caller's.
	if upstreamRequests != 1 {
		t.Errorf("upstream saw %d requests, want 1", upstreamRequests)
	}
	if sawModel != "upstream-x" {
		t.Errorf("upstream model = %q, want upstream-x (§7.2)", sawModel)
	}
	if sawAuth != "Bearer upstream-secret" { // pragma: allowlist secret — test fixture
		t.Errorf("upstream credential = %q, want the provider's own", sawAuth)
	}

	// 2. The response body, byte for byte. It is the neutral form re-encoded
	//    for the client's protocol with the requested name restored, so every
	//    field the exchange carried has to survive and the upstream id must not.
	const want = `{"id":"chatcmpl-fake","object":"chat.completion","created":1700000000,` +
		`"model":"model-x","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},` +
		`"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`
	if resp.body != want {
		t.Errorf("response body is not byte-correct\n got: %s\nwant: %s", resp.body, want)
	}

	// 3. The extension headers carry the routing decision and the cost.
	if got := resp.header.Get("X-Dorang-Provider"); got != "fake" {
		t.Errorf("x-dorang-provider = %q", got)
	}
	if got := resp.header.Get("X-Dorang-Credential"); got != "fake-1" {
		t.Errorf("x-dorang-credential = %q", got)
	}
	if got := resp.header.Get("X-Dorang-Tokens-Input"); got != "11" { // pragma: allowlist secret — test fixture
		t.Errorf("x-dorang-tokens-input = %q, want 11", got)
	}
	costHeader := resp.header.Get("X-Dorang-Cost-Usd")
	if costHeader == "" || strings.HasPrefix(costHeader, "0.000000000") {
		t.Errorf("x-dorang-cost-usd = %q, want a non-zero cost", costHeader)
	}
	requestID := resp.header.Get("X-Dorang-Request-Id")
	if requestID == "" {
		t.Fatal("no request id; the ledger cannot be joined against")
	}

	// 4. The ledger. Metering is asynchronous by design (§9.1), so the flush is
	//    driven explicitly rather than waited on.
	flushDeadline := time.Now().Add(10 * time.Second)
	var row store.RequestLog
	for {
		if err := a.Meter.Flush(ctx); err != nil {
			t.Fatalf("meter flush: %v", err)
		}
		page, err := a.Store.ListRequestsByKey(ctx, keyIDOf(t, ctx, a.Store, token),
			store.TimeRange{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour)},
			store.Page{Limit: 10})
		if err != nil {
			t.Fatalf("ledger query: %v", err)
		}
		if len(page.Rows) > 0 {
			row = page.Rows[0]
			break
		}
		if time.Now().After(flushDeadline) {
			t.Fatal("no ledger row was ever written")
		}
		time.Sleep(25 * time.Millisecond)
	}

	if row.ID != requestID {
		t.Errorf("ledger row id = %q, want the response's request id %q", row.ID, requestID)
	}
	if row.ModelGroup != "model-x" {
		t.Errorf("ledger model_group = %q", row.ModelGroup)
	}
	if row.UpstreamModel != "upstream-x" {
		t.Errorf("ledger upstream_model = %q", row.UpstreamModel)
	}
	if row.ProviderID != "fake" || row.CredentialID != "fake-1" {
		t.Errorf("ledger provider/credential = %q/%q", row.ProviderID, row.CredentialID)
	}
	if row.PromptTokens != 11 || row.CompletionTokens != 3 || row.TotalTokens != 14 {
		t.Errorf("ledger tokens = %d/%d/%d, want 11/3/14",
			row.PromptTokens, row.CompletionTokens, row.TotalTokens)
	}

	// 5. The cost. §8.3's whole point is that this is not silently zero:
	//    11 input tokens at $3.00 per 1M plus 3 output tokens at $15.00 per 1M
	//    is 33 000 + 45 000 = 78 000 nano-USD, exactly.
	const wantCost = 11*3_000 + 3*15_000
	if row.CostNano != wantCost {
		t.Errorf("ledger cost = %d nano-USD, want %d", row.CostNano, wantCost)
	}

	// 6. The capacity reservation was released. A leaked reservation is
	//    invisible until the axis saturates, so it is asserted here instead.
	snap := a.Broker.Snapshot()
	for _, axis := range snap.Axes {
		if axis.InUse != 0 {
			t.Errorf("axis %s/%s still holds %d reservation(s) after the request finished",
				axis.Axis, axis.Key, axis.InUse)
		}
	}
}

// TestRoundTripStreamRelay covers the other relay path: an event stream is
// forwarded frame by frame with the model name rewritten, and the terminal usage
// frame is still read for accounting.
func TestRoundTripStreamRelay(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "round-trip-pepper")

	const frames = "data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"upstream-x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"po\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"upstream-x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ng\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"upstream-x\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frames)
	}))
	defer up.Close()

	cfg := loadRoundTripConfig(t, dir, up.URL)
	ctx := context.Background()
	a, err := app.New(ctx, app.Options{Config: cfg, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)

	token := issueKey(t, ctx, a.Store)
	front := httptest.NewServer(a.Server)
	defer front.Close()

	body := `{"model":"model-x","stream":true,"stream_options":{"include_usage":true},` +
		`"messages":[{"role":"user","content":"ping"}]}`
	resp, err := postErr(front.URL+"/v1/chat/completions", token, body, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.status, resp.body)
	}
	if ct := resp.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want an event stream", ct)
	}
	// COMPATIBILITY §2.5: the client-facing name is on EVERY chunk.
	if strings.Contains(resp.body, "upstream-x") {
		t.Errorf("a chunk leaked the upstream model id:\n%s", resp.body)
	}
	if n := strings.Count(resp.body, `"model-x"`); n != 3 {
		t.Errorf("the client-facing name appears on %d chunk(s), want 3:\n%s", n, resp.body)
	}
	if !strings.Contains(resp.body, "data: [DONE]") {
		t.Errorf("the terminal frame did not survive the relay:\n%s", resp.body)
	}
	if !strings.Contains(resp.body, `"content":"po"`) || !strings.Contains(resp.body, `"content":"ng"`) {
		t.Errorf("content frames were lost:\n%s", resp.body)
	}
}

// TestRoundTripResponsesStream: the streamed Responses surface through the
// assembled stack. The event tree is synthesized from a chat upstream's
// chunks, the identity is dorang's own, and — the part only this height can
// see — the id the created event pinned resolves by GET to the object the
// completed event carried, because the store ran off the terminal.
func TestRoundTripResponsesStream(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "round-trip-pepper")

	const frames = "data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"upstream-x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"re\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"upstream-x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"sponse\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"upstream-x\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frames)
	}))
	defer up.Close()

	cfg := loadRoundTripConfig(t, dir, up.URL)
	ctx := context.Background()
	a, err := app.New(ctx, app.Options{Config: cfg, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)

	token := issueKey(t, ctx, a.Store)
	front := httptest.NewServer(a.Server)
	defer front.Close()

	body := `{"model":"model-x","stream":true,"store":true,"input":"ping"}`
	resp, err := postErr(front.URL+"/v1/responses", token, body, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.status, resp.body)
	}
	if ct := resp.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want an event stream", ct)
	}
	// The tree, in order: the created object opens the stream, the deltas
	// carry the text, the completed object closes it. No chat terminator and
	// no upstream identity leak.
	if !strings.HasPrefix(resp.body, "event: response.created\n") {
		t.Errorf("the stream does not open with response.created:\n%s", resp.body)
	}
	if !strings.Contains(resp.body, "event: response.output_text.delta") ||
		!strings.Contains(resp.body, "response") {
		t.Errorf("the text did not stream:\n%s", resp.body)
	}
	if !strings.Contains(resp.body, "event: response.completed") {
		t.Errorf("the stream has no terminal:\n%s", resp.body)
	}
	if strings.Contains(resp.body, "[DONE]") || strings.Contains(resp.body, "upstream-x") ||
		strings.Contains(resp.body, "chatcmpl-") {
		t.Errorf("the chat upstream's shapes leaked into a responses stream:\n%s", resp.body)
	}
	if !strings.Contains(resp.body, `"model":"model-x"`) {
		t.Errorf("the client-facing model must appear on the response objects:\n%s", resp.body)
	}

	// The store: the pinned id resolves to the terminal object. This is the
	// streamed spelling of the buffered invariant — a reference the client
	// holds must resolve to the turn it saw.
	id := resp.header.Get("X-Dorang-Request-Id")
	if id == "" {
		t.Fatal("no request id to key the response id from")
	}
	greq, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		front.URL+"/v1/responses/resp_"+id, nil)
	greq.Header.Set("Authorization", "Bearer "+token)
	gresp, err := http.DefaultClient.Do(greq)
	if err != nil {
		t.Fatal(err)
	}
	defer gresp.Body.Close()
	gbody, _ := io.ReadAll(gresp.Body)
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("GET stored response = %d: %s", gresp.StatusCode, gbody)
	}
	var stored struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(gbody, &stored); err != nil {
		t.Fatalf("stored body: %v: %s", err, gbody)
	}
	if stored.ID != "resp_"+id || stored.Status != "completed" {
		t.Errorf("stored = %s/%s, want resp_%s/completed", stored.ID, stored.Status, id)
	}
	if len(stored.Output) == 0 || len(stored.Output[0].Content) == 0 ||
		stored.Output[0].Content[0].Text != "response" {
		t.Errorf("stored output does not carry the answer:\n%s", gbody)
	}
}

// loadRoundTripConfig writes and loads a configuration whose only fake part is
// the provider's address.
//
// serverExtra is spliced into the `server:` block verbatim, for tests that need
// one more key there than the shared fixture carries. Each entry must already be
// indented and newline-terminated.
func loadRoundTripConfig(t *testing.T, dir, upstreamURL string, serverExtra ...string) *config.Config {
	t.Helper()
	src := fmt.Sprintf(`version: 1
server:
  listen: 127.0.0.1:0
  env: development
%s
storage:
  driver: sqlite
  sqlite:
    path: %s/dorang.db
metering:
  flush_interval: 20ms
  spool:
    dir: %s/spool
providers:
  - name: fake
    kind: openai
    base_url: %s/v1
    max_concurrency: 4
    capacity_group: fake-pool
credentials:
  - id: fake-1
    provider: fake
    key: upstream-secret
    capacity_group: fake-account
capacity:
  provider_groups:
    fake-pool: {max_concurrency: 8}
  credential_groups:
    fake-account: {max_concurrency: 4}
  models:
    - {provider: fake, model: upstream-x, max_concurrency: 4}
  global: {max_concurrency: 16}
classes:
  chat: [model-x]
models:
  - name: model-x
    class: chat
    strategy: [least_busy]
    deployments:
      - provider: fake
        upstream_model: upstream-x
        credentials: [fake-1]
pricing:
  currency: USD
  rules:
    - id: fake-tokens
      class: marginal_usage
      match: {provider: fake}
      rates: {input: "3.00", output: "15.00"}
`, strings.Join(serverExtra, ""), dir, dir, upstreamURL)

	path := filepath.Join(dir, "roundtrip.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("configuration:\n%v", err)
	}
	return cfg
}

// issueKey inserts a credential the way `dorangctl key create` does and returns
// the token.
func issueKey(t *testing.T, ctx context.Context, st *store.Store) string {
	t.Helper()
	const token = "sk-roundtrip-token" // pragma: allowlist secret — test fixture
	k := &store.APIKey{KeyAlias: "round-trip"}
	if err := st.NewAPIKeyFromToken(token, k); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	return token
}

func keyIDOf(t *testing.T, ctx context.Context, st *store.Store, token string) string {
	t.Helper()
	k, err := st.GetAPIKeyByLookup(ctx, store.KeyLookup(token))
	if err != nil {
		t.Fatal(err)
	}
	return k.ID
}
