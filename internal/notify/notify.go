package notify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RetryOptions bounds how hard a failing transport is retried.
//
// DESIGN §11.5 does not specify these numbers; §9.6 specifies the shape they
// have to have. A notification is a deferred write, and rule 1 says no deferred
// write may be unbounded: so there is a maximum attempt count, a maximum
// backoff, and a breaker that stops attempting altogether while the transport
// is known to be down. Every path out of the retry loop ends in either a
// delivery or a counted drop.
type RetryOptions struct {
	// MaxAttempts is how many times one message is offered to the transport.
	// Default 3. One means no retry.
	MaxAttempts int
	// InitialBackoff is the pause after the first failure; it doubles up to
	// MaxBackoff. Default 1s / 30s.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// BreakerThreshold is how many consecutive failed messages open the
	// breaker. Default 5.
	BreakerThreshold int
	// BreakerCooldown is how long the breaker stays open. Default 1 minute.
	BreakerCooldown time.Duration
}

func (r *RetryOptions) setDefaults() {
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = 3
	}
	if r.InitialBackoff <= 0 {
		r.InitialBackoff = time.Second
	}
	if r.MaxBackoff <= 0 {
		r.MaxBackoff = 30 * time.Second
	}
	if r.BreakerThreshold <= 0 {
		r.BreakerThreshold = 5
	}
	if r.BreakerCooldown <= 0 {
		r.BreakerCooldown = time.Minute
	}
}

// Options configures [New]. It mirrors config.Notifications plus the resolved
// secret and the extension hook.
type Options struct {
	// Driver is smtp, http, lua or none.
	// Drivers lists every transport a notification goes out on, each with
	// its own queue, workers, retries and breaker, so a mail server that is
	// down never delays or blocks the webhook and a webhook retry never sends
	// the email twice. Driver is the single-transport spelling and the two are
	// exclusive.
	Drivers []string
	Driver  string
	// Events are the enabled event names. Empty enables all of them: `driver:
	// none` is the off switch, so an empty event list is far more likely to
	// mean "everything" than "nothing".
	Events []string

	// From is the envelope sender.
	From string
	// To are the default recipients.
	To []string

	SMTP SMTPOptions
	HTTP HTTPOptions

	// Hook is the on_email extension point. It filters every driver and is the
	// transport for the lua driver.
	Hook EmailHook

	// QueueSize bounds the queue. Default 256. Reaching it drops, visibly.
	QueueSize int
	// Workers deliver from the queue. Default 1: mail is not a throughput
	// problem, and one worker makes the backoff a queue-wide pause rather than
	// N transports hammering a server that is already failing.
	Workers int

	// DedupPeriod is the default deduplication window. Default 1h.
	DedupPeriod time.Duration
	// DedupPeriods overrides it per event name. A zero value disables
	// deduplication for that event.
	DedupPeriods map[string]time.Duration

	Retry RetryOptions

	Logf func(string, ...any)
	Now  func() time.Time
}

type stats struct {
	posted           atomic.Uint64
	delivered        atomic.Uint64
	failed           atomic.Uint64
	retried          atomic.Uint64
	dropped          atomic.Uint64
	queueDrops       atomic.Uint64
	breakerDrops     atomic.Uint64
	suppressedByHook atomic.Uint64
	fieldsDropped    atomic.Uint64
	redacted         atomic.Uint64
}

