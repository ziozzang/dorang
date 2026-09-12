package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/wire/bedrock"
	"github.com/ziozzang/dorang/pkg/catalog"
)

type bedrockHostRec struct {
	mu             sync.Mutex
	path, auth, ct string
	amzDate        string
	sha            string
	token          string
	body           []byte
}

func serveBedrock(t *testing.T, f *fakeUpstream) *bedrockHostRec {
	t.Helper()
	h := &bedrockHostRec{}
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.path, h.auth, h.ct = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		h.amzDate, h.sha = r.Header.Get("X-Amz-Date"), r.Header.Get("X-Amz-Content-Sha256")
		h.token = r.Header.Get("X-Amz-Security-Token")
		h.body = f.last().body
		h.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/converse-stream") {
			// A real converse-stream reply is the binary framing under this
			// content type, which is what keeps it off the buffered path.
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			w.Write(bedrock.EncodeEventMessage(map[string]string{":message-type": "event", ":event-type": "messageStart", ":content-type": "application/json"}, []byte(`{"role":"assistant"}`)))
			w.Write(bedrock.EncodeEventMessage(map[string]string{":message-type": "event", ":event-type": "contentBlockDelta", ":content-type": "application/json"}, []byte(`{"contentBlockIndex":0,"delta":{"text":"yes"}}`)))
			w.Write(bedrock.EncodeEventMessage(map[string]string{":message-type": "event", ":event-type": "messageStop", ":content-type": "application/json"}, []byte(`{"stopReason":"end_turn"}`)))
			w.Write(bedrock.EncodeEventMessage(map[string]string{":message-type": "event", ":event-type": "metadata", ":content-type": "application/json"}, []byte(`{"usage":{"inputTokens":3,"outputTokens":1,"totalTokens":4}}`)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"output":{"message":{"role":"assistant","content":[{"text":"yes"}]}},"stopReason":"end_turn","usage":{"inputTokens":3,"outputTokens":1,"totalTokens":4}}`))
	})
	return h
}

func bedrockProvider(t *testing.T, f *fakeUpstream, region, token string) *Provider {
	t.Helper()
	p, err := NewProvider(Spec{Name: "br", Kind: "bedrock", API: catalog.APIBedrock, BaseURL: f.srv.URL,
		BedrockRegion: region, BedrockAccessKeyID: "AKIDEXAMPLE", BedrockSessionToken: token})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

// A Converse call is signed and answered.
//
// The observables are what the host received: the Converse route with the
// model in the path, a SigV4 Authorization scoped to the region and service,
// the payload hash header that the signature covers, and — because the secret
// access key is the credential — no bearer or api-key anywhere. The body is
// the Converse shape, and the answer comes back as the neutral one.
func TestABedrockCallIsSignedAndAnswered(t *testing.T) {
	f := newFakeUpstream(t)
	host := serveBedrock(t, f)
	b := testBackend("aws-secret-key")
	res := b.Do(context.Background(), target(bedrockProvider(t, f, "us-east-1", "")), chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if host.path != "/model/upstream-model/converse" {
		t.Errorf("host saw %q, want the Converse route with the model in the path", host.path)
	}
	if !strings.HasPrefix(host.auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") ||
		!strings.Contains(host.auth, "/us-east-1/bedrock/aws4_request") {
		t.Errorf("Authorization = %q", host.auth)
	}
	if host.amzDate == "" || host.sha == "" {
		t.Error("the signed amz headers did not reach the host")
	}
	// The credential is the SIGNING key, not a header.
	if strings.Contains(host.auth, "aws-secret-key") {
		t.Error("the secret key leaked into the Authorization header")
	}
	var body map[string]any
	if err := json.Unmarshal(host.body, &body); err != nil || body["messages"] == nil {
		t.Errorf("the body is not a Converse request: %s", host.body)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(res.Body, &out); err != nil || len(out.Choices) != 1 || out.Choices[0].Message.Content != "yes" {
		t.Errorf("caller answer: %v\n%s", err, res.Body)
	}
	if res.Usage.InputTokens != 3 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

// A session token is signed and sent.
func TestABedrockSessionTokenIsSignedAndSent(t *testing.T) {
	f := newFakeUpstream(t)
	host := serveBedrock(t, f)
	b := testBackend("aws-secret-key")
	res := b.Do(context.Background(), target(bedrockProvider(t, f, "eu-west-1", "sess-xyz")), chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if host.token != "sess-xyz" {
		t.Errorf("X-Amz-Security-Token = %q", host.token)
	}
	if !strings.Contains(host.auth, "x-amz-security-token") {
		t.Errorf("the session token was not in SignedHeaders: %q", host.auth)
	}
}

// A streaming call takes the converse-stream route and is relayed.
func TestABedrockStreamIsRelayed(t *testing.T) {
	f := newFakeUpstream(t)
	host := serveBedrock(t, f)
	b := testBackend("aws-secret-key")
	c := chatCall(catalog.APIOpenAIChat)
	c.Stream, c.Request.Stream = true, true
	rec := httptest.NewRecorder()
	res := b.Do(context.Background(), target(bedrockProvider(t, f, "us-east-1", "")), c, rec)
	if res.Err != nil {
		t.Fatalf("Do: %v", res.Err)
	}
	if host.path != "/model/upstream-model/converse-stream" {
		t.Errorf("host saw %q", host.path)
	}
	if !strings.Contains(rec.Body.String(), "yes") {
		t.Errorf("the client did not receive the relayed answer:\n%s", rec.Body.String())
	}
	if res.Usage.InputTokens != 3 {
		t.Errorf("usage after the stream: %+v", res.Usage)
	}
}

// The region can come from the base_url, and is required somewhere; the
// signing params are refused off the bedrock kind.
func TestBedrockRegionAndParamRules(t *testing.T) {
	t.Run("region from the host", func(t *testing.T) {
		p, err := NewProvider(Spec{Kind: "bedrock", API: catalog.APIBedrock,
			BaseURL: "https://bedrock-runtime.ap-northeast-2.amazonaws.com", BedrockAccessKeyID: "AKID"})
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		req, _ := http.NewRequest(http.MethodPost, p.BaseURL()+"/model/m/converse", nil)
		ad := bedrockAdapter{}
		if err := ad.signRequest(req, []byte(`{}`), "secret", p, timeFixed()); err != nil {
			t.Fatalf("sign: %v", err)
		}
		if !strings.Contains(req.Header.Get("Authorization"), "/ap-northeast-2/bedrock/") {
			t.Errorf("region not read from the host: %q", req.Header.Get("Authorization"))
		}
	})
	t.Run("default base from the region", func(t *testing.T) {
		p, err := NewProvider(Spec{Kind: "bedrock", API: catalog.APIBedrock, BedrockRegion: "us-west-2", BedrockAccessKeyID: "AKID"})
		if err != nil || p.BaseURL() != "https://bedrock-runtime.us-west-2.amazonaws.com" {
			t.Errorf("base = %q %v", p.BaseURL(), err)
		}
	})
	t.Run("access key id is required", func(t *testing.T) {
		if _, err := NewProvider(Spec{Kind: "bedrock", API: catalog.APIBedrock, BaseURL: "https://x.invalid", BedrockRegion: "us-east-1"}); err == nil {
			t.Error("a bedrock provider without an access key id was accepted")
		}
	})
	t.Run("signing params refused off the kind", func(t *testing.T) {
		if _, err := NewProvider(Spec{Kind: "openai", API: catalog.APIOpenAIChat, BaseURL: "https://x.invalid", BedrockRegion: "us-east-1"}); err == nil || !strings.Contains(err.Error(), "region") {
			t.Errorf("params.region on an openai provider: %v", err)
		}
	})
}

func timeFixed() time.Time { return time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC) }

// A session token echoed in an upstream error does not reach the caller.
//
// collectSecrets reads the headers actually put on the request; the SigV4
// session token rides X-Amz-Security-Token, so that header must be in the
// redaction set or an upstream that quotes the token back in an error hands
// the operator's short-lived AWS credential to the caller.
func TestABedrockSessionTokenDoesNotLeakInAnError(t *testing.T) {
	const token = "sess-secret-abc123" // pragma: allowlist secret -- test fixture
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		// A hostile or careless upstream reflecting the credential it saw.
		w.Write([]byte(`{"message":"invalid security token ` + token + `"}`))
	})
	b := testBackend("aws-secret-key")
	res := b.Do(context.Background(), target(bedrockProvider(t, f, "us-east-1", token)),
		chatCall(catalog.APIOpenAIChat), httptest.NewRecorder())
	if res.Err == nil {
		t.Fatal("a 403 became a success")
	}
	for _, field := range []string{res.Err.Message, res.Err.NativeMessage, string(res.ErrorBody)} {
		if strings.Contains(field, token) {
			t.Fatalf("the session token leaked into an error field: %q", field)
		}
	}
}
