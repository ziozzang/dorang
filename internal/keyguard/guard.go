package keyguard

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ziozzang/dorang/internal/notify"
)

// Action is what the guard does when both conditions hold. It mirrors
// `token_guard.action` of DESIGN §11.6.
type Action uint8

const (
	// ActionAlertOnly notifies and changes nothing.
	ActionAlertOnly Action = iota
	// ActionPend refuses the key with a distinct, documented error until an
	// operator releases it. THE DEFAULT.
	ActionPend
	// ActionThrottle is declared by §11.6 and is not implemented here. It is
	// present so that a configuration naming it is refused with an explanation
	// rather than silently downgraded to alert_only — a guard that quietly does
	// less than it was configured to do is worse than one that refuses to
	// start.
	ActionThrottle
	// ActionRevoke blocks the key. It is NOT the default and it is not
	// reversible in the same sense: the caller has to be reissued a credential.
	ActionRevoke
)

var actionNames = [...]string{"alert_only", "pend", "throttle", "revoke"}

// String returns the configuration spelling.
func (a Action) String() string {
	if int(a) >= len(actionNames) {
		return "alert_only"
	}
	return actionNames[a]
}

// ParseAction decodes a configured action.
func ParseAction(s string) (Action, error) {
	for i, n := range actionNames {
		if n == strings.TrimSpace(strings.ToLower(s)) {
			return Action(i), nil
		}
	}
	return ActionAlertOnly, fmt.Errorf("keyguard: %q is not a token_guard action; want one of %v",
		s, actionNames)
}

// ErrThrottleUnimplemented reports `action: throttle`.
var ErrThrottleUnimplemented = errors.New(
	"keyguard: token_guard.action throttle is designed (DESIGN §11.6) and not implemented; " +
		"configure pend, revoke or alert_only rather than have the guard do less than you asked")

// Condition is which of the two tests held. Both must hold before the guard
// acts, and reporting them separately is what lets an operator see WHY — or,
// far more often, why not.
type Condition uint8

const (
	// CondRelative: observed >= baseline x factor.
	CondRelative Condition = 1 << iota
	// CondAbsolute: observed >= min_absolute.
	CondAbsolute
)

// String renders the conditions that held.
func (c Condition) String() string {
	switch c {
	case CondRelative | CondAbsolute:
		return "relative+absolute"
	case CondRelative:
		return "relative"
	case CondAbsolute:
		return "absolute"
	}
	return "none"
}

// Config mirrors the `token_guard` block of DESIGN §11.6.
type Config struct {
	// Enabled turns the guard on. Off by default: an automated refusal is a
	// thing an operator opts into.
	Enabled bool
	// BaselineWindow is how much history the baseline is computed from.
	BaselineWindow time.Duration
	// Window is the interval the observed rate is measured over, and the unit
	// both rates are expressed in. §11.6's block does not name it; without it
	// "factor: 10" compares a number to a number of a different kind.
	Window time.Duration
	// Factor is the relative condition: observed >= baseline x factor.
	Factor float64
	// MinAbsolute is the absolute condition, in tokens per Window. Both must
	// hold. Without it the guard fires hardest on the quietest keys.
	MinAbsolute int64
	// MinHistory is how much history a key must have before the guard will act
	// on it. Below it the guard alerts and does nothing else, because a new key
	// has no baseline and every key would trip on its first busy hour.
	MinHistory time.Duration
	// Action is what to do when both conditions hold. The default is
	// [ActionPend].
	Action Action
	// Cooldown is how long the guard waits before acting on the same key again.
	Cooldown time.Duration
	// Now overrides the clock.
	Now func() time.Time
}

// Defaults, matching §11.6's example block.
const (
	DefaultBaselineWindow = 7 * 24 * time.Hour
	DefaultWindow         = time.Hour
	DefaultFactor         = 10.0
	DefaultMinAbsolute    = int64(100_000)
	// DefaultMinHistory is 24 hours. §11.6 requires "a stated minimum of
	// history" and does not state it; a day is the shortest span that contains
	// a whole daily cycle, and a baseline that does not contain one would call
	// every key's morning anomalous.
	DefaultMinHistory = 24 * time.Hour
	DefaultCooldown   = time.Hour
)

