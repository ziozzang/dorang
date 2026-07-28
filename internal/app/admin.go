package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/admin"
	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/store"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// The administration surface, mounted.
//
// `internal/admin` — twenty files, key issuance, /key/block, budgets, spend
// reporting, an audit trail and an embedded operator UI — had zero importers.
// The operational consequence was not a category, it was this: an operator
// holding a leaked `sk-` key had no API path to revoke it. `POST /key/block`
// answered 501 and the documented incident-response step was a hand-written
// `UPDATE api_keys SET blocked = 1`.
//
// # What is wired, and what is not
//
// Not everything. The surface declares thirteen dependencies and this process
// can honestly supply six of them; the rest have no storage behind them at all
// — `internal/store` has no Go code for `users`, `teams`, `team_members`,
// `deployments` or `model_aliases`, and no range-aggregating report query.
// Those endpoints answer `501 dependency_not_configured` NAMING the missing
// piece, which is the answer `internal/admin` was built to give and is the one
// an operator can act on. Handing them a half-working adapter over a table
// nobody writes would be worse than a 501: it would look like it worked.
//
// Wired:
//
//   - Keys — the whole lifecycle, including block and unblock. This is the one
//     that mattered.
//   - Hasher — issuing and rotating credentials, peppered through the same
//     values internal/auth and internal/store already share.
//   - Audit — every mutation, into the `audit_logs` table that has had a
//     schema, two indexes and a retention sweep since the first migration and
//     no writer.
//   - Ledger reads — /spend/logs, over the five indexed queries the store has.
//   - Capacity and Catalog — read-only reporting off objects App already holds.
//   - Health history — an in-process ring, which is what the package ships.
//
// Not wired, and named rather than implied:
//
//   - Directory (users, teams, members), ModelRegistry (deployments, aliases),
//     BudgetStore (ceilings): no store layer exists.
//   - Ledger.Report: no range-aggregating query exists, so /global/spend/report
//     and the three daily-activity endpoints answer 501.
//   - CredentialReporter, Pricer, Reloader: the objects exist in this process
//     but are not reachable from App as it stands.

// adminRoutePrefixes are the path families the administration surface owns.
//
// They are mounted as wildcard routes rather than one route per endpoint,
// because internal/admin owns its own exact table and mounting a second copy
// here would be two lists of paths that must agree — the defect shape the
// review named twice already (two authentication-header lists, five redirect
// policies). One list, in the package that serves it.
var adminRoutePrefixes = []struct{ pattern, name string }{
	{"/key/{rest...}", "admin_key"},
	{"/user/{rest...}", "admin_user"},
	{"/team/{rest...}", "admin_team"},
	{"/model/{rest...}", "admin_model"},
	{"/model_group/{rest...}", "admin_model_group"},
	{"/budget/{rest...}", "admin_budget"},
	{"/spend/{rest...}", "admin_spend"},
	{"/global/{rest...}", "admin_global"},
	{"/organization/{rest...}", "admin_organization"},
	{"/customer/{rest...}", "admin_customer"},
	{"/cache/{rest...}", "admin_cache"},
	{"/audit/{rest...}", "admin_audit"},
	{"/tag/{rest...}", "admin_tag"},
	{"/admin/{rest...}", "admin_native"},
	{"/health/history", "admin_health_history"},
}

