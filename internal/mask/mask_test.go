package mask

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const testSecret = "a cluster-wide secret, set by the operator"

func testFilter(t *testing.T, opts ...func(*Config)) *Filter {
	t.Helper()
	cfg := Config{
		Name:     "pii-mask",
		Patterns: []PatternSpec{{Name: "krrn"}, {Name: "email"}},
		Secret:   []byte(testSecret),
		Scope:    ScopeConversation,
	}
	for _, o := range opts {
		o(&cfg)
	}
	f, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

func session(t *testing.T, f *Filter, salt string) *Session {
	t.Helper()
	s, err := f.Session(SessionOptions{Salt: salt})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	return s
}

func mask(t *testing.T, s *Session, in string) string {
	t.Helper()
	out, _, err := s.Mask(in)
	if err != nil {
		t.Fatalf("Mask(%q): %v", in, err)
	}
	return out
}

// TestRoundTrip is the motivating case of DESIGN §10.5b: an identity number
// leaves the operator's control as a placeholder and comes back in the answer.
func TestRoundTrip(t *testing.T) {
	f := testFilter(t)
	s := session(t, f, "conv-1")

	const in = "고객 주민번호는 900101-1234567 이고 메일은 hong@example.com 입니다."
	masked := mask(t, s, in)

	if strings.Contains(masked, "900101-1234567") {
		t.Fatalf("the identity number survived masking: %q", masked)
	}
	if strings.Contains(masked, "hong@example.com") {
		t.Fatalf("the address survived masking: %q", masked)
	}
	if n := strings.Count(masked, Sentinel); n != 2 {
		t.Fatalf("want 2 placeholders, got %d: %q", n, masked)
	}

	// The model answers, quoting both placeholders back.
	answer := "확인했습니다. " + masked
	restored := s.Unmask(answer)
	if !strings.Contains(restored, "900101-1234567") || !strings.Contains(restored, "hong@example.com") {
		t.Fatalf("unmask did not restore: %q", restored)
	}
	if strings.Contains(restored, Sentinel) {
		t.Fatalf("a placeholder survived unmasking: %q", restored)
	}
	st := s.Stats()
	if st.Issued != 2 || st.Resolved != 2 || st.Unresolved != 0 {
		t.Fatalf("stats = %+v, want issued=2 resolved=2 unresolved=0", st)
	}
}

// TestPlaceholderShape pins the properties the rest of the design leans on.
func TestPlaceholderShape(t *testing.T) {
	f := testFilter(t)
	s := session(t, f, "conv-1")

	for i := 0; i < 500; i++ {
		out := mask(t, s, fmt.Sprintf("id 9001%02d-1%06d done", i%28+1, i))
		j := strings.Index(out, Sentinel)
		if j < 0 {
			t.Fatalf("no placeholder in %q", out)
		}
		ph := out[j : j+PlaceholderLen]
		if len(ph) != PlaceholderLen {
			t.Fatalf("placeholder %q is %d bytes, want %d", ph, len(ph), PlaceholderLen)
		}
		if !wellFormed(out, j) {
			t.Fatalf("placeholder %q is not well formed", ph)
		}
		if strings.ContainsAny(ph, "0123456789@") {
			// A placeholder that could match a digit-shaped or address-shaped
			// pattern would be masked again by a later pass, and one unmasking
			// pass would then restore a placeholder instead of a value.
			t.Fatalf("placeholder %q contains a digit or an @", ph)
		}
	}
}

// TestDeterminismAcrossFiltersAndTurns is the prefix-caching contract: the same
// text under the same scope must produce byte-identical output, from a filter
// built independently — a different node, a later process — and on a later turn
// of the same conversation.
func TestDeterminismAcrossFiltersAndTurns(t *testing.T) {
	const turn1 = "제 번호는 900101-1234567 입니다"
	const turn2 = turn1 + "\n다시 확인: 900101-1234567 와 a@b.com"

	f1 := testFilter(t)
	f2 := testFilter(t) // an independently constructed filter: another node

	a := mask(t, session(t, f1, "conv-1"), turn1)
	b := mask(t, session(t, f2, "conv-1"), turn1)
	if a != b {
		t.Fatalf("two filters masked the same text differently:\n%q\n%q", a, b)
	}

	// Turn 2 resends turn 1 and adds to it. The prefix must be byte-identical,
	// or the backend's KV cache misses on every turn of every masked
	// conversation.
	c := mask(t, session(t, f1, "conv-1"), turn2)
	if !strings.HasPrefix(c, a) {
		t.Fatalf("turn 2 did not extend turn 1:\n turn1=%q\n turn2=%q", a, c)
	}

	// A different conversation must not share placeholders, or the scope means
	// nothing.
	d := mask(t, session(t, f1, "conv-2"), turn1)
	if d == a {
		t.Fatalf("two conversations produced the same placeholder: %q", d)
	}

	// Neither must a different secret.
	f3 := testFilter(t, func(c *Config) { c.Secret = []byte("another cluster") })
	if e := mask(t, session(t, f3, "conv-1"), turn1); e == a {
		t.Fatalf("a different secret produced the same placeholder")
	}
	if f1.Version() == f3.Version() {
		t.Fatalf("Version did not change with the secret")
	}

	// Nor a different pattern set: it is part of the cache identity.
	f4 := testFilter(t, func(c *Config) { c.Patterns = []PatternSpec{{Name: "krrn"}} })
	if f1.Version() == f4.Version() {
		t.Fatalf("Version did not change with the pattern set")
	}
}

// TestScopeRequestIsUnlinkable checks the opposite end of the dial.
func TestScopeRequestIsUnlinkable(t *testing.T) {
	f := testFilter(t, func(c *Config) { c.Scope = ScopeRequest })
	const in = "900101-1234567"
	a := mask(t, session(t, f, "conv-1"), in)
	b := mask(t, session(t, f, "conv-1"), in)
	if a == b {
		t.Fatalf("scope=request produced a stable placeholder, so it links after all: %q", a)
	}
	if f.Scope().Deterministic() {
		t.Fatal("scope=request reports itself deterministic")
	}
}

// TestStreamingAcrossEveryBoundary feeds the answer through the streaming
// unmasker split at every possible offset, which is the only honest way to test
// "a placeholder can straddle a frame".
func TestStreamingAcrossEveryBoundary(t *testing.T) {
	f := testFilter(t)
	s := session(t, f, "conv-1")
	masked := mask(t, s, "id 900101-1234567 mail hong@example.com end")
	want := s.Unmask(masked)

	for cut := 0; cut <= len(masked); cut++ {
		s2 := session(t, f, "conv-1")
		if _, _, err := s2.Mask("id 900101-1234567 mail hong@example.com end"); err != nil {
			t.Fatal(err)
		}
		u := s2.Unmasker()
		var got strings.Builder
		got.WriteString(u.Write(masked[:cut]))
		if u.Held() > PlaceholderLen-1 {
			t.Fatalf("cut %d: held %d bytes, bound is %d", cut, u.Held(), PlaceholderLen-1)
		}
		got.WriteString(u.Write(masked[cut:]))
		got.WriteString(u.Flush())
		if got.String() != want {
			t.Fatalf("cut %d: got %q, want %q", cut, got.String(), want)
		}
	}
}

// TestStreamingByteAtATime is the same property under the worst possible
// framing.
func TestStreamingByteAtATime(t *testing.T) {
	f := testFilter(t)
	s := session(t, f, "conv-1")
	masked := mask(t, s, "a 900101-1234567 b")

	u := s.Unmasker()
	var got strings.Builder
	for i := 0; i < len(masked); i++ {
		got.WriteString(u.Write(masked[i : i+1]))
		if u.Held() > PlaceholderLen-1 {
			t.Fatalf("held %d bytes, bound is %d", u.Held(), PlaceholderLen-1)
		}
	}
	got.WriteString(u.Flush())
	if want := "a 900101-1234567 b"; got.String() != want {
		t.Fatalf("got %q, want %q", got.String(), want)
	}
}

// TestInventedPlaceholderIsLeftAloneAndCounted covers DESIGN §10.5b rule 3.
func TestInventedPlaceholderIsLeftAloneAndCounted(t *testing.T) {
	f := testFilter(t)
	s := session(t, f, "conv-1")
	masked := mask(t, s, "id 900101-1234567")
	real := masked[strings.Index(masked, Sentinel):]
	real = real[:PlaceholderLen]

	cases := []struct {
		name string
		text string
	}{
		{"invented", "[PII:aaaaaaaaaaaaaaaa]"},
		{"echoed inside a longer token", real[:PlaceholderLen-1] + "x]"},
		{"one character added", real[:PlaceholderLen-1] + "a]"},
		{"truncated", real[:PlaceholderLen-3] + "]"},
		{"translated", strings.ToUpper(real)},
		{"sentinel only", "[PII:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s2 := session(t, f, "conv-1")
			if _, _, err := s2.Mask("id 900101-1234567"); err != nil {
				t.Fatal(err)
			}
			before := s2.Stats().Unresolved
			out := s2.Unmask("the model says " + tc.text + " ok")
			if !strings.Contains(out, tc.text) {
				t.Fatalf("the text was rewritten: got %q, want it to contain %q", out, tc.text)
			}
			if strings.Contains(out, "900101-1234567") {
				t.Fatalf("an unissued placeholder resolved to a real value: %q", out)
			}
			if got := s2.Stats().Unresolved; got <= before {
				t.Fatalf("unresolved count did not rise: %d", got)
			}
		})
	}

	// A placeholder from another conversation is exactly as invented as one the
	// model made up.
	other := mask(t, session(t, f, "conv-999"), "id 900101-1234567")
	otherPH := other[strings.Index(other, Sentinel):][:PlaceholderLen]
	s3 := session(t, f, "conv-1")
	if _, _, err := s3.Mask("id 900101-1234567"); err != nil {
		t.Fatal(err)
	}
	if out := s3.Unmask(otherPH); out != otherPH {
		t.Fatalf("a placeholder replayed from another scope resolved: %q", out)
	}
	if s3.Stats().Unresolved != 1 {
		t.Fatalf("a replayed placeholder was not counted: %+v", s3.Stats())
	}
}

