package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const at0 = "2026-07-28T12:00:00Z"

func testTime() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) }

// TestKeyCreatedCannotCarryTheKey is DESIGN §11.5's "no secret in an email"
// stated as the case that matters most: the notification that fires at exactly
// the moment a secret exists.
//
// The caller here does the wrong thing on purpose — it attaches the key under
// four different names — and none of them survive.
func TestKeyCreatedCannotCarryTheKey(t *testing.T) {
	const key = "sk-live-9f2b41c7ea5d4e8a1b3c5d7f9e0a2b4c" // pragma: allowlist secret — test fixture

	n := &Notification{
		Event:   EventKeyCreated,
		Subject: Subject{Kind: "key", ID: "key-01"},
		Fields: []Field{
			{"key_id", "key-01"},
			{"key_name", "ci-runner"},
			{"key", key},
			{"api_key", key},
			{"secret", key},
			{"token", key},
			{"Authorization", "Bearer " + key},
		},
	}
	m, rs := render(n, "dorang@example.com", []string{"ops@example.com"}, DriverSMTP, testTime())

	if rs.dropped != 5 {
		t.Errorf("dropped %d fields, want the 5 that are not on the allow-list", rs.dropped)
	}
	rendered := m.Line + "\n" + m.Body + "\n" + m.RFC5322()
	if strings.Contains(rendered, key) || strings.Contains(rendered, "sk-live") { // pragma: allowlist secret — test fixture
		t.Fatalf("the key reached the message:\n%s", rendered)
	}
	for _, banned := range []string{"\nkey:", "\napi_key:", "\nsecret:", "\ntoken:", "authorization"} {
		if strings.Contains(strings.ToLower(rendered), banned) {
			t.Errorf("the message carries a %q field:\n%s", banned, rendered)
		}
	}
	// What it should carry is still there.
	for _, want := range []string{"key_id: key-01", "key_name: ci-runner"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("the message lost %q:\n%s", want, m.Body)
		}
	}
}

// TestNoKeyMaterialReachesAnyDriver is the same guarantee end to end: whatever
// the caller attaches, what leaves the process carries none of it.
func TestNoKeyMaterialReachesAnyDriver(t *testing.T) {
	const key = "sk-live-9f2b41c7ea5d4e8a1b3c5d7f9e0a2b4c" // pragma: allowlist secret — test fixture

	bodies := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- string(b)
	}))
	defer srv.Close()

	smtpSrv := newFakeSMTP(t)

	for _, tc := range []struct {
		name string
		set  func(*Options)
		read func() string
	}{
		{"http", func(o *Options) {
			o.Driver = DriverHTTP
			o.HTTP = HTTPOptions{URL: srv.URL, Secret: testWebhookSecret, Client: srv.Client()} // pragma: allowlist secret — test fixture
		}, func() string {
			select {
			case b := <-bodies:
				return b
			case <-time.After(5 * time.Second):
				t.Fatal("no webhook call")
				return ""
			}
		}},
		{"smtp", func(o *Options) {
			o.Driver = DriverSMTP
			o.SMTP = SMTPOptions{Addr: smtpSrv.addr(), Timeout: 5 * time.Second}
		}, func() string {
			waitFor(t, "the message", func() bool { return len(smtpSrv.messages()) > 0 })
			return strings.Join(smtpSrv.messages(), "\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, _ := newNotifier(t, tc.set)
			n.Notify(Notification{
				Event:   EventKeyCreated,
				Subject: Subject{Kind: "key", ID: "key-01"},
				Fields: []Field{
					{"key_id", "key-01"},
					{"key", key},
					{"key_name", key}, // an allow-listed field carrying a secret
				},
			})
			got := tc.read()
			if strings.Contains(got, key) || strings.Contains(got, "sk-live") { // pragma: allowlist secret — test fixture
				t.Fatalf("key material left the process:\n%s", got)
			}
			if !strings.Contains(got, Redacted) {
				t.Errorf("an allow-listed field with a secret value must be redacted:\n%s", got)
			}
		})
	}
}

func TestRenderRedactsSecretShapedValues(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"sk-abcdef", true},      // pragma: allowlist secret — test fixture
		{"sk_live_abcdef", true}, // pragma: allowlist secret — test fixture
		{"Bearer abc", true},
		{"ghp_0123456789abcdefghij", true},
		{"AKIAIOSFODNN7EXAMPLE", true},        // pragma: allowlist secret — test fixture
		{"AIzaSyA-1234", true},                // pragma: allowlist secret — test fixture
		{"-----BEGIN PRIVATE KEY-----", true}, // pragma: allowlist secret — test fixture
		{"glpat-abcdefghij", true},
		{"a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8", true}, // long, opaque, mixed // pragma: allowlist secret — test fixture
		{"10.00", false},
		{"81", false},
		{"plan-a", false},
		{"ci-runner", false},
		{"2026-07-28T12:00:00Z", false},
		{"9f2b41c7-ea5d-4e8a-9b3c-5d7f9e0a2b4c", false}, // a UUID is an id, not a secret
		{"", false},
		{"a batch of 400 requests completed", false},
		{strings.Repeat("a", 40), false}, // long but no digits: prose, not a token
	} {
		got, red := redactValue(tc.value)
		if red != tc.want {
			t.Errorf("redactValue(%q) redacted=%v, want %v", tc.value, red, tc.want)
		}
		if red && got != Redacted {
			t.Errorf("redactValue(%q) = %q", tc.value, got)
		}
	}
}

