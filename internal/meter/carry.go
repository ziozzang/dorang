package meter

import (
	"hash/crc32"
	"os"
	"path/filepath"
)

// The numeric path's durable carry-over.
//
// DESIGN 12.1 splits metering in two so that the trace payload may be dropped
// and the counters may not: "Nothing on this path is droppable. If the sink
// refuses a batch of rollups the merged buckets are carried over and merged
// into the next flush." That held for as long as the process lived, and only
// for that long — the carry-over was a slice, so a shutdown against an
// unreachable store lost every counter it held, which is precisely the case the
// promise was written for. The trace path had a spool for exactly this and the
// numeric path had nothing.
//
// This is that spool, in the shape the numeric path needs. It is one file
// rather than a segment sequence because the carry-over is not a stream: it is
// a bounded set (Config.MaxPendingBuckets) that is entirely rewritten on every
// flush, so an atomic replace is both simpler and safer than an append log —
// there is no torn tail to reason about, only the previous complete file or the
// next one.
const (
	carryName = "rollups.carry"
	carryTmp  = "rollups.carry.tmp"
)

// writeCarry replaces the carry-over file with buckets, atomically. An empty
// set removes the file: nothing outstanding must not read as "the last outage's
// counters are still owed".
func writeCarry(dir string, buckets []Bucket) error {
	if len(buckets) == 0 {
		return removeCarry(dir)
	}
	buf := appendCarryHeader(make([]byte, 0, carryHeaderLen+len(buckets)*96))
	var rec []byte
	for i := range buckets {
		rec = appendBucket(rec[:0], &buckets[i])
		var fh [frameHeaderLen]byte
		putLeU32(fh[0:4], uint32(len(rec)))
		putLeU32(fh[4:8], crc32.ChecksumIEEE(rec))
		buf = append(buf, fh[:]...)
		buf = append(buf, rec...)
	}

	tmp := filepath.Join(dir, carryTmp)
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, carryName))
}

func removeCarry(dir string) error {
	err := os.Remove(filepath.Join(dir, carryName))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// readCarry loads the carry-over left by a previous process. A missing file is
// the ordinary case and not an error; a truncated or corrupt tail yields
// everything before it, on the same reasoning the trace spool truncates a torn
// record — partial counters are worth more than none, and losing the whole file
// to its last few bytes is the failure mode this format exists to avoid.
func readCarry(dir string) ([]Bucket, error) {
	b, err := os.ReadFile(filepath.Join(dir, carryName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if err := checkCarryHeader(b); err != nil {
		// Written by a build with a different field list. Discarding it is the
		// same trade the spool makes at a version bump.
		return nil, nil
	}

	var out []Bucket
	off := carryHeaderLen
	for off+frameHeaderLen <= len(b) {
		l := int(leU32(b[off : off+4]))
		crc := leU32(b[off+4 : off+8])
		if l <= 0 || l > maxFrameLen || off+frameHeaderLen+l > len(b) {
			break
		}
		payload := b[off+frameHeaderLen : off+frameHeaderLen+l]
		if crc32.ChecksumIEEE(payload) != crc {
			break
		}
		bk, err := decodeBucket(payload)
		if err != nil {
			break
		}
		out = append(out, bk)
		off += frameHeaderLen + l
	}
	return out, nil
}

// persistPendingLocked makes the carry-over file match the carry-over in
// memory. It runs on every numeric flush, so the file is written when a flush
// fails and removed when the next one succeeds, and a crash or a Close at any
// point leaves the counters on disk rather than in a slice that is about to
// stop existing.
//
// It must be called with numMu held.
func (m *Meter) persistPendingLocked() {
	if m.carryDir == "" {
		return // no spool directory: the same forfeit memSpool makes for traces
	}
	if len(m.pending) == 0 {
		if !m.carryOnDisk {
			return
		}
		if err := removeCarry(m.carryDir); err != nil {
			m.setDegraded(ReasonSpoolError)
			return
		}
		m.carryOnDisk = false
		return
	}
	if err := writeCarry(m.carryDir, m.pending); err != nil {
		m.setDegraded(ReasonSpoolError)
		return
	}
	m.carryOnDisk = true
}
