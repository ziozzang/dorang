package probe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/quota"
)

// The secret every test authenticates with. It is long enough to be scrubbed
// (see minScrubLen) and distinctive enough that a substring search for it in an
// error message cannot match by accident.
const testSecret = "sk-test-9f4c1e77aa2b4d5e8c0f1a2b3c4d5e6f" // pragma: allowlist secret — test fixture

// zaiOK is a realistic successful payload: two token windows, one reset in unix
// milliseconds and one as an ISO-8601 string, plus a TIME_LIMIT whose meaning
// is contested.
const zaiOK = `{
  "code": 200, "msg": "success", "success": true,
  "data": {"limits": [
    {"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":37,"nextResetTime":1777819631597},
    {"type":"TOKENS_LIMIT","unit":1,"number":7,"percentage":12.5,"nextResetTime":"2026-04-11T07:00:00.528743+00:00"},
    {"type":"TIME_LIMIT","unit":1,"number":30,"percentage":4,"nextResetTime":null}
  ]}
}`

func testCredential(provider string) quota.Credential {
	return quota.NewCredential("cred-1", provider, testSecret)
}

// serve starts an httptest server for a handler and returns it, closed on
// cleanup.
func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// jsonHandler answers every request with one body and status.
func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// newProbe builds a prober against a test server with the timing gates open, so
// that a test exercises one behavior rather than the rate limiter.
func newProbe(t *testing.T, provider, base string, mut ...func(*Config)) *Prober {
	t.Helper()
	cfg := Config{
		BaseURL:     base,
		MinInterval: -1, // no floor: tests read as often as they like
		Timeout:     2 * time.Second,
	}
	for _, m := range mut {
		m(&cfg)
	}
	p, err := New(provider, cfg)
	if err != nil {
		t.Fatalf("New(%q): %v", provider, err)
	}
	return p
}

func TestReadNormalizesZAI(t *testing.T) {
	t.Parallel()
	s := serve(t, jsonHandler(200, zaiOK))
	p := newProbe(t, "zai", s.URL)

	snap, err := p.Read(context.Background(), testCredential("zai"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if snap.ProviderID != "zai" || snap.CredentialID != "cred-1" {
		t.Fatalf("snapshot identity = %q/%q", snap.ProviderID, snap.CredentialID)
	}
	if snap.FetchedAt.IsZero() {
		t.Fatal("FetchedAt is zero: quota uses it as the delta baseline")
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(snap.Windows))
	}

	// The five-hour token window, reset reported as unix milliseconds.
	w := snap.Windows[0]
	if w.Label != "tokens_limit:5h" {
		t.Errorf("label = %q", w.Label)
	}
	if !w.Mapped || w.Window != quota.Rolling(5*time.Hour) || w.Metric != quota.MetricTokensTotal {
		t.Errorf("window key = %v/%v mapped=%v", w.Window, w.Metric, w.Mapped)
	}
	if !w.Known || w.UsedPercent != 37 {
		t.Errorf("used percent = %v known=%v", w.UsedPercent, w.Known)
	}
	if got, want := w.ResetAt.UTC(), time.UnixMilli(1777819631597).UTC(); !got.Equal(want) {
		t.Errorf("reset = %v, want %v", got, want)
	}
	if w.Used != 0 || w.Limit != 0 {
		t.Errorf("percent-only window invented absolute figures: used=%d limit=%d", w.Used, w.Limit)
	}

	// The seven-day token window, reset reported as an ISO-8601 string. Both
	// encodings appear in one response and both must decode.
	w = snap.Windows[1]
	if w.Label != "tokens_limit:168h" {
		t.Errorf("label = %q", w.Label)
	}
	if !w.Mapped || w.Window != quota.Rolling(7*24*time.Hour) {
		t.Errorf("window = %v mapped=%v", w.Window, w.Mapped)
	}
	want := time.Date(2026, 4, 11, 7, 0, 0, 528743000, time.UTC)
	if !w.ResetAt.Equal(want) {
		t.Errorf("reset = %v, want %v", w.ResetAt, want)
	}

	// TIME_LIMIT: carried, labelled, and deliberately not filed under a metric.
	w = snap.Windows[2]
	if w.Mapped {
		t.Error("TIME_LIMIT was filed under a metric; its meaning is contested")
	}
	if !w.Known || w.UsedPercent != 4 {
		t.Errorf("TIME_LIMIT percentage lost: %v known=%v", w.UsedPercent, w.Known)
	}
	if got := snap.Unmapped(); len(got) != 1 || got[0] != "time_limit:720h" {
		t.Errorf("Unmapped() = %v", got)
	}

	// What reaches quota: only the two windows with a key.
	res := snap.ProbeResult()
	if len(res.Windows) != 2 {
		t.Fatalf("ProbeResult windows = %d, want 2 (the unmapped one must not reach quota)", len(res.Windows))
	}
	if res.Windows[0].UsedPercent != 37 || res.Windows[0].Used != 0 {
		t.Errorf("provider window = %+v", res.Windows[0])
	}
}

// A failed read must leave the last good snapshot exactly as it was. This is
// DESIGN §6.2's first rule: a failed read is not an exhausted quota, and a
// credential that goes dark keeps serving.
func TestFailureKeepsLastSnapshot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    error
	}{
		{"500", jsonHandler(500, `{"error":"boom"}`), ErrStatus},
		{"401", jsonHandler(401, `{"error":"bad key"}`), ErrUnauthorized},
		{"429", jsonHandler(429, `{"error":"slow down"}`), ErrRateLimited},
		{"garbage", jsonHandler(200, `<html>gateway timeout</html>`), ErrMalformed},
		{"empty", jsonHandler(200, `{"code":200,"success":true,"data":{"limits":[]}}`), ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var fail atomic.Bool
			s := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if fail.Load() {
					tc.handler(w, r)
					return
				}
				jsonHandler(200, zaiOK)(w, r)
			})
			p := newProbe(t, "zai", s.URL)
			cred := testCredential("zai")

			good, err := p.Read(context.Background(), cred)
			if err != nil {
				t.Fatalf("first Read: %v", err)
			}

			fail.Store(true)
			if _, err := p.Read(context.Background(), cred); !errors.Is(err, tc.want) {
				t.Fatalf("second Read error = %v, want %v", err, tc.want)
			}

			got, ok := p.Snapshot(cred.ID())
			if !ok {
				t.Fatal("the last good snapshot was discarded by a failed read")
			}
			if !got.FetchedAt.Equal(good.FetchedAt) {
				t.Errorf("FetchedAt moved on failure: %v, want %v", got.FetchedAt, good.FetchedAt)
			}
			if len(got.Windows) != len(good.Windows) {
				t.Errorf("windows changed on failure")
			}
			if h := p.Health(cred.ID()); h.Failures != 1 || h.Reason == "" {
				t.Errorf("health = %+v, want one failure with a reason", h)
			}
		})
	}
}

