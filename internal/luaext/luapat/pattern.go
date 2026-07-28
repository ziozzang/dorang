// Package luapat is a step-counted copy of gopher-lua's Lua pattern matcher.
//
// # Why a copy exists
//
// The upstream matcher is a backtracking VM with no bound on the number of
// steps it takes. Its cost is superlinear in the length of the subject — a
// three-item lazy pattern over 256 bytes runs for seconds — and none of it is
// observable from outside: it is one Go call, so the host's context check never
// runs, the instruction ceiling never ticks, and the wall-clock watchdog can
// only abandon the goroutine, not stop it. A plugin that calls string.find on
// caller-supplied text therefore walks straight through all three of DESIGN
// §11.5's ceilings.
//
// The fix has to count *steps*, and steps are only visible from inside the
// matcher. So this file is the upstream matcher with one change: every
// instruction dispatch, every start position and every collected match
// decrements a caller-supplied budget, and exhausting it stops the match with
// [ErrBudget] instead of running to completion.
//
// It is a copy rather than a rewrite on purpose. The semantics of Lua patterns
// are what a plugin author expects, and a hand-written matcher would be a
// second, subtly different dialect. Everything below is upstream's except the
// [Run] additions and one one-line lint fix marked where it occurs, and it
// should be re-copied — not re-derived — when gopher-lua is upgraded.
// TestBudgetedMatcherMatchesUpstream compares this against the real thing and is
// what makes "copy" checkable rather than aspirational.
//
// # Steps are not the only resource
//
// Counting steps bounds CPU and bounds nothing else. The VM is *recursive* —
// one Go frame per branch explored — so a greedy quantifier recurses once per
// character it consumes, and a step counter watches that happen without
// objecting: 800 KiB of caller text against `^(.*)=(.*)$` cost 800 K steps out
// of five million and **3 GB of goroutine stack**, eight concurrent requests at
// a time, with no ceiling firing. Go grows a goroutine stack to 1 GB before
// killing the *process*, so a large enough segment is a crash rather than a
// refusal.
//
// So [Run] carries three things, not one: the step budget, a depth ceiling
// ([MaxDepth]) that bounds the stack the same way, and the invocation's context,
// checked often enough that a match cannot outlive the wall clock that is
// supposed to bound it. All three are the copy's; none of them changes which
// strings match.
//
// Derived from github.com/yuin/gopher-lua/pm at v1.1.2.
//
//	The MIT License (MIT)
//	Copyright (c) 2015 Yusuke Inuzuka
//
//	Permission is hereby granted, free of charge, to any person obtaining a
//	copy of this software and associated documentation files (the "Software"),
//	to deal in the Software without restriction, including without limitation
//	the rights to use, copy, modify, merge, publish, distribute, sublicense,
//	and/or sell copies of the Software, and to permit persons to whom the
//	Software is furnished to do so, subject to the following conditions:
//
//	The above copyright notice and this permission notice shall be included in
//	all copies or substantial portions of the Software.
//
//	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
//	IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
//	FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
//	AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
//	LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
//	FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER
//	DEALINGS IN THE SOFTWARE.
package luapat

import (
	"context"
	"errors"
	"fmt"
)

const (
	EOS      = -1
	_UNKNOWN = -2
)

// MaxDepth is how deep the matcher may recurse before a match is refused.
//
// The VM explores a branch by calling itself, so depth is not an exotic
// property of an exotic pattern: `(.*)` recurses once per character it consumes,
// which makes the depth the *caller's* to choose exactly as the step count was.
// A frame measures about 400 bytes on the machine this was written on, so this
// ceiling is about 4 MB of goroutine stack per match — a number an operator can
// multiply by their concurrency, which is the property the old
// `maxRecursionLevel = 1000000` did not have: it was 400 MB per match, and Go
// kills the process at 1 GB. Measured at eight concurrent matches over 800 KiB
// of caller text: 3 072 MB before, 31 MB after.
//
// The cost of the ceiling is stated rather than hidden: a quantifier that has to
// carry more than this many characters in one run — `^%s*(.-)%s*$` over a
// segment larger than MaxDepth, say — is refused instead of served. That is a
// real restriction on long subjects, and it is the same restriction the step
// budget already imposes on expensive ones; a filter that needs to work on
// whole documents should ask the host for it (`dorang.mask` runs Go regexps
// with no stack of its own) rather than backtrack through them in Lua.
const MaxDepth = 10000

