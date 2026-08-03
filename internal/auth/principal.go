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
	// Pended refuses every request from this subject with a distinct,
	// reversible refusal (DESIGN §11.6).
	//
	// It is separate from Blocked because the two are different judgements and
	// they fail differently. Blocked is an operator's decision; Pended is the
	// token guard's *statistical* judgement, which might be wrong, and an
	// operator releases it in one action without reissuing a credential. A
	// caller who cannot tell the two apart cannot tell an outage from a policy.
	Pended bool
	// PendReason is the short, non-secret explanation an operator sees, and the
	// caller does not.
	PendReason string
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
	// MaxParallel is the concurrency ceiling.
	//
	// It is enforced by internal/capacity, which learns it from
	// capacity.Request.PrincipalMax — the value is carried out of here by the
	// dispatcher rather than read from here by the broker, because the broker's
	// own table is static configuration and this one arrives with the
	// credential. Until that wiring existed this field reached no enforcement
	// at all and the comment here said otherwise.
	MaxParallel *int64
}

// MostRestrictiveParallel returns the smallest concurrency ceiling declared
// across a set of subjects, or 0 when none declares one.
//
// Zero means "no ceiling" on both sides of this, matching internal/capacity's
// convention, so a subject that declares nothing cannot lower the result to
// zero and refuse everything.
func MostRestrictiveParallel(ls ...*Limits) int {
	out := int64(0)
	for _, l := range ls {
		if l == nil || l.MaxParallel == nil || *l.MaxParallel <= 0 {
			continue
		}
		if out == 0 || *l.MaxParallel < out {
			out = *l.MaxParallel
		}
	}
	return int(out)
}

// RateSource supplies the per-minute counters the rate ceilings are compared
// against, per subject.
//
// It is an interface on [Access] rather than two integers because the two
// integers were the defect: one observed value cannot answer for three
// subjects, so a team's requests-per-minute ceiling was being compared against
// one key's count. A team limit means "across the team", and only the counter's
// owner knows what a team's count is.
type RateSource interface {
	// ObservedRates returns the requests and tokens already counted for this
	// subject in the current minute. kind is "key", "user" or "team".
	ObservedRates(kind, id string) (rpm, tpm int64)
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
	// KeyID is the api_keys row id. It is the DURABLE identity: rotation mints a
	// new secret and leaves this, and everything hanging off it, alone
	// (DESIGN §11.2c).
	KeyID string
	// SecretID names WHICH of the key's secrets authenticated this request.
	//
	// A key has one id and one or more secrets, and during a rotation's grace
	// period both authenticate to the same principal. Carrying the secret id out
	// of authentication is what lets the ledger record which one was used, so an
	// operator can see whether the client actually rolled before the window
	// closes instead of finding out when it shuts.
	//
	// Each secret is its own cache entry — entries are keyed by the lookup, and
	// a lookup is derived from the secret — so this field is per-secret even
	// though every other field on the Principal is per-key.
	SecretID string
	// SecretGeneration counts rotations: 1 is the secret the key was issued
	// with. It is here so an operator reading a ledger sees "generation 1 is
	// still in use" without joining anything.
	SecretGeneration int
	// SecretExpiresAt is when THIS secret stops authenticating: the end of a
	// rotation's grace period, or the instant an early cut set. Zero means the
	// secret is current and expires only with the key.
	SecretExpiresAt time.Time
	// Tier is the key's tier (DESIGN §11.6). It comes from the stored row and
	// from nowhere else: an operator can grant a tier, a caller cannot claim
	// one (§10.5).
	Tier string
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
	// ObservedRPM is the requests already counted in the current minute, for
	// every subject. Zero skips the check.
	//
	// It is the single-subject fallback and is used only when Rates is nil. A
	// caller with more than one subject should supply Rates instead: this pair
	// compares one number against three different subjects' ceilings, which is
	// right only when they are all the same subject.
	ObservedRPM int
	// ObservedTPM is the tokens already counted in the current minute. Zero
	// skips the check. Same caveat as ObservedRPM.
	ObservedTPM int
	// Rates supplies per-subject counters. Nil falls back to ObservedRPM and
	// ObservedTPM.
	Rates RateSource
}

