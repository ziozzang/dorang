package admin

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

// Defaults. They are the notebook profile of §0.2: correct with no tuning.
const (
	// DefaultMaxTimeRange bounds a ledger query's width. It matches the
	// store's own default, so the admin surface refuses a too-wide range with
	// its own message rather than passing it down to be refused there.
	DefaultMaxTimeRange = 92 * 24 * time.Hour
	// DefaultPageSize and DefaultMaxPageSize bound ledger pagination.
	DefaultPageSize    = 100
	DefaultMaxPageSize = 1000
	// DefaultListLimit bounds a directory listing.
	DefaultListLimit    = 100
	DefaultMaxListLimit = 1000
	// DefaultUIPrefix is where the operator UI is mounted.
	DefaultUIPrefix = "/ui"
	// DefaultSessionCookie names the UI's read-only session cookie.
	DefaultSessionCookie = "dorang_admin_ui"
	// DefaultSessionTTL bounds a UI session.
	//
	// An hour, not a working day. A session holds an authorization decision
	// rather than the credential, so revoking the underlying key is not
	// observed until the session ends — which makes this value the revocation
	// lag, and a revocation lag measured in working days is not a revocation.
	DefaultSessionTTL = time.Hour
)

// Config wires the administration surface.
//
// Only Auth is unconditionally required. Every data dependency is optional in
// the sense that its absence is answerable: the endpoints that need it answer
// 501 with [CodeDependencyOff] naming the missing piece, which is the honest
// answer and one an operator can act on. What is not acceptable — and does not
// happen — is a nil dependency reaching a handler as a panic or as an empty
// result that reads like "there is nothing there".
type Config struct {
	// Auth resolves administrative credentials. Required.
	Auth Authenticator

	Keys      KeyStore
	Hasher    Hasher
	Directory Directory
	Models    ModelRegistry
	Budgets   BudgetStore
	Ledger    Ledger

	// Audit persists the trail. Required for any mutation: a mutation that
	// cannot be audited is refused before it happens (see [API.mutate]).
	Audit Auditor

	Credentials CredentialReporter
	Capacity    CapacityReporter
	Health      HealthHistory
	Catalog     Catalog
	Pricing     Pricer
	Reloader    Reloader

	// MaxTimeRange caps a ledger query's width. Zero means
	// DefaultMaxTimeRange.
	MaxTimeRange time.Duration
	// PageSize and MaxPageSize bound ledger pagination.
	PageSize    int
	MaxPageSize int
	// ListLimit and MaxListLimit bound directory listings.
	ListLimit    int
	MaxListLimit int

	// UIPrefix mounts the operator UI. Zero means DefaultUIPrefix; "-"
	// disables the UI entirely, for a deployment that serves the API only.
	UIPrefix string
	// SessionCookie names the UI session cookie. Zero means
	// DefaultSessionCookie.
	SessionCookie string
	// SessionTTL bounds a UI session. Zero means DefaultSessionTTL.
	SessionTTL time.Duration
	// DisableUISessions serves the UI without the cookie exchange, for a
	// deployment that puts its own authenticating proxy in front. The screens
	// then require the same header the API does.
	DisableUISessions bool

	// Now is the clock. Zero means time.Now.
	Now func() time.Time
	// NewID mints object ids. Zero means a random 26-character identifier.
	NewID func() string
	// NewToken mints a key's plaintext secret. Zero means 32 bytes of
	// crypto/rand, base64url, behind the "sk-" prefix every client library
	// already expects.
	NewToken func() (string, error)

	// RotationGrace is auth.rotation.grace: how long a rotated-away secret
	// keeps authenticating (DESIGN §11.2c). Zero means
	// [DefaultRotationGrace]; a request may override it per rotation, including
	// with "0" for an immediate cut.
	RotationGrace time.Duration
}

// DefaultRotationGrace matches auth.rotation.grace's default. A day is long
// enough that a client with a nightly deploy rolls inside it, which is the
// interval the number exists to cover.
const DefaultRotationGrace = 24 * time.Hour

// API is the administration HTTP surface. It is safe for concurrent use.
type API struct {
	cfg     Config
	routes  map[string]*route
	metrics Metrics

	ui *uiServer
}

// handler serves one administrative request. Returning an error is the normal
// way to refuse: the error is mapped to the wire exactly once, in ServeHTTP.
type handler func(c *call) error

// call is one authorized administrative request.
type call struct {
	w http.ResponseWriter
	r *http.Request
	p Principal
	a *API
}

func (c *call) ctx() context.Context { return c.r.Context() }

// route is one path and the methods it answers.
type route struct {
	methods map[string]handler
	allow   string
}

