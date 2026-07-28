package openai

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// benchBody builds a chat request of roughly n bytes, shaped like a real one:
// a long message array with one long string in it, which is where a request's
// bytes actually live and therefore where a scan pass costs what it costs.
func benchBody(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"bench-model","stream":false,"max_tokens":256,"messages":[`)
	b.WriteString(`{"role":"system","content":"You are a helpful assistant."}`)
	filler := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 8)
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, `,{"role":"user","content":"%s %d"}`, filler, i)
	}
	b.WriteString(`],"temperature":0.7,"top_p":0.95}`)
	return []byte(b.String())
}

// BenchmarkDecodeRequest measures the whole request decode at DESIGN §17's M3
// sizes. COMPATIBILITY 2.0's case-sensitive decode adds one structural scan of
// the body ahead of encoding/json's own; BenchmarkStrictFilter isolates that
// scan so the two numbers can be read against each other.
func BenchmarkDecodeRequest(b *testing.B) {
	for _, size := range []int{1 << 10, 32 << 10, 400 << 10} {
		body := benchBody(size)
		b.Run(fmt.Sprintf("%dKiB", len(body)>>10), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := DecodeRequest(body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkStrictFilter is the added cost on its own: the scan that finds no
// case-folded key and hands the original bytes straight back.
func BenchmarkStrictFilter(b *testing.B) {
	type alias Request
	for _, size := range []int{1 << 10, 32 << 10, 400 << 10} {
		body := benchBody(size)
		b.Run(fmt.Sprintf("%dKiB", len(body)>>10), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			var a alias
			for b.Loop() {
				if out := canonical.StrictBytes(body, &a); len(out) != len(body) {
					b.Fatal("filter rewrote a clean body")
				}
			}
		})
	}
}
