package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Wire shapes
// ---------------------------------------------------------------------------

// keyView is what a key looks like on the wire.
//
// There is no field here that could be turned back into the secret, and there
// is no field here that holds any part of it. §2.4 records a foreign system
// whose display column stored the key's trailing characters; `key_name` is a
// label derived from the *lookup*, which is itself a truncated digest, so
// nothing about the secret survives into this struct.
//
// Field names follow the shape-compatible surface (§2.3) because scripts depend
// on them. Where dorang has something the incumbent does not — hash_scheme,
// priority_class, source — the field is additive, which no reader breaks on.
type keyView struct {
	TokenID  string `json:"token_id"`
	KeyName  string `json:"key_name"`
	KeyAlias string `json:"key_alias,omitempty"`

	UserID string `json:"user_id"`
	TeamID string `json:"team_id"`

	Models             []string `json:"models"`
	AllowedRoutes      []string `json:"allowed_routes"`
	ObjectPermissionID string   `json:"object_permission_id,omitempty"`

	Spend         Money  `json:"spend"`
	MaxBudget     *Money `json:"max_budget"`
	SoftBudget    *Money `json:"soft_budget"`
	BudgetPeriod  string `json:"budget_duration"`
	BudgetResetAt Stamp  `json:"budget_reset_at"`

	RPMLimit      *int64 `json:"rpm_limit"`
	TPMLimit      *int64 `json:"tpm_limit"`
	MaxParallel   *int64 `json:"max_parallel_requests"`
	PriorityClass string `json:"priority_class"`

	Tags    []string `json:"tags"`
	Blocked bool     `json:"blocked"`
	Expires Stamp    `json:"expires"`

	// Tier is the key's tier (§11.6). It is settable only through this
	// operator-authenticated surface: §10.5's rule is that an operator can
	// grant and a caller cannot claim.
	Tier string `json:"tier"`
	// Pended and PendReason are §11.6's reversible refusal. They are separate
	// from `blocked` on the wire for the same reason they are separate in the
	// store: a caller who cannot tell them apart cannot tell an outage from a
	// policy.
	Pended     bool   `json:"pended"`
	PendedAt   Stamp  `json:"pended_at"`
	PendReason string `json:"pend_reason,omitempty"`

	HashScheme string `json:"hash_scheme"`
	Source     string `json:"source"`

	CreatedAt Stamp `json:"created_at"`
	UpdatedAt Stamp `json:"updated_at"`
}

func viewKey(k *Key) keyView {
	return keyView{
		TokenID:            k.ID,
		KeyName:            k.KeyLabel,
		KeyAlias:           k.KeyAlias,
		UserID:             k.UserID,
		TeamID:             k.TeamID,
		Models:             orEmpty(k.Models),
		AllowedRoutes:      orEmpty(k.AllowedRoutes),
		ObjectPermissionID: k.ObjectPermissionID,
		Spend:              Money(k.SpendNano),
		MaxBudget:          moneyPtr(k.MaxBudgetNano),
		SoftBudget:         moneyPtr(k.SoftBudgetNano),
		BudgetPeriod:       k.BudgetPeriod,
		BudgetResetAt:      Stamp(k.BudgetResetAt),
		RPMLimit:           k.RPMLimit,
		TPMLimit:           k.TPMLimit,
		MaxParallel:        k.MaxParallel,
		PriorityClass:      k.PriorityClass,
		Tags:               orEmpty(k.Tags),
		Blocked:            k.Blocked,
		Expires:            Stamp(k.ExpiresAt),
		Tier:               k.Tier,
		Pended:             !k.PendedAt.IsZero(),
		PendedAt:           Stamp(k.PendedAt),
		PendReason:         k.PendReason,
		HashScheme:         k.HashScheme,
		Source:             k.Source,
		CreatedAt:          Stamp(k.CreatedAt),
		UpdatedAt:          Stamp(k.UpdatedAt),
	}
}

