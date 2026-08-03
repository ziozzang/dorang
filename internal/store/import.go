package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultImportTable is the incumbent proxy's credential table.
const DefaultImportTable = "LiteLLM_VerificationToken"

// DefaultObjectPermissionTable is the incumbent's out-of-line permission table.
//
// A key's model restriction may live in the key's own `models` column or in a
// row of this table referenced by `object_permission_id`. The second is the one
// that fails OPEN if it is carried and not resolved: dorang reads the key's own
// `models` as empty, and an empty allow-list allows every model.
const DefaultObjectPermissionTable = "LiteLLM_ObjectPermissionTable"

// MissingTeamPolicy decides what happens to a key whose owning team is not
// present in the destination.
type MissingTeamPolicy string

// Missing-team policies.
const (
	// MissingTeamSkip refuses the key and reports it. This is the default,
	// because a key whose team limits cannot be resolved runs with no team
	// budget, no team rate limit and no team blocked flag -- it fails OPEN,
	// which is the failure mode DESIGN 2.4 is written against.
	MissingTeamSkip MissingTeamPolicy = "skip"
	// MissingTeamOrphan imports the key with its team reference dropped. It is
	// available for operators who are importing teams in a later pass and
	// accept the window; it is never the default.
	MissingTeamOrphan MissingTeamPolicy = "orphan"
)

// UntranslatablePolicy decides what happens to a key whose source allow-list
// holds an idiom dorang cannot express as a literal.
//
// It governs the incumbent's GROUP spellings only -- `allowed_routes:
// {llm_api_routes}`, `models: {all-team-models}` -- and never an unresolved
// `object_permission_id`. The distinction is the direction of failure: a group
// name carried verbatim matches nothing and refuses the key every route or
// every model, which is loud and safe; an unresolved object permission leaves
// the allow-list EMPTY, and empty allows everything. A fail-open row is not an
// operator's choice to make from a flag, so it is always refused.
type UntranslatablePolicy string

// Untranslatable policies.
const (
	// UntranslatableSkip refuses the key and names the idiom. This is the
	// default: the operator learns at import time, from the report, rather
	// than when a consumer is refused every route in production.
	UntranslatableSkip UntranslatablePolicy = "skip"
	// UntranslatableClear imports the key with the allow-list dorang could
	// not express CLEARED, which at key level means unrestricted. It is a
	// widening and it is therefore opt-in, named per key in the report. It
	// is the right answer when the incumbent's group was the whole of the
	// restriction -- `llm_api_routes` on a key that is not administrative
	// grants exactly what dorang's data plane serves -- and the wrong answer
	// otherwise, which is why the report names every key it touched.
	UntranslatableClear UntranslatablePolicy = "clear"
)

// ImportOptions configures ImportKeys.
type ImportOptions struct {
	// Table is the source table name. Zero value: DefaultImportTable.
	Table string

	// Now is the instant expiry is judged against. Zero means the store clock.
	Now time.Time

	// OnMissingTeam defaults to MissingTeamSkip.
	OnMissingTeam MissingTeamPolicy

	// OnUntranslatable defaults to UntranslatableSkip.
	OnUntranslatable UntranslatablePolicy

	// ObjectPermissionTable is the source table an `object_permission_id`
	// points into. Zero value: DefaultObjectPermissionTable.
	ObjectPermissionTable string

	// ExpectAdminKey should be set when the caller believes the administrative
	// credential is among the rows. It never is, and the report says so
	// loudly: a gateway that only reads an imported database loses admin auth
	// entirely (DESIGN 2.4).
	ExpectAdminKey bool

	// DryRun reads and reports without writing.
	DryRun bool

	// Limit caps the number of source rows read. Zero means all.
	Limit int

	// Source is the value written to api_keys.source. Zero value: "litellm".
	Source string
}

// NotMigrated is one source row that did not become an api_keys row.
type NotMigrated struct {
	// Lookup is the derived index key, which identifies the row without
	// revealing anything about the secret. Empty when the token was unusable.
	Lookup string
	Reason string
	Detail string
}

// DroppedColumn is a source column that exists and was deliberately not copied.
type DroppedColumn struct {
	Column string
	Reason string
}

// Untranslated is one authorization idiom the source expressed and dorang
// could not, named per key so that the report says what the rows cannot.
//
// The import's arithmetic was never the thing that failed here. Every row and
// every column was accounted for; what was silent was MEANING -- a restriction
// that survived the copy as data and stopped restricting anything. So each of
// these carries the column it came from, the value dorang could not honour, and
// what was done about it.
type Untranslated struct {
	// Lookup identifies the key without revealing the secret.
	Lookup string
	// Column is the source column: models, allowed_routes, object_permission_id.
	Column string
	// Value is the idiom itself -- the sentinel, the group name, or the
	// permission id that could not be resolved.
	Value string
	// Action is "refused" (the key was not imported) or "cleared" (the key
	// was imported with this allow-list dropped, which is a widening).
	Action string
	Detail string
}

// Untranslated actions.
const (
	actionRefused = "refused"
	actionCleared = "cleared"
)

