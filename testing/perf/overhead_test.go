package perf

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// DESIGN §15.1's gateway-overhead budget, measured end to end.
//
// # What "warm-local" means here, precisely
//
// §15.1 names the profile as "auth cached, local capacity, prefix on, ≤4 KiB
// body". Every one of those is arranged rather than assumed:
//
//   - auth cached — the warm-up authenticates every issued key at least once.
//     The first version of this harness warmed one key and rotated through
//     sixty-four, so almost every measured request took a store read and was a
//     cold-auth sample wearing a warm-local label. It moved the reported p50 by
//     a factor of five, which is the size of the mistake available here.
//   - local capacity — every axis of §5.1 has a real ceiling, four thousand
//     times larger than anything offered, so the broker acquires and releases
//     on every request and nothing ever queues.
//   - prefix on — the table is consulted and written per request.
//   - ≤4 KiB body — the arms sweep 256 B to 4 KiB, which is the profile's
//     stated ceiling, and the headline is quoted AT the ceiling. Quoting a
//     smaller body would be picking the number.
//
// Metering is on with traces at full sampling, a hundred pricing rules are
// loaded and two deployments compete, because §15.1's budget explicitly
// includes "authentication, routing, capacity acquisition, pricing, parameter
// transformation, and enqueueing metering".

// warmLocalP50 and warmLocalP99 are §15.1's published warm-local budget, as
// corrected to what this package measures.
//
// The p50 was published as 200 µs and was never measured end to end. It holds
// for a request of a few hundred bytes and not at the profile's stated 4 KiB
// ceiling, where the JSON passes over the body dominate everything else. This
// figure has now been corrected three times: up to 480 µs when it was first
// measured here, down to 375 µs when two duplicate parses came out of the
// decode, and down to 280 µs when the encode stopped re-scanning every nested
// type once per level of its nesting (§15.1, "A Marshaler's document is
// compacted into its parent"). It is still the codec that sets it. The p99 was
// published as 2 ms and holds with better than a factor of four.
const (
	warmLocalP50 = 280 * time.Microsecond
	warmLocalP99 = 2 * time.Millisecond
)

// p50Bound is what the gate asserts. It is not the budget: a budget is a
// statement about a quiet machine and a gate has to survive a busy one, so it
// is roughly twice the measured figure. What keeps it from being a bound that
// passes anything is the disagreement check below — a machine that cannot
// answer is skipped rather than accommodated, which is how a gate stays a gate
// instead of drifting upward every time CI is loaded.
//
// It came DOWN from 900 to 750 with the decode measurement and from 750 to 560
// with the encode one, which is the direction a bound derived from a
// measurement has to move when the measurement improves. A ceiling left where
// it was would stop catching the regression it exists for.
const p50Bound = 560 * time.Microsecond

// measurable is how far two measurements of the same thing may differ before
// the host is judged unable to answer. Same instrument and same reasoning as
// TestScenario16: measure twice, and when the machine disagrees with itself the
// number is noise and asserting a bound on noise fails honest builds and passes
// dishonest ones with equal probability.
const measurable = 0.40

func disagreement(a, b time.Duration) float64 {
	if a <= 0 || b <= 0 {
		return 1
	}
	lo, hi := a, b
	if lo > hi {
		lo, hi = hi, lo
	}
	return float64(hi-lo) / float64(hi)
}

// gated reports whether a wall-clock bound may be asserted, and says why not
// when it may not.
func gated(t *testing.T) bool {
	t.Helper()
	if raceEnabled {
		t.Log("NOTE: race-instrumented build; the figures above are inflated by " +
			"three to five times and no bound is asserted")
		return false
	}
	return true
}

// scaled shrinks a sample count on a race-instrumented build.
//
// No bound is asserted under -race, so the full population buys nothing there
// and costs a minute of everyone's `go test -race ./...`. The arms still run,
// which is what -race is for: the harness's own concurrency — the transport
// wrapper, the response-writer wrapper and the sharded collector — is exercised
// on every one of them.
func scaled(n int) int {
	if raceEnabled {
		return max(n/8, 64)
	}
	return n
}

func overheadOf(s []Sample) Dist {
	return Describe("overhead", s, func(s Sample) time.Duration { return s.Overhead })
}