// hydrateSpend replaces the stored spend column with what the keys have
// actually spent, in place, before they are rendered.
//
// It is done here rather than inside [viewKey] because viewKey also builds the
// before/after documents of the audit trail, and those must keep the value the
// operator's mutation actually wrote. An audit diff whose `spend` moves on its
// own describes traffic, not the change being audited.
//
// A reporter that fails is logged into the response by NOT changing anything:
// the error is swallowed deliberately, because /key/info's job is to describe a
// credential and refusing to describe it over a spend lookup would take an
// incident-response route down for an accounting figure. A key the reporter has
// no row for keeps its stored value for the same reason — absent is not zero,
// and the counter has nothing to say about a key that has never been budgeted.
func (c *call) hydrateSpend(keys ...*Key) {
	if c.a.cfg.Spend == nil || len(keys) == 0 {
		return
	}
	refs := make([]KeySpendRef, 0, len(keys))
	for _, k := range keys {
		if k != nil && k.ID != "" {
			refs = append(refs, KeySpendRef{ID: k.ID, Period: k.BudgetPeriod})
		}
	}
	if len(refs) == 0 {
		return
	}
	spend, err := c.a.cfg.Spend.KeySpend(c.ctx(), refs)
	if err != nil {
		return
	}
	for _, k := range keys {
		if k == nil {
			continue
		}
		if v, ok := spend[k.ID]; ok {
			k.SpendNano = v
		}
	}
}

// orEmpty renders a nil slice as [] rather than null. A script doing
// `for m in key["models"]` breaks on null and not on [], and the two mean the
// same thing here.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// generatedKey is the one and only response that carries a secret.
//
// It exists as its own type so that returning a secret is a deliberate act with
// a name, visible in a diff, rather than a field on the shared view that a
// later edit could start populating everywhere.
type generatedKey struct {
	// Key is the plaintext credential. It is returned here and nowhere else,
	// ever, and dorang does not store it: only the lookup and the digest are
	// written.
	Key     string  `json:"key"`
	TokenID string  `json:"token_id"`
	KeyName string  `json:"key_name"`
	Expires Stamp   `json:"expires"`
	Info    keyView `json:"info"`
	// Warning states the contract in the payload, because the operator reading
	// a terminal is the person who needs to know.
	Warning string `json:"warning"`
}

const returnedOnce = "this is the only time the key is returned; dorang stores a non-reversible digest and cannot show it again"

// ---------------------------------------------------------------------------
// Request shapes
// ---------------------------------------------------------------------------

// keySpec is the mutable surface of a key.
//
// Every field is a pointer so that "absent" is distinguishable from "present
// and zero". Clearing a nullable limit is not expressed as JSON null — null and
// absent are indistinguishable after decoding into a pointer — but by naming
// the field in `clear`. That is more verbose than the alternative and it is the
// only version that cannot silently drop a limit, which §2.4 lists among the
// ways a gateway fails open.
type keySpec struct {
	KeyAlias      *string   `json:"key_alias"`
	UserID        *string   `json:"user_id"`
	TeamID        *string   `json:"team_id"`
	Models        *[]string `json:"models"`
	AllowedRoutes *[]string `json:"allowed_routes"`

	MaxBudget     *Money  `json:"max_budget"`
	SoftBudget    *Money  `json:"soft_budget"`
	BudgetPeriod  *string `json:"budget_duration"`
	BudgetResetAt *Stamp  `json:"budget_reset_at"`

	RPMLimit    *int64 `json:"rpm_limit"`
	TPMLimit    *int64 `json:"tpm_limit"`
	MaxParallel *int64 `json:"max_parallel_requests"`

	PriorityClass *string `json:"priority_class"`
	// Tier is assigned by an operator (§11.6). It appears here and in no
	// request shape the gateway's data plane reads, which is the whole of
	// §10.5's asymmetry: an operator can grant a tier, a caller cannot claim
	// one.
	Tier    *string   `json:"tier"`
	Tags    *[]string `json:"tags"`
	Blocked *bool     `json:"blocked"`

	Expires  *Stamp  `json:"expires"`
	Duration *string `json:"duration"`

	// Clear names nullable fields to reset to "no limit configured".
	Clear []string `json:"clear"`
}

