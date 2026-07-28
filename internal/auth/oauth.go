package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OAuth credentials refresh themselves (DESIGN §11.2b).
//
// The feature is small; the mechanics are the point. A refresh happens ahead of
// expiry so it never sits on a request's critical path, exactly once per
// credential however many requests want it, writes the shared store atomically,
// and fails by standing the credential aside rather than by hammering an auth
// server. None of it ever puts a token in a log, an error or a snapshot.

// Defaults for an OAuth credential.
const (
	// DefaultRefreshMargin is how far ahead of expiry a token is renewed
	// (DESIGN §11.2b's refresh_margin).
	DefaultRefreshMargin = 5 * time.Minute
	// DefaultRefreshPoll is how often the background loop looks at the clock.
	DefaultRefreshPoll = 30 * time.Second
	// DefaultBackoffBase is the first wait after a failed refresh.
	DefaultBackoffBase = time.Second
	// DefaultBackoffCeiling caps the wait. Retrying a failed refresh in a tight
	// loop is how a recoverable expiry becomes a rate-limit ban.
	DefaultBackoffCeiling = 5 * time.Minute
)

// Health reasons. They are classifications, not messages: what leaves this
// subsystem is the credential's opaque id and its health, and a free-form
// message is where a token would eventually end up.
const (
	// OAuthRefreshFailed: the provider refused or could not be reached.
	OAuthRefreshFailed = "refresh_failed"
	// OAuthNoRefreshToken: there is nothing to exchange.
	OAuthNoRefreshToken = "no_refresh_token" // pragma: allowlist secret — an error code, not a credential
	// OAuthNoRefresher: the credential has no Refresher configured.
	OAuthNoRefresher = "no_refresher" // pragma: allowlist secret — an error code, not a credential
	// OAuthStoreUnreadable: the token store could not be read.
	OAuthStoreUnreadable = "token_store_unreadable"
	// OAuthStoreWriteFailed: the refreshed token could not be persisted.
	OAuthStoreWriteFailed = "token_store_write_failed"
)

// OAuth errors.
var (
	// ErrNoToken reports a credential that has no access token at all.
	ErrNoToken = errors.New("auth: oauth credential has no access token")
	// ErrTokenExpired reports an access token past its expiry. It is distinct
	// from ReasonExpired, which is about a client's key, not a provider's.
	ErrTokenExpired = errors.New("auth: oauth access token has expired")
	// ErrNoRefreshToken reports a credential with nothing to exchange.
	ErrNoRefreshToken = errors.New("auth: oauth credential has no refresh token")
	// ErrRefreshFailed reports a failed exchange. The provider's own error is
	// deliberately not wrapped: its text is outside this package's control and
	// could carry token material.
	ErrRefreshFailed = errors.New("auth: oauth refresh failed")
	// ErrBackingOff reports a refresh declined because the credential is
	// waiting out an earlier failure.
	ErrBackingOff = errors.New("auth: oauth refresh is backing off after an earlier failure")
	// ErrNoRefresher reports a credential with no Refresher.
	ErrNoRefresher = errors.New("auth: oauth credential has no refresher")
	// ErrDuplicateCredential reports two credentials with one id.
	ErrDuplicateCredential = errors.New("auth: an oauth credential with that id already exists")
)

// Refresher exchanges a refresh token for a new token set.
//
// Implementations live with their providers; this package ships only
// [FakeRefresher]. An implementation must not put token material into an error
// — [OAuthCredential] scrubs the tokens it knows about from anything it
// records, but it cannot scrub a token it has never seen.
type Refresher interface {
	// Refresh returns the successor to prev. It runs off the request path and
	// must honor the context deadline.
	Refresh(ctx context.Context, prev Token) (Token, error)
}

