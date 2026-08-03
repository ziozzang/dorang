package admin

import (
	"context"
	"net/http"
	"time"
)

// Everything package admin consumes is declared here, narrow enough that each
// one is implementable by a short fake — and the tests do exactly that.
//
// The shapes are chosen to map onto internal/store, internal/auth,
// internal/quota, internal/pricing, internal/capacity, internal/health and
// pkg/catalog without translation loss, not to be minimal for their own sake. A
// field that exists in the schema and would be lost crossing this boundary is a
// field an operator cannot see, so the value types are close to the rows.

// ---------------------------------------------------------------------------
// Authorization
// ---------------------------------------------------------------------------

// Principal is an authenticated caller as the administration surface needs it:
// an identity for the audit row and one authorization bit.
//
// It carries no credential material and this package never asks it for any.
type Principal interface {
	// ActorKind is "master" for the out-of-band administrative credential and
	// "key" for a stored credential. It lands in audit_logs.actor_kind.
	ActorKind() string
	// ActorID identifies the credential row, and is empty for the master
	// credential, which by construction has no row (DESIGN §2.4).
	ActorID() string
	// IsAdmin reports whether this caller may use the administration surface:
	// the master credential, or a key whose role is administrative. A key's
	// role is not on the key — it is on the owning user (users.role) — so the
	// join is the adapter's business, not this package's.
	IsAdmin() bool
	// AdminScope bounds what this administrator may see and change. It is a
	// required method rather than an optional interface: an implementation
	// that does not answer would have to be given a default, and the only
	// available defaults are "everything" — which is the finding — or
	// "nothing", which is a surface that silently stops working. Making it
	// part of the interface means a new Principal cannot compile without an
	// answer. See [Scope].
	AdminScope() Scope
}

// Authenticator resolves a request's credentials to a [Principal].
//
// It is handed the whole header map rather than a token because which of the
// accepted header names supplied the credential is the authenticator's
// business. A returned error becomes a 401; a principal that is not an admin
// becomes a 403.
type Authenticator interface {
	AuthenticateHeader(ctx context.Context, h http.Header) (Principal, error)
}

// ---------------------------------------------------------------------------
// Keys
// ---------------------------------------------------------------------------

// Key is one api_keys row as the administration surface sees it.
//
// Note what is absent, and note that it is absent by construction rather than
// by convention: there is no token, no digest, and no lookup. DESIGN §2.4 warns
// that a foreign system's display column stored the trailing characters of the
// secret; a type that cannot hold the secret cannot reproduce that pattern, and
// no later edit to a handler can reintroduce it without also editing this
// struct, which is a change a reviewer will see.
//
// Nullable limits are pointers because nil ("no limit configured") and 0 ("a
// limit of zero, allow nothing") are different answers, and flattening them is
// how a configured limit quietly stops existing.
type Key struct {
	ID string
	// KeyLabel is the non-reversible display label.
	KeyLabel string
	KeyAlias string

	UserID string
	TeamID string

	Models             []string
	AllowedRoutes      []string
	ObjectPermissionID string

	MaxBudgetNano  *int64
	SoftBudgetNano *int64
	BudgetPeriod   string
	BudgetResetAt  time.Time
	SpendNano      int64

	RPMLimit      *int64
	TPMLimit      *int64
	MaxParallel   *int64
	PriorityClass string

	Tags      []string
	Blocked   bool
	ExpiresAt time.Time

	// Tier is the key's tier (DESIGN §11.6). It is set here and nowhere else:
	// every route in this package requires an operator credential, which is the
	// whole content of "an operator can grant a tier, a caller cannot claim
	// one" (§10.5).
	Tier string
	// PendedAt is when the token guard pended the key, zero when it is not
	// pended (§11.6). It is separate from Blocked because a pend is reversible
	// in one action and a block is a decision, and a caller who cannot tell
	// them apart cannot tell an outage from a policy.
	PendedAt time.Time
	// PendReason records which condition tripped.
	PendReason string

	// HashScheme is "dorang_v1" or "legacy_sha256". It is metadata about how
	// the credential verifies, not a verifier, and an operator needs it to know
	// which keys still depend on the legacy migration window of §2.4.
	HashScheme string
	// Source distinguishes natively issued keys from imported ones.
	Source string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Expired reports whether the key's expiry has passed. A key with no expiry
// never expires.
func (k *Key) Expired(now time.Time) bool {
	return !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt)
}

