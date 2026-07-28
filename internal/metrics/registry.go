package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ContentType is the exposition format's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Collector is one subsystem's contribution to the scrape.
//
// The contract is short and every clause is load-bearing:
//
//   - Collect must not block. It reads live state, and the state it reads is
//     being written by the request path. Taking a lock that path takes
//     exclusively turns a scrape into a stall, and a scrape happens every
//     fifteen seconds forever.
//   - Collect must not perform I/O. A store query here makes /metrics fail
//     exactly when the store is the thing that has failed.
//   - Collect must omit what it cannot compute. Not zero — omit. See rule 3 of
//     the package comment.
//   - Collect may panic. The registry recovers, rolls the buffer back to the
//     byte before the collector started, and counts it. One broken adapter must
//     not cost the operator every other number on the page.
type Collector interface {
	// CollectorName identifies this collector in
	// `dorang_metrics_collector_panics_total`. Fixed cardinality.
	CollectorName() string
	// Collect appends this collector's families.
	Collect(w *Writer)
}

// CollectorFunc adapts a function to [Collector].
type CollectorFunc struct {
	Name string
	Fn   func(w *Writer)
}

// CollectorName implements [Collector].
func (c CollectorFunc) CollectorName() string { return c.Name }

// Collect implements [Collector].
func (c CollectorFunc) Collect(w *Writer) { c.Fn(w) }

// Registry renders every registered collector into one scrape.
//
// It satisfies internal/server's metrics-source interface, so wiring it is one
// field in server.Options and the HTTP surface keeps serving /metrics; and it
// satisfies http.Handler, so it can be mounted on an operator-only listener
// instead when the scrape must not be public.
type Registry struct {
	cs atomic.Pointer[[]Collector]

	// mu serializes rendering so that one buffer and one writer are reused
	// across scrapes. It is the registry's own mutex and nothing on the request
	// path can reach it, which is the property that keeps a slow scraper from
	// becoming a slow gateway.
	mu  sync.Mutex
	buf []byte
	w   Writer
	// folders and panicScratch are render scratch under mu. They are cached
	// rather than rebuilt because the registry's own bookkeeping should not be
	// the largest allocator in a scrape it exists to keep cheap.
	folders      atomic.Pointer[[]*folder]
	panicScratch []panicCount

	panics sync.Map // string -> *atomic.Uint64

	scrapes  atomic.Uint64
	lastNS   atomic.Int64
	lastSize atomic.Int64
	series   atomic.Int64
	dups     atomic.Uint64

	now func() time.Time
}

// New builds an empty registry. now may be nil.
func New(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	r := &Registry{now: now}
	empty := []Collector{}
	r.cs.Store(&empty)
	return r
}

// Register adds a collector. Order is preserved, and it is the order of the
// output: a scrape that reorders itself between requests is a diff nobody can
// read.
//
// Registration copies the slice and swaps a pointer, so it never blocks a
// scrape in progress and a scrape never blocks a reload.
func (r *Registry) Register(cs ...Collector) {
	for {
		old := r.cs.Load()
		next := make([]Collector, 0, len(*old)+len(cs))
		next = append(next, *old...)
		for _, c := range cs {
			if c != nil {
				next = append(next, c)
			}
		}
		if r.cs.CompareAndSwap(old, &next) {
			f := foldersOf(next)
			r.folders.Store(&f)
			return
		}
	}
}

// Collectors returns the registered collectors, for tests.
func (r *Registry) Collectors() []Collector { return *r.cs.Load() }

// Gather renders the whole scrape, appending to dst.
//
// Passing a nil dst gets the registry's own reused buffer, which is only valid
// until the next Gather; that is the allocation-free path and the one
// [Registry.Metrics] uses.
func (r *Registry) Gather(dst []byte) []byte {
	start := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	own := dst == nil
	if own {
		dst = r.buf[:0]
	}
	r.w.Reset(dst)

	for _, c := range *r.cs.Load() {
		r.collectOne(c)
	}
	r.selfMetrics(start)

	out := r.w.Bytes()
	if own {
		r.buf = out
	}
	r.scrapes.Add(1)
	r.series.Store(int64(r.w.Samples()))
	r.dups.Add(uint64(r.w.Duplicates()))
	r.lastSize.Store(int64(len(out)))
	r.lastNS.Store(int64(r.now().Sub(start)))
	return out
}

