package batch

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// asValidation asserts the error is a validation failure and returns it.
func asValidation(t *testing.T, err error) *ValidationErrors {
	t.Helper()
	if err == nil {
		t.Fatal("want a validation error, got nil")
	}
	var ve *ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationErrors, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("validation error should satisfy errors.Is(err, ErrInvalidRequest)")
	}
	return ve
}

func TestUploadRejectsEachInvalidClass(t *testing.T) {
	good := jsonlRow("req-1", "m1", "/v1/chat/completions", "hello")

	cases := []struct {
		name    string
		content string
		code    string
		line    int
		mention string
	}{
		{
			name:    "not json",
			content: good + "\n{\"custom_id\": broken}\n",
			code:    CodeInvalidJSON,
			line:    2,
		},
		{
			name:    "two values on one line",
			content: good + "\n" + good + " " + good + "\n",
			code:    CodeInvalidJSON,
			line:    2,
		},
		{
			name:    "missing custom_id",
			content: good + "\n" + `{"method":"POST","url":"/v1/chat/completions","body":{"model":"m1"}}` + "\n",
			code:    CodeMissingCustomID,
			line:    2,
		},
		{
			name:    "method is not POST",
			content: good + "\n" + `{"custom_id":"req-2","method":"GET","url":"/v1/chat/completions","body":{"model":"m1"}}` + "\n",
			code:    CodeInvalidMethod,
			line:    2,
			mention: "GET",
		},
		{
			name:    "unknown url",
			content: good + "\n" + `{"custom_id":"req-2","method":"POST","url":"/v1/moderations","body":{"model":"m1"}}` + "\n",
			code:    CodeInvalidURL,
			line:    2,
			mention: "/v1/moderations",
		},
		{
			name:    "missing body",
			content: good + "\n" + `{"custom_id":"req-2","method":"POST","url":"/v1/chat/completions"}` + "\n",
			code:    CodeMissingBody,
			line:    2,
		},
		{
			name:    "body is not an object",
			content: good + "\n" + `{"custom_id":"req-2","method":"POST","url":"/v1/chat/completions","body":[1,2]}` + "\n",
			code:    CodeInvalidBody,
			line:    2,
		},
		{
			name:    "missing model",
			content: good + "\n" + `{"custom_id":"req-2","method":"POST","url":"/v1/chat/completions","body":{"messages":[]}}` + "\n",
			code:    CodeMissingModel,
			line:    2,
		},
		{
			name:    "model no deployment serves",
			content: good + "\n" + jsonlRow("req-2", "nope", "/v1/chat/completions", "hi") + "\n",
			code:    CodeModelNotFound,
			line:    2,
			mention: `"nope"`,
		},
		{
			name:    "empty file",
			content: "\n   \n\n",
			code:    CodeEmptyFile,
			line:    0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			_, err := h.svc.UploadFile(context.Background(), UploadRequest{
				Filename: "in.jsonl", Purpose: PurposeBatch, Content: strings.NewReader(tc.content),
			})
			ve := asValidation(t, err)
			if ve.Len() != 1 {
				t.Fatalf("want exactly 1 error, got %d: %v", ve.Len(), ve)
			}
			e := ve.Errs[0]
			if e.Code != tc.code {
				t.Errorf("code = %q, want %q", e.Code, tc.code)
			}
			if e.Line != tc.line {
				t.Errorf("line = %d, want %d", e.Line, tc.line)
			}
			// The message must name the line. A validation failure that does
			// not is unactionable against a 50 000-line file.
			if tc.line > 0 {
				want := "line " + itoa(tc.line) + ":"
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name the offending line (%q)", err.Error(), want)
				}
			}
			if tc.mention != "" && !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.mention)
			}
		})
	}
}

