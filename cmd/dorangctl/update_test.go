package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/selfupdate"
)

func latestRelease(t *testing.T, tag string) *selfupdate.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[]}`, tag)
	}))
	t.Cleanup(srv.Close)
	return &selfupdate.Client{API: srv.URL}
}

type fakeDocker struct {
	calls   []string
	version string // what `dorang -version` answers
	failOn  string // a call prefix that fails
}

func (f *fakeDocker) run(_ context.Context, env []string, name string, args ...string) (string, error) {
	call := strings.TrimSpace(strings.Join(env, " ") + " " + name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, call)
	if f.failOn != "" && strings.Contains(call, f.failOn) {
		return "boom", fmt.Errorf("exit 1")
	}
	if strings.HasSuffix(call, "dorang -version") {
		return f.version + "\n", nil
	}
	return "", nil
}

func newDeployment(t *testing.T, env string, fd *fakeDocker, healthy *atomic.Bool, rel *selfupdate.Client) (*deployment, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(h.Close)
	var out strings.Builder
	return &deployment{
		dir: dir, services: []string{"dorang", "dorang-catalog-sync"}, health: h.URL,
		wait: 200 * time.Millisecond, poll: 10 * time.Millisecond, containerConfig: "/etc/dorang/config.yaml",
		out: &out, run: fd.run, client: rel, httpClient: h.Client(),
	}, dir
}

const releaseEnv = "# secrets\nDORANG_MASTER_KEY=sk\nDORANG_IMAGE=ghcr.io/ziozzang/dorang:0.1.0\nDORANG_VERSION=0.1.0\n"

func TestDeployMovesToTheNewReleaseInOrder(t *testing.T) {
	fd := &fakeDocker{version: "v0.2.0"}
	var healthy atomic.Bool
	healthy.Store(true)
	d, dir := newDeployment(t, releaseEnv, fd, &healthy, latestRelease(t, "v0.2.0"))
	if err := d.update(context.Background(), "", false, false); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"docker pull ghcr.io/ziozzang/dorang:0.2.0",
		"DORANG_IMAGE=ghcr.io/ziozzang/dorang:0.2.0 docker compose --project-directory " + dir +
			" run --rm --no-deps -T --entrypoint dorangctl dorang migrate --config /etc/dorang/config.yaml",
		"docker compose --project-directory " + dir + " up -d --no-deps --no-build --pull never dorang dorang-catalog-sync",
		"docker compose --project-directory " + dir + " exec -T dorang dorang -version",
	}
	if strings.Join(fd.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(fd.calls, "\n"), strings.Join(want, "\n"))
	}
	got, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if string(got) != "# secrets\nDORANG_MASTER_KEY=sk\nDORANG_IMAGE=ghcr.io/ziozzang/dorang:0.2.0\nDORANG_VERSION=0.2.0\n" {
		t.Errorf(".env = %q", got)
	}
	if b, _ := filepath.Glob(filepath.Join(dir, ".env.bak-*")); len(b) != 1 {
		t.Errorf("backups = %v", b)
	}
}

func TestDeployDoesNothingWhenCurrent(t *testing.T) {
	fd := &fakeDocker{}
	var healthy atomic.Bool
	d, _ := newDeployment(t, releaseEnv, fd, &healthy, latestRelease(t, "v0.1.0"))
	if err := d.update(context.Background(), "", false, false); err != nil || len(fd.calls) != 0 {
		t.Errorf("err %v, calls %v", err, fd.calls)
	}
}

func TestDeployRefusesANonReleaseDeployment(t *testing.T) {
	fd := &fakeDocker{}
	var healthy atomic.Bool
	d, _ := newDeployment(t, "DORANG_VERSION=da9f1029\n", fd, &healthy, latestRelease(t, "v0.1.0"))
	if err := d.update(context.Background(), "", false, false); err == nil || !strings.Contains(err.Error(), "DORANG_IMAGE") {
		t.Errorf("err = %v", err)
	}
}

func TestAFailedMigrationReplacesNothing(t *testing.T) {
	fd := &fakeDocker{failOn: "migrate"}
	var healthy atomic.Bool
	d, dir := newDeployment(t, releaseEnv, fd, &healthy, latestRelease(t, "v0.2.0"))
	if err := d.update(context.Background(), "", false, false); err == nil {
		t.Fatal("a failed migration was not reported")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ".env")); string(got) != releaseEnv {
		t.Errorf(".env changed: %q", got)
	}
	for _, c := range fd.calls {
		if strings.Contains(c, " up ") {
			t.Errorf("services were recreated after a failed migration: %s", c)
		}
	}
}

func TestAnUnhealthyReleaseIsRolledBack(t *testing.T) {
	fd := &fakeDocker{version: "v0.2.0"}
	var healthy atomic.Bool // never healthy on the new image
	d, dir := newDeployment(t, releaseEnv, fd, &healthy, latestRelease(t, "v0.2.0"))
	// The rollback's roll comes up healthy.
	orig := fd.run
	ups := 0
	d.run = func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), " up ") || (len(args) > 3 && args[3] == "up") {
			ups++
			if ups == 2 {
				healthy.Store(true)
			}
		}
		return orig(ctx, env, name, args...)
	}
	err := d.update(context.Background(), "", false, false)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ".env")); string(got) != releaseEnv {
		t.Errorf(".env was not restored: %q", got)
	}
	if ups != 2 {
		t.Errorf("compose up ran %d times, want the update and the rollback", ups)
	}
}

func TestAGatewayReportingTheWrongVersionIsRolledBack(t *testing.T) {
	fd := &fakeDocker{version: "v0.1.0"} // the old image is still what answers
	var healthy atomic.Bool
	healthy.Store(true)
	d, _ := newDeployment(t, releaseEnv, fd, &healthy, latestRelease(t, "v0.2.0"))
	if err := d.update(context.Background(), "", false, false); err == nil || !strings.Contains(err.Error(), "reports") {
		t.Errorf("err = %v", err)
	}
}

func TestSetEnvKeepsEverythingElse(t *testing.T) {
	got := string(setEnv([]byte("A=1\n# DORANG_IMAGE=commented\nDORANG_VERSION=x"), map[string]string{
		"DORANG_IMAGE": "img", "DORANG_VERSION": "0.1.0"}))
	if got != "A=1\n# DORANG_IMAGE=commented\nDORANG_VERSION=0.1.0\nDORANG_IMAGE=img\n" {
		t.Errorf("setEnv = %q", got)
	}
}
