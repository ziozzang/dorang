package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The incumbent's two directory tables, in the column subset the importer
// reads plus a few it must explain away.
const directoryDDL = `
CREATE TABLE "LiteLLM_TeamTable" (
    team_id TEXT PRIMARY KEY, team_alias TEXT, organization_id TEXT,
    admins TEXT, members TEXT, members_with_roles TEXT, metadata TEXT,
    max_budget REAL, spend REAL, models TEXT, max_parallel_requests INTEGER,
    tpm_limit INTEGER, rpm_limit INTEGER, budget_duration TEXT, budget_reset_at TEXT,
    blocked INTEGER, created_at TEXT, updated_at TEXT, model_spend TEXT, soft_budget REAL
);
CREATE TABLE "LiteLLM_UserTable" (
    user_id TEXT PRIMARY KEY, user_alias TEXT, team_id TEXT, sso_user_id TEXT,
    password TEXT, teams TEXT, user_role TEXT, max_budget REAL, spend REAL,
    user_email TEXT, models TEXT, metadata TEXT, max_parallel_requests INTEGER,
    tpm_limit INTEGER, rpm_limit INTEGER, budget_duration TEXT, budget_reset_at TEXT,
    created_at TEXT, updated_at TEXT
);`

type teamRow struct {
	id, alias, members, models, metadata, budgetPeriod string
	maxBudget, spend                                   float64
	rpm, tpm, maxParallel                              int64
	blocked                                            bool
}

type userRow struct {
	id, alias, email, role, models, budgetPeriod string
	maxBudget                                    float64
	rpm                                          int64
}

func newDirectorySource(t *testing.T, teams []teamRow, users []userRow) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "dir.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(directoryDDL); err != nil {
		t.Fatal(err)
	}
	for _, r := range teams {
		if _, err := db.Exec(`INSERT INTO "LiteLLM_TeamTable"
			(team_id, team_alias, members_with_roles, models, metadata, max_budget, spend, rpm_limit, tpm_limit,
			 max_parallel_requests, budget_duration, blocked, created_at, updated_at, soft_budget)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, r.alias, r.members, r.models, r.metadata, r.maxBudget, r.spend, r.rpm, r.tpm,
			r.maxParallel, r.budgetPeriod, r.blocked, "2025-01-01 00:00:00", "2025-06-01 00:00:00", 0); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range users {
		if _, err := db.Exec(`INSERT INTO "LiteLLM_UserTable"
			(user_id, user_alias, user_email, user_role, models, max_budget, rpm_limit, budget_duration,
			 password, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, r.alias, r.email, r.role, r.models, r.maxBudget, r.rpm, r.budgetPeriod,
			"never-read", "2025-01-01 00:00:00", "2025-06-01 00:00:00"); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// A team crosses with the limits the request path enforces.
//
// The point of the verb: a key referring to this team used to run with no
// team budget, no team rate limit, no team model list and no team blocked
// flag, because nothing created the row. Every one of those is read back
// from the store, and the members named in `members_with_roles` are carried
// when their users exist.
func TestImportTeamsCarriesTheEnforcedLimits(t *testing.T) {
	src := newDirectorySource(t, []teamRow{{
		id: "team-1", alias: "Research", members: `[{"user_id":"user-7","role":"admin"},{"user_id":"ghost","role":"user"}]`,
		models: `["gpt-4o","claude-x"]`, metadata: `{"cost_center":"r1"}`, maxBudget: 12.5, spend: 1.25,
		rpm: 60, tpm: 90000, maxParallel: 4, budgetPeriod: "30d", blocked: true,
	}}, nil)
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		seedUser(t, s, "user-7")
		rep, err := s.ImportTeams(ctx, src, DirectoryImportOptions{Members: true})
		if err != nil {
			t.Fatalf("ImportTeams: %v", err)
		}
		if rep.Imported != 1 || rep.Skipped != 0 {
			t.Fatalf("report: %+v\n%s", rep, rep.Summary())
		}
		got, err := s.GetTeam(ctx, "team-1")
		if err != nil {
			t.Fatalf("the team was not written: %v", err)
		}
		if got.Alias != "Research" || got.Name != "Research" || !got.Blocked {
			t.Errorf("identity or blocked flag lost: %+v", got)
		}
		if got.MaxBudgetNano == nil || *got.MaxBudgetNano != 12_500_000_000 || got.SpendNano != 1_250_000_000 {
			t.Errorf("money did not cross as nano: %+v", got)
		}
		if got.RPMLimit == nil || *got.RPMLimit != 60 || got.TPMLimit == nil || *got.TPMLimit != 90000 ||
			got.MaxParallel == nil || *got.MaxParallel != 4 || got.BudgetPeriod != "30d" {
			t.Errorf("limits did not cross: %+v", got)
		}
		if strings.Join(got.Models, ",") != "gpt-4o,claude-x" {
			t.Errorf("models = %v", got.Models)
		}
		if !strings.Contains(got.Metadata, "cost_center") {
			t.Errorf("metadata = %q", got.Metadata)
		}
		members, err := s.ListTeamMembers(ctx, "team-1")
		if err != nil {
			t.Fatalf("ListTeamMembers: %v", err)
		}
		if len(members) != 1 || members[0].UserID != "user-7" || members[0].Role != "admin" {
			t.Errorf("members = %+v, want user-7 as admin and the absent user skipped", members)
		}
		if rep.Members != 1 || rep.MembersSkipped != 1 {
			t.Errorf("member counts = %d/%d, want 1 carried and 1 skipped", rep.Members, rep.MembersSkipped)
		}
		// The columns the importer explains away are named, with a reason.
		var named bool
		for _, d := range rep.DroppedColumns {
			if d.Column == "soft_budget" && strings.Contains(d.Reason, "per key") {
				named = true
			}
		}
		if !named {
			t.Errorf("soft_budget was dropped without its reason:\n%s", rep.Summary())
		}
	})
}

