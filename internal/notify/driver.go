package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Driver names, matching internal/config's emailDrivers.
const (
	DriverSMTP = "smtp"
	DriverHTTP = "http"
	DriverLua  = "lua"
	DriverNone = "none"
)

// Driver delivers a rendered message.
//
// It is deliberately narrow: everything a driver could want to decide —
// whether the event is enabled, whether it is a duplicate, what the message
// says, whether a field is a secret — has already been decided. A driver
// transmits, and reports whether the transmission worked.
type Driver interface {
	// Name is the configuration spelling.
	Name() string
	// Deliver sends the message. A non-nil error is retried under the
	// notifier's bounded backoff and then dropped.
	Deliver(ctx context.Context, m *Message) error
}

// ErrNoTransport reports a driver that can filter but not send. It is returned
// by the lua driver when no extension registered an email transport, so a
// configuration that cannot deliver says so instead of counting deliveries that
// never happened.
var ErrNoTransport = errors.New("notify: the lua driver has no on_email transport registered")

// --- none -------------------------------------------------------------------

// noneDriver accepts and discards. It is the default, and it is not an error
// state: `driver: none` is how an operator turns notifications off.
type noneDriver struct{}

func (noneDriver) Name() string                            { return DriverNone }
func (noneDriver) Deliver(context.Context, *Message) error { return nil }

// --- smtp -------------------------------------------------------------------

// SMTPOptions configures the smtp driver.
type SMTPOptions struct {
	// Addr is host:port.
	Addr string
	// Username and Password authenticate. Password is a resolved secret and is
	// never rendered into a message or a log line.
	Username string
	Password string
	// StartTLS upgrades the connection when the server advertises it.
	// Authentication over a plaintext connection is refused by net/smtp for
	// anything but localhost, which is the behaviour we want.
	StartTLS bool
	// TLSSkipVerify disables certificate verification. It exists for a private
	// relay with a self-signed certificate and is off by default.
	TLSSkipVerify bool
	// Timeout bounds the whole exchange, connection included.
	Timeout time.Duration
	// HELO is the name dorang announces itself as.
	HELO string
	// Dial overrides the dialer. Tests use it; production leaves it nil.
	Dial func(ctx context.Context, addr string) (net.Conn, error)
}

type smtpDriver struct{ o SMTPOptions }

func (d *smtpDriver) Name() string { return DriverSMTP }

func (d *smtpDriver) Deliver(ctx context.Context, m *Message) error {
	if len(m.To) == 0 {
		return errors.New("notify: smtp: no recipient")
	}
	timeout := d.o.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := d.dial(ctx)
	if err != nil {
		return fmt.Errorf("notify: smtp: dial: %w", err)
	}
	// One deadline for the whole exchange. Without it a server that accepts the
	// connection and then says nothing holds a worker forever, which is the
	// same outage the queue exists to prevent, one layer down.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	host, _, err := net.SplitHostPort(d.o.Addr)
	if err != nil {
		host = d.o.Addr
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("notify: smtp: greeting: %w", err)
	}
	defer c.Close()

	helo := d.o.HELO
	if helo == "" {
		helo = "localhost"
	}
	if err := c.Hello(helo); err != nil {
		return fmt.Errorf("notify: smtp: helo: %w", err)
	}
	if d.o.StartTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			cfg := &tls.Config{ServerName: host, InsecureSkipVerify: d.o.TLSSkipVerify} //nolint:gosec // opt-in
			if err := c.StartTLS(cfg); err != nil {
				return fmt.Errorf("notify: smtp: starttls: %w", err)
			}
		}
	}
	if d.o.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", d.o.Username, d.o.Password, host)); err != nil {
			// The password is in the auth object, never in the error: net/smtp
			// reports the server's reply, not the credential.
			return fmt.Errorf("notify: smtp: auth: %w", err)
		}
	}
	if err := c.Mail(m.From); err != nil {
		return fmt.Errorf("notify: smtp: mail from: %w", err)
	}
	for _, to := range m.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("notify: smtp: rcpt to: %w", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("notify: smtp: data: %w", err)
	}
	if _, err := io.WriteString(w, m.RFC5322()); err != nil {
		return fmt.Errorf("notify: smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("notify: smtp: close: %w", err)
	}
	return c.Quit()
}

