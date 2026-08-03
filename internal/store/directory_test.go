package store

import (
	"context"
	"errors"
	"testing"
)

// The `users` and `teams` rows, and the join that makes them mean something.
//
// The CRUD below is the ordinary half. The half worth writing down is
// [TestResolvingAKeyCarriesItsOwnersInOneStatement]: until it existed, the
// credential path read a key and a secret and nothing else, so `users.blocked`
// and `teams.max_budget_nano` were columns that could be written and never read.
// Everything downstream of that — [auth.Principal.Authorize], the
// authenticator's kill switches, the budget gate — is nil-guarded on the owners,
// so the whole three-subject envelope DESIGN §11.2 defines silently collapsed to
// one subject.

func TestUserRoundTripAndConflicts(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		limit := int64(5_000_000_000)
		u := &User{
			ID: "u1", Email: "u1@example.test", Name: "One", Role: "admin",
			MaxBudgetNano: &limit, BudgetPeriod: "monthly",
			RPMLimit: int64p(60), TPMLimit: int64p(90_000),
			Models: []string{"model-a"}, Blocked: true,
		}
		if err := s.InsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetUser(ctx, "u1")
		if err != nil {
			t.Fatal(err)
		}
		// Every authorization field, because a field dropped in the scanner is a
		// limit an operator can set and the gateway will not enforce.
		switch {
		case got.Email != "u1@example.test" || got.Name != "One" || got.Role != "admin":
			t.Errorf("identity did not round trip: %+v", got)
		case got.MaxBudgetNano == nil || *got.MaxBudgetNano != limit:
			t.Errorf("max_budget = %v, want %d", got.MaxBudgetNano, limit)
		case got.BudgetPeriod != "monthly":
			t.Errorf("budget_period = %q", got.BudgetPeriod)
		case got.RPMLimit == nil || *got.RPMLimit != 60:
			t.Errorf("rpm_limit = %v", got.RPMLimit)
		case got.TPMLimit == nil || *got.TPMLimit != 90_000:
			t.Errorf("tpm_limit = %v", got.TPMLimit)
		case len(got.Models) != 1 || got.Models[0] != "model-a":
			t.Errorf("models = %v", got.Models)
		case !got.Blocked:
			t.Error("blocked did not round trip, which is the one column this file exists for")
		case got.Metadata != "{}":
			t.Errorf("metadata = %q, want the schema default", got.Metadata)
		}

		// A duplicate id and a duplicate EMAIL are both conflicts. Email has a
		// UNIQUE index and it is the collision an operator actually hits.
		if err := s.InsertUser(ctx, &User{ID: "u1", Email: "other@example.test"}); !errors.Is(err, ErrExists) {
			t.Errorf("duplicate id: %v, want ErrExists", err)
		}
		if err := s.InsertUser(ctx, &User{ID: "u2", Email: "u1@example.test"}); !errors.Is(err, ErrExists) {
			t.Errorf("duplicate email: %v, want ErrExists", err)
		}

		got.Blocked = false
		got.Role = "internal_user"
		if err := s.UpdateUser(ctx, got); err != nil {
			t.Fatal(err)
		}
		after, err := s.GetUser(ctx, "u1")
		if err != nil {
			t.Fatal(err)
		}
		if after.Blocked || after.Role != "internal_user" {
			t.Errorf("the update did not land: %+v", after)
		}
		// The role join the administration surface authorizes on reads the same
		// column, so the two cannot disagree about who is an administrator.
		if role, err := s.UserRole(ctx, "u1"); err != nil || role != "internal_user" {
			t.Errorf("UserRole = %q, %v", role, err)
		}

		if err := s.UpdateUser(ctx, &User{ID: "nope", Email: "n@example.test"}); !errors.Is(err, ErrNotFound) {
			t.Errorf("updating a missing user: %v, want ErrNotFound", err)
		}
		if _, err := s.GetUser(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetUser(missing): %v", err)
		}

		list, err := s.ListUsers(ctx, 0, 0)
		if err != nil || len(list) != 1 {
			t.Fatalf("ListUsers = %d rows, %v", len(list), err)
		}
		n, err := s.DeleteUsers(ctx, []string{"u1", "u1", "", "nope"})
		if err != nil || n != 1 {
			t.Errorf("DeleteUsers = %d, %v; want exactly the one row that existed", n, err)
		}
	})
}