// ImportReport is the full account of an import. Nothing is dropped silently:
// every row that did not migrate appears in NotMigrated with a reason, and
// every source column that was not carried appears in DroppedColumns.
type ImportReport struct {
	Table   string
	DryRun  bool
	Scanned int

	// Imported counts rows written (or that would have been written).
	Imported int

	// Expired counts rows imported AS EXPIRED. They are imported, never
	// resurrected: a verification against a real deployment found the large
	// majority of stored credentials already expired, and importing them as
	// live would silently restore revoked access (DESIGN 2.4).
	Expired int

	// Blocked counts rows imported with the blocked flag carried.
	Blocked int

	// MissingTeam counts rows referencing a team that is not present in the
	// destination.
	MissingTeam int

	// MissingUser counts rows referencing a user that is not present.
	MissingUser int

	// AlreadyPresent counts rows whose lookup already existed. They are left
	// untouched rather than overwritten -- re-importing must not undo an
	// administrator's later revocation.
	AlreadyPresent int

	// Skipped counts rows not written for any reason. len(NotMigrated) equals it.
	Skipped int

	// ObjectPermissions counts rows whose model restriction lived in the
	// incumbent's out-of-line permission table and was RESOLVED into the
	// key's own allow-list. A row counted here arrives restricted.
	ObjectPermissions int

	NotMigrated    []NotMigrated
	DroppedColumns []DroppedColumn

	// Untranslated names every authorization idiom that did not survive the
	// crossing with its meaning intact.
	Untranslated []Untranslated

	Warnings []string
}

func (r *ImportReport) skip(lookup, reason, detail string) {
	r.Skipped++
	r.NotMigrated = append(r.NotMigrated, NotMigrated{Lookup: lookup, Reason: reason, Detail: detail})
}

// untranslated records an idiom dorang could not express. It returns false when
// the key must not be imported, so call sites read as `if !rep.untranslated(...)`.
func (r *ImportReport) untranslated(lookup, column, value, action, detail string) bool {
	r.Untranslated = append(r.Untranslated, Untranslated{
		Lookup: lookup, Column: column, Value: value, Action: action, Detail: detail,
	})
	if action == actionRefused {
		r.skip(lookup, "untranslatable "+column, fmt.Sprintf("%q: %s", value, detail))
		return false
	}
	return true
}

func (r *ImportReport) warn(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	for _, w := range r.Warnings {
		if w == msg {
			return
		}
	}
	r.Warnings = append(r.Warnings, msg)
}

