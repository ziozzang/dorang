package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
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
		// wantNativeHas is a substring of the UPSTREAM's own message. It must
		// reach Error.NativeMessage — the ledger and the log — and it must not
		// reach the client-facing body, which is COMPATIBILITY §11.3: "The
		// upstream's own type, code, and message are recorded in the ledger and
		// surfaced in x-dorang-native-error-type / x-dorang-native-error-code.
		// They are not put in the response body."
		//
		// This assertion used to be the other way round — the message the
		// client sees had to CONTAIN the upstream's words — which is the
		// credential-exfiltration path this file's own plantedSecret exists to
		// catch, written into the test that was supposed to catch it.
		wantNativeHas string
	}{
		{
			name:   "openai nested",
			status: 400,
			body: `{"error":{"message":"bad model","type":"invalid_request_error",` +
				`"param":"model","code":"model_not_found"}}`,
			wantType: server.TypeInvalidRequest, wantCode: "model_not_found",
			shape: server.ShapeNested, wantNativeHas: "bad model",
		},
		{
			name:     "integer code",
			status:   429,
			body:     `{"error":{"message":"slow down","type":"RateLimitError","code":429}}`,
			wantType: server.TypeRateLimit, wantCode: "429",
			shape: server.ShapeNested, wantNativeHas: "slow down",
		},
		{
			name:     "flat envelope",
			status:   400,
			body:     `{"object":"error","message":"context too long","type":"BadRequestError","code":400}`,
			wantType: server.TypeInvalidRequest, wantCode: "400",
			shape: server.ShapeFlat, wantNativeHas: "context too long",
		},
		{
			name:     "anthropic",
			status:   401,
			body:     `{"type":"error","error":{"type":"authentication_error","message":"invalid key"}}`,
			wantType: server.TypeAuthentication, wantCode: "401",
			shape: server.ShapeAnthropic, wantNativeHas: "invalid key",
		},
		{
			name:     "bare string",
			status:   401,
			body:     `{"error":"Unauthorized"}`,
			wantType: server.TypeAuthentication, wantCode: "401",
			shape: server.ShapeBareString, wantNativeHas: "Unauthorized",
		},
		{
			name:     "fastapi detail string",
			status:   422,
			body:     `{"detail":"field required"}`,
			wantType: server.TypeInvalidRequest, wantCode: "422",
			shape: server.ShapeDetail, wantNativeHas: "field required",
		},
		{
			name:   "fastapi detail array",
			status: 422,
			body: `{"detail":[{"loc":["body","messages"],"msg":"field required",` +
				`"type":"value_error.missing"}]}`,
			wantType: server.TypeInvalidRequest, wantCode: "422",
			shape: server.ShapeDetail, wantNativeHas: "field required",
		},
		{
			name:     "not json",
			status:   502,
			body:     "<html><head><title>502 Bad Gateway</title></head></html>",
			wantType: server.TypeAPIError, wantCode: "502",
			shape: server.ShapeOpaque, wantNativeHas: "502",
		},
		{
			// No body, so there is no upstream text and nothing to keep out of
			// the envelope. The client still gets dorang's own wording rather
			// than an empty message, which is asserted below for every case.
			name:     "empty body",
			status:   500,
			body:     "",
			wantType: server.TypeAPIError, wantCode: "500",
			shape: server.ShapeOpaque,
		},
		{
			name:     "json but not an error",
			status:   503,
			body:     `{"status":"warming up"}`,
			wantType: server.TypeOverloaded, wantCode: "503",
			shape: server.ShapeOpaque, wantNativeHas: "warming up",
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
			if tc.wantNativeHas != "" {
				if !strings.Contains(e.NativeMessage, tc.wantNativeHas) {
					t.Errorf("native message = %q, want it to contain %q: the upstream's "+
						"own words have to survive somewhere an operator can read them",
						e.NativeMessage, tc.wantNativeHas)
				}
				if strings.Contains(e.Message, tc.wantNativeHas) {
					t.Errorf("message = %q contains the upstream's text %q: COMPATIBILITY "+
						"§11.3 keeps it out of the body, because a provider that echoes "+
						"the key back turns this field into a credential channel",
						e.Message, tc.wantNativeHas)
				}
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
			// NativeMessage is where the upstream's text lives now, so it is
			// where the scrubber has to reach. It goes to the ledger and the
			// log; a credential is no less leaked for arriving there.
			assertNoSecret(t, e.NativeMessage)
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
					`{"error":{"message":"bad key: `+plantedSecret+`","type":"authentication_error"}}`) // pragma: allowlist secret — test fixture
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
			assertNoSecret(t, res.Err.NativeMessage)
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
			if err := p.ApplyCredential(plantedSecret, nil, h); err != nil {
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

// TestOversizedUpstreamResponseIsRefused is the L5 half of the review's
// unbounded-read finding.
//
// The interactive dispatcher's success path was an unbounded io.ReadAll and was
// bounded; this package was written afterwards and shipped the same asymmetry —
// a 1 MiB cap on the error branch and no cap at all on the success branch. A
// backend answering 200 with a body larger than the ceiling is a remote OOM
// driven entirely from the upstream side of the trust boundary, at three to four
// times the body size once the buffer is decoded and re-encoded.
//
// The bound is set small here so the test costs a few hundred kilobytes rather
// than 32 MiB. The default is asserted separately, because a test that only
// exercises an injected limit proves nothing about the shipped one.
func TestOversizedUpstreamResponseIsRefused(t *testing.T) {
	const limit = 4096
	f := newFakeUpstream(t)
	f.answer(http.StatusOK, `{"id":"x","choices":[],"pad":"`+strings.Repeat("A", limit*2)+`"}`)

	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)
	be := New(Options{
		Credentials:      staticCredentials{secrets: map[string]string{"c1": plantedSecret}},
		Client:           NewClient(),
		MaxResponseBytes: limit,
	})
	res := be.Do(context.Background(), target(p), chatCall(catalog.APIOpenAIChat), nil)
	if res.Err == nil {
		t.Fatal("a response past the ceiling was buffered and served: the read is unbounded")
	}
	// The bound is on the READ, not on what is returned. readUpstreamBody is
	// checked directly for that, because an implementation that reads the whole
	// body and then measures it produces the same error and none of the safety.
	src := &countingReader{r: strings.NewReader(strings.Repeat("z", limit*4))}
	if _, err := readUpstreamBody(src, limit); err == nil {
		t.Fatal("readUpstreamBody accepted a body past the ceiling")
	} else if src.n > limit+1 {
		t.Fatalf("read %d bytes under a %d byte ceiling: the read is unbounded, "+
			"the refusal merely happens afterwards", src.n, limit)
	}
	if res.Err.Code != CodeUpstreamTooLarge {
		t.Errorf("code = %q, want %q", res.Err.Code, CodeUpstreamTooLarge)
	}
	if res.Retryable {
		t.Error("a fail-back hop would buffer another one; this must not be retryable")
	}
	if DefaultMaxResponseBytes <= 0 {
		t.Error("the shipped default must be a real ceiling, not zero")
	}

	// And a body under the ceiling still goes through, so the bound is a bound
	// and not a refusal of everything.
	f.answer(http.StatusOK, `{"id":"x","choices":[]}`)
	if res := be.Do(context.Background(), target(p), chatCall(catalog.APIOpenAIChat), nil); res.Err != nil {
		t.Fatalf("a response inside the ceiling was refused: %v", res.Err)
	}
}

// TestUnreachableUpstreamDoesNotNameInternalHosts is the review's MEDIUM on
// transport error text, applied to L5.
//
// internal/server/passthrough.go has refused to relay it since the relay was
// written, and internal/app/dispatch.go was changed to match. This package
// relayed `err.Error()` — which for a dial failure is `dial tcp 10.0.3.14:8000:
// connect: connection refused`, an outline of the operator's internal network
// handed to anyone holding an ordinary key.
func TestUnreachableUpstreamDoesNotNameInternalHosts(t *testing.T) {
	// A host that resolves to nothing, on a port nobody serves. The address is
	// what must not come back.
	const addr = "127.0.0.1:1"
	p, err := NewProvider(Spec{Name: "p1", Kind: "openai", API: catalog.APIOpenAIChat,
		BaseURL: "http://" + addr})
	if err != nil {
		t.Fatal(err)
	}
	res := testBackend(plantedSecret).Do(context.Background(), target(p),
		chatCall(catalog.APIOpenAIChat), nil)
	if res.Err == nil {
		t.Fatal("want an error")
	}
	if res.Err.Code != CodeUpstreamUnreachable {
		t.Fatalf("code = %q, want %q", res.Err.Code, CodeUpstreamUnreachable)
	}
	for _, leak := range []string{addr, "127.0.0.1", "dial tcp", "connection refused"} {
		if strings.Contains(res.Err.Message, leak) {
			t.Errorf("the client-facing message names %q: %q", leak, res.Err.Message)
		}
	}
	if strings.Contains(string(server.EncodeError(res.Err)), addr) {
		t.Errorf("the rendered envelope names the upstream address")
	}
}

// countingReader records how many bytes a reader was actually asked for.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestOpaqueRefusalKeepsItsMachineReadableHalf.
//
// [anthropic.OpaqueError] is dorang declining to fabricate opaque state, and it
// carries two fields for the caller: Reason, which its own ToError renders as
// the wire code, and Construct, which exists so the caller "can put it in
// x-dorang-allow-lossy and retry deliberately". encodeError used to flatten the
// whole thing into a sentence under the generic conversion_failed code, with no
// Unwrap — so the one construct id the retry mechanism takes never reached
// anybody, and the mechanism could not be used.
func TestOpaqueRefusalKeepsItsMachineReadableHalf(t *testing.T) {
	oe := &anthropic.OpaqueError{
		Reason:    anthropic.ReasonUnsignedThinking,
		Construct: canonical.ConstructThinkingBlock,
		Detail:    "messages[1].content[0]",
	}

	e := encodeError(oe)

	if e.Code != anthropic.ReasonUnsignedThinking {
		t.Errorf("code = %q, want the reason %q: a caller branching on the code has to be able "+
			"to name what was refused", e.Code, anthropic.ReasonUnsignedThinking)
	}
	if e.Code == CodeConversionFailed {
		t.Error("the refusal is still flattened under the generic code")
	}
	if !strings.Contains(e.Message, canonical.ConstructThinkingBlock) {
		t.Errorf("the construct id x-dorang-allow-lossy takes is not in the refusal: %q", e.Message)
	}
	if e.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: no sibling deployment accepts it either", e.Status)
	}

	// Everything else still lands where it did.
	if got := encodeError(errNilRequest); got.Code != CodeConversionFailed {
		t.Errorf("an ordinary encode failure now reports %q", got.Code)
	}
}