// clearable names the fields `clear` accepts. An unknown name is refused rather
// than ignored: a caller who misspells "max_budget" and gets a 200 believes a
// budget was removed when it was not.
var clearable = map[string]func(*Key){
	"key_alias":             func(k *Key) { k.KeyAlias = "" },
	"user_id":               func(k *Key) { k.UserID = "" },
	"team_id":               func(k *Key) { k.TeamID = "" },
	"models":                func(k *Key) { k.Models = nil },
	"allowed_routes":        func(k *Key) { k.AllowedRoutes = nil },
	"max_budget":            func(k *Key) { k.MaxBudgetNano = nil },
	"soft_budget":           func(k *Key) { k.SoftBudgetNano = nil },
	"budget_duration":       func(k *Key) { k.BudgetPeriod = ""; k.BudgetResetAt = time.Time{} },
	"rpm_limit":             func(k *Key) { k.RPMLimit = nil },
	"tpm_limit":             func(k *Key) { k.TPMLimit = nil },
	"max_parallel_requests": func(k *Key) { k.MaxParallel = nil },
	"tags":                  func(k *Key) { k.Tags = nil },
	"tier":                  func(k *Key) { k.Tier = "" },
	"expires":               func(k *Key) { k.ExpiresAt = time.Time{} },
}

// apply folds the spec onto a key.
func (s *keySpec) apply(k *Key, now time.Time) error {
	for _, name := range s.Clear {
		fn, ok := clearable[strings.TrimSpace(name)]
		if !ok {
			return badRequest("clear names an unknown field %q", name).withParam("clear")
		}
		fn(k)
	}
	setString(&k.KeyAlias, s.KeyAlias)
	setString(&k.UserID, s.UserID)
	setString(&k.TeamID, s.TeamID)
	setStrings(&k.Models, s.Models)
	setStrings(&k.AllowedRoutes, s.AllowedRoutes)
	setStrings(&k.Tags, s.Tags)
	setString(&k.PriorityClass, s.PriorityClass)
	setString(&k.Tier, s.Tier)
	if s.MaxBudget != nil {
		k.MaxBudgetNano = nanoPtr(s.MaxBudget)
	}
	if s.SoftBudget != nil {
		k.SoftBudgetNano = nanoPtr(s.SoftBudget)
	}
	setString(&k.BudgetPeriod, s.BudgetPeriod)
	if s.BudgetResetAt != nil {
		k.BudgetResetAt = s.BudgetResetAt.Time()
	}
	if s.RPMLimit != nil {
		k.RPMLimit = s.RPMLimit
	}
	if s.TPMLimit != nil {
		k.TPMLimit = s.TPMLimit
	}
	if s.MaxParallel != nil {
		k.MaxParallel = s.MaxParallel
	}
	if s.Blocked != nil {
		k.Blocked = *s.Blocked
	}
	if s.Expires != nil && s.Duration != nil {
		return badRequest("expires and duration are two ways to say the same thing; send one").withParam("duration")
	}
	if s.Expires != nil {
		k.ExpiresAt = s.Expires.Time()
	}
	if s.Duration != nil {
		d, err := parseDuration(*s.Duration)
		if err != nil {
			return badRequest("duration: %s", err).withParam("duration")
		}
		if d == 0 {
			k.ExpiresAt = time.Time{}
		} else {
			k.ExpiresAt = now.Add(d).UTC()
		}
	}
	if k.SoftBudgetNano != nil && k.MaxBudgetNano != nil && *k.SoftBudgetNano > *k.MaxBudgetNano {
		return badRequest("soft_budget must not exceed max_budget").withParam("soft_budget")
	}
	return nil
}

