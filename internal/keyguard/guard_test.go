package keyguard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/auth"
	"github.com/ziozzang/dorang/internal/notify"
)

// DESIGN §11.6. The four things the guard has to get right, each because an
// automated revocation is itself a denial of service — and §11.2c's rider: the
// guard's tests assert the key STOPS SERVING, not that a flag was set.

const (
	testPepper = "guard-test-pepper-not-a-real-secret" // pragma: allowlist secret — test fixture
	testMaster = "sk-guard-test-master"                // pragma: allowlist secret — test fixture
	guardedTok = "sk-guard-subject"                    // pragma: allowlist secret — test fixture
	guardedKey = "key-under-guard"
)

var epoch = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// authEnforcer is the enforcer under test: it changes durable state AND drops
// the key from the authenticator, which is the pair §11.2c requires. Anything
// that did only the first half would pass a flag test and fail every test here.
type authEnforcer struct {
	a *auth.Authenticator

	mu     sync.Mutex
	pended map[string]bool
	reason map[string]string
	block  map[string]bool
	// rows is the "store": what a snapshot reload would find.
	rows map[string]auth.Record
	now  func() time.Time
}

func newAuthEnforcer(t *testing.T, now func() time.Time, sink auth.Sink) (*authEnforcer, *auth.Authenticator) {
	t.Helper()
	e := &authEnforcer{
		pended: map[string]bool{}, reason: map[string]string{}, block: map[string]bool{},
		rows: map[string]auth.Record{}, now: now,
	}
	// The enforcer is also the store, which is what production looks like: the
	// invalidation drops the cached copy and the next request re-reads the
	// durable row. A harness with no store behind it would report a dropped
	// entry as an unknown key and would prove nothing about the pend.
	a, err := auth.New(auth.Config{
		Pepper: testPepper, MasterKey: testMaster, Now: now, Sink: sink, Store: e,
		// A long entry TTL on purpose. If the pend were relying on the TTL to
		// take effect, an hour is how long these tests would have to wait.
		EntryTTL: time.Hour, NegativeTTL: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	e.a = a
	return e, a
}

// LoadByLookup implements auth.Store over the enforcer's rows.
func (e *authEnforcer) LoadByLookup(_ context.Context, lookup string) (auth.Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, r := range e.rows {
		if r.Lookup != lookup {
			continue
		}
		r.Principal.Key.Pended = e.pended[id]
		r.Principal.Key.PendReason = e.reason[id]
		r.Principal.Key.Blocked = e.block[id]
		return r, nil
	}
	return auth.Record{}, auth.ErrNotFound
}

func (e *authEnforcer) issue(t *testing.T, token, keyID string) {
	t.Helper()
	h, err := auth.NewHasher(testPepper, auth.LegacyPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := h.Hash(auth.SchemeDorangV1, token)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.rows[keyID] = auth.Record{
		Lookup: auth.LookupKey(token), Digest: d, Scheme: auth.SchemeDorangV1,
		Principal: auth.Principal{KeyID: keyID, SecretID: keyID + ".1", Tier: "commercial"},
	}
	e.mu.Unlock()
	e.publish(t)
}

// publish rebuilds the snapshot from the rows, the way a reload would.
func (e *authEnforcer) publish(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	recs := make([]auth.Record, 0, len(e.rows))
	for id, r := range e.rows {
		r.Principal.Key.Pended = e.pended[id]
		r.Principal.Key.Blocked = e.block[id]
		recs = append(recs, r)
	}
	e.mu.Unlock()
	if err := e.a.Load(recs); err != nil {
		t.Fatal(err)
	}
}

func (e *authEnforcer) Pend(ctx context.Context, keyID, reason string) error {
	e.mu.Lock()
	e.pended[keyID] = true
	e.reason[keyID] = reason
	rec, ok := e.rows[keyID]
	e.mu.Unlock()
	if !ok {
		return errors.New("no such key")
	}
	// The durable half is done. Now the half that decides whether the key
	// actually stops serving.
	return e.a.Announce(ctx, auth.Invalidation{
		KeyID: keyID, Lookups: []string{rec.Lookup}, Cause: auth.CausePended, At: e.now(),
	})
}

func (e *authEnforcer) Release(ctx context.Context, keyID string) error {
	e.mu.Lock()
	delete(e.pended, keyID)
	delete(e.reason, keyID)
	rec := e.rows[keyID]
	e.mu.Unlock()
	return e.a.Announce(ctx, auth.Invalidation{
		KeyID: keyID, Lookups: []string{rec.Lookup}, Cause: auth.CauseReleased, At: e.now(),
	})
}

func (e *authEnforcer) Revoke(ctx context.Context, keyID string) error {
	e.mu.Lock()
	e.block[keyID] = true
	rec := e.rows[keyID]
	e.mu.Unlock()
	return e.a.Announce(ctx, auth.Invalidation{
		KeyID: keyID, Lookups: []string{rec.Lookup}, Cause: auth.CauseRevoked, At: e.now(),
	})
}

// pendedFlag is what the enforcer recorded, used only to prove that the flag
// and the effect are different assertions.
func (e *authEnforcer) pendedFlag(keyID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pended[keyID]
}

// serving reports whether the token still authenticates and authorizes.
func serving(a *auth.Authenticator, token string, now time.Time) error {
	p, err := a.Authenticate(context.Background(), token)
	if err != nil {
		return err
	}
	return p.Authorize(auth.Access{Now: now})
}

// harness wires a guard over a history, an enforcer and a capturing notifier.
type harness struct {
	g    *Guard
	e    *authEnforcer
	a    *auth.Authenticator
	h    *MemHistory
	n    *notify.Notifier
	sent *capture
	now  func() time.Time
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	var mu sync.Mutex
	now := epoch
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	cap := &capture{}
	n, err := notify.New(notify.Options{
		Driver: notify.DriverLua, Hook: cap, From: "a@b", To: []string{"c@d"},
		Workers: 1, Now: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close(context.Background()) })

	e, a := newAuthEnforcer(t, clock, nil)
	hist := NewMemHistory(time.Minute, 7*24*time.Hour, 0)

	cfg := Config{
		Enabled: true, BaselineWindow: 7 * 24 * time.Hour, Window: time.Hour,
		Factor: 10, MinAbsolute: 100_000, MinHistory: 24 * time.Hour,
		Action: ActionPend, Cooldown: time.Hour, Now: clock,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	g, err := New(cfg, hist, e, n)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{g: g, e: e, a: a, h: hist, n: n, sent: cap, now: clock}
}

func (h *harness) drain(t *testing.T) []notify.Message {
	t.Helper()
	// One worker, a synchronous hook: draining is waiting for the queue to
	// empty, which a short deadline bounds without a sleep-and-hope.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.n.Stats().QueueDepth == 0 && h.n.Stats().Posted == h.sent.count() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	return h.sent.take()
}

type capture struct {
	mu   sync.Mutex
	msgs []notify.Message
	n    int
}

// OnEmail is both the filter and, for the lua driver, the transport. It
// captures rather than delivers, which is what makes "every action announces
// itself" an assertion instead of a hope.
func (c *capture) OnEmail(_ context.Context, m *notify.Message) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, *m)
	c.n++
	return false, nil
}

func (c *capture) CanDeliver() bool { return true }

func (c *capture) count() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return uint64(c.n)
}

func (c *capture) take() []notify.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.msgs
	c.msgs = nil
	return out
}

