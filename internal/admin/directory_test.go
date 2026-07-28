package admin

import (
	"net/http"
	"strings"
	"testing"
)

func TestUserLifecycleAndAudit(t *testing.T) {
	h := newHarness(t)

	body := h.expectStatus(h.do(http.MethodPost, "/user/new", map[string]any{
		"user_email": "ada@example.test", "user_name": "Ada", "user_role": "admin",
		"max_budget": 100,
	}), http.StatusOK)
	user := body["user"].(map[string]any)
	id := user["user_id"].(string)
	if user["user_email"] != "ada@example.test" || user["user_role"] != "admin" {
		t.Fatalf("user = %v", user)
	}

	e, _ := h.store.lastAudit()
	if e.Action != "user.new" || e.Before != "" || !strings.Contains(e.After, "ada@example.test") {
		t.Fatalf("audit = %+v", e)
	}

	body = h.expectStatus(h.do(http.MethodGet, "/user/info?user_id="+id, nil), http.StatusOK)
	if body["user"].(map[string]any)["user_name"] != "Ada" {
		t.Fatalf("info = %v", body)
	}
	if _, ok := body["keys"]; !ok {
		t.Error("user/info should list the keys the user owns when a key store is configured")
	}

	h.expectStatus(h.do(http.MethodPost, "/user/update", map[string]any{
		"user_id": id, "user_name": "Ada L", "clear": []string{"max_budget"},
	}), http.StatusOK)
	e, _ = h.store.lastAudit()
	if !strings.Contains(e.Before, "Ada\"") || !strings.Contains(e.After, "Ada L") {
		t.Fatalf("update audit does not carry both states: before=%s after=%s", e.Before, e.After)
	}

	body = h.expectStatus(h.do(http.MethodGet, "/user/list", nil), http.StatusOK)
	if len(body["users"].([]any)) != 1 {
		t.Fatalf("list = %v", body)
	}

	body = h.expectStatus(h.do(http.MethodPost, "/user/delete",
		map[string]any{"user_ids": []string{id}}), http.StatusOK)
	if body["deleted"] != 1.0 {
		t.Fatalf("deleted = %v", body["deleted"])
	}
	h.expectFault(h.do(http.MethodGet, "/user/info?user_id="+id, nil),
		http.StatusNotFound, CodeNotFound)
}

func TestUserRequiresEmailAndRejectsUnknownRole(t *testing.T) {
	h := newHarness(t)
	h.expectFault(h.do(http.MethodPost, "/user/new", map[string]any{}),
		http.StatusBadRequest, CodeInvalidRequest)
	h.expectFault(h.do(http.MethodPost, "/user/new",
		map[string]any{"user_email": "x@y.z", "user_role": "superuser"}),
		http.StatusBadRequest, CodeInvalidRequest)
}

func TestDuplicateUserIsAConflict(t *testing.T) {
	h := newHarness(t)
	h.expectStatus(h.do(http.MethodPost, "/user/new",
		map[string]any{"user_email": "dup@example.test"}), http.StatusOK)
	h.expectFault(h.do(http.MethodPost, "/user/new",
		map[string]any{"user_email": "dup@example.test"}), http.StatusConflict, CodeConflict)
}

func TestTeamLifecycleAndMembership(t *testing.T) {
	h := newHarness(t)

	u := h.expectStatus(h.do(http.MethodPost, "/user/new",
		map[string]any{"user_email": "grace@example.test"}), http.StatusOK)
	userID := u["user"].(map[string]any)["user_id"].(string)

	body := h.expectStatus(h.do(http.MethodPost, "/team/new", map[string]any{
		"team_name": "platform", "max_budget": 500, "budget_duration": "monthly",
	}), http.StatusOK)
	team := body["team"].(map[string]any)
	teamID := team["team_id"].(string)

	body = h.expectStatus(h.do(http.MethodPost, "/team/member_add", map[string]any{
		"team_id": teamID, "user_id": userID, "role": "admin",
	}), http.StatusOK)
	members := body["team"].(map[string]any)["members"].([]any)
	if len(members) != 1 || members[0].(map[string]any)["user_id"] != userID {
		t.Fatalf("members = %v", members)
	}
	e, _ := h.store.lastAudit()
	if e.Action != "team.member_add" {
		t.Fatalf("action = %q", e.Action)
	}
	if strings.Contains(e.Before, userID) {
		t.Error("before state already contains the added member")
	}
	if !strings.Contains(e.After, userID) {
		t.Error("after state does not contain the added member")
	}

	// Adding twice is a conflict, not a silent second row.
	h.expectFault(h.do(http.MethodPost, "/team/member_add", map[string]any{
		"team_id": teamID, "user_id": userID,
	}), http.StatusConflict, CodeConflict)

	// The nested member form the incumbent uses is accepted too.
	h.expectStatus(h.do(http.MethodPost, "/team/member_delete", map[string]any{
		"team_id": teamID, "member": map[string]any{"user_id": userID},
	}), http.StatusOK)

	body = h.expectStatus(h.do(http.MethodGet, "/team/info?team_id="+teamID, nil), http.StatusOK)
	if len(body["team"].(map[string]any)["members"].([]any)) != 0 {
		t.Fatal("member was not removed")
	}

	h.expectStatus(h.do(http.MethodGet, "/team/list", nil), http.StatusOK)
	h.expectStatus(h.do(http.MethodPost, "/team/delete",
		map[string]any{"team_ids": []string{teamID}}), http.StatusOK)
	h.expectFault(h.do(http.MethodGet, "/team/info?team_id="+teamID, nil),
		http.StatusNotFound, CodeNotFound)
}

func TestTeamMemberAddRequiresAKnownUser(t *testing.T) {
	h := newHarness(t)
	body := h.expectStatus(h.do(http.MethodPost, "/team/new",
		map[string]any{"team_name": "t"}), http.StatusOK)
	teamID := body["team"].(map[string]any)["team_id"].(string)

	h.expectFault(h.do(http.MethodPost, "/team/member_add",
		map[string]any{"team_id": teamID, "user_id": "nobody"}),
		http.StatusNotFound, CodeNotFound)
}

func TestTeamRequiresName(t *testing.T) {
	h := newHarness(t)
	h.expectFault(h.do(http.MethodPost, "/team/new", map[string]any{}),
		http.StatusBadRequest, CodeInvalidRequest)
}

func TestMetadataMustBeAJSONObject(t *testing.T) {
	h := newHarness(t)
	h.expectFault(h.do(http.MethodPost, "/team/new",
		map[string]any{"team_name": "t", "metadata": "not json"}),
		http.StatusBadRequest, CodeInvalidRequest)
}
