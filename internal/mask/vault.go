package mask

import (
	"fmt"
	"sync"
	"time"
)

// DefaultMaxRetained caps the retention vault.
//
// At roughly 200 bytes an entry this is about 13 MB — less than a hundred
// in-flight 40k-token request bodies, which dorang holds without comment. The
// vault is cheap and it is sized to be useful rather than to be small.
const DefaultMaxRetained = 65536

// DefaultRetain is the reverse table's lifetime when the operator does not name
// one. It wants to be at least as long as the provider keeps its own
// conversation state, and erring long costs memory while erring short costs a
// wrong answer, so the default errs long.
const DefaultRetain = time.Hour

// Vault is the retained reverse table: placeholder → original, in memory, for a
// while.
//
// # Why derivation is not enough
//
// Derivation makes the *forward* direction reproducible — the same value masks
// to the same placeholder on any node at any time, which is what keeps the
// upstream bytes identical and the backend's KV cache alive. It cannot make the
// *reverse* direction work when the plaintext is not in the request, because an
// HMAC does not invert. That happens whenever the conversation state lives on
// the provider's side (the Responses API with previous_response_id, or any
// provider-held session) and, more generally, any time the model quotes a
// placeholder whose source turn was not resent.
//
// So the two mechanisms are not alternatives; they solve opposite directions and
// both are needed.
//
// # Why the table is retained and the secret is replicated, not the other way round
//
// This table is node-local, and dorang runs more than one node. A conversation
// that lands on a different node than the one that masked it finds an empty
// vault — which costs an unresolved placeholder, counted and visible. If
// *derivation* were node-local instead, that same failover would change every
// placeholder, change the upstream bytes, and kill the prefix cache silently.
// So the thing that is replicated is one small constant that an operator sets
// once and rotates deliberately, and the thing that is node-local is the table:
// unbounded, full of plaintext, and different on every request.
//
// # What is not negotiable
//
// DESIGN §10.5b's rule that survives whole is *durability*, not lifetime: this
// table is memory only. It has no store, no file, no serialization that could
// acquire one by accident, and its String, GoString and MarshalJSON are
// redacted. It is bounded by entry count, and eviction is stated rather than
// emergent: expired entries first, then oldest-inserted. A rising Evicted count
// beside a rising Unresolved count is the signal that the cap is too small for
// the traffic, which is why both are counted.
type Vault struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	now   func() time.Time
	m     map[vaultKey]vaultEntry
	order []vaultKey
	stats VaultStats
}

// vaultKey scopes an entry.
//
// Keying by placeholder alone would be a cross-conversation leak with a
// plausible attack: placeholders are stable, so a caller who has seen one from
// another conversation could ask the model to emit it — not send it, emit it,
// since anything they send is escaped — and the unmasker would resolve it out of
// the shared table. Scoping the key means a placeholder only resolves inside the
// scope that issued it, which is the same boundary the derivation already draws.
type vaultKey struct {
	scope       string
	placeholder string
}

type vaultEntry struct {
	value string
	exp   time.Time
}

// VaultStats are the vault's counts. Like everything else exported from this
// package they are arithmetic: safe for a metric, a log line, or an event.
type VaultStats struct {
	Stored   int
	Hits     int
	Misses   int
	Expired  int
	Evicted  int
	Replaced int
}

func newVault(ttl time.Duration, max int, now func() time.Time) *Vault {
	if ttl <= 0 {
		ttl = DefaultRetain
	}
	if max <= 0 {
		max = DefaultMaxRetained
	}
	if now == nil {
		now = time.Now
	}
	return &Vault{
		ttl: ttl,
		max: max,
		now: now,
		m:   make(map[vaultKey]vaultEntry),
	}
}

// TTL returns the configured lifetime.
func (v *Vault) TTL() time.Duration { return v.ttl }

// Len reports how many mappings are held.
func (v *Vault) Len() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.m)
}

// Stats returns the counts.
func (v *Vault) Stats() VaultStats {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stats
}

func (v *Vault) put(scope, ph, value string) {
	k := vaultKey{scope: scope, placeholder: ph}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if e, ok := v.m[k]; ok {
		// Same scope and placeholder means the same value — derivation is a
		// function — so this is a refresh, not a conflict. Refreshing is the
		// point: an active conversation keeps its own entries alive.
		e.exp = now.Add(v.ttl)
		v.m[k] = e
		v.stats.Replaced++
		return
	}
	if len(v.m) >= v.max {
		v.sweep(now)
	}
	for len(v.m) >= v.max && len(v.order) > 0 {
		oldest := v.order[0]
		v.order = v.order[1:]
		if _, ok := v.m[oldest]; ok {
			delete(v.m, oldest)
			v.stats.Evicted++
		}
	}
	v.m[k] = vaultEntry{value: value, exp: now.Add(v.ttl)}
	v.order = append(v.order, k)
	v.stats.Stored++
}

func (v *Vault) get(scope, ph string) (string, bool) {
	k := vaultKey{scope: scope, placeholder: ph}
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.m[k]
	if !ok {
		v.stats.Misses++
		return "", false
	}
	if !v.now().Before(e.exp) {
		delete(v.m, k)
		v.stats.Expired++
		v.stats.Misses++
		return "", false
	}
	v.stats.Hits++
	return e.value, true
}

// sweep drops expired entries. It runs on insert pressure rather than on a
// timer: a background goroutine per filter would be a goroutine per
// configuration reload, and a vault nobody is writing to has no work to do.
func (v *Vault) sweep(now time.Time) {
	kept := v.order[:0]
	for _, k := range v.order {
		e, ok := v.m[k]
		if !ok {
			continue
		}
		if !now.Before(e.exp) {
			delete(v.m, k)
			v.stats.Expired++
			continue
		}
		kept = append(kept, k)
	}
	v.order = kept
}

// String renders the vault without its contents.
func (v *Vault) String() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return fmt.Sprintf("mask.Vault(ttl=%s entries=%d/%d contents=redacted)", v.ttl, len(v.m), v.max)
}

// GoString renders the vault without its contents, for %#v.
func (v *Vault) GoString() string { return v.String() }

// MarshalJSON renders the vault without its contents.
func (v *Vault) MarshalJSON() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return []byte(fmt.Sprintf(`{"ttl":%q,"entries":%d,"contents":"redacted"}`, v.ttl.String(), len(v.m))), nil
}