// Summary renders the report as one human-readable block, for
// `dorangctl import keys`.
func (r ImportReport) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "import from %s: scanned %d, imported %d (%d expired, %d blocked), skipped %d\n",
		r.Table, r.Scanned, r.Imported, r.Expired, r.Blocked, r.Skipped)
	if r.DryRun {
		b.WriteString("  (dry run: nothing was written)\n")
	}
	if r.AlreadyPresent > 0 {
		fmt.Fprintf(&b, "  already present, left untouched: %d\n", r.AlreadyPresent)
	}
	if r.MissingTeam > 0 {
		fmt.Fprintf(&b, "  referenced a missing team: %d\n", r.MissingTeam)
	}
	if r.MissingUser > 0 {
		fmt.Fprintf(&b, "  referenced a missing user: %d\n", r.MissingUser)
	}
	if r.ObjectPermissions > 0 {
		fmt.Fprintf(&b, "  object permissions resolved into the key's own model allow-list: %d\n",
			r.ObjectPermissions)
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

// ---------------------------------------------------------------------------
// Source column handling
// ---------------------------------------------------------------------------

// importedColumns are the source columns dorang carries. DESIGN 2.4:
// authorization fields must be carried or the gateway fails open.
var importedColumns = map[string]string{
	"token":                 "token_hash / lookup",
	"key_alias":             "key_alias",
	"expires":               "expires_at",
	"blocked":               "blocked",
	"models":                "models (allow-list)",
	"allowed_routes":        "allowed_routes (allow-list)",
	"max_budget":            "max_budget_nano",
	"soft_budget":           "soft_budget_nano",
	"spend":                 "spend_nano",
	"budget_duration":       "budget_period",
	"budget_reset_at":       "budget_reset_at",
	"rpm_limit":             "rpm_limit",
	"tpm_limit":             "tpm_limit",
	"max_parallel_requests": "max_parallel",
	"team_id":               "team_id",
	"user_id":               "user_id",
	"object_permission_id":  "object_permission_id",
	"tags":                  "tags",
	"created_at":            "created_at",
	"updated_at":            "updated_at",
}

// refusedColumns are source columns dorang refuses to copy, with the reason.
// key_name is the important one: the incumbent stores the trailing characters
// of the secret there for display. dorang derives a non-reversible label from
// the digest instead (DESIGN 2.4), so this column is not selected, not read,
// and not stored.
var refusedColumns = map[string]string{
	"key_name": "stores trailing characters of the secret; dorang derives a non-reversible label instead",
}

// ---------------------------------------------------------------------------
// Authorization idioms that do not mean the same thing on the other side
//
// dorang's allow-lists are literal: a model name, or a path, or "*", or a path
// prefix ending "/*" (internal/auth.allowedIn). The incumbent's are not. It
// ships GROUP names that it expands internally, and it keeps some restrictions
// in a different table entirely. Copying either one verbatim produces a row
// that is byte-for-byte faithful and means something else -- and, in one
// direction, means nothing at all.
//
// Measured against a live incumbent with 54 keys: three of the four importable
// keys carried `allowed_routes: {llm_api_routes}` and were refused every route
// with 403 route_not_allowed; two carried `models: {all-team-models}` and were
// refused every model; one carried an `object_permission_id`, whose row
// happened to hold an empty `models`. The other row in that same permission
// table restricted to a single model, and a key pointing at it would have been
// imported with access to all 39.
// ---------------------------------------------------------------------------

// litellmModelSentinels are the incumbent's model-list sentinels: entries that
// are not model names but instructions about the set of model names.
var litellmModelSentinels = map[string]string{
	"all-proxy-models":  "every model configured on the proxy",
	"all-team-models":   "every model the owning team may reach",
	"all-model-access":  "every model configured on the proxy",
	"no-default-models": "no model at all unless one is granted explicitly",
}

// litellmWideningModelSentinels are the subset that ADD to the set. A union
// containing "everything" is everything, so when one of these appears the whole
// key-level list is the thing that has to go, not the entry: dropping the
// sentinel and keeping the literals beside it would leave the key NARROWER than
// the incumbent had it, which is a different wrong answer and a quieter one.
//
// `no-default-models` is deliberately not here. It restricts, so clearing it
// would widen, and a widening is never applied to an idiom whose whole purpose
// was to deny.
var litellmWideningModelSentinels = map[string]bool{
	"all-proxy-models": true,
	"all-team-models":  true,
	"all-model-access": true,
}

// isRouteGroup reports whether an `allowed_routes` entry is one of the
// incumbent's route GROUPS rather than a path dorang can match.
//
// The test is structural rather than a list of known names, because the list is
// the incumbent's and it grows: dorang matches a path, a path prefix ending
// "/*", or "*", and every one of those starts with '/' or is exactly "*".
// Anything else -- `llm_api_routes`, `openai_routes`, `info_routes`, a group an
// operator defined themselves -- is a name for a set dorang cannot enumerate.
// Refusing by shape means a group dorang has never heard of is refused too,
// which is the direction that cannot fail open.
func isRouteGroup(entry string) bool {
	e := strings.TrimSpace(entry)
	return e != "" && e != "*" && !strings.HasPrefix(e, "/")
}

// translateAllowLists reconciles the incumbent's allow-list idioms with
// dorang's, on the key that has already been built. It reports false when the
// key must not be imported.
func translateAllowLists(k *APIKey, policy UntranslatablePolicy, rep *ImportReport) bool {
	// models
	var widening, denying []string
	for _, m := range k.Models {
		lc := strings.ToLower(strings.TrimSpace(m))
		if _, isSentinel := litellmModelSentinels[lc]; !isSentinel {
			continue
		}
		if litellmWideningModelSentinels[lc] {
			widening = append(widening, lc)
		} else {
			denying = append(denying, lc)
		}
	}
	for _, s := range denying {
		// Not clearable in either policy: it exists to deny, an empty
		// dorang list allows everything, and dorang has no spelling for
		// "nothing" that an administrator would not mistake for a typo.
		if !rep.untranslated(k.Lookup, "models", s, actionRefused,
			"the incumbent reads this as "+litellmModelSentinels[s]+
				"; dorang's empty model allow-list means the opposite, and it has no "+
				"spelling for a key that may reach no model. Block the key instead") {
			return false
		}
	}
	for _, s := range widening {
		detail := "the incumbent reads this as " + litellmModelSentinels[s] +
			"; dorang reads it as the literal name of a model, so the key would be refused " +
			"every model. Clear the key's model allow-list to defer to its team, or list the " +
			"models by name"
		if policy != UntranslatableClear {
			if !rep.untranslated(k.Lookup, "models", s, actionRefused, detail) {
				return false
			}
			continue
		}
		if !rep.untranslated(k.Lookup, "models", s, actionCleared, detail+
			" (cleared: the key is unrestricted at key level and its team's limits still apply)") {
			return false
		}
		k.Models = nil
	}

	// allowed_routes
	for _, r := range k.AllowedRoutes {
		if !isRouteGroup(r) {
			continue
		}
		detail := "the incumbent expands this route GROUP into a set of paths; dorang matches " +
			"literal paths, so the entry matches no request and the key is refused every " +
			"route -- including /v1/models. List the paths, or clear the allow-list"
		if policy != UntranslatableClear {
			if !rep.untranslated(k.Lookup, "allowed_routes", r, actionRefused, detail) {
				return false
			}
			continue
		}
		if !rep.untranslated(k.Lookup, "allowed_routes", r, actionCleared, detail+
			" (cleared: the key may reach every route dorang serves it)") {
			return false
		}
		k.AllowedRoutes = nil
	}
	return true
}

// ---------------------------------------------------------------------------
// The out-of-line permission table
// ---------------------------------------------------------------------------

// objectPermission is one resolved row of the incumbent's permission table.
type objectPermission struct {
	// models is the restriction dorang can express.
	models []string
	// beyond names restriction columns dorang cannot express and that were
	// not empty on this row. A key pointing at such a row is refused: the
	// alternative is to enforce part of a restriction and file the rest as
	// unrestricted, which is the shape of every fail-open in this file.
	beyond []string
}

// objectPermissionMeta are the columns of the permission table that are not
// restrictions. Everything else that carries a value is treated as one, so a
// column this code has never heard of refuses the key rather than being
// silently ignored -- the failure that produced this function.
var objectPermissionMeta = map[string]bool{
	"id": true, "object_permission_id": true, "models": true,
	"created_at": true, "updated_at": true, "created_by": true, "updated_by": true,
}

// objectPermissions resolves `object_permission_id` against the source.
//
// It loads the whole table once, on first need: the table has a row per
// distinct permission set, not per key, and an import that touches it at all
// touches it for most of its rows.
type objectPermissions struct {
	table  string
	rows   map[string]objectPermission
	err    error
	loaded bool
}

func (o *objectPermissions) load(ctx context.Context, src *sql.DB) error {
	if o.loaded {
		return o.err
	}
	o.loaded = true
	o.rows = map[string]objectPermission{}

	present, err := sourceColumns(ctx, src, o.table)
	if err != nil {
		o.err = fmt.Errorf("store: %s: %w", o.table, err)
		return o.err
	}
	cols := make([]string, 0, len(present))
	var idCol string
	for c := range present {
		switch strings.ToLower(c) {
		case "object_permission_id":
			idCol = c
		case "id":
			if idCol == "" {
				idCol = c
			}
		}
		cols = append(cols, c)
	}
	if idCol == "" {
		o.err = fmt.Errorf("store: %s has no object_permission_id column", o.table)
		return o.err
	}
	sort.Strings(cols)
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteIdent(c)
	}
	rows, err := src.QueryContext(ctx,
		"SELECT "+strings.Join(quoted, ", ")+" FROM "+quoteIdent(o.table))
	if err != nil {
		o.err = fmt.Errorf("store: read %s: %w", o.table, err)
		return o.err
	}
	defer rows.Close()

	names, err := rows.Columns()
	if err != nil {
		o.err = err
		return o.err
	}
	for rows.Next() {
		raw := make([]any, len(names))
		ptrs := make([]any, len(names))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			o.err = err
			return o.err
		}
		var (
			id  string
			p   objectPermission
			rec = map[string]any{}
		)
		for i, n := range names {
			rec[strings.ToLower(n)] = raw[i]
		}
		id = asString(rec[strings.ToLower(idCol)])
		if id == "" {
			continue
		}
		p.models = asList(rec["models"])
		for n, v := range rec {
			if objectPermissionMeta[n] || len(asList(v)) == 0 {
				continue
			}
			p.beyond = append(p.beyond, n)
		}
		sort.Strings(p.beyond)
		o.rows[id] = p
	}
	o.err = rows.Err()
	return o.err
}