// OAuthConfig configures one credential (DESIGN §11.2b):
//
//	credentials:
//	  - id: plan-oauth-1
//	    provider: some-plan
//	    auth: oauth
//	    oauth:
//	      source: file
//	      path: ~/.some-vendor/auth.json
//	      refresh_margin: 5m
//	      account_id_field: account_id
type OAuthConfig struct {
	// ID is the credential's opaque id. It is the only identifier that leaves
	// this subsystem, and it is stable across every refresh: a conversation
	// pinned to this account stays pinned, because the account is the same
	// account and the token is an implementation detail of talking to it
	// (DESIGN §7.4a2).
	ID string
	// Provider names the provider kind, for reporting.
	Provider string
	// Source selects where the token is read from.
	Source TokenSource
	// Path is the store, for SourceFile. A leading ~/ is expanded.
	Path string
	// Command is the argv to run, for SourceExec.
	Command []string
	// EnvVar is the variable to read, for SourceEnv.
	EnvVar string
	// StoreFormat names a vendor's store layout. Empty is [FormatGeneric].
	//
	// It is spelled StoreFormat rather than Format because this type carries a
	// Format METHOD, which is what keeps a %v of a configuration from printing a
	// path to a credential file.
	StoreFormat StoreFormat
	// Fields names the store's keys where they are not the format's. Each name
	// set here overrides the format's own.
	Fields TokenFields
	// AccountHeader is the header the account id is sent in, where the provider
	// requires one. Empty means the account id is not sent.
	AccountHeader string
	// RefreshMargin is how far ahead of expiry to renew.
	RefreshMargin time.Duration
	// PollInterval is how often the background loop checks. It is clamped to at
	// most a quarter of RefreshMargin, so the margin cannot be slept through.
	PollInterval time.Duration
	// BackoffBase and BackoffCeiling bound retries after a failure.
	BackoffBase, BackoffCeiling time.Duration
	// ExecTimeout bounds a SourceExec command.
	ExecTimeout time.Duration
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// Validate checks the shape of a configuration and fills its defaults.
func (c *OAuthConfig) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("auth: an oauth credential needs an id")
	}
	switch c.Source {
	case SourceFile:
		if strings.TrimSpace(c.Path) == "" {
			return fmt.Errorf("auth: oauth credential %s: source file needs a path", c.ID)
		}
		c.Path = expandHome(c.Path)
	case SourceExec:
		if len(c.Command) == 0 || strings.TrimSpace(c.Command[0]) == "" {
			return fmt.Errorf("auth: oauth credential %s: source exec needs a command", c.ID)
		}
	case SourceEnv:
		if strings.TrimSpace(c.EnvVar) == "" {
			return fmt.Errorf("auth: oauth credential %s: source env needs a variable name", c.ID)
		}
	default:
		return fmt.Errorf("auth: oauth credential %s: unknown source", c.ID)
	}
	if c.StoreFormat != "" {
		if _, err := ParseStoreFormat(string(c.StoreFormat)); err != nil {
			return fmt.Errorf("auth: oauth credential %s: %w", c.ID, err)
		}
	}
	c.Fields = c.StoreFormat.Fields(c.Fields)
	if c.RefreshMargin <= 0 {
		c.RefreshMargin = DefaultRefreshMargin
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultRefreshPoll
	}
	if quarter := c.RefreshMargin / 4; c.PollInterval > quarter {
		// A poll slower than the margin could step over the whole window and
		// find the token already expired, which is precisely the request-path
		// refresh this design exists to avoid.
		c.PollInterval = quarter
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Millisecond
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = DefaultBackoffBase
	}
	if c.BackoffCeiling <= 0 {
		c.BackoffCeiling = DefaultBackoffCeiling
	}
	if c.BackoffCeiling < c.BackoffBase {
		c.BackoffCeiling = c.BackoffBase
	}
	if c.ExecTimeout <= 0 {
		c.ExecTimeout = DefaultExecTimeout
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return nil
}

// String redacts. A configuration names a token store, and a store's path is
// not a secret, but the type is adjacent enough to key material that redacting
// it costs nothing.
func (c OAuthConfig) String() string {
	return fmt.Sprintf("auth.OAuthConfig{id:%s provider:%s source:%s}", c.ID, c.Provider, c.Source)
}

// GoString redacts %#v.
func (c OAuthConfig) GoString() string { return c.String() }

// Format redacts every other verb.
func (c OAuthConfig) Format(f fmt.State, verb rune) { writeRedacted(f, verb, c.String()) }

