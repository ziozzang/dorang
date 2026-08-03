package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DESIGN §11.2c — key rotation.
//
// Client keys are managed the way a credential should be: the identity is
// durable and the secret is not. A key has an id and one or more secrets;
// rotation mints a new secret and leaves the old one valid for a grace period.
//
// Everything else about the key — its tier, budget, spend to date, model
// allow-list, rate limits, team, and its whole ledger history — belongs to the
// ID, not the secret. That is the entire point. A rotation that also reset the
// limits would be a re-provisioning, and an operator facing that will put it
// off, which is how a five-year-old secret happens. Nothing in this file writes
// a column of api_keys other than the three that denormalize the CURRENT
// secret's verifier.

// Rotation errors.
var (
	// ErrNoCurrentSecret reports a key whose secrets table holds no current
	// row. It cannot happen through this package's API — InsertAPIKey writes
	// generation 1 in the same transaction as the key — and it is a distinct
	// error rather than ErrNotFound because the two mean different things to
	// whoever has to fix it.
	ErrNoCurrentSecret = errors.New("store: api key has no current secret")
	// ErrTooManySecrets reports a rotation that would exceed
	// auth.rotation.max_secrets.
	ErrTooManySecrets = errors.New("store: rotating would leave more secrets in flight than max_secrets allows")
	// ErrSecretRetired reports a secret whose grace period ended or that was
	// cut short.
	ErrSecretRetired = errors.New("store: api key secret has been retired")
)

// KeySecret is one row of api_key_secrets: one credential attached to one
// durable key id.
type KeySecret struct {
	ID    string
	KeyID string
	// Generation counts rotations. 1 is the secret the key was issued with.
	Generation int64

	Lookup     string
	TokenHash  string
	HashScheme HashScheme
	KeyLabel   string

	// Current marks the secret a rotation would replace. Exactly one per key,
	// enforced by a partial unique index rather than by convention.
	Current   bool
	CreatedAt time.Time
	// ExpiresAt is the end of the grace period for a superseded secret. Zero
	// means the secret does not expire on its own.
	ExpiresAt time.Time
	// RevokedAt is an early cut. Zero means none.
	RevokedAt time.Time
}

// Retired reports whether the secret has stopped authenticating at now, either
// because its grace period ended or because an operator cut it short.
//
// The boundary is inclusive: a secret expiring at T is refused at T.
func (s *KeySecret) Retired(now time.Time) bool {
	if s == nil {
		return true
	}
	if !s.RevokedAt.IsZero() && !now.Before(s.RevokedAt) {
		return true
	}
	return !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt)
}

// Age reports how long the secret has existed. It is what auth.rotation.max_age
// is compared against — a policy that warns, never an execution that breaks a
// working integration on a timer.
func (s *KeySecret) Age(now time.Time) time.Duration { return now.Sub(s.CreatedAt) }

const keySecretColumns = `id, key_id, generation, lookup, token_hash, hash_scheme,
	key_label, is_current, created_at, expires_at, revoked_at`

const keySecretColumnCount = 11

// keySecretScan mirrors [apiKeyScan]: one declaration of the destination order,
// reused by the single-row read and by the JOIN that authentication uses.
type keySecretScan struct {
	scheme               string
	expiresAt, revokedAt sql.NullInt64
	createdAt            int64
}

func (t *keySecretScan) dests(s *KeySecret) []any {
	return []any{&s.ID, &s.KeyID, &s.Generation, &s.Lookup, &s.TokenHash, &t.scheme,
		&s.KeyLabel, &s.Current, &t.createdAt, &t.expiresAt, &t.revokedAt}
}

func (t *keySecretScan) finish(s *KeySecret) {
	s.HashScheme = HashScheme(t.scheme)
	s.CreatedAt = TimeAt(t.createdAt)
	s.ExpiresAt = TimeAt(nullInt(t.expiresAt))
	s.RevokedAt = TimeAt(nullInt(t.revokedAt))
}

