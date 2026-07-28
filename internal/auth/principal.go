package auth

import (
	"strings"
	"time"
)

// Limits is the authorization envelope of one subject: an API key, the user
// that owns it, or the team that owns the user.
//
// R1-A recorded these as the columns that must be carried or the gateway fails
// open. They are carried here, and [Principal.Authorize] enforces every one.
// A zero value imposes no restriction, so a subject that was imported without
// a field is unrestricted on that field and restricted by whatever the other
// subjects say — never accidentally unrestricted overall.
type Limits struct {
	// Blocked refuses every request from this subject.
	Blocked bool
	// ExpiresAt refuses every request at or after this instant. Zero means the
	// subject never expires. An expired subject is refused, not resurrected.
	ExpiresAt time.Time
	// Models is an allow-list of client-facing model names. Empty allows all;
	// the entry "*" also allows all.
	Models []string
	// AllowedRoutes is an allow-list of request paths. Empty allows all. An
	// entry may be an exact path, a prefix ending in "/*", or "*".
	AllowedRoutes []string
	// MaxBudgetNanoUSD is the spend ceiling in nano-USD. The unit matches
	// internal/quota, which owns budget accounting; auth only refuses a
	// subject whose recorded spend has already reached its ceiling.
	MaxBudgetNanoUSD *int64
	// SpentNanoUSD is the spend recorded for the current budget period.
	SpentNanoUSD int64
	// BudgetPeriod is the window the ceiling applies over, in the configuration
	// spelling ("monthly", "daily", "30d", …). Empty means the deployment's
	// default.
	//
	// It travels with the ceiling because the two are one fact. A limit carried
	// without its period is a limit whose reset date the gate has to guess, and
	// a monthly budget guessed as daily is thirty times too permissive on the
	// second day of the month.
	BudgetPeriod string
	// BudgetResetAt is when the current budget period ends. Spend recorded for
	// a period that has already ended does not refuse anything.
	BudgetResetAt time.Time
	// RPMLimit is the requests-per-minute ceiling.
	RPMLimit *int64
	// TPMLimit is the tokens-per-minute ceiling.
	TPMLimit *int64
	// MaxParallel is the concurrency ceiling. It is enforced by
	// internal/capacity and carried here so the value survives import.
	MaxParallel *int64
}

// Limit builds a limit value. The numeric limits are pointers because nil
// ("no limit configured") and 0 ("a limit of zero: allow nothing") are
// different answers, and flattening them into a bare 0 is how a configured
// limit quietly stops existing — the fail-open shape R1-A warns about. The
// storage layer carries the same distinction, so nothing is lost in
// translation either way.
func Limit(v int64) *int64 { return &v }

// Principal is an authenticated caller. It is immutable once loaded and is
// shared by every request that presents the same credential, so callers must
// not mutate it or the slices it holds.
//
// A Principal holds no credential material: not the token, not the digest.
type Principal struct {
	// KeyID is the api_keys row id.
	KeyID string
	// Label is a non-reversible display label. R1-A: never copy an incumbent's
	// column that stores trailing characters of the secret.
	Label string
	// UserID and TeamID are the owning principals, "" when unowned.
	UserID string
	TeamID string
	// PriorityClass feeds the scheduler.
	PriorityClass string
	// Tags feed routing and reporting.
	Tags []string
	// Key, User and Team are the three limit envelopes. User and Team are nil
	// when the key is unowned.
	Key  Limits
	User *Limits
	Team *Limits
	// RequireOwner refuses a key that has no owning user or team. It is off by
	// default because imported keys legitimately have neither.
	RequireOwner bool
	// Master marks the out-of-band administrative credential. A master
	// principal is never a stored row (R1-A).
	Master bool
}

// IsMaster reports whether this is the out-of-band administrative credential.
func (p *Principal) IsMaster() bool { return p != nil && p.Master }

