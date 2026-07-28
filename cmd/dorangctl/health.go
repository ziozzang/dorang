package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// EnvListen supplies the default --addr, so a container that moved the listener
// does not also have to rewrite its health probe.
const EnvListen = "DORANG_LISTEN"

// defaultHealthAddr matches config's default server.listen.
const defaultHealthAddr = "http://127.0.0.1:4100"

// runHealth probes a running gateway and exits 0 only when it answers healthy.
//
// It exists because the container image's HEALTHCHECK has always invoked
// `dorangctl health --addr …` and no such subcommand existed: the CLI answered
// `unknown command "health"` and exited 2, so every container built from this
// image reported unhealthy after start-period + 3 × interval, forever. The image
// is distroless — there is no curl, no wget and no shell — so the probe has to
// be the binary itself.
//
// Liveness is the default for the same reason the Dockerfile comment gives: a
// draining node is alive and must not be restarted (§13), and a probe that
// conflates the two turns a graceful drain into a kill.
func (e env) runHealth(args []string) int {
	fs := newFlagSet("health", e)
	addr := fs.String("addr", defaultListenAddr(), "gateway base URL")
	path := fs.String("path", "/health/liveliness", "health path to probe")
	ready := fs.Bool("ready", false, "probe readiness instead of liveness")
	timeout := fs.Duration("timeout", 3*time.Second, "probe timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *ready {
		*path = "/health/readiness"
	}

	url := strings.TrimRight(*addr, "/") + *path
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return e.fail("health: %v", err)
	}
	// A fresh client with no proxy and no keep-alive: a probe that reused a
	// connection could report a stale success, and a probe routed through an
	// egress proxy would be checking the proxy.
	client := &http.Client{
		Timeout:   *timeout,
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
	}
	resp, err := client.Do(req)
	if err != nil {
		return e.fail("health: %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return e.fail("health: %s answered %d", url, resp.StatusCode)
	}
	fmt.Fprintf(e.stdout, "ok %s\n", url)
	return 0
}

// defaultListenAddr turns the configured listen address into a URL to probe.
//
// A bare ":4100" or "0.0.0.0:4100" is a bind address, not a destination: the
// probe runs inside the container, so it dials loopback on the same port.
func defaultListenAddr() string {
	v := strings.TrimSpace(os.Getenv(EnvListen))
	if v == "" {
		return defaultHealthAddr
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		return v
	}
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return defaultHealthAddr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
