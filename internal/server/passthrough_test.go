package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstreamRecorder captures what the passthrough engine actually sent.
type upstreamRecorder struct {
	mu     sync.Mutex
	last   *http.Request
	body   string
	handle func(w http.ResponseWriter, r *http.Request)
}

func (u *upstreamRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.last = r.Clone(context.Background())
	u.body = string(b)
	u.mu.Unlock()
	if u.handle != nil {
		u.handle(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"upstream":"ok"}`)
}

func (u *upstreamRecorder) request(t *testing.T) *http.Request {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.last == nil {
		t.Fatal("upstream was never called")
	}
	return u.last
}

// providerSecret is the credential that must never reach a client.
const providerSecret = "sk-provider-DO-NOT-LEAK" // pragma: allowlist secret — test fixture

// newPassthroughServer wires a dorang server in front of a fake provider.
func newPassthroughServer(t *testing.T, rec *upstreamRecorder, mut func(*PassthroughRoute)) (*Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(rec)
	t.Cleanup(up.Close)

	route := PassthroughRoute{
		Prefix:   "/anthropic",
		Provider: "anthropic-main",
		BaseURL:  up.URL + "/base",
		Meter:    true,
		Credential: func(h http.Header) {
			h.Set(HeaderXAPIKey, providerSecret)
		},
	}
	if mut != nil {
		mut(&route)
	}
	s := newTestServer(t, func(o *Options) { o.Passthrough = []PassthroughRoute{route} })
	return s, up
}

// TestPassthroughRelaysBodyBothWays without parsing it (DESIGN §10.6 step 3).
func TestPassthroughRelaysBodyBothWays(t *testing.T) {
	rec := &upstreamRecorder{}
	s, _ := newPassthroughServer(t, rec, nil)

	// A body that is not JSON at all: the engine must not care.
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages?beta=true",
		strings.NewReader("\x00\x01 not json \xff"))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	r.Header.Set("Content-Type", "application/octet-stream")
	w := do(s, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"upstream":"ok"}` {
		t.Errorf("response body %q", w.Body.String())
	}
	got := rec.request(t)
	if got.URL.Path != "/base/v1/messages" {
		t.Errorf("upstream path %q, want /base/v1/messages", got.URL.Path)
	}
	if got.URL.RawQuery != "beta=true" {
		t.Errorf("upstream query %q", got.URL.RawQuery)
	}
	rec.mu.Lock()
	body := rec.body
	rec.mu.Unlock()
	if body != "\x00\x01 not json \xff" {
		t.Errorf("upstream body %q — the engine altered it", body)
	}
}

// TestPassthroughUnmappedPrefixIsRefused. "Unmapped prefixes are not served —
// this is not an open proxy" is the security boundary of §10.6, and the way it
// is enforced is that no route exists, so the request lands on the same 501 as
// any other unknown path rather than on a relay that guesses a host.
func TestPassthroughUnmappedPrefixIsRefused(t *testing.T) {
	rec := &upstreamRecorder{}
	s, _ := newPassthroughServer(t, rec, nil)
	for _, p := range []string{"/openai/v1/chat/completions", "/vllm/generate", "/anthropicX/v1"} {
		w := do(s, post(p, `{}`))
		if w.Code != http.StatusNotImplemented {
			t.Errorf("%s: status %d, want 501", p, w.Code)
		}
	}
	rec.mu.Lock()
	called := rec.last != nil
	rec.mu.Unlock()
	if called {
		t.Fatal("an unmapped prefix reached an upstream")
	}
}

// TestPassthroughRejectsTraversal covers both spellings. net/http decodes %2e%2e
// before the handler sees it, so validating the decoded path catches the encoded
// form too — which is why the check is not a blocklist of literal "..".
func TestPassthroughRejectsTraversal(t *testing.T) {
	rec := &upstreamRecorder{}
	s, _ := newPassthroughServer(t, rec, nil)

	cases := []string{
		"/anthropic/../secret",
		"/anthropic/v1/../../secret",
		"/anthropic/%2e%2e/secret",
		"/anthropic/%2E%2E/%2E%2E/etc/passwd",
		"/anthropic/v1/..%2fsecret",
		"/anthropic/v1\\..\\secret",
	}
	for _, p := range cases {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		r.Header.Set(HeaderAuthorization, "Bearer good")
		w := do(s, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", p, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), "passthrough_path_rejected") {
			t.Errorf("%s: body %s", p, w.Body.String())
		}
	}
	rec.mu.Lock()
	called := rec.last != nil
	rec.mu.Unlock()
	if called {
		t.Fatal("a traversal attempt reached an upstream")
	}
}

// TestPassthroughCredentialNeverReachesTheClient is the other half of the
// security boundary. dorang attaches the provider credential going out; it must
// appear nowhere in what comes back, including when the provider itself echoes
// it into a response header.
func TestPassthroughCredentialNeverReachesTheClient(t *testing.T) {
	rec := &upstreamRecorder{
		handle: func(w http.ResponseWriter, r *http.Request) {
			// A provider that reflects the credential it was given. Some do.
			w.Header().Set(HeaderXAPIKey, r.Header.Get(HeaderXAPIKey))
			w.Header().Set(HeaderAuthorization, "Bearer "+providerSecret)
			w.Header().Set("X-Harmless", "kept")
			w.WriteHeader(200)
			io.WriteString(w, `{"ok":true}`)
		},
	}
	s, _ := newPassthroughServer(t, rec, nil)

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader("{}"))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	w := do(s, r)

	for name, vs := range w.Header() {
		for _, v := range vs {
			if strings.Contains(v, providerSecret) {
				t.Errorf("provider credential leaked in response header %s: %s", name, v)
			}
		}
	}
	if strings.Contains(w.Body.String(), providerSecret) {
		t.Error("provider credential leaked in the response body")
	}
	if w.Header().Get("X-Harmless") != "kept" {
		t.Error("an unrelated upstream header was dropped")
	}

	// And the outbound side: the client credential is gone, the provider's is
	// there. COMPATIBILITY §7.3.
	out := rec.request(t)
	if out.Header.Get(HeaderAuthorization) != "" {
		t.Errorf("client credential forwarded upstream: %q", out.Header.Get(HeaderAuthorization))
	}
	if out.Header.Get(HeaderXAPIKey) != providerSecret {
		t.Errorf("provider credential not attached: %q", out.Header.Get(HeaderXAPIKey))
	}
}

