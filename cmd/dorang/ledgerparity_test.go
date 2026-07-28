package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/store"
)

// The upstream's own accounting for the equivalence test.
//
// Every breakdown field is a SUBSET, which is what makes the two rules
// distinguishable: 40 of the 100 prompt tokens came from cache and 10 more were
// written to it, and 8 of the 20 completion tokens were reasoning. The total the
// client is owed is 100 + 20 = 120. Summing all five fields — the rule the
// ledger used — gives 178.
const (
	equivInput     = 100
	equivOutput    = 20
	equivCacheRead = 40
	equivCacheWrit = 10
	equivReasoning = 8

	// equivWireTotal is what the response body must say, and therefore what the
	// ledger row must say.
	equivWireTotal = equivInput + equivOutput
	// equivDoubleCounted is what the old rule produced. It is spelled out so a
	// failure names the defect rather than just the delta.
	equivDoubleCounted = equivInput + equivOutput + equivCacheRead + equivCacheWrit + equivReasoning
)

const equivUpstreamAnswer = `{"id":"chatcmpl-equiv","object":"chat.completion","created":1700000000,` +
	`"model":"upstream-x","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},` +
	`"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,` +
	`"prompt_tokens_details":{"cached_tokens":40},` +
	`"completion_tokens_details":{"reasoning_tokens":8},` +
	`"cache_creation_input_tokens":10}}`

// TestLedgerTotalTokensEqualsTheAnswerTheClientGot is the equivalence C1 is
// about, asserted the only way that can settle it: the number in the ledger row
// against the number in the response body of the SAME request.
//
// Comparing two functions to each other would have passed throughout the defect.
// There were two definitions of "total tokens" in the binary — canonical.Usage's
// input+output on the wire, and a sum of all five breakdown fields in
// meter.Tokens — and each was internally consistent. What was not consistent was
// dorang's answer against dorang's bill: the wire said 120 and /spend/logs said
// 128 for one request, and a live session's rollup read 30,929 against 30,355.
// The over-count grows with the cache and reasoning fraction, which is to say
// with exactly the workloads a gateway is deployed to aggregate, and it is
// silent and in the over-reporting direction.
//
// It also pins C3 and C4 on the same request: deployment_id against the header
// that carries it, and streamed against a turn that was not a stream.
func TestLedgerTotalTokensEqualsTheAnswerTheClientGot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "equivalence-pepper")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, equivUpstreamAnswer)
	}))
	defer up.Close()

	ctx := context.Background()
	a, err := app.New(ctx, app.Options{Config: loadRoundTripConfig(t, dir, up.URL), Logf: t.Logf})
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

	// 1. What the client was sent, read out of the body it was sent.
	var answer struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			PromptDetails    struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(resp.body), &answer); err != nil {
		t.Fatalf("decoding the response: %v\n%s", err, resp.body)
	}
	u := answer.Usage
	// The fixture is only meaningful if the counts that make the two rules
	// differ actually survived to the client. Without them this test would pass
	// against either rule.
	if u.PromptDetails.CachedTokens != equivCacheRead {
		t.Fatalf("the response carried cached_tokens = %d, want %d: the fixture no "+
			"longer distinguishes the two rules", u.PromptDetails.CachedTokens, equivCacheRead)
	}
	if u.CompletionDetails.ReasoningTokens != equivReasoning {
		t.Fatalf("the response carried reasoning_tokens = %d, want %d",
			u.CompletionDetails.ReasoningTokens, equivReasoning)
	}
	if u.TotalTokens != equivWireTotal {
		t.Fatalf("the response's own total_tokens = %d, want %d", u.TotalTokens, equivWireTotal)
	}

	deployment := resp.header.Get("X-Dorang-Deployment")
	if deployment == "" {
		t.Fatal("no x-dorang-deployment on the response, so the ledger column proves nothing")
	}
	requestID := resp.header.Get("X-Dorang-Request-Id")

	// 2. What the ledger recorded for that same request.
	row := awaitLedgerRow(t, ctx, a, token)
	if row.ID != requestID {
		t.Fatalf("ledger row id = %q, want the response's request id %q", row.ID, requestID)
	}

	if row.TotalTokens != u.TotalTokens {
		extra := ""
		if row.TotalTokens == equivDoubleCounted {
			extra = " — which is input+output+cache_read+cache_write+reasoning, " +
				"the sum that counts the cached prefix and the reasoning tokens twice"
		}
		t.Errorf("ledger total_tokens = %d, response total_tokens = %d%s\n"+
			"dorang told the customer one number and billed against another",
			row.TotalTokens, u.TotalTokens, extra)
	}
	// The breakdown columns were always individually right; this keeps them so,
	// and keeps the row internally addable.
	if row.PromptTokens != u.PromptTokens || row.CompletionTokens != u.CompletionTokens {
		t.Errorf("ledger prompt/completion = %d/%d, response = %d/%d",
			row.PromptTokens, row.CompletionTokens, u.PromptTokens, u.CompletionTokens)
	}
	if row.CachedTokens != equivCacheRead || row.ReasoningTokens != equivReasoning {
		t.Errorf("ledger cached/reasoning = %d/%d, want %d/%d",
			row.CachedTokens, row.ReasoningTokens, equivCacheRead, equivReasoning)
	}
	if row.PromptTokens+row.CompletionTokens != row.TotalTokens {
		t.Errorf("the ledger row does not add up: %d + %d != %d",
			row.PromptTokens, row.CompletionTokens, row.TotalTokens)
	}

	// 3. C3: deployment_id, against the header that carries the same value.
	if row.DeploymentID != deployment {
		t.Errorf("ledger deployment_id = %q, x-dorang-deployment = %q: per-deployment "+
			"attribution is in the header and not in the row, which is the worst of "+
			"the two arrangements — an operator who checks the header believes it was recorded",
			row.DeploymentID, deployment)
	}

	// 4. C4, negative half: this turn was not a stream.
	if row.Streamed {
		t.Error("ledger streamed = true for a complete JSON answer")
	}
}

// TestStreamedRequestIsRecordedAsStreamed is C4's positive half, against a turn
// that definitively streamed: the response is an event stream and the client
// read it frame by frame. Every row said false before, including this one.
//
// It asserts the C1 equivalence on the streaming path too. A wire/ledger split
// that exists only when the answer streams is this project's recurring shape,
// and a non-streaming assertion alone would not see it.
func TestStreamedRequestIsRecordedAsStreamed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "equivalence-pepper")

	const frames = "data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"upstream-x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"po\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"upstream-x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ng\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"chatcmpl-s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"upstream-x\",\"choices\":[]," +
		"\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120," +
		"\"prompt_tokens_details\":{\"cached_tokens\":40}," +
		"\"completion_tokens_details\":{\"reasoning_tokens\":8}}}\n\n" +
		"data: [DONE]\n\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frames)
	}))
	defer up.Close()

	ctx := context.Background()
	a, err := app.New(ctx, app.Options{Config: loadRoundTripConfig(t, dir, up.URL), Logf: t.Logf})
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
		t.Fatalf("content-type = %q: this turn did not stream, so it proves nothing", ct)
	}
	wireTotal := totalFromUsageFrame(t, resp.body)

	row := awaitLedgerRow(t, ctx, a, token)
	if !row.Streamed {
		t.Error("ledger streamed = false for a turn the client read as an event stream: " +
			"the column has a writer, a reader and a JSON name, and had no producer")
	}
	if row.TotalTokens != wireTotal {
		t.Errorf("ledger total_tokens = %d, the stream's own usage frame said %d",
			row.TotalTokens, wireTotal)
	}
	if got := resp.header.Get("X-Dorang-Deployment"); got != row.DeploymentID {
		t.Errorf("ledger deployment_id = %q, x-dorang-deployment = %q", row.DeploymentID, got)
	}
}

// totalFromUsageFrame reads total_tokens out of the last frame that carries a
// usage object — the number the streaming client actually saw.
func totalFromUsageFrame(t *testing.T, sse string) int64 {
	t.Helper()
	var total int64 = -1
	for _, line := range strings.Split(sse, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var frame struct {
			Usage *struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue
		}
		if frame.Usage != nil {
			total = frame.Usage.TotalTokens
		}
	}
	if total < 0 {
		t.Fatalf("no usage frame in the stream, so there is nothing to reconcile against:\n%s", sse)
	}
	return total
}

// TestConfiguredBodyCapIsHonoured is A1: the 32 MiB cap was not configurable at
// all — no schema key existed and internal/app never set the option — while the
// refusal named `max_body_bytes` as though it were a setting. A client posting a
// larger body than the incumbent accepted had no remedy short of a rebuild.
//
// Both directions are asserted from one body, because only the pair proves the
// setting is what decided: the same bytes are refused under the small cap and
// served under the large one.
func TestConfiguredBodyCapIsHonoured(t *testing.T) {
	const (
		small = "  max_body_bytes: 4KiB"
		large = "  max_body_bytes: 1MiB"
	)
	// Comfortably over 4 KiB and comfortably under 1 MiB.
	padding := strings.Repeat("x", 16<<10)
	body := `{"model":"model-x","messages":[{"role":"user","content":"` + padding + `"}]}`

	for _, c := range []struct {
		name, extra string
		want        int
	}{
		{"under the configured cap", large, http.StatusOK},
		{"over the configured cap", small, http.StatusRequestEntityTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("DORANG_KEY_PEPPER", "body-cap-pepper")

			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, upstreamAnswer)
			}))
			defer up.Close()

			ctx := context.Background()
			cfg := loadRoundTripConfig(t, dir, up.URL, c.extra)
			a, err := app.New(ctx, app.Options{Config: cfg, Logf: t.Logf})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close(ctx)

			token := issueKey(t, ctx, a.Store)
			front := httptest.NewServer(a.Server)
			defer front.Close()

			resp, err := postErr(front.URL+"/v1/chat/completions", token, body, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if resp.status != c.want {
				t.Fatalf("status = %d, want %d: the configured cap did not decide\nbody = %s",
					resp.status, c.want, resp.body)
			}
			if c.want == http.StatusRequestEntityTooLarge &&
				!strings.Contains(resp.body, "max_body_bytes") {
				t.Errorf("the refusal does not name the setting: %s", resp.body)
			}
		})
	}
}

// awaitLedgerRow flushes the meter until the request's row is durable and
// returns it. Metering is asynchronous by design (§9.1), so the flush is driven
// rather than waited on.
func awaitLedgerRow(t *testing.T, ctx context.Context, a *app.App, token string) store.RequestLog {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
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
			return page.Rows[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("no ledger row was ever written")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
