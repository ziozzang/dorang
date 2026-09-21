package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/admin"
	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/cluster"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/pricing"
	"github.com/ziozzang/dorang/internal/quota"
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
// Not everything, and the line is drawn on one question: does the row this
// surface writes reach a decision the gateway makes? Where it does, the endpoint
// is served. Where it cannot, the endpoint answers `501
// dependency_not_configured` NAMING the missing piece, which is the answer
// `internal/admin` was built to give and is the one an operator can act on.
// Handing them a half-working adapter over a table nobody reads would be worse
// than a 501: it would look like it worked.
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
//   - Ledger reads — /spend/logs, over the five indexed queries the store has,
//     and /global/spend/report over the three rollup materializations of §9.4.
//     The report groups by day and by one of model, key or team, because those
//     are the keys the rollups actually have; §9.4 keeps purpose-built
//     materializations rather than a cube, so any other grouping is refused by
//     name instead of turned into a table scan.
//   - Capacity and Catalog — read-only reporting off objects App already holds.
//   - Health history — an in-process ring, which is what the package ships.
//   - Directory — users, teams and membership, over the `users`, `teams` and
//     `team_members` tables. See [adminDirectory]. It is wired together with the
//     owner join in [store.Store.ResolveKeyRecord] and never before it: a
//     `blocked` flag the authorizer cannot read is worse than no route, because
//     it reads as a control and is a note.
//   - Budgets — the ceiling on a key, a user or a team. See [adminBudgets].
//
// Not wired, and named rather than implied:
//
//   - ModelRegistry (the `/model/*` WRITES). This one is NOT waiting on a store
//     layer; it is waiting on a reader. The routing table is compiled from
//     configuration by [buildRouter] — `models[]`, aliases and classes out of
//     internal/config — and the `deployments` and `model_aliases` tables have no
//     reader anywhere in the binary. An adapter over them would let an operator
//     POST /model/new, get a 200, see the row in the database, and route no
//     traffic differently, which is precisely the "stores a value nobody reads"
//     failure the Directory work above exists to remove. It stays a 501 that
//     names the gap until routing reads the table or the table is dropped.
//
//     The READ half is wired, and the distinction is the point: see
//     [adminRouting]. `/model/*` still refuses, and the operator UI's models
//     screen — which only ever reads — is served from the routing table this
//     process compiled, because the alternative was a navigation item that
//     answered 501 on every click in every deployment.
//   - The daily-activity endpoints. They ask for a second, per-day per-subject
//     per-MODEL breakdown, and no materialization is keyed on two dimensions at
//     once. /global/spend/report answers the single-dimension question and
//     /spend/logs the per-request one; a cube would be the bloat §9.4 refuses.
//   - CredentialReporter, Reloader: the objects exist in this process but are
//     not reachable from App as it stands.
//
// Pricer was on that last line and is wired now. It had the worst shape of the
// three: `POST /admin/pricing/preview` and `POST /spend/calculate` are complete
// in internal/admin and answered `501 dependency_unavailable` in every
// deployment, so §8.4's "one engine, one answer" held for `dorangctl price` and
// for the ledger and for neither of the two HTTP surfaces §8.4 names. See
// [adminPricer].

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
		Spend:  &adminSpendReporter{st: a.Store, now: a.now},

		// Users, teams and the ceilings on them. Both are wired only because
		// the authorization envelope now carries what they write — see
		// [adminDirectory] and [adminBudgets].
		Directory: &adminDirectory{st: a.Store},
		Budgets:   &adminBudgets{st: a.Store, now: a.now},

		// The invalidation half of DESIGN §11.2c. Without it every credential
		// mutation on this surface — block first among them — was a durable
		// write the fleet honoured when its snapshot next refreshed, 60 s later
		// on every node that did not serve the call. cluster.KeyControl already
		// published for the five controls it owns; this is the same publication
		// for the lifecycle internal/admin owns.
		Invalidator: adminInvalidatorOrNil(a.KeyControl),

		// The read-only half of the models screen. `Models` — the editable
		// registry `/model/*` writes through — stays unset for the reason above;
		// this is what the gateway actually routes on. See [adminRouting].
		Routing: &adminRouting{cfg: a.Config},

		Capacity:    &adminCapacityReporter{b: a.Broker},
		Credentials: &adminCredentials{a: a},
		// The monitoring screen's real-time pulse: this node's live HTTP
		// counters, the same set GET /metrics exposes. Late-bound to a.Server
		// because the server does not exist yet — see [adminSurface].
		Surface:    &adminSurface{a: a},
		Prometheus: a.Metrics,
		Traffic:    &a.traffic,
		// Config reload and structured config edits (take a deployment in or out
		// of routing). Both late-bind to the file watcher via SetConfigControl,
		// so they answer "not ready" until start-up wires it.
		Reloader:     &adminReloader{a: a},
		ConfigWriter: &adminConfigWriter{a: a},
		Setup:        &adminSetup{a: a},
		Catalog:      &adminCatalog{c: a.Catalog},
		Pricing:      &adminPricer{d: a.dispatch},
		Health:       admin.NewMemoryHealthHistory(1024, a.now()),

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

