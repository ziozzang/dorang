package app

import (
	"context"
	"fmt"
	"github.com/ziozzang/dorang/internal/config"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func accountCapacityYAML(url, wait string) string {
	return fmt.Sprintf(`version: 1
capacity:
  principals:
    default: {max_concurrent: 32, max_queue: 10, max_queue_wait: %s}
  credential_groups:
    account1: {max_concurrency: 3, max_queue: 10}
    account2: {max_concurrency: 3, max_queue: 10}
providers:
  - {name: ollama, kind: openai, base_url: %q}
credentials:
  - {id: account1, provider: ollama, capacity_group: account1, key_env: DORANG_APP_TEST_KEY}
  - {id: account2, provider: ollama, capacity_group: account2, key_env: DORANG_SECOND_TEST_KEY}
models:
  - name: m1
    strategy: [priority]
    deployments:
      - {provider: ollama, upstream_model: m1, credentials: [account1]}
      - {provider: ollama, upstream_model: m1, credentials: [account2]}
  - name: m2
    strategy: [priority]
    deployments:
      - {provider: ollama, upstream_model: m2, credentials: [account1]}
      - {provider: ollama, upstream_model: m2, credentials: [account2]}
`, wait, url)
}

func waitCapacity(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("capacity condition did not become true")
}

const capacityAnswer = `{"id":"1","object":"chat.completion","created":1,"model":"m1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func TestTenRequestsShareTwoAccountsAcrossModels(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) { tenRequestsShareAccounts(t, stream) })
	}
}

func tenRequestsShareAccounts(t *testing.T, stream bool, cfgMut ...func(*config.Config)) {
	t.Setenv("DORANG_SECOND_TEST_KEY", "second-account-test-key")
	var active, peak, calls [2]atomic.Int64
	release := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var once [2]sync.Once
	unblock := func(i int) { once[i].Do(func() { close(release[i]) }) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		i := 0
		if strings.Contains(r.Header.Get("Authorization"), "second-account-test-key") {
			i = 1
		}
		n := active[i].Add(1)
		defer active[i].Add(-1)
		for old := peak[i].Load(); n > old; old = peak[i].Load() {
			if peak[i].CompareAndSwap(old, n) {
				break
			}
		}
		calls[i].Add(1)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
			w.(http.Flusher).Flush()
		}
		<-release[i]
		if stream {
			fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, capacityAnswer)
		}
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { unblock(0); unblock(1) })
	var mut func(*config.Config)
	if len(cfgMut) > 0 {
		mut = cfgMut[0]
	}
	yaml := accountCapacityYAML(up.URL, "2s")
	// Production has a lower-priority provider too. Saturation must wait for
	// an account in the preferred pool instead of sending these four elsewhere.
	yaml = strings.Replace(yaml, "credentials:\n", fmt.Sprintf("  - {name: fallback, kind: openai, base_url: %q}\ncredentials:\n", up.URL), 1)
	yaml = strings.Replace(yaml, "models:\n", "  - {id: fallback, provider: fallback, key_env: DORANG_APP_TEST_KEY}\nmodels:\n", 1)
	for _, model := range []string{"m1", "m2"} {
		line := fmt.Sprintf("      - {provider: ollama, upstream_model: %s, credentials: [account2]}", model)
		yaml = strings.Replace(yaml, line, line+fmt.Sprintf("\n      - {provider: fallback, upstream_model: %s, credentials: [fallback], priority: 1}", model), 1)
	}
	a := newWiringApp(t, yaml, mut, func(o *Options) { o.Upstream = up.Client() })
	key := issueKey(t, a, nil)
	done := make(chan *httptest.ResponseRecorder, 10)
	for i := 0; i < 10; i++ {
		go func(i int) {
			done <- callWith(a, key, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(`{"model":"m%d","stream":%v,"messages":[{"role":"user","content":"go"}]}`, 1+i%2, stream))
		}(i)
	}
	waitCapacity(t, func() bool { return active[0].Load() == 3 && active[1].Load() == 3 && a.Broker.Snapshot().Waiting == 4 })
	unblock(1)
	// All seven requests served by account 2 finish while account 1 holds its three.
	for i := 0; i < 7; i++ {
		select {
		case w := <-done:
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("queued requests did not spill to the free account")
		}
	}
	// Account 1 is still blocked; let its requests finish.
	unblock(0)
	for i := 0; i < 3; i++ {
		w := <-done
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
	if calls[0].Load() != 3 || calls[1].Load() != 7 {
		t.Fatalf("account requests %d, %d; want 3, 7", calls[0].Load(), calls[1].Load())
	}
	for i := range peak {
		if peak[i].Load() != 3 {
			t.Errorf("account %d peak = %d, want 3", i+1, peak[i].Load())
		}
	}
	if a.Broker.Snapshot().Reservations != 0 || a.Broker.Snapshot().Waiting != 0 {
		t.Fatal("capacity leaked")
	}
}

func TestAccount429FallsBackAndTemporaryLimitHasRetryAfter(t *testing.T) {
	for _, both := range []bool{false, true} {
		t.Run(fmt.Sprint(both), func(t *testing.T) {
			t.Setenv("DORANG_SECOND_TEST_KEY", "second-account-test-key")
			var calls [2]atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				i := 0
				if strings.Contains(r.Header.Get("Authorization"), "second-account-test-key") {
					i = 1
				}
				calls[i].Add(1)
				w.Header().Set("Content-Type", "application/json")
				if both || i == 0 {
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":{"code":"insufficient_quota","message":"too many concurrent requests"}}`)
					return
				}
				fmt.Fprint(w, capacityAnswer)
			}))
			defer up.Close()
			a := newWiringApp(t, accountCapacityYAML(up.URL, "100ms"), nil, func(o *Options) { o.Upstream = up.Client() })
			key := issueKey(t, a, nil)
			w := callWith(a, key, http.MethodPost, "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"go"}]}`)
			if both {
				if w.Code != 429 || !strings.Contains(w.Body.String(), `"code":"rate_limit_exceeded"`) || w.Header().Get("Retry-After") == "" {
					t.Fatalf("status=%d retry=%s body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
				}
			} else if w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if calls[0].Load() != 1 || calls[1].Load() != 1 {
				t.Fatalf("upstream attempts %d, %d; want one per account", calls[0].Load(), calls[1].Load())
			}
			for _, id := range deploymentIDs(a.Config())[0] {
				s := a.Health.Stats(id)
				if s.Failures != 0 || s.RateLimitStrikes != 0 {
					t.Fatalf("temporary limit counted as lasting failure: %+v", s)
				}
			}
		})
	}
}

