package admin

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// DESIGN §11.2c and §11.6 on the wire: rotation with a grace period, an early
// cut, and the pend/release pair.
//
// The shape of every one of these follows one rule: the identity is durable and
// the secret is not. `key_id` never changes, the response says when the OLD
// secret stops working, and nothing here can reset a limit.

// secretView is one of a key's secrets as an operator sees it.
//
// There is no field here that could be turned back into the secret. Not the
// digest, not the lookup, not a trailing fragment — §2.4 records an incumbent
// whose display column stored the last characters of the key, and this type is
// how that stays impossible rather than merely absent.
type secretView struct {
	SecretID   string `json:"secret_id"`
	Generation int64  `json:"generation"`
	KeyName    string `json:"key_name"`
	// Current marks the secret a rotation would replace.
	Current   bool  `json:"current"`
	CreatedAt Stamp `json:"created_at"`
	// ExpiresAt is the end of this secret's grace period; absent for the
	// current secret, which expires with the key and not before it.
	ExpiresAt Stamp `json:"expires_at"`
	// RevokedAt is set when an operator cut this secret short rather than
	// letting its grace period run out.
	RevokedAt Stamp `json:"revoked_at"`
	// LastUsedAt is the field an operator actually reads before letting a grace
	// period close: it answers "did the client roll?" without waiting to find
	// out when the window shuts.
	LastUsedAt Stamp `json:"last_used_at"`
	// Status is "current", "grace", "expired" or "revoked" — the same four
	// states the timestamps encode, spelled out so a script does not have to
	// re-derive them from three nullable dates.
	Status string `json:"status"`
	// Age is how long this secret has existed, rendered for a human. `max_age`
	// is a policy that warns; this is what it warns about.
	Age string `json:"age"`
}

func viewSecret(s KeySecret, now time.Time) secretView {
	v := secretView{
		SecretID:   s.ID,
		Generation: s.Generation,
		KeyName:    s.KeyLabel,
		Current:    s.Current,
		CreatedAt:  Stamp(s.CreatedAt),
		ExpiresAt:  Stamp(s.ExpiresAt),
		RevokedAt:  Stamp(s.RevokedAt),
		LastUsedAt: Stamp(s.LastUsedAt),
		Age:        now.Sub(s.CreatedAt).Round(time.Hour).String(),
	}
	switch {
	case !s.RevokedAt.IsZero() && !now.Before(s.RevokedAt):
		v.Status = "revoked"
	case !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt):
		v.Status = "expired"
	case s.Current:
		v.Status = "current"
	default:
		v.Status = "grace"
	}
	return v
}

// rotatedKey is the response of POST /key/rotate. It is the second and last
// type in this package that carries a secret, and it is its own type for the
// same reason [generatedKey] is: returning one has to be a deliberate act with
// a name, visible in a diff.
type rotatedKey struct {
	// Key is the new plaintext credential, returned here and nowhere else.
	Key     string `json:"key"`
	TokenID string `json:"token_id"`
	KeyName string `json:"key_name"`

	// PreviousExpiresAt is when the OLD secret stops authenticating. It is the
	// point of the response: a caller that has to compute this from a policy
	// and a clock will compute it wrong, and the consequence is finding out
	// when the window shuts.
	PreviousExpiresAt Stamp `json:"previous_expires_at"`
	// PreviousSecretID and Generation identify what was replaced, so the
	// operator can match the ledger's `secret_id` against it.
	PreviousSecretID string `json:"previous_secret_id"`
	Generation       int64  `json:"generation"`
	// Grace is the configured window, echoed so the response is
	// self-explanatory in a terminal.
	Grace string `json:"grace"`
	// Secrets is every secret now attached to the key.
	Secrets []secretView `json:"secrets"`

	Info    keyView `json:"info"`
	Warning string  `json:"warning"`
	// Note states the property that makes rotation worth doing.
	Note string `json:"note"`
}

const rotationNote = "the key id, its tier, budget, spend, allow-list, rate limits, team and ledger " +
	"history are unchanged: rotation replaces the secret and nothing else"

