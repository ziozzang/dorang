package admin

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// The users & teams screen.
//
// The administration API has carried the full directory surface — /user/*,
// /team/* and team membership — since v1; this file is the operator UI over
// it. It reads the SAME Directory the API reads (no second data path) and
// mutates through the SAME handlers the API exposes, dispatched by the one
// [uiActionPath] every other UI mutation uses. The read is server-rendered;
// the writes are the create forms and the per-row controls, each a row of
// [uiActions] added in this file's init.
//
// Destructive actions (delete a user, delete a team) are never the control
// under the cursor: the row's link re-renders this screen with a named
// confirmation, and only that confirmation carries the POST — the same "one
// deliberate second action" the keys screen gives a deletion, without a
// separate template.

// screenUsers renders the directory: users, then teams.
func (s *uiServer) screenUsers(w http.ResponseWriter, r *http.Request, v viewer) {
	d := s.api.cfg.Directory
	if d == nil {
		s.renderMessage(w, r, http.StatusNotImplemented, v, "Users & teams",
			"A directory store is not configured in this process, so there are no users or teams to manage.",
			CodeDependencyOff)
		return
	}
	if !v.scope.Global {
		s.renderMessage(w, r, http.StatusForbidden, v, "Users & teams",
			"Managing the directory is a deployment-wide action; this session is scoped to a team.",
			CodeForbidden)
		return
	}

	lim := s.api.cfg.MaxListLimit
	users, uerr := d.ListUsers(r.Context(), ListOptions{Limit: lim})
	if uerr != nil {
		s.renderError(w, r, v, "Users & teams", uerr)
		return
	}
	teams, terr := d.ListTeams(r.Context(), ListOptions{Limit: lim})
	if terr != nil {
		s.renderError(w, r, v, "Users & teams", terr)
		return
	}

	pg := usersPage{
		page:  s.newPage("users", "Users & teams", v),
		Roles: []string{"internal_user", "proxy_admin", "proxy_admin_viewer", "internal_user_viewer"},
	}
	for _, u := range users {
		pg.Users = append(pg.Users, userRow{
			ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role, Blocked: u.Blocked,
			Budget: budgetString(u.MaxBudgetNano, u.BudgetPeriod), Spend: formatNano(u.SpendNano),
			RPM: limitString(u.RPMLimit), TPM: limitString(u.TPMLimit),
			Models: strings.Join(u.Models, ", "),
		})
	}
	sort.Slice(pg.Users, func(i, j int) bool { return pg.Users[i].Email < pg.Users[j].Email })
	for _, t := range teams {
		n := t.Name
		if n == "" {
			n = t.Alias
		}
		members, _ := d.ListTeamMembers(r.Context(), t.ID)
		pg.Teams = append(pg.Teams, teamRow{
			ID: t.ID, Name: n, Alias: t.Alias, Org: t.OrganizationID, Blocked: t.Blocked,
			Budget: budgetString(t.MaxBudgetNano, t.BudgetPeriod), Spend: formatNano(t.SpendNano),
			RPM: limitString(t.RPMLimit), TPM: limitString(t.TPMLimit), Members: len(members),
			Models: strings.Join(t.Models, ", "),
		})
	}
	sort.Slice(pg.Teams, func(i, j int) bool { return pg.Teams[i].Name < pg.Teams[j].Name })

	// The inline delete confirmation. A GET with ?del_user=<id> (or ?del_team)
	// re-renders this same screen with a banner naming the target and the one
	// POST that removes it; nothing is written on the GET.
	if id := strings.TrimSpace(r.URL.Query().Get("del_user")); id != "" {
		pg.Confirm = &deleteConfirm{Kind: "user", Action: "delete_user", Field: "user_id", ID: id, Label: labelFor(pg.Users, id)}
	} else if id := strings.TrimSpace(r.URL.Query().Get("del_team")); id != "" {
		pg.Confirm = &deleteConfirm{Kind: "team", Action: "delete_team", Field: "team_id", ID: id, Label: teamLabelFor(pg.Teams, id)}
	}

	s.render(w, r, http.StatusOK, "users", pg)
}

