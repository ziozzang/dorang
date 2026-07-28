package admin

import (
	"net/http"
	"strings"
	"testing"
)

func TestBudgetLifecycleBySubject(t *testing.T) {
	h := newHarness(t)

	body := h.expectStatus(h.do(http.MethodPost, "/budget/new", map[string]any{
		"subject_kind": "team", "subject_id": "eng",
		"max_budget": 250, "soft_budget": 200, "budget_duration": "monthly",
	}), http.StatusOK)
	b := body["budget"].(map[string]any)
	if b["budget_id"] != "team:eng" {
		t.Fatalf("budget_id = %v", b["budget_id"])
	}
	if b["max_budget"] != 250.0 || b["remaining"] != 250.0 {
		t.Fatalf("budget = %v", b)
	}

	e, _ := h.store.lastAudit()
	if e.Action != "budget.new" || e.ObjectID != "team:eng" || e.Before != "" {
		t.Fatalf("audit = %+v", e)
	}

	// Creating twice is a conflict; updating something that does not exist is
	// a 404. The two endpoints differ in exactly that, deliberately.
	h.expectFault(h.do(http.MethodPost, "/budget/new", map[string]any{
		"budget_id": "team:eng", "max_budget": 300,
	}), http.StatusConflict, CodeConflict)

	h.expectFault(h.do(http.MethodPost, "/budget/update", map[string]any{
		"budget_id": "team:nobody", "max_budget": 300,
	}), http.StatusNotFound, CodeNotFound)

	body = h.expectStatus(h.do(http.MethodPost, "/budget/update", map[string]any{
		"budget_id": "team:eng", "max_budget": 300,
	}), http.StatusOK)
	if body["budget"].(map[string]any)["max_budget"] != 300.0 {
		t.Fatalf("update did not take: %v", body)
	}
	e, _ = h.store.lastAudit()
	if !strings.Contains(e.Before, "250") || !strings.Contains(e.After, "300") {
		t.Fatalf("update audit does not carry both ceilings: %s / %s", e.Before, e.After)
	}

	body = h.expectStatus(h.do(http.MethodGet, "/budget/info?budget_id=team:eng", nil), http.StatusOK)
	if body["budget"].(map[string]any)["subject_kind"] != "team" {
		t.Fatalf("info = %v", body)
	}

	body = h.expectStatus(h.do(http.MethodGet, "/budget/list", nil), http.StatusOK)
	if len(body["budgets"].([]any)) != 1 {
		t.Fatalf("list = %v", body)
	}

	h.expectStatus(h.do(http.MethodPost, "/budget/delete",
		map[string]any{"budget_id": "team:eng"}), http.StatusOK)
	e, _ = h.store.lastAudit()
	// Clearing a budget removes the ceiling and not the spend; the trail must
	// say which happened.
	if !strings.Contains(e.After, "spend_preserved") {
		t.Errorf("delete audit does not record that spend survives: %s", e.After)
	}
	body = h.expectStatus(h.do(http.MethodGet, "/budget/info?budget_id=team:eng", nil), http.StatusOK)
	if body["budget"].(map[string]any)["max_budget"] != nil {
		t.Fatalf("ceiling survived the delete: %v", body)
	}
}

// The subject-shaped budget id is the one place the request body deviates from
// the incumbent, so the refusal explains itself rather than saying "invalid".
func TestBudgetIDMustNameASubject(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodPost, "/budget/new", map[string]any{
		"budget_id": "my-budget", "max_budget": 10,
	})
	body := h.expectFault(rec, http.StatusBadRequest, CodeInvalidRequest)
	msg := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "subject") {
		t.Errorf("refusal does not explain the shape: %q", msg)
	}
}

func TestBudgetSubjectKindIsClosed(t *testing.T) {
	h := newHarness(t)
	h.expectFault(h.do(http.MethodPost, "/budget/new", map[string]any{
		"subject_kind": "department", "subject_id": "x", "max_budget": 1,
	}), http.StatusBadRequest, CodeInvalidRequest)
}

func TestGlobalBudgetNeedsNoSubjectID(t *testing.T) {
	h := newHarness(t)
	body := h.expectStatus(h.do(http.MethodPost, "/budget/new", map[string]any{
		"subject_kind": "global", "max_budget": 1000,
	}), http.StatusOK)
	if body["budget"].(map[string]any)["budget_id"] != "global:" {
		t.Fatalf("budget_id = %v", body["budget"])
	}
}

func TestBudgetRequiresACeiling(t *testing.T) {
	h := newHarness(t)
	h.expectFault(h.do(http.MethodPost, "/budget/new",
		map[string]any{"budget_id": "user:u1"}), http.StatusBadRequest, CodeInvalidRequest)
}

func TestBudgetRemainingIsNullWhenUnlimited(t *testing.T) {
	h := newHarness(t)
	h.store.budgets[BudgetSubject{Kind: "user", ID: "u1"}] = Budget{
		Subject: BudgetSubject{Kind: "user", ID: "u1"}, SpentNano: 5_000_000_000,
	}
	body := h.expectStatus(h.do(http.MethodGet, "/budget/info?budget_id=user:u1", nil), http.StatusOK)
	b := body["budget"].(map[string]any)
	if b["remaining"] != nil {
		t.Fatalf("an unlimited budget claimed a remaining amount: %v", b["remaining"])
	}
	if b["spend"] != 5.0 {
		t.Fatalf("spend = %v", b["spend"])
	}
}
