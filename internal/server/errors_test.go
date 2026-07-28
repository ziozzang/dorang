package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestErrorEnvelopeGolden fixes the exact bytes. COMPATIBILITY §7.1 is a
// four-key object in a fixed order with a string code and a nullable param, and
// "byte-for-byte" means the separators and the escaping too (§2.1a).
func TestErrorEnvelopeGolden(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{
			name: "no param renders null",
			err:  NewError(http.StatusUnauthorized, TypeAuthentication, "invalid key"),
			want: `{"error":{"message":"invalid key","type":"authentication_error","param":null,"code":"401"}}`,
		},
		{
			name: "param renders as a string",
			err: NewError(http.StatusBadRequest, TypeInvalidRequest, "missing field").
				WithParam("model").WithCode("missing_model"),
			want: `{"error":{"message":"missing field","type":"invalid_request_error","param":"model","code":"missing_model"}}`,
		},
		{
			// Go's encoding/json would render this as && and
			// <b>. No other server does that, and a client diffing
			// wire bytes against OpenAI sees a divergence in every error it
			// ever receives.
			name: "HTML escaping is off",
			err:  NewError(http.StatusBadRequest, TypeInvalidRequest, `a && b, <b>c</b>`),
			want: `{"error":{"message":"a && b, <b>c</b>","type":"invalid_request_error","param":null,"code":"400"}}`,
		},
		{
			name: "non-ASCII stays raw UTF-8",
			err:  NewError(http.StatusBadRequest, TypeInvalidRequest, "모델을 찾을 수 없습니다 🚀"),
			want: `{"error":{"message":"모델을 찾을 수 없습니다 🚀","type":"invalid_request_error","param":null,"code":"400"}}`,
		},
		{
			name: "control characters and quotes are escaped",
			err:  NewError(http.StatusBadRequest, TypeInvalidRequest, "line\nbreak \"quoted\" \x01"),
			want: `{"error":{"message":"line\nbreak \"quoted\" \u0001","type":"invalid_request_error","param":null,"code":"400"}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(EncodeError(c.err)); got != c.want {
				t.Errorf("envelope bytes\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestNormalizeEveryUpstreamShape runs the five envelope shapes SGLANG.md §6.2
// found on one server, vLLM's integer code (VLLM.md §2.2), FastAPI's validation
// detail, and bodies that are not error envelopes at all. Every one produces the
// same four-key object with a string code.
func TestNormalizeEveryUpstreamShape(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
		shape  Shape
		native string
	}{
		{
			name:   "openai nested",
			status: 400,
			body:   `{"error":{"message":"bad model","type":"invalid_request_error","param":"model","code":"model_not_found"}}`,
			want:   `{"error":{"message":"bad model","type":"invalid_request_error","param":"model","code":"model_not_found"}}`,
			shape:  ShapeNested,
		},
		{
			// vLLM: integer code, Python exception name as the type. An OpenAI
			// SDK branching on a string code raises before the message is ever
			// surfaced.
			name:   "nested with an integer code and a python type",
			status: 400,
			body:   `{"error":{"message":"bad request","type":"BadRequestError","param":null,"code":400}}`,
			want:   `{"error":{"message":"bad request","type":"invalid_request_error","param":null,"code":"400"}}`,
			shape:  ShapeNested,
			native: "BadRequestError",
		},
		{
			// SGLang shape (a): flat. An OpenAI SDK reads
			// body["error"]["message"]; against this, that key does not exist.
			name:   "flat with object=error",
			status: 400,
			body:   `{"object":"error","message":"flat message","type":"BadRequest","param":null,"code":400}`,
			want:   `{"error":{"message":"flat message","type":"invalid_request_error","param":null,"code":"400"}}`,
			shape:  ShapeFlat,
			native: "BadRequest",
		},
		{
			// The same server, streaming: nested. Two shapes, one process.
			name:   "flat and nested agree after normalization",
			status: 400,
			body:   `{"error":{"message":"flat message","type":"Bad Request","param":null,"code":400}}`,
			want:   `{"error":{"message":"flat message","type":"invalid_request_error","param":null,"code":"400"}}`,
			shape:  ShapeNested,
			native: "Bad Request",
		},
		{
			// SGLang shape (d): Anthropic-shaped, from /v1/messages on the
			// same deployment.
			name:   "anthropic",
			status: 400,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens is required"}}`,
			want:   `{"error":{"message":"max_tokens is required","type":"invalid_request_error","param":null,"code":"400"}}`,
			shape:  ShapeAnthropic,
		},
		{
			// SGLang shape (e): a bare string from the auth middleware.
			name:   "bare string",
			status: 401,
			body:   `{"error": "Unauthorized"}`,
			want:   `{"error":{"message":"Unauthorized","type":"authentication_error","param":null,"code":"401"}}`,
			shape:  ShapeBareString,
		},
		{
			name:   "fastapi detail string",
			status: 422,
			body:   `{"detail":"field required"}`,
			want:   `{"error":{"message":"field required","type":"invalid_request_error","param":null,"code":"422"}}`,
			shape:  ShapeDetail,
		},
		{
			name:   "fastapi detail array",
			status: 422,
			body:   `{"detail":[{"loc":["body","model"],"msg":"field required","type":"value_error.missing"}]}`,
			want:   `{"error":{"message":"[{\"loc\":[\"body\",\"model\"],\"msg\":\"field required\",\"type\":\"value_error.missing\"}]","type":"invalid_request_error","param":null,"code":"422"}}`,
			shape:  ShapeDetail,
		},
		{
			name:   "type is stringified status",
			status: 400,
			body:   `{"object":"error","message":"m","type":"400","param":null,"code":"400"}`,
			want:   `{"error":{"message":"m","type":"invalid_request_error","param":null,"code":"400"}}`,
			shape:  ShapeFlat,
			native: "400",
		},
		{
			name:   "html from a load balancer",
			status: 502,
			body:   "<html><head><title>502 Bad Gateway</title></head></html>",
			want:   `{"error":{"message":"Bad Gateway: <html><head><title>502 Bad Gateway</title></head></html>","type":"api_error","param":null,"code":"502"}}`,
			shape:  ShapeOpaque,
		},
		{
			name:   "empty body",
			status: 504,
			body:   "",
			want:   `{"error":{"message":"Gateway Timeout","type":"timeout_error","param":null,"code":"504"}}`,
			shape:  ShapeOpaque,
		},
		{
			name:   "json that is not an error",
			status: 500,
			body:   `{"result":"surprise"}`,
			want:   `{"error":{"message":"Internal Server Error: {\"result\":\"surprise\"}","type":"api_error","param":null,"code":"500"}}`,
			shape:  ShapeOpaque,
		},
		{
			name:   "code as an object is carried rather than dropped",
			status: 400,
			body:   `{"error":{"message":"m","type":"invalid_request_error","code":{"inner":"x"}}}`,
			want:   `{"error":{"message":"m","type":"invalid_request_error","param":null,"code":"{\"inner\":\"x\"}"}}`,
			shape:  ShapeNested,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := Normalize(c.status, []byte(c.body))
			if got := string(EncodeError(e)); got != c.want {
				t.Errorf("normalized\n got %s\nwant %s", got, c.want)
			}
			if e.Shape != c.shape {
				t.Errorf("shape %v, want %v", e.Shape, c.shape)
			}
			if e.NativeType != c.native {
				t.Errorf("native type %q, want %q", e.NativeType, c.native)
			}
			if e.Status != c.status {
				t.Errorf("status %d, want %d", e.Status, c.status)
			}
		})
	}
}

