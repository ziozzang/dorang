// Package alerts decides when a condition that can run out — a provider plan
// window, a vendor's spend limit or balance, a key's or team's budget, a plan
// that has stopped serving — is news, and hands the news to internal/notify.
//
// News is a TRANSITION and nothing else: a condition that has just become a
// problem, and the same condition recovering. Each is announced once. A
// condition that stays low is not announced again unless a reminder interval
// is configured (it is off by default), because a state notification repeated
// while nothing changes is spam. A figure hovering at the threshold does not
// flap: it becomes low at Percent and recovers only above RecoverPercent.
//
// A condition is observed as low or recovered as often as its source sees it:
// on every request for a budget, after every usage-probe poll for a vendor
// figure. Observing is cheap — a map lookup — unless the observation changes
// the state.
//
// Those transitions are decided by the store, not by this process. Each is a
// conditional write on the condition's alert_state row, and only the
// observation that wins it announces, so one crossing is one notification
// however many nodes see it and however often the gateway restarts. The row
// also records which transports the announcement is still owed to: a
// transport that fails is retried from there by [Engine.Sweep], and a
// transport that delivered is never sent the same announcement again.
//
// Without a store the same state machine runs in memory, which keeps the
// one-per-crossing property within a process and loses it across restarts.
package alerts

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ziozzang/dorang/internal/notify"
)

// Store is the durable half: internal/store implements it.
type Store interface {
	AlertRaise(ctx context.Context, key, event string, now, remindBefore time.Time,
		pending, payload string) (bool, int64, error)
	AlertResolve(ctx context.Context, key string, now time.Time, pending, payload string) (bool, int64, error)
	AlertDelivered(ctx context.Context, key string, seq int64, driver string, now time.Time) error
	AlertsPending(ctx context.Context, before time.Time, limit int) ([]Pending, error)
	AlertRetryClaim(ctx context.Context, key string, seq int64, last, now time.Time) ([]string, bool, error)
	AlertsActive(ctx context.Context, limit int) ([]Active, error)
}

// Active is a condition the store holds as low.
type Active struct {
	Key      string
	LastSent time.Time
}

// Pending is an announcement some transport still owes.
type Pending struct {
	Key      string
	Event    string
	Seq      int64
	Pending  []string
	Payload  string
	LastSent time.Time
}

// Condition is one observation.
type Condition struct {
	Event   notify.Event
	Key     string // stable identity of the thing observed
	Subject notify.Subject
	Low     bool
	// Fields describe it; they are rendered only when the observation is news.
	Fields []notify.Field
}

// Options configures an Engine.
type Options struct {
	Notifier *notify.Notifier
	Store    Store // nil keeps the state in memory
	// Percent is the share of a ceiling left at which a condition is low.
	Percent float64
	// FloorNano is the absolute floor for a balance, in nano-USD; 0 is none.
	FloorNano int64
	// RecoverPercent is the share left above which a low condition has
	// recovered; between Percent and it the state does not change. Default
	// twice Percent.
	RecoverPercent float64
	// Remind, when positive, re-announces a condition that has stayed low that
	// long. Zero — the default — announces only transitions.
	Remind time.Duration
	// RetryAfter is how long an undelivered announcement waits before Sweep
	// sends it again to the transports that still owe it.
	RetryAfter time.Duration
	// Recheck bounds how long this process trusts what it last learned about a
	// condition before asking the store again. Another node may have cleared
	// or raised it since; the store is the authority. Default 1m.
	Recheck time.Duration
	Now     func() time.Time
	Logf    func(string, ...any)
}

type cached struct {
	low      bool
	lastSent time.Time
	// checked is when the store last confirmed this state.
	checked time.Time
}

// Engine is the state machine. A nil *Engine observes nothing.
type Engine struct {
	o     Options
	mu    sync.Mutex
	cache map[string]cached
	// lows counts cached conditions that are low, so that a caller on the
	// request path can skip building a key it would only report as fine.
	lows atomic.Int64
}

// AnyLow reports whether this process knows of any condition that is low.
func (e *Engine) AnyLow() bool { return e != nil && e.lows.Load() > 0 }

