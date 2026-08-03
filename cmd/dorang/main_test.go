package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The process tests run the real main through a re-exec of the test binary.
// Nothing is mocked at the process boundary: the child parses flags, loads the
// configuration, opens SQLite, listens, serves, and drains on a signal, which is
// the only way to test that a single binary with no dependencies actually runs
// (DESIGN §0.2).
const childEnv = "DORANG_TEST_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// child is a running dorang process under test.
type child struct {
	cmd    *exec.Cmd
	addr   string
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	mu     sync.Mutex
}

var listeningRE = regexp.MustCompile(`listening on ([^\s]+)`)

// start launches the gateway and waits for it to announce its address.
func start(t *testing.T, args []string, extraEnv ...string) *child {
	t.Helper()
	c := &child{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	c.cmd = exec.Command(os.Args[0], args...)
	c.cmd.Env = append(os.Environ(), childEnv+"=1")
	c.cmd.Env = append(c.cmd.Env, extraEnv...)

	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := c.cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}

	addrc := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			c.mu.Lock()
			c.stdout.WriteString(line + "\n")
			c.mu.Unlock()
			if m := listeningRE.FindStringSubmatch(line); m != nil {
				select {
				case addrc <- m[1]:
				default:
				}
			}
		}
	}()
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			c.mu.Lock()
			c.stderr.WriteString(sc.Text() + "\n")
			c.mu.Unlock()
		}
	}()

	select {
	case c.addr = <-addrc:
	case <-time.After(30 * time.Second):
		_ = c.cmd.Process.Kill()
		t.Fatalf("the gateway never announced a listening address\nstderr:\n%s", c.err())
	}
	t.Cleanup(func() {
		if c.cmd.ProcessState == nil {
			_ = c.cmd.Process.Kill()
			_ = c.cmd.Wait()
		}
	})
	return c
}

func (c *child) err() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stderr.String()
}

func (c *child) url(path string) string { return "http://" + c.addr + path }

// signalAndWait sends sig and waits for the process to exit.
func (c *child) signalAndWait(t *testing.T, sig os.Signal, within time.Duration) int {
	t.Helper()
	if err := c.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return 0
		}
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			return ee.ExitCode()
		}
		t.Fatalf("wait: %v", err)
	case <-time.After(within):
		_ = c.cmd.Process.Kill()
		t.Fatalf("the process did not exit within %s\nstderr:\n%s", within, c.err())
	}
	return -1
}

func asExitError(err error, out **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*out = ee
		return true
	}
	return false
}

