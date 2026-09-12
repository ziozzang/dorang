package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ziozzang/dorang/ui"
)

// The operator UI reads, and — for the credential lifecycle of §11.2c and
// §11.6 — writes.
//
// It authenticates with a cookie, because a browser cannot be made to send a
// bearer header on a normal navigation. This file used to argue that a
// cookie-authenticated *mutating* surface is a cross-site request forgery
// surface "that would then need tokens, double-submit checks and a review of
// every form", and therefore that creating, rotating and revoking a key belong
// on the API. The argument was right about the cost and the cost is now paid
// rather than avoided. What follows is what was bought with it, because a
// mutating surface with weak forgery defences is worse than no surface at all:
// the operator believes it is safe.
//
// # The security model, in three sentences that are all enforced by construction
//
//  1. The API never accepts the cookie. This half is unchanged and is now the
//     older half. It is not a promise about somebody else's [Authenticator]:
//     [API.authenticate] hands over a header map with `Cookie` REMOVED, so a
//     session cookie is not merely unread on the API — it is not there. A test
//     with an authenticator that would happily accept one asserts the 401.
//  2. A mutation is reachable only through [uiServer.authorize], which for any
//     method other than GET and HEAD refuses unless the request carries the
//     token minted with the session it presents. That is not a guard a handler
//     remembers to call: authorize is the ONLY producer of the [viewer] value
//     a screen or an action needs, so a route added later without it has no
//     scope to filter by, no principal to act as and no page to render.
//  3. There is exactly one mutating URL — [uiActionPath] — and every action it
//     will perform is one row of [uiActions]. The dispatcher reads that table
//     and so does the test that forges a cross-site POST at each row, so an
//     action added tomorrow is covered by the forgery test on the same commit.
//
// The session cookie is `SameSite=Strict` and an unsafe request whose `Origin`
// or `Sec-Fetch-Site` positively disagrees with this host is refused before
// anything parses its body. Neither is the mechanism: both are layers over the
// token, which is what a browser cannot obtain across origins.
//
// # What the operator's choice costs, stated where it is paid
//
// §2.4's one-time secret is now shown in a browser tab. It is in the DOM, in
// that tab's scroll-back, in session restore, and in any screenshot or screen
// share of the window — none of which dorang can withdraw. `secret.html` says
// exactly that at the moment the secret is on screen, rather than in a document
// nobody is reading at 3 a.m. What dorang can promise it does promise: the
// secret is held in exactly one place ([uiSession.reveal]), for exactly one
// navigation, taken out under the same lock that reads it, and never written to
// a URL, a log line, an audit row or the store. A reload of the page that
// showed it answers 410 and cannot show it again.
//
// # Mutating requires a session, and a header-authenticated view is read-only
//
// A viewer authorized by the API's own credential — curl, or a deployment
// fronted by an authenticating proxy with `DisableUISessions` — reads the
// screens and cannot act on them. There is no browser client for a bearer
// header, so the only thing such a path could add is a forgeable surface for a
// proxy that authenticates with a cookie of its own, which dorang cannot see
// and therefore cannot bind a token to. Those deployments use the API, which is
// where they were already.

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
	// actorKind and actorID name the credential the session was created from.
	//
	// They are what makes the session re-checkable. The cookie is deliberately
	// not the credential, so the session cannot be re-authenticated — but the
	// key it was issued from has a durable id, and whether THAT key still
	// serves is a question the key store answers on every request. See
	// [uiServer.sessionAuthorized]. The master credential has no row by
	// construction (§2.4) and therefore an empty id.
	actorKind string
	actorID   string
	// scope is the administrative scope resolved when the session was created.
	// It is stored rather than re-derived because the cookie is deliberately
	// not the credential, so there is nothing left to re-derive it from. A
	// scope CHANGE is not re-derived either — it is DETECTED, by checking the
	// stored scope against the key's own team on every request, and a session
	// whose scope no longer covers its key ends rather than lingering.
	scope   Scope
	expires time.Time

	// csrf is this session's synchronizer token: 32 bytes of crypto/rand,
	// minted with the session and never leaving it.
	//
	// It is a per-session server-side token rather than a double-submit cookie
	// because there is already a server-side session table to hang it on, and
	// because double-submit is only as strong as the weakest thing that can set
	// a cookie on the parent domain — a sibling host on the same registrable
	// domain, or one plaintext response on a deployment that has not finished
	// its TLS rollout. Nothing outside this process can produce this value, and
	// [uiServer.authorize] compares it in constant time before any unsafe
	// request is allowed to become a [viewer].
	csrf string

	// reveal is the one-time secret waiting to be shown to this session, and
	// the only place this process ever holds a plaintext credential.
	//
	// It lives here, on the session, rather than in a table keyed by a ticket,
	// so that nothing about the secret — not even a handle to it — is ever put
	// in a URL: the redirect after a create or a rotate is a static path. It is
	// taken out by [uiServer.takeReveal] under the same lock that reads it, so
	// two tabs racing produce one page with the secret and one 410.
	reveal *revealed

	// flash is the one-line outcome of the last mutation, shown on the next
	// page and then gone. It exists so that a successful action can redirect
	// (a reload must not re-submit a mutation) without putting its result in a
	// query string.
	flash string
}

