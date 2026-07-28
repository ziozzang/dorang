package probe

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
)

// The errors this package returns. Every one of them is a sentinel so that a
// caller can classify a failure without reading its text, and none of them ever
// carries a provider's response body — see [Prober.Read].
var (
	// ErrNoEndpoint reports a provider that publishes no account-status
	// endpoint the serving credential can read. It is not a failure: it is the
	// documented answer for most providers, and [Supported] carries the reason.
	ErrNoEndpoint = errors.New("probe: provider publishes no credential-scoped quota endpoint")
	// ErrUnknownProvider reports a provider this package has never examined,
	// which is a different statement from ErrNoEndpoint.
	ErrUnknownProvider = errors.New("probe: unknown provider")
	// ErrNoSecret reports a credential that resolved to no token at all.
	ErrNoSecret = errors.New("probe: credential resolved to an empty secret")
	// ErrWrongCredentialKind reports a secret this endpoint provably cannot
	// accept — an API key where the endpoint is OAuth-only. Refusing beats
	// spending a request to be told 401, which is how a prober gets an account
	// rate-limited (DESIGN §6.2, §11.2b).
	ErrWrongCredentialKind = errors.New("probe: credential is not the kind this endpoint accepts")
	// ErrThrottled reports that the prober deliberately did not read, because
	// it is backing off. Nothing failed at the provider; the last good snapshot
	// stands and its staleness grows.
	ErrThrottled = errors.New("probe: not read: prober is backing off")
	// ErrTransport reports a request that never produced a response.
	ErrTransport = errors.New("probe: transport failure")
	// ErrUnauthorized reports 401 or 403.
	ErrUnauthorized = errors.New("probe: credential rejected by the provider")
	// ErrRateLimited reports 429.
	ErrRateLimited = errors.New("probe: rate limited by the provider")
	// ErrStatus reports any other unsuccessful status.
	ErrStatus = errors.New("probe: unexpected status")
	// ErrTooLarge reports a response past [Config.MaxBody].
	ErrTooLarge = errors.New("probe: response too large")
	// ErrMalformed reports a body that did not decode, or decoded to nothing
	// usable. It is deliberately distinct from a transport failure: it means the
	// endpoint answered and this package does not understand the answer, which
	// is a reason to ship a fix rather than to retry harder.
	ErrMalformed = errors.New("probe: malformed response")
)

// redacted replaces a secret wherever one is found in text this package keeps.
const redacted = "(redacted)"

// Auth supplies the material one probe authenticates with, resolved at probe
// time rather than captured at registration.
//
// The indirection exists for OAuth credentials (DESIGN §11.2b). An OAuth
// credential's access token is replaced by its own background refresher, so a
// prober that captured the string once would keep presenting a token that has
// since expired — and would then answer every poll with a 401, which is exactly
// the auth-server hammering §11.2b forbids and the account rate-limiting §6.2's
// prober exists to prevent.
type Auth interface {
	// Token returns the bearer material for one read. The returned string is a
	// secret: it is remembered only so that it can be scrubbed out of anything
	// recorded, and is never stored in a snapshot.
	Token(ctx context.Context) (string, error)
}

// AuthFunc adapts a function to [Auth]. auth.OAuthCredential.AccessToken is the
// intended argument; this package does not import internal/auth, so that a
// credential source can be anything.
type AuthFunc func(context.Context) (string, error)

