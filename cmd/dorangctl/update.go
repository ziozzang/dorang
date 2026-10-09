package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ziozzang/dorang/internal/selfupdate"
)

// `dorangctl update` installs a release.
//
// Without --deploy it replaces this executable, exactly as `dorang update`
// replaces the gateway's. With --deploy DIR it moves a Docker Compose
// deployment to the release's container image instead:
//
//  1. DIR/.env names the running release (DORANG_VERSION) and the image the
//     Compose file runs (DORANG_IMAGE). A deployment without DORANG_IMAGE is
//     not release-based and is refused rather than converted.
//  2. The release image is pulled, and its own `dorangctl migrate` runs once
//     against the deployment's database through `docker compose run`, so a
//     migration that fails stops the update before anything was replaced.
//  3. .env is rewritten in place (backup beside it) and the services are
//     recreated with --no-deps --pull never; the database is never touched.
//  4. The health URL must answer 200 and the gateway must report the new
//     version. Otherwise .env is restored and the services recreated on the
//     previous image, and the command fails.
//
// One run at a time per directory (an flock on DIR/.dorang-update.lock), so a
// timer that fires while a slow update is still running does nothing.

func (e env) runUpdate(args []string) int {
	fs := newFlagSet("update", e)
	var (
		check    = fs.Bool("check", false, "only report whether a newer release exists")
		force    = fs.Bool("force", false, "install even when already current")
		tag      = fs.String("version", "", "install this release instead of the latest")
		deploy   = fs.String("deploy", "", "a Docker Compose deployment directory to move to the release image")
		services = fs.String("services", "dorang,dorang-catalog-sync", "with --deploy: the services to recreate; the first is the gateway")
		health   = fs.String("health", "http://127.0.0.1:4000/health/liveliness", "with --deploy: the gateway's health URL")
		wait     = fs.Duration("health-timeout", 3*time.Minute, "with --deploy: how long the new gateway has to become healthy")
		cfgPath  = fs.String("container-config", "/etc/dorang/config.yaml", "with --deploy: the configuration path inside the container")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	c := &selfupdate.Client{Token: os.Getenv("GITHUB_TOKEN")}
	if *deploy == "" {
		if err := c.Update(ctx, selfupdate.Options{
			Binary: "dorangctl", Current: versionString(), Tag: *tag,
			Check: *check, Force: *force, Out: e.stdout,
		}); err != nil {
			return e.fail("update: %v", err)
		}
		return 0
	}
	d := &deployment{
		dir: *deploy, services: splitComma(*services), health: *health, wait: *wait,
		containerConfig: *cfgPath, out: e.stdout, run: dockerRun, client: c,
		httpClient: &http.Client{Timeout: 5 * time.Second}, poll: 3 * time.Second,
	}
	if err := d.update(ctx, *tag, *check, *force); err != nil {
		return e.fail("update --deploy: %v", err)
	}
	return 0
}

// runner runs one command and returns its combined output.
type runner func(ctx context.Context, env []string, name string, args ...string) (string, error)

func dockerRun(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	return b.String(), err
}

type deployment struct {
	dir             string
	services        []string
	health          string
	wait            time.Duration
	containerConfig string
	out             interface{ Write([]byte) (int, error) }
	run             runner
	client          *selfupdate.Client
	httpClient      *http.Client
	poll            time.Duration
}

func (d *deployment) logf(format string, args ...any) {
	fmt.Fprintf(d.out, format+"\n", args...)
}

func (d *deployment) compose(ctx context.Context, env []string, args ...string) (string, error) {
	return d.run(ctx, env, "docker", append([]string{"compose", "--project-directory", d.dir}, args...)...)
}

func (d *deployment) update(ctx context.Context, tag string, check, force bool) error {
	if len(d.services) == 0 {
		return errors.New("--services names no service")
	}
	unlock, err := lockDir(d.dir)
	if err != nil {
		return err
	}
	defer unlock()

	envPath := filepath.Join(d.dir, ".env")
	raw, err := os.ReadFile(envPath)
	if err != nil {
		return err
	}
	vars := parseEnv(raw)
	if vars["DORANG_IMAGE"] == "" {
		return fmt.Errorf("%s sets no DORANG_IMAGE: this deployment does not run a release image "+
			"(set DORANG_IMAGE=%s:<version> and image: ${DORANG_IMAGE} in the Compose file first)",
			envPath, selfupdate.Image)
	}
	current := vars["DORANG_VERSION"]

	rel, err := d.client.Fetch(ctx, tag)
	if err != nil {
		return fmt.Errorf("release lookup: %w", err)
	}
	latest := rel.Version()
	cmp := selfupdate.Compare(latest, current)
	d.logf("deployment %s runs %s; release %s", d.dir, current, latest)
	if check || (cmp <= 0 && tag == "" && !force) {
		return nil
	}
	image := selfupdate.Image + ":" + latest
	prevImage := vars["DORANG_IMAGE"]

	d.logf("pulling %s", image)
	if out, err := d.run(ctx, nil, "docker", "pull", image); err != nil {
		return fmt.Errorf("docker pull %s: %v\n%s", image, err, out)
	}
	next := []string{"DORANG_IMAGE=" + image}
	d.logf("migrating the database with %s", image)
	if out, err := d.compose(ctx, next, "run", "--rm", "--no-deps", "-T", "--entrypoint", "dorangctl",
		d.services[0], "migrate", "--config", d.containerConfig); err != nil {
		return fmt.Errorf("migrate: %v\n%s", err, out)
	}

	backup := envPath + ".bak-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.WriteFile(backup, raw, 0o600); err != nil {
		return err
	}
	updated := setEnv(raw, map[string]string{"DORANG_IMAGE": image, "DORANG_VERSION": latest})
	if err := writeInPlace(envPath, updated); err != nil {
		return err
	}
	d.logf("recreating %s on %s (previous %s, .env backup %s)", strings.Join(d.services, ", "), image, prevImage, backup)
	if err := d.roll(ctx, "v"+latest); err != nil {
		d.logf("rolling back to %s: %v", prevImage, err)
		if werr := writeInPlace(envPath, raw); werr != nil {
			return fmt.Errorf("%v; and restoring .env failed: %v", err, werr)
		}
		if rerr := d.roll(ctx, ""); rerr != nil {
			return fmt.Errorf("%v; and the rollback did not come up healthy: %v", err, rerr)
		}
		return fmt.Errorf("release %s was rolled back: %v", latest, err)
	}
	d.logf("deployment %s now runs %s", d.dir, latest)
	return nil
}

