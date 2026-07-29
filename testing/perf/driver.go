package perf

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// request bodies
// -----------------------------------------------------------------------------

// Body renders a chat request of approximately size bytes.
//
// The filler goes in the LAST message so that two requests in the same
// conversation share a prefix: the prefix chain hashes the raw bytes in order,
// so padding at the front would make every request share everything.
func Body(conversation, size int, stream bool) []byte {
	head := `{"model":"` + Model + `","messages":[` +
		`{"role":"system","content":"You are a careful assistant. Conversation ` +
		strconv.Itoa(conversation) + `."},` +
		`{"role":"user","content":"`
	tail := `"}]`
	if stream {
		tail += `,"stream":true,"stream_options":{"include_usage":true}`
	}
	tail += `}`
	fill := size - len(head) - len(tail)
	if fill < 1 {
		fill = 1
	}
	var b strings.Builder
	b.Grow(size + 16)
	b.WriteString(head)
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789 "
	for i := range fill {
		b.WriteByte(alphabet[i%len(alphabet)])
	}
	b.WriteString(tail)
	return []byte(b.String())
}

// -----------------------------------------------------------------------------
// the driver
// -----------------------------------------------------------------------------

// Load describes one measured arm.
type Load struct {
	// Concurrency is how many client goroutines offer requests.
	Concurrency int
	// Requests is the total to send once recording starts.
	Requests int
	// Warmup is how many to send first, uncounted. The connection pool, the
	// auth cache, the pricing memo, the prefix table and SQLite's page cache
	// are all cold on the first request and none of them is what §15.1's
	// warm-local profile describes.
	Warmup int
	// Stream selects the streaming arm.
	Stream bool
	// BodySize is the request body size in bytes.
	BodySize int
	// Conversations is how many distinct prefixes the corpus holds.
	Conversations int
}

// Run offers the load and returns the recorded samples.
//
// The client-observed round trip is stitched onto each sample afterwards rather
// than during: correlating them per request would need a header, and stamping
// one would change the thing being measured. They are matched by arrival order,
// which is exact for the sequential arm and approximate for the parallel one —
// and E2E is only ever quoted as an upper bracket, never as the figure.
func Run(t testing.TB, g *Gateway, l Load) []Sample {
	t.Helper()
	if l.Concurrency <= 0 {
		l.Concurrency = 1
	}
	if l.BodySize <= 0 {
		l.BodySize = 4 << 10
	}
	if l.Conversations <= 0 {
		l.Conversations = 8
	}
	corpus := make([][]byte, l.Conversations)
	for i := range corpus {
		corpus[i] = Body(i, l.BodySize, l.Stream)
	}

	ctx := context.Background()
	warmup(t, g, l, corpus)

	g.ResetUpstreams()
	g.Collector.Start(l.Requests + 16)
	var (
		sent   atomic.Int64
		failed atomic.Int64
		e2e    = make([][]time.Duration, l.Concurrency)
		wg     sync.WaitGroup
	)
	start := time.Now()
	for w := range l.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mine := make([]time.Duration, 0, l.Requests/l.Concurrency+1)
			for i := 0; ; i++ {
				if sent.Add(1) > int64(l.Requests) {
					break
				}
				body := corpus[(i*l.Concurrency+w)%len(corpus)]
				t0 := time.Now()
				err := g.one(ctx, body, g.Secrets[(i*l.Concurrency+w)%len(g.Secrets)])
				mine = append(mine, time.Since(t0))
				if err != nil {
					failed.Add(1)
				}
			}
			e2e[w] = mine
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	g.Collector.Stop()

	if n := failed.Load(); n > 0 {
		t.Fatalf("perf: %d of %d requests failed", n, l.Requests)
	}
	if n := g.Collector.Dropped(); n > 0 {
		t.Fatalf("perf: the collector dropped %d samples", n)
	}

	samples := g.Collector.Samples()
	// Stitch the client-observed times on in sorted order. Both sides are the
	// same population; pairing them by rank is what makes the two distributions
	// comparable without a per-request join key.
	var flat []time.Duration
	for _, s := range e2e {
		flat = append(flat, s...)
	}
	sortDurations(flat)
	sortSamplesByTotal(samples)
	for i := range samples {
		if i < len(flat) {
			samples[i].E2E = flat[i]
		}
	}
	t.Logf("perf: %d requests in %v (%.0f req/s offered by %d clients), upstream saw %d",
		len(samples), elapsed.Round(time.Millisecond),
		float64(len(samples))/elapsed.Seconds(), l.Concurrency, g.UpstreamCount())
	return samples
}

