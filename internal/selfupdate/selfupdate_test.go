package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompareAndIsRelease(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.1.0", "v0.1.0", 0},
		{"0.2.0", "0.1.9", 1},
		{"0.10.0", "0.9.0", 1},
		{"1.0.0", "1.0.0-rc1", 0},
		{"0.1.0", "da9f1029-20261008T231711Z", 1}, // a commit export is below every release
		{"devel", "0.0.1", -1},
	} {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	for v, want := range map[string]bool{"v0.1.0": true, "0.1.0": true, "1.2.3-rc1": true,
		"devel": false, "dev": false, "da9f1029-20261008T231711Z": false, "": false} {
		if IsRelease(v) != want {
			t.Errorf("IsRelease(%q) = %v", v, !want)
		}
	}
}

func TestAssetName(t *testing.T) {
	if n, err := AssetName("dorang", "v0.1.0", "linux", "amd64"); err != nil || n != "dorang_0.1.0_linux_amd64" {
		t.Errorf("AssetName = %q, %v", n, err)
	}
	for _, p := range [][2]string{{"windows", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}} {
		if _, err := AssetName("dorang", "0.1.0", p[0], p[1]); err == nil {
			t.Errorf("%s/%s has no release build and was named", p[0], p[1])
		}
	}
}

// fakeGitHub serves one release whose dorang asset is payload.
func fakeGitHub(t *testing.T, tag string, payload []byte, sum string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		name, _ := AssetName("dorang", tag, "linux", "amd64")
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest", "/repos/" + Repo + "/releases/tags/" + tag:
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":"%s/a"},`+
				`{"name":"SHA256SUMS","browser_download_url":"%s/sums"}]}`, tag, name, srv.URL, srv.URL)
		case "/a":
			_, _ = w.Write(payload)
		case "/sums":
			fmt.Fprintf(w, "%s  %s\n", sum, name)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestADownloadIsInstalledOnlyWhenItsDigestMatches(t *testing.T) {
	payload := []byte("#!/bin/true\n")
	h := sha256.Sum256(payload)
	good := hex.EncodeToString(h[:])
	srv, _ := fakeGitHub(t, "v0.1.0", payload, good)
	c := &Client{API: srv.URL}
	ctx := context.Background()

	rel, err := c.Fetch(ctx, "")
	if err != nil || rel.Version() != "0.1.0" {
		t.Fatalf("Fetch = %+v, %v", rel, err)
	}
	if _, err := c.Fetch(ctx, "0.1.0"); err != nil {
		t.Errorf("a tag without its v was not found: %v", err)
	}
	sums, err := c.Checksums(ctx, rel)
	if err != nil || sums["dorang_0.1.0_linux_amd64"] != good {
		t.Fatalf("Checksums = %v, %v", sums, err)
	}
	a, _ := rel.Find("dorang_0.1.0_linux_amd64")
	dir := t.TempDir()
	path, err := c.DownloadVerified(ctx, a, good, dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("the download is not executable: %v", fi.Mode())
	}
	if _, err := c.DownloadVerified(ctx, a, strings.Repeat("0", 64), dir); err == nil ||
		!strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("a mismatched digest was accepted: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".dorang-update-*"))
	if len(left) != 1 {
		t.Errorf("a refused download was left behind: %v", left)
	}
}

func TestUpdateCheckChangesNothing(t *testing.T) {
	srv, hits := fakeGitHub(t, "v0.2.0", nil, "")
	var out strings.Builder
	err := (&Client{API: srv.URL}).Update(context.Background(), Options{
		Binary: "dorang", Current: "v0.1.0", Check: true, Out: &out})
	if err != nil || !strings.Contains(out.String(), "0.2.0 is available") {
		t.Errorf("Update --check = %q, %v", out.String(), err)
	}
	if hits.Load() != 1 {
		t.Errorf("a check made %d requests, want only the lookup", hits.Load())
	}
}

func TestNotifyAnnouncesANewerReleaseOnce(t *testing.T) {
	srv, _ := fakeGitHub(t, "v0.2.0", nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 8)
	logf := func(f string, a ...any) { lines <- fmt.Sprintf(f, a...) }
	(&Client{API: srv.URL}).Notify(ctx, "v0.1.0", 20*time.Millisecond, logf)
	select {
	case l := <-lines:
		if !strings.Contains(l, "release 0.2.0 is available (running v0.1.0)") {
			t.Errorf("notice = %q", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notice")
	}
	select {
	case l := <-lines:
		t.Errorf("the same release was announced twice: %q", l)
	case <-time.After(100 * time.Millisecond):
	}

	quiet := make(chan string, 1)
	(&Client{API: srv.URL}).Notify(ctx, "devel", time.Hour, func(f string, a ...any) { quiet <- f })
	t.Setenv(EnvNoCheck, "1")
	(&Client{API: srv.URL}).Notify(ctx, "v0.1.0", time.Hour, func(f string, a ...any) { quiet <- f })
	select {
	case l := <-quiet:
		t.Errorf("a development build or an opted-out gateway checked: %q", l)
	case <-time.After(100 * time.Millisecond):
	}
}
