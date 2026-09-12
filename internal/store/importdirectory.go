package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Source tables for the directory import: the incumbent's teams and users.
const (
	DefaultTeamTable = "LiteLLM_TeamTable"
	DefaultUserTable = "LiteLLM_UserTable"
)

// DirectoryImportOptions governs [Store.ImportTeams] and [Store.ImportUsers].
//
// # Why these exist beside ImportKeys
//
// `import keys` carries a key's team_id and user_id and then warns: "imported
// keys reference teams that are not present; their team-scoped limits do not
// apply". MIGRATION §3.3 calls that the fail-open failure, and it was the ONLY
// outcome an operator could reach, because nothing created the rows the keys
// referred to. A team's budget ceiling, rate limit, model allow-list and
// blocked flag are enforced on the request path now; a plain key import
// planted none of them. These two verbs do.
type DirectoryImportOptions struct {
	// Table is the source table. Zero value: [DefaultTeamTable] or
	// [DefaultUserTable] by verb.
	Table string
	// Now is the instant recorded on rows without their own timestamps. Zero
	// means the store clock.
	Now time.Time
	// OnUntranslatable is the key importer's policy applied to the model
	// allow-list, with the same meaning (MIGRATION §3.6). Defaults to skip.
	OnUntranslatable UntranslatablePolicy
	// DryRun reads and reports without writing.
	DryRun bool
	// Limit reads at most N source rows; zero reads all.
	Limit int
	// Members (teams only) carries `members_with_roles` into team_members.
	// A member whose user row is absent is reported, not invented: import
	// users first.
	Members bool
	// SyntheticEmailDomain (users only) gives a user without an email one
	// spelled `<user_id>@<domain>`, because dorang requires an email on a
	// user row and the incumbent does not. Empty skips such users, reported.
	SyntheticEmailDomain string
}

// DirectoryReport is what one directory import found and did.
type DirectoryReport struct {
	Table   string
	DryRun  bool
	Scanned int
	// Imported counts rows written, or that WOULD be written on a dry run.
	Imported int
	// AlreadyPresent counts rows whose id exists here and were left alone: a
	// re-run must not undo an administrator's later change.
	AlreadyPresent int
	Skipped        int
	// Members counts team_members rows written from members_with_roles, and
	// MembersSkipped the ones whose user is not present.
	Members        int
	MembersSkipped int

	NotMigrated    []NotMigrated
	DroppedColumns []DroppedColumn
	Untranslated   []Untranslated
	Warnings       []string
}

func (r *DirectoryReport) skip(lookup, reason, detail string) {
	r.Skipped++
	r.NotMigrated = append(r.NotMigrated, NotMigrated{Lookup: lookup, Reason: reason, Detail: detail})
}

func (r *DirectoryReport) untranslated(lookup, column, value, action, detail string) bool {
	r.Untranslated = append(r.Untranslated, Untranslated{
		Lookup: lookup, Column: column, Value: value, Action: action, Detail: detail,
	})
	if action == actionRefused {
		r.skip(lookup, "untranslatable "+column, fmt.Sprintf("%q: %s", value, detail))
		return false
	}
	return true
}

func (r *DirectoryReport) warn(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	for _, w := range r.Warnings {
		if w == msg {
			return
		}
	}
	r.Warnings = append(r.Warnings, msg)
}

// Summary renders the report in the key importer's shape.
func (r DirectoryReport) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "import from %s: scanned %d, imported %d, skipped %d\n",
		r.Table, r.Scanned, r.Imported, r.Skipped)
	if r.DryRun {
		b.WriteString("  (dry run: nothing was written)\n")
	}
	if r.AlreadyPresent > 0 {
		fmt.Fprintf(&b, "  already present, left untouched: %d\n", r.AlreadyPresent)
	}
	if r.Members > 0 || r.MembersSkipped > 0 {
		fmt.Fprintf(&b, "  team members carried: %d, skipped because the user is not present: %d\n",
			r.Members, r.MembersSkipped)
	}
	for _, u := range r.Untranslated {
		fmt.Fprintf(&b, "  %s [%s]: %s = %q -- %s\n", u.Action, u.Lookup, u.Column, u.Value, u.Detail)
	}
	for _, d := range r.DroppedColumns {
		fmt.Fprintf(&b, "  column not migrated: %s -- %s\n", d.Column, d.Reason)
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "  WARNING: %s\n", w)
	}
	for _, n := range r.NotMigrated {
		fmt.Fprintf(&b, "  not migrated [%s]: %s %s\n", n.Lookup, n.Reason, n.Detail)
	}
	return b.String()
}

