package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The `users`, `teams` and `team_members` rows, in Go.
//
// # Why this file is the load-bearing half of a user administration surface
//
// The tables have existed since the first migration. What did not exist was any
// Go code that reads them, and the consequence was not "an endpoint answers
// 501". DESIGN §11.2 says the authorization envelope is THREE subjects — the
// key, the user that owns it, the team that owns the user — and that the most
// restrictive wins. [github.com/ziozzang/dorang/internal/auth.Principal] carries
// all three and [auth.Principal.Authorize] enforces all three. But the only
// conversion that builds a Principal from a stored row could not populate the
// second and third, because nothing here could read them: `users.blocked` and
// `teams.blocked` were columns no code path could observe, and every one of
// their guards — in Authorize, in the authenticator's kill switches, in
// budgetSubjectsOf, in MostRestrictiveParallel — was nil-guarded away.
//
// So a user block was not slow to propagate. It did not exist. The same is true
// of a team's `max_budget_nano`, its rate ceilings and its model allow-list.
//
// That is why [Owners] and [Store.ResolveKeyRecord] are in this file next to the
// CRUD: the administration routes are how an operator SETS the flag, and the
// owner join is how the gateway READS it. Shipping either alone produces a
// control that is a note.
//
// Every query goes through Store.exec/query/queryRow like the rest of the
// package, so placeholder rewriting and statement counting hold, and no
// identifier is ever built from a value.

