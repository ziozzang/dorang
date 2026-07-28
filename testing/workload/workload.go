// Package workload is the fixed benchmark workload of docs/DESIGN.md §15.3.
//
// "20k req/s" means nothing without saying what a request is, so the workload is
// pinned here rather than chosen per run:
//
//   - bodies of 1 KiB, 32 KiB and 400 KiB
//   - api keys drawn from a SKEWED distribution, not a uniform one
//   - 100 pricing rules loaded
//   - prefix routing measured both on and off
//   - metering persisting to a REAL store, not a nop sink
//
// Two numbers are reported separately and both must hold:
//
//   - Served throughput — what the request path sustains.
//   - Durable throughput — what the storage layer absorbs without growing a
//     backlog.
//
// They are not the same number and reporting only the first is how a gateway
// ships with a metering pipeline that silently falls behind: the request path
// stays fast precisely because the writes it enqueues are someone else's
// problem. §12.1 makes the backlog visible; this measures it.
package workload

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"
)

// The three body sizes of §15.3.
const (
	Size1KiB   = 1 << 10
	Size32KiB  = 32 << 10
	Size400KiB = 400 << 10
)

// Sizes is the fixed rotation. It is a rotation rather than a random draw so two
// runs offer the same bytes and the numbers are comparable.
var Sizes = [...]int{Size1KiB, Size1KiB, Size1KiB, Size32KiB, Size32KiB, Size400KiB}

// Model is the client-facing name every request asks for. It is opaque and is
// compared whole (DESIGN §2.1).
const Model = "chat-large"

// Keys is the principal population. Real traffic is dominated by a handful of
// keys, so the distribution below is skewed rather than uniform: a uniform draw
// makes every cache, every accumulator shard and every rollup row behave better
// than they will in production.
const Keys = 512

