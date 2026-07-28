package meter

import (
	"encoding/binary"
	"errors"
	"time"
)

// The spool's on-disk encoding. It is a private format with one job: survive a
// crash mid-append and be cheap to write. JSON would be neither -- a torn JSON
// object is unrecoverable without a framing layer anyway, so the framing layer
// is all there is.
//
//	segment := header record*
//	header  := "DRSP" version:u16 reserved:u16
//	record  := len:u32 crc32:u32 payload[len]
//	payload := version:u8 field*
//
// A record whose length or CRC does not check out ends the segment: everything
// before it is intact and everything after is a torn tail from a crash.

// The two versions move together when a trace field is added. The record codec
// reads positionally, so a segment written by a build with a different field
// list cannot be decoded field by field -- and a decode failure mid-stream is
// reported as spool corruption, which is a false alarm for what is really an
// upgrade. Bumping the SEGMENT version instead makes an old segment fail its
// header check, and an unrecognised segment is removed at open (see
// scanSegment): the tail of traces a previous build had not yet flushed is
// lost, which DESIGN §9.6 rule 2 already prices as "precision, not
// correctness", and nothing is reported as damage.
const (
	spoolMagic     = "DRSP"
	spoolVersion   = 3
	spoolHeaderLen = 8
	frameHeaderLen = 8
	traceCodecVer  = 3
	// maxFrameLen bounds a single record so a corrupt length cannot make the
	// reader allocate arbitrarily.
	maxFrameLen = 1 << 20
)

var (
	errCorrupt    = errors.New("meter: spool record is corrupt")
	errShortFrame = errors.New("meter: spool record is truncated")
	errBadHeader  = errors.New("meter: spool segment header is not recognised")

	// ErrSpoolFull is returned by the spool when accepting a trace would
	// exceed the configured size cap. It is the point at which trace payloads
	// start being dropped, and the meter raises Degraded rather than
	// swallowing it.
	ErrSpoolFull = errors.New("meter: trace spool is full")
)

func appendSpoolHeader(dst []byte) []byte {
	dst = append(dst, spoolMagic...)
	dst = binary.LittleEndian.AppendUint16(dst, spoolVersion)
	return binary.LittleEndian.AppendUint16(dst, 0)
}

func checkSpoolHeader(b []byte) error {
	if len(b) < spoolHeaderLen || string(b[:4]) != spoolMagic {
		return errBadHeader
	}
	if binary.LittleEndian.Uint16(b[4:]) != spoolVersion {
		return errBadHeader
	}
	return nil
}

func appendStr(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func takeStr(p []byte) (string, []byte, error) {
	n, k := binary.Uvarint(p)
	if k <= 0 || n > uint64(len(p)-k) {
		return "", nil, errCorrupt
	}
	return string(p[k : k+int(n)]), p[k+int(n):], nil
}

func takeInt(p []byte) (int64, []byte, error) {
	v, k := binary.Varint(p)
	if k <= 0 {
		return 0, nil, errCorrupt
	}
	return v, p[k:], nil
}

// appendTrace encodes t onto dst. Field order is fixed; a change to it is a
// change to spoolVersion, and an old segment with a version this build does
// not know is discarded rather than misread.
func appendTrace(dst []byte, t *Trace) []byte {
	dst = append(dst, traceCodecVer)
	dst = appendStr(dst, t.RequestID)
	dst = appendStr(dst, t.TraceID)
	dst = appendStr(dst, t.SpanID)
	dst = appendStr(dst, t.ParentSpanID)
	dst = binary.AppendVarint(dst, t.Time.UnixMicro())

	dst = appendStr(dst, t.APIKeyID)
	dst = appendStr(dst, t.SecretID)
	dst = appendStr(dst, t.UserID)
	dst = appendStr(dst, t.TeamID)
	dst = appendStr(dst, t.ModelGroup)
	dst = appendStr(dst, t.Provider)
	dst = appendStr(dst, t.CredentialID)
	dst = appendStr(dst, t.Endpoint)
	dst = appendStr(dst, t.UpstreamModel)
	dst = binary.AppendVarint(dst, int64(t.Status))

	dst = appendStr(dst, t.Excerpt)

	dst = binary.AppendVarint(dst, int64(t.Latency))
	dst = binary.AppendVarint(dst, int64(t.TTFT))
	dst = binary.AppendVarint(dst, int64(t.QueueWait))
	dst = binary.AppendVarint(dst, int64(t.RouteTime))
	dst = binary.AppendVarint(dst, int64(t.CapacityWait))
	dst = binary.AppendVarint(dst, int64(t.UpstreamConnect))

	dst = binary.AppendVarint(dst, t.Tokens.Input)
	dst = binary.AppendVarint(dst, t.Tokens.Output)
	dst = binary.AppendVarint(dst, t.Tokens.CacheRead)
	dst = binary.AppendVarint(dst, t.Tokens.CacheWrite)
	dst = binary.AppendVarint(dst, t.Tokens.Reasoning)
	dst = binary.AppendVarint(dst, t.CostNano)
	dst = binary.AppendVarint(dst, int64(t.Retries))

	dst = appendStr(dst, t.FallbackReason)
	dst = appendStr(dst, t.ErrorMessage)
	return dst
}

func decodeTrace(p []byte) (Trace, error) {
	var t Trace
	if len(p) == 0 || p[0] != traceCodecVer {
		return t, errCorrupt
	}
	p = p[1:]

	var err error
	str := func(dstp *string) bool {
		var s string
		s, p, err = takeStr(p)
		if err != nil {
			return false
		}
		*dstp = s
		return true
	}
	num := func() int64 {
		if err != nil {
			return 0
		}
		var v int64
		v, p, err = takeInt(p)
		return v
	}

	if !str(&t.RequestID) || !str(&t.TraceID) || !str(&t.SpanID) || !str(&t.ParentSpanID) {
		return t, errCorrupt
	}
	t.Time = time.UnixMicro(num()).UTC()

	if !str(&t.APIKeyID) || !str(&t.SecretID) || !str(&t.UserID) || !str(&t.TeamID) || !str(&t.ModelGroup) ||
		!str(&t.Provider) || !str(&t.CredentialID) || !str(&t.Endpoint) || !str(&t.UpstreamModel) {
		return t, errCorrupt
	}
	t.Status = int(num())
	if !str(&t.Excerpt) {
		return t, errCorrupt
	}

	t.Latency = time.Duration(num())
	t.TTFT = time.Duration(num())
	t.QueueWait = time.Duration(num())
	t.RouteTime = time.Duration(num())
	t.CapacityWait = time.Duration(num())
	t.UpstreamConnect = time.Duration(num())

	t.Tokens.Input = num()
	t.Tokens.Output = num()
	t.Tokens.CacheRead = num()
	t.Tokens.CacheWrite = num()
	t.Tokens.Reasoning = num()
	t.CostNano = num()
	t.Retries = int(num())

	if err != nil {
		return Trace{}, errCorrupt
	}
	if !str(&t.FallbackReason) || !str(&t.ErrorMessage) {
		return t, errCorrupt
	}
	return t, nil
}
