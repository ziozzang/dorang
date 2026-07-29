package openai

import (
	"fmt"
	"strings"
	"testing"
)

// benchBodyDeep builds a request of roughly n bytes with the nesting an agentic
// client produces: array content with typed parts, a cache breakpoint, tool
// declarations and an assistant turn carrying tool calls. Where benchBody is
// three objects deep, this is six.
func benchBodyDeep(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"bench-model","stream":false,"max_tokens":256,` +
		`"tools":[{"type":"function","function":{"name":"lookup",` +
		`"description":"look a thing up","parameters":{"type":"object",` +
		`"properties":{"q":{"type":"string"}}}}}],"messages":[` +
		`{"role":"system","content":"You are a helpful assistant."}`)
	filler := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 4)
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, `,{"role":"user","content":[`+
			`{"type":"text","text":"%s %d"},`+
			`{"type":"text","text":"and a second part","cache_control":{"type":"ephemeral"}}]}`,
			filler, i)
		fmt.Fprintf(&b, `,{"role":"assistant","content":null,"tool_calls":[`+
			`{"id":"call_%d","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"%d\"}"}}]}`,
			i, i)
		fmt.Fprintf(&b, `,{"role":"tool","tool_call_id":"call_%d","content":"result %d"}`, i, i)
	}
	b.WriteString(`],"temperature":0.7,"top_p":0.95}`)
	return []byte(b.String())
}

// BenchmarkMarshalRequest is the encode half of what BenchmarkDecodeRequest
// measures: the neutral request that came off the wire, re-serialized as the
// bytes the upstream is sent.
//
// It is built from the SAME body the decode benchmark uses, decoded once, so
// the two figures are two halves of one request and can be added.
func BenchmarkMarshalRequest(b *testing.B) {
	for _, size := range []int{1 << 10, 32 << 10, 400 << 10} {
		body := benchBody(size)
		req, err := DecodeRequest(body)
		if err != nil {
			b.Fatal(err)
		}
		out, err := MarshalRequest(req, nil)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("%dKiB", len(out)>>10), func(b *testing.B) {
			b.SetBytes(int64(len(out)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := MarshalRequest(req, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkMarshalRequestDeep is the shape the nesting cost actually shows up
// in: array content with parts, tool calls and a cache breakpoint, which is
// what an agentic client sends. A flat string-content request is three levels
// deep; this one is six.
func BenchmarkMarshalRequestDeep(b *testing.B) {
	body := benchBodyDeep(4 << 10)
	req, err := DecodeRequest(body)
	if err != nil {
		b.Fatal(err)
	}
	out, err := MarshalRequest(req, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(out)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := MarshalRequest(req, nil); err != nil {
			b.Fatal(err)
		}
	}
}