// resolve folds the row's restriction into the key's own model allow-list, and
// reports false when the key must not be imported.
//
// Never widening, in any branch. The intersection is taken rather than the
// union because the two lists are two restrictions on one key, and a key that
// is refused by either one is refused.
func (o *objectPermissions) resolve(ctx context.Context, src *sql.DB, k *APIKey, rep *ImportReport) bool {
	id := k.ObjectPermissionID
	if id == "" {
		return true
	}
	if err := o.load(ctx, src); err != nil {
		return rep.untranslated(k.Lookup, "object_permission_id", id, actionRefused,
			"the key's model restriction lives in "+o.table+", which could not be read ("+
				err.Error()+"); importing it would leave the key with an EMPTY model "+
				"allow-list, and empty allows every model")
	}
	p, ok := o.rows[id]
	if !ok {
		return rep.untranslated(k.Lookup, "object_permission_id", id, actionRefused,
			"no such row in "+o.table+"; dorang cannot tell a permission set that restricts "+
				"nothing from one that was deleted, and an unresolved id imports as an EMPTY "+
				"model allow-list, which allows every model")
	}
	if len(p.beyond) > 0 {
		return rep.untranslated(k.Lookup, "object_permission_id", id, actionRefused,
			"the permission row also restricts "+strings.Join(p.beyond, ", ")+
				", which dorang has no allow-list for; enforcing the model half and filing the "+
				"rest as unrestricted is the fail-open this resolution exists to close")
	}
	if len(p.models) == 0 {
		// The row restricts nothing dorang can express and nothing it
		// cannot. The key keeps whatever its own column said.
		return true
	}
	for _, m := range p.models {
		if _, isSentinel := litellmModelSentinels[strings.ToLower(strings.TrimSpace(m))]; isSentinel {
			// Resolved to another idiom. Refused by the same rule and for
			// the same reason as one on the key itself.
			return rep.untranslated(k.Lookup, "object_permission_id", id, actionRefused,
				"the permission row's models list holds the sentinel "+strconv.Quote(m)+
					", which dorang reads as the literal name of a model")
		}
	}
	merged := intersectModels(k.Models, p.models)
	if len(merged) == 0 {
		return rep.untranslated(k.Lookup, "object_permission_id", id, actionRefused,
			"the key's own models list and the permission row's have no model in common, so "+
				"the key may reach none; dorang's empty allow-list means the opposite")
	}
	k.Models = merged
	rep.ObjectPermissions++
	return true
}

