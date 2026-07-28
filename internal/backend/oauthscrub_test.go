package backend

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// refreshingApplier is an OAuth credential that mints a NEW token on every
// request, the way a real one does across a refresh.
//
// It is the case [collectSecrets] exists for. This package never sees the token:
// it is applied by code that did not exist when the credential was declared, it
// is not in the credential table, and it is different from the one any earlier
// request used. A scrubber built from anything but the outbound headers has
// nothing to match on.
type refreshingApplier struct{ n atomic.Int64 }

func (a *refreshingApplier) token() string {
	return "oauth-refreshed-token-" + strconv.FormatInt(a.n.Load(), 10)
}

func (a *refreshingApplier) Apply(h http.Header) error {
	a.n.Add(1)
	h.Set("Authorization", "Bearer "+a.token())
	return nil
}

// TestARefreshedOAuthTokenIsScrubbedFromARelayedErrorBody.
//
// Res.ErrorBody is the upstream's own bytes, relayed verbatim apart from the
// scrub — it is what a batch job's output file carries (internal/app/batch.go),
// so an upstream that answers 401 with "Invalid credentials: Bearer …" puts the
// token in a file the client downloads. DESIGN §10.6 rule 4, on the response
// direction.
//
// The assertion is specifically about a token that was NOT the one the
// credential started with: reading the credential table instead of the headers
// would scrub the wrong string and pass every test written against a static key.
func TestARefreshedOAuthTokenIsScrubbedFromARelayedErrorBody(t *testing.T) {
	f := newFakeUpstream(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		// Several OpenAI-compatible servers answer exactly like this.
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid credentials: ` +
			r.Header.Get("Authorization") + `","type":"invalid_request_error"}}`))
	})
	p := testProvider(t, f, "openai", catalog.APIOpenAIChat)

	applier := &refreshingApplier{}
	be := New(Options{Credentials: staticCredentials{
		oauth: map[string]Applier{"c1": applier},
	}})

	res := be.Do(context.Background(), target(p), chatCall(catalog.APIOpenAIChat), nil)
	if res.Err == nil {
		t.Fatal("a 401 was served as a success")
	}
	sent := applier.token()
	if sent == "" {
		t.Fatal("the applier never ran, so this test asserts nothing")
	}

	// The upstream really did echo it, or the assertion below is vacuous.
	if got := f.last().header.Get("Authorization"); got != "Bearer "+sent {
		t.Fatalf("the upstream received %q, want %q", got, "Bearer "+sent)
	}
	if len(res.ErrorBody) == 0 {
		t.Fatal("no upstream error body was relayed, so there is nothing to have scrubbed")
	}
	if !strings.Contains(string(res.ErrorBody), redacted) {
		t.Fatalf("the relayed body was not scrubbed at all: %s", res.ErrorBody)
	}
	for _, artifact := range []struct{ name, body string }{
		{"the relayed upstream error body", string(res.ErrorBody)},
		{"the error message", res.Err.Message},
		{"the upstream's own message", res.Err.NativeMessage},
	} {
		if strings.Contains(artifact.body, sent) {
			t.Errorf("%s carries the refreshed token: %s", artifact.name, artifact.body)
		}
	}
}