// adminRoutes renders the administration surface as gateway routes.
//
// Nil when no administration surface was built, in which case the paths keep
// answering the 501 they answered before — `plannedRoutePrefixes` in
// internal/server still lists them, so the answer is still "declared, not
// built" rather than "no such path".
func (a *App) adminRoutes() []server.Route {
	if a.Admin == nil {
		return nil
	}
	h := func(w http.ResponseWriter, rq *server.Request) error {
		// The administration surface authorizes for itself: entry (is this an
		// administrator at all) and scope (which subjects may it act on). The
		// gateway has already authenticated, so an anonymous caller never gets
		// here — see the Public comment below — and admin.API re-resolves the
		// same header through the same authenticator, which is a cache hit.
		a.Admin.ServeHTTP(w, rq.HTTP)
		return nil
	}
	out := make([]server.Route, 0, len(adminRoutePrefixes)+2)
	for _, p := range adminRoutePrefixes {
		out = append(out, server.Route{
			Pattern: p.pattern,
			Methods: server.MethodGET | server.MethodHEAD | server.MethodPOST |
				server.MethodPUT | server.MethodDELETE | server.MethodOPTIONS,
			Name:   p.name,
			Family: server.FamilyAdmin,
			// NeedsBody is false and must be: admin.API decodes r.Body itself,
			// under its own cap, and a body the gateway had already drained
			// would arrive empty. It also means these routes never enter the
			// request-replay budget, which exists for retryable inference and
			// not for a one-shot mutation.
			NeedsBody: false,
			// No model is reachable from any administrative path. Declaring it
			// is not a formality — internal/server refuses a route with no
			// ModelAuth answer at registration, which is what stops the next
			// route from being added without one.
			ModelAuth: server.ModelAuthNone,
			Handler:   h,
		})
	}
	if p := a.Admin.UIPrefix(); p != "" {
		// The UI is Public in the gateway's sense — it authenticates itself —
		// because it has to serve a sign-in form to a caller who by definition
		// has not signed in yet. Every screen behind that form goes through
		// admin.API's own authenticate(), and the session cookie holds an
		// authorization decision rather than a credential.
		for _, pat := range []string{p, p + "/{rest...}"} {
			out = append(out, server.Route{
				Pattern:   pat,
				Methods:   server.MethodGET | server.MethodHEAD | server.MethodPOST,
				Name:      "admin_ui",
				Family:    server.FamilyAdmin,
				Public:    true,
				NeedsBody: false,
				ModelAuth: server.ModelAuthNone,
				Handler:   h,
			})
		}
	}
	return out
}

// buildAdmin assembles the administration surface, or returns nil when this
// process has no store to administer.
func (a *App) buildAdmin() (*admin.API, error) {
	if a.Store == nil {
		// Nothing to administer and nothing to audit. A surface that could
		// issue keys into a store that does not exist would be worse than its
		// absence.
		return nil, nil
	}
	hasher, err := auth.NewHasher(a.pepper, auth.LegacyPolicy{})
	if err != nil {
		// No pepper means no issuance (DESIGN §2.4 refuses a pepperless
		// digest). The rest of the surface — block, unblock, list, spend — does
		// not need one, so the surface is still built and only the issuing
		// endpoints answer that the pepper is absent.
		hasher = nil
	}
	return admin.New(admin.Config{
		Auth:   &adminAuthenticator{auth: a.Auth, store: a.Store},
		Keys:   &adminKeyStore{st: a.Store},
		Hasher: adminHasherOrNil(hasher),
		Audit:  &adminAuditor{st: a.Store, newID: admin.NewID},
		Ledger: &adminLedger{st: a.Store},

		Capacity: &adminCapacityReporter{b: a.Broker},
		Catalog:  &adminCatalog{c: a.Catalog},
		Health:   admin.NewMemoryHealthHistory(1024, a.now()),

		Now: a.now,
	})
}

// adminHasherOrNil keeps a typed nil out of the interface. A typed nil in an
// interface is non-nil, and admin.API's "is a hasher configured" check is a nil
// test — the difference between a named 501 and a panic.
func adminHasherOrNil(h *auth.Hasher) admin.Hasher {
	if h == nil {
		return nil
	}
	return &adminHasher{h: h}
}

// ---------------------------------------------------------------------------
// Authorization
// ---------------------------------------------------------------------------

// adminRoles are the roles that make a key administrative.
//
// It is the subset of internal/admin's own `knownRoles` that means "may
// administer". `internal_user` and `internal_viewer` are ordinary tenants and
// are deliberately absent: they exist in the vocabulary so that an import from
// an incumbent does not lose them, not so that they can administer anything.
var adminRoles = map[string]bool{
	"admin":        true,
	"admin_viewer": true,
	"proxy_admin":  true,
}

// adminAuthenticator resolves an administrative caller.
//
// The role join is here rather than in internal/admin because a key carries no
// role of its own — it is on the owning user (DESIGN §9.2) — and the package
// that owns the schema is the one that should do the join.
type adminAuthenticator struct {
	auth  *auth.Authenticator
	store *store.Store
}