func setString(dst *string, src *string) {
	if src != nil {
		*dst = *src
	}
}

func setStrings(dst *[]string, src *[]string) {
	if src != nil {
		*dst = append([]string(nil), (*src)...)
	}
}

// parseDuration accepts "30d", "12h", "45m" and Go's own syntax. Days are
// included because every operator writing a key expiry thinks in days and Go's
// parser does not.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" || strings.EqualFold(s, "never") {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n < 0 {
			return 0, errors.New("expected a non-negative number of days, as in \"30d\"")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, errors.New("expected a duration such as \"30d\", \"12h\" or \"90m\"")
	}
	if d < 0 {
		return 0, errors.New("duration must not be negative")
	}
	return d, nil
}

// keyRef identifies a key for a single-object operation.
type keyRef struct {
	KeyID   string `json:"key_id"`
	TokenID string `json:"token_id"`
	// Key is accepted only so that it can be refused with an explanation. See
	// [call] keyID.
	Key string `json:"key"`
}

type keyIDs struct {
	Keys     []string `json:"keys"`
	KeyIDs   []string `json:"key_ids"`
	TokenIDs []string `json:"token_ids"`
}

// keyID resolves the key a request is about.
//
// A plaintext key is refused rather than accepted. The incumbent identifies a
// key by its own secret, which puts live credentials into URLs, access logs,
// shell history and proxy traces; dorang addresses keys by id. This is a
// deliberate, named deviation from §2.3 rather than an oversight, and it is
// reported as such instead of failing obscurely.
func (c *call) keyID(ref keyRef) (string, error) {
	if strings.TrimSpace(ref.Key) != "" || queryString(c.r, "key") != "" {
		f := badRequest("identify a key by key_id; dorang does not accept a plaintext credential " +
			"as a request parameter, because that puts a live secret into URLs and access logs")
		f.Code = "secret_in_request"
		f.Param = "key"
		return "", f
	}
	for _, v := range []string{ref.KeyID, ref.TokenID} {
		if s := strings.TrimSpace(v); s != "" {
			return s, nil
		}
	}
	if s := queryString(c.r, "key_id", "token_id"); s != "" {
		return s, nil
	}
	return "", badRequest("key_id is required").withParam("key_id")
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// POST /key/generate — mint a credential and return it exactly once.
func (c *call) keyGenerate() error {
	ks, err := c.a.keys()
	if err != nil {
		return err
	}
	h, err := c.a.hasher()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}

	var spec keySpec
	if err := decodeOptionalBody(c.w, c.r, &spec); err != nil {
		return err
	}

	now := c.a.now().UTC()
	k := &Key{
		ID:            c.a.cfg.NewID(),
		PriorityClass: "default",
		Source:        "native",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := spec.apply(k, now); err != nil {
		return err
	}
	// A scoped administrator may only mint keys on its own team. Without this
	// the scope is decorative: issue a key with team_id set to somebody else's
	// team and every later check passes, because the key really is on that
	// team by then.
	if err := c.permitTeamParam(k.TeamID); err != nil {
		return err
	}

	token, err := c.a.cfg.NewToken()
	if err != nil {
		return newFault(http.StatusInternalServerError, CodeInternal, typeAPI,
			"a credential could not be minted")
	}
	digest, scheme, err := h.Hash(token)
	if err != nil {
		// The usual cause is an absent pepper: §2.4 refuses a pepperless
		// "HMAC", which is an unsalted digest wearing a costume.
		return unimplemented(CodeDependencyOff,
			"credentials cannot be issued: the key pepper is not configured (DESIGN §2.4)")
	}
	k.HashScheme = scheme
	k.KeyLabel = h.Label(token)

	v := Verifier{Lookup: h.Lookup(token), TokenHash: digest, HashScheme: scheme}
	if err := ks.CreateKey(c.ctx(), k, v); err != nil {
		return err
	}
	c.a.metrics.keysIssued.Add(1)

	view := viewKey(k)
	// Nothing is announced here, and that is deliberate rather than an omission
	// (see [Invalidator]). A key that has never been seen has no cached copy to
	// drop; the one thing a node can hold about it is a NEGATIVE entry from a
	// request that used the credential before it existed, and those carry the
	// short negative TTL (`auth.DefaultNegativeTTL`, 5 s) precisely so that this
	// case needs no message.
	//
	// The audit row records the key that was created — never the secret. The
	// view type is the only thing in scope, so this cannot regress.
	if err := c.recordAudit("key.generate", "key", k.ID, nil, view); err != nil {
		return err
	}

	writeJSON(c.w, c.r, http.StatusOK, generatedKey{
		Key:     token,
		TokenID: k.ID,
		KeyName: k.KeyLabel,
		Expires: Stamp(k.ExpiresAt),
		Info:    view,
		Warning: returnedOnce,
	})
	return nil
}

// GET|POST /key/info
func (c *call) keyInfo() error {
	ks, err := c.a.keys()
	if err != nil {
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
	k, err := c.loadKey(ks, id)
	if err != nil {
		return err
	}
	c.hydrateSpend(k)
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"key": viewKey(k)})
	return nil
}