func (c *Config) fill() {
	if c.BaselineWindow <= 0 {
		c.BaselineWindow = DefaultBaselineWindow
	}
	if c.Window <= 0 {
		c.Window = DefaultWindow
	}
	if c.Factor <= 0 {
		c.Factor = DefaultFactor
	}
	if c.MinAbsolute <= 0 {
		c.MinAbsolute = DefaultMinAbsolute
	}
	if c.MinHistory <= 0 {
		c.MinHistory = DefaultMinHistory
	}
	if c.Cooldown <= 0 {
		c.Cooldown = DefaultCooldown
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Validate reports a configuration the guard cannot honour.
func (c Config) Validate() error {
	if c.Action == ActionThrottle {
		return ErrThrottleUnimplemented
	}
	if c.Factor < 0 {
		return errors.New("keyguard: token_guard.trigger.factor must not be negative")
	}
	if c.MinAbsolute < 0 {
		return errors.New("keyguard: token_guard.trigger.min_absolute must not be negative")
	}
	if c.BaselineWindow > 0 && c.Window > 0 && c.BaselineWindow < c.Window {
		return fmt.Errorf("keyguard: baseline_window (%s) is shorter than the observation window (%s); "+
			"the baseline would be computed from less traffic than it is compared against",
			c.BaselineWindow, c.Window)
	}
	return nil
}

// Enforcer applies the guard's decision.
//
// It is deliberately two things per action and not one. Marking a key pended in
// the store is the durable half; making every node's auth snapshot drop it is
// the half that decides whether the key actually stops serving. §11.2c: a guard
// that pends a key and leaves it serving for a cache TTL has not stopped
// anything, it has started a timer. An implementation that satisfies only the
// first half of this interface passes no test in this package.
type Enforcer interface {
	// Pend marks keyID pended and stops it serving on every node.
	Pend(ctx context.Context, keyID, reason string) error
	// Release clears a pend and lets the key serve again. ONE action —
	// §11.6's requirement — so this method takes nothing but the id.
	Release(ctx context.Context, keyID string) error
	// Revoke blocks the key and stops it serving on every node.
	Revoke(ctx context.Context, keyID string) error
}

// Finding is one evaluation of one key. It is returned as well as notified, so
// a caller can assert on it and an operator can read it.
type Finding struct {
	KeyID string
	// Observed is the tokens used in the most recent Window.
	Observed int64
	// Baseline is the key's own average tokens per Window over
	// BaselineWindow, excluding the observation window itself — a baseline that
	// included the spike would be raised by it.
	Baseline float64
	// Ratio is Observed / Baseline. It is +Inf for a key whose baseline is zero
	// and which used anything, which is honest: any traffic is infinitely more
	// than none, and it is the ABSOLUTE condition that decides whether that
	// matters.
	Ratio float64
	// Condition is which tests held.
	Condition Condition
	// History is how much history the key has.
	History time.Duration
	// NewKey is true when History is below MinHistory. A new key can only be
	// alerted about.
	NewKey bool
	// Action is what the guard actually did, which is not always what the
	// configuration says: a new key is alerted about whatever the action is.
	Action Action
	// Cooled is true when the finding was suppressed by the cooldown.
	Cooled bool
	// At is when the evaluation happened.
	At time.Time
}

// Tripped reports whether BOTH conditions held. This is the conjunction §11.6
// requires, in one place, so that no caller can accidentally act on one.
func (f Finding) Tripped() bool {
	return f.Condition&CondRelative != 0 && f.Condition&CondAbsolute != 0
}

// Acted reports whether the guard changed anything.
func (f Finding) Acted() bool { return f.Action == ActionPend || f.Action == ActionRevoke }

// String renders the finding for a log line.
func (f Finding) String() string {
	return fmt.Sprintf("key=%s observed=%d baseline=%.1f ratio=%s condition=%s new_key=%t action=%s",
		f.KeyID, f.Observed, f.Baseline, ratioString(f.Ratio), f.Condition, f.NewKey, f.Action)
}

// Guard is the token guard.
//
// A nil *Guard is the disabled guard: every method is a constant and costs one
// nil check, which is how a deployment that has not configured the guard pays
// nothing for it.
type Guard struct {
	cfg   Config
	hist  History
	enf   Enforcer
	notif *notify.Notifier

	mu       sync.Mutex
	lastAct  map[string]time.Time
	findings int
	acted    int
	alerted  int
}

// New builds a guard. It returns (nil, nil) when the guard is disabled, so a
// caller holds a typed nil rather than a table it will never consult.
func New(cfg Config, h History, e Enforcer, n *notify.Notifier) (*Guard, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if h == nil {
		return nil, errors.New("keyguard: New needs a History; the guard compares a key against its own past")
	}
	if e == nil {
		return nil, errors.New("keyguard: New needs an Enforcer; a guard that cannot stop a key " +
			"is a guard that sets a flag (DESIGN §11.2c)")
	}
	cfg.fill()
	return &Guard{cfg: cfg, hist: h, enf: e, notif: n, lastAct: map[string]time.Time{}}, nil
}

// Observe records usage. It is the metering path's entry point and does nothing
// but write history; evaluation happens in [Guard.Sweep].
func (g *Guard) Observe(ctx context.Context, keyID string, tokens int64, at time.Time) error {
	if g == nil {
		return nil
	}
	if at.IsZero() {
		at = g.cfg.Now()
	}
	return g.hist.Add(ctx, keyID, tokens, at)
}

// Evaluate examines one key and takes the configured action.
//
// The order is the order of §11.6's four rules, and each one can only make the
// outcome weaker:
//
//  1. Compute both conditions. Both must hold, or nothing happens beyond the
//     finding being returned.
//  2. A key with less than MinHistory of history is a NEW key: it is alerted
//     about and never acted on, whatever the action says.
//  3. The cooldown suppresses a repeat.
//  4. Whatever happened is announced.
func (g *Guard) Evaluate(ctx context.Context, keyID string) (Finding, error) {
	if g == nil {
		return Finding{}, nil
	}
	now := g.cfg.Now()
	f := Finding{KeyID: keyID, At: now, Action: ActionAlertOnly}

	windowStart := now.Add(-g.cfg.Window)
	observed, err := g.hist.Window(ctx, keyID, windowStart, now)
	if err != nil {
		return Finding{}, err
	}
	f.Observed = observed

	// The baseline EXCLUDES the observation window. Including it would let the
	// spike raise the very number it is compared against, which is how a
	// tenfold departure measures as ninefold and a guard misses the thing it
	// was built for.
	baseStart := now.Add(-g.cfg.BaselineWindow)
	baseTotal, err := g.hist.Window(ctx, keyID, baseStart, windowStart)
	if err != nil {
		return Finding{}, err
	}
	since, err := g.hist.Since(ctx, keyID)
	if err != nil {
		return Finding{}, err
	}
	if !since.IsZero() {
		f.History = now.Sub(since)
	}
	f.NewKey = f.History < g.cfg.MinHistory

	// The baseline is averaged over the history that EXISTS, not over the
	// configured window. A key with two days of history in a seven-day window
	// would otherwise have its average divided by seven and look five times
	// busier than it is.
	span := windowStart.Sub(baseStart)
	if !since.IsZero() && since.After(baseStart) {
		span = windowStart.Sub(since)
	}
	if span > 0 {
		f.Baseline = float64(baseTotal) / (float64(span) / float64(g.cfg.Window))
	}

	f.Ratio = ratio(float64(observed), f.Baseline)
	if f.Baseline > 0 && float64(observed) >= f.Baseline*g.cfg.Factor {
		f.Condition |= CondRelative
	}
	if f.Baseline <= 0 && observed > 0 {
		// No baseline at all: every unit of traffic is an infinite departure.
		// This is why the absolute condition is mandatory and not advisory.
		f.Condition |= CondRelative
	}
	if observed >= g.cfg.MinAbsolute {
		f.Condition |= CondAbsolute
	}

	g.mu.Lock()
	g.findings++
	last, seen := g.lastAct[keyID]
	cooled := seen && now.Sub(last) < g.cfg.Cooldown
	g.mu.Unlock()

	if !f.Tripped() {
		return f, nil
	}
	f.Cooled = cooled
	if cooled {
		return f, nil
	}

	action := g.cfg.Action
	if f.NewKey {
		// Rule 3. A new key has no baseline and the guard must not treat that
		// as anomalous, so it is downgraded to an alert here rather than
		// excluded above — the operator still gets told, which is the point of
		// "only alerts" rather than "does nothing".
		action = ActionAlertOnly
	}
	f.Action = action

	switch action {
	case ActionPend:
		if err := g.enf.Pend(ctx, keyID, f.reason()); err != nil {
			return f, fmt.Errorf("keyguard: pending %s: %w", keyID, err)
		}
	case ActionRevoke:
		if err := g.enf.Revoke(ctx, keyID); err != nil {
			return f, fmt.Errorf("keyguard: revoking %s: %w", keyID, err)
		}
	}

	g.mu.Lock()
	g.lastAct[keyID] = now
	if f.Acted() {
		g.acted++
	} else {
		g.alerted++
	}
	g.mu.Unlock()

	g.announce(f)
	return f, nil
}

// Sweep evaluates every key with history and returns the findings that tripped.
//
// It is a periodic job, not a request-path call: the guard is a statistical
// judgement over a window and there is nothing a per-request evaluation would
// see sooner.
func (g *Guard) Sweep(ctx context.Context) ([]Finding, error) {
	if g == nil {
		return nil, nil
	}
	keys, err := g.hist.Keys(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(keys) // deterministic order, so a sweep is reproducible
	var out []Finding
	var errs []error
	for _, id := range keys {
		f, err := g.Evaluate(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if f.Tripped() {
			out = append(out, f)
		}
	}
	return out, errors.Join(errs...)
}

// Release clears a pend. This is §11.6's "released by an operator in one
// action", and it is one call taking one argument for exactly that reason.
//
// The release is announced too. An operator who released a key wants the same
// record of it that the pend produced, and the person who ends an incident is
// often not the person who was paged for it.
func (g *Guard) Release(ctx context.Context, keyID, by string) error {
	if g == nil {
		return nil
	}
	if err := g.enf.Release(ctx, keyID); err != nil {
		return fmt.Errorf("keyguard: releasing %s: %w", keyID, err)
	}
	g.mu.Lock()
	delete(g.lastAct, keyID)
	g.mu.Unlock()

	g.notif.Send(notify.Notification{
		Event:   notify.EventTokenGuard,
		Subject: notify.Subject{Kind: "key", ID: keyID},
		At:      g.cfg.Now(),
		Fields: []notify.Field{
			{Name: "key_id", Value: keyID},
			{Name: "action", Value: "release"},
			{Name: "released_by", Value: by},
		},
	})
	return nil
}

// announce sends the §11.6 event. It carries the observed rate, the baseline
// and which condition tripped, because those three are what the operator needs
// to decide whether the guard was right — and it is sent with Send rather than
// Notify, so that a pend is never deduplicated away behind an earlier alert for
// the same key.
func (g *Guard) announce(f Finding) {
	g.notif.Send(notify.Notification{
		Event:   notify.EventTokenGuard,
		Subject: notify.Subject{Kind: "key", ID: f.KeyID},
		At:      f.At,
		Fields: []notify.Field{
			{Name: "key_id", Value: f.KeyID},
			{Name: "action", Value: f.Action.String()},
			{Name: "condition", Value: f.Condition.String()},
			{Name: "observed_rate", Value: strconv.FormatInt(f.Observed, 10)},
			{Name: "baseline_rate", Value: strconv.FormatFloat(f.Baseline, 'f', 1, 64)},
			{Name: "factor", Value: ratioString(f.Ratio)},
			{Name: "factor_threshold", Value: strconv.FormatFloat(g.cfg.Factor, 'f', -1, 64)},
			{Name: "absolute", Value: strconv.FormatInt(f.Observed, 10)},
			{Name: "absolute_threshold", Value: strconv.FormatInt(g.cfg.MinAbsolute, 10)},
			{Name: "window", Value: g.cfg.Window.String()},
			{Name: "baseline_window", Value: g.cfg.BaselineWindow.String()},
			{Name: "history", Value: f.History.Round(time.Minute).String()},
			{Name: "min_history", Value: g.cfg.MinHistory.String()},
		},
	})
}

// reason is the short, non-secret explanation written to the key's row.
func (f Finding) reason() string {
	return fmt.Sprintf("token guard: %d tokens in the window against a baseline of %.0f (%sx), %s",
		f.Observed, f.Baseline, ratioString(f.Ratio), f.Condition)
}

// Stats is what the guard has done.
type Stats struct {
	// Evaluations is how many keys were examined.
	Evaluations int
	// Acted is how many keys were pended or revoked.
	Acted int
	// Alerted is how many tripped but were only alerted about — a new key, or
	// an alert_only configuration. It is counted separately because the two
	// are different outcomes and an operator comparing them is asking whether
	// the guard is calibrated.
	Alerted int
}

// Stats returns a snapshot.
func (g *Guard) Stats() Stats {
	if g == nil {
		return Stats{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return Stats{Evaluations: g.findings, Acted: g.acted, Alerted: g.alerted}
}

// Config returns the guard's configuration, filled with defaults.
func (g *Guard) Config() Config {
	if g == nil {
		return Config{}
	}
	return g.cfg
}

// ratio is observed/baseline, +Inf for a key with no baseline that used
// something. +Inf is the honest answer — any traffic is infinitely more than
// none — and it is the ABSOLUTE condition that decides whether it matters.
func ratio(observed, baseline float64) float64 {
	if baseline <= 0 {
		if observed <= 0 {
			return 0
		}
		return math.Inf(1)
	}
	return observed / baseline
}

func ratioString(r float64) string {
	if math.IsInf(r, 0) || math.IsNaN(r) {
		return "inf"
	}
	return strconv.FormatFloat(r, 'f', 1, 64)
}
