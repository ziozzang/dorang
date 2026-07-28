package openai

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ziozzang/dorang/internal/canonical"
)

// TestToolNameTruncationRoundTrips covers COMPATIBILITY 5.3.
func TestToolNameTruncationRoundTrips(t *testing.T) {
	long := "mcp__filesystem__read_text_file_with_a_very_long_qualified_namespace_and_more"
	if len(long) <= MaxToolNameLen {
		t.Fatalf("test fixture is only %d bytes", len(long))
	}
	names := NewToolNames()
	var warned []Warning
	short := names.Shorten(long, func(w Warning) { warned = append(warned, w) })

	if len(short) > MaxToolNameLen {
		t.Fatalf("shortened name is %d bytes, limit is %d", len(short), MaxToolNameLen)
	}
	if short == long {
		t.Fatal("name was not shortened")
	}
	if got := names.Restore(short); got != long {
		t.Fatalf("Restore(%q) = %q, want %q", short, got, long)
	}
	if !hasWarning(warned, WarnToolNameTruncated) {
		t.Errorf("truncation was not reported: %+v", warned)
	}
	// Stable: the same input yields the same short form.
	if again := names.Shorten(long, nil); again != short {
		t.Errorf("second Shorten gave %q, want %q", again, short)
	}
}

// TestToolNamesUnderLimitAreUntouched keeps the common case free.
func TestToolNamesUnderLimitAreUntouched(t *testing.T) {
	names := NewToolNames()
	for _, n := range []string{"f", "get_weather", strings.Repeat("a", MaxToolNameLen)} {
		if got := names.Shorten(n, nil); got != n {
			t.Errorf("Shorten(%q) = %q", n, got)
		}
	}
	if names.Len() != 0 {
		t.Errorf("mapping recorded %d entries for names that fit", names.Len())
	}
}

// TestToolNameSharedPrefixDoesNotCollide is the reason the short form ends in a
// hash of the FULL name rather than being a plain truncation.
func TestToolNameSharedPrefixDoesNotCollide(t *testing.T) {
	prefix := strings.Repeat("x", 70)
	a, b := prefix+"__alpha", prefix+"__beta"
	names := NewToolNames()
	sa, sb := names.Shorten(a, nil), names.Shorten(b, nil)
	if sa == sb {
		t.Fatalf("two distinct tools collapsed onto %q", sa)
	}
	if names.Restore(sa) != a || names.Restore(sb) != b {
		t.Fatal("restore crossed the two names")
	}
}

// TestToolNameTruncationIsUTF8Safe guards against splitting a rune.
func TestToolNameTruncationIsUTF8Safe(t *testing.T) {
	name := strings.Repeat("한", 40) // 3 bytes each
	names := NewToolNames()
	short := names.Shorten(name, nil)
	if len(short) > MaxToolNameLen {
		t.Fatalf("shortened to %d bytes", len(short))
	}
	if !utf8.ValidString(short) {
		t.Fatalf("shortening produced invalid UTF-8: %q", short)
	}
	if names.Restore(short) != name {
		t.Error("restore failed for a multi-byte name")
	}
}

// TestToolNameRestoredOnTheWayBack is the end-to-end path: a long name is
// shortened for the upstream and restored in the stream the client sees.
func TestToolNameRestoredOnTheWayBack(t *testing.T) {
	long := "mcp__github__create_pull_request_review_comment_on_a_specific_line_number"
	req := &canonical.Request{
		Model:    "qwen3.5:397b",
		Messages: []canonical.Message{canonical.TextMessage(canonical.RoleUser, "go")},
		Tools:    []canonical.Tool{{Name: long}},
	}
	opt := &EncodeOptions{}
	w, err := EncodeRequest(req, opt)
	if err != nil {
		t.Fatal(err)
	}
	short := w.Tools[0].Function.Name
	if short == long {
		t.Fatal("the over-long tool name reached the upstream unchanged")
	}

	// The upstream now calls the SHORT name; the client must see the long one.
	var buf strings.Builder
	s := NewStreamWriter(&buf, StreamConfig{
		ID: testID, Created: testCreated, Model: "qwen3.5:397b", ToolNames: opt.ToolNames,
	})
	if err := s.WriteChunk(&Chunk{Choices: []ChunkChoice{{
		Index: 0,
		Delta: Delta{ToolCalls: []ToolCallDelta{{
			Index: 0, ID: ptr("call_1"), Type: ptr("function"),
			Function: &FunctionDelta{Name: &short},
		}}},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"name":"`+long+`"`) {
		t.Fatalf("client saw a name it never declared:\n%s", buf.String())
	}
}

// TestNilToolNamesDoesNotShorten: without a mapping to record into, shortening
// would produce a name that can never be restored.
func TestNilToolNamesDoesNotShorten(t *testing.T) {
	var names *ToolNames
	long := strings.Repeat("z", 100)
	if got := names.Shorten(long, nil); got != long {
		t.Errorf("nil mapping shortened irreversibly to %q", got)
	}
	if got := names.Restore("anything"); got != "anything" {
		t.Errorf("nil mapping Restore = %q", got)
	}
}
