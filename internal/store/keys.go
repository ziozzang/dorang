package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// HashScheme names how a credential's secret is stored (DESIGN 2.4).
type HashScheme string

// Hash schemes.
const (
	// SchemeDorangV1 is HMAC-SHA256(pepper, token). A stolen database is not
	// offline-attackable.
	SchemeDorangV1 HashScheme = "dorang_v1"
	// SchemeLegacySHA256 is an unsalted single-round digest, matching the
	// common incumbent scheme. Supported only during a migration window.
	SchemeLegacySHA256 HashScheme = "legacy_sha256"
)

// Key-authentication errors. They are separate from ErrBadCredential because
// the caller must be able to log why without being able to fail open: every
// one of them means "no principal".
var (
	ErrKeyExpired = errors.New("store: api key expired")
	ErrKeyBlocked = errors.New("store: api key blocked")
)

// APIKey is one row of api_keys: the authorization fields that must be carried
// or the gateway fails open (DESIGN 2.4).
//
// Nullable limits are pointers because nil ("no limit configured") and 0 ("a
// limit of zero, allow nothing") are different answers, and flattening them
// into 0 is how a rate limit quietly stops existing.
type APIKey struct {
	ID string

	// Lookup is sha256(token)[:16] in hex: the scheme-independent index key.
	Lookup     string
	TokenHash  string
	HashScheme HashScheme

	// KeyLabel is a non-reversible display label. It never contains any part
	// of the secret (DESIGN 2.4).
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

	// Source distinguishes natively issued keys from imported ones.
	Source string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Expired reports whether the key's expiry has passed. A key with no expiry
// never expires.
func (k *APIKey) Expired(now time.Time) bool {
	return !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt)
}

// Authenticated is the result of AuthenticateKey.
type Authenticated struct {
	Key *APIKey
	// NeedsRehash is true when the token verified under legacy_sha256 and
	// auth.rehash_on_use should schedule an asynchronous upgrade to
	// dorang_v1 (DESIGN 2.4). The upgrade is RehashKey.
	NeedsRehash bool
}

// KeyLookup returns the scheme-independent index key for a token:
// the first 16 bytes of sha256(token), in lower-case hex.
//
// This is what makes two hash schemes stay one lookup. It is not a secret
// verifier -- it is a shortened digest and is treated as an index value only.
func KeyLookup(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

// HashLegacySHA256 is the incumbent scheme: an unsalted single-round SHA-256 of
// the full token, lower-case hex.
func HashLegacySHA256(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// HashDorangV1 is HMAC-SHA256(pepper, token), lower-case hex.
func HashDorangV1(pepper []byte, token string) (string, error) {
	if len(pepper) == 0 {
		return "", ErrNoPepper
	}
	m := hmac.New(sha256.New, pepper)
	m.Write([]byte(token))
	return hex.EncodeToString(m.Sum(nil)), nil
}

// HashToken hashes under the named scheme.
func HashToken(scheme HashScheme, pepper []byte, token string) (string, error) {
	switch scheme {
	case SchemeDorangV1:
		return HashDorangV1(pepper, token)
	case SchemeLegacySHA256:
		return HashLegacySHA256(token), nil
	default:
		return "", fmt.Errorf("store: unknown hash scheme %q", scheme)
	}
}

// LabelFor derives the non-reversible display label for a token.
//
// The incumbent schema stores trailing characters of the secret in its display
// column. dorang does not copy that column and does not reproduce it: the label
// is derived from the digest, so it identifies the key in a UI and reveals
// nothing that helps reconstruct it.
func LabelFor(token string) string { return labelFromLookup(KeyLookup(token)) }

func labelFromLookup(lookup string) string {
	if len(lookup) > 8 {
		lookup = lookup[:8]
	}
	return "key-" + lookup
}

// AuthenticateKey resolves a bearer token to a principal.
//
// It issues exactly ONE query regardless of hash scheme: the row is selected by
// the scheme-independent lookup key, and only verification branches (DESIGN
// 2.4). Verification is a constant-time comparison in both branches.
//
// Expiry and the blocked flag are enforced here rather than returned for the
// caller to check, because the failure mode they guard against is a caller who
// forgets.
func (s *Store) AuthenticateKey(ctx context.Context, token string) (Authenticated, error) {
	if token == "" {
		return Authenticated{}, ErrBadCredential
	}
	key, err := s.GetAPIKeyByLookup(ctx, KeyLookup(token))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Authenticated{}, ErrBadCredential
		}
		return Authenticated{}, err
	}

	now := s.now()
	var legacy bool
	switch key.HashScheme {
	case SchemeDorangV1:
		want, err := HashDorangV1(s.cfg.Pepper, token)
		if err != nil {
			return Authenticated{}, err
		}
		if !constantTimeHexEqual(want, key.TokenHash) {
			return Authenticated{}, ErrBadCredential
		}
	case SchemeLegacySHA256:
		// The comparison happens either way so that a disabled legacy scheme
		// is not distinguishable from a wrong secret by timing; only the
		// returned error differs.
		ok := constantTimeHexEqual(HashLegacySHA256(token), key.TokenHash)
		if !s.cfg.Legacy.allowed(now) {
			return Authenticated{}, ErrLegacyDisabled
		}
		if !ok {
			return Authenticated{}, ErrBadCredential
		}
		legacy = true
	default:
		return Authenticated{}, fmt.Errorf("store: key %s has unknown hash scheme %q", key.ID, key.HashScheme)
	}

	if key.Blocked {
		return Authenticated{}, ErrKeyBlocked
	}
	if key.Expired(now) {
		return Authenticated{}, ErrKeyExpired
	}
	return Authenticated{Key: key, NeedsRehash: legacy}, nil
}