// Stats is a snapshot of what the notifier has done. Every number that
// represents a lost notification is here, because §9.6 rule 1 requires the loss
// to be visible rather than inferred.
type Stats struct {
	// Posted is how many notifications were accepted into the queue.
	Posted uint64
	// Suppressed is how many were deduplicated away at admission.
	Suppressed uint64
	// Delivered is how many reached a transport successfully.
	Delivered uint64
	// Failed counts individual delivery attempts that errored, retries
	// included.
	Failed uint64
	// Retried is how many attempts were made after the first.
	Retried uint64
	// Dropped is how many notifications were lost: a full queue, an exhausted
	// retry budget, or an open breaker.
	Dropped uint64
	// QueueDrops and BreakerDrops break Dropped down by cause.
	QueueDrops   uint64
	BreakerDrops uint64
	// SuppressedByHook is how many the on_email extension refused.
	SuppressedByHook uint64
	// FieldsDropped is how many fields were removed for not being on their
	// event's allow-list.
	FieldsDropped uint64
	// Redacted is how many field values looked like key material.
	Redacted uint64
	// DedupEvictions is how many deduplication entries were forgotten to keep
	// the table bounded. A forgotten subject may alert twice in a period.
	DedupEvictions uint64
	// QueueDepth is the current queue occupancy.
	QueueDepth uint64
}

// Notifier is the notification pipeline.
//
// A nil *Notifier is the disabled notifier: every method is a constant and
// costs one nil check, which is how a `driver: none` deployment pays nothing
// for the mechanism.
type Notifier struct {
	opts     Options
	mask     uint16
	channels []*channel
	hook     EmailHook
	dedup    *deduper
	to       []string

	stop    chan struct{}
	closing atomic.Bool
	workers sync.WaitGroup

	ctx    context.Context
	cancel context.CancelFunc

	lastWarn atomic.Int64

	st stats

	now       func() time.Time
	logf      func(string, ...any)
	closeOnce sync.Once
}

// job is one queued notification. Its slices are owned by the job so the
// caller's Fields may be reused the moment Send returns.
// channel is one transport: its driver, its queue and its breaker.
type channel struct {
	drv Driver
	q   chan *job

	breakerMu    sync.Mutex
	breakerFails int
	breakerUntil time.Time
}

type job struct {
	n      Notification
	fields []Field
	to     []string
}

var jobPool = sync.Pool{New: func() any { return &job{} }}

func getJob() *job { return jobPool.Get().(*job) }

func putJob(j *job) {
	j.n = Notification{}
	j.fields = j.fields[:0]
	j.to = j.to[:0]
	jobPool.Put(j)
}

// New builds a notifier.
//
// It returns (nil, nil) for `driver: none`, so a deployment that has not
// configured notifications holds a typed nil rather than a queue, a worker and
// a deduplication table it will never use.
func New(opts Options) (*Notifier, error) {
	names, err := driverNames(opts.Driver, opts.Drivers)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, nil
	}
	opts.Driver, opts.Drivers = strings.Join(names, ","), names
	opts.Retry.setDefaults()
	if opts.QueueSize <= 0 {
		opts.QueueSize = 256
	}
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if opts.DedupPeriod <= 0 {
		opts.DedupPeriod = time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	channels := make([]*channel, 0, len(names))
	for _, name := range names {
		drv, err := newDriver(&opts, name)
		if err != nil {
			return nil, err
		}
		channels = append(channels, &channel{drv: drv, q: make(chan *job, opts.QueueSize)})
	}

	mask, err := eventMask(opts.Events)
	if err != nil {
		return nil, err
	}

	periods := defaultPeriods(opts.DedupPeriod)
	for name, d := range opts.DedupPeriods {
		ev, ok := ParseEvent(name)
		if !ok {
			return nil, fmt.Errorf("notify: %q is not a notification event", name)
		}
		periods[ev] = d
	}

	n := &Notifier{
		opts:     opts,
		mask:     mask,
		channels: channels,
		hook:     opts.Hook,
		dedup:    newDeduper(periods),
		to:       append([]string(nil), opts.To...),
		stop:     make(chan struct{}),
		now:      opts.Now,
		logf:     opts.Logf,
	}
	n.ctx, n.cancel = context.WithCancel(context.Background())

	for _, ch := range channels {
		n.workers.Add(opts.Workers)
		for i := 0; i < opts.Workers; i++ {
			go n.run(ch)
		}
	}
	return n, nil
}