// revealed is a secret on its way to exactly one page.
//
// Everything here except [revealed.Secret] is safe to render twice; the whole
// value is discarded together anyway, because the moment the secret is gone the
// rest of it is a worse copy of what the keys screen already shows.
type revealed struct {
	// Secret is the plaintext credential. It is never persisted, never logged,
	// never put in a URL and never re-fetchable.
	Secret string
	// Action is "created" or "rotated" — the two operations that mint one.
	Action string
	KeyID  string
	Label  string
	Alias  string
	// Expires is the new key's own expiry, empty when it never expires.
	Expires string
	// PreviousExpires and Grace are the rotation's grace window: when the OLD
	// secret stops authenticating, which is the fact the operator has to act on
	// before it does.
	PreviousExpires string
	Grace           string
	// deadline bounds how long an unread reveal sits in memory. A secret nobody
	// came back for is dropped rather than kept for the session's hour.
	deadline time.Time
}

// revealTTL bounds an unclaimed reveal. It is the width of one redirect, not a
// window an operator is expected to work inside: the page is served by the very
// next request the browser makes.
const revealTTL = 2 * time.Minute

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
	// a plan cost accrues with its period (§8.1) rather than with usage — so a
	// quiet hour reads as poor leverage and a busy one as excellent, when
	// neither is a fact about the plan. The screen therefore renders it only for the whole
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

// navScreen is one entry in the header navigation.
//
// The nav is built from what this process can actually serve THIS viewer, and
// not from a fixed list of three. It was a fixed list, and the consequence was
// the models screen: advertised on every page, answering 501 on every click in
// every deployment, because nothing was wired behind it. An operator clicking a
// permanent 501 does not learn that a dependency is optional — they learn the
// product is broken.
type navScreen struct {
	Screen  string
	Href    string
	Label   string
	Current bool
}

// navGroup is a labelled section of the sidebar. Grouping is presentation only:
// the same screens, bucketed so the nav reads as a short list of sections
// rather than one long column. A group with no available screen is not emitted.
type navGroup struct {
	Title   string
	Screens []navScreen
}

// navSections is the order the sidebar groups appear in and which screen each
// carries. A screen not named here still navigates (it stays in the flat list),
// but it lands in the catch-all last group rather than being dropped.
var navSections = []struct {
	title   string
	screens []string
}{
	{"access", []string{"keys", "users"}},
	{"catalog", []string{"models"}},
	{"observability", []string{"usage", "monitoring"}},
}

type page struct {
	Title   string
	Screen  string
	Base    string
	APIBase string
	// Screens is the navigation: the screens this process can serve to this
	// viewer, in order. Empty renders no navigation at all. It stays flat for
	// the "somewhere to go" computation and for callers that read the whole
	// list; the sidebar renders NavGroups, which is the same screens bucketed.
	Screens []navScreen
	// NavGroups is Screens grouped into labelled sidebar sections. Derived from
	// Screens, so the two never disagree about which screens exist.
	NavGroups []navGroup
	// Home is where a message page sends someone who has nowhere else to go —
	// the first screen that works, or empty when none does. It is deliberately
	// not hard-wired to the keys screen: a page that failed sending an operator
	// to another page that cannot answer is two refusals and one click.
	Home      string
	HomeLabel string
	// Provenance is this screen's claim about where its figures come from, and
	// it is per screen because it is a CLAIM.
	//
	// It used to be one sentence in the shared layout — "every figure comes
	// from the same engine the API and the CLI use, so there is one answer
	// rather than three" — printed on every page including the ones with no
	// figures at all, and including the keys screen while its spend column
	// disagreed with /key/list about the same key from the same store. That is
	// worse than printing nothing: it is the strongest claim on the page, it
	// cites the section that makes it, and a reader who believes it does not
	// go and cross-check. A screen now says where its numbers come from, or
	// says nothing.
	Provenance string
	// Session is true when a cookie session is in play, which is the only case
	// where a sign-out control means anything.
	Session bool
	// CanMutate reports that this viewer may act, not merely look. It is true
	// exactly when there is a cookie session, because a mutation is proved by
	// that session's token and a header-authenticated viewer has none.
	//
	// It gates the CONTROLS. It is not the enforcement — [uiServer.authorize]
	// is, on every unsafe request — and the distinction is the same one the nav
	// draws: a control that is not offered is a click an operator is spared,
	// and a control that is offered is still checked.
	CanMutate bool
	// CSRF is the session's token, rendered into every form. It is present only
	// when CanMutate is, so a template cannot accidentally emit an empty one
	// and produce a form that always refuses.
	CSRF string
	// ActionPath is the UI's one mutating URL, relative to Base. Templates take
	// it from here rather than spelling it out, so the dispatcher's constant and
	// every form's action attribute are the same string.
	ActionPath string
	// Actor names the credential this view is authorized by.
	Actor     string
	Notice    string
	Problem   string
	Generated time.Time
}