// ErrBudget reports that a match was stopped because it had spent everything
// the caller was willing to pay for. It is not a pattern error: the pattern was
// valid and the subject was valid, and the answer is simply not affordable.
var ErrBudget = errors.New("luapat: pattern match exceeded its step budget")

// ErrDepth reports that a match recursed deeper than [MaxDepth]. It is separate
// from [ErrBudget] because the resource is different — stack, not CPU — and an
// operator reading a diagnostic needs to know which ceiling to raise.
var ErrDepth = errors.New("luapat: pattern match recursed deeper than the sandbox allows")

// ctxCheckSteps is how many matcher steps run between two cancellation checks.
//
// A step is about 22 ns, so this is roughly 22 µs of matching between checks:
// far below any wall clock worth configuring, and far above the cost of the
// check itself, which is a non-blocking channel receive.
const ctxCheckSteps = 1024

// Run is what one match may spend.
//
// It is the whole of this copy's addition to upstream, gathered into one value
// so the recursive VM carries one extra pointer rather than three.
type Run struct {
	// Budget is the step allowance. It is decremented as the match proceeds and
	// is left holding whatever was not spent, so the caller learns the exact
	// cost of the call rather than an estimate of it. It goes negative by
	// exactly the step that ran out.
	Budget int64
	// Done is the invocation's cancellation channel, or nil for a match that
	// nothing may interrupt. A nil channel is only right where there is no
	// invocation — a test, or the comparison harness.
	//
	// It exists because the wall-clock watchdog cannot stop a host call: before
	// this, a priced match ran to its step budget after the request it belonged
	// to had been answered, abandoned and forgotten, and the watchdog's promise
	// that "a hook that ignores its context delays the request by the ceiling
	// and no more" was true of the hook and false of the matcher underneath it.
	Done <-chan struct{}

	// next counts down to the next cancellation check.
	next int64
}

// spend charges one step and, every [ctxCheckSteps], asks whether anyone is
// still waiting for the answer.
func (r *Run) spend() {
	r.Budget--
	if r.Budget < 0 {
		panic(errBudgetExhausted{})
	}
	r.next--
	if r.next > 0 {
		return
	}
	r.next = ctxCheckSteps
	select {
	case <-r.Done:
		panic(errCancelled{})
	default:
	}
}

// MatchCost is charged for each match added to the result set, on top of the
// steps the match itself took.
//
// A match is a MatchData with a capture slice behind it, so a pattern that
// matches at every position (`gsub(s, "", "x")`) would otherwise allocate
// proportionally to the subject while spending one step per position. Charging
// for the record as well as for the search is what keeps the *host* memory a
// match set occupies bounded by the same budget.
const MatchCost = 16

// The panic values used to unwind out of the recursive VM. Each is converted to
// its error by Find's recover, in the same style as the pattern errors upstream
// already unwinds this way.
type (
	errBudgetExhausted struct{}
	errTooDeep         struct{}
	errCancelled       struct{}
)

/* Error {{{ */

type Error struct {
	Pos     int
	Message string
}

func newError(pos int, message string, args ...interface{}) *Error {
	if len(args) == 0 {
		return &Error{pos, message}
	}
	return &Error{pos, fmt.Sprintf(message, args...)}
}

func (e *Error) Error() string {
	switch e.Pos {
	case EOS:
		return fmt.Sprintf("%s at EOS", e.Message)
	case _UNKNOWN:
		// Upstream writes fmt.Sprintf("%s", e.Message) here. The only deviation
		// in the copy other than the budget, and it is a lint fix, not a
		// behaviour change.
		return e.Message
	default:
		return fmt.Sprintf("%s at %d", e.Message, e.Pos)
	}
}

/* }}} */

/* MatchData {{{ */

type MatchData struct {
	// captured positions
	// layout
	// xxxx xxxx xxxx xxx0 : caputured positions
	// xxxx xxxx xxxx xxx1 : position captured positions
	captures []uint32
}

func newMatchState() *MatchData { return &MatchData{[]uint32{}} }

func (st *MatchData) addPosCapture(s, pos int) {
	for s+1 >= len(st.captures) {
		st.captures = append(st.captures, 0)
	}
	st.captures[s] = (uint32(pos) << 1) | 1
	st.captures[s+1] = (uint32(pos) << 1) | 1
}

func (st *MatchData) setCapture(s, pos int) uint32 {
	for s >= len(st.captures) {
		st.captures = append(st.captures, 0)
	}
	v := st.captures[s]
	st.captures[s] = (uint32(pos) << 1)
	return v
}

