package openai

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// frames builds an SSE body from JSON payloads.
func frames(payloads ...string) string {
	var b strings.Builder
	for _, p := range payloads {
		b.WriteString("data: ")
		b.WriteString(p)
		b.WriteString("\n\n")
	}
	b.WriteString(DoneFrame)
	return b.String()
}

func chunkJSON(model, delta string) string {
	return `{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1753660800,"model":"` +
		model + `","choices":[{"index":0,"delta":{"content":"` + delta + `"}}]}`
}

// scanAll feeds body through a scanner in the given split sizes.
func scanAll(t *testing.T, body string, opt ScannerOptions, split int) (string, *Scanner) {
	t.Helper()
	var out bytes.Buffer
	s := NewScanner(&out, opt)
	if split <= 0 {
		if _, err := s.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	} else {
		for i := 0; i < len(body); i += split {
			j := min(i+split, len(body))
			if _, err := s.Write([]byte(body[i:j])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	return out.String(), s
}

// TestScannerRewritesModelOnEveryFrame covers COMPATIBILITY 2.5 on the relay
// path: restamping is per frame, not a first-frame patch.
func TestScannerRewritesModelOnEveryFrame(t *testing.T) {
	body := frames(
		chunkJSON("upstream-internal-name", "a"),
		chunkJSON("upstream-internal-name", "b"),
		chunkJSON("upstream-internal-name", "c"),
	)
	for _, split := range []int{0, 1, 3, 7, 64, 1000} {
		got, _ := scanAll(t, body, ScannerOptions{From: "upstream-internal-name", To: "qwen3.5:397b"}, split)
		if n := strings.Count(got, `"model":"qwen3.5:397b"`); n != 3 {
			t.Errorf("split=%d: rewrote %d of 3 frames:\n%s", split, n, got)
		}
		if strings.Contains(got, "upstream-internal-name") {
			t.Errorf("split=%d: the upstream name leaked:\n%s", split, got)
		}
		if !strings.HasSuffix(got, DoneFrame) {
			t.Errorf("split=%d: [DONE] was mangled:\n%q", split, got)
		}
	}
}

// TestScannerReplacementLengthsDiffer is the case revision 1's byte-offset
// patch could not handle: the replacement is a different length, in both
// directions, and the field lands at a different offset in every frame.
func TestScannerReplacementLengthsDiffer(t *testing.T) {
	cases := [][2]string{
		{"m", "deepseek-v4-flash:cloud"},   // grows
		{"a-very-long-upstream-name", "m"}, // shrinks
		{"gemma4:31b", "zai:glm-5.1"},      // same length class, different bytes
	}
	for _, c := range cases {
		body := frames(chunkJSON(c[0], "x"), chunkJSON(c[0], "yy"))
		got, _ := scanAll(t, body, ScannerOptions{From: c[0], To: c[1]}, 5)
		want := frames(chunkJSON(c[1], "x"), chunkJSON(c[1], "yy"))
		if got != want {
			t.Errorf("%q -> %q\n got: %q\nwant: %q", c[0], c[1], got, want)
		}
	}
}

// TestScannerOpaqueModelNames freezes the colon-bearing names.
func TestScannerOpaqueModelNames(t *testing.T) {
	for _, from := range opaqueModels {
		for _, to := range opaqueModels {
			if from == to {
				continue
			}
			body := frames(chunkJSON(from, "x"))
			got, _ := scanAll(t, body, ScannerOptions{From: from, To: to}, 11)
			if !strings.Contains(got, `"model":"`+to+`"`) {
				t.Errorf("%q -> %q produced:\n%s", from, to, got)
			}
		}
	}
}

// TestScannerDegradesToPlainCopy covers DESIGN §7.2's short circuit.
func TestScannerDegradesToPlainCopy(t *testing.T) {
	s := NewScanner(io.Discard, ScannerOptions{From: "gemma4:31b", To: "gemma4:31b"})
	if !s.Passthrough() {
		t.Error("identical names did not short-circuit to a plain copy")
	}
	s = NewScanner(io.Discard, ScannerOptions{From: "a", To: ""})
	if !s.Passthrough() {
		t.Error("no rewrite requested did not short-circuit")
	}
	s = NewScanner(io.Discard, ScannerOptions{From: "a", To: "a", CollectUsage: true})
	if s.Passthrough() {
		t.Error("usage extraction must defeat the short circuit")
	}
	s = NewScanner(io.Discard, ScannerOptions{From: "a", To: "b"})
	if s.Passthrough() {
		t.Error("a required rewrite must defeat the short circuit")
	}

	body := frames(chunkJSON("gemma4:31b", "x"))
	got, _ := scanAll(t, body, ScannerOptions{From: "gemma4:31b", To: "gemma4:31b"}, 3)
	if got != body {
		t.Errorf("pass-through altered the bytes:\n got: %q\nwant: %q", got, body)
	}
}

// TestScannerReadsOnlyTheTerminalUsageFrame covers the other half of §15.2.3.
func TestScannerReadsOnlyTheTerminalUsageFrame(t *testing.T) {
	body := frames(
		chunkJSON("up", "a"),
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{}}],`+
			`"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":8}}}`,
	)
	for _, split := range []int{0, 1, 13, 97} {
		_, s := scanAll(t, body, ScannerOptions{From: "up", To: "gemma4:31b", CollectUsage: true}, split)
		u, ok := s.Usage()
		if !ok {
			t.Fatalf("split=%d: usage frame was not read", split)
		}
		if u.InputTokens != 11 || u.OutputTokens != 4 || u.CacheReadTokens != 8 {
			t.Errorf("split=%d: usage = %+v", split, u)
		}
	}
}

// TestScannerIgnoresNullUsage keeps a per-frame "usage":null from dragging
// every frame into a JSON decoder.
func TestScannerIgnoresNullUsage(t *testing.T) {
	body := frames(
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"up","choices":[],"usage":null}`,
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"up","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`,
	)
	_, s := scanAll(t, body, ScannerOptions{From: "up", To: "b", CollectUsage: true}, 0)
	u, ok := s.Usage()
	if !ok || u.InputTokens != 2 {
		t.Fatalf("usage = %+v ok=%v", u, ok)
	}
}