// roll recreates the services from the current .env and waits for health. A
// non-empty want is the version the gateway must report.
func (d *deployment) roll(ctx context.Context, want string) error {
	// --no-build as well as --pull never: a Compose file that still carries a
	// build: section must not turn a missing image into a local build.
	args := append([]string{"up", "-d", "--no-deps", "--no-build", "--pull", "never"}, d.services...)
	if out, err := d.compose(ctx, nil, args...); err != nil {
		return fmt.Errorf("compose up: %v\n%s", err, out)
	}
	deadline := time.Now().Add(d.wait)
	var last error
	for time.Now().Before(deadline) {
		if last = d.healthy(ctx); last == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.poll):
		}
	}
	if last != nil {
		return fmt.Errorf("not healthy within %s: %v", d.wait, last)
	}
	if want == "" {
		return nil
	}
	out, err := d.compose(ctx, nil, "exec", "-T", d.services[0], "dorang", "-version")
	if err != nil {
		return fmt.Errorf("version check: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(out); selfupdate.Compare(got, want) != 0 {
		return fmt.Errorf("the gateway reports %q, want %s", got, want)
	}
	return nil
}

func (d *deployment) healthy(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.health, nil)
	if err != nil {
		return err
	}
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", d.health, resp.StatusCode)
	}
	return nil
}

func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".dorang-update.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another update of %s is running", dir)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// parseEnv reads KEY=VALUE lines; comments and blank lines are skipped.
func parseEnv(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}

// setEnv replaces the values of the named keys, appending any that are absent,
// and leaves every other line — comments included — as it was.
func setEnv(b []byte, set map[string]string) []byte {
	lines := strings.Split(string(b), "\n")
	seen := map[string]bool{}
	for i, line := range lines {
		k, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		k = strings.TrimSpace(k)
		if v, want := set[k]; want {
			lines[i] = k + "=" + v
			seen[k] = true
		}
	}
	out := strings.Join(lines, "\n")
	for _, k := range []string{"DORANG_IMAGE", "DORANG_VERSION"} {
		if v, want := set[k]; want && !seen[k] {
			if !strings.HasSuffix(out, "\n") && out != "" {
				out += "\n"
			}
			out += k + "=" + v + "\n"
		}
	}
	return []byte(out)
}

// writeInPlace rewrites a file without changing its inode, so a bind mount or
// an open handle keeps seeing it, and keeps its mode.
func writeInPlace(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
