package mask

import "strings"

// Unmasker restores placeholders in a stream.
//
// # The bound
//
// A placeholder can straddle two frames — the model emits `…[PII:kfma` in one
// delta and `dpblnbeghcoi]…` in the next — so the unmasker holds a tail. The
// bound is [PlaceholderLen]-1 = 21 bytes: at most one byte less than a whole
// placeholder is ever held back, because that is the longest run that could
// still turn out to be the start of one.
//
// That bound is a real limit and it is why placeholders are short and
// fixed-width (DESIGN §10.5b rule 4). A variable-length placeholder would need a
// tail as long as the longest one an operator might configure, and a placeholder
// longer than the tail could not be reassembled at all.
//
// It costs no latency worth the name: a frame is emitted immediately, minus at
// most 21 trailing bytes, and those go out with the next frame or at flush. It
// costs no correctness at a frame boundary either, because the held bytes are
// prepended to the next chunk before anything is decided about them.
//
// # It is one pass
//
// §10.5 established that reconstructing the stream is the normal case and that
// every rewrite composes into a single scan. This is one of those rules, not a
// second pipeline: it takes decoded text, returns decoded text, and holds
// nothing but its tail.
type Unmasker struct {
	s     Session
	carry string
}

// Unmasker starts a streaming unmask over this session.
func (s Session) Unmasker() *Unmasker { return &Unmasker{s: s} }

// Write feeds the next piece of text and returns the piece that is safe to emit.
//
// The returned string may be shorter than the input — up to [PlaceholderLen]-1
// bytes shorter — or longer, when a placeholder was replaced by a longer
// original.
func (u *Unmasker) Write(text string) string {
	if text == "" {
		return ""
	}
	buf := text
	if u.carry != "" {
		buf = u.carry + text
		u.carry = ""
	}
	cut := holdFrom(buf)
	if cut < len(buf) {
		// Copy rather than reslice so the held tail does not pin the whole
		// chunk — the carry is bounded, the chunk is not.
		u.carry = string(append([]byte(nil), buf[cut:]...))
	}
	return u.s.substitute(buf[:cut])
}

// Flush emits whatever is held. It is called once, at the end of the stream: a
// tail that never completed is text, and text is the caller's.
func (u *Unmasker) Flush() string {
	if u.carry == "" {
		return ""
	}
	out := u.s.substitute(u.carry)
	u.carry = ""
	return out
}

// Held reports how many bytes are currently held back. It is bounded by
// [PlaceholderLen]-1 and is exported so a test can assert the bound rather than
// trust this comment.
func (u *Unmasker) Held() int { return len(u.carry) }

// holdFrom returns the offset at which s stops being safe to emit: the earliest
// position in the last PlaceholderLen-1 bytes where a placeholder could still be
// starting.
func holdFrom(s string) int {
	start := len(s) - (PlaceholderLen - 1)
	if start < 0 {
		start = 0
	}
	for j := start; j < len(s); j++ {
		if s[j] != Sentinel[0] {
			continue
		}
		if partialPlaceholder(s[j:]) {
			return j
		}
	}
	return len(s)
}

// partialPlaceholder reports whether t, which is shorter than a whole
// placeholder, could be the beginning of one.
func partialPlaceholder(t string) bool {
	if len(t) >= PlaceholderLen {
		return false
	}
	if len(t) <= len(Sentinel) {
		return t == Sentinel[:len(t)]
	}
	if t[:len(Sentinel)] != Sentinel {
		return false
	}
	for k := len(Sentinel); k < len(t); k++ {
		if c := t[k]; c < 'a' || c > 'p' {
			return false
		}
	}
	return true
}

// substitute replaces every whole placeholder this request issued, leaves
// everything else exactly as it is, and counts what it left.
func (s Session) substitute(text string) string {
	if text == "" || !strings.Contains(text, Sentinel) {
		return text
	}
	t := s.tab
	t.mu.Lock()
	defer t.mu.Unlock()

	var b strings.Builder
	b.Grow(len(text))
	i := 0
	for i < len(text) {
		j := strings.Index(text[i:], Sentinel)
		if j < 0 {
			b.WriteString(text[i:])
			break
		}
		j += i
		b.WriteString(text[i:j])

		if wellFormed(text, j) {
			ph := text[j : j+PlaceholderLen]
			if orig, ok := s.lookup(ph); ok {
				b.WriteString(orig)
				t.stats.Resolved++
			} else {
				// DESIGN §10.5b rule 3: an invented, truncated, translated or
				// replayed placeholder is left exactly as it is, and counted.
				b.WriteString(ph)
				t.stats.Unresolved++
			}
			i = j + PlaceholderLen
			continue
		}
		// Sentinel-shaped but not a placeholder. Also a signal, also left alone.
		b.WriteString(Sentinel)
		t.stats.Unresolved++
		i = j + len(Sentinel)
	}
	return b.String()
}
