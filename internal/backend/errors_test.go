package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// The secret every test in this file plants. It is deliberately distinctive:
// every assertion is "this string appears nowhere", and a value that could
// occur by accident would make those assertions vacuous.
const plantedSecret = "sk-CANARY-9f3b21-do-not-leak" // pragma: allowlist secret — test fixture

// TestErrorNormalization covers every upstream envelope the documents record.
//
// The shapes are not hypothetical. SGLANG.md §6.2 observes five of them from a
// single process — flat for the OpenAI routes, nested for the same errors when
// streaming, OpenAI-nested for /v1/responses, Anthropic-shaped for /v1/messages,
// and a bare string from the auth middleware — and DESIGN §4.3 observes two from
// a single host. "The provider" is not a unit of consistency, so the normalizer
// is tested against the union rather than against one vendor.
func TestErrorNormalization(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantType string
		wantCode string
		shape    server.Shape
		// wantMessageHas is a substring the client must be able to see.
		wantMessageHas string
	}{
		{
			name:   "openai nested",
			status: 400,
			body: `{"error":{"message":"bad model","type":"invalid_request_error",` +
				`"param":"model","code":"model_not_found"}}`,
			wantType: server.TypeInvalidRequest, wantCode: "model_not_found",
			shape: server.ShapeNested, wantMessageHas: "bad model",
		},
		{
			name:     "integer code",
			status:   429,
			body:     `{"error":{"message":"slow down","type":"RateLimitError","code":429}}`,
			wantType: server.TypeRateLimit, wantCode: "429",
			shape: server.ShapeNested, wantMessageHas: "slow down",
		},
		{
			name:     "flat envelope",
			status:   400,
			body:     `{"object":"error","message":"context too long","type":"BadRequestError","code":400}`,
			wantType: server.TypeInvalidRequest, wantCode: "400",
			shape: server.ShapeFlat, wantMessageHas: "context too long",
		},
		{
			name:     "anthropic",
			status:   401,
			body:     `{"type":"error","error":{"type":"authentication_error","message":"invalid key"}}`,
			wantType: server.TypeAuthentication, wantCode: "401",
			shape: server.ShapeAnthropic, wantMessageHas: "invalid key",
		},
		{
			name:     "bare string",
			status:   401,
			body:     `{"error":"Unauthorized"}`,
			wantType: server.TypeAuthentication, wantCode: "401",
			shape: server.ShapeBareString, wantMessageHas: "Unauthorized",
		},
		{
			name:     "fastapi detail string",
			status:   422,
			body:     `{"detail":"field required"}`,
			wantType: server.TypeInvalidRequest, wantCode: "422",
			shape: server.ShapeDetail, wantMessageHas: "field required",
		},
		{
			name:   "fastapi detail array",
			status: 422,
			body: `{"detail":[{"loc":["body","messages"],"msg":"field required",` +
				`"type":"value_error.missing"}]}`,
			wantType: server.TypeInvalidRequest, wantCode: "422",
			shape: server.ShapeDetail, wantMessageHas: "field required",
		},
		{
			name:     "not json",
			status:   502,
			body:     "<html><head><title>502 Bad Gateway</title></head></html>",
			wantType: server.TypeAPIError, wantCode: "502",
			shape: server.ShapeOpaque, wantMessageHas: "502",
		},
		{
			name:     "empty body",
			status:   500,
			body:     "",
			wantType: server.TypeAPIError, wantCode: "500",
			shape: server.ShapeOpaque, wantMessageHas: "Internal",
		},
		{
			name:     "json but not an error",
			status:   503,
			body:     `{"status":"warming up"}`,
			wantType: server.TypeOverloaded, wantCode: "503",
			shape: server.ShapeOpaque, wantMessageHas: "warming up",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t)
			f.answer(tc.status, tc.body)
			p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

			res := testBackend(plantedSecret).Do(context.Background(), target(p),
				chatCall(catalog.APIOpenAIChat), nil)
			if res.Err == nil {
				t.Fatal("want an error")
			}
			e := res.Err
			if e.Status != tc.status {
				t.Errorf("status = %d, want %d", e.Status, tc.status)
			}
			if e.Type != tc.wantType {
				t.Errorf("type = %q, want %q", e.Type, tc.wantType)
			}
			if e.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", e.Code, tc.wantCode)
			}
			if e.Shape != tc.shape {
				t.Errorf("shape = %v, want %v", e.Shape, tc.shape)
			}
			if !strings.Contains(e.Message, tc.wantMessageHas) {
				t.Errorf("message = %q, want it to contain %q", e.Message, tc.wantMessageHas)
			}
			if !server.KnownTypes(e.Type) {
				t.Errorf("type %q is outside the canonical vocabulary; a client that branches on "+
					"type must never see a vendor string", e.Type)
			}

			// The envelope that actually reaches a client: code is a STRING,
			// always, because an integer there raises inside the SDK before the
			// message is ever surfaced.
			var envelope struct {
				Error struct {
					Message string          `json:"message"`
					Type    string          `json:"type"`
					Param   *string         `json:"param"`
					Code    json.RawMessage `json:"code"`
				} `json:"error"`
			}
			raw := server.EncodeError(e)
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatalf("the rendered envelope is not JSON: %v", err)
			}
			if len(envelope.Error.Code) == 0 || envelope.Error.Code[0] != '"' {
				t.Errorf("code on the wire = %s, want a JSON string", envelope.Error.Code)
			}
			if envelope.Error.Message == "" {
				t.Error("an empty message is a client-side crash waiting to happen")
			}
			assertNoSecret(t, string(raw))
			assertNoSecret(t, e.Message)
		})
	}
}

