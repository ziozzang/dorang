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
	"strings"
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
	// ErrKeyPended is the token guard's refusal (DESIGN §11.6). It is distinct
	// from ErrKeyBlocked because a pend is reversible in one operator action
	// and a block is a decision; a caller who cannot tell them apart cannot
	// tell an outage from a policy.
	ErrKeyPended = errors.New("store: api key pended")
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

	// Tier is the key's tier (DESIGN §11.6). It is assigned by an operator and
	// there is no request field that writes it: §10.5's rule is that an
	// operator can grant urgency and a caller cannot claim it. Empty means the
	// configured default tier.
	Tier string
	// PendedAt is when the token guard pended this key, zero when it is not
	// pended (§11.6). It is separate from Blocked because the two are different
	// judgements: Blocked is an operator's decision, a pend is a statistical
	// one that might be wrong and that an operator releases in one action
	// without reissuing a credential.
	PendedAt time.Time
	// PendReason records which condition tripped, in words.
	PendReason string

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
	// Secret is WHICH of the key's secrets verified (DESIGN §11.2c). The ledger
	// records it, so an operator can see whether the client actually rolled
	// before the grace window closes instead of finding out when it shuts.
	Secret *KeySecret
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
	key, secret, err := s.ResolveKeyByLookup(ctx, KeyLookup(token))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Authenticated{}, ErrBadCredential
		}
		return Authenticated{}, err
	}

	now := s.now()
	var legacy bool
	// The digest verified against belongs to the SECRET, not to the key. During
	// a rotation's grace period the two differ, and verifying against the key's
	// denormalized copy would refuse the caller who has not rolled yet.
	switch secret.HashScheme {
	case SchemeDorangV1:
		want, err := HashDorangV1(s.cfg.Pepper, token)
		if err != nil {
			return Authenticated{}, err
		}
		if !constantTimeHexEqual(want, secret.TokenHash) {
			return Authenticated{}, ErrBadCredential
		}
	case SchemeLegacySHA256:
		// The comparison happens either way so that a disabled legacy scheme
		// is not distinguishable from a wrong secret by timing; only the
		// returned error differs.
		ok := constantTimeHexEqual(HashLegacySHA256(token), secret.TokenHash)
		if !s.cfg.Legacy.allowed(now) {
			return Authenticated{}, ErrLegacyDisabled
		}
		if !ok {
			return Authenticated{}, ErrBadCredential
		}
		legacy = true
	default:
		return Authenticated{}, fmt.Errorf("store: key %s has unknown hash scheme %q", key.ID, secret.HashScheme)
	}

	if key.Blocked {
		return Authenticated{}, ErrKeyBlocked
	}
	if key.Pended() {
		return Authenticated{}, ErrKeyPended
	}
	if key.Expired(now) {
		return Authenticated{}, ErrKeyExpired
	}
	// The secret's own expiry: the key is fine and this credential is not.
	if secret.Retired(now) {
		return Authenticated{}, ErrSecretRetired
	}
	return Authenticated{Key: key, Secret: secret, NeedsRehash: legacy}, nil
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
	return s.SetKeyDigest(ctx, keyID, KeyLookup(token), hash, SchemeDorangV1)
}

// SetKeyDigest performs the same upgrade as [Store.RehashKey] from a digest the
// caller has ALREADY computed, rather than from the plaintext token.
//
// That distinction is the whole reason this method exists. An authenticator
// verifying a legacy row has computed the dorang_v1 digest as a by-product —
// verification derives both digests from one pass over the token — so handing it
// back costs nothing, while handing back the *token* would mean holding a live
// credential past the moment it was verified, on a queue, off the request path.
// Without this method rehash-on-use cannot be wired at all: the only upgrade
// primitive takes a secret its caller must not keep.
//
// Only the upgrade direction is accepted. A general "set this row's digest"
// would let a caller overwrite a live dorang_v1 credential with anything, so
// scheme must be [SchemeDorangV1] and the row must still be legacy. Both
// conditions live in the statement, which makes a repeated upgrade and two
// concurrent upgrades no-ops rather than races.
//
// lookup is matched as well as the id. It is immutable, so a digest that does
// not belong to the row named by keyID can never be written by naming the id
// alone.
func (s *Store) SetKeyDigest(ctx context.Context, keyID, lookup, digest string, scheme HashScheme) error {
	if scheme != SchemeDorangV1 {
		return fmt.Errorf("store: SetKeyDigest upgrades to %s only, not %q", SchemeDorangV1, scheme)
	}
	if keyID == "" || lookup == "" {
		return ErrBadCredential
	}
	if len(digest) != hex.EncodedLen(sha256.Size) {
		return fmt.Errorf("store: SetKeyDigest wants a %d-character hex digest, got %d",
			hex.EncodedLen(sha256.Size), len(digest))
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fmt.Errorf("store: SetKeyDigest digest is not hex: %w", err)
	}
	// Both copies are upgraded in one transaction. api_key_secrets is the
	// authority and api_keys.token_hash is its denormalized copy for the
	// current secret; upgrading one and not the other would leave a key that
	// verifies under one scheme and reports the other.
	//
	// The secrets row is matched on the LOOKUP, not on "the current secret":
	// a legacy secret can be one that is still inside a rotation's grace
	// period, and rehash-on-use has to upgrade whichever one actually verified.
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.txExec(ctx, tx, `
			UPDATE api_key_secrets
			   SET token_hash = ?, hash_scheme = ?
			 WHERE key_id = ? AND lookup = ? AND hash_scheme = ?`,
			digest, string(SchemeDorangV1), keyID, lookup, string(SchemeLegacySHA256)); err != nil {
			return err
		}
		_, err := s.txExec(ctx, tx, `
			UPDATE api_keys
			   SET token_hash = ?, hash_scheme = ?, updated_at = ?
			 WHERE id = ? AND lookup = ? AND hash_scheme = ?`,
			digest, string(SchemeDorangV1), Micros(s.now()),
			keyID, lookup, string(SchemeLegacySHA256))
		// Zero rows is not an error. Either another node upgraded the row
		// first, or the (id, lookup) pair names no legacy row — and the second
		// cannot start succeeding later, because lookup never changes.
		return err
	})
}