type keyRow struct {
	*Key
	Expired bool
	// Pended is §11.6's reversible refusal. It is a separate state from
	// blocked and from expired because it is a separate fact: the token guard
	// pended the key, an operator releases it in one action, and a screen that
	// renders a pended key as "active" describes a credential the gateway is
	// currently refusing as one it is serving.
	Pended bool
}

type keysPage struct {
	page
	Keys []keyRow
	// The controls this process can actually carry out, asked one step earlier
	// than the handler that would refuse them — the same rule [uiServer.nav]
	// applies to whole screens. Rotation and pend are optional halves of the
	// key store (§11.2c, §11.6) and issuance needs a pepper (§2.4); a row that
	// offered all four everywhere would answer a named 501 on click in the
	// deployments that have none of them, which teaches an operator that the
	// product is broken rather than that a dependency is optional.
	CanCreate bool
	CanRotate bool
	CanPend   bool
}

// keyActions reports which parts of the lifecycle this process can perform.
func (s *uiServer) keyActions() (create, rotate, pend bool) {
	ks := s.api.cfg.Keys
	if ks == nil {
		return false, false, false
	}
	_, rotate = ks.(RotatingKeyStore)
	_, pend = ks.(PendableKeyStore)
	return s.api.cfg.Hasher != nil, rotate, pend
}

type modelsPage struct {
	page
	Deployments []*Deployment
	Aliases     []Alias
	// Compiled reports that the rows came from the routing table this process
	// compiled rather than from an editable registry. The distinction is on the
	// page because it changes what an operator does next: one is edited in
	// `models:` and reloaded with SIGHUP, the other over `/model/*`.
	Compiled bool

	HealthAvailable bool
	Credentials     []CredentialStatus

	CapacityAvailable bool
	Capacity          CapacityOccupancy

	// CanToggle reports that this process can take a deployment in or out of
	// routing from the UI: a config writer is wired and the viewer may mutate.
	CanToggle bool
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
	screens := s.nav(screen, v)
	pg := page{
		Title:      title,
		Screen:     screen,
		Base:       s.base(),
		APIBase:    "",
		Screens:    screens,
		NavGroups:  navGroups(screens),
		Session:    v.signedIn && !s.api.cfg.DisableUISessions,
		CanMutate:  v.canMutate(),
		CSRF:       v.csrf,
		ActionPath: uiActionPath,
		Actor:      v.actor,
		Notice:     s.takeFlash(v.session),
		Generated:  s.api.now(),
	}
	if !pg.CanMutate {
		pg.CSRF = ""
	}
	for _, n := range pg.Screens {
		if !n.Current {
			pg.Home, pg.HomeLabel = n.Href, n.Label
			break
		}
	}
	return pg
}

// nav lists the screens this process can serve this viewer.
//
// A screen is offered when its dependency is present AND the viewer's scope
// admits it. Both halves are the same rule as the screen's own guard, asked one
// step earlier: the guards stay — a link is not an authorization decision — but
// an operator is no longer invited to click something that can only refuse
// them. A team-scoped administrator therefore sees `keys`, because that screen
// answers for their team, and does not see the two that are deployment-wide.
func (s *uiServer) nav(current string, v viewer) []navScreen {
	cat, _ := s.api.modelCatalog()
	all := []struct {
		screen, label string
		available     bool
	}{
		{"keys", "keys", s.api.cfg.Keys != nil},
		{"users", "users & teams", s.api.cfg.Directory != nil && v.scope.Global},
		{"models", "models & deployments", cat != nil && v.scope.Global},
		{"usage", "usage & cost", s.api.cfg.Ledger != nil && v.scope.Global},
		{"monitoring", "monitoring", (s.api.cfg.Ledger != nil || s.api.cfg.Credentials != nil || s.api.cfg.Capacity != nil || s.api.cfg.Surface != nil) && v.scope.Global},
	}
	out := make([]navScreen, 0, len(all))
	for _, n := range all {
		if !n.available {
			continue
		}
		out = append(out, navScreen{
			Screen:  n.screen,
			Href:    s.base() + "/" + n.screen,
			Label:   n.label,
			Current: n.screen == current,
		})
	}
	return out
}