func TestTeamRoundTripAndMembership(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		limit := int64(1_000_000_000)
		team := &Team{
			ID: "t1", Name: "Engineering", Alias: "eng", OrganizationID: "org-1",
			MaxBudgetNano: &limit, BudgetPeriod: "daily",
			RPMLimit: int64p(600), TPMLimit: int64p(120_000), MaxParallel: int64p(4),
			Models: []string{"model-a", "model-b"}, Blocked: true,
		}
		if err := s.InsertTeam(ctx, team); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetTeam(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case got.Name != "Engineering" || got.Alias != "eng" || got.OrganizationID != "org-1":
			t.Errorf("identity did not round trip: %+v", got)
		case got.MaxBudgetNano == nil || *got.MaxBudgetNano != limit || got.BudgetPeriod != "daily":
			t.Errorf("budget did not round trip: %+v", got)
		case got.MaxParallel == nil || *got.MaxParallel != 4:
			t.Errorf("max_parallel = %v; it is the one limit users does not have", got.MaxParallel)
		case len(got.Models) != 2:
			t.Errorf("models = %v", got.Models)
		case !got.Blocked:
			t.Error("blocked did not round trip")
		}
		if err := s.InsertTeam(ctx, &Team{ID: "t1"}); !errors.Is(err, ErrExists) {
			t.Errorf("duplicate team: %v, want ErrExists", err)
		}

		// Membership needs both sides to exist. The schema has foreign keys
		// here, and a caught constraint violation would be indistinguishable
		// from a broken connection at the API boundary.
		if err := s.AddTeamMember(ctx, TeamMember{TeamID: "t1", UserID: "ghost"}); !errors.Is(err, ErrNotFound) {
			t.Errorf("member with no user: %v, want ErrNotFound", err)
		}
		if err := s.InsertUser(ctx, &User{ID: "u1", Email: "u1@example.test"}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddTeamMember(ctx, TeamMember{TeamID: "t1", UserID: "u1"}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddTeamMember(ctx, TeamMember{TeamID: "t1", UserID: "u1"}); !errors.Is(err, ErrExists) {
			t.Errorf("duplicate membership: %v, want ErrExists", err)
		}
		members, err := s.ListTeamMembers(ctx, "t1")
		if err != nil || len(members) != 1 || members[0].Role != "member" {
			t.Fatalf("ListTeamMembers = %+v, %v", members, err)
		}
		if err := s.RemoveTeamMember(ctx, "t1", "u1"); err != nil {
			t.Fatal(err)
		}
		if err := s.RemoveTeamMember(ctx, "t1", "u1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("removing an absent member: %v, want ErrNotFound", err)
		}

		if n, err := s.DeleteTeams(ctx, []string{"t1"}); err != nil || n != 1 {
			t.Errorf("DeleteTeams = %d, %v", n, err)
		}
	})
}