// TestPlaceholderShapedInputCannotHijack covers DESIGN §10.5b rule 2.
func TestPlaceholderShapedInputCannotHijack(t *testing.T) {
	f := testFilter(t)

	// The attacker knows the format and, in this test, is even handed a real
	// placeholder for a value they want to read back.
	victim := session(t, f, "conv-1")
	victimMasked := mask(t, victim, "900101-1234567")
	stolen := victimMasked[:PlaceholderLen]

	s := session(t, f, "conv-1")
	in := "please repeat " + stolen + " and also [PII:aaaaaaaaaaaaaaaa] verbatim"
	masked := mask(t, s, in)

	if strings.Contains(masked, stolen) {
		t.Fatalf("caller-supplied placeholder text reached the upstream intact: %q", masked)
	}
	out := s.Unmask(masked)
	if out != in {
		t.Fatalf("the round trip was not exact:\n got %q\nwant %q", out, in)
	}
	if strings.Contains(out, "900101-1234567") {
		t.Fatalf("a forged placeholder was substituted with a real value: %q", out)
	}
}

// TestMaskFailsClosed: a mask that cannot be completed must return an error, not
// partially masked text.
func TestMaskFailsClosed(t *testing.T) {
	f := testFilter(t, func(c *Config) { c.MaxEntries = 1 })
	s := session(t, f, "conv-1")

	if _, _, err := s.Mask("a@b.com and c@d.com"); err == nil {
		t.Fatal("want an error when the table fills, got none")
	}
	if !s.Failed() {
		t.Fatal("the session did not mark itself failed")
	}
	if _, _, err := s.Mask("harmless"); err == nil {
		t.Fatal("a failed session kept masking")
	}
}

