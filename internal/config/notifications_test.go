package config

import (
	"strings"
	"testing"
	"time"
)

// TestNotificationDefaults pins the pipeline numbers of §11.5's delivery path.
// They are bounds rather than preferences, so a change to one of them should be
// a decision somebody made on purpose.
func TestNotificationDefaults(t *testing.T) {
	c := mustLoad(t)
	n := c.Notifications
	for _, tc := range []struct {
		name string
		got  any
		want any
	}{
		{"driver", n.Email.Driver, "none"},
		{"queue_size", n.QueueSize, 256},
		{"workers", n.Workers, 1},
		{"dedup_period", n.DedupPeriod.Duration(), time.Hour},
		{"retry.max_attempts", n.Retry.MaxAttempts, 3},
		{"retry.initial_backoff", n.Retry.InitialBackoff.Duration(), time.Second},
		{"retry.max_backoff", n.Retry.MaxBackoff.Duration(), 30 * time.Second},
		{"retry.breaker_threshold", n.Retry.BreakerThreshold, 5},
		{"retry.breaker_cooldown", n.Retry.BreakerCooldown.Duration(), time.Minute},
		{"smtp.timeout", n.Email.SMTP.Timeout.Duration(), 10 * time.Second},
		{"http.timeout", n.Email.HTTP.Timeout.Duration(), 10 * time.Second},
	} {
		if tc.got != tc.want {
			t.Errorf("notifications.%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestNotificationSMTPShape(t *testing.T) {
	t.Setenv("DORANG_TEST_SMTP_PASSWORD", "hunter2")
	c := mustLoad(t, `
notifications:
  email:
    driver: smtp
    from: dorang@example.com
    to: [ops@example.com, oncall@example.com]
    smtp:
      addr: smtp.example.com:587
      username: dorang
      key_env: DORANG_TEST_SMTP_PASSWORD
      starttls: true
      timeout: 5s
  events: [budget_80pct, budget_exceeded]
  dedup_period: 30m
  dedup_periods:
    quota_exhausted: 5m
`)
	e := c.Notifications.Email
	if e.Driver != "smtp" || e.SMTP.Addr != "smtp.example.com:587" {
		t.Fatalf("smtp settings = %+v", e.SMTP)
	}
	if len(e.To) != 2 || e.From != "dorang@example.com" {
		t.Errorf("recipients = %v from %q", e.To, e.From)
	}
	v, ok := e.SMTP.Password.Value()
	if !ok || v != "hunter2" {
		t.Errorf("the SMTP password did not resolve: %v", ok)
	}
	// The secret is never renderable.
	if strings.Contains(e.SMTP.Password.String(), "hunter2") {
		t.Error("the SMTP password must not render")
	}
	if c.Notifications.DedupPeriod.Duration() != 30*time.Minute {
		t.Errorf("dedup_period = %v", c.Notifications.DedupPeriod)
	}
	if c.Notifications.DedupPeriods["quota_exhausted"].Duration() != 5*time.Minute {
		t.Errorf("dedup_periods = %v", c.Notifications.DedupPeriods)
	}
}

func TestNotificationValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		path string
		msg  string
	}{
		{
			"smtp without an address",
			"notifications:\n  email:\n    driver: smtp\n    from: a@b.c\n    to: [d@e.f]\n",
			"notifications.email.smtp.addr", "host:port",
		},
		{
			"smtp address without a port",
			"notifications:\n  email:\n    driver: smtp\n    from: a@b.c\n    to: [d@e.f]\n" +
				"    smtp:\n      addr: smtp.example.com\n",
			"notifications.email.smtp.addr", "host:port",
		},
		{
			"smtp without a sender",
			"notifications:\n  email:\n    driver: smtp\n    to: [d@e.f]\n" +
				"    smtp:\n      addr: h:25\n",
			"notifications.email.from", "must be set",
		},
		{
			"smtp without a recipient",
			"notifications:\n  email:\n    driver: smtp\n    from: a@b.c\n" +
				"    smtp:\n      addr: h:25\n",
			"notifications.email.to", "at least one recipient",
		},
		{
			"a recipient that is not an address",
			"notifications:\n  email:\n    driver: http\n    to: [\"not-an-address\"]\n" +
				"    http:\n      url: https://example.invalid/hook\n      key_env: DORANG_TEST_FIXTURE_KEY\n",
			"notifications.email.to[0]", "not an email address",
		},
		{
			"a sender that is not an address",
			"notifications:\n  email:\n    driver: http\n    from: nobody\n" +
				"    http:\n      url: https://example.invalid/hook\n      key_env: DORANG_TEST_FIXTURE_KEY\n",
			"notifications.email.from", "not an email address",
		},
		{
			"http without a url",
			"notifications:\n  email:\n    driver: http\n    http:\n      key_env: DORANG_TEST_FIXTURE_KEY\n",
			"notifications.email.http.url", "must be set",
		},
		{
			"http with a url dorang cannot post to",
			"notifications:\n  email:\n    driver: http\n    http:\n      url: \"ftp://x/y\"\n" +
				"      key_env: DORANG_TEST_FIXTURE_KEY\n",
			"notifications.email.http.url", "http or https",
		},
		{
			// §11.5 rule 1: a webhook delivery is signed, so the secret is
			// required rather than optional.
			"http without a signing secret",
			"notifications:\n  email:\n    driver: http\n" +
				"    http:\n      url: https://example.invalid/hook\n",
			"notifications.email.http.key_env", "cannot tell a real delivery from a forged one",
		},
		{
			"an unknown event",
			"notifications:\n  events: [not_an_event]\n",
			"notifications.events[0]", "not a known value",
		},
		{
			"an unknown event in dedup_periods",
			"notifications:\n  dedup_periods:\n    not_an_event: 5m\n",
			`notifications.dedup_periods["not_an_event"]`, "not a known value",
		},
		{
			"a backoff ceiling below the floor",
			"notifications:\n  retry:\n    initial_backoff: 60s\n    max_backoff: 1s\n",
			"notifications.retry.max_backoff", "must not be shorter",
		},
		{
			"a negative queue",
			"notifications:\n  queue_size: -1\n",
			"notifications.queue_size", "must not be negative",
		},
		{
			// The lua driver delivers through the on_email hook. Selecting it
			// without the hook is a mail system that sends nothing.
			"the lua driver without the hook",
			"notifications:\n  email:\n    driver: lua\n",
			"notifications.email.driver", "extensions.lua.enabled is false",
		},
		{
			"the lua driver with on_email excluded",
			"notifications:\n  email:\n    driver: lua\n" +
				"extensions:\n  lua:\n    enabled: true\n    dir: /tmp/x\n    hooks: [on_request]\n" +
				"    limits: {instructions: 1000, memory_mb: 1, timeout: 1s}\n",
			"notifications.email.driver", "does not list it",
		},
		{
			"a password without a username",
			"notifications:\n  email:\n    driver: smtp\n    from: a@b.c\n    to: [d@e.f]\n" +
				"    smtp:\n      addr: h:25\n      starttls: true\n      key_env: DORANG_TEST_FIXTURE_KEY\n",
			"notifications.email.smtp.username", "alongside an SMTP password",
		},
		{
			"a username without STARTTLS",
			"notifications:\n  email:\n    driver: smtp\n    from: a@b.c\n    to: [d@e.f]\n" +
				"    smtp:\n      addr: h:25\n      username: u\n      key_env: DORANG_TEST_FIXTURE_KEY\n",
			"notifications.email.smtp.starttls", "unencrypted connection",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			if err == nil {
				t.Fatalf("expected a validation error at %s", tc.path)
			}
			if !hasProblem(err, tc.path, tc.msg) {
				t.Errorf("missing problem at %s (%s); got %v", tc.path, tc.msg, problemPaths(err))
			}
		})
	}
}

// TestLuaDriverWithTheHookEnabledValidates is the positive case for the
// cross-check above.
func TestLuaDriverWithTheHookEnabledValidates(t *testing.T) {
	if _, err := loadYAML(t, "notifications:\n  email:\n    driver: lua\n"+
		"extensions:\n  lua:\n    enabled: true\n    dir: /tmp/x\n"+
		"    hooks: [on_request, on_email]\n"+
		"    limits: {instructions: 1000, memory_mb: 1, timeout: 1s}\n"); err != nil {
		t.Fatalf("a lua driver with the hook enabled must validate: %v", err)
	}
}

// TestWebhookURLMayCarryAQuery: a shadow reference is joined with a request
// path so a query cannot survive it; a webhook URL is used whole, and every
// hosted webhook puts its token in one.
func TestWebhookURLMayCarryAQuery(t *testing.T) {
	if _, err := loadYAML(t, "notifications:\n  email:\n    driver: http\n"+
		"    http:\n      url: \"https://hooks.example.invalid/x?token=abc\"\n"+
		"      key_env: DORANG_TEST_FIXTURE_KEY\n"); err != nil {
		t.Fatalf("a webhook URL with a query must validate: %v", err)
	}
}