// Resolving a credential carries its owners, and still costs ONE statement.
//
// Both halves are the point. The owners have to be there or nothing downstream
// can enforce them; and they have to arrive on the same round trip, or a user's
// limits become a per-miss cost that the next person optimizing the hot path
// removes. §2.4's rule is that one lookup selects the row and only verification
// branches, and there is a test that counts the statements — this is it, for the
// three-subject shape.
func TestResolvingAKeyCarriesItsOwnersInOneStatement(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		userLimit, teamLimit := int64(7_000_000), int64(9_000_000)
		if err := s.InsertUser(ctx, &User{
			ID: "u1", Email: "u1@example.test", Blocked: true,
			MaxBudgetNano: &userLimit, BudgetPeriod: "monthly", RPMLimit: int64p(11),
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertTeam(ctx, &Team{
			ID: "t1", Name: "Engineering",
			MaxBudgetNano: &teamLimit, BudgetPeriod: "daily", MaxParallel: int64p(3),
			Models: []string{"model-a"},
		}); err != nil {
			t.Fatal(err)
		}

		const owned = "sk-owned-key-fixture" // pragma: allowlist secret — test fixture
		k := &APIKey{UserID: "u1", TeamID: "t1"}
		if err := s.NewAPIKeyFromToken(owned, k); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertAPIKey(ctx, k); err != nil {
			t.Fatal(err)
		}

		before := s.StatementCount()
		rec, err := s.ResolveKeyRecord(ctx, KeyLookup(owned))
		if err != nil {
			t.Fatal(err)
		}
		if n := s.StatementCount() - before; n != 1 {
			t.Errorf("resolving a key and its owners issued %d statements, want exactly 1: "+
				"reading the owners separately puts two extra round trips on every "+
				"authentication miss (§2.4)", n)
		}
		if rec.Owners.User == nil {
			t.Fatal("the owning user did not come back; auth.Principal.User will be nil and " +
				"users.blocked will refuse nothing")
		}
		if !rec.Owners.User.Blocked {
			t.Error("users.blocked did not survive the join")
		}
		if rec.Owners.User.MaxBudgetNano == nil || *rec.Owners.User.MaxBudgetNano != userLimit {
			t.Errorf("the user's ceiling did not survive the join: %v", rec.Owners.User.MaxBudgetNano)
		}
		if rec.Owners.User.RPMLimit == nil || *rec.Owners.User.RPMLimit != 11 {
			t.Errorf("the user's rpm_limit did not survive the join: %v", rec.Owners.User.RPMLimit)
		}
		if rec.Owners.Team == nil {
			t.Fatal("the owning team did not come back")
		}
		if rec.Owners.Team.MaxBudgetNano == nil || *rec.Owners.Team.MaxBudgetNano != teamLimit {
			t.Errorf("the team's ceiling did not survive the join: %v", rec.Owners.Team.MaxBudgetNano)
		}
		if rec.Owners.Team.MaxParallel == nil || *rec.Owners.Team.MaxParallel != 3 {
			t.Errorf("the team's max_parallel did not survive the join: %v", rec.Owners.Team.MaxParallel)
		}
		if len(rec.Owners.Team.Models) != 1 || rec.Owners.Team.Models[0] != "model-a" {
			t.Errorf("the team's allow-list did not survive the join: %v", rec.Owners.Team.Models)
		}

		// The bulk snapshot reads the same three subjects, or a node that
		// reloaded would serve an envelope one subject short of the node that
		// missed.
		recs, err := s.ListKeyRecords(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range recs {
			if r.Key.ID != k.ID {
				continue
			}
			found = true
			if r.Owners.User == nil || !r.Owners.User.Blocked || r.Owners.Team == nil {
				t.Error("the snapshot reload dropped the owners the per-key path carries")
			}
		}
		if !found {
			t.Fatal("ListKeyRecords did not return the planted key")
		}
	})
}

// An unowned key, and a key whose owner row is gone, both resolve to no owner.
//
// The schema carries no foreign key on `api_keys.user_id` on purpose —
// authentication must not fail because of referential noise — so a dangling id
// is a state that has to have an answer. A LEFT JOIN gives it the same answer an
// unowned key gets; an inner join would turn a working credential into an
// unknown key, which fails a caller closed for a reason they cannot act on.
func TestAKeyWithNoOwnerAndAKeyWithADanglingOwnerBothResolve(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		const lone = "sk-lone-key-fixture" // pragma: allowlist secret — test fixture
		k1 := &APIKey{}
		if err := s.NewAPIKeyFromToken(lone, k1); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertAPIKey(ctx, k1); err != nil {
			t.Fatal(err)
		}
		rec, err := s.ResolveKeyRecord(ctx, KeyLookup(lone))
		if err != nil {
			t.Fatalf("an unowned key did not resolve: %v", err)
		}
		if rec.Owners.User != nil || rec.Owners.Team != nil {
			t.Errorf("an unowned key resolved to owners %+v", rec.Owners)
		}

		const dangling = "sk-dangling-key-fixture" // pragma: allowlist secret — test fixture
		k2 := &APIKey{UserID: "never-existed", TeamID: "also-never"}
		if err := s.NewAPIKeyFromToken(dangling, k2); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertAPIKey(ctx, k2); err != nil {
			t.Fatal(err)
		}
		rec, err = s.ResolveKeyRecord(ctx, KeyLookup(dangling))
		if err != nil {
			t.Fatalf("a key naming a missing owner did not resolve: %v", err)
		}
		if rec.Owners.User != nil || rec.Owners.Team != nil {
			t.Errorf("a dangling owner id produced owners %+v", rec.Owners)
		}
		if rec.Key.UserID != "never-existed" {
			t.Errorf("the key lost its user_id: %q", rec.Key.UserID)
		}
	})
}

