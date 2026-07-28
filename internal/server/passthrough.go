package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// Passthrough authentication modes (DESIGN §10.6 step 1).
const (
	// PassthroughAuthDorang authenticates with a dorang key and replaces the
	// credential with the provider's. The default, and the only mode where a
	// caller never sees a provider secret.
	PassthroughAuthDorang = "dorang"
	// PassthroughAuthClient forwards the caller's own credential to the
	// provider and does not authenticate locally. For a prefix that fronts a
	// provider the caller already has an account with.
	PassthroughAuthClient = "client"
	// PassthroughAuthNone authenticates nothing and forwards nothing. Only
	// sensible for a local, already-protected backend.
	PassthroughAuthNone = "none"
)

// PassthroughRoute opens one provider-native prefix (DESIGN §10.6).
//
// Nothing is open by default and nothing is inferred: a prefix that is not
// configured is not served. COMPATIBILITY §9 records that in the audited
// deployment no provider credential existed for any vendor catch-all and
// passthrough was disabled everywhere, which is why the engine ships disabled
// and why "unmapped prefixes are not served" is a security property rather than
// a convenience.
type PassthroughRoute struct {
	// Prefix is the path prefix to open, e.g. "/anthropic". It is stripped
	// before the remainder is joined onto BaseURL.
	Prefix string
	// Provider is the provider id, reported in headers and metering. Never a
	// secret.
	Provider string
	// BaseURL is the upstream base, e.g. "https://api.example.com/v1".
	BaseURL string
	// Auth is one of the Passthrough auth modes; "" means dorang.
	Auth string
	// Meter enables best-effort accounting for this prefix.
	Meter bool
	// Timeout bounds one relayed request; 0 uses the server's request timeout.
	Timeout time.Duration
	// Credential applies the provider credential to the outbound header. It is
	// a function rather than a string so that no secret is stored in a config
	// struct this package can log, and so that rotation is the credential
	// store's business.
	Credential func(http.Header)
	// AllowWebSocket enables the frame-for-frame relay of step 6.
	AllowWebSocket bool
	// Dial overrides the dialer for a WebSocket upgrade. Nil dials directly.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// passthroughRoute is the compiled form.
type passthroughRoute struct {
	prefix     string
	provider   string
	base       *url.URL
	basePath   string
	auth       string
	meter      bool
	timeout    time.Duration
	credential func(http.Header)
	allowWS    bool
	dial       func(ctx context.Context, network, addr string) (net.Conn, error)
}

// ptParam is the wildcard name used by every compiled passthrough pattern.
const ptParam = "ptpath"

// ErrBadPassthrough reports a passthrough route that cannot be compiled.
var ErrBadPassthrough = errors.New("server: invalid passthrough route")

// compilePassthrough turns configured prefixes into routes.
func compilePassthrough(routes []PassthroughRoute) ([]*Route, error) {
	out := make([]*Route, 0, len(routes))
	seen := make(map[string]struct{}, len(routes))
	for i := range routes {
		r := routes[i]
		if r.Prefix == "" || r.Prefix[0] != '/' || r.Prefix == "/" {
			return nil, ErrBadPassthrough
		}
		r.Prefix = strings.TrimRight(r.Prefix, "/")
		if _, dup := seen[r.Prefix]; dup {
			return nil, ErrBadPassthrough
		}
		seen[r.Prefix] = struct{}{}

		base, err := url.Parse(r.BaseURL)
		if err != nil || base.Scheme == "" || base.Host == "" {
			return nil, ErrBadPassthrough
		}
		auth := r.Auth
		if auth == "" {
			auth = PassthroughAuthDorang
		}
		switch auth {
		case PassthroughAuthDorang, PassthroughAuthClient, PassthroughAuthNone:
		default:
			return nil, ErrBadPassthrough
		}
		pt := &passthroughRoute{
			prefix:     r.Prefix,
			provider:   r.Provider,
			base:       base,
			basePath:   strings.TrimRight(base.Path, "/"),
			auth:       auth,
			meter:      r.Meter,
			timeout:    r.Timeout,
			credential: r.Credential,
			allowWS:    r.AllowWebSocket,
			dial:       r.Dial,
		}
		out = append(out, &Route{
			Pattern: r.Prefix + "/{" + ptParam + "...}",
			Methods: MethodGET | MethodHEAD | MethodPOST | MethodPUT |
				MethodPATCH | MethodDELETE | MethodOPTIONS,
			Name:      "passthrough:" + r.Prefix,
			Family:    FamilyPassthrough,
			Public:    auth != PassthroughAuthDorang,
			NeedsBody: false, // step 3: relay without parsing, streaming
			Handler:   passthroughHandler(pt),
		})
	}
	return out, nil
}

// defaultPassthroughClient builds the client used when none is supplied.
//
// Redirects are never followed. A 30x from an upstream would make the client
// re-issue the request — with the provider credential attached — to a host the
// upstream chose, which turns any compromised or misconfigured backend into a
// credential exfiltration primitive. Relaying the 30x to the caller instead is
// both safer and more honest.
func defaultPassthroughClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

// hopByHop are the headers that belong to a single transport hop and must not
// be forwarded in either direction (RFC 9110 §7.6.1).
var hopByHop = [...]string{
	"Connection", "Proxy-Connection", "Keep-Alive",
	"Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// stripHopByHop removes the hop-by-hop set, including the headers that
// Connection itself names — which is the half a hand-written list always
// forgets, and the half an attacker uses to smuggle one.
func stripHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = textproto.TrimString(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHop {
		h.Del(name)
	}
}

// relayBufferSize bounds the streaming relay's working set. The body is never
// held whole, in either direction: DESIGN §15.5 prohibits full-body buffering
// past the replay budget, and a 16 MiB response through this path allocates
// this much and no more.
const relayBufferSize = 32 << 10

var relayBufPool = sync.Pool{New: func() any { b := make([]byte, relayBufferSize); return &b }}

// usageScanLimit bounds how much of a JSON response the best-effort meter will
// look at. Step 5 of §10.6 wants usage priced when the response carries it,
// and step 3 forbids parsing the relayed body — the reconciliation is a bounded
// tee: a small JSON answer is scanned, anything larger or streamed is metered
// by counts and bytes alone.
const usageScanLimit = 64 << 10

// passthroughHandler serves one configured prefix.
func passthroughHandler(pt *passthroughRoute) Handler {
	return func(w http.ResponseWriter, rq *Request) error {
		return rq.srv.servePassthrough(w, rq, pt)
	}
}

func (s *Server) servePassthrough(w http.ResponseWriter, rq *Request, pt *passthroughRoute) error {
	s.metrics.passthrough.Add(1)
	r := rq.HTTP

	target, e := pt.target(r)
	if e != nil {
		return e
	}
	rq.Result.Provider = pt.provider
	rq.Result.Deployment = pt.provider

	if pt.allowWS && isWebSocketUpgrade(r.Header) {
		return s.relayWebSocket(w, rq, pt, target)
	}

	cfg := s.snap.Load()
	ctx := rq.Context()
	if pt.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(rq.HTTP.Context(), pt.timeout)
		defer cancel()
	}

	var body io.Reader
	if r.Body != nil && r.Body != http.NoBody {
		body = http.MaxBytesReader(nil, r.Body, cfg.maxBody)
	}
	out, err := http.NewRequestWithContext(ctx, r.Method, target.String(), body)
	if err != nil {
		return NewError(http.StatusBadGateway, TypeAPIError,
			"could not build the upstream request").WithCode("passthrough_bad_target")
	}
	out.ContentLength = r.ContentLength
	out.Header = passthroughHeader(r.Header, rq, pt)

	resp, err := s.ptClient.Do(out)
	if err != nil {
		return passthroughDialError(err)
	}
	defer resp.Body.Close()

	h := w.Header()
	copyResponseHeaders(h, resp.Header)
	w.WriteHeader(resp.StatusCode)
	rq.bytesIn = r.ContentLength
	if rq.bytesIn < 0 {
		rq.bytesIn = 0
	}
	if r.Method == http.MethodHead {
		return nil
	}
	usage := relayBody(w, resp)
	if pt.meter {
		rq.Result.Tokens = usage
	}
	return nil
}

// target computes the upstream URL for a relayed request.
//
// This is the security boundary of §10.6: the prefix is stripped, the remainder
// is normalized, traversal is rejected, and the result is checked to still live
// under the configured base path. Everything that is not a configured prefix
// never reaches here at all — the route table has no entry for it.
func (pt *passthroughRoute) target(r *http.Request) (*url.URL, *Error) {
	dec := r.URL.Path
	if !strings.HasPrefix(dec, pt.prefix) {
		return nil, passthroughRefusal("path does not lie under the configured prefix")
	}
	rest := dec[len(pt.prefix):]
	if rest != "" && rest[0] != '/' {
		// /anthropicX must not be served by the /anthropic prefix.
		return nil, passthroughRefusal("path does not lie under the configured prefix")
	}
	if e := validateRelayPath(rest); e != nil {
		return nil, e
	}

	joined := pt.basePath + rest
	if joined == "" {
		joined = "/"
	}
	cleaned := path.Clean(joined)
	if strings.HasSuffix(joined, "/") && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	if !withinBase(cleaned, pt.basePath) {
		return nil, passthroughRefusal("normalized path escapes the configured base path")
	}

	out := *pt.base
	out.Path = cleaned
	out.RawPath = ""
	// Preserve percent-encoding the caller chose: %2F inside a segment is not
	// a path separator upstream and re-encoding it as one changes the request.
	if esc := r.URL.EscapedPath(); esc != dec && strings.HasPrefix(esc, pt.prefix) {
		if raw := pt.basePath + esc[len(pt.prefix):]; raw != cleaned {
			out.RawPath = raw
		}
	}
	out.RawQuery = r.URL.RawQuery
	out.Fragment = ""
	out.RawFragment = ""
	return &out, nil
}

// validateRelayPath rejects a path that must never be joined onto a base URL.
//
// The check runs on the *decoded* path, which is what catches both spellings of
// a traversal: net/http has already turned %2e%2e into "..", so a single rule
// covers the encoded and unencoded forms rather than a blocklist that covers
// whichever one the author thought of.
func validateRelayPath(p string) *Error {
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c < 0x20, c == 0x7f:
			return passthroughRefusal("path contains a control character")
		case c == '\\':
			return passthroughRefusal("path contains a backslash")
		}
	}
	for rest := p; rest != ""; {
		rest = strings.TrimPrefix(rest, "/")
		seg := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seg, rest = rest[:i], rest[i:]
		} else {
			rest = ""
		}
		if seg == ".." {
			return passthroughRefusal("path traversal is not permitted")
		}
	}
	return nil
}