// constantTimeHexEqual compares two hex digests without leaking where they
// first differ. Length is compared first because it is not secret.
func constantTimeHexEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// RehashKey upgrades a legacy_sha256 row to dorang_v1 using a token that has
// already verified. This is the asynchronous half of auth.rehash_on_use: the
// migration completes without downtime and without a flag day.
//
// It is conditional on the row still being legacy, so two concurrent upgrades
// and a repeated upgrade are all no-ops rather than races.
func (s *Store) RehashKey(ctx context.Context, keyID, token string) error {
	hash, err := HashDorangV1(s.cfg.Pepper, token)
	if err != nil {
		return err
	}
	if KeyLookup(token) == "" {
		return ErrBadCredential
	}
	res, err := s.exec(ctx, `
		UPDATE api_keys
		   SET token_hash = ?, hash_scheme = ?, updated_at = ?
		 WHERE id = ? AND lookup = ? AND hash_scheme = ?`,
		hash, string(SchemeDorangV1), Micros(s.now()),
		keyID, KeyLookup(token), string(SchemeLegacySHA256))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Either already upgraded by another node, or the token does not
		// belong to that row. Both are "nothing to do"; the second cannot
		// succeed later either, because lookup is immutable.
		return nil
	}
	return nil
}

const apiKeyColumns = `id, lookup, token_hash, hash_scheme, key_label, key_alias,
	user_id, team_id, models, allowed_routes, object_permission_id,
	max_budget_nano, soft_budget_nano, budget_period, budget_reset_at, spend_nano,
	rpm_limit, tpm_limit, max_parallel, priority_class, tags, blocked, expires_at,
	source, created_at, updated_at`

