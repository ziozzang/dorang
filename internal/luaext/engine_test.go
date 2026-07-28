package luaext

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writePolicy(t *testing.T, dir, name, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newEngine builds an engine from policy sources named by hook file.
func newEngine(t *testing.T, files map[string]string, opts ...func(*Options)) *Engine {
	t.Helper()
	o := Options{
		Enabled: true,
		Limits:  Limits{Instructions: 100000, MemoryBytes: 1 << 20, Timeout: 2 * time.Second},
		Logf:    func(string, ...any) {},
	}
	if len(files) > 0 {
		dir := t.TempDir()
		for name, src := range files {
			writePolicy(t, dir, name, src)
		}
		o.Dir = dir
	}
	for _, fn := range opts {
		fn(&o)
	}
	e, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e == nil {
		t.Fatal("New returned a nil engine for an enabled configuration")
	}
	return e
}

// --- disabled is free -------------------------------------------------------

// TestDisabledEngineIsFree is DESIGN §11.5's "disabled by default on the hot
// path" as an assertion. With hooks off the cost is one nil check, and the
// allocator is never touched.
func TestDisabledEngineIsFree(t *testing.T) {
	e, err := New(Options{Enabled: false, Dir: "/nonexistent"})
	if err != nil {
		t.Fatalf("a disabled engine must not fail to build: %v", err)
	}
	if e != nil {
		t.Fatalf("a disabled engine must be a typed nil, got %#v", e)
	}

	ctx := context.Background()
	rv := &RequestView{Model: "gpt-4", KeyID: "k1"}
	sv := &RouteView{Model: "gpt-4", Provider: "p"}
	pv := &ResponseView{Model: "gpt-4", Status: 200}
	ev := &EmailView{Event: "key_created"}

	for _, tc := range []struct {
		name string
		fn   func()
	}{
		{"Enabled", func() {
			if e.Enabled(HookRequest) || e.Enabled(HookRoute) ||
				e.Enabled(HookResponse) || e.Enabled(HookEmail) {
				panic("a nil engine reported a hook as enabled")
			}
		}},
		{"OnRequest", func() { _ = e.OnRequest(ctx, rv) }},
		{"OnRoute", func() { _ = e.OnRoute(ctx, sv) }},
		{"OnResponse", func() { _ = e.OnResponse(ctx, pv) }},
		{"OnEmail", func() { _, _ = e.OnEmail(ctx, ev) }},
	} {
		if n := testing.AllocsPerRun(500, tc.fn); n != 0 {
			t.Errorf("%s on a disabled engine allocated %v times, want 0", tc.name, n)
		}
	}
}

// TestEnabledEngineIsFreeForUnregisteredHooks: turning one hook on must not put
// a cost on the other three. The mask is per hook, not per engine.
func TestEnabledEngineIsFreeForUnregisteredHooks(t *testing.T) {
	e := newEngine(t, map[string]string{
		"on_response.policy": `set observed = "yes"` + "\n",
	})
	if !e.Enabled(HookResponse) {
		t.Fatal("on_response should be enabled")
	}
	ctx := context.Background()
	rv := &RequestView{Model: "gpt-4"}
	if n := testing.AllocsPerRun(500, func() { _ = e.OnRequest(ctx, rv) }); n != 0 {
		t.Errorf("OnRequest with no on_request unit allocated %v times, want 0", n)
	}
}

// --- each hook fires with the right inputs ----------------------------------

func TestEachHookFiresWithItsInputs(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "on_request.policy", `set saw = "request" if model == "gpt-4" and input_tokens > 10`+"\n")
	writePolicy(t, dir, "on_route.policy", `set saw = "route" if provider == "plan-a"`+"\n")
	writePolicy(t, dir, "on_response.policy", `set saw = "response" if status == 200`+"\n")
	writePolicy(t, dir, "on_email.policy", `set saw = "email" if event == "key_created"`+"\n")

	var seen [numHooks]atomic.Value
	native := Native{
		Name: "recorder",
		Request: func(_ context.Context, v *RequestView, _ *RequestDecision) {
			seen[HookRequest].Store(*v)
		},
		Route: func(_ context.Context, v *RouteView, _ *RouteDecision) {
			seen[HookRoute].Store(*v)
		},
		Response: func(_ context.Context, v *ResponseView, _ *ResponseDecision) {
			seen[HookResponse].Store(*v)
		},
		Email: func(_ context.Context, v *EmailView, _ *EmailDecision) error {
			seen[HookEmail].Store(*v)
			return nil
		},
	}
	e, err := New(Options{
		Enabled: true, Dir: dir, Native: []Native{native},
		Limits: Limits{Instructions: 10000, MemoryBytes: 1 << 20, Timeout: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	rd := e.OnRequest(ctx, &RequestView{Model: "gpt-4", InputTokens: 42, KeyID: "k1"})
	if v, ok := rd.Tag("saw"); !ok || v != "request" {
		t.Errorf("on_request tag = %q/%v, want \"request\"", v, ok)
	}
	if got := seen[HookRequest].Load().(RequestView); got.Model != "gpt-4" || got.InputTokens != 42 {
		t.Errorf("on_request native saw %+v", got)
	}

	sd := e.OnRoute(ctx, &RouteView{Model: "gpt-4", Provider: "plan-a", Deployment: "d1"})
	if v, ok := sd.Tag("saw"); !ok || v != "route" {
		t.Errorf("on_route tag = %q/%v", v, ok)
	}
	if got := seen[HookRoute].Load().(RouteView); got.Deployment != "d1" {
		t.Errorf("on_route native saw %+v", got)
	}

	pd := e.OnResponse(ctx, &ResponseView{Model: "gpt-4", Status: 200, OutputTokens: 7})
	if v, ok := pd.Tag("saw"); !ok || v != "response" {
		t.Errorf("on_response tag = %q/%v", v, ok)
	}
	if got := seen[HookResponse].Load().(ResponseView); got.OutputTokens != 7 {
		t.Errorf("on_response native saw %+v", got)
	}

	ed, err := e.OnEmail(ctx, &EmailView{Event: "key_created", SubjectID: "k1"})
	if err != nil {
		t.Fatalf("OnEmail: %v", err)
	}
	if v, ok := ed.Tag("saw"); !ok || v != "email" {
		t.Errorf("on_email tag = %q/%v", v, ok)
	}
	if got := seen[HookEmail].Load().(EmailView); got.SubjectID != "k1" {
		t.Errorf("on_email native saw %+v", got)
	}
}

// --- fail-closed: a deny is obeyed ------------------------------------------

func TestDenyIsHonoured(t *testing.T) {
	e := newEngine(t, map[string]string{
		"on_request.policy": `deny "gpt-4 is not available on this key" if model == "gpt-4"` + "\n" +
			`set tier = "ok"` + "\n",
	})
	d := e.OnRequest(context.Background(), &RequestView{Model: "gpt-4"})
	if !d.Denied {
		t.Fatal("an explicit deny must be honoured (DESIGN §11.5 fail-closed)")
	}
	if d.Reason != "gpt-4 is not available on this key" {
		t.Errorf("reason = %q", d.Reason)
	}
	if d.Code != DefaultDenyCode {
		t.Errorf("code = %q, want %q", d.Code, DefaultDenyCode)
	}
	if e.Stats().Denies != 1 {
		t.Errorf("denies = %d, want 1", e.Stats().Denies)
	}

	// A request the rule does not match proceeds, and the later rule still runs.
	d = e.OnRequest(context.Background(), &RequestView{Model: "gpt-3.5"})
	if d.Denied {
		t.Fatal("a non-matching request must not be denied")
	}
	if v, _ := d.Tag("tier"); v != "ok" {
		t.Errorf("tier = %q, want ok", v)
	}
}

func TestRouteDenyIsHonoured(t *testing.T) {
	e := newEngine(t, map[string]string{
		"on_route.policy": `deny "this team may not leave the region" if provider == "plan-b"` + "\n",
	})
	d := e.OnRoute(context.Background(), &RouteView{Provider: "plan-b"})
	if !d.Denied {
		t.Fatal("a deny on the routing decision must be honoured")
	}
	if d.Reason != "this team may not leave the region" || d.Code != DefaultDenyCode {
		t.Errorf("decision = %+v", d)
	}
	if d = e.OnRoute(context.Background(), &RouteView{Provider: "plan-a"}); d.Denied {
		t.Fatal("a non-matching route must not be refused")
	}
}

// --- fail-open: ceilings ----------------------------------------------------

// TestInstructionCeilingSkipsAndWarns: a program that outruns its instruction
// budget is skipped and the request proceeds. Fail-open is the whole point.
func TestInstructionCeilingSkipsAndWarns(t *testing.T) {
	var warned atomic.Int64
	e := newEngine(t, map[string]string{
		"on_request.policy": strings.Repeat(`deny "no" if model == "never-matches"`+"\n", 50),
	}, func(o *Options) {
		o.Limits = Limits{Instructions: 5, MemoryBytes: 1 << 20, Timeout: time.Second}
		o.Logf = func(string, ...any) { warned.Add(1) }
	})

	d := e.OnRequest(context.Background(), &RequestView{Model: "gpt-4"})
	if d.Denied {
		t.Fatal("a hook that exceeded its instruction ceiling must not be able to refuse")
	}
	if got := e.Stats().LimitHits; got == 0 {
		t.Error("a ceiling breach must be counted")
	}
	if warned.Load() == 0 {
		t.Error("a ceiling breach must warn")
	}
}

// TestInstructionCeilingCannotBeUsedToDeny: a program whose *deny* rule is the
// one that runs out of budget fails open, and a matching deny inside budget
// still denies. The asymmetry has to be a property of completion, not of luck.
func TestMemoryCeilingSkipsAndWarns(t *testing.T) {
	e := newEngine(t, map[string]string{
		"on_request.policy": `deny "no" if model == "gpt-4"` + "\n",
	}, func(o *Options) {
		// One operand costs valueOverhead bytes, so a ceiling below that is
		// exceeded by the first push.
		o.Limits = Limits{Instructions: 1000, MemoryBytes: 8, Timeout: time.Second}
	})
	d := e.OnRequest(context.Background(), &RequestView{Model: "gpt-4"})
	if d.Denied {
		t.Fatal("a hook that exceeded its memory ceiling must not be able to refuse")
	}
	if e.Stats().LimitHits == 0 {
		t.Error("a memory ceiling breach must be counted")
	}
}

// TestWallClockCeilingIsEnforcedByTheCaller is the "a hook cannot block a
// request past its ceiling" requirement, with a hook that deliberately ignores
// its context. Nothing about the hook cooperates; the caller stops waiting.
func TestWallClockCeilingIsEnforcedByTheCaller(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	e := newEngine(t, nil, func(o *Options) {
		o.Limits = Limits{Instructions: 1000, MemoryBytes: 1 << 20, Timeout: 25 * time.Millisecond}
		o.Native = []Native{{
			Name: "wedged",
			Request: func(_ context.Context, _ *RequestView, d *RequestDecision) {
				// Ignores the context entirely. Only the caller's watchdog can
				// end this.
				<-release
				d.Denied = true
			},
		}}
	})

	start := time.Now()
	d := e.OnRequest(context.Background(), &RequestView{Model: "gpt-4"})
	elapsed := time.Since(start)

	if d.Denied {
		t.Fatal("an abandoned hook must not be able to refuse the request")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the request waited %v on a wedged hook", elapsed)
	}
	if e.Stats().Timeouts == 0 {
		t.Error("an abandoned invocation must be counted")
	}
}

// TestRepeatedAbandonmentTripsTheHookOff: a wedged extension leaks one
// goroutine per invocation, so it is switched off rather than left to accumulate.
func TestRepeatedAbandonmentTripsTheHookOff(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	e := newEngine(t, nil, func(o *Options) {
		o.Limits = Limits{Instructions: 1000, MemoryBytes: 1 << 20, Timeout: time.Millisecond}
		o.Native = []Native{{
			Name:    "wedged",
			Request: func(context.Context, *RequestView, *RequestDecision) { <-release },
		}}
	})

	v := &RequestView{Model: "gpt-4"}
	for i := 0; i < maxConsecutiveTimeouts+2 && !e.Tripped(HookRequest); i++ {
		e.OnRequest(context.Background(), v)
	}
	if !e.Tripped(HookRequest) {
		t.Fatal("a hook abandoned this many times must be switched off")
	}
	if e.Enabled(HookRequest) {
		t.Fatal("a tripped hook must report itself disabled")
	}
	// And once tripped it is free again.
	if n := testing.AllocsPerRun(200, func() { _ = e.OnRequest(context.Background(), v) }); n != 0 {
		t.Errorf("a tripped hook allocated %v times, want 0", n)
	}
}

// --- abandonment is bounded -------------------------------------------------

// settledGoroutines reads the goroutine count once it stops moving, so a
// baseline is not taken in the middle of someone else's teardown.
func settledGoroutines(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	last := -1
	for {
		runtime.Gosched()
		n := runtime.NumGoroutine()
		if n == last {
			return n
		}
		last = n
		if time.Now().After(deadline) {
			return n
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForGoroutines polls rather than sleeps: an abandoned hook returns when it
// returns, and a fixed sleep either flakes or hides the thing being measured.
func waitForGoroutines(want int, timeout time.Duration) (int, bool) {
	deadline := time.Now().Add(timeout)
	for {
		n := runtime.NumGoroutine()
		if n <= want {
			return n, true
		}
		if time.Now().After(deadline) {
			return n, false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestAbandonedInvocationsAreBounded is the promise the trip alone did not make.
//
// maxConsecutiveTimeouts bounds abandonment over *time* and says nothing about
// an instant: before this bound existed, 32 requests to a wedged hook produced
// 32 goroutines that were still running after the hook had been switched off, so
// an operator saw hooks disabled and a machine still pinned with nothing joining
// the two. The bound here is a supply: [maxAbandoned] outstanding invocations
// per hook, after which invocations are refused before a goroutine exists to
// abandon.
func TestAbandonedInvocationsAreBounded(t *testing.T) {
	release := make(chan struct{})
	var closeOnce sync.Once
	stop := func() { closeOnce.Do(func() { close(release) }) }
	t.Cleanup(stop)

	var running atomic.Int64
	e := newEngine(t, nil, func(o *Options) {
		o.Limits = Limits{Instructions: 1000, MemoryBytes: 1 << 20, Timeout: 5 * time.Millisecond}
		o.Native = []Native{{
			Name: "wedged",
			Request: func(context.Context, *RequestView, *RequestDecision) {
				running.Add(1)
				defer running.Add(-1)
				// Ignores its context, as a hook stuck in an uninterruptible
				// call would. Only the test can end this.
				<-release
			},
		}}
	})

	base := settledGoroutines(t)
	v := &RequestView{Model: "gpt-4"}

	const invocations = 4 * maxConsecutiveTimeouts
	for i := 0; i < invocations; i++ {
		if d := e.OnRequest(context.Background(), v); d.Denied {
			t.Fatal("an abandoned hook refused a request; abandonment fails open")
		}
		if n := int(running.Load()); n > maxAbandoned {
			t.Fatalf("after %d invocations, %d hooks are running at once, want at most %d",
				i+1, n, maxAbandoned)
		}
		if n := e.Abandoned(HookRequest); n > maxAbandoned {
			t.Fatalf("after %d invocations, %d are outstanding, want at most %d",
				i+1, n, maxAbandoned)
		}
	}

	// The goroutine count is the property that matters, because the counter
	// above is the fix's own bookkeeping and could agree with itself while
	// leaking. Slack covers the harness's own churn under -race.
	const slack = 8
	if n := runtime.NumGoroutine(); n > base+maxAbandoned+slack {
		t.Fatalf("%d invocations of a wedged hook left %d goroutines (baseline %d); "+
			"at most %d may be outstanding", invocations, n, base, maxAbandoned)
	}
	if !e.Tripped(HookRequest) {
		t.Fatal("a hook that saturated its abandonment budget this long must be switched off")
	}
	if e.Stats().Refused == 0 {
		t.Error("refusing an invocation rather than leaking one must be counted")
	}

	// And when the wedged hooks finally return, the goroutines go with them.
	stop()
	if n, ok := waitForGoroutines(base+slack, 5*time.Second); !ok {
		t.Fatalf("goroutines settled at %d, baseline %d: abandoned hooks did not exit", n, base)
	}
}

// TestAbandonmentIsBoundedAtABurst is the bound the test above did not make.
//
// [maxAbandoned] is checked at admission and counted at abandonment, so a burst
// passes admission before any of it has been counted: sixteen simultaneous
// requests to a wedged hook produced sixteen abandoned goroutines against a
// published bound of eight, with no trip involved and nothing a plugin did
// wrong. A bound a request rate can exceed is not a bound.
//
// What holds now is a reservation taken before the goroutine exists: at most
// [maxLive] goroutines per hook at any instant, whatever arrives at once.
func TestAbandonmentIsBoundedAtABurst(t *testing.T) {
	release := make(chan struct{})
	var closeOnce sync.Once
	stop := func() { closeOnce.Do(func() { close(release) }) }
	t.Cleanup(stop)

	var running, peak atomic.Int64
	e := newEngine(t, nil, func(o *Options) {
		o.Limits = Limits{Instructions: 1000, MemoryBytes: 1 << 20, Timeout: 20 * time.Millisecond}
		o.Native = []Native{{
			Name: "wedged",
			Request: func(context.Context, *RequestView, *RequestDecision) {
				n := running.Add(1)
				defer running.Add(-1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				// Ignores its context, as a hook stuck in an uninterruptible
				// host call does. Only the test can end this.
				<-release
			},
		}}
	})

	base := settledGoroutines(t)
	// Larger than the cap, and all at once: the shape the sequential test cannot
	// produce, because it is the simultaneity that beat the old check.
	const burst = 4 * maxLive
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d := e.OnRequest(context.Background(), &RequestView{Model: "gpt-4"}); d.Denied {
				t.Error("an abandoned hook refused a request; abandonment fails open")
			}
		}()
	}
	wg.Wait()

	if n := peak.Load(); n > maxLive {
		t.Fatalf("a burst of %d ran %d hooks at once, want at most %d: the bound is one a "+
			"request rate can raise", burst, n, maxLive)
	}
	if n := e.Abandoned(HookRequest); n > maxLive {
		t.Fatalf("a burst of %d left %d outstanding, want at most %d", burst, n, maxLive)
	}
	// The goroutine count is the property that matters; the counters above are
	// the fix's own bookkeeping and could agree with themselves while leaking.
	const slack = 8
	if n := runtime.NumGoroutine(); n > base+maxLive+slack {
		t.Fatalf("a burst of %d left %d goroutines (baseline %d); at most %d may exist",
			burst, n, base, maxLive)
	}
	if e.Stats().Refused == 0 {
		t.Error("a burst past the supply must be refused rather than run, and counted")
	}

	stop()
	if n, ok := waitForGoroutines(base+slack, 5*time.Second); !ok {
		t.Fatalf("goroutines settled at %d, baseline %d: abandoned hooks did not exit", n, base)
	}
}

// TestALateHookIsNotAnAbandonedOne: a hook that finishes as its deadline passes
// has leaked nothing, so it counts toward nothing.
//
// The distinction is the whole meaning of the trip. "Abandoned" is supposed to
// name a goroutine nobody is waiting for and nobody can stop; a hook that
// observes its cancellation and returns is neither, and counting it as one is
// counting a leak that did not happen — thirty-two of which switch a hook off
// for the life of the engine. Everything inside the sandbox now observes its
// cancellation, down to the pattern matcher, so this is the common case rather
// than the exotic one.
func TestALateHookIsNotAnAbandonedOne(t *testing.T) {
	var ran atomic.Int64
	e := newEngine(t, nil, func(o *Options) {
		o.Limits = Limits{Instructions: 1000, MemoryBytes: 1 << 20, Timeout: 5 * time.Millisecond}
		o.Native = []Native{{
			Name: "late",
			Request: func(ctx context.Context, _ *RequestView, _ *RequestDecision) {
				ran.Add(1)
				// Returns when its context ends, which is the deadline itself:
				// the finish and the abandonment race, every time.
				<-ctx.Done()
			},
		}}
	})

	const invocations = maxConsecutiveTimeouts + 4
	for i := 0; i < invocations; i++ {
		if d := e.OnRequest(context.Background(), &RequestView{Model: "gpt-4"}); d.Denied {
			t.Fatalf("invocation %d denied the request", i)
		}
	}
	if st := e.Stats(); st.Timeouts != 0 {
		t.Fatalf("a hook that always came back was counted as abandoned %d times: %+v",
			st.Timeouts, st)
	}
	if e.Abandoned(HookRequest) != 0 {
		t.Fatalf("%d outstanding after every invocation returned", e.Abandoned(HookRequest))
	}
	if e.Tripped(HookRequest) {
		t.Fatalf("%d invocations that leaked nothing switched the hook off", invocations)
	}
	if n := ran.Load(); n != invocations {
		t.Fatalf("the hook ran %d times, want %d: invocations were refused for a backlog "+
			"that never existed", n, invocations)
	}
}

// TestABacklogRefusalDoesNotTripAFailClosedHook is the correction to a rule that
// was right for the hook it was written for.
//
// Refusals at the abandonment bound count toward the trip so that a wedged
// *enrichment* hook is switched off rather than left refusing in silence. A
// masking filter has no such silence: a fail-closed refusal stops the request,
// which is the loudest thing the gateway does. Counting them there meant a
// refusal costing microseconds could reach maxConsecutiveTimeouts in a burst,
// and the trip is for the life of the engine.
func TestABacklogRefusalDoesNotTripAFailClosedHook(t *testing.T) {
	if !HookFilterRequest.failsClosed() {
		t.Fatal("the filter hook is the fail-closed one; this test is about that")
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	e := luaEngine(t, `dorang.register("on_filter_request", function(f) f.doc.text(1) end)`,
		func(o *Options) {
			o.Limits = Limits{Instructions: 1000, MemoryBytes: 1 << 20, Timeout: time.Millisecond}
		})
	// Saturate the abandonment supply by hand: what is under test is what the
	// refusals do, not how the backlog arose.
	e.abandoned[HookFilterRequest].Store(maxAbandoned)

	s := "900101-1234567"
	for i := 0; i < 4*maxConsecutiveTimeouts; i++ {
		doc := NewDoc()
		doc.Add("m0", &s)
		d := e.FilterRequest(context.Background(), &FilterView{Doc: doc, Mask: staticMasker{}})
		if !d.Refuse {
			t.Fatalf("refusal %d: a filter that did not run served the request: %+v", i, d)
		}
	}
	if e.Tripped(HookFilterRequest) {
		t.Fatalf("%d cheap refusals switched a masking filter off for the life of the engine",
			4*maxConsecutiveTimeouts)
	}
	if e.Stats().Refused == 0 {
		t.Error("the refusals were not counted")
	}

	// And the drain recovers: the same hook serves again once the backlog does.
	e.abandoned[HookFilterRequest].Store(0)
	doc := NewDoc()
	doc.Add("m0", &s)
	if d := e.FilterRequest(context.Background(), &FilterView{Doc: doc, Mask: staticMasker{}}); d.Refuse {
		t.Fatalf("the filter did not recover when its backlog drained: %+v", d)
	}
}

// TestABacklogRefusalStillTripsAFailOpenHook is the other half: the instruction
// the correction above narrows is still in force where it was right.
func TestABacklogRefusalStillTripsAFailOpenHook(t *testing.T) {
	e := newEngine(t, nil, func(o *Options) {
		o.Limits = Limits{Instructions: 1000, MemoryBytes: 1 << 20, Timeout: time.Millisecond}
		o.Native = []Native{{
			Name:    "enrich",
			Request: func(context.Context, *RequestView, *RequestDecision) {},
		}}
	})
	e.abandoned[HookRequest].Store(maxAbandoned)

	for i := 0; i < maxConsecutiveTimeouts+1 && !e.Tripped(HookRequest); i++ {
		e.OnRequest(context.Background(), &RequestView{Model: "gpt-4"})
	}
	if !e.Tripped(HookRequest) {
		t.Fatal("a wedged enrichment hook that refuses in silence must still be switched off")
	}
}

// TestASearchThatBlewItsCeilingDoesNotOutliveTheRequest is the other half of the
// same failure, and the half the bound above cannot fix.
//
// A hook abandoned inside a single uninterruptible builtin never observes its
// cancellation, so before the search family was priced these goroutines spun for
// as long as the pattern took — a core each, outliving the request by seconds.
// Every one of them must now die on its own, because the ceiling fires *inside*
// the builtin. Fewer invocations are used than [maxAbandoned] deliberately: none
// of them may be refused, so each abandoned goroutine has to exit by itself.
func TestASearchThatBlewItsCeilingDoesNotOutliveTheRequest(t *testing.T) {
	e := luaEngine(t, `
		dorang.register("on_request", function(req)
			string.find(req.key_id, ".-.-.-@")
		end)`, func(o *Options) {
		o.Limits = Limits{Instructions: 5_000_000, MemoryBytes: 32 << 20, Timeout: time.Millisecond}
	})

	base := settledGoroutines(t)
	subject := strings.Repeat("a", 1024)
	const invocations = maxAbandoned - 2
	for i := 0; i < invocations; i++ {
		if d := e.OnRequest(context.Background(), &RequestView{KeyID: subject}); d.Denied {
			t.Fatal("an abandoned hook refused a request")
		}
	}
	if st := e.Stats(); st.Refused != 0 {
		t.Fatalf("%d invocations should all have run: %+v", invocations, st)
	}

	// The stated bound: an abandoned search exits within its own remaining
	// instruction budget, which at the package default is a fraction of a
	// second. Five seconds is the margin, not the expectation.
	if n, ok := waitForGoroutines(base+4, 5*time.Second); !ok {
		t.Fatalf("goroutines settled at %d, baseline %d: %d abandoned searches are still running",
			n, base, invocations)
	}
}

// --- fail-open: panics ------------------------------------------------------

func TestPanicIsContained(t *testing.T) {
	var warned atomic.Int64
	e := newEngine(t, nil, func(o *Options) {
		o.Logf = func(string, ...any) { warned.Add(1) }
		o.Native = []Native{{
			Name: "crasher",
			Request: func(_ context.Context, _ *RequestView, d *RequestDecision) {
				// Sets the refusal *and then* crashes. The refusal must not
				// survive: a deny is only honoured from a hook that completed.
				d.Denied = true
				d.Reason = "should never be seen"
				panic("boom")
			},
			Route: func(context.Context, *RouteView, *RouteDecision) {
				panic("boom")
			},
			Response: func(context.Context, *ResponseView, *ResponseDecision) {
				panic("boom")
			},
			Email: func(context.Context, *EmailView, *EmailDecision) error {
				panic("boom")
			},
		}}
	})
	ctx := context.Background()

	if d := e.OnRequest(ctx, &RequestView{Model: "m"}); d.Denied {
		t.Fatal("a panicking hook must not be able to refuse a request")
	}
	if d := e.OnRoute(ctx, &RouteView{Provider: "p"}); d.Denied {
		t.Fatal("a panicking hook must not be able to refuse a route")
	}
	e.OnResponse(ctx, &ResponseView{Status: 200})
	if d, err := e.OnEmail(ctx, &EmailView{Event: "invite"}); err != nil || d.Denied {
		t.Fatalf("a panicking hook must not suppress a notification: %v %+v", err, d)
	}
	if got := e.Stats().Panics; got != 4 {
		t.Errorf("panics = %d, want 4", got)
	}
	if warned.Load() == 0 {
		t.Error("a panic must warn")
	}
}

func TestPanicErrorMessage(t *testing.T) {
	err := &PanicError{Hook: HookRequest, Unit: "x", Value: "boom"}
	if got := err.Error(); !strings.Contains(got, "on_request") || !strings.Contains(got, "boom") {
		t.Errorf("Error() = %q", got)
	}
	if !isPanic(error(err)) {
		t.Error("isPanic should recognise a PanicError")
	}
}

// --- cancellation -----------------------------------------------------------

func TestCancelledRequestDoesNotCountAsAHookFailure(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	e := newEngine(t, nil, func(o *Options) {
		o.Limits = Limits{Instructions: 100, MemoryBytes: 1 << 20, Timeout: time.Minute}
		o.Native = []Native{{
			Name:    "slow",
			Request: func(context.Context, *RequestView, *RequestDecision) { <-release },
		}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if d := e.OnRequest(ctx, &RequestView{Model: "m"}); d.Denied {
		t.Fatal("a cancelled request must fail open")
	}
}

// --- the email transport ----------------------------------------------------

func TestEmailHookCanSuppressAndDeliver(t *testing.T) {
	var delivered atomic.Int64
	deliverErr := errors.New("smtp is down")
	var fail atomic.Bool

	e := newEngine(t, map[string]string{
		"on_email.policy": `deny "not for robots" if recipient endswith "@example.invalid"` + "\n",
	}, func(o *Options) {
		o.Native = []Native{{
			Name: "transport",
			Email: func(_ context.Context, _ *EmailView, _ *EmailDecision) error {
				if fail.Load() {
					return deliverErr
				}
				delivered.Add(1)
				return nil
			},
		}}
	})
	if !e.HasEmailTransport() {
		t.Fatal("a native Email function is a transport")
	}
	ctx := context.Background()

	d, err := e.OnEmail(ctx, &EmailView{Event: "invite", Recipient: "bot@example.invalid"})
	if err != nil || !d.Denied {
		t.Fatalf("the filter must suppress: %+v %v", d, err)
	}
	if delivered.Load() != 0 {
		t.Error("a suppressed message must not be delivered")
	}

	if _, err := e.OnEmail(ctx, &EmailView{Event: "invite", Recipient: "ops@example.com"}); err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if delivered.Load() != 1 {
		t.Errorf("delivered = %d, want 1", delivered.Load())
	}

	fail.Store(true)
	if _, err := e.OnEmail(ctx, &EmailView{Event: "invite", Recipient: "ops@example.com"}); !errors.Is(err, deliverErr) {
		t.Errorf("a delivery failure must surface to the caller, got %v", err)
	}
}

func TestNoEmailTransportWithProgramsOnly(t *testing.T) {
	e := newEngine(t, map[string]string{
		"on_email.policy": `allow` + "\n",
	})
	if e.HasEmailTransport() {
		t.Fatal("a policy program can suppress but cannot deliver")
	}
}

// --- construction -----------------------------------------------------------

func TestNewRejectsUnknownHookName(t *testing.T) {
	if _, err := New(Options{Enabled: true, Hooks: []string{"on_teatime"}}); err == nil {
		t.Fatal("an unknown hook name must be a configuration error")
	}
}

func TestNewRejectsNativeForUnlistedHook(t *testing.T) {
	_, err := New(Options{
		Enabled: true,
		Hooks:   []string{"on_response"},
		Native: []Native{{
			Name:    "x",
			Request: func(context.Context, *RequestView, *RequestDecision) {},
		}},
	})
	if err == nil {
		t.Fatal("a native for a hook the configuration excludes must be an error, not a silent skip")
	}
}

func TestNewRejectsUnnamedNative(t *testing.T) {
	_, err := New(Options{
		Enabled: true,
		Native:  []Native{{Request: func(context.Context, *RequestView, *RequestDecision) {}}},
	})
	if err == nil {
		t.Fatal("a native without a name cannot be reported in a warning")
	}
}

func TestHookNamesMatchTheDesign(t *testing.T) {
	want := []string{"on_request", "on_route", "on_response", "on_email"}
	for i, n := range want {
		h, ok := ParseHook(n)
		if !ok || int(h) != i {
			t.Errorf("ParseHook(%q) = %v/%v", n, h, ok)
		}
		if Hook(i).String() != n {
			t.Errorf("Hook(%d).String() = %q, want %q", i, Hook(i).String(), n)
		}
	}
	if _, ok := ParseHook("on_nothing"); ok {
		t.Error("ParseHook accepted an unknown name")
	}
	if Hook(99).String() != "unknown" {
		t.Error("an out-of-range hook should render as unknown")
	}
}

func TestNilEngineAccessors(t *testing.T) {
	var e *Engine
	if e.Tripped(HookRequest) {
		t.Error("a nil engine is not tripped")
	}
	if p, n := e.Units(HookRequest); p != 0 || n != 0 {
		t.Error("a nil engine has no units")
	}
	if e.Limits() != (Limits{}) {
		t.Error("a nil engine has no limits")
	}
	if e.Stats() != (Stats{}) {
		t.Error("a nil engine has no statistics")
	}
	if e.HasEmailTransport() {
		t.Error("a nil engine has no transport")
	}
}

func TestLimitsDefaults(t *testing.T) {
	var l Limits
	l.setDefaults()
	if l.Instructions == 0 || l.MemoryBytes == 0 || l.Timeout == 0 {
		t.Fatalf("setDefaults left a ceiling at zero: %+v", l)
	}
}
