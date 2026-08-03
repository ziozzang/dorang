package openai

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

func chunkLine(model, text string) string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"` +
		model + `","choices":[{"index":0,"delta":{"content":"` + text + `"}}]}` + "\n\n"
}

// The streaming witness. On the aliasing path the scanner REWRITES the model
// field, so the upstream's own answer survives nowhere else; on the pass-name
// path it is not rewritten and nothing else looks at it either. Both must be
// read.
func TestScannerReadsTheUpstreamModel(t *testing.T) {
	cases := []struct {
		name         string
		from, to     string
		served       string
		wantAgree    canonical.ModelAgreement
		wantName     string
		wantRewrites bool
	}{
		{
			name: "substituted while rewriting", from: "glm-5.1", to: "m1",
			served: "glm-5.2", wantAgree: canonical.ModelSubstituted,
			wantName: "glm-5.2", wantRewrites: true,
		},
		{
			// The client-facing name and the upstream name are the same string,
			// so nothing is rewritten — and the reading still has to happen,
			// because this is the configuration in which the substitution
			// reaches the CLIENT unaltered.
			name: "substituted without rewriting", from: "glm-5.1", to: "glm-5.1",
			served: "glm-5.2", wantAgree: canonical.ModelSubstituted,
			wantName: "glm-5.2",
		},
		{
			name: "echo", from: "glm-5.2", to: "m1", served: "glm-5.2",
			wantAgree: canonical.ModelEcho, wantName: "glm-5.2", wantRewrites: true,
		},
		{
			name: "refinement", from: "gpt-4o", to: "m1", served: "gpt-4o-2024-08-06",
			wantAgree: canonical.ModelRefined, wantName: "gpt-4o-2024-08-06", wantRewrites: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			s := NewScanner(&out, ScannerOptions{From: c.from, To: c.to, CollectUsage: true})
			body := chunkLine(c.served, "he") + chunkLine(c.served, "llo") + "data: [DONE]\n\n"
			if _, err := io.Copy(s, strings.NewReader(body)); err != nil {
				t.Fatalf("Copy: %v", err)
			}
			if err := s.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			if got := s.ModelAgreement(); got != c.wantAgree {
				t.Errorf("ModelAgreement = %v, want %v", got, c.wantAgree)
			}
			if got := s.ServedModel(); got != c.wantName {
				t.Errorf("ServedModel = %q, want %q", got, c.wantName)
			}
			if c.wantRewrites && strings.Contains(out.String(), c.served) {
				t.Errorf("the relayed stream still names %q; COMPATIBILITY 2.5 requires the "+
					"client-facing name on every chunk", c.served)
			}
		})
	}
}

// A stream that never names a model is unobserved, not a substitution. The
// absence of B is not a disagreement with A.
func TestScannerWithoutAModelFieldIsUnobserved(t *testing.T) {
	var out bytes.Buffer
	s := NewScanner(&out, ScannerOptions{From: "glm-5.1", To: "m1", CollectUsage: true})
	body := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	if _, err := io.Copy(s, strings.NewReader(body)); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := s.ModelAgreement(); got != canonical.ModelUnobserved {
		t.Errorf("ModelAgreement = %v, want unobserved", got)
	}
	if got := s.ServedModel(); got != "" {
		t.Errorf("ServedModel = %q, want empty", got)
	}
}

// A name carrying a JSON escape is reported unobserved rather than compared
// unescaped: the scanner deliberately does not decode a frame, and a name that
// needs an escape cannot be compared byte-wise against a configuration string
// that does not.
func TestScannerDoesNotGuessAtAnEscapedName(t *testing.T) {
	var out bytes.Buffer
	s := NewScanner(&out, ScannerOptions{From: "glm-5.1", To: "m1", CollectUsage: true})
	// The escaped spelling is the SAME name to a JSON parser. The scanner is
	// not one: it locates the value without decoding it, so the bytes it holds
	// are not the name and must not be compared as if they were.
	body := `data: {"model":"glm\u002d5.2","choices":[]}` + "\n\n"
	if _, err := io.Copy(s, strings.NewReader(body)); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	_ = s.Flush()
	if got := s.ModelAgreement(); got != canonical.ModelUnobserved {
		t.Errorf("ModelAgreement = %v, want unobserved for an escaped name", got)
	}
}

// A name longer than the recording bound is unobserved rather than truncated. A
// truncated name compares as a different model, and a control that invents a
// substitution is worse than one that misses it.
func TestScannerIgnoresAnOverlongName(t *testing.T) {
	var out bytes.Buffer
	s := NewScanner(&out, ScannerOptions{From: "glm-5.1", To: "m1", CollectUsage: true})
	long := strings.Repeat("x", canonical.MaxServedModelBytes+1)
	if _, err := io.Copy(s, strings.NewReader(chunkLine(long, "hi"))); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	_ = s.Flush()
	if got := s.ModelAgreement(); got != canonical.ModelUnobserved {
		t.Errorf("ModelAgreement = %v, want unobserved", got)
	}
}

// The relay is the per-token path (DESIGN §15). The reading is taken from the
// FIRST frame that carries a model field and never taken again, so a stream of
// ten thousand chunks pays for one comparison and no allocation.
//
// The scanner is constructed and warmed outside the measured closure, because
// the Scanner itself and its carry-over buffer are the per-STREAM allocations
// this path always had; what must stay at zero is the per-FRAME cost.
func TestScannerModelReadingDoesNotAllocatePerFrame(t *testing.T) {
	for _, c := range []struct{ name, from, to string }{
		{"rewriting", "glm-5.1", "m1"},
		{"not rewriting", "glm-5.1", "glm-5.1"},
		// The worst case for the reading: nothing ever names a model, so the
		// probe never stops looking. It still must not allocate.
		{"never named", "glm-5.1", "glm-5.1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			frame := []byte(chunkLine("glm-5.2", "token"))
			if c.name == "never named" {
				frame = []byte(`data: {"choices":[{"delta":{"content":"token"}}]}` + "\n\n")
			}
			s := NewScanner(io.Discard, ScannerOptions{From: c.from, To: c.to, CollectUsage: true})
			// Warm: the first frame settles the reading and grows nothing else.
			if _, err := s.Write(frame); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if n := testing.AllocsPerRun(2000, func() {
				if _, err := s.Write(frame); err != nil {
					t.Fatal(err)
				}
			}); n != 0 {
				t.Errorf("the relay allocated %v times per frame, want 0", n)
			}
		})
	}
}

// ServedModel is the one call that allocates, which is why it is a separate
// accessor: the caller reads it only when the verdict makes the name worth
// having, so an ordinary stream never pays for it.
func TestModelAgreementDoesNotAllocate(t *testing.T) {
	s := NewScanner(io.Discard, ScannerOptions{From: "glm-5.1", To: "m1", CollectUsage: true})
	if _, err := s.Write([]byte(chunkLine("glm-5.2", "hi"))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sink := canonical.ModelUnobserved
	if n := testing.AllocsPerRun(1000, func() { sink |= s.ModelAgreement() }); n != 0 {
		t.Errorf("ModelAgreement allocated %v times, want 0", n)
	}
	if !sink.Substituted() {
		t.Fatal("the warm-up did not record the substitution")
	}
}

func BenchmarkScannerRelayFrame(b *testing.B) {
	frame := []byte(chunkLine("glm-5.2", "token"))
	s := NewScanner(io.Discard, ScannerOptions{From: "glm-5.1", To: "m1", CollectUsage: true})
	_, _ = s.Write(frame)
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Write(frame)
	}
}
