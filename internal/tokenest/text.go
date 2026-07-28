package tokenest

// Text estimates a string's token count, biased high.
//
// It is exported for callers outside this package that hold one field rather
// than a whole request. Everything else here goes through [Request] and friends.
func Text(s string) int64 { return text(s) }

// TextBytes is [Text] over a byte slice, so that raw JSON already in hand is
// counted without being converted to a string first — the conversion would
// allocate a copy of a field that can be megabytes.
func TextBytes(b []byte) int64 { return text(b) }

// textLike is what the scanner accepts. One implementation serves both because
// the scan is over bytes either way: it classifies by UTF-8 lead byte and never
// needs the decoded rune, so there is no string/[]byte split to duplicate and no
// conversion to allocate.
type textLike interface{ ~string | ~[]byte }

// The run classes. They are bytes rather than an enum so the run state is a
// register, not a struct.
const (
	classNone  = byte(0)
	classWord  = byte('w')
	classPunct = byte('p')
	classSpace = byte('s')
)

// text is the script-aware scan.
//
// ASCII is accumulated into runs and each run is divided by its class budget,
// because a tokenizer merges within a run and not across one: "tokenizer" is one
// or two tokens, ")]}," is one or two, and a newline plus eight spaces is one.
// Everything above ASCII is charged per character, not per byte — that is the
// whole correction, and it is what makes the count independent of whether the
// script needs two bytes or three.
//
// It runs in one pass with no allocation and no decoding: a lead byte says how
// long the sequence is and whether it is in the astral planes, which is all the
// classification needs.
func text[T textLike](t T) int64 {
	var (
		total    int64
		runClass = classNone
		runLen   int64
	)
	for i := 0; i < len(t); {
		c := t[i]
		if c < 0x80 {
			class := classPunct
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
				class = classWord
			case c == ' ', c == '\t', c == '\n', c == '\r', c == '\v', c == '\f':
				class = classSpace
			}
			if class != runClass {
				total += runTokens(runClass, runLen)
				runClass, runLen = class, 0
			}
			runLen++
			i++
			continue
		}
		total += runTokens(runClass, runLen)
		runClass, runLen = classNone, 0
		switch {
		case c >= 0xF0:
			// A four-byte sequence: emoji, and the astral planes generally. Two
			// tokens, because emoji routinely tokenize as a pair and a modifier
			// sequence as more.
			total += 2
			i += 4
		case c >= 0xE0:
			// Three bytes: Hangul, the CJK ideographs, most of the scripts that
			// bytes/3 was silently charging exactly one token per character with
			// no margin at all.
			total++
			i += 3
		case c >= 0xC0:
			total++
			i += 2
		default:
			// A continuation byte with no lead: the input is not valid UTF-8.
			// It is charged rather than skipped, because a body dorang cannot
			// decode is not a body it may under-count.
			total++
			i++
		}
	}
	return total + runTokens(runClass, runLen)
}

// runTokens divides one same-class ASCII run by its budget, rounding up.
func runTokens(class byte, n int64) int64 {
	if n <= 0 {
		return 0
	}
	switch class {
	case classWord:
		return (n + asciiWordBytesPerToken - 1) / asciiWordBytesPerToken
	case classSpace:
		return (n + asciiSpaceBytesPerToken - 1) / asciiSpaceBytesPerToken
	default:
		return (n + asciiPunctBytesPerToken - 1) / asciiPunctBytesPerToken
	}
}
