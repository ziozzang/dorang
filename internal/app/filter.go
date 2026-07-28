package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/ziozzang/dorang/internal/backend"
	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/luaext"
	"github.com/ziozzang/dorang/internal/mask"
	"github.com/ziozzang/dorang/internal/metrics"
	"github.com/ziozzang/dorang/internal/server"
)

// Transform filters (DESIGN §10.5b): the join between the plugin surface
// (internal/luaext) and the reversible mask (internal/mask).
//
// The split of responsibility is the design, and it is worth stating where both
// halves are visible:
//
//   - The **plugin** decides what is sensitive, walking the request's text
//     segments. It is the operator's code, and their disclosure rules are
//     theirs.
//   - The **host** owns the mask table, mints placeholders, and does every
//     unmasking pass. Lua never sees the table and never touches a frame.
//
// # One mask per model, not one per filter entry
//
// A model may list several filters, and they share one mask session. Two
// sessions would mean two placeholder namespaces in one body, and an unmasker
// that had to guess which table a placeholder came from — or worse, one that
// counted the other session's placeholders as invented. So a model's filters
// contribute their patterns to one set, run in configured order, and share one
// table.

// filterTable resolves a client-facing model name to its filters. It is built
// once per configuration generation and swapped with the rest of the dispatch
// state.
//
// Resolution is by the **client-facing model**, deliberately. Attaching filters
// to a deployment would mean a fail-back hop to another deployment masked
// differently, which changes the bytes and breaks both the prefix claim and the
// unmasking. The model is known before routing; the deployment is not.
type filterTable struct {
	byModel map[string]*modelFilter
}

type modelFilter struct {
	model   string
	plugins []string
	mask    *mask.Filter
	// onRequest is `on: [request]`. onResponse is recorded for completeness and
	// is deliberately not consulted: once a request has been masked, unmasking
	// the answer is not optional — leaving it off would return the gateway's own
	// placeholders to the caller. Configuration refuses `on: [response]` alone
	// for the mirror-image reason.
	onRequest  bool
	onResponse bool
}

func (t *filterTable) forModel(name string) *modelFilter {
	if t == nil || len(t.byModel) == 0 {
		return nil
	}
	return t.byModel[name]
}

// pluginsFor returns the luaext plugin declarations for the configured filters.
func filterPlugins(cfg *config.Config) []luaext.Plugin {
	out := make([]luaext.Plugin, 0, len(cfg.Filters.Plugins))
	for _, p := range cfg.Filters.Plugins {
		fail, _ := luaext.ParseFailMode(p.Fail)
		out = append(out, luaext.Plugin{
			Name:   p.Name,
			Path:   config.ExpandPath(p.Path),
			Config: p.Config,
			Fail:   fail,
		})
	}
	return out
}

// buildFilters compiles the per-model filter chains.
func buildFilters(cfg *config.Config, logf func(string, ...any)) (*filterTable, error) {
	t := &filterTable{}
	secret, _ := cfg.Filters.Secret.Value()

	for i := range cfg.Models {
		m := &cfg.Models[i]
		if len(m.Filters) == 0 {
			continue
		}
		mf := &modelFilter{model: m.Name}
		var patterns []mask.PatternSpec
		var scope mask.Scope
		var retain = mask.DefaultRetain
		for j := range m.Filters {
			f := &m.Filters[j]
			mf.plugins = append(mf.plugins, f.Plugin)
			for _, on := range f.On {
				switch on {
				case config.FilterOnRequest:
					mf.onRequest = true
				case config.FilterOnResponse:
					mf.onResponse = true
				}
			}
			if j == 0 {
				s, ok := mask.ParseScope(f.Scope)
				if !ok {
					return nil, fmt.Errorf("app: models[%d].filters[%d].scope: %q is not a scope", i, j, f.Scope)
				}
				scope = s
				if d := f.Retain.Duration(); d > 0 {
					retain = d
				}
			}
			for _, p := range f.Patterns {
				patterns = append(patterns, mask.PatternSpec{Name: p.Name, Regexp: p.Regexp})
			}
		}
		if len(patterns) > 0 {
			mk, err := mask.New(mask.Config{
				Name:     m.Name,
				Patterns: patterns,
				Secret:   []byte(secret),
				Scope:    scope,
				Retain:   retain,
			})
			if err != nil {
				return nil, fmt.Errorf("app: models[%d] (%s): %w", i, m.Name, err)
			}
			mf.mask = mk
		}
		if mf.mask != nil && logf != nil {
			// The identity of (pattern set ‖ secret) is worth one startup line:
			// when it changes, every masked conversation's upstream bytes change
			// with it and every backend prefix cache cold-starts. An operator
			// correlating a latency step with a config edit has nothing else to
			// go on.
			logf("app: filter: %s: patterns=%v scope=%s derivation=%s",
				m.Name, mf.mask.Patterns(), mf.mask.Scope(), mf.mask.Version())
		}
		if t.byModel == nil {
			t.byModel = make(map[string]*modelFilter, 4)
		}
		t.byModel[m.Name] = mf
	}
	return t, nil
}

