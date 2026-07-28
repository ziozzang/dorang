package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// plantedRefresh is the refresh token these tests send. It is long and
// distinctive so that a leak into an error message is unmistakable.
const plantedRefresh = "test-fixture-refresh-token-0123456789abcdef" // pragma: allowlist secret — test fixture

// TestHTTPRefresherExchangesARefreshToken is the standard grant, in both
// spellings, asserted on the wire rather than on the result.
func TestHTTPRefresherExchangesARefreshToken(t *testing.T) {
	for _, tc := range []struct {
		name     string
		encoding RefreshEncoding
		ctype    string
	}{
		{"form", EncodingForm, "application/x-www-form-urlencoded"},
		{"json", EncodingJSON, "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotType, gotGrant, gotRefresh, gotClient, gotSecret string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotType = r.Header.Get("Content-Type")
				body, _ := io.ReadAll(r.Body)
				if strings.HasPrefix(gotType, "application/json") {
					var m map[string]string
					_ = json.Unmarshal(body, &m)
					gotGrant, gotRefresh = m["grant_type"], m["refresh_token"]
					gotClient, gotSecret = m["client_id"], m["client_secret"]
				} else {
					v, _ := url.ParseQuery(string(body))
					gotGrant, gotRefresh = v.Get("grant_type"), v.Get("refresh_token")
					gotClient, gotSecret = v.Get("client_id"), v.Get("client_secret")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"access_token":"test-fixture-minted-access",`+
					`"refresh_token":"test-fixture-minted-refresh","expires_in":3600}`)
			}))
			defer srv.Close()

			now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
			r, err := NewHTTPRefresher(RefreshConfig{
				TokenURL:     srv.URL,
				ClientID:     "test-fixture-client",
				ClientSecret: "test-fixture-client-secret",
				Encoding:     tc.encoding,
				Client:       srv.Client(),
				Now:          func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("NewHTTPRefresher: %v", err)
			}
			tok, err := r.Refresh(context.Background(), Token{Refresh: plantedRefresh})
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if !strings.HasPrefix(gotType, tc.ctype) {
				t.Errorf("content type = %q, want %q", gotType, tc.ctype)
			}
			if gotGrant != "refresh_token" {
				t.Errorf("grant_type = %q", gotGrant)
			}
			if gotRefresh != plantedRefresh {
				t.Errorf("the refresh token did not reach the endpoint")
			}
			if gotClient != "test-fixture-client" || gotSecret != "test-fixture-client-secret" {
				t.Errorf("client credentials not sent: id=%q secret set=%t",
					gotClient, gotSecret != "")
			}
			if tok.Access != "test-fixture-minted-access" || tok.Refresh != "test-fixture-minted-refresh" {
				t.Errorf("the minted token was not decoded")
			}
			if want := now.Add(time.Hour); !tok.ExpiresAt.Equal(want) {
				t.Errorf("expiry = %v, want %v from expires_in", tok.ExpiresAt, want)
			}
		})
	}
}

// TestARefusedExchangeCarriesNoResponseBody. Several token endpoints answer 400
// with the grant they refused quoted in the body, so the body is the one thing
// that must not travel — DESIGN §11.2b's "tokens are secrets" applied to the
// path that is most likely to break the rule, because it is the error path.
func TestARefusedExchangeCarriesNoResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		// The endpoint echoes the whole request, which is the shape that turns a
		// helpful error into a credential disclosure.
		_, _ = fmt.Fprintf(w, `{"error":"invalid_grant","error_description":%q}`, string(body))
	}))
	defer srv.Close()

	r, err := NewHTTPRefresher(RefreshConfig{
		TokenURL: srv.URL, ClientID: "test-fixture-client", Client: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewHTTPRefresher: %v", err)
	}
	_, err = r.Refresh(context.Background(), Token{Refresh: plantedRefresh})
	if err == nil {
		t.Fatal("a 400 was accepted as a token set")
	}
	if strings.Contains(err.Error(), plantedRefresh) {
		t.Fatalf("the refresh token is in the error: %v", err)
	}
	if strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("the response body reached the error: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("the error does not say what the endpoint answered: %v", err)
	}
}

// TestAPlaintextTokenURLIsRefused. The body of this request is a refresh token,
// and a refresh token is the account. A warning would scroll past; a refusal at
// construction is a control.
func TestAPlaintextTokenURLIsRefused(t *testing.T) {
	_, err := NewHTTPRefresher(RefreshConfig{
		TokenURL: "http://auth.example.invalid/token", ClientID: "c",
	})
	if err == nil {
		t.Fatal("a plaintext token endpoint was accepted")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
	// Loopback is allowed: that is where a test's fake endpoint lives, and a
	// listener on the loopback interface is not on a network.
	if _, err := NewHTTPRefresher(RefreshConfig{
		TokenURL: "http://127.0.0.1:9/token", ClientID: "c",
	}); err != nil {
		t.Errorf("a loopback endpoint was refused: %v", err)
	}
}

// TestARefresherWithoutAClientIDIsRefused, because every endpoint that serves a
// public client requires one and the failure otherwise arrives as a 400 an hour
// later, from a background loop, with nothing in the file to explain it.
func TestARefresherWithoutAClientIDIsRefused(t *testing.T) {
	if _, err := NewHTTPRefresher(RefreshConfig{TokenURL: "https://a.invalid/t"}); err == nil {
		t.Fatal("a refresher with no client_id was accepted")
	}
	if _, err := NewHTTPRefresher(RefreshConfig{ClientID: "c"}); err == nil {
		t.Fatal("a refresher with no token_url was accepted")
	}
}

// TestARefresherRedactsItself. It holds a client secret, and this repository's
// rule is that a type holding key material renders as a redaction under every
// verb rather than relying on nobody formatting it.
func TestARefresherRedactsItself(t *testing.T) {
	r, err := NewHTTPRefresher(RefreshConfig{
		TokenURL: "https://auth.example.invalid/token", ClientID: "c",
		ClientSecret: "test-fixture-client-secret-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		fmt.Sprintf("%v", r), fmt.Sprintf("%s", r), fmt.Sprintf("%#v", r),
		fmt.Sprintf("%+v", r), fmt.Sprintf("%q", r), fmt.Sprintf("%d", r),
	} {
		if strings.Contains(s, "test-fixture-client-secret-value") {
			t.Fatalf("the client secret is printable: %s", s)
		}
	}
}

// TestNoRefreshTokenIsNotAnExchange. Posting an empty refresh_token spends a
// round trip to be told what dorang already knew.
func TestNoRefreshTokenIsNotAnExchange(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer srv.Close()
	r, err := NewHTTPRefresher(RefreshConfig{
		TokenURL: srv.URL, ClientID: "c", Client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(context.Background(), Token{Access: "a"}); err == nil {
		t.Fatal("an exchange with no refresh token succeeded")
	}
	if calls != 0 {
		t.Errorf("the endpoint was contacted %d times with nothing to exchange", calls)
	}
}
