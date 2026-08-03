package openai

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/ziozzang/dorang/internal/canonical"
)

// DefaultMaxFrameBytes bounds how much of an unterminated frame the scanner
// will hold before giving up on rewriting it. An upstream that never sends a
// newline must not be able to grow dorang's heap without limit.
const DefaultMaxFrameBytes = 4 << 20

// ScannerOptions configures a [Scanner].
type ScannerOptions struct {
	// From is the model name dorang SENT upstream. It decides whether the
	// rewrite can be skipped entirely — the rewrite itself is unconditional,
	// because COMPATIBILITY 2.5 requires the client-facing name on every chunk
	// regardless of what the backend put there — and it is the name the
	// upstream's own answer is compared against; see [Scanner.ModelAgreement].
	From string
	// To is the client-facing name written into every frame's model field.
	// Empty disables rewriting.
	To string
	// CollectUsage reads the terminal usage frame.
	CollectUsage bool
	// MaxFrameBytes overrides [DefaultMaxFrameBytes].
	MaxFrameBytes int
}

// Scanner relays an upstream SSE stream to a client, rewriting the model field.
//
// # Why a scanner and not a decoder
//
// DESIGN §7.2 requires the response body to carry the name the client asked
// for, and COMPATIBILITY 2.5 requires it on every chunk. Decoding and
// re-encoding each frame would put a JSON parse and several allocations on the
// per-token path. This scanner is single-pass and line-oriented, never decodes
// a frame, carries a partial frame across reads, and touches only the model
// value — everything else is forwarded byte-for-byte.
//
// # Why not patch the first frame by byte offset
//
// Revision 1 of the design claimed the rewrite could be an in-place byte patch
// of the first frame. That was withdrawn, and not for an edge case:
//
//   - Replacement lengths differ in the NORMAL case. "model-small" and
//     "qwen3.5:397b" are not the same length, so an in-place swap is possible
//     only in the rare case where they happen to match.
//   - Frames split at arbitrary read boundaries, so "model":"…" can straddle
//     two reads. A patcher that assumes it lies in the first buffer either
//     misses it or writes into the middle of a JSON string.
//   - The field is not reliably in the first frame; some backends emit it only
//     with the terminal usage frame.
//   - Compression forecloses it entirely — patching compressed bytes is not a
//     thing. dorang requests Accept-Encoding: identity upstream for exactly
//     this reason, and because it must read the terminal usage frame anyway.
//
// # Degrading to a plain copy
//
// When the requested and upstream names are identical (or no rewrite was asked
// for) and no usage extraction is needed, [NewScanner] short-circuits to
// io.Writer pass-through and the relay costs one copy.
//
// A Scanner implements io.Writer so the relay is io.Copy(scanner, upstream);
// call [Scanner.Flush] when the upstream body ends.
type Scanner struct {
	dst io.Writer

	// to is the replacement model name, pre-escaped as JSON string CONTENT
	// (no surrounding quotes), so the rewrite never allocates.
	to      []byte
	rewrite bool
	collect bool
	pass    bool

	maxFrame int

	// tail carries a partial line across reads. Reused, so a steady stream of
	// whole frames never allocates after the first.
	tail []byte
	// overflow marks that an oversized tail was already forwarded verbatim and
	// the rest of that line must be too.
	overflow bool

	usage    Usage
	hasUsage bool

	// from is the name dorang sent upstream, and served the first name the
	// upstream put in a frame. They are the two sides of the §17.1 rule-3
	// comparison — one read out of dorang's own configuration, one read out of
	// the provider's body — and agree is the verdict, computed once.
	//
	// served is a fixed array rather than a slice so that recording a name
	// costs no allocation: the scanner is already one heap object per stream
	// and this makes it 128 bytes larger rather than adding a second.
	from      string
	served    [canonical.MaxServedModelBytes]byte
	servedLen int
	agree     canonical.ModelAgreement
	sawModel  bool

	wrote bool
	// trailNL counts newlines at the end of what has been written, which is how
	// COMPATIBILITY 1.5's "only a frame missing its delimiter is re-framed" is
	// decided without buffering a frame.
	trailNL int

	err error
}

// NewScanner returns a scanner writing to dst.
func NewScanner(dst io.Writer, opt ScannerOptions) *Scanner {
	s := &Scanner{dst: dst, collect: opt.CollectUsage, maxFrame: opt.MaxFrameBytes, from: opt.From}
	if s.maxFrame <= 0 {
		s.maxFrame = DefaultMaxFrameBytes
	}
	if opt.To != "" && opt.To != opt.From {
		if b, err := Marshal(opt.To); err == nil && len(b) >= 2 {
			s.to = b[1 : len(b)-1]
			s.rewrite = true
		}
	}
	s.pass = !s.rewrite && !s.collect
	return s
}

// Passthrough reports whether the scanner degraded to a plain copy.
func (s *Scanner) Passthrough() bool { return s.pass }