// TestScannerDoesNotRewriteInsideStrings is the false-positive guard: the byte
// sequence "model" cannot appear inside a JSON string because the quotes would
// be escaped, and this pins that reasoning.
func TestScannerDoesNotRewriteInsideStrings(t *testing.T) {
	payload := `{"id":"x","object":"chat.completion.chunk","created":1,"model":"up",` +
		`"choices":[{"index":0,"delta":{"content":"the \"model\":\"fake\" is not a key"}}]}`
	body := frames(payload)
	got, _ := scanAll(t, body, ScannerOptions{From: "up", To: "real:name"}, 0)
	if !strings.Contains(got, `"model":"real:name"`) {
		t.Fatalf("the real key was not rewritten:\n%s", got)
	}
	if !strings.Contains(got, `the \"model\":\"fake\" is not a key`) {
		t.Fatalf("the escaped occurrence inside content was corrupted:\n%s", got)
	}
	if strings.Count(got, "real:name") != 1 {
		t.Fatalf("rewrote more than the one real key:\n%s", got)
	}
}

// TestScannerPassesRawSSELinesVerbatim covers COMPATIBILITY 1.5.
func TestScannerPassesRawSSELinesVerbatim(t *testing.T) {
	body := ": a comment\nevent: ping\ndata: " + chunkJSON("up", "x") + "\n\n" + DoneFrame
	got, _ := scanAll(t, body, ScannerOptions{From: "up", To: "down"}, 4)
	if !strings.HasPrefix(got, ": a comment\nevent: ping\n") {
		t.Fatalf("raw lines were not forwarded verbatim:\n%q", got)
	}
	if !strings.Contains(got, `"model":"down"`) {
		t.Fatalf("the data line was not rewritten:\n%q", got)
	}
}

// TestScannerReframesAnUnterminatedFrame covers the second half of 1.5.
func TestScannerReframesAnUnterminatedFrame(t *testing.T) {
	body := "data: " + chunkJSON("up", "x") // no trailing \n\n at all
	got, _ := scanAll(t, body, ScannerOptions{From: "up", To: "down"}, 0)
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("frame missing its delimiter was not re-framed: %q", got)
	}
	if !strings.Contains(got, `"model":"down"`) {
		t.Fatalf("the held partial frame was not rewritten: %q", got)
	}
}

// TestScannerEmptyStreamStaysEmpty covers COMPATIBILITY 1.4 on the relay path.
func TestScannerEmptyStreamStaysEmpty(t *testing.T) {
	got, _ := scanAll(t, "", ScannerOptions{From: "up", To: "down"}, 0)
	if got != "" {
		t.Fatalf("an empty upstream stream produced %q", got)
	}
}