// intersectModels returns the allow-list that honours both restrictions. An
// empty list on either side is "unrestricted", so it yields to the other.
func intersectModels(key, perm []string) []string {
	switch {
	case len(key) == 0:
		return append([]string(nil), perm...)
	case len(perm) == 0:
		return append([]string(nil), key...)
	}
	// "*" on either side is that side declining to restrict.
	if slices.Contains(key, "*") {
		return append([]string(nil), perm...)
	}
	if slices.Contains(perm, "*") {
		return append([]string(nil), key...)
	}
	out := make([]string, 0, len(key))
	for _, m := range key {
		if slices.Contains(perm, m) {
			out = append(out, m)
		}
	}
	return out
}

// OpenSource opens a read-only handle on a foreign database for [ImportKeys].
//
// It exists so that the driver names live in one place. [Open] knows that
// "sqlite" and "pgx" are what the two blank imports at the top of store.go
// register; a caller that needed a second handle had to know it too, and a
// second copy of that knowledge is how a build without one of the drivers turns
// into a runtime "unknown driver" at the moment of a cutover.
//
// No migration is applied and no schema is assumed: the source belongs to
// another product, and [ImportKeys] enumerates its columns before selecting any
// of them.
func OpenSource(ctx context.Context, driver Dialect, dsn string) (*sql.DB, error) {
	var (
		db  *sql.DB
		err error
	)
	switch driver {
	case DialectSQLite:
		db, err = sql.Open("sqlite", sqliteDSN(dsn))
	case DialectPostgres:
		db, err = sql.Open("pgx", dsn)
	default:
		return nil, fmt.Errorf("store: unknown source driver %q", driver)
	}
	if err != nil {
		return nil, fmt.Errorf("store: open %s source: %w", driver, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		// The DSN is deliberately absent from the message: it is the one string
		// in an import that routinely carries a password, and this error is the
		// one an operator pastes into a ticket.
		return nil, fmt.Errorf("store: cannot reach the %s source database: %w", driver, err)
	}
	return db, nil
}

// ImportKeys migrates credentials from a foreign LiteLLM_VerificationToken-shaped
// table into api_keys.
//
// [OpenSource] opens src, and `dorangctl import keys` is the operator-facing
// invocation.
//
// # How the secret is not needed
//
// The source stores sha256(full "sk-..." token) as lower-case hex. dorang's
// scheme-independent index key is defined as sha256(token)[:16] -- which is the
// first 32 hex characters of exactly that digest. So the importer derives both
// `lookup` and `token_hash` from the stored digest alone, with hash_scheme
// legacy_sha256, and never needs the plaintext. Verification then upgrades to
// dorang_v1 on first use through RehashKey, when the plaintext is briefly in
// hand (DESIGN 2.4). This is why lookup is defined the way it is: any other
// definition would have forced a flag day.
//
// # What it refuses to do
//
//   - It never resurrects an expired row. Expired credentials import as expired
//     and are counted.
//   - It never copies the display column that holds trailing characters of the
//     secret.
//   - It never invents an administrative credential. The master key is compared
//     out-of-band against a configured value and is not a row; the report warns
//     when the caller appears to expect otherwise.
//   - It never overwrites a key that already exists in the destination.
//
// Provider credentials in a sealed store are out of scope here, but the same
// warning applies: read them BEFORE rotating the sealing key, because where the
// sealing key defaults to the admin key, rotating it destroys every stored
// provider credential (DESIGN 2.4).
func (s *Store) ImportKeys(ctx context.Context, src *sql.DB, opts ImportOptions) (ImportReport, error) {
	if src == nil {
		return ImportReport{}, errors.New("store: import needs a source database")
	}
	if opts.Table == "" {
		opts.Table = DefaultImportTable
	}
	if !validIdent(opts.Table) {
		return ImportReport{}, fmt.Errorf("%w: source table %q", ErrBadIdentifier, opts.Table)
	}
	if opts.OnMissingTeam == "" {
		opts.OnMissingTeam = MissingTeamSkip
	}
	if opts.OnUntranslatable == "" {
		opts.OnUntranslatable = UntranslatableSkip
	}
	if opts.ObjectPermissionTable == "" {
		opts.ObjectPermissionTable = DefaultObjectPermissionTable
	}
	if !validIdent(opts.ObjectPermissionTable) {
		return ImportReport{}, fmt.Errorf("%w: object permission table %q",
			ErrBadIdentifier, opts.ObjectPermissionTable)
	}
	if opts.Source == "" {
		opts.Source = "litellm"
	}
	now := opts.Now
	if now.IsZero() {
		now = s.now()
	}

	rep := ImportReport{Table: opts.Table, DryRun: opts.DryRun}

	present, err := sourceColumns(ctx, src, opts.Table)
	if err != nil {
		return rep, err
	}
	if _, ok := present["token"]; !ok {
		return rep, fmt.Errorf("store: source table %q has no token column", opts.Table)
	}

	// Account for every column before reading a single row.
	var selected []string
	for col := range present {
		lc := strings.ToLower(col)
		if reason, refused := refusedColumns[lc]; refused {
			rep.DroppedColumns = append(rep.DroppedColumns, DroppedColumn{Column: col, Reason: reason})
			continue
		}
		if _, carried := importedColumns[lc]; carried {
			selected = append(selected, col)
			continue
		}
		rep.DroppedColumns = append(rep.DroppedColumns, DroppedColumn{
			Column: col,
			Reason: "no dorang equivalent; review before decommissioning the source",
		})
	}
	sort.Strings(selected)
	sort.Slice(rep.DroppedColumns, func(i, j int) bool {
		return rep.DroppedColumns[i].Column < rep.DroppedColumns[j].Column
	})

	if opts.ExpectAdminKey {
		rep.warn("the administrative/master credential is out-of-band and is never a row in this table; " +
			"configure server.master_key_env or the gateway has no admin authentication at all")
	}

	// Build the SELECT from the surviving columns only. The refused display
	// column is not merely ignored downstream -- it is never fetched.
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

	teamKnown := map[string]bool{}
	userKnown := map[string]bool{}
	perms := &objectPermissions{table: opts.ObjectPermissionTable}

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

		key, decision := s.buildImportedKey(rec, now, opts, &rep)
		if decision != "" {
			continue
		}

		// Meaning, not data. The row is already faithful; these two decide
		// whether what it says still says it on this side. The permission
		// table is resolved FIRST, because a sentinel may be waiting inside
		// it and must be refused by the same rule as one on the key.
		if !perms.resolve(ctx, src, key, &rep) {
			continue
		}
		if !translateAllowLists(key, opts.OnUntranslatable, &rep) {
			continue
		}

		if key.TeamID != "" {
			ok, err := s.exists(ctx, teamKnown, "teams", key.TeamID)
			if err != nil {
				return rep, err
			}
			if !ok {
				rep.MissingTeam++
				if opts.OnMissingTeam == MissingTeamSkip {
					rep.skip(key.Lookup, "missing team",
						fmt.Sprintf("team %q is not present; importing it would leave the key with no team budget, "+
							"no team rate limit and no team blocked flag", key.TeamID))
					continue
				}
				rep.warn("imported keys reference teams that are not present; their team-scoped limits do not apply")
				key.TeamID = ""
			}
		}
		if key.UserID != "" {
			ok, err := s.exists(ctx, userKnown, "users", key.UserID)
			if err != nil {
				return rep, err
			}
			if !ok {
				rep.MissingUser++
				rep.warn("imported keys reference users that are not present; the user id is carried so a later " +
					"user import links up, but user-scoped budgets do not apply until it does")
			}
		}

		if key.Expired(now) {
			rep.Expired++
		}
		if key.Blocked {
			rep.Blocked++
		}

		if opts.DryRun {
			rep.Imported++
			continue
		}
		written, err := s.insertImportedKey(ctx, key)
		if err != nil {
			return rep, fmt.Errorf("store: import key %s: %w", key.Lookup, err)
		}
		if !written {
			rep.AlreadyPresent++
			rep.Expired -= boolInt(key.Expired(now))
			rep.Blocked -= boolInt(key.Blocked)
			continue
		}
		rep.Imported++
	}
	if err := rows.Err(); err != nil {
		return rep, err
	}

	if rep.Expired > 0 {
		rep.warn("%d credential(s) were already expired at import time and were imported AS EXPIRED; "+
			"they will not authenticate", rep.Expired)
	}
	var refused, cleared int
	for _, u := range rep.Untranslated {
		if u.Action == actionRefused {
			refused++
		} else {
			cleared++
		}
	}
	if refused > 0 {
		rep.warn("%d credential(s) carry an authorization idiom dorang cannot express and were REFUSED; "+
			"each is named above with the column and the value. Resolve them at the source, or "+
			"re-run with --on-untranslatable=clear to import them with that allow-list dropped "+
			"(a widening: the key becomes unrestricted at key level)", refused)
	}
	if cleared > 0 {
		rep.warn("%d credential(s) were imported with an allow-list CLEARED because dorang could not "+
			"express what the source said; each is named above. A cleared allow-list is "+
			"unrestricted at key level -- confirm every one of them", cleared)
	}
	return rep, nil
}