func eventMask(names []string) (uint16, error) {
	if len(names) == 0 {
		return 1<<numEvents - 1, nil
	}
	var m uint16
	for _, name := range names {
		ev, ok := ParseEvent(name)
		if !ok {
			return 0, fmt.Errorf("notify: %q is not a notification event; want one of %v",
				name, eventNames)
		}
		m |= 1 << uint(ev)
	}
	return m, nil
}

// Enabled reports whether an event is configured to notify.
func (n *Notifier) Enabled(ev Event) bool {
	if n == nil || int(ev) >= numEvents {
		return false
	}
	return n.mask&(1<<uint(ev)) != 0
}

// Driver returns the configured transport's name, "none" when disabled.
func (n *Notifier) Driver() string {
	if n == nil {
		return DriverNone
	}
	names := make([]string, len(n.channels))
	for i, ch := range n.channels {
		names[i] = ch.drv.Name()
	}
	return strings.Join(names, ",")
}

// driverNames resolves the single and the list spelling into the transports
// to build. "none" alone is no transport; beside another it is refused.
func driverNames(single string, list []string) ([]string, error) {
	single = strings.ToLower(strings.TrimSpace(single))
	if single != "" && len(list) > 0 {
		return nil, errors.New("notify: driver and drivers are exclusive; list every transport under drivers")
	}
	if single != "" {
		list = []string{single}
	}
	var out []string
	seen := map[string]bool{}
	for _, d := range list {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || d == DriverNone {
			if len(list) > 1 {
				return nil, errors.New("notify: drivers lists none beside a transport")
			}
			continue
		}
		if seen[d] {
			return nil, fmt.Errorf("notify: drivers lists %q twice", d)
		}
		seen[d] = true
		out = append(out, d)
	}
	return out, nil
}

// Admit reserves this subject's alert slot for the period containing at.
//
// This is the request-path half of the API and the only part of this package a
// hot path may call. It allocates nothing and takes a 1/64th shard lock for the
// duration of one map operation. It returns true at most once per subject per
// period, so a caller writes:
//
//	if nf.Admit(notify.EventBudget80, subj, now) {
//	    nf.Send(notify.Notification{...})   // builds the fields only now
//	}
//
// Building the notification behind the gate is the point: budget_80pct is true
// on every request past the threshold, and the allocation that renders it must
// happen once per period rather than once per request.
func (n *Notifier) Admit(ev Event, subj Subject, at time.Time) bool {
	if n == nil || n.closing.Load() || !n.Enabled(ev) {
		return false
	}
	if at.IsZero() {
		at = n.now()
	}
	return n.dedup.admit(ev, subj, at)
}

// Send queues a notification without deduplicating it.
//
// It never blocks: a full queue is a counted drop, because §9.6 rule 3 is that
// back-pressure from a deferred write must not reach the request path.
func (n *Notifier) Send(no Notification) bool {
	if n == nil || n.closing.Load() || !n.Enabled(no.Event) {
		return false
	}
	if no.At.IsZero() {
		no.At = n.now()
	}
	// One job per transport, each on its own queue: a transport whose queue is
	// full drops its copy and the others still go.
	queued := false
	for _, ch := range n.channels {
		if len(no.Only) > 0 && !slices.Contains(no.Only, ch.drv.Name()) {
			continue
		}
		j := getJob()
		j.fields = append(j.fields[:0], no.Fields...)
		j.to = append(j.to[:0], no.To...)
		j.n = no
		j.n.Fields = j.fields
		j.n.To = j.to

		select {
		case ch.q <- j:
			queued = true
		default:
			putJob(j)
			n.st.dropped.Add(1)
			n.st.queueDrops.Add(1)
			n.warn("notify: the %s queue is full; a %s notification for %s was dropped",
				ch.drv.Name(), no.Event, no.Subject)
		}
	}
	if queued {
		n.st.posted.Add(1)
	}
	return queued
}

// Notify deduplicates and queues in one call. It is the form for callers that
// are not on the request path.
func (n *Notifier) Notify(no Notification) bool {
	if n == nil {
		return false
	}
	if !n.Admit(no.Event, no.Subject, no.At) {
		return false
	}
	return n.Send(no)
}