// Token implements [Auth].
func (f AuthFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// StaticAuth wraps a fixed secret, which is what an API-key credential has.
func StaticAuth(secret string) Auth {
	return AuthFunc(func(context.Context) (string, error) { return secret, nil })
}

// Window is one allowance a provider reported, normalized into the vocabulary
// internal/quota consumes.
//
// A window is emitted only when it says something true. A provider figure that
// could not be read as a proportion, a window whose length the provider did not
// describe, a reset instant in a format that does not name its zone: each
// leaves the corresponding field zero and says so through Known or Mapped,
// because an over-optimistic quota reading routes traffic into an exhausted
// account (DESIGN §6.2).
type Window struct {
	// Label is the provider's own name for this window, lowercased and
	// qualified by its length — "tokens_limit:5h", "five_hour". It is what an
	// [Allowance] matches on and what [Snapshot.Unmapped] lists.
	Label string
	// Window and Metric are the key quota files this figure under. They are
	// meaningful only when Mapped is true.
	//
	// For a percent-only window the metric is a *key*, not a measurement: the
	// figure is a proportion of a number the provider did not publish, so it
	// cannot gate anything (see the package doc). Choosing the metric an
	// operator most likely configured therefore costs nothing and buys the
	// reset instant §7.5a(c) needs. An [Allowance] overrides it.
	Window quota.Window
	Metric quota.Metric
	// Mapped reports whether this window could be filed under a key at all. A
	// provider window whose meaning is contested — z.ai's TIME_LIMIT, a
	// per-model sub-window quota has no dimension for — is carried unmapped
	// rather than filed under a guess.
	Mapped bool
	// UsedPercent is the provider's consumed figure in [0,100], valid only when
	// Known is true.
	UsedPercent float64
	Known       bool
	// Used and Limit are absolute figures in the metric's units. They are zero
	// unless the provider published a ceiling or an [Allowance] declared one;
	// zero means quota keeps this window for its reset instant alone.
	Used, Limit int64
	// ResetAt is when the provider says this window resets. Zero is unknown,
	// which §7.5a(c) scores as no urgency rather than as imminent.
	ResetAt time.Time
}

// providerWindow projects onto quota's shape. ok is false for a window that
// could not be filed under a key: absent is how this package spells unknown,
// and quota reads an absent window as "no provider opinion", which falls back
// to local metering.
func (w Window) providerWindow() (quota.ProviderWindow, bool) {
	if !w.Mapped || !w.Window.Valid() || !w.Metric.Valid() {
		return quota.ProviderWindow{}, false
	}
	pw := quota.ProviderWindow{
		Window:  w.Window,
		Metric:  w.Metric,
		Used:    w.Used,
		Limit:   w.Limit,
		ResetAt: w.ResetAt,
	}
	if w.Known {
		pw.UsedPercent = w.UsedPercent
	}
	return pw, true
}

// Balance is a prepaid balance: what is left, with no window and no reset.
//
// It is deliberately not a [Window]. A balance answers "is there money", not
// "how much of an allowance has been consumed"; there is no ceiling to be a
// proportion of, and §6.2's used-versus-limit arithmetic has nothing to work
// with. It also carries no urgency at all under §7.5a(c) constraint 1 — a
// balance that rolls over loses nothing by being spent later.
type Balance struct {
	// Currency is the provider's own code, unconverted. A provider that bills
	// in CNY is reported in CNY: inventing an exchange rate would put a made-up
	// number where a real one is expected.
	Currency string
	// RemainingNano is what is left, in nano-units of Currency, matching the
	// fixed-point scale quota uses for money.
	RemainingNano int64
	// Available is the provider's own verdict on whether the credential can
	// still serve. It is reported, never acted on here.
	Available bool
}

// Snapshot is one successful read of a provider's account status.
//
// It contains no credential material. That is a property of construction, not
// of filtering: labels are built by this package from the shapes it recognizes,
// never copied out of a provider's response.
type Snapshot struct {
	ProviderID   string
	CredentialID string
	// FetchedAt is when the figures were read. quota uses it as the baseline
	// instant for the local delta, so it is the read time and it is preserved
	// verbatim when a cached snapshot is replayed.
	FetchedAt time.Time
	Windows   []Window
	Balances  []Balance
}

// Unmapped lists the labels of windows the provider reported that could not be
// filed under a quota key. It exists so that "the prober did not understand
// this" is visible rather than silent, and so that an operator can write the
// [Allowance] that files it.
func (s Snapshot) Unmapped() []string {
	var out []string
	for _, w := range s.Windows {
		if !w.Mapped {
			out = append(out, w.Label)
		}
	}
	return out
}

// ProbeResult projects the snapshot onto the shape [quota.Tracker] adopts.
func (s Snapshot) ProbeResult() quota.ProbeResult {
	r := quota.ProbeResult{
		ProviderID:   s.ProviderID,
		CredentialID: s.CredentialID,
		FetchedAt:    s.FetchedAt,
	}
	for _, w := range s.Windows {
		if pw, ok := w.providerWindow(); ok {
			r.Windows = append(r.Windows, pw)
		}
	}
	return r
}

// Allowance re-files one provider-reported window under the key an operator
// actually configured, and optionally declares how large the allowance is.
//
// Both halves matter, and each fixes a different silent failure:
//
//   - quota is keyed by (window, metric). A provider window filed under a key
//     no [quota.Rule] uses is inert — no gate, no urgency, no error. Naming the
//     rule's key here is how the reset instant reaches the rule that needs it.
//   - A provider that publishes only a percentage gives no figure in the
//     metric's units, and §6.2's max() cannot combine a percentage with a token
//     count. Limit supplies the missing ceiling, turning the proportion into a
//     figure the combination can use. It is the operator's assertion about
//     their own plan, which is why this package will not infer it.
type Allowance struct {
	// Label is the [Window.Label] this mapping applies to, matched
	// case-insensitively.
	Label string
	// Window and Metric are the configured rule's key. Both are required.
	Window quota.Window
	Metric quota.Metric
	// Limit is the allowance's absolute size in the metric's units. Zero leaves
	// the window percent-only.
	Limit int64
}

// Validate checks an allowance is fully specified.
func (a Allowance) Validate() error {
	if strings.TrimSpace(a.Label) == "" {
		return errors.New("probe: allowance has no label")
	}
	if !a.Window.Valid() {
		return fmt.Errorf("probe: allowance %q has no valid window", a.Label)
	}
	if !a.Metric.Valid() {
		return fmt.Errorf("probe: allowance %q has no valid metric", a.Label)
	}
	if a.Limit < 0 {
		return fmt.Errorf("probe: allowance %q has a negative limit", a.Label)
	}
	return nil
}

// Defaults for [Config]. Each of the timing ones is part of rule 6 in the
// package doc: a poll loop that retries hard against an account-status endpoint
// can get the account limited, which is the failure the loop exists to prevent.
const (
	// DefaultMinInterval floors the spacing between two reads of one
	// credential. A caller polling faster than this is served the last snapshot
	// with its original fetch instant, so staleness stays honest and no extra
	// request is spent.
	DefaultMinInterval = 30 * time.Second
	// DefaultBackoffBase is the wait after the first failed read.
	DefaultBackoffBase = 30 * time.Second
	// DefaultBackoffCeiling caps the exponential wait.
	DefaultBackoffCeiling = 15 * time.Minute
	// DefaultMaxConcurrent caps this package's in-flight reads per provider, so
	// that a fleet of credentials polled in one round arrives as a trickle
	// rather than as a burst that looks like an attack.
	DefaultMaxConcurrent = 4
	// DefaultMaxBody caps a response read. A hostile or misconfigured endpoint
	// must not be able to spend the gateway's memory.
	DefaultMaxBody int64 = 1 << 20
	// DefaultTimeout bounds one read when the caller's context carries no
	// deadline. quota.Registry always supplies one; a direct caller may not.
	DefaultTimeout = 10 * time.Second
	// DefaultUserAgent identifies the reader truthfully.
	DefaultUserAgent = "dorang-probe/1"
)

// backoffJitter is the fraction the backoff is perturbed by, so that a fleet of
// credentials that failed together does not retry together.
const backoffJitter = 0.4

// Config configures one provider's prober.
type Config struct {
	// BaseURL overrides the provider's host, for tests and for a gateway that
	// fronts a provider. Empty uses the provider's documented host.
	BaseURL string
	// Client is the HTTP client. nil builds one that does not follow redirects
	// — a redirect from a provider endpoint is a misconfiguration or a captive
	// portal, and following one resends the credential to whatever answered.
	Client *http.Client
	// UserAgent identifies this reader.
	UserAgent string
	// Timeout bounds one read when the caller's context has no deadline.
	Timeout time.Duration
	// MinInterval, BackoffBase, BackoffCeiling and MaxConcurrent tune the
	// prober's own rate limiting; zero takes the Default above. A negative
	// MinInterval removes the floor entirely, which is a thing to do in a test
	// and not in a deployment.
	MinInterval    time.Duration
	BackoffBase    time.Duration
	BackoffCeiling time.Duration
	MaxConcurrent  int
	// MaxBody caps a response body.
	MaxBody int64
	// Allowances re-file provider windows under configured rule keys.
	Allowances []Allowance
	// Auth supplies per-credential tokens, keyed by credential id. A credential
	// with no entry authenticates with quota.Credential.Secret.
	Auth map[string]Auth
	// Now overrides the clock.
	Now func() time.Time
}

func (c *Config) withDefaults() {
	if c.UserAgent == "" {
		c.UserAgent = DefaultUserAgent
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MinInterval < 0 {
		c.MinInterval = 0
	} else if c.MinInterval == 0 {
		c.MinInterval = DefaultMinInterval
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
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = DefaultMaxConcurrent
	}
	if c.MaxBody <= 0 {
		c.MaxBody = DefaultMaxBody
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// reading is what a provider-specific decoder produces: figures, no policy.
type reading struct {
	windows  []Window
	balances []Balance
}

// source is the provider-specific half of a prober — one request and one pure
// decoder. Everything else (auth, timeouts, rate limiting, scrubbing, snapshot
// retention) is shared, so a new provider is a request builder and a decode
// function, and every payload shape is a table test with no HTTP in it.
type source interface {
	// name is the canonical provider this source reads.
	name() string
	// endpoint is the URL it reads, for [Supported] and for diagnostics.
	endpoint(base string) string
	// accepts rejects a secret the endpoint provably cannot use, before a
	// request is spent finding out.
	accepts(secret string) error
	// request builds the authenticated read.
	request(ctx context.Context, base, token, userAgent string) (*http.Request, error)
	// decode turns a response body into figures. It is pure: no clock, no I/O.
	decode(body []byte) (reading, error)
}

// credState is everything the prober remembers about one credential. It holds
// no secret: the scrubber keeps secrets only to remove them from text, and
// reason is already scrubbed.
type credState struct {
	scrub    scrubber
	snap     Snapshot
	hasSnap  bool
	inflight bool
	failures int
	retryAt  time.Time
	lastTry  time.Time
	lastOK   time.Time
	reason   string
}

// Health is the prober's own view of one credential, which is deliberately not
// the tracker's: quota records whether a *figure* is stale, this records
// whether the *reader* is working, and a caller that conflates them cannot tell
// "the provider is down" from "we chose not to ask".
type Health struct {
	CredentialID string
	// Failures counts consecutive failed reads.
	Failures int
	// Reason is the last failure, already scrubbed. It never contains a
	// response body and never contains a secret.
	Reason string
	// LastSuccess and LastAttempt are zero when there has been none.
	LastSuccess time.Time
	LastAttempt time.Time
	// NextAttempt is when the backoff gate opens; zero when it is open.
	NextAttempt time.Time
}

// Prober reads one provider's account status for the credentials registered
// against it. It implements [quota.Prober].
//
// A Prober is safe for concurrent use.
type Prober struct {
	id  string
	src source
	cfg Config
	cl  *http.Client
	sem chan struct{}

	mu   sync.Mutex
	cred map[string]*credState
}

// newProber builds a prober over a source. [New] is the public constructor.
func newProber(id string, src source, cfg Config) (*Prober, error) {
	cfg.withDefaults()
	for _, a := range cfg.Allowances {
		if err := a.Validate(); err != nil {
			return nil, err
		}
	}
	cl := cfg.Client
	if cl == nil {
		cl = defaultClient()
	}
	// The caller's slices and maps are copied rather than aliased: a prober
	// reads them from its polling goroutine, and a configuration reload that
	// wrote through the same map would be a data race on a credential.
	cfg.Allowances = slices.Clone(cfg.Allowances)
	cfg.Auth = maps.Clone(cfg.Auth)
	return &Prober{
		id:   id,
		src:  src,
		cfg:  cfg,
		cl:   cl,
		sem:  make(chan struct{}, cfg.MaxConcurrent),
		cred: map[string]*credState{},
	}, nil
}

// ProviderID implements [quota.Prober]. It is the id this prober was built
// with, not the source's canonical name, so that it matches whatever the
// operator called the provider in configuration.
func (p *Prober) ProviderID() string { return p.id }

// Endpoint is the URL this prober reads.
func (p *Prober) Endpoint() string { return p.src.endpoint(p.cfg.BaseURL) }

// SetAuth registers a token source for one credential, overriding its static
// secret. It is how an OAuth credential is wired in (DESIGN §11.2b).
func (p *Prober) SetAuth(credID string, a Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cfg.Auth == nil {
		p.cfg.Auth = map[string]Auth{}
	}
	p.cfg.Auth[credID] = a
}

// Probe implements [quota.Prober].
func (p *Prober) Probe(ctx context.Context, cred quota.Credential) (quota.ProbeResult, error) {
	s, err := p.Read(ctx, cred)
	if err != nil {
		return quota.ProbeResult{}, err
	}
	return s.ProbeResult(), nil
}

// Snapshot returns the last good read for a credential.
func (p *Prober) Snapshot(credID string) (Snapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.cred[credID]
	if !ok || !st.hasSnap {
		return Snapshot{}, false
	}
	return st.snap.clone(), true
}

// Health returns the prober's view of one credential.
func (p *Prober) Health(credID string) Health {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := Health{CredentialID: credID}
	st, ok := p.cred[credID]
	if !ok {
		return h
	}
	h.Failures, h.Reason = st.failures, st.reason
	h.LastSuccess, h.LastAttempt, h.NextAttempt = st.lastOK, st.lastTry, st.retryAt
	return h
}

// clone deep-copies a snapshot so a caller cannot mutate the retained one.
func (s Snapshot) clone() Snapshot {
	s.Windows = append([]Window(nil), s.Windows...)
	s.Balances = append([]Balance(nil), s.Balances...)
	return s
}

// Read performs one read and returns the normalized snapshot.
//
// Four properties are load-bearing, and each is a rule from DESIGN §6.2 rather
// than an HTTP concern:
//
//   - A failure returns an error and changes nothing else. The last good
//     snapshot stays exactly as it was, with its original fetch instant, so the
//     staleness quota reports keeps growing truthfully and the credential goes
//     on serving. A failed read is not an exhausted quota.
//   - No error carries the response body. A provider's error body can echo the
//     key back, and wrapping it puts the key straight into the message — the
//     same reasoning §11.2b applies to a refresher's errors. What is returned
//     is a sentinel, the provider id and the status; what is recorded is
//     additionally passed through a scrubber holding every secret this
//     credential has presented, because a transport error can carry the URL and
//     some providers authenticate in a query parameter.
//   - The prober rate-limits itself. A failed read closes a gate that reopens
//     on an exponential, jittered backoff, and a provider's Retry-After extends
//     it. Nothing here retries within a read.
//   - Reads of one provider are capped in flight, so a round that polls fifty
//     credentials does not arrive as fifty simultaneous requests.
func (p *Prober) Read(ctx context.Context, cred quota.Credential) (Snapshot, error) {
	now := p.cfg.Now()
	st := p.stateFor(cred.ID())

	// The gate, in order. Backoff first: while it is closed the answer is
	// "we did not ask", which is not a provider failure and must not be
	// reported as one.
	p.mu.Lock()
	switch {
	case now.Before(st.retryAt):
		p.mu.Unlock()
		return Snapshot{}, fmt.Errorf("%w: %s: %s remaining", ErrThrottled, p.id,
			st.retryAt.Sub(now).Round(time.Second))
	case st.inflight:
		// A second concurrent read of the same credential adds load and can
		// only produce the same answer.
		snap, ok := st.snap, st.hasSnap
		p.mu.Unlock()
		if ok {
			return snap.clone(), nil
		}
		return Snapshot{}, fmt.Errorf("%w: %s: a read is already in flight", ErrThrottled, p.id)
	case st.hasSnap && p.cfg.MinInterval > 0 && now.Sub(st.lastTry) < p.cfg.MinInterval:
		// Too soon to have anything new to say. Replaying the snapshot with its
		// original FetchedAt keeps staleness honest — re-reading would spend a
		// request to learn the same thing.
		snap := st.snap.clone()
		p.mu.Unlock()
		return snap, nil
	}
	st.inflight = true
	st.lastTry = now
	p.mu.Unlock()

	snap, err := p.read(ctx, cred, st)

	p.mu.Lock()
	defer p.mu.Unlock()
	st.inflight = false
	if err != nil {
		st.failures++
		st.reason = st.scrub.text(err.Error())
		st.retryAt = p.cfg.Now().Add(p.backoff(st.failures, retryAfterOf(err)))
		return Snapshot{}, err
	}
	st.failures, st.reason, st.retryAt = 0, "", time.Time{}
	st.lastOK = snap.FetchedAt
	st.snap, st.hasSnap = snap, true
	return snap.clone(), nil
}

// read is the part that talks to the provider. It holds no lock.
func (p *Prober) read(ctx context.Context, cred quota.Credential, st *credState) (Snapshot, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.cfg.Timeout)
		defer cancel()
	}

	// The in-flight cap is taken before the token is resolved, so that a
	// refresh is not started for a read that is about to be cancelled.
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return Snapshot{}, fmt.Errorf("%w: %s: waiting for a slot: %w", ErrTransport, p.id, ctx.Err())
	}

	token, err := p.token(ctx, cred)
	if err != nil {
		return Snapshot{}, err
	}
	st.scrub.remember(token)
	if err := p.src.accepts(token); err != nil {
		return Snapshot{}, err
	}

	req, err := p.src.request(ctx, p.cfg.BaseURL, token, p.cfg.UserAgent)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %s: %s", ErrTransport, p.id, st.scrub.text(err.Error()))
	}
	body, err := p.do(ctx, st, req)
	if err != nil {
		return Snapshot{}, err
	}
	fetchedAt := p.cfg.Now()

	r, err := p.src.decode(body)
	if err != nil {
		// The decoder's error describes the shape, never the content: a body
		// that failed to parse is exactly the body most likely to be an error
		// page with the key in it.
		return Snapshot{}, fmt.Errorf("%w: %s: %s", ErrMalformed, p.id, st.scrub.text(err.Error()))
	}
	if len(r.windows) == 0 && len(r.balances) == 0 {
		// A parseable answer that carries no figures is a failure, not a
		// success with nothing in it. quota.Tracker.Adopt takes FetchedAt from
		// any result, so adopting an empty one would refresh the staleness of
		// figures that were not re-read — a snapshot that reports itself fresh
		// while saying what it said an hour ago. §6.2 does not address this
		// case; treating it as a failed read is the reading that keeps
		// staleness honest, and it is visible in Health rather than silent.
		return Snapshot{}, fmt.Errorf("%w: %s: response carried no quota figures", ErrMalformed, p.id)
	}
	return Snapshot{
		ProviderID:   p.id,
		CredentialID: cred.ID(),
		FetchedAt:    fetchedAt,
		Windows:      p.applyAllowances(r.windows),
		Balances:     r.balances,
	}, nil
}