const apiKeyColumns = `id, lookup, token_hash, hash_scheme, key_label, key_alias,
	user_id, team_id, models, allowed_routes, object_permission_id,
	max_budget_nano, soft_budget_nano, budget_period, budget_reset_at, spend_nano,
	rpm_limit, tpm_limit, max_parallel, priority_class, tags, blocked, expires_at,
	source, created_at, updated_at, tier, pended_at, pend_reason`

// apiKeyColumnCount must match the placeholder count in InsertAPIKey. A column
// added to the list and not to the VALUES clause is a runtime error on the
// first insert, which is the one place this is worth asserting in a constant.
const apiKeyColumnCount = 29

// GetAPIKeyByLookup fetches a key row by any of its secrets' index keys.
//
// The lookup is resolved through api_key_secrets rather than through the
// denormalized column on api_keys, because after a rotation the key has two
// live index keys and only one of them is on that column. Both must authenticate
// to the same principal during the grace period — that is §11.2c's contract —
// and a query keyed on the current secret alone would refuse the caller who has
// not rolled yet as an unknown key.
func (s *Store) GetAPIKeyByLookup(ctx context.Context, lookup string) (*APIKey, error) {
	row := s.queryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys
		 WHERE id = (SELECT key_id FROM api_key_secrets WHERE lookup = ?)`, lookup)
	return scanAPIKey(row)
}

// resolveKeyQuery selects a key and the one secret an index key belongs to, in
// ONE statement.
//
// §2.4's rule that one lookup selects the row and only verification branches
// survives rotation intact: a key with two live secrets is two rows in
// api_key_secrets with two index keys, and the join is still a unique-index
// probe followed by a primary-key probe inside a single query. Splitting it
// into two round trips would double the cost of every authentication miss, and
// there is a test that counts the statements.
var resolveKeyQuery = `SELECT ` + prefixCols(apiKeyColumns, "k") + `, ` +
	prefixCols(keySecretColumns, "s") + `
	  FROM api_key_secrets s JOIN api_keys k ON k.id = s.key_id
	 WHERE s.lookup = ?`

// ResolveKeyByLookup returns the key AND the specific secret an index key
// belongs to.
//
// The pair is what authentication actually needs. The key carries the
// authorization envelope, which belongs to the id; the secret carries the
// digest to verify against and its own expiry, which belongs to the secret. A
// caller handed only the key would verify a grace-period credential against the
// current secret's digest and refuse it.
func (s *Store) ResolveKeyByLookup(ctx context.Context, lookup string) (*APIKey, *KeySecret, error) {
	var (
		k  APIKey
		kt apiKeyScan
		v  KeySecret
		vt keySecretScan
	)
	dest := append(kt.dests(&k), vt.dests(&v)...)
	err := s.queryRow(ctx, resolveKeyQuery, lookup).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	kt.finish(&k)
	vt.finish(&v)
	return &k, &v, nil
}

// GetAPIKey fetches a key row by id.
func (s *Store) GetAPIKey(ctx context.Context, id string) (*APIKey, error) {
	row := s.queryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id = ?`, id)
	return scanAPIKey(row)
}

type rowScanner interface{ Scan(dest ...any) error }

// apiKeyScan holds the nullable and encoded intermediates one api_keys row
// needs on the way in.
//
// It exists so that the destination list and the conversion that follows it can
// be reused by a JOINed query, which is what keeps authentication at ONE
// statement now that a key has several secrets (DESIGN §2.4's rule, §11.2c's
// shape). Two hand-written scanners over the same columns would drift, and the
// symptom of that drift is a column read into the wrong field.
type apiKeyScan struct {
	scheme, label                                      string
	alias, userID, teamID, objPerm                     sql.NullString
	models, routes, tags                               string
	maxBudget, softBudget, resetAt, rpm, tpm, parallel sql.NullInt64
	budgetPeriod                                       sql.NullString
	expiresAt, pendedAt                                sql.NullInt64
	createdAt, updatedAt                               int64
}

