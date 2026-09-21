package app

import (
	"context"
	"errors"
	"time"

	"github.com/ziozzang/dorang/internal/admin"
	"github.com/ziozzang/dorang/internal/quota"
	"github.com/ziozzang/dorang/internal/store"
)

// The users, teams and budgets half of the administration surface.
//
// # Why these two adapters arrived together with an owner join and not before it
//
// `internal/admin` has had complete `/user/*`, `/team/*` and `/budget/*`
// handlers — with scope checks, an audit trail and invalidation fan-out — since
// the surface was written, behind two dependencies this package left nil. The
// obvious reading is that the endpoints were missing a store layer. They were,
// but that was the smaller half.
//
// DESIGN §11.2 says a request is authorized against three subjects — the key,
// its user, its team — and that the most restrictive wins. [auth.Principal]
// carries all three. Nothing in production ever populated the second and third,
// because [cluster.AuthPrincipal] had no owner rows to populate them from, so
// `p.User` and `p.Team` were nil on every request the gateway has ever served
// and every guard that reads them was nil-guarded away. A user block refused
// nothing. A team ceiling bound nothing. A team's rate and concurrency limits
// reached no enforcement.
//
// Wiring `Directory` alone would therefore have produced the exact failure this
// surface exists to prevent: `POST /user/update` with `blocked: true` would
// answer 200, write the row, publish an invalidation to every node — and the
// user's keys would go on serving, forever, on every node including the one that
// served the call. That is strictly worse than the 501, because a 501 is
// noticed. So the join in [store.Store.ResolveKeyRecord] and these adapters are
// one change, and the test that proves it asserts a refusal rather than a row.
//
// # Conversion, and what it must not lose
//
// Both directions are total over the columns each type has. A field that exists
// in the schema and is dropped here is a limit an operator can set and the
// gateway will not enforce — the same class of defect one layer up.

// ---------------------------------------------------------------------------
// Directory
// ---------------------------------------------------------------------------

// adminDirectory implements admin.Directory over the `users`, `teams` and
// `team_members` tables.
type adminDirectory struct{ st *store.Store }

func (d *adminDirectory) CreateUser(ctx context.Context, u *admin.User) error {
	row := storeUserFrom(u)
	if err := d.st.InsertUser(ctx, row); err != nil {
		return adminStoreError(err)
	}
	// The store mints the id and stamps the timestamps when the caller supplied
	// none. Handing them back is what lets the handler's response and its audit
	// row name the object that was actually created rather than the request that
	// asked for it.
	u.ID, u.CreatedAt, u.UpdatedAt = row.ID, row.CreatedAt, row.UpdatedAt
	return nil
}

func (d *adminDirectory) GetUser(ctx context.Context, id string) (*admin.User, error) {
	row, err := d.st.GetUser(ctx, id)
	if err != nil {
		return nil, adminStoreError(err)
	}
	return adminUserFrom(row), nil
}

func (d *adminDirectory) UpdateUser(ctx context.Context, u *admin.User) error {
	row := storeUserFrom(u)
	if err := d.st.UpdateUser(ctx, row); err != nil {
		return adminStoreError(err)
	}
	u.UpdatedAt = row.UpdatedAt
	return nil
}

func (d *adminDirectory) DeleteUsers(ctx context.Context, ids []string) (int, error) {
	n, err := d.st.DeleteUsers(ctx, ids)
	return n, adminStoreError(err)
}

func (d *adminDirectory) ListUsers(ctx context.Context, o admin.ListOptions) ([]*admin.User, error) {
	rows, err := d.st.ListUsers(ctx, o.Limit, o.Offset)
	if err != nil {
		return nil, adminStoreError(err)
	}
	out := make([]*admin.User, 0, len(rows))
	for _, r := range rows {
		out = append(out, adminUserFrom(r))
	}
	return out, nil
}

