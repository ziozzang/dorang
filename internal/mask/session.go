package mask

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
	"time"
)

// Sentinel opens every placeholder. It is the only string the unmasker looks
// for, and after the escape pass it is the only one the upstream ever sees.
const Sentinel = "[PII:"

// digestBytes is how much of the HMAC a placeholder carries.
//
// 64 bits. A collision means two different originals derive the same
// placeholder, which would make the unmasker restore the wrong text — so it is
// not tolerated, it is detected and fails the mask closed (see [ErrCollision]).
// At the default entry cap the probability of ever seeing one is around 10⁻¹³,
// and the alternative to 64 bits is a wider placeholder, which costs tokens on
// every masked value in every request.
const digestBytes = 8

// alphabet encodes four bits per character using letters only.
//
// Letters only is a correctness property, not a style: a placeholder contains no
// digit and no `@`, so no digit-shaped or address-shaped pattern can match one,
// and a second masking pass over already-masked text is a no-op. See the package
// documentation.
const alphabet = "abcdefghijklmnop"

// PlaceholderLen is the exact byte length of every placeholder.
//
// It is what [Unmasker] bounds its held tail by, and it is fixed so that a
// placeholder is either wholly present in a window or wholly absent.
const PlaceholderLen = len(Sentinel) + 2*digestBytes + 1

// Scope decides how far a placeholder's stability reaches — and therefore both
// how much prefix cache survives and how much a provider can link.
//
// They are the same dial. See the package documentation.
type Scope uint8

const (
	// ScopeConversation is the default: placeholders are stable across the
	// turns of one conversation and differ between conversations. The provider
	// already sees a conversation's turns together, so this leaks nothing it did
	// not have, and it is exactly the span over which a KV cache is reused.
	ScopeConversation Scope = iota
	// ScopePrincipal makes placeholders stable across everything one key sends.
	ScopePrincipal
	// ScopeTenant makes them stable across a team.
	ScopeTenant
	// ScopeRequest derives from fresh randomness, so nothing links and nothing
	// caches. A caller choosing it must disable prefix affinity for the route;
	// silently defeating §7.4b would be worse than refusing to.
	ScopeRequest
)

var scopeNames = [...]string{"conversation", "principal", "tenant", "request"}

// String returns the configuration spelling.
func (s Scope) String() string {
	if int(s) >= len(scopeNames) {
		return "unknown"
	}
	return scopeNames[s]
}

// ParseScope resolves a configuration spelling.
func ParseScope(s string) (Scope, bool) {
	for i, n := range scopeNames {
		if n == s {
			return Scope(i), true
		}
	}
	return 0, false
}

// Deterministic reports whether the scope produces placeholders that repeat
// across requests — which is to say, whether prefix caching survives it.
func (s Scope) Deterministic() bool { return s != ScopeRequest }

// Errors a mask can fail with. Every one of them fails the request closed.
var (
	// ErrTableFull reports that one request asked for more distinct
	// placeholders than the filter allows. Failing here rather than growing is
	// deliberate: an unbounded table is an unbounded memory footprint sized by
	// the caller.
	ErrTableFull = errors.New("mask: too many distinct values in one request")
	// ErrCollision reports two originals deriving the same placeholder. It is
	// astronomically unlikely and catastrophic if ignored, which is exactly the
	// combination that earns a check rather than a comment.
	ErrCollision = errors.New("mask: placeholder collision")
	// ErrNoSecret reports a deterministic scope with no cluster-wide secret.
	ErrNoSecret = errors.New("mask: a deterministic scope needs a cluster-wide secret")
)

// Config builds a [Filter]. It is config-load-time data: one Filter serves every
// request for the model it is attached to, and is replaced on reload.
type Config struct {
	// Name identifies the filter in errors and counters.
	Name string
	// Patterns are applied in order. See [NewPatternSet].
	Patterns []PatternSpec
	// Secret is the cluster-wide seed placeholders are derived from. It must be
	// the same on every node and across restarts, or the same text masks
	// differently after a failover and every backend's prefix cache cold-starts.
	// Rotating it invalidates every cache, so it is a staged, announced
	// operation.
	Secret []byte
	// Scope bounds placeholder stability.
	Scope Scope
	// MaxEntries caps distinct values per request. Zero takes DefaultMaxEntries.
	MaxEntries int
	// Retain is how long the reverse table holds a mapping. Zero takes
	// [DefaultRetain].
	//
	// It should be at least as long as the provider keeps its own conversation
	// state, because that is the case the reverse table exists for. There is no
	// setting that turns it off: erring long costs bounded memory, and erring
	// short costs a caller the gateway's own machinery in place of their text.
	// See [Vault].
	Retain time.Duration
	// MaxRetained caps the vault. Zero takes DefaultMaxRetained.
	MaxRetained int
	// Now and Rand are test seams.
	Now  func() time.Time
	Rand io.Reader
}