// filterRequest runs the transform filters over a decoded request.
//
// It is called from decode, before the prefix digest is computed and before the
// input-token estimate is taken, because both must describe what the upstream
// will actually be sent (DESIGN §10.5b rule 5).
func (d *dispatcher) filterRequest(ctx context.Context, st *dispatchState,
	rq *server.Request, c *call) error {

	mf := st.filters.forModel(rq.Model)
	if mf == nil || !mf.onRequest {
		return nil
	}
	doc, ok := requestDoc(c)
	if !ok {
		// Fail closed. A model carrying a masking filter, used on a surface
		// whose text this build cannot reach, must not quietly send that text
		// upstream unmasked.
		return server.NewError(http.StatusUnprocessableEntity, server.TypeInvalidRequest,
			"this model has a transform filter, which does not apply to "+c.kind.String()+
				" requests; the request is refused rather than sent unfiltered").
			WithCode("filter_unsupported_surface")
	}

	var masker luaext.Masker
	if mf.mask != nil {
		if !mf.mask.Scope().Deterministic() {
			// Per-request placeholders mean the upstream bytes differ on every
			// turn, so there is no prefix to claim. Suppressing the claim is
			// honest; leaving it would pin a conversation to a deployment for a
			// cache hit that cannot happen.
			c.noAffinity = true
		}
		sess, err := mf.mask.Session(mask.SessionOptions{Salt: filterSalt(mf, rq, c)})
		if err != nil {
			return server.NewError(http.StatusInternalServerError, server.TypeAPIError,
				"the request filter could not start").WithCode("filter_failed")
		}
		c.mask = sess
		masker = sess
	}

	v := luaext.FilterView{
		RequestID: rq.ID,
		Model:     rq.Model,
		Stream:    rq.Stream,
		Doc:       doc,
		Mask:      masker,
		Plugins:   mf.plugins,
	}
	if rq.Route != nil {
		v.Route = rq.Route.Name
	}
	if p, ok := rq.Principal.(*principal); ok && p != nil && p.p != nil {
		v.KeyID, v.KeyName = p.p.KeyID, p.p.Label
		v.UserID, v.TeamID = p.p.UserID, p.p.TeamID
		v.Priority = p.p.PriorityClass
	}

	fd := st.hooks.FilterRequest(ctx, &v)
	if fd.Failed && d.logf != nil {
		// The error names the filter and the failure, never the text.
		d.logf("app: request filter on %s: %v", rq.Model, fd.Err)
	}
	if fd.Refuse {
		c.mask = nil
		reason := fd.Reason
		if reason == "" {
			reason = "a required request filter did not complete"
		}
		return server.NewError(http.StatusUnprocessableEntity, server.TypeInvalidRequest, reason).
			WithCode(fd.Code)
	}
	if c.mask != nil && c.mask.Failed() {
		c.mask = nil
		return server.NewError(http.StatusUnprocessableEntity, server.TypeInvalidRequest,
			"a required request filter did not complete").WithCode(luaext.FilterDenyCode)
	}
	return nil
}

// filterSalt derives the scope's identity.
//
// For a conversation the salt is the caller's session header when they sent one,
// and otherwise a digest of the conversation's *opening* — which is stable
// across turns for the same reason the prefix chain is: the protocols resend the
// whole history, so turn N+1 opens the same way turn N did. A client that
// trims its history changes the opening and gets new placeholders, which costs a
// cache miss and never a wrong answer.
func filterSalt(mf *modelFilter, rq *server.Request, c *call) string {
	switch mf.mask.Scope() {
	case mask.ScopeRequest:
		return ""
	case mask.ScopePrincipal:
		return "principal\x00" + principalID(rq)
	case mask.ScopeTenant:
		team := ""
		if p, ok := rq.Principal.(*principal); ok && p != nil && p.p != nil {
			team = p.p.TeamID
		}
		if team == "" {
			// Without a tenant there is nothing to scope to, and widening
			// silently to "everyone" would be the opposite of what was asked
			// for. Fall back to the principal, which is narrower.
			return "principal\x00" + principalID(rq)
		}
		return "tenant\x00" + team
	default:
		if s := rq.HTTP.Header.Get(HeaderSession); s != "" {
			return "session\x00" + s
		}
		return "opening\x00" + mask.Salt(openingSegment(c.creq)...)
	}
}

