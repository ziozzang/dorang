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

// A leaked key can be revoked through the API.
//
// The security review's plainest sentence was not a finding, it was a
// consequence: "there is no way to revoke a leaked API key through the API."
// `internal/admin` had /key/block, /key/unblock and an audit trail, and zero
// importers, so the documented incident-response step was a hand-written
// `UPDATE api_keys SET blocked = 1` against the database — which needs shell
// access to the box, cannot be done by an on-call runbook over HTTP, and leaves
// no audit row.
//
// This test is the whole point of mounting the surface. It runs the incident:
// a key is working, the operator blocks it with one HTTP call, and the very
// next request from that key is refused. Nothing here touches the database
// directly except to plant the key and to read the audit row back.
func TestALeakedKeyCanBeRevokedThroughTheAPI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "revoke-test-pepper")

	const masterKey = "sk-master-revoke-test" // pragma: allowlist secret — test fixture
	t.Setenv("DORANG_MASTER_KEY", masterKey)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamAnswer)
	}))
	defer up.Close()

	ctx := context.Background()
	a, err := app.New(ctx, app.Options{Config: loadRoundTripConfig(t, dir, up.URL), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(context.Background()) }()
	if a.Admin == nil {
		t.Fatal("no administration surface was built: internal/admin is unmounted again")
	}

	const leaked = "sk-the-leaked-key" // pragma: allowlist secret — test fixture
	k := &store.APIKey{KeyAlias: "leaked"}
	if err := a.Store.NewAPIKeyFromToken(leaked, k); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.InsertAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}

	front := httptest.NewServer(a.Server)
	defer front.Close()
	const body = `{"model":"model-x","max_tokens":4,"messages":[{"role":"user","content":"ping"}]}`

	if r := post(t, front.URL+"/v1/chat/completions", leaked, body); r.status != http.StatusOK {
		t.Fatalf("the key should work before it is blocked: %d %s", r.status, r.body)
	}

	// The revocation, over HTTP, with the master credential. Before this was
	// mounted the answer here was 501 route_not_implemented.
	blocked := post(t, front.URL+"/key/block", masterKey, `{"key_id":"`+k.ID+`"}`)
	if blocked.status != http.StatusOK {
		t.Fatalf("POST /key/block: %d %s", blocked.status, blocked.body)
	}
	if !strings.Contains(blocked.body, `"blocked":true`) {
		t.Errorf("the block response should report the new state: %s", blocked.body)
	}

	// The authenticator caches a loaded credential, so the block is observed
	// within the entry TTL rather than instantly. Waiting for the refusal is
	// the honest assertion — the operator's question is "how long until it
	// stops working", and the answer must be a bounded number and not "never".
	deadline := time.Now().Add(90 * time.Second)
	var last response
	for time.Now().Before(deadline) {
		last = post(t, front.URL+"/v1/chat/completions", leaked, body)
		if last.status != http.StatusOK {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if last.status == http.StatusOK {
		t.Fatalf("the blocked key is still serving requests: %d %s", last.status, last.body)
	}

	// The trail. §2.3 audits every mutation, and until this change the
	// audit_logs table had a schema, two indexes, a retention sweep that
	// deletes from it, and no writer anywhere in the tree.
	var (
		action, actorKind, objectID string
		after                       string
	)
	row := a.Store.DB().QueryRow(
		`SELECT action, actor_kind, object_id, after_state FROM audit_logs ORDER BY ts DESC LIMIT 1`)
	if err := row.Scan(&action, &actorKind, &objectID, &after); err != nil {
		t.Fatalf("no audit row was written for the revocation: %v", err)
	}
	if action != "key.block" || objectID != k.ID {
		t.Errorf("audit row = action %q object %q, want key.block on %q", action, objectID, k.ID)
	}
	if actorKind != "master" {
		t.Errorf("audit actor_kind = %q, want master", actorKind)
	}
	// The trail records the object, never the credential.
	for _, secret := range []string{leaked, masterKey, k.Lookup, k.TokenHash} {
		if strings.Contains(after, secret) {
			t.Fatalf("the audit row carries key material: %s", after)
		}
	}

	// And the key is visible to the operator by id, with no secret in the view.
	info := post(t, front.URL+"/key/info", masterKey, `{"key_id":"`+k.ID+`"}`)
	if info.status != http.StatusOK {
		t.Fatalf("POST /key/info: %d %s", info.status, info.body)
	}
	var infoBody map[string]any
	if err := json.Unmarshal([]byte(info.body), &infoBody); err != nil {
		t.Fatalf("key info is not JSON: %v", err)
	}
	for _, secret := range []string{leaked, k.Lookup, k.TokenHash} {
		if strings.Contains(info.body, secret) {
			t.Fatalf("/key/info returned key material: %s", info.body)
		}
	}
}

// An ordinary key cannot reach the administration surface.
//
// Mounting it put forty privileged paths on the same origin as the inference
// API. The entry condition is that the caller is the master credential or a key
// whose owning user holds an administrative role; an ordinary tenant key is
// authenticated by the gateway and then refused by the surface.
func TestAnOrdinaryKeyCannotAdminister(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DORANG_KEY_PEPPER", "revoke-test-pepper")
	t.Setenv("DORANG_MASTER_KEY", "sk-master-revoke-test") // pragma: allowlist secret — test fixture

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamAnswer)
	}))
	defer up.Close()

	ctx := context.Background()
	a, err := app.New(ctx, app.Options{Config: loadRoundTripConfig(t, dir, up.URL), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(context.Background()) }()

	const victim = "sk-victim-key" // pragma: allowlist secret — test fixture
	const tenant = "sk-tenant-key" // pragma: allowlist secret — test fixture
	for _, tok := range []string{victim, tenant} {
		k := &store.APIKey{}
		if err := a.Store.NewAPIKeyFromToken(tok, k); err != nil {
			t.Fatal(err)
		}
		if err := a.Store.InsertAPIKey(ctx, k); err != nil {
			t.Fatal(err)
		}
	}

	front := httptest.NewServer(a.Server)
	defer front.Close()

	for _, path := range []string{"/key/list", "/key/block", "/spend/logs", "/admin/status"} {
		r := post(t, front.URL+path, tenant, `{}`)
		if r.status != http.StatusForbidden {
			t.Errorf("%s with an ordinary key: %d %s, want 403", path, r.status, r.body)
		}
	}
	// And with no credential at all, before the route table can be read.
	for _, path := range []string{"/key/list", "/admin/status"} {
		r := post(t, front.URL+path, "", `{}`)
		if r.status != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated: %d %s, want 401", path, r.status, r.body)
		}
	}
}
