package admin

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// userView is a users row on the wire (§11.4).
type userView struct {
	UserID string `json:"user_id"`
	Email  string `json:"user_email"`
	Name   string `json:"user_name"`
	Role   string `json:"user_role"`

	Spend         Money  `json:"spend"`
	MaxBudget     *Money `json:"max_budget"`
	BudgetPeriod  string `json:"budget_duration"`
	BudgetResetAt Stamp  `json:"budget_reset_at"`

	RPMLimit *int64 `json:"rpm_limit"`
	TPMLimit *int64 `json:"tpm_limit"`

	Models   []string `json:"models"`
	Blocked  bool     `json:"blocked"`
	Metadata string   `json:"metadata,omitempty"`

	CreatedAt Stamp `json:"created_at"`
	UpdatedAt Stamp `json:"updated_at"`
}

func viewUser(u *User) userView {
	return userView{
		UserID:        u.ID,
		Email:         u.Email,
		Name:          u.Name,
		Role:          u.Role,
		Spend:         Money(u.SpendNano),
		MaxBudget:     moneyPtr(u.MaxBudgetNano),
		BudgetPeriod:  u.BudgetPeriod,
		BudgetResetAt: Stamp(u.BudgetResetAt),
		RPMLimit:      u.RPMLimit,
		TPMLimit:      u.TPMLimit,
		Models:        orEmpty(u.Models),
		Blocked:       u.Blocked,
		Metadata:      u.Metadata,
		CreatedAt:     Stamp(u.CreatedAt),
		UpdatedAt:     Stamp(u.UpdatedAt),
	}
}

// userSpec is the mutable surface of a user. Pointers and an explicit `clear`
// list, for the same reason as [keySpec].
type userSpec struct {
	UserID *string `json:"user_id"`
	Email  *string `json:"user_email"`
	Name   *string `json:"user_name"`
	Role   *string `json:"user_role"`

	MaxBudget     *Money  `json:"max_budget"`
	BudgetPeriod  *string `json:"budget_duration"`
	BudgetResetAt *Stamp  `json:"budget_reset_at"`

	RPMLimit *int64 `json:"rpm_limit"`
	TPMLimit *int64 `json:"tpm_limit"`

	Models   *[]string `json:"models"`
	Blocked  *bool     `json:"blocked"`
	Metadata *string   `json:"metadata"`

	Clear []string `json:"clear"`
}

var userClearable = map[string]func(*User){
	"max_budget":      func(u *User) { u.MaxBudgetNano = nil },
	"budget_duration": func(u *User) { u.BudgetPeriod = ""; u.BudgetResetAt = time.Time{} },
	"rpm_limit":       func(u *User) { u.RPMLimit = nil },
	"tpm_limit":       func(u *User) { u.TPMLimit = nil },
	"models":          func(u *User) { u.Models = nil },
	"metadata":        func(u *User) { u.Metadata = "" },
}

// knownRoles is the administrative role vocabulary.
//
// A key is admin-capable because its owning user is; there is no role column on
// api_keys (§9.2). An unknown role is refused rather than stored, because a
// typo in a role is a silent privilege change in whichever direction the
// authenticator's comparison happens to fall.
var knownRoles = map[string]bool{
	"admin":           true,
	"admin_viewer":    true,
	"internal_user":   true,
	"internal_viewer": true,
	"proxy_admin":     true,
}

func (s *userSpec) apply(u *User) error {
	for _, name := range s.Clear {
		fn, ok := userClearable[strings.TrimSpace(name)]
		if !ok {
			return badRequest("clear names an unknown field %q", name).withParam("clear")
		}
		fn(u)
	}
	setString(&u.Email, s.Email)
	setString(&u.Name, s.Name)
	if s.Role != nil {
		role := strings.TrimSpace(*s.Role)
		if role != "" && !knownRoles[role] {
			return badRequest("user_role %q is not one of the known roles", role).withParam("user_role")
		}
		u.Role = role
	}
	if s.MaxBudget != nil {
		u.MaxBudgetNano = nanoPtr(s.MaxBudget)
	}
	setString(&u.BudgetPeriod, s.BudgetPeriod)
	if s.BudgetResetAt != nil {
		u.BudgetResetAt = s.BudgetResetAt.Time()
	}
	if s.RPMLimit != nil {
		u.RPMLimit = s.RPMLimit
	}
	if s.TPMLimit != nil {
		u.TPMLimit = s.TPMLimit
	}
	setStrings(&u.Models, s.Models)
	if s.Blocked != nil {
		u.Blocked = *s.Blocked
	}
	setString(&u.Metadata, s.Metadata)
	if u.Metadata != "" && !validJSONObject(u.Metadata) {
		return badRequest("metadata must be a JSON object").withParam("metadata")
	}
	return nil
}

