package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/luaext"
	"github.com/ziozzang/dorang/internal/notify"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
)

// The extension and notification wiring (DESIGN §11.5).
//
// Both subsystems are built here for the same reason everything else is: they
// are the only two places that need to know about each other, and neither
// internal/luaext nor internal/notify imports the other. internal/notify
// declares [notify.EmailHook]; internal/luaext implements the hooks; the
// adapter below is the join.

// buildHooks compiles extensions.lua, plus any hooks the embedder registered in
// Go, into an engine.
//
// A configuration with neither produces a nil engine, and a nil engine costs one
// branch per hook point on the request path. That is DESIGN §11.5's "disabled by
// default on the hot path" — not a flag consulted inside a live object, but the
// absence of the object.
//
// The policy directory is read only when extensions.lua.enabled is true.
// extensions.lua.dir has a default, so reading it unconditionally would turn
// "no such directory" into a startup failure for every deployment that never
// asked for extensions.
func buildHooks(cfg *config.Config, natives []luaext.Native,
	logf func(string, ...any), now func() time.Time) (*luaext.Engine, error) {

	lua := cfg.Extensions.Lua
	plugins := filterPlugins(cfg)
	o := luaext.Options{
		// A configured plugin turns the engine on by itself. A `filters:` block
		// that did nothing because `extensions.lua.enabled` was false elsewhere
		// in the file is a masking filter that does not mask, which is the one
		// failure this feature must not have.
		Enabled: lua.Enabled || len(natives) > 0 || len(plugins) > 0,
		Hooks:   lua.Hooks,
		Native:  natives,
		Plugins: plugins,
		Limits: luaext.Limits{
			Instructions: lua.Limits.Instructions,
			MemoryBytes:  int64(lua.Limits.MemoryMB) << 20,
			Timeout:      lua.Limits.Timeout.Duration(),
		},
		Logf: logf,
		Now:  now,
	}
	if lua.Enabled {
		o.Dir = config.ExpandPath(lua.Dir)
	}
	eng, err := luaext.New(o)
	if err != nil {
		return nil, fmt.Errorf("app: extensions.lua: %w", err)
	}
	return eng, nil
}

// buildNotifier assembles the notification pipeline.
func buildNotifier(cfg *config.Config, hooks *luaext.Engine,
	logf func(string, ...any), now func() time.Time) (*notify.Notifier, error) {

	n := &cfg.Notifications
	password, _ := n.Email.SMTP.Password.Value()
	webhookSecret, _ := n.Email.HTTP.Secret.Value()

	periods := make(map[string]time.Duration, len(n.DedupPeriods))
	for name, d := range n.DedupPeriods {
		periods[name] = d.Duration()
	}

	var hook notify.EmailHook
	if hooks.Enabled(luaext.HookEmail) {
		hook = &emailHook{eng: hooks}
	}

	nf, err := notify.New(notify.Options{
		Driver: n.Email.Driver,
		Events: n.Events,
		From:   n.Email.From,
		To:     n.Email.To,
		SMTP: notify.SMTPOptions{
			Addr:          n.Email.SMTP.Addr,
			Username:      n.Email.SMTP.Username,
			Password:      password,
			StartTLS:      n.Email.SMTP.StartTLS,
			TLSSkipVerify: n.Email.SMTP.TLSSkipVerify,
			Timeout:       n.Email.SMTP.Timeout.Duration(),
			HELO:          n.Email.SMTP.HELO,
		},
		HTTP: notify.HTTPOptions{
			URL:     n.Email.HTTP.URL,
			Secret:  webhookSecret,
			Timeout: n.Email.HTTP.Timeout.Duration(),
			Headers: n.Email.HTTP.Headers,
		},
		Hook:         hook,
		QueueSize:    n.QueueSize,
		Workers:      n.Workers,
		DedupPeriod:  n.DedupPeriod.Duration(),
		DedupPeriods: periods,
		Retry: notify.RetryOptions{
			MaxAttempts:      n.Retry.MaxAttempts,
			InitialBackoff:   n.Retry.InitialBackoff.Duration(),
			MaxBackoff:       n.Retry.MaxBackoff.Duration(),
			BreakerThreshold: n.Retry.BreakerThreshold,
			BreakerCooldown:  n.Retry.BreakerCooldown.Duration(),
		},
		Logf: logf,
		Now:  now,
	})
	if err != nil {
		return nil, fmt.Errorf("app: notifications: %w", err)
	}
	return nf, nil
}