func scanKeySecret(row rowScanner) (*KeySecret, error) {
	var (
		s KeySecret
		t keySecretScan
	)
	err := row.Scan(t.dests(&s)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.finish(&s)
	return &s, nil
}

func (s *Store) insertKeySecret(ctx context.Context, tx *sql.Tx, k *KeySecret) error {
	if k.KeyID == "" || k.Lookup == "" || k.TokenHash == "" || k.HashScheme == "" {
		return errors.New("store: a key secret needs key_id, lookup, token_hash and hash_scheme")
	}
	if k.ID == "" {
		k.ID = NewID()
	}
	if k.Generation <= 0 {
		k.Generation = 1
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = s.now()
	}
	q := `INSERT INTO api_key_secrets (` + keySecretColumns + `) VALUES (` +
		placeholders(keySecretColumnCount) + `)`
	args := []any{
		k.ID, k.KeyID, k.Generation, k.Lookup, k.TokenHash, string(k.HashScheme),
		k.KeyLabel, k.Current, Micros(k.CreatedAt),
		nullMicros(k.ExpiresAt), nullMicros(k.RevokedAt),
	}
	var err error
	if tx != nil {
		_, err = s.txExec(ctx, tx, q, args...)
	} else {
		_, err = s.exec(ctx, q, args...)
	}
	return err
}

// ListKeySecrets returns every secret of a key, newest generation first.
//
// It returns retired secrets too. An operator asking "did the client roll?"
// needs to see the one that is about to stop working, and a list that hid it
// would answer the question only after it stopped mattering.
func (s *Store) ListKeySecrets(ctx context.Context, keyID string) ([]*KeySecret, error) {
	rows, err := s.query(ctx, `SELECT `+keySecretColumns+`
		  FROM api_key_secrets WHERE key_id = ? ORDER BY generation DESC`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*KeySecret
	for rows.Next() {
		sec, err := scanKeySecret(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, rows.Err()
}

// RotationPolicy mirrors the `auth.rotation` block of DESIGN §11.2c.
type RotationPolicy struct {
	// Grace is how long the old secret keeps authenticating after a rotation.
	// Zero cuts it immediately, which is a deliberate choice an operator may
	// make and not a default.
	Grace time.Duration
	// MaxSecrets bounds how many secrets a key may have live at once. Zero
	// means [DefaultMaxSecrets].
	MaxSecrets int
	// MaxAge is a POLICY, not an execution: dorang warns and reports, and does
	// not silently break a working integration on a timer. Nothing in this file
	// reads it; [Store.KeysOverdueForRotation] does.
	MaxAge time.Duration
}

// Rotation defaults, matching §11.2c's example block.
const (
	DefaultRotationGrace = 24 * time.Hour
	DefaultMaxSecrets    = 2
	DefaultMaxAge        = 90 * 24 * time.Hour
)

func (p RotationPolicy) fill() RotationPolicy {
	if p.MaxSecrets <= 0 {
		p.MaxSecrets = DefaultMaxSecrets
	}
	return p
}

// Rotation is what a rotation did. It is returned rather than logged because
// `POST /key/rotate` has to report when the old secret expires, and a caller
// that has to infer that from a policy plus a clock will infer it wrong.
type Rotation struct {
	// KeyID is the durable identity, unchanged.
	KeyID string
	// New is the secret just minted. Its plaintext is NOT here: the plaintext
	// exists only in the caller's hand and is returned exactly once, by the
	// handler that minted it.
	New *KeySecret
	// Previous is the secret that was current a moment ago, now superseded.
	// Nil only if the key had none, which InsertAPIKey makes impossible.
	Previous *KeySecret
	// PreviousExpiresAt is when the old secret stops authenticating. This is
	// the field the API response is about.
	PreviousExpiresAt time.Time
	// Retired lists secrets this rotation cut immediately to stay within
	// max_secrets, oldest first.
	Retired []*KeySecret
	// InvalidateLookups are the index keys that must stop serving NOW: the ones
	// this rotation retired outright. The secret in grace is not among them —
	// it still authenticates, which is the point of a grace period.
	InvalidateLookups []string
}

// RotateKey mints a new secret for an existing key and leaves the old one valid
// for the policy's grace period.
//
// Everything that is not the verifier is untouched — that is the guarantee, and
// it is structural rather than careful: the UPDATE names exactly three columns.
//
// max_secrets is enforced by retiring the OLDEST superseded secrets outright,
// not by refusing the rotation. Refusing would mean an operator who rotates
// twice in a hurry — which is exactly what a suspected compromise looks like —
// is blocked by a bookkeeping limit at the worst possible moment.
func (s *Store) RotateKey(ctx context.Context, keyID string, v KeySecret, p RotationPolicy) (Rotation, error) {
	p = p.fill()
	if keyID == "" {
		return Rotation{}, ErrBadCredential
	}
	if v.Lookup == "" || v.TokenHash == "" || v.HashScheme == "" {
		return Rotation{}, errors.New("store: RotateKey needs the new secret's lookup, token_hash and hash_scheme")
	}
	now := s.now()
	var out Rotation
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		secrets, err := s.listKeySecretsTx(ctx, tx, keyID)
		if err != nil {
			return err
		}
		if len(secrets) == 0 {
			return ErrNoCurrentSecret
		}
		var cur *KeySecret
		maxGen := int64(0)
		for _, sec := range secrets {
			if sec.Generation > maxGen {
				maxGen = sec.Generation
			}
			if sec.Current {
				cur = sec
			}
		}
		if cur == nil {
			return ErrNoCurrentSecret
		}

		// The old secret is superseded: it stops being current and acquires an
		// expiry. Its row, its generation and its identity are otherwise
		// untouched, so the ledger rows that name it still resolve.
		graceEnd := now.Add(p.Grace)
		if _, err := s.txExec(ctx, tx, `
			UPDATE api_key_secrets SET is_current = ?, expires_at = ? WHERE id = ?`,
			false, Micros(graceEnd), cur.ID); err != nil {
			return err
		}
		cur.Current = false
		cur.ExpiresAt = graceEnd

		v.KeyID = keyID
		v.Generation = maxGen + 1
		v.Current = true
		v.ExpiresAt = time.Time{}
		v.RevokedAt = time.Time{}
		if v.ID == "" {
			v.ID = fmt.Sprintf("%s.%d", keyID, v.Generation)
		}
		if v.CreatedAt.IsZero() {
			v.CreatedAt = now
		}
		if err := s.insertKeySecret(ctx, tx, &v); err != nil {
			return err
		}

		// The denormalized copy on api_keys follows the current secret, and
		// nothing else on that row is written. Three columns, named.
		if _, err := s.txExec(ctx, tx, `
			UPDATE api_keys SET lookup = ?, token_hash = ?, hash_scheme = ?, key_label = ?, updated_at = ?
			 WHERE id = ?`,
			v.Lookup, v.TokenHash, string(v.HashScheme), v.KeyLabel, Micros(now), keyID); err != nil {
			return err
		}

		out = Rotation{
			KeyID:             keyID,
			New:               &v,
			Previous:          cur,
			PreviousExpiresAt: graceEnd,
		}

		// max_secrets: count the ones still able to authenticate after this
		// rotation, and cut the oldest until the count fits.
		live := []*KeySecret{&v, cur}
		for _, sec := range secrets {
			if sec.ID == cur.ID || sec.Retired(now) {
				continue
			}
			live = append(live, sec)
		}
		for len(live) > p.MaxSecrets {
			// The oldest generation is the one that has had the longest to
			// roll, so it is the one to cut.
			oldest := live[0]
			oi := 0
			for i, sec := range live {
				if sec.Generation < oldest.Generation {
					oldest, oi = sec, i
				}
			}
			if oldest.ID == v.ID {
				// Cutting the secret just minted would be absurd; a
				// max_secrets of zero or one is a configuration that cannot be
				// honoured and saying so is better than obeying it.
				return fmt.Errorf("%w: max_secrets is %d", ErrTooManySecrets, p.MaxSecrets)
			}
			if _, err := s.txExec(ctx, tx, `
				UPDATE api_key_secrets SET revoked_at = ?, is_current = ? WHERE id = ?`,
				Micros(now), false, oldest.ID); err != nil {
				return err
			}
			oldest.RevokedAt = now
			out.Retired = append(out.Retired, oldest)
			out.InvalidateLookups = append(out.InvalidateLookups, oldest.Lookup)
			live = append(live[:oi], live[oi+1:]...)
			if oldest.ID == cur.ID {
				out.PreviousExpiresAt = now
			}
		}
		return nil
	})
	if err != nil {
		return Rotation{}, err
	}
	return out, nil
}

// EndGrace cuts every superseded secret of a key immediately.
//
// This is what a suspected compromise needs: rotate now, cut the old secret
// immediately, keep everything else. The current secret is never touched — the
// caller who just rolled must not be locked out by the control that protects
// them.
//
// It returns the index keys that must stop serving, so the caller can publish
// one invalidation for them. Without that publication the cut is a database
// change that the fleet honours a cache TTL later, which is not what "ended
// early" means.
func (s *Store) EndGrace(ctx context.Context, keyID string) ([]string, error) {
	now := s.now()
	var lookups []string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		secrets, err := s.listKeySecretsTx(ctx, tx, keyID)
		if err != nil {
			return err
		}
		for _, sec := range secrets {
			if sec.Current || sec.Retired(now) {
				continue
			}
			if _, err := s.txExec(ctx, tx, `
				UPDATE api_key_secrets SET revoked_at = ? WHERE id = ?`,
				Micros(now), sec.ID); err != nil {
				return err
			}
			lookups = append(lookups, sec.Lookup)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return lookups, nil
}

func (s *Store) listKeySecretsTx(ctx context.Context, tx *sql.Tx, keyID string) ([]*KeySecret, error) {
	rows, err := tx.QueryContext(ctx, s.rebind(`SELECT `+keySecretColumns+`
		  FROM api_key_secrets WHERE key_id = ? ORDER BY generation ASC`), keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*KeySecret
	for rows.Next() {
		sec, err := scanKeySecret(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, rows.Err()
}

// KeyLookups returns every index key attached to a key id, retired ones
// included.
//
// The retired ones matter: an invalidation names the lookups it wants dropped,
// and a node that learned a secret before it was cut must be told about that
// secret and not only about the live ones.
func (s *Store) KeyLookups(ctx context.Context, keyID string) ([]string, error) {
	rows, err := s.query(ctx, `SELECT lookup FROM api_key_secrets WHERE key_id = ?`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// KeyRecord is one key paired with one of its secrets: the unit an
// authentication snapshot is built from.
//
// A key in a rotation grace period contributes TWO of these, with the same Key
// and different Secrets, because it has two index keys and both authenticate.
type KeyRecord struct {
	Key    *APIKey
	Secret *KeySecret
	// Owners is the key's owning user and team, either nil when unowned. It is
	// on the record rather than fetched beside it because DESIGN §11.2's
	// authorization envelope is all three subjects at once: a snapshot entry
	// built from a key whose owner was read a moment later would be an entry
	// whose three halves describe two different instants.
	Owners Owners
}

// ListKeyRecords returns every (key, secret) pair, for a snapshot reload.
//
// This is what a node reads on rejoin (§11.2c rule 4): serving from a snapshot
// known to be stale is worse than a brief pause at startup, so a returning node
// reloads rather than trusting what it had.
//
// Retired secrets are included. They are loaded so that they are refused as
// RETIRED rather than as unknown — the same reason §2.4 gives for loading
// expired rows — and the authenticator gives a refusing row the short negative
// lifetime, so they cost a few bytes and no freshness.
//
// # The owners are interned
//
// A deployment's keys share a small number of users and teams — that is what a
// team is — and this runs on every rejoin AND on the periodic reload, which is
// `entry_ttl/2` apart. Materializing a fresh User and Team per SECRET would
// build fifty thousand copies of a few dozen rows every half-TTL, including a
// JSON decode of each one's model allow-list. They are therefore built once per
// id and shared.
//
// Sharing is safe because nothing downstream mutates them: [auth.Principal] is
// documented immutable and its limits are copied out of these rows field by
// field, so what the records share is a read-only row and not a decision.
func (s *Store) ListKeyRecords(ctx context.Context, limit int) ([]KeyRecord, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := s.query(ctx, `SELECT `+prefixCols(apiKeyColumns, "k")+`, `+
		prefixCols(keySecretColumns, "s")+`, `+ownerCols+`
		  FROM api_key_secrets s JOIN api_keys k ON k.id = s.key_id`+ownerJoin+`
		 ORDER BY s.key_id, s.generation LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyRecord
	users := map[string]*User{}
	teams := map[string]*Team{}
	for rows.Next() {
		var (
			k  APIKey
			kt apiKeyScan
			v  KeySecret
			vt keySecretScan
			ut userScan
			tt teamScan
		)
		dest := append(kt.dests(&k), vt.dests(&v)...)
		dest = append(dest, ut.dests()...)
		dest = append(dest, tt.dests()...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		kt.finish(&k)
		vt.finish(&v)
		var own Owners
		if ut.id.Valid {
			if u, ok := users[ut.id.String]; ok {
				own.User = u
			} else {
				own.User = ut.user()
				users[ut.id.String] = own.User
			}
		}
		if tt.id.Valid {
			if tm, ok := teams[tt.id.String]; ok {
				own.Team = tm
			} else {
				own.Team = tt.team()
				teams[tt.id.String] = own.Team
			}
		}
		out = append(out, KeyRecord{Key: &k, Secret: &v, Owners: own})
	}
	return out, rows.Err()
}

// --- pend and release ---------------------------------------------------------

// PendKey marks a key pended and returns the index keys that must stop serving.
//
// A pend is the token guard's action (§11.6) and it is deliberately the
// reversible one: [Store.ReleaseKey] undoes it in one statement, and the caller
// keeps the credential it already has. Automatic revocation of a key that turns
// out to be legitimately busy is an outage the operator did not choose, and it
// is not reversible in the same sense.
//
// It is idempotent: pending an already-pended key does not move pended_at, so
// "when was this pended" survives a second alert.
func (s *Store) PendKey(ctx context.Context, keyID, reason string) ([]string, error) {
	if keyID == "" {
		return nil, ErrBadCredential
	}
	now := s.now()
	res, err := s.exec(ctx, `
		UPDATE api_keys SET pended_at = ?, pend_reason = ?, updated_at = ?
		 WHERE id = ? AND pended_at IS NULL`,
		Micros(now), reason, Micros(now), keyID)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// Either the key is already pended or it does not exist. The two are
		// distinguished by reading it, because "I pended a key that is not
		// there" is worth an error and "it was already pended" is not.
		if _, gerr := s.GetAPIKey(ctx, keyID); gerr != nil {
			return nil, gerr
		}
	}
	return s.KeyLookups(ctx, keyID)
}

// ReleaseKey clears a pend. This is the "released by an operator in ONE action"
// of §11.6, and it is one statement for exactly that reason.
func (s *Store) ReleaseKey(ctx context.Context, keyID string) ([]string, error) {
	if keyID == "" {
		return nil, ErrBadCredential
	}
	now := s.now()
	if _, err := s.exec(ctx, `
		UPDATE api_keys SET pended_at = NULL, pend_reason = '', updated_at = ?
		 WHERE id = ?`, Micros(now), keyID); err != nil {
		return nil, err
	}
	return s.KeyLookups(ctx, keyID)
}

// RevokeKey blocks a key outright and returns the index keys that must stop
// serving. It is the irreversible control, and it is a separate method from
// PendKey so that choosing it is a decision with a name.
func (s *Store) RevokeKey(ctx context.Context, keyID string) ([]string, error) {
	if keyID == "" {
		return nil, ErrBadCredential
	}
	now := s.now()
	if _, err := s.exec(ctx, `UPDATE api_keys SET blocked = ?, updated_at = ? WHERE id = ?`,
		true, Micros(now), keyID); err != nil {
		return nil, err
	}
	return s.KeyLookups(ctx, keyID)
}

// KeysOverdueForRotation returns the keys whose current secret is older than
// max_age, with its age.
//
// §11.2c: max_age is a policy, not an execution. dorang warns and reports; it
// does not silently break a working integration on a timer. This method is the
// reporting, and there is deliberately no counterpart that acts on it.
func (s *Store) KeysOverdueForRotation(ctx context.Context, maxAge time.Duration) (map[string]time.Duration, error) {
	if maxAge <= 0 {
		return nil, nil
	}
	cutoff := s.now().Add(-maxAge)
	rows, err := s.query(ctx, `
		SELECT key_id, created_at FROM api_key_secrets
		 WHERE is_current = ? AND created_at < ?`, true, Micros(cutoff))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Duration{}
	now := s.now()
	for rows.Next() {
		var (
			id string
			at int64
		)
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = now.Sub(TimeAt(at))
	}
	return out, rows.Err()
}