// User is one `users` row.
//
// Role is the administrative role and is what makes a key admin-capable, since
// the key row carries none of its own (DESIGN §9.2). Everything else on it is an
// authorization limit that binds every key the user owns.
type User struct {
	ID    string
	Email string
	Name  string
	Role  string

	MaxBudgetNano *int64
	BudgetPeriod  string
	BudgetResetAt time.Time
	SpendNano     int64

	RPMLimit *int64
	TPMLimit *int64

	Models  []string
	Blocked bool
	// Metadata is verbatim JSON. It is opaque to this package and to
	// authorization; the column exists so an import does not lose what the
	// source system carried.
	Metadata string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Team is one `teams` row.
type Team struct {
	ID             string
	Name           string
	Alias          string
	OrganizationID string

	MaxBudgetNano *int64
	BudgetPeriod  string
	BudgetResetAt time.Time
	SpendNano     int64

	RPMLimit    *int64
	TPMLimit    *int64
	MaxParallel *int64

	Models   []string
	Blocked  bool
	Metadata string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TeamMember is one `team_members` row.
//
// Note what it is NOT: it is not how a key learns its team. A key's team is
// `api_keys.team_id` (DESIGN §9.2), which is what [Owners] joins on and what the
// administrative scope is derived from. Membership records who belongs to a team
// for reporting and for the directory's own listing, and no cached authorization
// decision is derived from it — which is why the membership routes deliberately
// publish no invalidation.
type TeamMember struct {
	TeamID        string
	UserID        string
	Role          string
	MaxBudgetNano *int64
	SpendNano     int64
	CreatedAt     time.Time
}

// DefaultDirectoryListLimit and MaxDirectoryListLimit bound a listing, for the
// reason [DefaultAPIKeyListLimit] gives: a list query that can return every row
// is a query that will one day be run against a deployment with a million of
// them.
const (
	DefaultDirectoryListLimit = 100
	MaxDirectoryListLimit     = 1000
)

const userColumns = `id, email, name, role, max_budget_nano, budget_period,
	budget_reset_at, spend_nano, rpm_limit, tpm_limit, models, blocked, metadata,
	created_at, updated_at`

const teamColumns = `id, name, alias, organization_id, max_budget_nano, budget_period,
	budget_reset_at, spend_nano, rpm_limit, tpm_limit, max_parallel, models, blocked,
	metadata, created_at, updated_at`

const teamMemberColumns = `team_id, user_id, role, max_budget_nano, spend_nano, created_at`

// userScan holds the intermediates one `users` row needs on the way in.
//
// EVERY column is scanned nullable, including the NOT NULL ones. That is not
// sloppiness: the same destination list serves a direct SELECT and the LEFT JOIN
// in [resolveKeyQuery], and in a LEFT JOIN with no matching row every column comes
// back NULL regardless of what the schema says. Two scanners over one column
// list would drift, and the symptom of that drift is a limit read into the wrong
// field.
type userScan struct {
	id, email, name, role, period, models, metadata sql.NullString
	maxBudget, resetAt, spend, rpm, tpm             sql.NullInt64
	created, updated                                sql.NullInt64
	blocked                                         sql.NullBool
}

func (t *userScan) dests() []any {
	return []any{&t.id, &t.email, &t.name, &t.role, &t.maxBudget, &t.period,
		&t.resetAt, &t.spend, &t.rpm, &t.tpm, &t.models, &t.blocked, &t.metadata,
		&t.created, &t.updated}
}

// user materializes the scanned row, or nil when the LEFT JOIN matched nothing.
func (t *userScan) user() *User {
	if !t.id.Valid {
		return nil
	}
	return &User{
		ID: t.id.String, Email: str(t.email), Name: str(t.name), Role: str(t.role),
		MaxBudgetNano: nullableInt(t.maxBudget),
		BudgetPeriod:  str(t.period),
		BudgetResetAt: TimeAt(nullInt(t.resetAt)),
		SpendNano:     nullInt(t.spend),
		RPMLimit:      nullableInt(t.rpm),
		TPMLimit:      nullableInt(t.tpm),
		Models:        decodeStrings(str(t.models)),
		Blocked:       t.blocked.Bool,
		Metadata:      str(t.metadata),
		CreatedAt:     TimeAt(nullInt(t.created)),
		UpdatedAt:     TimeAt(nullInt(t.updated)),
	}
}

// teamScan is [userScan] for a `teams` row, and is all-nullable for the same
// reason.
type teamScan struct {
	id, name, alias, org, period, models, metadata sql.NullString
	maxBudget, resetAt, spend, rpm, tpm, parallel  sql.NullInt64
	created, updated                               sql.NullInt64
	blocked                                        sql.NullBool
}

func (t *teamScan) dests() []any {
	return []any{&t.id, &t.name, &t.alias, &t.org, &t.maxBudget, &t.period,
		&t.resetAt, &t.spend, &t.rpm, &t.tpm, &t.parallel, &t.models, &t.blocked,
		&t.metadata, &t.created, &t.updated}
}

func (t *teamScan) team() *Team {
	if !t.id.Valid {
		return nil
	}
	return &Team{
		ID: t.id.String, Name: str(t.name), Alias: str(t.alias),
		OrganizationID: str(t.org),
		MaxBudgetNano:  nullableInt(t.maxBudget),
		BudgetPeriod:   str(t.period),
		BudgetResetAt:  TimeAt(nullInt(t.resetAt)),
		SpendNano:      nullInt(t.spend),
		RPMLimit:       nullableInt(t.rpm),
		TPMLimit:       nullableInt(t.tpm),
		MaxParallel:    nullableInt(t.parallel),
		Models:         decodeStrings(str(t.models)),
		Blocked:        t.blocked.Bool,
		Metadata:       str(t.metadata),
		CreatedAt:      TimeAt(nullInt(t.created)),
		UpdatedAt:      TimeAt(nullInt(t.updated)),
	}
}

// ---------------------------------------------------------------------------
// Owners: the join authorization depends on
// ---------------------------------------------------------------------------

// Owners is a key's owning user and team rows, either of which is nil when the
// key does not name one.
//
// It is a type rather than two arguments so that a caller building an
// authorization envelope cannot supply one and forget the other, and so that
// adding a fourth subject one day is one field rather than a signature every
// call site has to be re-audited against.
//
// A nil member means "this key has no such owner", which is the unrestricted
// case, and it is deliberately the SAME value as "the row named an owner that no
// longer exists". The schema carries no foreign key on `api_keys.user_id` for
// the reason the migration states — authentication must not fail because of
// referential noise — so a dangling owner id is a state the gateway has to have
// an answer for, and the answer is the one every other unset limit gets.
type Owners struct {
	User *User
	Team *Team
}

// ownerJoin is the LEFT JOIN clause every credential read carries.
//
// It is a LEFT join and not an inner one because a key with no user, a key with
// no team, and a key whose owner row was deleted must all still authenticate —
// an inner join would turn any of those into "unknown key", which fails a
// working credential closed for a reason the caller cannot act on and the
// operator did not choose.
const ownerJoin = ` LEFT JOIN users u ON u.id = k.user_id` +
	` LEFT JOIN teams t ON t.id = k.team_id`

// ownerCols is the owner half of a credential read's column list.
var ownerCols = prefixCols(userColumns, "u") + `, ` + prefixCols(teamColumns, "t")

// LoadOwners reads one key's owning user and team.
//
// It exists for the caller that already holds an [APIKey] and needs the rest of
// its envelope — the batch executor, which resolves a row's owner from an api
// key id hours after the request that created it. The credential path does not
// use it: that path reads the owners in the SAME statement as the key (see
// [Store.ResolveKeyRecord]), because DESIGN §2.4's rule is that authentication
// costs one round trip and three would have made a user's limits something the
// hot path pays for per miss.
//
// Ids that name no row yield a nil member rather than an error. See [Owners].
func (s *Store) LoadOwners(ctx context.Context, userID, teamID string) (Owners, error) {
	var own Owners
	if userID != "" {
		u, err := s.GetUser(ctx, userID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Owners{}, err
		}
		own.User = u
	}
	if teamID != "" {
		t, err := s.GetTeam(ctx, teamID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Owners{}, err
		}
		own.Team = t
	}
	return own, nil
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// InsertUser writes a user row, failing with [ErrExists] if the id or the email
// is taken.
//
// The duplicate check is an explicit read inside the transaction rather than a
// caught constraint violation, for the reason [Store.CreateBatch] gives: "is
// this a duplicate key or a broken connection" is not a question a caller should
// answer by matching on driver message text. Email is checked as well as id
// because `users_email_key` is a UNIQUE index and it is the collision an
// operator actually hits.
func (s *Store) InsertUser(ctx context.Context, u *User) error {
	if u == nil {
		return errors.New("store: InsertUser needs a user")
	}
	if u.ID == "" {
		u.ID = NewID()
	}
	if u.Email == "" {
		return errors.New("store: a user needs an email")
	}
	if u.Role == "" {
		u.Role = "internal_user"
	}
	if err := checkUserAmounts(u); err != nil {
		return err
	}
	now := s.now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx,
			s.rebind(`SELECT 1 FROM users WHERE id = ? OR email = ?`), u.ID, u.Email).Scan(&one)
		if err == nil {
			return fmt.Errorf("%w: user %s", ErrExists, u.ID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = s.txExec(ctx, tx,
			`INSERT INTO users (`+userColumns+`) VALUES (`+placeholders(15)+`)`,
			u.ID, u.Email, u.Name, u.Role,
			ptrInt(u.MaxBudgetNano), nullStr(u.BudgetPeriod), nullMicros(u.BudgetResetAt),
			u.SpendNano, ptrInt(u.RPMLimit), ptrInt(u.TPMLimit),
			encodeStrings(u.Models), u.Blocked, metadataOr(u.Metadata),
			Micros(u.CreatedAt), Micros(u.UpdatedAt))
		return err
	})
}

// GetUser fetches a user row by id, or [ErrNotFound].
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	if id == "" {
		return nil, ErrNotFound
	}
	var t userScan
	err := s.queryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id).Scan(t.dests()...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t.user(), nil
}

// UpdateUser rewrites every mutable field of a user row.
//
// `spend_nano` is in the SET list and `id` is not. The spend column is the
// operator-settable figure an import carries; enforcement counts against
// `budget_state`, which this does not touch — so lowering a ceiling here does
// not silently reset what has been consumed under it.
func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	if u == nil || u.ID == "" {
		return errors.New("store: UpdateUser requires a user id")
	}
	if err := checkUserAmounts(u); err != nil {
		return err
	}
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = s.now()
	}
	const q = `UPDATE users SET
		email = ?, name = ?, role = ?,
		max_budget_nano = ?, budget_period = ?, budget_reset_at = ?, spend_nano = ?,
		rpm_limit = ?, tpm_limit = ?, models = ?, blocked = ?, metadata = ?,
		updated_at = ?
		WHERE id = ?`
	res, err := s.exec(ctx, q,
		u.Email, u.Name, u.Role,
		ptrInt(u.MaxBudgetNano), nullStr(u.BudgetPeriod), nullMicros(u.BudgetResetAt),
		u.SpendNano, ptrInt(u.RPMLimit), ptrInt(u.TPMLimit),
		encodeStrings(u.Models), u.Blocked, metadataOr(u.Metadata),
		Micros(u.UpdatedAt), u.ID)
	if err != nil {
		return err
	}
	return affectedOrNotFound(res)
}

