package admin

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// teamView is a teams row on the wire (§11.4).
type teamView struct {
	TeamID         string `json:"team_id"`
	TeamName       string `json:"team_name"`
	TeamAlias      string `json:"team_alias,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`

	Spend         Money  `json:"spend"`
	MaxBudget     *Money `json:"max_budget"`
	BudgetPeriod  string `json:"budget_duration"`
	BudgetResetAt Stamp  `json:"budget_reset_at"`

	RPMLimit    *int64 `json:"rpm_limit"`
	TPMLimit    *int64 `json:"tpm_limit"`
	MaxParallel *int64 `json:"max_parallel_requests"`

	Models   []string `json:"models"`
	Blocked  bool     `json:"blocked"`
	Metadata string   `json:"metadata,omitempty"`

	Members []memberView `json:"members"`

	CreatedAt Stamp `json:"created_at"`
	UpdatedAt Stamp `json:"updated_at"`
}

// memberView is a team_members row.
type memberView struct {
	UserID    string `json:"user_id"`
	Role      string `json:"role"`
	Spend     Money  `json:"spend"`
	MaxBudget *Money `json:"max_budget"`
	CreatedAt Stamp  `json:"created_at"`
}

func viewTeam(t *Team, members []TeamMember) teamView {
	mv := make([]memberView, 0, len(members))
	for _, m := range members {
		mv = append(mv, memberView{
			UserID:    m.UserID,
			Role:      m.Role,
			Spend:     Money(m.SpendNano),
			MaxBudget: moneyPtr(m.MaxBudgetNano),
			CreatedAt: Stamp(m.CreatedAt),
		})
	}
	return teamView{
		TeamID:         t.ID,
		TeamName:       t.Name,
		TeamAlias:      t.Alias,
		OrganizationID: t.OrganizationID,
		Spend:          Money(t.SpendNano),
		MaxBudget:      moneyPtr(t.MaxBudgetNano),
		BudgetPeriod:   t.BudgetPeriod,
		BudgetResetAt:  Stamp(t.BudgetResetAt),
		RPMLimit:       t.RPMLimit,
		TPMLimit:       t.TPMLimit,
		MaxParallel:    t.MaxParallel,
		Models:         orEmpty(t.Models),
		Blocked:        t.Blocked,
		Metadata:       t.Metadata,
		Members:        mv,
		CreatedAt:      Stamp(t.CreatedAt),
		UpdatedAt:      Stamp(t.UpdatedAt),
	}
}

type teamSpec struct {
	TeamID         *string `json:"team_id"`
	TeamName       *string `json:"team_name"`
	TeamAlias      *string `json:"team_alias"`
	OrganizationID *string `json:"organization_id"`

	MaxBudget     *Money  `json:"max_budget"`
	BudgetPeriod  *string `json:"budget_duration"`
	BudgetResetAt *Stamp  `json:"budget_reset_at"`

	RPMLimit    *int64 `json:"rpm_limit"`
	TPMLimit    *int64 `json:"tpm_limit"`
	MaxParallel *int64 `json:"max_parallel_requests"`

	Models   *[]string `json:"models"`
	Blocked  *bool     `json:"blocked"`
	Metadata *string   `json:"metadata"`

	Clear []string `json:"clear"`
}

var teamClearable = map[string]func(*Team){
	"max_budget":            func(t *Team) { t.MaxBudgetNano = nil },
	"budget_duration":       func(t *Team) { t.BudgetPeriod = ""; t.BudgetResetAt = time.Time{} },
	"rpm_limit":             func(t *Team) { t.RPMLimit = nil },
	"tpm_limit":             func(t *Team) { t.TPMLimit = nil },
	"max_parallel_requests": func(t *Team) { t.MaxParallel = nil },
	"models":                func(t *Team) { t.Models = nil },
	"team_alias":            func(t *Team) { t.Alias = "" },
	"organization_id":       func(t *Team) { t.OrganizationID = "" },
	"metadata":              func(t *Team) { t.Metadata = "" },
}

