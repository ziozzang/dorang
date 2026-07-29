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
	spoolMagic = "DRSP"
	// Version 5 adds the trace's cost decomposition (marginal and subscription).
	spoolVersion   = 5
	spoolHeaderLen = 8
	frameHeaderLen = 8
	traceCodecVer  = 5
	// maxFrameLen bounds a single record so a corrupt length cannot make the
	// reader allocate arbitrarily.
	maxFrameLen = 1 << 20

	// The numeric carry-over file uses the same framing under its own magic and
	// its own version, so a rollup record can never be mistaken for a trace
	// record and a field-list change to either one does not invalidate the
	// other. An unrecognised header is discarded exactly as a segment's is.
	carryMagic = "DRRU"
	// Version 2 adds the bucket's cost decomposition.
	carryVersion   = 2
	carryHeaderLen = 8
	bucketCodecVer = 2
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

func appendCarryHeader(dst []byte) []byte {
	dst = append(dst, carryMagic...)
	dst = binary.LittleEndian.AppendUint16(dst, carryVersion)
	return binary.LittleEndian.AppendUint16(dst, 0)
}

func checkCarryHeader(b []byte) error {
	if len(b) < carryHeaderLen || string(b[:4]) != carryMagic {
		return errBadHeader
	}
	if binary.LittleEndian.Uint16(b[4:]) != carryVersion {
		return errBadHeader
	}
	return nil
}

// appendBucket encodes one merged rollup row. Like appendTrace it reads
// positionally, so a change to the field list is a change to carryVersion.
func appendBucket(dst []byte, b *Bucket) []byte {
	dst = append(dst, bucketCodecVer)
	dst = appendStr(dst, b.APIKeyID)
	dst = appendStr(dst, b.TeamID)
	dst = appendStr(dst, b.ModelGroup)
	dst = appendStr(dst, b.Provider)
	dst = appendStr(dst, b.CredentialID)
	dst = appendStr(dst, b.Endpoint)
	dst = binary.AppendVarint(dst, int64(b.StatusClass))
	dst = binary.AppendVarint(dst, b.HourStart.Unix())

	dst = binary.AppendVarint(dst, b.Requests)
	dst = binary.AppendVarint(dst, b.Errors)
	dst = binary.AppendVarint(dst, b.Tokens.Input)
	dst = binary.AppendVarint(dst, b.Tokens.Output)
	dst = binary.AppendVarint(dst, b.Tokens.CacheRead)
	dst = binary.AppendVarint(dst, b.Tokens.CacheWrite)
	dst = binary.AppendVarint(dst, b.Tokens.Reasoning)
	dst = binary.AppendVarint(dst, b.CostNano)
	dst = binary.AppendVarint(dst, b.MarginalCostNano)
	dst = binary.AppendVarint(dst, b.SubscriptionCostNano)
	dst = binary.AppendVarint(dst, int64(b.LatencySum))
	dst = binary.AppendVarint(dst, int64(b.TTFTSum))
	return binary.AppendVarint(dst, b.TTFTCount)
}

func decodeBucket(p []byte) (Bucket, error) {
	var b Bucket
	if len(p) == 0 || p[0] != bucketCodecVer {
		return b, errCorrupt
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

	if !str(&b.APIKeyID) || !str(&b.TeamID) || !str(&b.ModelGroup) ||
		!str(&b.Provider) || !str(&b.CredentialID) || !str(&b.Endpoint) {
		return Bucket{}, errCorrupt
	}
	sc := num()
	if sc < 0 || sc >= int64(numStatusClasses) {
		return Bucket{}, errCorrupt
	}
	b.StatusClass = StatusClass(sc)
	b.HourStart = time.Unix(num(), 0).UTC()

	b.Requests = num()
	b.Errors = num()
	b.Tokens.Input = num()
	b.Tokens.Output = num()
	b.Tokens.CacheRead = num()
	b.Tokens.CacheWrite = num()
	b.Tokens.Reasoning = num()
	b.CostNano = num()
	b.MarginalCostNano = num()
	b.SubscriptionCostNano = num()
	b.LatencySum = time.Duration(num())
	b.TTFTSum = time.Duration(num())
	b.TTFTCount = num()
	if err != nil {
		return Bucket{}, errCorrupt
	}
	return b, nil
}

// appendBool encodes a flag as a varint, so it decodes through the same takeInt
// the numeric fields use and a field-list change stays one shape.
func appendBool(dst []byte, b bool) []byte {
	var v int64
	if b {
		v = 1
	}
	return binary.AppendVarint(dst, v)
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
	dst = appendStr(dst, t.DeploymentID)
	dst = binary.AppendVarint(dst, int64(t.Status))
	dst = appendBool(dst, t.Streamed)

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
	dst = binary.AppendVarint(dst, t.MarginalCostNano)
	dst = binary.AppendVarint(dst, t.SubscriptionCostNano)
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
		!str(&t.Provider) || !str(&t.CredentialID) || !str(&t.Endpoint) || !str(&t.UpstreamModel) ||
		!str(&t.DeploymentID) {
		return t, errCorrupt
	}
	t.Status = int(num())
	t.Streamed = num() != 0
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
	t.MarginalCostNano = num()
	t.SubscriptionCostNano = num()
	t.Retries = int(num())

	if err != nil {
		return Trace{}, errCorrupt
	}
	if !str(&t.FallbackReason) || !str(&t.ErrorMessage) {
		return t, errCorrupt
	}
	return t, nil
}