// observed returns the counters to compare one subject's ceilings against.
func (a Access) observed(kind, id string) (rpm, tpm int64) {
	if a.Rates != nil && id != "" {
		return a.Rates.ObservedRates(kind, id)
	}
	return int64(a.ObservedRPM), int64(a.ObservedTPM)
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
	if p.SecretRetired(now) {
		return refuse(ReasonSecretRetired, "secret", "")
	}
	if err := p.Key.authorize("key", p.KeyID, a, now); err != nil {
		return err
	}
	if p.User != nil {
		if err := p.User.authorize("user", p.UserID, a, now); err != nil {
			return err
		}
	}
	if p.Team != nil {
		if err := p.Team.authorize("team", p.TeamID, a, now); err != nil {
			return err
		}
	}
	return nil
}

// SecretRetired reports whether the secret that authenticated has passed its
// own expiry — the end of a rotation grace period, or an early cut.
//
// The boundary is inclusive, as everywhere else: a secret expiring at T is
// refused at T. It is a property of the secret, not of the key: the key, its
// budget, its spend and its ledger history are untouched, which is the entire
// point of rotation (DESIGN §11.2c).
func (p *Principal) SecretRetired(now time.Time) bool {
	return p != nil && !p.SecretExpiresAt.IsZero() && !now.Before(p.SecretExpiresAt)
}

// Refusing reports whether this principal would be refused whatever the request
// asked for — blocked, pended, expired, out of budget, or presenting a retired
// secret.
//
// The authenticator uses it to decide a cache entry's lifetime. A principal
// that is refusing is a NEGATIVE answer even though a row was found, and
// §11.2c's rule 3 is that a negative answer is cheap to re-check and must not
// be held for as long as a serving one. Treating a found-but-refused row like a
// serving row is what made the revocation window a full entry TTL wide.
func (p *Principal) Refusing(now time.Time) bool {
	if p == nil {
		return true
	}
	if p.Master {
		return false
	}
	if p.SecretRetired(now) {
		return true
	}
	for _, l := range []*Limits{&p.Key, p.User, p.Team} {
		if l == nil {
			continue
		}
		if l.Blocked || l.Pended || l.Expired(now) || l.BudgetExceeded(now) {
			return true
		}
	}
	return false
}

// authorize applies one subject's limits. id names the subject the rate
// counters are read for.
func (l *Limits) authorize(subject, id string, a Access, now time.Time) error {
	if l.Blocked {
		return refuse(ReasonBlocked, subject, "")
	}
	// A pend is checked after the block and before everything else. It is a
	// refusal in its own right rather than a second spelling of blocked, so the
	// caller's error names a condition an operator can release in one action.
	if l.Pended {
		return refuse(ReasonPended, subject, "")
	}
	if l.Expired(now) {
		return refuse(ReasonExpired, subject, "")
	}
	if a.Model != "" && !ModelAllowed(l.Models, a.Model) {
		return refuse(ReasonModelNotAllowed, subject, a.Model)
	}
	if a.Route != "" && !allowedIn(l.AllowedRoutes, a.Route, true) {
		return refuse(ReasonRouteNotAllowed, subject, a.Route)
	}
	if l.BudgetExceeded(now) {
		return refuse(ReasonBudgetExceeded, subject, "")
	}
	if l.RPMLimit != nil || l.TPMLimit != nil {
		rpm, tpm := a.observed(subject, id)
		if l.RPMLimit != nil && rpm >= *l.RPMLimit {
			return refuse(ReasonRateLimited, subject, "rpm")
		}
		if l.TPMLimit != nil && tpm >= *l.TPMLimit {
			return refuse(ReasonRateLimited, subject, "tpm")
		}
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

// ModelAllowed reports whether one subject's allow-list admits a model name.
//
// It is exported because it is the SAME rule the gate applies — [Limits.authorize]
// calls it below — and internal/app needs it to filter GET /v1/models. Those two
// answers must agree: a model the listing shows and the gate refuses is a 403 on
// a name the client was just told it could use, and a model the listing hides
// and the gate serves is a capability the operator cannot see.
//
// It existed twice until now, once here and once as internal/app's own
// `modelAllowed`. The two agreed, which is the only reason the duplication was
// survivable and is not a reason to keep it: the copies in internal/redact
// agreed too, until one of them leaked a credential and the other corrupted
// ordinary text. Names are compared WHOLE — a model name is opaque and nothing
// splits it (DESIGN §2.1) — so no "/*" prefix rule applies here even though the
// route allow-list beside it has one.
func ModelAllowed(list []string, model string) bool {
	return allowedIn(list, model, false)
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