// buildImportedKey maps one source row. A non-empty second return value means
// the row was rejected and already recorded in the report.
func (s *Store) buildImportedKey(rec map[string]any, now time.Time, opts ImportOptions, rep *ImportReport) (*APIKey, string) {
	digest := strings.TrimSpace(asString(rec["token"]))
	switch {
	case digest == "":
		rep.skip("", "empty token", "the row carries no credential")
		return nil, "empty"
	case strings.HasPrefix(digest, "sk-"):
		// A plaintext secret in the token column is how an administrative
		// credential ends up stored as a row in the incumbent. dorang refuses
		// it on both counts: the admin credential is out-of-band, and dorang
		// does not ingest plaintext secrets from a foreign database.
		rep.warn("a source row holds a plaintext credential rather than a digest; the administrative/master " +
			"credential is out-of-band in dorang and is never imported as a row -- configure server.master_key_env")
		rep.skip("", "plaintext token", "token column holds an sk-... literal, not a sha256 digest")
		return nil, "plaintext"
	case !isSHA256Hex(digest):
		rep.skip("", "unrecognised token format",
			fmt.Sprintf("expected a lower-case 64-character sha256 hex digest, got %d characters", len(digest)))
		return nil, "format"
	}

	// lookup is the first 16 bytes of sha256(token) in hex -- the first 32
	// characters of the digest the source already stores. No plaintext needed.
	lookup := digest[:32]

	k := &APIKey{
		Lookup:     lookup,
		TokenHash:  digest,
		HashScheme: SchemeLegacySHA256,
		KeyLabel:   labelFromLookup(lookup),
		KeyAlias:   asString(rec["key_alias"]),
		UserID:     asString(rec["user_id"]),
		TeamID:     asString(rec["team_id"]),

		Models:             asList(rec["models"]),
		AllowedRoutes:      asList(rec["allowed_routes"]),
		ObjectPermissionID: asString(rec["object_permission_id"]),
		Tags:               asList(rec["tags"]),

		BudgetPeriod:  asString(rec["budget_duration"]),
		PriorityClass: "default",
		Source:        opts.Source,
	}

	if alias := strings.ToLower(k.KeyAlias); strings.Contains(alias, "master") || strings.Contains(alias, "admin") {
		rep.warn("a source row is aliased %q; if that is the administrative credential, note that dorang compares "+
			"the master key out-of-band and never as a row", k.KeyAlias)
	}

	// Expiry: carried exactly as found. A past expiry stays in the past.
	if t, ok := asTime(rec["expires"]); ok {
		k.ExpiresAt = t
	}
	k.Blocked = asBool(rec["blocked"])

	if t, ok := asTime(rec["budget_reset_at"]); ok {
		k.BudgetResetAt = t
	}
	if t, ok := asTime(rec["created_at"]); ok {
		k.CreatedAt = t
	}
	if t, ok := asTime(rec["updated_at"]); ok {
		k.UpdatedAt = t
	}

	for _, m := range []struct {
		col string
		dst **int64
	}{
		{"max_budget", &k.MaxBudgetNano},
		{"soft_budget", &k.SoftBudgetNano},
	} {
		v, ok := rec[m.col]
		if !ok || v == nil {
			continue
		}
		nano, err := nanoFromAny(v)
		if err != nil {
			rep.skip(lookup, "budget out of range", fmt.Sprintf("%s: %v", m.col, err))
			return nil, "budget"
		}
		*m.dst = int64p(nano)
	}
	if v, ok := rec["spend"]; ok && v != nil {
		nano, err := nanoFromAny(v)
		if err != nil {
			rep.skip(lookup, "spend out of range", err.Error())
			return nil, "spend"
		}
		k.SpendNano = nano
	}

	k.RPMLimit = asIntPtr(rec["rpm_limit"])
	k.TPMLimit = asIntPtr(rec["tpm_limit"])
	k.MaxParallel = asIntPtr(rec["max_parallel_requests"])

	return k, ""
}

