package openai

import "sync"

// MaxToolNameLen is the OpenAI limit on a tool name (COMPATIBILITY 5.3).
const MaxToolNameLen = 64

// hashHexLen is how many hex digits of the full-name hash are appended.
const hashHexLen = 8

// ToolNames records the shortening applied to over-long tool names so that a
// call coming back can be matched to the tool the caller declared.
//
// COMPATIBILITY 5.3: truncation alone is not enough. If dorang sends
// "a_very_long_tool_name…" truncated and the model calls the truncated name,
// the caller — who never saw the truncated name — cannot match the call to its
// tool, and the agentic loop stalls on turn two with no error anywhere.
//
// A ToolNames is safe for concurrent use so that one instance can be shared by
// the request encoder and the response decoder of a streaming exchange, which
// run on different goroutines.
type ToolNames struct {
	mu      sync.RWMutex
	toShort map[string]string
	toLong  map[string]string
}

// NewToolNames returns an empty mapping.
func NewToolNames() *ToolNames { return &ToolNames{} }

// Shorten returns a name of at most [MaxToolNameLen] bytes, recording the
// mapping when it had to shorten. Names that already fit are returned unchanged
// and cost nothing.
//
// The short form is prefix + '_' + 8 hex digits of a hash of the FULL name, so
// it is stable for a given input and two names sharing a prefix do not collide.
// A hash collision — which needs the same 55-byte prefix and the same 32 bits —
// is resolved by perturbing the hash, at the cost of the short form no longer
// being derivable without this map. The map is authoritative for restoring, so
// that is a cost and not a correctness problem.
func (t *ToolNames) Shorten(name string, warn WarnFunc) string {
	if len(name) <= MaxToolNameLen {
		return name
	}
	if t == nil {
		// No mapping to record into. Shortening here would produce a name that
		// can never be restored, which is worse than a name the upstream will
		// reject with a clear message.
		return name
	}

	t.mu.RLock()
	short, ok := t.toShort[name]
	t.mu.RUnlock()
	if ok {
		return short
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if short, ok := t.toShort[name]; ok {
		return short
	}
	if t.toShort == nil {
		t.toShort = make(map[string]string, 4)
		t.toLong = make(map[string]string, 4)
	}
	prefix := truncateRunes(name, MaxToolNameLen-hashHexLen-1)
	for salt := uint64(0); ; salt++ {
		cand := prefix + "_" + hex8(fnv1a(name)+salt*0x9e3779b97f4a7c15)
		if have, clash := t.toLong[cand]; !clash || have == name {
			t.toShort[name] = cand
			t.toLong[cand] = name
			warn.warn(WarnToolNameTruncated, name)
			return cand
		}
	}
}

// Restore maps a shortened name back to the caller's original. Unknown names
// are returned unchanged, which is the common case: most tool names fit.
func (t *ToolNames) Restore(short string) string {
	if t == nil || short == "" {
		return short
	}
	t.mu.RLock()
	long, ok := t.toLong[short]
	t.mu.RUnlock()
	if ok {
		return long
	}
	return short
}

// Len reports how many names were shortened.
func (t *ToolNames) Len() int {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.toShort)
}

// truncateRunes cuts s to at most n bytes without splitting a UTF-8 sequence.
// A split sequence would produce an invalid-UTF-8 tool name, which some
// backends reject and others accept and then echo back mangled.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// fnv1a is FNV-1a/64, hand-rolled to keep the hot path free of an interface
// call and an allocation.
func fnv1a(s string) uint64 {
	const offset = 14695981039346656037
	const prime = 1099511628211
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

const hexDigits = "0123456789abcdef"

func hex8(h uint64) string {
	var b [hashHexLen]byte
	for i := hashHexLen - 1; i >= 0; i-- {
		b[i] = hexDigits[h&0xf]
		h >>= 4
	}
	return string(b[:])
}