// Usage returns the counts read from the terminal usage frame.
func (s *Scanner) Usage() (canonical.Usage, bool) {
	if !s.hasUsage {
		return canonical.Usage{}, false
	}
	return *usageToCanonical(&s.usage), true
}

// Write consumes a chunk of the upstream body.
//
// It always reports len(p) consumed on success, because a relay that reports a
// short write makes io.Copy synthesize ErrShortWrite for a stream that was in
// fact fully processed — the partial line is held, not dropped.
func (s *Scanner) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.pass {
		n, err := s.dst.Write(p)
		if err != nil {
			s.err = err
		} else {
			s.note(p)
		}
		return n, err
	}
	total := len(p)

	// Complete a line carried over from the previous read.
	if len(s.tail) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.tail = append(s.tail, p...)
			s.checkOverflow()
			return total, s.err
		}
		s.tail = append(s.tail, p[:i+1]...)
		p = p[i+1:]
		var err error
		if s.overflow {
			s.overflow = false
			err = s.write(s.tail)
		} else {
			err = s.emit(s.tail)
		}
		s.tail = s.tail[:0]
		if err != nil {
			return 0, err
		}
	}

	// Scan complete lines in place, batching every untouched byte into as few
	// writes as possible: an unmodified read is forwarded with one Write call.
	start, pos := 0, 0
	for pos < len(p) {
		i := bytes.IndexByte(p[pos:], '\n')
		if i < 0 {
			break
		}
		ls, le := pos, pos+i+1
		line := p[ls:le]
		if s.collect && !s.hasUsage {
			s.scanUsage(line)
		}
		if s.rewrite {
			if vs, ve, ok := modelValue(line); ok {
				s.noteServed(line[vs:ve])
				if err := s.write(p[start : ls+vs]); err != nil {
					return 0, err
				}
				if err := s.write(s.to); err != nil {
					return 0, err
				}
				start = ls + ve
			}
		} else if !s.sawModel {
			if vs, ve, ok := modelValue(line); ok {
				s.noteServed(line[vs:ve])
			}
		}
		pos = le
	}
	if pos > start {
		if err := s.write(p[start:pos]); err != nil {
			return 0, err
		}
	}
	if pos < len(p) {
		s.tail = append(s.tail, p[pos:]...)
		s.checkOverflow()
	}
	return total, s.err
}

// Flush emits any held partial frame and re-frames it if it is missing its
// delimiter (COMPATIBILITY 1.5). An upstream that produced nothing produces
// nothing here either — no [DONE], no newlines (1.4).
func (s *Scanner) Flush() error {
	if s.err != nil {
		return s.err
	}
	if s.pass {
		return nil
	}
	if len(s.tail) > 0 {
		var err error
		if s.overflow {
			s.overflow = false
			err = s.write(s.tail)
		} else {
			err = s.emit(s.tail)
		}
		s.tail = s.tail[:0]
		if err != nil {
			return err
		}
	}
	if s.wrote && s.trailNL < 2 {
		if err := s.write(newlines[:2-s.trailNL]); err != nil {
			return err
		}
	}
	return nil
}

var newlines = []byte("\n\n")

// emit forwards one complete line, rewriting the model value if it holds one.
func (s *Scanner) emit(line []byte) error {
	if s.collect && !s.hasUsage {
		s.scanUsage(line)
	}
	if s.rewrite {
		if vs, ve, ok := modelValue(line); ok {
			s.noteServed(line[vs:ve])
			if err := s.write(line[:vs]); err != nil {
				return err
			}
			if err := s.write(s.to); err != nil {
				return err
			}
			return s.write(line[ve:])
		}
	} else if !s.sawModel {
		if vs, ve, ok := modelValue(line); ok {
			s.noteServed(line[vs:ve])
		}
	}
	return s.write(line)
}

// ModelAgreement reports how the model name the UPSTREAM put in its frames
// compares with [ScannerOptions.From], the name dorang sent it.
//
// This is the streaming half of DESIGN §17.1 rule 3's "A disagrees with B",
// and a stream is where the disagreement is hardest to see: the field is
// per-chunk, dorang relays the body without buffering it, and — on the ordinary
// aliasing path, where To differs from From — the rewrite REPLACES the
// upstream's answer with the client-facing name on its way out, so the evidence
// never reaches the client at all. The verdict is taken before the splice.
//
// The reading is [canonical.ModelUnobserved] until the first frame carrying a
// model field, and never changes afterwards: the value is not re-read, so a
// stream of ten thousand chunks pays for one comparison. A frame whose model
// value contains a JSON escape is not decoded and reports unobserved rather
// than a guess — a name needing an escape cannot be compared byte-wise against
// a configuration string that does not.
//
// It is also unobserved for the whole of a stream on which the scanner
// degraded to a plain copy ([Scanner.Passthrough]), because nothing was read.
// dorang's own relay always asks for usage collection, so that path is not
// taken there.
//
// ModelAgreement allocates nothing.
func (s *Scanner) ModelAgreement() canonical.ModelAgreement { return s.agree }

