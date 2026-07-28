package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testWebhookSecret signs webhook deliveries in these tests. A webhook without
// one is refused at construction, which is DESIGN §11.5 rule 1.
const testWebhookSecret = "test-webhook-signing-secret" // pragma: allowlist secret — test fixture

// clock is a settable clock. Both the test and the worker read it, so it is
// guarded.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newNotifier(t *testing.T, fns ...func(*Options)) (*Notifier, *clock) {
	t.Helper()
	ck := newClock()
	o := Options{
		Driver: DriverNone,
		From:   "dorang@example.com",
		To:     []string{"ops@example.com"},
		Now:    ck.now,
		Logf:   func(string, ...any) {},
		Retry: RetryOptions{
			MaxAttempts: 3, InitialBackoff: time.Millisecond,
			MaxBackoff: 2 * time.Millisecond, BreakerThreshold: 3,
			BreakerCooldown: time.Minute,
		},
	}
	for _, fn := range fns {
		fn(&o)
	}
	n, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n != nil {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = n.Close(ctx)
		})
	}
	return n, ck
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- disabled ---------------------------------------------------------------

func TestDriverNoneIsATypedNil(t *testing.T) {
	n, err := New(Options{Driver: DriverNone})
	if err != nil {
		t.Fatal(err)
	}
	if n != nil {
		t.Fatal("driver none must build no queue, no worker and no table")
	}
	if n.Enabled(EventBudget80) || n.Admit(EventBudget80, Subject{}, time.Now()) ||
		n.Send(Notification{}) || n.Notify(Notification{}) || n.Degraded() ||
		n.BreakerOpen() {
		t.Error("every method on a nil notifier must be a constant")
	}
	if n.Driver() != DriverNone {
		t.Error("a nil notifier reports the none driver")
	}
	if n.Stats() != (Stats{}) {
		t.Error("a nil notifier has no statistics")
	}
	if err := n.Close(context.Background()); err != nil {
		t.Error(err)
	}
}

// TestAdmitIsFreeOnTheRequestPath: DESIGN §11.5's "never on the request path"
// starts with the gate that *is* on it. Admit is one hash and one map
// operation, and it must not touch the allocator.
func TestAdmitIsFreeOnTheRequestPath(t *testing.T) {
	n, ck := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP.URL = "http://127.0.0.1:1/x"
		o.HTTP.Secret = testWebhookSecret // pragma: allowlist secret — test fixture
	})
	subj := Subject{Kind: "key", ID: "k-1"}
	at := ck.now()
	// The first call admits; every subsequent one is the suppressed path, which
	// is the one a request actually takes.
	n.Admit(EventBudget80, subj, at)
	if got := testing.AllocsPerRun(500, func() { n.Admit(EventBudget80, subj, at) }); got != 0 {
		t.Errorf("Admit allocated %v times, want 0", got)
	}

	var nilN *Notifier
	if got := testing.AllocsPerRun(500, func() { nilN.Admit(EventBudget80, subj, at) }); got != 0 {
		t.Errorf("Admit on a disabled notifier allocated %v times, want 0", got)
	}
}

// --- deduplication ----------------------------------------------------------

// TestOncePerSubjectPerPeriod is the difference between a useful alert and one
// everybody filters: budget_80pct is true on every request past the threshold,
// and must be sent once.
func TestOncePerSubjectPerPeriod(t *testing.T) {
	n, ck := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP.URL = "http://127.0.0.1:1/unused"
		o.HTTP.Secret = testWebhookSecret // pragma: allowlist secret — test fixture
		o.DedupPeriod = time.Hour
	})
	a := Subject{Kind: "key", ID: "k-a"}
	b := Subject{Kind: "key", ID: "k-b"}

	admitted := 0
	for i := 0; i < 1000; i++ {
		if n.Admit(EventBudget80, a, ck.now()) {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted %d alerts for one subject in one period, want 1", admitted)
	}

	// A different subject is a different alert.
	if !n.Admit(EventBudget80, b, ck.now()) {
		t.Fatal("a second subject must alert independently")
	}
	// A different event for the same subject is a different alert.
	if !n.Admit(EventBudgetExceeded, a, ck.now()) {
		t.Fatal("a second event must alert independently")
	}

	// The next period alerts again.
	ck.advance(time.Hour)
	if !n.Admit(EventBudget80, a, ck.now()) {
		t.Fatal("the next period must alert again")
	}
	if got := n.Stats().Suppressed; got != 999 {
		t.Errorf("suppressed = %d, want 999", got)
	}
}