// openingSegment is the part of a conversation that does not change as it grows.
func openingSegment(creq *canonical.Request) []string {
	if creq == nil {
		return nil
	}
	out := []string{creq.System.Flatten()}
	for i := range creq.Messages {
		if creq.Messages[i].Role == canonical.RoleUser {
			out = append(out, creq.Messages[i].Content.Flatten())
			break
		}
	}
	return out
}

// requestDoc collects the text a filter may rewrite.
//
// # What is deliberately not collected
//
//   - A thinking block's text. It travels with integrity material the provider
//     signed (canonical.Thinking.Signature) and dorang replays byte-identically
//     or not at all (§10.2). Rewriting the text under a signature it no longer
//     matches would be a corruption dorang caused.
//   - A tool call's arguments. canonical.ToolUse.Input is kept raw precisely
//     because re-encoding a decoded map reorders keys, and "a model that emitted
//     a specific ordering on turn one must see the same bytes echoed on turn
//     two". A filter that re-encoded it would change those bytes and break the
//     determinism the whole design just bought. This is a real gap — PII does
//     appear in tool arguments — and it is a stated one rather than a silent
//     one.
//   - Base64 and URL sources. Masking inside an encoded payload produces neither
//     a valid payload nor a mask.
func requestDoc(c *call) (*luaext.Doc, bool) {
	creq := c.creq
	if creq == nil {
		return nil, false
	}
	doc := luaext.NewDoc()
	for i := range creq.System {
		addBlock(doc, "system["+strconv.Itoa(i)+"]", &creq.System[i])
	}
	for i := range creq.Messages {
		m := &creq.Messages[i]
		base := "message[" + strconv.Itoa(i) + "]"
		for j := range m.Content {
			addBlock(doc, base+".content["+strconv.Itoa(j)+"]", &m.Content[j])
		}
	}
	if creq.Prompt != nil {
		for i := range creq.Prompt.Texts {
			doc.Add("prompt["+strconv.Itoa(i)+"]", &creq.Prompt.Texts[i])
		}
	}
	return doc, true
}

func addBlock(doc *luaext.Doc, label string, b *canonical.Block) {
	switch b.Kind {
	case canonical.KindText:
		doc.Add(label, &b.Text)
	case canonical.KindDocument, canonical.KindImage:
		if b.Source != nil && b.Source.Kind == canonical.SourceText {
			doc.Add(label+".source", &b.Source.Data)
		}
	case canonical.KindToolResult:
		if b.ToolResult == nil {
			return
		}
		for k := range b.ToolResult.Content {
			addBlock(doc, label+".result["+strconv.Itoa(k)+"]", &b.ToolResult.Content[k])
		}
	}
}

// ---------------------------------------------------------------------------
// the response side, which is entirely the host's

// transform is this request's response-side filter in the shape internal/backend
// takes, or nil when nothing was masked — which is every request on a model with
// no filter configured, and costs one nil check there.
//
// The mask session itself does not cross the seam. §10.5b rule 1 is that the
// table appears in no log line, no metric label, no trace and no ledger row, and
// the narrowest way to keep that true is for L5 to hold neutral events and
// neutral responses and never a table.
func (c *call) transform() backend.Transform {
	if c.mask == nil {
		return nil
	}
	return maskTransform{c: c}
}

type maskTransform struct{ c *call }

// Response implements backend.Transform.
func (m maskTransform) Response(r *canonical.Response) { unmaskResponse(m.c, r) }

// Stream implements backend.Transform.
func (m maskTransform) Stream() backend.StreamTransform { return newStreamUnmasker(m.c) }

// unmaskResponse restores a complete answer in place.
func unmaskResponse(c *call, resp *canonical.Response) {
	if c.mask == nil || resp == nil {
		return
	}
	for i := range resp.Choices {
		m := &resp.Choices[i].Message
		for j := range m.Content {
			unmaskBlock(c, &m.Content[j])
		}
		if m.Refusal != "" {
			m.Refusal = c.mask.Unmask(m.Refusal)
		}
	}
}

func unmaskBlock(c *call, b *canonical.Block) {
	switch b.Kind {
	case canonical.KindText:
		b.Text = c.mask.Unmask(b.Text)
	case canonical.KindToolResult:
		if b.ToolResult == nil {
			return
		}
		for k := range b.ToolResult.Content {
			unmaskBlock(c, &b.ToolResult.Content[k])
		}
	}
}