// The incumbent's team columns and where each lands.
var teamImportedColumns = map[string]string{
	"team_id":               "id",
	"team_alias":            "alias / name",
	"organization_id":       "organization_id",
	"max_budget":            "max_budget_nano",
	"spend":                 "spend_nano",
	"budget_duration":       "budget_period",
	"budget_reset_at":       "budget_reset_at",
	"tpm_limit":             "tpm_limit",
	"rpm_limit":             "rpm_limit",
	"max_parallel_requests": "max_parallel",
	"models":                "models (allow-list)",
	"blocked":               "blocked",
	"metadata":              "metadata",
	"members_with_roles":    "team_members (with --members)",
	"created_at":            "created_at",
	"updated_at":            "updated_at",
}

// teamDroppedColumns are the team columns with a stated reason, so the report
// says why rather than "no equivalent".
var teamDroppedColumns = map[string]string{
	"admins":                     "membership is carried from members_with_roles, which holds both admins and members with their roles",
	"members":                    "membership is carried from members_with_roles, which holds both admins and members with their roles",
	"model_spend":                "per-model spend is a ledger fact in dorang, not a directory column",
	"model_max_budget":           "budgets are per subject in dorang; a per-model ceiling is expressed with pricing rules and key or team budgets",
	"soft_budget":                "soft budgets are per key in dorang (api_keys.soft_budget_nano)",
	"budget_limits":              "the incumbent's per-window sub-limits; dorang's ceiling is one amount over one period",
	"team_member_permissions":    "no dorang equivalent; team members act through their keys",
	"default_team_member_models": "no dorang equivalent; a member's keys carry their own allow-lists",
	"object_permission_id":       "resolved per KEY by import keys; a team-level permission object is not consulted",
}

// The incumbent's user columns and where each lands.
var userImportedColumns = map[string]string{
	"user_id":         "id",
	"user_alias":      "name",
	"user_email":      "email",
	"user_role":       "role",
	"max_budget":      "max_budget_nano",
	"spend":           "spend_nano",
	"budget_duration": "budget_period",
	"budget_reset_at": "budget_reset_at",
	"tpm_limit":       "tpm_limit",
	"rpm_limit":       "rpm_limit",
	"models":          "models (allow-list)",
	"metadata":        "metadata",
	"created_at":      "created_at",
	"updated_at":      "updated_at",
}

var userDroppedColumns = map[string]string{
	"password":               "credential material; dorang has no user login and stores no password",
	"sso_user_id":            "identity-provider linkage; dorang has no user login",
	"team_id":                "membership is carried by the TEAM side (members_with_roles, import teams --members); a key carries its own team_id",
	"teams":                  "membership is carried by the TEAM side (members_with_roles, import teams --members); a key carries its own team_id",
	"max_parallel_requests":  "per key in dorang; a user has no parallelism ceiling of its own",
	"model_spend":            "per-model spend is a ledger fact in dorang, not a directory column",
	"model_max_budget":       "budgets are per subject in dorang; a per-model ceiling is expressed with pricing rules and key or user budgets",
	"allowed_cache_controls": "no dorang equivalent",
	"object_permission_id":   "resolved per KEY by import keys; a user-level permission object is not consulted",
}

// ImportTeams migrates teams from a LiteLLM_TeamTable-shaped source.
func (s *Store) ImportTeams(ctx context.Context, src *sql.DB, opts DirectoryImportOptions) (DirectoryReport, error) {
	if opts.Table == "" {
		opts.Table = DefaultTeamTable
	}
	return s.importDirectory(ctx, src, opts, "teams", "team_id", teamImportedColumns, teamDroppedColumns, s.importTeamRow)
}

// ImportUsers migrates users from a LiteLLM_UserTable-shaped source.
func (s *Store) ImportUsers(ctx context.Context, src *sql.DB, opts DirectoryImportOptions) (DirectoryReport, error) {
	if opts.Table == "" {
		opts.Table = DefaultUserTable
	}
	return s.importDirectory(ctx, src, opts, "users", "user_id", userImportedColumns, userDroppedColumns, s.importUserRow)
}

// rowImporter builds and writes one row, reporting what it could not carry.
type rowImporter func(ctx context.Context, rec map[string]any, now time.Time, opts DirectoryImportOptions, rep *DirectoryReport) error