// adminInvalidatorOrNil does the same for the key controls, for the same reason:
// a typed nil behind admin.Invalidator would turn "no invalidation path is
// configured" from a skipped call into a panic on the incident route.
func adminInvalidatorOrNil(c *cluster.KeyControl) admin.Invalidator {
	if c == nil {
		return nil
	}
	return c
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
	//
	// This check reaches further than it used to, and the consequence is worth
	// stating rather than discovering. Now that a principal carries its owning
	// user and team, blocking a user disqualifies that user's administrative
	// keys too — which is right, and is the point. Authorize also asks about
	// BUDGET, and a team whose `teams.spend_nano` an operator has set above its
	// `max_budget_nano` would therefore lose its team-scoped administrators,
	// including their ability to raise the ceiling. Three things keep that from
	// being a trap: the column is written by imports and by `/team/update`, not
	// by the request path (the gate counts against `budget_state`); the failure
	// is fail-closed rather than fail-open; and the master credential is
	// out-of-band and authorizes unconditionally, so `DORANG_MASTER_KEY` is
	// always the way out. OPERATIONS §3.2 says so.
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

// ReplaceVerifier is `/key/regenerate`: a new secret with no grace at all.
//
// It is [store.Store.RotateKey] with a zero grace followed by
// [store.Store.EndGrace], and not a second write path of its own. An earlier
// version was one — a direct UPDATE of api_keys.lookup/token_hash/hash_scheme —
// and those columns are the DENORMALIZED copy: authentication resolves through
// api_key_secrets, so the leaked secret went on authenticating and the freshly
// minted one was unknown to the gateway. A regeneration that regenerates
// nothing is the worst shape an incident path can have.
//
// A grace window is right for a planned rotation and wrong for an incident, and
// this is the incident path — so the difference is the argument, not the
// function.
func (k *adminKeyStore) ReplaceVerifier(ctx context.Context, id string, v admin.Verifier,
	label string, now time.Time) error {

	if _, err := k.rotate(ctx, id, v, label, 0, now); err != nil {
		return err
	}
	if _, err := k.st.EndGrace(ctx, id); err != nil {
		return adminStoreError(err)
	}
	return nil
}

// Rotate implements admin.RotatingKeyStore: `/key/rotate`, DESIGN §11.2c.
//
// The old secret keeps authenticating for grace, which is the whole difference
// from ReplaceVerifier above, and everything that is not the verifier — tier,
// budget, spend, allow-list, rate limits, team, ledger history — belongs to the
// key id and is untouched.
func (k *adminKeyStore) Rotate(ctx context.Context, id string, v admin.Verifier,
	label string, grace time.Duration, now time.Time) (admin.RotationResult, error) {

	rot, err := k.rotate(ctx, id, v, label, grace, now)
	if err != nil {
		return admin.RotationResult{}, err
	}
	out := admin.RotationResult{PreviousExpiresAt: rot.PreviousExpiresAt}
	if rot.New != nil {
		out.New = adminSecretFrom(rot.New)
	}
	if rot.Previous != nil {
		out.Previous = adminSecretFrom(rot.Previous)
	}
	for _, s := range rot.Retired {
		out.Retired = append(out.Retired, adminSecretFrom(s))
	}
	return out, nil
}

func (k *adminKeyStore) rotate(ctx context.Context, id string, v admin.Verifier,
	label string, grace time.Duration, now time.Time) (store.Rotation, error) {

	rot, err := k.st.RotateKey(ctx, id, store.KeySecret{
		Lookup:     v.Lookup,
		TokenHash:  v.TokenHash,
		HashScheme: store.HashScheme(v.HashScheme),
		KeyLabel:   label,
		CreatedAt:  now,
	}, store.RotationPolicy{Grace: grace})
	if err != nil {
		return store.Rotation{}, adminStoreError(err)
	}
	return rot, nil
}

// EndGrace implements admin.RotatingKeyStore: cut every superseded secret now.
func (k *adminKeyStore) EndGrace(ctx context.Context, id string, _ time.Time) (int, error) {
	cut, err := k.st.EndGrace(ctx, id)
	if err != nil {
		return 0, adminStoreError(err)
	}
	return len(cut), nil
}

// ListSecrets implements admin.RotatingKeyStore. Retired secrets are included:
// an operator asking "did the client roll?" needs to see the one about to stop
// working.
func (k *adminKeyStore) ListSecrets(ctx context.Context, id string) ([]admin.KeySecret, error) {
	rows, err := k.st.ListKeySecrets(ctx, id)
	if err != nil {
		return nil, adminStoreError(err)
	}
	out := make([]admin.KeySecret, 0, len(rows))
	for _, s := range rows {
		out = append(out, adminSecretFrom(s))
	}
	return out, nil
}

// Pend implements admin.PendableKeyStore (§11.6): the reversible refusal.
func (k *adminKeyStore) Pend(ctx context.Context, id, reason string, _ time.Time) error {
	_, err := k.st.PendKey(ctx, id, reason)
	return adminStoreError(err)
}

// Release implements admin.PendableKeyStore: one action, as §11.6 requires.
func (k *adminKeyStore) Release(ctx context.Context, id string, _ time.Time) error {
	_, err := k.st.ReleaseKey(ctx, id)
	return adminStoreError(err)
}

// adminSecretFrom renders one stored secret for the operator surface. It
// deliberately carries neither the lookup nor the digest: nothing here may be
// turned back into the secret.
func adminSecretFrom(s *store.KeySecret) admin.KeySecret {
	if s == nil {
		return admin.KeySecret{}
	}
	return admin.KeySecret{
		ID:         s.ID,
		Generation: s.Generation,
		KeyLabel:   s.KeyLabel,
		Current:    s.Current,
		CreatedAt:  s.CreatedAt,
		ExpiresAt:  s.ExpiresAt,
		RevokedAt:  s.RevokedAt,
	}
}

// adminKeyFrom renders a stored key for the administration surface.
//
// Tier and the pend pair are copied across for the reason every other field is:
// both types have the column, and a field that exists on both sides and is
// dropped in the middle reports a fixed answer — `"tier": ""`, `"pended":
// false` — for every key on every deployment, which is the shape of defect this
// package keeps finding. §11.6's pend is the one that shows: an operator
// reading `/ui/keys` saw "active" beside a credential the gateway was refusing.
//
// The reverse direction ([storeKeyFrom]) deliberately does NOT carry them.
// `UpdateAPIKey` writes neither column — a pend is applied and released by its
// own store methods, which is what makes "released in ONE action" true — so
// carrying them into the update path would be a write nothing performs, wearing
// the appearance of one.
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
		Tier: k.Tier, PendedAt: k.PendedAt, PendReason: k.PendReason,
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
		page, err = l.st.ListRequestsByUser(ctx, q.UserID, r, p)
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

// Report aggregates one of DESIGN §9.4's three materializations over a bounded
// range.
//
// It answered 501 until now, and the reason it did was that no range query
// existed: internal/store had three single-bucket reads and nothing that could
// answer "a week, grouped by model". The counters were correct the whole time
// and unreadable through the administration surface, which is the worse half of
// the pair — an operator could see one hour of one key and could not see a week
// of anything.
//
// What it can group by is exactly what the rollups are keyed on, because §9.4
// keeps purpose-built materializations rather than a cube: day, and one of
// model, key or team. Anything else is refused BY NAME rather than answered
// with a table scan, which §9.3 refuses on purpose. Naming the gap is what lets
// an operator reach for /spend/logs instead of retrying the same query.
func (l *adminLedger) Report(ctx context.Context, q admin.ReportQuery) (admin.Report, error) {
	dim, byDay, byDim, err := rollupPlan(q.GroupBy)
	if err != nil {
		return admin.Report{}, err
	}
	rows, total, err := l.st.ReadRollupRange(ctx, store.RollupQuery{
		Dim:   dim,
		Range: store.TimeRange{Start: q.Range.Start, End: q.Range.End},
		ByDay: byDay, ByDim: byDim, Limit: q.Limit,
	})
	if err != nil {
		return admin.Report{}, adminStoreError(err)
	}
	out := admin.Report{Range: q.Range, Total: rollupUsage(total)}
	out.Rows = make([]admin.ReportRow, 0, len(rows))
	for _, r := range rows {
		row := admin.ReportRow{Day: r.Day, Usage: rollupUsage(r.Usage)}
		switch dim {
		case store.RollupKey:
			row.KeyID = r.Dim
		case store.RollupTeam:
			row.TeamID = r.Dim
		default:
			row.ModelGroup = r.Dim
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// adminSpendReporter answers "what has this key spent" from the per-key rollup
// of DESIGN §9.4.
//
// It is the producer `/key/info`'s `spend` never had. The field was rendered
// from `api_keys.spend_nano`, which is written by imports and by nothing on the
// request path, so **every key on every deployment reported `"spend": 0`** —
// while the same key's ledger rows, its own `x-dorang-spend-usd` header and
// `/global/spend/report?group_by=key` all agreed on a different number. A silent
// zero on the route a per-key spend dashboard asks first is worse than an error,
// because an error is noticed.
//
// # Why the rollup and not the durable budget counter
//
// `budget_state.spent_nano` is what enforcement reads, and it is deliberately
// not what a key has spent: a node charges a whole lease block to it BEFORE
// spending a unit of it (§9.6), so it runs up to one block ahead of reality
// while a block is live. Printing that next to `max_budget` would show an
// operator a number that jumps by the block size and settles back. The rollup
// is what agrees with the ledger row for row, which is why the report endpoint
// answers from it too.
//
// # The window
//
// A key's spend is measured over the period its ceiling is enforced over, which
// is the same window and the same period start the budget gate derives — an
// unparseable `budget_duration` falls back to monthly in both places, so the
// figure and the ceiling beside it cannot disagree about which month they mean.
type adminSpendReporter struct {
	st  *store.Store
	now func() time.Time
}

// KeySpend implements admin.SpendReporter.
//
// One query per distinct budget window, not one per key: a page of keys usually
// shares a window, and a query per row is how an operator list becomes a store
// outage at the page size operators actually use.
func (r *adminSpendReporter) KeySpend(ctx context.Context, keys []admin.KeySpendRef) (map[string]int64, error) {
	if r == nil || r.st == nil || len(keys) == 0 {
		return nil, nil
	}
	now := r.now()
	byWindow := make(map[quota.Window][]string, 2)
	for _, k := range keys {
		if k.ID == "" {
			continue
		}
		w, err := quota.ParseWindow(k.Period)
		if err != nil || !w.Valid() {
			// The same fallback budgetSubjectsOf takes. A period that does not
			// parse must not become "no window at all", or the spend beside a
			// ceiling would be measured over a window nothing enforces.
			w = quota.Monthly
		}
		byWindow[w] = append(byWindow[w], k.ID)
	}
	out := make(map[string]int64, len(keys))
	for w, ids := range byWindow {
		got, err := r.st.KeySpendRange(ctx, ids, store.TimeRange{
			Start: w.PeriodStart(now),
			End:   w.PeriodEnd(now),
		})
		if err != nil {
			return nil, adminStoreError(err)
		}
		for id, v := range got {
			out[id] = v
		}
	}
	return out, nil
}

// rollupPlan maps a requested grouping onto the materialization that carries it.
func rollupPlan(groups []admin.GroupBy) (dim store.RollupDim, byDay, byDim bool, err error) {
	// The default materialization for a time-only report is the per-model one:
	// every inference request carries a model group, while a request on a public
	// route carries no key and a key outside a team carries no team, so it is
	// the one whose coverage is not conditional on how a deployment is organized.
	dim = store.RollupModel
	seen := ""
	for _, g := range groups {
		switch g {
		case admin.GroupByDay:
			byDay = true
		case admin.GroupByModel, admin.GroupByKey, admin.GroupByTeam:
			if byDim {
				return 0, false, false, unsupportedLedger(
					"a report can be grouped by day and by ONE of model, key or team: " +
						seen + " and " + string(g) + " have separate rollups (§9.4 keeps " +
						"purpose-built materializations, not a cube), so /spend/logs is the " +
						"only place the two are joined")
			}
			byDim, seen = true, string(g)
			switch g {
			case admin.GroupByKey:
				dim = store.RollupKey
			case admin.GroupByTeam:
				dim = store.RollupTeam
			default:
				dim = store.RollupModel
			}
		default:
			return 0, false, false, unsupportedLedger(
				"there is no rollup keyed by " + string(g) + " in this build; a report can " +
					"group by day, model, key or team, and /spend/logs carries the rest per request")
		}
	}
	return dim, byDay, byDim, nil
}

// rollupUsage converts a rollup counter set into the administration surface's.
//
// The notional figure (§8.5) is carried across with the flag that says whether
// it means anything. It used to be hard-wired to `NotionalKnown: false`, with a
// comment that the rollups had no notional column — they have had one since
// migration 0002, and what was actually missing was a writer and a way to say
// "this sum is whole". Both exist now (see [store.UsageDelta.NotionalKnown]),
// and the consequence was not abstract: the usage screen's notional and
// leverage tiles read `unavailable` in every window of every deployment, which
// is honest and is furniture — a tile that can never be anything else.
//
// The cost DECOMPOSITION is read from its own columns. It used to be
// `MarginalCostNano: d.CostNano` — the whole total reported as marginal usage —
// which is the same conflation §8.1 forbids by name, one aggregate up from the
// ledger row where it was also happening. It matters here for a reason it does
// not matter on a single row: `/global/spend/report?group_by=model` is what an
// operator compares models by, and routing compares the marginal figure
// precisely so that a sunk plan cost cannot make a saturated plan look cheap.
// Rows written before the columns existed report 0 for both, which is honest —
// the split was never recorded and there is nothing to recover it from.
func rollupUsage(d store.UsageDelta) admin.Usage {
	return admin.Usage{
		Requests: d.Requests, Errors: d.Errors,
		PromptTokens: d.PromptTokens, CompletionTokens: d.CompletionTokens,
		CachedTokens: d.CachedTokens, ReasoningTokens: d.ReasoningTokens,
		TotalTokens: d.TotalTokens,
		CostNano:    d.CostNano, MarginalCostNano: d.MarginalNano,
		SubscriptionCostNano: d.SubscriptionNano,
		NotionalNano:         d.NotionalNano,
		NotionalKnown:        d.NotionalKnown(),
		LatencyMSSum:         d.LatencyMSSum,
	}
}

// unsupportedLedger builds the refusal for a ledger query this build has no
// index to answer.
//
// admin.Unsupported carries the sentinel AND the message. It used to paste the
// sentinel in as text —
// `errors.New("app: " + msg + " (" + admin.ErrUnsupported.Error() + ")")` —
// which reads identically in a log and is invisible to errors.Is. The 501 case
// in admin.faultFor therefore never matched, an unfiltered /spend/logs answered
// `500 internal_error`, and msg — the only part of the refusal a caller can act
// on — went with it. A 500 says "gateway fault, retry"; the truth was "add a
// query parameter", and this says which.
func unsupportedLedger(msg string) error {
	return admin.Unsupported("%s", msg)
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
		// The list-rate equivalent and whether the row has one. The flag is not
		// decoration: reporting a missing notional as zero is the one thing
		// DESIGN §8.5 says not to do, because it makes a subscription look
		// infinitely efficient.
		NotionalNano:  r.NotionalNano,
		NotionalKnown: r.NotionalKnown,
		// The §8.6 disclosure: a variable price whose factor the ledger does
		// not carry is an invoice nobody can dispute.
		UtilMultiplierPPM: r.UtilMultiplierPPM,
		UtilPPM:           r.UtilPPM,
		UtilSource:        r.UtilSource,
		LatencyMS:         r.LatencyMS, TTFTMS: r.TTFTMS, QueueMS: r.QueueMS,
		CapacityWaitMS: r.CapacityWaitMS, UpstreamMS: r.UpstreamMS,
		FallbackCount: r.FallbackCount, Streamed: r.Streamed,
		TraceID: r.TraceID, SessionID: r.SessionID, NodeID: r.NodeID,
		BatchID: r.BatchID, Tags: r.Tags,
	}
}

// ---------------------------------------------------------------------------
// The routing table, read-only
// ---------------------------------------------------------------------------

// adminRouting answers "what does this gateway route" from the configuration it
// compiled, for the operator UI's models screen.
//
// # Why this exists when /model/* is still a 501
//
// The reasoning above stands and is unchanged: `deployments` and
// `model_aliases` have a schema and no reader, so an adapter over them would
// let an operator POST /model/new, get a 200, see the row, and route nothing
// differently. That argument is about WRITES.
//
// A read-only screen is a different question, and it had a worse answer: the
// navigation advertised "Models & deployments" on all three pages and every
// click answered 501, in every deployment, because [admin.Config.Models] was
// never set. dorang knows its model set perfectly well — it serves it on
// `/v1/models`, routes on it, and stamps these very deployment ids into
// `x-dorang-deployment` and the ledger. Refusing to show it was not caution; it
// was a link that could not work.
//
// So the screen is served from here, `/model/*` keeps answering 501 with the
// message that names the gap, and the page says which of the two it is looking
// at. See [admin.Config.Routing].
//
// # Why the configuration and not the router
//
// The same reason [adminPricer] reads the catalog off the dispatcher's state:
// the configuration is swapped by pointer on SIGHUP, and a captured copy would
// keep describing the process's start-up state — while "did my reload take
// effect" is the main reason an operator opens this screen at all. The ids come
// from [deploymentIDs], the function the router itself compiles with.
type adminRouting struct{ cfg func() *config.Config }

// ListDeployments implements admin.ModelCatalog.
func (r *adminRouting) ListDeployments(context.Context) ([]*admin.Deployment, error) {
	cfg := r.cfg()
	if cfg == nil {
		return nil, nil
	}
	ids := deploymentIDs(cfg)
	// Occurrence mirrors deploymentIDs' #n disambiguation, so the number the
	// enable/disable control sends back names the same deployment the id does.
	seen := make(map[string]int)
	out := make([]*admin.Deployment, 0, len(cfg.Models))
	for i := range cfg.Models {
		m := &cfg.Models[i]
		for j := range m.Deployments {
			d := &m.Deployments[j]
			base := m.Name + "|" + d.Provider + "|" + d.UpstreamModel
			occurrence := seen[base]
			seen[base]++
			dep := &admin.Deployment{
				ID:            ids[i][j],
				ModelGroup:    m.Name,
				ProviderID:    d.Provider,
				UpstreamModel: d.UpstreamModel,
				Occurrence:    occurrence,
				CredentialIDs: append([]string(nil), d.Credentials...),
				// Zero weight is one to the router, so it is one here. A column
				// reading 0 beside a deployment that takes its full share of
				// round-robin traffic describes the file, not the behaviour.
				Weight:   max(d.Weight, 1),
				Priority: d.Priority,
				Params:   "{}",
				// Reflect the config flag: nil means enabled (the default every
				// prior config means), so this reads false only where an operator
				// wrote enabled:false. buildRouter skips those from routing; the
				// models screen shows them as disabled rather than hiding them,
				// which is what lets an operator re-enable one.
				Enabled: d.Enabled == nil || *d.Enabled,
			}
			timeout := d.Timeout.Duration()
			if p, ok := cfg.Provider(d.Provider); ok && timeout == 0 {
				// The effective timeout, resolved exactly as buildRouter
				// resolves it. Showing the deployment's own empty value would
				// report "no timeout" for a deployment that has its provider's.
				timeout = p.Timeout.Duration()
			}
			dep.TimeoutMS = msPtr(timeout)
			dep.StreamTimeoutMS = msPtr(d.StreamTimeout.Duration())
			for _, lim := range d.Limits {
				switch lim.Metric {
				case config.MetricRPM:
					dep.RPMLimit = limitPtr(lim.Value)
				case config.MetricTPM:
					dep.TPMLimit = limitPtr(lim.Value)
				}
			}
			// MaxParallel is deliberately left unset. A deployment-level
			// `max_concurrent` is not read by anything — concurrency ceilings
			// come from `capacity:` and are keyed on the credential, the
			// provider group and the route — and rendering one would put a
			// limit on the screen that nothing enforces, which is the defect
			// this screen was opened to remove rather than to relocate.
			out = append(out, dep)
		}
	}
	return out, nil
}

// ListAliases implements admin.ModelCatalog.
func (r *adminRouting) ListAliases(context.Context) ([]admin.Alias, error) {
	cfg := r.cfg()
	if cfg == nil {
		return nil, nil
	}
	out := make([]admin.Alias, 0, len(cfg.Aliases))
	for alias, group := range cfg.Aliases {
		out = append(out, admin.Alias{Alias: alias, ModelGroup: group})
	}
	return out, nil
}

func msPtr(d time.Duration) *int64 {
	if d <= 0 {
		return nil
	}
	v := d.Milliseconds()
	return &v
}

// limitPtr renders a declared ceiling, and an undeclared one as "no limit"
// rather than as a limit of zero — the two are different answers and flattening
// them is how a configured ceiling quietly stops existing.
func limitPtr(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}

// ---------------------------------------------------------------------------
// Read-only reporting
// ---------------------------------------------------------------------------

// adminSurface reports this process's live HTTP counters — the same set GET
// /metrics exposes — into the monitoring screen's real-time pulse.
//
// It reads a.Server at call time rather than at construction on purpose:
// buildAdmin runs BEFORE the server exists, because the server is built with
// the admin routes buildAdmin returns (app.go wires a.Admin, then a.Server). An
// eager read would capture a nil forever; a per-call read sees the server as
// soon as start-up has set it, and answers "not ready yet" until then.
type adminSurface struct{ a *App }

func (s *adminSurface) Surface(context.Context) (admin.Surface, error) {
	srv := s.a.Server
	if srv == nil {
		// The window between buildAdmin and a.Server being set. The screen
		// renders the section's "could not be read" line, which is the honest
		// answer for a process that has not finished starting.
		return admin.Surface{}, admin.ErrUnsupported
	}
	st := srv.Stats()
	id := ""
	if s.a.Node != nil {
		id = s.a.Node.ID()
	}
	out := admin.Surface{
		NodeID:        id,
		Requests:      int64(st.Requests),
		Class2xx:      int64(st.ByClass[2]),
		Class3xx:      int64(st.ByClass[3]),
		Class4xx:      int64(st.ByClass[4]),
		Class5xx:      int64(st.ByClass[5]),
		InFlight:      st.InFlight,
		BytesIn:       int64(st.BytesIn),
		BytesOut:      int64(st.BytesOut),
		UptimeSeconds: int64(st.Uptime.Seconds()),
		Ready:         st.Ready,
		MetricsPath:   "/metrics",
	}
	if st.Requests > 0 {
		out.AvgLatencyMS = int64(st.DurationSumNS / st.Requests / 1_000_000)
	}
	return out, nil
}

// SetConfigControl gives the admin surface the config path and an immediate
// reload trigger, wired after the file watcher that owns the reload is built
// (the watcher is created with the app it reloads, so it does not exist at New
// time). Before this is called, the reloader and config writer answer
// "not ready" — the same late-bind adminSurface uses for the server.
func (a *App) SetConfigControl(path string, reload func() error) {
	a.configPath = path
	a.reloadNow = reload
}

// adminReloader re-reads the config file on demand for /admin/config/reload,
// through the same watcher path a SIGHUP or a file change takes.
type adminReloader struct{ a *App }

func (r *adminReloader) Reload(context.Context) (admin.ReloadResult, error) {
	if r.a.reloadNow == nil {
		return admin.ReloadResult{}, admin.ErrUnsupported
	}
	if err := r.a.reloadNow(); err != nil {
		return admin.ReloadResult{}, err
	}
	return admin.ReloadResult{LoadedAt: r.a.now()}, nil
}

// adminConfigWriter takes a deployment in or out of routing by editing the
// config file and re-reading it. It validates the candidate before it touches
// the live file, and writes in place because the config is a single-file bind
// mount — see [writeConfigInPlace].
type adminConfigWriter struct{ a *App }

func (w *adminConfigWriter) SetDeploymentEnabled(_ context.Context, group, provider, upstream string, occurrence int, enabled bool) error {
	path := w.a.configPath
	if path == "" || w.a.reloadNow == nil {
		return admin.ErrUnsupported
	}
	// One writer at a time on this node, and the lock is held across the apply
	// so the writer does not race its own reload. Cross-node writers are
	// serialized by the exclusive file lock inside EditLocked; this mutex adds
	// same-process ordering the file lock alone would not give.
	w.a.configWriteMu.Lock()
	defer w.a.configWriteMu.Unlock()

	// The whole read-edit-validate-write runs under an exclusive lock on the
	// shared config inode, so a concurrent edit on the other node cannot lose
	// this one and no reader sees a partial file. The candidate is validated
	// inside the transaction, before any byte is written: a file that would not
	// load is never written, or the next start-up could not come up.
	err := config.EditLocked(path, func(cur []byte) ([]byte, error) {
		next, err := config.SetDeploymentEnabled(cur, group, provider, upstream, occurrence, enabled)
		if err != nil {
			return nil, err
		}
		if _, err := config.LoadBytes(next); err != nil {
			return nil, fmt.Errorf("the edited config would not load, so nothing was written: %w", err)
		}
		return next, nil
	})
	if err != nil {
		return err
	}
	// Apply on this node at once and surface a refusal: reloadNow returns the
	// error if the process declines the new file, so the operator learns the
	// file changed but routing did not, rather than reading a false success.
	// The other nodes' watchers see the change within their poll interval.
	return w.a.reloadNow()
}

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
// Pricing
// ---------------------------------------------------------------------------

// adminPricer is §8.4's "one engine, one answer" for the two HTTP surfaces that
// were missing it.
//
// `POST /admin/pricing/preview` and `POST /spend/calculate` have a complete
// implementation in internal/admin behind a `Pricing` dependency this package
// never set, so both answered `501 dependency_unavailable` in every deployment
// — while `dorangctl price` and the request path priced through the same engine
// perfectly well. §8.4 names the two endpoints; there was one answer and two of
// the three surfaces could not reach it.
//
// It reads the catalog off [dispatcher.state] rather than capturing one, for
// the reason [App.Reload] gives: the price catalog is immutable by construction
// and swapped by pointer on SIGHUP, so a captured one would keep quoting the
// configuration the process started with. That is not a cosmetic staleness —
// picking up an edited `pricing.catalog` is the main reason an operator sends a
// SIGHUP at all, and the preview is where they would go to check that it took.
type adminPricer struct{ d *dispatcher }

// errNoPriceCatalog is a running process with no loaded price catalog, which is
// a fault rather than a configuration.
var errNoPriceCatalog = errors.New("app: the price catalog is not loaded")

func (p *adminPricer) Explain(_ context.Context, req admin.PriceRequest) (admin.PriceExplanation, error) {
	if p.d == nil {
		return admin.PriceExplanation{}, errNoPriceCatalog
	}
	st := p.d.state()
	if st == nil || st.pricing == nil {
		// Unreachable rather than merely unlikely: [New] swaps a dispatch state
		// carrying a non-nil catalog before it builds this surface, and
		// [App.Reload] only ever swaps in another one. It is a fault and is
		// reported as one — answering "no pricing engine" would say the
		// dependency is absent by configuration, which is the thing that was
		// wrong here for every deployment and must not be reintroduced as a
		// fallback.
		return admin.PriceExplanation{}, errNoPriceCatalog
	}
	x := st.pricing.Explain(pricing.Request{
		Provider:         req.Provider,
		Model:            req.Model,
		Credential:       req.Credential,
		Deployment:       req.Deployment,
		InputTokens:      req.InputTokens,
		OutputTokens:     req.OutputTokens,
		CacheReadTokens:  req.CacheReadTokens,
		CacheWriteTokens: req.CacheWriteTokens,
		ReasoningTokens:  req.ReasoningTokens,
		Requests:         req.Requests,
		Characters:       req.Characters,
		// admin.PriceRequest has one `seconds`, and internal/pricing has two.
		// It maps to compute seconds — the request's own wall time — and NOT to
		// audio seconds, because the two are different billable quantities and
		// substituting one for the other is how a ten-minute recording was
		// billed as the eight seconds the transcription took (§10.7). A
		// per_audio_second rule therefore reports `no_price` here rather than
		// being quoted from the wrong quantity, which is the honest answer for
		// a figure the caller did not supply.
		Seconds: req.Seconds,
		At:      req.At,
	})
	if x.Err != "" {
		return admin.PriceExplanation{}, errors.New(x.Err)
	}
	return viewExplanation(x), nil
}

func viewExplanation(x pricing.Explanation) admin.PriceExplanation {
	out := admin.PriceExplanation{
		Currency:         x.Currency,
		MarginalNano:     x.Cost.MarginalNano,
		SubscriptionNano: x.Cost.SubscriptionNano,
		AdjustmentNano:   x.Cost.AdjustmentNano,
		TotalNano:        x.Cost.TotalNano,
		Components:       viewComponents(x.Cost.Components),
		Missing:          x.Cost.Missing,
		Notes:            x.Notes,
		Notional: admin.PriceNotional{
			RuleID:     x.Notional.RuleID,
			Source:     x.Notional.Source,
			AsOf:       x.Notional.AsOfText,
			AgeSeconds: int64(x.Notional.Age / time.Second),
			Nano:       x.Notional.Nano,
			Components: viewComponents(x.Notional.Components),
			Missing:    x.Notional.Missing,
		},
	}
	for _, a := range x.Cost.AppliedRules {
		out.Applied = append(out.Applied, admin.PriceRule{
			RuleID: a.RuleID, Class: a.Class.String(), Level: a.Level.String(),
			Priority: a.Priority, Order: a.Order, Why: a.Why(),
		})
	}
	for _, ct := range x.Classes {
		t := admin.PriceClassTrace{Class: ct.Class.String()}
		for _, c := range ct.Considered {
			t.Considered = append(t.Considered, admin.PriceConsidered{
				RuleID: c.RuleID, Level: c.Level.String(), Priority: c.Priority,
				Order: c.Order, Eligible: c.Eligible, Selected: c.Selected,
				Reason: c.Reason,
			})
		}
		out.Classes = append(out.Classes, t)
	}
	return out
}

func viewComponents(cs []pricing.Component) []admin.PriceComponent {
	if len(cs) == 0 {
		return nil
	}
	out := make([]admin.PriceComponent, 0, len(cs))
	for _, c := range cs {
		out = append(out, admin.PriceComponent{
			Name: c.Name, RuleID: c.RuleID, Rate: c.Rate, Unit: c.Unit.String(),
			Quantity: c.Quantity, Scale: c.Scale, SubtotalNano: c.SubtotalNano,
		})
	}
	return out
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

func (*adminLedger) ReportGroups() []string { return []string{"model", "key", "team", "day"} }
