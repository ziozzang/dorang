package httpguard

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUnsolicitedGzipIsNeverExpanded(t *testing.T) {
	var bomb bytes.Buffer
	gz := gzip.NewWriter(&bomb)
	_, _ = io.Copy(gz, io.LimitReader(zeroReader{}, 1<<20))
	gz.Close()
	if bomb.Len() > 16<<10 {
		t.Fatalf("test fixture is not highly compressed: %d bytes", bomb.Len())
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "" {
			t.Error("advertised decompression")
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(bomb.Bytes())
	}))
	defer s.Close()
	tr := Transport()
	defer tr.CloseIdleConnections()
	res, err := (&http.Client{Transport: tr}).Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil || res.Uncompressed || !bytes.Equal(b, bomb.Bytes()) {
		t.Fatalf("response expanded: %d bytes, %v", len(b), err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }
