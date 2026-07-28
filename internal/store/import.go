package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultImportTable is the incumbent proxy's credential table.
const DefaultImportTable = "LiteLLM_VerificationToken"

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

// ImportOptions configures ImportKeys.
type ImportOptions struct {
	// Table is the source table name. Zero value: DefaultImportTable.
	Table string

	// Now is the instant expiry is judged against. Zero means the store clock.
	Now time.Time

	// OnMissingTeam defaults to MissingTeamSkip.
	OnMissingTeam MissingTeamPolicy

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

	NotMigrated    []NotMigrated
	DroppedColumns []DroppedColumn
	Warnings       []string
}

func (r *ImportReport) skip(lookup, reason, detail string) {
	r.Skipped++
	r.NotMigrated = append(r.NotMigrated, NotMigrated{Lookup: lookup, Reason: reason, Detail: detail})
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

// Summary renders the report as one human-readable block, for `dorang import`.
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

// ImportKeys migrates credentials from a foreign LiteLLM_VerificationToken-shaped
// table into api_keys.
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
	const q = `INSERT INTO api_keys (` + apiKeyColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (lookup) DO NOTHING`
	res, err := s.exec(ctx, q,
		k.ID, k.Lookup, k.TokenHash, string(k.HashScheme), k.KeyLabel, nullStr(k.KeyAlias),
		nullStr(k.UserID), nullStr(k.TeamID), encodeStrings(k.Models), encodeStrings(k.AllowedRoutes),
		nullStr(k.ObjectPermissionID),
		ptrInt(k.MaxBudgetNano), ptrInt(k.SoftBudgetNano), nullStr(k.BudgetPeriod),
		nullMicros(k.BudgetResetAt), k.SpendNano,
		ptrInt(k.RPMLimit), ptrInt(k.TPMLimit), ptrInt(k.MaxParallel), k.PriorityClass,
		encodeStrings(k.Tags), k.Blocked, nullMicros(k.ExpiresAt),
		k.Source, Micros(k.CreatedAt), Micros(k.UpdatedAt))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
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
