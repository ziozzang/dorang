package notify

import (
	"strings"
	"time"
)

// Event is one of DESIGN §11.5's notification events.
type Event uint8

const (
	// EventKeyCreated fires when an API key is issued. It carries the key's
	// identity and never the key.
	EventKeyCreated Event = iota
	// EventBudget80 fires the first time a subject's spend passes 80% of its
	// ceiling in a period (DESIGN §6.4).
	EventBudget80
	// EventBudgetExceeded fires when a reservation is refused for want of
	// budget.
	EventBudgetExceeded
	// EventQuotaExhausted fires when a provider quota window is spent (§6.1).
	EventQuotaExhausted
	// EventCredentialUnhealthy fires when a credential's circuit opens (§7.5a).
	EventCredentialUnhealthy
	// EventBatchCompleted fires when a batch job reaches a terminal state
	// (§11.1).
	EventBatchCompleted
	// EventInvite fires when a user is invited to a team (§11.4).
	EventInvite

	numEvents = 7
)

// eventNames is indexed by Event and matches internal/config's
// notificationEvents exactly.
var eventNames = [numEvents]string{
	"key_created", "budget_80pct", "budget_exceeded", "quota_exhausted",
	"credential_unhealthy", "batch_completed", "invite",
}

// String returns the configuration spelling.
func (e Event) String() string {
	if int(e) >= len(eventNames) {
		return "unknown"
	}
	return eventNames[e]
}

// ParseEvent resolves a configuration spelling.
func ParseEvent(s string) (Event, bool) {
	for i, n := range eventNames {
		if n == s {
			return Event(i), true
		}
	}
	return 0, false
}

// Events returns every event in configuration order.
func Events() []Event {
	out := make([]Event, numEvents)
	for i := range out {
		out[i] = Event(i)
	}
	return out
}

// titles are the human-readable headline for each event.
var titles = [numEvents]string{
	"API key created",
	"budget at 80%",
	"budget exceeded",
	"provider quota exhausted",
	"credential unhealthy",
	"batch completed",
	"team invitation",
}

// Title returns the subject-line headline.
func (e Event) Title() string {
	if int(e) >= len(titles) {
		return e.String()
	}
	return titles[e]
}

// Subject is what a notification is about: the thing an operator would
// deduplicate on. A budget alert for one key must not suppress the alert for
// another.
type Subject struct {
	Kind string
	ID   string
}

// String renders the subject.
func (s Subject) String() string {
	switch {
	case s.Kind == "" && s.ID == "":
		return "gateway"
	case s.ID == "":
		return s.Kind
	case s.Kind == "":
		return s.ID
	}
	return s.Kind + ":" + s.ID
}

// Field is one named value in a notification.
type Field struct {
	Name  string
	Value string
}

// Notification is what a caller posts. It is copied on the way into the queue,
// so the caller may reuse its Fields slice immediately.
type Notification struct {
	Event   Event
	Subject Subject
	// To overrides the configured recipients for this one message. Empty uses
	// the configured list, which is the normal case.
	To []string
	// Fields carry the detail. Names outside the event's allow-list are
	// dropped and counted; see [EventFields].
	Fields []Field
	// At is when the condition was observed. Zero uses the notifier's clock.
	At time.Time
}

// EventFields is the complete set of field names each event may carry.
//
// It is an allow-list rather than a deny-list on purpose. A deny-list is a
// record of what somebody remembered to forbid; an allow-list makes adding a
// field a decision, and the decision is exactly the moment to ask whether the
// field is a secret. Everything not listed is dropped and counted.
var EventFields = map[Event][]string{
	EventKeyCreated: {
		"key_id", "key_name", "user_id", "team_id", "models",
		"budget_usd", "expires_at", "created_by",
	},
	EventBudget80: {
		"subject", "period", "limit_usd", "spent_usd", "percent", "window_ends",
	},
	EventBudgetExceeded: {
		"subject", "period", "limit_usd", "spent_usd", "window_ends", "request_id",
	},
	EventQuotaExhausted: {
		"subject", "provider", "credential_id", "window", "limit", "used", "resets_at",
	},
	EventCredentialUnhealthy: {
		"credential_id", "provider", "deployment", "state",
		"consecutive_failures", "last_error", "cooldown",
	},
	EventBatchCompleted: {
		"batch_id", "status", "total", "completed", "failed", "cost_usd", "duration",
	},
	EventInvite: {
		"invite_id", "email", "team_id", "role", "expires_at", "accept_path",
	},
}

// deniedExact are field names that never reach a message whatever an allow-list
// says. `key_id` names a key; `key` is one.
var deniedExact = map[string]bool{
	"key": true, "api_key": true, "apikey": true, "token": true,
	"access_token": true, "refresh_token": true, "secret": true,
	"password": true, "passwd": true, "credential": true, "authorization": true,
	"auth": true, "bearer": true, "cookie": true, "pepper": true,
	"key_hash": true, "hash": true, "private_key": true, "client_secret": true,
}

// deniedSubstrings never belong in a field name at all.
var deniedSubstrings = []string{"secret", "password", "passwd", "apikey", "api_key", "pepper"}

// DeniedField reports whether a field name may never appear in a message. It is
// the second of the three gates the package documentation describes, and it
// applies even to a name somebody added to an allow-list.
func DeniedField(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if deniedExact[n] {
		return true
	}
	for _, s := range deniedSubstrings {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// allowedFields is EventFields as a lookup, built once.
var allowedFields = func() map[Event]map[string]bool {
	out := make(map[Event]map[string]bool, len(EventFields))
	for ev, names := range EventFields {
		m := make(map[string]bool, len(names))
		for _, n := range names {
			m[n] = true
		}
		out[ev] = m
	}
	return out
}()

// fieldAllowed reports whether a field name may appear in this event.
func fieldAllowed(ev Event, name string) bool {
	if DeniedField(name) {
		return false
	}
	m, ok := allowedFields[ev]
	return ok && m[strings.ToLower(strings.TrimSpace(name))]
}
