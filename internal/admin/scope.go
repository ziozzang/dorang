package admin

import (
	"errors"
	"net/http"
	"sort"
	"strings"
)

// Scope is how much of the deployment one administrator may see and change.
//
// # Why this type exists
//
// The security review's finding was that authorization on this surface was a
// single global bit — the master credential, or any key whose owning user held
// an administrative role — and that every listing and info endpoint then took
// the subject id it filtered on FROM THE REQUEST. The two combine into the
// worst version of either: an `admin_viewer` on team A asks for team B's spend
// by naming team B, and gets it. The comment that framed the absent permission
// model as a considered choice was defensible only while every administrator
// was a full administrator, which is not what `admin_viewer` implies and is not
// what a shared gateway is.
//
// # The model
//
// There are exactly two kinds of administrator and the difference is a fact the
// schema already holds, so no new column and no new role vocabulary was
// invented for this:
//
//   - GLOBAL. The out-of-band master credential, and an administrative key that
//     belongs to no team (`api_keys.team_id` is empty). This is the operator.
//     It is what every deployment has today and its behaviour is unchanged.
//   - TEAM-SCOPED. An administrative key that DOES belong to a team. It
//     administers that team's keys, that team's members, that team's budgets
//     and that team's spend, and nothing else.
//
// Inventing an `admin_team` role would have meant a schema change and a
// migration to express something `api_keys.team_id` already says. Deriving the
// scope from the team the key is on has the property that matters: it cannot be
// forgotten. A key that is on a team is scoped by being on a team.
//
// # Fail-closed
//
// The zero Scope admits nothing. That is deliberate: an adapter that forgets to
// answer the question, or a Principal implementation that returns a zero value
// from a code path nobody thought about, produces a surface that refuses
// everything rather than one that permits everything. The cost of being wrong
// in this direction is an operator filing a bug; in the other direction it is
// cross-tenant administration, which is the finding.
type Scope struct {
	// Global admits every subject on every endpoint.
	Global bool
	// Teams are the team ids a non-global administrator is confined to. Empty
	// with Global false admits nothing.
	Teams []string
}

// GlobalScope is the operator's scope.
func GlobalScope() Scope { return Scope{Global: true} }

