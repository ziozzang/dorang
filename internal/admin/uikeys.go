package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The operator UI's mutating surface: the credential lifecycle of DESIGN
// §11.2c and §11.6, from a browser.
//
// Three properties hold this file together, and each one is a shape rather than
// a rule somebody has to keep:
//
//  1. ONE URL. [uiActionPath] is the only path in the UI that accepts an unsafe
//     method, and [uiServer.serve] refuses every other one before dispatching.
//     A forgery test that covers this path covers the whole mutating surface,
//     now and after the next action is added.
//  2. ONE TABLE. Every action is a row of [uiActions], naming the
//     administration path it posts to. The dispatcher reads the table; so does
//     the test that forges a cross-site POST at each row, and so does the test
//     that asserts every named path is one this build actually serves.
//  3. THE SAME HANDLER. An action does not reach into the key store. It calls
//     the administration handler registered at that path, in this process, with
//     the signed-in operator as the principal — so the scope check, the audit
//     row and the §11.2c invalidation are not reimplemented here and cannot
//     drift from what a script gets. See [uiServer.invoke].

// uiActionPath is the UI's only mutating URL, relative to the mount prefix.
const uiActionPath = "/keys/action"

// csrfField names the hidden input every form carries. It is read from the body
// and never from the query string; see [uiSession.proves].
const csrfField = "csrf"

// maxUIForm bounds a submitted form. An operator's key specification is a few
// hundred bytes; 64 KiB is generous and is two orders of magnitude below
// net/http's own 10 MB default for a urlencoded body, which is 10 MB of a
// gateway spent on a request that has not yet proved it may exist.
const maxUIForm = 64 << 10

// ---------------------------------------------------------------------------
// The action table
// ---------------------------------------------------------------------------

// uiField is one extra input a confirmation page offers, beyond the key it is
// about. There is at most one per action deliberately: a confirmation page that
// grows a form is a confirmation page nobody reads.
type uiField struct {
	Name        string
	Label       string
	Placeholder string
	Help        string
}

// uiAction is one row of the credential lifecycle as the UI performs it.
type uiAction struct {
	// api is the administration path this action posts to. It is the same path
	// a script calls and the same handler answers it.
	api string
	// verb and past are the action in the operator's words: "block", "blocked".
	// They appear in the confirmation heading, in the failure page and in the
	// notice afterwards, so the three cannot describe different operations.
	verb string
	past string
	// confirm sends the operator through a page that names the key before
	// anything happens. It is set for everything that stops a credential
	// working or cannot be undone, and unset for the two that restore service:
	// §11.6 requires a pended key to be released in ONE action, and an
	// interstitial is not one.
	confirm bool
	// danger styles the control and the confirmation as irreversible.
	danger bool
	// reveals says the response carries a one-time secret, which is handled by
	// [uiServer.act] and by no other code path.
	reveals bool
	// consequence is what the confirmation page tells the operator this will do
	// — in terms of what stops working, not in terms of what is written.
	consequence string
	// field is the one extra input the confirmation page offers, or nil.
	field *uiField
	// needs reports whether this process can perform the action at all, so a
	// control for an optional half of the key store is not offered where there
	// is nothing behind it.
	needs func(s *uiServer) bool
	// body builds the JSON request from the submitted form.
	body func(f url.Values) (any, error)
	// notice renders the flash shown on the page after the redirect. Nil takes
	// the default, which is the verb and the key id.
	notice func(f url.Values, res map[string]any) string
}

