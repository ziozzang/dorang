package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TrafficBuffer retains only bounded request metadata, never headers or payloads.
// The durable ledger remains the source for historical queries and billing.
type TrafficBuffer struct {
	mu       sync.Mutex
	rows     [256]LiveRequest
	next     int
	count    int
	sequence uint64
}

type LiveRequest struct {
	Sequence   uint64    `json:"sequence"`
	At         time.Time `json:"at"`
	ID         string    `json:"id"`
	Model      string    `json:"model"`
	Provider   string    `json:"provider"`
	Deployment string    `json:"deployment"`
	Credential string    `json:"credential"`
	Key        string    `json:"key"`
	Team       string    `json:"team"`
	User       string    `json:"user"`
	Endpoint   string    `json:"endpoint"`
	Status     int       `json:"status"`
	Error      string    `json:"error"`
	LatencyMS  int64     `json:"latency_ms"`
	TTFTMS     int64     `json:"ttft_ms"`
	WaitMS     int64     `json:"wait_ms"`
	Input      int64     `json:"input"`
	Output     int64     `json:"output"`
	CacheRead  int64     `json:"cache_read"`
	CacheWrite int64     `json:"cache_write"`
	Reasoning  int64     `json:"reasoning"`
	Cost       Money     `json:"cost"`
	Priced     bool      `json:"priced"`
	Streamed   bool      `json:"streamed"`
}

func (b *TrafficBuffer) Record(r LiveRequest) {
	// Strings may be backed by a much larger incoming request; detach and cap them.
	for _, p := range []*string{&r.ID, &r.Model, &r.Provider, &r.Deployment, &r.Credential, &r.Key, &r.Team, &r.User, &r.Endpoint, &r.Error} {
		*p = strings.Clone((*p)[:min(len(*p), 256)])
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sequence++
	r.Sequence = b.sequence
	b.rows[b.next] = r
	b.next = (b.next + 1) % len(b.rows)
	b.count = min(b.count+1, len(b.rows))
}

func (b *TrafficBuffer) Recent(now time.Time) []LiveRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]LiveRequest, 0, b.count)
	for i := 0; i < b.count; i++ {
		r := b.rows[(b.next-1-i+len(b.rows))%len(b.rows)]
		if !r.At.Before(now.Add(-15 * time.Minute)) {
			out = append(out, r)
		}
	}
	return out
}

// PrometheusSource is the same registry the HTTP /metrics endpoint serves.
type PrometheusSource interface{ Metrics([]byte) []byte }

type telemetrySnapshot struct {
	At          time.Time          `json:"at"`
	Surface     *Surface           `json:"surface,omitempty"`
	Prometheus  string             `json:"prometheus,omitempty"`
	Requests    []LiveRequest      `json:"requests"`
	Credentials []credentialView   `json:"credentials"`
	Capacity    *CapacityOccupancy `json:"capacity,omitempty"`
	Problems    []string           `json:"problems"`
}

type telemetryCache struct {
	mu       sync.Mutex
	at       time.Time
	snapshot telemetrySnapshot
	streams  atomic.Int32
}

func (a *API) telemetrySnapshot(ctx context.Context) telemetrySnapshot {
	a.telemetry.mu.Lock()
	defer a.telemetry.mu.Unlock()
	if time.Since(a.telemetry.at) < 2*time.Second {
		return a.telemetry.snapshot
	}
	cfg := a.cfg
	out := telemetrySnapshot{At: a.now(), Requests: []LiveRequest{}, Credentials: []credentialView{}, Problems: []string{}}
	if cfg.Surface != nil {
		sf, err := cfg.Surface.Surface(ctx)
		if err != nil {
			out.Problems = append(out.Problems, "Runtime counters unavailable")
		} else {
			out.Surface = &sf
		}
	}
	if cfg.Prometheus != nil {
		raw := cfg.Prometheus.Metrics(nil)
		if len(raw) > 4<<20 {
			out.Problems = append(out.Problems, "Prometheus snapshot exceeds 4 MiB")
		} else {
			out.Prometheus = string(raw)
		}
	}
	if cfg.Traffic != nil {
		out.Requests = cfg.Traffic.Recent(out.At)
	}
	if cfg.Credentials != nil {
		cs, err := cfg.Credentials.Credentials(ctx)
		if err != nil {
			out.Problems = append(out.Problems, "Credential health unavailable")
		} else {
			for _, c := range cs {
				out.Credentials = append(out.Credentials, viewCredential(c))
			}
		}
	}
	if cfg.Capacity != nil {
		cap, err := cfg.Capacity.Occupancy(ctx)
		if err != nil {
			out.Problems = append(out.Problems, "Capacity unavailable")
		} else {
			out.Capacity = &cap
		}
	}
	a.telemetry.at = time.Now()
	a.telemetry.snapshot = out
	return out
}

func (c *call) adminTelemetry() error {
	if err := c.requireGlobal("live telemetry"); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, c.a.telemetrySnapshot(c.ctx()))
	return nil
}

func (c *call) adminRecentRequests() error {
	if err := c.requireGlobal("live request metadata"); err != nil {
		return err
	}
	if c.a.cfg.Traffic == nil {
		return dependencyOff("live request recorder", "recent requests")
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"requests": c.a.cfg.Traffic.Recent(c.a.now()), "retention_seconds": 900, "capacity": 256, "scope": "this node"})
	return nil
}

// Each connection is bounded in time and write latency. Authorization is rechecked
// before every frame; collectors are shared across tabs through a two-second cache.
func (s *uiServer) streamTelemetry(w http.ResponseWriter, r *http.Request, v viewer) {
	if !v.scope.Global {
		http.Error(w, "Global operator access required", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "text/event-stream")
		return
	}
	if s.api.telemetry.streams.Add(1) > 16 {
		s.api.telemetry.streams.Add(-1)
		http.Error(w, "Too many live views; close another tab", http.StatusTooManyRequests)
		return
	}
	defer s.api.telemetry.streams.Add(-1)
	ctl := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lifetime := time.NewTimer(25 * time.Second)
	defer lifetime.Stop()
	for {
		if v.session != "" {
			sess, ok := s.lookupSession(v.session)
			if !ok || !s.sessionAuthorized(r.Context(), sess) {
				_ = ctl.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_, _ = fmt.Fprint(w, "event: expired\ndata: {}\n\n")
				_ = ctl.Flush()
				return
			}
		} else if p, err := s.api.authenticate(r); err != nil || !p.AdminScope().Global {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		data := s.api.telemetrySnapshot(ctx)
		cancel()
		if r.Context().Err() != nil {
			return
		}
		raw, err := json.Marshal(data)
		if err != nil {
			return
		}
		_ = ctl.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err = fmt.Fprintf(w, "retry: 2000\nevent: telemetry\ndata: %s\n\n", raw); err != nil {
			return
		}
		if ctl.Flush() != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-lifetime.C:
			return
		case <-ticker.C:
		}
	}
}
