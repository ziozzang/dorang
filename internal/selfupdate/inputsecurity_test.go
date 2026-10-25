package selfupdate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/parseguard"
)

func TestReleaseRejectsDeepIgnoredMetadata(t *testing.T) {
	metadata := `{"tag_name":"v1.0.0","ignored":` + strings.Repeat("[", 129) + "0" + strings.Repeat("]", 129) + "}"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(metadata))
	}))
	defer s.Close()
	c := &Client{API: s.URL}
	if _, err := c.Fetch(context.Background(), ""); !errors.Is(err, parseguard.ErrDepth) {
		t.Fatalf("deep release metadata accepted: %v", err)
	}
}

func TestDownloadRejectsOversizeInsteadOfAcceptingPrefix(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 2048)))
	}))
	defer s.Close()
	c := &Client{}
	if b, err := c.download(context.Background(), s.URL, 1024); b != nil || !errors.Is(err, parseguard.ErrSize) {
		t.Fatalf("oversize download returned a successful prefix: bytes=%d err=%v", len(b), err)
	}
}
