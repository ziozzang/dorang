package server

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPassthroughPreservesCompressedRequests(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte(`{"model":"model-x","messages":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{PassthroughAuthDorang, PassthroughAuthClient, PassthroughAuthNone} {
		for _, contentType := range []string{"application/json", "application/octet-stream"} {
			t.Run(auth+"/"+contentType, func(t *testing.T) {
				rec := &upstreamRecorder{}
				s, _ := newPassthroughServer(t, rec, func(r *PassthroughRoute) { r.Auth = auth })
				r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(compressed.Bytes()))
				r.Header.Set(HeaderAuthorization, "Bearer good")
				r.Header.Set("Content-Type", contentType)
				r.Header.Set("Content-Encoding", "gzip")
				w := do(s, r)
				if w.Code != http.StatusOK {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				got := rec.request(t)
				if got.Header.Get("Content-Encoding") != "gzip" || got.Header.Get("Content-Type") != contentType {
					t.Fatalf("encoding or content type changed: %v", got.Header)
				}
				rec.mu.Lock()
				body := rec.body
				rec.mu.Unlock()
				if !bytes.Equal([]byte(body), compressed.Bytes()) || got.ContentLength != int64(compressed.Len()) {
					t.Fatal("compressed request bytes or length changed")
				}
			})
		}
	}
}

func TestCompressedPassthroughStillEnforcesModelRestrictions(t *testing.T) {
	rec := &upstreamRecorder{}
	up := httptest.NewServer(rec)
	defer up.Close()
	s := newTestServer(t, func(o *Options) {
		o.Auth = restrictedAuth("model-x")
		o.Passthrough = []PassthroughRoute{{Prefix: "/native", BaseURL: up.URL}}
	})
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte(`{"model":"model-x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/native/v1/messages", bytes.NewReader(compressed.Bytes()))
	r.Header.Set(HeaderAuthorization, "Bearer good")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Encoding", "gzip")
	if w := do(s, r); w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", w.Code, w.Body.String())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.last != nil {
		t.Fatal("compressed body bypassed the model restriction")
	}
}
