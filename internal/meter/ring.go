package meter

import (
	"crypto/sha256"
	"encoding/hex"
	"sync/atomic"
	"unicode/utf8"
)

// ring is a bounded lock-free MPMC queue of trace payloads (Vyukov's
// sequence-numbered slot ring). It has three properties Record needs and a
// channel does not have all of:
//
//   - push never blocks and never parks; a full ring is a returned false, not
//     a wait, so a stalled consumer cannot stall a request.
//   - push allocates nothing: the Trace is copied into a preallocated slot and
//     the excerpt memcpy'd into a preallocated byte arena.
//   - push takes no lock. A buffered channel would take the channel mutex even
//     on a non-blocking send, which is a contended lock at gateway
//     concurrency -- the exact thing DESIGN 15.5 forbids on the hot path.
//
// Copying the excerpt rather than keeping the caller's string also means the
// ring never pins a caller buffer: retaining a 512-byte substring of a 400 KiB
// request body would pin the whole body for as long as the trace sat queued.
type ring struct {
	mask    uint64
	excSize int

	_    [cacheLine]byte
	head atomic.Uint64
	_    [cacheLine]byte
	tail atomic.Uint64
	_    [cacheLine]byte

	buf   []slot
	arena []byte

	mode  ExcerptMode
	chars int
}

type slot struct {
	seq    atomic.Uint64
	val    Trace
	excLen int32
}

func newRing(size int, excSize int, mode ExcerptMode, chars int) *ring {
	r := &ring{
		mask:    uint64(size - 1),
		excSize: excSize,
		buf:     make([]slot, size),
		mode:    mode,
		chars:   chars,
	}
	if excSize > 0 {
		r.arena = make([]byte, size*excSize)
	}
	for i := range r.buf {
		r.buf[i].seq.Store(uint64(i))
	}
	return r
}

// cap returns the number of slots.
func (r *ring) capacity() int { return len(r.buf) }

// depth is an estimate of the number of queued traces. It is an estimate
// because head and tail are read separately; it is only ever used for
// reporting and for deciding whether to poke the drainer.
func (r *ring) depth() uint64 {
	h := r.head.Load()
	t := r.tail.Load()
	if h < t {
		return 0
	}
	return h - t
}

// push copies t and excerpt into the ring. It returns false when the ring is
// full. t.Excerpt is ignored; the excerpt is passed separately so the caller
// does not have to build a truncated string (which would allocate).
func (r *ring) push(t *Trace, excerpt string) bool {
	pos := r.head.Load()
	for {
		s := &r.buf[pos&r.mask]
		seq := s.seq.Load()
		switch dif := int64(seq) - int64(pos); {
		case dif == 0:
			if r.head.CompareAndSwap(pos, pos+1) {
				s.val = *t
				s.val.Excerpt = ""
				s.excLen = 0
				if r.excSize > 0 && len(excerpt) > 0 {
					off := int(pos&r.mask) * r.excSize
					s.excLen = int32(copy(r.arena[off:off+r.excSize], excerpt))
				}
				// Release: the consumer reads val only after observing this
				// store, so the copy above happens-before the read.
				s.seq.Store(pos + 1)
				return true
			}
			pos = r.head.Load()
		case dif < 0:
			return false // full
		default:
			pos = r.head.Load()
		}
	}
}

// pop moves the oldest trace into out. It returns false when the ring is
// empty. Unlike push, pop allocates: it materialises the excerpt string. That
// is deliberate -- the cost belongs to the drainer, not to the request.
func (r *ring) pop(out *Trace) bool {
	pos := r.tail.Load()
	for {
		s := &r.buf[pos&r.mask]
		seq := s.seq.Load()
		switch dif := int64(seq) - int64(pos+1); {
		case dif == 0:
			if r.tail.CompareAndSwap(pos, pos+1) {
				*out = s.val
				if n := int(s.excLen); n > 0 {
					off := int(pos&r.mask) * r.excSize
					out.Excerpt = r.renderExcerpt(r.arena[off : off+n])
				}
				// Drop the slot's string references so a queued-then-drained
				// trace does not keep its ids alive until the slot is reused.
				s.val = Trace{}
				s.excLen = 0
				s.seq.Store(pos + r.mask + 1)
				return true
			}
			pos = r.tail.Load()
		case dif < 0:
			return false // empty
		default:
			pos = r.tail.Load()
		}
	}
}

// renderExcerpt turns the raw arena bytes into the configured representation.
func (r *ring) renderExcerpt(b []byte) string {
	b = truncateRunes(b, r.chars)
	if len(b) == 0 {
		return ""
	}
	if r.mode == ExcerptHash {
		sum := sha256.Sum256(b)
		var dst [7 + 2*sha256.Size]byte
		copy(dst[:], "sha256:")
		hex.Encode(dst[7:], sum[:])
		return string(dst[:])
	}
	return string(b)
}

// truncateRunes cuts b to at most n runes and drops a trailing partial rune.
// The arena copy is a byte-wise memcpy, so the tail may be mid-sequence.
func truncateRunes(b []byte, n int) []byte {
	count := 0
	i := 0
	for i < len(b) {
		if count == n {
			return b[:i]
		}
		if !utf8.FullRune(b[i:]) {
			// A multi-byte sequence cut by the arena boundary. Stop before
			// it: a decoder downstream should never be handed a torn rune.
			return b[:i]
		}
		_, size := utf8.DecodeRune(b[i:])
		i += size
		count++
	}
	return b
}
