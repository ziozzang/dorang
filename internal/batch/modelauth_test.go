package batch

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// An input file naming a model the uploading key may not use is refused, per
// row, at upload.
//
// This is the first half of the critical finding. The batch create body is
// {"input_file_id","endpoint","completion_window","metadata"} — it names no
// model — so the gate scanned "" and skipped the allow-list entirely, and the
// second check (Principal.AllowsModel) lived only in the interactive handler.
// The models a batch actually calls are one per JSONL row, and nothing looked
// at them.
func TestUploadRefusesARowNamingADisallowedModel(t *testing.T) {
	h := newHarness(t, nil)

	// This key may use m1 and nothing else.
	only := func(model string) error {
		if model == "m1" {
			return nil
		}
		return errors.New("this key is not allowed to use the requested model")
	}

	content := line("a", "m1") + line("b", "m2") + line("c", "m1")
	_, err := h.svc.UploadFile(context.Background(), UploadRequest{
		Filename: "in.jsonl", Purpose: PurposeBatch, OwnerKeyID: "key-1",
		Content: strings.NewReader(content), Authorize: only,
	})

	ve := asValidation(t, err)
	if ve.Len() != 1 {
		t.Fatalf("want exactly one refusal, got %d: %v", ve.Len(), ve)
	}
	e := ve.Errs[0]
	if e.Code != CodeModelNotAllowed {
		t.Errorf("code %q, want %q", e.Code, CodeModelNotAllowed)
	}
	if e.Line != 2 {
		t.Errorf("line %d, want 2 — the offending row", e.Line)
	}
	if e.Param != "body.model" {
		t.Errorf("param %q, want body.model", e.Param)
	}
}

// A file every row of which is allowed is accepted.
func TestUploadAcceptsRowsTheKeyMayUse(t *testing.T) {
	h := newHarness(t, nil)
	content := line("a", "m1") + line("b", "m1")
	if _, err := h.svc.UploadFile(context.Background(), UploadRequest{
		Filename: "in.jsonl", Purpose: PurposeBatch, OwnerKeyID: "key-1",
		Content: strings.NewReader(content), Authorize: allowAllModels,
	}); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
}

// A batch input file cannot be accepted at all without an authorizer.
//
// This is the structural half: the omission that produced the bypass — nobody
// asked the allow-list anything — is not a permissive default here, it is a
// refusal. A caller that forgets to pass one gets a 400 rather than an
// unchecked file.
func TestUploadRefusesABatchFileWithNoAuthorizer(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.svc.UploadFile(context.Background(), UploadRequest{
		Filename: "in.jsonl", Purpose: PurposeBatch, OwnerKeyID: "key-1",
		Content: strings.NewReader(line("a", "m1")),
	})
	if err == nil {
		t.Fatal("a batch input file was accepted with no model authorizer")
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}
}

// A non-batch purpose is not a list of model calls and needs no authorizer.
func TestUploadOfANonBatchPurposeNeedsNoAuthorizer(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.UploadPurposes = []string{PurposeBatch, "user_data"}
	})
	if _, err := h.svc.UploadFile(context.Background(), UploadRequest{
		Filename: "notes.txt", Purpose: "user_data", OwnerKeyID: "key-1",
		Content: strings.NewReader("anything at all"),
	}); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
}

// A row is refused for the ALLOW-LIST with a different code than for a model
// no deployment serves, so a refusal does not tell the caller which models
// exist.
func TestModelNotAllowedIsDistinctFromModelNotFound(t *testing.T) {
	if CodeModelNotAllowed == CodeModelNotFound {
		t.Fatal("the two refusals share a code, so one leaks the other's information")
	}
}

// line renders one JSONL row.
func line(id, model string) string {
	return `{"custom_id":"` + id + `","method":"POST","url":"/v1/chat/completions",` +
		`"body":{"model":"` + model + `","messages":[{"role":"user","content":"x"}]}}` + "\n"
}