// DeleteUsers removes user rows by id and reports how many went away.
//
// It does NOT delete the keys those users owned. That is deliberate and it is
// the fail-closed direction only because of what happens next: a key whose
// `user_id` no longer resolves loses its user's envelope, so a key that was
// under a blocked user would start serving again if the block were removed by
// deleting the user. The administration surface therefore announces a delete
// with [auth.CauseRevoked] over the owned keys, and an operator ending access
// deletes or blocks the keys. Cascading here would silently destroy credentials
// an operator did not name.
func (s *Store) DeleteUsers(ctx context.Context, ids []string) (int, error) {
	return s.deleteByIDs(ctx, "users", ids)
}

// ListUsers returns user rows, newest first.
//
// The ordering is (created_at DESC, id DESC) rather than created_at alone,
// because two rows written in the same microsecond would otherwise page
// non-deterministically — one appearing on two pages and another on none.
func (s *Store) ListUsers(ctx context.Context, limit, offset int) ([]*User, error) {
	limit, offset = boundPage(limit, offset)
	rows, err := s.query(ctx, `SELECT `+userColumns+` FROM users
		 ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*User, 0, 16)
	for rows.Next() {
		var t userScan
		if err := rows.Scan(t.dests()...); err != nil {
			return nil, err
		}
		out = append(out, t.user())
	}
	return out, rows.Err()
}

func checkUserAmounts(u *User) error {
	if u.MaxBudgetNano != nil {
		if err := checkAmount(*u.MaxBudgetNano, "max_budget"); err != nil {
			return err
		}
	}
	return checkAmount(u.SpendNano, "spend")
}

// ---------------------------------------------------------------------------
// Teams
// ---------------------------------------------------------------------------

// InsertTeam writes a team row, failing with [ErrExists] if the id is taken.
func (s *Store) InsertTeam(ctx context.Context, t *Team) error {
	if t == nil {
		return errors.New("store: InsertTeam needs a team")
	}
	if t.ID == "" {
		t.ID = NewID()
	}
	if err := checkTeamAmounts(t); err != nil {
		return err
	}
	now := s.now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, s.rebind(`SELECT 1 FROM teams WHERE id = ?`), t.ID).Scan(&one)
		if err == nil {
			return fmt.Errorf("%w: team %s", ErrExists, t.ID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = s.txExec(ctx, tx,
			`INSERT INTO teams (`+teamColumns+`) VALUES (`+placeholders(16)+`)`,
			t.ID, t.Name, nullStr(t.Alias), nullStr(t.OrganizationID),
			ptrInt(t.MaxBudgetNano), nullStr(t.BudgetPeriod), nullMicros(t.BudgetResetAt),
			t.SpendNano, ptrInt(t.RPMLimit), ptrInt(t.TPMLimit), ptrInt(t.MaxParallel),
			encodeStrings(t.Models), t.Blocked, metadataOr(t.Metadata),
			Micros(t.CreatedAt), Micros(t.UpdatedAt))
		return err
	})
}

// GetTeam fetches a team row by id, or [ErrNotFound].
func (s *Store) GetTeam(ctx context.Context, id string) (*Team, error) {
	if id == "" {
		return nil, ErrNotFound
	}
	var t teamScan
	err := s.queryRow(ctx, `SELECT `+teamColumns+` FROM teams WHERE id = ?`, id).Scan(t.dests()...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t.team(), nil
}

// UpdateTeam rewrites every mutable field of a team row.
func (s *Store) UpdateTeam(ctx context.Context, t *Team) error {
	if t == nil || t.ID == "" {
		return errors.New("store: UpdateTeam requires a team id")
	}
	if err := checkTeamAmounts(t); err != nil {
		return err
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = s.now()
	}
	const q = `UPDATE teams SET
		name = ?, alias = ?, organization_id = ?,
		max_budget_nano = ?, budget_period = ?, budget_reset_at = ?, spend_nano = ?,
		rpm_limit = ?, tpm_limit = ?, max_parallel = ?,
		models = ?, blocked = ?, metadata = ?, updated_at = ?
		WHERE id = ?`
	res, err := s.exec(ctx, q,
		t.Name, nullStr(t.Alias), nullStr(t.OrganizationID),
		ptrInt(t.MaxBudgetNano), nullStr(t.BudgetPeriod), nullMicros(t.BudgetResetAt),
		t.SpendNano, ptrInt(t.RPMLimit), ptrInt(t.TPMLimit), ptrInt(t.MaxParallel),
		encodeStrings(t.Models), t.Blocked, metadataOr(t.Metadata),
		Micros(t.UpdatedAt), t.ID)
	if err != nil {
		return err
	}
	return affectedOrNotFound(res)
}

// DeleteTeams removes team rows by id and reports how many went away. Its
// `team_members` rows go with it, by the schema's ON DELETE CASCADE; the keys
// that named the team do not, for the reason [Store.DeleteUsers] gives.
func (s *Store) DeleteTeams(ctx context.Context, ids []string) (int, error) {
	return s.deleteByIDs(ctx, "teams", ids)
}

// ListTeams returns team rows, newest first.
func (s *Store) ListTeams(ctx context.Context, limit, offset int) ([]*Team, error) {
	limit, offset = boundPage(limit, offset)
	rows, err := s.query(ctx, `SELECT `+teamColumns+` FROM teams
		 ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Team, 0, 16)
	for rows.Next() {
		var t teamScan
		if err := rows.Scan(t.dests()...); err != nil {
			return nil, err
		}
		out = append(out, t.team())
	}
	return out, rows.Err()
}