// TeamScope confines an administrator to the named teams. An empty id list
// produces a scope that admits nothing, which is the correct reading of "an
// administrative key that belongs to no team it may administer".
func TeamScope(teams ...string) Scope {
	out := make([]string, 0, len(teams))
	for _, t := range teams {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return Scope{Teams: out}
}

// AllowsTeam reports whether this scope covers a team id.
//
// An empty id is NOT covered by a team scope. That is the load-bearing case:
// keys, users and budgets with no team are the deployment-wide ones, and a
// team administrator must not reach them by omitting the field — the same
// mistake `batch.ownedBy` makes when it reads an empty owner as "everyone's".
func (s Scope) AllowsTeam(id string) bool {
	if s.Global {
		return true
	}
	if id == "" {
		return false
	}
	for _, t := range s.Teams {
		if t == id {
			return true
		}
	}
	return false
}

// Empty reports a scope that admits nothing.
func (s Scope) Empty() bool { return !s.Global && len(s.Teams) == 0 }

// String renders the scope for an audit row and an error message.
func (s Scope) String() string {
	if s.Global {
		return "global"
	}
	if len(s.Teams) == 0 {
		return "none"
	}
	return "teams:" + strings.Join(s.Teams, ",")
}

// ---------------------------------------------------------------------------
// Enforcement
// ---------------------------------------------------------------------------

// scope is the calling administrator's scope.
func (c *call) scope() Scope {
	if c.p == nil {
		return Scope{}
	}
	return c.p.AdminScope()
}

// requireGlobal refuses an endpoint that has no team-scoped meaning.
//
// Deployment configuration — deployments, aliases, credential health, capacity,
// the catalog, pricing and `/admin/config/reload` — belongs to the operator, not
// to a tenant. A team administrator asking for it is refused rather than served
// a filtered view, because there is no honest filtered view of "reload the
// process's configuration".
func (c *call) requireGlobal(what string) error {
	if c.scope().Global {
		return nil
	}
	f := newFault(http.StatusForbidden, CodeOutOfScope, typePermission,
		"%s is administered for the whole deployment, and this credential is scoped to %s",
		what, c.scope().String())
	return f
}

// Two refusals, and the difference between them is deliberate.
//
//   - A PARAMETER naming a team outside the scope — a listing filter, the
//     team a new key is being created on — is refused with 403
//     [CodeOutOfScope]. The answer is the same whether that team exists or
//     not, so it discloses nothing, and it tells the caller which parameter to
//     fix.
//   - An OBJECT the scope does not cover — a key, a user, a team fetched by id
//     — is refused with 404, identical to an id that does not exist. To a
//     scoped administrator the two cases are indistinguishable on purpose:
//     "you may not see it" and "it is not there" are the same fact from
//     outside, and answering 403 would turn every id into an existence oracle.
//
// permitTeamParam is the first of those.
func (c *call) permitTeamParam(id string) error {
	if c.scope().AllowsTeam(id) {
		return nil
	}
	return outOfScope("team", id, c.scope())
}

// loadKey fetches a key by id and hides one the caller may not administer.
//
// Every single-object key handler goes through this rather than calling GetKey
// directly, so "fetch it" and "check it" cannot be separated by a later edit —
// the previous shape, where the fetch was in the handler and the check was
// nowhere, is what the finding was.
func (c *call) loadKey(ks KeyStore, id string) (*Key, error) {
	k, err := ks.GetKey(c.ctx(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, notFound("key", id)
		}
		return nil, err
	}
	if !c.scope().AllowsTeam(k.TeamID) {
		return nil, notFound("key", id)
	}
	return k, nil
}

// loadUser fetches a user by id and hides one the caller may not administer.
//
// A user has no team column: membership is `team_members`. A team
// administrator may therefore act on a user exactly when that user is a member
// of one of its teams, which costs one membership listing per team in scope —
// a handful of rows, on the administrative path — and the alternative is
// trusting the caller's own claim about who it is entitled to see.
func (c *call) loadUser(d Directory, id string) (*User, error) {
	u, err := d.GetUser(c.ctx(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, notFound("user", id)
		}
		return nil, err
	}
	ok, err := c.userInScope(d, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("user", id)
	}
	return u, nil
}

// userInScope reports whether the caller may administer a user.
func (c *call) userInScope(d Directory, id string) (bool, error) {
	s := c.scope()
	if s.Global {
		return true, nil
	}
	if id == "" || s.Empty() {
		return false, nil
	}
	for _, team := range s.Teams {
		members, err := d.ListTeamMembers(c.ctx(), team)
		if err != nil {
			return false, err
		}
		for _, m := range members {
			if m.UserID == id {
				return true, nil
			}
		}
	}
	return false, nil
}

// loadTeamScoped fetches a team by id and hides one the caller may not
// administer.
func (c *call) loadTeamScoped(d Directory, id string) (*Team, []TeamMember, error) {
	if !c.scope().AllowsTeam(id) {
		return nil, nil, notFound("team", id)
	}
	return c.loadTeam(d, id)
}

// scopedTeamFilter turns a request-supplied team filter into an enforced one.
//
// A global administrator gets what it asked for. A team administrator asking
// for a team it holds gets that team; asking for a team it does not hold is
// refused, not silently rewritten — a listing that quietly returns someone
// else's team's shape with none of its rows is indistinguishable from an empty
// team, and an operator debugging that has been lied to. Asking for nothing
// gets its own scope, which is the case that closes the finding: the previous
// code read this field and passed it to the store unchecked.
func (c *call) scopedTeamFilter(requested string) (string, error) {
	s := c.scope()
	if s.Global {
		return requested, nil
	}
	if s.Empty() {
		return "", outOfScope("team", requested, s)
	}
	if requested != "" {
		if !s.AllowsTeam(requested) {
			return "", outOfScope("team", requested, s)
		}
		return requested, nil
	}
	if len(s.Teams) == 1 {
		return s.Teams[0], nil
	}
	// More than one team in scope and no filter asked for: the store cannot
	// express "any of these" through a single-valued filter, so nothing is
	// pushed down and the result is filtered here instead. Correct either way;
	// the push-down is an optimization and the post-filter is the enforcement.
	return "", nil
}

// keepKeysInScope drops rows the caller may not see.
//
// It runs on every listing even when a filter was pushed into the store,
// because the enforcement must not depend on a store honouring a filter. A
// store that ignores KeyFilter.TeamID is a bug; a store that ignores it and
// thereby leaks another tenant's keys is the finding again.
func (c *call) keepKeysInScope(in []*Key) []*Key {
	s := c.scope()
	if s.Global {
		return in
	}
	out := in[:0:0]
	for _, k := range in {
		if k != nil && s.AllowsTeam(k.TeamID) {
			out = append(out, k)
		}
	}
	return out
}

// outOfScope is the one refusal every scope check returns.
//
// The subject id is echoed because the caller supplied it and already knows it;
// nothing about a subject the caller could not otherwise see is disclosed by
// saying "not yours".
func outOfScope(kind, id string, s Scope) error {
	if id == "" {
		return newFault(http.StatusForbidden, CodeOutOfScope, typePermission,
			"this credential administers %s and cannot act on a %s outside it",
			s.String(), kind)
	}
	return newFault(http.StatusForbidden, CodeOutOfScope, typePermission,
		"%s %q is outside this credential's administrative scope (%s)", kind, id, s.String())
}

// permitBudgetSubject refuses a budget whose subject is outside the caller's
// scope.
//
// The subject vocabulary of §6.4 splits cleanly. `global` and `credential` are
// operator-wide: a credential is a provider account the operator pays for, and
// a global ceiling bounds the whole deployment. `team`, `user` and `key` are
// tenant subjects, and each resolves to a team the same way its object does.
//
// A `key` subject costs one key lookup, because the budget row carries the key
// id and not the key's team. When there is no key store configured, a scoped
// administrator is refused rather than admitted: the question "whose key is
// this" has no answer in that process, and an unanswerable authorization
// question is a refusal.
func (c *call) permitBudgetSubject(sub BudgetSubject) error {
	s := c.scope()
	if s.Global {
		return nil
	}
	switch sub.Kind {
	case "global", "credential":
		return c.requireGlobal("a " + sub.Kind + " budget")
	case "team":
		return c.permitTeamParam(sub.ID)
	case "user":
		d, err := c.a.directory()
		if err != nil {
			return err
		}
		ok, err := c.userInScope(d, sub.ID)
		if err != nil {
			return err
		}
		if !ok {
			return outOfScope("user", sub.ID, s)
		}
		return nil
	case "key":
		ks, err := c.a.keys()
		if err != nil {
			return err
		}
		k, err := ks.GetKey(c.ctx(), sub.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Not found and not permitted are the same answer for a scoped
				// caller: see the note above permitTeamParam.
				return outOfScope("key", sub.ID, s)
			}
			return err
		}
		if !s.AllowsTeam(k.TeamID) {
			return outOfScope("key", sub.ID, s)
		}
		return nil
	default:
		// A kind the vocabulary does not contain never reaches here — the spec
		// resolver refuses it — but an unknown subject is refused rather than
		// permitted, because that is the direction a new kind should default
		// to when this switch is not updated with it.
		return outOfScope(sub.Kind, sub.ID, s)
	}
}