// TestUpstreamRetryAfterReachesTheClient is COMPATIBILITY §11.4: without it
// every SDK's backoff degrades to a fixed guess, and the provider's own value is
// the only accurate one anybody has.
func TestUpstreamRetryAfterReachesTheClient(t *testing.T) {
	f := newFakeUpstream(t)
	f.answer(http.StatusTooManyRequests, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
	f.header = http.Header{"Retry-After": []string{"7"}}

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	res := testBackend("sk").Do(context.Background(), target(p), chatCall(catalog.APIOpenAIChat), nil)
	if res.Err == nil {
		t.Fatal("want an error")
	}
	if res.Err.RetryAfterSeconds != 7 {
		t.Errorf("Retry-After on the client error = %d, want 7", res.Err.RetryAfterSeconds)
	}
	if res.RetryAfter.Seconds() != 7 {
		t.Errorf("Retry-After on the result = %v, want 7s: the router needs it to set a cooldown",
			res.RetryAfter)
	}
}

// TestRedirectIsNeverFollowed is DESIGN §10.6's third security rule.
//
// The attacker-nominated host is a second server in this process. If it is ever
// reached, it records the request — and the assertion is that it recorded
// nothing, not merely that the response was odd.
func TestRedirectIsNeverFollowed(t *testing.T) {
	attacker := newFakeUpstream(t)
	attacker.answer(http.StatusOK, `{"id":"x","choices":[]}`)

	f := newFakeUpstream(t)
	for _, status := range []int{301, 302, 303, 307, 308} {
		f.setHandler(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", attacker.srv.URL+"/v1/chat/completions")
			w.WriteHeader(status)
		})
		p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
		res := testBackend(plantedSecret).Do(context.Background(), target(p),
			chatCall(catalog.APIOpenAIChat), nil)

		if res.Err == nil {
			t.Fatalf("%d: a redirect must be refused, not followed", status)
		}
		if res.Err.Code != CodeUpstreamRedirect {
			t.Errorf("%d: code = %q, want %q", status, res.Err.Code, CodeUpstreamRedirect)
		}
		if attacker.count() != 0 {
			t.Fatalf("%d: the attacker-nominated host was reached — with the provider credential", status)
		}
		// The Location is upstream-chosen text and must not be quoted back.
		if strings.Contains(res.Err.Message, attacker.srv.URL) {
			t.Errorf("%d: the redirect target is relayed in the error: %q", status, res.Err.Message)
		}
		assertNoSecret(t, res.Err.Message)
	}
}