// fakeUpstream is a provider that answers chat completions, optionally after a
// delay.
//
// The second return is the handshake. dorang runs in a child process here, so a
// test that needs a request to be genuinely IN FLIGHT has no way to ask the
// gateway — but this handler runs in the test's own process and is entered only
// once the request has crossed dorang and been dispatched. Receiving on the
// channel is therefore proof, where a sleep is a guess that gets it wrong on a
// loaded machine in the direction that fails the test.
func fakeUpstream(t *testing.T, delay time.Duration) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	// Buffered and sent to without blocking: the signal must never be able to
	// hold a request open for a test that is not listening for it.
	arrived := make(chan struct{}, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, `{"error":{"message":"no such route"}}`, http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"upstream-x"`)) {
			// §7.2: upstream must receive the deployment's real model id, not
			// the client-facing name.
			http.Error(w, `{"error":{"message":"wrong upstream model"}}`, http.StatusBadRequest)
			return
		}
		select {
		case arrived <- struct{}{}:
		default:
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamAnswer)
	}))
	t.Cleanup(srv.Close)
	return srv, arrived
}

const upstreamAnswer = `{"id":"chatcmpl-fake","object":"chat.completion","created":1700000000,` +
	`"model":"upstream-x","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},` +
	`"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`

// writeConfig renders a minimal but complete configuration into dir.
func writeConfig(t *testing.T, dir, upstreamURL string) string {
	t.Helper()
	cfg := fmt.Sprintf(`version: 1
server:
  listen: 127.0.0.1:0
  env: development
storage:
  driver: sqlite
  sqlite:
    path: %s/dorang.db
metering:
  flush_interval: 20ms
  spool:
    dir: %s/spool
providers:
  - name: fake
    kind: openai
    base_url: %s/v1
credentials:
  - id: fake-1
    provider: fake
    key: upstream-secret
models:
  - name: model-x
    deployments:
      - provider: fake
        upstream_model: upstream-x
        credentials: [fake-1]
pricing:
  currency: USD
  rules:
    - id: fake-tokens
      class: marginal_usage
      match: {provider: fake}
      rates: {input: "3.00", output: "15.00"}
`, dir, dir, upstreamURL)
	path := filepath.Join(dir, "dorang.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const chatRequest = `{"model":"model-x","messages":[{"role":"user","content":"ping"}]}`

// TestSingleBinaryServesWithNoDependencies is DESIGN §0.2's whole claim, run as
// a process: no PostgreSQL, no Redis, no pre-created database, no environment
// beyond a temporary HOME, and a request completes through a fake upstream.
func TestSingleBinaryServesWithNoDependencies(t *testing.T) {
	dir := t.TempDir()
	up, _ := fakeUpstream(t, 0)
	cfgPath := writeConfig(t, dir, up.URL)

	c := start(t, []string{"--config", cfgPath},
		"HOME="+dir,
		"DORANG_MASTER_KEY=sk-master-test",
		"DORANG_KEY_PEPPER=test-pepper")

	waitReady(t, c)

	resp := post(t, c.url("/v1/chat/completions"), "sk-master-test", chatRequest) // pragma: allowlist secret — test fixture
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, body = %s\nstderr:\n%s", resp.status, resp.body, c.err())
	}
	if !strings.Contains(resp.body, `"pong"`) {
		t.Errorf("the assistant message did not survive: %s", resp.body)
	}
	// §7.2: the body carries the name the client asked for, never the upstream id.
	if strings.Contains(resp.body, "upstream-x") {
		t.Errorf("the response leaked the upstream model id: %s", resp.body)
	}
	if !strings.Contains(resp.body, `"model-x"`) {
		t.Errorf("the response did not carry the requested model name: %s", resp.body)
	}
	if resp.header.Get("X-Dorang-Request-Id") == "" {
		t.Error("no request id header; the ledger cannot be joined against")
	}
	if got := resp.header.Get("X-Dorang-Provider"); got != "fake" {
		t.Errorf("x-dorang-provider = %q, want fake", got)
	}

	if code := c.signalAndWait(t, syscall.SIGTERM, 30*time.Second); code != 0 {
		t.Errorf("exit code = %d, want 0\nstderr:\n%s", code, c.err())
	}
	// The database exists because the process created it, which is the part of
	// "no dependencies" that is easy to claim and easy to get wrong.
	if _, err := os.Stat(filepath.Join(dir, "dorang.db")); err != nil {
		t.Errorf("the store was never created: %v", err)
	}
}

// TestZeroConfigurationStarts covers the other half of §0.2: no configuration
// file at all. The documented defaults apply and the health surface answers.
func TestZeroConfigurationStarts(t *testing.T) {
	dir := t.TempDir()
	c := start(t, []string{"--listen", "127.0.0.1:0"},
		"HOME="+dir,
		"DORANG_CONFIG=",
		"PWD="+dir)
	waitReady(t, c)

	// No models are configured, so the list is empty rather than absent.
	resp := get(t, c.url("/v1/models"), "")
	if resp.status != http.StatusUnauthorized {
		t.Errorf("an unauthenticated model list should be 401, got %d", resp.status)
	}
	if code := c.signalAndWait(t, syscall.SIGTERM, 30*time.Second); code != 0 {
		t.Errorf("exit code = %d, want 0\nstderr:\n%s", code, c.err())
	}
}

// TestDrainFinishesInFlightRequests is §13's contract: readiness goes false
// immediately, the listener closes, and work already accepted runs to
// completion.
func TestDrainFinishesInFlightRequests(t *testing.T) {
	dir := t.TempDir()
	up, arrived := fakeUpstream(t, 1500*time.Millisecond)
	cfgPath := writeConfig(t, dir, up.URL)

	c := start(t, []string{"--config", cfgPath},
		"HOME="+dir,
		"DORANG_MASTER_KEY=sk-master-test",
		"DORANG_KEY_PEPPER=test-pepper")
	waitReady(t, c)

	type outcome struct {
		resp response
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := postErr(c.url("/v1/chat/completions"), "sk-master-test", chatRequest, 30*time.Second) // pragma: allowlist secret — test fixture
		done <- outcome{r, err}
	}()

	// Let the request reach the upstream, then ask the process to stop while it
	// is still in flight. The upstream says when that has happened; the request
	// is held there for another 1.5s, so the SIGTERM below lands squarely
	// inside it. Guessing a duration instead gets this exactly backwards on a
	// busy machine — the signal arrives before the request is accepted, the
	// listener closes under it, and the drain is blamed for losing a request it
	// never had.
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("the request never reached the upstream, so nothing was ever in flight to drain")
	}
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the in-flight request did not finish: %v\nstderr:\n%s", got.err, c.err())
		}
		if got.resp.status != http.StatusOK {
			t.Fatalf("in-flight request status = %d, body = %s", got.resp.status, got.resp.body)
		}
		if !strings.Contains(got.resp.body, `"pong"`) {
			t.Errorf("the drained request lost its body: %s", got.resp.body)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the in-flight request never completed")
	}

	waitExit(t, c, 30*time.Second)
}

// TestRefusesUnsafeConfigurations covers the three configurations the design
// refuses to start on. Each must exit non-zero with a message that names the
// problem: a process that refuses without saying why is indistinguishable from
// a crash.
func TestRefusesUnsafeConfigurations(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		expect string
	}{
		{
			name: "cluster with local capacity accounting (§5.6)",
			yaml: `version: 1
cluster: {enabled: true, capacity_mode: local}
`,
			expect: "capacity_mode",
		},
		{
			name: "inline literal secret outside development (§4.1)",
			yaml: `version: 1
server: {env: production}
providers:
  - {name: p, kind: openai, base_url: "https://example.invalid/v1"}
credentials:
  - {id: c, provider: p, key: "a-literal-secret"}
`,
			expect: "inline literal secret",
		},
		{
			name: "legacy key hashing with no expiry date (§2.4)",
			yaml: `version: 1
auth:
  legacy: {enabled: true}
`,
			expect: "auth.legacy.until",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "dorang.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			// --check and a real start must agree; both are exercised.
			for _, args := range [][]string{
				{"--check", "--config", path},
				{"--config", path, "--listen", "127.0.0.1:0"},
			} {
				out, code := runChild(t, dir, args...)
				if code == 0 {
					t.Fatalf("%v exited 0; it must refuse\noutput:\n%s", args, out)
				}
				if !strings.Contains(out, tc.expect) {
					t.Errorf("%v did not name the problem %q\noutput:\n%s", args, tc.expect, out)
				}
			}
		})
	}
}