type usersPage struct {
	page
	Users   []userRow
	Teams   []teamRow
	Roles   []string
	Confirm *deleteConfirm
}

type userRow struct {
	ID, Email, Name, Role   string
	Blocked                 bool
	Budget, Spend, RPM, TPM string
	Models                  string
}

type teamRow struct {
	ID, Name, Alias, Org    string
	Blocked                 bool
	Budget, Spend, RPM, TPM string
	Members                 int
	Models                  string
}

type deleteConfirm struct {
	Kind, Action, Field, ID, Label string
}

func labelFor(rows []userRow, id string) string {
	for _, u := range rows {
		if u.ID == id {
			if u.Email != "" {
				return u.Email
			}
			return u.ID
		}
	}
	return id
}

func teamLabelFor(rows []teamRow, id string) string {
	for _, t := range rows {
		if t.ID == id {
			if t.Name != "" {
				return t.Name
			}
			return t.ID
		}
	}
	return id
}

// budgetString renders "$12.50 / 30d" or "—" when no ceiling is set.
func budgetString(nano *int64, period string) string {
	if nano == nil {
		return "—"
	}
	s := "$" + formatNano(*nano)
	if strings.TrimSpace(period) != "" {
		s += " / " + period
	}
	return s
}

func limitString(v *int64) string {
	if v == nil {
		return "—"
	}
	return strconv.FormatInt(*v, 10)
}

// directoryPresent gates every directory action on the store being configured.
func directoryPresent(s *uiServer) bool { return s.api.cfg.Directory != nil }

// formField reads a trimmed value; ok reports whether it was non-empty.
func formField(f url.Values, k string) (string, bool) {
	v := strings.TrimSpace(f.Get(k))
	return v, v != ""
}

func putStr(m map[string]any, key string, f url.Values, field string) {
	if v, ok := formField(f, field); ok {
		m[key] = v
	}
}

func putInt(m map[string]any, key string, f url.Values, field string) error {
	if v, ok := formField(f, field); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return badRequest("%s must be a whole number", field)
		}
		m[key] = n
	}
	return nil
}