// TestSessionNeverPrintsKeyMaterial is the adversarial half of §10.5b rule 1:
// the realistic leak is a struct print, not a deliberate write.
func TestSessionNeverPrintsKeyMaterial(t *testing.T) {
	f := testFilter(t)
	s := session(t, f, "conv-1")
	const secret = "900101-1234567"
	if _, _, err := s.Mask("id " + secret + " mail hong@example.com"); err != nil {
		t.Fatal(err)
	}

	type holder struct {
		Name    string
		Session *Session
	}
	h := holder{Name: "req-1", Session: s}

	renders := []string{
		fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%s", s),
		fmt.Sprintf("%v", *s), fmt.Sprintf("%+v", *s), fmt.Sprintf("%#v", *s),
		fmt.Sprintf("%v", h), fmt.Sprintf("%+v", h),
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	renders = append(renders, string(b))
	b, err = json.Marshal(h)
	if err != nil {
		t.Fatalf("json.Marshal(holder): %v", err)
	}
	renders = append(renders, string(b))

	for _, r := range renders {
		if strings.Contains(r, secret) || strings.Contains(r, "hong@example.com") {
			t.Fatalf("a rendering carried the original text: %q", r)
		}
	}
}

// TestVaultRestoresWhatDerivationCannot covers the direction an HMAC cannot go:
// a placeholder in the answer whose plaintext was not in the question, which is
// what happens when the conversation state lives on the provider's side.
func TestVaultRestoresWhatDerivationCannot(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	f := testFilter(t, func(c *Config) {
		c.Retain = 10 * time.Minute
		c.Now = clock
	})

	s1 := session(t, f, "conv-1")
	masked := mask(t, s1, "id 900101-1234567")
	ph := masked[strings.Index(masked, Sentinel):][:PlaceholderLen]

	// Turn 2 does not resend the history — the provider holds it — so this
	// session's own table is empty and only the vault can invert.
	s2 := session(t, f, "conv-1")
	if out := s2.Unmask("as discussed, " + ph); !strings.Contains(out, "900101-1234567") {
		t.Fatalf("the vault did not restore a provider-held placeholder: %q", out)
	}

	// A different conversation must not be able to read it, even though the
	// vault is shared. Anything the caller *sends* is escaped, so the attack is
	// to make the model emit a placeholder seen elsewhere — and the vault is
	// keyed by scope precisely so that does not resolve.
	other := session(t, f, "conv-2")
	if out := other.Unmask("the model emits " + ph); strings.Contains(out, "900101-1234567") {
		t.Fatalf("a placeholder resolved outside the scope that issued it: %q", out)
	}
	if other.Stats().Unresolved != 1 {
		t.Fatalf("the cross-scope attempt was not counted: %+v", other.Stats())
	}

	// Past the lifetime it is gone, and the placeholder comes back unresolved
	// and counted rather than silently wrong.
	now = now.Add(11 * time.Minute)
	s3 := session(t, f, "conv-1")
	if out := s3.Unmask(ph); out != ph {
		t.Fatalf("an expired mapping still resolved: %q", out)
	}
	if s3.Stats().Unresolved != 1 {
		t.Fatalf("an expired mapping was not counted: %+v", s3.Stats())
	}

	if got := fmt.Sprintf("%v %#v", f.Vault(), f.Vault()); strings.Contains(got, "900101") {
		t.Fatalf("the vault printed its contents: %q", got)
	}
	b, err := json.Marshal(f.Vault())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "900101") {
		t.Fatalf("the vault marshalled its contents: %s", b)
	}
}