// withinBase reports whether p is base or lies under it.
func withinBase(p, base string) bool {
	if base == "" || base == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == base || strings.HasPrefix(p, base+"/")
}

// passthroughRefusal builds the 400 for a path the engine will not relay.
func passthroughRefusal(msg string) *Error {
	return NewError(http.StatusBadRequest, TypeInvalidRequest, msg).
		WithCode("passthrough_path_rejected").WithParam("path")
}

// passthroughDialError maps a transport failure onto a status.
func passthroughDialError(err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return NewError(http.StatusGatewayTimeout, TypeTimeout,
			"upstream did not respond within the passthrough timeout").
			WithCode("passthrough_timeout")
	}
	if errors.Is(err, context.Canceled) {
		// The client hung up. Nothing will read the answer; say so honestly
		// rather than reporting an upstream failure that did not happen.
		return NewError(499, TypeInvalidRequest, "client closed the request").
			WithCode("client_closed_request")
	}
	// The transport error text can name internal hosts and ports. It is
	// deliberately not relayed.
	return NewError(http.StatusBadGateway, TypeAPIError,
		"could not reach the upstream provider").WithCode("passthrough_unreachable")
}

// passthroughHeader builds the outbound header set.
//
// Step 4 of §10.6: replace only dorang-owned headers, strip hop-by-hop. And
// COMPATIBILITY §7.3: all six accepted authentication headers are stripped
// before forwarding, whichever one the caller used — in the client-credential
// mode they are deliberately kept, which is the entire meaning of that mode.
func passthroughHeader(in http.Header, rq *Request, pt *passthroughRoute) http.Header {
	out := make(http.Header, len(in)+2)
	for k, vs := range in {
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	stripHopByHop(out)
	out.Del(HeaderDetail)
	out.Del(HeaderUsageEvents)

	if pt.auth != PassthroughAuthClient {
		StripAuthHeaders(out)
	}
	if pt.credential != nil {
		pt.credential(out)
	}
	// The request id travels upstream so a provider-side trace and dorang's
	// ledger row share a join key.
	out.Set(HeaderRequestID, rq.ID)
	return out
}

// copyResponseHeaders relays the upstream's headers to the client.
//
// Hop-by-hop headers are removed, and so is every authentication header: a
// backend that echoes its own credential — or the one dorang just sent it —
// into a response header must not have that relayed to the caller. Provider
// credentials never reach the client (§10.6, security boundary), and that has
// to hold even when the provider is the one leaking them.
func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	stripHopByHop(dst)
	StripAuthHeaders(dst)
}