// dests returns the Scan destinations for apiKeyColumns, in that exact order.
func (t *apiKeyScan) dests(k *APIKey) []any {
	return []any{&k.ID, &k.Lookup, &k.TokenHash, &t.scheme, &t.label, &t.alias,
		&t.userID, &t.teamID, &t.models, &t.routes, &t.objPerm,
		&t.maxBudget, &t.softBudget, &t.budgetPeriod, &t.resetAt, &k.SpendNano,
		&t.rpm, &t.tpm, &t.parallel, &k.PriorityClass, &t.tags, &k.Blocked, &t.expiresAt,
		&k.Source, &t.createdAt, &t.updatedAt, &k.Tier, &t.pendedAt, &k.PendReason}
}

func (t *apiKeyScan) finish(k *APIKey) {
	k.HashScheme = HashScheme(t.scheme)
	k.KeyLabel = t.label
	k.KeyAlias = str(t.alias)
	k.UserID = str(t.userID)
	k.TeamID = str(t.teamID)
	k.ObjectPermissionID = str(t.objPerm)
	k.Models = decodeStrings(t.models)
	k.AllowedRoutes = decodeStrings(t.routes)
	k.Tags = decodeStrings(t.tags)
	k.MaxBudgetNano = nullableInt(t.maxBudget)
	k.SoftBudgetNano = nullableInt(t.softBudget)
	k.BudgetPeriod = str(t.budgetPeriod)
	k.BudgetResetAt = TimeAt(nullInt(t.resetAt))
	k.RPMLimit = nullableInt(t.rpm)
	k.TPMLimit = nullableInt(t.tpm)
	k.MaxParallel = nullableInt(t.parallel)
	k.ExpiresAt = TimeAt(nullInt(t.expiresAt))
	k.PendedAt = TimeAt(nullInt(t.pendedAt))
	k.CreatedAt = TimeAt(t.createdAt)
	k.UpdatedAt = TimeAt(t.updatedAt)
}

func scanAPIKey(row rowScanner) (*APIKey, error) {
	var (
		k APIKey
		t apiKeyScan
	)
	err := row.Scan(t.dests(&k)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.finish(&k)
	return &k, nil
}

// prefixCols qualifies a column list with a table alias, so a JOIN reuses the
// single declaration of the column ORDER that the scanner depends on.
func prefixCols(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// Pended reports whether the token guard has pended this key (DESIGN §11.6).
func (k *APIKey) Pended() bool { return !k.PendedAt.IsZero() }

// InsertAPIKey writes a key row and its first secret.
//
// The caller supplies Lookup, TokenHash and HashScheme, or NewAPIKeyFromToken
// derives them. Both writes happen in one transaction: a key row with no
// generation-1 secret would authenticate through the denormalized columns and
// then rotate into a state where the secrets table disagreed with them
// (DESIGN §11.2c).
func (s *Store) InsertAPIKey(ctx context.Context, k *APIKey) error {
	return s.withTx(ctx, func(tx *sql.Tx) error { return s.insertAPIKey(ctx, tx, k) })
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

	q := `INSERT INTO api_keys (` + apiKeyColumns + `) VALUES (` + placeholders(apiKeyColumnCount) + `)`
	args := []any{
		k.ID, k.Lookup, k.TokenHash, string(k.HashScheme), k.KeyLabel, nullStr(k.KeyAlias),
		nullStr(k.UserID), nullStr(k.TeamID), encodeStrings(k.Models), encodeStrings(k.AllowedRoutes),
		nullStr(k.ObjectPermissionID),
		ptrInt(k.MaxBudgetNano), ptrInt(k.SoftBudgetNano), nullStr(k.BudgetPeriod),
		nullMicros(k.BudgetResetAt), k.SpendNano,
		ptrInt(k.RPMLimit), ptrInt(k.TPMLimit), ptrInt(k.MaxParallel), k.PriorityClass,
		encodeStrings(k.Tags), k.Blocked, nullMicros(k.ExpiresAt),
		k.Source, Micros(k.CreatedAt), Micros(k.UpdatedAt),
		k.Tier, nullMicros(k.PendedAt), k.PendReason,
	}
	var err error
	if tx != nil {
		_, err = s.txExec(ctx, tx, q, args...)
	} else {
		_, err = s.exec(ctx, q, args...)
	}
	if err != nil {
		return err
	}
	return s.insertKeySecret(ctx, tx, &KeySecret{
		ID:         k.ID + ".1",
		KeyID:      k.ID,
		Generation: 1,
		Lookup:     k.Lookup,
		TokenHash:  k.TokenHash,
		HashScheme: k.HashScheme,
		KeyLabel:   k.KeyLabel,
		Current:    true,
		CreatedAt:  k.CreatedAt,
	})
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