func (d *adminDirectory) CreateTeam(ctx context.Context, t *admin.Team) error {
	row := storeTeamFrom(t)
	if err := d.st.InsertTeam(ctx, row); err != nil {
		return adminStoreError(err)
	}
	t.ID, t.CreatedAt, t.UpdatedAt = row.ID, row.CreatedAt, row.UpdatedAt
	return nil
}

func (d *adminDirectory) GetTeam(ctx context.Context, id string) (*admin.Team, error) {
	row, err := d.st.GetTeam(ctx, id)
	if err != nil {
		return nil, adminStoreError(err)
	}
	return adminTeamFrom(row), nil
}

func (d *adminDirectory) UpdateTeam(ctx context.Context, t *admin.Team) error {
	row := storeTeamFrom(t)
	if err := d.st.UpdateTeam(ctx, row); err != nil {
		return adminStoreError(err)
	}
	t.UpdatedAt = row.UpdatedAt
	return nil
}

func (d *adminDirectory) DeleteTeams(ctx context.Context, ids []string) (int, error) {
	n, err := d.st.DeleteTeams(ctx, ids)
	return n, adminStoreError(err)
}

func (d *adminDirectory) ListTeams(ctx context.Context, o admin.ListOptions) ([]*admin.Team, error) {
	rows, err := d.st.ListTeams(ctx, o.Limit, o.Offset)
	if err != nil {
		return nil, adminStoreError(err)
	}
	out := make([]*admin.Team, 0, len(rows))
	for _, r := range rows {
		out = append(out, adminTeamFrom(r))
	}
	return out, nil
}

func (d *adminDirectory) AddTeamMember(ctx context.Context, m admin.TeamMember) error {
	return adminStoreError(d.st.AddTeamMember(ctx, store.TeamMember{
		TeamID: m.TeamID, UserID: m.UserID, Role: m.Role,
		MaxBudgetNano: m.MaxBudgetNano, SpendNano: m.SpendNano,
		CreatedAt: m.CreatedAt,
	}))
}

func (d *adminDirectory) RemoveTeamMember(ctx context.Context, teamID, userID string) error {
	return adminStoreError(d.st.RemoveTeamMember(ctx, teamID, userID))
}

func (d *adminDirectory) ListTeamMembers(ctx context.Context, teamID string) ([]admin.TeamMember, error) {
	rows, err := d.st.ListTeamMembers(ctx, teamID)
	if err != nil {
		return nil, adminStoreError(err)
	}
	out := make([]admin.TeamMember, 0, len(rows))
	for _, m := range rows {
		out = append(out, admin.TeamMember{
			TeamID: m.TeamID, UserID: m.UserID, Role: m.Role,
			MaxBudgetNano: m.MaxBudgetNano, SpendNano: m.SpendNano,
			CreatedAt: m.CreatedAt,
		})
	}
	return out, nil
}