// POST /user/new
func (c *call) userNew() error {
	d, err := c.a.directory()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	// Creating a directory user is deployment-wide: a user exists before any
	// team owns them, so there is no team for a scoped administrator to create
	// one "inside". A team administrator adds an EXISTING user to its team with
	// /team/member_add, which is scoped.
	if err := c.requireGlobal("creating a directory user"); err != nil {
		return err
	}
	var spec userSpec
	if err := decodeBody(c.w, c.r, &spec); err != nil {
		return err
	}
	if spec.Email == nil || strings.TrimSpace(*spec.Email) == "" {
		return badRequest("user_email is required").withParam("user_email")
	}
	now := c.a.now().UTC()
	u := &User{
		ID:        c.a.cfg.NewID(),
		Role:      "internal_user",
		Metadata:  "{}",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if spec.UserID != nil && strings.TrimSpace(*spec.UserID) != "" {
		u.ID = strings.TrimSpace(*spec.UserID)
	}
	if err := spec.apply(u); err != nil {
		return err
	}
	// Unlike `/key/generate`, this route accepts a caller-chosen id, so a key may
	// ALREADY name it — an imported credential whose owner had not been created
	// yet is the ordinary way to arrive here. Those keys were serving with no
	// owner's envelope and are about to serve with one, including its block flag
	// and its ceiling, so they are announced. On a freshly minted id this finds
	// nothing and publishes nothing, which is the normal case.
	owned, err := c.ownedKeys(KeyFilter{UserID: u.ID})
	if err != nil {
		return err
	}
	if err := d.CreateUser(c.ctx(), u); err != nil {
		if errors.Is(err, ErrConflict) {
			return newFault(http.StatusConflict, CodeConflict, typeInvalidRequest,
				"a user with that id or email already exists")
		}
		return err
	}
	view := viewUser(u)
	if err := c.appliedTo(owned, CauseUpdated, "user.new", "user", u.ID, nil, view); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"user": view})
	return nil
}

// GET|POST /user/info
func (c *call) userInfo() error {
	d, err := c.a.directory()
	if err != nil {
		return err
	}
	var ref struct {
		UserID string `json:"user_id"`
	}
	if err := decodeOptionalBody(c.w, c.r, &ref); err != nil {
		return err
	}
	id := firstNonEmpty(ref.UserID, queryString(c.r, "user_id"))
	if id == "" {
		return badRequest("user_id is required").withParam("user_id")
	}
	u, err := c.loadUser(d, id)
	if err != nil {
		return err
	}
	out := map[string]any{"user": viewUser(u)}

	// The keys a user owns are part of "user info" for every operator who asks
	// the question, and it is one query on an index that exists. They are
	// filtered by scope: a user may hold keys on several teams, and a
	// administrator of one of them has no business reading the others.
	if ks := c.a.cfg.Keys; ks != nil {
		keys, err := ks.ListKeys(c.ctx(), KeyFilter{UserID: id, Limit: c.a.cfg.ListLimit})
		if err != nil {
			return err
		}
		keys = c.keepKeysInScope(keys)
		c.hydrateSpend(keys...)
		views := make([]keyView, 0, len(keys))
		for _, k := range keys {
			views = append(views, viewKey(k))
		}
		out["keys"] = views
	}
	writeJSON(c.w, c.r, http.StatusOK, out)
	return nil
}