// TestEventsWithoutDeduplicationAlwaysAdmit: key_created is unique per key by
// construction, and suppressing it would suppress it forever.
func TestEventsWithoutDeduplicationAlwaysAdmit(t *testing.T) {
	n, ck := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP.URL = "http://127.0.0.1:1/x"
		o.HTTP.Secret = testWebhookSecret // pragma: allowlist secret — test fixture
	})
	subj := Subject{Kind: "key", ID: "k-1"}
	for _, ev := range []Event{EventKeyCreated, EventBatchCompleted, EventInvite} {
		for i := 0; i < 3; i++ {
			if !n.Admit(ev, subj, ck.now()) {
				t.Fatalf("%s must not be deduplicated", ev)
			}
		}
	}
}

func TestDedupPeriodOverridePerEvent(t *testing.T) {
	n, ck := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP.URL = "http://127.0.0.1:1/x"
		o.HTTP.Secret = testWebhookSecret // pragma: allowlist secret — test fixture
		o.DedupPeriods = map[string]time.Duration{"key_created": time.Hour}
	})
	subj := Subject{Kind: "key", ID: "k-1"}
	if !n.Admit(EventKeyCreated, subj, ck.now()) {
		t.Fatal("first admits")
	}
	if n.Admit(EventKeyCreated, subj, ck.now()) {
		t.Fatal("the override must apply")
	}
}

func TestDedupTableIsBounded(t *testing.T) {
	d := newDeduper(defaultPeriods(time.Hour))
	d.cap = 4
	at := time.Unix(0, 0)
	for i := 0; i < 20000; i++ {
		d.admit(EventBudget80, Subject{Kind: "key", ID: string(rune('a'+i%26)) + strings.Repeat("x", i%40)}, at)
	}
	for i := range d.shards {
		if got := len(d.shards[i].m); got > d.cap {
			t.Fatalf("shard %d holds %d entries, over the cap %d", i, got, d.cap)
		}
	}
}

func TestUnknownEventNames(t *testing.T) {
	if _, err := New(Options{Driver: DriverHTTP, HTTP: HTTPOptions{URL: "http://x/y", Secret: testWebhookSecret}, // pragma: allowlist secret — test fixture
		Events: []string{"nope"}}); err == nil {
		t.Error("an unknown event name must be a configuration error")
	}
	if _, err := New(Options{Driver: DriverHTTP, HTTP: HTTPOptions{URL: "http://x/y", Secret: testWebhookSecret}, // pragma: allowlist secret — test fixture
		DedupPeriods: map[string]time.Duration{"nope": time.Hour}}); err == nil {
		t.Error("an unknown event name in dedup_periods must be a configuration error")
	}
}

func TestOnlyConfiguredEventsFire(t *testing.T) {
	n, ck := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP.URL = "http://127.0.0.1:1/x"
		o.HTTP.Secret = testWebhookSecret // pragma: allowlist secret — test fixture
		o.Events = []string{"budget_exceeded"}
	})
	if n.Enabled(EventBudget80) {
		t.Error("an unlisted event must be disabled")
	}
	if !n.Enabled(EventBudgetExceeded) {
		t.Error("a listed event must be enabled")
	}
	if n.Admit(EventBudget80, Subject{ID: "x"}, ck.now()) {
		t.Error("an unlisted event must not admit")
	}
	if n.Send(Notification{Event: EventBudget80}) {
		t.Error("an unlisted event must not queue")
	}
	if n.Enabled(Event(200)) {
		t.Error("an out-of-range event is not enabled")
	}
}

