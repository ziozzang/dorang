package luaext

import (
	"errors"
	"fmt"
)

// The policy language.
//
//	# a comment; -- is accepted too
//	deny "gpt-4 is not available to this team" if model == "gpt-4" and team_id != "eng"
//	deny if body_bytes > 1048576
//	set tier = "bulk" if input_tokens > 100000
//	allow if key_id == "k-privileged"
//
// One rule per line: an action, then an optional `if` guard. Rules are
// evaluated top to bottom; `deny` and `allow` stop evaluation and `set`
// accumulates.
//
// The grammar has no loops, no function calls, no assignment to a name a later
// rule can read, and no way to build a string. That is not an accident of
// scope — it is the property that makes the ceilings §11.5 asks for a backstop
// rather than the only line of defence. A program in this language terminates
// after at most one pass over its own rules, and its peak live data is bounded
// by its own source.

type action uint8

const (
	actNone action = iota
	// actDeny refuses (on_request) or suppresses (on_email).
	actDeny
	// actAllow stops evaluation and permits.
	actAllow
	// actSet annotates.
	actSet
)

func (a action) String() string {
	switch a {
	case actDeny:
		return "deny"
	case actAllow:
		return "allow"
	case actSet:
		return "set"
	}
	return "?"
}

// legalActions says which verbs each hook understands. A verb used on the wrong
// hook is a load error rather than a silent no-op: "deny" in on_response reads
// like a refusal and is not one, and finding that out from production traffic
// is the expensive way.
func legalActions(h Hook) []action {
	switch h {
	case HookRequest:
		return []action{actDeny, actAllow, actSet}
	case HookRoute:
		return []action{actDeny, actAllow, actSet}
	case HookResponse:
		return []action{actSet}
	case HookEmail:
		return []action{actDeny, actAllow, actSet}
	}
	return nil
}

func actionLegal(h Hook, a action) bool {
	for _, x := range legalActions(h) {
		if x == a {
			return true
		}
	}
	return false
}

type opcode uint8

const (
	opConst opcode = iota
	opField
	opEq
	opNe
	opLt
	opLe
	opGt
	opGe
	opContains
	opStartsWith
	opEndsWith
	opIn
	opNot
	opJumpIfFalse // peek: jump when the top of the stack is false
	opJumpIfTrue  // peek: jump when the top of the stack is true
	opPop
)

type instr struct {
	op opcode
	a  int32
}

type rule struct {
	act  action
	name string // set: the tag name
	arg  value  // deny: the reason; set: the value
	cond []instr
	line int
}

// Program is one compiled policy file.
//
// It is immutable after [Compile] and safe to share across goroutines:
// evaluation writes only to a caller-owned machine.
type Program struct {
	// Name is the file the program came from, used in warnings.
	Name string
	// Hook is the point it is registered at.
	Hook Hook

	rules  []rule
	consts []value
	depth  int
}

// Rules reports how many rules the program has. Callers use it for logging; it
// is also the bound on how much work one invocation of the program can be.
func (p *Program) Rules() int { return len(p.rules) }

// maxStack bounds an expression's operand depth. Nothing readable comes close;
// the cap exists so the evaluator can use a fixed-size stack and never grow.
const maxStack = 16

// maxListElements bounds an `in` list.
const maxListElements = 64

// maxTagNameLen bounds a `set` name.
const maxTagNameLen = 32

// Compile turns policy source into a program for one hook.
func Compile(name string, h Hook, src []byte) (*Program, error) {
	if len(src) > maxSourceBytes {
		return nil, fmt.Errorf("luaext: %s: policy source is %d bytes, over the %d-byte ceiling",
			name, len(src), maxSourceBytes)
	}
	if int(h) >= numHooks {
		return nil, fmt.Errorf("luaext: %s: unknown hook", name)
	}
	toks, err := newLexer(string(src)).lex()
	if err != nil {
		return nil, fmt.Errorf("luaext: %s: %w", name, err)
	}
	p := &parser{
		name:   name,
		hook:   h,
		toks:   toks,
		fields: fieldsFor(h),
		prog:   &Program{Name: name, Hook: h},
	}
	if err := p.parseProgram(); err != nil {
		return nil, fmt.Errorf("luaext: %s: %w", name, err)
	}
	if p.prog.depth > maxStack {
		return nil, fmt.Errorf("luaext: %s: an expression needs %d operand slots, over the %d-slot ceiling",
			name, p.prog.depth, maxStack)
	}
	return p.prog, nil
}

type parser struct {
	name   string
	hook   Hook
	toks   []token
	pos    int
	fields fieldTable
	prog   *Program

	// per-expression state
	code     []instr
	depth    int
	maxDepth int
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) advance() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *parser) errf(t token, format string, args ...any) error {
	return fmt.Errorf("line %d: "+format, append([]any{t.line}, args...)...)
}

func (p *parser) expect(k tokKind) (token, error) {
	t := p.peek()
	if t.kind != k {
		return t, p.errf(t, "expected %s, found %s", k, describe(t))
	}
	return p.advance(), nil
}

