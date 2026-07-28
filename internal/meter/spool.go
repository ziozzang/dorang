package meter

import (
	"bufio"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// spooler is the durable staging area between the trace ring and the sink. It
// is the piece that makes a store stall cost disk instead of data (DESIGN
// 12.1, REVIEW R1-B finding 1).
//
// Append/Next/Commit is a read-then-acknowledge protocol, not a dequeue: the
// records handed out by Next stay on disk until Commit, so a crash between the
// sink write and the acknowledgement replays them rather than losing them.
// That makes delivery at-least-once and requires the sink to be idempotent on
// RequestID, which Sink's contract states.
type spooler interface {
	// Append writes traces durably. It returns how many were accepted; a
	// short count with ErrSpoolFull means the cap was reached.
	Append(traces []Trace) (int, error)
	// Next appends up to max un-acknowledged traces to dst.
	Next(max int, dst []Trace) ([]Trace, error)
	// Commit acknowledges the last Next.
	Commit() error
	// Depth reports un-acknowledged records and bytes held.
	Depth() (records, bytes int64)
	Sync() error
	Close() error
}

// ---------------------------------------------------------------- mem spool

// memSpool is the spooler used when no directory is configured. It is bounded
// and correct but forfeits durability, which is the whole point of the spool;
// Config.SpoolDir documents that production must set it.
type memSpool struct {
	mu       sync.Mutex
	max      int64
	bytes    int64
	q        []Trace
	sizes    []int64
	uncommit int
	enc      []byte
}

func newMemSpool(max int64) *memSpool { return &memSpool{max: max} }

func (s *memSpool) Append(traces []Trace) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range traces {
		s.enc = appendTrace(s.enc[:0], &traces[i])
		n := int64(len(s.enc)) + frameHeaderLen
		if s.bytes+n > s.max {
			return i, ErrSpoolFull
		}
		s.q = append(s.q, traces[i])
		s.sizes = append(s.sizes, n)
		s.bytes += n
	}
	return len(traces), nil
}

func (s *memSpool) Next(max int, dst []Trace) ([]Trace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := min(max, len(s.q))
	s.uncommit = n
	return append(dst, s.q[:n]...), nil
}

func (s *memSpool) Commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < s.uncommit; i++ {
		s.bytes -= s.sizes[i]
	}
	s.q = append(s.q[:0], s.q[s.uncommit:]...)
	s.sizes = append(s.sizes[:0], s.sizes[s.uncommit:]...)
	s.uncommit = 0
	return nil
}

func (s *memSpool) Depth() (int64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.q)), s.bytes
}

func (s *memSpool) Sync() error  { return nil }
func (s *memSpool) Close() error { return nil }

// --------------------------------------------------------------- disk spool

const (
	segPrefix  = "seg-"
	segSuffix  = ".spool"
	cursorName = "cursor"
	cursorTmp  = "cursor.tmp"
)

type dseg struct {
	seq  uint64
	path string
	size int64

	f *os.File      // write handle, non-nil only for the active segment
	w *bufio.Writer // buffered writer over f
}

func (s *dseg) sealed() bool { return s.f == nil }

// diskSpool is an append-only sequence of segment files with a read cursor.
//
// Layout under dir:
//
//	seg-<seq>.spool   append-only records, oldest seq first
//	cursor            "<seq> <offset>": the first un-acknowledged record
//
// The cursor is rewritten atomically (write + rename) on every Commit, so a
// crash re-delivers at most one batch. A segment is deleted once fully
// acknowledged, which is what bounds disk use in the healthy case; the size
// cap is what bounds it in the unhealthy one.
type diskSpool struct {
	dir      string
	maxBytes int64
	segBytes int64
	fsync    bool

	mu      sync.Mutex
	segs    []*dseg
	nextSeq uint64
	total   int64 // bytes on disk across all segments
	records int64 // un-acknowledged records

	readIdx int
	readOff int64

	// pending read state, applied by Commit
	pIdx    int
	pOff    int64
	pN      int
	pending bool

	enc  []byte // append scratch
	rbuf []byte // frame read scratch

	rf    *os.File // cached read handle
	rfSeq uint64
	rfOk  bool
}

func openDiskSpool(dir string, maxBytes, segBytes int64, fsync bool) (*diskSpool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("meter: create spool dir: %w", err)
	}
	s := &diskSpool{dir: dir, maxBytes: maxBytes, segBytes: segBytes, fsync: fsync}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("meter: read spool dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, segPrefix) || !strings.HasSuffix(name, segSuffix) {
			continue
		}
		seq, err := strconv.ParseUint(name[len(segPrefix):len(name)-len(segSuffix)], 16, 64)
		if err != nil {
			continue
		}
		s.segs = append(s.segs, &dseg{seq: seq, path: filepath.Join(dir, name)})
	}
	sort.Slice(s.segs, func(i, j int) bool { return s.segs[i].seq < s.segs[j].seq })

	cursorSeq, cursorOff := s.readCursor()

	// Validate every segment: a crash mid-append leaves a torn tail, which is
	// truncated rather than treated as corruption of the whole file.
	kept := s.segs[:0]
	for _, sg := range s.segs {
		valid, err := scanSegment(sg.path)
		if err != nil {
			// An unreadable or unrecognisable segment is removed. Keeping it
			// would wedge the spool permanently; the loss is already counted
			// by the fact that these traces never reach the sink.
			_ = os.Remove(sg.path)
			continue
		}
		if sg.seq < cursorSeq {
			// Fully acknowledged before the crash but not yet unlinked.
			_ = os.Remove(sg.path)
			continue
		}
		sg.size = valid
		kept = append(kept, sg)
	}
	s.segs = kept

	for _, sg := range s.segs {
		s.total += sg.size
		if sg.seq > s.nextSeq {
			s.nextSeq = sg.seq
		}
	}
	if len(s.segs) > 0 {
		s.nextSeq++
	}

	// Position the cursor. Un-drained segments are replayed from the cursor
	// before any new write, which is the restart guarantee.
	s.readIdx, s.readOff = 0, spoolHeaderLen
	if len(s.segs) > 0 && s.segs[0].seq == cursorSeq && cursorOff > 0 {
		if cursorOff > s.segs[0].size {
			cursorOff = s.segs[0].size
		}
		s.readOff = cursorOff
	}
	if err := s.countRecords(); err != nil {
		return nil, err
	}

	// Reopen the newest segment for appending when it still has room.
	if n := len(s.segs); n > 0 && s.segs[n-1].size < s.segBytes {
		last := s.segs[n-1]
		f, err := os.OpenFile(last.path, os.O_WRONLY, 0o600)
		if err == nil {
			if _, err := f.Seek(last.size, 0); err != nil {
				_ = f.Close()
			} else {
				if err := f.Truncate(last.size); err != nil {
					_ = f.Close()
				} else {
					last.f, last.w = f, bufio.NewWriter(f)
					s.nextSeq = last.seq + 1
				}
			}
		}
	}
	return s, nil
}

func (s *diskSpool) readCursor() (seq uint64, off int64) {
	b, err := os.ReadFile(filepath.Join(s.dir, cursorName))
	if err != nil {
		return 0, 0
	}
	var q uint64
	var o int64
	if n, _ := fmt.Sscanf(string(b), "%d %d", &q, &o); n != 2 {
		return 0, 0
	}
	return q, o
}

func (s *diskSpool) writeCursor() error {
	seq := uint64(0)
	off := int64(0)
	if s.readIdx < len(s.segs) {
		seq, off = s.segs[s.readIdx].seq, s.readOff
	} else if n := len(s.segs); n > 0 {
		seq, off = s.segs[n-1].seq, s.segs[n-1].size
	}
	tmp := filepath.Join(s.dir, cursorTmp)
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d %d\n", seq, off)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, cursorName))
}

// countRecords walks from the cursor to the end, so Depth is honest across a
// restart instead of reporting zero until the first drain.
func (s *diskSpool) countRecords() error {
	var n int64
	idx, off := s.readIdx, s.readOff
	for idx < len(s.segs) {
		sg := s.segs[idx]
		if off >= sg.size {
			idx++
			off = spoolHeaderLen
			continue
		}
		_, next, err := s.readFrame(sg, off)
		if err != nil {
			break
		}
		off = next
		n++
	}
	s.records = n
	return nil
}