// A budget ceiling is set on the subject row, and clearing it leaves the spend.
//
// The second half is the one an operator depends on during an outage: clearing
// a limit has to restore service without erasing what has been consumed under
// it, because the consumption belongs to the ledger and restoring service is not
// a billing decision.
func TestSubjectBudgetsAreSetOnTheSubjectRowAndClearedWithoutTouchingSpend(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		if err := s.InsertTeam(ctx, &Team{ID: "t1", Name: "Engineering", SpendNano: 42}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertUser(ctx, &User{ID: "u1", Email: "u1@example.test"}); err != nil {
			t.Fatal(err)
		}

		limit := int64(250_000_000)
		if err := s.SetSubjectBudget(ctx, Subject{Kind: SubjectTeam, ID: "t1"},
			BudgetCeiling{MaxBudgetNano: &limit, Period: "monthly"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetSubjectBudget(ctx, Subject{Kind: SubjectTeam, ID: "t1"})
		if err != nil {
			t.Fatal(err)
		}
		if got.MaxBudgetNano == nil || *got.MaxBudgetNano != limit || got.Period != "monthly" {
			t.Fatalf("the ceiling did not land: %+v", got)
		}
		if got.SpendNano != 42 {
			t.Errorf("setting a ceiling changed recorded spend to %d", got.SpendNano)
		}

		// The subject's own row still carries everything else. A read-modify-
		// write would have blanked the name.
		team, err := s.GetTeam(ctx, "t1")
		if err != nil || team.Name != "Engineering" {
			t.Errorf("setting a budget rewrote the rest of the row: %+v, %v", team, err)
		}

		if err := s.ClearSubjectBudget(ctx, Subject{Kind: SubjectTeam, ID: "t1"}); err != nil {
			t.Fatal(err)
		}
		got, err = s.GetSubjectBudget(ctx, Subject{Kind: SubjectTeam, ID: "t1"})
		if err != nil {
			t.Fatal(err)
		}
		if got.MaxBudgetNano != nil {
			t.Errorf("the ceiling survived being cleared: %v", got.MaxBudgetNano)
		}
		if got.SpendNano != 42 {
			t.Errorf("clearing a ceiling dropped recorded spend to %d; /budget/delete removes a "+
				"limit, not a ledger", got.SpendNano)
		}

		// A subject that exists with no ceiling is not "missing": reporting it
		// as such would make "this team is unlimited" indistinguishable from
		// "this team does not exist" on the route an operator checks after
		// clearing one.
		if _, err := s.GetSubjectBudget(ctx, Subject{Kind: SubjectUser, ID: "u1"}); err != nil {
			t.Errorf("a user with no ceiling: %v, want a budget of none", err)
		}
		if _, err := s.GetSubjectBudget(ctx, Subject{Kind: SubjectTeam, ID: "gone"}); !errors.Is(err, ErrNotFound) {
			t.Errorf("a team that does not exist: %v, want ErrNotFound", err)
		}

		// A soft budget is an api_keys column. Dropping it silently on a team
		// would be a limit an operator set that nothing stores.
		soft := int64(1)
		if err := s.SetSubjectBudget(ctx, Subject{Kind: SubjectTeam, ID: "t1"},
			BudgetCeiling{MaxBudgetNano: &limit, SoftBudgetNano: &soft}); !errors.Is(err, ErrNoBudgetSubject) {
			t.Errorf("a soft budget on a team: %v, want ErrNoBudgetSubject", err)
		}
		// And a subject kind with no ceiling column at all is named rather than
		// written somewhere nothing reads.
		for _, kind := range []SubjectKind{SubjectCredential, SubjectGlobal} {
			if err := s.SetSubjectBudget(ctx, Subject{Kind: kind, ID: "x"},
				BudgetCeiling{MaxBudgetNano: &limit}); !errors.Is(err, ErrNoBudgetSubject) {
				t.Errorf("a %s budget: %v, want ErrNoBudgetSubject", kind, err)
			}
		}

		if err := s.SetSubjectBudget(ctx, Subject{Kind: SubjectUser, ID: "u1"},
			BudgetCeiling{MaxBudgetNano: &limit}); err != nil {
			t.Fatal(err)
		}
		list, err := s.ListSubjectBudgets(ctx, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Subject.Kind != SubjectUser {
			t.Errorf("ListSubjectBudgets = %+v, want only the one subject that declares a ceiling", list)
		}
	})
}

// A budget page is a page: it spans three tables and it is still one window.
//
// The first version applied `LIMIT ? OFFSET ?` per table, which reads as working
// and is not: `limit: 3` returned up to nine rows next to a `"limit": 3` in the
// answer, and pages after the first re-ordered silently as the three tables ran
// out at different offsets. Nothing about that is visible from a single-table
// fixture, which is why this one plants a ceiling on all three.
func TestABudgetPageSpansTheThreeSubjectTablesAsOneWindow(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		limit := int64(1_000)

		for _, id := range []string{"u-a", "u-b"} {
			if err := s.InsertUser(ctx, &User{ID: id, Email: id + "@example.test",
				MaxBudgetNano: &limit}); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range []string{"t-a", "t-b"} {
			if err := s.InsertTeam(ctx, &Team{ID: id, Name: id, MaxBudgetNano: &limit}); err != nil {
				t.Fatal(err)
			}
		}
		for i, tok := range []string{"sk-page-key-a", "sk-page-key-b"} { // pragma: allowlist secret — test fixtures
			k := &APIKey{ID: string(rune('a'+i)) + "-key", MaxBudgetNano: &limit}
			if err := s.NewAPIKeyFromToken(tok, k); err != nil {
				t.Fatal(err)
			}
			if err := s.InsertAPIKey(ctx, k); err != nil {
				t.Fatal(err)
			}
		}

		all, err := s.ListSubjectBudgets(ctx, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 6 {
			t.Fatalf("ListSubjectBudgets returned %d of 6 declared ceilings", len(all))
		}

		// A page of three is three rows, not three per table.
		page, err := s.ListSubjectBudgets(ctx, 3, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 3 {
			t.Fatalf("a page of 3 returned %d rows; the limit is applied per table rather than "+
				"to the answer, so the reported `limit` and the row count disagree", len(page))
		}

		// And the pages tile the whole list exactly once, in the same order.
		var walked []SubjectBudget
		for off := 0; ; off += 2 {
			p, err := s.ListSubjectBudgets(ctx, 2, off)
			if err != nil {
				t.Fatal(err)
			}
			if len(p) == 0 {
				break
			}
			walked = append(walked, p...)
			if off > 20 {
				t.Fatal("paging did not terminate")
			}
		}
		if len(walked) != len(all) {
			t.Fatalf("paging two at a time visited %d rows, want %d", len(walked), len(all))
		}
		for i := range walked {
			if walked[i].Subject != all[i].Subject {
				t.Fatalf("page %d holds %+v, want %+v: the order is not stable across offsets",
					i, walked[i].Subject, all[i].Subject)
			}
		}

		// A page beyond the bounded scan is refused rather than answered with a
		// truncated one, because a short page is how a caller decides it has
		// reached the end.
		if _, err := s.ListSubjectBudgets(ctx, 100, MaxBudgetListScan); !errors.Is(err, ErrBudgetPageTooDeep) {
			t.Errorf("a page past the bounded scan: %v, want ErrBudgetPageTooDeep", err)
		}
	})
}