// POST /user/update
func (c *call) userUpdate() error {
	d, err := c.a.directory()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var spec userSpec
	if err := decodeBody(c.w, c.r, &spec); err != nil {
		return err
	}
	if spec.UserID == nil || strings.TrimSpace(*spec.UserID) == "" {
		return badRequest("user_id is required").withParam("user_id")
	}
	id := strings.TrimSpace(*spec.UserID)
	u, err := c.loadUser(d, id)
	if err != nil {
		return err
	}
	before := viewUser(u)
	updated := *u
	if err := spec.apply(&updated); err != nil {
		return err
	}
	// user_role is the bit that decides who is an administrator at all
	// (Principal.IsAdmin joins through it), so writing it is an escalation
	// primitive: a team administrator that could set a role could promote
	// itself, or one of its members, to global. It stays with the operator.
	if updated.Role != u.Role {
		if err := c.requireGlobal("changing a user's administrative role"); err != nil {
			return err
		}
	}
	// Every field this handler writes except the role and the metadata is an
	// authorization field the hot path reads off its cached snapshot, through
	// [auth.Principal.User]: the block flag, the model allow-list, the rate
	// ceilings, the budget and its period. An update that is not announced is
	// therefore an update the fleet honours a credential-cache TTL later —
	// sixty seconds — which is what `{"blocked": true}` through this route used
	// to mean. `blocked` is singled out only in the CAUSE, because blocking the
	// person is what an operator does when someone leaves and a node's log
	// should spell it that way.
	owned, err := c.ownedKeys(KeyFilter{UserID: id})
	if err != nil {
		return err
	}
	cause := CauseUpdated
	if updated.Blocked && !u.Blocked {
		cause = CauseRevoked
	}
	updated.UpdatedAt = c.a.now().UTC()
	if err := d.UpdateUser(c.ctx(), &updated); err != nil {
		return err
	}
	after := viewUser(&updated)
	if err := c.appliedTo(owned, cause, "user.update", "user", id, before, after); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"user": after})
	return nil
}

// POST /user/delete
func (c *call) userDelete() error {
	d, err := c.a.directory()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	// The mirror of userNew: removing a user from the directory removes them
	// from every team, including teams this caller does not administer.
	if err := c.requireGlobal("deleting a directory user"); err != nil {
		return err
	}
	var body struct {
		UserIDs []string `json:"user_ids"`
		UserID  string   `json:"user_id"`
	}
	if err := decodeBody(c.w, c.r, &body); err != nil {
		return err
	}
	ids := mergeIDs(body.UserIDs, []string{body.UserID})
	if len(ids) == 0 {
		return badRequest("user_ids must name at least one user").withParam("user_ids")
	}
	before := make([]userView, 0, len(ids))
	var owned []string
	for _, id := range ids {
		u, err := d.GetUser(c.ctx(), id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return err
		}
		before = append(before, viewUser(u))
		// Enumerated before the delete, because afterwards there is nothing left
		// to enumerate. Whether the store cascades the keys away or leaves them
		// orphaned, every one of them is serving from a cached entry that names
		// an owner who is about to stop existing.
		ks, err := c.ownedKeys(KeyFilter{UserID: id})
		if err != nil {
			return err
		}
		owned = append(owned, ks...)
	}
	n, err := d.DeleteUsers(c.ctx(), ids)
	if err != nil {
		return err
	}
	// CauseRevoked rather than CauseUpdated: removing a person from the directory
	// is the incident spelling of this route — it is what an operator runs when
	// someone leaves — and a node's log should say the credentials were revoked
	// rather than that something about them changed.
	if err := c.appliedTo(owned, CauseRevoked, "user.delete", "user", strings.Join(ids, ","),
		map[string]any{"users": before}, map[string]any{"deleted": n}); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"deleted": n, "deleted_users": ids})
	return nil
}

// GET|POST /user/list
func (c *call) userList() error {
	d, err := c.a.directory()
	if err != nil {
		return err
	}
	o, err := c.listOptions()
	if err != nil {
		return err
	}
	users, err := d.ListUsers(c.ctx(), o)
	if err != nil {
		return err
	}
	out := make([]userView, 0, len(users))
	for _, u := range users {
		ok, err := c.userInScope(d, u.ID)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		out = append(out, viewUser(u))
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"users":  out,
		"limit":  o.Limit,
		"offset": o.Offset,
		"count":  len(out),
	})
	return nil
}

// listOptions reads and bounds a directory listing's pagination.
func (c *call) listOptions() (ListOptions, error) {
	limit, err := queryInt(c.r, "limit", c.a.cfg.ListLimit)
	if err != nil {
		return ListOptions{}, err
	}
	offset, err := queryInt(c.r, "offset", 0)
	if err != nil {
		return ListOptions{}, err
	}
	if limit <= 0 || limit > c.a.cfg.MaxListLimit {
		return ListOptions{}, badRequest("limit must be between 1 and %d", c.a.cfg.MaxListLimit).withParam("limit")
	}
	if offset < 0 {
		return ListOptions{}, badRequest("offset must not be negative").withParam("offset")
	}
	return ListOptions{Limit: limit, Offset: offset}, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