// relayBody streams the response through a bounded buffer.
//
// The body is never parsed and never held whole (§10.6 step 3). A JSON answer
// small enough to be worth it is teed into a bounded scratch buffer so the
// best-effort meter can read a usage object out of it; past that limit the tee
// simply stops and the request is metered by bytes.
func relayBody(w http.ResponseWriter, resp *http.Response) Usage {
	bufp := relayBufPool.Get().(*[]byte)
	defer relayBufPool.Put(bufp)
	buf := *bufp

	scan := jsonish(resp.Header.Get("Content-Type"))
	var tee []byte
	if scan {
		tee = make([]byte, 0, 1024)
	}

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if scan {
				if len(tee)+n <= usageScanLimit {
					tee = append(tee, buf[:n]...)
				} else {
					scan, tee = false, nil
				}
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return Usage{}
			}
			flush(w)
		}
		if err != nil {
			break
		}
	}
	if scan && len(tee) > 0 {
		if u, ok := scanUsage(tee); ok {
			return u
		}
	}
	return Usage{}
}

// jsonish reports whether a content type is worth scanning for usage.
func jsonish(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	return ct == "application/json" || strings.HasSuffix(ct, "+json")
}

// flush pushes bytes to the client after every chunk. A relay that does not
// flush turns a streaming upstream into a buffered one, which is invisible in a
// test and immediately obvious to anyone watching tokens appear.
func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
		return
	}
	_ = http.NewResponseController(w).Flush()
}