// DefaultMaxEntries caps distinct masked values in one request.
const DefaultMaxEntries = 1024

// Filter is a compiled, immutable filter: pattern set, derivation secret, scope,
// and the optional retention vault.
//
// It is safe for concurrent use and is shared by every request for its model.
type Filter struct {
	name    string
	set     *PatternSet
	secret  []byte
	scope   Scope
	max     int
	vault   *Vault
	rand    io.Reader
	version string
}

// New compiles a filter.
func New(cfg Config) (*Filter, error) {
	set, err := NewPatternSet(cfg.Patterns)
	if err != nil {
		return nil, err
	}
	if cfg.Scope.Deterministic() && len(cfg.Secret) == 0 {
		return nil, fmt.Errorf("%w: filter %q uses scope %s; set a cluster-wide secret "+
			"or use scope %s", ErrNoSecret, cfg.Name, cfg.Scope, ScopeRequest)
	}
	f := &Filter{
		name:   cfg.Name,
		set:    set,
		secret: append([]byte(nil), cfg.Secret...),
		scope:  cfg.Scope,
		max:    cfg.MaxEntries,
		rand:   cfg.Rand,
	}
	if f.max <= 0 {
		f.max = DefaultMaxEntries
	}
	if f.rand == nil {
		f.rand = rand.Reader
	}
	f.vault = newVault(cfg.Retain, cfg.MaxRetained, cfg.Now)

	// version identifies (pattern set ‖ secret). Two filters that agree on it
	// mask identically; two that do not must not be allowed to look alike to a
	// cache. It is a digest, so it names the secret without carrying it.
	h := hmac.New(sha256.New, f.secret)
	for _, p := range set.pats {
		writeLP(h, p.name)
		writeLP(h, p.re.String())
	}
	f.version = hex.EncodeToString(h.Sum(nil)[:8])
	return f, nil
}

// Name returns the filter's configured name.
func (f *Filter) Name() string { return f.name }

// Scope returns the configured scope.
func (f *Filter) Scope() Scope { return f.scope }

// Patterns lists the pattern names in application order.
func (f *Filter) Patterns() []string { return f.set.Names() }

// Version identifies the derivation — pattern set and secret — without carrying
// either.
//
// It belongs in any cache identity that a masked body feeds, because an operator
// who edits the pattern set between two turns of one conversation changes what
// gets masked, and a cache claim made under the old set is then a claim about
// bytes the backend never saw. Changing this must *miss* a cache, never corrupt
// a claim.
func (f *Filter) Version() string { return f.version }

// Vault returns the retention vault, or nil when retention is off.
func (f *Filter) Vault() *Vault { return f.vault }

// SessionOptions parameterises one request's session.
type SessionOptions struct {
	// Salt is the scope's identity: a session id for ScopeConversation, a key id
	// for ScopePrincipal, a team id for ScopeTenant. It is ignored — and must
	// be — for ScopeRequest.
	//
	// It is mixed into the derivation, never stored, and never leaves the
	// process.
	Salt string
}

// Session is one request's mask.
//
// It has no exported field, and its String, GoString and MarshalJSON are all
// redacted, because the way key material reaches a log is a struct print.
type Session struct {
	f     *Filter
	scope Scope
	tab   *tables
}

// tables holds everything a Session must not print. Keeping it behind a pointer
// is what lets String have a value receiver — so both Session and *Session
// satisfy fmt.Stringer, and a copied Session prints the same redacted summary
// rather than its innards.
type tables struct {
	mu  sync.Mutex
	h   hash.Hash
	fwd map[string]string // original → placeholder
	rev map[string]string // placeholder → original
	// scopeTag names this session's scope in the retention vault. It is a
	// digest of the scope key, so it identifies the scope without carrying the
	// salt — which may itself be caller text — and it is what stops a
	// placeholder from one conversation resolving inside another.
	scopeTag string
	perPat   []int
	stats    Stats
	failed   bool
	buf      []byte
}