func TestDeniedFieldNames(t *testing.T) {
	for _, name := range []string{
		"key", "api_key", "apikey", "token", "access_token", "refresh_token",
		"secret", "client_secret", "password", "credential", "authorization",
		"bearer", "cookie", "pepper", "key_hash", "hash", "private_key",
		"KEY", " Api_Key ", "the_secret_thing",
	} {
		if !DeniedField(name) {
			t.Errorf("DeniedField(%q) = false", name)
		}
	}
	for _, name := range []string{"key_id", "key_name", "limit_usd", "provider", "batch_id"} {
		if DeniedField(name) {
			t.Errorf("DeniedField(%q) = true; it is an identifier, not a credential", name)
		}
	}
}

// TestAllowListsAndRefusalListDoNotOverlap: the two gates have to be
// independent, or the second one is decoration.
func TestAllowListsAndRefusalListDoNotOverlap(t *testing.T) {
	for ev, names := range EventFields {
		for _, name := range names {
			if DeniedField(name) {
				t.Errorf("%s allows %q, which the refusal list forbids", ev, name)
			}
			if !fieldAllowed(ev, name) {
				t.Errorf("%s allows %q but fieldAllowed says no", ev, name)
			}
		}
	}
	if fieldAllowed(EventKeyCreated, "provider") {
		t.Error("a field from another event's list must not be accepted")
	}
	if fieldAllowed(Event(99), "anything") {
		t.Error("an unknown event allows nothing")
	}
}

// TestHeaderInjectionIsStripped: a field value reaches the subject line, so a
// value carrying CRLF is a header-injection primitive.
func TestHeaderInjectionIsStripped(t *testing.T) {
	n := &Notification{
		Event:   EventInvite,
		Subject: Subject{Kind: "user", ID: "u\r\nBcc: attacker@example.com"},
		Fields: []Field{
			{"email", "victim@example.com\r\nBcc: attacker@example.com"},
			{"role", "member"},
		},
	}
	m, _ := render(n, "dorang@example.com", []string{"ops@example.com"}, DriverSMTP, testTime())
	if strings.ContainsAny(m.Line, "\r\n") {
		t.Errorf("the subject line carries a line break: %q", m.Line)
	}
	// The subject survives as Q-encoded text, which is the correct outcome: the
	// bytes are carried, they are just not a header any more. What must not
	// exist is a header line the caller wrote.
	head, _, _ := strings.Cut(m.RFC5322(), "\r\n\r\n")
	want := map[string]bool{"from": true, "to": true, "subject": true, "date": true,
		"mime-version": true, "content-type": true, "x-dorang-event": true,
		"x-dorang-notification-id": true}
	for _, line := range strings.Split(head, "\r\n") {
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue // a folded continuation of the header above
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok || !want[strings.ToLower(name)] {
			t.Errorf("a header was injected: %q\n%s", line, head)
		}
	}
	for _, f := range m.Fields {
		if strings.ContainsAny(f.Value, "\r\n") {
			t.Errorf("field %q carries a line break", f.Name)
		}
	}
}

func TestRFC5322Shape(t *testing.T) {
	n := &Notification{
		Event:   EventBudget80,
		Subject: Subject{Kind: "key", ID: "k-1"},
		Fields:  []Field{{"limit_usd", "10.00"}, {"spent_usd", "8.10"}},
	}
	m, _ := render(n, "dorang@example.com", []string{"a@example.com", "b@example.com"},
		DriverSMTP, testTime())
	msg := m.RFC5322()
	for _, want := range []string{
		"From: dorang@example.com\r\n",
		"To: a@example.com, b@example.com\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
		"X-Dorang-Event: budget_80pct\r\n",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message is missing %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(m.Body, "time: "+at0) {
		t.Errorf("the body does not carry the observation time:\n%s", m.Body)
	}
	if strings.Contains(msg, "\n\n") && !strings.Contains(msg, "\r\n\r\n") {
		t.Error("the message has a bare newline separator")
	}
}

func TestSubjectLineNamesTheEventAndSubject(t *testing.T) {
	m, _ := render(&Notification{Event: EventQuotaExhausted,
		Subject: Subject{Kind: "credential", ID: "acct-1"}},
		"a@b", []string{"c@d"}, DriverSMTP, testTime())
	if !strings.Contains(m.Line, "quota") || !strings.Contains(m.Line, "credential:acct-1") {
		t.Errorf("subject line = %q", m.Line)
	}
}

func TestWebhookCarriesOnlyRedactedFields(t *testing.T) {
	m, _ := render(&Notification{
		Event:   EventKeyCreated,
		Subject: Subject{Kind: "key", ID: "k"},
		Fields:  []Field{{"key_id", "k"}, {"key", "sk-secret-value"}}, // pragma: allowlist secret — test fixture
	}, "a@b", []string{"c@d"}, DriverHTTP, testTime())

	w := webhook{Event: m.Event.String(), Fields: map[string]string{}}
	for _, f := range m.Fields {
		w.Fields[f.Name] = f.Value
	}
	buf, err := json.Marshal(&w)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(buf), "sk-secret-value") { // pragma: allowlist secret — test fixture
		t.Fatalf("the webhook payload carries key material: %s", buf)
	}
}