// GetAPIKeyByLookup fetches a key row by its scheme-independent lookup value.
func (s *Store) GetAPIKeyByLookup(ctx context.Context, lookup string) (*APIKey, error) {
	row := s.queryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE lookup = ?`, lookup)
	return scanAPIKey(row)
}

// GetAPIKey fetches a key row by id.
func (s *Store) GetAPIKey(ctx context.Context, id string) (*APIKey, error) {
	row := s.queryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id = ?`, id)
	return scanAPIKey(row)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanAPIKey(row rowScanner) (*APIKey, error) {
	var (
		k                                                  APIKey
		scheme, label                                      string
		alias, userID, teamID, objPerm                     sql.NullString
		models, routes, tags                               string
		maxBudget, softBudget, resetAt, rpm, tpm, parallel sql.NullInt64
		budgetPeriod                                       sql.NullString
		expiresAt                                          sql.NullInt64
		createdAt, updatedAt                               int64
	)
	err := row.Scan(&k.ID, &k.Lookup, &k.TokenHash, &scheme, &label, &alias,
		&userID, &teamID, &models, &routes, &objPerm,
		&maxBudget, &softBudget, &budgetPeriod, &resetAt, &k.SpendNano,
		&rpm, &tpm, &parallel, &k.PriorityClass, &tags, &k.Blocked, &expiresAt,
		&k.Source, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.HashScheme = HashScheme(scheme)
	k.KeyLabel = label
	k.KeyAlias = str(alias)
	k.UserID = str(userID)
	k.TeamID = str(teamID)
	k.ObjectPermissionID = str(objPerm)
	k.Models = decodeStrings(models)
	k.AllowedRoutes = decodeStrings(routes)
	k.Tags = decodeStrings(tags)
	k.MaxBudgetNano = nullableInt(maxBudget)
	k.SoftBudgetNano = nullableInt(softBudget)
	k.BudgetPeriod = str(budgetPeriod)
	k.BudgetResetAt = TimeAt(nullInt(resetAt))
	k.RPMLimit = nullableInt(rpm)
	k.TPMLimit = nullableInt(tpm)
	k.MaxParallel = nullableInt(parallel)
	k.ExpiresAt = TimeAt(nullInt(expiresAt))
	k.CreatedAt = TimeAt(createdAt)
	k.UpdatedAt = TimeAt(updatedAt)
	return &k, nil
}

// InsertAPIKey writes a key row. The caller supplies Lookup, TokenHash and
// HashScheme, or NewAPIKeyFromToken derives them.
func (s *Store) InsertAPIKey(ctx context.Context, k *APIKey) error {
	return s.insertAPIKey(ctx, nil, k)
}

func (s *Store) insertAPIKey(ctx context.Context, tx *sql.Tx, k *APIKey) error {
	if k.ID == "" {
		k.ID = NewID()
	}
	if k.Lookup == "" || k.TokenHash == "" || k.HashScheme == "" {
		return errors.New("store: api key needs lookup, token_hash and hash_scheme")
	}
	if k.KeyLabel == "" {
		k.KeyLabel = labelFromLookup(k.Lookup)
	}
	if k.PriorityClass == "" {
		k.PriorityClass = "default"
	}
	if k.Source == "" {
		k.Source = "native"
	}
	now := s.now()
	if k.CreatedAt.IsZero() {
		k.CreatedAt = now
	}
	k.UpdatedAt = now
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

	const q = `INSERT INTO api_keys (` + apiKeyColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	args := []any{
		k.ID, k.Lookup, k.TokenHash, string(k.HashScheme), k.KeyLabel, nullStr(k.KeyAlias),
		nullStr(k.UserID), nullStr(k.TeamID), encodeStrings(k.Models), encodeStrings(k.AllowedRoutes),
		nullStr(k.ObjectPermissionID),
		ptrInt(k.MaxBudgetNano), ptrInt(k.SoftBudgetNano), nullStr(k.BudgetPeriod),
		nullMicros(k.BudgetResetAt), k.SpendNano,
		ptrInt(k.RPMLimit), ptrInt(k.TPMLimit), ptrInt(k.MaxParallel), k.PriorityClass,
		encodeStrings(k.Tags), k.Blocked, nullMicros(k.ExpiresAt),
		k.Source, Micros(k.CreatedAt), Micros(k.UpdatedAt),
	}
	var err error
	if tx != nil {
		_, err = s.txExec(ctx, tx, q, args...)
	} else {
		_, err = s.exec(ctx, q, args...)
	}
	return err
}

// NewAPIKeyFromToken fills Lookup, TokenHash, HashScheme and KeyLabel for a
// freshly issued token. Newly issued keys are always dorang_v1; legacy_sha256
// is import-only.
func (s *Store) NewAPIKeyFromToken(token string, k *APIKey) error {
	hash, err := HashDorangV1(s.cfg.Pepper, token)
	if err != nil {
		return err
	}
	k.Lookup = KeyLookup(token)
	k.TokenHash = hash
	k.HashScheme = SchemeDorangV1
	if k.KeyLabel == "" {
		k.KeyLabel = labelFromLookup(k.Lookup)
	}
	return nil
}