// seed writes `perWindow` tokens into every Window-sized slot of the given span
// ending at `until`, which is what a key with a steady history looks like.
func seed(t *testing.T, h History, keyID string, perWindow int64, span, window time.Duration, until time.Time) {
	t.Helper()
	for at := until.Add(-span); at.Before(until); at = at.Add(window) {
		if err := h.Add(context.Background(), keyID, perWindow, at); err != nil {
			t.Fatal(err)
		}
	}
}

// --- the four rules -----------------------------------------------------------

func TestPendIsTheDefaultAndTheKeyStopsServing(t *testing.T) {
	h := newHarness(t, nil)
	h.e.issue(t, guardedTok, guardedKey)

	// A week of steady history at 20 000 tokens/hour, then a spike to 400 000
	// in the last hour: twentyfold, and far past the absolute floor.
	seed(t, h.h, guardedKey, 20_000, 7*24*time.Hour, time.Hour, epoch.Add(-time.Hour))
	if err := h.h.Add(context.Background(), guardedKey, 400_000, epoch.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	if err := serving(h.a, guardedTok, epoch); err != nil {
		t.Fatalf("the key was not serving to begin with: %v", err)
	}

	f, err := h.g.Evaluate(context.Background(), guardedKey)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Tripped() {
		t.Fatalf("the guard did not trip on a twentyfold departure: %s", f)
	}
	if f.Action != ActionPend {
		t.Fatalf("the default action is %s, want pend", f.Action)
	}

	// THE assertion. A flag test would pass here against an implementation that
	// leaves the key serving for a cache TTL, which is the defect §11.2c
	// records.
	err = serving(h.a, guardedTok, epoch)
	if !errors.Is(err, auth.ErrPended) {
		t.Fatalf("a pended key is still serving: %v", err)
	}
	// And the flag alone would not have proved it.
	if !h.e.pendedFlag(guardedKey) {
		t.Error("the durable half did not happen either")
	}
}

func TestRelativeOnlyAndAbsoluteOnlyEachFailToTrip(t *testing.T) {
	t.Run("relative only", func(t *testing.T) {
		// A key that used 10 tokens an hour and now uses 200: twentyfold, and
		// nowhere near the absolute floor. Without the floor the guard fires
		// hardest on the quietest keys.
		h := newHarness(t, nil)
		h.e.issue(t, guardedTok, guardedKey)
		seed(t, h.h, guardedKey, 10, 7*24*time.Hour, time.Hour, epoch.Add(-time.Hour))
		if err := h.h.Add(context.Background(), guardedKey, 200, epoch.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		f, err := h.g.Evaluate(context.Background(), guardedKey)
		if err != nil {
			t.Fatal(err)
		}
		if f.Condition&CondRelative == 0 {
			t.Fatal("the relative condition did not hold on a twentyfold increase")
		}
		if f.Condition&CondAbsolute != 0 {
			t.Fatal("the absolute condition held on 200 tokens")
		}
		if f.Tripped() || f.Acted() {
			t.Fatalf("the guard acted on the relative condition alone: %s", f)
		}
		if err := serving(h.a, guardedTok, epoch); err != nil {
			t.Fatalf("a quiet key was stopped: %v", err)
		}
	})

	t.Run("absolute only", func(t *testing.T) {
		// A key that has always been busy stays busy. Way past the floor, and
		// no departure from its own baseline at all.
		h := newHarness(t, nil)
		h.e.issue(t, guardedTok, guardedKey)
		seed(t, h.h, guardedKey, 500_000, 7*24*time.Hour, time.Hour, epoch.Add(-time.Hour))
		if err := h.h.Add(context.Background(), guardedKey, 500_000, epoch.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		f, err := h.g.Evaluate(context.Background(), guardedKey)
		if err != nil {
			t.Fatal(err)
		}
		if f.Condition&CondAbsolute == 0 {
			t.Fatal("the absolute condition did not hold on 500 000 tokens")
		}
		if f.Condition&CondRelative != 0 {
			t.Fatalf("the relative condition held on a key running at its own baseline (ratio %.2f)", f.Ratio)
		}
		if f.Tripped() || f.Acted() {
			t.Fatalf("the guard acted on the absolute condition alone: %s", f)
		}
		if err := serving(h.a, guardedTok, epoch); err != nil {
			t.Fatalf("a legitimately busy key was stopped: %v", err)
		}
	})
}

func TestANewKeyOnlyAlerts(t *testing.T) {
	h := newHarness(t, nil)
	h.e.issue(t, guardedTok, guardedKey)

	// Two hours of history against a 24-hour minimum, and a genuine spike. Both
	// conditions hold; the guard must still not act.
	if err := h.h.Add(context.Background(), guardedKey, 1_000, epoch.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := h.h.Add(context.Background(), guardedKey, 900_000, epoch.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	f, err := h.g.Evaluate(context.Background(), guardedKey)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Tripped() {
		t.Fatalf("both conditions were supposed to hold: %s", f)
	}
	if !f.NewKey {
		t.Fatalf("a key with %v of history was not treated as new against a %v minimum",
			f.History, h.g.Config().MinHistory)
	}
	if f.Action != ActionAlertOnly {
		t.Fatalf("a new key was %sed; below a stated minimum of history the guard only alerts", f.Action)
	}
	if err := serving(h.a, guardedTok, epoch); err != nil {
		t.Fatalf("a new key stopped serving on its first busy hour: %v", err)
	}
	// "only alerts" means the operator is still told.
	msgs := h.drain(t)
	if len(msgs) == 0 {
		t.Fatal("a new key tripped and nobody was told; that is not an alert")
	}
}

func TestEveryActionAnnouncesItselfWithTheNumbers(t *testing.T) {
	h := newHarness(t, nil)
	h.e.issue(t, guardedTok, guardedKey)
	seed(t, h.h, guardedKey, 20_000, 7*24*time.Hour, time.Hour, epoch.Add(-time.Hour))
	if err := h.h.Add(context.Background(), guardedKey, 400_000, epoch.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	f, err := h.g.Evaluate(context.Background(), guardedKey)
	if err != nil {
		t.Fatal(err)
	}
	msgs := h.drain(t)
	if len(msgs) != 1 {
		t.Fatalf("the pend produced %d notifications, want exactly 1", len(msgs))
	}
	m := msgs[0]
	if m.Event != notify.EventTokenGuard {
		t.Errorf("the notification is a %s", m.Event)
	}
	// §11.6: the observed rate, the baseline, and WHICH CONDITION tripped.
	fields := map[string]string{}
	for _, fl := range m.Fields {
		fields[fl.Name] = fl.Value
	}
	for _, name := range []string{"observed_rate", "baseline_rate", "condition", "action", "key_id"} {
		if fields[name] == "" {
			t.Errorf("the notification carries no %s; the operator has to go and look", name)
		}
	}
	if fields["condition"] != "relative+absolute" {
		t.Errorf("condition = %q, want both", fields["condition"])
	}
	if fields["action"] != "pend" {
		t.Errorf("action = %q", fields["action"])
	}
	if fields["observed_rate"] != "400000" {
		t.Errorf("observed_rate = %q, want the number the guard actually saw", fields["observed_rate"])
	}
	// Nothing that could be a credential.
	for _, fl := range m.Fields {
		if notify.DeniedField(fl.Name) {
			t.Errorf("the guard's notification carries %q", fl.Name)
		}
	}
	_ = f

	// The release announces too, and is not deduplicated away behind the pend.
	if err := h.g.Release(context.Background(), guardedKey, "operator@example.com"); err != nil {
		t.Fatal(err)
	}
	msgs = h.drain(t)
	if len(msgs) != 1 {
		t.Fatalf("the release produced %d notifications, want 1", len(msgs))
	}
	for _, fl := range msgs[0].Fields {
		if fl.Name == "action" && fl.Value != "release" {
			t.Errorf("the release announced itself as %q", fl.Value)
		}
	}
}

func TestPendIsReleasedInOneActionAndTheKeyServesAgain(t *testing.T) {
	h := newHarness(t, nil)
	h.e.issue(t, guardedTok, guardedKey)
	seed(t, h.h, guardedKey, 20_000, 7*24*time.Hour, time.Hour, epoch.Add(-time.Hour))
	if err := h.h.Add(context.Background(), guardedKey, 400_000, epoch.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.g.Evaluate(context.Background(), guardedKey); err != nil {
		t.Fatal(err)
	}
	if err := serving(h.a, guardedTok, epoch); !errors.Is(err, auth.ErrPended) {
		t.Fatalf("the key was not pended: %v", err)
	}

	// ONE action, and the key serves again on the very next request — not after
	// a TTL. A pend an operator cannot end is not the reversible control §11.6
	// chose over revocation.
	start := time.Now()
	if err := h.g.Release(context.Background(), guardedKey, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := serving(h.a, guardedTok, epoch); err != nil {
		t.Fatalf("a released key is still refused: %v", err)
	}
	t.Logf("release took effect in %v", time.Since(start))

	// And the caller kept the credential it already had.
	if _, err := h.a.Authenticate(context.Background(), guardedTok); err != nil {
		t.Fatalf("the release reissued the credential: %v", err)
	}
}

func TestRevokeIsAvailableAndIsNotTheDefault(t *testing.T) {
	if (Config{}).Action != ActionAlertOnly {
		t.Fatal("the zero Action is not alert_only")
	}
	// The DEFAULT the design states is pend, and it is what a Guard built from
	// §11.6's block does. ActionRevoke has to be asked for by name.
	h := newHarness(t, func(c *Config) { c.Action = ActionRevoke })
	h.e.issue(t, guardedTok, guardedKey)
	seed(t, h.h, guardedKey, 20_000, 7*24*time.Hour, time.Hour, epoch.Add(-time.Hour))
	if err := h.h.Add(context.Background(), guardedKey, 400_000, epoch.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	f, err := h.g.Evaluate(context.Background(), guardedKey)
	if err != nil {
		t.Fatal(err)
	}
	if f.Action != ActionRevoke {
		t.Fatalf("action = %s", f.Action)
	}
	if serving(h.a, guardedTok, epoch) == nil {
		t.Fatal("a revoked key kept serving")
	}
}

func TestThrottleIsRefusedRatherThanSilentlyDowngraded(t *testing.T) {
	_, err := New(Config{Enabled: true, Action: ActionThrottle}, NewMemHistory(0, 0, 0), &noopEnforcer{}, nil)
	if !errors.Is(err, ErrThrottleUnimplemented) {
		t.Fatalf("action: throttle was accepted and quietly did something else: %v", err)
	}
}

func TestTheCooldownSuppressesARepeat(t *testing.T) {
	h := newHarness(t, nil)
	h.e.issue(t, guardedTok, guardedKey)
	seed(t, h.h, guardedKey, 20_000, 7*24*time.Hour, time.Hour, epoch.Add(-time.Hour))
	if err := h.h.Add(context.Background(), guardedKey, 400_000, epoch.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.g.Evaluate(context.Background(), guardedKey); err != nil {
		t.Fatal(err)
	}
	h.drain(t)

	f, err := h.g.Evaluate(context.Background(), guardedKey)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Cooled {
		t.Error("a second evaluation inside the cooldown was not suppressed")
	}
	if msgs := h.drain(t); len(msgs) != 0 {
		t.Errorf("a suppressed evaluation still notified %d times", len(msgs))
	}
}

func TestTheBaselineExcludesTheSpikeItIsComparedAgainst(t *testing.T) {
	// A baseline that included the observation window would be raised by the
	// spike, so a tenfold departure would measure as less than tenfold and the
	// guard would miss what it exists for.
	h := newHarness(t, func(c *Config) {
		c.BaselineWindow = 10 * time.Hour
		c.Factor = 10
		c.MinAbsolute = 1
		c.MinHistory = time.Hour
	})
	h.e.issue(t, guardedTok, guardedKey)
	// Nine hours at exactly 1000/hour, then 10 000 in the last hour: exactly
	// tenfold against the true baseline, and 5.3x against one that swallowed
	// the spike.
	seed(t, h.h, guardedKey, 1000, 9*time.Hour, time.Hour, epoch.Add(-time.Hour))
	if err := h.h.Add(context.Background(), guardedKey, 10_000, epoch.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f, err := h.g.Evaluate(context.Background(), guardedKey)
	if err != nil {
		t.Fatal(err)
	}
	if f.Baseline < 900 || f.Baseline > 1100 {
		t.Fatalf("baseline = %.1f, want ~1000; the spike leaked into it", f.Baseline)
	}
	if !f.Tripped() {
		t.Fatalf("an exactly tenfold departure did not trip: %s", f)
	}
}

func TestSweepEvaluatesEveryKeyAndReportsOnlyTheTrips(t *testing.T) {
	h := newHarness(t, nil)
	h.e.issue(t, guardedTok, guardedKey)
	quiet := "quiet-key"
	seed(t, h.h, guardedKey, 20_000, 7*24*time.Hour, time.Hour, epoch.Add(-time.Hour))
	if err := h.h.Add(context.Background(), guardedKey, 400_000, epoch.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	seed(t, h.h, quiet, 5, 7*24*time.Hour, time.Hour, epoch.Add(-time.Hour))

	trips, err := h.g.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(trips) != 1 || trips[0].KeyID != guardedKey {
		t.Fatalf("the sweep reported %+v, want just the departing key", trips)
	}
	if h.g.Stats().Evaluations < 2 {
		t.Error("the sweep did not examine every key with history")
	}
}

func TestTheDisabledGuardCostsNothingAndDoesNothing(t *testing.T) {
	g, err := New(Config{Enabled: false, Action: ActionPend}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g != nil {
		t.Fatal("a disabled guard allocated a table it will never consult")
	}
	// Every method is safe on the nil guard.
	if err := g.Observe(context.Background(), "k", 1, epoch); err != nil {
		t.Error(err)
	}
	if f, err := g.Evaluate(context.Background(), "k"); err != nil || f.Tripped() {
		t.Error("the disabled guard evaluated something")
	}
	if _, err := g.Sweep(context.Background()); err != nil {
		t.Error(err)
	}
	if err := g.Release(context.Background(), "k", "op"); err != nil {
		t.Error(err)
	}
}

func TestAGuardWithoutAnEnforcerIsRefused(t *testing.T) {
	if _, err := New(Config{Enabled: true}, NewMemHistory(0, 0, 0), nil, nil); err == nil {
		t.Fatal("a guard that cannot stop a key was accepted; it would only set flags")
	}
	if _, err := New(Config{Enabled: true}, nil, &noopEnforcer{}, nil); err == nil {
		t.Fatal("a guard with no history was accepted; it has nothing to compare a key against")
	}
}

func TestMemHistoryIsBoundedAndForgetsInTheSafeDirection(t *testing.T) {
	h := NewMemHistory(time.Minute, time.Hour, 4)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := h.Add(ctx, string(rune('a'+i)), 1, epoch.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if h.Len() > 4 {
		t.Fatalf("the table grew to %d keys against a bound of 4", h.Len())
	}
	// Retention: a sample older than the window is dropped, and the key's
	// reported history shrinks with it — which makes the key NEW again, and a
	// new key can only be alerted about. Losing history in the safe direction
	// is the only acceptable way to lose it.
	k := "long-lived"
	if err := h.Add(ctx, k, 100, epoch); err != nil {
		t.Fatal(err)
	}
	if err := h.Add(ctx, k, 100, epoch.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	since, err := h.Since(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	if since.Before(epoch.Add(2 * time.Hour)) {
		t.Errorf("history claims to start at %v, older than anything still held", since)
	}
	total, err := h.Window(ctx, k, epoch.Add(-time.Hour), epoch.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if total != 100 {
		t.Errorf("the aged-out sample is still counted: %d", total)
	}
}

type noopEnforcer struct{}

func (noopEnforcer) Pend(context.Context, string, string) error { return nil }
func (noopEnforcer) Release(context.Context, string) error      { return nil }
func (noopEnforcer) Revoke(context.Context, string) error       { return nil }