// collectOne runs one collector with a recover around it.
func (r *Registry) collectOne(c Collector) {
	mark := r.w.Len()
	defer func() {
		if v := recover(); v != nil {
			r.w.truncate(mark)
			r.panicCounter(c.CollectorName()).Add(1)
		}
	}()
	c.Collect(&r.w)
}

func (r *Registry) panicCounter(name string) *atomic.Uint64 {
	if v, ok := r.panics.Load(name); ok {
		return v.(*atomic.Uint64)
	}
	v, _ := r.panics.LoadOrStore(name, new(atomic.Uint64))
	return v.(*atomic.Uint64)
}

// selfMetrics reports on the collection itself.
//
// A metrics endpoint that does not measure itself is the one blind spot in the
// system it exists to make visible: an operator whose scrape has quietly grown
// to forty thousand series and two hundred milliseconds finds out from
// Prometheus's own logs, or from a timeout.
func (r *Registry) selfMetrics(start time.Time) {
	w := &r.w
	took := r.now().Sub(start)

	w.Metric("dorang_metrics_scrapes_total", Counter,
		"Scrapes rendered.")
	w.Uint(r.scrapes.Load() + 1)

	w.Metric("dorang_metrics_collection_duration_seconds", Gauge,
		"Wall time the last collection took, excluding the self-metrics themselves.")
	w.Float(took.Seconds())

	w.Metric("dorang_metrics_series", Gauge,
		"Series in this scrape, counted as it was rendered.")
	w.Int(int64(w.Samples()))

	w.Metric("dorang_metrics_duplicate_families_total", Counter,
		"Times two collectors opened the same family name. A non-zero value means "+
			"Prometheus is rejecting this scrape, because a second TYPE line for one "+
			"name is a parse error.")
	w.Uint(r.dups.Load() + uint64(w.Duplicates()))

	w.Metric("dorang_metrics_collector_panics_total", Counter,
		"Panics recovered inside a collector. The collector's families are missing "+
			"from the scrape; everything else is intact.")
	seen := r.panicScratch[:0]
	r.panics.Range(func(k, v any) bool {
		seen = append(seen, panicCount{k.(string), v.(*atomic.Uint64).Load()})
		return true
	})
	r.panicScratch = seen
	sortSlice(seen, func(a, b panicCount) bool { return a.name < b.name })
	for _, p := range seen {
		w.Label("collector", p.name)
		w.Uint(p.v)
	}

	w.Metric("dorang_metrics_cardinality_folds_total", Counter,
		"Series folded into the "+OverflowSentinel+" label because a family reached its "+
			"cardinality cap. Non-zero means resolution was lost — and that it was lost "+
			"here, deliberately, rather than by the process running out of memory.")
	if fs := r.folders.Load(); fs != nil {
		for _, f := range *fs {
			w.Label("family", f.family)
			w.Uint(f.Folds())
		}
	}
}

// panicCount is one collector's recovered-panic tally, held in render scratch.
type panicCount struct {
	name string
	v    uint64
}

// folderSource is implemented by a collector that owns capped families.
type folderSource interface {
	folders() []*folder
}

func foldersOf(cs []Collector) []*folder {
	var out []*folder
	for _, c := range cs {
		if fs, ok := c.(folderSource); ok {
			out = append(out, fs.folders()...)
		}
	}
	sortSlice(out, func(a, b *folder) bool { return a.family < b.family })
	return out
}

// Metrics appends the scrape to dst. It is internal/server's metrics-source
// interface, kept structural so that internal/server imports nothing.
//
// It appends into the caller's buffer rather than handing back the registry's,
// because the registry's is reused by the next scrape and two scrapers are the
// normal case, not an edge one.
func (r *Registry) Metrics(dst []byte) []byte {
	if dst == nil {
		dst = make([]byte, 0, r.lastSize.Load()+512)
	}
	return r.Gather(dst)
}

// ServeHTTP renders the scrape. HEAD is answered with the length and no body,
// which is what a load balancer's health check asks for.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body := r.Metrics(nil)
	h := w.Header()
	h.Set("Content-Type", ContentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if req.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// LastCollection reports how long the last scrape took and how many bytes it
// produced, for tests and for a health body.
func (r *Registry) LastCollection() (time.Duration, int64) {
	return time.Duration(r.lastNS.Load()), r.lastSize.Load()
}