// Verifier is the stored, non-reversible half of a credential: the
// scheme-independent index key and the digest that verifies a presented token.
//
// It is a separate argument from [Key] so that the two travel separately. A
// handler that returns a Key cannot accidentally return a Verifier, because it
// never had one.
type Verifier struct {
	// Lookup is sha256(token)[:16] in hex — the scheme-independent index key
	// that keeps two hash schemes one lookup (§2.4).
	Lookup string
	// TokenHash is the digest under HashScheme.
	TokenHash string
	// HashScheme is the scheme TokenHash was produced under.
	HashScheme string
}

// KeyFilter narrows /key/list.
type KeyFilter struct {
	UserID string
	TeamID string
	// Blocked, when set, restricts to blocked or unblocked keys.
	Blocked *bool
	Limit   int
	Offset  int
}

// KeySpendRef names one key whose spend is wanted, and the budget period the
// answer must be measured over.
//
// The period travels with the id because the answer depends on it: a key's
// spend is what it has spent in the window its ceiling is enforced over, and
// asking for "the spend" without saying over what is how a daily ceiling gets
// compared against a month of consumption.
type KeySpendRef struct {
	ID string
	// Period is the key's budget_duration ("daily", "monthly", …). Empty means
	// the deployment's default window, which is what the gate itself falls back
	// to for a key whose period does not parse.
	Period string
}

// SpendReporter answers what a key has actually spent.
//
// It exists because `api_keys.spend_nano` — the column [Key.SpendNano] carries
// and /key/info reported — is written by nothing on the request path. Budget
// enforcement lives in the durable counter the gate reserves against, which is
// why enforcement worked while the column stayed at zero, and **/key/info
// answered `"spend": 0` for every key on every deployment** while the same
// key's ledger rows, its own response headers and /global/spend/report all
// agreed on a different number.
//
// A silent zero is the specific failure this surface exists to avoid, and this
// is the route a per-key spend dashboard reaches for first. The figure is
// therefore taken from the counter that has it, and the stored column is left
// to the audit trail, where an operator's mutation is being diffed and a
// number that moves on its own is noise.
//
// Optional: a deployment with no counter behind it keeps the stored column. It
// is a batch call because /key/list renders a page of them and a query per row
// is how an operator page becomes a store outage.
type SpendReporter interface {
	// KeySpend returns spend in nano-currency, keyed by key id. A key with no
	// counter row is absent from the map rather than present at zero — the
	// caller then has an unhydrated column and knows it.
	KeySpend(ctx context.Context, keys []KeySpendRef) (map[string]int64, error)
}

// KeyStore is the credential half of the store.
type KeyStore interface {
	// CreateKey inserts a key together with its verifier. It returns
	// [ErrConflict] if the id or lookup already exists.
	CreateKey(ctx context.Context, k *Key, v Verifier) error
	// GetKey returns one key, or [ErrNotFound].
	GetKey(ctx context.Context, id string) (*Key, error)
	// UpdateKey writes every mutable field of k.
	UpdateKey(ctx context.Context, k *Key) error
	// DeleteKeys removes keys by id and reports how many rows went away.
	DeleteKeys(ctx context.Context, ids []string) (int, error)
	// ListKeys returns keys matching f, newest first.
	ListKeys(ctx context.Context, f KeyFilter) ([]*Key, error)
	// ReplaceVerifier rotates the secret behind an existing key id, keeping
	// every authorization field. The label changes with the secret because it
	// is derived from it.
	//
	// It replaces the secret OUTRIGHT, with no grace period, which is what
	// `/key/regenerate` has always meant. `/key/rotate` uses [RotatingKeyStore]
	// instead.
	ReplaceVerifier(ctx context.Context, id string, v Verifier, label string, now time.Time) error
}

