package router

import (
	"sync/atomic"

	"github.com/ziozzang/dorang/internal/capacity"
)

// Rotation is how a credential is chosen among the ones a deployment may use
// (`key_rotation.strategy`).
//
// It is a different axis from [Strategy]. A Strategy orders DEPLOYMENTS — which
// backend serves this request — and a Rotation orders the CREDENTIALS of the one
// that won. The two vocabularies do not overlap and never did: `least_used` is
// not a deployment strategy and `lowest_cost` is not a rotation, so folding them
// into one list would only have made both wrong.
//
// It is a hint, not an override. The chosen credential is offered to
// internal/capacity as the preferred candidate, which tries it first and then
// spills through the rest in configuration order (§5.3). A rotation therefore
// changes which account is tried first and never which accounts are eligible —
// a saturated "least used" account still yields to the next one.
type Rotation string

// The rotation strategies, spelled exactly as internal/config validates them.
const (
	// RotationFailover always prefers the first configured credential and moves
	// on only when it has no room. It is the behaviour every strategy used to
	// get, because none of them was read.
	RotationFailover Rotation = "failover"
	// RotationRoundRobin advances one credential per request, per deployment.
	RotationRoundRobin Rotation = "round_robin"
	// RotationLeastUsed prefers the credential with the fewest reservations
	// outstanding on its own key axis.
	RotationLeastUsed Rotation = "least_used"
	// RotationRandom picks uniformly.
	RotationRandom Rotation = "random"
)

var allRotations = [...]Rotation{
	RotationFailover, RotationRoundRobin, RotationLeastUsed, RotationRandom,
}

// ParseRotation decodes a configured rotation strategy name.
func ParseRotation(s string) (Rotation, bool) {
	for _, k := range allRotations {
		if Rotation(s) == k {
			return k, true
		}
	}
	return "", false
}

// Rotation reports the credential-rotation strategy in effect. Diagnostics and
// assembly tests only — it is what lets a caller assert that the configured
// strategy actually reached the router, which is the edge that did not exist.
func (r *Router) Rotation() Rotation { return r.cfg.Rotation }

// rotationState is the per-deployment cursor round_robin advances. It is indexed
// by the router-wide deployment index, like rrState, so it needs no map and no
// lock.
type rotationState struct {
	cursor []atomic.Uint64
}

func (s *rotationState) next(ridx int) uint64 {
	if ridx < 0 || ridx >= len(s.cursor) {
		return 0
	}
	return s.cursor[ridx].Add(1) - 1
}

// rotate picks the credential to prefer among creds.
//
// It runs only when nothing more specific has already chosen: a credential pin
// and a sticky entry both outrank a rotation, because they are statements about
// THIS conversation and a rotation is a statement about load. Returning "" leaves
// the broker's own order — the preferred entry first, then declaration order —
// which is exactly what failover means.
func (r *Router) rotate(d *Deployment, creds []capacity.Candidate) string {
	if len(creds) < 2 {
		return ""
	}
	switch r.cfg.Rotation {
	case RotationRoundRobin:
		return creds[int(r.rot.next(d.ridx)%uint64(len(creds)))].ID
	case RotationRandom:
		i := int(r.rnd() * float64(len(creds)))
		if i >= len(creds) {
			i = len(creds) - 1
		}
		return creds[i].ID
	case RotationLeastUsed:
		// One lock acquisition for the whole pool rather than one per
		// credential: this runs on the routing hot path for every candidate
		// deployment, and a mutex round trip per account would make the
		// least-used strategy the most expensive one to have configured.
		return r.deps.Capacity.LeastUsedKey(d.Provider, creds)
	}
	return ""
}