// uiActions is the whole mutating surface of the operator UI.
//
// Eight rows, and the eight administration paths they name are the credential
// lifecycle: mint, retire, rotate, cut the grace short, and the two reversible
// pairs. `/key/update` is deliberately absent and `/key/regenerate` too; the
// reasons are with the table rather than in a commit message, because the next
// person to want them will read this.
//
//   - `/key/update` is a form over twenty fields with an ordering constraint
//     between two of them and a `clear` list whose semantics are "absent is not
//     null". A browser form cannot express "leave this alone" and "set this to
//     nothing" as different things without inventing a third state per field,
//     and a form that silently sent the empty string for every untouched input
//     would clear a budget an operator never looked at. It is the shape the
//     API is good at and a form is bad at, so it stays on the API.
//   - `/key/regenerate` is `/key/rotate` with a zero grace, which the rotate
//     confirmation offers as a field. Two controls for one operation is two
//     things an operator has to know the difference between at the moment they
//     are least able to look it up.
var uiActions = map[string]uiAction{
	"create": {
		api: "/key/generate", verb: "create a key", past: "created",
		reveals: true,
		needs:   func(s *uiServer) bool { return s.api.cfg.Hasher != nil },
		body:    newKeyBody,
	},
	"edit": {
		api: "/key/update", verb: "save changes to", past: "updated",
		body: editKeyBody,
		notice: func(f url.Values, _ map[string]any) string {
			return "Key " + f.Get("key_id") + " updated. The fields you left empty are now unset."
		},
	},
	"rotate": {
		api: "/key/rotate", verb: "rotate", past: "rotated",
		confirm: true, reveals: true,
		consequence: "A new secret is minted and returned once. The key id, its tier, budget, " +
			"spend, allow-list, rate limits, team and ledger history are unchanged. The OLD " +
			"secret keeps authenticating for the grace period below, so a client that has not " +
			"rolled yet keeps working until it ends.",
		field: &uiField{
			Name: "grace", Label: "grace",
			Placeholder: "24h",
			Help: "How long the old secret keeps working: \"24h\", \"30d\", \"90m\". " +
				"Leave empty for this deployment's auth.rotation.grace. " +
				"\"0\" cuts the old secret immediately, which is what a suspected " +
				"compromise wants and is not the default.",
		},
		needs: func(s *uiServer) bool { _, ok := s.api.cfg.Keys.(RotatingKeyStore); return ok },
		body: func(f url.Values) (any, error) {
			b := map[string]any{"key_id": f.Get("key_id")}
			if g := strings.TrimSpace(f.Get("grace")); g != "" {
				b["grace"] = g
			}
			return b, nil
		},
	},
	"cut": {
		api: "/key/rotate/cut", verb: "cut the grace period", past: "cut short",
		confirm: true, danger: true,
		consequence: "Every superseded secret of this key stops authenticating now, on every " +
			"node, rather than when its grace period runs out. The CURRENT secret is not " +
			"touched, so a client that has already rolled is unaffected — and one that has " +
			"not will start failing immediately.",
		needs: func(s *uiServer) bool { _, ok := s.api.cfg.Keys.(RotatingKeyStore); return ok },
		body:  keyRefBody,
		notice: func(f url.Values, res map[string]any) string {
			n, _ := res["cut"].(float64)
			return fmt.Sprintf("Key %s: %d superseded secret(s) cut. The current secret is unchanged.",
				f.Get("key_id"), int(n))
		},
	},
	"block": {
		api: "/key/block", verb: "block", past: "blocked",
		confirm: true, danger: true,
		consequence: "The key stops serving on every node within the bound OPERATIONS §3.1 " +
			"publishes, and every client holding it starts failing. It is reversible — " +
			"unblock is one click — but the outage between the two is real.",
		body: keyRefBody,
	},
	"unblock": {
		api: "/key/unblock", verb: "unblock", past: "unblocked",
		body: keyRefBody,
	},
	"pend": {
		api: "/key/pend", verb: "pend", past: "pended",
		confirm: true,
		consequence: "The key is refused with a distinct, documented error until it is " +
			"released, which is one click. Nothing is revoked and no credential has to be " +
			"reissued — but the caller is refused from now until then.",
		field: &uiField{
			Name: "reason", Label: "reason",
			Placeholder: "why this key is being held",
			Help: "Recorded on the key and in the audit row. An operator reading this in a " +
				"week is the person it is for.",
		},
		needs: func(s *uiServer) bool { _, ok := s.api.cfg.Keys.(PendableKeyStore); return ok },
		body: func(f url.Values) (any, error) {
			b := map[string]any{"key_id": f.Get("key_id")}
			if v := strings.TrimSpace(f.Get("reason")); v != "" {
				b["reason"] = v
			}
			return b, nil
		},
	},
	"release": {
		api: "/key/release", verb: "release", past: "released",
		needs: func(s *uiServer) bool { _, ok := s.api.cfg.Keys.(PendableKeyStore); return ok },
		body:  keyRefBody,
	},
	"delete": {
		api: "/key/delete", verb: "delete", past: "deleted",
		confirm: true, danger: true,
		consequence: "The row is removed and cannot be brought back. The ledger keeps its " +
			"rows, so this key's history survives as an id nothing resolves — which is why " +
			"OPERATIONS §3 says to BLOCK a leaked key rather than delete it: a blocked key " +
			"stays present and is refused as such, and a deleted one is refused as unknown.",
		body: func(f url.Values) (any, error) {
			id := strings.TrimSpace(f.Get("key_id"))
			if id == "" {
				return nil, errors.New("no key was named")
			}
			// One id, always. /key/delete takes a list because a script may
			// have one; a browser row has exactly one key in it, and a UI that
			// could post a list is a UI where a bug posts the wrong list.
			return map[string]any{"keys": []string{id}}, nil
		},
		notice: func(f url.Values, res map[string]any) string {
			n, _ := res["deleted"].(float64)
			if int(n) == 0 {
				return "Key " + f.Get("key_id") + " was already gone; nothing was deleted."
			}
			return "Key " + f.Get("key_id") + " is deleted. Its ledger rows remain and now " +
				"name an id nothing resolves."
		},
	},
}