// keepBudgetsInScope drops budget rows the caller may not see.
func (c *call) keepBudgetsInScope(in []Budget) ([]Budget, error) {
	if c.scope().Global {
		return in, nil
	}
	out := make([]Budget, 0, len(in))
	for _, b := range in {
		if err := c.permitBudgetSubject(b.Subject); err != nil {
			var f *fault
			if errors.As(err, &f) && (f.Status == http.StatusForbidden || f.Status == http.StatusNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// activityScope is the set of dimension values a scoped administrator may see
// in a daily-activity report.
//
// It returns nil for a global administrator, which means "no restriction" —
// distinct from an empty non-nil set, which means "nothing", and the two must
// not be confused because one of them is the whole deployment.
//
// The tag dimension has no answer. A tag is a caller-chosen string with no
// owner: two teams can use the same tag and there is no join that says which
// rows behind it belong to whom. Rather than guess, tag activity is refused for
// a scoped administrator — the honest answer for a dimension that cannot be
// scoped is that it cannot be scoped.
func (c *call) activityScope(dim GroupBy) (map[string]bool, error) {
	s := c.scope()
	if s.Global {
		return nil, nil
	}
	switch dim {
	case GroupByTeam:
		out := make(map[string]bool, len(s.Teams))
		for _, t := range s.Teams {
			out[t] = true
		}
		return out, nil
	case GroupByUser:
		d, err := c.a.directory()
		if err != nil {
			return nil, err
		}
		out := map[string]bool{}
		for _, t := range s.Teams {
			members, err := d.ListTeamMembers(c.ctx(), t)
			if err != nil {
				return nil, err
			}
			for _, m := range members {
				out[m.UserID] = true
			}
		}
		return out, nil
	default:
		return nil, c.requireGlobal("activity by " + string(dim))
	}
}
