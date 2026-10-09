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

func (m *memStore) AlertClear(_ context.Context, key string, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[key]
	if r == nil || !r.low {
		return false, nil
	}
	r.low, r.pending = false, nil
	return true, nil
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
		if r.low && len(r.pending) > 0 && !r.lastSent.After(before) {
			out = append(out, Pending{Key: k, Event: r.event, Seq: r.seq, Pending: r.pending, Payload: r.payload, LastSent: r.lastSent})
		}
	}
	return out, nil
}

func (m *memStore) AlertRetryClaim(_ context.Context, key string, seq int64, last, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[key]
	if r == nil || r.seq != seq || !r.lastSent.Equal(last) {
		return false, nil
	}
	r.lastSent = now
	return true, nil
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
	e := New(Options{Notifier: n, Store: st, Percent: 5, Remind: 6 * time.Hour,
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

func TestOneCrossingIsOneNotification(t *testing.T) {
	for name, st := range map[string]Store{"memory": nil, "store": &memStore{rows: map[string]*memRow{}}} {
		t.Run(name, func(t *testing.T) {
			e, sk, clock := setup(t, st)
			ctx := context.Background()
			e.Observe(ctx, cond(true))
			wait(t, func() bool { return sk.count() == 1 })
			for i := 0; i < 50; i++ {
				e.Observe(ctx, cond(true))
			}
			clock.set(clock.Add(time.Hour))
			e.Observe(ctx, cond(true))
			time.Sleep(30 * time.Millisecond)
			if sk.count() != 1 {
				t.Fatalf("%d notifications for one crossing", sk.count())
			}
			if !e.AnyLow() {
				t.Error("AnyLow is false with a condition low")
			}
			if f := sk.doc(0)["fields"].(map[string]any); f["state"] != "low" {
				t.Errorf("fields = %v", f)
			}

			// Still low past the reminder interval: one reminder.
			clock.set(clock.Add(6 * time.Hour))
			e.Observe(ctx, cond(true))
			wait(t, func() bool { return sk.count() == 2 })
			if f := sk.doc(1)["fields"].(map[string]any); f["state"] != "reminder" {
				t.Errorf("second notification state = %v", f["state"])
			}

			// Recovery re-arms; the next crossing is news at once.
			e.Observe(ctx, cond(false))
			if e.AnyLow() {
				t.Error("AnyLow after recovery")
			}
			e.Observe(ctx, cond(true))
			wait(t, func() bool { return sk.count() == 3 })
		})
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

func TestThresholds(t *testing.T) {
	e := New(Options{Notifier: &notify.Notifier{}, Percent: 5, FloorNano: 2_000_000_000})
	if low, pct := e.LowFraction(5, 100); !low || pct != 5 {
		t.Errorf("5 of 100 left: %v %v", low, pct)
	}
	if low, _ := e.LowFraction(5.01, 100); low {
		t.Error("5.01% left is low")
	}
	if low, _ := e.LowFraction(0, 0); low {
		t.Error("a zero ceiling is low")
	}
	// Balance: under the floor whatever the share; at the share whatever the floor.
	if low, _, _ := e.BalanceLow(1_900_000_000, 0); !low {
		t.Error("$1.90 under a $2 floor with no total is not low")
	}
	if low, pct, has := e.BalanceLow(19_339_677_412, 40_000_000_000); low || !has || pct < 48 || pct > 49 {
		t.Errorf("OpenRouter's 48%% left: %v %v %v", low, pct, has)
	}
	if low, _, _ := e.BalanceLow(30_000_000_000, 1_000_000_000_000); !low {
		t.Error("3% of a $1000 credit is not low")
	}
}

func TestUSD(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 1_500_000_000: "1.5", 340: "0.00000034", -2_000_000_000: "-2"} {
		if got := USD(n); got != want {
			t.Errorf("USD(%d) = %q, want %q", n, got, want)
		}
	}
}