// insertImportedKey writes the row unless its lookup is already present.
// Reports whether it wrote.
func (s *Store) insertImportedKey(ctx context.Context, k *APIKey) (bool, error) {
	if k.ID == "" {
		k.ID = NewID()
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = s.now()
	}
	if k.UpdatedAt.IsZero() {
		k.UpdatedAt = s.now()
	}
	q := `INSERT INTO api_keys (` + apiKeyColumns + `) VALUES (` + placeholders(apiKeyColumnCount) + `)
		ON CONFLICT (lookup) DO NOTHING`
	res, err := s.exec(ctx, q,
		k.ID, k.Lookup, k.TokenHash, string(k.HashScheme), k.KeyLabel, nullStr(k.KeyAlias),
		nullStr(k.UserID), nullStr(k.TeamID), encodeStrings(k.Models), encodeStrings(k.AllowedRoutes),
		nullStr(k.ObjectPermissionID),
		ptrInt(k.MaxBudgetNano), ptrInt(k.SoftBudgetNano), nullStr(k.BudgetPeriod),
		nullMicros(k.BudgetResetAt), k.SpendNano,
		ptrInt(k.RPMLimit), ptrInt(k.TPMLimit), ptrInt(k.MaxParallel), k.PriorityClass,
		encodeStrings(k.Tags), k.Blocked, nullMicros(k.ExpiresAt),
		k.Source, Micros(k.CreatedAt), Micros(k.UpdatedAt),
		k.Tier, nullMicros(k.PendedAt), k.PendReason)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	// An imported key gets its existing secret as generation 1, exactly as a
	// natively issued one does. Without it the key would authenticate through
	// the denormalized columns and have nothing to rotate (DESIGN §11.2c).
	if err := s.insertKeySecret(ctx, nil, &KeySecret{
		ID: k.ID + ".1", KeyID: k.ID, Generation: 1,
		Lookup: k.Lookup, TokenHash: k.TokenHash, HashScheme: k.HashScheme,
		KeyLabel: k.KeyLabel, Current: true, CreatedAt: k.CreatedAt,
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) exists(ctx context.Context, cache map[string]bool, table, id string) (bool, error) {
	if v, ok := cache[id]; ok {
		return v, nil
	}
	var one int
	err := s.queryRow(ctx, "SELECT 1 FROM "+table+" WHERE id = ?", id).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		cache[id] = false
		return false, nil
	case err != nil:
		return false, err
	default:
		cache[id] = true
		return true, nil
	}
}

// sourceColumns discovers the source table's columns without reading a row, so
// that a schema-version difference is a report line rather than a scan error.
func sourceColumns(ctx context.Context, src *sql.DB, table string) (map[string]struct{}, error) {
	rows, err := src.QueryContext(ctx, "SELECT * FROM "+quoteIdent(table)+" WHERE 1 = 0")
	if err != nil {
		return nil, fmt.Errorf("store: inspect %s: %w", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(cols))
	for _, c := range cols {
		out[c] = struct{}{}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Foreign value coercion. The source may be PostgreSQL or SQLite, and the two
// hand back different Go types for the same logical column.
// ---------------------------------------------------------------------------

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

func asBool(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case int64:
		return t != 0
	case float64:
		return t != 0
	case string:
		b, _ := strconv.ParseBool(t)
		return b
	case []byte:
		b, _ := strconv.ParseBool(string(t))
		return b
	default:
		return false
	}
}

func asIntPtr(v any) *int64 {
	switch t := v.(type) {
	case nil:
		return nil
	case int64:
		return int64p(t)
	case int32:
		return int64p(int64(t))
	case int:
		return int64p(int64(t))
	case float64:
		return int64p(int64(t))
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil {
			return nil
		}
		return int64p(n)
	case []byte:
		n, err := strconv.ParseInt(strings.TrimSpace(string(t)), 10, 64)
		if err != nil {
			return nil
		}
		return int64p(n)
	default:
		return nil
	}
}

var timeFormats = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func asTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case nil:
		return time.Time{}, false
	case time.Time:
		if t.IsZero() {
			return time.Time{}, false
		}
		return t.UTC(), true
	case int64:
		// Distinguish seconds from micros by magnitude: anything past year
		// 33658 in seconds is far more likely to be microseconds.
		if t > 1_000_000_000_000 {
			return time.UnixMicro(t).UTC(), true
		}
		return time.Unix(t, 0).UTC(), true
	case float64:
		return time.Unix(int64(t), 0).UTC(), true
	case string:
		return parseTimeString(t)
	case []byte:
		return parseTimeString(string(t))
	default:
		return time.Time{}, false
	}
}

func parseTimeString(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, f := range timeFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), true
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return asTime(n)
	}
	return time.Time{}, false
}

