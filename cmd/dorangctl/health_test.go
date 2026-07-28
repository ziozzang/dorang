package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDockerfileHealthcheckInvokesARealSubcommand.
//
// The image's HEALTHCHECK ran `dorangctl health --addr …` and there was no
// `health` subcommand: the CLI answered `unknown command "health"` and exited 2,
// so every container built from this image reported unhealthy after
// start-period + 3 × interval, forever. The image is distroless — no curl, no
// wget, no shell — so nothing else in it could have made the probe work either.
//
// This test reads the Dockerfile rather than asserting the subcommand exists,
// because "the CLI has a health command" was never the thing that was wrong. The
// two have to agree, and only a test that reads both can say so.
func TestDockerfileHealthcheckInvokesARealSubcommand(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("Dockerfile: %v", err)
	}
	line := healthcheckLine(string(data))
	if line == "" {
		t.Fatal("no HEALTHCHECK CMD in the Dockerfile")
	}
	argv := execFormArgs(line)
	if len(argv) < 2 {
		t.Fatalf("HEALTHCHECK is not in exec form or takes no subcommand: %s", line)
	}
	if base := filepath.Base(argv[0]); base != "dorangctl" {
		t.Fatalf("HEALTHCHECK runs %q; this test only knows how to check dorangctl", argv[0])
	}

	// The decisive assertion: run the real dispatcher with the image's own
	// arguments and require it not to be the unknown-command path. A wrong
	// subcommand, a wrong flag name, or a flag that was renamed all fail here.
	var out, errOut bytes.Buffer
	code := run(append(argv[1:], "--timeout", "10ms"), &out, &errOut)
	if strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("the image's HEALTHCHECK is not a command this binary has: %s", errOut.String())
	}
	if strings.Contains(errOut.String(), "flag provided but not defined") {
		t.Fatalf("the image's HEALTHCHECK passes a flag this binary does not define: %s",
			errOut.String())
	}
	// Nothing is listening in a unit test, so a connection failure is the
	// expected outcome and exit 1 is correct. Exit 2 is "you asked for something
	// that does not exist", which is the failure mode being guarded.
	if code == 2 {
		t.Fatalf("HEALTHCHECK exited 2 (usage error): %s", errOut.String())
	}
}

// healthcheckLine returns the CMD part of the HEALTHCHECK instruction, joining
// backslash continuations.
func healthcheckLine(src string) string {
	src = strings.ReplaceAll(src, "\\\n", " ")
	for _, l := range strings.Split(src, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "HEALTHCHECK") {
			continue
		}
		if i := strings.Index(l, "CMD "); i >= 0 {
			return strings.TrimSpace(l[i+4:])
		}
	}
	return ""
}

var jsonStr = regexp.MustCompile(`"([^"]*)"`)

// execFormArgs parses a JSON-array exec form into its arguments.
func execFormArgs(cmd string) []string {
	cmd = strings.TrimSpace(cmd)
	if !strings.HasPrefix(cmd, "[") {
		return nil
	}
	var out []string
	for _, m := range jsonStr.FindAllStringSubmatch(cmd, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestHealthProbesTheGateway covers the subcommand itself: 200 is healthy,
// anything else is not, and a gateway that cannot be reached is a failure rather
// than a silent success.
func TestHealthProbesTheGateway(t *testing.T) {
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health/liveliness" && r.URL.Path != "/health/readiness" {
			t.Errorf("probed %s", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	status = http.StatusOK
	if code := run([]string{"health", "--addr", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("a healthy gateway exited %d: %s", code, errOut.String())
	}
	status = http.StatusServiceUnavailable
	if code := run([]string{"health", "--addr", srv.URL}, &out, &errOut); code == 0 {
		t.Fatal("a draining gateway was reported healthy")
	}
	// Readiness is a different endpoint on purpose: a draining node is alive and
	// must not be restarted (§13), so the default probe is liveness.
	status = http.StatusOK
	if code := run([]string{"health", "--addr", srv.URL, "--ready"}, &out, &errOut); code != 0 {
		t.Fatalf("--ready exited %d", code)
	}
	if code := run([]string{"health", "--addr", "http://127.0.0.1:1", "--timeout", "50ms"},
		&out, &errOut); code == 0 {
		t.Fatal("an unreachable gateway was reported healthy")
	}
}

// TestHealthAddrDefaultsToLoopback: a bind address is not a destination. The
// probe runs inside the container, so ":4100" and "0.0.0.0:4100" both mean
// loopback.
func TestHealthAddrDefaultsToLoopback(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", defaultHealthAddr},
		{":4100", "http://127.0.0.1:4100"},
		{"0.0.0.0:8080", "http://127.0.0.1:8080"},
		{"http://gateway:4100", "http://gateway:4100"},
	} {
		t.Setenv(EnvListen, tc.env)
		if got := defaultListenAddr(); got != tc.want {
			t.Errorf("DORANG_LISTEN=%q gives %q, want %q", tc.env, got, tc.want)
		}
	}
}

// TestStateDirIsDeclaredAndRead is the other half of the same defect: the image
// sets DORANG_STATE_DIR and nothing read it, so every container wrote its
// database, its spool and its generated key pepper into the writable layer
// instead of the declared volume — and lost them on restart.
func TestStateDirIsDeclaredAndRead(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("Dockerfile: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, "ENV DORANG_STATE_DIR=") {
		t.Fatal("the image no longer declares DORANG_STATE_DIR")
	}
	dir := volumeOf(src)
	if dir == "" {
		t.Fatal("the image declares no VOLUME")
	}
	if !strings.Contains(src, "ENV DORANG_STATE_DIR="+dir) {
		t.Fatalf("DORANG_STATE_DIR does not point at the declared volume %q", dir)
	}
}

var volumeRe = regexp.MustCompile(`VOLUME\s+\["([^"]+)"\]`)

func volumeOf(src string) string {
	m := volumeRe.FindStringSubmatch(src)
	if m == nil {
		return ""
	}
	return m[1]
}