func (s *teamSpec) apply(t *Team) error {
	for _, name := range s.Clear {
		fn, ok := teamClearable[strings.TrimSpace(name)]
		if !ok {
			return badRequest("clear names an unknown field %q", name).withParam("clear")
		}
		fn(t)
	}
	setString(&t.Name, s.TeamName)
	setString(&t.Alias, s.TeamAlias)
	setString(&t.OrganizationID, s.OrganizationID)
	if s.MaxBudget != nil {
		t.MaxBudgetNano = nanoPtr(s.MaxBudget)
	}
	setString(&t.BudgetPeriod, s.BudgetPeriod)
	if s.BudgetResetAt != nil {
		t.BudgetResetAt = s.BudgetResetAt.Time()
	}
	if s.RPMLimit != nil {
		t.RPMLimit = s.RPMLimit
	}
	if s.TPMLimit != nil {
		t.TPMLimit = s.TPMLimit
	}
	if s.MaxParallel != nil {
		t.MaxParallel = s.MaxParallel
	}
	setStrings(&t.Models, s.Models)
	if s.Blocked != nil {
		t.Blocked = *s.Blocked
	}
	setString(&t.Metadata, s.Metadata)
	if t.Metadata != "" && !validJSONObject(t.Metadata) {
		return badRequest("metadata must be a JSON object").withParam("metadata")
	}
	return nil
}

// POST /team/new
func (c *call) teamNew() error {
	d, err := c.a.directory()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var spec teamSpec
	if err := decodeBody(c.w, c.r, &spec); err != nil {
		return err
	}
	now := c.a.now().UTC()
	t := &Team{ID: c.a.cfg.NewID(), Metadata: "{}", CreatedAt: now, UpdatedAt: now}
	if spec.TeamID != nil && strings.TrimSpace(*spec.TeamID) != "" {
		t.ID = strings.TrimSpace(*spec.TeamID)
	}
	if err := spec.apply(t); err != nil {
		return err
	}
	if t.Name == "" {
		return badRequest("team_name is required").withParam("team_name")
	}
	if err := d.CreateTeam(c.ctx(), t); err != nil {
		if errors.Is(err, ErrConflict) {
			return newFault(http.StatusConflict, CodeConflict, typeInvalidRequest,
				"a team with that id already exists")
		}
		return err
	}
	view := viewTeam(t, nil)
	if err := c.recordAudit("team.new", "team", t.ID, nil, view); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"team": view})
	return nil
}

// GET|POST /team/info
func (c *call) teamInfo() error {
	d, err := c.a.directory()
	if err != nil {
		return err
	}
	var ref struct {
		TeamID string `json:"team_id"`
	}
	if err := decodeOptionalBody(c.w, c.r, &ref); err != nil {
		return err
	}
	id := firstNonEmpty(ref.TeamID, queryString(c.r, "team_id"))
	if id == "" {
		return badRequest("team_id is required").withParam("team_id")
	}
	t, members, err := c.loadTeam(d, id)
	if err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"team": viewTeam(t, members)})
	return nil
}

func (c *call) loadTeam(d Directory, id string) (*Team, []TeamMember, error) {
	t, err := d.GetTeam(c.ctx(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, notFound("team", id)
		}
		return nil, nil, err
	}
	members, err := d.ListTeamMembers(c.ctx(), id)
	if err != nil {
		return nil, nil, err
	}
	return t, members, nil
}

// POST /team/update
func (c *call) teamUpdate() error {
	d, err := c.a.directory()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var spec teamSpec
	if err := decodeBody(c.w, c.r, &spec); err != nil {
		return err
	}
	if spec.TeamID == nil || strings.TrimSpace(*spec.TeamID) == "" {
		return badRequest("team_id is required").withParam("team_id")
	}
	id := strings.TrimSpace(*spec.TeamID)
	t, members, err := c.loadTeam(d, id)
	if err != nil {
		return err
	}
	before := viewTeam(t, members)
	updated := *t
	if err := spec.apply(&updated); err != nil {
		return err
	}
	updated.UpdatedAt = c.a.now().UTC()
	if err := d.UpdateTeam(c.ctx(), &updated); err != nil {
		return err
	}
	after := viewTeam(&updated, members)
	if err := c.recordAudit("team.update", "team", id, before, after); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"team": after})
	return nil
}