// TestGatewayOverheadWarmLocal is §15.1's headline number, measured.
func TestGatewayOverheadWarmLocal(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: the §15.1 gate is a measurement, not a unit test")
	}
	g := NewGateway(t, Opts{
		// A fixed, known upstream delay. It is the instrument the separation
		// needs: if the reported overhead moved when this moved, the harness
		// would be measuring the upstream and saying so.
		UpstreamLatency: 2 * time.Millisecond,
		Prefix:          true,
		Metering:        true,
	})

	load := Load{Concurrency: 4, Requests: scaled(2000), Warmup: scaled(500), BodySize: 4 << 10}

	first := overheadOf(Run(t, g, load))
	second := overheadOf(Run(t, g, load))

	t.Log("")
	t.Log("=== DESIGN §15.1 warm-local gateway overhead =======================")
	t.Logf("  boundary: handler entry to handler return, minus every nanosecond")
	t.Logf("            spent waiting for the upstream (see the package comment)")
	t.Logf("  %s", first)
	t.Logf("  %s", second)
	t.Logf("  published: p50 %v, p99 %v", warmLocalP50, warmLocalP99)
	t.Log("===================================================================")

	if first.NonWarm > 0 || second.NonWarm > 0 {
		t.Errorf("%d + %d samples dialled a new upstream connection: not a warm profile",
			first.NonWarm, second.NonWarm)
	}
	if first.MultipleAttempt > 0 || second.MultipleAttempt > 0 {
		t.Errorf("%d + %d samples made more than one upstream round trip: "+
			"a retry or a fallback happened and these are not warm-local samples",
			first.MultipleAttempt, second.MultipleAttempt)
	}

	if !gated(t) {
		return
	}
	if spread := disagreement(first.P50, second.P50); spread > measurable {
		t.Skipf("this machine cannot hold a %v budget: the same measurement came back "+
			"%v then %v, a spread of %.0f%% (over %.0f%%). Run it alone, or with -short "+
			"to skip it by name.", p50Bound, first.P50, second.P50, 100*spread, 100*measurable)
	}

	if first.P50 > p50Bound {
		t.Errorf("warm-local p50 is %v, over the %v gate (§15.1 publishes %v)",
			first.P50, p50Bound, warmLocalP50)
	}
	// The p99 is the claim §15.1 makes that holds with room to spare, and it is
	// the one an operator notices: a p50 average is a story about the machine,
	// a p99 is a story about a client that timed out.
	if first.P99 > warmLocalP99 {
		t.Errorf("warm-local p99 is %v, over §15.1's published %v", first.P99, warmLocalP99)
	}
}

// TestTheInstrumentSeparatesTheUpstream is the inverse test for this whole
// package: a harness that reported total latency as gateway overhead would
// produce a beautiful table of numbers that meant nothing.
//
// The property is that an upstream delay lands on the UPSTREAM side. A known
// delay is injected into the fake and the two halves are read separately: the
// upstream half must absorb essentially all of it, and the overhead half must
// stay where it was.
//
// It is a ratio test and not an equality test, because the two arms cannot be
// compared at the same request RATE — a gateway answering 1500 req/s has warmer
// caches and a higher clock than one answering 90, and the overhead half really
// does differ between them. What must not happen, and what this catches, is the
// injected ten milliseconds appearing in the overhead column.
func TestTheInstrumentSeparatesTheUpstream(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this is a measurement, not a unit test")
	}
	const injected = 10 * time.Millisecond
	measure := func(lat time.Duration) (over, up Dist) {
		g := NewGateway(t, Opts{UpstreamLatency: lat, Prefix: true, Metering: true})
		s := Run(t, g, Load{Concurrency: 8, Requests: scaled(800), Warmup: scaled(300),
			BodySize: 4 << 10})
		return overheadOf(s),
			Describe("upstream", s, func(s Sample) time.Duration { return s.Upstream })
	}
	fastOver, fastUp := measure(0)
	slowOver, slowUp := measure(injected)

	t.Log("")
	t.Log("=== the instrument, checked against itself =========================")
	t.Logf("  upstream delay 0:     %s", fastOver)
	t.Logf("                        %s", fastUp)
	t.Logf("  upstream delay %v:  %s", injected, slowOver)
	t.Logf("                        %s", slowUp)
	t.Log("===================================================================")

	if !gated(t) {
		return
	}
	if fastUp.P50 > injected/2 {
		t.Errorf("with no injected delay the upstream half already reads %v; "+
			"the split is measuring something other than the upstream", fastUp.P50)
	}
	if slowUp.P50 < injected {
		t.Errorf("a %v upstream delay showed up as only %v of upstream time",
			injected, slowUp.P50)
	}
	// The whole point: the delay did not land in the overhead column. Half of
	// the injected delay is a very loose bound and it is meant to be — the arms
	// run at different request rates and the overhead genuinely differs — but
	// an instrument that leaked the delay would miss it by ten times this.
	if grew := slowOver.P50 - fastOver.P50; grew > injected/2 {
		t.Errorf("gateway overhead grew by %v when %v was injected into the UPSTREAM; "+
			"the separation leaks", grew, injected)
	}
}