// POST /key/update
func (c *call) keyUpdate() error {
	ks, err := c.a.keys()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var body struct {
		keyRef
		keySpec
	}
	if err := decodeBody(c.w, c.r, &body); err != nil {
		return err
	}
	id, err := c.keyID(body.keyRef)
	if err != nil {
		return err
	}
	k, err := c.loadKey(ks, id)
	if err != nil {
		return err
	}
	before := viewKey(k)

	updated := *k
	if err := body.keySpec.apply(&updated, c.a.now()); err != nil {
		return err
	}
	// The update may MOVE the key: an unchecked team_id would let a scoped
	// administrator hand its own key to another team, or adopt one, which is
	// the scope check bypassed by writing through it rather than around it.
	if err := c.permitTeamParam(updated.TeamID); err != nil {
		return err
	}
	updated.UpdatedAt = c.a.now().UTC()
	if err := ks.UpdateKey(c.ctx(), &updated); err != nil {
		return err
	}
	after := viewKey(&updated)
	// Every field this handler writes is an authorization field the hot path
	// reads off its cached snapshot — the block flag, the expiry, the model and
	// route allow-lists, the tier, the ceilings — so an update that is not
	// announced is an update the fleet honours a cache TTL later. `blocked` is
	// singled out only in the CAUSE, because `{"blocked": true}` through this
	// route is a revocation by another name and an operator reading a node's log
	// should see it spelled that way.
	cause := CauseUpdated
	if updated.Blocked && !k.Blocked {
		cause = CauseRevoked
	}
	if err := c.applied(id, cause, "key.update", before, after); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"key": after})
	return nil
}