// TestNormalizeNeverEmitsANumericCode is the invariant §7.1 exists for, checked
// against every numeric spelling a backend might use.
func TestNormalizeNeverEmitsANumericCode(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"m","code":429}}`,
		`{"object":"error","message":"m","code":-1}`,
		`{"error":{"message":"m","code":0}}`,
		`{"error":{"message":"m","code":1.5}}`,
	} {
		e := Normalize(429, []byte(body))
		if e.Code == "" {
			t.Fatalf("%s: empty code", body)
		}
		if !e.NativeCodeWasNumeric {
			t.Errorf("%s: numeric code not flagged", body)
		}
		enc := string(EncodeError(e))
		// The code is inside quotes in the rendered envelope, always.
		if !containsQuotedCode(enc) {
			t.Errorf("%s: code is not a JSON string in %s", body, enc)
		}
	}
}

func containsQuotedCode(s string) bool {
	i := indexOf(s, `"code":`)
	if i < 0 {
		return false
	}
	return i+7 < len(s) && s[i+7] == '"'
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestWriteErrorSetsRetryAfterAndNativeType checks the two out-of-band signals
// a normalized error carries.
func TestWriteErrorSetsRetryAfterAndNativeType(t *testing.T) {
	e := Normalize(429, []byte(`{"object":"error","message":"slow down","type":"RateLimitError","code":429}`))
	e.RetryAfterSeconds = 7
	w := httptest.NewRecorder()
	WriteError(w, e)

	if w.Code != 429 {
		t.Fatalf("status %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After %q, want 7", got)
	}
	if got := w.Header().Get(HeaderNativeErrorType); got != "RateLimitError" {
		t.Errorf("native error type %q, want RateLimitError", got)
	}
	want := `{"error":{"message":"slow down","type":"rate_limit_error","param":null,"code":"429"}}`
	if got := w.Body.String(); got != want {
		t.Errorf("body\n got %s\nwant %s", got, want)
	}
}

// TestSSEErrorFraming is COMPATIBILITY §1.3: a mid-stream error is delivered in
// band, followed by the terminator. There is no other channel — the status went
// out with the first frame and is already 200.
func TestSSEErrorFraming(t *testing.T) {
	e := NewError(http.StatusBadGateway, TypeAPIError, "upstream died")
	got := string(appendSSEError(nil, e))
	want := "data: {\"error\":{\"message\":\"upstream died\",\"type\":\"api_error\",\"param\":null,\"code\":\"502\"}}\n\ndata: [DONE]\n\n"
	if got != want {
		t.Errorf("frames\n got %q\nwant %q", got, want)
	}
}

// TestAppendNanoUSD covers the money formatter, which never sees a float.
func TestAppendNanoUSD(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{1, "0.000000001"},
		{123456, "0.000123456"},
		{1_500_000_000, "1.5"},
		{1_000_000_000, "1"},
		{-250_000_000, "-0.25"},
		{999_999_999_999, "999.999999999"},
	}
	for _, c := range cases {
		if got := string(appendNanoUSD(nil, c.in)); got != c.want {
			t.Errorf("appendNanoUSD(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
