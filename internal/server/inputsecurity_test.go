package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type unreadBomb struct {
	read int64
	fill byte
}

func (b *unreadBomb) Read(p []byte) (int, error) {
	b.read += int64(len(p))
	for i := range p {
		p[i] = b.fill
	}
	return len(p), nil
}
func (*unreadBomb) Close() error { return nil }

func TestCompressedRequestsRejectedWithoutReading(t *testing.T) {
	s := newTestServer(t, nil)
	for _, encoding := range []string{"gzip", "br", "zstd", "deflate", "gzip, br"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/audio/transcriptions"} {
			src := &unreadBomb{}
			r := httptest.NewRequest(http.MethodPost, path, io.LimitReader(src, 1<<30))
			r.Header.Set(HeaderAuthorization, "Bearer good")
			r.Header.Set("Content-Encoding", encoding)
			w := do(s, r)
			if w.Code != http.StatusUnsupportedMediaType || src.read != 0 {
				t.Fatalf("%s %s: status %d read %d", path, encoding, w.Code, src.read)
			}
		}
	}
}

func TestDeepJSONRefusedAtGate(t *testing.T) {
	s := newTestServer(t, nil)
	for _, depth := range []int{127, 10000} {
		body := `{"model":"model-x","ignored":` + strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth) + "}"
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set(HeaderAuthorization, "Bearer good")
		w := do(s, r)
		want := http.StatusOK
		if depth > 127 {
			want = http.StatusBadRequest
		}
		if w.Code != want {
			t.Fatalf("depth %d: %d %s", depth, w.Code, w.Body.String())
		}
	}
}

func TestBodyReadAndAllocationUseEffectiveLimit(t *testing.T) {
	// A nonpositive option must apply the default before trusting Content-Length.
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	src := &unreadBomb{}
	r.Body = src
	r.ContentLength = DefaultMaxBodyBytes + 1
	var b Body
	if err := b.read(r, 0, nil); err == nil || err.Status != http.StatusRequestEntityTooLarge || b.buf != nil || src.read != 0 {
		t.Fatalf("large declaration allocated or read: %v %d", err, src.read)
	}
	r.ContentLength = -1
	if err := b.read(r, 1024, nil); err == nil || err.Status != http.StatusRequestEntityTooLarge || src.read != 1025 {
		t.Fatalf("unbounded chunked read: %v %d", err, src.read)
	}
	b.release()
}

func TestDefaultClientsDisableCompression(t *testing.T) {
	c := defaultPassthroughClient()
	if !c.Transport.(*http.Transport).DisableCompression {
		t.Fatal("passthrough decompresses peer input")
	}
}

func TestWebSocketUpgradeHeaderReadIsBounded(t *testing.T) {
	src := &unreadBomb{fill: 'a'}
	r := io.MultiReader(strings.NewReader("HTTP/1.1 101 Switching Protocols\r\nX-Bomb: "), io.LimitReader(src, maxUpgradeHeaderBytes*4))
	_, _, err := readUpgradeResponse(r, &http.Request{Method: http.MethodGet})
	if err == nil || src.read > maxUpgradeHeaderBytes {
		t.Fatalf("err=%T bytes=%d", err, src.read)
	}
}

func TestWebSocketHeaderLimitPreservesBufferedAndLongFrames(t *testing.T) {
	frame := strings.Repeat("payload", maxUpgradeHeaderBytes)
	r := strings.NewReader("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n" + frame)
	resp, br, err := readUpgradeResponse(r, &http.Request{Method: http.MethodGet})
	if err != nil || resp.StatusCode != 101 {
		t.Fatal(resp, err)
	}
	// Buffered bytes and later reads must both survive removal of the header cap.
	if br.Buffered() == 0 {
		t.Fatal("fixture did not exercise read-ahead")
	}
	b, err := io.ReadAll(br)
	if err != nil || string(b) != frame {
		t.Fatalf("frame %d bytes: %v", len(b), err)
	}
}