func TestFullAccountsBoundQueueWaitAndCancellation(t *testing.T) {
	t.Setenv("DORANG_SECOND_TEST_KEY", "second-account-test-key")
	var calls atomic.Int64
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, capacityAnswer)
	}))
	t.Cleanup(up.Close)
	t.Cleanup(unblock)
	yaml := strings.ReplaceAll(accountCapacityYAML(up.URL, "2s"), "max_concurrency: 3, max_queue: 10", "max_concurrency: 3, max_queue: 1")
	yaml = strings.Replace(yaml, "max_concurrent: 32, max_queue: 10", "max_concurrent: 32, max_queue: 1", 1)
	a := newWiringApp(t, yaml, nil, func(o *Options) { o.Upstream = up.Client() })
	key := issueKey(t, a, nil)
	done := make(chan *httptest.ResponseRecorder, 6)
	const body = `{"model":"m1","messages":[{"role":"user","content":"go"}]}`
	for i := 0; i < 6; i++ {
		go func() { done <- callWith(a, key, http.MethodPost, "/v1/chat/completions", body) }()
	}
	waitCapacity(t, func() bool { return calls.Load() == 6 })
	start := time.Now()
	w := callWith(a, key, http.MethodPost, "/v1/chat/completions", body)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || time.Since(start) > 3*time.Second {
		t.Fatalf("queue did not time out promptly: %d %s", w.Code, w.Body.String())
	}
	if calls.Load() != 6 || a.Broker.Snapshot().Waiting != 0 {
		t.Fatal("timed out request reached upstream or leaked a waiter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+key)
	cancelled := make(chan struct{})
	go func() { a.Server.ServeHTTP(httptest.NewRecorder(), r); close(cancelled) }()
	waitCapacity(t, func() bool { return a.Broker.Snapshot().Waiting == 1 })
	start = time.Now()
	overflow := callWith(a, key, http.MethodPost, "/v1/chat/completions", body)
	if overflow.Code != 429 || overflow.Header().Get("Retry-After") == "" || time.Since(start) > time.Second {
		t.Fatalf("queue overflow did not refuse promptly: %d", overflow.Code)
	}
	if a.Broker.Snapshot().Waiting != 1 || calls.Load() != 6 {
		t.Fatal("queue ceiling was exceeded")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	if calls.Load() != 6 || a.Broker.Snapshot().Waiting != 0 {
		t.Fatal("cancelled request reached upstream or leaked a waiter")
	}
	unblock()
	for i := 0; i < 6; i++ {
		if w := <-done; w.Code != 200 {
			t.Fatalf("held request status %d", w.Code)
		}
	}
	if a.Broker.Snapshot().Reservations != 0 {
		t.Fatal("reservation leaked after cancellation")
	}
}
