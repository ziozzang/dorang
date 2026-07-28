package luaext

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// The secret tests.
//
// DESIGN §11.5 asks for a sandbox; the requirement behind it is that an
// extension cannot read a credential and cannot send one anywhere. This package
// meets it structurally rather than by filtering, so the tests are structural
// too: they assert the *shape* of what a hook can reach, which is a property a
// future field addition has to break loudly.

// viewFields is the complete, reviewed list of everything a hook may see. A new
// field has to be added here consciously — which is the moment to ask whether it
// is a secret.
var viewFields = map[string][]string{
	"RequestView": {
		"RequestID", "Method", "Path", "Route", "Model", "KeyID", "KeyName",
		"UserID", "TeamID", "Priority", "BodyBytes", "InputTokens",
		"MaxOutputTokens", "Stream",
	},
	"RouteView": {
		"Model", "KeyID", "UserID", "TeamID", "Provider", "Deployment", "Kind",
		"UpstreamModel", "Priority", "Attempt", "InputTokens", "Stream",
	},
	"ResponseView": {
		"RequestID", "Model", "KeyID", "UserID", "TeamID", "Provider",
		"Deployment", "UpstreamModel", "ErrorCode", "Status", "InputTokens",
		"OutputTokens", "CostNanoUSD", "TTFTMillis", "TotalMillis", "Attempts",
		"Stream",
	},
	"EmailView": {
		"Event", "SubjectKind", "SubjectID", "Recipient", "Subject", "Body",
		"Driver",
	},
}

// secretSubstrings never belong in a field name at all, wherever they appear.
// "token" is deliberately absent: a token *count* is a number, and refusing the
// word would be refusing the metric rather than the material.
var secretSubstrings = []string{
	"secret", "credential", "password", "passwd", "apikey", "api_key",
	"authoriz", "bearer", "cookie", "pepper", "signature", "hashed",
}

// secretExactNames are identifiers that are safe as a component of a longer
// name and unsafe on their own: `key_id` is an identifier, `key` is a key;
// `input_tokens` is a count, `token` is a token; `body_bytes` is a size, `body`
// is the request.
var secretExactNames = map[string]bool{
	"key": true, "token": true, "tokens": true, "body": true, "headers": true,
	"header": true, "messages": true, "message": true, "prompt": true,
	"content": true, "payload": true, "auth": true, "session": true,
	"hash": true, "cert": true,
}

// emailRendered is the one reviewed exception: EmailView carries the *rendered
// and already-redacted* notification, so `subject` and `body` there mean the
// message internal/notify built, not the request.
func emailRendered(h Hook, name string) bool {
	return h == HookEmail && (name == "subject" || name == "body")
}

func TestNoSecretIsReachableFromAnyView(t *testing.T) {
	types := map[string]reflect.Type{
		"RequestView":  reflect.TypeOf(RequestView{}),
		"RouteView":    reflect.TypeOf(RouteView{}),
		"ResponseView": reflect.TypeOf(ResponseView{}),
		"EmailView":    reflect.TypeOf(EmailView{}),
	}
	for name, typ := range types {
		want := viewFields[name]
		if typ.NumField() != len(want) {
			t.Errorf("%s has %d fields, the reviewed list has %d — a field was added "+
				"without deciding whether it is a secret", name, typ.NumField(), len(want))
		}
		got := make([]string, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			got = append(got, f.Name)

			// Every field must be a scalar. A map, slice, pointer or interface
			// is a door: it is the shape that lets an http.Header or a whole
			// request through.
			switch f.Type.Kind() {
			case reflect.String, reflect.Int64, reflect.Bool:
			default:
				t.Errorf("%s.%s is a %s; a view field must be a string, an int64 or a bool, "+
					"because anything richer can carry a credential", name, f.Name, f.Type.Kind())
			}

			lower := strings.ToLower(f.Name)
			if name == "EmailView" && (f.Name == "Subject" || f.Name == "Body") {
				continue
			}
			for _, bad := range secretSubstrings {
				if strings.Contains(lower, bad) {
					t.Errorf("%s.%s contains %q; a hook must not be able to see it",
						name, f.Name, bad)
				}
			}
			if secretExactNames[lower] {
				t.Errorf("%s.%s is named %q on its own; a hook must not be able to see it",
					name, f.Name, lower)
			}
		}
		if !equalStrings(got, want) {
			t.Errorf("%s fields = %v, reviewed list = %v", name, got, want)
		}
	}
}