// Session starts one request's session.
func (f *Filter) Session(o SessionOptions) (*Session, error) {
	salt := o.Salt
	if f.scope == ScopeRequest {
		var b [16]byte
		if _, err := io.ReadFull(f.rand, b[:]); err != nil {
			return nil, fmt.Errorf("mask: %s: %w", f.name, err)
		}
		salt = string(b[:])
	}

	// The scope key is derived once per request; each value then costs one
	// HMAC over the value alone.
	k := hmac.New(sha256.New, f.secret)
	writeLP(k, f.scope.String())
	writeLP(k, salt)
	key := k.Sum(nil)

	s := &Session{
		f:     f,
		scope: f.scope,
		tab: &tables{
			h:        hmac.New(sha256.New, key),
			fwd:      make(map[string]string, 8),
			rev:      make(map[string]string, 8),
			scopeTag: hex.EncodeToString(key[:8]),
			perPat:   make([]int, len(f.set.pats)),
			buf:      make([]byte, 0, sha256.Size),
		},
	}
	return s, nil
}

// writeLP writes a length-prefixed string so that concatenation is injective:
// ("ab","c") and ("a","bc") must not hash alike.
func writeLP(h io.Writer, s string) {
	var n [4]byte
	l := len(s)
	n[0] = byte(l >> 24)
	n[1] = byte(l >> 16)
	n[2] = byte(l >> 8)
	n[3] = byte(l)
	_, _ = h.Write(n[:])
	_, _ = io.WriteString(h, s)
}

// derive computes the placeholder for a value. Same value, same scope, same
// secret ⇒ same placeholder, on every node, for ever.
func (t *tables) derive(value string) string {
	t.h.Reset()
	_, _ = io.WriteString(t.h, value)
	t.buf = t.h.Sum(t.buf[:0])

	var out [PlaceholderLen]byte
	copy(out[:], Sentinel)
	p := len(Sentinel)
	for _, b := range t.buf[:digestBytes] {
		out[p] = alphabet[b>>4]
		out[p+1] = alphabet[b&0x0f]
		p += 2
	}
	out[p] = ']'
	return string(out[:])
}

// issue returns the placeholder for original, minting it if this request has not
// seen the value yet.
func (t *tables) issue(original string, max int, vault *Vault) (string, error) {
	if p, ok := t.fwd[original]; ok {
		return p, nil
	}
	if len(t.fwd) >= max {
		t.failed = true
		return "", fmt.Errorf("%w: the limit is %d", ErrTableFull, max)
	}
	ph := t.derive(original)
	if prev, ok := t.rev[ph]; ok && prev != original {
		t.failed = true
		return "", ErrCollision
	}
	t.fwd[original] = ph
	t.rev[ph] = original
	t.stats.Issued++
	if vault != nil {
		vault.put(t.scopeTag, ph, original)
	}
	return ph, nil
}

// Mask replaces every match in text and returns the masked text and how many
// values it replaced.
//
// A non-nil error means the mask did not happen and the caller must refuse the
// request (DESIGN §10.5b: a failed mask fails closed). The returned string is
// not usable in that case.
func (s Session) Mask(text string) (string, int, error) {
	if text == "" {
		return text, 0, nil
	}
	t := s.tab
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed {
		return "", 0, fmt.Errorf("mask: %s: the session already failed", s.f.name)
	}

	set := s.f.set
	locs := set.re.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		return text, 0, nil
	}

	var b strings.Builder
	b.Grow(len(text) + len(locs)*PlaceholderLen)
	last, n := 0, 0
	for _, m := range locs {
		lo, hi := m[0], m[1]
		if lo < last {
			continue
		}
		ph, err := t.issue(text[lo:hi], s.f.max, s.f.vault)
		if err != nil {
			return "", 0, fmt.Errorf("mask: %s: %w", s.f.name, err)
		}
		b.WriteString(text[last:lo])
		b.WriteString(ph)
		last = hi
		n++
		if i := set.which(m); i >= 0 {
			t.perPat[i]++
		}
	}
	b.WriteString(text[last:])
	return b.String(), n, nil
}

// MaskValue masks one exact value and returns its placeholder.
//
// It is the escape hatch for a rule no regular expression can state — a filter
// plugin that knows the shape of its operator's customer numbers, or that only
// masks an address when it appears next to a name. The value is masked whole:
// this is not a pattern, it is a decision the plugin already made.
func (s Session) MaskValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	t := s.tab
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed {
		return "", fmt.Errorf("mask: %s: the session already failed", s.f.name)
	}
	ph, err := t.issue(value, s.f.max, s.f.vault)
	if err != nil {
		return "", fmt.Errorf("mask: %s: %w", s.f.name, err)
	}
	return ph, nil
}

