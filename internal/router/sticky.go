package router

import (
	"sync"
	"time"
)

// stickyKey is (tenant, group, session) with tenant leading, so two tenants can
// never share a pin even when they use the same session id (DESIGN §7.4a).
type stickyKey struct {
	tenant  string
	group   string
	session string
}

func (k stickyKey) zero() bool { return k.session == "" }

type stickyEntry struct {
	deployment string
	credential string
	created    time.Time
}

// stickyStore holds session pins.
//
// Entries expire from CREATION, not from last use. That is the whole point:
// the pin is a bet that the upstream cache still holds this conversation, and
// the cache is gone once the TTL elapses whether or not the caller kept asking.
// Refreshing on use would keep pinning traffic to a backend whose cache went
// cold, which is exactly the outcome the TTL exists to prevent — and it is the
// opposite of internal/prefix, where refreshing on use tracks reality because
// each use re-warms the cache it is betting on.
type stickyStore struct {
	mu  sync.RWMutex
	m   map[stickyKey]stickyEntry
	ttl time.Duration
	now func() time.Time
}

func newStickyStore(ttl time.Duration, now func() time.Time) *stickyStore {
	return &stickyStore{m: make(map[stickyKey]stickyEntry), ttl: ttl, now: now}
}

// get returns a live pin. An expired entry is a miss and is not resurrected.
func (s *stickyStore) get(k stickyKey) (stickyEntry, bool) {
	if s == nil || k.zero() {
		return stickyEntry{}, false
	}
	s.mu.RLock()
	e, ok := s.m[k]
	s.mu.RUnlock()
	if !ok {
		return stickyEntry{}, false
	}
	if s.expired(e) {
		s.discard(k)
		return stickyEntry{}, false
	}
	return e, true
}

func (s *stickyStore) expired(e stickyEntry) bool {
	return s.ttl > 0 && !s.now().Before(e.created.Add(s.ttl))
}

// put creates a pin if there is none, and leaves an existing live pin's
// creation time alone. Overwriting the creation time on every use would turn
// creation-time expiry into last-use expiry by the back door.
func (s *stickyStore) put(k stickyKey, deployment, credential string) {
	if s == nil || k.zero() {
		return
	}
	now := s.now()
	s.mu.Lock()
	if e, ok := s.m[k]; ok && !(s.ttl > 0 && !now.Before(e.created.Add(s.ttl))) {
		if e.deployment == deployment && e.credential == credential {
			s.mu.Unlock()
			return
		}
	}
	s.m[k] = stickyEntry{deployment: deployment, credential: credential, created: now}
	s.mu.Unlock()
}

// discard drops a pin. An unhealthy or exhausted target discards it
// immediately (§7.4a) rather than letting the TTL run out while every request
// in the session is routed at a backend that is not answering.
func (s *stickyStore) discard(k stickyKey) {
	if s == nil || k.zero() {
		return
	}
	s.mu.Lock()
	delete(s.m, k)
	s.mu.Unlock()
}

// purge drops every expired entry. It is the periodic sweep behind
// routing.sticky.purge_interval; lookups already expire lazily, so this bounds
// memory for sessions that are never asked about again.
func (s *stickyStore) purge() int {
	if s == nil {
		return 0
	}
	n := 0
	s.mu.Lock()
	for k, e := range s.m {
		if s.expired(e) {
			delete(s.m, k)
			n++
		}
	}
	s.mu.Unlock()
	return n
}

func (s *stickyStore) len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}