func init() {
	// The directory actions, added to the one action table the dispatcher
	// reads. Each posts to the administration handler the API already serves,
	// so there is one authorization path and one implementation.
	add := func(name string, a uiAction) { uiActions[name] = a }

	add("create_user", uiAction{
		api: "/user/new", verb: "create a user", past: "created",
		needs: directoryPresent,
		body: func(f url.Values) (any, error) {
			email, ok := formField(f, "user_email")
			if !ok {
				return nil, badRequest("an email is required")
			}
			m := map[string]any{"user_email": email}
			putStr(m, "user_name", f, "user_name")
			putStr(m, "user_role", f, "user_role")
			putStr(m, "budget_duration", f, "budget_duration")
			if v, ok := formField(f, "max_budget"); ok {
				m["max_budget"] = v // Money parses a dollar string into nano
			}
			if err := putInt(m, "rpm_limit", f, "rpm_limit"); err != nil {
				return nil, err
			}
			if err := putInt(m, "tpm_limit", f, "tpm_limit"); err != nil {
				return nil, err
			}
			return m, nil
		},
		notice: func(f url.Values, res map[string]any) string {
			if id := stringField(res, "user_id"); id != "" {
				return "User created: " + id + "."
			}
			return "User created."
		},
	})
	add("block_user", uiAction{
		api: "/user/update", verb: "block a user", past: "blocked", needs: directoryPresent,
		body: func(f url.Values) (any, error) {
			return map[string]any{"user_id": f.Get("user_id"), "blocked": true}, nil
		},
		notice: func(f url.Values, _ map[string]any) string { return "User " + f.Get("user_id") + " is blocked." },
	})
	add("unblock_user", uiAction{
		api: "/user/update", verb: "unblock a user", past: "unblocked", needs: directoryPresent,
		body: func(f url.Values) (any, error) {
			return map[string]any{"user_id": f.Get("user_id"), "blocked": false}, nil
		},
		notice: func(f url.Values, _ map[string]any) string { return "User " + f.Get("user_id") + " is unblocked." },
	})
	add("delete_user", uiAction{
		api: "/user/delete", verb: "delete a user", past: "deleted", confirm: true, danger: true, needs: directoryPresent,
		consequence: "The user row is removed. Keys that named it keep working but lose its user-scoped budget and limits.",
		body:        func(f url.Values) (any, error) { return map[string]any{"user_id": f.Get("user_id")}, nil },
		notice:      func(f url.Values, _ map[string]any) string { return "User " + f.Get("user_id") + " is deleted." },
	})

	add("create_team", uiAction{
		api: "/team/new", verb: "create a team", past: "created", needs: directoryPresent,
		body: func(f url.Values) (any, error) {
			name, ok := formField(f, "team_name")
			if !ok {
				return nil, badRequest("a team name is required")
			}
			m := map[string]any{"team_name": name}
			putStr(m, "team_alias", f, "team_alias")
			putStr(m, "organization_id", f, "organization_id")
			putStr(m, "budget_duration", f, "budget_duration")
			if v, ok := formField(f, "max_budget"); ok {
				m["max_budget"] = v
			}
			if err := putInt(m, "rpm_limit", f, "rpm_limit"); err != nil {
				return nil, err
			}
			if err := putInt(m, "tpm_limit", f, "tpm_limit"); err != nil {
				return nil, err
			}
			return m, nil
		},
		notice: func(f url.Values, res map[string]any) string {
			if id := stringField(res, "team_id"); id != "" {
				return "Team created: " + id + "."
			}
			return "Team created."
		},
	})
	add("block_team", uiAction{
		api: "/team/update", verb: "block a team", past: "blocked", needs: directoryPresent,
		body: func(f url.Values) (any, error) {
			return map[string]any{"team_id": f.Get("team_id"), "blocked": true}, nil
		},
		notice: func(f url.Values, _ map[string]any) string { return "Team " + f.Get("team_id") + " is blocked." },
	})
	add("unblock_team", uiAction{
		api: "/team/update", verb: "unblock a team", past: "unblocked", needs: directoryPresent,
		body: func(f url.Values) (any, error) {
			return map[string]any{"team_id": f.Get("team_id"), "blocked": false}, nil
		},
		notice: func(f url.Values, _ map[string]any) string { return "Team " + f.Get("team_id") + " is unblocked." },
	})
	add("delete_team", uiAction{
		api: "/team/delete", verb: "delete a team", past: "deleted", confirm: true, danger: true, needs: directoryPresent,
		consequence: "The team row is removed. Keys and users that named it keep working but lose its team-scoped budget and limits.",
		body:        func(f url.Values) (any, error) { return map[string]any{"team_id": f.Get("team_id")}, nil },
		notice:      func(f url.Values, _ map[string]any) string { return "Team " + f.Get("team_id") + " is deleted." },
	})
	add("add_member", uiAction{
		api: "/team/member_add", verb: "add a team member", past: "added", needs: directoryPresent,
		body: func(f url.Values) (any, error) {
			team, ok := formField(f, "team_id")
			if !ok {
				return nil, badRequest("a team is required")
			}
			user, ok := formField(f, "user_id")
			if !ok {
				return nil, badRequest("a user id is required")
			}
			m := map[string]any{"team_id": team, "user_id": user}
			putStr(m, "role", f, "role")
			return m, nil
		},
		notice: func(f url.Values, _ map[string]any) string { return "Member " + f.Get("user_id") + " added." },
	})
	add("remove_member", uiAction{
		api: "/team/member_delete", verb: "remove a team member", past: "removed", needs: directoryPresent,
		body: func(f url.Values) (any, error) {
			return map[string]any{"team_id": f.Get("team_id"), "user_id": f.Get("user_id")}, nil
		},
		notice: func(f url.Values, _ map[string]any) string { return "Member " + f.Get("user_id") + " removed." },
	})
}

func stringField(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[k].(string); ok {
		return v
	}
	// The user/team handlers nest the created object; try the common shapes.
	for _, wrap := range []string{"user", "team"} {
		if obj, ok := m[wrap].(map[string]any); ok {
			if v, ok := obj[k].(string); ok {
				return v
			}
		}
	}
	return ""
}