func (ad *adminAuthenticator) AuthenticateHeader(ctx context.Context, h http.Header) (admin.Principal, error) {
	p, err := ad.auth.AuthenticateHeader(ctx, h)
	if err != nil {
		return nil, admin.ErrUnauthenticated
	}
	if p == nil {
		return nil, admin.ErrUnauthenticated
	}
	if p.Master {
		return adminPrincipal{kind: "master", scope: admin.GlobalScope(), admin: true}, nil
	}
	// A credential that authenticated but may not administer is returned as a
	// NON-ADMIN principal rather than as an error. internal/admin draws the
	// distinction on purpose — an error from the authenticator is a 401 ("no
	// usable credential"), a principal that is not an admin is a 403 ("valid
	// credential, wrong authority") — and collapsing the two would tell a
	// tenant holding a working key that its key does not work.
	notAdmin := adminPrincipal{kind: "key", id: p.KeyID}

	// A blocked or expired credential is not an administrator, whatever its
	// user's role says. Authorize with an Access carrying only the clock asks
	// exactly the subject-level questions — blocked, expired, no owner —
	// without asking about a model or a route, neither of which an
	// administrative call has.
	if err := p.Authorize(auth.Access{Now: ad.now()}); err != nil {
		return notAdmin, nil
	}
	role, err := ad.store.UserRole(ctx, p.UserID)
	if err != nil {
		// No owning user, or no row: not an administrator. This is the
		// fail-closed direction and it is the common case — an ordinary key
		// reaching an administrative path.
		return notAdmin, nil
	}
	if !adminRoles[strings.TrimSpace(role)] {
		return notAdmin, nil
	}
	return adminPrincipal{
		kind:  "key",
		id:    p.KeyID,
		admin: true,
		scope: adminScopeFor(p),
	}, nil
}

func (ad *adminAuthenticator) now() time.Time { return time.Now() }

// adminScopeFor derives an administrative scope from the key itself.
//
// The rule and its reasoning are in admin.Scope's documentation. The part that
// belongs here is where the fact comes from: `api_keys.team_id`, which the
// authenticator already carries on every principal. An administrative key on a
// team administers that team; an administrative key on no team is the operator.
//
// Deriving it rather than storing it is what makes it un-forgettable. There is
// no `admin_team` column to leave unset and no migration to skip.
func adminScopeFor(p *auth.Principal) admin.Scope {
	if p.TeamID != "" {
		return admin.TeamScope(p.TeamID)
	}
	return admin.GlobalScope()
}

type adminPrincipal struct {
	kind  string
	id    string
	admin bool
	scope admin.Scope
}

func (p adminPrincipal) ActorKind() string       { return p.kind }
func (p adminPrincipal) ActorID() string         { return p.id }
func (p adminPrincipal) IsAdmin() bool           { return p.admin }
func (p adminPrincipal) AdminScope() admin.Scope { return p.scope }

// ---------------------------------------------------------------------------
// Keys
// ---------------------------------------------------------------------------

type adminKeyStore struct{ st *store.Store }

func (k *adminKeyStore) CreateKey(ctx context.Context, in *admin.Key, v admin.Verifier) error {
	row := storeKeyFrom(in)
	row.Lookup = v.Lookup
	row.TokenHash = v.TokenHash
	row.HashScheme = store.HashScheme(v.HashScheme)
	if err := k.st.InsertAPIKey(ctx, row); err != nil {
		return adminStoreError(err)
	}
	return nil
}

func (k *adminKeyStore) GetKey(ctx context.Context, id string) (*admin.Key, error) {
	row, err := k.st.GetAPIKey(ctx, id)
	if err != nil {
		return nil, adminStoreError(err)
	}
	return adminKeyFrom(row), nil
}

func (k *adminKeyStore) UpdateKey(ctx context.Context, in *admin.Key) error {
	return adminStoreError(k.st.UpdateAPIKey(ctx, storeKeyFrom(in)))
}

func (k *adminKeyStore) DeleteKeys(ctx context.Context, ids []string) (int, error) {
	n, err := k.st.DeleteAPIKeys(ctx, ids)
	return n, adminStoreError(err)
}