// ServedModel returns the model name the upstream put in its own frames, or ""
// if none was read.
//
// It converts the recorded bytes to a string, so it allocates. Callers read it
// only when [Scanner.ModelAgreement] says there is something worth naming,
// which keeps the ordinary stream free of the allocation.
func (s *Scanner) ServedModel() string {
	if s.servedLen == 0 {
		return ""
	}
	return string(s.served[:s.servedLen])
}

// noteServed records the first model name a frame carried and settles the
// comparison. Subsequent frames are not consulted.
func (s *Scanner) noteServed(v []byte) {
	if s.sawModel {
		return
	}
	s.sawModel = true
	if len(v) == 0 || len(v) > len(s.served) || bytes.IndexByte(v, '\\') >= 0 {
		return
	}
	s.servedLen = copy(s.served[:], v)
	s.agree = canonical.CompareModelBytes(s.from, s.served[:s.servedLen])
}

func (s *Scanner) write(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if _, err := s.dst.Write(b); err != nil {
		s.err = err
		return err
	}
	s.note(b)
	return nil
}

// note tracks trailing newlines for the re-framing decision.
func (s *Scanner) note(b []byte) {
	if len(b) == 0 {
		return
	}
	s.wrote = true
	n := 0
	for i := len(b) - 1; i >= 0 && b[i] == '\n'; i-- {
		n++
	}
	if n == len(b) {
		s.trailNL += n
	} else {
		s.trailNL = n
	}
}

func (s *Scanner) checkOverflow() {
	if len(s.tail) <= s.maxFrame {
		return
	}
	// Forward what is held and stop trying to rewrite this frame. Faithful
	// forwarding beats an unbounded buffer; the model field of one pathological
	// frame is not worth the heap.
	if err := s.write(s.tail); err != nil {
		return
	}
	s.tail = s.tail[:0]
	s.overflow = true
}

var (
	modelKey = []byte(`"model"`)
	usageKey = []byte(`"usage"`)
	dataKey  = []byte("data:")
)

// modelValue locates the model field's value inside one SSE line and returns
// the byte range of the value CONTENT, excluding the quotes.
//
// It does not parse JSON. It does not have to: the byte sequence `"model"`
// cannot occur inside a JSON string, because every quote inside a string is
// escaped as \" and so the opening quote of the pattern can only be a real
// token boundary. The remaining false positive is a nested object that also has
// a "model" member, which the chunk shape does not have. Only the first match
// is rewritten.
func modelValue(line []byte) (start, end int, ok bool) {
	// A line that is not a data line has nothing to rewrite. Raw SSE lines pass
	// through verbatim (COMPATIBILITY 1.5).
	if !bytes.HasPrefix(bytes.TrimLeft(line, " \t"), dataKey) {
		return 0, 0, false
	}
	off := 0
	for {
		j := bytes.Index(line[off:], modelKey)
		if j < 0 {
			return 0, 0, false
		}
		k := off + j + len(modelKey)
		off = k
		k = skipSpace(line, k)
		if k >= len(line) || line[k] != ':' {
			continue
		}
		k = skipSpace(line, k+1)
		if k >= len(line) || line[k] != '"' {
			continue
		}
		k++
		vs := k
		for k < len(line) {
			switch line[k] {
			case '\\':
				k += 2
				continue
			case '"':
				return vs, k, true
			}
			k++
		}
		return 0, 0, false // unterminated string: leave the frame alone
	}
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	return i
}

// scanUsage reads the terminal usage frame, and only that frame.
//
// The bytes.Index gate is what keeps the promise of "reads only the terminal
// usage frame": every ordinary content frame fails it and is never handed to a
// JSON decoder. A "usage":null that some backends put on every frame also fails
// it, because the value must be an object.
func (s *Scanner) scanUsage(line []byte) {
	i := bytes.Index(line, usageKey)
	if i < 0 {
		return
	}
	k := skipSpace(line, i+len(usageKey))
	if k >= len(line) || line[k] != ':' {
		return
	}
	k = skipSpace(line, k+1)
	if k >= len(line) || line[k] != '{' {
		return
	}
	end, ok := matchObject(line, k)
	if !ok {
		return
	}
	var u Usage
	if err := json.Unmarshal(line[k:end], &u); err != nil {
		return
	}
	s.usage = u
	s.hasUsage = true
}

// matchObject returns the index just past the object starting at b[i] == '{'.
func matchObject(b []byte, i int) (int, bool) {
	depth := 0
	inStr := false
	for ; i < len(b); i++ {
		c := b[i]
		if inStr {
			switch c {
			case '\\':
				i++
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}