// --- drivers ----------------------------------------------------------------

func TestHTTPDriverDelivers(t *testing.T) {
	type got struct {
		body   []byte
		header http.Header
	}
	ch := make(chan got, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- got{body: b, header: r.Header.Clone()}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP = HTTPOptions{URL: srv.URL, Secret: testWebhookSecret, Client: srv.Client(), // pragma: allowlist secret — test fixture
			Headers: map[string]string{"X-Test": "1"}}
	})
	if !n.Notify(Notification{
		Event:   EventBudget80,
		Subject: Subject{Kind: "key", ID: "k-1"},
		Fields:  []Field{{"limit_usd", "10.00"}, {"spent_usd", "8.10"}, {"percent", "81"}},
	}) {
		t.Fatal("Notify was refused")
	}

	var g got
	select {
	case g = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook was never called")
	}
	if g.header.Get("X-Dorang-Event") != "budget_80pct" || g.header.Get("X-Test") != "1" {
		t.Errorf("headers = %v", g.header)
	}
	var w webhook
	if err := json.Unmarshal(g.body, &w); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if w.Event != "budget_80pct" || w.Subject != "key:k-1" {
		t.Errorf("payload = %+v", w)
	}
	if w.Fields["spent_usd"] != "8.10" {
		t.Errorf("fields = %v", w.Fields)
	}
	waitFor(t, "the delivery to be counted", func() bool { return n.Stats().Delivered == 1 })
}

func TestHTTPDriverCountsAnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP = HTTPOptions{URL: srv.URL, Secret: testWebhookSecret, Client: srv.Client()} // pragma: allowlist secret — test fixture
	})
	n.Notify(Notification{Event: EventInvite, Subject: Subject{Kind: "user", ID: "u"}})
	waitFor(t, "the drop to be counted", func() bool { return n.Stats().Dropped == 1 })
	if got := n.Stats().Failed; got != 3 {
		t.Errorf("failed attempts = %d, want the configured MaxAttempts of 3", got)
	}
	if !n.Degraded() {
		t.Error("a dropped notification must show as degraded")
	}
}

// --- the lua driver ---------------------------------------------------------

type fakeHook struct {
	mu       sync.Mutex
	seen     []*Message
	drop     bool
	deliver  bool
	err      error
	filtered int
}

func (h *fakeHook) OnEmail(_ context.Context, m *Message) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.filtered++
	h.seen = append(h.seen, m)
	return h.drop, h.err
}

func (h *fakeHook) CanDeliver() bool { return h.deliver }

func TestLuaDriverDeliversThroughTheHook(t *testing.T) {
	h := &fakeHook{deliver: true}
	n, _ := newNotifier(t, func(o *Options) { o.Driver = DriverLua; o.Hook = h })
	n.Notify(Notification{Event: EventKeyCreated, Subject: Subject{Kind: "key", ID: "k-1"}})
	waitFor(t, "the hook to deliver", func() bool { return n.Stats().Delivered == 1 })

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.seen) != 1 || h.seen[0].Event != EventKeyCreated {
		t.Fatalf("the hook saw %+v", h.seen)
	}
	if h.seen[0].Driver != DriverLua {
		t.Errorf("the message must name its driver, got %q", h.seen[0].Driver)
	}
}