// A hanging provider is a failed read like any other, and must not hold a
// caller past its deadline.
func TestHangingProviderTimesOutAndKeepsSnapshot(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var hang atomic.Bool
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		jsonHandler(200, zaiOK)(w, r)
	})
	t.Cleanup(func() { close(release) })

	p := newProbe(t, "zai", s.URL, func(c *Config) { c.Timeout = 60 * time.Millisecond })
	cred := testCredential("zai")
	good, err := p.Read(context.Background(), cred)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}

	hang.Store(true)
	start := time.Now()
	_, err = p.Read(context.Background(), cred)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("error = %v, want ErrTransport", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error does not report the deadline: %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("a hang held the caller for %v", elapsed)
	}
	got, ok := p.Snapshot(cred.ID())
	if !ok || !got.FetchedAt.Equal(good.FetchedAt) {
		t.Error("a hang discarded the last good snapshot")
	}
}

// Every error path, checked for the credential. A provider's error body can
// echo the key back; a transport error quotes the URL, which for a provider
// that authenticates in a query parameter contains the key.
func TestSecretsNeverAppearInErrors(t *testing.T) {
	t.Parallel()
	echo := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// The hostile case: the provider quotes the Authorization header
			// straight back into its error body.
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"message":"invalid key %s","auth":%q}}`,
				testSecret, r.Header.Get("Authorization"))
		}
	}
	cred := testCredential("zai")

	cases := []struct {
		name string
		read func(t *testing.T) error
	}{
		{"401 echoing the key", func(t *testing.T) error {
			s := serve(t, echo(401))
			return errOf(newProbe(t, "zai", s.URL).Read(context.Background(), cred))
		}},
		{"500 echoing the key", func(t *testing.T) error {
			s := serve(t, echo(500))
			return errOf(newProbe(t, "zai", s.URL).Read(context.Background(), cred))
		}},
		{"429 echoing the key", func(t *testing.T) error {
			s := serve(t, echo(429))
			return errOf(newProbe(t, "zai", s.URL).Read(context.Background(), cred))
		}},
		{"200 with an unparseable body echoing the key", func(t *testing.T) error {
			s := serve(t, echo(200))
			return errOf(newProbe(t, "zai", s.URL).Read(context.Background(), cred))
		}},
		{"oversized body echoing the key", func(t *testing.T) error {
			s := serve(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"pad":"` + strings.Repeat(testSecret, 200) + `"}`))
			})
			p := newProbe(t, "zai", s.URL, func(c *Config) { c.MaxBody = 64 })
			return errOf(p.Read(context.Background(), cred))
		}},
		{"transport error quoting a URL with the key in it", func(t *testing.T) error {
			// A closed listener, with the credential in the query string the
			// way a key-in-URL provider would need.
			dead := httptest.NewServer(jsonHandler(200, `{}`))
			base := dead.URL + "/?api_key=" + testSecret
			dead.Close()
			p := newProbe(t, "zai", base)
			return errOf(p.Read(context.Background(), cred))
		}},
		{"a token source that fails with the token in its message", func(t *testing.T) error {
			s := serve(t, jsonHandler(200, zaiOK))
			p := newProbe(t, "zai", s.URL)
			p.SetAuth(cred.ID(), AuthFunc(func(context.Context) (string, error) {
				return "", fmt.Errorf("refresh failed for token %s", testSecret)
			}))
			return errOf(p.Read(context.Background(), cred))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.read(t)
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), testSecret) {
				t.Fatalf("error leaks the credential: %v", err)
			}
		})
	}
}

