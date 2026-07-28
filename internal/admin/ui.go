package admin

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ziozzang/dorang/ui"
)

// The operator UI is deliberately read-only.
//
// It authenticates with a cookie, because a browser cannot be made to send a
// bearer header on a normal navigation, and a cookie-authenticated *mutating*
// surface is a cross-site request forgery surface that would then need tokens,
// double-submit checks and a review of every form. §11.3 asks for three screens
// — keys, models and deployments, usage and cost — and all three are views.
// Creating a key, rotating one, changing a deployment: those go through the
// API, with a bearer token, from a terminal or a script, where the one-time
// secret of §2.4 can be handled properly rather than pasted into a browser tab.
//
// The API never accepts the cookie, and the UI never accepts a mutation. Those
// two sentences are the whole security model, and both are enforced structurally
// rather than by review: the cookie is read only inside this file, and no route
// registered here has a side effect beyond the session itself.

// uiServer renders the three screens.
type uiServer struct {
	api    *API
	pages  map[string]*template.Template
	assets fs.FS

	mu       sync.Mutex
	sessions map[string]uiSession
}

type uiSession struct {
	// actor is the label shown in the page header. An operator with several
	// credentials open in several tabs needs to know which one this tab is.
	actor string
	// scope is the administrative scope resolved when the session was created.
	// It is stored rather than re-derived because the cookie is deliberately
	// not the credential, so there is nothing left to re-derive it from — which
	// also means a scope CHANGE is not observed until the session ends, on the
	// same one-hour bound as a revocation.
	scope   Scope
	expires time.Time
}

// newUIServer parses the embedded templates once, at construction.
//
// Parsing at construction rather than per request means a broken template is a
// startup failure rather than a 500 discovered by an operator during an
// incident. The test that asserts the UI "actually parses and serves" is
// therefore mostly a test that this constructor is reached.
func newUIServer(a *API) (*uiServer, error) {
	tpls := ui.Templates()
	pages := map[string]*template.Template{}
	for _, name := range ui.Pages() {
		t, err := template.New("layout.html").Funcs(uiFuncs).ParseFS(tpls, "layout.html", name+".html")
		if err != nil {
			return nil, fmt.Errorf("admin: parsing ui template %q: %w", name, err)
		}
		pages[name] = t
	}
	return &uiServer{
		api:      a,
		pages:    pages,
		assets:   ui.Assets(),
		sessions: map[string]uiSession{},
	}, nil
}

// ---------------------------------------------------------------------------
// Template helpers
// ---------------------------------------------------------------------------

var uiFuncs = template.FuncMap{
	"rfc3339": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format(time.RFC3339)
	},
	"stamp": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04")
	},
	"date": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02")
	},
	"money": func(nano int64) string { return formatNano(nano) },
	"deref": func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	},
	"limit": func(p *int64) string {
		if p == nil {
			return "—"
		}
		return strconv.FormatInt(*p, 10)
	},
	"num": humanInt,
	"ms": func(v int64) string {
		if v <= 0 {
			return "—"
		}
		return strconv.FormatInt(v, 10)
	},
	"healthClass": func(h string) string {
		switch h {
		case "healthy":
			return "state-ok"
		case "half_open":
			return "state-warn"
		case "":
			return "unavailable"
		default:
			return "state-bad"
		}
	},
	// leverage renders notional ÷ billed over the window. §8.5 is explicit that
	// this ratio is meaningful per *period* and misleading per request, because
	// a flat plan with no marginal rule amortizes by elapsed time — so a quiet
	// hour reads as poor leverage and a busy one as excellent, when neither is a
	// fact about the plan. The screen therefore renders it only for the whole
	// selected window, and the caption says so.
	"leverage": func(u Usage) string {
		if !u.NotionalKnown {
			return "—"
		}
		if u.CostNano <= 0 {
			if u.NotionalNano > 0 {
				return "∞"
			}
			return "—"
		}
		ratio := float64(u.NotionalNano) / float64(u.CostNano)
		return strconv.FormatFloat(ratio, 'f', 2, 64) + "×"
	},
}