// POST /key/delete
func (c *call) keyDelete() error {
	ks, err := c.a.keys()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var body keyIDs
	if err := decodeBody(c.w, c.r, &body); err != nil {
		return err
	}
	ids := mergeIDs(body.Keys, body.KeyIDs, body.TokenIDs)
	if len(ids) == 0 {
		return badRequest("keys must name at least one key id").withParam("keys")
	}
	for _, id := range ids {
		if strings.HasPrefix(id, keyPrefix) {
			f := badRequest("keys must contain key ids, not plaintext credentials")
			f.Code = "secret_in_request"
			f.Param = "keys"
			return f
		}
	}

	// The before state is read first, so the audit row records what was
	// removed. A deletion whose trail says only "some keys were deleted" is a
	// trail nobody can reconcile against.
	before := make([]keyView, 0, len(ids))
	for _, id := range ids {
		// loadKey, not GetKey: a bulk delete is the easiest place to reach a
		// key outside the caller's scope, because the caller supplies the whole
		// list and nothing else in this handler looks at any single one of
		// them. An id the caller may not administer is refused rather than
		// skipped — silently dropping it from a delete list and reporting a
		// smaller "deleted" count reads like the key was already gone.
		k, err := c.loadKey(ks, id)
		if err != nil {
			var f *fault
			if errors.As(err, &f) && f.Status == http.StatusNotFound && c.scope().Global {
				continue
			}
			return err
		}
		before = append(before, viewKey(k))
	}

	n, err := ks.DeleteKeys(c.ctx(), ids)
	if err != nil {
		return err
	}

	// A deleted key is a revoked key as far as every node's cache is concerned:
	// the row is gone, nothing will refuse it, and a node holding a cached copy
	// serves it until the entry TTL expires. The announcement names the ids
	// rather than the lookups because the rows that held the lookups no longer
	// exist — which is exactly why an invalidation is authoritative on the
	// durable key id (see [Invalidator]).
	//
	// One announcement per id, and the FIRST failure is remembered rather than
	// returned: stopping half way through would leave the remaining keys of a
	// bulk delete serving on the TTL because an earlier one could not be
	// published.
	var invErr error
	for _, id := range ids {
		if err := c.invalidate(id, CauseRevoked); err != nil && invErr == nil {
			invErr = err
		}
	}
	if err := c.recordAudit("key.delete", "key", strings.Join(ids, ","),
		map[string]any{"keys": before}, map[string]any{"deleted": n}); err != nil {
		return err
	}
	if invErr != nil {
		return invErr
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"deleted":      n,
		"deleted_keys": ids,
	})
	return nil
}

// GET|POST /key/list
func (c *call) keyList() error {
	ks, err := c.a.keys()
	if err != nil {
		return err
	}
	limit, err := queryInt(c.r, "limit", c.a.cfg.ListLimit)
	if err != nil {
		return err
	}
	offset, err := queryInt(c.r, "offset", 0)
	if err != nil {
		return err
	}
	if limit <= 0 || limit > c.a.cfg.MaxListLimit {
		return badRequest("limit must be between 1 and %d", c.a.cfg.MaxListLimit).withParam("limit")
	}
	if offset < 0 {
		return badRequest("offset must not be negative").withParam("offset")
	}
	blocked, err := queryBool(c.r, "blocked")
	if err != nil {
		return err
	}
	// The team filter is the finding: it used to be read straight off the
	// request and handed to the store, so naming a team was the same as being
	// entitled to it. It is now narrowed against the caller's own scope, and
	// the result is filtered again below in case a store ignores it.
	team, err := c.scopedTeamFilter(queryString(c.r, "team_id"))
	if err != nil {
		return err
	}
	f := KeyFilter{
		UserID:  queryString(c.r, "user_id"),
		TeamID:  team,
		Blocked: blocked,
		Limit:   limit,
		Offset:  offset,
	}
	keys, err := ks.ListKeys(c.ctx(), f)
	if err != nil {
		return err
	}
	keys = c.keepKeysInScope(keys)
	// One batched lookup for the page, not one per row: a per-row query is how
	// a list endpoint becomes a store outage at the page size an operator
	// actually uses.
	c.hydrateSpend(keys...)
	out := make([]keyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, viewKey(k))
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{
		"keys":   out,
		"limit":  limit,
		"offset": offset,
		"count":  len(out),
	})
	return nil
}