// The recorded reason is kept as well as returned, so it gets the same check.
func TestHealthReasonIsScrubbed(t *testing.T) {
	t.Parallel()
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprintf(w, `{"message":"key %s is disabled"}`, testSecret)
	})
	p := newProbe(t, "zai", s.URL)
	cred := testCredential("zai")
	if _, err := p.Read(context.Background(), cred); err == nil {
		t.Fatal("want an error")
	}
	h := p.Health(cred.ID())
	if h.Reason == "" {
		t.Fatal("no reason recorded")
	}
	if strings.Contains(h.Reason, testSecret) {
		t.Fatalf("health reason leaks the credential: %q", h.Reason)
	}
}

// A snapshot is a record that outlives the read, so it gets the check too.
func TestSnapshotCarriesNoCredential(t *testing.T) {
	t.Parallel()
	// A provider that returns the key inside the fields the decoder reads.
	body := fmt.Sprintf(`{"code":200,"success":true,"data":{"limits":[
	  {"type":%q,"unit":3,"number":5,"percentage":10,"nextResetTime":1777819631597}]}}`, testSecret)
	s := serve(t, jsonHandler(200, body))
	p := newProbe(t, "zai", s.URL)
	snap, err := p.Read(context.Background(), testCredential("zai"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%+v", snap), testSecret) {
		t.Fatalf("snapshot carries the credential: %+v", snap)
	}
	// A label comes from the source's known vocabulary, so an unrecognized type
	// — a key echoed into the field — cannot appear at all, not even mangled.
	if len(snap.Windows) != 1 || snap.Windows[0].Label != "other:5h" {
		t.Errorf("label = %q, want %q", snap.Windows[0].Label, "other:5h")
	}
	if snap.Windows[0].Mapped {
		t.Error("an unrecognized limit type was filed under a quota key")
	}
}

func errOf[T any](_ T, err error) error { return err }

// A failed read closes the gate. Retrying immediately against an account-status
// endpoint is how a prober gets the account rate-limited, which is the failure
// it exists to prevent.
func TestFailureBacksOffAndDoesNotSpendARequest(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		jsonHandler(500, `{}`)(w, r)
	})
	p := newProbe(t, "zai", s.URL, func(c *Config) {
		c.BackoffBase = time.Minute
		c.BackoffCeiling = time.Hour
	})
	cred := testCredential("zai")

	if _, err := p.Read(context.Background(), cred); !errors.Is(err, ErrStatus) {
		t.Fatalf("first Read: %v", err)
	}
	_, err := p.Read(context.Background(), cred)
	if !errors.Is(err, ErrThrottled) {
		t.Fatalf("second Read = %v, want ErrThrottled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("requests = %d, want 1: the backoff spent another one", got)
	}
	h := p.Health(cred.ID())
	if h.NextAttempt.IsZero() {
		t.Error("no next attempt recorded")
	}
	// Jittered by ±20%, so a range rather than a value. Jitter is not
	// decoration: a fleet of credentials that failed together would otherwise
	// retry together, which is the burst the backoff exists to avoid.
	if d := time.Until(h.NextAttempt); d < 45*time.Second || d > 75*time.Second {
		t.Errorf("backoff = %v, want about a minute", d)
	}
}

// A 429's Retry-After outranks the prober's own backoff when it is longer.
func TestRetryAfterIsObeyed(t *testing.T) {
	t.Parallel()
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(429)
	})
	p := newProbe(t, "zai", s.URL, func(c *Config) { c.BackoffBase = time.Second })
	cred := testCredential("zai")
	if _, err := p.Read(context.Background(), cred); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Read: %v", err)
	}
	if d := time.Until(p.Health(cred.ID()).NextAttempt); d < 9*time.Minute {
		t.Errorf("next attempt in %v; the provider asked for ten minutes", d)
	}
}