func (s *Store) importDirectory(ctx context.Context, src *sql.DB, opts DirectoryImportOptions,
	dest, idCol string, carried, dropped map[string]string, one rowImporter) (DirectoryReport, error) {

	if src == nil {
		return DirectoryReport{}, errors.New("store: import needs a source database")
	}
	if !validIdent(opts.Table) {
		return DirectoryReport{}, fmt.Errorf("%w: source table %q", ErrBadIdentifier, opts.Table)
	}
	if opts.OnUntranslatable == "" {
		opts.OnUntranslatable = UntranslatableSkip
	}
	now := opts.Now
	if now.IsZero() {
		now = s.now()
	}
	rep := DirectoryReport{Table: opts.Table, DryRun: opts.DryRun}

	present, err := sourceColumns(ctx, src, opts.Table)
	if err != nil {
		return rep, err
	}
	if _, ok := present[idCol]; !ok {
		return rep, fmt.Errorf("store: source table %q has no %s column", opts.Table, idCol)
	}
	var selected []string
	for col := range present {
		lc := strings.ToLower(col)
		if _, ok := carried[lc]; ok {
			selected = append(selected, col)
			continue
		}
		reason, ok := dropped[lc]
		if !ok {
			reason = "no dorang equivalent; review before decommissioning the source"
		}
		rep.DroppedColumns = append(rep.DroppedColumns, DroppedColumn{Column: col, Reason: reason})
	}
	sort.Strings(selected)
	sort.Slice(rep.DroppedColumns, func(i, j int) bool {
		return rep.DroppedColumns[i].Column < rep.DroppedColumns[j].Column
	})

	quoted := make([]string, len(selected))
	for i, c := range selected {
		quoted[i] = quoteIdent(c)
	}
	q := "SELECT " + strings.Join(quoted, ", ") + " FROM " + quoteIdent(opts.Table)
	if opts.Limit > 0 {
		q += " LIMIT " + strconv.Itoa(opts.Limit)
	}
	rows, err := src.QueryContext(ctx, q)
	if err != nil {
		return rep, fmt.Errorf("store: read %s: %w", opts.Table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return rep, err
	}
	known := map[string]bool{}
	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return rep, err
		}
		rec := map[string]any{}
		for i, c := range cols {
			rec[strings.ToLower(c)] = raw[i]
		}
		rep.Scanned++
		id := strings.TrimSpace(asString(rec[idCol]))
		if id == "" {
			rep.skip("(row "+strconv.Itoa(rep.Scanned)+")", "no id", "the "+idCol+" column is empty")
			continue
		}
		// A row already here is left alone: a re-run must not undo an
		// administrator's later change, the same rule the key importer keeps.
		exists, err := s.exists(ctx, known, dest, id)
		if err != nil {
			return rep, err
		}
		if exists {
			rep.AlreadyPresent++
			continue
		}
		if err := one(ctx, rec, now, opts, &rep); err != nil {
			return rep, err
		}
	}
	return rep, rows.Err()
}

// importTeamRow builds one team and, unless dry-running, writes it and its
// members.
func (s *Store) importTeamRow(ctx context.Context, rec map[string]any, now time.Time,
	opts DirectoryImportOptions, rep *DirectoryReport) error {

	id := strings.TrimSpace(asString(rec["team_id"]))
	t := &Team{
		ID:             id,
		Alias:          strings.TrimSpace(asString(rec["team_alias"])),
		OrganizationID: strings.TrimSpace(asString(rec["organization_id"])),
		BudgetPeriod:   strings.TrimSpace(asString(rec["budget_duration"])),
		RPMLimit:       asIntPtr(rec["rpm_limit"]),
		TPMLimit:       asIntPtr(rec["tpm_limit"]),
		MaxParallel:    asIntPtr(rec["max_parallel_requests"]),
		Blocked:        asBool(rec["blocked"]),
		Metadata:       jsonOrEmpty(rec["metadata"]),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	t.Name = t.Alias
	if t.Name == "" {
		t.Name = id
	}
	if v, ok := asTime(rec["budget_reset_at"]); ok {
		t.BudgetResetAt = v
	}
	if v, ok := asTime(rec["created_at"]); ok {
		t.CreatedAt = v
	}
	if v, ok := asTime(rec["updated_at"]); ok {
		t.UpdatedAt = v
	}
	if v, ok := rec["max_budget"]; ok && v != nil {
		nano, err := nanoFromAny(v)
		if err != nil {
			rep.skip(id, "budget out of range", err.Error())
			return nil
		}
		t.MaxBudgetNano = int64p(nano)
	}
	if v, ok := rec["spend"]; ok && v != nil {
		nano, err := nanoFromAny(v)
		if err != nil {
			rep.skip(id, "spend out of range", err.Error())
			return nil
		}
		t.SpendNano = nano
	}
	models, ok := translateModels(id, asList(rec["models"]), opts.OnUntranslatable, rep)
	if !ok {
		return nil
	}
	t.Models = models
	if err := checkTeamAmounts(t); err != nil {
		rep.skip(id, "amount refused", err.Error())
		return nil
	}

	members := teamMembersOf(rec["members_with_roles"])
	rep.Imported++
	if opts.DryRun {
		if opts.Members {
			rep.Members += len(members)
		}
		return nil
	}
	if err := s.InsertTeam(ctx, t); err != nil {
		return fmt.Errorf("store: import team %s: %w", id, err)
	}
	if !opts.Members {
		return nil
	}
	for _, m := range members {
		m.TeamID = id
		err := s.AddTeamMember(ctx, m)
		switch {
		case err == nil:
			rep.Members++
		case errors.Is(err, ErrExists):
			rep.Members++
		case errors.Is(err, ErrNotFound):
			rep.MembersSkipped++
			rep.warn("team members whose user row is not present were not carried; run `import users` first, then re-run `import teams` — teams already present are left untouched, so carry members on the first run")
		default:
			return fmt.Errorf("store: import team %s member %s: %w", id, m.UserID, err)
		}
	}
	return nil
}

// teamMembersOf reads the incumbent's `members_with_roles`:
// `[{"user_id": "...", "role": "admin", "user_email": "..."}]`.
func teamMembersOf(v any) []TeamMember {
	var raw string
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		raw = t
	case []byte:
		raw = string(t)
	default:
		return nil
	}
	var in []struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if json.Unmarshal([]byte(raw), &in) != nil {
		return nil
	}
	out := make([]TeamMember, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		uid := strings.TrimSpace(m.UserID)
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		out = append(out, TeamMember{UserID: uid, Role: strings.TrimSpace(m.Role)})
	}
	return out
}