// warmup runs the path until nothing on it is cold.
//
// "Warm" is not one thing and getting it wrong is how a warm-local measurement
// silently becomes a cold-auth one. Four caches have to be primed, and the
// first version of this function primed one of them:
//
//   - The connection pool, per client goroutine. A dial inside the measured
//     window is a millisecond.
//   - The AUTH CACHE, per api key. This is the one that was missed: the harness
//     issues many keys and rotated through all of them while warming only one,
//     so almost every measured request took a store read and was a §15.1
//     cold-auth sample wearing a warm-local label. It moved the reported p50 by
//     a factor of five.
//   - The prefix table and the sticky table, per conversation.
//   - The pricing memo and SQLite's page cache, which one request reaches.
func warmup(t testing.TB, g *Gateway, l Load, corpus [][]byte) {
	t.Helper()
	ctx := context.Background()
	per := max(l.Warmup/l.Concurrency, len(g.Secrets)/l.Concurrency+len(corpus)+2)
	var wg sync.WaitGroup
	for w := range l.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range per {
				// Stride the key population by worker so that between them
				// every issued key is authenticated at least once.
				secret := g.Secrets[(i*l.Concurrency+w)%len(g.Secrets)]
				if err := g.one(ctx, corpus[i%len(corpus)], secret); err != nil {
					t.Errorf("perf: warmup: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// one issues a single request and drains the answer.
func (g *Gateway) one(ctx context.Context, body []byte, secret string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	out, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(out, 400))
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// Throughput drives the gateway flat out for d and reports requests per second.
//
// It is a separate entry point from [Run] because the two questions are
// different: Run offers a fixed count and asks what each one cost, while this
// offers as much as the machine will carry and asks where the ceiling is.
func Throughput(t testing.TB, g *Gateway, concurrency int, d time.Duration, stream bool, bodySize int) (float64, []Sample) {
	t.Helper()
	corpus := make([][]byte, 16)
	for i := range corpus {
		corpus[i] = Body(i, bodySize, stream)
	}
	ctx := context.Background()

	warmup(t, g, Load{Concurrency: concurrency, Warmup: 4 * concurrency}, corpus)

	// Room for a quarter of a million requests a second for the whole run. A
	// collector that fills stops recording, and what it stops recording is the
	// END of the run — so a truncated distribution is not merely smaller, it is
	// a distribution of the first half of a load test. Sizing it generously and
	// failing on a drop is the only version of this that can be quoted.
	g.ResetUpstreams()
	g.Collector.Start(int(250_000 * d.Seconds()))
	var (
		done   atomic.Int64
		failed atomic.Int64
		stop   = make(chan struct{})
		wg     sync.WaitGroup
	)
	for w := range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := g.one(ctx, corpus[(i+w)%len(corpus)], g.Secrets[(i+w)%len(g.Secrets)]); err != nil {
					failed.Add(1)
				}
				done.Add(1)
			}
		}()
	}
	start := time.Now()
	time.Sleep(d)
	close(stop)
	wg.Wait()
	elapsed := time.Since(start)
	g.Collector.Stop()

	if n := failed.Load(); n > 0 {
		t.Errorf("perf: %d requests failed under load", n)
	}
	if n := g.Collector.Dropped(); n > 0 {
		t.Errorf("perf: the collector dropped %d samples; the distribution covers "+
			"only the start of the run and must not be quoted", n)
	}
	out := g.Collector.Samples()
	g.ResetUpstreams()
	return float64(done.Load()) / elapsed.Seconds(), out
}
