package shadow

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HeaderShadow marks a request as a shadow copy.
//
// It is the loop guard, and it is not optional. If the reference gateway is
// another dorang with shadowing configured — which is exactly the arrangement
// "run alongside" produces while two gateways are being compared in both
// directions — then without a marker each shadow call is itself shadowed, and
// two gateways amplify one request into an unbounded exchange between them.
// [Shadower.Sample] refuses any request carrying it.
const HeaderShadow = "X-Dorang-Shadow"

// HeaderShadowRequestID carries the original request's id to the reference, so
// a difference found here can be traced to a row on both sides.
const HeaderShadowRequestID = "X-Dorang-Shadow-Request-Id"

// forwardedHeaders is the allow-list of client headers copied to the reference.
//
// An allow-list, not a deny-list. The client's credential must never reach the
// reference gateway — it is a different principal's key at a different vendor —
// and "we remembered to delete all six accepted spellings" (COMPATIBILITY §7.3)
// is a weaker guarantee than "we only ever copied these". Everything the wire
// protocols actually need is here; anything else is the caller's business with
// dorang, not with the reference.
// Accept-Encoding is deliberately absent. Setting it explicitly turns off Go's
// transparent gzip handling, so the reply would arrive compressed and every
// comparison would decide the reference sent an unreadable body. Leaving it to
// the transport keeps the comparison looking at JSON.
var forwardedHeaders = []string{
	"Content-Type",
	"Accept",
	"Anthropic-Version",
	"Anthropic-Beta",
	"OpenAI-Beta",
	"OpenAI-Organization",
	"OpenAI-Project",
}

// defaultClient is the client used when [Options] names none.
//
// Redirects are not followed, for the reason internal/server gives for the
// passthrough client: a 30x would re-issue the request, with the reference
// credential attached, to a host the reference chose.
func defaultClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

// reply is what came back from the reference.
type reply struct {
	status int
	header http.Header
	head   []byte
	tail   []byte
	// truncated says the body exceeded the read cap, so head and tail do not
	// meet.
	truncated bool
	total     int64
	// costNanoUSD is the reference's own reported cost when it supplied one,
	// and -1 when it did not. A dorang reference always attaches
	// x-dorang-cost-usd (DESIGN §10.4); nothing else is required to.
	costNanoUSD int64
	err         error
}

// call issues one reference request.
//
// It runs on a worker, never on the request path. Every failure it can produce
// — an unreachable host, a refused connection, a timeout, a 500, a truncated
// body — is a value in the returned reply, because the caller's only correct
// reaction to any of them is to record it and move on.
func (s *Shadower) call(ctx context.Context, j *job) *reply {
	rp := &reply{costNanoUSD: -1}

	u := url.URL{Scheme: s.scheme, Host: s.host, Path: s.prefix + j.path, RawQuery: j.rawQuery}
	var body io.Reader
	if len(j.reqBody) > 0 {
		body = bytes.NewReader(j.reqBody)
	}
	req, err := http.NewRequestWithContext(ctx, j.method, u.String(), body)
	if err != nil {
		rp.err = err
		return rp
	}
	req.ContentLength = int64(len(j.reqBody))

	for _, name := range forwardedHeaders {
		if v := j.reqHeader.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	// Defence in depth: the allow-list above cannot have copied a credential,
	// and this makes that true even if somebody adds a name to it carelessly.
	stripAuth(req.Header)

	if s.opts.ReferenceKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.opts.ReferenceKey)
		// The Anthropic family authenticates with x-api-key and ignores
		// Authorization. Setting both means one configuration works for both
		// families; the key goes only to the reference, which owns it.
		if strings.Contains(j.path, "/messages") || j.reqHeader.Get("Anthropic-Version") != "" {
			req.Header.Set("X-Api-Key", s.opts.ReferenceKey)
		}
	}
	req.Header.Set(HeaderShadow, "1")
	req.Header.Set(HeaderShadowRequestID, j.requestID)

	resp, err := s.opts.Client.Do(req)
	if err != nil {
		rp.err = err
		return rp
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	rp.status = resp.StatusCode
	rp.header = resp.Header.Clone()
	rp.head, rp.tail, rp.total, rp.truncated, err = readWindows(resp.Body, s.opts.MaxCaptureBytes, tailWindow)
	if err != nil {
		// A body that failed part-way through is still worth what was read: the
		// status and the headers are already a comparison, and the prefix is
		// marked truncated so nothing is claimed about the rest.
		rp.truncated = true
	}
	rp.costNanoUSD = reportedCost(resp.Header)
	return rp
}