// importUserRow builds one user and, unless dry-running, writes it.
func (s *Store) importUserRow(ctx context.Context, rec map[string]any, now time.Time,
	opts DirectoryImportOptions, rep *DirectoryReport) error {

	id := strings.TrimSpace(asString(rec["user_id"]))
	u := &User{
		ID:           id,
		Email:        strings.TrimSpace(asString(rec["user_email"])),
		Name:         strings.TrimSpace(asString(rec["user_alias"])),
		Role:         strings.TrimSpace(asString(rec["user_role"])),
		BudgetPeriod: strings.TrimSpace(asString(rec["budget_duration"])),
		RPMLimit:     asIntPtr(rec["rpm_limit"]),
		TPMLimit:     asIntPtr(rec["tpm_limit"]),
		Metadata:     jsonOrEmpty(rec["metadata"]),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if u.Email == "" {
		if opts.SyntheticEmailDomain == "" {
			rep.skip(id, "no email",
				"dorang requires an email on a user row and the source has none; re-run with a synthetic "+
					"email domain to give it `<user_id>@<domain>`, or add the email at the source")
			return nil
		}
		u.Email = id + "@" + opts.SyntheticEmailDomain
	}
	if v, ok := asTime(rec["budget_reset_at"]); ok {
		u.BudgetResetAt = v
	}
	if v, ok := asTime(rec["created_at"]); ok {
		u.CreatedAt = v
	}
	if v, ok := asTime(rec["updated_at"]); ok {
		u.UpdatedAt = v
	}
	if v, ok := rec["max_budget"]; ok && v != nil {
		nano, err := nanoFromAny(v)
		if err != nil {
			rep.skip(id, "budget out of range", err.Error())
			return nil
		}
		u.MaxBudgetNano = int64p(nano)
	}
	if v, ok := rec["spend"]; ok && v != nil {
		nano, err := nanoFromAny(v)
		if err != nil {
			rep.skip(id, "spend out of range", err.Error())
			return nil
		}
		u.SpendNano = nano
	}
	models, ok := translateModels(id, asList(rec["models"]), opts.OnUntranslatable, rep)
	if !ok {
		return nil
	}
	u.Models = models
	rep.Imported++
	if opts.DryRun {
		return nil
	}
	if err := s.InsertUser(ctx, u); err != nil {
		return fmt.Errorf("store: import user %s: %w", id, err)
	}
	return nil
}

// jsonOrEmpty keeps a metadata value only when it is a JSON document.
func jsonOrEmpty(v any) string {
	var raw string
	switch t := v.(type) {
	case string:
		raw = t
	case []byte:
		raw = string(t)
	default:
		return ""
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || !json.Valid([]byte(raw)) {
		return ""
	}
	return raw
}