// Within the floor, a read is answered from the last snapshot rather than by
// spending a request — and the fetch instant is preserved, so quota's staleness
// keeps telling the truth about how old the figures are.
func TestMinIntervalReplaysTheSnapshotWithoutLying(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		jsonHandler(200, zaiOK)(w, r)
	})
	p := newProbe(t, "zai", s.URL, func(c *Config) { c.MinInterval = time.Hour })
	cred := testCredential("zai")

	first, err := p.Read(context.Background(), cred)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	second, err := p.Read(context.Background(), cred)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("requests = %d, want 1", calls.Load())
	}
	if !second.FetchedAt.Equal(first.FetchedAt) {
		t.Errorf("the replayed snapshot claimed to be fresher than it is: %v vs %v",
			second.FetchedAt, first.FetchedAt)
	}
}

// The snapshot a caller receives is a copy: mutating it must not corrupt what
// the prober will replay.
func TestSnapshotIsCopied(t *testing.T) {
	t.Parallel()
	s := serve(t, jsonHandler(200, zaiOK))
	p := newProbe(t, "zai", s.URL)
	cred := testCredential("zai")
	snap, err := p.Read(context.Background(), cred)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	snap.Windows[0].UsedPercent = 999
	again, _ := p.Snapshot(cred.ID())
	if again.Windows[0].UsedPercent != 37 {
		t.Errorf("the retained snapshot was mutated through the returned copy")
	}
}

// An OAuth credential's token is replaced by its own refresher (DESIGN §11.2b),
// so the prober must resolve it per read rather than capture it once.
func TestOAuthTokenIsResolvedPerRead(t *testing.T) {
	t.Parallel()
	var seen []string
	var mu sync.Mutex
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		jsonHandler(200, zaiOK)(w, r)
	})
	p := newProbe(t, "zai", s.URL)
	var n atomic.Int64
	cred := testCredential("zai")
	p.SetAuth(cred.ID(), AuthFunc(func(context.Context) (string, error) {
		return fmt.Sprintf("rotating-token-%d", n.Add(1)), nil
	}))

	for range 2 {
		if _, err := p.Read(context.Background(), cred); err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] == seen[1] {
		t.Fatalf("authorization headers = %v; the second read reused the first token", seen)
	}
	if seen[1] != "Bearer rotating-token-2" { // pragma: allowlist secret — test fixture
		t.Errorf("second header = %q", seen[1])
	}
}

// A credential with no secret is not probed at all.
func TestEmptySecretIsRefused(t *testing.T) {
	t.Parallel()
	s := serve(t, jsonHandler(200, zaiOK))
	p := newProbe(t, "zai", s.URL)
	_, err := p.Read(context.Background(), quota.NewCredential("c", "zai", "   "))
	if !errors.Is(err, ErrNoSecret) {
		t.Fatalf("error = %v, want ErrNoSecret", err)
	}
}

// A redirect is not followed: it would resend the credential to whatever
// answered.
func TestRedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	var elsewhere atomic.Int64
	sink := serve(t, func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		jsonHandler(200, zaiOK)(w, r)
	})
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	})
	p := newProbe(t, "zai", s.URL)
	if _, err := p.Read(context.Background(), testCredential("zai")); !errors.Is(err, ErrStatus) {
		t.Fatalf("error = %v, want ErrStatus", err)
	}
	if elsewhere.Load() != 0 {
		t.Error("the credential was resent to the redirect target")
	}
}

// A response larger than the cap is refused rather than read into memory.
func TestOversizedBodyIsRefused(t *testing.T) {
	t.Parallel()
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		for range 1000 {
			_, _ = w.Write([]byte(strings.Repeat("x", 1024)))
		}
	})
	p := newProbe(t, "zai", s.URL, func(c *Config) { c.MaxBody = 4096 })
	if _, err := p.Read(context.Background(), testCredential("zai")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
}

// Two concurrent reads of one credential make one request: the second can only
// learn what the first is already learning, and doubling the load on an
// account-status endpoint is what rule 6 forbids.
func TestConcurrentReadsOfOneCredentialCoalesce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		jsonHandler(200, zaiOK)(w, r)
	})
	p := newProbe(t, "zai", s.URL)
	cred := testCredential("zai")

	done := make(chan error, 1)
	go func() {
		_, err := p.Read(context.Background(), cred)
		done <- err
	}()
	<-entered
	// No snapshot yet, so the coalesced read reports that it did not ask.
	if _, err := p.Read(context.Background(), cred); !errors.Is(err, ErrThrottled) {
		t.Errorf("concurrent read = %v, want ErrThrottled", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}
