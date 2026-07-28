package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// isWebSocketUpgrade reports whether a request asks for a protocol upgrade to
// WebSocket. Both header values are matched case-insensitively, and Connection
// is a comma list rather than a single token — a client that sends
// "Connection: keep-alive, Upgrade" is asking for an upgrade and a naive
// equality check says it is not.
func isWebSocketUpgrade(h http.Header) bool {
	if !strings.EqualFold(h.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// wsHandshakeTimeout bounds the upstream half of the upgrade. Once the upgrade
// completes there is no timeout: a WebSocket that is idle for an hour is
// working as intended.
const wsHandshakeTimeout = 30 * time.Second

// relayWebSocket relays a WebSocket upgrade frame-for-frame (DESIGN §10.6
// step 6).
//
// "Frame-for-frame" is achieved by not knowing what a frame is. Once both sides
// have agreed to the upgrade the relay is two byte copies in opposite
// directions; it never parses an opcode, never reassembles a fragment, never
// masks or unmasks a payload, and therefore cannot corrupt an extension it has
// never heard of. The only WebSocket-specific logic in this function is
// recognizing the handshake.
func (s *Server) relayWebSocket(w http.ResponseWriter, rq *Request, pt *passthroughRoute, target *url.URL) error {
	network, addr, tlsCfg := dialTarget(target)

	dial := pt.dial
	if dial == nil {
		var d net.Dialer
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return d.DialContext(ctx, network, addr)
		}
	}

	ctx, cancel := context.WithTimeout(rq.HTTP.Context(), wsHandshakeTimeout)
	defer cancel()

	up, err := dial(ctx, network, addr)
	if err != nil {
		return passthroughDialError(err)
	}
	closeUp := true
	defer func() {
		if closeUp {
			_ = up.Close()
		}
	}()
	if tlsCfg != nil {
		tc := tls.Client(up, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return NewError(http.StatusBadGateway, TypeAPIError,
				"upstream TLS handshake failed").WithCode("passthrough_tls_failed")
		}
		up = tc
	}

	// The upgrade request keeps Connection and Upgrade: they are hop-by-hop by
	// definition, and an upgrade is precisely the case where the hop being
	// negotiated is the one being forwarded.
	outHeader := make(http.Header, len(rq.HTTP.Header))
	for k, vs := range rq.HTTP.Header {
		cp := make([]string, len(vs))
		copy(cp, vs)
		outHeader[k] = cp
	}
	outHeader.Del("Proxy-Connection")
	outHeader.Del("Proxy-Authorization")
	outHeader.Del(HeaderDetail)
	outHeader.Del(HeaderUsageEvents)
	if pt.auth != PassthroughAuthClient {
		StripAuthHeaders(outHeader)
	}
	if pt.credential != nil {
		pt.credential(outHeader)
	}
	outHeader.Set(HeaderRequestID, rq.ID)

	out := &http.Request{
		Method:     http.MethodGet,
		URL:        target,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     outHeader,
		Host:       target.Host,
	}
	_ = up.SetDeadline(time.Now().Add(wsHandshakeTimeout))
	if err := out.Write(up); err != nil {
		return NewError(http.StatusBadGateway, TypeAPIError,
			"could not send the upgrade request upstream").WithCode("passthrough_upgrade_failed")
	}

	br := bufio.NewReader(up)
	resp, err := http.ReadResponse(br, out)
	if err != nil {
		return NewError(http.StatusBadGateway, TypeAPIError,
			"upstream did not answer the upgrade").WithCode("passthrough_upgrade_failed")
	}
	_ = up.SetDeadline(time.Time{})

	if resp.StatusCode != http.StatusSwitchingProtocols {
		// The upstream declined. Relay its answer as an ordinary response
		// rather than inventing one: a 401 from the provider is information
		// the caller needs, and a synthesized 502 would hide it.
		defer resp.Body.Close()
		h := w.Header()
		copyResponseHeaders(h, resp.Header)
		w.WriteHeader(resp.StatusCode)
		_ = relayBody(w, resp)
		return nil
	}

	conn, cbuf, err := hijack(w)
	if err != nil {
		return NewError(http.StatusInternalServerError, TypeAPIError,
			"this server cannot relay a protocol upgrade").WithCode("upgrade_unsupported")
	}
	closeUp = false
	s.metrics.wsUpgrades.Add(1)

	// The 101 goes back verbatim, including whichever subprotocol and
	// extensions the upstream selected. Rewriting any of it would make dorang a
	// participant in a negotiation it does not understand.
	if err := writeSwitchingProtocols(cbuf.Writer, resp); err != nil {
		_ = conn.Close()
		_ = up.Close()
		return nil
	}

	pipe(conn, cbuf, up, br)
	return nil
}

// writeSwitchingProtocols sends the 101 status line and headers.
func writeSwitchingProtocols(w *bufio.Writer, resp *http.Response) error {
	if _, err := w.WriteString("HTTP/1.1 101 Switching Protocols\r\n"); err != nil {
		return err
	}
	if err := resp.Header.Write(w); err != nil {
		return err
	}
	if _, err := w.WriteString("\r\n"); err != nil {
		return err
	}
	return w.Flush()
}

// pipe copies bytes in both directions until either side closes.
//
// Each direction gets its own bounded buffer from the same pool the HTTP relay
// uses; nothing is accumulated, so a long-lived socket costs two buffers rather
// than a growing one.
func pipe(client net.Conn, cbuf *bufio.ReadWriter, upstream net.Conn, ubuf *bufio.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		copyBounded(upstream, cbuf)
		halfClose(upstream)
	}()
	go func() {
		defer wg.Done()
		copyBounded(client, ubuf)
		halfClose(client)
	}()
	wg.Wait()
	_ = client.Close()
	_ = upstream.Close()
}

// copyBounded is io.Copy with a pooled buffer, so that neither direction
// allocates 32 KiB per relayed connection.
func copyBounded(dst io.Writer, src io.Reader) {
	bufp := relayBufPool.Get().(*[]byte)
	defer relayBufPool.Put(bufp)
	_, _ = io.CopyBuffer(writerOnly{dst}, src, *bufp)
}

// writerOnly hides a ReaderFrom implementation so that CopyBuffer actually uses
// the buffer it was given rather than delegating and allocating its own.
type writerOnly struct{ io.Writer }

// halfClose shuts down the write side if the connection supports it, so the
// peer sees a clean EOF rather than a reset.
func halfClose(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// hijack takes the client connection.
func hijack(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return http.NewResponseController(w).Hijack()
}

// dialTarget resolves a URL to a dial target and, for https/wss, a TLS config.
func dialTarget(u *url.URL) (network, addr string, cfg *tls.Config) {
	host := u.Hostname()
	port := u.Port()
	secure := u.Scheme == "https" || u.Scheme == "wss"
	if port == "" {
		if secure {
			port = "443"
		} else {
			port = "80"
		}
	}
	if secure {
		cfg = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	}
	return "tcp", net.JoinHostPort(host, port), cfg
}
