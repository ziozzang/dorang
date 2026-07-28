package luaext

import (
	"strings"
	"testing"
)

// evalRequest compiles a one-hook program and runs it against a view under a
// generous budget, returning what it decided.
func evalRequest(t *testing.T, src string, v *RequestView) result {
	t.Helper()
	p, err := Compile("test.policy", HookRequest, []byte(src))
	if err != nil {
		t.Fatalf("compile %q: %v", src, err)
	}
	b := newBudget(Limits{Instructions: 100000, MemoryBytes: 1 << 20})
	var res result
	if _, err := p.run(v, &b, &res); err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	return res
}

func TestPolicyExpressions(t *testing.T) {
	v := &RequestView{
		Model:       "gpt-4-turbo",
		KeyID:       "k-1",
		TeamID:      "eng",
		Path:        "/v1/chat/completions",
		InputTokens: 1200,
		BodyBytes:   4096,
		Stream:      true,
	}
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{`model == "gpt-4-turbo"`, true},
		{`model != "gpt-4-turbo"`, false},
		{`model startswith "gpt-4"`, true},
		{`model endswith "turbo"`, true},
		{`model contains "4-tur"`, true},
		{`model contains "claude"`, false},
		{`team_id in ["eng", "ops"]`, true},
		{`team_id in ["sales"]`, false},
		{`team_id in []`, false},
		{`input_tokens > 1000`, true},
		{`input_tokens >= 1200`, true},
		{`input_tokens < 1000`, false},
		{`input_tokens <= 1200`, true},
		{`body_bytes == 4096`, true},
		{`stream == true`, true},
		{`stream != false`, true},
		{`not stream`, false},
		{`stream and input_tokens > 1000`, true},
		{`stream and input_tokens > 100000`, false},
		{`stream or input_tokens > 100000`, true},
		{`not stream or team_id == "eng"`, true},
		{`(model == "x" or team_id == "eng") and stream`, true},
		{`key_id < "k-2"`, true},
		{`path startswith "/v1/"`, true},
		{`input_tokens > -1`, true},
	} {
		res := evalRequest(t, `deny "d" if `+tc.expr+"\n", v)
		if res.denied != tc.want {
			t.Errorf("%s => %v, want %v", tc.expr, res.denied, tc.want)
		}
	}
}