func checkTeamAmounts(t *Team) error {
	if t.MaxBudgetNano != nil {
		if err := checkAmount(*t.MaxBudgetNano, "max_budget"); err != nil {
			return err
		}
	}
	return checkAmount(t.SpendNano, "spend")
}

// ---------------------------------------------------------------------------
// Team membership
// ---------------------------------------------------------------------------

// AddTeamMember records that a user belongs to a team.
//
// It returns [ErrNotFound] when either side does not exist and [ErrExists] on a
// duplicate. Both are checked inside the transaction: the schema has foreign
// keys here (unlike `api_keys`), and a caught constraint violation would be
// indistinguishable from a broken connection at the API boundary.
func (s *Store) AddTeamMember(ctx context.Context, m TeamMember) error {
	if m.TeamID == "" || m.UserID == "" {
		return errors.New("store: AddTeamMember needs a team id and a user id")
	}
	if m.Role == "" {
		m.Role = "member"
	}
	if m.MaxBudgetNano != nil {
		if err := checkAmount(*m.MaxBudgetNano, "max_budget"); err != nil {
			return err
		}
	}
	if err := checkAmount(m.SpendNano, "spend"); err != nil {
		return err
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = s.now()
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, probe := range []struct{ q, id string }{
			{`SELECT 1 FROM teams WHERE id = ?`, m.TeamID},
			{`SELECT 1 FROM users WHERE id = ?`, m.UserID},
		} {
			var one int
			if err := tx.QueryRowContext(ctx, s.rebind(probe.q), probe.id).Scan(&one); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
		}
		var one int
		err := tx.QueryRowContext(ctx,
			s.rebind(`SELECT 1 FROM team_members WHERE team_id = ? AND user_id = ?`),
			m.TeamID, m.UserID).Scan(&one)
		if err == nil {
			return fmt.Errorf("%w: %s is already in team %s", ErrExists, m.UserID, m.TeamID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = s.txExec(ctx, tx,
			`INSERT INTO team_members (`+teamMemberColumns+`) VALUES (`+placeholders(6)+`)`,
			m.TeamID, m.UserID, m.Role, ptrInt(m.MaxBudgetNano), m.SpendNano,
			Micros(m.CreatedAt))
		return err
	})
}

// RemoveTeamMember drops one membership row, or returns [ErrNotFound].
func (s *Store) RemoveTeamMember(ctx context.Context, teamID, userID string) error {
	if teamID == "" || userID == "" {
		return ErrNotFound
	}
	res, err := s.exec(ctx, `DELETE FROM team_members WHERE team_id = ? AND user_id = ?`,
		teamID, userID)
	if err != nil {
		return err
	}
	return affectedOrNotFound(res)
}

// ListTeamMembers returns one team's membership, oldest first.
func (s *Store) ListTeamMembers(ctx context.Context, teamID string) ([]TeamMember, error) {
	if teamID == "" {
		return nil, nil
	}
	rows, err := s.query(ctx, `SELECT `+teamMemberColumns+` FROM team_members
		 WHERE team_id = ? ORDER BY created_at, user_id LIMIT ?`,
		teamID, MaxDirectoryListLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TeamMember, 0, 8)
	for rows.Next() {
		var (
			m         TeamMember
			maxBudget sql.NullInt64
			created   int64
		)
		if err := rows.Scan(&m.TeamID, &m.UserID, &m.Role, &maxBudget, &m.SpendNano,
			&created); err != nil {
			return nil, err
		}
		m.MaxBudgetNano = nullableInt(maxBudget)
		m.CreatedAt = TimeAt(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Budget ceilings
// ---------------------------------------------------------------------------

// BudgetCeiling is the SETTABLE half of a budget: what an operator declares.
//
// The consumed half lives in `budget_state` and is read through
// [Store.GetBudgetState]. Keeping them apart is the point — DESIGN §9.2 puts the
// ceiling on the subject row and the consumption in its own counter, and a
// "budget" object that carried both would invite a write that resets spend as a
// side effect of lowering a limit. Nothing here writes `budget_state`.
type BudgetCeiling struct {
	MaxBudgetNano *int64
	// SoftBudgetNano is the alert threshold. Only `api_keys` has the column; a
	// user or team ceiling carries none, and [Store.SetSubjectBudget] refuses a
	// soft budget on those subjects by name rather than dropping it.
	SoftBudgetNano *int64
	Period         string
	ResetAt        time.Time
}

// ErrNoBudgetSubject reports a budget addressed to a subject this schema has no
// ceiling column for.
//
// §9.2 puts a ceiling on `api_keys`, `users` and `teams` and nowhere else. A
// credential's spend is a provider quota (§6.2) and a global ceiling has no row
// at all, so a `/budget/new` naming either is refused BY NAME rather than
// written into a table nothing reads — which is the failure this whole surface
// exists to avoid one layer up.
var ErrNoBudgetSubject = errors.New("store: this subject has no budget ceiling column")

// budgetTable maps a subject kind onto the table its ceiling lives on.
func budgetTable(kind SubjectKind) (string, bool) {
	switch kind {
	case SubjectKey:
		return "api_keys", true
	case SubjectUser:
		return "users", true
	case SubjectTeam:
		return "teams", true
	}
	return "", false
}

// SetSubjectBudget writes one subject's ceiling.
//
// It is a targeted UPDATE of the budget columns rather than a read-modify-write
// of the whole row, so that two operators changing a limit and a name at the
// same moment do not silently undo each other. The table name comes from
// [budgetTable]'s closed set and never from a caller's string.
func (s *Store) SetSubjectBudget(ctx context.Context, sub Subject, c BudgetCeiling) error {
	table, ok := budgetTable(sub.Kind)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoBudgetSubject, sub.Kind)
	}
	if sub.ID == "" {
		return ErrNotFound
	}
	for name, v := range map[string]*int64{"max_budget": c.MaxBudgetNano, "soft_budget": c.SoftBudgetNano} {
		if v != nil {
			if err := checkAmount(*v, name); err != nil {
				return err
			}
		}
	}
	set := `max_budget_nano = ?, budget_period = ?, budget_reset_at = ?, updated_at = ?`
	args := []any{ptrInt(c.MaxBudgetNano), nullStr(c.Period), nullMicros(c.ResetAt), Micros(s.now())}
	if sub.Kind == SubjectKey {
		set = `max_budget_nano = ?, soft_budget_nano = ?, budget_period = ?, budget_reset_at = ?, updated_at = ?`
		args = []any{ptrInt(c.MaxBudgetNano), ptrInt(c.SoftBudgetNano), nullStr(c.Period),
			nullMicros(c.ResetAt), Micros(s.now())}
	} else if c.SoftBudgetNano != nil {
		return fmt.Errorf("%w: a soft budget is an api_keys column; %s has none",
			ErrNoBudgetSubject, sub.Kind)
	}
	args = append(args, sub.ID)
	res, err := s.exec(ctx, `UPDATE `+table+` SET `+set+` WHERE id = ?`, args...)
	if err != nil {
		return err
	}
	return affectedOrNotFound(res)
}

// SubjectBudget is one subject's ceiling as stored, with the subject that
// declared it.
type SubjectBudget struct {
	Subject Subject
	BudgetCeiling
	SpendNano int64
	UpdatedAt time.Time
}

// GetSubjectBudget reads one subject's ceiling, or [ErrNotFound] when the
// subject row does not exist.
//
// A subject that exists with no ceiling is NOT an error: it is a budget of
// "none", and reporting it as missing would make "this team is unlimited"
// indistinguishable from "this team does not exist" on the route an operator
// checks after clearing a ceiling.
func (s *Store) GetSubjectBudget(ctx context.Context, sub Subject) (SubjectBudget, error) {
	table, ok := budgetTable(sub.Kind)
	if !ok {
		return SubjectBudget{}, fmt.Errorf("%w: %s", ErrNoBudgetSubject, sub.Kind)
	}
	if sub.ID == "" {
		return SubjectBudget{}, ErrNotFound
	}
	// CAST rather than a bare NULL: PostgreSQL types a naked NULL literal as
	// `unknown`, and the driver hands an untyped nil to a sql.NullInt64
	// destination. SQLite accepts the same cast, so one statement serves both.
	cols := `max_budget_nano, CAST(NULL AS BIGINT), budget_period, budget_reset_at, spend_nano, updated_at`
	if sub.Kind == SubjectKey {
		cols = `max_budget_nano, soft_budget_nano, budget_period, budget_reset_at, spend_nano, updated_at`
	}
	var (
		maxB, softB, resetAt sql.NullInt64
		period               sql.NullString
		spend, updated       int64
	)
	err := s.queryRow(ctx, `SELECT `+cols+` FROM `+table+` WHERE id = ?`, sub.ID).
		Scan(&maxB, &softB, &period, &resetAt, &spend, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return SubjectBudget{}, ErrNotFound
	}
	if err != nil {
		return SubjectBudget{}, err
	}
	return SubjectBudget{
		Subject: sub,
		BudgetCeiling: BudgetCeiling{
			MaxBudgetNano:  nullableInt(maxB),
			SoftBudgetNano: nullableInt(softB),
			Period:         str(period),
			ResetAt:        TimeAt(nullInt(resetAt)),
		},
		SpendNano: spend,
		UpdatedAt: TimeAt(updated),
	}, nil
}

// ClearSubjectBudget removes a subject's ceiling and leaves its spend alone.
//
// Leaving the spend is the whole point and it is what makes this the control an
// operator reaches for during a budget outage: clearing a limit must let traffic
// through immediately WITHOUT erasing what has been consumed under it, because
// the consumption is the ledger's and an operator restoring service is not
// making a billing decision.
func (s *Store) ClearSubjectBudget(ctx context.Context, sub Subject) error {
	return s.SetSubjectBudget(ctx, sub, BudgetCeiling{})
}

// MaxBudgetListScan bounds how far [Store.ListSubjectBudgets] will page.
//
// A page of budgets spans three tables, so the offset cannot be pushed into the
// queries — see there — and the rows before it have to be read and dropped. The
// cap is what keeps that from being an unbounded read behind a query parameter,
// and reaching it is REPORTED rather than silently truncated: a short page is
// how a caller decides it has reached the end of the list.
const MaxBudgetListScan = 10_000

// ErrBudgetPageTooDeep reports an offset past [MaxBudgetListScan].
var ErrBudgetPageTooDeep = errors.New("store: budget listing offset is beyond the bounded scan")

// ListSubjectBudgets enumerates every subject that declares a ceiling.
//
// It is three queries rather than a UNION, because the three tables have
// different columns and a UNION over them would need per-table NULL padding kept
// in the same order in three places.
//
// The page is applied across the CONCATENATION, ordered key, user, team, and not
// per table. Per-table paging was the first version and it was wrong in a way
// that reads as working: `limit: 100` returned up to three hundred rows next to
// a `"limit": 100` in the answer, and every page after the first re-ordered
// silently as the three tables ran out at different offsets. So each table is
// read up to `offset+limit` and the window is taken once, over one sequence.
func (s *Store) ListSubjectBudgets(ctx context.Context, limit, offset int) ([]SubjectBudget, error) {
	limit, offset = boundPage(limit, offset)
	span := offset + limit
	if span > MaxBudgetListScan {
		return nil, fmt.Errorf("%w: offset %d + limit %d exceeds %d",
			ErrBudgetPageTooDeep, offset, limit, MaxBudgetListScan)
	}
	out := make([]SubjectBudget, 0, 16)
	for _, k := range []SubjectKind{SubjectKey, SubjectUser, SubjectTeam} {
		table, _ := budgetTable(k)
		soft := `CAST(NULL AS BIGINT)`
		if k == SubjectKey {
			soft = `soft_budget_nano`
		}
		rows, err := s.query(ctx, `SELECT id, max_budget_nano, `+soft+`,
			budget_period, budget_reset_at, spend_nano, updated_at
			  FROM `+table+` WHERE max_budget_nano IS NOT NULL
			 ORDER BY id LIMIT ?`, span)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var (
				id                   string
				maxB, softB, resetAt sql.NullInt64
				period               sql.NullString
				spend, updated       int64
			)
			if err := rows.Scan(&id, &maxB, &softB, &period, &resetAt, &spend, &updated); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, SubjectBudget{
				Subject: Subject{Kind: k, ID: id},
				BudgetCeiling: BudgetCeiling{
					MaxBudgetNano:  nullableInt(maxB),
					SoftBudgetNano: nullableInt(softB),
					Period:         str(period),
					ResetAt:        TimeAt(nullInt(resetAt)),
				},
				SpendNano: spend,
				UpdatedAt: TimeAt(updated),
			})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		// Nothing past the window can ever be returned, whichever table it came
		// from, so the remaining tables are not read at all once it is full.
		if len(out) >= span {
			break
		}
	}
	if offset >= len(out) {
		return nil, nil
	}
	end := offset + limit
	if end > len(out) {
		end = len(out)
	}
	return out[offset:end], nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// deleteByIDs removes rows by primary key from one of this file's tables.
//
// table is a compile-time constant supplied by the two callers below and never a
// value; the only thing built from the ids is the NUMBER of placeholders, which
// is not one either.
func (s *Store) deleteByIDs(ctx context.Context, table string, ids []string) (int, error) {
	ids = dedupeNonEmpty(ids)
	if len(ids) == 0 {
		return 0, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	res, err := s.exec(ctx, `DELETE FROM `+table+` WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}

// affectedOrNotFound turns "the UPDATE matched nothing" into [ErrNotFound].
//
// A driver that cannot count rows is not a reason to report a write that may not
// have happened as a failure either, so it reports success — the same choice
// [Store.UpdateAPIKey] makes, for the same reason.
func affectedOrNotFound(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func boundPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = DefaultDirectoryListLimit
	}
	if limit > MaxDirectoryListLimit {
		limit = MaxDirectoryListLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// metadataOr keeps the NOT NULL metadata column at its documented default. An
// empty string would satisfy the constraint and break every reader that expects
// a JSON document.
func metadataOr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}