// available reports whether this process can perform the action.
func (a uiAction) available(s *uiServer) bool {
	if s.api.cfg.Keys == nil {
		return false
	}
	return a.needs == nil || a.needs(s)
}

// keyRefBody is the body of every action that takes nothing but an id.
func keyRefBody(f url.Values) (any, error) {
	id := strings.TrimSpace(f.Get("key_id"))
	if id == "" {
		return nil, errors.New("no key was named")
	}
	return map[string]any{"key_id": id}, nil
}

// newKeyBody turns the create form into a [keySpec].
//
// Only fields the operator actually filled in are sent. An empty input means
// "not set", never "set to empty": the two are different answers everywhere
// else on this surface (see [keySpec]'s `clear`), and a form that posted the
// empty string for every untouched box would be a form that clears a budget
// nobody looked at.
func newKeyBody(f url.Values) (any, error) {
	b := map[string]any{}
	for _, n := range []string{
		"key_alias", "user_id", "team_id", "budget_duration", "duration",
		"tier", "priority_class",
	} {
		if v := strings.TrimSpace(f.Get(n)); v != "" {
			b[n] = v
		}
	}
	for _, n := range []string{"models", "allowed_routes", "tags"} {
		if list := splitList(f.Get(n)); len(list) > 0 {
			b[n] = list
		}
	}
	// The amount travels as a decimal STRING. [Money] accepts one and parses it
	// with integer arithmetic; encoding it as a JSON number here would put an
	// operator's budget through float64, which §8.3 forbids for exactly the
	// reason it looks harmless.
	if v := strings.TrimSpace(f.Get("max_budget")); v != "" {
		if _, err := parseNano(v); err != nil {
			return nil, errors.New("budget must be an exact decimal amount, as in \"250\" or \"12.50\"")
		}
		b["max_budget"] = v
	}
	for _, n := range []string{"rpm_limit", "tpm_limit"} {
		v := strings.TrimSpace(f.Get(n))
		if v == "" {
			continue
		}
		i, err := strconv.ParseInt(v, 10, 64)
		if err != nil || i < 0 {
			return nil, errors.New(n + " must be a whole number of requests or tokens per minute")
		}
		b[n] = i
	}
	return b, nil
}