func (st *MatchData) restoreCapture(s int, pos uint32) { st.captures[s] = pos }

func (st *MatchData) CaptureLength() int { return len(st.captures) }

func (st *MatchData) IsPosCapture(idx int) bool { return (st.captures[idx] & 1) == 1 }

func (st *MatchData) Capture(idx int) int { return int(st.captures[idx] >> 1) }

/* }}} */

/* scanner {{{ */

type scannerState struct {
	Pos     int
	started bool
}

type scanner struct {
	src   []byte
	State scannerState
	saved scannerState
}

func newScanner(src []byte) *scanner {
	return &scanner{
		src: src,
		State: scannerState{
			Pos:     0,
			started: false,
		},
		saved: scannerState{},
	}
}

func (sc *scanner) Length() int { return len(sc.src) }

func (sc *scanner) Next() int {
	if !sc.State.started {
		sc.State.started = true
		if len(sc.src) == 0 {
			sc.State.Pos = EOS
		}
	} else {
		sc.State.Pos = sc.NextPos()
	}
	if sc.State.Pos == EOS {
		return EOS
	}
	return int(sc.src[sc.State.Pos])
}

func (sc *scanner) CurrentPos() int {
	return sc.State.Pos
}

func (sc *scanner) NextPos() int {
	if sc.State.Pos == EOS || sc.State.Pos >= len(sc.src)-1 {
		return EOS
	}
	if !sc.State.started {
		return 0
	} else {
		return sc.State.Pos + 1
	}
}

func (sc *scanner) Peek() int {
	cureof := sc.State.Pos == EOS
	ch := sc.Next()
	if !cureof {
		if sc.State.Pos == EOS {
			sc.State.Pos = len(sc.src) - 1
		} else {
			sc.State.Pos--
			if sc.State.Pos < 0 {
				sc.State.Pos = 0
				sc.State.started = false
			}
		}
	}
	return ch
}

func (sc *scanner) Save() { sc.saved = sc.State }

func (sc *scanner) Restore() { sc.State = sc.saved }

/* }}} */

/* bytecode {{{ */

type opCode int

const (
	opChar opCode = iota
	opMatch
	opTailMatch
	opJmp
	opSplit
	opSave
	opPSave
	opBrace
	opNumber
)

type inst struct {
	OpCode   opCode
	Class    class
	Operand1 int
	Operand2 int
}

/* }}} */

/* classes {{{ */

type class interface {
	Matches(ch int) bool
}

type dotClass struct{}

func (pn *dotClass) Matches(ch int) bool { return true }

type charClass struct {
	Ch int
}

func (pn *charClass) Matches(ch int) bool { return pn.Ch == ch }

type singleClass struct {
	Class int
}

func (pn *singleClass) Matches(ch int) bool {
	ret := false
	switch pn.Class {
	case 'a', 'A':
		ret = 'A' <= ch && ch <= 'Z' || 'a' <= ch && ch <= 'z'
	case 'c', 'C':
		ret = (0x00 <= ch && ch <= 0x1F) || ch == 0x7F
	case 'd', 'D':
		ret = '0' <= ch && ch <= '9'
	case 'l', 'L':
		ret = 'a' <= ch && ch <= 'z'
	case 'p', 'P':
		ret = (0x21 <= ch && ch <= 0x2f) || (0x3a <= ch && ch <= 0x40) || (0x5b <= ch && ch <= 0x60) || (0x7b <= ch && ch <= 0x7e)
	case 's', 'S':
		switch ch {
		case ' ', '\f', '\n', '\r', '\t', '\v':
			ret = true
		}
	case 'u', 'U':
		ret = 'A' <= ch && ch <= 'Z'
	case 'w', 'W':
		ret = '0' <= ch && ch <= '9' || 'A' <= ch && ch <= 'Z' || 'a' <= ch && ch <= 'z'
	case 'x', 'X':
		ret = '0' <= ch && ch <= '9' || 'a' <= ch && ch <= 'f' || 'A' <= ch && ch <= 'F'
	case 'z', 'Z':
		ret = ch == 0
	default:
		return ch == pn.Class
	}
	if 'A' <= pn.Class && pn.Class <= 'Z' {
		return !ret
	}
	return ret
}

type setClass struct {
	IsNot   bool
	Classes []class
}

func (pn *setClass) Matches(ch int) bool {
	for _, class := range pn.Classes {
		if class.Matches(ch) {
			return !pn.IsNot
		}
	}
	return pn.IsNot
}