// humanInt groups digits so that a seven-figure token count is readable at a
// glance, which is the only reason an operator looks at it.
func humanInt(v int64) string {
	s := strconv.FormatInt(v, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Page data
// ---------------------------------------------------------------------------

type page struct {
	Title   string
	Screen  string
	Base    string
	APIBase string
	Nav     bool
	// Session is true when a cookie session is in play, which is the only case
	// where a sign-out control means anything.
	Session bool
	// Actor names the credential this view is authorized by.
	Actor     string
	Notice    string
	Problem   string
	Generated time.Time
}

type keyRow struct {
	*Key
	Expired bool
}

type keysPage struct {
	page
	Keys []keyRow
}

type modelsPage struct {
	page
	Deployments []*Deployment
	Aliases     []Alias

	HealthAvailable bool
	Credentials     []CredentialStatus

	CapacityAvailable bool
	Capacity          CapacityOccupancy
}

type usageRow struct {
	Day   time.Time
	Value string
	Usage
}

type usagePage struct {
	page
	Range             Range
	Totals            Usage
	NotionalAvailable bool
	Days              []usageRow
	Models            []usageRow
	Teams             []usageRow
}

type loginPage struct {
	page
	Next string
}

type messagePage struct {
	page
	Heading string
	Body    string
	Code    string
}

func (s *uiServer) base() string { return s.api.cfg.UIPrefix }

func (s *uiServer) newPage(screen, title string, v viewer) page {
	return page{
		Title:     title,
		Screen:    screen,
		Base:      s.base(),
		APIBase:   "",
		Nav:       true,
		Session:   v.signedIn && !s.api.cfg.DisableUISessions,
		Actor:     v.actor,
		Generated: s.api.now(),
	}
}

// viewer is who is looking at a screen: the label to show, whether a cookie
// session is what authorized them, and — the part that is not decoration — the
// scope their answers are bounded by.
//
// The scope travels on the viewer rather than being looked up per screen
// because a screen that had to remember to ask would be a screen that could
// forget to, and the keys screen forgetting means one tenant's operator reading
// every tenant's credentials.
type viewer struct {
	actor    string
	signedIn bool
	scope    Scope
}

// actorLabel renders a principal for the page header. It is an identity, never
// a credential: the master credential has no row and therefore no id.
func actorLabel(p Principal) string {
	if p.ActorKind() == "master" {
		return "master credential"
	}
	if id := p.ActorID(); id != "" {
		return p.ActorKind() + " " + id
	}
	return p.ActorKind()
}

// ---------------------------------------------------------------------------
// Serving
// ---------------------------------------------------------------------------

// serve dispatches a UI request. rest is the path with the mount prefix
// removed.
func (s *uiServer) serve(w http.ResponseWriter, r *http.Request, rest string) {
	s.securityHeaders(w)

	switch rest {
	case "", "/":
		http.Redirect(w, r, s.base()+"/keys", http.StatusSeeOther)
		return
	case "/assets/style.css", "/assets/app.js":
		s.serveAsset(w, r, strings.TrimPrefix(rest, "/assets/"))
		return
	case "/login":
		s.serveLogin(w, r)
		return
	case "/logout":
		s.serveLogout(w, r)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "the operator UI is read-only", http.StatusMethodNotAllowed)
		return
	}

	v, ok := s.authorize(w, r)
	if !ok {
		return
	}

	switch rest {
	case "/keys":
		s.screenKeys(w, r, v)
	case "/models":
		s.screenModels(w, r, v)
	case "/usage":
		s.screenUsage(w, r, v)
	default:
		// The same rule as the API: an unimplemented screen says so rather
		// than pretending the URL was wrong.
		s.renderMessage(w, r, http.StatusNotImplemented, v,
			"Not implemented",
			"There is no such screen. Version 1 ships keys, models and deployments, and usage and cost (DESIGN §11.3).",
			CodeNotImplemented)
	}
}

// securityHeaders locks the page down to its own origin. The content security
// policy is not decoration: it is the machine-checkable form of §11.3's "no
// external assets", and a stylesheet or script that ever reached for a CDN
// would stop rendering rather than silently start phoning home.
func (s *uiServer) securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
			"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
}

func (s *uiServer) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	f, err := s.assets.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "asset unavailable", http.StatusInternalServerError)
		return
	}
	switch {
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	// Embedded assets change only with the binary, so they may be cached; the
	// header set by securityHeaders is for pages, not for these.
	w.Header().Set("Cache-Control", "public, max-age=300")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
}

