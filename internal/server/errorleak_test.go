package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// providerKey is a fabricated credential in the shape a provider echoes back.
const providerKey = "sk-proj-FAKE0000000000000000000000000000000000000000000000" // pragma: allowlist secret — fabricated

// No upstream error body reaches the client-facing envelope, on any shape.
//
// COMPATIBILITY §11.3 says the upstream's own type, code and message are
// recorded out of band and are NOT put in the response body. Normalize put the
// message in the body on all five branches. The consequence is not stylistic:
// several OpenAI-compatible servers answer 401 with the offending key quoted in
// the message, so any authenticated tenant could read the operator's provider
// credential out of dorang's own error response by sending one request while a
// credential was invalid, revoked, or mid-rotation. A hostile backend — which
// DESIGN §4.4 makes a first-class provider class, self-hosted vLLM and SGLang —
// does not need to wait: it can answer every request with the x-api-key header
// it was just handed.
//
// The four structured branches had no bound at all. The one bounded branch
// clamped at 256 bytes, with a comment claiming that stopped a credential being
// relayed; an sk- key is 51 to 164 characters and fits with room to spare.
func TestUpstreamErrorBodyNeverReachesTheClient(t *testing.T) {
	bodies := []struct {
		name string
		body string
	}{
		{"openai nested", `{"error":{"message":"Invalid API key: ` + providerKey + `","type":"invalid_request_error"}}`},
		{"anthropic", `{"type":"error","error":{"type":"authentication_error","message":"bad key ` + providerKey + `"}}`},
		{"sglang flat", `{"object":"error","message":"Invalid API key: ` + providerKey + `","code":401}`},
		{"sglang bare string", `{"error":"Unauthorized: ` + providerKey + `"}`},
		{"fastapi detail", `{"detail":"header x-api-key=` + providerKey + ` rejected"}`},
		{"opaque html", `<html><body>upstream rejected ` + providerKey + `</body></html>`},
		{"json that is not an error", `{"echo":"` + providerKey + `"}`},
	}
	for _, b := range bodies {
		t.Run(b.name, func(t *testing.T) {
			e := Normalize(http.StatusUnauthorized, []byte(b.body))

			if strings.Contains(e.Message, providerKey) {
				t.Fatalf("the provider credential is in the client-facing message: %q", e.Message)
			}
			if strings.Contains(string(EncodeError(e)), providerKey) {
				t.Fatalf("the provider credential is in the wire envelope: %s", EncodeError(e))
			}
			// And the whole upstream text, not just the key, stays out.
			if e.Message != canonicalMessage(http.StatusUnauthorized) {
				t.Errorf("message %q is not dorang's canonical text for a 401", e.Message)
			}
			// It is still recorded, or an outage becomes undebuggable.
			if e.NativeMessage == "" {
				t.Error("the upstream's message was discarded rather than recorded out of band")
			}
		})
	}
}

// The recorded native text is bounded, so a backend cannot write a megabyte
// into every ledger row.
func TestNativeMessageIsBounded(t *testing.T) {
	huge := strings.Repeat("A", 64<<10)
	e := Normalize(500, []byte(`{"error":{"message":"`+huge+`"}}`))
	if len(e.NativeMessage) > nativeMessageLimit {
		t.Errorf("native message is %d bytes, want at most %d", len(e.NativeMessage), nativeMessageLimit)
	}
}

// An upstream-controlled type string is checked and clamped before it becomes a
// response header value.
//
// The same package refuses to echo a CLIENT-controlled path into a header
// without this check, on the stated reasoning that "the framework probably
// catches it" is not a security argument. An upstream is a strictly less
// trusted source and was getting the weaker treatment: unvalidated and
// unbounded, up to the 1 MiB error-body read.
func TestNativeErrorTypeHeaderIsCheckedAndClamped(t *testing.T) {
	t.Run("CRLF is refused", func(t *testing.T) {
		e := Normalize(400, []byte(`{"error":{"message":"m","type":"a\r\n\r\nX-Injected: yes"}}`))
		w := httptest.NewRecorder()
		WriteError(w, e)
		if got := w.Header().Get(HeaderNativeErrorType); got != "" {
			t.Errorf("a CRLF-bearing native type became a header value: %q", got)
		}
		if w.Header().Get("X-Injected") != "" {
			t.Error("a header was injected through the native error type")
		}
	})

	t.Run("an enormous type is clamped", func(t *testing.T) {
		huge := strings.Repeat("T", 8<<10)
		e := Normalize(400, []byte(`{"error":{"message":"m","type":"`+huge+`"}}`))
		w := httptest.NewRecorder()
		WriteError(w, e)
		if got := w.Header().Get(HeaderNativeErrorType); len(got) > nativeTypeHeaderLimit {
			t.Errorf("native type header is %d bytes, want at most %d", len(got), nativeTypeHeaderLimit)
		}
	})
}
