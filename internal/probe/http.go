package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultClient is the client used when the caller supplies none.
//
// Redirects are not followed, for the same reason internal/app does not follow
// them on the request path: a redirect from a provider endpoint is a
// misconfiguration or a captive portal, and following one resends the provider
// credential to whatever answered.
func defaultClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2: true,
			// A prober makes one small request per credential per interval, so
			// it keeps a small pool: an account-status endpoint is not a hot
			// path and holding connections open against it buys nothing.
			MaxIdleConns:          8,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

// statusError reports an unsuccessful response.
//
// What it deliberately does not contain is the body. A provider's error body
// can echo the key back, so wrapping it verbatim would put the credential
// straight into every log line that prints the error — the reasoning DESIGN
// §11.2b applies to a refresher's errors, applied here. The status code and the
// classification are what a caller can act on; the body is what a caller would
// leak.
type statusError struct {
	sentinel   error
	provider   string
	code       int
	retryAfter time.Duration
}

func (e *statusError) Error() string {
	s := fmt.Sprintf("%s: %s: status %d", e.sentinel, e.provider, e.code)
	if e.retryAfter > 0 {
		s += fmt.Sprintf(" (retry after %s)", e.retryAfter.Round(time.Second))
	}
	return s
}

func (e *statusError) Unwrap() error { return e.sentinel }

// RetryAfter is what the provider asked for, zero when it asked for nothing.
func (e *statusError) RetryAfter() time.Duration { return e.retryAfter }

// retryAfterOf extracts a provider's own back-off request from an error.
func retryAfterOf(err error) time.Duration {
	var ra interface{ RetryAfter() time.Duration }
	if errors.As(err, &ra) {
		return ra.RetryAfter()
	}
	return 0
}

// do performs one request and returns the body, bounded.
func (p *Prober) do(ctx context.Context, st *credState, req *http.Request) ([]byte, error) {
	resp, err := p.cl.Do(req)
	if err != nil {
		// A context that is already done explains the failure by itself, and
		// its error is a sentinel with nothing provider-supplied in it. Prefer
		// it: the transport's own message quotes the URL.
		if ce := ctx.Err(); ce != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrTransport, p.id, ce)
		}
		return nil, fmt.Errorf("%w: %s: %s", ErrTransport, p.id, st.scrub.text(err.Error()))
	}
	defer func() {
		// Drain a bounded amount so the connection can be reused, then close.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode/100 != 2 {
		return nil, p.statusErr(resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, p.cfg.MaxBody+1))
	if err != nil {
		if ce := ctx.Err(); ce != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrTransport, p.id, ce)
		}
		return nil, fmt.Errorf("%w: %s: reading body: %s", ErrTransport, p.id, st.scrub.text(err.Error()))
	}
	if int64(len(body)) > p.cfg.MaxBody {
		return nil, fmt.Errorf("%w: %s: over %d bytes", ErrTooLarge, p.id, p.cfg.MaxBody)
	}
	return body, nil
}

// statusErr classifies an unsuccessful response.
func (p *Prober) statusErr(resp *http.Response) error {
	e := &statusError{provider: p.id, code: resp.StatusCode}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		e.sentinel = ErrUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests:
		e.sentinel = ErrRateLimited
		e.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	default:
		e.sentinel = ErrStatus
		if resp.StatusCode/100 == 5 {
			// A 503 may carry a Retry-After too, and obeying it is the whole of
			// rule 6: the endpoint that reports a rate limit is the endpoint a
			// prober can get rate-limited against.
			e.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		}
	}
	return e
}

// maxRetryAfter caps what a provider can ask for. An hour is already far longer
// than any poll interval; a larger value is more likely a header bug than a
// genuine request, and honoring it would silence a credential for a day.
const maxRetryAfter = time.Hour

// parseRetryAfter reads both spellings RFC 9110 allows: delay-seconds and an
// HTTP-date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		return clampRetryAfter(time.Duration(secs) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return clampRetryAfter(t.Sub(now))
	}
	return 0
}

func clampRetryAfter(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return 0
	case d > maxRetryAfter:
		return maxRetryAfter
	}
	return d
}

// newRequest builds a JSON GET for a source.
func newRequest(ctx context.Context, url, userAgent string, header http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

// baseOr returns the configured base URL or the provider's documented one,
// without a trailing slash.
func baseOr(base, fallback string) string {
	if strings.TrimSpace(base) == "" {
		return fallback
	}
	return strings.TrimRight(strings.TrimSpace(base), "/")
}