// Stats returns a snapshot.
func (n *Notifier) Stats() Stats {
	if n == nil {
		return Stats{}
	}
	return Stats{
		Posted:           n.st.posted.Load(),
		Suppressed:       n.dedup.suppressed.Load(),
		Delivered:        n.st.delivered.Load(),
		Failed:           n.st.failed.Load(),
		Retried:          n.st.retried.Load(),
		Dropped:          n.st.dropped.Load(),
		QueueDrops:       n.st.queueDrops.Load(),
		BreakerDrops:     n.st.breakerDrops.Load(),
		SuppressedByHook: n.st.suppressedByHook.Load(),
		FieldsDropped:    n.st.fieldsDropped.Load(),
		Redacted:         n.st.redacted.Load(),
		DedupEvictions:   n.dedup.evictions.Load(),
		QueueDepth:       n.queueDepth(),
	}
}

// Degraded reports whether notifications are being lost. It is the health
// surface's question, and the answer is never inferred from a log line.
func (n *Notifier) queueDepth() uint64 {
	var d uint64
	for _, ch := range n.channels {
		d += uint64(len(ch.q))
	}
	return d
}

func (n *Notifier) Degraded() bool {
	if n == nil {
		return false
	}
	return n.st.dropped.Load() > 0
}

// Close stops the workers, draining what is already queued until ctx expires.
func (n *Notifier) Close(ctx context.Context) error {
	if n == nil {
		return nil
	}
	var err error
	n.closeOnce.Do(func() {
		n.closing.Store(true)
		close(n.stop)

		done := make(chan struct{})
		go func() {
			n.workers.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			// The grace period is over. Cancelling the shared context is what
			// frees a worker parked in a backoff or waiting on a transport
			// that does not answer.
			n.cancel()
			<-done
			err = ctx.Err()
		}
		n.cancel()
	})
	return err
}

// --- the worker -------------------------------------------------------------

func (n *Notifier) run(ch *channel) {
	defer n.workers.Done()
	for {
		select {
		case j := <-ch.q:
			n.deliver(ch, j)
			putJob(j)
		case <-n.stop:
			for {
				select {
				case j := <-ch.q:
					n.deliver(ch, j)
					putJob(j)
				default:
					return
				}
			}
		}
	}
}

func (n *Notifier) deliver(ch *channel, j *job) {
	to := j.n.To
	if len(to) == 0 {
		to = n.to
	}
	m, rs := render(&j.n, n.opts.From, to, ch.drv.Name(), j.n.At)
	if rs.dropped > 0 {
		n.st.fieldsDropped.Add(uint64(rs.dropped))
	}
	if rs.redacted > 0 {
		n.st.redacted.Add(uint64(rs.redacted))
	}

	// The on_email hook filters every driver. For the lua driver it is also the
	// transport, so running it here as well would run it twice.
	if n.hook != nil && ch.drv.Name() != DriverLua {
		dropped, err := n.hook.OnEmail(n.ctx, m)
		if err != nil {
			// A filter that fails must not swallow an alert: fail open, as
			// DESIGN §11.5 requires of every hook that is not an explicit deny.
			n.warn("notify: the on_email hook failed and was ignored: %v", err)
		} else if dropped {
			n.st.suppressedByHook.Add(1)
			j.n.delivered(ch.drv.Name())
			return
		}
	}

	if ch.breakerOpen(n.now()) {
		n.st.dropped.Add(1)
		n.st.breakerDrops.Add(1)
		return
	}

	var lastErr error
	for attempt := 0; attempt < n.opts.Retry.MaxAttempts; attempt++ {
		if attempt > 0 {
			n.st.retried.Add(1)
		}
		err := ch.drv.Deliver(n.ctx, m)
		switch {
		case err == nil:
			n.st.delivered.Add(1)
			ch.onDeliverySucceeded()
			j.n.delivered(ch.drv.Name())
			return
		case errors.Is(err, errDropped):
			n.st.suppressedByHook.Add(1)
			ch.onDeliverySucceeded()
			j.n.delivered(ch.drv.Name())
			return
		case errors.Is(err, ErrNoTransport):
			// Retrying will not conjure a transport. This is a configuration
			// error and it is reported as one rather than as a flaky mailer.
			n.st.dropped.Add(1)
			n.warn("notify: %v: configure a native on_email transport or change "+
				"notifications.email.driver", err)
			return
		}
		n.st.failed.Add(1)
		lastErr = err
		if attempt+1 >= n.opts.Retry.MaxAttempts {
			break
		}
		if !n.sleep(n.backoff(attempt)) {
			break
		}
	}

	n.st.dropped.Add(1)
	if ch.onDeliveryFailed(n.now(), n.opts.Retry) {
		n.warn("notify: %s delivery has failed %d times; pausing for %v",
			ch.drv.Name(), n.opts.Retry.BreakerThreshold, n.opts.Retry.BreakerCooldown)
	}
	n.warn("notify: a %s notification for %s was dropped after %d %s attempts: %v",
		m.Event, m.Subject, n.opts.Retry.MaxAttempts, ch.drv.Name(), lastErr)
}