// token resolves the credential's material for this read.
func (p *Prober) token(ctx context.Context, cred quota.Credential) (string, error) {
	p.mu.Lock()
	a := p.cfg.Auth[cred.ID()]
	p.mu.Unlock()
	if a == nil {
		a = StaticAuth(cred.Secret())
	}
	tok, err := a.Token(ctx)
	if err != nil {
		// The Auth is provider code — an OAuth refresher, an exec'd command —
		// and its error can carry a token. It is not wrapped, for the reason
		// §11.2b gives: wrapping would put its unscrubbed text back in.
		return "", fmt.Errorf("%w: %s: token source failed", ErrNoSecret, p.id)
	}
	if strings.TrimSpace(tok) == "" {
		return "", fmt.Errorf("%w: %s", ErrNoSecret, p.id)
	}
	return tok, nil
}

// applyAllowances re-files and scales windows per the operator's declarations.
func (p *Prober) applyAllowances(ws []Window) []Window {
	if len(p.cfg.Allowances) == 0 {
		return ws
	}
	for i := range ws {
		for _, a := range p.cfg.Allowances {
			if !strings.EqualFold(a.Label, ws[i].Label) {
				continue
			}
			ws[i].Window, ws[i].Metric, ws[i].Mapped = a.Window, a.Metric, true
			if a.Limit > 0 && ws[i].Known {
				ws[i].Limit = a.Limit
				ws[i].Used = scalePercent(a.Limit, ws[i].UsedPercent)
			}
			break
		}
	}
	return ws
}

