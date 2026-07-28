package app

import (
	"sync"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
)

// rateMeter counts requests and tokens per subject per minute, and is what
// makes rpm_limit and tpm_limit mean anything.
//
// The two ceilings were stored, imported, administered and documented, and the
// only two constructors of auth.Access left the observed counters at zero — so
// every comparison was `0 >= limit`, false for every positive limit. The check
// was even unit-tested, by a test that assigned the observed values itself. A
// test that supplies the input it is testing the handling of cannot notice that
// nothing supplies it in production.
//
// # Why a tumbling minute rather than a sliding window
//
// "Requests per minute" is compared against a counter that resets on the minute
// boundary, which is what internal/quota's own counter does (minuteOf) and what
// every provider's published rate limit means in practice. A sliding window
// would be more even and would cost a ring buffer per subject; the ceiling
// exists to stop runaway spend, not to shape traffic to the millisecond.
//
// # Why the map is dropped whole
//
// Eviction is the minute rollover: the previous minute's map is discarded and a
// new one starts empty. Memory is therefore bounded by the number of distinct
// subjects seen in ONE minute, with no sweeper, no TTL per entry and no way for
// a flood of distinct ids to accumulate — which matters because a subject id
// reaching here has already authenticated, but a compromised key can still
// present many user and team ids over time.
type rateMeter struct {
	now func() time.Time

	mu     sync.Mutex
	minute int64
	counts map[string]*rateCount
}

type rateCount struct {
	requests int64
	tokens   int64
}

func newRateMeter(now func() time.Time) *rateMeter {
	if now == nil {
		now = time.Now
	}
	return &rateMeter{now: now, counts: make(map[string]*rateCount)}
}

// ObservedRates implements auth.RateSource.
func (m *rateMeter) ObservedRates(kind, id string) (rpm, tpm int64) {
	if m == nil || id == "" {
		return 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollLocked()
	if c, ok := m.counts[kind+":"+id]; ok {
		return c.requests, c.tokens
	}
	return 0, 0
}

// record adds one request and/or some tokens to every subject of a principal.
//
// All three subjects are counted, not just the key. A team's requests-per-
// minute ceiling is a statement about the team, so every key under it has to
// increment the same counter or the ceiling is per-key again — the same shape
// as the budget defect one file over.
func (m *rateMeter) record(p *auth.Principal, requests, tokens int64) {
	if m == nil || p == nil || (requests == 0 && tokens == 0) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollLocked()
	for _, s := range []struct {
		kind string
		id   string
		on   bool
	}{
		{"key", p.KeyID, true},
		{"user", p.UserID, p.User != nil},
		{"team", p.TeamID, p.Team != nil},
	} {
		if !s.on || s.id == "" {
			continue
		}
		k := s.kind + ":" + s.id
		c := m.counts[k]
		if c == nil {
			c = &rateCount{}
			m.counts[k] = c
		}
		c.requests += requests
		c.tokens += tokens
	}
}

// rollLocked discards the previous minute's counts.
func (m *rateMeter) rollLocked() {
	min := m.now().Unix() / 60
	if min == m.minute {
		return
	}
	m.minute = min
	// A fresh map rather than clear(): a minute that saw a hundred thousand
	// subjects should not leave a hundred thousand buckets of capacity behind
	// for the next one.
	m.counts = make(map[string]*rateCount)
}