func adminUserFrom(u *store.User) *admin.User {
	if u == nil {
		return nil
	}
	return &admin.User{
		ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role,
		MaxBudgetNano: u.MaxBudgetNano, BudgetPeriod: u.BudgetPeriod,
		BudgetResetAt: u.BudgetResetAt, SpendNano: u.SpendNano,
		RPMLimit: u.RPMLimit, TPMLimit: u.TPMLimit,
		Models: u.Models, Blocked: u.Blocked, Metadata: u.Metadata,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

func storeUserFrom(u *admin.User) *store.User {
	if u == nil {
		return nil
	}
	return &store.User{
		ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role,
		MaxBudgetNano: u.MaxBudgetNano, BudgetPeriod: u.BudgetPeriod,
		BudgetResetAt: u.BudgetResetAt, SpendNano: u.SpendNano,
		RPMLimit: u.RPMLimit, TPMLimit: u.TPMLimit,
		Models: u.Models, Blocked: u.Blocked, Metadata: u.Metadata,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

func adminTeamFrom(t *store.Team) *admin.Team {
	if t == nil {
		return nil
	}
	return &admin.Team{
		ID: t.ID, Name: t.Name, Alias: t.Alias, OrganizationID: t.OrganizationID,
		MaxBudgetNano: t.MaxBudgetNano, BudgetPeriod: t.BudgetPeriod,
		BudgetResetAt: t.BudgetResetAt, SpendNano: t.SpendNano,
		RPMLimit: t.RPMLimit, TPMLimit: t.TPMLimit, MaxParallel: t.MaxParallel,
		Models: t.Models, Blocked: t.Blocked, Metadata: t.Metadata,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func storeTeamFrom(t *admin.Team) *store.Team {
	if t == nil {
		return nil
	}
	return &store.Team{
		ID: t.ID, Name: t.Name, Alias: t.Alias, OrganizationID: t.OrganizationID,
		MaxBudgetNano: t.MaxBudgetNano, BudgetPeriod: t.BudgetPeriod,
		BudgetResetAt: t.BudgetResetAt, SpendNano: t.SpendNano,
		RPMLimit: t.RPMLimit, TPMLimit: t.TPMLimit, MaxParallel: t.MaxParallel,
		Models: t.Models, Blocked: t.Blocked, Metadata: t.Metadata,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// ---------------------------------------------------------------------------
// Budgets
// ---------------------------------------------------------------------------

// adminBudgets implements admin.BudgetStore.
//
// # Where a budget lives, in two halves
//
// dorang has no reusable named-budget object: §9.2 puts the CEILING on the
// subject row (`api_keys.max_budget_nano` and its siblings) and the CONSUMPTION
// in `budget_state`. This adapter writes only the first and reads both, and
// that split is the whole reason `/budget/delete` is a usable incident control:
// clearing a ceiling lets traffic through again without erasing what has been
// spent under it.
//
// # Which spend figure is reported, and why it is not the one /key/info uses
//
// The consumption here comes from `budget_state` — the durable counter the gate
// reserves against — and deliberately NOT from the §9.4 rollups that
// [adminSpendReporter] reads. The two answer different questions and this
// surface asks the enforcement one: an operator looking at a budget wants to
// know whether lowering the ceiling to X will refuse, and only the number the
// gate compares against can answer that. The rollup is what agrees with the
// ledger row for row and is the right answer for "what has this key cost"; it
// runs BEHIND the counter, because a node charges a whole lease block before
// spending a unit of it (§9.6), so quoting it beside a ceiling would show a
// remaining balance the gate does not believe in.
type adminBudgets struct {
	st  *store.Store
	now func() time.Time
}

func (b *adminBudgets) SetBudget(ctx context.Context, in admin.Budget) error {
	if in.SoftBudgetNano != nil && in.Subject.Kind != "key" {
		return admin.Unsupported("Soft budget limits are supported only for API keys; use a hard budget for users and teams.")
	}
	sub, err := storeSubject(in.Subject)
	if err != nil {
		return err
	}
	return adminStoreError(b.st.SetSubjectBudget(ctx, sub, store.BudgetCeiling{
		MaxBudgetNano:  in.MaxBudgetNano,
		SoftBudgetNano: in.SoftBudgetNano,
		Period:         in.Period,
		ResetAt:        in.PeriodEnd,
	}))
}

func (b *adminBudgets) GetBudget(ctx context.Context, s admin.BudgetSubject) (admin.Budget, error) {
	sub, err := storeSubject(s)
	if err != nil {
		return admin.Budget{}, err
	}
	got, err := b.st.GetSubjectBudget(ctx, sub)
	if err != nil {
		return admin.Budget{}, adminStoreError(err)
	}
	return b.hydrate(ctx, s, got), nil
}

func (b *adminBudgets) ClearBudget(ctx context.Context, s admin.BudgetSubject) error {
	sub, err := storeSubject(s)
	if err != nil {
		return err
	}
	return adminStoreError(b.st.ClearSubjectBudget(ctx, sub))
}

func (b *adminBudgets) ListBudgets(ctx context.Context, o admin.ListOptions) ([]admin.Budget, error) {
	rows, err := b.st.ListSubjectBudgets(ctx, o.Limit, o.Offset)
	if errors.Is(err, store.ErrBudgetPageTooDeep) {
		// Named rather than turned into a 500. A budget page spans three tables,
		// so the offset cannot be pushed into the queries and the rows before it
		// have to be read and dropped; past a bound this build refuses instead
		// of scanning. The refusal carries the number, because "use a smaller
		// offset" is something the caller can act on and "internal error" is not.
		return nil, admin.Unsupported("%s", err.Error())
	}
	if err != nil {
		return nil, adminStoreError(err)
	}
	out := make([]admin.Budget, 0, len(rows))
	for _, r := range rows {
		s := admin.BudgetSubject{Kind: string(r.Subject.Kind), ID: r.Subject.ID}
		out = append(out, b.hydrate(ctx, s, r))
	}
	return out, nil
}

// hydrate joins a stored ceiling to the counter the gate enforces against.
//
// A subject with no counter row has spent nothing in this period, which is a
// fact rather than a gap, so it reports zero. A counter read that fails for any
// other reason reports zero as well and leaves the ceiling intact: the ceiling
// is the operator's own input being read back, and refusing to show it because
// a second table was unavailable would break `/budget/update`'s read-after-write
// — including on the path an operator uses to END a budget outage.
func (b *adminBudgets) hydrate(ctx context.Context, s admin.BudgetSubject,
	got store.SubjectBudget) admin.Budget {

	out := admin.Budget{
		Subject:        s,
		MaxBudgetNano:  got.MaxBudgetNano,
		SoftBudgetNano: got.SoftBudgetNano,
		Period:         got.Period,
		SpentNano:      got.SpendNano,
		UpdatedAt:      got.UpdatedAt,
		// ReservedNano and ReservedUntil stay zero. Migration 0006 dropped both
		// columns: the reservation mechanism they belonged to shipped with no
		// caller, and the safety net they carried is the lease block's TTL
		// instead (§9.6). Reporting a number here would be reporting a
		// mechanism that does not run.
	}
	w, err := quota.ParseWindow(got.Period)
	if err != nil || !w.Valid() {
		// The same fallback budgetSubjectsOf takes. A period that does not parse
		// must not become "no window at all", or the spend shown beside a
		// ceiling would be measured over a window nothing enforces.
		w = quota.Monthly
	}
	now := b.now()
	out.PeriodStart, out.PeriodEnd = w.PeriodStart(now), w.PeriodEnd(now)
	state, err := b.st.GetBudgetState(ctx, store.Subject{
		Kind: store.SubjectKind(s.Kind), ID: s.ID,
	}, w.String(), out.PeriodStart)
	if err == nil {
		out.SpentNano = state.SpentNano
	} else if errors.Is(err, store.ErrNotFound) {
		out.SpentNano = 0
	}
	return out
}

// storeSubject narrows the administration surface's open subject vocabulary onto
// the three kinds this schema carries a ceiling for.
//
// §6.4 allows a budget on a credential and on the deployment as a whole; §9.2
// gives neither a column. Rather than accept the write and drop it, or invent a
// row nothing reads, the two are refused BY NAME with a 501 — which is the same
// answer, and the same reasoning, that keeps `/model/*` closed.
func storeSubject(s admin.BudgetSubject) (store.Subject, error) {
	switch store.SubjectKind(s.Kind) {
	case store.SubjectKey, store.SubjectUser, store.SubjectTeam:
		return store.Subject{Kind: store.SubjectKind(s.Kind), ID: s.ID}, nil
	}
	return store.Subject{}, admin.Unsupported(
		"a %s budget has no ceiling column in this schema (DESIGN §9.2 puts the ceiling on the "+
			"subject row, and api_keys, users and teams are the three that have one); set the "+
			"ceiling on a key, a user or a team instead", s.Kind)
}

func (*adminBudgets) BudgetKinds() []string { return []string{"key", "user", "team"} }

func (*adminBudgets) SoftBudgetKinds() []string { return []string{"key"} }