// scanSegment validates path and returns the offset of the first byte that is
// not part of an intact record, truncating the file there.
func scanSegment(path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	if size < spoolHeaderLen {
		return 0, errBadHeader
	}
	var hdr [spoolHeaderLen]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return 0, err
	}
	if err := checkSpoolHeader(hdr[:]); err != nil {
		return 0, err
	}

	off := int64(spoolHeaderLen)
	var fh [frameHeaderLen]byte
	buf := make([]byte, 0, 4096)
	for off < size {
		if size-off < frameHeaderLen {
			break
		}
		if _, err := f.ReadAt(fh[:], off); err != nil {
			break
		}
		l := int64(leU32(fh[0:4]))
		crc := leU32(fh[4:8])
		if l <= 0 || l > maxFrameLen || off+frameHeaderLen+l > size {
			break
		}
		if int64(cap(buf)) < l {
			buf = make([]byte, l)
		}
		buf = buf[:l]
		if _, err := f.ReadAt(buf, off+frameHeaderLen); err != nil {
			break
		}
		if crc32.ChecksumIEEE(buf) != crc {
			break
		}
		off += frameHeaderLen + l
	}
	if off < size {
		if err := f.Truncate(off); err != nil {
			return 0, err
		}
	}
	return off, nil
}

func leU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func putLeU32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func (s *diskSpool) Append(traces []Trace) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	written := 0
	for i := range traces {
		s.enc = appendTrace(s.enc[:0], &traces[i])
		n := int64(frameHeaderLen + len(s.enc))
		if s.total+n > s.maxBytes {
			return written, s.flushActive(ErrSpoolFull)
		}
		sg, err := s.activeFor(n)
		if err != nil {
			return written, s.flushActive(err)
		}
		var fh [frameHeaderLen]byte
		putLeU32(fh[0:4], uint32(len(s.enc)))
		putLeU32(fh[4:8], crc32.ChecksumIEEE(s.enc))
		if _, err := sg.w.Write(fh[:]); err != nil {
			return written, s.flushActive(err)
		}
		if _, err := sg.w.Write(s.enc); err != nil {
			return written, s.flushActive(err)
		}
		sg.size += n
		s.total += n
		s.records++
		written++
	}
	return written, s.flushActive(nil)
}

// flushActive pushes the buffered writer to the OS so a concurrent reader sees
// the records, and returns first if it is non-nil.
func (s *diskSpool) flushActive(first error) error {
	n := len(s.segs)
	if n == 0 {
		return first
	}
	sg := s.segs[n-1]
	if sg.w == nil {
		return first
	}
	if err := sg.w.Flush(); err != nil && first == nil {
		first = err
	}
	if s.fsync && sg.f != nil {
		if err := sg.f.Sync(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// activeFor returns the segment to append n bytes to, rolling when needed.
func (s *diskSpool) activeFor(n int64) (*dseg, error) {
	if k := len(s.segs); k > 0 {
		sg := s.segs[k-1]
		if !sg.sealed() && sg.size+n <= s.segBytes {
			return sg, nil
		}
		if !sg.sealed() {
			if err := s.seal(sg); err != nil {
				return nil, err
			}
		}
	}
	seq := s.nextSeq
	s.nextSeq++
	path := filepath.Join(s.dir, fmt.Sprintf("%s%016x%s", segPrefix, seq, segSuffix))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("meter: create spool segment: %w", err)
	}
	w := bufio.NewWriter(f)
	if _, err := w.Write(appendSpoolHeader(nil)); err != nil {
		_ = f.Close()
		return nil, err
	}
	sg := &dseg{seq: seq, path: path, size: spoolHeaderLen, f: f, w: w}
	s.segs = append(s.segs, sg)
	s.total += spoolHeaderLen
	return sg, nil
}

func (s *diskSpool) seal(sg *dseg) error {
	if sg.w != nil {
		if err := sg.w.Flush(); err != nil {
			return err
		}
	}
	var err error
	if sg.f != nil {
		if s.fsync {
			err = sg.f.Sync()
		}
		if cerr := sg.f.Close(); err == nil {
			err = cerr
		}
	}
	sg.f, sg.w = nil, nil
	return err
}

func (s *diskSpool) readerFor(sg *dseg) (*os.File, error) {
	if s.rfOk && s.rfSeq == sg.seq {
		return s.rf, nil
	}
	if s.rf != nil {
		_ = s.rf.Close()
		s.rf, s.rfOk = nil, false
	}
	f, err := os.Open(sg.path)
	if err != nil {
		return nil, err
	}
	s.rf, s.rfSeq, s.rfOk = f, sg.seq, true
	return f, nil
}

func (s *diskSpool) readFrame(sg *dseg, off int64) ([]byte, int64, error) {
	f, err := s.readerFor(sg)
	if err != nil {
		return nil, 0, err
	}
	if sg.size-off < frameHeaderLen {
		return nil, 0, errShortFrame
	}
	var fh [frameHeaderLen]byte
	if _, err := f.ReadAt(fh[:], off); err != nil {
		return nil, 0, errShortFrame
	}
	l := int64(leU32(fh[0:4]))
	crc := leU32(fh[4:8])
	if l <= 0 || l > maxFrameLen || off+frameHeaderLen+l > sg.size {
		return nil, 0, errShortFrame
	}
	if int64(cap(s.rbuf)) < l {
		s.rbuf = make([]byte, l)
	}
	buf := s.rbuf[:l]
	if _, err := f.ReadAt(buf, off+frameHeaderLen); err != nil {
		return nil, 0, errShortFrame
	}
	if crc32.ChecksumIEEE(buf) != crc {
		return nil, 0, errCorrupt
	}
	return buf, off + frameHeaderLen + l, nil
}

func (s *diskSpool) Next(max int, dst []Trace) ([]Trace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx, off := s.readIdx, s.readOff
	n := 0
	for n < max && idx < len(s.segs) {
		sg := s.segs[idx]
		if off >= sg.size {
			if idx == len(s.segs)-1 {
				break // caught up with the active segment
			}
			idx++
			off = spoolHeaderLen
			continue
		}
		payload, next, err := s.readFrame(sg, off)
		if err == errShortFrame {
			break
		}
		if err != nil {
			// A CRC failure inside an already-validated segment means the
			// file changed underneath us. Stop and report; the meter turns
			// this into ReasonSpoolError rather than looping on it.
			s.pIdx, s.pOff, s.pN, s.pending = idx, off, n, n > 0
			return dst, err
		}
		t, derr := decodeTrace(payload)
		if derr != nil {
			s.pIdx, s.pOff, s.pN, s.pending = idx, off, n, n > 0
			return dst, derr
		}
		dst = append(dst, t)
		off = next
		n++
	}
	s.pIdx, s.pOff, s.pN, s.pending = idx, off, n, true
	return dst, nil
}

func (s *diskSpool) Commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pending {
		return nil
	}
	s.pending = false
	s.records -= int64(s.pN)
	if s.records < 0 {
		s.records = 0
	}

	drop := s.pIdx
	// The segment the cursor now sits in is also droppable when it is sealed
	// and fully read.
	if drop < len(s.segs) {
		sg := s.segs[drop]
		if sg.sealed() && s.pOff >= sg.size {
			drop++
			s.pOff = spoolHeaderLen
		}
	}
	for i := 0; i < drop; i++ {
		sg := s.segs[i]
		if s.rfOk && s.rfSeq == sg.seq {
			_ = s.rf.Close()
			s.rf, s.rfOk = nil, false
		}
		if !sg.sealed() {
			_ = s.seal(sg)
		}
		_ = os.Remove(sg.path)
		s.total -= sg.size
	}
	s.segs = append(s.segs[:0], s.segs[drop:]...)
	s.readIdx = 0
	s.readOff = s.pOff
	if len(s.segs) == 0 {
		s.readOff = spoolHeaderLen
	}

	// Steady state: one active segment, fully read. Rewind it rather than
	// letting it grow to segBytes before it can be recycled.
	if len(s.segs) == 1 {
		sg := s.segs[0]
		if !sg.sealed() && s.readOff >= sg.size && sg.size > spoolHeaderLen {
			if err := sg.w.Flush(); err == nil {
				if err := sg.f.Truncate(spoolHeaderLen); err == nil {
					if _, err := sg.f.Seek(spoolHeaderLen, 0); err == nil {
						sg.w.Reset(sg.f)
						s.total -= sg.size - spoolHeaderLen
						sg.size = spoolHeaderLen
						s.readOff = spoolHeaderLen
						if s.rfOk && s.rfSeq == sg.seq {
							_ = s.rf.Close()
							s.rf, s.rfOk = nil, false
						}
					}
				}
			}
		}
	}
	return s.writeCursor()
}

func (s *diskSpool) Depth() (int64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records, s.total
}

func (s *diskSpool) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushActive(nil); err != nil {
		return err
	}
	if n := len(s.segs); n > 0 && s.segs[n-1].f != nil {
		return s.segs[n-1].f.Sync()
	}
	return nil
}

func (s *diskSpool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for _, sg := range s.segs {
		if !sg.sealed() {
			if err := s.seal(sg); err != nil && first == nil {
				first = err
			}
		}
	}
	if s.rf != nil {
		_ = s.rf.Close()
		s.rf, s.rfOk = nil, false
	}
	if err := s.writeCursor(); err != nil && first == nil {
		first = err
	}
	return first
}