// authorize resolves the viewer. It reports whether a cookie session was used,
// so the layout can offer a sign-out control only when there is something to
// sign out of.
func (s *uiServer) authorize(w http.ResponseWriter, r *http.Request) (viewer, bool) {
	if !s.api.cfg.DisableUISessions {
		if c, err := r.Cookie(s.api.cfg.SessionCookie); err == nil {
			if sess, live := s.lookupSession(c.Value); live {
				// The session carries the scope that was resolved when it was
				// created, alongside the authorization decision it already
				// held. Re-deriving it per request would need the credential,
				// which the cookie deliberately is not.
				return viewer{actor: sess.actor, signedIn: true, scope: sess.scope}, true
			}
		}
	}
	// Fall back to the API's own credential, so that a deployment fronted by an
	// authenticating proxy — or an operator with curl — reaches the same pages.
	if p, err := s.api.authenticate(r); err == nil {
		return viewer{actor: actorLabel(p), scope: p.AdminScope()}, true
	}
	if s.api.cfg.DisableUISessions {
		http.Error(w, "an administrative credential is required", http.StatusUnauthorized)
		return viewer{}, false
	}
	next := r.URL.Path
	if r.URL.RawQuery != "" {
		next += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, s.base()+"/login?next="+urlQueryEscape(next), http.StatusSeeOther)
	return viewer{}, false
}

func urlQueryEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "&", "%26"), "?", "%3F")
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// A session is a random opaque id held in a cookie, resolved against a table in
// this process. Three consequences, all stated rather than discovered:
//
//   - The cookie is not the credential. Signing out revokes the session, and a
//     stolen cookie expires on its own.
//   - Revocation of the underlying key is not observed mid-session. The session
//     TTL therefore *is* the revocation lag, which is why the default is an hour
//     rather than a working day.
//   - Sessions are per process. With N nodes an operator signs in again on
//     whichever node they land on, which is the explicit answer §0.2 asks every
//     stateful mechanism to give. Nothing else in the system depends on them.
func (s *uiServer) lookupSession(id string) (uiSession, bool) {
	if id == "" {
		return uiSession{}, false
	}
	now := s.api.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return uiSession{}, false
	}
	if !now.Before(sess.expires) {
		delete(s.sessions, id)
		return uiSession{}, false
	}
	return sess, true
}

func (s *uiServer) newSession(p Principal) string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("admin: crypto/rand unavailable: " + err.Error())
	}
	id := base64.RawURLEncoding.EncodeToString(b[:])
	now := s.api.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	// Sweep on write. Sessions are few and short-lived, so a sweep here is
	// cheaper and simpler than a goroutine that must then be stopped.
	for k, v := range s.sessions {
		if !now.Before(v.expires) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = uiSession{
		actor:   actorLabel(p),
		scope:   p.AdminScope(),
		expires: now.Add(s.api.cfg.SessionTTL),
	}
	return id
}

func (s *uiServer) dropSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (s *uiServer) serveLogin(w http.ResponseWriter, r *http.Request) {
	if s.api.cfg.DisableUISessions {
		http.Error(w, "UI sessions are disabled; present the administrative credential as a header",
			http.StatusNotImplemented)
		return
	}
	next := safeNext(s.base(), r.FormValue("next"))

	if r.Method != http.MethodPost {
		s.render(w, r, http.StatusOK, "login", loginPage{
			page: page{Title: "Sign in", Screen: "login", Base: s.base(), Generated: s.api.now()},
			Next: next,
		})
		return
	}

	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		s.render(w, r, http.StatusBadRequest, "login", loginPage{
			page: page{Title: "Sign in", Screen: "login", Base: s.base(),
				Problem: "A credential is required.", Generated: s.api.now()},
			Next: next,
		})
		return
	}

	// The credential is verified through the same authenticator the API uses,
	// by synthesizing the header it expects. There is no second code path that
	// decides who is an administrator, and therefore no second place for the
	// two to disagree.
	probe := r.Clone(r.Context())
	probe.Header = http.Header{}
	probe.Header.Set("Authorization", "Bearer "+key)
	p, err := s.api.authenticate(probe)
	if err != nil {
		s.render(w, r, http.StatusUnauthorized, "login", loginPage{
			page: page{Title: "Sign in", Screen: "login", Base: s.base(),
				Problem: "That credential is not an administrative credential.", Generated: s.api.now()},
			Next: next,
		})
		return
	}

	id := s.newSession(p)
	http.SetCookie(w, &http.Cookie{
		Name:     s.api.cfg.SessionCookie,
		Value:    id,
		Path:     s.base(),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		Expires:  s.api.now().Add(s.api.cfg.SessionTTL),
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *uiServer) serveLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.api.cfg.SessionCookie); err == nil {
		s.dropSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.api.cfg.SessionCookie,
		Value:    "",
		Path:     s.base(),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, s.base()+"/login", http.StatusSeeOther)
}