// TestPassthroughClientAuthModeForwardsTheCallersCredential — the one mode
// where forwarding is the point, and the reason stripping is a mode decision
// rather than an unconditional rule.
func TestPassthroughClientAuthModeForwardsTheCallersCredential(t *testing.T) {
	rec := &upstreamRecorder{}
	s, _ := newPassthroughServer(t, rec, func(r *PassthroughRoute) {
		r.Auth = PassthroughAuthClient
		r.Credential = nil
	})
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader("{}"))
	r.Header.Set(HeaderXAPIKey, "caller-own-key")
	w := do(s, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if got := rec.request(t).Header.Get(HeaderXAPIKey); got != "caller-own-key" {
		t.Errorf("caller credential %q, want it forwarded", got)
	}
}

// TestPassthroughStripsHopByHopBothWays, including the headers Connection
// itself names — the half a hand-written list always forgets, and the half an
// attacker uses to smuggle one.
func TestPassthroughStripsHopByHopBothWays(t *testing.T) {
	rec := &upstreamRecorder{
		handle: func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Connection", "X-Response-Hop")
			h.Set("X-Response-Hop", "should-not-survive")
			h.Set("Keep-Alive", "timeout=5")
			h.Set("X-Kept", "yes")
			w.WriteHeader(200)
		},
	}
	s, _ := newPassthroughServer(t, rec, nil)

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader("{}"))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	r.Header.Set("Connection", "X-Request-Hop, Keep-Alive")
	r.Header.Set("X-Request-Hop", "should-not-survive")
	r.Header.Set("Proxy-Authorization", "Basic zzz")
	r.Header.Set("X-Sent", "yes")
	w := do(s, r)

	out := rec.request(t)
	for _, name := range []string{"Connection", "X-Request-Hop", "Keep-Alive", "Proxy-Authorization"} {
		if v := out.Header.Get(name); v != "" {
			t.Errorf("hop-by-hop header %s forwarded upstream: %q", name, v)
		}
	}
	if out.Header.Get("X-Sent") != "yes" {
		t.Error("an ordinary request header was dropped")
	}

	for _, name := range []string{"X-Response-Hop", "Keep-Alive"} {
		if v := w.Header().Get(name); v != "" {
			t.Errorf("hop-by-hop header %s relayed to the client: %q", name, v)
		}
	}
	if w.Header().Get("X-Kept") != "yes" {
		t.Error("an ordinary response header was dropped")
	}
}

