// Command dorang is the gateway server.
//
// One process serves every protocol surface; there is no worker split (DESIGN
// §1). It loads a configuration file, opens the store, assembles the subsystems
// through internal/app, serves, hot-reloads on SIGHUP or a change to the file,
// and drains on SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/config"
)

// version is stamped at build time with -ldflags "-X main.version=…". Without
// it the module's own build information answers, which is what `go install`
// produces.
var version = ""

// EnvConfigPath names the environment variable that supplies --config's default.
const EnvConfigPath = "DORANG_CONFIG"

// DefaultConfigPath is where the server looks when neither --config nor
// DORANG_CONFIG names a file.
//
// A MISSING file at this path is not an error. DESIGN §0.2 requires a single
// binary with no required dependencies to serve requests, and a configuration
// file is a dependency like any other: with no file, the documented defaults
// apply and the gateway comes up on SQLite with no upstreams. A file named
// explicitly and then not found IS an error, because that is a typo, not a
// choice.
const DefaultConfigPath = "dorang.yaml"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dorang", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: dorang [flags]\n\nflags:\n")
		fs.PrintDefaults()
	}
	var (
		path        = fs.String("config", "", "configuration file (default $DORANG_CONFIG or "+DefaultConfigPath+")")
		check       = fs.Bool("check", false, "validate the configuration and exit")
		showVersion = fs.Bool("version", false, "print the version and exit")
		listen      = fs.String("listen", "", "override server.listen")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, versionString())
		return 0
	}

	configPath, explicit := resolveConfigPath(*path)
	cfg, warnings, err := loadConfig(configPath, explicit, *check)
	for _, w := range warnings {
		fmt.Fprintf(stderr, "dorang: warning: %s\n", w)
	}
	if err != nil {
		reportConfigError(stderr, configPath, err)
		return 1
	}

	if *check {
		// Valid is not the same as intended. --check is the last thing that runs
		// before a deploy and the last chance to say that a rate is a millionth
		// of what its card says; it stays an `ok` because pricing is never a
		// reason to refuse to serve (§8.3).
		for _, a := range cfg.Advisories() {
			fmt.Fprintf(stderr, "dorang: warning: %s\n", a)
		}
		fmt.Fprintf(stdout, "%s: ok\n", describeSource(configPath, explicit))
		return 0
	}
	if *listen != "" {
		cfg.Server.Listen = *listen
	}
	return serve(cfg, configPath, explicit, stdout, stderr)
}

// resolveConfigPath applies the flag, then the environment, then the default.
func resolveConfigPath(flagValue string) (path string, explicit bool) {
	if flagValue != "" {
		return flagValue, true
	}
	if v := os.Getenv(EnvConfigPath); v != "" {
		return v, true
	}
	return DefaultConfigPath, false
}

// loadConfig reads and validates the file.
//
// Under --check, a secret that cannot be READ here is a warning rather than a
// failure: a configuration is routinely linted on a machine that does not hold
// the deployment's key material, and refusing to check the other four hundred
// lines because of it helps nobody. Everything else stays an error, including
// an inline literal secret outside development — that is a refusal to start
// (§4.1), not an environment difference.
func loadConfig(path string, explicit, check bool) (*config.Config, []string, error) {
	if !explicit {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			cfg, err := config.LoadBytes([]byte("version: 1\n"))
			return cfg, nil, err
		}
	}
	if !check {
		cfg, err := config.Load(path)
		return cfg, nil, err
	}
	// Under --check the policy is app.CheckConfig's, which dorangctl uses too:
	// one answer, not two that happen to agree today.
	cfg, warnings, err := app.CheckConfig(path)
	return cfg, warnings, err
}

// reportConfigError prints every problem at once.
//
// internal/config collects them rather than stopping at the first, and the three
// configurations DESIGN refuses to start on — clustering with local capacity
// accounting (§5.6), an inline literal secret outside development (§4.1), and
// legacy key hashing with no expiry date (§2.4) — arrive through exactly this
// path. Printing only "invalid configuration" would swallow the one line that
// says which of them it was.
func reportConfigError(w io.Writer, path string, err error) {
	problems := config.Problems(err)
	if len(problems) == 0 {
		fmt.Fprintf(w, "dorang: %s: %v\n", path, err)
		return
	}
	fmt.Fprintf(w, "dorang: %s: refusing to start, %d problem(s):\n", path, len(problems))
	for _, p := range problems {
		fmt.Fprintf(w, "  %s\n", p.Error())
	}
}

func describeSource(path string, explicit bool) string {
	if !explicit {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return "built-in defaults (no configuration file)"
		}
	}
	return path
}

// serve assembles the gateway, listens, and drains on a signal.
func serve(cfg *config.Config, path string, explicit bool, stdout, stderr io.Writer) int {
	logger := log.New(stderr, "", log.LstdFlags|log.LUTC)
	logf := func(format string, args ...any) { logger.Printf(format, args...) }

	a, err := app.New(context.Background(), app.Options{
		Config: cfg,
		Logf:   logf,
	})
	if err != nil {
		fmt.Fprintf(stderr, "dorang: %v\n", err)
		return 1
	}

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "dorang: listen %s: %v\n", cfg.Server.Listen, err)
		_ = a.Close(context.Background())
		return 1
	}

	// The watcher covers both hot-reload triggers of §4.1: a change to the file
	// and SIGHUP. It is the same code internal/config tests, rather than a
	// second implementation here.
	if explicit || fileExists(path) {
		w, werr := config.NewWatcher(path,
			config.WithSignals(syscall.SIGHUP),
			config.WithReloadHandler(func(c *config.Config) {
				if err := a.Reload(c); err != nil {
					logf("dorang: reload refused, keeping the running configuration: %v", err)
					return
				}
				logf("dorang: configuration reloaded from %s", path)
			}),
			config.WithErrorHandler(func(err error) {
				logf("dorang: reload failed, keeping the running configuration: %v", err)
			}),
		)
		if werr != nil {
			logf("dorang: configuration watch disabled: %v", werr)
		} else {
			w.Start()
			defer w.Close()
		}
	}

	fmt.Fprintf(stdout, "dorang %s listening on %s (%s)\n",
		versionString(), ln.Addr(), describeSource(path, explicit))

	// ServeSignals rather than a second signal loop here. The wiring it holds —
	// the pre-stop delay of §13, and the second signal that skips it — is the
	// wiring internal/server tests; a copy of it in this file would be the copy
	// nobody exercises.
	serveErr := a.Server.ServeSignals(ln)

	// The drain is over; everything the process owns comes down in order. The
	// grace here is the configured one again, so a slow final metering flush
	// cannot hold the process open past what the operator asked for.
	//
	// The whole budget an orchestrator must allow for is therefore
	// `pre_stop_delay + 2 x shutdown_grace`, plus a few seconds for the two
	// bounded cut-over waits inside the drain (§13).
	grace := cfg.Server.ShutdownGrace.Duration()
	if grace <= 0 {
		grace = 30 * time.Second
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := a.Close(closeCtx); err != nil {
		logf("dorang: shutdown: %v", err)
	}

	switch {
	case serveErr == nil:
		return 0
	case errors.Is(serveErr, context.DeadlineExceeded):
		fmt.Fprintln(stderr, "dorang: drain grace expired with requests still in flight")
		return 1
	default:
		fmt.Fprintf(stderr, "dorang: %v\n", serveErr)
		return 1
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "devel"
}