// keySetBlocked builds the handlers for POST /key/block and POST /key/unblock.
// They are one implementation because they differ in exactly one boolean, and
// two copies of this would drift.
//
// `/key/block` is the incident path of OPERATIONS §3.1: it is what an operator
// reaches for when a key leaks. It therefore ANNOUNCES the block (§11.2c) rather
// than leaving the fleet to discover it when the credential cache expires — see
// [Invalidator]. The unblock announces too, for the reason a pend's release
// does: a key that starts serving again only when a TTL says so is a key the
// operator cannot un-break in one action.
func keySetBlocked(blocked bool) handler {
	return func(c *call) error {
		ks, err := c.a.keys()
		if err != nil {
			return err
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
		k, err := c.loadKey(ks, id)
		if err != nil {
			return err
		}
		before := viewKey(k)
		updated := *k
		updated.Blocked = blocked
		updated.UpdatedAt = c.a.now().UTC()
		if err := ks.UpdateKey(c.ctx(), &updated); err != nil {
			return err
		}
		after := viewKey(&updated)
		action, cause := "key.unblock", CauseUpdated
		if blocked {
			action, cause = "key.block", CauseRevoked
		}
		if err := c.applied(id, cause, action, before, after); err != nil {
			return err
		}
		writeJSON(c.w, c.r, http.StatusOK, map[string]any{"key": after})
		return nil
	}
}

// POST /key/regenerate — rotate the secret behind an existing key id.
//
// Every authorization field survives, which is the point: rotating a credential
// must not silently widen what it can do. The new secret is returned exactly
// once, exactly as at creation.
func (c *call) keyRegenerate() error {
	ks, err := c.a.keys()
	if err != nil {
		return err
	}
	h, err := c.a.hasher()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var body struct {
		keyRef
		keySpec
	}
	if err := decodeOptionalBody(c.w, c.r, &body); err != nil {
		return err
	}
	id, err := c.keyID(body.keyRef)
	if err != nil {
		return err
	}
	k, err := c.loadKey(ks, id)
	if err != nil {
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
	v := Verifier{Lookup: h.Lookup(token), TokenHash: digest, HashScheme: scheme}
	if err := ks.ReplaceVerifier(c.ctx(), id, v, label, now); err != nil {
		return err
	}

	updated := *k
	updated.KeyLabel = label
	updated.HashScheme = scheme
	updated.UpdatedAt = now
	// A rotation may carry a new expiry, which is the one field an operator
	// routinely changes at the same moment.
	if err := body.keySpec.apply(&updated, now); err != nil {
		return err
	}
	if !sameSpec(k, &updated) {
		if err := ks.UpdateKey(c.ctx(), &updated); err != nil {
			return err
		}
	}
	c.a.metrics.keysIssued.Add(1)

	after := viewKey(&updated)
	// The old secret is cut OUTRIGHT here — that is what regenerate has always
	// meant, and OPERATIONS §3.1 offers it as the incident alternative to a
	// block on the strength of "the cutover is immediate". It is immediate only
	// on the node that served the call unless the cut is announced: every other
	// node goes on verifying the compromised secret from its snapshot until the
	// entry TTL expires. `grace_cut` rather than `rotated` is the honest cause,
	// because a secret stopped working.
	if err := c.applied(id, CauseGraceCut, "key.regenerate", before, after); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, generatedKey{
		Key:     token,
		TokenID: id,
		KeyName: label,
		Expires: Stamp(updated.ExpiresAt),
		Info:    after,
		Warning: returnedOnce,
	})
	return nil
}

// sameSpec reports whether a rotation changed anything beyond the verifier,
// which ReplaceVerifier has already written.
func sameSpec(a, b *Key) bool {
	av, bv := viewKey(a), viewKey(b)
	av.KeyName, bv.KeyName = "", ""
	av.HashScheme, bv.HashScheme = "", ""
	av.UpdatedAt, bv.UpdatedAt = Stamp(time.Time{}), Stamp(time.Time{})
	as, _ := auditJSON(av)
	bs, _ := auditJSON(bv)
	return as == bs
}

func mergeIDs(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, v := range l {
			v = strings.TrimSpace(v)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// decodeOptionalBody decodes a body that may legitimately be absent — a GET, or
// a POST whose parameters are all in the query string.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return nil
	}
	return decodeBody(w, r, v)
}