// emailHook adapts internal/luaext's on_email onto internal/notify's
// [notify.EmailHook]. It is the only place the two packages meet.
//
// The view it builds carries the *rendered and already-redacted* message, which
// is what makes the hook safe to run: internal/notify removed the key material
// before this point, so there is none for an extension to read.
type emailHook struct{ eng *luaext.Engine }

func (h *emailHook) CanDeliver() bool { return h.eng.HasEmailTransport() }

func (h *emailHook) OnEmail(ctx context.Context, m *notify.Message) (bool, error) {
	var recipient string
	if len(m.To) > 0 {
		recipient = m.To[0]
	}
	v := luaext.EmailView{
		Event:       m.Event.String(),
		SubjectKind: m.Subject.Kind,
		SubjectID:   m.Subject.ID,
		Recipient:   recipient,
		Subject:     m.Line,
		Body:        m.Body,
		Driver:      m.Driver,
	}
	d, err := h.eng.OnEmail(ctx, &v)
	if err != nil {
		return false, err
	}
	return d.Denied, nil
}

// --- view construction ------------------------------------------------------
//
// Each of these is only ever called behind an Enabled check, so the cost of
// building a view belongs to a deployment that asked for hooks.

func requestView(rq *server.Request, c *call) luaext.RequestView {
	v := luaext.RequestView{
		RequestID:       rq.ID,
		Method:          rq.Method,
		Path:            rq.Path,
		Model:           rq.Model,
		Stream:          rq.Stream,
		BodyBytes:       int64(len(c.body)),
		InputTokens:     c.rreq.InputTokens,
		MaxOutputTokens: c.rreq.MaxOutputTokens,
	}
	if rq.Route != nil {
		v.Route = rq.Route.Name
	}
	if p, ok := rq.Principal.(*principal); ok && p != nil && p.p != nil {
		// Label is auth's non-reversible display label (R1-A): it is a name,
		// never trailing characters of the secret.
		v.KeyID, v.KeyName = p.p.KeyID, p.p.Label
		v.UserID, v.TeamID = p.p.UserID, p.p.TeamID
		v.Priority = p.p.PriorityClass
	}
	return v
}

func routeView(rq *server.Request, c *call, dec *router.Decision) luaext.RouteView {
	v := luaext.RouteView{
		Model:         rq.Model,
		Provider:      dec.Provider,
		Deployment:    dec.Deployment,
		Kind:          string(dec.Kind),
		UpstreamModel: dec.UpstreamModel,
		Attempt:       int64(rq.Result.Attempt),
		InputTokens:   c.rreq.InputTokens,
		Stream:        rq.Stream,
	}
	if p, ok := rq.Principal.(*principal); ok && p != nil && p.p != nil {
		v.KeyID, v.UserID, v.TeamID = p.p.KeyID, p.p.UserID, p.p.TeamID
		v.Priority = p.p.PriorityClass
	}
	return v
}

func responseView(rq *server.Request, dec *router.Decision, res *result, status int) luaext.ResponseView {
	v := luaext.ResponseView{
		RequestID:    rq.ID,
		Model:        rq.Model,
		Status:       int64(status),
		InputTokens:  rq.Result.Tokens.Input,
		OutputTokens: rq.Result.Tokens.Output,
		CostNanoUSD:  rq.Result.CostNanoUSD,
		TTFTMillis:   res.ttft.Milliseconds(),
		TotalMillis:  res.total.Milliseconds(),
		Attempts:     int64(rq.Result.Attempt),
		Stream:       rq.Stream,
	}
	if dec != nil {
		v.Provider, v.Deployment, v.UpstreamModel = dec.Provider, dec.Deployment, dec.UpstreamModel
	}
	if p, ok := rq.Principal.(*principal); ok && p != nil && p.p != nil {
		v.KeyID, v.UserID, v.TeamID = p.p.KeyID, p.p.UserID, p.p.TeamID
	}
	if e, ok := res.err.(*server.Error); ok && e != nil {
		v.ErrorCode = e.Code
	}
	return v
}

// hookDenied turns an extension's refusal into a client-visible error.
//
// It is a 403 rather than a 400: the request was well-formed and the gateway's
// policy refused it, which is exactly what 403 means. The reason comes from the
// extension, which is why internal/luaext strips nothing from it and why the
// views carry no internals for an extension to leak into one.
func hookDenied(d *luaext.RequestDecision) error {
	return server.NewError(http.StatusForbidden, server.TypePermission, d.Reason).WithCode(d.Code)
}
