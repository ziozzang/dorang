package openai

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

func runScanner(body []byte, opt ScannerOptions, split int) string {
	var out bytes.Buffer
	s := NewScanner(&out, opt)
	if split <= 0 {
		_, _ = s.Write(body)
	} else {
		for i := 0; i < len(body); i += split {
			j := min(i+split, len(body))
			_, _ = s.Write(body[i:j])
		}
	}
	_ = s.Flush()
	return out.String()
}

// FuzzScanner drives the relay scanner with arbitrary bytes at arbitrary split
// points and asserts the two properties the design depends on:
//
//  1. Chunking invariance — the output must not depend on where the upstream
//     read boundaries happened to fall. This is the property revision 1's
//     first-frame byte patch did not have.
//  2. Byte fidelity when nothing is rewritten — the scanner is a relay, so with
//     no rewrite to apply the input must come out unchanged apart from the
//     re-framing of a frame that arrived without its delimiter
//     (COMPATIBILITY 1.5), which can add at most two bytes.
func FuzzScanner(f *testing.F) {
	f.Add([]byte(frames(chunkJSON("up", "a"))), uint8(3), "up", "down")
	f.Add([]byte(frames(chunkJSON("gemma4:31b", "hello"), chunkJSON("gemma4:31b", "!"))),
		uint8(1), "gemma4:31b", "qwen3.5:397b")
	f.Add([]byte("data: "+chunkJSON("up", "x")), uint8(7), "up", "zai:glm-5.1")
	f.Add([]byte(": comment\nevent: x\ndata: {\"model\":\"a\"}\n\n"), uint8(2), "a", "b")
	f.Add([]byte(`data: {"model":"a","choices":[{"delta":{"content":"\"model\":\"b\""}}]}`+"\n\n"),
		uint8(5), "a", "c")
	f.Add([]byte(`data: {"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`+"\n\n"),
		uint8(4), "", "x")
	f.Add([]byte{}, uint8(1), "", "")
	f.Add([]byte("\n\n\n"), uint8(1), "a", "b")
	f.Add([]byte(`data: {"model":"unterminated`), uint8(3), "a", "b")

	f.Fuzz(func(t *testing.T, body []byte, split uint8, from, to string) {
		opt := ScannerOptions{From: from, To: to, CollectUsage: true}
		n := int(split)%17 + 1

		whole := runScanner(body, opt, 0)
		chunked := runScanner(body, opt, n)
		if whole != chunked {
			t.Fatalf("output depends on read boundaries (split=%d)\n whole: %q\nsplit: %q", n, whole, chunked)
		}

		// With no rewrite to apply the relay must be byte-faithful.
		noRewrite := ScannerOptions{From: from, To: from, CollectUsage: true}
		got := runScanner(body, noRewrite, n)
		if !strings.HasPrefix(got, string(body)) {
			t.Fatalf("relay altered or dropped bytes\n got: %q\nwant prefix: %q", got, body)
		}
		if len(got) > len(body)+2 {
			t.Fatalf("relay added %d bytes; re-framing may add at most 2", len(got)-len(body))
		}
		if len(body) == 0 && got != "" {
			t.Fatalf("an empty stream produced %q", got)
		}
	})
}

// FuzzChunkDecode checks that decoding an arbitrary frame never panics and that
// anything that decodes re-encodes into the closed finish_reason enumeration.
func FuzzChunkDecode(f *testing.F) {
	f.Add([]byte(chunkJSON("gemma4:31b", "hi")))
	f.Add([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"WHAT_IS_THIS"}]}`))
	f.Add([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0}]}}]}`))
	f.Add([]byte(`{"usage":{"prompt_tokens":1}}`))

	allowed := map[string]bool{
		FinishStop: true, FinishLength: true, FinishToolCalls: true,
		FinishContentFilter: true, FinishFunctionCall: true,
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, events, err := DecodeChunk(raw, nil)
		if err != nil {
			return
		}
		var out bytes.Buffer
		s := NewStreamWriter(&out, StreamConfig{ID: testID, Created: testCreated, Model: testModel})
		for _, ev := range events {
			if ev.Type == canonical.EventError {
				continue
			}
			if err := s.WriteEvent(ev); err != nil {
				t.Fatalf("re-encode failed: %v", err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(out.String(), "\n") {
			i := strings.Index(line, `"finish_reason":"`)
			if i < 0 {
				continue
			}
			rest := line[i+len(`"finish_reason":"`):]
			j := strings.IndexByte(rest, '"')
			if j < 0 {
				t.Fatalf("malformed finish_reason in %q", line)
			}
			if !allowed[rest[:j]] {
				t.Fatalf("finish_reason %q escaped the enumeration (COMPATIBILITY 4.2)", rest[:j])
			}
		}
	})
}