// TestGatewayOverheadByBodySize is the shape behind the headline.
//
// It is a separate test because the single number is misleading on its own:
// overhead is a fixed cost plus a per-byte one, and the per-byte term is four
// JSON passes over the body (client decode, upstream encode, upstream decode,
// client encode). An operator sizing a deployment needs the slope, not the
// intercept.
//
// The slope is what moved when the duplicate parses inside those passes came
// out: 186/251/469/1179 µs before, 158/198/355/1037 after; and again when the
// encode stopped compacting every nested type into its parent, 145/163/246/508.
// The intercept barely moved either time, because the intercept was never the
// codec.
func TestGatewayOverheadByBodySize(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this is a measurement, not a unit test")
	}
	type row struct {
		size int
		d    Dist
	}
	var rows []row
	for _, size := range []int{256, 1 << 10, 4 << 10, 16 << 10} {
		g := NewGateway(t, Opts{
			UpstreamLatency: 2 * time.Millisecond, Prefix: true, Metering: true,
		})
		s := Run(t, g, Load{Concurrency: 4, Requests: scaled(1000), Warmup: scaled(500),
			BodySize: size})
		rows = append(rows, row{size, Describe(fmt.Sprintf("%6d B", size), s,
			func(s Sample) time.Duration { return s.Overhead })})
	}

	t.Log("")
	t.Log("=== gateway overhead by request body size =========================")
	for _, r := range rows {
		t.Logf("  %s", r.d)
	}
	t.Log("===================================================================")

	if !gated(t) {
		return
	}
	// The claim being pinned is the SHAPE: cost grows with the body, so a
	// profile that names a body size is the only kind of profile that means
	// anything. If this ever inverts, either the codec stopped reading the body
	// or the harness stopped sending one.
	if rows[0].d.P50 >= rows[len(rows)-1].d.P50 {
		t.Errorf("overhead did not grow with body size: %v at %d B, %v at %d B",
			rows[0].d.P50, rows[0].size, rows[len(rows)-1].d.P50, rows[len(rows)-1].size)
	}
}

// TestGatewayOverheadLargeBodies measures §15.1's `prefix-1MiB` and
// `prefix-16MiB` rows, published as p99 6 ms and 60 ms.
//
// Neither holds — 34 ms and 512 ms measured — and the row is misnamed as well as
// wrong: the prefix chain is not the cost. `internal/prefix`'s own benchmark
// puts a 16 MiB chain at 7.1 ms and 728 B — about 1% of the measured figure —
// while the four JSON passes over the body are the other 99%. A profile named
// after the cheap component invites exactly the wrong optimization.
//
// The 16 MiB arm is opt-in because it takes the better part of a minute. Set
// DORANG_PERF_16MIB=1.
func TestGatewayOverheadLargeBodies(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this is a measurement, not a unit test")
	}
	arms := []struct {
		name     string
		size     int
		requests int
		// published is §15.1's p99 for this profile, or zero where it states
		// none.
		published time.Duration
	}{
		{"256 KiB", 256 << 10, scaled(120), 0},
		{"1 MiB", 1 << 20, scaled(60), 6 * time.Millisecond},
	}
	if os.Getenv("DORANG_PERF_16MIB") != "" {
		arms = append(arms, struct {
			name      string
			size      int
			requests  int
			published time.Duration
		}{"16 MiB", 16 << 20, scaled(20), 60 * time.Millisecond})
	}

	t.Log("")
	t.Log("=== DESIGN §15.1 large-body profiles ==============================")
	for _, a := range arms {
		g := NewGateway(t, Opts{
			UpstreamLatency: 2 * time.Millisecond, Prefix: true, Metering: true,
		})
		s := Run(t, g, Load{Concurrency: 2, Requests: a.requests, Warmup: 8,
			BodySize: a.size, Conversations: 2})
		d := Describe(a.name, s, func(s Sample) time.Duration { return s.Overhead })
		if a.published > 0 {
			t.Logf("  %s   published p99 %v", d, a.published)
		} else {
			t.Logf("  %s", d)
		}
		if !raceEnabled && a.published > 0 && d.P99 <= a.published {
			t.Errorf("%s came in at p99 %v, under the %v this document records as NOT "+
				"holding. Either the codec got an order of magnitude faster — in which "+
				"case §15.1 should be corrected upward again — or this arm stopped "+
				"measuring the body.", a.name, d.P99, a.published)
		}
	}
	t.Logf("  the prefix chain is ~1%% of these figures (internal/prefix's own")
	t.Logf("  benchmark: 7.1 ms and 728 B for 16 MiB); the rest is the codec.")
	t.Log("===================================================================")
}