// TestCheckAcceptsTheShippedExample keeps deploy/config.example.yaml honest: it
// is the file operators copy, and it must pass the validator the server runs.
//
// The example's secret references point at an environment and a key file that a
// linting machine legitimately does not have, and --check reports those as
// warnings rather than failures — see loadConfig. Everything else is checked in
// full.
func TestCheckAcceptsTheShippedExample(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "config.example.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example configuration not present: %v", err)
	}
	out, code := runChild(t, t.TempDir(), "--check", "--config", path)
	if code != 0 {
		t.Fatalf("--check on the shipped example exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("--check said nothing about the outcome:\n%s", out)
	}
}

// TestCheckWarnsAboutAPerTokenRateAndStillStarts is the gateway's half of the
// advisory `dorangctl config lint` prints. --check is the last thing that runs
// before a deploy, and it answered `ok` while every request priced to a
// millionth of its card (CONFIG §13.1c) — the rule matched, so nothing else
// had anything to say.
//
// It stays an `ok`. A wrong price is a wrong invoice; refusing to start over
// one is a wrong invoice for everybody upstream at the same time (§8.3).
func TestCheckWarnsAboutAPerTokenRateAndStillStarts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dorang.yaml")
	const cfg = `version: 1
server: {env: development}
providers:
  - {name: p1, kind: openai, base_url: "https://example.invalid/v1"}
credentials:
  - {id: c1, provider: p1, key: dev-secret}
models:
  - name: m1
    deployments: [{provider: p1, upstream_model: u1, credentials: [c1]}]
pricing:
  currency: USD
  rules:
    - {id: r1, class: marginal_usage, match: {provider: p1}, rates: {input: "0.000003"}}
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runChild(t, dir, "--check", "--config", path)
	if code != 0 {
		t.Fatalf("--check exited %d on a legal configuration:\n%s", code, out)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("--check no longer reports the outcome:\n%s", out)
	}
	if !strings.Contains(out, "per MILLION tokens") {
		t.Errorf("--check said ok and nothing else about a rate a millionth of its card:\n%s", out)
	}
}

func TestVersionAndUsage(t *testing.T) {
	out, code := runChild(t, t.TempDir(), "--version")
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Errorf("--version exited %d with %q", code, out)
	}
}

// ---------------------------------------------------------------- helpers

// runChild runs the gateway to completion and returns everything it printed.
func runChild(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), childEnv+"=1", "HOME="+dir)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return buf.String(), 0
		}
		var ee *exec.ExitError
		if asExitError(err, &ee) {
			return buf.String(), ee.ExitCode()
		}
		t.Fatal(err)
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the process did not exit:\n%s", buf.String())
	}
	return buf.String(), -1
}

func waitReady(t *testing.T, c *child) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(c.url("/health/readiness"))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the gateway never became ready\nstderr:\n%s", c.err())
}

func waitExit(t *testing.T, c *child, within time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("exit: %v\nstderr:\n%s", err, c.err())
		}
	case <-time.After(within):
		_ = c.cmd.Process.Kill()
		t.Fatalf("the process did not exit within %s", within)
	}
}

type response struct {
	status int
	body   string
	header http.Header
}

func post(t *testing.T, url, token, body string) response {
	t.Helper()
	r, err := postErr(url, token, body, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func postErr(url, token, body string, timeout time.Duration) (response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Ask for the full extension-header set, so the routing decision is
	// observable rather than opt-out (DESIGN §10.4).
	req.Header.Set("X-Dorang-Detail", "full")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, body: string(b), header: resp.Header}, err
}

func get(t *testing.T, url, token string) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, body: string(b), header: resp.Header}
}

// TestHotReloadOnSIGHUP is §4.1's other half: every section reloads, and an
// in-flight snapshot is not disturbed. The observable proof is that a model
// group added to the file appears in GET /v1/models without a restart.
func TestHotReloadOnSIGHUP(t *testing.T) {
	dir := t.TempDir()
	up, _ := fakeUpstream(t, 0)
	cfgPath := writeConfig(t, dir, up.URL)

	c := start(t, []string{"--config", cfgPath},
		"HOME="+dir,
		"DORANG_MASTER_KEY=sk-master-test",
		"DORANG_KEY_PEPPER=test-pepper")
	waitReady(t, c)

	before := get(t, c.url("/v1/models"), "sk-master-test") // pragma: allowlist secret — test fixture
	if strings.Contains(before.body, "model-y") {
		t.Fatalf("model-y is present before the reload: %s", before.body)
	}

	// Add a second model group and a matching class, then ask for a reload.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(reorderForAppend(string(data))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		after := get(t, c.url("/v1/models"), "sk-master-test") // pragma: allowlist secret — test fixture
		if strings.Contains(after.body, "model-y") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SIGHUP did not reload the model list: %s\nstderr:\n%s", after.body, c.err())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The gateway still serves after the swap.
	resp := post(t, c.url("/v1/chat/completions"), "sk-master-test", chatRequest) // pragma: allowlist secret — test fixture
	if resp.status != http.StatusOK {
		t.Errorf("status after reload = %d, body = %s", resp.status, resp.body)
	}
	if code := c.signalAndWait(t, syscall.SIGTERM, 30*time.Second); code != 0 {
		t.Errorf("exit code = %d\nstderr:\n%s", code, c.err())
	}
}

// reorderForAppend inserts a second model group into the models: block of the
// configuration writeConfig produced.
func reorderForAppend(src string) string {
	const anchor = "pricing:\n"
	extra := `  - name: model-y
    deployments:
      - provider: fake
        upstream_model: upstream-y
        credentials: [fake-1]
`
	i := strings.Index(src, anchor)
	if i < 0 {
		return src + extra
	}
	return src[:i] + extra + src[i:]
}
