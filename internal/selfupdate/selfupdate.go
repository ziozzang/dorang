// Package selfupdate finds dorang's GitHub releases and installs them.
//
// A release carries one raw executable per binary and platform, named
// `<binary>_<version>_<goos>_<goarch>` (dorang_0.1.0_linux_amd64), and a
// SHA256SUMS file covering every asset. An executable is installed only after
// its digest matches SHA256SUMS, and it replaces the running one by rename in
// the same directory, so a reader never sees a half-written file.
//
// The same lookup drives three things: `dorang update` and `dorangctl update`
// (replace this executable), the gateway's once-a-day notice that a newer
// release exists, and `dorangctl update --deploy` (move a Compose deployment to
// the release's container image).
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo is where releases are published.
const Repo = "ziozzang/dorang"

// Image is the container image a release publishes, tagged with the version
// without its leading "v" (ghcr.io/ziozzang/dorang:0.1.0).
const Image = "ghcr.io/ziozzang/dorang"

// EnvNoCheck disables the gateway's background release check.
const EnvNoCheck = "DORANG_NO_UPDATE_CHECK"

// CheckInterval is how often the gateway asks for the latest release.
const CheckInterval = 24 * time.Hour

const githubAPI = "https://api.github.com"

// Release is one GitHub release.
type Release struct {
	TagName string  `json:"tag_name"`
	Body    string  `json:"body"`
	Assets  []Asset `json:"assets"`
}

// Asset is one file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Version is the tag without its leading "v".
func (r *Release) Version() string { return strings.TrimPrefix(r.TagName, "v") }

// Find returns the asset of that name.
func (r *Release) Find(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Client is the HTTP side of a lookup. The zero value uses the GitHub API and
// http.DefaultClient; tests point API at a fake.
type Client struct {
	HTTP  *http.Client
	API   string
	Repo  string
	Token string // optional; raises the API's rate limit
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) endpoint(tag string) string {
	api, repo := c.API, c.Repo
	if api == "" {
		api = githubAPI
	}
	if repo == "" {
		repo = Repo
	}
	if tag == "" {
		return api + "/repos/" + repo + "/releases/latest"
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	return api + "/repos/" + repo + "/releases/tags/" + tag
}

// Fetch returns the latest release, or the one tagged tag.
func (c *Client) Fetch(ctx context.Context, tag string) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(tag), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "dorang-selfupdate")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("GitHub API %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, errors.New("the release has no tag")
	}
	return &rel, nil
}

// AssetName is the raw executable of binary for a platform.
func AssetName(binary, version, goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64":
	default:
		return "", fmt.Errorf("no release build for %s/%s", goos, goarch)
	}
	return fmt.Sprintf("%s_%s_%s_%s", binary, strings.TrimPrefix(version, "v"), goos, goarch), nil
}

var releaseVersion = regexp.MustCompile(`^v?\d+\.\d+\.\d+([-+].*)?$`)

// IsRelease reports whether v is a release version rather than a development
// build ("devel", a commit-stamped export, "dev").
func IsRelease(v string) bool { return releaseVersion.MatchString(strings.TrimSpace(v)) }

// Compare orders two versions numerically by their dotted fields, ignoring a
// leading "v" and any pre-release or build suffix. A version that is not a
// release compares below every release.
func Compare(a, b string) int {
	af, bf := fields(a), fields(b)
	for i := 0; i < len(af) || i < len(bf); i++ {
		var x, y int
		if i < len(af) {
			x = af[i]
		}
		if i < len(bf) {
			y = bf[i]
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

func fields(v string) []int {
	if !IsRelease(v) {
		return nil
	}
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// Checksums reads the release's SHA256SUMS.
func (c *Client) Checksums(ctx context.Context, rel *Release) (map[string]string, error) {
	a, ok := rel.Find("SHA256SUMS")
	if !ok {
		return nil, fmt.Errorf("release %s has no SHA256SUMS", rel.TagName)
	}
	b, err := c.download(ctx, a.URL, 1<<20)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && len(f[0]) == 64 {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out, nil
}

func (c *Client) download(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "dorang-selfupdate")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// DownloadVerified writes the asset to a temporary file in dir and returns its
// path, only if its SHA-256 is want. The file is executable.
func (c *Client) DownloadVerified(ctx context.Context, a Asset, want, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "dorang-selfupdate")
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", a.Name, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, ".dorang-update-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			os.Remove(name)
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, 512<<20)); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", a.Name, got, want)
	}
	if err := os.Chmod(name, 0o755); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

// Executable is the running executable with symlinks resolved, so that an
// update replaces the file and not the link to it.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// Options is one `update` invocation.
type Options struct {
	Binary  string // "dorang" or "dorangctl"
	Current string // the running version
	Tag     string // install this release instead of the latest
	Check   bool   // report only
	Force   bool   // reinstall even when current
	Out     io.Writer
}

// Update replaces the running executable with a release's build of it.
func (c *Client) Update(ctx context.Context, o Options) error {
	rel, err := c.Fetch(ctx, o.Tag)
	if err != nil {
		return fmt.Errorf("release lookup: %w", err)
	}
	latest := rel.Version()
	cmp := Compare(latest, o.Current)
	switch {
	case cmp > 0:
		fmt.Fprintf(o.Out, "%s %s -> %s is available\n", o.Binary, o.Current, latest)
	case cmp == 0:
		fmt.Fprintf(o.Out, "%s %s is the latest release\n", o.Binary, o.Current)
	default:
		fmt.Fprintf(o.Out, "%s %s is newer than release %s\n", o.Binary, o.Current, latest)
	}
	if o.Check || (cmp <= 0 && o.Tag == "" && !o.Force) {
		return nil
	}
	name, err := AssetName(o.Binary, latest, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	a, ok := rel.Find(name)
	if !ok {
		return fmt.Errorf("release %s has no asset %s", rel.TagName, name)
	}
	sums, err := c.Checksums(ctx, rel)
	if err != nil {
		return err
	}
	want := sums[name]
	if want == "" {
		return fmt.Errorf("SHA256SUMS has no checksum for %s", name)
	}
	exe, err := Executable()
	if err != nil {
		return err
	}
	tmp, err := c.DownloadVerified(ctx, a, want, filepath.Dir(exe))
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot update %s without write permission to its directory: %w", exe, err)
		}
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	fmt.Fprintf(o.Out, "updated %s: %s -> %s\n", exe, o.Current, latest)
	return nil
}

// Notify asks for the latest release now and then every interval until ctx
// ends, and calls logf once for each newer release it sees. It does nothing
// for a development build or when EnvNoCheck is set, and a failed lookup is
// silent: a release check is never a reason for a gateway to say anything
// about its own health.
func (c *Client) Notify(ctx context.Context, current string, interval time.Duration, logf func(string, ...any)) {
	if os.Getenv(EnvNoCheck) != "" || !IsRelease(current) {
		return
	}
	announced := ""
	check := func() {
		lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		rel, err := c.Fetch(lctx, "")
		if err != nil {
			return
		}
		if v := rel.Version(); Compare(v, current) > 0 && v != announced {
			announced = v
			logf("dorang: release %s is available (running %s): https://github.com/%s/releases/tag/%s",
				v, current, Repo, rel.TagName)
		}
	}
	go func() {
		check()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				check()
			}
		}
	}()
}