// which reports the index of the pattern that produced a match, or -1 for the
// built-in sentinel escape.
func (s *PatternSet) which(m []int) int {
	for i := range s.pats {
		g := 2 * s.pats[i].group
		if g < len(m) && m[g] >= 0 {
			return i
		}
	}
	return -1
}

// wellFormed reports whether a placeholder starts at i: the sentinel, exactly
// 2*digestBytes alphabet characters, and the closing bracket. Nothing else is a
// placeholder, which is what makes "echoed inside a longer token" not a match.
func wellFormed(s string, i int) bool {
	if i < 0 || i+PlaceholderLen > len(s) {
		return false
	}
	if s[i:i+len(Sentinel)] != Sentinel {
		return false
	}
	if s[i+PlaceholderLen-1] != ']' {
		return false
	}
	for k := i + len(Sentinel); k < i+PlaceholderLen-1; k++ {
		c := s[k]
		if c < 'a' || c > 'p' {
			return false
		}
	}
	return true
}

// lookup resolves a placeholder against this request's table, then the retention
// vault when the flow has provider-held state.
func (s Session) lookup(ph string) (string, bool) {
	t := s.tab
	if v, ok := t.rev[ph]; ok {
		return v, true
	}
	if s.f.vault != nil {
		return s.f.vault.get(t.scopeTag, ph)
	}
	return "", false
}

// Unmask restores a complete text. It is [Unmasker] over one chunk.
func (s Session) Unmask(text string) string {
	u := s.Unmasker()
	out := u.Write(text)
	return out + u.Flush()
}

// Stats returns the counts. They are the only thing about a session that may be
// logged, exported, or put in a metric.
func (s Session) Stats() Stats {
	t := s.tab
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.stats
	st.Patterns = make([]PatternCount, len(t.perPat))
	for i := range t.perPat {
		st.Patterns[i] = PatternCount{Name: s.f.set.pats[i].name, Count: t.perPat[i]}
	}
	return st
}

// Failed reports whether the session has failed. A failed session masks nothing
// further; the request it belongs to must be refused.
func (s Session) Failed() bool {
	t := s.tab
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failed
}

// Stats are one session's counts.
type Stats struct {
	// Issued is how many distinct values were masked.
	Issued int
	// Resolved is how many placeholders were restored in the answer.
	Resolved int
	// Unresolved is how many placeholder-shaped strings appeared in the answer
	// that this request did not issue: echoed inside a longer token, truncated,
	// replayed from another scope, or invented. DESIGN §10.5b calls it "a signal
	// worth seeing" and it is the reason the unmasker counts instead of
	// shrugging.
	Unresolved int
	// Patterns breaks Issued down by pattern name.
	Patterns []PatternCount
}

// PatternCount is how many values one pattern matched.
type PatternCount struct {
	Name  string
	Count int
}

// String renders a session without its contents.
func (s Session) String() string {
	if s.tab == nil {
		return "mask.Session(uninitialised)"
	}
	t := s.tab
	t.mu.Lock()
	defer t.mu.Unlock()
	return fmt.Sprintf("mask.Session(filter=%s scope=%s issued=%d resolved=%d unresolved=%d table=redacted)",
		s.f.name, s.scope, t.stats.Issued, t.stats.Resolved, t.stats.Unresolved)
}

// GoString renders a session without its contents, for %#v.
func (s Session) GoString() string { return s.String() }

// MarshalJSON renders a session without its contents, so that a session which
// finds its way into a structured log line or a trace payload carries counts
// rather than the table.
func (s Session) MarshalJSON() ([]byte, error) {
	if s.tab == nil {
		return []byte(`{"filter":"","table":"redacted"}`), nil
	}
	t := s.tab
	t.mu.Lock()
	defer t.mu.Unlock()
	return []byte(fmt.Sprintf(`{"filter":%q,"scope":%q,"issued":%d,"resolved":%d,"unresolved":%d,"table":"redacted"}`,
		s.f.name, s.scope, t.stats.Issued, t.stats.Resolved, t.stats.Unresolved)), nil
}

// Salt builds a scope salt out of parts. It is a digest, so the parts — which
// may themselves be caller text — do not survive into the salt.
func Salt(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		writeLP(h, p)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
