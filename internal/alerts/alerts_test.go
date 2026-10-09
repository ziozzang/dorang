package alerts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/notify"
)

// memStore is alert_state's semantics in a map, for the engine's tests; the
// store's own tests run the SQL against both dialects.
type memStore struct {
	mu   sync.Mutex
	rows map[string]*memRow
	// failRaise makes the next raise fail, as a store outage would.
	delivered []string
}

type memRow struct {
	low      bool
	lastSent time.Time
	seq      int64
	pending  []string
	payload  string
	event    string
}

func (m *memStore) AlertRaise(_ context.Context, key, event string, now, remindBefore time.Time,
	pending, payload string) (bool, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[key]
	if r == nil {
		m.rows[key] = &memRow{low: true, lastSent: now, seq: 1, pending: strings.Split(pending, ","), payload: payload, event: event}
		return true, 1, nil
	}
	if r.low && r.lastSent.After(remindBefore) {
		return false, 0, nil
	}
	r.low, r.lastSent, r.seq, r.pending, r.payload = true, now, r.seq+1, strings.Split(pending, ","), payload
	return true, r.seq, nil
}

func (m *memStore) AlertResolve(_ context.Context, key string, now time.Time, pending, payload string) (bool, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[key]
	if r == nil || !r.low {
		return false, 0, nil
	}
	r.low, r.lastSent, r.seq, r.pending, r.payload = false, now, r.seq+1, strings.Split(pending, ","), payload
	return true, r.seq, nil
}

func (m *memStore) AlertDelivered(_ context.Context, key string, seq int64, driver string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delivered = append(m.delivered, driver)
	if r := m.rows[key]; r != nil && r.seq == seq {
		var keep []string
		for _, p := range r.pending {
			if p != driver {
				keep = append(keep, p)
			}
		}
		r.pending = keep
	}
	return nil
}

func (m *memStore) AlertsPending(_ context.Context, before time.Time, _ int) ([]Pending, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Pending
	for k, r := range m.rows {
		if len(r.pending) > 0 && !r.lastSent.After(before) {
			out = append(out, Pending{Key: k, Event: r.event, Seq: r.seq, Pending: r.pending, Payload: r.payload, LastSent: r.lastSent})
		}
	}
	return out, nil
}

func (m *memStore) AlertRetryClaim(_ context.Context, key string, seq int64, last, now time.Time) ([]string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[key]
	if r == nil || r.seq != seq || !r.lastSent.Equal(last) || len(r.pending) == 0 {
		return nil, false, nil
	}
	r.lastSent = now
	return append([]string(nil), r.pending...), true, nil
}

func (m *memStore) AlertsActive(_ context.Context, _ int) ([]Active, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Active
	for k, r := range m.rows {
		if r.low {
			out = append(out, Active{Key: k, LastSent: r.lastSent})
		}
	}
	return out, nil
}

type sink struct {
	mu   sync.Mutex
	docs []map[string]any
}

func (s *sink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.docs) }

func (s *sink) doc(i int) map[string]any { s.mu.Lock(); defer s.mu.Unlock(); return s.docs[i] }

// testClock is read by delivery workers while the test moves it.
type testClock struct{ ns atomic.Int64 }

func (c *testClock) now() time.Time                { return time.Unix(0, c.ns.Load()).UTC() }
func (c *testClock) Add(d time.Duration) time.Time { return c.now().Add(d) }
func (c *testClock) set(t time.Time)               { c.ns.Store(t.UnixNano()) }

func setup(t *testing.T, st Store) (*Engine, *sink, *testClock) {
	return setupRemind(t, st, 0)
}

func setupRemind(t *testing.T, st Store, remind time.Duration) (*Engine, *sink, *testClock) {
	t.Helper()
	sk := &sink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var d map[string]any
		_ = json.Unmarshal(b, &d)
		sk.mu.Lock()
		sk.docs = append(sk.docs, d)
		sk.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	clock := &testClock{}
	clock.set(time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC))
	n, err := notify.New(notify.Options{
		Driver: notify.DriverHTTP, HTTP: notify.HTTPOptions{URL: srv.URL, Secret: "s3cret-signing"},
		Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close(context.Background()) })
	e := New(Options{Notifier: n, Store: st, Percent: 5, Remind: remind,
		RetryAfter: time.Minute, Now: clock.now})
	return e, sk, clock
}