// TestCredentialAbsentFromEveryErrorPath walks every way this package can fail
// and asserts the secret is in none of them.
//
// It is a table rather than one case per test because the property is
// "everywhere", and a per-path test invites the next path to be added without
// one.
func TestCredentialAbsentFromEveryErrorPath(t *testing.T) {
	paths := []struct {
		name  string
		setup func(t *testing.T) (*Backend, Target, *Call)
	}{
		{
			name: "upstream 4xx echoing the key",
			setup: func(t *testing.T) (*Backend, Target, *Call) {
				f := newFakeUpstream(t)
				// A backend that echoes the credential it was given, which is
				// exactly what §10.6's fourth rule exists for.
				f.answer(http.StatusUnauthorized,
					`{"error":{"message":"bad key: `+plantedSecret+`","type":"authentication_error"}}`)
				p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
				return testBackend(plantedSecret), target(p), chatCall(catalog.APIOpenAIChat)
			},
		},
		{
			name: "upstream 500 with an HTML page",
			setup: func(t *testing.T) (*Backend, Target, *Call) {
				f := newFakeUpstream(t)
				f.answer(http.StatusInternalServerError,
					"<html>Authorization: Bearer "+plantedSecret+"</html>")
				p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
				return testBackend(plantedSecret), target(p), chatCall(catalog.APIOpenAIChat)
			},
		},
		{
			name: "unreachable upstream",
			setup: func(t *testing.T) (*Backend, Target, *Call) {
				p, err := NewProvider(Spec{Name: "p", Kind: "openai",
					API: catalog.APIOpenAIChat, BaseURL: "http://127.0.0.1:1"})
				if err != nil {
					t.Fatal(err)
				}
				return testBackend(plantedSecret), target(p), chatCall(catalog.APIOpenAIChat)
			},
		},
		{
			name: "undecodable answer",
			setup: func(t *testing.T) (*Backend, Target, *Call) {
				f := newFakeUpstream(t)
				f.answer(http.StatusOK, `{"choices": "not an array"}`)
				p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
				return testBackend(plantedSecret), target(p), chatCall(catalog.APIOpenAIChat)
			},
		},
		{
			name: "unsupported operation",
			setup: func(t *testing.T) (*Backend, Target, *Call) {
				f := newFakeUpstream(t)
				p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
				c := chatCall(catalog.APIOpenAIChat)
				c.Op = OpCountTokens
				return testBackend(plantedSecret), target(p), c
			},
		},
		{
			name: "refused kind",
			setup: func(t *testing.T) (*Backend, Target, *Call) {
				f := newFakeUpstream(t)
				p, err := NewProvider(Spec{Name: "b", Kind: "bedrock",
					API: catalog.APIAnthropicMessages, BaseURL: f.srv.URL})
				if err != nil {
					t.Fatal(err)
				}
				return testBackend(plantedSecret), target(p), chatCall(catalog.APIOpenAIChat)
			},
		},
		{
			name: "oauth credential whose own error carries the token",
			setup: func(t *testing.T) (*Backend, Target, *Call) {
				f := newFakeUpstream(t)
				p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
				be := New(Options{Credentials: staticCredentials{
					oauth: map[string]Applier{"c1": failingApplier{leak: plantedSecret}},
				}})
				return be, target(p), chatCall(catalog.APIOpenAIChat)
			},
		},
		{
			name: "refused redirect",
			setup: func(t *testing.T) (*Backend, Target, *Call) {
				f := newFakeUpstream(t)
				f.setHandler(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Location", "https://evil.example/"+plantedSecret)
					w.WriteHeader(http.StatusFound)
				})
				p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
				return testBackend(plantedSecret), target(p), chatCall(catalog.APIOpenAIChat)
			},
		},
	}

	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			be, tg, c := tc.setup(t)
			var seen []Failure
			be.obs = observerFunc(func(f Failure) { seen = append(seen, f) })

			res := be.Do(context.Background(), tg, c, nil)
			if res.Err == nil {
				t.Fatal("want an error on this path")
			}
			assertNoSecret(t, res.Err.Message)
			assertNoSecret(t, res.Err.Code)
			assertNoSecret(t, res.Err.NativeType)
			assertNoSecret(t, string(server.EncodeError(res.Err)))
			for _, f := range seen {
				// The observer is the metering seam and receives the same
				// scrutiny: a record that reaches a log is a record that reaches
				// a log aggregator.
				assertNoSecret(t, f.Provider+f.Credential+f.UpstreamModel+f.Code+f.NativeErrorType)
			}
		})
	}
}

// TestOAuthCredentialSatisfiesApplier is a compile-time claim made executable:
// the interface this package declares is the one internal/auth already has, not
// one internal/auth would have to grow a method for.
func TestOAuthCredentialSatisfiesApplier(t *testing.T) {
	var _ Applier = (*auth.OAuthCredential)(nil)
}

// TestCredentialSpellingPerFamily pins the four spellings apart. Sending the
// wrong one is a 401 that looks exactly like a wrong key.
func TestCredentialSpellingPerFamily(t *testing.T) {
	cases := []struct {
		kind   string
		api    catalog.API
		header string
		value  string
	}{
		{"openai", catalog.APIOpenAIChat, "Authorization", "Bearer " + plantedSecret},
		{"anthropic", catalog.APIAnthropicMessages, "x-api-key", plantedSecret},
		{"google", catalog.APIGemini, "x-goog-api-key", plantedSecret},
		{"cohere", catalog.APICohere, "Authorization", "Bearer " + plantedSecret},
		{"jina", catalog.APIJina, "Authorization", "Bearer " + plantedSecret},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			p, err := NewProvider(Spec{Name: tc.kind, Kind: tc.kind, API: tc.api,
				BaseURL: "https://example.invalid"})
			if err != nil {
				t.Fatal(err)
			}
			h := http.Header{}
			if err := p.applyCredential(plantedSecret, nil, h); err != nil {
				t.Fatalf("applyCredential: %v", err)
			}
			if got := h.Get(tc.header); got != tc.value {
				t.Errorf("%s = %q, want %q", tc.header, got, tc.value)
			}
		})
	}
}

// observerFunc adapts a function to Observer.
type observerFunc func(Failure)

func (f observerFunc) UpstreamFailure(x Failure) { f(x) }

func assertNoSecret(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, plantedSecret) {
		t.Fatalf("a credential reached a client-visible string: %q", s)
	}
	// The bearer prefix on its own is enough to leak a key that was clipped.
	if strings.Contains(s, "Bearer sk-") {
		t.Fatalf("an authorization header reached a client-visible string: %q", s)
	}
}