func TestVaultEvictsOldestWhenFull(t *testing.T) {
	now := time.Now()
	f := testFilter(t, func(c *Config) {
		c.Retain = time.Hour
		c.MaxRetained = 4
		c.Now = func() time.Time { return now }
	})
	s := session(t, f, "conv-1")
	var first string
	for i := 0; i < 10; i++ {
		out := mask(t, s, fmt.Sprintf("mail user%d@example.com", i))
		if i == 0 {
			first = out[strings.Index(out, Sentinel):][:PlaceholderLen]
		}
	}
	if n := f.Vault().Len(); n > 4 {
		t.Fatalf("the vault grew past its cap: %d", n)
	}
	if v := f.Vault().Stats(); v.Evicted == 0 {
		t.Fatalf("nothing was evicted: %+v", v)
	}
	// The evicted one is gone, and a session that never masked it gets it back
	// unresolved rather than wrong.
	s2 := session(t, f, "conv-1")
	if out := s2.Unmask(first); out != first {
		t.Fatalf("an evicted mapping resolved: %q", out)
	}
}

func TestPatternSetRules(t *testing.T) {
	if _, err := New(Config{Name: "x", Secret: []byte("s")}); err == nil {
		t.Fatal("a filter with no patterns was accepted")
	}
	if _, err := New(Config{
		Name: "x", Secret: []byte("s"),
		Patterns: []PatternSpec{{Name: "loose", Regexp: `\d*`}},
	}); err == nil {
		t.Fatal("a pattern matching the empty string was accepted")
	}
	if _, err := New(Config{
		Name: "x", Secret: []byte("s"),
		Patterns: []PatternSpec{{Name: "nope"}},
	}); err == nil {
		t.Fatal("an unknown built-in was accepted")
	}
	if _, err := New(Config{
		Name: "x", Secret: []byte("s"),
		Patterns: []PatternSpec{{Name: "krrn"}, {Name: "krrn"}},
	}); err == nil {
		t.Fatal("a duplicate pattern name was accepted")
	}
	if _, err := New(Config{
		Name: "x", Scope: ScopeConversation,
		Patterns: []PatternSpec{{Name: "krrn"}},
	}); err == nil {
		t.Fatal("a deterministic scope with no secret was accepted")
	}
	// scope=request needs no secret: it derives from fresh randomness.
	if _, err := New(Config{
		Name: "x", Scope: ScopeRequest,
		Patterns: []PatternSpec{{Name: "krrn"}},
	}); err != nil {
		t.Fatalf("scope=request rejected without a secret: %v", err)
	}
}

