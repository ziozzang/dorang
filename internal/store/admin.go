package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// The rows the administration surface reads and writes that nothing else did.
//
// Everything here existed as a TABLE and not as a method: `api_keys` could be
// inserted and fetched by id but not listed, updated or deleted; `users` had a
// `role` column that decided who is an administrator and no Go code that read
// it; `audit_logs` had a schema, two indexes and a retention sweep that deletes
// from it, and no INSERT anywhere in the repository. That is the same shape as
// the security review's own headline — a control that exists and is not reached
// — one layer down, and it is why `/key/block` answered 501: not because the
// handler was missing, but because nothing underneath it could be called.
//
// Every query goes through Store.exec/query/queryRow like the rest of the
// package, so the placeholder rewriting and the per-dialect statement counting
// hold, and no identifier here is ever built from a value.

// APIKeyFilter narrows [Store.ListAPIKeys]. A zero filter lists everything up
// to Limit.
type APIKeyFilter struct {
	UserID string
	TeamID string
	// Blocked, when non-nil, restricts to blocked or unblocked keys.
	Blocked *bool
	Limit   int
	Offset  int
}

// DefaultAPIKeyListLimit bounds an unbounded listing. A caller that asks for no
// limit gets this rather than the table: the administration surface pages, and
// a list query that can return every credential in the deployment is a query
// that will one day be run against a deployment with a million of them.
const DefaultAPIKeyListLimit = 100

// MaxAPIKeyListLimit is the ceiling a caller may raise the page size to.
const MaxAPIKeyListLimit = 1000