// New builds an engine, or returns nil when there is no notifier to tell.
func New(o Options) *Engine {
	if o.Notifier == nil {
		return nil
	}
	if o.Percent <= 0 {
		o.Percent = 5
	}
	if o.RecoverPercent <= o.Percent {
		o.RecoverPercent = o.Percent * 2
	}
	if o.RecoverPercent > 100 {
		o.RecoverPercent = 100
	}
	if o.RetryAfter <= 0 {
		o.RetryAfter = 5 * time.Minute
	}
	if o.Recheck <= 0 {
		o.Recheck = time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return &Engine{o: o, cache: map[string]cached{}}
}

// Percent is the configured threshold.
func (e *Engine) Percent() float64 {
	if e == nil {
		return 0
	}
	return e.o.Percent
}

// Verdict is what one figure says about its condition.
type Verdict int8

const (
	// Hold is a figure in the hysteresis band, or one that cannot be judged:
	// the condition keeps whatever state it had and nothing is observed.
	Hold Verdict = iota
	Low
	OK
)

// Judge classifies left of limit: Low at or under Percent, OK above
// RecoverPercent, Hold between. It also returns the percentage left. A
// non-positive limit has nothing to be a share of and is OK.
func (e *Engine) Judge(left, limit float64) (Verdict, float64) {
	if e == nil || limit <= 0 {
		return OK, 0
	}
	if left < 0 {
		left = 0
	}
	pct := left / limit * 100
	switch {
	case pct <= e.o.Percent:
		return Low, pct
	case pct > e.o.RecoverPercent:
		return OK, pct
	}
	return Hold, pct
}

// JudgeBalance classifies a prepaid balance: Low under the absolute floor or —
// when the provider states what was credited — at or under Percent of it; OK
// only when it is clear of both by the same hysteresis (half again the floor,
// above RecoverPercent). hasPct says the percentage means something.
func (e *Engine) JudgeBalance(remainingNano, totalNano int64) (v Verdict, pct float64, hasPct bool) {
	if e == nil {
		return OK, 0, false
	}
	floor := e.o.FloorNano
	pv := Hold
	if totalNano > 0 {
		pv, pct = e.Judge(float64(remainingNano), float64(totalNano))
		hasPct = true
	}
	switch {
	case floor > 0 && remainingNano < floor, pv == Low:
		return Low, pct, hasPct
	case (floor == 0 || remainingNano >= floor+floor/2) && (pv == OK || (!hasPct && floor > 0)):
		return OK, pct, hasPct
	}
	return Hold, pct, hasPct
}

// Observe records one observation and announces it when it is a transition:
// Low on a condition that was not low, or Low=false on one that was. It does
// not block on delivery. Fields describe the condition and are rendered only
// when there is something to announce, with a `state` of low, reminder or
// recovered.
func (e *Engine) Observe(ctx context.Context, c Condition) {
	if e == nil || c.Key == "" {
		return
	}
	now := e.o.Now()
	e.mu.Lock()
	st, known := e.cache[c.Key]
	// Without a store the cache is the only state there is, and always fresh.
	fresh := known && (e.o.Store == nil || now.Sub(st.checked) < e.o.Recheck)
	switch {
	case !c.Low && fresh && !st.low:
		e.mu.Unlock()
		return
	case c.Low && fresh && st.low && (e.o.Remind <= 0 || now.Sub(st.lastSent) < e.o.Remind):
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	state := "low"
	switch {
	case !c.Low:
		state = "recovered"
	case known && st.low:
		state = "reminder"
	}
	fields := append(append([]notify.Field(nil), c.Fields...), notify.Field{Name: "state", Value: state})
	payload, _ := json.Marshal(payloadDoc{Kind: c.Subject.Kind, ID: c.Subject.ID, Fields: fields})
	pending := strings.Join(e.drivers(), ",")

	if !c.Low {
		announce, seq := known && st.low, int64(0)
		if e.o.Store != nil {
			var err error
			if announce, seq, err = e.o.Store.AlertResolve(ctx, c.Key, now, pending, string(payload)); err != nil {
				e.o.Logf("alerts: %s: %v", c.Key, err)
				return
			}
		}
		e.set(c.Key, cached{checked: now})
		if announce {
			e.send(c.Event, c.Key, seq, c.Subject, fields, nil)
		}
		return
	}

	announce, seq := true, int64(0)
	if e.o.Store != nil {
		// A reminder is due only when one is configured; otherwise the claim
		// succeeds only for a condition that was not low.
		remindBefore := time.Unix(0, 0)
		if e.o.Remind > 0 {
			remindBefore = now.Add(-e.o.Remind)
		}
		var err error
		announce, seq, err = e.o.Store.AlertRaise(ctx, c.Key, c.Event.String(), now,
			remindBefore, pending, string(payload))
		if err != nil {
			e.o.Logf("alerts: %s: %v", c.Key, err)
			return
		}
	}
	// A lost claim keeps the cached announcement time: another node announced,
	// and the store is asked again after Recheck either way.
	lastSent := now
	if !announce && known && st.low {
		lastSent = st.lastSent
	}
	e.set(c.Key, cached{low: true, lastSent: lastSent, checked: now})
	if announce {
		e.send(c.Event, c.Key, seq, c.Subject, fields, nil)
	}
}

func (e *Engine) set(key string, st cached) {
	e.mu.Lock()
	prev, had := e.cache[key]
	e.cache[key] = st
	switch {
	case st.low && !(had && prev.low):
		e.lows.Add(1)
	case !st.low && had && prev.low:
		e.lows.Add(-1)
	}
	e.mu.Unlock()
}

func (e *Engine) drivers() []string {
	return strings.Split(e.o.Notifier.Driver(), ",")
}

func (e *Engine) send(ev notify.Event, key string, seq int64, subj notify.Subject,
	fields []notify.Field, only []string) {

	no := notify.Notification{Event: ev, Subject: subj, Fields: fields, At: e.o.Now(), Only: only}
	if e.o.Store != nil {
		st := e.o.Store
		no.Delivered = func(driver string) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := st.AlertDelivered(ctx, key, seq, driver, e.o.Now()); err != nil {
				e.o.Logf("alerts: %s: recording %s delivery: %v", key, driver, err)
			}
		}
	}
	if !e.o.Notifier.Send(no) {
		e.o.Logf("alerts: %s: %s could not be queued; it is retried from the store", key, ev)
	}
}

type payloadDoc struct {
	Kind   string         `json:"kind,omitempty"`
	ID     string         `json:"id,omitempty"`
	Fields []notify.Field `json:"fields"`
}

// Load primes the cache with the conditions the store holds as low, so that a
// process that has just started clears them when it observes a recovery (the
// request path asks only while something is known to be low).
func (e *Engine) Load(ctx context.Context) error {
	if e == nil || e.o.Store == nil {
		return nil
	}
	act, err := e.o.Store.AlertsActive(ctx, 10_000)
	if err != nil {
		return err
	}
	for _, a := range act {
		// checked stays zero: the first observation of each asks the store.
		e.set(a.Key, cached{low: true, lastSent: a.LastSent})
	}
	return nil
}

// Sweep resends announcements a transport has not delivered after RetryAfter,
// to that transport only. It is a no-op without a store.
func (e *Engine) Sweep(ctx context.Context) {
	if e == nil || e.o.Store == nil {
		return
	}
	now := e.o.Now()
	pend, err := e.o.Store.AlertsPending(ctx, now.Add(-e.o.RetryAfter), 64)
	if err != nil {
		e.o.Logf("alerts: sweep: %v", err)
		return
	}
	for _, p := range pend {
		ev, ok := notify.ParseEvent(p.Event)
		if !ok {
			continue
		}
		var doc payloadDoc
		if json.Unmarshal([]byte(p.Payload), &doc) != nil {
			continue
		}
		owed, claimed, err := e.o.Store.AlertRetryClaim(ctx, p.Key, p.Seq, p.LastSent, now)
		if err != nil || !claimed {
			continue
		}
		e.send(ev, p.Key, p.Seq, notify.Subject{Kind: doc.Kind, ID: doc.ID}, doc.Fields, owed)
	}
}

// Percent renders a percentage for a notification field.
func Percent(p float64) string { return strconv.FormatFloat(p, 'f', 2, 64) }

// USD renders nano-USD for a notification field.
func USD(nano int64) string {
	neg := nano < 0
	if neg {
		nano = -nano
	}
	s := strconv.FormatInt(nano/1e9, 10) + "." + strings.TrimRight(
		strconv.FormatInt(1e9+nano%1e9, 10)[1:], "0")
	s = strings.TrimSuffix(s, ".")
	if neg {
		s = "-" + s
	}
	return s
}