type rangeClass struct {
	Begin class
	End   class
}

func (pn *rangeClass) Matches(ch int) bool {
	switch begin := pn.Begin.(type) {
	case *charClass:
		end, ok := pn.End.(*charClass)
		if !ok {
			return false
		}
		return begin.Ch <= ch && ch <= end.Ch
	}
	return false
}

// }}}

// patterns {{{

type pattern interface{}

type singlePattern struct {
	Class class
}

type seqPattern struct {
	MustHead bool
	MustTail bool
	Patterns []pattern
}

type repeatPattern struct {
	Type  int
	Class class
}

type posCapPattern struct{}

type capPattern struct {
	Pattern pattern
}

type numberPattern struct {
	N int
}

type bracePattern struct {
	Begin int
	End   int
}

// }}}

/* parse {{{ */

func parseClass(sc *scanner, allowset bool) class {
	ch := sc.Next()
	switch ch {
	case '%':
		return &singleClass{sc.Next()}
	case '.':
		if allowset {
			return &dotClass{}
		}
		return &charClass{ch}
	case '[':
		if allowset {
			return parseClassSet(sc)
		}
		return &charClass{ch}
	//case '^' '$', '(', ')', ']', '*', '+', '-', '?':
	//	panic(newError(sc.CurrentPos(), "invalid %c", ch))
	case EOS:
		panic(newError(sc.CurrentPos(), "unexpected EOS"))
	default:
		return &charClass{ch}
	}
}

func parseClassSet(sc *scanner) class {
	set := &setClass{false, []class{}}
	if sc.Peek() == '^' {
		set.IsNot = true
		sc.Next()
	}
	isrange := false
	for {
		ch := sc.Peek()
		switch ch {
		// case '[':
		// 	panic(newError(sc.CurrentPos(), "'[' can not be nested"))
		case EOS:
			panic(newError(sc.CurrentPos(), "unexpected EOS"))
		case ']':
			if len(set.Classes) > 0 {
				sc.Next()
				goto exit
			}
			fallthrough
		case '-':
			if len(set.Classes) > 0 {
				sc.Next()
				isrange = true
				continue
			}
			fallthrough
		default:
			set.Classes = append(set.Classes, parseClass(sc, false))
		}
		if isrange {
			begin := set.Classes[len(set.Classes)-2]
			end := set.Classes[len(set.Classes)-1]
			set.Classes = set.Classes[0 : len(set.Classes)-2]
			set.Classes = append(set.Classes, &rangeClass{begin, end})
			isrange = false
		}
	}
exit:
	if isrange {
		set.Classes = append(set.Classes, &charClass{'-'})
	}

	return set
}

func parsePattern(sc *scanner, toplevel bool) *seqPattern {
	pat := &seqPattern{}
	if toplevel {
		if sc.Peek() == '^' {
			sc.Next()
			pat.MustHead = true
		}
	}
	for {
		ch := sc.Peek()
		switch ch {
		case '%':
			sc.Save()
			sc.Next()
			switch sc.Peek() {
			case '0':
				panic(newError(sc.CurrentPos(), "invalid capture index"))
			case '1', '2', '3', '4', '5', '6', '7', '8', '9':
				pat.Patterns = append(pat.Patterns, &numberPattern{sc.Next() - 48})
			case 'b':
				sc.Next()
				pat.Patterns = append(pat.Patterns, &bracePattern{sc.Next(), sc.Next()})
			default:
				sc.Restore()
				pat.Patterns = append(pat.Patterns, &singlePattern{parseClass(sc, true)})
			}
		case '.', '[', ']':
			pat.Patterns = append(pat.Patterns, &singlePattern{parseClass(sc, true)})
		//case ']':
		//	panic(newError(sc.CurrentPos(), "invalid ']'"))
		case ')':
			if toplevel {
				panic(newError(sc.CurrentPos(), "invalid ')'"))
			}
			return pat
		case '(':
			sc.Next()
			if sc.Peek() == ')' {
				sc.Next()
				pat.Patterns = append(pat.Patterns, &posCapPattern{})
			} else {
				ret := &capPattern{parsePattern(sc, false)}
				if sc.Peek() != ')' {
					panic(newError(sc.CurrentPos(), "unfinished capture"))
				}
				sc.Next()
				pat.Patterns = append(pat.Patterns, ret)
			}
		case '*', '+', '-', '?':
			sc.Next()
			if len(pat.Patterns) > 0 {
				spat, ok := pat.Patterns[len(pat.Patterns)-1].(*singlePattern)
				if ok {
					pat.Patterns = pat.Patterns[0 : len(pat.Patterns)-1]
					pat.Patterns = append(pat.Patterns, &repeatPattern{ch, spat.Class})
					continue
				}
			}
			pat.Patterns = append(pat.Patterns, &singlePattern{&charClass{ch}})
		case '$':
			if toplevel && (sc.NextPos() == sc.Length()-1 || sc.NextPos() == EOS) {
				pat.MustTail = true
			} else {
				pat.Patterns = append(pat.Patterns, &singlePattern{&charClass{ch}})
			}
			sc.Next()
		case EOS:
			sc.Next()
			goto exit
		default:
			sc.Next()
			pat.Patterns = append(pat.Patterns, &singlePattern{&charClass{ch}})
		}
	}
exit:
	return pat
}