// streamUnmasker is the response half of §10.5b, composed into §10.5's single
// pass over the stream.
//
// It holds a bounded tail — at most mask.PlaceholderLen-1 bytes — because a
// placeholder can straddle two frames. When an event carries no text to attach
// the tail to and the stream is about to move on, the tail is emitted as its own
// delta rather than dropped: a frame must stay valid, and text the model sent
// must reach the client even when it happened to look like the beginning of a
// placeholder.
type streamUnmasker struct {
	u      *mask.Unmasker
	choice int
}

func newStreamUnmasker(c *call) *streamUnmasker {
	if c.mask == nil {
		return nil
	}
	return &streamUnmasker{u: c.mask.Unmasker()}
}

// Rewrite implements backend.StreamTransform: it applies the unmasker to one
// event and reports whether a synthesized flush event must be written *before*
// it.
func (s *streamUnmasker) Rewrite(ev *canonical.StreamEvent) (flush *canonical.StreamEvent) {
	if s == nil {
		return nil
	}
	text := false
	for i := range ev.Delta.Content {
		b := &ev.Delta.Content[i]
		if b.Kind != canonical.KindText {
			continue
		}
		text = true
		s.choice = ev.Choice
		b.Text = s.u.Write(b.Text)
	}
	if ev.Delta.Refusal != "" {
		text = true
		ev.Delta.Refusal = s.u.Write(ev.Delta.Refusal)
	}
	if text || s.u.Held() == 0 {
		return nil
	}
	return s.Flush()
}

// Flush implements backend.StreamTransform: it turns whatever is held into a
// delta of its own, or nil when nothing is held.
func (s *streamUnmasker) Flush() *canonical.StreamEvent {
	if s == nil || s.u.Held() == 0 {
		return nil
	}
	out := s.u.Flush()
	if out == "" {
		return nil
	}
	return &canonical.StreamEvent{
		Type:   canonical.EventDelta,
		Choice: s.choice,
		Delta:  canonical.Delta{Content: []canonical.Block{canonical.TextBlock(out)}},
	}
}

// filterCounters are what a completed request may say about its mask: counts,
// and nothing else.
//
// DESIGN §10.5b rule 1 — the table appears in no log line, no metric label, no
// trace and no ledger row — is enforced here by there being no path from these
// numbers back to the text. They are unlabelled for a second reason: a label
// carrying a model or a key would be caller-influenced cardinality on a family
// that fires on every masked request.
type filterCounters struct {
	masked     atomic.Uint64
	restored   atomic.Uint64
	unresolved atomic.Uint64
	refused    atomic.Uint64
}

// observe folds one finished request's session into the process counters and
// says whether anything is worth telling the operator about.
func (d *dispatcher) observeFilter(c *call) {
	if c.mask == nil {
		return
	}
	st := c.mask.Stats()
	d.filter.masked.Add(uint64(st.Issued))
	d.filter.restored.Add(uint64(st.Resolved))
	if st.Unresolved == 0 {
		return
	}
	d.filter.unresolved.Add(uint64(st.Unresolved))
	if d.logf != nil {
		// §10.5b rule 3: an invented placeholder is a signal worth having. The
		// line carries the count and the model, never the placeholder — a
		// placeholder is half of the mapping, and printing it beside a response
		// would be the leak this whole section exists to prevent.
		d.logf("app: filter: %s: %d placeholder-shaped strings in the answer were not issued "+
			"by this request and were left alone", c.model, st.Unresolved)
	}
}

// filterMetrics exposes the counters.
func (d *dispatcher) filterMetrics() metrics.Collector {
	return metrics.CollectorFunc{Name: "filter", Fn: func(w *metrics.Writer) {
		w.Metric("dorang_filter_masked_total", metrics.Counter,
			"Values a transform filter replaced with a placeholder before the request went "+
				"upstream (DESIGN §10.5b).")
		w.Uint(d.filter.masked.Load())
		w.Metric("dorang_filter_restored_total", metrics.Counter,
			"Placeholders restored to the caller's own text in an answer.")
		w.Uint(d.filter.restored.Load())
		w.Metric("dorang_filter_unresolved_total", metrics.Counter,
			"Placeholder-shaped strings in an answer that this request never issued: echoed "+
				"inside a longer token, truncated, replayed from another scope, or invented. "+
				"They are left exactly as they are; a rising count means a model is producing "+
				"text that looks like gateway machinery.")
		w.Uint(d.filter.unresolved.Load())
		w.Metric("dorang_filter_refused_total", metrics.Counter,
			"Requests stopped because a fail-closed filter did not complete. This is the one "+
				"place an extension refuses traffic on purpose (§10.5b).")
		w.Uint(d.filter.refused.Load())
	}}
}