// POST /team/delete
func (c *call) teamDelete() error {
	d, err := c.a.directory()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var body struct {
		TeamIDs []string `json:"team_ids"`
		TeamID  string   `json:"team_id"`
	}
	if err := decodeBody(c.w, c.r, &body); err != nil {
		return err
	}
	ids := mergeIDs(body.TeamIDs, []string{body.TeamID})
	if len(ids) == 0 {
		return badRequest("team_ids must name at least one team").withParam("team_ids")
	}
	before := make([]teamView, 0, len(ids))
	for _, id := range ids {
		t, err := d.GetTeam(c.ctx(), id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return err
		}
		members, err := d.ListTeamMembers(c.ctx(), id)
		if err != nil {
			return err
		}
		before = append(before, viewTeam(t, members))
	}
	n, err := d.DeleteTeams(c.ctx(), ids)
	if err != nil {
		return err
	}
	if err := c.recordAudit("team.delete", "team", strings.Join(ids, ","),
		map[string]any{"teams": before}, map[string]any{"deleted": n}); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"deleted": n, "deleted_teams": ids})
	return nil
}

// GET|POST /team/list
func (c *call) teamList() error {
	d, err := c.a.directory()
	if err != nil {
		return err
	}
	o, err := c.listOptions()
	if err != nil {
		return err
	}
	teams, err := d.ListTeams(c.ctx(), o)
	if err != nil {
		return err
	}
	out := make([]teamView, 0, len(teams))
	for _, t := range teams {
		members, err := d.ListTeamMembers(c.ctx(), t.ID)
		if err != nil {
			return err
		}
		out = append(out, viewTeam(t, members))
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"teams":  out,
		"limit":  o.Limit,
		"offset": o.Offset,
		"count":  len(out),
	})
	return nil
}

// memberSpec is one membership change.
type memberSpec struct {
	TeamID string `json:"team_id"`
	// Member is the incumbent's nested form; user_id is the flat one. Both are
	// accepted because both are in the wild.
	Member    *memberRef `json:"member"`
	UserID    string     `json:"user_id"`
	Role      string     `json:"role"`
	MaxBudget *Money     `json:"max_budget"`
}

type memberRef struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

func (s *memberSpec) resolve() (userID, role string) {
	userID, role = s.UserID, s.Role
	if s.Member != nil {
		if userID == "" {
			userID = s.Member.UserID
		}
		if role == "" {
			role = s.Member.Role
		}
	}
	if role == "" {
		role = "member"
	}
	return strings.TrimSpace(userID), strings.TrimSpace(role)
}

// POST /team/member_add
func (c *call) teamMemberAdd() error {
	d, err := c.a.directory()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var spec memberSpec
	if err := decodeBody(c.w, c.r, &spec); err != nil {
		return err
	}
	teamID := strings.TrimSpace(spec.TeamID)
	if teamID == "" {
		return badRequest("team_id is required").withParam("team_id")
	}
	userID, role := spec.resolve()
	if userID == "" {
		return badRequest("user_id is required").withParam("user_id")
	}
	t, members, err := c.loadTeam(d, teamID)
	if err != nil {
		return err
	}
	before := viewTeam(t, members)

	m := TeamMember{
		TeamID:        teamID,
		UserID:        userID,
		Role:          role,
		MaxBudgetNano: nanoPtr(spec.MaxBudget),
		CreatedAt:     c.a.now().UTC(),
	}
	if err := d.AddTeamMember(c.ctx(), m); err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound("user", userID)
		}
		if errors.Is(err, ErrConflict) {
			return newFault(http.StatusConflict, CodeConflict, typeInvalidRequest,
				"user %q is already a member of team %q", userID, teamID)
		}
		return err
	}
	members, err = d.ListTeamMembers(c.ctx(), teamID)
	if err != nil {
		return err
	}
	after := viewTeam(t, members)
	if err := c.recordAudit("team.member_add", "team", teamID, before, after); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"team": after})
	return nil
}

// POST /team/member_delete
func (c *call) teamMemberDelete() error {
	d, err := c.a.directory()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var spec memberSpec
	if err := decodeBody(c.w, c.r, &spec); err != nil {
		return err
	}
	teamID := strings.TrimSpace(spec.TeamID)
	if teamID == "" {
		return badRequest("team_id is required").withParam("team_id")
	}
	userID, _ := spec.resolve()
	if userID == "" {
		return badRequest("user_id is required").withParam("user_id")
	}
	t, members, err := c.loadTeam(d, teamID)
	if err != nil {
		return err
	}
	before := viewTeam(t, members)
	if err := d.RemoveTeamMember(c.ctx(), teamID, userID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound("team member", userID)
		}
		return err
	}
	members, err = d.ListTeamMembers(c.ctx(), teamID)
	if err != nil {
		return err
	}
	after := viewTeam(t, members)
	if err := c.recordAudit("team.member_delete", "team", teamID, before, after); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"team": after})
	return nil
}