// editKeyBody maps the edit form to a /key/update body under end-state
// semantics: the form is prefilled with the key's current values, so a box left
// as it is re-sends that value, and a box the operator EMPTIES resets the field
// through the update route's `clear` list. That is the answer to the objection
// the keys screen used to carry — that a form cannot tell "leave this alone"
// from "set this to nothing" — because a prefilled form has no "leave alone"
// state to confuse: what you see is the key's whole desired shape.
//
// Fields the form does not manage (allowed_routes, priority_class) are simply
// not sent, so they are left unchanged rather than cleared.
func editKeyBody(f url.Values) (any, error) {
	id := strings.TrimSpace(f.Get("key_id"))
	if id == "" {
		return nil, errors.New("no key was named")
	}
	b := map[string]any{"key_id": id}
	var clear []string

	for _, n := range []string{"key_alias", "user_id", "team_id", "tier", "budget_duration"} {
		if v := strings.TrimSpace(f.Get(n)); v != "" {
			b[n] = v
		} else {
			clear = append(clear, n)
		}
	}
	for _, n := range []string{"models", "tags"} {
		if list := splitList(f.Get(n)); len(list) > 0 {
			b[n] = list
		} else {
			clear = append(clear, n)
		}
	}
	for _, n := range []string{"max_budget", "soft_budget"} {
		v := strings.TrimSpace(f.Get(n))
		if v == "" {
			clear = append(clear, n)
			continue
		}
		if _, err := parseNano(v); err != nil {
			return nil, errors.New(n + ` must be an exact decimal amount, as in "250" or "12.50"`)
		}
		b[n] = v
	}
	for _, n := range []string{"rpm_limit", "tpm_limit", "max_parallel_requests"} {
		v := strings.TrimSpace(f.Get(n))
		if v == "" {
			clear = append(clear, n)
			continue
		}
		i, err := strconv.ParseInt(v, 10, 64)
		if err != nil || i < 0 {
			return nil, errors.New(n + " must be a whole number")
		}
		b[n] = i
	}
	// Expiry as a duration from now: "30d", "90d"; empty means no expiry.
	if v := strings.TrimSpace(f.Get("expires_in")); v != "" {
		b["duration"] = v
	} else {
		clear = append(clear, "expires")
	}
	if len(clear) > 0 {
		b["clear"] = clear
	}
	return b, nil
}

// nanoDollars renders a nullable nano amount as a decimal-dollar string for a
// form input: empty for "no ceiling", exact to the cent otherwise. It is the
// inverse of the decimal string [Money] parses, so a value round-trips through
// the edit form unchanged.
func nanoDollars(n *int64) string {
	if n == nil {
		return ""
	}
	v := *n
	neg := v < 0
	if neg {
		v = -v
	}
	cents := (v + 5_000_000) / 10_000_000 // nano to cents, rounded
	s := strconv.FormatInt(cents/100, 10) + "." + fmt.Sprintf("%02d", cents%100)
	if neg {
		s = "-" + s
	}
	return s
}

// intStr renders a nullable limit for a form input: empty when unset.
func intStr(n *int64) string {
	if n == nil {
		return ""
	}
	return strconv.FormatInt(*n, 10)
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Calling the administration surface from the UI
// ---------------------------------------------------------------------------

// invoke runs one administration handler in this process, as the signed-in
// operator, and returns its JSON response.
//
// # Why this and not a store call
//
// Every route in [uiActions] publishes a §11.2c invalidation, writes an audit
// row with the operator as the actor, and enforces [Scope] on the key it
// touches. A UI that reached past the handler into [KeyStore] would have to
// repeat all three, and the copy that drifts is the one nobody is looking at —
// which is how a screen came to render `api_keys.spend_nano` in a column headed
// "spend" while /key/list reported the ledger's figure for the same key.
//
// # What the synthetic request carries, and what it deliberately does not
//
// The method, the path, a JSON body, and the browser's own remote address and
// user agent so the audit row describes the operator rather than this process.
// It carries NO `Cookie` and NO `Authorization`: the handler does not
// authenticate — [API.ServeHTTP] does that, and this call has already been
// authorized by [uiServer.authorize] — so a credential on this request would be
// a credential with nothing to do and one more place for one to be.
//
// # The response
//
// Returned as bytes and decoded by the caller. For `create` and `rotate` those
// bytes contain the one-time secret, so nothing on this path logs a body, puts
// one in an error message or keeps one after the caller is done with it. A
// handler that refuses returns an error instead, and the bytes are empty.
func (s *uiServer) invoke(r *http.Request, v viewer, path string, body any) ([]byte, error) {
	if v.principal == nil {
		// Unreachable: authorize does not hand out a mutating viewer without
		// one. Refusing here rather than dereferencing nil is the difference
		// between a 500 with a sentence and a panic in an operator's face.
		return nil, newFault(http.StatusInternalServerError, CodeInternal, typeAPI,
			"the session resolved to no principal")
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, newFault(http.StatusInternalServerError, CodeInternal, typeAPI,
			"the request could not be encoded")
	}

	rt := s.api.routes[cleanPath(path)]
	if rt == nil {
		return nil, unimplemented(CodeNotImplemented,
			"%s is not an implemented administration route", path)
	}
	h := rt.methods[http.MethodPost]
	if h == nil {
		return nil, newFault(http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			typeInvalidRequest, "%s does not accept a mutation", path)
	}

	req := (&http.Request{
		Method:        http.MethodPost,
		URL:           &url.URL{Path: path},
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}, "User-Agent": {r.UserAgent()}},
		Body:          io.NopCloser(bytes.NewReader(buf)),
		ContentLength: int64(len(buf)),
		Host:          r.Host,
		RemoteAddr:    r.RemoteAddr,
	}).WithContext(r.Context())

	cw := &captureWriter{}
	if err := h(&call{w: cw, r: req, p: v.principal, a: s.api}); err != nil {
		return nil, err
	}
	if cw.status >= 400 {
		// A handler that WROTE a refusal rather than returning one. Only
		// writeJSON's own encoding failure takes that path today, and it must
		// not be read back as success: decoding a fault envelope as a mint
		// response would tell the operator "the secret was lost" about a call
		// that never made one.
		return nil, newFault(cw.status, CodeInternal, typeAPI,
			"the administration surface refused the request")
	}
	return cw.body.Bytes(), nil
}