func TestPolicyShortCircuit(t *testing.T) {
	// `or` must not evaluate its right side once the left is true, and `and`
	// must not evaluate its right once the left is false. The observable proof
	// is the instruction count.
	for _, tc := range []struct {
		expr  string
		model string
	}{
		{`model == "a" or model == "b" or model == "c"`, "a"},
		{`model == "a" and model == "b" and model == "c"`, "z"},
	} {
		p, err := Compile("t.policy", HookRequest, []byte(`deny if `+tc.expr+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		b := newBudget(Limits{Instructions: 100000, MemoryBytes: 1 << 20})
		var res result
		if _, err := p.run(&RequestView{Model: tc.model}, &b, &res); err != nil {
			t.Fatal(err)
		}
		used := int64(100000) - b.instrLeft
		if used > 8 {
			t.Errorf("%s used %d instructions; the operator was not short-circuited", tc.expr, used)
		}
	}
}

func TestPolicyRuleOrderAndAccumulation(t *testing.T) {
	src := `
# comments and blank lines are fine
set tier = "bulk" if input_tokens > 1000
set region = "eu"
-- a Lua-style comment is accepted too
deny "too big" if body_bytes > 1000000
`
	res := evalRequest(t, src, &RequestView{InputTokens: 2000, BodyBytes: 10})
	if res.denied {
		t.Fatal("no rule matched the deny")
	}
	if v, ok := res.tags.Tag("tier"); !ok || v != "bulk" {
		t.Errorf("tier = %q/%v", v, ok)
	}
	if v, ok := res.tags.Tag("region"); !ok || v != "eu" {
		t.Errorf("region = %q/%v", v, ok)
	}

	res = evalRequest(t, src, &RequestView{InputTokens: 1, BodyBytes: 2000000})
	if !res.denied || res.reason != "too big" {
		t.Errorf("expected the deny to fire, got %+v", res)
	}
}

func TestPolicyAllowStopsEvaluation(t *testing.T) {
	src := `allow if key_id == "k-vip"` + "\n" + `deny "everyone else" ` + "\n"
	if res := evalRequest(t, src, &RequestView{KeyID: "k-vip"}); res.denied || !res.allowed {
		t.Errorf("the allow must short-circuit the deny: %+v", res)
	}
	if res := evalRequest(t, src, &RequestView{KeyID: "k-other"}); !res.denied {
		t.Error("the unconditional deny must fire for everyone else")
	}
}

func TestPolicySetValueKinds(t *testing.T) {
	res := evalRequest(t, "set a = \"s\"\nset b = 42\nset c = true\n", &RequestView{})
	for _, tc := range []struct{ name, want string }{{"a", "s"}, {"b", "42"}, {"c", "true"}} {
		if v, ok := res.tags.Tag(tc.name); !ok || v != tc.want {
			t.Errorf("%s = %q/%v, want %q", tc.name, v, ok, tc.want)
		}
	}
}

func TestPolicyTagsAreBounded(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < maxTags+5; i++ {
		sb.WriteString("set t")
		sb.WriteByte(byte('a' + i))
		sb.WriteString(" = \"v\"\n")
	}
	res := evalRequest(t, sb.String(), &RequestView{})
	if got := len(res.tags.Tags()); got != maxTags {
		t.Errorf("tags = %d, want the fixed ceiling %d", got, maxTags)
	}
}

func TestPolicySetOverwritesTheSameName(t *testing.T) {
	res := evalRequest(t, "set t = \"one\"\nset t = \"two\"\n", &RequestView{})
	if v, _ := res.tags.Tag("t"); v != "two" {
		t.Errorf("t = %q, want two", v)
	}
	if len(res.tags.Tags()) != 1 {
		t.Errorf("setting the same name twice should not add a slot")
	}
}

// --- compile-time refusals --------------------------------------------------

func TestCompileRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook Hook
		src  string
		want string
	}{
		{"unknown field", HookRequest, `deny if authorization == "x"` + "\n", "no field"},
		{"secret-ish field", HookRequest, `deny if api_key == "x"` + "\n", "no field"},
		{"body field", HookRequest, `deny if body == "x"` + "\n", "no field"},
		{"header field", HookRequest, `deny if headers == "x"` + "\n", "no field"},
		{"unknown verb", HookRequest, `log "hello"` + "\n", "not a rule verb"},
		{"deny in on_response", HookResponse, `deny "no"` + "\n", "is not available in"},
		{"exclude is not a verb", HookRequest, `exclude "p"` + "\n", "not a rule verb"},
		{"mismatched comparison", HookRequest, `deny if model == 5` + "\n", "never equal"},
		{"ordering on bools", HookRequest, `deny if stream > stream` + "\n", "two numbers or two strings"},
		{"in against a string", HookRequest, `deny if model in "abc"` + "\n", "compares a string against a list"},
		{"contains a number", HookRequest, `deny if model contains 5` + "\n", "compares two strings"},
		{"non-condition guard", HookRequest, `deny if model` + "\n", "must be a condition"},
		{"and on non-conditions", HookRequest, `deny if model and stream` + "\n", "needs conditions"},
		{"or on non-conditions", HookRequest, `deny if stream or model` + "\n", "needs conditions"},
		{"not on a string", HookRequest, `deny if not model` + "\n", "needs a condition"},
		{"two rules on a line", HookRequest, `allow allow` + "\n", "one rule per line"},
		{"empty file", HookRequest, "\n# nothing\n", "no rules"},
		{"unterminated string", HookRequest, `deny "oops` + "\n", "unterminated string"},
		{"bad escape", HookRequest, `deny "a\qb"` + "\n", "unknown escape"},
		{"missing paren", HookRequest, `deny if (model == "a"` + "\n", "expected )"},
		{"set without value", HookRequest, `set t =` + "\n", "expected a string"},
		{"set without name", HookRequest, `set = "x"` + "\n", "expected identifier"},
		{"float", HookRequest, `deny if input_tokens > 1.5` + "\n", "unexpected character"},
		{"stray character", HookRequest, `deny if model == @` + "\n", "unexpected character"},
		{"bang alone", HookRequest, `deny if model ! "x"` + "\n", "did you mean"},
		{"list of numbers", HookRequest, `deny if model in [1]` + "\n", "expected string"},
	} {
		_, err := Compile("t.policy", tc.hook, []byte(tc.src))
		if err == nil {
			t.Errorf("%s: expected a compile error for %q", tc.name, tc.src)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
}

func TestCompileRejectsOversizedSource(t *testing.T) {
	src := make([]byte, maxSourceBytes+1)
	for i := range src {
		src[i] = '\n'
	}
	if _, err := Compile("big.policy", HookRequest, src); err == nil ||
		!strings.Contains(err.Error(), "ceiling") {
		t.Errorf("an oversized policy file must be refused: %v", err)
	}
}

func TestCompileRejectsLongTagName(t *testing.T) {
	name := strings.Repeat("x", maxTagNameLen+1)
	if _, err := Compile("t.policy", HookRequest, []byte("set "+name+" = \"v\"\n")); err == nil {
		t.Error("an over-long tag name must be refused")
	}
}

func TestCompileRejectsOversizedList(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`deny if model in [`)
	for i := 0; i <= maxListElements; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`"x"`)
	}
	sb.WriteString("]\n")
	if _, err := Compile("t.policy", HookRequest, []byte(sb.String())); err == nil ||
		!strings.Contains(err.Error(), "at most") {
		t.Errorf("an oversized list must be refused: %v", err)
	}
}