// safeNext refuses an open redirect. Only a path under this UI's own mount is
// accepted; anything else falls back to the first screen.
func safeNext(base, next string) string {
	if next == "" || !strings.HasPrefix(next, base+"/") || strings.HasPrefix(next, base+"//") {
		return base + "/keys"
	}
	if strings.Contains(next, "\n") || strings.Contains(next, "\r") {
		return base + "/keys"
	}
	return next
}

// ---------------------------------------------------------------------------
// Screens
// ---------------------------------------------------------------------------

func (s *uiServer) screenKeys(w http.ResponseWriter, r *http.Request, v viewer) {
	ks := s.api.cfg.Keys
	if ks == nil {
		s.renderMessage(w, r, http.StatusNotImplemented, v, "Keys",
			"A key store is not configured in this process.", CodeDependencyOff)
		return
	}
	f := KeyFilter{Limit: s.api.cfg.MaxListLimit}
	if !v.scope.Global && len(v.scope.Teams) == 1 {
		f.TeamID = v.scope.Teams[0]
	}
	keys, err := ks.ListKeys(r.Context(), f)
	if err != nil {
		s.renderError(w, r, v, "Keys", err)
		return
	}
	now := s.api.now()
	rows := make([]keyRow, 0, len(keys))
	for _, k := range keys {
		// The filter above is an optimization; this is the enforcement. A
		// screen must not be the one place the scope is not applied, and the
		// UI reads the SAME store the API reads.
		if !v.scope.AllowsTeam(k.TeamID) {
			continue
		}
		rows = append(rows, keyRow{Key: k, Expired: k.Expired(now)})
	}
	s.render(w, r, http.StatusOK, "keys", keysPage{
		page: s.newPage("keys", "Keys", v),
		Keys: rows,
	})
}

func (s *uiServer) screenModels(w http.ResponseWriter, r *http.Request, v viewer) {
	// Deployment configuration, credential health and capacity occupancy are
	// the operator's, and there is no per-team view of them.
	if !v.scope.Global {
		s.renderMessage(w, r, http.StatusForbidden, v, "Models & deployments",
			"This credential administers "+v.scope.String()+
				". Models and deployments are administered for the whole deployment.",
			CodeOutOfScope)
		return
	}
	reg := s.api.cfg.Models
	if reg == nil {
		s.renderMessage(w, r, http.StatusNotImplemented, v, "Models & deployments",
			"A model registry is not configured in this process.", CodeDependencyOff)
		return
	}
	deps, err := reg.ListDeployments(r.Context())
	if err != nil {
		s.renderError(w, r, v, "Models & deployments", err)
		return
	}
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].ModelGroup != deps[j].ModelGroup {
			return deps[i].ModelGroup < deps[j].ModelGroup
		}
		return deps[i].ID < deps[j].ID
	})
	aliases, err := reg.ListAliases(r.Context())
	if err != nil {
		s.renderError(w, r, v, "Models & deployments", err)
		return
	}
	sort.Slice(aliases, func(i, j int) bool { return aliases[i].Alias < aliases[j].Alias })

	pg := modelsPage{
		page:        s.newPage("models", "Models & deployments", v),
		Deployments: deps,
		Aliases:     aliases,
	}
	if s.api.cfg.Credentials != nil {
		creds, err := s.api.cfg.Credentials.Credentials(r.Context())
		if err == nil {
			sort.Slice(creds, func(i, j int) bool { return creds[i].ID < creds[j].ID })
			pg.HealthAvailable = true
			pg.Credentials = creds
		}
	}
	if s.api.cfg.Capacity != nil {
		occ, err := s.api.cfg.Capacity.Occupancy(r.Context())
		if err == nil {
			pg.CapacityAvailable = true
			pg.Capacity = occ
		}
	}
	s.render(w, r, http.StatusOK, "models", pg)
}