// --- webhook signing --------------------------------------------------------

// TestWebhookDeliveryIsSigned is DESIGN §11.5 rule 1. The receiver recomputes
// the MAC over exactly what it received and gets the same answer, and a body a
// forger changed does not verify.
func TestWebhookDeliveryIsSigned(t *testing.T) {
	type delivery struct {
		body   []byte
		header http.Header
	}
	ch := make(chan delivery, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- delivery{body: b, header: r.Header.Clone()}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP = HTTPOptions{
			URL: srv.URL, Secret: testWebhookSecret, Client: srv.Client(), // pragma: allowlist secret — test fixture
			// A configured header must not be able to rewrite the signature.
			Headers: map[string]string{HeaderSignature: "t=1,v1=forged"},
		}
	})
	n.Notify(Notification{Event: EventQuotaExhausted,
		Subject: Subject{Kind: "credential", ID: "acct-1"}})

	var d delivery
	select {
	case d = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}

	sig := d.header.Get(HeaderSignature)
	if sig == "" || sig == "t=1,v1=forged" {
		t.Fatalf("signature = %q; a configured header must not overwrite it", sig)
	}
	tsPart, macPart, ok := strings.Cut(sig, ",")
	if !ok || !strings.HasPrefix(tsPart, "t=") || !strings.HasPrefix(macPart, "v1=") {
		t.Fatalf("signature is not t=…,v1=…: %q", sig)
	}
	ts, err := strconv.ParseInt(strings.TrimPrefix(tsPart, "t="), 10, 64)
	if err != nil {
		t.Fatalf("timestamp: %v", err)
	}

	// The receiver's half of the contract.
	mac := hmac.New(sha256.New, []byte(testWebhookSecret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte{'.'})
	mac.Write(d.body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(strings.TrimPrefix(macPart, "v1="))) {
		t.Fatalf("the signature does not verify over the delivered body")
	}

	// A forged body does not verify against the delivered signature.
	forged := append(append([]byte(nil), d.body...), ' ')
	mac.Reset()
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte{'.'})
	mac.Write(forged)
	if hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(want)) {
		t.Fatal("a modified body produced the same MAC")
	}

	// The signature never carries the secret itself.
	if strings.Contains(sig, testWebhookSecret) {
		t.Fatal("the signature header leaks the secret")
	}
	if strings.Contains(string(d.body), testWebhookSecret) {
		t.Fatal("the payload leaks the signing secret")
	}
}

// TestIdempotencyKeyIsStableAcrossRetries: at-least-once delivery is only safe
// to receive if a retry is recognisable as the same message.
func TestIdempotencyKeyIsStableAcrossRetries(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get(HeaderIdempotencyKey))
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP = HTTPOptions{URL: srv.URL, Secret: testWebhookSecret, Client: srv.Client()} // pragma: allowlist secret — test fixture
	})
	n.Notify(Notification{Event: EventBatchCompleted, Subject: Subject{Kind: "batch", ID: "b-1"}})
	waitFor(t, "the delivery to succeed after retries", func() bool { return n.Stats().Delivered == 1 })

	mu.Lock()
	defer mu.Unlock()
	if len(keys) < 3 {
		t.Fatalf("expected retries, saw %d attempts", len(keys))
	}
	for _, k := range keys {
		if k == "" {
			t.Fatal("a delivery carried no idempotency key")
		}
		if k != keys[0] {
			t.Fatalf("the key changed between retries: %q then %q", keys[0], k)
		}
	}
}

// TestMessageIDDistinguishesNotifications: stable per notification, different
// between them, or a receiver deduplicating on it would swallow real alerts.
func TestMessageIDDistinguishesNotifications(t *testing.T) {
	base := testTime()
	a := messageID(EventBudget80, Subject{Kind: "key", ID: "k1"}, base)
	if a != messageID(EventBudget80, Subject{Kind: "key", ID: "k1"}, base) {
		t.Error("the same notification must produce the same id")
	}
	for _, other := range []string{
		messageID(EventBudgetExceeded, Subject{Kind: "key", ID: "k1"}, base),
		messageID(EventBudget80, Subject{Kind: "key", ID: "k2"}, base),
		messageID(EventBudget80, Subject{Kind: "user", ID: "k1"}, base),
		messageID(EventBudget80, Subject{Kind: "key", ID: "k1"}, base.Add(time.Nanosecond)),
	} {
		if other == a {
			t.Errorf("a different notification produced the same id %q", a)
		}
	}
}