// captureWriter collects a handler's JSON response instead of writing it to the
// browser. It is deliberately the smallest thing that satisfies
// [http.ResponseWriter]: nothing here flushes, hijacks or streams, because
// nothing on the administrative surface does.
type captureWriter struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header {
	if c.h == nil {
		c.h = http.Header{}
	}
	return c.h
}

func (c *captureWriter) Write(b []byte) (int, error) { return c.body.Write(b) }

func (c *captureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

// ---------------------------------------------------------------------------
// The dispatcher
// ---------------------------------------------------------------------------

// act performs one action. It is reached only from [uiServer.serve], only on
// [uiActionPath], and only with a viewer [uiServer.authorize] produced after
// comparing the session's token.
//
// Success REDIRECTS and failure RENDERS, which is not a stylistic choice. A
// mutation that answered 200 on the POST would be a mutation a reload
// re-submits; a redirect makes the reload idempotent. A failure has a status
// worth carrying — 403 out of scope, 404 gone, 501 dependency absent — and
// redirecting would flatten all of them to 303 and lose the reason with them.
func (s *uiServer) act(w http.ResponseWriter, r *http.Request, v viewer) {
	if err := r.ParseForm(); err != nil {
		s.renderMessage(w, r, http.StatusBadRequest, v, "Malformed form",
			"That form could not be read. It may have exceeded the size this UI accepts.",
			CodeInvalidRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("action"))
	a, ok := uiActions[name]
	if !ok {
		s.renderMessage(w, r, http.StatusBadRequest, v, "Unknown action",
			"There is no such action on this screen.", CodeInvalidRequest)
		return
	}
	if !a.available(s) {
		s.renderMessage(w, r, http.StatusNotImplemented, v, "Not available here",
			"This deployment's key store cannot "+a.verb+
				". The API answers the same route with the same reason and names the missing piece.",
			CodeDependencyOff)
		return
	}
	body, err := a.body(r.PostForm)
	if err != nil {
		s.renderMessage(w, r, http.StatusBadRequest, v, "Could not "+a.verb,
			err.Error(), CodeInvalidRequest)
		return
	}

	raw, err := s.invoke(r, v, a.api, body)
	if err != nil {
		s.renderActionError(w, r, v, a, err)
		return
	}
	s.api.metrics.uiMutations.Add(1)

	if a.reveals {
		rv, err := revealFrom(a.past, raw)
		if err != nil {
			// The key exists — the handler returned — and the secret could not
			// be got out of the response. Saying so is the only honest answer:
			// it is not recoverable, and the operator's next step is to rotate
			// or delete the key that was just made.
			s.renderMessage(w, r, http.StatusInternalServerError, v, "The secret was lost",
				"The key was "+a.past+" and its one-time secret could not be rendered. "+
					"dorang cannot show it again. Rotate the key to mint another, or delete it.",
				CodeInternal)
			return
		}
		s.stashReveal(v.session, rv)
		http.Redirect(w, r, s.base()+"/keys/secret", http.StatusSeeOther)
		return
	}

	var res map[string]any
	_ = json.Unmarshal(raw, &res)
	msg := ""
	if a.notice != nil {
		msg = a.notice(r.PostForm, res)
	}
	if msg == "" {
		msg = "Key " + r.PostFormValue("key_id") + " is " + a.past + "."
	}
	s.setFlash(v.session, msg)
	// The screen to return to. Key forms send none and default to /keys
	// (safeNext's own default); the users screen sends its own path. safeNext
	// admits only a path under this UI's mount, so a crafted return cannot
	// bounce the operator off-site.
	http.Redirect(w, r, safeNext(s.base(), r.PostFormValue("return")), http.StatusSeeOther)
}

// renderActionError shows a refusal from the administration surface.
//
// The rule is [uiServer.renderError]'s, with one exception that matters. A 500
// normally has its text replaced, because an error from a driver can carry a
// DSN and a DSN can carry a password. The two codes this package mints itself
// for a mutation that DID happen are the exception: their text is the operator's
// next step — reconcile rather than retry, or SIGHUP the fleet — and replacing
// it with "the data source refused the request" would describe the opposite of
// what occurred.
func (s *uiServer) renderActionError(w http.ResponseWriter, r *http.Request, v viewer,
	a uiAction, err error) {

	f := faultFor(err)
	body := f.Message
	if f.Status >= 500 && f.Code != CodeAuditWriteFailed && f.Code != CodeInvalidationFailed {
		body = "The administration surface refused the request."
	}
	s.renderMessage(w, r, f.Status, v, "Could not "+a.verb, body, f.Code)
}

// revealFrom pulls the one-time secret out of a mint response.
//
// It reads both shapes — [generatedKey] and [rotatedKey] — because the two
// differ only in the grace fields, and a second function would be a second place
// for a secret to be handled.
func revealFrom(action string, raw []byte) (*revealed, error) {
	var g struct {
		Key               string  `json:"key"`
		TokenID           string  `json:"token_id"`
		KeyName           string  `json:"key_name"`
		PreviousExpiresAt Stamp   `json:"previous_expires_at"`
		Grace             string  `json:"grace"`
		Info              keyView `json:"info"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, err
	}
	if g.Key == "" || g.TokenID == "" {
		return nil, errors.New("admin: the mint response carried no secret")
	}
	rv := &revealed{
		Secret: g.Key,
		Action: action,
		KeyID:  g.TokenID,
		Label:  g.KeyName,
		Alias:  g.Info.KeyAlias,
		Grace:  g.Grace,
	}
	if t := g.Info.Expires.Time(); !t.IsZero() {
		rv.Expires = t.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	if t := g.PreviousExpiresAt.Time(); !t.IsZero() {
		rv.PreviousExpires = t.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	return rv, nil
}

// ---------------------------------------------------------------------------
// The screens the actions need
// ---------------------------------------------------------------------------

// keyCard is the key a confirmation page is about, rendered.
//
// It holds strings rather than a [Key] because it exists to be READ before an
// irreversible click: the operator is checking that this is the row they meant,
// and "team t_7, spent 41.20 of 250, active" is what answers that question.
type keyCard struct {
	ID     string
	Label  string
	Alias  string
	User   string
	Team   string
	Spend  string
	Budget string
	State  string
	Expiry string
	Models []string
	Tags   []string
}

func cardFor(k keyView) keyCard {
	c := keyCard{
		ID: k.TokenID, Label: k.KeyName, Alias: k.KeyAlias,
		User: k.UserID, Team: k.TeamID,
		Spend: k.Spend.String(), Budget: "none",
		State: "active", Expiry: "never",
		Models: k.Models, Tags: k.Tags,
	}
	if k.MaxBudget != nil {
		c.Budget = k.MaxBudget.String()
	}
	switch {
	case k.Blocked:
		c.State = "blocked"
	case k.Pended:
		c.State = "pended"
	}
	if t := k.Expires.Time(); !t.IsZero() {
		c.Expiry = t.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	return c
}

type confirmPage struct {
	page
	Action      string
	Verb        string
	Danger      bool
	Consequence string
	Field       *uiField
	Key         keyCard
}

type newKeyPage struct {
	page
	// Team is prefilled for a team-scoped administrator, because that is the
	// only team they may mint on and typing it again is a chance to get it
	// wrong.
	Team string
	// TeamFixed says the field is not theirs to change: the API refuses a key
	// minted onto another team (see permitTeamParam), and a box that accepts a
	// value the server will reject is a box that lies.
	TeamFixed bool
}

type secretPage struct {
	page
	Secret *revealed
}

// screenNewKey renders the create form.
func (s *uiServer) screenNewKey(w http.ResponseWriter, r *http.Request, v viewer) {
	a := uiActions["create"]
	if !v.canMutate() {
		s.refuseReadOnly(w, r, v, "create a key")
		return
	}
	if !a.available(s) {
		s.renderMessage(w, r, http.StatusNotImplemented, v, "New key",
			"This process cannot issue credentials: the key pepper is not configured "+
				"(DESIGN §2.4 refuses a pepperless digest).", CodeDependencyOff)
		return
	}
	pg := newKeyPage{page: s.newPage("keys", "New key", v)}
	if !v.scope.Global && len(v.scope.Teams) == 1 {
		pg.Team, pg.TeamFixed = v.scope.Teams[0], true
	}
	s.render(w, r, http.StatusOK, "newkey", pg)
}

// editKeyPage prefills the edit form with a key's current values. Every field
// is a string ready to be an input's value, because the form's whole contract
// is that what it shows is the key's desired end-state — see [editKeyBody].
type editKeyPage struct {
	page
	ID, Label, Alias, UserID, TeamID string
	Models, Tags                     string
	MaxBudget, SoftBudget            string
	BudgetPeriod                     string
	RPM, TPM, MaxParallel            string
	Tier                             string
	ExpiresAt                        time.Time
	Blocked                          bool
}

// screenEditKey renders the prefilled edit form for one key.
//
// It loads the key through the same store the API reads and applies the same
// scope check the mutating routes do, so an operator cannot open an edit form
// for a key they could not edit. The save itself goes through the `edit`
// action and /key/update, so it writes the audit row and publishes the
// invalidation exactly as a scripted update would.
func (s *uiServer) screenEditKey(w http.ResponseWriter, r *http.Request, v viewer) {
	ks := s.api.cfg.Keys
	if ks == nil {
		s.renderMessage(w, r, http.StatusNotImplemented, v, "Edit key",
			"A key store is not configured in this process.", CodeDependencyOff)
		return
	}
	if !v.canMutate() {
		s.refuseReadOnly(w, r, v, "edit a key")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("key_id"))
	if id == "" {
		s.renderMessage(w, r, http.StatusBadRequest, v, "Edit key", "No key was named.", CodeInvalidRequest)
		return
	}
	k, err := ks.GetKey(r.Context(), id)
	if err != nil {
		s.renderError(w, r, v, "Edit key", err)
		return
	}
	if !v.scope.AllowsTeam(k.TeamID) {
		s.renderMessage(w, r, http.StatusForbidden, v, "Edit key",
			"This key belongs to a team this session does not administer.", CodeForbidden)
		return
	}
	pg := editKeyPage{
		page:         s.newPage("keys", "Edit key", v),
		ID:           k.ID,
		Label:        k.KeyLabel,
		Alias:        k.KeyAlias,
		UserID:       k.UserID,
		TeamID:       k.TeamID,
		Models:       strings.Join(k.Models, ", "),
		Tags:         strings.Join(k.Tags, ", "),
		MaxBudget:    nanoDollars(k.MaxBudgetNano),
		SoftBudget:   nanoDollars(k.SoftBudgetNano),
		BudgetPeriod: k.BudgetPeriod,
		RPM:          intStr(k.RPMLimit),
		TPM:          intStr(k.TPMLimit),
		MaxParallel:  intStr(k.MaxParallel),
		Tier:         k.Tier,
		ExpiresAt:    time.Time(k.ExpiresAt),
		Blocked:      k.Blocked,
	}
	s.render(w, r, http.StatusOK, "editkey", pg)
}

// screenConfirm renders the page between a destructive control and the mutation
// it performs.
//
// It is a GET, so a reload re-reads the key rather than re-running anything, and
// the only thing on it that can act is a form carrying the session's token. The
// key is fetched through `/key/info` — the same handler, the same scope check —
// so a confirmation page cannot show an operator a key they are not entitled to
// act on.
func (s *uiServer) screenConfirm(w http.ResponseWriter, r *http.Request, v viewer) {
	if !v.canMutate() {
		s.refuseReadOnly(w, r, v, "act on a key")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("action"))
	a, ok := uiActions[name]
	if !ok || !a.confirm {
		s.renderMessage(w, r, http.StatusBadRequest, v, "Unknown action",
			"There is no such action to confirm.", CodeInvalidRequest)
		return
	}
	if !a.available(s) {
		s.renderMessage(w, r, http.StatusNotImplemented, v, "Not available here",
			"This deployment's key store cannot "+a.verb+".", CodeDependencyOff)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("key_id"))
	if id == "" {
		s.renderMessage(w, r, http.StatusBadRequest, v, "No key named",
			"That link did not name a key.", CodeInvalidRequest)
		return
	}

	raw, err := s.invoke(r, v, "/key/info", map[string]any{"key_id": id})
	if err != nil {
		s.renderActionError(w, r, v, a, err)
		return
	}
	var got struct {
		Key keyView `json:"key"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		s.renderMessage(w, r, http.StatusInternalServerError, v, "Could not "+a.verb,
			"The key could not be read.", CodeInternal)
		return
	}

	pg := confirmPage{
		page:        s.newPage("keys", "Confirm: "+a.verb, v),
		Action:      name,
		Verb:        a.verb,
		Danger:      a.danger,
		Consequence: a.consequence,
		Field:       a.field,
		Key:         cardFor(got.Key),
	}
	s.render(w, r, http.StatusOK, "confirm", pg)
}

// screenSecret shows a one-time secret, once.
//
// The secret is taken out of the session BEFORE the page is rendered, and there
// is no path that puts it back. That is the whole design: a reload finds
// nothing and answers 410, a second tab racing the first finds nothing, and a
// template that failed halfway would lose the secret rather than leave it
// sitting in memory to be shown twice. Losing it costs a rotation; showing it
// twice costs the property the page is about.
//
// HEAD is refused rather than served. A HEAD writes no body, so serving one
// would consume a secret nobody could read — and a prefetcher or a link checker
// makes that happen without an operator ever clicking.
func (s *uiServer) screenSecret(w http.ResponseWriter, r *http.Request, v viewer) {
	if r.Method == http.MethodHead {
		w.Header().Set("Allow", "GET")
		http.Error(w, "the one-time secret is served to GET only", http.StatusMethodNotAllowed)
		return
	}
	if v.session == "" {
		s.renderMessage(w, r, http.StatusNotFound, v, "No secret",
			"A one-time secret belongs to a signed-in session, and this view is authorized "+
				"by a header.", CodeNotFound)
		return
	}
	rv := s.takeReveal(v.session)
	if rv == nil {
		// 410, not 404. The distinction is the point of the page: this URL DID
		// hold something and it is deliberately unrecoverable.
		s.renderMessage(w, r, http.StatusGone, v, "Already shown",
			"That secret was shown once and is gone. dorang stores a non-reversible digest "+
				"and cannot produce it again — not from the database, not from a backup, not "+
				"from a log. If you did not capture it, rotate the key: the id, budget, spend, "+
				"allow-list and ledger history survive a rotation.",
			CodeNotFound)
		return
	}
	s.render(w, r, http.StatusOK, "secret",
		secretPage{page: s.newPage("keys", "Your new secret", v), Secret: rv})
}

// refuseReadOnly answers a viewer who may look at the screens and not act on
// them: a header-authenticated one, or one whose session went away between the
// page and the click.
func (s *uiServer) refuseReadOnly(w http.ResponseWriter, r *http.Request, v viewer, what string) {
	s.renderMessage(w, r, http.StatusForbidden, v, "Read-only view",
		"This view cannot "+what+". A mutation is proved by a session's own token, and "+
			"this view is authorized by a header — sign in for a session, or use the "+
			"administration API.",
		CodeCrossSiteRefused)
}