func (d *smtpDriver) dial(ctx context.Context) (net.Conn, error) {
	if d.o.Dial != nil {
		return d.o.Dial(ctx, d.o.Addr)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", d.o.Addr)
}

// --- http -------------------------------------------------------------------

// HTTPOptions configures the http driver, which POSTs the notification to a
// webhook.
type HTTPOptions struct {
	// URL receives the POST.
	URL string
	// Secret signs the delivery (DESIGN §11.5 rule 1). internal/config makes it
	// required for this driver: the payload carries budget and quota state, and
	// a receiver with no secret cannot tell a real delivery from a forged one.
	Secret string
	// Timeout bounds the request.
	Timeout time.Duration
	// Headers are added to the request. They are configuration, not secrets:
	// anything sensitive belongs in the URL's own credential handling.
	Headers map[string]string
	// Client overrides the HTTP client.
	Client *http.Client
}

// Webhook headers.
const (
	// HeaderSignature carries the HMAC as `t=<unix>,v1=<hex sha256>`. The
	// timestamp is inside the signed string, not only beside it, so a captured
	// delivery cannot be replayed with a fresh one.
	HeaderSignature = "X-Dorang-Signature"
	// HeaderIdempotencyKey is stable across retries of the same notification,
	// which is what makes at-least-once delivery safe to receive.
	HeaderIdempotencyKey = "X-Dorang-Idempotency-Key"
	// HeaderEvent names the event.
	HeaderEvent = "X-Dorang-Event"
)

// sign returns the value of [HeaderSignature] for a body.
//
// The signed string is "<unix seconds>.<body>". Signing the body alone would
// let anyone who ever observed one delivery repeat it forever; signing the
// timestamp separately would let it be rewritten. A receiver checks the MAC and
// then rejects a timestamp outside its own tolerance.
func sign(secret string, at time.Time, body []byte) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	m.Write([]byte{'.'})
	m.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

type httpDriver struct {
	o      HTTPOptions
	client *http.Client
}

func (d *httpDriver) Name() string { return DriverHTTP }

// webhook is the wire shape. It is the message and nothing else: the same
// redacted fields the SMTP driver would have sent.
type webhook struct {
	// ID is the idempotency key: stable across retries of this notification,
	// so a receiver of an at-least-once stream can deduplicate.
	ID        string            `json:"id"`
	Event     string            `json:"event"`
	Subject   string            `json:"subject"`
	Kind      string            `json:"subject_kind,omitempty"`
	SubjectID string            `json:"subject_id,omitempty"`
	To        []string          `json:"to,omitempty"`
	Line      string            `json:"subject_line"`
	Body      string            `json:"body"`
	Fields    map[string]string `json:"fields,omitempty"`
	At        string            `json:"at"`
}

func (d *httpDriver) Deliver(ctx context.Context, m *Message) error {
	if d.o.URL == "" {
		return errors.New("notify: http: no url")
	}
	w := webhook{
		ID:        m.ID,
		Event:     m.Event.String(),
		Subject:   m.Subject.String(),
		Kind:      m.Subject.Kind,
		SubjectID: m.Subject.ID,
		To:        m.To,
		Line:      m.Line,
		Body:      m.Body,
		At:        m.At.UTC().Format(time.RFC3339),
	}
	if len(m.Fields) > 0 {
		w.Fields = make(map[string]string, len(m.Fields))
		for _, f := range m.Fields {
			w.Fields[f.Name] = f.Value
		}
	}
	buf, err := json.Marshal(&w)
	if err != nil {
		return fmt.Errorf("notify: http: encode: %w", err)
	}

	timeout := d.o.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.o.URL, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("notify: http: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderEvent, m.Event.String())
	req.Header.Set(HeaderIdempotencyKey, m.ID)
	if d.o.Secret != "" {
		req.Header.Set(HeaderSignature, sign(d.o.Secret, m.At, buf))
	}
	// Configured headers are applied last but cannot overwrite the signature:
	// a header map that could rewrite the MAC would make the MAC advisory.
	for k, v := range d.o.Headers {
		if strings.EqualFold(k, HeaderSignature) {
			continue
		}
		req.Header.Set(k, v)
	}

	client := d.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: http: post: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify: http: webhook answered %d", resp.StatusCode)
	}
	return nil
}

// --- lua --------------------------------------------------------------------

// EmailHook is DESIGN §11.5's on_email extension point.
//
// internal/luaext implements the hooks and internal/app adapts them onto this
// interface, so neither package imports the other (DESIGN §1). The hook runs
// for *every* driver as a filter; it is also the transport for the `lua`
// driver.
type EmailHook interface {
	// OnEmail filters and, when CanDeliver reports true, delivers. A true
	// dropped suppresses the message; a non-nil error is a delivery failure.
	OnEmail(ctx context.Context, m *Message) (dropped bool, err error)
	// CanDeliver reports whether the hook is a transport as well as a filter.
	CanDeliver() bool
}

type luaDriver struct {
	hook EmailHook
}

func (d *luaDriver) Name() string { return DriverLua }

func (d *luaDriver) Deliver(ctx context.Context, m *Message) error {
	if d.hook == nil || !d.hook.CanDeliver() {
		return ErrNoTransport
	}
	dropped, err := d.hook.OnEmail(ctx, m)
	if err != nil {
		return err
	}
	if dropped {
		return errDropped
	}
	return nil
}

// errDropped is internal: it distinguishes "the extension suppressed this" from
// "delivery failed", so a suppression is counted as a suppression and never
// retried.
var errDropped = errors.New("notify: suppressed by the on_email hook")

// newDriver builds the configured transport.
func newDriver(o *Options) (Driver, error) {
	switch strings.ToLower(strings.TrimSpace(o.Driver)) {
	case "", DriverNone:
		return noneDriver{}, nil
	case DriverSMTP:
		if o.SMTP.Addr == "" {
			return nil, errors.New("notify: driver smtp needs notifications.email.smtp.addr")
		}
		if o.From == "" {
			return nil, errors.New("notify: driver smtp needs notifications.email.from")
		}
		return &smtpDriver{o: o.SMTP}, nil
	case DriverHTTP:
		if o.HTTP.URL == "" {
			return nil, errors.New("notify: driver http needs notifications.email.http.url")
		}
		if o.HTTP.Secret == "" {
			return nil, errors.New("notify: driver http needs a signing secret: " +
				"a webhook delivery is signed (DESIGN §11.5), and an unsigned receiver " +
				"cannot tell a real delivery from a forged one")
		}
		return &httpDriver{o: o.HTTP, client: o.HTTP.Client}, nil
	case DriverLua:
		return &luaDriver{hook: o.Hook}, nil
	}
	return nil, fmt.Errorf("notify: %q is not a driver; want one of smtp, http, lua, none", o.Driver)
}