func (k *adminKeyStore) ListKeys(ctx context.Context, f admin.KeyFilter) ([]*admin.Key, error) {
	rows, err := k.st.ListAPIKeys(ctx, store.APIKeyFilter{
		UserID: f.UserID, TeamID: f.TeamID, Blocked: f.Blocked,
		Limit: f.Limit, Offset: f.Offset,
	})
	if err != nil {
		return nil, adminStoreError(err)
	}
	out := make([]*admin.Key, 0, len(rows))
	for _, r := range rows {
		out = append(out, adminKeyFrom(r))
	}
	return out, nil
}

// ReplaceVerifier rotates the secret behind a key id.
//
// The cutover is immediate and the reasoning is at
// [store.Store.ReplaceKeyVerifier]: a grace window is right for a planned
// rotation and wrong for an incident, and this is the incident path.
func (k *adminKeyStore) ReplaceVerifier(ctx context.Context, id string, v admin.Verifier,
	label string, now time.Time) error {

	return adminStoreError(k.st.ReplaceKeyVerifier(ctx, id,
		v.Lookup, v.TokenHash, store.HashScheme(v.HashScheme), label, now))
}

func adminKeyFrom(k *store.APIKey) *admin.Key {
	return &admin.Key{
		ID: k.ID, KeyLabel: k.KeyLabel, KeyAlias: k.KeyAlias,
		UserID: k.UserID, TeamID: k.TeamID,
		Models: k.Models, AllowedRoutes: k.AllowedRoutes,
		ObjectPermissionID: k.ObjectPermissionID,
		MaxBudgetNano:      k.MaxBudgetNano, SoftBudgetNano: k.SoftBudgetNano,
		BudgetPeriod: k.BudgetPeriod, BudgetResetAt: k.BudgetResetAt, SpendNano: k.SpendNano,
		RPMLimit: k.RPMLimit, TPMLimit: k.TPMLimit, MaxParallel: k.MaxParallel,
		PriorityClass: k.PriorityClass, Tags: k.Tags,
		Blocked: k.Blocked, ExpiresAt: k.ExpiresAt,
		HashScheme: string(k.HashScheme), Source: k.Source,
		CreatedAt: k.CreatedAt, UpdatedAt: k.UpdatedAt,
	}
}

func storeKeyFrom(k *admin.Key) *store.APIKey {
	return &store.APIKey{
		ID: k.ID, KeyLabel: k.KeyLabel, KeyAlias: k.KeyAlias,
		UserID: k.UserID, TeamID: k.TeamID,
		Models: k.Models, AllowedRoutes: k.AllowedRoutes,
		ObjectPermissionID: k.ObjectPermissionID,
		MaxBudgetNano:      k.MaxBudgetNano, SoftBudgetNano: k.SoftBudgetNano,
		BudgetPeriod: k.BudgetPeriod, BudgetResetAt: k.BudgetResetAt, SpendNano: k.SpendNano,
		RPMLimit: k.RPMLimit, TPMLimit: k.TPMLimit, MaxParallel: k.MaxParallel,
		PriorityClass: k.PriorityClass, Tags: k.Tags,
		Blocked: k.Blocked, ExpiresAt: k.ExpiresAt,
		HashScheme: store.HashScheme(k.HashScheme), Source: k.Source,
		CreatedAt: k.CreatedAt, UpdatedAt: k.UpdatedAt,
	}
}

// ---------------------------------------------------------------------------
// Hashing
// ---------------------------------------------------------------------------

type adminHasher struct{ h *auth.Hasher }

func (a *adminHasher) Lookup(token string) string { return a.h.Lookup(token).Hex() }

// Hash always issues under dorang_v1. The interface offers no way to ask for
// legacy_sha256 and this implementation offers no way to produce one: legacy is
// an import format, and a surface that could mint one would be a surface that
// could downgrade a credential.
func (a *adminHasher) Hash(token string) (string, string, error) {
	d, err := a.h.Hash(auth.SchemeDorangV1, token)
	if err != nil {
		return "", "", err
	}
	return d.Hex(), auth.SchemeDorangV1.String(), nil
}