// A dry run writes nothing, and a re-run leaves an existing row alone.
func TestImportTeamsIsReportFirstAndIdempotent(t *testing.T) {
	src := newDirectorySource(t, []teamRow{{id: "team-1", alias: "Imported"}}, nil)
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		rep, err := s.ImportTeams(ctx, src, DirectoryImportOptions{DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Imported != 1 || !rep.DryRun {
			t.Errorf("dry run report: %+v", rep)
		}
		if _, err := s.GetTeam(ctx, "team-1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a dry run wrote the team: %v", err)
		}
		// An administrator's own row stays theirs.
		if err := s.InsertTeam(ctx, &Team{ID: "team-1", Name: "Ours", Alias: "Ours"}); err != nil {
			t.Fatal(err)
		}
		rep, err = s.ImportTeams(ctx, src, DirectoryImportOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if rep.AlreadyPresent != 1 || rep.Imported != 0 {
			t.Errorf("re-run report: %+v", rep)
		}
		got, _ := s.GetTeam(ctx, "team-1")
		if got == nil || got.Alias != "Ours" {
			t.Errorf("the re-run overwrote the administrator's row: %+v", got)
		}
	})
}

// The model-list idioms are the key importer's, applied to a team.
func TestImportTeamsAppliesTheAllowListIdioms(t *testing.T) {
	src := newDirectorySource(t, []teamRow{
		{id: "wide", alias: "wide", models: `["all-proxy-models","gpt-4o"]`},
		{id: "none", alias: "none", models: `["no-default-models"]`},
	}, nil)
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		rep, err := s.ImportTeams(ctx, src, DirectoryImportOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Imported != 0 || rep.Skipped != 2 {
			t.Errorf("skip policy: %+v\n%s", rep, rep.Summary())
		}
		rep, err = s.ImportTeams(ctx, src, DirectoryImportOptions{OnUntranslatable: UntranslatableClear})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Imported != 1 || rep.Skipped != 1 {
			t.Errorf("clear policy: %+v\n%s", rep, rep.Summary())
		}
		got, err := s.GetTeam(ctx, "wide")
		if err != nil {
			t.Fatalf("the widened team was not written: %v", err)
		}
		if len(got.Models) != 0 {
			t.Errorf("a cleared allow-list kept entries: %v", got.Models)
		}
		if _, err := s.GetTeam(ctx, "none"); !errors.Is(err, ErrNotFound) {
			t.Error("a deny-everything sentinel was imported under either policy")
		}
	})
}

// A user crosses with its role and limits; one without an email is skipped
// unless a synthetic domain is given, and the password column never travels.
func TestImportUsersCarriesRoleAndLimits(t *testing.T) {
	src := newDirectorySource(t, nil, []userRow{
		{id: "user-7", alias: "Seven", email: "seven@example.test", role: "proxy_admin", models: `["gpt-4o"]`, maxBudget: 3, rpm: 10, budgetPeriod: "1d"},
		{id: "no-mail", alias: "Nobody", role: "internal_user"},
	})
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		rep, err := s.ImportUsers(ctx, src, DirectoryImportOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Imported != 1 || rep.Skipped != 1 {
			t.Fatalf("report: %+v\n%s", rep, rep.Summary())
		}
		got, err := s.GetUser(ctx, "user-7")
		if err != nil {
			t.Fatalf("the user was not written: %v", err)
		}
		if got.Email != "seven@example.test" || got.Name != "Seven" || got.Role != "proxy_admin" {
			t.Errorf("identity lost: %+v", got)
		}
		if got.MaxBudgetNano == nil || *got.MaxBudgetNano != 3_000_000_000 || got.RPMLimit == nil || *got.RPMLimit != 10 || got.BudgetPeriod != "1d" {
			t.Errorf("limits did not cross: %+v", got)
		}
		var pw bool
		for _, d := range rep.DroppedColumns {
			if d.Column == "password" && strings.Contains(d.Reason, "credential") {
				pw = true
			}
		}
		if !pw {
			t.Errorf("the password column was not refused by name:\n%s", rep.Summary())
		}
		rep, err = s.ImportUsers(ctx, src, DirectoryImportOptions{SyntheticEmailDomain: "imported.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Imported != 1 || rep.AlreadyPresent != 1 {
			t.Errorf("synthetic-email run: %+v\n%s", rep, rep.Summary())
		}
		u, err := s.GetUser(ctx, "no-mail")
		if err != nil || u.Email != "no-mail@imported.invalid" {
			t.Errorf("synthetic email: %+v %v", u, err)
		}
	})
}
