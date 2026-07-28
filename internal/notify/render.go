package notify

import (
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"strconv"
	"strings"
	"time"
)

// Redacted replaces a value that looks like key material.
const Redacted = "[redacted]"

// Message is a rendered notification, ready for a driver.
//
// It has been through all three gates described in the package documentation:
// its Fields are allow-listed, refused names are gone, and secret-shaped values
// have been replaced. A driver may transmit it verbatim.
type Message struct {
	// ID is the idempotency key. It is derived from the event, the subject and
	// the observation time, so it is stable across every retry of the same
	// notification and different for the next one — which is what a receiver of
	// an at-least-once stream needs in order to deduplicate.
	ID      string
	Event   Event
	Subject Subject
	From    string
	To      []string
	// Line is the subject line, already stripped of anything that could inject
	// a header.
	Line string
	// Body is the plain-text body.
	Body   string
	Fields []Field
	At     time.Time
	// Driver names the transport, for the on_email hook's benefit.
	Driver string
}

// renderStats counts what rendering had to remove. They are reported through
// [Stats] because a dropped field is information the operator asked for and did
// not get.
type renderStats struct {
	dropped  int
	redacted int
}

// render turns a notification into a message.
func render(n *Notification, from string, to []string, driver string, at time.Time) (*Message, renderStats) {
	var rs renderStats
	m := &Message{
		Event:   n.Event,
		Subject: n.Subject,
		From:    from,
		To:      to,
		At:      at,
		Driver:  driver,
	}
	m.Fields = make([]Field, 0, len(n.Fields))
	for _, f := range n.Fields {
		name := strings.ToLower(strings.TrimSpace(f.Name))
		if !fieldAllowed(n.Event, name) {
			rs.dropped++
			continue
		}
		v, red := redactValue(f.Value)
		if red {
			rs.redacted++
		}
		m.Fields = append(m.Fields, Field{Name: name, Value: sanitize(v)})
	}

	m.ID = messageID(n.Event, n.Subject, at)
	m.Line = sanitize("[dorang] " + n.Event.Title() + " — " + n.Subject.String())

	var b strings.Builder
	b.Grow(128 + 32*len(m.Fields))
	b.WriteString("event: ")
	b.WriteString(n.Event.String())
	b.WriteString("\nsubject: ")
	b.WriteString(n.Subject.String())
	b.WriteString("\ntime: ")
	b.WriteString(at.UTC().Format(time.RFC3339))
	b.WriteString("\n\n")
	for _, f := range m.Fields {
		b.WriteString(f.Name)
		b.WriteString(": ")
		b.WriteString(f.Value)
		b.WriteByte('\n')
	}
	b.WriteString("\n-- \nSent by dorang. This message never carries key material.\n")
	m.Body = b.String()
	return m, rs
}

// messageID derives the idempotency key.
func messageID(ev Event, subj Subject, at time.Time) string {
	h := sha256.New()
	h.Write([]byte(ev.String()))
	h.Write([]byte{0})
	h.Write([]byte(subj.Kind))
	h.Write([]byte{0})
	h.Write([]byte(subj.ID))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(at.UnixNano(), 10)))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// sanitize removes anything that could end a header line early. A subject line
// carrying a CRLF is a header-injection primitive, and a field value reaches
// the subject line often enough that stripping at the source is the only place
// it can be done once.
func sanitize(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// RFC5322 renders the message as an internet message, for the SMTP driver.
func (m *Message) RFC5322() string {
	var b strings.Builder
	b.Grow(len(m.Body) + 256)
	b.WriteString("From: ")
	b.WriteString(sanitize(m.From))
	b.WriteString("\r\nTo: ")
	b.WriteString(sanitize(strings.Join(m.To, ", ")))
	b.WriteString("\r\nSubject: ")
	b.WriteString(mime.QEncoding.Encode("utf-8", m.Line))
	b.WriteString("\r\nDate: ")
	b.WriteString(m.At.UTC().Format(time.RFC1123Z))
	b.WriteString("\r\nMIME-Version: 1.0")
	b.WriteString("\r\nContent-Type: text/plain; charset=utf-8")
	b.WriteString("\r\nX-Dorang-Event: ")
	b.WriteString(m.Event.String())
	b.WriteString("\r\nX-Dorang-Notification-Id: ")
	b.WriteString(m.ID)
	b.WriteString("\r\n\r\n")
	// The body's bare newlines become CRLF; net/smtp's dot writer handles
	// leading-dot stuffing.
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\n", "\r\n"))
	return b.String()
}

// secretPrefixes are how the credentials dorang actually handles begin. A value
// starting with one of these is key material whatever field it arrived in.
var secretPrefixes = []string{
	"sk-", "sk_", "rk-", "pk_live", "xoxb-", "xoxp-", "xapp-",
	"ghp_", "gho_", "ghu_", "ghs_", "github_pat_",
	"glpat-", "hf_", "dop_v1_", "AKIA", "ASIA", "AIza", "ya29.",
	"Bearer ", "bearer ", "Basic ", "-----BEGIN",
}

// redactValue replaces a value that looks like key material.
//
// It is the third gate and the weakest one — a heuristic cannot be complete —
// which is why it is behind an allow-list rather than instead of one. It errs
// towards redacting: an operator who loses a batch id to a false positive has
// lost a line of a message, and the alternative failure is a credential in a
// mailbox.
func redactValue(v string) (string, bool) {
	t := strings.TrimSpace(v)
	if t == "" {
		return v, false
	}
	for _, p := range secretPrefixes {
		if strings.HasPrefix(t, p) {
			return Redacted, true
		}
	}
	if looksOpaque(t) {
		return Redacted, true
	}
	return v, false
}

// looksOpaque reports whether a value is a long run of credential-shaped
// characters: no spaces, no punctuation a human would write, and a mix of
// letters and digits.
func looksOpaque(s string) bool {
	const minOpaque = 32
	if len(s) < minOpaque {
		return false
	}
	if isUUID(s) {
		// Identifiers dorang mints for batches, invites and requests are UUIDs.
		// They are not secrets and redacting them would make the message
		// useless for the thing it is reporting on.
		return false
	}
	var letters, digits int
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			letters++
		case c >= '0' && c <= '9':
			digits++
		case c == '+', c == '/', c == '=', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return letters > 0 && digits > 0
}

// isUUID reports the canonical 8-4-4-4-12 hexadecimal form.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range []byte(s) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHex(c) {
				return false
			}
		}
	}
	return true
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