func wait(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func cond(low bool) Condition {
	return Condition{Event: notify.EventRemainingLow, Key: "k", Low: low,
		Subject: notify.Subject{Kind: "credential", ID: "c1"},
		Fields:  []notify.Field{{Name: "percent_left", Value: "4.00"}}}
}

// TestEachTransitionIsOneNotification: a problem is announced when it starts
// and its recovery when it ends, once each, and nothing while the state holds —
// not after an hour, not after a day.
func TestEachTransitionIsOneNotification(t *testing.T) {
	for name, st := range map[string]Store{"memory": nil, "store": &memStore{rows: map[string]*memRow{}}} {
		t.Run(name, func(t *testing.T) {
			e, sk, clock := setup(t, st)
			ctx := context.Background()
			e.Observe(ctx, cond(true))
			wait(t, func() bool { return sk.count() == 1 })
			for _, d := range []time.Duration{time.Minute, time.Hour, 24 * time.Hour} {
				clock.set(clock.Add(d))
				e.Observe(ctx, cond(true))
			}
			time.Sleep(30 * time.Millisecond)
			if sk.count() != 1 {
				t.Fatalf("%d notifications while the problem held", sk.count())
			}
			if f := sk.doc(0)["fields"].(map[string]any); f["state"] != "low" {
				t.Errorf("fields = %v", f)
			}

			e.Observe(ctx, cond(false))
			wait(t, func() bool { return sk.count() == 2 })
			if f := sk.doc(1)["fields"].(map[string]any); f["state"] != "recovered" {
				t.Errorf("recovery state = %v", f["state"])
			}
			if line, _ := sk.doc(1)["subject_line"].(string); !strings.Contains(line, "recovered") {
				t.Errorf("recovery subject line = %q", line)
			}
			for i := 0; i < 5; i++ {
				e.Observe(ctx, cond(false))
			}
			if e.AnyLow() {
				t.Error("AnyLow after recovery")
			}
			e.Observe(ctx, cond(true))
			wait(t, func() bool { return sk.count() == 3 })
			time.Sleep(30 * time.Millisecond)
			if sk.count() != 3 {
				t.Fatalf("%d notifications for three transitions", sk.count())
			}
		})
	}
}

func TestARecoveryWithoutAProblemIsNotNews(t *testing.T) {
	e, sk, _ := setup(t, &memStore{rows: map[string]*memRow{}})
	e.Observe(context.Background(), cond(false))
	time.Sleep(30 * time.Millisecond)
	if sk.count() != 0 {
		t.Fatal("announced a recovery from nothing")
	}
}

func TestARemindIsOnlyWhenConfigured(t *testing.T) {
	e, sk, clock := setupRemind(t, &memStore{rows: map[string]*memRow{}}, 6*time.Hour)
	ctx := context.Background()
	e.Observe(ctx, cond(true))
	wait(t, func() bool { return sk.count() == 1 })
	clock.set(clock.Add(6*time.Hour + time.Minute))
	e.Observe(ctx, cond(true))
	wait(t, func() bool { return sk.count() == 2 })
	if f := sk.doc(1)["fields"].(map[string]any); f["state"] != "reminder" {
		t.Errorf("state = %v", f["state"])
	}
}

// TestAnotherNodesAnnouncementIsNotRepeated: the store already holds a fresh
// announcement (another node, or this process before a restart), so this
// engine stays quiet.
func TestAnotherNodesAnnouncementIsNotRepeated(t *testing.T) {
	st := &memStore{rows: map[string]*memRow{}}
	e, sk, clock := setup(t, st)
	st.rows["k"] = &memRow{low: true, lastSent: clock.Add(-time.Minute), seq: 4}
	e.Observe(context.Background(), cond(true))
	time.Sleep(30 * time.Millisecond)
	if sk.count() != 0 {
		t.Fatalf("re-announced what another node announced a minute ago")
	}
}

func TestDeliveryIsRecordedAndSweepRetriesOnlyTheOwedTransport(t *testing.T) {
	st := &memStore{rows: map[string]*memRow{}}
	e, sk, clock := setup(t, st)
	ctx := context.Background()
	e.Observe(ctx, cond(true))
	wait(t, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.delivered) == 1
	})
	st.mu.Lock()
	p := st.rows["k"].pending
	st.mu.Unlock()
	if len(p) != 0 {
		t.Fatalf("pending after delivery = %v", p)
	}
	// A transport that never confirmed: the sweep sends to it alone.
	st.mu.Lock()
	st.rows["k"].pending = []string{"http"}
	st.mu.Unlock()
	clock.set(clock.Add(2 * time.Minute))
	e.Sweep(ctx)
	wait(t, func() bool { return sk.count() == 2 })
	e.Sweep(ctx) // claimed already: nothing more
	time.Sleep(30 * time.Millisecond)
	if sk.count() != 2 {
		t.Errorf("a retry was sent %d times", sk.count()-1)
	}
}