// CredentialHealth is everything that leaves this subsystem about a credential
// (DESIGN §11.2b): its opaque id and its health. There is no field here that
// can hold token material.
type CredentialHealth struct {
	// ID is the credential's opaque id.
	ID string
	// Provider names the provider kind.
	Provider string
	// Healthy reports whether the credential should be offered traffic. An
	// unhealthy credential may still hold a working token — a failed refresh
	// does not invalidate the token it failed to replace — so this steps the
	// credential aside exactly as an exhausted quota does (DESIGN §6.1), rather
	// than making it unusable.
	Healthy bool
	// Reason is one of the OAuth* classifications, empty when healthy.
	Reason string
	// Detail is a short human-readable cause with every token this credential
	// has ever held removed from it.
	Detail string
	// Failures counts consecutive failed refreshes.
	Failures int
	// NextAttempt is when the backoff allows another try.
	NextAttempt time.Time
	// LastRefresh is when the token was last successfully renewed.
	LastRefresh time.Time
	// ExpiresAt is when the current access token stops working, zero when the
	// store did not say. It is metadata about a token, not the token.
	ExpiresAt time.Time
	// Refreshes counts successful renewals.
	Refreshes uint64
	// StoreLoads counts tokens adopted from the store rather than exchanged.
	//
	// It is here because a credential with no Refresher — the safe default, where
	// the vendor's CLI keeps its own token current and dorang only reads it —
	// leaves Refreshes at zero forever. Without this number that deployment has
	// no signal at all that the credential is being kept current, and a store
	// that silently stopped being updated looks exactly like one that is fine.
	StoreLoads uint64
}

