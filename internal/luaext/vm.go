package luaext

import "strings"

// The evaluator.
//
// It is a stack machine over a fixed-size operand stack that lives on the
// caller's goroutine stack. It allocates nothing, takes no lock, and has no
// error path except the two ceilings: the compiler has already proved that
// every operand has the type its operator expects, so there is no type error
// left to report and no dynamic dispatch to do.

// budget is one invocation's share of the ceilings. It is per invocation rather
// than per program: see [Limits].
type budget struct {
	instrLeft int64
	memMax    int64
	memLive   int64
	memPeak   int64
	// unlimited short-circuits both checks. It is only set when the caller
	// configured a zero limit, which internal/config refuses for an enabled
	// engine.
	noInstr bool
	noMem   bool
}

func newBudget(l Limits) budget {
	return budget{
		instrLeft: l.Instructions,
		memMax:    l.MemoryBytes,
		noInstr:   l.Instructions <= 0,
		noMem:     l.MemoryBytes <= 0,
	}
}

func (b *budget) step(n int64) error {
	if b.noInstr {
		return nil
	}
	b.instrLeft -= n
	if b.instrLeft < 0 {
		return ErrInstructionLimit
	}
	return nil
}

func (b *budget) charge(n int64) error {
	b.memLive += n
	if b.memLive > b.memPeak {
		b.memPeak = b.memLive
	}
	if !b.noMem && b.memLive > b.memMax {
		return ErrMemoryLimit
	}
	return nil
}

func (b *budget) refund(n int64) { b.memLive -= n }

// machine is one evaluation's operand stack. It is a value on the caller's
// stack; nothing here escapes.
type machine struct {
	st [maxStack]value
	sp int
}

func (m *machine) push(b *budget, v value) error {
	if m.sp >= maxStack {
		// Unreachable: the compiler rejects a program that needs more slots.
		// Kept as a bound rather than a panic, because "unreachable" inside a
		// sandbox is a claim, not a guarantee.
		return ErrMemoryLimit
	}
	if err := b.charge(v.bytes()); err != nil {
		return err
	}
	m.st[m.sp] = v
	m.sp++
	return nil
}

func (m *machine) pop(b *budget) value {
	m.sp--
	v := m.st[m.sp]
	m.st[m.sp] = value{}
	b.refund(v.bytes())
	return v
}

func (m *machine) top() value { return m.st[m.sp-1] }

// eval runs a compiled condition. A nil condition is an unconditional rule.
func eval(p *Program, code []instr, v viewer, b *budget) (bool, error) {
	if len(code) == 0 {
		return true, nil
	}
	var m machine
	for pc := 0; pc < len(code); {
		if err := b.step(1); err != nil {
			return false, err
		}
		in := code[pc]
		pc++
		switch in.op {
		case opConst:
			if err := m.push(b, p.consts[in.a]); err != nil {
				return false, err
			}
		case opField:
			if err := m.push(b, v.field(int(in.a))); err != nil {
				return false, err
			}
		case opPop:
			m.pop(b)
		case opNot:
			x := m.pop(b)
			if err := m.push(b, boolValue(!x.b)); err != nil {
				return false, err
			}
		case opJumpIfFalse:
			if !m.top().b {
				pc = int(in.a)
			}
		case opJumpIfTrue:
			if m.top().b {
				pc = int(in.a)
			}
		default:
			r := m.pop(b)
			l := m.pop(b)
			res, err := binary(in.op, l, r, b)
			if err != nil {
				return false, err
			}
			if err := m.push(b, boolValue(res)); err != nil {
				return false, err
			}
		}
	}
	if m.sp != 1 {
		// Also unreachable by construction; a wrong answer is worse than a
		// skipped hook, so this fails open rather than guessing.
		return false, ErrInstructionLimit
	}
	return m.top().b, nil
}

func binary(op opcode, l, r value, b *budget) (bool, error) {
	switch op {
	case opEq:
		return same(l, r), nil
	case opNe:
		return !same(l, r), nil
	case opLt, opLe, opGt, opGe:
		return order(op, l, r), nil
	case opContains:
		return strings.Contains(l.str, r.str), nil
	case opStartsWith:
		return strings.HasPrefix(l.str, r.str), nil
	case opEndsWith:
		return strings.HasSuffix(l.str, r.str), nil
	case opIn:
		// A list is capped at maxListElements and each comparison is charged,
		// so `in` cannot be the cheap syntax that hides an expensive scan.
		if err := b.step(int64(len(r.list))); err != nil {
			return false, err
		}
		for _, s := range r.list {
			if s == l.str {
				return true, nil
			}
		}
		return false, nil
	}
	return false, nil
}

func same(l, r value) bool {
	switch l.k {
	case kindString:
		return l.str == r.str
	case kindNumber:
		return l.num == r.num
	case kindBool:
		return l.b == r.b
	}
	return false
}

func order(op opcode, l, r value) bool {
	var c int
	if l.k == kindString {
		c = strings.Compare(l.str, r.str)
	} else {
		switch {
		case l.num < r.num:
			c = -1
		case l.num > r.num:
			c = 1
		}
	}
	switch op {
	case opLt:
		return c < 0
	case opLe:
		return c <= 0
	case opGt:
		return c > 0
	case opGe:
		return c >= 0
	}
	return false
}

// run evaluates a whole program against a view and folds the result into out.
//
// It returns true when evaluation was terminated by an allow or a deny, which
// tells the engine to stop before the next program.
func (p *Program) run(v viewer, b *budget, out *result) (stop bool, err error) {
	for i := range p.rules {
		r := &p.rules[i]
		if err := b.step(1); err != nil {
			return false, err
		}
		ok, err := eval(p, r.cond, v, b)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		switch r.act {
		case actDeny:
			out.denied = true
			out.reason = r.arg.str
			out.by = p.Name
			return true, nil
		case actAllow:
			out.allowed = true
			out.by = p.Name
			return true, nil
		case actSet:
			if err := b.charge(int64(len(r.name) + len(r.arg.str) + valueOverhead)); err != nil {
				return false, err
			}
			out.tags.set(r.name, r.arg.String())
		}
	}
	return false, nil
}

// result is what one invocation accumulated across its units. It is folded into
// the caller's typed decision only when the invocation completed.
type result struct {
	tags    tagset
	denied  bool
	allowed bool
	reason  string
	by      string
}