func TestDuplicateCustomIDRejected(t *testing.T) {
	h := newHarness(t, nil)
	content := strings.Join([]string{
		jsonlRow("req-a", "m1", "/v1/chat/completions", "one"),
		jsonlRow("req-b", "m1", "/v1/chat/completions", "two"),
		jsonlRow("req-a", "m1", "/v1/chat/completions", "three"),
	}, "\n") + "\n"

	_, err := h.svc.UploadFile(context.Background(), UploadRequest{
		Filename: "dup.jsonl", Purpose: PurposeBatch, Content: strings.NewReader(content),
	})
	ve := asValidation(t, err)
	if ve.Len() != 1 {
		t.Fatalf("want 1 error, got %d: %v", ve.Len(), ve)
	}
	e := ve.Errs[0]
	if e.Code != CodeDuplicateID {
		t.Errorf("code = %q, want %q", e.Code, CodeDuplicateID)
	}
	if e.Line != 3 {
		t.Errorf("line = %d, want 3", e.Line)
	}
	// Both lines matter: the caller has to look at the pair to decide which one
	// is the mistake.
	msg := err.Error()
	for _, want := range []string{"line 3:", `"req-a"`, "line 1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

func TestValidationReportsEveryBadLine(t *testing.T) {
	h := newHarness(t, nil)
	content := strings.Join([]string{
		jsonlRow("req-1", "m1", "/v1/chat/completions", "ok"),
		`{"custom_id":"req-2","method":"GET","url":"/v1/chat/completions","body":{"model":"m1"}}`,
		jsonlRow("req-3", "nope", "/v1/chat/completions", "bad model"),
		jsonlRow("req-1", "m1", "/v1/chat/completions", "dup"),
	}, "\n") + "\n"

	_, err := h.svc.UploadFile(context.Background(), UploadRequest{
		Purpose: PurposeBatch, Content: strings.NewReader(content),
	})
	ve := asValidation(t, err)
	if ve.Len() != 3 {
		t.Fatalf("want 3 errors, got %d: %v", ve.Len(), ve)
	}
	wantLines := []int{2, 3, 4}
	wantCodes := []string{CodeInvalidMethod, CodeModelNotFound, CodeDuplicateID}
	for i, e := range ve.Errs {
		if e.Line != wantLines[i] || e.Code != wantCodes[i] {
			t.Errorf("error %d = line %d %s, want line %d %s", i, e.Line, e.Code, wantLines[i], wantCodes[i])
		}
	}
}

func TestValidationCeilings(t *testing.T) {
	t.Run("line too large", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.MaxRowBytes = 200 })
		content := jsonlRow("req-1", "m1", "/v1/chat/completions", "short") + "\n" +
			jsonlRow("req-2", "m1", "/v1/chat/completions", strings.Repeat("x", 500)) + "\n"
		_, err := h.svc.UploadFile(context.Background(), UploadRequest{
			Purpose: PurposeBatch, Content: strings.NewReader(content),
		})
		ve := asValidation(t, err)
		if ve.Errs[0].Code != CodeLineTooLarge || ve.Errs[0].Line != 2 {
			t.Fatalf("got %s on line %d, want %s on line 2", ve.Errs[0].Code, ve.Errs[0].Line, CodeLineTooLarge)
		}
		if !strings.Contains(err.Error(), "line 2:") {
			t.Errorf("error %q does not name the line", err.Error())
		}
	})

	t.Run("too many rows", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.MaxRows = 2 })
		_, err := h.svc.UploadFile(context.Background(), UploadRequest{
			Purpose: PurposeBatch, Content: strings.NewReader(jsonlFile(3, "m1")),
		})
		ve := asValidation(t, err)
		if ve.Errs[0].Code != CodeTooManyRequests {
			t.Fatalf("code = %s, want %s", ve.Errs[0].Code, CodeTooManyRequests)
		}
		if ve.Errs[0].Line != 3 {
			t.Errorf("line = %d, want 3 (the row that crossed the ceiling)", ve.Errs[0].Line)
		}
	})

	t.Run("file too large", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.MaxFileBytes = 100 })
		_, err := h.svc.UploadFile(context.Background(), UploadRequest{
			Purpose: PurposeBatch, Content: strings.NewReader(jsonlFile(10, "m1")),
		})
		ve := asValidation(t, err)
		if ve.Errs[0].Code != CodeFileTooLarge {
			t.Fatalf("code = %s, want %s", ve.Errs[0].Code, CodeFileTooLarge)
		}
		// Nothing oversized may be left behind: the ceiling is also the disk an
		// upload can consume.
		if n := h.blobs.count(); n != 0 {
			t.Errorf("%d blobs left after a rejected upload, want 0", n)
		}
	})

	t.Run("custom_id too long", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.MaxCustomIDChars = 8 })
		content := jsonlRow(strings.Repeat("i", 9), "m1", "/v1/chat/completions", "hi") + "\n"
		_, err := h.svc.UploadFile(context.Background(), UploadRequest{
			Purpose: PurposeBatch, Content: strings.NewReader(content),
		})
		ve := asValidation(t, err)
		if ve.Errs[0].Code != CodeCustomIDTooLong {
			t.Fatalf("code = %s, want %s", ve.Errs[0].Code, CodeCustomIDTooLong)
		}
	})
}

// A batch whose rows target a different endpoint than the batch cannot be
// caught at upload — the file does not belong to a batch yet. It fails the
// batch, with the errors list naming the line, which is what the vendor does.
func TestEndpointMismatchFailsTheBatch(t *testing.T) {
	h := newHarness(t, nil)
	content := jsonlRow("req-1", "m1", "/v1/chat/completions", "hi") + "\n" +
		jsonlRow("req-2", "m1", "/v1/embeddings", "hi") + "\n"
	fid := h.upload(content)

	b := h.create(fid)
	final := h.await(b.ID, StatusFailed)
	if final.Errors == nil || len(final.Errors.Data) != 1 {
		t.Fatalf("want one error on the batch, got %+v", final.Errors)
	}
	e := final.Errors.Data[0]
	if e.Code != CodeEndpointMismatch {
		t.Errorf("code = %s, want %s", e.Code, CodeEndpointMismatch)
	}
	if e.Line != 2 {
		t.Errorf("line = %d, want 2", e.Line)
	}
	if !strings.Contains(e.Message, "line 2") {
		t.Errorf("message %q does not name the line", e.Message)
	}
	if h.exec.totalCalls() != 0 {
		t.Errorf("a batch that failed validation executed %d rows, want 0", h.exec.totalCalls())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