// New validates the configuration and builds the surface.
func New(cfg Config) (*API, error) {
	if cfg.Auth == nil {
		return nil, errors.New("admin: Config.Auth is required")
	}
	if cfg.MaxTimeRange <= 0 {
		cfg.MaxTimeRange = DefaultMaxTimeRange
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = DefaultPageSize
	}
	if cfg.MaxPageSize <= 0 {
		cfg.MaxPageSize = DefaultMaxPageSize
	}
	if cfg.PageSize > cfg.MaxPageSize {
		cfg.PageSize = cfg.MaxPageSize
	}
	if cfg.ListLimit <= 0 {
		cfg.ListLimit = DefaultListLimit
	}
	if cfg.MaxListLimit <= 0 {
		cfg.MaxListLimit = DefaultMaxListLimit
	}
	if cfg.ListLimit > cfg.MaxListLimit {
		cfg.ListLimit = cfg.MaxListLimit
	}
	if cfg.UIPrefix == "" {
		cfg.UIPrefix = DefaultUIPrefix
	}
	if cfg.SessionCookie == "" {
		cfg.SessionCookie = DefaultSessionCookie
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = DefaultSessionTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewID == nil {
		cfg.NewID = newID
	}
	if cfg.NewToken == nil {
		cfg.NewToken = newToken
	}
	if cfg.RotationGrace <= 0 {
		cfg.RotationGrace = DefaultRotationGrace
	}

	a := &API{cfg: cfg, routes: map[string]*route{}}
	a.register()

	if cfg.UIPrefix != "-" {
		u, err := newUIServer(a)
		if err != nil {
			return nil, err
		}
		a.ui = u
	}
	return a, nil
}

// Now reports the configured clock's instant.
func (a *API) now() time.Time { return a.cfg.Now() }

// Metrics returns a snapshot of the surface's fixed-cardinality counters
// (§12.3). Nothing here is labelled by a path, an object id or a caller: a
// counter labelled with a caller-supplied string hands every caller a way to
// grow the process's memory without limit.
func (a *API) Metrics() MetricsSnapshot { return a.metrics.snapshot() }

// mount registers one method on one path.
func (a *API) mount(method, p string, h handler) {
	rt := a.routes[p]
	if rt == nil {
		rt = &route{methods: map[string]handler{}}
		a.routes[p] = rt
	}
	rt.methods[method] = h
	names := make([]string, 0, len(rt.methods)+1)
	for m := range rt.methods {
		names = append(names, m)
	}
	names = append(names, http.MethodOptions)
	sort.Strings(names)
	rt.allow = strings.Join(names, ", ")
}

// read mounts a read-only endpoint. GET is the honest method; POST is accepted
// alongside it because the incumbent's own clients POST to several of these
// paths and §2.3 exists so those clients keep working.
func (a *API) read(p string, h handler) {
	a.mount(http.MethodGet, p, h)
	a.mount(http.MethodPost, p, h)
}

// write mounts a mutating endpoint.
func (a *API) write(p string, h handler) {
	a.mount(http.MethodPost, p, h)
}

// stub mounts a path that answers 501 with a specific code, so that a caller
// probing the surface is told which piece is missing rather than being left to
// guess from a 404 (§0.2).
func (a *API) stub(p, code, reason string) {
	h := func(c *call) error { return unimplemented(code, "%s", reason) }
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		a.mount(m, p, h)
	}
}

// ServeHTTP dispatches one administrative request.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := cleanPath(r.URL.Path)

	if a.ui != nil && (p == a.cfg.UIPrefix || strings.HasPrefix(p, a.cfg.UIPrefix+"/")) {
		a.metrics.uiRequests.Add(1)
		a.ui.serve(w, r, strings.TrimPrefix(p, a.cfg.UIPrefix))
		return
	}

	a.metrics.requests.Add(1)

	// Authentication comes FIRST — before the route lookup, before OPTIONS,
	// before the 405 and before the 501.
	//
	// It used to come last, and everything above it was an oracle: an
	// unauthenticated caller learned which administrative routes this build
	// serves, which methods each one accepts, and — from the difference between
	// the generic 501 and a stub's specific code — which features exist. None
	// of that is catastrophic on its own. All of it is free reconnaissance
	// against a surface whose whole job is privileged mutation, and none of it
	// is information an anonymous caller has any business having.
	//
	// The ordering also removes a subtler hazard the review named as
	// "read-before-auth": every answer above depended on values taken from the
	// request — the path, the method — and each one is a place where a future
	// handler-shaped refactor could start reading the BODY before anyone had
	// decided the caller was allowed to send one. There is now exactly one
	// thing that happens before the authorization decision, and it is
	// normalizing the path so the decision can be made about it.
	pr, err := a.authenticate(r)
	if err != nil {
		a.metrics.authFailures.Add(1)
		writeFault(w, r, faultFor(err))
		return
	}

	rt := a.routes[p]
	if rt == nil {
		// Not a 404. §0.2: an unimplemented administrative route answers 501
		// with a reason, because a caller must be able to distinguish "dorang
		// has not built this" from "you typed the URL wrong", and only a code
		// distinguishes them. The caller is authenticated by this point, so the
		// reason is being told to someone entitled to it.
		a.metrics.unimplemented.Add(1)
		writeFault(w, r, unimplemented(CodeNotImplemented,
			"%s is not an implemented administration route; see DESIGN §2.3 for the shape-compatible set", p))
		return
	}

	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", rt.allow)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	h, ok := rt.methods[r.Method]
	if !ok && r.Method == http.MethodHead {
		h, ok = rt.methods[http.MethodGet]
	}
	if !ok {
		w.Header().Set("Allow", rt.allow)
		writeFault(w, r, newFault(http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			typeInvalidRequest, "%s is not allowed on %s", r.Method, p))
		return
	}

	if err := h(&call{w: w, r: r, p: pr, a: a}); err != nil {
		f := faultFor(err)
		if f.Status >= 500 {
			a.metrics.serverErrors.Add(1)
		}
		if f.Status == http.StatusNotImplemented {
			a.metrics.unimplemented.Add(1)
		}
		writeFault(w, r, f)
	}
}

