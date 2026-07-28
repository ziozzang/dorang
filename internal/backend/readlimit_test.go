package backend

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// The tests in this file assert HOW MANY BYTES WERE READ, not what the caller
// was told.
//
// That distinction is the whole finding. An earlier test for the same defect
// asserted that an oversized response was refused, and it kept passing after the
// bound was deleted, because the length check ran after the read: the caller got
// an error and the process had already buffered the body. The condition being
// prevented is memory exhaustion, so the only assertion that prevents it counts
// bytes off the wire.

// meteredReader is an endless source of one byte, counting what was taken.
//
// It stops at cap and reports io.EOF so that a regression fails the test instead
// of hanging it — an unbounded read against a genuinely endless reader never
// returns. cap is therefore set well above every ceiling under test, and the
// assertion is on n, never on the fact that the read finished.
type meteredReader struct {
	n   int64
	cap int64
	pad byte
}

func (m *meteredReader) Read(p []byte) (int, error) {
	if m.n >= m.cap {
		return 0, io.EOF
	}
	if int64(len(p)) > m.cap-m.n {
		p = p[:m.cap-m.n]
	}
	for i := range p {
		p[i] = m.pad
	}
	m.n += int64(len(p))
	return len(p), nil
}

func (m *meteredReader) Close() error { return nil }

// TestUpstreamBodyReadIsBounded is the [HIGH] unbounded-read finding, asserted
// on the number of bytes taken from the upstream.
//
// The error branch of finish has always stopped at maxErrorBody; the success
// branch was io.ReadAll(resp.Body) with nothing in front of it, so a backend
// that answers 200 with a multi-gigabyte body drives the gateway out of memory
// and takes every other tenant's in-flight request with it.
func TestUpstreamBodyReadIsBounded(t *testing.T) {
	const limit = 64 << 10

	// The reader would supply thirty-two times the ceiling.
	src := &meteredReader{cap: limit * 32, pad: 'z'}
	body, err := readUpstreamBody(src, limit)

	if src.n > limit+1 {
		t.Fatalf("read %d bytes under a %d byte ceiling: the body is buffered first and "+
			"measured afterwards, which is the defect and not the fix", src.n, limit)
	}
	if !errors.Is(err, errUpstreamTooLarge) {
		t.Fatalf("err = %v, want errUpstreamTooLarge", err)
	}
	if body != nil {
		t.Errorf("a refused body was still returned: %d bytes", len(body))
	}

	// A body inside the ceiling is returned whole, so the bound is a bound and
	// not a refusal of everything.
	want := strings.Repeat("a", limit/2)
	got, err := readUpstreamBody(strings.NewReader(want), limit)
	if err != nil {
		t.Fatalf("a body inside the ceiling was refused: %v", err)
	}
	if string(got) != want {
		t.Errorf("body was truncated: %d bytes, want %d", len(got), len(want))
	}

	// And the shipped default is a real ceiling: a test that only ever exercises
	// an injected limit says nothing about what a deployment runs with.
	if DefaultMaxResponseBytes <= 0 {
		t.Error("DefaultMaxResponseBytes is not a ceiling")
	}
	if b := New(Options{Client: NewClient()}); b.maxBody != DefaultMaxResponseBytes {
		t.Errorf("an unconfigured Backend has maxBody = %d, want %d", b.maxBody, DefaultMaxResponseBytes)
	}
}

// What the CALLER is told about an oversized body — the 502, the
// upstream_response_too_large code, the refusal to mark it retryable — is
// asserted by TestOversizedUpstreamResponseIsRefused in errors_test.go. That is
// deliberately a separate test from this one: the two halves failed
// independently once already, and a single test asserting both is a test that
// can be satisfied by fixing either.

// TestSSEFrameReadIsBounded is the same defect on the streaming path, which is
// where it survived: nextSSEData called bufio.Reader.ReadString, whose contract
// is to grow until it finds the delimiter. An upstream that opens a frame and
// then never sends a newline made the gateway buffer the whole stream.
//
// Counting again, for the same reason: a check on the length of the returned
// payload would pass against an implementation that read every byte first.
func TestSSEFrameReadIsBounded(t *testing.T) {
	const limit = 64 << 10
	const bufSize = 8 << 10

	// `data: ` and then no newline, ever.
	src := &meteredReader{cap: limit * 32, pad: 'x'}
	body := io.MultiReader(strings.NewReader("data: "), src)
	br := bufio.NewReaderSize(body, bufSize)

	var buf strings.Builder
	payload, err := nextSSEData(br, &buf, limit)

	// bufio may hold one buffer's worth beyond what the ceiling admits, because
	// the ceiling is checked on what ReadSlice returns.
	if src.n > limit+bufSize {
		t.Fatalf("read %d bytes for one frame under a %d byte ceiling: the frame is "+
			"buffered without limit", src.n, limit)
	}
	if !errors.Is(err, errStreamFrameTooLarge) {
		t.Fatalf("err = %v, want errStreamFrameTooLarge", err)
	}
	if payload != "" {
		t.Errorf("a refused frame still returned %d bytes", len(payload))
	}
	if buf.Len() != 0 {
		t.Errorf("the frame buffer still holds %d bytes after the refusal", buf.Len())
	}
}

// TestSSEFrameCeilingCountsTheWholeFrame closes the other half: a frame is
// allowed to spread its payload over many data: lines, so a per-line ceiling
// alone bounds nothing. Each line here is far inside the limit and their sum is
// not.
func TestSSEFrameCeilingCountsTheWholeFrame(t *testing.T) {
	const limit = 4096
	line := "data: " + strings.Repeat("y", 512) + "\n"
	var sb strings.Builder
	for i := 0; i < 64; i++ {
		sb.WriteString(line)
	}
	sb.WriteString("\n")

	br := bufio.NewReaderSize(strings.NewReader(sb.String()), 1024)
	var buf strings.Builder
	if _, err := nextSSEData(br, &buf, limit); !errors.Is(err, errStreamFrameTooLarge) {
		t.Fatalf("err = %v, want errStreamFrameTooLarge: %d bytes of data: lines were "+
			"accumulated under a %d byte ceiling", err, 64*512, limit)
	}
	if buf.Len() != 0 {
		t.Errorf("the frame buffer still holds %d bytes after the refusal", buf.Len())
	}
}

// TestSSEFramesUnderTheCeilingStillParse is the regression guard: the ceiling
// must not change how an ordinary stream reads, including the multi-line frames
// SSE permits and the keep-alive comments upstreams send.
func TestSSEFramesUnderTheCeilingStillParse(t *testing.T) {
	const stream = ": keep-alive\n" +
		"data: {\"a\":1}\n" +
		"\n" +
		"data: line one\n" +
		"data: line two\n" +
		"\n" +
		"data: [DONE]\n" +
		"\n"
	br := bufio.NewReaderSize(strings.NewReader(stream), 16)
	var buf strings.Builder
	want := []string{`{"a":1}`, "line one\nline two", "[DONE]"}
	for _, w := range want {
		got, err := nextSSEData(br, &buf, maxStreamFrame)
		if err != nil {
			t.Fatalf("nextSSEData: %v", err)
		}
		if got != w {
			t.Fatalf("payload = %q, want %q", got, w)
		}
	}
	if _, err := nextSSEData(br, &buf, maxStreamFrame); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}