// String renders the health without any token material, which is why it is
// written out rather than left to %+v over a struct that might grow a field.
func (h CredentialHealth) String() string {
	state := "healthy"
	if !h.Healthy {
		state = "unhealthy(" + h.Reason + ")"
	}
	exp := "none"
	if !h.ExpiresAt.IsZero() {
		exp = h.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf(
		"auth.CredentialHealth{id:%s provider:%s %s failures:%d expires:%s refreshes:%d loads:%d}",
		h.ID, h.Provider, state, h.Failures, exp, h.Refreshes, h.StoreLoads)
}

// OAuthCredential is one credential that refreshes itself.
//
// Reads of the current token are lock-free: a refresh in flight must not add
// latency to a request that is using the token it is replacing.
//
// An OAuthCredential is safe for concurrent use.
type OAuthCredential struct {
	cfg   OAuthConfig
	store tokenStore
	rf    Refresher

	// fl makes refresh single-flight per credential. The key is the credential
	// id, of which there is exactly one here: that is the scope the design
	// requires, and reusing the type keeps one implementation of the pattern.
	fl flight[string, Token]

	tok atomic.Pointer[Token]

	mu          sync.Mutex
	healthy     bool
	reason      string
	detail      string
	failures    int
	nextAttempt time.Time
	lastRefresh time.Time
	// secrets are the token strings this credential has held. They are kept so
	// that anything recorded about a failure can have them removed, including
	// text that came from a provider's own error.
	secrets []string

	refreshes atomic.Uint64
	loads     atomic.Uint64
}

// maxRememberedSecrets bounds the scrub list. Two token sets — the current one
// and the one it replaced — are what a stale error can plausibly mention.
const maxRememberedSecrets = 4

// NewOAuthCredential builds a credential. It does not read the store or contact
// anything; [OAuthCredential.Reload] and [OAuthCredential.Run] do that.
//
// refresher may be nil, which is a credential that is read but never renewed —
// an env-sourced token managed entirely outside dorang.
func NewOAuthCredential(cfg OAuthConfig, refresher Refresher) (*OAuthCredential, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := &OAuthCredential{cfg: cfg, rf: refresher, healthy: true}
	switch cfg.Source {
	case SourceFile:
		c.store = &fileStore{path: cfg.Path, fields: cfg.Fields}
	case SourceExec:
		c.store = &execStore{command: cfg.Command, fields: cfg.Fields, timeout: cfg.ExecTimeout}
	case SourceEnv:
		c.store = &envStore{name: cfg.EnvVar, fields: cfg.Fields}
	}
	c.tok.Store(&Token{})
	return c, nil
}

// ID is the credential's opaque id. It never changes, which is what makes an
// affinity pin survive a refresh (DESIGN §7.4a2).
func (c *OAuthCredential) ID() string { return c.cfg.ID }

// Provider names the provider kind.
func (c *OAuthCredential) Provider() string { return c.cfg.Provider }

// String redacts.
func (c *OAuthCredential) String() string {
	return fmt.Sprintf("auth.OAuthCredential{id:%s provider:%s token:(redacted)}",
		c.cfg.ID, c.cfg.Provider)
}

// GoString redacts %#v.
func (c *OAuthCredential) GoString() string { return c.String() }

// Format redacts every other verb.
func (c *OAuthCredential) Format(f fmt.State, verb rune) { writeRedacted(f, verb, c.String()) }

// current reads the token without taking a lock.
func (c *OAuthCredential) current() Token {
	if t := c.tok.Load(); t != nil {
		return *t
	}
	return Token{}
}

// AccessToken returns the token to send upstream.
//
// It never refreshes and never blocks: a refresh in flight is invisible here,
// which is the whole point of renewing ahead of expiry. An unhealthy credential
// whose token has not yet expired still answers — a failed refresh does not
// invalidate the token it failed to replace, it only means this credential
// should stop being chosen.
func (c *OAuthCredential) AccessToken() (string, error) {
	t := c.current()
	if t.Empty() {
		return "", fmt.Errorf("%w (%s)", ErrNoToken, c.cfg.ID)
	}
	if t.Expired(c.cfg.Now()) {
		return "", fmt.Errorf("%w (%s)", ErrTokenExpired, c.cfg.ID)
	}
	return t.Access, nil
}

// AccountID is the vendor's account identifier, where the store carried one.
func (c *OAuthCredential) AccountID() string { return c.current().AccountID }

// ExpiresAt is when the current access token stops working, zero when unknown.
func (c *OAuthCredential) ExpiresAt() time.Time { return c.current().ExpiresAt }

// Apply sets the credential's headers on an outbound request.
//
// It is called after [Strip] has removed the client's own credential headers,
// never before: the client credential must not reach a provider, and the
// provider credential must not be stripped after it is applied.
func (c *OAuthCredential) Apply(h http.Header) error {
	tok, err := c.AccessToken()
	if err != nil {
		return err
	}
	h.Set(HeaderAuthorization, "Bearer "+tok)
	if c.cfg.AccountHeader != "" {
		if id := c.current().AccountID; id != "" {
			h.Set(c.cfg.AccountHeader, id)
		}
	}
	return nil
}

// Health returns the credential's health. It is the only thing besides the id
// that leaves this subsystem.
func (c *OAuthCredential) Health() CredentialHealth {
	t := c.current()
	c.mu.Lock()
	defer c.mu.Unlock()
	return CredentialHealth{
		ID:          c.cfg.ID,
		Provider:    c.cfg.Provider,
		Healthy:     c.healthy,
		Reason:      c.reason,
		Detail:      c.detail,
		Failures:    c.failures,
		NextAttempt: c.nextAttempt,
		LastRefresh: c.lastRefresh,
		ExpiresAt:   t.ExpiresAt,
		Refreshes:   c.refreshes.Load(),
		StoreLoads:  c.loads.Load(),
	}
}

// Healthy reports whether the credential should be offered traffic.
func (c *OAuthCredential) Healthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.healthy
}

// Refreshes counts successful renewals. Diagnostics and tests.
func (c *OAuthCredential) Refreshes() uint64 { return c.refreshes.Load() }

// StoreLoads counts tokens adopted from the store rather than exchanged. A
// credential whose vendor CLI keeps its own token current shows loads and no
// refreshes, which is the cheapest and safest way for this to work.
func (c *OAuthCredential) StoreLoads() uint64 { return c.loads.Load() }

// Reload reads the store and adopts whatever it holds, without refreshing. It
// is the startup path, and it is also how a token the vendor's CLI renewed on
// its own is picked up.
func (c *OAuthCredential) Reload() error {
	t, err := c.store.Load()
	if err != nil {
		c.fail(OAuthStoreUnreadable, err)
		return err
	}
	c.loads.Add(1)
	c.adopt(t)
	c.recover()
	return nil
}