// tailWindow is how much of a reference response's end is kept. It is smaller
// than the head because its only job is to carry the terminator.
const tailWindow = 16 << 10

// readWindows reads r into a head window and a tail window.
//
// Two windows for the same reason internal/server captures two: a stream's
// terminator is the last thing on the wire, and §14.1 requires it to be
// compared, so a head buffer alone loses exactly the thing the comparison is
// for.
func readWindows(r io.Reader, headMax, tailMax int) (head, tail []byte, total int64, truncated bool, err error) {
	head = make([]byte, 0, min(headMax, 32<<10))
	buf := make([]byte, 32<<10)
	ring := make([]byte, 0, tailMax)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			total += int64(n)
			p := buf[:n]
			if len(head) < headMax {
				take := headMax - len(head)
				if take > len(p) {
					take = len(p)
				}
				head = append(head, p[:take]...)
			}
			if tailMax > 0 {
				if len(p) >= tailMax {
					ring = append(ring[:0], p[len(p)-tailMax:]...)
				} else {
					if len(ring)+len(p) > tailMax {
						drop := len(ring) + len(p) - tailMax
						ring = append(ring[:0], ring[drop:]...)
					}
					ring = append(ring, p...)
				}
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				err = rerr
			}
			break
		}
	}
	if total > int64(len(head)) {
		tail = ring
		// Trim the overlap, so head and tail are disjoint and in order. When
		// they meet exactly the whole body was read and nothing is truncated.
		if gap := total - int64(len(head)) - int64(len(tail)); gap < 0 {
			tail = tail[-gap:]
		} else if gap > 0 {
			truncated = true
		}
	}
	return head, tail, total, truncated, err
}

// reportedCost reads a cost the reference gateway attached to its own response.
// Only dorang is required to (DESIGN §10.4); anything else returns -1 and the
// reservation estimate stands.
func reportedCost(h http.Header) int64 {
	v := h.Get("X-Dorang-Cost-Usd")
	if v == "" {
		return -1
	}
	n, ok := parseNanoUSD(v)
	if !ok {
		return -1
	}
	return n
}

// stripAuth removes every credential-bearing header spelling dorang accepts.
func stripAuth(h http.Header) {
	for _, name := range []string{
		"Authorization", "Api-Key", "X-Api-Key", "X-Goog-Api-Key",
		"Ocp-Apim-Subscription-Key", "X-Dorang-Api-Key",
	} {
		delete(h, name)
	}
	for k := range h {
		lk := strings.ToLower(k)
		if lk == "authorization" || strings.HasSuffix(lk, "api-key") ||
			lk == "ocp-apim-subscription-key" {
			delete(h, k)
		}
	}
}

// parseNanoUSD reads a plain decimal amount into nano-USD without going through
// a float. Money never goes through binary floating point (DESIGN §8.3).
func parseNanoUSD(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		s = s[1:]
	}
	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" && fracPart == "" {
		return 0, false
	}
	var whole int64
	for i := 0; i < len(intPart); i++ {
		c := intPart[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if whole > (1<<62)/10 {
			return 0, false
		}
		whole = whole*10 + int64(c-'0')
	}
	var frac int64
	for i := 0; i < 9; i++ {
		frac *= 10
		if i < len(fracPart) {
			c := fracPart[i]
			if c < '0' || c > '9' {
				return 0, false
			}
			frac += int64(c - '0')
		}
	}
	for i := 9; i < len(fracPart); i++ {
		if c := fracPart[i]; c < '0' || c > '9' {
			return 0, false
		}
	}
	const nano = 1_000_000_000
	if whole > (1<<62)/nano {
		return 0, false
	}
	n := whole*nano + frac
	if neg {
		n = -n
	}
	return n, true
}
