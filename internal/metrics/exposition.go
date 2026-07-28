package metrics

import (
	"strconv"
)

// Type is a metric family's Prometheus type.
type Type uint8

// The three types dorang emits. There is deliberately no summary: a quantile
// computed per process cannot be aggregated across a fleet, and DESIGN §13 has
// dorang running as more than one process.
const (
	Counter Type = iota
	Gauge
	Histogram
)

// String returns the name used on the TYPE line.
func (t Type) String() string {
	switch t {
	case Counter:
		return "counter"
	case Gauge:
		return "gauge"
	case Histogram:
		return "histogram"
	}
	return "untyped"
}

// Writer renders the Prometheus text exposition format, version 0.0.4.
//
// Hand-rolled, and that is a decision rather than an omission: the format is a
// few hundred bytes of specification that has not changed since 2014, while
// prometheus/client_golang pulls in protobuf, a compression library and a
// reflection-heavy registry. DESIGN §0.2 requires the notebook profile to have
// no required dependencies, and the same reasoning that chose a cgo-free SQLite
// driver applies here.
//
// A Writer is not safe for concurrent use. [Registry] owns exactly one and
// serializes access to it.
type Writer struct {
	b    []byte
	lbl  []byte
	hbuf []uint64

	// The family header is deferred until the first sample, so that a
	// collector which decides it has nothing to report emits nothing at all —
	// rule 3 of the package comment. A HELP/TYPE pair with no series underneath
	// is how "unconfigured" gets mistaken for "zero" by a reader who assumes
	// the absence of a sample means the absence of load.
	pending  bool
	pendName string
	pendHelp string
	pendType Type

	open     string
	openType Type

	seen    map[string]Type
	dups    int
	samples int
}

// Reset prepares the writer to render into dst, keeping the label scratch.
func (w *Writer) Reset(dst []byte) {
	w.b = dst
	w.lbl = w.lbl[:0]
	w.pending = false
	w.open, w.openType = "", Counter
	if w.seen == nil {
		w.seen = make(map[string]Type, 128)
	} else {
		clear(w.seen)
	}
	w.dups = 0
	w.samples = 0
}

// Bytes returns what has been rendered so far.
func (w *Writer) Bytes() []byte { return w.b }

// Len is the current length, used to roll a panicking collector back.
func (w *Writer) Len() int { return len(w.b) }

// truncate rolls the buffer back to n and drops any half-built sample.
func (w *Writer) truncate(n int) {
	if n <= len(w.b) {
		w.b = w.b[:n]
	}
	w.lbl = w.lbl[:0]
	w.pending = false
}

// Duplicates is the number of times a family name was opened twice in one
// render. It is not merely untidy: a second `# TYPE` line for the same name
// makes Prometheus reject the whole scrape, so it is counted and exported.
func (w *Writer) Duplicates() int { return w.dups }

// Samples is the number of series rendered.
func (w *Writer) Samples() int { return w.samples }

// Metric opens a family. Nothing is written until the first sample.
func (w *Writer) Metric(name string, t Type, help string) {
	w.lbl = w.lbl[:0]
	w.pending = true
	w.pendName, w.pendType, w.pendHelp = name, t, help
	w.open, w.openType = name, t
}

// header writes the deferred HELP and TYPE lines.
func (w *Writer) header() {
	if !w.pending {
		return
	}
	w.pending = false
	if _, ok := w.seen[w.pendName]; ok {
		w.dups++
	}
	w.seen[w.pendName] = w.pendType

	w.b = append(w.b, "# HELP "...)
	w.b = append(w.b, w.pendName...)
	w.b = append(w.b, ' ')
	w.b = appendEscapedHelp(w.b, w.pendHelp)
	w.b = append(w.b, "\n# TYPE "...)
	w.b = append(w.b, w.pendName...)
	w.b = append(w.b, ' ')
	w.b = append(w.b, w.pendType.String()...)
	w.b = append(w.b, '\n')
}

// Label adds one label to the next sample. Labels accumulate until a sample is
// written, then reset.
func (w *Writer) Label(name, value string) {
	if len(w.lbl) > 0 {
		w.lbl = append(w.lbl, ',')
	}
	w.lbl = append(w.lbl, name...)
	w.lbl = append(w.lbl, '=', '"')
	w.lbl = appendEscapedLabel(w.lbl, value)
	w.lbl = append(w.lbl, '"')
}