// scalePercent turns a percentage of a declared allowance into a figure in the
// metric's units, rounding up. Up, because rounding a usage figure down is the
// direction that routes into an account with less left than dorang believes.
func scalePercent(limit int64, pct float64) int64 {
	v := math.Ceil(float64(limit) * pct / 100)
	if v < 0 {
		return 0
	}
	if v > float64(limit) {
		return limit
	}
	return int64(v)
}

// stateFor returns the per-credential state, creating it once.
func (p *Prober) stateFor(credID string) *credState {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.cred[credID]
	if !ok {
		st = &credState{}
		p.cred[credID] = st
	}
	return st
}

// backoff is the wait after n consecutive failures, jittered, and never shorter
// than a Retry-After the provider asked for.
func (p *Prober) backoff(n int, retryAfter time.Duration) time.Duration {
	d := p.cfg.BackoffBase
	for i := 1; i < n; i++ {
		if d >= p.cfg.BackoffCeiling/2 {
			d = p.cfg.BackoffCeiling
			break
		}
		d *= 2
	}
	if d > p.cfg.BackoffCeiling {
		d = p.cfg.BackoffCeiling
	}
	f := 1 - backoffJitter/2 + backoffJitter*rand.Float64()
	d = time.Duration(float64(d) * f)
	if retryAfter > d {
		return retryAfter
	}
	return d
}