// TestPassthroughDoesNotFollowRedirects. A 30x from an upstream would make the
// client library re-issue the request — with the provider credential attached —
// to a host the upstream chose. Relaying the 30x is both safer and more honest.
func TestPassthroughDoesNotFollowRedirects(t *testing.T) {
	var attacker *httptest.Server
	attacker = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("redirect was followed to the attacker host with %q",
			r.Header.Get(HeaderXAPIKey))
		w.WriteHeader(200)
	}))
	defer attacker.Close()

	rec := &upstreamRecorder{
		handle: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, attacker.URL+"/collect", http.StatusFound)
		},
	}
	s, _ := newPassthroughServer(t, rec, nil)
	r := httptest.NewRequest(http.MethodGet, "/anthropic/v1/messages", nil)
	r.Header.Set(HeaderAuthorization, "Bearer good")
	w := do(s, r)
	if w.Code != http.StatusFound {
		t.Fatalf("status %d, want the 302 relayed", w.Code)
	}
}

// TestPassthroughMetersBestEffort: a JSON answer with a usage object is read,
// a streamed one is not, and neither can fail the request.
func TestPassthroughMetersBestEffort(t *testing.T) {
	rec := &upstreamRecorder{
		handle: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"m1","usage":{"input_tokens":40,"output_tokens":9,`+
				`"cache_read_input_tokens":12}}`)
		},
	}
	m := &recordingMeter{}
	up := httptest.NewServer(rec)
	defer up.Close()
	s := newTestServer(t, func(o *Options) {
		o.Meter = m
		o.Passthrough = []PassthroughRoute{{
			Prefix: "/anthropic", Provider: "p1", BaseURL: up.URL, Meter: true,
		}}
	})
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader("{}"))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	if w := do(s, r); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	ev := m.last(t)
	if !ev.Passthrough {
		t.Error("event not marked as passthrough")
	}
	if ev.Result.Tokens.Input != 40 || ev.Result.Tokens.Output != 9 || ev.Result.Tokens.CacheRead != 12 {
		t.Errorf("usage %+v", ev.Result.Tokens)
	}
}

// TestPassthroughMeterPanicDoesNotFailTheRequest — step 5, literally.
func TestPassthroughMeterPanicDoesNotFailTheRequest(t *testing.T) {
	rec := &upstreamRecorder{}
	up := httptest.NewServer(rec)
	defer up.Close()
	s := newTestServer(t, func(o *Options) {
		o.Meter = panicMeter{}
		o.Passthrough = []PassthroughRoute{{
			Prefix: "/anthropic", Provider: "p1", BaseURL: up.URL, Meter: true,
		}}
	})
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader("{}"))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	w := do(s, r)
	if w.Code != http.StatusOK || w.Body.String() != `{"upstream":"ok"}` {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
}

// TestPassthroughUpstreamUnreachableDoesNotLeakInternals: a dial error names
// hosts and ports the caller has no business seeing.
func TestPassthroughUpstreamUnreachableDoesNotLeakInternals(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Passthrough = []PassthroughRoute{{
			Prefix: "/dead", Provider: "p1",
			BaseURL: "http://127.0.0.1:1/internal-only",
		}}
	})
	r := httptest.NewRequest(http.MethodGet, "/dead/x", nil)
	r.Header.Set(HeaderAuthorization, "Bearer good")
	w := do(s, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "127.0.0.1") || strings.Contains(body, "internal-only") {
		t.Errorf("transport error leaked internals: %s", body)
	}
}

// TestPassthroughRouteCompileRejectsBadConfig — an open proxy by typo is still
// an open proxy.
func TestPassthroughRouteCompileRejectsBadConfig(t *testing.T) {
	bad := [][]PassthroughRoute{
		{{Prefix: "", BaseURL: "http://x"}},
		{{Prefix: "/", BaseURL: "http://x"}},
		{{Prefix: "anthropic", BaseURL: "http://x"}},
		{{Prefix: "/a", BaseURL: ""}},
		{{Prefix: "/a", BaseURL: "not-a-url"}},
		{{Prefix: "/a", BaseURL: "http://x", Auth: "whatever"}},
		{{Prefix: "/a", BaseURL: "http://x"}, {Prefix: "/a", BaseURL: "http://y"}},
	}
	for i, set := range bad {
		if _, err := compilePassthrough(set); err == nil {
			t.Errorf("case %d: accepted %+v", i, set)
		}
	}
}

// countingWriter records how much was written and the largest single write,
// which is what proves the relay is streaming rather than buffering.
type countingWriter struct {
	http.ResponseWriter
	total    int64
	maxWrite int
	flushes  int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if len(p) > c.maxWrite {
		c.maxWrite = len(p)
	}
	c.total += int64(len(p))
	return len(p), nil
}

func (c *countingWriter) Flush() { c.flushes++ }

// TestPassthroughStreamsWithoutBuffering moves 16 MiB through the relay and
// checks two things a buffered implementation could not satisfy: no single
// write exceeds the bounded relay buffer, and total allocation stays under a
// megabyte. DESIGN §15.5 prohibits full-body buffering past the replay budget,
// and a relay that quietly calls io.ReadAll violates it invisibly — the
// response is byte-identical either way.
func TestPassthroughStreamsWithoutBuffering(t *testing.T) {
	const size = 16 << 20
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(200)
		chunk := make([]byte, 64<<10)
		for written := 0; written < size; written += len(chunk) {
			w.Write(chunk)
		}
	}))
	defer up.Close()

	s := newTestServer(t, func(o *Options) {
		o.Passthrough = []PassthroughRoute{{Prefix: "/big", Provider: "p", BaseURL: up.URL}}
	})

	r := httptest.NewRequest(http.MethodGet, "/big/stream", nil)
	r.Header.Set(HeaderAuthorization, "Bearer good")
	cw := &countingWriter{ResponseWriter: httptest.NewRecorder()}

	before, after := measureAlloc(func() { s.ServeHTTP(cw, r) })

	if cw.total != size {
		t.Fatalf("relayed %d bytes, want %d", cw.total, size)
	}
	if cw.maxWrite > relayBufferSize {
		t.Errorf("single write of %d bytes exceeds the %d-byte relay buffer",
			cw.maxWrite, relayBufferSize)
	}
	if cw.flushes < size/relayBufferSize/2 {
		t.Errorf("only %d flushes for %d bytes — the relay is batching", cw.flushes, size)
	}
	if grew := after - before; grew > 1<<20 {
		t.Errorf("relaying %d bytes allocated %d bytes; a streaming relay allocates a buffer, not a body",
			size, grew)
	}
}

// TestPassthroughRequestBodyIsNotBuffered checks the other direction. A 16 MiB
// upload is relayed while the process holds a 32 KiB buffer, not 16 MiB.
func TestPassthroughRequestBodyIsNotBuffered(t *testing.T) {
	const size = 16 << 20
	var received int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received = n
		w.WriteHeader(204)
	}))
	defer up.Close()

	s := newTestServer(t, func(o *Options) {
		o.MaxBodyBytes = size * 2
		o.Passthrough = []PassthroughRoute{{Prefix: "/big", Provider: "p", BaseURL: up.URL}}
	})

	r := httptest.NewRequest(http.MethodPost, "/big/upload", &zeroReader{n: size})
	r.ContentLength = size
	r.Header.Set(HeaderAuthorization, "Bearer good")
	w := httptest.NewRecorder()

	before, after := measureAlloc(func() { s.ServeHTTP(w, r) })

	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d", w.Code)
	}
	if received != size {
		t.Fatalf("upstream received %d bytes, want %d", received, size)
	}
	if grew := after - before; grew > 2<<20 {
		t.Errorf("uploading %d bytes allocated %d bytes; the request body was buffered", size, grew)
	}
}

// zeroReader yields n zero bytes without allocating them up front.
type zeroReader struct{ n int }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	if len(p) > z.n {
		p = p[:z.n]
	}
	for i := range p {
		p[i] = 0
	}
	z.n -= len(p)
	return len(p), nil
}

// --- WebSocket relay ---------------------------------------------------------

// wsEchoUpstream is a minimal upstream that completes an upgrade and echoes
// every byte. It speaks no WebSocket framing, which is exactly the point: the
// relay must not care either.
func wsEchoUpstream(t *testing.T) (addr string, gotHeader chan http.Header) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	gotHeader = make(chan http.Header, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		gotHeader <- req.Header.Clone()
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: fake\r\nSec-WebSocket-Protocol: chat\r\n\r\n")
		buf := make([]byte, 4096)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return ln.Addr().String(), gotHeader
}

// TestWebSocketRelay is DESIGN §10.6 step 6. Frame-for-frame is achieved by not
// knowing what a frame is: once both sides agree, the relay is two byte copies,
// so an extension dorang has never heard of survives it intact.
func TestWebSocketRelay(t *testing.T) {
	addr, gotHeader := wsEchoUpstream(t)

	s := newTestServer(t, func(o *Options) {
		o.Passthrough = []PassthroughRoute{{
			Prefix: "/ws", Provider: "realtime", BaseURL: "http://" + addr + "/base",
			AllowWebSocket: true,
			Credential:     func(h http.Header) { h.Set(HeaderXAPIKey, providerSecret) },
		}}
	})
	front := httptest.NewServer(s)
	defer front.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	io.WriteString(conn, "GET /ws/v1/realtime HTTP/1.1\r\n"+
		"Host: x\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Authorization: Bearer good\r\n\r\n")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101", resp.StatusCode)
	}
	// The upstream's negotiated subprotocol survives untouched.
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "chat" {
		t.Errorf("subprotocol %q, want chat", got)
	}

	// Bytes that are not valid WebSocket frames still round-trip: the relay
	// does not parse them.
	payload := []byte{0x81, 0x05, 'h', 'e', 'l', 'l', 'o', 0xff, 0x00, 0xfe}
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(br, echoed); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echoed) != string(payload) {
		t.Errorf("echo %x, want %x", echoed, payload)
	}

	select {
	case h := <-gotHeader:
		if h.Get(HeaderXAPIKey) != providerSecret {
			t.Errorf("provider credential not attached to the upgrade: %q", h.Get(HeaderXAPIKey))
		}
		if h.Get(HeaderAuthorization) != "" {
			t.Errorf("client credential forwarded on the upgrade: %q", h.Get(HeaderAuthorization))
		}
		if h.Get("Upgrade") != "websocket" {
			t.Error("the Upgrade header was stripped, which would break the handshake")
		}
		if h.Get("Sec-Websocket-Key") == "" {
			t.Error("the handshake key was not forwarded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never saw the upgrade request")
	}
}

// TestWebSocketUpgradeDeclinedIsRelayed: a 401 from the provider is information
// the caller needs; a synthesized 502 would hide it.
func TestWebSocketUpgradeDeclinedIsRelayed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		if _, err := http.ReadRequest(br); err != nil {
			return
		}
		const body = `{"error":"no realtime"}`
		io.WriteString(conn, "HTTP/1.1 401 Unauthorized\r\n"+
			"Content-Type: application/json\r\n"+
			"Content-Length: "+strconv.Itoa(len(body))+"\r\n"+
			"Connection: close\r\n\r\n"+body)
	}()

	s := newTestServer(t, func(o *Options) {
		o.Passthrough = []PassthroughRoute{{
			Prefix: "/ws", Provider: "realtime",
			BaseURL: "http://" + ln.Addr().String(), AllowWebSocket: true,
		}}
	})
	r := httptest.NewRequest(http.MethodGet, "/ws/v1/realtime", nil)
	r.Header.Set(HeaderAuthorization, "Bearer good")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	w := do(s, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want the upstream 401 relayed", w.Code)
	}
}

// TestWebSocketNotEnabledFallsBackToHTTP: AllowWebSocket is opt-in per prefix,
// so a prefix that fronts a plain REST API cannot be talked into hijacking a
// connection.
func TestWebSocketNotEnabledFallsBackToHTTP(t *testing.T) {
	rec := &upstreamRecorder{}
	s, _ := newPassthroughServer(t, rec, nil) // AllowWebSocket defaults false
	r := httptest.NewRequest(http.MethodGet, "/anthropic/v1/messages", nil)
	r.Header.Set(HeaderAuthorization, "Bearer good")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	w := do(s, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if out := rec.request(t); out.Header.Get("Upgrade") != "" {
		t.Error("an upgrade header was forwarded on a non-upgrade relay")
	}
}