// rotateRequest is the body of POST /key/rotate.
type rotateRequest struct {
	keyRef
	// Grace overrides auth.rotation.grace for this rotation. "0" cuts the old
	// secret immediately, which is a deliberate choice an operator may make and
	// is not the default.
	Grace *string `json:"grace"`
}

// POST /key/rotate — mint a new secret behind an existing key id.
func (c *call) keyRotate() error {
	ks, err := c.a.keys()
	if err != nil {
		return err
	}
	rs, ok := ks.(RotatingKeyStore)
	if !ok {
		// A named 501 rather than a regeneration: `/key/regenerate` replaces
		// the secret outright, and answering `/key/rotate` with it would cut
		// the caller off at the instant the operator asked for a grace period.
		return unimplemented(CodeDependencyOff,
			"this deployment's key store does not implement rotation with a grace period "+
				"(DESIGN §11.2c); POST /key/regenerate replaces the secret outright instead")
	}
	h, err := c.a.hasher()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}

	var body rotateRequest
	if err := decodeOptionalBody(c.w, c.r, &body); err != nil {
		return err
	}
	id, err := c.keyID(body.keyRef)
	if err != nil {
		return err
	}
	grace := c.a.cfg.RotationGrace
	if body.Grace != nil {
		d, err := parseDuration(*body.Grace)
		if err != nil {
			return badRequest("grace: %s", err).withParam("grace")
		}
		grace = d
	}

	k, err := ks.GetKey(c.ctx(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound("key", id)
		}
		return err
	}
	before := viewKey(k)

	token, err := c.a.cfg.NewToken()
	if err != nil {
		return newFault(http.StatusInternalServerError, CodeInternal, typeAPI,
			"a credential could not be minted")
	}
	digest, scheme, err := h.Hash(token)
	if err != nil {
		return unimplemented(CodeDependencyOff,
			"credentials cannot be issued: the key pepper is not configured (DESIGN §2.4)")
	}
	now := c.a.now().UTC()
	label := h.Label(token)
	res, err := rs.Rotate(c.ctx(), id,
		Verifier{Lookup: h.Lookup(token), TokenHash: digest, HashScheme: scheme}, label, grace, now)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound("key", id)
		}
		return err
	}
	c.a.metrics.keysIssued.Add(1)

	updated := *k
	updated.KeyLabel = label
	updated.HashScheme = scheme
	updated.UpdatedAt = now
	after := viewKey(&updated)

	secrets, err := rs.ListSecrets(c.ctx(), id)
	if err != nil {
		return err
	}
	views := make([]secretView, 0, len(secrets))
	for _, s := range secrets {
		views = append(views, viewSecret(s, now))
	}

	// The audit row records the rotation, never the secret. Only view types are
	// in scope here, so it cannot regress into recording one.
	if err := c.recordAudit("key.rotate", "key", id, before, map[string]any{
		"key":                 after,
		"generation":          res.New.Generation,
		"previous_secret_id":  res.Previous.ID,
		"previous_expires_at": Stamp(res.PreviousExpiresAt),
		"grace":               grace.String(),
		"secrets":             views,
	}); err != nil {
		return err
	}

	writeJSON(c.w, c.r, http.StatusOK, rotatedKey{
		Key:               token,
		TokenID:           id,
		KeyName:           label,
		PreviousExpiresAt: Stamp(res.PreviousExpiresAt),
		PreviousSecretID:  res.Previous.ID,
		Generation:        res.New.Generation,
		Grace:             grace.String(),
		Secrets:           views,
		Info:              after,
		Warning:           returnedOnce,
		Note:              rotationNote,
	})
	return nil
}