// Label derives the display label from the lookup digest and never from the
// secret's own characters — the incumbent mistake DESIGN §2.4 names.
func (a *adminHasher) Label(token string) string { return store.LabelFor(token) }

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

type adminAuditor struct {
	st    *store.Store
	newID func() string
}

func (a *adminAuditor) Record(ctx context.Context, e admin.AuditEntry) error {
	id := e.ID
	if id == "" {
		id = a.newID()
	}
	ts := e.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	return a.st.InsertAuditLog(ctx, store.AuditLog{
		ID:         id,
		TS:         ts,
		ActorKind:  e.ActorKind,
		ActorID:    e.ActorID,
		Action:     e.Action,
		ObjectKind: e.ObjectKind,
		ObjectID:   e.ObjectID,
		Before:     e.Before,
		After:      e.After,
		IP:         e.IP,
		UserAgent:  e.UserAgent,
	})
}

// ---------------------------------------------------------------------------
// Ledger
// ---------------------------------------------------------------------------

// adminLedger serves /spend/logs from the five indexed queries internal/store
// has, and refuses the report endpoints that have no query behind them.
type adminLedger struct{ st *store.Store }

func (l *adminLedger) ListRequests(ctx context.Context, q admin.LogQuery) (admin.LogPage, error) {
	r := store.TimeRange{Start: q.Range.Start, End: q.Range.End}
	p := store.Page{Limit: q.Limit}
	if q.After != nil {
		p.After = &store.Cursor{TS: q.After.TS, ID: q.After.ID}
	}

	// The filter with an index wins, in the order of how selective it is. A
	// query with no filter at all is refused rather than turned into a table
	// scan: DESIGN §9.3's rule is that unbounded search is refused, not
	// answered slowly.
	var (
		page store.LedgerPage
		err  error
	)
	switch {
	case q.TraceID != "":
		page, err = l.st.ListRequestsByTrace(ctx, q.TraceID, r, p)
	case q.KeyID != "":
		page, err = l.st.ListRequestsByKey(ctx, q.KeyID, r, p)
	case q.TeamID != "":
		page, err = l.st.ListRequestsByTeam(ctx, q.TeamID, r, p)
	case q.Tag != "":
		page, err = l.st.ListRequestsByTag(ctx, q.Tag, r, p)
	case q.ErrorsOnly:
		page, err = l.st.ListErrors(ctx, r, p)
	case q.UserID != "":
		// user_id is a stored column with no index of its own, so there is no
		// query to run. Naming which filters DO work is the difference between
		// a caller retrying the same thing and a caller fixing it.
		return admin.LogPage{}, unsupportedLedger(
			"the ledger has no per-user index; filter by key_id, team_id, trace_id or tag")
	default:
		return admin.LogPage{}, unsupportedLedger(
			"a ledger query needs a filter: key_id, team_id, trace_id, tag or errors_only")
	}
	if err != nil {
		return admin.LogPage{}, adminStoreError(err)
	}

	out := admin.LogPage{Rows: make([]admin.LogRow, 0, len(page.Rows))}
	for _, row := range page.Rows {
		out.Rows = append(out.Rows, adminLogRow(row))
	}
	if page.Next != nil {
		out.Next = &admin.Cursor{TS: page.Next.TS, ID: page.Next.ID}
	}
	return out, nil
}

// Report has no implementation because there is no query. internal/store has
// three single-bucket rollup reads and one per-credential spend total; none of
// them answers "top N over a range grouped by a dimension". A hand-rolled
// aggregate over request_logs would be a scan with no index, which §9.3 refuses
// on purpose.
func (l *adminLedger) Report(context.Context, admin.ReportQuery) (admin.Report, error) {
	return admin.Report{}, unsupportedLedger(
		"aggregate spend reporting has no rollup query behind it in this build; " +
			"/spend/logs serves the per-request ledger")
}

func unsupportedLedger(msg string) error {
	return errors.New("app: " + msg + " (" + admin.ErrUnsupported.Error() + ")")
}