// backoff is exponential from InitialBackoff to MaxBackoff, jittered so that a
// cluster of nodes recovering from the same outage does not reconnect in step.
func (n *Notifier) backoff(attempt int) time.Duration {
	d := n.opts.Retry.InitialBackoff
	for i := 0; i < attempt && d < n.opts.Retry.MaxBackoff; i++ {
		d *= 2
	}
	if d > n.opts.Retry.MaxBackoff {
		d = n.opts.Retry.MaxBackoff
	}
	// ±12.5%, taken from the clock rather than from a random source: this needs
	// to be spread, not unpredictable, and a package-level RNG is a dependency
	// on global state for no benefit.
	jitter := (n.now().UnixNano() & 0xff) - 0x80 // -128..127
	d += time.Duration(int64(d) / 1024 * jitter)
	if d < 0 {
		d = n.opts.Retry.InitialBackoff
	}
	return d
}

// sleep pauses, returning false when the notifier is shutting down.
func (n *Notifier) sleep(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-n.ctx.Done():
		return false
	}
}

// --- the breaker ------------------------------------------------------------

func (ch *channel) breakerOpen(now time.Time) bool {
	ch.breakerMu.Lock()
	defer ch.breakerMu.Unlock()
	if ch.breakerUntil.IsZero() {
		return false
	}
	if now.Before(ch.breakerUntil) {
		return true
	}
	ch.breakerUntil = time.Time{}
	return false
}

func (ch *channel) onDeliverySucceeded() {
	ch.breakerMu.Lock()
	ch.breakerFails = 0
	ch.breakerUntil = time.Time{}
	ch.breakerMu.Unlock()
}

// onDeliveryFailed counts a dropped notification and reports whether that
// opened the breaker.
func (ch *channel) onDeliveryFailed(now time.Time, r RetryOptions) bool {
	ch.breakerMu.Lock()
	defer ch.breakerMu.Unlock()
	ch.breakerFails++
	if ch.breakerFails >= r.BreakerThreshold {
		ch.breakerUntil = now.Add(r.BreakerCooldown)
		return true
	}
	return false
}

// BreakerOpen reports whether any transport is pausing delivery.
func (n *Notifier) BreakerOpen() bool {
	if n == nil {
		return false
	}
	now := n.now()
	for _, ch := range n.channels {
		ch.breakerMu.Lock()
		open := !ch.breakerUntil.IsZero() && now.Before(ch.breakerUntil)
		ch.breakerMu.Unlock()
		if open {
			return true
		}
	}
	return false
}

func (n *Notifier) warn(format string, args ...any) {
	now := n.now().UnixNano()
	last := n.lastWarn.Load()
	if now-last < int64(time.Second) {
		return
	}
	if !n.lastWarn.CompareAndSwap(last, now) {
		return
	}
	n.logf(format, args...)
}