// Due reports whether the credential wants a refresh at now: the token is
// inside its margin (or missing), and the backoff from any earlier failure has
// elapsed.
func (c *OAuthCredential) Due(now time.Time) bool {
	if !c.current().NeedsRefresh(now, c.cfg.RefreshMargin) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !now.Before(c.nextAttempt)
}

// RefreshIfDue renews the token when it is inside its margin. It is what the
// background loop calls, and it is a no-op the rest of the time.
func (c *OAuthCredential) RefreshIfDue(ctx context.Context) error {
	if !c.Due(c.cfg.Now()) {
		return nil
	}
	_, err := c.refresh(ctx, false)
	return err
}

// Refresh renews the token now, subject to the backoff. Concurrent calls are
// coalesced into one exchange.
func (c *OAuthCredential) Refresh(ctx context.Context) error {
	_, err := c.refresh(ctx, true)
	return err
}

// RefreshOn401 is the fallback path, not the mechanism (DESIGN §11.2b).
//
// Renewal ahead of expiry is how a token stays valid; a 401 is a second signal,
// because clock skew and server-side revocation both exist. stale is the access
// token the request was refused with: if the credential has already moved on
// from it, the caller simply retries with the current one and nothing is
// exchanged. That is what keeps a burst of 401s — every in-flight request on a
// revoked token — from becoming a burst of refreshes.
//
// The caller retries once. Retrying again on a second 401 is what turns a
// revoked credential into a rate-limit ban, and the backoff here is the second
// line of defence, not the first.
func (c *OAuthCredential) RefreshOn401(ctx context.Context, stale string) (string, error) {
	if cur := c.current(); !cur.Empty() && cur.Access != stale {
		return cur.Access, nil
	}
	t, err := c.refresh(ctx, true)
	if err != nil {
		return "", err
	}
	return t.Access, nil
}

// refresh is the single-flight entry point. Every path that can exchange a
// token goes through it, so "one refresh per credential" is a property of one
// function rather than of every caller.
func (c *OAuthCredential) refresh(ctx context.Context, force bool) (Token, error) {
	t, err, _ := c.fl.do(c.cfg.ID, func() (Token, error) { return c.refreshOnce(ctx, force) })
	if errors.Is(err, errFlightAborted) {
		return Token{}, fmt.Errorf("%w (%s): refresh did not complete", ErrRefreshFailed, c.cfg.ID)
	}
	return t, err
}

// refreshOnce performs at most one exchange.
func (c *OAuthCredential) refreshOnce(ctx context.Context, force bool) (Token, error) {
	now := c.cfg.Now()
	cur := c.current()

	// Re-read the shared store first. The vendor's CLI refreshes this token
	// too, and adopting what it wrote costs one file read and avoids spending a
	// refresh token that another process has already replaced. Single-flight
	// covers this process; the store is shared with something outside it.
	if fresh, err := c.store.Load(); err == nil {
		if fresh.newerThan(cur) {
			c.loads.Add(1)
			c.adopt(fresh)
			c.recover()
			cur = fresh
		}
	} else if cur.Empty() {
		// Nothing in memory and nothing readable: there is no refresh token to
		// exchange, so this is where it ends.
		c.fail(OAuthStoreUnreadable, err)
		return Token{}, err
	}

	if !force && !cur.NeedsRefresh(now, c.cfg.RefreshMargin) {
		return cur, nil
	}

	// The backoff gate sits here rather than only in the loop, so that the 401
	// fallback cannot walk around it. A failing credential must not be able to
	// retry against an auth server at request rate.
	c.mu.Lock()
	waiting := now.Before(c.nextAttempt)
	until := c.nextAttempt
	c.mu.Unlock()
	if waiting {
		return Token{}, fmt.Errorf("%w (%s): next attempt at %s",
			ErrBackingOff, c.cfg.ID, until.UTC().Format(time.RFC3339))
	}

	if c.rf == nil {
		err := fmt.Errorf("%w (%s)", ErrNoRefresher, c.cfg.ID)
		c.fail(OAuthNoRefresher, err)
		return Token{}, err
	}
	if cur.Refresh == "" {
		err := fmt.Errorf("%w (%s)", ErrNoRefreshToken, c.cfg.ID)
		c.fail(OAuthNoRefreshToken, err)
		return Token{}, err
	}

	next, err := c.rf.Refresh(ctx, cur)
	if err != nil {
		// The provider's error object is not wrapped: its text is outside this
		// package's control. What is kept is a scrubbed rendering, in the
		// health record, where an operator can see it.
		wrapped := fmt.Errorf("%w (%s): %s", ErrRefreshFailed, c.cfg.ID, c.scrub(err.Error()))
		c.fail(OAuthRefreshFailed, wrapped)
		return Token{}, wrapped
	}
	if next.Empty() {
		wrapped := fmt.Errorf("%w (%s): the provider returned no access token",
			ErrRefreshFailed, c.cfg.ID)
		c.fail(OAuthRefreshFailed, wrapped)
		return Token{}, wrapped
	}
	next = next.merge(cur)

	// Adopt before persisting. The token is already valid upstream at this
	// point, and a store that cannot be written is a reporting problem, not a
	// reason to keep sending a token that is about to expire.
	c.adopt(next)
	c.refreshes.Add(1)
	c.mu.Lock()
	c.lastRefresh = now
	c.mu.Unlock()

	if c.store.Writable() {
		if err := c.store.Save(next); err != nil {
			// The exchange consumed a refresh token that the vendor's CLI is
			// still holding, and the successor could not be recorded. Standing
			// aside is the honest outcome: the account's stored state no longer
			// matches reality and every other reader of that store will fail.
			wrapped := fmt.Errorf("auth: oauth credential %s: %s", c.cfg.ID, c.scrub(err.Error()))
			c.fail(OAuthStoreWriteFailed, wrapped)
			return next, wrapped
		}
	}
	c.recover()
	return next, nil
}