func describe(t token) string {
	switch t.kind {
	case tokIdent, tokString, tokNumber:
		return fmt.Sprintf("%s %q", t.kind, t.text)
	}
	return t.kind.String()
}

func (p *parser) skipNewlines() {
	for p.peek().kind == tokNewline {
		p.advance()
	}
}

func (p *parser) parseProgram() error {
	for {
		p.skipNewlines()
		if p.peek().kind == tokEOF {
			break
		}
		r, err := p.parseRule()
		if err != nil {
			return err
		}
		p.prog.rules = append(p.prog.rules, r)
		switch t := p.peek(); t.kind {
		case tokNewline:
			p.advance()
		case tokEOF:
		default:
			return p.errf(t, "one rule per line: unexpected %s after the rule", describe(t))
		}
	}
	if len(p.prog.rules) == 0 {
		return errors.New("no rules: an empty policy file is more likely a mistake than a decision")
	}
	return nil
}

func (p *parser) parseRule() (rule, error) {
	head := p.peek()
	if head.kind != tokIdent {
		return rule{}, p.errf(head, "a rule starts with one of %s; found %s",
			actionList(p.hook), describe(head))
	}
	var r rule
	r.line = head.line

	switch head.text {
	case "deny":
		p.advance()
		r.act = actDeny
		if p.peek().kind == tokString {
			r.arg = strValue(p.advance().text)
		}
	case "allow":
		p.advance()
		r.act = actAllow
	case "set":
		p.advance()
		r.act = actSet
		nameTok, err := p.expect(tokIdent)
		if err != nil {
			return r, err
		}
		if len(nameTok.text) > maxTagNameLen {
			return r, p.errf(nameTok, "tag name %q is longer than %d characters",
				nameTok.text, maxTagNameLen)
		}
		r.name = nameTok.text
		if _, err := p.expect(tokAssign); err != nil {
			return r, err
		}
		v, err := p.parseLiteral()
		if err != nil {
			return r, err
		}
		r.arg = v
	default:
		return rule{}, p.errf(head, "%q is not a rule verb; %s understands %s",
			head.text, p.hook, actionList(p.hook))
	}

	if !actionLegal(p.hook, r.act) {
		return rule{}, p.errf(head, "%q is not available in %s; it understands %s",
			r.act, p.hook, actionList(p.hook))
	}

	// The optional guard.
	if t := p.peek(); t.kind == tokIdent && t.text == "if" {
		p.advance()
		code, err := p.compileCondition()
		if err != nil {
			return r, err
		}
		r.cond = code
	}
	return r, nil
}

func actionList(h Hook) string {
	acts := legalActions(h)
	out := ""
	for i, a := range acts {
		if i > 0 {
			out += ", "
		}
		out += a.String()
	}
	return out
}

func (p *parser) parseLiteral() (value, error) {
	t := p.peek()
	switch t.kind {
	case tokString:
		p.advance()
		return strValue(t.text), nil
	case tokNumber:
		p.advance()
		return numValue(t.num), nil
	case tokIdent:
		switch t.text {
		case "true":
			p.advance()
			return boolValue(true), nil
		case "false":
			p.advance()
			return boolValue(false), nil
		}
	}
	return value{}, p.errf(t, "expected a string, number or boolean literal, found %s", describe(t))
}

// --- expression compilation -------------------------------------------------

func (p *parser) compileCondition() ([]instr, error) {
	p.code = nil
	p.depth, p.maxDepth = 0, 0
	k, err := p.compileOr()
	if err != nil {
		return nil, err
	}
	if k != kindBool {
		return nil, fmt.Errorf("line %d: an `if` guard must be a condition, not a %s", p.peek().line, k)
	}
	if p.maxDepth > p.prog.depth {
		p.prog.depth = p.maxDepth
	}
	code := p.code
	p.code = nil
	return code, nil
}

func (p *parser) emit(op opcode, a int32) int {
	p.code = append(p.code, instr{op: op, a: a})
	return len(p.code) - 1
}

func (p *parser) push(n int) {
	p.depth += n
	if p.depth > p.maxDepth {
		p.maxDepth = p.depth
	}
}

func (p *parser) constIndex(v value) int32 {
	for i, c := range p.prog.consts {
		if c.k == v.k && c.num == v.num && c.b == v.b && c.str == v.str && c.list == nil && v.list == nil {
			return int32(i)
		}
	}
	p.prog.consts = append(p.prog.consts, v)
	return int32(len(p.prog.consts) - 1)
}

func (p *parser) compileOr() (kind, error) {
	k, err := p.compileAnd()
	if err != nil {
		return k, err
	}
	for {
		t := p.peek()
		if t.kind != tokIdent || t.text != "or" {
			return k, nil
		}
		if k != kindBool {
			return k, p.errf(t, "`or` needs conditions on both sides; the left side is a %s", k)
		}
		p.advance()
		jmp := p.emit(opJumpIfTrue, 0)
		p.emit(opPop, 0)
		p.depth--
		rk, err := p.compileAnd()
		if err != nil {
			return rk, err
		}
		if rk != kindBool {
			return rk, p.errf(t, "`or` needs conditions on both sides; the right side is a %s", rk)
		}
		p.code[jmp].a = int32(len(p.code))
	}
}

