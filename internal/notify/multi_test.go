package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type webhookSink struct {
	mu     sync.Mutex
	bodies []string
	status int
}

func newWebhook(t *testing.T, status int) (*webhookSink, string) {
	t.Helper()
	s := &webhookSink{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(b))
		code := s.status
		s.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func (s *webhookSink) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEveryTransportDeliversIndependently: email and webhook each get the
// notification, a failing webhook does not stop the email, and the delivery
// callback names exactly the transports that delivered.
func TestEveryTransportDeliversIndependently(t *testing.T) {
	mail := newFakeSMTP(t)
	hook, url := newWebhook(t, http.StatusInternalServerError)
	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = ""
		o.Drivers = []string{DriverSMTP, DriverHTTP}
		o.SMTP = SMTPOptions{Addr: mail.addr(), Timeout: 2 * time.Second}
		o.HTTP = HTTPOptions{URL: url, Secret: "s3cret-signing"}
	})
	if got := n.Driver(); got != "smtp,http" {
		t.Fatalf("Driver() = %q", got)
	}
	var mu sync.Mutex
	var delivered []string
	n.Send(Notification{Event: EventRemainingLow, Subject: Subject{Kind: "credential", ID: "openrouter-1"},
		Fields:    []Field{{Name: "percent_left", Value: "4.10"}},
		Delivered: func(d string) { mu.Lock(); delivered = append(delivered, d); mu.Unlock() }})

	waitUntil(t, func() bool { return len(mail.messages()) == 1 && len(hook.got()) == 3 })
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(delivered, ",") != "smtp" {
		t.Errorf("delivered = %v, want smtp alone (the webhook failed every attempt)", delivered)
	}
}

func TestOnlyRestrictsTheTransports(t *testing.T) {
	mail := newFakeSMTP(t)
	hook, url := newWebhook(t, http.StatusOK)
	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = ""
		o.Drivers = []string{DriverSMTP, DriverHTTP}
		o.SMTP = SMTPOptions{Addr: mail.addr(), Timeout: 2 * time.Second}
		o.HTTP = HTTPOptions{URL: url, Secret: "s3cret-signing"}
	})
	n.Send(Notification{Event: EventRemainingLow, Only: []string{DriverHTTP}})
	waitUntil(t, func() bool { return len(hook.got()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if len(mail.messages()) != 0 {
		t.Error("a retry meant for the webhook also sent the email")
	}
}

func TestChatWebhookFormats(t *testing.T) {
	for _, f := range []struct{ format, key string }{{"slack", "text"}, {"discord", "content"}} {
		t.Run(f.format, func(t *testing.T) {
			hook, url := newWebhook(t, http.StatusOK)
			n, _ := newNotifier(t, func(o *Options) {
				o.Driver = DriverHTTP
				o.HTTP = HTTPOptions{URL: url, Secret: "s3cret-signing", Format: f.format}
			})
			n.Send(Notification{Event: EventRemainingLow, Subject: Subject{Kind: "credential", ID: "c1"},
				Fields: []Field{{Name: "percent_left", Value: "3.00"}}})
			waitUntil(t, func() bool { return len(hook.got()) == 1 })
			var doc map[string]string
			if err := json.Unmarshal([]byte(hook.got()[0]), &doc); err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(doc))
			for k := range doc {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != f.key || !strings.Contains(doc[f.key], "3.00") {
				t.Errorf("body = %v", doc)
			}
		})
	}
}

func TestDriversAreValidated(t *testing.T) {
	for _, o := range []Options{
		{Driver: DriverHTTP, Drivers: []string{DriverSMTP}},
		{Drivers: []string{DriverHTTP, DriverHTTP}},
		{Drivers: []string{DriverNone, DriverHTTP}},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("New(%+v) was accepted", o)
		}
	}
}

// TestTemplateFormatRendersAnyReceiversShape renders home-monitor's body,
// {"title","text","level"}, and sends a token header taken from the options.
func TestTemplateFormatRendersAnyReceiversShape(t *testing.T) {
	var gotAuth string
	var mu sync.Mutex
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body, gotAuth = string(b), r.Header.Get("Authorization")
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP = HTTPOptions{URL: srv.URL, Secret: "s3cret-signing", Format: "template",
			Template: `{"title": {{json .Title}}, "text": {{json .Body}}, "level": {{json .Level}}, ` +
				`"key": {{json .Key}}, "left": {{json (index .Fields "percent_left")}}}`,
			Headers: map[string]string{"Authorization": "Bearer tok-123"}}
	})
	n.Send(Notification{Event: EventQuotaExhausted, Subject: Subject{Kind: "credential", ID: "ollama-1"},
		Fields: []Field{{Name: "provider", Value: "ollama-cloud"}}})
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return body != "" })
	mu.Lock()
	defer mu.Unlock()
	var doc map[string]string
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if doc["level"] != "error" || doc["key"] != "quota_exhausted:credential:ollama-1" ||
		!strings.Contains(doc["text"], "ollama-cloud") || doc["title"] == "" || doc["left"] != "" {
		t.Errorf("doc = %v", doc)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestTemplateFormatNeedsAValidTemplate(t *testing.T) {
	for _, tmpl := range []string{"", "{{ .Nope "} {
		if _, err := New(Options{Driver: DriverHTTP, HTTP: HTTPOptions{URL: "http://x", Secret: "s",
			Format: "template", Template: tmpl}}); err == nil {
			t.Errorf("template %q was accepted", tmpl)
		}
	}
}

func TestTemplateStatusFollowsTheTransition(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	n, _ := newNotifier(t, func(o *Options) {
		o.Driver = DriverHTTP
		o.HTTP = HTTPOptions{URL: srv.URL, Secret: "s3cret-signing", Format: "template",
			Template: `{{.Status}} {{.Level}} {{.Title}}`,
			Headers:  map[string]string{"Content-Type": "text/plain"}}
	})
	n.Send(Notification{Event: EventRemainingLow, Subject: Subject{Kind: "credential", ID: "c1"},
		Fields: []Field{{Name: "state", Value: "low"}}})
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(bodies) == 1 })
	n.Send(Notification{Event: EventRemainingLow, Subject: Subject{Kind: "credential", ID: "c1"},
		Fields: []Field{{Name: "state", Value: "recovered"}}})
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(bodies) == 2 })
	mu.Lock()
	defer mu.Unlock()
	if !strings.HasPrefix(bodies[0], "problem warn ") || !strings.HasPrefix(bodies[1], "ok info ") ||
		!strings.Contains(bodies[1], "recovered: running low") {
		t.Errorf("bodies = %q", bodies)
	}
}

func TestATemplateThatBreaksJSONIsRefused(t *testing.T) {
	_, err := New(Options{Driver: DriverHTTP, HTTP: HTTPOptions{URL: "http://x", Secret: "s",
		Format: "template", Template: `{"text": "{{.Body}}"}`}})
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("err = %v", err)
	}
	// The same splice is fine in a body declared as plain text.
	if _, err := New(Options{Driver: DriverHTTP, HTTP: HTTPOptions{URL: "http://x", Secret: "s",
		Format: "template", Template: `{{.Title}}: {{.Body}}`,
		Headers: map[string]string{"Content-Type": "text/plain"}}}); err != nil {
		t.Fatalf("a text/plain template was refused: %v", err)
	}
}
