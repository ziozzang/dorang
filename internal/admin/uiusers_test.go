package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The users screen renders the directory and its create forms for an operator
// who can mutate.
func TestUsersScreenRenders(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	c, _ := signInUI(t, h, adminToken)

	rec := uiGet(h, "/ui/users", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/users = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Users &amp; teams", `action" value="create_user"`, `action" value="create_team"`, `action" value="add_member"`} {
		if !strings.Contains(body, want) {
			t.Errorf("users screen missing %q", want)
		}
	}
	// The nav offers the screen.
	if !strings.Contains(body, `href="/ui/users"`) {
		t.Error("the nav does not link the users screen")
	}
}

// A user is created, blocked, then deleted through the UI action dispatcher,
// and each change is visible on the next render — the UI reads and writes the
// same store the API does.
func TestUsersScreenCreateBlockDelete(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	c, token := signInUI(t, h, adminToken)

	// create
	rec := uiPost(h, "/ui"+uiActionPath, url.Values{
		"csrf": {token}, "action": {"create_user"}, "return": {"/ui/users"},
		"user_email": {"alex@example.com"}, "user_name": {"Alex"}, "user_role": {"internal_user"},
		"max_budget": {"250"}, "rpm_limit": {"600"},
	}, c)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/users" {
		t.Fatalf("create_user = %d loc=%q\n%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	users, _ := h.store.ListUsers(t.Context(), ListOptions{Limit: 100})
	var uid string
	for _, u := range users {
		if u.Email == "alex@example.com" {
			uid = u.ID
			if u.MaxBudgetNano == nil || *u.MaxBudgetNano != 250_000_000_000 {
				t.Errorf("budget did not cross as nano: %+v", u.MaxBudgetNano)
			}
			if u.RPMLimit == nil || *u.RPMLimit != 600 {
				t.Errorf("rpm did not cross: %+v", u.RPMLimit)
			}
		}
	}
	if uid == "" {
		t.Fatalf("the created user is not in the store: %+v", users)
	}
	if body := uiGet(h, "/ui/users", c).Body.String(); !strings.Contains(body, "alex@example.com") {
		t.Error("the created user does not appear on the screen")
	}

	// block
	rec = uiPost(h, "/ui"+uiActionPath, url.Values{
		"csrf": {token}, "action": {"block_user"}, "user_id": {uid}, "return": {"/ui/users"},
	}, c)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("block_user = %d", rec.Code)
	}
	if u, _ := h.store.GetUser(t.Context(), uid); u == nil || !u.Blocked {
		t.Errorf("user not blocked: %+v", u)
	}

	// the delete confirmation is a GET that writes nothing
	if body := uiGet(h, "/ui/users?del_user="+uid, c).Body.String(); !strings.Contains(body, "Delete this user") {
		t.Error("the delete confirmation did not render")
	}
	if u, _ := h.store.GetUser(t.Context(), uid); u == nil {
		t.Error("the confirmation GET deleted the user; it must only render")
	}

	// delete
	rec = uiPost(h, "/ui"+uiActionPath, url.Values{
		"csrf": {token}, "action": {"delete_user"}, "user_id": {uid}, "return": {"/ui/users"},
	}, c)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete_user = %d\n%s", rec.Code, rec.Body.String())
	}
	if u, _ := h.store.GetUser(t.Context(), uid); u != nil {
		t.Errorf("user survived delete: %+v", u)
	}
}

// The user edit form is prefilled and writes through /user/update under the
// same end-state semantics as the key edit form: a kept field keeps its value,
// an emptied clearable field is reset. Revert check: stop the form clearing an
// emptied budget and the budget assertion fails.
func TestEditUserFromTheUISetsAndClearsFields(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(h)
	c, token := signInUI(t, h, adminToken)

	rec := uiPost(h, "/ui"+uiActionPath, url.Values{
		"csrf": {token}, "action": {"create_user"}, "return": {"/ui/users"},
		"user_email": {"edit@example.com"}, "user_name": {"Editable"}, "user_role": {"internal_user"},
		"max_budget": {"250"}, "rpm_limit": {"600"},
	}, c)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create_user = %d\n%s", rec.Code, rec.Body.String())
	}
	users, _ := h.store.ListUsers(t.Context(), ListOptions{Limit: 100})
	var uid string
	for _, u := range users {
		if u.Email == "edit@example.com" {
			uid = u.ID
		}
	}
	if uid == "" {
		t.Fatal("created user not found")
	}

	page := uiGet(h, "/ui/users/edit_user?user_id="+uid, c)
	if page.Code != http.StatusOK {
		t.Fatalf("GET edit_user = %d\n%s", page.Code, page.Body.String())
	}
	for _, want := range []string{`value="edit@example.com"`, `value="250.00"`, `value="600"`} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("edit form not prefilled with %q", want)
		}
	}

	// Raise rpm, keep email/role, empty the budget (clears it).
	rec = uiPost(h, "/ui"+uiActionPath, url.Values{
		"csrf": {token}, "action": {"update_user"}, "user_id": {uid}, "return": {"/ui/users"},
		"user_email": {"edit@example.com"}, "user_role": {"internal_user"},
		"rpm_limit": {"900"}, "max_budget": {""},
	}, c)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update_user = %d\n%s", rec.Code, rec.Body.String())
	}
	u, _ := h.store.GetUser(t.Context(), uid)
	if u == nil || u.RPMLimit == nil || *u.RPMLimit != 900 {
		t.Errorf("rpm not updated to 900: %+v", u)
	}
	if u != nil && u.MaxBudgetNano != nil {
		t.Errorf("emptied budget box did not clear the budget: %d", *u.MaxBudgetNano)
	}
}

// The screen is refused where no directory is configured, rather than 500ing.
func TestUsersScreenNeedsDirectory(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Directory = nil })
	seedAdminSession(h)
	c, _ := signInUI(t, h, adminToken)
	rec := uiGet(h, "/ui/users", c)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /ui/users with no directory = %d, want 501", rec.Code)
	}
}
