// Package alerts decides when a condition that can run out — a provider plan
// window, a vendor's spend limit or balance, a key's or team's budget, a plan
// that has stopped serving — is news, and hands the news to internal/notify.
//
// A condition is observed as low or not low, as often as its source sees it:
// on every request for a budget, after every usage-probe poll for a vendor
// figure. Observing is cheap — a map lookup — unless the observation changes
// something: a condition that has just become low, that has stayed low past
// the reminder interval, or that has recovered.
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
	AlertClear(ctx context.Context, key string, now time.Time) (bool, error)
	AlertDelivered(ctx context.Context, key string, seq int64, driver string, now time.Time) error
	AlertsPending(ctx context.Context, before time.Time, limit int) ([]Pending, error)
	AlertRetryClaim(ctx context.Context, key string, seq int64, last, now time.Time) (bool, error)
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
	// Remind is how long a condition stays low before it is announced again.
	Remind time.Duration
	// RetryAfter is how long an undelivered announcement waits before Sweep
	// sends it again to the transports that still owe it.
	RetryAfter time.Duration
	Now        func() time.Time
	Logf       func(string, ...any)
}

type cached struct {
	low      bool
	lastSent time.Time
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
	if o.Remind <= 0 {
		o.Remind = 6 * time.Hour
	}
	if o.RetryAfter <= 0 {
		o.RetryAfter = 5 * time.Minute
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

// LowFraction reports whether left of limit is at or under the threshold, and
// the percentage left. A non-positive limit is never low: there is nothing to
// be a share of.
func (e *Engine) LowFraction(left, limit float64) (bool, float64) {
	if e == nil || limit <= 0 {
		return false, 0
	}
	if left < 0 {
		left = 0
	}
	pct := left / limit * 100
	return pct <= e.o.Percent, pct
}

// BalanceLow reports whether a balance is low: under the absolute floor, or —
// when the provider states what was credited — at or under the threshold
// share of it.
func (e *Engine) BalanceLow(remainingNano, totalNano int64) (bool, float64, bool) {
	if e == nil {
		return false, 0, false
	}
	low := e.o.FloorNano > 0 && remainingNano < e.o.FloorNano
	if totalNano > 0 {
		l, pct := e.LowFraction(float64(remainingNano), float64(totalNano))
		return low || l, pct, true
	}
	return low, 0, false
}

// Observe records one observation and announces it when it is news. It does
// not block on delivery.
func (e *Engine) Observe(ctx context.Context, c Condition) {
	if e == nil || c.Key == "" {
		return
	}
	now := e.o.Now()
	e.mu.Lock()
	st, known := e.cache[c.Key]
	switch {
	case !c.Low && known && !st.low:
		e.mu.Unlock()
		return
	case c.Low && known && st.low && now.Sub(st.lastSent) < e.o.Remind:
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	if !c.Low {
		was := st.low
		if e.o.Store != nil {
			var err error
			if was, err = e.o.Store.AlertClear(ctx, c.Key, now); err != nil {
				e.o.Logf("alerts: %s: %v", c.Key, err)
				return
			}
		}
		e.set(c.Key, cached{})
		if was {
			e.o.Logf("alerts: %s recovered", c.Key)
		}
		return
	}

	state := "low"
	if known && st.low {
		state = "reminder"
	}
	fields := append(append([]notify.Field(nil), c.Fields...), notify.Field{Name: "state", Value: state})
	drivers := e.drivers()
	announce, seq := true, int64(0)
	if e.o.Store != nil {
		payload, _ := json.Marshal(payloadDoc{Kind: c.Subject.Kind, ID: c.Subject.ID, Fields: fields})
		var err error
		announce, seq, err = e.o.Store.AlertRaise(ctx, c.Key, c.Event.String(), now,
			now.Add(-e.o.Remind), strings.Join(drivers, ","), string(payload))
		if err != nil {
			e.o.Logf("alerts: %s: %v", c.Key, err)
			return
		}
	}
	// Whoever announced, this process now knows the condition is low and was
	// announced about now; it will not ask again before the reminder is due.
	e.set(c.Key, cached{low: true, lastSent: now})
	if !announce {
		return
	}
	e.send(c.Event, c.Key, seq, c.Subject, fields, nil)
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
		claimed, err := e.o.Store.AlertRetryClaim(ctx, p.Key, p.Seq, p.LastSent, now)
		if err != nil || !claimed {
			continue
		}
		e.send(ev, p.Key, p.Seq, notify.Subject{Kind: doc.Kind, ID: doc.ID}, doc.Fields, p.Pending)
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
