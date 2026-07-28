package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/store"
)

// A batch row is the only paid model call in this gateway that is not made by
// the request that asked for it. The request that created the batch is long
// gone — possibly by hours, possibly by a process restart — so the two things
// the interactive gate does with the caller's identity, the model allow-list
// and the budget hold, have nothing to read unless the identity is recovered.
//
// It was not recovered. The scheduler resolved each row's model against the
// process-wide target table and dispatched, which meant a key restricted to one
// model could name any model in the catalog on a JSONL line, and that batch
// spend reached no budget counter at all.
//
// This file is the recovery. [ownerResolver] turns the owning key id carried on
// every ExecRequest back into an *auth.Principal, and [execIdentity] is what a
// row must hold before it can be dispatched: it cannot be constructed without
// the allow-list decision, and constructing it is the only way to reach the
// budget hold.

// ownerTTL is how long a resolved owner is reused.
//
// It is short because it bounds the window in which a revoked or re-budgeted
// key keeps its old envelope on the batch path. It is not zero because a 50 000
// row batch would otherwise be 50 000 lookups of a row that changes rarely, and
// DESIGN §9.6 forbids a per-request store query on a hot path — a batch row is
// not the interactive hot path, but 50 000 of them are somebody's database.
const ownerTTL = 30 * time.Second

// ownerCacheMax bounds the resolver's map. Batches belong to a small number of
// keys; the cap exists so an unbounded number of distinct owners cannot grow it
// without limit.
const ownerCacheMax = 1024

// principalLoader resolves an api key id to its authorization envelope.
type principalLoader interface {
	principalByKeyID(ctx context.Context, keyID string) (*auth.Principal, error)
}

// storePrincipalLoader is the real loader.
type storePrincipalLoader struct{ st *store.Store }

// principalByKeyID implements principalLoader.
//
// It reads the same row internal/auth would have read, through the same
// conversion, so the batch path and the interactive path cannot come to
// different conclusions about the same key. Deriving a second, batch-shaped
// view of a credential's limits would be a second place for a field to go
// missing, which is precisely what R1-A recorded.
func (l *storePrincipalLoader) principalByKeyID(ctx context.Context, keyID string) (*auth.Principal, error) {
	if l == nil || l.st == nil {
		return nil, errors.New("app: no store is configured to resolve a batch owner")
	}
	k, err := l.st.GetAPIKey(ctx, keyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errOwnerGone
		}
		return nil, err
	}
	rec, err := recordFromAPIKey(k)
	if err != nil {
		return nil, err
	}
	p := rec.Principal
	return &p, nil
}

// errOwnerGone is the batch owner having been deleted between creation and
// dispatch. It is terminal: the rows belong to a credential that no longer
// exists and no retry brings it back.
var errOwnerGone = &terminalError{msg: "the credential that owns this batch no longer exists"}

// terminalError is an executor error that must not be retried. internal/batch
// asks for exactly this shape (its unexported `terminal` interface).
type terminalError struct{ msg string }

func (e *terminalError) Error() string  { return e.msg }
func (e *terminalError) Terminal() bool { return true }

// ownerResolver caches resolved batch owners for a short time.
type ownerResolver struct {
	load principalLoader
	now  func() time.Time

	mu   sync.Mutex
	ents map[string]ownerEntry
}

type ownerEntry struct {
	p       *auth.Principal
	expires time.Time
}

func newOwnerResolver(load principalLoader, now func() time.Time) *ownerResolver {
	return &ownerResolver{load: load, now: now, ents: make(map[string]ownerEntry)}
}

// resolve returns the owning principal for a key id.
func (r *ownerResolver) resolve(ctx context.Context, keyID string) (*auth.Principal, error) {
	if r == nil || r.load == nil {
		return nil, errors.New("app: batch owner cannot be resolved")
	}
	if keyID == "" {
		// A batch with no owner is one created without a principal. There is no
		// allow-list to consult and no subject to charge, and dispatching it
		// would be spending the operator's credential on behalf of nobody.
		return nil, &terminalError{msg: "this batch records no owning credential"}
	}
	now := r.now()
	r.mu.Lock()
	if e, ok := r.ents[keyID]; ok && now.Before(e.expires) {
		r.mu.Unlock()
		return e.p, nil
	}
	r.mu.Unlock()

	p, err := r.load.principalByKeyID(ctx, keyID)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if len(r.ents) >= ownerCacheMax {
		clear(r.ents)
	}
	r.ents[keyID] = ownerEntry{p: p, expires: now.Add(ownerTTL)}
	r.mu.Unlock()
	return p, nil
}

// execIdentity is a batch row's authorization envelope, resolved.
//
// The type is unexported and has no exported constructor other than
// [ownerResolver.identity], which refuses to return one for a model the owner
// may not use. That is the structural half: a row cannot reach
// batchExecutor.Execute's upstream call without holding one, and holding one
// means the allow-list said yes.
type execIdentity struct {
	p    *auth.Principal
	subs []budgetSubject
}

// identity resolves the owner of a row and authorizes the model it names.
//
// The two are one operation on purpose. Every previous arrangement had the
// identity available and the check optional; here the check is what produces
// the value the caller needs, so skipping it means having nothing to dispatch
// with.
func (r *ownerResolver) identity(ctx context.Context, keyID, model string,
	now time.Time) (*execIdentity, error) {

	p, err := r.resolve(ctx, keyID)
	if err != nil {
		return nil, err
	}
	// The kill switches first, exactly as internal/auth.use applies them: a
	// blocked or expired credential must not keep spending through a batch it
	// submitted while it was live. This is the closest thing to revocation the
	// batch path has, and it is the reason a long-running batch is not a way to
	// outlive a block.
	if err := p.Authorize(auth.Access{Now: now, Model: model}); err != nil {
		return nil, &terminalError{msg: authorizationRefusal(err)}
	}
	if !principalAllowsModel(p, model) {
		return nil, &terminalError{msg: fmt.Sprintf(
			"the credential that owns this batch is not allowed to use model %q", model)}
	}
	return &execIdentity{p: p, subs: budgetSubjectsOf(p)}, nil
}

// authorizationRefusal renders an auth refusal for a batch row's error file
// without leaking anything the caller is not entitled to know.
func authorizationRefusal(err error) string {
	var ae *auth.Error
	if errors.As(err, &ae) {
		return ae.Error()
	}
	return "the credential that owns this batch is no longer authorized"
}

// principalAllowsModel is [principal.AllowsModel] over a bare *auth.Principal.
//
// It exists because the batch path holds the auth type directly rather than the
// server-facing wrapper, and duplicating the RULE would be one more place for
// the answer to differ. The rule itself lives in modelAllowed, which both use.
func principalAllowsModel(p *auth.Principal, model string) bool {
	if p == nil {
		return false
	}
	if p.Master {
		return true
	}
	for _, l := range []*auth.Limits{&p.Key, p.User, p.Team} {
		if l == nil {
			continue
		}
		if !modelAllowed(l.Models, model) {
			return false
		}
	}
	return true
}