func (s *uiServer) screenUsage(w http.ResponseWriter, r *http.Request, v viewer) {
	// The usage screen is a deployment-wide aggregate over the ledger, which
	// carries no subject filter. Same rule as /global/spend/report.
	if !v.scope.Global {
		s.renderMessage(w, r, http.StatusForbidden, v, "Usage & cost",
			"This credential administers "+v.scope.String()+
				". The usage screen aggregates the whole deployment.",
			CodeOutOfScope)
		return
	}
	led := s.api.cfg.Ledger
	if led == nil {
		s.renderMessage(w, r, http.StatusNotImplemented, v, "Usage & cost",
			"A ledger is not configured in this process.", CodeDependencyOff)
		return
	}

	// The screen supplies a bounded default window rather than omitting one.
	// §9.3 refuses an unbounded query; a UI that offered "all time" would be
	// asking for exactly that, so the control is a date range with a sane
	// default and no way to clear it.
	now := s.api.now().UTC()
	end := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
	start := end.Add(-7 * 24 * time.Hour)
	if v := r.URL.Query().Get("start_date"); v != "" {
		if t, _, err := parseBound(v); err == nil {
			start = t
		}
	}
	if v := r.URL.Query().Get("end_date"); v != "" {
		if t, dateOnly, err := parseBound(v); err == nil {
			if dateOnly {
				t = t.Add(24 * time.Hour)
			}
			end = t
		}
	}
	pg := usagePage{
		page:  s.newPage("usage", "Usage & cost", v),
		Range: Range{Start: start, End: end},
	}
	if !end.After(start) {
		pg.Problem = "The end of the window must be after its start."
		s.render(w, r, http.StatusBadRequest, "usage", pg)
		return
	}
	if width := end.Sub(start); width > s.api.cfg.MaxTimeRange {
		pg.Problem = "That window is wider than this deployment allows (" +
			s.api.cfg.MaxTimeRange.String() + "). Narrow it; a partial answer would look complete."
		s.render(w, r, http.StatusBadRequest, "usage", pg)
		return
	}

	rng := Range{Start: start, End: end}
	byDay, err := led.Report(r.Context(), ReportQuery{Range: rng, GroupBy: []GroupBy{GroupByDay}, Limit: 400})
	if err != nil {
		s.renderError(w, r, v, "Usage & cost", err)
		return
	}
	byModel, err := led.Report(r.Context(), ReportQuery{Range: rng, GroupBy: []GroupBy{GroupByModel}, Limit: 200})
	if err != nil {
		s.renderError(w, r, v, "Usage & cost", err)
		return
	}
	byTeam, err := led.Report(r.Context(), ReportQuery{Range: rng, GroupBy: []GroupBy{GroupByTeam}, Limit: 200})
	if err != nil {
		s.renderError(w, r, v, "Usage & cost", err)
		return
	}

	pg.Totals = byDay.Total
	pg.NotionalAvailable = byDay.Total.NotionalKnown
	if !pg.NotionalAvailable {
		s.api.metrics.notionalMissing.Add(1)
	}
	for _, row := range byDay.Rows {
		pg.Days = append(pg.Days, usageRow{Day: row.Day, Usage: row.Usage})
	}
	sort.Slice(pg.Days, func(i, j int) bool { return pg.Days[i].Day.After(pg.Days[j].Day) })
	for _, row := range byModel.Rows {
		pg.Models = append(pg.Models, usageRow{Value: row.ModelGroup, Usage: row.Usage})
	}
	sort.Slice(pg.Models, func(i, j int) bool { return pg.Models[i].CostNano > pg.Models[j].CostNano })
	for _, row := range byTeam.Rows {
		pg.Teams = append(pg.Teams, usageRow{Value: row.TeamID, Usage: row.Usage})
	}
	sort.Slice(pg.Teams, func(i, j int) bool { return pg.Teams[i].CostNano > pg.Teams[j].CostNano })

	s.render(w, r, http.StatusOK, "usage", pg)
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// render executes a page into a buffer first. A template that fails halfway
// through would otherwise have already written a status and half a document,
// and the operator would be looking at a truncated table with no indication
// that anything went wrong.
func (s *uiServer) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "no such page", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(buf.Bytes())
}

func (s *uiServer) renderMessage(w http.ResponseWriter, r *http.Request, status int, v viewer,
	heading, body, code string) {
	pg := s.newPage("", heading, v)
	pg.Title = heading
	s.render(w, r, status, "message", messagePage{page: pg, Heading: heading, Body: body, Code: code})
}

// renderError shows a dependency's refusal without leaking its text. An error
// from a driver can carry a DSN, and a DSN can carry a password, so only
// dorang's own classification reaches the page.
func (s *uiServer) renderError(w http.ResponseWriter, r *http.Request, v viewer, heading string, err error) {
	f := faultFor(err)
	body := f.Message
	if f.Status >= 500 && !errors.Is(err, ErrUnsupported) {
		body = "The data source refused the request."
	}
	s.renderMessage(w, r, f.Status, v, heading, body, f.Code)
}
