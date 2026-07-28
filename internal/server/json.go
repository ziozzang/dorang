package server

import (
	"strconv"
	"sync"
	"unicode/utf8"
)

// The serializer is normative, not incidental (COMPATIBILITY §2.1a). A
// "byte-for-byte" claim is meaningless without saying which serializer, so
// dorang authors its own JSON with these three properties fixed:
//
//  1. Compact separators — no space after ':' or ','.
//  2. Raw UTF-8 — no \uXXXX escaping of non-ASCII.
//  3. HTML escaping off — encoding/json turns '&' into & and '<' into
//     < by default, which no other server does, and which would make
//     `a && b` in an error message unreadable to a client diffing wire bytes.
//
// Hand-rolling also keeps reflection off the hot path (DESIGN §15.5) and makes
// key order a property of the code rather than of struct field order.

// hexDigits is the lowercase alphabet for \u escapes.
const hexDigits = "0123456789abcdef"

// replacementRune is U+FFFD as its UTF-8 bytes.
var replacementRune = []byte(string(utf8.RuneError))

// appendJSONString appends s as a JSON string literal, quotes included.
//
// Only the characters JSON requires to be escaped are escaped: the quote, the
// backslash, and C0 controls. Everything else — including '<', '>', '&', and
// every multi-byte UTF-8 sequence — is written as-is.
//
// Invalid UTF-8 is replaced with U+FFFD rather than emitted raw, because a
// response body that is not valid UTF-8 is not valid JSON and every client
// parser rejects the whole envelope rather than the one bad byte.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, replacementRune...)
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// appendInt appends a base-10 integer. It is the only integer formatter used on
// the hot path; fmt is prohibited there (DESIGN §15.5).
func appendInt(dst []byte, v int64) []byte { return strconv.AppendInt(dst, v, 10) }

// appendNanoUSD renders a nano-USD amount as a plain decimal with no exponent
// and no trailing zeros: 1_500_000_000 becomes "1.5", 123_456 becomes
// "0.000123456", 0 becomes "0".
//
// Money never goes through a float and never through fmt (DESIGN §15.5).
func appendNanoUSD(dst []byte, nano int64) []byte {
	if nano < 0 {
		dst = append(dst, '-')
		// Negating math.MinInt64 overflows; the saturated value is close
		// enough for a header that describes a cost, and the alternative is a
		// panic on a value that cannot occur.
		if nano == -1<<63 {
			nano = 1<<63 - 1
		} else {
			nano = -nano
		}
	}
	dst = strconv.AppendInt(dst, nano/1e9, 10)
	frac := nano % 1e9
	if frac == 0 {
		return dst
	}
	var d [9]byte
	for i := 8; i >= 0; i-- {
		d[i] = byte('0' + frac%10)
		frac /= 10
	}
	n := 9
	for n > 0 && d[n-1] == '0' {
		n--
	}
	dst = append(dst, '.')
	return append(dst, d[:n]...)
}

// bufPool holds scratch byte slices for response encoding. Buffers larger than
// bufReturnLimit are dropped rather than retained, so that one oversized
// response does not pin memory for the life of the process.
var bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 1024); return &b }}

const bufReturnLimit = 64 << 10

func getBuf() *[]byte {
	b := bufPool.Get().(*[]byte)
	*b = (*b)[:0]
	return b
}

func putBuf(b *[]byte) {
	if cap(*b) > bufReturnLimit {
		return
	}
	bufPool.Put(b)
}