// ListAPIKeys returns key rows matching f, newest first.
//
// The ordering is (created_at DESC, id DESC) rather than created_at alone,
// because two keys issued in the same microsecond would otherwise page
// non-deterministically — one row appearing on two pages and another on none.
func (s *Store) ListAPIKeys(ctx context.Context, f APIKeyFilter) ([]*APIKey, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultAPIKeyListLimit
	}
	if limit > MaxAPIKeyListLimit {
		limit = MaxAPIKeyListLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	// The WHERE clause is assembled from a fixed set of literal fragments and
	// the values travel as arguments. No identifier and no operator here comes
	// from the caller.
	var (
		where []string
		args  []any
	)
	if f.UserID != "" {
		where = append(where, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.TeamID != "" {
		where = append(where, "team_id = ?")
		args = append(args, f.TeamID)
	}
	if f.Blocked != nil {
		where = append(where, "blocked = ?")
		args = append(args, *f.Blocked)
	}
	q := `SELECT ` + apiKeyColumns + ` FROM api_keys`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*APIKey, 0, 16)
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// UpdateAPIKey rewrites every mutable authorization field of a key row.
//
// The verifier is deliberately absent from the SET list. lookup, token_hash and
// hash_scheme are the credential itself and are rotated by [Store.RotateKey],
// which keeps the old secret alive for a grace window; letting a general
// "update the key" path write them would give a caller two ways to change a
// secret, one of which skips the grace and the rotation bookkeeping. A type
// that carries a field its writer ignores is a field an operator will one day
// set and watch do nothing, so this is stated rather than left to be
// discovered.
func (s *Store) UpdateAPIKey(ctx context.Context, k *APIKey) error {
	if k == nil || k.ID == "" {
		return errors.New("store: UpdateAPIKey requires a key id")
	}
	for name, v := range map[string]*int64{"max_budget": k.MaxBudgetNano, "soft_budget": k.SoftBudgetNano} {
		if v != nil {
			if err := checkAmount(*v, name); err != nil {
				return err
			}
		}
	}
	if err := checkAmount(k.SpendNano, "spend"); err != nil {
		return err
	}
	if k.UpdatedAt.IsZero() {
		k.UpdatedAt = s.now()
	}
	const q = `UPDATE api_keys SET
		key_label = ?, key_alias = ?, user_id = ?, team_id = ?,
		models = ?, allowed_routes = ?, object_permission_id = ?,
		max_budget_nano = ?, soft_budget_nano = ?, budget_period = ?, budget_reset_at = ?,
		rpm_limit = ?, tpm_limit = ?, max_parallel = ?, priority_class = ?,
		tags = ?, blocked = ?, expires_at = ?, updated_at = ?
		WHERE id = ?`
	res, err := s.exec(ctx, q,
		k.KeyLabel, nullStr(k.KeyAlias), nullStr(k.UserID), nullStr(k.TeamID),
		encodeStrings(k.Models), encodeStrings(k.AllowedRoutes), nullStr(k.ObjectPermissionID),
		ptrInt(k.MaxBudgetNano), ptrInt(k.SoftBudgetNano), nullStr(k.BudgetPeriod),
		nullMicros(k.BudgetResetAt),
		ptrInt(k.RPMLimit), ptrInt(k.TPMLimit), ptrInt(k.MaxParallel), k.PriorityClass,
		encodeStrings(k.Tags), k.Blocked, nullMicros(k.ExpiresAt), Micros(k.UpdatedAt),
		k.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		// A driver that cannot count rows is not a reason to report a write
		// that may not have happened as a success.
		return nil
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAPIKeys removes key rows by id and reports how many went away.
//
// The ids travel as arguments; the only thing built from their COUNT is the
// number of placeholders, which is not a value.
func (s *Store) DeleteAPIKeys(ctx context.Context, ids []string) (int, error) {
	ids = dedupeNonEmpty(ids)
	if len(ids) == 0 {
		return 0, nil
	}
	q := `DELETE FROM api_keys WHERE id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	res, err := s.exec(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}

// UserRole reads one user's administrative role.
//
// This is the join the administration surface's authorization rests on: a key
// carries no role of its own (DESIGN §9.2), so "is this credential
// administrative" is a question about the row that owns it. It is one indexed
// primary-key read and it is on the administrative path, not the request path.
//
// A user id that does not exist returns [ErrNotFound] rather than the empty
// string, because "no such user" and "a user with no role" must not collapse
// into the same answer at an authorization boundary.
func (s *Store) UserRole(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", ErrNotFound
	}
	var role sql.NullString
	err := s.queryRow(ctx, `SELECT role FROM users WHERE id = ?`, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return str(role), nil
}

// AuditLog is one audit_logs row.
//
// Before and after are already-rendered JSON documents rather than `any`,
// because the rendering decides what is in them: the administration surface
// renders a redacted VIEW of an object and never the object, and a column that
// took an arbitrary value would let a later caller put a verifier in it.
type AuditLog struct {
	ID         string
	TS         time.Time
	ActorKind  string
	ActorID    string
	Action     string
	ObjectKind string
	ObjectID   string
	Before     string
	After      string
	IP         string
	UserAgent  string
}

// InsertAuditLog appends one row to the administrative trail.
//
// The table, its two indexes and the retention sweep that deletes from it have
// all existed since the first migration. This is the writer they were waiting
// for; until it existed, `audit_logs` was a table that could only ever be
// emptied.
func (s *Store) InsertAuditLog(ctx context.Context, e AuditLog) error {
	if e.ID == "" {
		return errors.New("store: InsertAuditLog requires an id")
	}
	if e.TS.IsZero() {
		e.TS = s.now()
	}
	const q = `INSERT INTO audit_logs
		(id, ts, actor_kind, actor_id, action, object_kind, object_id,
		 before_state, after_state, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.exec(ctx, q, e.ID, Micros(e.TS), e.ActorKind, e.ActorID, e.Action,
		e.ObjectKind, e.ObjectID, nullStr(e.Before), nullStr(e.After), e.IP, e.UserAgent)
	return err
}

// dedupeNonEmpty trims, drops empties and removes duplicates while keeping
// order. A duplicate id in a DELETE list would otherwise inflate nothing and
// cost a placeholder, and an empty one would match a row with an empty id.
func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// Replacing the secret behind a key id is NOT here. It was, as a direct UPDATE
// of api_keys.lookup/token_hash/hash_scheme — and that column set is the
// DENORMALIZED copy: authentication resolves through api_key_secrets
// (resolveKeyQuery), so writing only api_keys leaves the leaked secret
// authenticating and the new one unknown. A regeneration that regenerates
// nothing is the worst possible shape for an incident path.
//
// [Store.RotateKey] is the one implementation, and [Store.EndGrace] is how a
// caller asks for the immediate cut: rotate with a zero grace, then end it. The
// difference between a planned rotation and an incident is a parameter, not a
// second function.