// TestKRRNPattern documents what the built-in does and does not match.
func TestKRRNPattern(t *testing.T) {
	f := testFilter(t, func(c *Config) { c.Patterns = []PatternSpec{{Name: "krrn"}} })
	s := session(t, f, "c")
	masked := []string{
		"900101-1234567", // hyphenated
		"9001011234567",  // bare
		"001231-3456789", // 2000s
		"991231-8123456", // foreign resident
	}
	for _, v := range masked {
		if out := mask(t, s, "x "+v+" y"); strings.Contains(out, v) {
			t.Errorf("%q was not masked: %q", v, out)
		}
	}
	kept := []string{
		"900101-9234567", // gender digit out of range
		"901301-1234567", // month 13
		"900132-1234567", // day 32
		"90010-11234567", // wrong shape
	}
	for _, v := range kept {
		if out := mask(t, s, "x "+v+" y"); !strings.Contains(out, v) {
			t.Errorf("%q was masked and should not have been: %q", v, out)
		}
	}
}

// TestOperatorPatternsAreTheOperators: a jurisdiction dorang has never heard of.
func TestOperatorPatterns(t *testing.T) {
	f := testFilter(t, func(c *Config) {
		c.Patterns = []PatternSpec{{Name: "employee", Regexp: `EMP-\d{6}`}}
	})
	s := session(t, f, "c")
	out := mask(t, s, "ticket for EMP-004211 please")
	if strings.Contains(out, "EMP-004211") {
		t.Fatalf("an operator pattern did not apply: %q", out)
	}
	if got := s.Unmask(out); got != "ticket for EMP-004211 please" {
		t.Fatalf("round trip: %q", got)
	}
	if p := s.Stats().Patterns; len(p) != 1 || p[0].Name != "employee" || p[0].Count != 1 {
		t.Fatalf("per-pattern counts: %+v", p)
	}
}

// TestOverlappingPatternsMaskTheLongerValue: overlap resolves leftmost-longest
// over the combined expression, so a narrow pattern cannot leave the tail of a
// wider one in the request — and the answer does not depend on the order the
// operator happened to list them in.
func TestOverlappingPatternsMaskTheLongerValue(t *testing.T) {
	orders := [][]PatternSpec{
		{{Name: "six", Regexp: `\d{6}`}, {Name: "krrn"}},
		{{Name: "krrn"}, {Name: "six", Regexp: `\d{6}`}},
	}
	var first string
	for i, specs := range orders {
		f := testFilter(t, func(c *Config) { c.Patterns = specs })
		out := mask(t, session(t, f, "c"), "x 900101-1234567 y")
		if strings.Contains(out, "1234567") {
			t.Fatalf("order %d left the tail of the number in the request: %q", i, out)
		}
		if i == 0 {
			first = out
		} else if out != first {
			t.Fatalf("pattern order changed the result:\n%q\n%q", first, out)
		}
	}
}

// TestMaskIsByteStableAcrossRepetition is the property jikji's
// internal/compositor/chat_proof.go pins with a 200-trial regression test: any
// map iteration or ordering that leaked into the output would show up as a
// different byte string on some run, because Go randomises map order per
// process and per iteration.
func TestMaskIsByteStableAcrossRepetition(t *testing.T) {
	var in strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&in, "user%d@example.com and 9001%02d-1%06d; ", i, i%28+1, i)
	}
	f := testFilter(t)
	want := mask(t, session(t, f, "conv-1"), in.String())
	for trial := 0; trial < 200; trial++ {
		f2 := testFilter(t)
		if got := mask(t, session(t, f2, "conv-1"), in.String()); got != want {
			t.Fatalf("trial %d differed", trial)
		}
	}
}

// TestBroadOperatorPatternCannotEatAPlaceholder: an operator pattern loose
// enough to match a placeholder must not mask one, or a single unmasking pass
// would restore a placeholder instead of a value.
func TestBroadOperatorPatternCannotEatAPlaceholder(t *testing.T) {
	f := testFilter(t, func(c *Config) {
		c.Patterns = []PatternSpec{{Name: "krrn"}, {Name: "letters", Regexp: `[a-p]{8,}`}}
	})
	s := session(t, f, "c")
	in := "id 900101-1234567 and the word mailbag"
	out := mask(t, s, in)
	if got := s.Unmask(out); got != in {
		t.Fatalf("round trip through a placeholder-eating pattern:\n got %q\nwant %q", got, in)
	}
}