func TestCompileRejectsUnknownHook(t *testing.T) {
	if _, err := Compile("t.policy", Hook(9), []byte("allow\n")); err == nil {
		t.Error("an unknown hook must be refused")
	}
}

// TestEveryHookCompilesItsOwnVerbs is the load-time half of the safety story:
// a verb is either meaningful for the hook or a refusal, never a no-op.
func TestEveryHookCompilesItsOwnVerbs(t *testing.T) {
	src := map[Hook]string{
		HookRequest:  "deny \"x\"\n",
		HookRoute:    "deny \"x\"\n",
		HookResponse: "set t = \"x\"\n",
		HookEmail:    "deny \"x\"\n",
	}
	for h, s := range src {
		if _, err := Compile("t.policy", h, []byte(s)); err != nil {
			t.Errorf("%s: %v", h, err)
		}
	}
}

// --- ceilings at the program level ------------------------------------------

func TestProgramInstructionCeiling(t *testing.T) {
	p, err := Compile("t.policy", HookRequest, []byte(strings.Repeat("set a = \"b\"\n", 20)))
	if err != nil {
		t.Fatal(err)
	}
	b := newBudget(Limits{Instructions: 5, MemoryBytes: 1 << 20})
	var res result
	if _, err := p.run(&RequestView{}, &b, &res); err != ErrInstructionLimit {
		t.Fatalf("err = %v, want ErrInstructionLimit", err)
	}
}

func TestProgramMemoryCeiling(t *testing.T) {
	p, err := Compile("t.policy", HookRequest, []byte(`deny if model == "x"`+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	b := newBudget(Limits{Instructions: 1000, MemoryBytes: 4})
	var res result
	if _, err := p.run(&RequestView{Model: "x"}, &b, &res); err != ErrMemoryLimit {
		t.Fatalf("err = %v, want ErrMemoryLimit", err)
	}
}

func TestUnlimitedBudget(t *testing.T) {
	p, err := Compile("t.policy", HookRequest, []byte(`deny if model == "x"`+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	b := newBudget(Limits{}) // both ceilings off
	var res result
	if _, err := p.run(&RequestView{Model: "x"}, &b, &res); err != nil {
		t.Fatalf("an unlimited budget must not fail: %v", err)
	}
	if !res.denied {
		t.Error("the rule should have matched")
	}
}

// TestPolicyTerminates is the argument for the smaller language stated as a
// test: there is no syntax that can loop, so the instruction count of any
// program is bounded by the program itself.
func TestPolicyTerminates(t *testing.T) {
	for _, src := range []string{
		"while true do end\n",
		"for i = 1, 10 do end\n",
		"function f() end\n",
		"goto top\n",
		"repeat until false\n",
	} {
		if _, err := Compile("t.policy", HookRequest, []byte(src)); err == nil {
			t.Errorf("%q compiled; the language must have no way to loop", src)
		}
	}
}

func TestProgramAccessors(t *testing.T) {
	p, err := Compile("t.policy", HookRequest, []byte("allow\nset a = \"b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Rules() != 2 {
		t.Errorf("Rules() = %d, want 2", p.Rules())
	}
	if p.Name != "t.policy" || p.Hook != HookRequest {
		t.Errorf("program identity = %q/%s", p.Name, p.Hook)
	}
}

func TestKindAndValueRendering(t *testing.T) {
	for _, tc := range []struct {
		v    value
		want string
	}{
		{strValue("x"), "x"},
		{numValue(-3), "-3"},
		{boolValue(true), "true"},
		{boolValue(false), "false"},
		{listValue([]string{"a"}), ""},
	} {
		if got := tc.v.String(); got != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.v.k, got, tc.want)
		}
	}
	for k, want := range map[kind]string{
		kindNone: "none", kindString: "string", kindNumber: "number",
		kindBool: "bool", kindList: "list",
	} {
		if k.String() != want {
			t.Errorf("kind %d = %q, want %q", k, k.String(), want)
		}
	}
}

func TestEscapesInStrings(t *testing.T) {
	res := evalRequest(t, "set t = \"a\\tb\\nc\\\\d\\\"e\"\n", &RequestView{})
	if v, _ := res.tags.Tag("t"); v != "a\tb\nc\\d\"e" {
		t.Errorf("escapes = %q", v)
	}
}