type iptr struct {
	insts   []inst
	capture int
}

func compilePattern(p pattern, ps ...*iptr) []inst {
	var ptr *iptr
	toplevel := false
	if len(ps) == 0 {
		toplevel = true
		ptr = &iptr{[]inst{inst{opSave, nil, 0, -1}}, 2}
	} else {
		ptr = ps[0]
	}
	switch pat := p.(type) {
	case *singlePattern:
		ptr.insts = append(ptr.insts, inst{opChar, pat.Class, -1, -1})
	case *seqPattern:
		for _, cp := range pat.Patterns {
			compilePattern(cp, ptr)
		}
	case *repeatPattern:
		idx := len(ptr.insts)
		switch pat.Type {
		case '*':
			ptr.insts = append(ptr.insts,
				inst{opSplit, nil, idx + 1, idx + 3},
				inst{opChar, pat.Class, -1, -1},
				inst{opJmp, nil, idx, -1})
		case '+':
			ptr.insts = append(ptr.insts,
				inst{opChar, pat.Class, -1, -1},
				inst{opSplit, nil, idx, idx + 2})
		case '-':
			ptr.insts = append(ptr.insts,
				inst{opSplit, nil, idx + 3, idx + 1},
				inst{opChar, pat.Class, -1, -1},
				inst{opJmp, nil, idx, -1})
		case '?':
			ptr.insts = append(ptr.insts,
				inst{opSplit, nil, idx + 1, idx + 2},
				inst{opChar, pat.Class, -1, -1})
		}
	case *posCapPattern:
		ptr.insts = append(ptr.insts, inst{opPSave, nil, ptr.capture, -1})
		ptr.capture += 2
	case *capPattern:
		c0, c1 := ptr.capture, ptr.capture+1
		ptr.capture += 2
		ptr.insts = append(ptr.insts, inst{opSave, nil, c0, -1})
		compilePattern(pat.Pattern, ptr)
		ptr.insts = append(ptr.insts, inst{opSave, nil, c1, -1})
	case *bracePattern:
		ptr.insts = append(ptr.insts, inst{opBrace, nil, pat.Begin, pat.End})
	case *numberPattern:
		ptr.insts = append(ptr.insts, inst{opNumber, nil, pat.N, -1})
	}
	if toplevel {
		if p.(*seqPattern).MustTail {
			ptr.insts = append(ptr.insts, inst{opSave, nil, 1, -1}, inst{opTailMatch, nil, -1, -1})
		}
		ptr.insts = append(ptr.insts, inst{opSave, nil, 1, -1}, inst{opMatch, nil, -1, -1})
	}
	return ptr.insts
}

/* }}} parse */

/* VM {{{ */