// Duration returns how long each arm runs. §15.3 specifies ten minutes of steady
// state, which is a soak rather than something `go test` should do on every run,
// so the default is short and the full figure is opt-in:
//
//	DORANG_WORKLOAD_SECONDS=600 go test -run TestWorkload ./testing/workload/
func Duration() time.Duration {
	if v := os.Getenv("DORANG_WORKLOAD_SECONDS"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return time.Second
}

// FullRun reports whether the run is long enough to be quoted as the §15.3
// figure rather than as a smoke test.
func FullRun() bool { return Duration() >= 10*time.Minute }

// KeyPicker draws api key ids from a skewed distribution.
//
// The skew is Zipfian, which is what key populations actually look like: a few
// keys carry most of the traffic. It matters for more than realism — the
// metering accumulator is bounded per shard, the auth snapshot is a hash lookup,
// and the rollup rows are per (key, hour). A uniform draw exercises none of the
// hot-key behaviour any of them were designed for.
type KeyPicker struct {
	zipf *rand.Zipf
	ids  []string
}

// NewKeyPicker builds a picker over n keys, seeded deterministically so two runs
// draw the same sequence.
func NewKeyPicker(n int, seed uint64) *KeyPicker {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	ids := make([]string, n)
	for i := range ids {
		ids[i] = "key-" + strconv.Itoa(i)
	}
	// s > 1 and v = 1 give the classic long tail; imax is the last index.
	return &KeyPicker{zipf: rand.NewZipf(r, 1.2, 1, uint64(n-1)), ids: ids}
}

// Next returns the next key id.
func (p *KeyPicker) Next() string { return p.ids[p.zipf.Uint64()] }

// Distribution reports how many draws landed on each of the top n keys, for the
// benchmark to print. A flat distribution here means the skew was lost.
func Distribution(picker *KeyPicker, draws, top int) []int {
	counts := make(map[string]int, 64)
	for range draws {
		counts[picker.Next()]++
	}
	out := make([]int, 0, top)
	for i := range top {
		out = append(out, counts["key-"+strconv.Itoa(i)])
	}
	return out
}

// Body renders a chat-completions request body of approximately size bytes.
//
// The filler is placed inside the LAST message so that requests sharing a
// conversation share a prefix: the prefix chain hashes the raw request bytes in
// order, so padding at the front would make every request share everything and
// padding nowhere would make the prefix arm measure nothing.
func Body(conversation int, size int) []byte {
	head := `{"model":"` + Model + `","messages":[` +
		`{"role":"system","content":"You are a careful assistant. Conversation ` +
		strconv.Itoa(conversation) + `."},` +
		`{"role":"user","content":"`
	tail := `"}]}`
	fill := size - len(head) - len(tail)
	if fill < 1 {
		fill = 1
	}
	var b strings.Builder
	b.Grow(size + 16)
	b.WriteString(head)
	// A repeating but non-degenerate filler: a single repeated byte would let a
	// hash chain that ignored length still look correct.
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789 "
	for i := range fill {
		b.WriteByte(alphabet[i%len(alphabet)])
	}
	b.WriteString(tail)
	return []byte(b.String())
}

// PricingCatalog renders a catalog with n marginal rules plus a subscription and
// an adjustment, so the index has something to discriminate on.
//
// §8.2: rules are indexed by their static dimensions at load, so a request
// evaluates only the handful that can possibly apply to it. A catalog with one
// rule would make that index free and the measurement meaningless.
func PricingCatalog(n int) string {
	var b strings.Builder
	b.WriteString("currency: USD\nrules:\n")
	for i := range n {
		// Most rules match a deployment this workload never touches. That is the
		// point: the cost of pricing must not grow with the size of the catalog.
		dep := "dep-" + strconv.Itoa(i)
		if i == 0 {
			dep = "d1"
		}
		fmt.Fprintf(&b,
			"  - { id: r%d, match: { deployment: %s }, unit: per_1m_tokens, input: \"%d.%02d\", output: \"%d.%02d\" }\n",
			i, dep, i%7, i%100, (i%5)+1, (i*7)%100)
	}
	b.WriteString("  - id: plan\n    class: fixed_subscription\n" +
		"    match: { deployment: d1 }\n    amount_per_period: \"200.00\"\n    period: monthly\n")
	b.WriteString("  - id: margin\n    class: adjustment\n    match: { deployment: d1 }\n" +
		"    op: percent\n    amount: \"5\"\n    applies_to: marginal\n")
	return b.String()
}

// Result is one arm's measurement.
type Result struct {
	Name string
	// Unit names what Requests counts: "req" for the served arms, "rows" for
	// the durable one. They are deliberately not the same unit — a rollup row
	// covers many requests — and printing both as "req/s" would invite exactly
	// the comparison §15.3 splits the two numbers to prevent.
	Unit     string
	Requests int64
	Bytes    int64
	Elapsed  time.Duration
	Errors   int64
}

// PerSecond is the served throughput.
func (r Result) PerSecond() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Requests) / r.Elapsed.Seconds()
}

// MiBPerSecond is the request bytes the path moved.
func (r Result) MiBPerSecond() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Bytes) / (1 << 20) / r.Elapsed.Seconds()
}

func (r Result) String() string {
	unit := r.Unit
	if unit == "" {
		unit = "req"
	}
	out := fmt.Sprintf("%-26s %10.0f %s/s", r.Name, r.PerSecond(), unit)
	if r.Bytes > 0 {
		out += fmt.Sprintf("  %8.1f MiB/s", r.MiBPerSecond())
	} else {
		out += "               "
	}
	return out + fmt.Sprintf("  (%d %s in %v, %d errors)",
		r.Requests, unit, r.Elapsed.Round(time.Millisecond), r.Errors)
}

// Corpus is the pre-rendered request bodies an arm draws from.
//
// They are built once, before the clock starts: rendering a 400 KiB body costs
// more than serving it, and a benchmark that builds its own input inside the
// timed loop is measuring strings.Builder.
type Corpus struct {
	bodies [][]byte
}

// NewCorpus renders conversations × the size rotation.
func NewCorpus(conversations int) *Corpus {
	c := &Corpus{}
	for conv := range conversations {
		for _, size := range Sizes {
			c.bodies = append(c.bodies, Body(conv, size))
		}
	}
	return c
}

// At returns the i-th body, cycling.
func (c *Corpus) At(i int) []byte { return c.bodies[i%len(c.bodies)] }

// Len is how many distinct bodies the corpus holds.
func (c *Corpus) Len() int { return len(c.bodies) }