// adopt publishes a token and remembers its secrets for scrubbing.
func (c *OAuthCredential) adopt(t Token) {
	prev := c.current()
	c.tok.Store(&t)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range []string{prev.Access, prev.Refresh, t.Access, t.Refresh} {
		if s == "" {
			continue
		}
		if !containsString(c.secrets, s) {
			c.secrets = append(c.secrets, s)
		}
	}
	if n := len(c.secrets); n > maxRememberedSecrets {
		c.secrets = append(c.secrets[:0], c.secrets[n-maxRememberedSecrets:]...)
	}
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// fail records a failed refresh: the credential steps aside and the next
// attempt is pushed out exponentially, to a ceiling.
func (c *OAuthCredential) fail(reason string, err error) {
	now := c.cfg.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	c.healthy = false
	c.reason = reason
	c.detail = c.scrubLocked(err.Error())
	c.nextAttempt = now.Add(c.backoffLocked())
}

// recover clears a failure.
func (c *OAuthCredential) recover() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.healthy = true
	c.reason, c.detail = "", ""
	c.failures = 0
	c.nextAttempt = time.Time{}
}

// backoffLocked is base × 2^(failures-1), capped. It is deterministic: a
// jittered backoff would be untestable here, and the stampede this one has to
// avoid is against an auth server rather than between nodes.
func (c *OAuthCredential) backoffLocked() time.Duration {
	d := c.cfg.BackoffBase
	for range c.failures - 1 {
		if d >= c.cfg.BackoffCeiling/2 {
			return c.cfg.BackoffCeiling
		}
		d *= 2
	}
	if d > c.cfg.BackoffCeiling {
		d = c.cfg.BackoffCeiling
	}
	return d
}

// scrub removes every token this credential has held from a message.
//
// It exists because the text may have come from a provider's Refresher, which
// this package does not control. It is a backstop, not the mechanism: nothing
// inside this package interpolates a token into a message in the first place.
func (c *OAuthCredential) scrub(s string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scrubLocked(s)
}

func (c *OAuthCredential) scrubLocked(s string) string {
	t := c.current()
	for _, secret := range append([]string{t.Access, t.Refresh}, c.secrets...) {
		if len(secret) < 4 {
			continue // too short to be a token; replacing it would mangle text
		}
		s = strings.ReplaceAll(s, secret, "(redacted)")
	}
	return s
}