func TestLuaDriverWithoutATransportSaysSo(t *testing.T) {
	var logged []string
	var mu sync.Mutex
	h := &fakeHook{deliver: false}
	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverLua
		o.Hook = h
		o.Logf = func(f string, a ...any) {
			mu.Lock()
			logged = append(logged, f)
			mu.Unlock()
		}
	})
	n.Notify(Notification{Event: EventInvite, Subject: Subject{ID: "u"}})
	// The ErrNoTransport branch counts the drop and logs the warning on the next
	// statement, so Dropped == 1 does not mean the line the assertion below
	// reads exists yet — one preemption in that window and `logged` is empty.
	// The warning is what is asserted on, so the warning is what is waited for;
	// it cannot appear without the drop that precedes it.
	waitFor(t, "the operator to be told", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(logged) > 0
	})
	if got := n.Stats().Dropped; got != 1 {
		t.Errorf("a missing transport is exactly one drop: dropped = %d", got)
	}
	if got := n.Stats().Failed; got != 0 {
		t.Errorf("a missing transport is a configuration error, not a retryable failure: failed = %d", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(logged, " "), "on_email") {
		t.Errorf("the operator must be told: %v", logged)
	}
}

// TestTheEmailHookFiltersEveryDriver: on_email is a hook, not only the lua
// driver's transport, so it gets to suppress an SMTP or webhook message too.
func TestTheEmailHookFiltersEveryDriver(t *testing.T) {
	hits := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
	}))
	defer srv.Close()

	h := &fakeHook{drop: true}
	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP = HTTPOptions{URL: srv.URL, Secret: testWebhookSecret, Client: srv.Client()} // pragma: allowlist secret — test fixture
		o.Hook = h
	})
	n.Notify(Notification{Event: EventInvite, Subject: Subject{ID: "u"}})
	waitFor(t, "the suppression to be counted", func() bool { return n.Stats().SuppressedByHook == 1 })
	select {
	case <-hits:
		t.Fatal("a suppressed message must not reach the transport")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAFailingEmailHookFailsOpen(t *testing.T) {
	hits := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
	}))
	defer srv.Close()

	h := &fakeHook{drop: true, err: context.DeadlineExceeded}
	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP = HTTPOptions{URL: srv.URL, Secret: testWebhookSecret, Client: srv.Client()} // pragma: allowlist secret — test fixture
		o.Hook = h
	})
	n.Notify(Notification{Event: EventInvite, Subject: Subject{ID: "u"}})
	select {
	case <-hits:
	case <-time.After(5 * time.Second):
		t.Fatal("a hook that errored must not swallow the alert")
	}
}

// --- smtp -------------------------------------------------------------------

// fakeSMTP is the smallest server net/smtp will talk to.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	msgs []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeSMTP) addr() string { return s.ln.Addr().String() }

func (s *fakeSMTP) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

func (s *fakeSMTP) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.session(c)
	}
}

func (s *fakeSMTP) session(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	write := func(str string) {
		w.WriteString(str)
		w.Flush()
	}
	write("220 fake ESMTP\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			write("250-fake greets you\r\n250 HELP\r\n")
		case strings.HasPrefix(cmd, "HELO"):
			write("250 fake\r\n")
		case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"),
			strings.HasPrefix(cmd, "RSET"), strings.HasPrefix(cmd, "NOOP"):
			write("250 OK\r\n")
		case strings.HasPrefix(cmd, "DATA"):
			write("354 go ahead\r\n")
			var body strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
				body.WriteString(l)
			}
			s.mu.Lock()
			s.msgs = append(s.msgs, body.String())
			s.mu.Unlock()
			write("250 queued\r\n")
		case strings.HasPrefix(cmd, "QUIT"):
			write("221 bye\r\n")
			return
		default:
			write("500 what\r\n")
		}
	}
}

