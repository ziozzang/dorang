package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/backend"
	"github.com/ziozzang/dorang/internal/batch"
	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// The broker's chosen credential is the one the batch executor authenticates
// with — not the provider's first.
//
// The two halves used to be decided separately. The scheduler reserved against
// the provider's whole candidate set, and the wiring then sent the row with
// whichever credential sorted first, so a row admitted because account B had
// room went out as account A. Both accounts' concurrency counts were then wrong
// in opposite directions, and the symptom — A returning 429s for traffic it was
// never sent — appears nowhere near the cause.
//
// The rig below saturates the first credential so the broker is forced onto the
// second, then checks which secret reached the wire.
func TestBatchExecutorUsesTheReservedCredential(t *testing.T) {
	ctx := context.Background()

	var sawAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,` +
			`"model":"up-1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},` +
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()

	prov, err := backend.NewProvider(backend.Spec{
		Name: "p1", Kind: "openai", API: catalog.APIOpenAIChat, BaseURL: up.URL + "/v1",
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	table := &upstreamTable{
		providers: map[string]*backend.Provider{"p1": prov},
		creds: map[string]*credential{
			"cred-a": {id: "cred-a", provider: "p1", capacityGroup: "acct-a", maxConcurrent: 1, secret: "secret-a"},
			"cred-b": {id: "cred-b", provider: "p1", capacityGroup: "acct-b", maxConcurrent: 1, secret: "secret-b"},
		},
	}

	broker := capacity.New(capacity.Config{
		CredentialGroups: map[string]int{"acct-a": 1, "acct-b": 1},
		SweepInterval:    -1,
	})
	defer broker.Close()

	reserver := &batchReserver{broker: broker, table: table}

	// Hold the only slot on the first account, so the second is the only one
	// with room.
	held, err := reserver.Acquire(ctx, batch.CapacityRequest{
		Provider: "p1", UpstreamModel: "up-1", PrincipalID: "key-1",
	})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if held.CredentialID() != "cred-a" {
		t.Fatalf("the first reservation landed on %q, want cred-a", held.CredentialID())
	}
	defer held.Release()

	resv, err := reserver.Acquire(ctx, batch.CapacityRequest{
		Provider: "p1", UpstreamModel: "up-1", PrincipalID: "key-1",
	})
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	defer resv.Release()
	if resv.CredentialID() != "cred-b" {
		t.Fatalf("the second reservation landed on %q, want cred-b — the first account "+
			"is saturated", resv.CredentialID())
	}

	d := newDispatcher(up.Client(), t.Logf, time.Now)
	d.swap(&dispatchState{upstreams: table})
	exec := &batchExecutor{d: d}

	res, err := exec.Execute(ctx, &batch.ExecRequest{
		BatchID:       "batch-1",
		CustomID:      "req-0",
		Endpoint:      "/v1/chat/completions",
		Model:         "m1",
		Provider:      "p1",
		UpstreamModel: "up-1",
		Credential:    resv.CredentialID(),
		Body:          []byte(`{"model":"m1","messages":[{"role":"user","content":"ping"}]}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if sawAuth != "Bearer secret-b" { // pragma: allowlist secret — test fixture
		t.Errorf("the row was dispatched with %q, want the reserved credential's "+
			"secret %q — the reservation holds slots on cred-b's axes",
			sawAuth, "Bearer secret-b")
	}
}