// sampleName writes `name{labels}` for the open family, with an optional
// suffix (used by the histogram's _bucket/_sum/_count) and an optional extra
// label appended after the accumulated ones (used for `le`).
func (w *Writer) sampleName(suffix string, extra []byte) {
	w.header()
	w.b = append(w.b, w.open...)
	w.b = append(w.b, suffix...)
	if len(w.lbl) > 0 || len(extra) > 0 {
		w.b = append(w.b, '{')
		w.b = append(w.b, w.lbl...)
		if len(extra) > 0 {
			if len(w.lbl) > 0 {
				w.b = append(w.b, ',')
			}
			w.b = append(w.b, extra...)
		}
		w.b = append(w.b, '}')
	}
	w.b = append(w.b, ' ')
	w.samples++
}

// Uint writes an unsigned sample and clears the accumulated labels.
func (w *Writer) Uint(v uint64) {
	w.sampleName("", nil)
	w.b = strconv.AppendUint(w.b, v, 10)
	w.b = append(w.b, '\n')
	w.lbl = w.lbl[:0]
}

// Int writes a signed sample.
func (w *Writer) Int(v int64) {
	w.sampleName("", nil)
	w.b = strconv.AppendInt(w.b, v, 10)
	w.b = append(w.b, '\n')
	w.lbl = w.lbl[:0]
}

// Float writes a floating-point sample.
func (w *Writer) Float(v float64) {
	w.sampleName("", nil)
	w.b = appendFloat(w.b, v)
	w.b = append(w.b, '\n')
	w.lbl = w.lbl[:0]
}

// Bool writes 1 or 0. A state is a gauge with two values, not a missing metric:
// "the circuit is closed" is a fact dorang knows, unlike "this deployment's
// latency", which it may not.
func (w *Writer) Bool(v bool) {
	if v {
		w.Int(1)
		return
	}
	w.Int(0)
}

// HistogramSample writes the cumulative buckets, sum and count of h under the
// accumulated labels.
func (w *Writer) HistogramSample(h *Hist) {
	lbl := w.lbl
	var scratch [40]byte

	counts, sum, total := h.snapshot(w.hbuf)
	w.hbuf = counts[:0]
	var cum uint64
	for i, bound := range h.bounds {
		cum += counts[i]
		w.lbl = lbl
		le := append(scratch[:0], "le=\""...)
		le = appendFloat(le, bound)
		le = append(le, '"')
		w.sampleName("_bucket", le)
		w.b = strconv.AppendUint(w.b, cum, 10)
		w.b = append(w.b, '\n')
	}
	cum += counts[len(h.bounds)]
	w.lbl = lbl
	w.sampleName("_bucket", append(scratch[:0], `le="+Inf"`...))
	w.b = strconv.AppendUint(w.b, cum, 10)
	w.b = append(w.b, '\n')

	w.lbl = lbl
	w.sampleName("_sum", nil)
	w.b = appendFloat(w.b, sum)
	w.b = append(w.b, '\n')

	w.lbl = lbl
	w.sampleName("_count", nil)
	w.b = strconv.AppendUint(w.b, total, 10)
	w.b = append(w.b, '\n')

	w.lbl = lbl[:0]
}

// appendFloat renders a value the way Prometheus's own parser reads it back.
func appendFloat(dst []byte, v float64) []byte {
	switch {
	case v != v:
		return append(dst, "NaN"...)
	case v > maxFloat:
		return append(dst, "+Inf"...)
	case v < -maxFloat:
		return append(dst, "-Inf"...)
	}
	return strconv.AppendFloat(dst, v, 'g', -1, 64)
}

const maxFloat = 1.7976931348623157e308

// appendEscapedHelp escapes a HELP string: backslash and newline only.
func appendEscapedHelp(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

// appendEscapedLabel escapes a label value: backslash, double quote, newline.
//
// The values here are ids from configuration, not caller-supplied strings, but
// "the values are probably safe" is how an injection gets shipped. A model name
// is opaque and may contain anything (DESIGN §2.1).
func appendEscapedLabel(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			dst = append(dst, '\\', '\\')
		case '"':
			dst = append(dst, '\\', '"')
		case '\n':
			dst = append(dst, '\\', 'n')
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

// statusStrings avoids a strconv.Itoa allocation per observation for the status
// label. DESIGN §15.5 prohibits formatted string construction on the hot path,
// and while the observation point is past the client's last byte, an allocation
// per request is an allocation per request.
var statusStrings = func() [600]string {
	var out [600]string
	for i := range out {
		out[i] = strconv.Itoa(i)
	}
	return out
}()

func statusLabel(code int) string {
	if code >= 0 && code < len(statusStrings) {
		return statusStrings[code]
	}
	return "unknown"
}