// Access describes the request being authorized.
type Access struct {
	// Now is the decision instant. Zero means time.Now().
	Now time.Time
	// Model is the client-facing model name. "" skips the model check.
	Model string
	// Route is the request path. "" skips the route check.
	Route string
	// ObservedRPM is the requests already counted for this key in the current
	// minute. Zero skips the check; internal/quota supplies the number.
	ObservedRPM int
	// ObservedTPM is the tokens already counted for this key in the current
	// minute. Zero skips the check.
	ObservedTPM int
}

// Authorize enforces every authorization field across the key, its user and
// its team. The most restrictive wins (DESIGN §11.2): any subject may refuse.
//
// The master credential is authorized unconditionally — it is out-of-band and
// has no stored row to carry limits.
func (p *Principal) Authorize(a Access) error {
	if p == nil {
		return refuse(ReasonUnknownKey, "", "")
	}
	if p.Master {
		return nil
	}
	now := a.Now
	if now.IsZero() {
		now = time.Now()
	}
	if p.RequireOwner && p.UserID == "" && p.TeamID == "" {
		return refuse(ReasonNoPrincipal, "key", p.KeyID)
	}
	if err := p.Key.authorize("key", a, now); err != nil {
		return err
	}
	if p.User != nil {
		if err := p.User.authorize("user", a, now); err != nil {
			return err
		}
	}
	if p.Team != nil {
		if err := p.Team.authorize("team", a, now); err != nil {
			return err
		}
	}
	return nil
}

// authorize applies one subject's limits.
func (l *Limits) authorize(subject string, a Access, now time.Time) error {
	if l.Blocked {
		return refuse(ReasonBlocked, subject, "")
	}
	if l.Expired(now) {
		return refuse(ReasonExpired, subject, "")
	}
	if a.Model != "" && !allowedIn(l.Models, a.Model, false) {
		return refuse(ReasonModelNotAllowed, subject, a.Model)
	}
	if a.Route != "" && !allowedIn(l.AllowedRoutes, a.Route, true) {
		return refuse(ReasonRouteNotAllowed, subject, a.Route)
	}
	if l.BudgetExceeded(now) {
		return refuse(ReasonBudgetExceeded, subject, "")
	}
	if l.RPMLimit != nil && int64(a.ObservedRPM) >= *l.RPMLimit {
		return refuse(ReasonRateLimited, subject, "rpm")
	}
	if l.TPMLimit != nil && int64(a.ObservedTPM) >= *l.TPMLimit {
		return refuse(ReasonRateLimited, subject, "tpm")
	}
	return nil
}

// Expired reports whether the subject's expiry has passed. The boundary is
// inclusive: a key expiring at T is refused at T.
func (l *Limits) Expired(now time.Time) bool {
	return !l.ExpiresAt.IsZero() && !now.Before(l.ExpiresAt)
}

// BudgetExceeded reports whether recorded spend has reached the ceiling for the
// current period. Spend belonging to a period that has already ended is
// ignored: the row is stale, not over budget.
//
// A configured ceiling of zero refuses everything, which is the point of
// carrying the ceiling as a pointer.
func (l *Limits) BudgetExceeded(now time.Time) bool {
	if l.MaxBudgetNanoUSD == nil {
		return false
	}
	if !l.BudgetResetAt.IsZero() && !now.Before(l.BudgetResetAt) {
		return false
	}
	return l.SpentNanoUSD >= *l.MaxBudgetNanoUSD
}

// allowedIn reports whether v is admitted by an allow-list. An empty list
// allows everything — that is what an unrestricted key looks like in the
// store. "*" allows everything explicitly. When prefix is set, an entry ending
// in "/*" matches any path under it.
func allowedIn(list []string, v string, prefix bool) bool {
	if len(list) == 0 {
		return true
	}
	for _, e := range list {
		if e == "*" || e == v {
			return true
		}
		if prefix && strings.HasSuffix(e, "/*") &&
			strings.HasPrefix(v, e[:len(e)-1]) {
			return true
		}
	}
	return false
}