// TestScannerChunkingInvariance is the property the tail buffer exists for.
func TestScannerChunkingInvariance(t *testing.T) {
	body := frames(
		chunkJSON("upstream-name", "hello"),
		chunkJSON("upstream-name", " world"),
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"upstream-name","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
	)
	opt := ScannerOptions{From: "upstream-name", To: "deepseek-v4-flash:cloud", CollectUsage: true}
	whole, _ := scanAll(t, body, opt, 0)
	for _, split := range []int{1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144} {
		got, _ := scanAll(t, body, opt, split)
		if got != whole {
			t.Fatalf("split=%d changed the output\n got: %q\nwant: %q", split, got, whole)
		}
	}
}

// countingWriter records writes without allocating.
type countingWriter struct {
	n      int
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	w.writes++
	return len(p), nil
}

// TestScannerDoesNotAllocatePerFrame is the §15.2 claim, asserted rather than
// asserted-in-prose: single-pass, no-decode, and no per-frame allocation.
func TestScannerDoesNotAllocatePerFrame(t *testing.T) {
	body := []byte(frames(
		chunkJSON("upstream-name", "a"),
		chunkJSON("upstream-name", "b"),
		chunkJSON("upstream-name", "c"),
		chunkJSON("upstream-name", "d"),
	))

	t.Run("short-circuited plain copy", func(t *testing.T) {
		w := &countingWriter{}
		s := NewScanner(w, ScannerOptions{From: "gemma4:31b", To: "gemma4:31b"})
		if !s.Passthrough() {
			t.Fatal("expected the pass-through path")
		}
		if got := testingAllocsPerRun(100, func() { _, _ = s.Write(body) }); got != 0 {
			t.Errorf("pass-through allocated %v times per run", got)
		}
	})

	t.Run("scanning with no rewrite needed", func(t *testing.T) {
		// CollectUsage defeats the short circuit, so this exercises the real
		// line scanner on frames that need no model rewrite.
		w := &countingWriter{}
		s := NewScanner(w, ScannerOptions{From: "up", To: "up", CollectUsage: true})
		if s.Passthrough() {
			t.Fatal("expected the scanning path")
		}
		if got := testingAllocsPerRun(100, func() { _, _ = s.Write(body) }); got != 0 {
			t.Errorf("scanning allocated %v times per run", got)
		}
		if w.writes == 0 {
			t.Fatal("nothing was forwarded")
		}
	})

	t.Run("scanning with a rewrite on every frame", func(t *testing.T) {
		w := &countingWriter{}
		s := NewScanner(w, ScannerOptions{From: "upstream-name", To: "qwen3.5:397b"})
		if got := testingAllocsPerRun(100, func() { _, _ = s.Write(body) }); got != 0 {
			t.Errorf("rewriting allocated %v times per run", got)
		}
	})

	t.Run("partial frames reuse the tail buffer", func(t *testing.T) {
		w := &countingWriter{}
		s := NewScanner(w, ScannerOptions{From: "upstream-name", To: "qwen3.5:397b"})
		// Warm the tail buffer to its steady-state capacity first.
		for i := 0; i < len(body); i += 7 {
			_, _ = s.Write(body[i:min(i+7, len(body))])
		}
		if got := testingAllocsPerRun(50, func() {
			for i := 0; i < len(body); i += 7 {
				_, _ = s.Write(body[i:min(i+7, len(body))])
			}
		}); got != 0 {
			t.Errorf("split writes allocated %v times per run", got)
		}
	})
}

// TestScannerUnterminatedFrameIsBounded checks the heap guard.
func TestScannerUnterminatedFrameIsBounded(t *testing.T) {
	var out bytes.Buffer
	s := NewScanner(&out, ScannerOptions{From: "a", To: "b", MaxFrameBytes: 64})
	junk := bytes.Repeat([]byte("x"), 4096)
	for i := 0; i < 16; i++ {
		if _, err := s.Write(junk); err != nil {
			t.Fatal(err)
		}
		if len(s.tail) > 64+len(junk) {
			t.Fatalf("tail grew to %d bytes against a 64-byte cap", len(s.tail))
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if out.Len() < 16*len(junk) {
		t.Fatalf("forwarded %d of %d bytes — the overflow path dropped data", out.Len(), 16*len(junk))
	}
}

// TestScannerWriteErrorsPropagate keeps a failed client write from being
// silently swallowed into a stream that looks complete.
func TestScannerWriteErrorsPropagate(t *testing.T) {
	s := NewScanner(errWriter{}, ScannerOptions{From: "a", To: "b"})
	if _, err := s.Write([]byte("data: {\"model\":\"a\"}\n\n")); err == nil {
		t.Fatal("write error was swallowed")
	}
	if _, err := s.Write([]byte("more")); err == nil {
		t.Fatal("scanner kept accepting writes after a failure")
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