// asList accepts a JSON array, a PostgreSQL array literal, or a single value,
// which is the range of shapes the incumbent's allow-list columns take across
// its own backends.
func asList(v any) []string {
	var s string
	switch t := v.(type) {
	case nil:
		return nil
	case []string:
		return t
	case string:
		s = t
	case []byte:
		s = string(t)
	default:
		return nil
	}
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return nil
	}
	if strings.HasPrefix(s, "[") {
		var out []string
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
		return nil
	}
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		inner := strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
		if inner == "" {
			return nil
		}
		parts := strings.Split(inner, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			p = strings.Trim(p, `"`)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return []string{s}
}

// nanoFromAny converts a source money value to nano-units.
//
// The source stores money as a binary float, so the conversion cannot be
// exact; it is rounded half-away-from-zero once, here, and range-checked.
// Everything downstream of this point is exact integer arithmetic
// (DESIGN 8.3).
func nanoFromAny(v any) (int64, error) {
	var f float64
	switch t := v.(type) {
	case nil:
		return 0, nil
	case float64:
		f = t
	case float32:
		f = float64(t)
	case int64:
		f = float64(t)
	case int:
		f = float64(t)
	case string:
		p, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, fmt.Errorf("not a number: %q", t)
		}
		f = p
	case []byte:
		return nanoFromAny(string(t))
	default:
		return 0, fmt.Errorf("unsupported money type %T", v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%w: %v", ErrAmountRange, f)
	}
	nano := math.Round(f * 1e9)
	if math.Abs(nano) > float64(MaxAmountNano) {
		return 0, fmt.Errorf("%w: %v", ErrAmountRange, f)
	}
	return int64(nano), nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