// POST /key/rotate/cut — end a rotation's grace period immediately.
//
// This is what a suspected compromise needs: rotate now, cut the old secret
// immediately, keep everything else. The current secret is never touched, so
// the caller who has already rolled is not locked out by the control that is
// protecting them.
func (c *call) keyCutGrace() error {
	ks, err := c.a.keys()
	if err != nil {
		return err
	}
	rs, ok := ks.(RotatingKeyStore)
	if !ok {
		return unimplemented(CodeDependencyOff,
			"this deployment's key store does not implement rotation with a grace period (DESIGN §11.2c)")
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var ref keyRef
	if err := decodeOptionalBody(c.w, c.r, &ref); err != nil {
		return err
	}
	id, err := c.keyID(ref)
	if err != nil {
		return err
	}
	now := c.a.now().UTC()
	n, err := rs.EndGrace(c.ctx(), id, now)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound("key", id)
		}
		return err
	}
	secrets, err := rs.ListSecrets(c.ctx(), id)
	if err != nil {
		return err
	}
	views := make([]secretView, 0, len(secrets))
	for _, s := range secrets {
		views = append(views, viewSecret(s, now))
	}
	if err := c.recordAudit("key.rotate.cut", "key", id,
		map[string]any{"cut": n}, map[string]any{"secrets": views}); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"key_id":  id,
		"cut":     n,
		"secrets": views,
		"note": "the current secret is unchanged; only superseded secrets were cut, " +
			"so a client that has already rolled is unaffected",
	})
	return nil
}

// GET|POST /key/secrets — list a key's secrets.
func (c *call) keySecrets() error {
	ks, err := c.a.keys()
	if err != nil {
		return err
	}
	rs, ok := ks.(RotatingKeyStore)
	if !ok {
		return unimplemented(CodeDependencyOff,
			"this deployment's key store does not track secrets separately (DESIGN §11.2c)")
	}
	var ref keyRef
	if err := decodeOptionalBody(c.w, c.r, &ref); err != nil {
		return err
	}
	id, err := c.keyID(ref)
	if err != nil {
		return err
	}
	secrets, err := rs.ListSecrets(c.ctx(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound("key", id)
		}
		return err
	}
	now := c.a.now().UTC()
	views := make([]secretView, 0, len(secrets))
	for _, s := range secrets {
		views = append(views, viewSecret(s, now))
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"key_id":  id,
		"secrets": views,
	})
	return nil
}

// pendRequest is the body of POST /key/pend.
type pendRequest struct {
	keyRef
	Reason *string `json:"reason"`
}

// keySetPended builds the handlers for POST /key/pend and POST /key/release.
//
// They are one implementation because they differ in one boolean, exactly as
// block and unblock do. The release takes nothing but an id: §11.6's
// requirement is that an operator releases a pended key in ONE action, and a
// release that needed a reason, a confirmation or a second call would not be
// one.
func keySetPended(pended bool) handler {
	return func(c *call) error {
		ks, err := c.a.keys()
		if err != nil {
			return err
		}
		ps, ok := ks.(PendableKeyStore)
		if !ok {
			return unimplemented(CodeDependencyOff,
				"this deployment's key store does not implement pend (DESIGN §11.6); "+
					"POST /key/block is the irreversible alternative")
		}
		if err := c.requireAudit(); err != nil {
			return err
		}
		var body pendRequest
		if err := decodeOptionalBody(c.w, c.r, &body); err != nil {
			return err
		}
		id, err := c.keyID(body.keyRef)
		if err != nil {
			return err
		}
		k, err := ks.GetKey(c.ctx(), id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return notFound("key", id)
			}
			return err
		}
		before := viewKey(k)

		now := c.a.now().UTC()
		reason := ""
		if body.Reason != nil {
			reason = strings.TrimSpace(*body.Reason)
		}
		if pended {
			if reason == "" {
				reason = "pended by an operator"
			}
			err = ps.Pend(c.ctx(), id, reason, now)
		} else {
			err = ps.Release(c.ctx(), id, now)
		}
		if err != nil {
			return err
		}

		updated := *k
		if pended {
			updated.PendedAt = now
			updated.PendReason = reason
		} else {
			updated.PendedAt = time.Time{}
			updated.PendReason = ""
		}
		updated.UpdatedAt = now
		after := viewKey(&updated)

		action := "key.release"
		note := "the key serves again; the caller keeps the credential it already has"
		if pended {
			action = "key.pend"
			note = "the key is refused with a distinct, documented error and can be released " +
				"in one action; it was not revoked, so no credential has to be reissued"
		}
		if err := c.recordAudit(action, "key", id, before, after); err != nil {
			return err
		}
		writeJSON(c.w, c.r, http.StatusOK, map[string]any{"key": after, "note": note})
		return nil
	}
}