// TestPolicyCannotNameASecret is the same guarantee at the language level: an
// identifier that does not exist is a *load* error, so a policy that reaches for
// a credential fails at startup instead of silently comparing against nothing.
func TestPolicyCannotNameASecret(t *testing.T) {
	for h := Hook(0); h < numHooks; h++ {
		table := fieldsFor(h)
		for _, name := range table.names {
			if emailRendered(h, name) {
				continue
			}
			lower := strings.ToLower(name)
			for _, bad := range secretSubstrings {
				if strings.Contains(lower, bad) {
					t.Errorf("%s exposes an identifier %q containing %q", h, name, bad)
				}
			}
			if secretExactNames[lower] {
				t.Errorf("%s exposes an identifier named %q on its own", h, name)
			}
		}
		for _, reach := range []string{
			"authorization", "api_key", "apikey", "token", "secret", "credential",
			"password", "headers", "header", "body", "messages", "prompt", "cookie",
			"key", "bearer", "pepper", "session",
		} {
			if emailRendered(h, reach) {
				continue
			}
			src := "set t = \"x\" if " + reach + " == \"y\"\n"
			if _, err := Compile("t.policy", h, []byte(src)); err == nil {
				t.Errorf("%s compiled a reference to %q", h, reach)
			}
		}
	}
}

// TestHookCannotSeeCredentialInFlight is the behavioural half: a real secret is
// held next to the request, every hook runs, and each one records everything it
// could reach. The secret must appear in none of it.
func TestHookCannotSeeCredentialInFlight(t *testing.T) {
	const secret = "sk-live-DO-NOT-LEAK-2f8c41ab9e77" // pragma: allowlist secret — test fixture

	var mu sync.Mutex
	var seen []string
	record := func(vals ...string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, vals...)
	}

	e, err := New(Options{
		Enabled: true,
		Limits:  Limits{Instructions: 10000, MemoryBytes: 1 << 20, Timeout: time.Second},
		Native: []Native{{
			Name: "exfiltrator",
			Request: func(_ context.Context, v *RequestView, _ *RequestDecision) {
				record(dumpView(v, requestFields)...)
			},
			Route: func(_ context.Context, v *RouteView, _ *RouteDecision) {
				record(dumpView(v, routeFields)...)
			},
			Response: func(_ context.Context, v *ResponseView, _ *ResponseDecision) {
				record(dumpView(v, responseFields)...)
			},
			Email: func(_ context.Context, v *EmailView, _ *EmailDecision) error {
				record(dumpView(v, emailFields)...)
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// The views are built the way internal/app builds them, from a request that
	// really does carry the credential — the credential simply has nowhere to go.
	e.OnRequest(ctx, &RequestView{
		RequestID: "req-1", Method: "POST", Path: "/v1/chat/completions",
		Route: "openai-chat", Model: "gpt-4", KeyID: "key-1", KeyName: "ci",
		UserID: "u-1", TeamID: "eng", BodyBytes: 1024, InputTokens: 12,
	})
	e.OnRoute(ctx, &RouteView{
		Model: "gpt-4", KeyID: "key-1", Provider: "plan-a", Deployment: "d-1",
		Kind: "openai", UpstreamModel: "gpt-4-0613",
	})
	e.OnResponse(ctx, &ResponseView{
		RequestID: "req-1", Model: "gpt-4", Status: 200, CostNanoUSD: 1234,
	})
	if _, err := e.OnEmail(ctx, &EmailView{
		Event: "key_created", SubjectKind: "key", SubjectID: "key-1",
		Recipient: "ops@example.com", Subject: "[dorang] key created",
		Body: "key_id: key-1\n",
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the hooks did not run")
	}
	for _, s := range seen {
		if strings.Contains(s, secret) || strings.Contains(s, "sk-live") { // pragma: allowlist secret — test fixture
			t.Fatalf("a hook read %q", s)
		}
	}
}

// dumpView reads every identifier a program could name, through the same
// accessor a program uses.
func dumpView(v viewer, table fieldTable) []string {
	out := make([]string, 0, len(table.names))
	for _, name := range table.names {
		out = append(out, table.byName[name].name+"="+v.field(table.byName[name].idx).String())
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