// RotatingKeyStore is the optional half of [KeyStore] that implements DESIGN
// §11.2c: a key has an id and one or more secrets, and rotation mints a new
// secret while leaving the old one valid for a grace period.
//
// It is optional so that a deployment whose key store predates rotation answers
// `/key/rotate` with a named 501 rather than with a regeneration that silently
// cut the caller off. Doing less than the route promises, under the route's
// name, is the failure mode §17.1 calls this codebase's dominant one.
type RotatingKeyStore interface {
	// Rotate mints a new secret for an existing key and leaves the current one
	// valid for grace. Everything else about the key — tier, budget, spend,
	// allow-list, rate limits, team, ledger history — belongs to the id and is
	// untouched.
	Rotate(ctx context.Context, id string, v Verifier, label string, grace time.Duration, now time.Time) (RotationResult, error)
	// EndGrace cuts every superseded secret immediately, keeping the current
	// one. This is what a suspected compromise needs.
	EndGrace(ctx context.Context, id string, now time.Time) (int, error)
	// ListSecrets returns the key's secrets, newest generation first. Retired
	// ones are included: an operator asking "did the client roll?" needs to see
	// the one that is about to stop working.
	ListSecrets(ctx context.Context, id string) ([]KeySecret, error)
}

// PendableKeyStore is the optional half of [KeyStore] that implements §11.6's
// pend: the reversible refusal the token guard uses, released by an operator in
// ONE action.
type PendableKeyStore interface {
	// Pend refuses the key with a distinct, documented error until it is
	// released.
	Pend(ctx context.Context, id, reason string, now time.Time) error
	// Release clears a pend. One argument, because it is one action.
	Release(ctx context.Context, id string, now time.Time) error
}

// KeySecret is one of a key's secrets, as an operator sees it. It holds nothing
// that could be turned back into the secret: not the digest, not the lookup.
type KeySecret struct {
	ID string
	// Generation counts rotations; 1 is the secret the key was issued with.
	Generation int64
	// KeyLabel is the non-reversible display label for this secret.
	KeyLabel string
	// Current marks the secret a rotation would replace.
	Current   bool
	CreatedAt time.Time
	// ExpiresAt is the end of this secret's grace period, zero for the current
	// one.
	ExpiresAt time.Time
	// RevokedAt is an early cut, zero when there was none. It is separate from
	// ExpiresAt so that "the grace ran out" and "an operator ended it" are
	// distinguishable a month later.
	RevokedAt time.Time
	// LastUsedAt is when a request last authenticated with this secret, zero if
	// never. It is the field an operator actually reads before letting a grace
	// period close.
	LastUsedAt time.Time
}

// RotationResult is what a rotation did.
type RotationResult struct {
	// Previous is the secret that was current a moment ago.
	Previous KeySecret
	// New is the secret just minted. Its plaintext is not here — it is returned
	// exactly once, by the handler that minted it.
	New KeySecret
	// PreviousExpiresAt is when the old secret stops authenticating. This is
	// the field the response is about: a caller that has to infer it from a
	// policy plus a clock will infer it wrong.
	PreviousExpiresAt time.Time
	// Retired names the secrets this rotation cut outright to stay within
	// max_secrets.
	Retired []KeySecret
}

// Hasher turns a plaintext token into the three non-reversible values the
// store keeps. It is internal/auth's and internal/store's hashing narrowed to
// what issuing a key needs.
//
// The pepper lives behind this interface and never crosses it, so nothing in
// package admin can be asked to produce a verifier without the configured
// pepper being present.
type Hasher interface {
	// Lookup returns the scheme-independent index key for a token.
	Lookup(token string) string
	// Hash returns the stored digest for a token and the scheme it used. Keys
	// dorang issues are always dorang_v1; legacy_sha256 exists for import, and
	// this interface deliberately offers no way to issue one.
	Hash(token string) (digest, scheme string, err error)
	// Label returns the non-reversible display label for a token. It must not
	// be derivable back into any part of the secret (§2.4).
	Label(token string) string
}

// ---------------------------------------------------------------------------
// Users and teams (DESIGN §11.4)
// ---------------------------------------------------------------------------