// TestJudgeHasHysteresis: low at 5%, recovered only above 10%, and nothing in
// between, so a figure hovering at the line cannot flap.
func TestJudgeHasHysteresis(t *testing.T) {
	e := New(Options{Notifier: &notify.Notifier{}, Percent: 5, FloorNano: 2_000_000_000})
	for _, tc := range []struct {
		left float64
		want Verdict
	}{{5, Low}, {0, Low}, {5.01, Hold}, {10, Hold}, {10.01, OK}, {100, OK}} {
		if v, _ := e.Judge(tc.left, 100); v != tc.want {
			t.Errorf("%v%% left: %v, want %v", tc.left, v, tc.want)
		}
	}
	if v, _ := e.Judge(0, 0); v != OK {
		t.Error("a zero ceiling is not OK")
	}
	for _, tc := range []struct {
		rem, total int64
		want       Verdict
	}{
		{1_900_000_000, 0, Low},                   // under the $2 floor, no total
		{2_500_000_000, 0, Hold},                  // above the floor, inside half again
		{3_000_000_000, 0, OK},                    // clear of it
		{19_339_677_412, 40_000_000_000, OK},      // OpenRouter's 48% left
		{30_000_000_000, 1_000_000_000_000, Low},  // 3% of $1000
		{80_000_000_000, 1_000_000_000_000, Hold}, // 8%: inside the band
	} {
		if v, _, _ := e.JudgeBalance(tc.rem, tc.total); v != tc.want {
			t.Errorf("balance %d of %d: %v, want %v", tc.rem, tc.total, v, tc.want)
		}
	}
}

func TestUSD(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 1_500_000_000: "1.5", 340: "0.00000034", -2_000_000_000: "-2"} {
		if got := USD(n); got != want {
			t.Errorf("USD(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestAnotherNodesRecoveryIsSeenAfterRecheck: this process cached the
// condition as low; another node cleared it and it crossed again. After
// Recheck this process asks the store, which lets it announce the new crossing
// instead of waiting out the reminder.
func TestAnotherNodesRecoveryIsSeenAfterRecheck(t *testing.T) {
	st := &memStore{rows: map[string]*memRow{}}
	e, sk, clock := setup(t, st)
	ctx := context.Background()
	e.Observe(ctx, cond(true))
	wait(t, func() bool { return sk.count() == 1 })
	st.mu.Lock()
	st.rows["k"].low = false // another node saw the recovery
	st.mu.Unlock()
	clock.set(clock.Add(30 * time.Second))
	e.Observe(ctx, cond(true)) // inside Recheck: trusted cache, quiet
	time.Sleep(30 * time.Millisecond)
	if sk.count() != 1 {
		t.Fatal("asked the store inside Recheck")
	}
	clock.set(clock.Add(time.Minute))
	e.Observe(ctx, cond(true))
	wait(t, func() bool { return sk.count() == 2 })
}

// TestLoadPrimesConditionsAfterARestart: a fresh process learns what is low,
// so its first observation of a recovery clears the row.
func TestLoadPrimesConditionsAfterARestart(t *testing.T) {
	st := &memStore{rows: map[string]*memRow{"k": {low: true, seq: 1}}}
	e, _, _ := setup(t, st)
	if err := e.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.AnyLow() {
		t.Fatal("a restarted process does not know the condition is low")
	}
	e.Observe(context.Background(), cond(false))
	if st.rows["k"].low || e.AnyLow() {
		t.Error("the recovery did not clear the stored condition")
	}
}