// authenticate resolves the caller and enforces the entry condition §2.3 has:
// the out-of-band master credential, or a key with an administrative role.
//
// It answers "may this caller use the administration surface at all". WHICH
// subjects they may then act on is [Scope], enforced per handler — the two are
// separate questions and the review's finding was that only the first one was
// being asked.
func (a *API) authenticate(r *http.Request) (Principal, error) {
	p, err := a.cfg.Auth.AuthenticateHeader(r.Context(), r.Header)
	if err != nil {
		var f *fault
		if errors.As(err, &f) {
			return nil, f
		}
		return nil, ErrUnauthenticated
	}
	if p == nil {
		return nil, ErrUnauthenticated
	}
	if !p.IsAdmin() {
		return nil, ErrForbidden
	}
	return p, nil
}

// cleanPath normalizes a request path for exact matching. A trailing slash is
// removed so that /key/list/ and /key/list are the same route rather than one
// route and one 501.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	p = path.Clean(p)
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimRight(p, "/")
	}
	return p
}

// ---------------------------------------------------------------------------
// Dependency guards
// ---------------------------------------------------------------------------

func (a *API) keys() (KeyStore, error) {
	if a.cfg.Keys == nil {
		return nil, dependencyOff("key store", "credential administration")
	}
	return a.cfg.Keys, nil
}

func (a *API) hasher() (Hasher, error) {
	if a.cfg.Hasher == nil {
		return nil, dependencyOff("key hasher", "issuing credentials")
	}
	return a.cfg.Hasher, nil
}

func (a *API) directory() (Directory, error) {
	if a.cfg.Directory == nil {
		return nil, dependencyOff("directory", "user and team administration")
	}
	return a.cfg.Directory, nil
}

func (a *API) models() (ModelRegistry, error) {
	if a.cfg.Models == nil {
		return nil, dependencyOff("model registry", "model and deployment administration")
	}
	return a.cfg.Models, nil
}

func (a *API) budgets() (BudgetStore, error) {
	if a.cfg.Budgets == nil {
		return nil, dependencyOff("budget store", "budget administration")
	}
	return a.cfg.Budgets, nil
}

func (a *API) ledger() (Ledger, error) {
	if a.cfg.Ledger == nil {
		return nil, dependencyOff("ledger", "spend reporting")
	}
	return a.cfg.Ledger, nil
}

// ---------------------------------------------------------------------------
// Identifiers
// ---------------------------------------------------------------------------

var idAlphabet = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// newID mints a random object identifier. Random rather than sequential: an
// administrative id that leaks a creation order also leaks a rate.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it ever
		// does, minting a guessable id is the wrong recovery.
		panic("admin: crypto/rand unavailable: " + err.Error())
	}
	return idAlphabet.EncodeToString(b[:])
}

// keyPrefix is the prefix every dorang-issued key carries. Client libraries
// pattern-match on it, and §2.4's lookup is computed over the whole token, so
// the prefix costs nothing.
const keyPrefix = "sk-"

// newToken mints a key's plaintext secret: 32 bytes of cryptographic randomness
// behind the conventional prefix. It is returned to the caller exactly once and
// is never stored — only its lookup and its digest are.
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return keyPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// UIPrefix is where the operator UI is mounted, or "" when it is disabled.
//
// The caller that mounts this surface needs the answer and must not guess it:
// the prefix is configurable, "-" disables the UI entirely, and a mount that
// assumed the default would serve a 501 on the configured path and route
// nothing to the real one.
func (a *API) UIPrefix() string {
	if a.ui == nil {
		return ""
	}
	return a.cfg.UIPrefix
}

// NewID mints a random administrative object identifier. It is exported so
// that an adapter minting audit-row ids uses the same generator the surface
// uses for everything else — random rather than sequential, because an id that
// leaks a creation order also leaks a rate.
func NewID() string { return newID() }