// Run is the background loop, one per credential (DESIGN §11.2b, §9.6). It
// reads the store once, then renews the token whenever it enters its margin,
// until the context is cancelled. Nothing on the request path waits for it.
func (c *OAuthCredential) Run(ctx context.Context) {
	_ = c.Reload()
	if err := ctx.Err(); err != nil {
		return
	}
	_ = c.RefreshIfDue(ctx)

	t := time.NewTicker(c.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Errors are recorded in the credential's health, not returned:
			// this loop's failure mode is a credential that steps aside, not a
			// gateway that stops.
			_ = c.RefreshIfDue(ctx)
		}
	}
}

// OAuthManager owns a set of OAuth credentials and their background loops.
//
// An OAuthManager is safe for concurrent use.
type OAuthManager struct {
	mu     sync.RWMutex
	creds  map[string]*OAuthCredential
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

// NewOAuthManager builds an empty manager.
func NewOAuthManager() *OAuthManager {
	return &OAuthManager{creds: map[string]*OAuthCredential{}}
}

// String redacts.
func (m *OAuthManager) String() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return fmt.Sprintf("auth.OAuthManager{credentials:%d tokens:(redacted)}", len(m.creds))
}

// GoString redacts %#v.
func (m *OAuthManager) GoString() string { return m.String() }

// Format redacts every other verb.
func (m *OAuthManager) Format(f fmt.State, verb rune) { writeRedacted(f, verb, m.String()) }

// Add registers a credential. Two credentials cannot share an id: the id is
// what an affinity pin holds, so a duplicate would silently move a pinned
// conversation to another account.
func (m *OAuthManager) Add(c *OAuthCredential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.creds[c.ID()]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicateCredential, c.ID())
	}
	m.creds[c.ID()] = c
	return nil
}

// Credential resolves an id to its credential. It is what an affinity pin
// resolves through, and it returns the same credential across every refresh.
func (m *OAuthManager) Credential(id string) (*OAuthCredential, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.creds[id]
	return c, ok
}

// IDs returns the registered credential ids, sorted.
func (m *OAuthManager) IDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.creds))
	for id := range m.creds {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Start runs every credential's background loop. It returns immediately.
func (m *OAuthManager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil || m.closed {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	for _, c := range m.creds {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			c.Run(ctx)
		}()
	}
}

// Close stops the background loops and waits for them. It is idempotent.
func (m *OAuthManager) Close() {
	m.mu.Lock()
	cancel := m.cancel
	m.cancel, m.closed = nil, true
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

// Snapshot reports every credential's health, sorted by id.
//
// It is the administrative view, and it carries no token material: the type it
// returns has no field that could hold any (DESIGN §11.2b).
func (m *OAuthManager) Snapshot() []CredentialHealth {
	m.mu.RLock()
	out := make([]CredentialHealth, 0, len(m.creds))
	for _, c := range m.creds {
		out = append(out, c.Health())
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// FakeRefresher is a Refresher for tests: it mints successors without contacting
// anything. It ships so that the refresh mechanics — coalescing, backoff,
// atomic persistence — can be tested without a provider, exactly as
// quota.StaticProber does for provider-reported quota.
type FakeRefresher struct {
	mu sync.Mutex
	// TTL is how long each minted token lasts.
	TTL time.Duration
	// Now supplies the clock. Required for a deterministic expiry.
	Now func() time.Time
	// Err, when set, makes every refresh fail with it.
	Err error
	// Block, when non-nil, holds every refresh until it is closed. It is how a
	// test observes a refresh that is in flight.
	Block chan struct{}

	calls  int
	serial int
}

// Refresh implements Refresher.
func (f *FakeRefresher) Refresh(ctx context.Context, prev Token) (Token, error) {
	f.mu.Lock()
	f.calls++
	f.serial++
	serial, block, err, ttl, now := f.serial, f.Block, f.Err, f.TTL, f.Now
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return Token{}, ctx.Err()
		}
	}
	if err != nil {
		return Token{}, err
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	if now == nil {
		now = time.Now
	}
	return Token{
		Access:    fmt.Sprintf("access-%d", serial),
		Refresh:   fmt.Sprintf("refresh-%d", serial),
		ExpiresAt: now().Add(ttl),
		AccountID: prev.AccountID,
	}, nil
}

// Calls counts refreshes served.
func (f *FakeRefresher) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// SetError makes every subsequent refresh fail.
func (f *FakeRefresher) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Err = err
}