func TestSMTPDriverDelivers(t *testing.T) {
	srv := newFakeSMTP(t)
	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverSMTP
		o.SMTP = SMTPOptions{Addr: srv.addr(), Timeout: 5 * time.Second}
	})
	n.Notify(Notification{
		Event:   EventCredentialUnhealthy,
		Subject: Subject{Kind: "credential", ID: "acct-1"},
		Fields:  []Field{{"provider", "plan-a"}, {"state", "open"}},
	})
	// Both, because neither implies the other. The fake server appends the
	// message from its own session goroutine while the worker is still inside
	// Deliver, and the worker counts the delivery only once Deliver has
	// returned — so waiting on the message and then reading Delivered catches
	// the worker mid-window and reads 0, and waiting on Delivered alone would
	// be a claim about the server's goroutine that nothing here establishes.
	waitFor(t, "the message to arrive and be counted", func() bool {
		return len(srv.messages()) == 1 && n.Stats().Delivered == 1
	})

	msg := srv.messages()[0]
	for _, want := range []string{
		"From: dorang@example.com",
		"To: ops@example.com",
		"X-Dorang-Event: credential_unhealthy",
		"provider: plan-a",
		"state: open",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not contain %q:\n%s", want, msg)
		}
	}
	if n.Stats().Delivered != 1 {
		t.Errorf("delivered = %d", n.Stats().Delivered)
	}
}

// TestDeadSMTPBacksOffAndCountsDrops is the "a failing mail server must not
// retry hard" requirement: bounded attempts, a bounded pause between them, a
// counted drop at the end, and a breaker that stops trying at all.
func TestDeadSMTPBacksOffAndCountsDrops(t *testing.T) {
	// A port nothing is listening on: bind it, learn the number, release it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	n, ck := newNotifier(t, func(o *Options) {
		o.Driver = DriverSMTP
		o.SMTP = SMTPOptions{Addr: dead, Timeout: 200 * time.Millisecond}
		o.Retry = RetryOptions{
			MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 4 * time.Millisecond,
			BreakerThreshold: 2, BreakerCooldown: time.Minute,
		}
	})

	start := time.Now()
	for i := 0; i < 5; i++ {
		n.Send(Notification{Event: EventBudgetExceeded, Subject: Subject{Kind: "key", ID: "k-1"}})
	}
	waitFor(t, "every notification to be resolved", func() bool { return n.Stats().Dropped == 5 })
	elapsed := time.Since(start)

	st := n.Stats()
	if st.Dropped != 5 {
		t.Errorf("dropped = %d, want 5", st.Dropped)
	}
	if st.Retried == 0 {
		t.Error("a failing transport must be retried at least once")
	}
	// Two messages exhaust the retry budget and open the breaker; the remaining
	// three are dropped without a connection attempt. Bounded, not hard.
	if st.BreakerDrops == 0 {
		t.Error("the breaker must stop the attempts")
	}
	if st.Failed >= 15 {
		t.Errorf("failed attempts = %d: every message was retried in full, so the "+
			"breaker did not engage", st.Failed)
	}
	if !n.BreakerOpen() {
		t.Error("the breaker should be open")
	}
	if elapsed > 30*time.Second {
		t.Errorf("the retries took %v", elapsed)
	}
	if !n.Degraded() {
		t.Error("dropped notifications must show as degraded")
	}

	// The breaker closes after its cooldown.
	ck.advance(2 * time.Minute)
	if n.BreakerOpen() {
		t.Error("the breaker must close after the cooldown")
	}
}

// --- the queue --------------------------------------------------------------

// TestFullQueueDropsVisibly is §9.6 rule 3: back-pressure from a deferred write
// must not reach the caller. It surfaces as a counted drop.
func TestFullQueueDropsVisibly(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP = HTTPOptions{URL: srv.URL, Secret: testWebhookSecret, Client: srv.Client(), Timeout: time.Minute} // pragma: allowlist secret — test fixture
		o.QueueSize = 2
	})

	var accepted, refused int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			if n.Send(Notification{Event: EventBatchCompleted,
				Subject: Subject{Kind: "batch", ID: "b"}}) {
				accepted++
			} else {
				refused++
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Send blocked; it must never block the caller")
	}
	if refused == 0 {
		t.Fatal("a bounded queue must refuse when it is full")
	}
	if n.Stats().QueueDrops == 0 {
		t.Error("a queue drop must be counted")
	}
	if !n.Degraded() {
		t.Error("queue drops must show as degraded")
	}
}