func (p *parser) compileAnd() (kind, error) {
	k, err := p.compileNot()
	if err != nil {
		return k, err
	}
	for {
		t := p.peek()
		if t.kind != tokIdent || t.text != "and" {
			return k, nil
		}
		if k != kindBool {
			return k, p.errf(t, "`and` needs conditions on both sides; the left side is a %s", k)
		}
		p.advance()
		jmp := p.emit(opJumpIfFalse, 0)
		p.emit(opPop, 0)
		p.depth--
		rk, err := p.compileNot()
		if err != nil {
			return rk, err
		}
		if rk != kindBool {
			return rk, p.errf(t, "`and` needs conditions on both sides; the right side is a %s", rk)
		}
		p.code[jmp].a = int32(len(p.code))
	}
}

func (p *parser) compileNot() (kind, error) {
	t := p.peek()
	if t.kind == tokIdent && t.text == "not" {
		p.advance()
		k, err := p.compileNot()
		if err != nil {
			return k, err
		}
		if k != kindBool {
			return k, p.errf(t, "`not` needs a condition, found a %s", k)
		}
		p.emit(opNot, 0)
		return kindBool, nil
	}
	return p.compileComparison()
}

// wordOps are the comparison verbs spelled as words.
var wordOps = map[string]opcode{
	"contains":   opContains,
	"startswith": opStartsWith,
	"endswith":   opEndsWith,
	"in":         opIn,
}

func (p *parser) compileComparison() (kind, error) {
	lk, err := p.compilePrimary()
	if err != nil {
		return lk, err
	}
	t := p.peek()

	var op opcode
	switch t.kind {
	case tokEq:
		op = opEq
	case tokNe:
		op = opNe
	case tokLt:
		op = opLt
	case tokLe:
		op = opLe
	case tokGt:
		op = opGt
	case tokGe:
		op = opGe
	case tokIdent:
		w, ok := wordOps[t.text]
		if !ok {
			return lk, nil // not a comparison; hand the operand back
		}
		op = w
	default:
		return lk, nil
	}
	p.advance()

	rk, err := p.compilePrimary()
	if err != nil {
		return rk, err
	}

	switch op {
	case opIn:
		if lk != kindString || rk != kindList {
			return kindNone, p.errf(t, "`in` compares a string against a list, found %s in %s", lk, rk)
		}
	case opContains, opStartsWith, opEndsWith:
		if lk != kindString || rk != kindString {
			return kindNone, p.errf(t, "`%s` compares two strings, found %s and %s", t.text, lk, rk)
		}
	case opLt, opLe, opGt, opGe:
		if lk != rk || (lk != kindNumber && lk != kindString) {
			return kindNone, p.errf(t, "an ordering comparison needs two numbers or two strings, found %s and %s", lk, rk)
		}
	default: // opEq, opNe
		if lk != rk {
			return kindNone, p.errf(t, "%s and %s are never equal; compare like with like", lk, rk)
		}
		if lk == kindList {
			return kindNone, p.errf(t, "lists cannot be compared; use `in`")
		}
	}
	p.emit(op, 0)
	p.depth-- // two operands in, one result out
	return kindBool, nil
}

func (p *parser) compilePrimary() (kind, error) {
	t := p.peek()
	switch t.kind {
	case tokLParen:
		p.advance()
		k, err := p.compileOr()
		if err != nil {
			return k, err
		}
		if _, err := p.expect(tokRParen); err != nil {
			return k, err
		}
		return k, nil

	case tokString:
		p.advance()
		p.emit(opConst, p.constIndex(strValue(t.text)))
		p.push(1)
		return kindString, nil

	case tokNumber:
		p.advance()
		p.emit(opConst, p.constIndex(numValue(t.num)))
		p.push(1)
		return kindNumber, nil

	case tokLBracket:
		return p.compileList()

	case tokIdent:
		switch t.text {
		case "true", "false":
			p.advance()
			p.emit(opConst, p.constIndex(boolValue(t.text == "true")))
			p.push(1)
			return kindBool, nil
		}
		spec, ok := p.fields.byName[t.text]
		if !ok {
			return kindNone, p.errf(t, "%s has no field %q; it has %v", p.hook, t.text, p.fields.names)
		}
		p.advance()
		p.emit(opField, int32(spec.idx))
		p.push(1)
		return spec.kind, nil
	}
	return kindNone, p.errf(t, "expected a value, found %s", describe(t))
}

func (p *parser) compileList() (kind, error) {
	open := p.advance() // [
	var elems []string
	for {
		if p.peek().kind == tokRBracket {
			p.advance()
			break
		}
		t, err := p.expect(tokString)
		if err != nil {
			return kindNone, err
		}
		if len(elems) >= maxListElements {
			return kindNone, p.errf(open, "a list may hold at most %d elements", maxListElements)
		}
		elems = append(elems, t.text)
		if p.peek().kind == tokComma {
			p.advance()
			continue
		}
	}
	p.prog.consts = append(p.prog.consts, listValue(elems))
	p.emit(opConst, int32(len(p.prog.consts)-1))
	p.push(1)
	return kindList, nil
}