// Simple recursive virtual machine based on the
// "Regular Expression Matching: the Virtual Machine Approach" (https://swtch.com/~rsc/regexp/regexp2.html)
func recursiveVM(src []byte, insts []inst, pc, sp, recLevel int, r *Run, ms ...*MatchData) (bool, int, *MatchData) {
	recLevel++
	if recLevel > MaxDepth {
		// Upstream raises "pattern/input too complex" at a million frames, which
		// is a limit on nothing an operator can afford: a million frames is
		// nearly half a gigabyte of stack, and the caller chooses the depth by
		// choosing the length of the text. The ceiling is lowered and the
		// unwind is the copy's own, so a depth breach is reported as the
		// resource it is rather than as a malformed pattern.
		panic(errTooDeep{})
	}
	var m *MatchData
	if len(ms) == 0 {
		m = newMatchState()
	} else {
		m = ms[0]
	}
redo:
	// The one addition to upstream's dispatch: every step costs a step. `goto
	// redo` jumps here, so a long run of opChar is charged per character and a
	// backtracking explosion is charged per branch explored — which is the
	// whole point, because the explosion is what the ceiling could not see.
	// Every so often it also asks whether the request is still there.
	r.spend()
	inst := insts[pc]
	switch inst.OpCode {
	case opChar:
		if sp >= len(src) || !inst.Class.Matches(int(src[sp])) {
			return false, sp, m
		}
		pc++
		sp++
		goto redo
	case opMatch:
		return true, sp, m
	case opTailMatch:
		return sp >= len(src), sp, m
	case opJmp:
		pc = inst.Operand1
		goto redo
	case opSplit:
		if ok, nsp, _ := recursiveVM(src, insts, inst.Operand1, sp, recLevel, r, m); ok {
			return true, nsp, m
		}
		pc = inst.Operand2
		goto redo
	case opSave:
		s := m.setCapture(inst.Operand1, sp)
		if ok, nsp, _ := recursiveVM(src, insts, pc+1, sp, recLevel, r, m); ok {
			return true, nsp, m
		}
		m.restoreCapture(inst.Operand1, s)
		return false, sp, m
	case opPSave:
		m.addPosCapture(inst.Operand1, sp+1)
		pc++
		goto redo
	case opBrace:
		if sp >= len(src) || int(src[sp]) != inst.Operand1 {
			return false, sp, m
		}
		count := 1
		for sp = sp + 1; sp < len(src); sp++ {
			if int(src[sp]) == inst.Operand2 {
				count--
			}
			if count == 0 {
				pc++
				sp++
				goto redo
			}
			if int(src[sp]) == inst.Operand1 {
				count++
			}
		}
		return false, sp, m
	case opNumber:
		idx := inst.Operand1 * 2
		if idx >= m.CaptureLength()-1 {
			panic(newError(_UNKNOWN, "invalid capture index"))
		}
		capture := src[m.Capture(idx):m.Capture(idx+1)]
		for i := 0; i < len(capture); i++ {
			if i+sp >= len(src) || capture[i] != src[i+sp] {
				return false, sp, m
			}
		}
		pc++
		sp += len(capture)
		goto redo
	}
	panic("should not reach here")
}

/* }}} */

/* API {{{ */

// Find matches p against src, spending at most r.Budget steps and at most
// [MaxDepth] frames, and stopping if r.Done closes.
//
// r.Budget is decremented as the match proceeds and is left holding whatever was
// not spent, so the caller learns the exact cost of the call rather than an
// estimate of it. When it runs out the match stops and the error is [ErrBudget];
// when the depth ceiling is reached the error is [ErrDepth]; when the context is
// done it is [context.Canceled]. In all three the partial matches are *not*
// returned, because a truncated answer is a wrong answer and the caller's job is
// to refuse, not to guess.
//
// Passing a budget large enough never to be reached, a subject that does not
// reach [MaxDepth] and a nil Done reproduces upstream's behaviour exactly.
func Find(p string, src []byte, offset, limit int, r *Run) (matches []*MatchData, err error) {
	defer func() {
		if v := recover(); v != nil {
			switch pv := v.(type) {
			case *Error:
				err = pv
			case errBudgetExhausted:
				matches, err = nil, ErrBudget
			case errTooDeep:
				matches, err = nil, ErrDepth
			case errCancelled:
				matches, err = nil, context.Canceled
			default:
				panic(v)
			}
		}
	}()
	if r.next <= 0 {
		r.next = ctxCheckSteps
	}
	// Parsing and compiling the pattern is linear in the pattern, and a plugin
	// can build a pattern as long as its memory ceiling allows, so it is
	// charged too rather than being a free prologue on every call.
	r.Budget -= int64(len(p))
	if r.Budget < 0 {
		return nil, ErrBudget
	}
	pat := parsePattern(newScanner([]byte(p)), true)
	insts := compilePattern(pat)
	matches = []*MatchData{}
	for sp := offset; sp <= len(src); {
		ok, nsp, ms := recursiveVM(src, insts, 0, sp, 0, r)
		sp++
		if ok {
			if sp < nsp {
				sp = nsp
			}
			r.Budget -= MatchCost
			if r.Budget < 0 {
				return nil, ErrBudget
			}
			matches = append(matches, ms)
		}
		if len(matches) == limit || pat.MustHead {
			break
		}
	}
	return
}

/* }}} */