func adminLogRow(r store.RequestLog) admin.LogRow {
	return admin.LogRow{
		ID: r.ID, TS: r.TS,
		APIKeyID: r.APIKeyID, UserID: r.UserID, TeamID: r.TeamID,
		CredentialID: r.CredentialID, ProviderID: r.ProviderID, DeploymentID: r.DeploymentID,
		ModelGroup: r.ModelGroup, UpstreamModel: r.UpstreamModel, Endpoint: r.Endpoint,
		Status: r.Status, ErrorClass: r.ErrorClass,
		PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
		CachedTokens: r.CachedTokens, ReasoningTokens: r.ReasoningTokens,
		TotalTokens: r.TotalTokens,
		CostNano:    r.CostNano, MarginalCostNano: r.MarginalCostNano,
		SubscriptionCostNano: r.SubscriptionCostNano,
		// NotionalKnown stays false: the ledger has no notional column in this
		// build, and reporting a missing list rate as zero is the one thing
		// DESIGN §8.5 says not to do.
		LatencyMS: r.LatencyMS, TTFTMS: r.TTFTMS, QueueMS: r.QueueMS,
		CapacityWaitMS: r.CapacityWaitMS, UpstreamMS: r.UpstreamMS,
		FallbackCount: r.FallbackCount, Streamed: r.Streamed,
		TraceID: r.TraceID, SessionID: r.SessionID, NodeID: r.NodeID,
		BatchID: r.BatchID, Tags: r.Tags,
	}
}

// ---------------------------------------------------------------------------
// Read-only reporting
// ---------------------------------------------------------------------------

type adminCapacityReporter struct{ b *capacity.Broker }

func (a *adminCapacityReporter) Occupancy(context.Context) (admin.CapacityOccupancy, error) {
	if a.b == nil {
		return admin.CapacityOccupancy{}, nil
	}
	s := a.b.Snapshot()
	out := admin.CapacityOccupancy{
		Waiting: s.Waiting, Reservations: s.Reservations,
		Grants: s.Grants, Wakeups: s.Wakeups, Expired: s.Expired,
		Axes: make([]admin.AxisOccupancy, 0, len(s.Axes)),
	}
	for _, ax := range s.Axes {
		out.Axes = append(out.Axes, admin.AxisOccupancy{
			Axis: ax.Axis.String(), Key: ax.Key,
			InUse: ax.InUse, Limit: ax.Limit, Waiting: ax.Waiting,
		})
	}
	return out, nil
}

type adminCatalog struct{ c *catalog.Catalog }

func (a *adminCatalog) ExplainModel(kind, model string) (admin.ModelExplanation, error) {
	if a.c == nil {
		return admin.ModelExplanation{}, nil
	}
	info := a.c.Model(kind, model)
	fields := a.c.Explain(kind, model)
	out := admin.ModelExplanation{
		KindKnown: info.KindKnown, ModelKnown: info.ModelKnown,
		Layers: info.Layers, MatchedPrefix: info.MatchedPrefix,
		Verified: info.Verified, Note: info.Note,
		Fields: make([]admin.FieldOrigin, 0, len(fields)),
	}
	for _, f := range fields {
		out.Fields = append(out.Fields, admin.FieldOrigin{
			Field: f.Field, Value: f.Value, Layer: f.Layer,
			Origin: string(f.Origin), Source: f.Source,
		})
	}
	return out, nil
}

func (a *adminCatalog) UnverifiedModels() []string {
	if a.c == nil {
		return nil
	}
	return a.c.UnverifiedModels()
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// adminStoreError translates internal/store's sentinels into internal/admin's.
//
// The two packages keep separate vocabularies deliberately — neither imports
// the other — so errors.Is does not bridge them and this is the one place the
// mapping lives. An unrecognized error passes through unchanged and becomes a
// 500 with a fixed message: admin.faultFor does not echo it, because a driver
// error can carry a DSN and a DSN can carry a password.
func adminStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return admin.ErrNotFound
	case errors.Is(err, store.ErrExists):
		return admin.ErrConflict
	case errors.Is(err, store.ErrUnboundedRange):
		return admin.ErrUnboundedRange
	case errors.Is(err, store.ErrRangeTooWide):
		return admin.ErrRangeTooWide
	}
	return err
}