// navGroups buckets the flat nav into the sections of [navSections], in that
// order, dropping any section that ends up empty. A screen no section names
// falls into a trailing "more" group rather than vanishing, so adding a screen
// and forgetting to place it degrades to "ungrouped", not "unreachable".
func navGroups(flat []navScreen) []navGroup {
	byScreen := make(map[string]navScreen, len(flat))
	for _, n := range flat {
		byScreen[n.Screen] = n
	}
	placed := make(map[string]bool, len(flat))
	out := make([]navGroup, 0, len(navSections)+1)
	for _, sec := range navSections {
		g := navGroup{Title: sec.title}
		for _, screen := range sec.screens {
			if n, ok := byScreen[screen]; ok {
				g.Screens = append(g.Screens, n)
				placed[screen] = true
			}
		}
		if len(g.Screens) > 0 {
			out = append(out, g)
		}
	}
	var rest navGroup
	for _, n := range flat {
		if !placed[n.Screen] {
			rest.Screens = append(rest.Screens, n)
		}
	}
	if len(rest.Screens) > 0 {
		rest.Title = "more"
		out = append(out, rest)
	}
	return out
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

	// principal is who a mutation acts AS.
	//
	// It travels on the viewer for the same reason the scope does: the action
	// dispatcher hands it straight to an administration handler, and a
	// dispatcher that had to look one up would be a dispatcher that could look
	// up the wrong one. For a cookie session it is rebuilt per request from the
	// session's own facts, and only after [uiServer.sessionAuthorized] has
	// re-read the key's row.
	principal Principal
	// session is the cookie session's id, or empty for a header-authenticated
	// viewer. It addresses the two one-shot slots — the reveal and the flash —
	// and nothing else.
	session string
	// csrf is the session's token. Empty means this viewer cannot mutate, which
	// is the same statement as "this viewer has no session".
	csrf string
}

// canMutate reports whether this viewer may act rather than only look.
//
// One expression, one caller per question: the page uses it to decide whether
// to render controls and [uiServer.authorize] uses it to decide whether to hand
// out a viewer for an unsafe request at all. A viewer with no token cannot have
// proved one, so the two answers cannot drift apart.
func (v viewer) canMutate() bool { return v.session != "" && v.csrf != "" && v.principal != nil }

// sessionPrincipal is the administrator a cookie session acts as.
//
// It is rebuilt from the session's stored facts on every request rather than
// captured as an interface value at sign-in, so that it cannot outlive the
// checks around it: it is produced only on the path that has just re-read the
// key's row (see [uiServer.sessionAuthorized]). IsAdmin is true by construction
// because a session is created only from a principal that already was one —
// with the residual that sessionAuthorized names: an administrative role
// revoked on the owning USER is not on the key, so it is observed on the
// session TTL rather than on the next click.
type sessionPrincipal struct {
	kind  string
	id    string
	scope Scope
}

func (p sessionPrincipal) ActorKind() string { return p.kind }
func (p sessionPrincipal) ActorID() string   { return p.id }
func (p sessionPrincipal) IsAdmin() bool     { return true }
func (p sessionPrincipal) AdminScope() Scope { return p.scope }

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

// safeMethod reports a method with no side effects, in HTTP's sense. It is the
// hinge of the whole forgery defence: everything that is not safe needs the
// session's token, and there is one expression that says which is which.
func safeMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead }

// serve dispatches a UI request. rest is the path with the mount prefix
// removed.
//
// The order below is the security model's order and not an accident of
// readability. An unsafe method is bounded, origin-checked and — inside
// [uiServer.authorize] — token-checked before any handler sees it; the safe
// screens and the one unsafe route are then dispatched from the same authorized
// viewer.
func (s *uiServer) serve(w http.ResponseWriter, r *http.Request, rest string) {
	s.securityHeaders(w)

	switch rest {
	case "", "/":
		http.Redirect(w, r, s.base()+"/keys", http.StatusSeeOther)
		return
	case "/assets/style.css", "/assets/app.js":
		s.serveAsset(w, r, strings.TrimPrefix(rest, "/assets/"))
		return
	}

	safe := safeMethod(r.Method)
	if !safe {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, HEAD, POST")
			http.Error(w, "the operator UI answers GET, HEAD and POST", http.StatusMethodNotAllowed)
			return
		}
		// Bound the form before anything can parse it. net/http's own default
		// for a urlencoded body is 10 MB, which is 10 MB of an operator's
		// gateway spent on a request that will be refused for want of a token.
		r.Body = http.MaxBytesReader(w, r.Body, maxUIForm)
		// A layer over the token, not the mechanism: a request whose origin
		// POSITIVELY disagrees with this host is refused here, and one that
		// declares no origin at all falls through to the token, which is the
		// check that cannot be satisfied from another site.
		if !sameOrigin(r) {
			s.refuseCrossSite(w, r, "the request declares an origin that is not this gateway")
			return
		}
	}

	switch rest {
	case "/login":
		// Sign-in is the one unsafe route with no session to bind a token to,
		// because it is what creates one. The origin check above is what stands
		// in for the token here, and a forged sign-in logs the victim into the
		// ATTACKER's session rather than the other way round — an annoyance
		// that mints nothing, not an escalation.
		s.serveLogin(w, r)
		return
	case "/logout":
		// Also before authorize, and deliberately: a session that has stopped
		// authorizing (a revoked key, an expired cookie) must still be
		// clearable, and dropping a session is the one state change that can
		// only ever reduce what the caller holds.
		s.serveLogout(w, r)
		return
	}

	v, ok := s.authorize(w, r)
	if !ok {
		return
	}

	if !safe {
		if rest != uiActionPath {
			// Not a route somebody forgot to mount. There is one mutating URL
			// in this UI and this is not it, which is worth saying rather than
			// answering a bare 405 on a page an operator thought was a form.
			w.Header().Set("Allow", "GET, HEAD")
			s.renderMessage(w, r, http.StatusMethodNotAllowed, v, "Not a form",
				"Only "+s.base()+uiActionPath+" accepts a mutation, and every action it "+
					"performs is one row of the table in internal/admin/uikeys.go.",
				CodeMethodNotAllowed)
			return
		}
		s.act(w, r, v)
		return
	}

	switch rest {
	case "/keys":
		s.screenKeys(w, r, v)
	case "/keys/new":
		s.screenNewKey(w, r, v)
	case "/keys/edit":
		s.screenEditKey(w, r, v)
	case "/keys/confirm":
		s.screenConfirm(w, r, v)
	case "/keys/secret":
		s.screenSecret(w, r, v)
	case uiActionPath:
		// The mutating URL, reached with a safe method. It is a form target and
		// not a screen, so it says so rather than answering "no such screen" —
		// which is what a bookmarked action would otherwise get.
		w.Header().Set("Allow", "POST")
		s.renderMessage(w, r, http.StatusMethodNotAllowed, v, "Not a screen",
			"That URL performs an action and is reached by submitting a form on the keys screen.",
			CodeMethodNotAllowed)
	case "/users":
		s.screenUsers(w, r, v)
	case "/users/edit_user":
		s.screenEditUser(w, r, v)
	case "/users/edit_team":
		s.screenEditTeam(w, r, v)
	case "/models":
		s.screenModels(w, r, v)
	case "/usage":
		s.screenUsage(w, r, v)
	case "/monitoring":
		s.screenMonitoring(w, r, v)
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

// authorize resolves the viewer, and is the only function that produces one.
//
// That is the load-bearing sentence of this file. A screen needs a viewer for
// its scope, an action needs one for its principal and its token, and neither
// can be reached with anything else — so the cross-site check below is not a
// guard a future handler has to remember, it is the toll on the only road in.
//
// For an unsafe method the session's token must be present in the FORM BODY and
// equal to the one minted with the session. Body and not query string: a token
// in a URL travels into the browser's history, into the `Referer` of anything
// the page links to, and into every access log between here and the operator.
//
// It also reports whether a cookie session was used, so the layout can offer a
// sign-out control only when there is something to sign out of.
func (s *uiServer) authorize(w http.ResponseWriter, r *http.Request) (viewer, bool) {
	safe := safeMethod(r.Method)
	if !s.api.cfg.DisableUISessions {
		if c, err := r.Cookie(s.api.cfg.SessionCookie); err == nil {
			if sess, live := s.lookupSession(c.Value); live {
				// The session carries the scope that was resolved when it was
				// created, alongside the authorization decision it already
				// held. Re-deriving it per request would need the credential,
				// which the cookie deliberately is not — but the credential's
				// id is here, and whether it still serves is re-asked now.
				if s.sessionAuthorized(r.Context(), sess) {
					if !safe && !sess.proves(r) {
						s.refuseCrossSite(w, r, "the form did not carry this session's token")
						return viewer{}, false
					}
					return viewer{
						actor:    sess.actor,
						signedIn: true,
						scope:    sess.scope,
						principal: sessionPrincipal{
							kind: sess.actorKind, id: sess.actorID, scope: sess.scope,
						},
						session: c.Value,
						csrf:    sess.csrf,
					}, true
				}
				// The credential behind this session is gone. Drop the session
				// rather than leave it to expire, so that a second tab holding
				// the same cookie does not have to discover this again.
				s.dropSession(c.Value)
			}
		}
	}
	// Fall back to the API's own credential, so that a deployment fronted by an
	// authenticating proxy — or an operator with curl — reaches the same pages.
	//
	// It reaches the READING half and stops there. A bearer header is not
	// something a browser attaches on its own, so there is no browser client to
	// serve here; what there might be is a proxy that turns a cookie of its own
	// into this header, and a token dorang cannot bind to a session it cannot
	// see is not a defence. Such a deployment mutates over the API, which is
	// where it already was.
	if p, err := s.api.authenticate(r); err == nil {
		if !safe {
			s.refuseCrossSite(w, r,
				"a header-authenticated view of the UI is read-only; sign in for a session, "+
					"or use the administration API")
			return viewer{}, false
		}
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

// proves reports whether an unsafe request carries this session's own token.
//
// The comparison is constant time. It is not obviously necessary for a value
// compared once per click — but the alternative is an argument about how many
// requests an attacker can make, and this costs nothing.
//
// A session with no token cannot be proved by anything, which is the fail-closed
// direction: an entry built by some future path that forgets to mint one refuses
// every mutation rather than admitting every one.
func (sess uiSession) proves(r *http.Request) bool {
	if sess.csrf == "" {
		return false
	}
	// PostFormValue reads the BODY only. A cross-site form can post a body, but
	// it cannot know what to put in it; a cross-site navigation can put anything
	// in a query string, which is why the query string is not consulted.
	got := r.PostFormValue(csrfField)
	return subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) == 1
}

// sameOrigin reports whether an unsafe request plausibly came from this UI.
//
// It refuses only on a POSITIVE disagreement. `Sec-Fetch-Site` is trustworthy
// where it exists — a browser sets it and a page cannot — and `Origin` is sent
// on every cross-origin form post; a request that declares neither is answered
// by the token, which is the check that actually holds. Refusing the silent case
// would break `curl` against a test deployment and would move the defence onto a
// header, which is the thing this function is deliberately not.
func sameOrigin(r *http.Request) bool {
	switch strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")) {
	case "same-origin", "none":
		return true
	case "":
		// Not a browser that sets it, or an older one: fall through to Origin.
	default:
		// "cross-site" and "same-site" both mean another origin sent this.
		return false
	}
	o := strings.TrimSpace(r.Header.Get("Origin"))
	switch o {
	case "":
		return true
	case "null":
		// A sandboxed iframe or a `file://` document. Neither is this UI.
		return false
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// refuseCrossSite is the one refusal every forgery check returns.
//
// It renders a page rather than a bare 403 because whoever sees it is usually
// not an attacker: it is an operator whose session ended in another tab, or who
// left a form open past the hour. The page says which, and says that nothing
// changed, which is the sentence they actually need.
func (s *uiServer) refuseCrossSite(w http.ResponseWriter, r *http.Request, why string) {
	s.api.metrics.uiForgeries.Add(1)
	s.renderMessage(w, r, http.StatusForbidden, viewer{}, "Refused",
		"That request was not proved to have come from this UI: "+why+
			". If you were signed in, your session may have ended — sign in again and repeat "+
			"the action. Nothing was changed.",
		CodeCrossSiteRefused)
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// A session is a random opaque id held in a cookie, resolved against a table in
// this process. Three consequences, all stated rather than discovered:
//
//   - The cookie is not the credential. Signing out revokes the session, and a
//     stolen cookie expires on its own.
//   - Revocation of the underlying key IS observed mid-session, on the next
//     request. See [uiServer.sessionAuthorized]; the TTL is a ceiling on an
//     idle tab, not the revocation lag.
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

// sessionAuthorized re-asks whether the credential this session was created
// from still authorizes it.
//
// # Why it exists
//
// The session held an authorization decision and nothing re-examined it, so
// blocking, pending, expiring or deleting the key an operator had signed in
// with left that operator's open tab administering the deployment for up to the
// session TTL — an hour, against the 270 ms bound §11.2c publishes for the same
// revocation everywhere else. An administrative session that outlives the
// credential that created it is the same gap the invalidation work closed on
// the request path, and it is worse here: this is the surface the credential
// was revoked FROM.
//
// # What is checked, and what a failure means
//
// The key's own row, by the id the session carries: gone, blocked, pended or
// expired all end the session. That is exactly what [Authenticator] would find,
// minus the owning user's role — which is not on the key (§9.2) and is
// therefore not this package's join to make. A role revoked on the owning user
// is still observed on the TTL, and that is stated rather than implied.
//
// This is what makes a MUTATING session defensible, and it is why that residual
// is worth naming twice. A revoked administrator cannot mint a key on the next
// click, because the click re-reads the row the revocation wrote. An
// administrator whose USER lost its role can, for up to the session TTL, and
// closing that would mean either duplicating internal/app's role vocabulary in
// this package — the "two lists that must agree" defect this codebase keeps
// finding — or a second dependency that re-derives a principal from a key id.
// Neither is built. OPERATIONS §3.2 says so in the operator's words: revoke the
// KEY, not only the role, and the session ends on the next click.
//
// The scope is checked too, against the same fact it was derived from: a
// session whose stored scope no longer covers its key's team is ended rather
// than left holding a scope the key no longer has.
//
// A store that cannot answer ends the session. That is the fail-closed
// direction and it costs nothing an operator can feel: signing in again goes
// through the same store, so a store that cannot answer this cannot
// authenticate anyone either.
//
// The master credential is exempt because it has no row to check (§2.4). It is
// out-of-band, revoked by changing DORANG_MASTER_KEY and restarting, which
// takes the session table with it.
func (s *uiServer) sessionAuthorized(ctx context.Context, sess uiSession) bool {
	if sess.actorKind != "key" || sess.actorID == "" {
		return true
	}
	ks := s.api.cfg.Keys
	if ks == nil {
		// No key store in this process: there is no revocation this package can
		// observe, so there is none to enforce. The screens that need one say so
		// on their own.
		return true
	}
	k, err := ks.GetKey(ctx, sess.actorID)
	if err != nil || k == nil {
		return false
	}
	if k.Blocked || !k.PendedAt.IsZero() || k.Expired(s.api.now()) {
		return false
	}
	return sess.scope.AllowsTeam(k.TeamID)
}

func (s *uiServer) newSession(p Principal) string {
	id, csrf := randomToken(), randomToken()
	now := s.api.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	// Sweep on write. Sessions are few and short-lived, so a sweep here is
	// cheaper and simpler than a goroutine that must then be stopped. It is also
	// where an unclaimed secret goes: a session that expires takes its reveal
	// with it, so the only way a plaintext credential outlives its navigation is
	// if nobody ever signs in again, and even then it is bounded by the TTL.
	for k, v := range s.sessions {
		if !now.Before(v.expires) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = uiSession{
		actor:     actorLabel(p),
		actorKind: p.ActorKind(),
		actorID:   p.ActorID(),
		scope:     p.AdminScope(),
		csrf:      csrf,
		expires:   now.Add(s.api.cfg.SessionTTL),
	}
	return id
}

// randomToken mints 32 bytes of cryptographic randomness, base64url. It is the
// session id and the session's token, which are two different values for two
// different jobs: one is sent by the browser automatically and the other is not.
func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("admin: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func (s *uiServer) dropSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Deleting the entry takes any unread reveal with it. Signing out is
	// therefore also the answer to "I opened the secret page on the wrong
	// screen": there is nothing left to come back to.
	delete(s.sessions, id)
}

// stashReveal parks a freshly minted secret for exactly one navigation.
//
// It REPLACES anything already there. Two creates in a row must not leave the
// first secret in memory waiting for a page nobody is going to open.
func (s *uiServer) stashReveal(id string, rv *revealed) {
	if id == "" || rv == nil {
		return
	}
	rv.deadline = s.api.now().Add(revealTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return
	}
	sess.reveal = rv
	s.sessions[id] = sess
}

// takeReveal removes the pending secret and returns it, or nil.
//
// Take, not read: the clear and the fetch happen under one lock, so two tabs
// racing for the same secret produce one page that shows it and one that says it
// is gone. A reveal past its deadline is cleared and reported as absent, which
// is the same answer a reload gets and is the only answer this page has.
func (s *uiServer) takeReveal(id string) *revealed {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok || sess.reveal == nil {
		return nil
	}
	rv := sess.reveal
	sess.reveal = nil
	s.sessions[id] = sess
	if !s.api.now().Before(rv.deadline) {
		return nil
	}
	return rv
}

// setFlash records the one-line outcome of a mutation for the page that follows
// the redirect.
func (s *uiServer) setFlash(id, msg string) {
	if id == "" || msg == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return
	}
	sess.flash = msg
	s.sessions[id] = sess
}

// takeFlash reads and clears the pending notice. A message that survived a
// refresh would be an operator reading a stale "deleted" beside a key that is
// still there.
func (s *uiServer) takeFlash(id string) string {
	if id == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok || sess.flash == "" {
		return ""
	}
	msg := sess.flash
	sess.flash = ""
	s.sessions[id] = sess
	return msg
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
		Secure:   overTLS(r),
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
		Secure:   overTLS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, s.base()+"/login", http.StatusSeeOther)
}

// overTLS reports whether the BROWSER's hop was encrypted, which is what the
// Secure attribute is about.
//
// `r.TLS != nil` answers a different question — whether TLS terminated at this
// process — and the two differ in the deployment OPERATIONS recommends: behind
// a reverse proxy holding the certificate, dorang sees plaintext and the
// session cookie shipped without Secure, so a later plaintext request to the
// same host would carry an administrative session in the clear.
//
// The forwarded headers are read for this and for nothing else, and the
// direction they can be abused in is the harmless one: a caller who spoofs
// `X-Forwarded-Proto: https` gets a cookie marked Secure, which their own
// plaintext browser will then decline to send back. Marking too much is a
// self-inflicted sign-out; marking too little is a credential on the wire. No
// authorization decision is taken from these headers.
func overTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(firstField(r.Header.Get("X-Forwarded-Proto"))), "https") {
		return true
	}
	// RFC 7239, which some proxies send instead: `Forwarded: for=…;proto=https`.
	for _, part := range strings.Split(firstField(r.Header.Get("Forwarded")), ";") {
		k, v, ok := strings.Cut(part, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "proto") &&
			strings.EqualFold(strings.Trim(strings.TrimSpace(v), `"`), "https") {
			return true
		}
	}
	return false
}

// firstField takes the first comma-separated element of a forwarded header.
// A chain of proxies appends, so the first element is the hop closest to the
// browser — which is the one whose scheme the cookie's Secure attribute is
// about.
func firstField(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		return v[:i]
	}
	return v
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
		rows = append(rows, keyRow{Key: k, Expired: k.Expired(now), Pended: !k.PendedAt.IsZero()})
	}
	// The same enrichment /key/info and /key/list get, from the same function,
	// after the scope filter so that the batch is the page and not the store.
	//
	// Reading the same STORE as the API is not the same as showing the same
	// NUMBER, and this column is where the difference showed: `spend` was
	// rendered straight off [Key.SpendNano], which is `api_keys.spend_nano` —
	// a column no request-path writer touches — so the screen read 0 for every
	// key while /key/list, the ledger and /global/spend/report all agreed on
	// another figure. See [API.hydrateSpend]; it is called here for exactly the
	// keys this page renders.
	kk := make([]*Key, 0, len(rows))
	for _, row := range rows {
		kk = append(kk, row.Key)
	}
	s.api.hydrateSpend(r.Context(), kk...)
	pg := s.newPage("keys", "Keys", v)
	// The claim, on the screen that earns it. `spend` is the same figure
	// /key/list reports, from the same reporter, for the same key — which is
	// what a test asserts by reading both, because until it does the sentence
	// is a hope.
	pg.Provenance = "spend is the §9.4 rollup /key/list reports, not " +
		"api_keys.spend_nano; the ceilings are the stored ones the gate enforces."
	create, rotate, pend := s.keyActions()
	s.render(w, r, http.StatusOK, "keys", keysPage{
		page: pg, Keys: rows,
		CanCreate: pg.CanMutate && create,
		CanRotate: pg.CanMutate && rotate,
		CanPend:   pg.CanMutate && pend,
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
	// The screen is a VIEW, and it is served from whatever this process can
	// answer the view's question with. `/model/*` — the write surface — still
	// answers 501 wherever no editable registry exists, because a deployment
	// row nothing routes on is a write with no effect. Reading is not the same
	// act: the gateway knows its model set, serves it on /v1/models, and a nav
	// item that 501s on every click in every deployment tells an operator less
	// than the table it was hiding.
	reg, compiled := s.api.modelCatalog()
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
		Compiled:    compiled,
	}
	pg.CanToggle = pg.CanMutate && s.api.cfg.ConfigWriter != nil
	if compiled {
		pg.Provenance = "The deployments and aliases are the routing table this process " +
			"compiled, and the ids are the ones the ledger records."
	} else {
		pg.Provenance = "The deployments and aliases are the registry rows /model/info returns."
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
	pg.Provenance = "Every figure is read from the same §9.4 rollups " +
		"/global/spend/report aggregates, over the window shown."
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