// User is one users row.
type User struct {
	ID    string
	Email string
	Name  string
	// Role is the administrative role. It is what makes a key admin-capable,
	// since the key row carries no role of its own.
	Role string

	MaxBudgetNano *int64
	BudgetPeriod  string
	BudgetResetAt time.Time
	SpendNano     int64

	RPMLimit *int64
	TPMLimit *int64

	Models   []string
	Blocked  bool
	Metadata string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Team is one teams row.
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

// TeamMember is one team_members row.
type TeamMember struct {
	TeamID        string
	UserID        string
	Role          string
	MaxBudgetNano *int64
	SpendNano     int64
	CreatedAt     time.Time
}

// ListOptions bounds a directory listing. Directory objects are not a ledger,
// so they take an offset rather than a keyset cursor: the tables are small and
// an operator paging through teams is not a scan whose cost grows with the page
// number.
type ListOptions struct {
	Limit  int
	Offset int
}

// Directory is the users-and-teams half of the store.
type Directory interface {
	CreateUser(ctx context.Context, u *User) error
	GetUser(ctx context.Context, id string) (*User, error)
	UpdateUser(ctx context.Context, u *User) error
	DeleteUsers(ctx context.Context, ids []string) (int, error)
	ListUsers(ctx context.Context, o ListOptions) ([]*User, error)

	CreateTeam(ctx context.Context, t *Team) error
	GetTeam(ctx context.Context, id string) (*Team, error)
	UpdateTeam(ctx context.Context, t *Team) error
	DeleteTeams(ctx context.Context, ids []string) (int, error)
	ListTeams(ctx context.Context, o ListOptions) ([]*Team, error)

	AddTeamMember(ctx context.Context, m TeamMember) error
	RemoveTeamMember(ctx context.Context, teamID, userID string) error
	ListTeamMembers(ctx context.Context, teamID string) ([]TeamMember, error)
}

// ---------------------------------------------------------------------------
// Models and deployments
// ---------------------------------------------------------------------------

// Deployment is one deployments row: a model group's backing target.
type Deployment struct {
	ID         string
	ModelGroup string
	ProviderID string
	// UpstreamModel is the real model id at the provider. Opaque: no component
	// splits it on any character (§2.1).
	UpstreamModel string
	CredentialIDs []string

	Weight   int
	Priority int

	RPMLimit        *int64
	TPMLimit        *int64
	MaxParallel     *int64
	TimeoutMS       *int64
	StreamTimeoutMS *int64

	// Params is the deployment's parameter overlay, verbatim JSON.
	Params  string
	Enabled bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Alias is one model_aliases row.
type Alias struct {
	Alias      string
	ModelGroup string
}

// ModelRegistry is the routing-configuration half of the store.
type ModelRegistry interface {
	CreateDeployment(ctx context.Context, d *Deployment) error
	GetDeployment(ctx context.Context, id string) (*Deployment, error)
	UpdateDeployment(ctx context.Context, d *Deployment) error
	DeleteDeployment(ctx context.Context, id string) error
	ListDeployments(ctx context.Context) ([]*Deployment, error)
	ListAliases(ctx context.Context) ([]Alias, error)
}

// ---------------------------------------------------------------------------
// Budgets
// ---------------------------------------------------------------------------

// BudgetSubject identifies what a budget attaches to (§6.4): a key, user,
// team, credential, or the deployment as a whole.
type BudgetSubject struct {
	Kind string
	ID   string
}

// Budget is a subject's ceiling and its consumption for the current period.
type Budget struct {
	Subject BudgetSubject

	MaxBudgetNano  *int64
	SoftBudgetNano *int64
	Period         string
	PeriodStart    time.Time
	PeriodEnd      time.Time

	SpentNano     int64
	ReservedNano  int64
	ReservedUntil time.Time

	UpdatedAt time.Time
}

// BudgetStore is budget definition and state.
//
// dorang has no reusable named-budget object: §9.2's schema carries the ceiling
// on the subject row (api_keys.max_budget_nano and its siblings) and the
// consumption in budget_state. Budgets are therefore addressed by subject
// rather than by a budget id, which is the one place the shape-compatible
// surface's request body differs from the incumbent's.
type BudgetStore interface {
	SetBudget(ctx context.Context, b Budget) error
	GetBudget(ctx context.Context, s BudgetSubject) (Budget, error)
	ClearBudget(ctx context.Context, s BudgetSubject) error
	ListBudgets(ctx context.Context, o ListOptions) ([]Budget, error)
}

// ---------------------------------------------------------------------------
// Ledger
// ---------------------------------------------------------------------------

// Range is a half-open [Start, End) window. Every ledger query takes one, and
// an absent or inverted one is refused rather than defaulted (§9.3).
type Range struct {
	Start time.Time
	End   time.Time
}

// Cursor is a keyset pagination position: the (ts, id) of the last row of the
// previous page. Offsets are not offered over a ledger, because an offset is a
// scan whose cost grows with the page number.
type Cursor struct {
	TS time.Time
	ID string
}

// LogQuery is one page of ledger rows. Exactly the filters §9.3 has an index
// for are offered; anything else would be a partition scan wearing a query
// parameter.
type LogQuery struct {
	Range Range
	Limit int
	After *Cursor

	KeyID   string
	TeamID  string
	UserID  string
	TraceID string
	Tag     string
	// ErrorsOnly restricts to status >= 400, which has its own partial index.
	ErrorsOnly bool
}

// LogRow is one ledger row.
type LogRow struct {
	ID string
	TS time.Time

	APIKeyID     string
	UserID       string
	TeamID       string
	CredentialID string
	ProviderID   string
	DeploymentID string

	ModelGroup    string
	UpstreamModel string
	Endpoint      string

	Status     int
	ErrorClass string

	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	ReasoningTokens  int64
	TotalTokens      int64

	CostNano             int64
	MarginalCostNano     int64
	SubscriptionCostNano int64
	// NotionalNano is the list-rate equivalent (§8.5). NotionalKnown says
	// whether it means anything: a missing notional rate is reported as
	// unavailable, never as zero.
	NotionalNano  int64
	NotionalKnown bool
	// The utilization disclosure (§8.6): the factor applied to this request's
	// rate in parts per million, the occupancy it came from, and where that
	// reading came from. UtilSource is empty on an ordinary rate card, which is
	// how a reader tells "this price does not move" from "it moved by 1.000000".
	UtilMultiplierPPM int64
	UtilPPM           int64
	UtilSource        string

	LatencyMS      int64
	TTFTMS         int64
	QueueMS        int64
	CapacityWaitMS int64
	UpstreamMS     int64

	FallbackCount int
	Streamed      bool

	TraceID   string
	SessionID string
	NodeID    string
	BatchID   string
	Tags      []string
}

// LogPage is one page of ledger rows plus the cursor for the next one. Next is
// nil when the page was not full, which is the only reliable end-of-results
// signal for a table still being written to.
type LogPage struct {
	Rows []LogRow
	Next *Cursor
}

// Usage is the counter set every aggregate carries.
type Usage struct {
	Requests         int64
	Errors           int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	ReasoningTokens  int64
	TotalTokens      int64

	CostNano             int64
	MarginalCostNano     int64
	SubscriptionCostNano int64
	// NotionalNano is the list-rate equivalent, and NotionalKnown says whether
	// it is available. It is a separate field from CostNano by construction: it
	// must never reach billing, budget, quota or routing (§8.5).
	NotionalNano  int64
	NotionalKnown bool

	LatencyMSSum int64
}

// Add folds o into u. A notional total is known only if every contributing part
// was known — one unpriced model makes the sum an understatement, and an
// understatement presented as a total is exactly the flattering answer §8.5
// forbids.
func (u *Usage) Add(o Usage) {
	u.Requests += o.Requests
	u.Errors += o.Errors
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.CachedTokens += o.CachedTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.TotalTokens += o.TotalTokens
	u.CostNano += o.CostNano
	u.MarginalCostNano += o.MarginalCostNano
	u.SubscriptionCostNano += o.SubscriptionCostNano
	u.NotionalNano += o.NotionalNano
	u.NotionalKnown = u.NotionalKnown && o.NotionalKnown
	u.LatencyMSSum += o.LatencyMSSum
}

// GroupBy names one dimension of a spend report.
type GroupBy string

// Report dimensions. Each maps onto an index or a rollup; there is deliberately
// no free-form grouping, because a dimension without an index is a scan.
const (
	GroupByDay      GroupBy = "day"
	GroupByModel    GroupBy = "model"
	GroupByKey      GroupBy = "key"
	GroupByTeam     GroupBy = "team"
	GroupByUser     GroupBy = "user"
	GroupByTag      GroupBy = "tag"
	GroupByProvider GroupBy = "provider"
)

// ReportQuery asks for aggregated spend over a bounded window.
type ReportQuery struct {
	Range   Range
	GroupBy []GroupBy
	Limit   int
}

// ReportRow is one aggregate bucket. Only the dimensions named in the query's
// GroupBy are populated.
type ReportRow struct {
	Day        time.Time
	ModelGroup string
	KeyID      string
	TeamID     string
	UserID     string
	Tag        string
	ProviderID string
	Usage      Usage
}

// Report is an aggregated spend answer.
type Report struct {
	Range Range
	Rows  []ReportRow
	Total Usage
}

// Ledger is the request-log half of the store.
//
// Every method takes a bounded range and every list method paginates. An
// implementation must refuse an unbounded range with [ErrUnboundedRange] rather
// than widening it to a default: §9.3 says refused, and a silently capped query
// returns a partial answer that looks complete.
type Ledger interface {
	ListRequests(ctx context.Context, q LogQuery) (LogPage, error)
	Report(ctx context.Context, q ReportQuery) (Report, error)
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// AuditEntry is one audit_logs row. Actor, action, before and after are all
// required by §2.3's "everything is audited"; before is empty for a creation
// and after is empty for a deletion, and both being empty is a bug.
type AuditEntry struct {
	ID string
	TS time.Time

	ActorKind string
	ActorID   string

	Action     string
	ObjectKind string
	ObjectID   string

	// Before and After are JSON documents, or empty for "did not exist". They
	// are rendered from the same redacted value types the API returns, so an
	// audit row cannot carry a secret the API would not.
	Before string
	After  string

	IP        string
	UserAgent string
}

// Auditor persists audit rows.
type Auditor interface {
	Record(ctx context.Context, e AuditEntry) error
}

// ---------------------------------------------------------------------------
// Native reporting: credentials, quota, capacity, health, catalog, pricing
// ---------------------------------------------------------------------------

// QuotaWindow is one credential quota window's consumption (§6.2, §12.3).
type QuotaWindow struct {
	Window string
	Metric string
	Used   int64
	// Limit is zero when the provider declared none.
	Limit   int64
	UsedPct int
	ResetAt time.Time
	// Source is "local", "provider" or "combined": §6.2 combines the two
	// rather than replacing one with the other, and an operator reading a
	// percentage needs to know which they are looking at.
	Source string
	// Stale reports that the provider-reported half is older than its
	// freshness budget, so the number is a floor rather than a fact.
	Stale bool
}

// CredentialStatus is one credential's health and quota (§12.3).
//
// It carries ids and health only. No key material reaches this package, so none
// can appear here (§11.2b).
type CredentialStatus struct {
	ID         string
	ProviderID string
	// Health is "healthy", "unavailable", "half_open" or "unknown".
	Health              string
	UnavailableUntil    time.Time
	ConsecutiveFailures int

	Requests int64
	Failures int64
	Opens    int64

	TTFTMS       int64
	LatencyMS    int64
	TokensPerSec float64

	Quota     []QuotaWindow
	UpdatedAt time.Time
}

// CredentialReporter supplies credential health and quota snapshots.
type CredentialReporter interface {
	Credentials(ctx context.Context) ([]CredentialStatus, error)
}

// AxisOccupancy is one capacity axis key's occupancy (§5.2).
type AxisOccupancy struct {
	Axis string
	Key  string
	// InUse is the number of committed reservations on this key.
	InUse int
	// Limit is the configured ceiling before any interactive reserve; zero
	// means unlimited.
	Limit int
	// Waiting is the queue depth on this key.
	Waiting int
}

// CapacityOccupancy is a consistent view of the capacity broker.
type CapacityOccupancy struct {
	Axes []AxisOccupancy
	// Waiting is the number of distinct blocked acquisitions. It is not the sum
	// of AxisOccupancy.Waiting, because one waiter may sit in several queues.
	Waiting      int
	Reservations int
	Grants       uint64
	Wakeups      uint64
	Expired      uint64
}

// CapacityReporter supplies capacity occupancy.
type CapacityReporter interface {
	Occupancy(ctx context.Context) (CapacityOccupancy, error)
}

// HealthEvent is one recorded health transition or probe result.
type HealthEvent struct {
	TS time.Time
	// SubjectKind is "credential", "deployment" or "provider".
	SubjectKind string
	Subject     string
	// State is the state entered: "healthy", "unavailable", "half_open".
	State  string
	Reason string
	// Status is the HTTP status that produced the transition, when there was
	// one; zero otherwise.
	Status    int
	LatencyMS int64
}

// HealthQuery bounds a health history read. It takes a range for the same
// reason every ledger query does.
type HealthQuery struct {
	Range   Range
	Subject string
	Limit   int
}

// HealthHistory serves /health/history.
//
// §9.2 has credential_state, which is a current state and not a history, so
// there is no table behind this in the shipped schema. The interface exists so
// that a process which does keep transitions — see [MemoryHealthHistory] — can
// answer, and so that a process which does not answers 501 with a code rather
// than an empty list that looks like "nothing ever failed".
type HealthHistory interface {
	History(ctx context.Context, q HealthQuery) ([]HealthEvent, error)
}

// FieldOrigin is where one composed catalog field came from (§4.3).
type FieldOrigin struct {
	Field string
	Value string
	// Layer is "kind", "prefix" or "model".
	Layer string
	// Origin is the source kind: embedded, file, environment.
	Origin string
	Source string
}

// ModelExplanation is the provenance of a composed model entry.
type ModelExplanation struct {
	Kind  string
	Model string

	Fields        []FieldOrigin
	Layers        []string
	MatchedPrefix string
	Verified      string
	KindKnown     bool
	ModelKnown    bool
	Note          string
}

// Catalog is pkg/catalog narrowed to what the admin surface shows.
type Catalog interface {
	ExplainModel(kind, model string) (ModelExplanation, error)
	// UnverifiedModels lists models whose existence has not been confirmed
	// against the provider. §4.3 treats an unverified capability claim as a
	// defect, so the list is a work queue rather than trivia.
	UnverifiedModels() []string
}

// PriceRequest is the input to the pricing preview of §8.4.
type PriceRequest struct {
	Provider   string
	Model      string
	Credential string
	Deployment string

	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64

	Requests   int64
	Characters int64
	Seconds    float64

	At time.Time
}

// PriceComponent is one priced line.
type PriceComponent struct {
	Name   string
	RuleID string
	// Rate is the exact decimal as written in the catalog, never a float.
	Rate     string
	Unit     string
	Quantity int64
	// Scale is the negative power of ten Quantity is expressed in.
	Scale        int32
	SubtotalNano int64
}

// PriceRule is one rule that contributed to a price.
type PriceRule struct {
	RuleID   string
	Class    string
	Level    string
	Priority int
	Order    int
	// Why renders the selection reason in words (§8.4: "why each rule was
	// selected").
	Why string
}

// PriceConsidered is one rule that was examined, and what became of it.
type PriceConsidered struct {
	RuleID   string
	Level    string
	Priority int
	Order    int
	Eligible bool
	Selected bool
	Reason   string
}

// PriceClassTrace is the evaluation of one rule class.
type PriceClassTrace struct {
	Class      string
	Considered []PriceConsidered
}

// PriceNotional is the audit trail of a notional figure (§8.5). source and
// as_of are required of every notional rule, so they travel with the number:
// an estimate with no provenance is a guess wearing a currency symbol.
type PriceNotional struct {
	RuleID string
	Source string
	AsOf   string
	// AgeSeconds is how stale the rate was at the priced instant.
	AgeSeconds int64
	Nano       int64
	Components []PriceComponent
	// Missing reports that no notional rule matched, so Nano is unavailable
	// rather than zero.
	Missing bool
}

// PriceExplanation is the preview endpoint's answer: the applied rule chain per
// class, each component's rate and quantity, the subtotal, the final amount,
// and why each rule was selected.
type PriceExplanation struct {
	Currency string

	MarginalNano     int64
	SubscriptionNano int64
	AdjustmentNano   int64
	// TotalNano is the billed figure. The notional amount is deliberately not a
	// term in it.
	TotalNano int64

	Components []PriceComponent
	Applied    []PriceRule
	Classes    []PriceClassTrace
	Notional   PriceNotional

	// Missing reports that no marginal rule matched: the cost is zero and the
	// caller is expected to treat that as a defect, not as free traffic.
	Missing bool
	Notes   []string
}

// Pricer is internal/pricing's explain path. The admin calculator and the CLI
// use the same engine, so there is one answer rather than three (§8.4).
type Pricer interface {
	Explain(ctx context.Context, req PriceRequest) (PriceExplanation, error)
}

// ReloadResult reports what a configuration reload did.
type ReloadResult struct {
	Version   string
	LoadedAt  time.Time
	Changed   []string
	Warnings  []string
	Unchanged bool
}

// Reloader re-reads configuration without a restart.
type Reloader interface {
	Reload(ctx context.Context) (ReloadResult, error)
}