// TestGatewayAddedTTFTStreaming is §15.1's "Added TTFT, streaming: p99 < 1 ms".
//
// The quantity is how much later the client's first byte is than it would have
// been if the gateway were a wire, which is not a difference of two arms: it is
// per request, (client TTFT) minus (upstream TTFT), where the upstream's own
// TTFT is measured from the connection being in hand to the first byte of
// upstream content arriving. Everything in between is the gateway — the decode,
// the routing, the capacity acquisition, the upstream encode and the relay of
// the first frame.
func TestGatewayAddedTTFTStreaming(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this is a measurement, not a unit test")
	}
	g := NewGateway(t, Opts{
		UpstreamLatency: 2 * time.Millisecond,
		// Frames that arrive over time, which is what a relay relays. One frame
		// is not a stream: it never exercises the partial-frame carry the
		// §15.2.3 scanner exists for.
		InterFrame: 50 * time.Microsecond,
		Frames:     24,
		Prefix:     true, Metering: true,
	})
	load := Load{Concurrency: 4, Requests: scaled(1000), Warmup: scaled(300),
		BodySize: 4 << 10, Stream: true}

	run := func() (Dist, Dist, int) {
		s := Run(t, g, load)
		valid := s[:0]
		for _, x := range s {
			if x.TTFTValid {
				valid = append(valid, x)
			}
		}
		return Describe("added TTFT", valid, func(s Sample) time.Duration { return s.AddedTTFT }),
			Describe("overhead", valid, func(s Sample) time.Duration { return s.Overhead }),
			len(s) - len(valid)
	}
	ttft, over, missing := run()
	ttft2, _, _ := run()

	t.Log("")
	t.Log("=== DESIGN §15.1 streaming ========================================")
	t.Logf("  %s", ttft)
	t.Logf("  %s", ttft2)
	t.Logf("  %s   (whole-request, relay time excluded from the upstream side)", over)
	t.Logf("  published: added TTFT p99 < 1 ms")
	t.Log("===================================================================")

	if missing > 0 {
		t.Errorf("%d streamed samples produced no first-byte timing at all", missing)
	}
	if ttft.N == 0 {
		t.Fatal("no streamed samples")
	}
	if !gated(t) {
		return
	}
	// The disagreement check has to be on the statistic the gate ASSERTS, and
	// it was not: it compared the two p50s and then failed the build on a p99.
	// Those are different quantities with different noise — a p50 built from a
	// thousand samples is stable on a loaded machine and the p99 is built from
	// ten, so this gate would fail honest builds whenever the host had a
	// neighbour, and did. Measured back to back on a busy machine it read
	// 2.59 ms then 798 µs from the same binary against the same fake, which is
	// not a measurement of anything.
	if spread := disagreement(ttft.P99, ttft2.P99); spread > measurable {
		t.Skipf("this machine cannot hold a sub-millisecond budget: the p99 came "+
			"back %v then %v, a spread of %.0f%% (over %.0f%%). Run it alone, or "+
			"with -short to skip it by name.",
			ttft.P99, ttft2.P99, 100*spread, 100*measurable)
	}
	if ttft.P99 > time.Millisecond {
		t.Errorf("added TTFT p99 is %v, over §15.1's published 1 ms", ttft.P99)
	}
}