func TestCloseDrainsWhatIsQueued(t *testing.T) {
	srv := newFakeSMTP(t)
	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverSMTP
		o.SMTP = SMTPOptions{Addr: srv.addr(), Timeout: 5 * time.Second}
	})
	for i := 0; i < 5; i++ {
		n.Send(Notification{Event: EventBatchCompleted, Subject: Subject{Kind: "batch", ID: "b"}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(srv.messages()); got != 5 {
		t.Errorf("delivered %d of 5 queued messages on shutdown", got)
	}
	// Close is idempotent, and a closed notifier accepts nothing more.
	if err := n.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if n.Send(Notification{Event: EventInvite}) {
		t.Error("a closed notifier must not accept new notifications")
	}
}

// --- configuration ----------------------------------------------------------

func TestDriverConfigurationErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Options
	}{
		{"smtp without an address", Options{Driver: DriverSMTP, From: "a@b"}},
		{"smtp without a sender", Options{Driver: DriverSMTP, SMTP: SMTPOptions{Addr: "h:25"}}},
		{"http without a url", Options{Driver: DriverHTTP, HTTP: HTTPOptions{Secret: testWebhookSecret}}}, // pragma: allowlist secret — test fixture
		{"http without a signing secret", Options{Driver: DriverHTTP, HTTP: HTTPOptions{URL: "http://x/y"}}},
		{"an unknown driver", Options{Driver: DriverSMTP + "x"}},
	} {
		if _, err := New(tc.o); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

func TestEventNamesMatchTheDesign(t *testing.T) {
	// §11.5's seven, then the two §11.6 and §11.2c added. The order is
	// load-bearing — Event is an index into eventNames and into the enabled
	// mask — so this list is asserted position by position and new events are
	// appended, never inserted.
	want := []string{
		"key_created", "budget_80pct", "budget_exceeded", "quota_exhausted",
		"credential_unhealthy", "batch_completed", "invite",
		"token_guard", "key_rotated",
	}
	if len(Events()) != len(want) {
		t.Fatalf("Events() has %d entries, the design lists %d", len(Events()), len(want))
	}
	for i, name := range want {
		ev, ok := ParseEvent(name)
		if !ok || int(ev) != i {
			t.Errorf("ParseEvent(%q) = %v/%v", name, ev, ok)
		}
		if Event(i).String() != name {
			t.Errorf("Event(%d) = %q, want %q", i, Event(i).String(), name)
		}
		if Event(i).Title() == "" {
			t.Errorf("%s has no title", name)
		}
		if _, ok := EventFields[Event(i)]; !ok {
			t.Errorf("%s has no field allow-list", name)
		}
	}
	if _, ok := ParseEvent("nope"); ok {
		t.Error("ParseEvent accepted an unknown name")
	}
	if Event(99).String() != "unknown" || Event(99).Title() != "unknown" {
		t.Error("an out-of-range event should render as unknown")
	}
}

func TestSubjectRendering(t *testing.T) {
	for _, tc := range []struct {
		s    Subject
		want string
	}{
		{Subject{}, "gateway"},
		{Subject{Kind: "key"}, "key"},
		{Subject{ID: "k-1"}, "k-1"},
		{Subject{Kind: "key", ID: "k-1"}, "key:k-1"},
	} {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("%+v = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestSendCopiesTheCallersFields(t *testing.T) {
	srv := newFakeSMTP(t)
	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverSMTP
		o.SMTP = SMTPOptions{Addr: srv.addr(), Timeout: 5 * time.Second}
	})
	fields := []Field{{"provider", "plan-a"}}
	n.Send(Notification{Event: EventCredentialUnhealthy,
		Subject: Subject{Kind: "credential", ID: "c"}, Fields: fields})
	// The caller reuses its slice immediately, which it is entitled to do.
	fields[0] = Field{"provider", "MUTATED"}
	waitFor(t, "the message", func() bool { return len(srv.messages()) == 1 })
	if strings.Contains(srv.messages()[0], "MUTATED") {
		t.Error("Send must copy the caller's fields, not retain them")
	}
}
