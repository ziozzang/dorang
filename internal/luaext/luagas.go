package luaext

import (
	"strconv"

	"github.com/yuin/gopher-lua/ast"
)

// Instruction and memory ceilings for a general-purpose Lua VM.
//
// gopher-lua gives one of DESIGN §11.5's three ceilings for free — a context
// check runs between VM instructions, so a wall clock can stop a hook — and
// neither of the other two. There is no per-state instruction counter and no
// per-state allocation accounting. This file builds both, because a wall clock
// alone is not a sandbox: a hook allocating in a tight loop exhausts the process
// long before a 200 ms deadline fires, and the process holds provider
// credentials.
//
// # How the instruction ceiling is built
//
// Source is parsed to an AST, the AST is rewritten, and the rewritten AST is
// compiled. Two rewrites:
//
//  1. A charge call is inserted at the head of every function body, every loop
//     body, and before every `goto`. Those are the only three ways Lua *source*
//     can execute an unbounded number of instructions — everything else is
//     straight-line code whose length is fixed when the plugin loads. So the
//     number of VM instructions a plugin can execute is bounded by
//     (charges granted) × (longest straight-line run), both of which are known.
//     The charge is the static node count of the block, so the counter tracks
//     work rather than merely counting back-edges.
//
//     It bounds instructions and not *work*, and the difference is a hole unless
//     something else closes it: one instruction can enter a builtin that runs for
//     seconds, and no rewrite of the source can see inside host code. So the
//     builtins whose work is not bounded by what they return are charged for
//     that work before they run — see luapattern.go, which is where the ceiling
//     was found to be walkable and how it stopped being.
//
//  2. `a .. b` becomes a call to a charged host concatenation. Concatenation is
//     the one operator that can allocate more than a constant per instruction,
//     and `s = s .. s` doubles: forty iterations is a terabyte, which no
//     instruction ceiling can catch and no wall clock can survive.
//
//  3. A string operand of a *comparison* and a dynamic *table key* are charged
//     for their length. `a == b`, `a < b` and `t[k]` are single VM instructions
//     that compare or hash every byte, so an O(1) charge bought a caller
//     O(64 KiB) of work — see [sizeGlobal] for the measurements and for why the
//     charge wraps an operand rather than replacing the operator.
//
// # How the memory ceiling is built
//
// Every allocation a plugin can cause is either O(1) per charge — hence bounded
// by the instruction ceiling — or is charged explicitly against the memory
// budget before it happens. The explicit ones are concatenation and the library
// functions whose output can exceed their input: string.rep, string.format,
// string.gsub, string.byte, string.char, table.concat and unpack. Each of those
// pre-flights its worst case against the *remaining* budget and refuses if it
// would not fit, so the ceiling is enforced before the allocation rather than
// discovered after it. gsub and gmatch collect every match before returning, so
// the match set is charged too.
//
// The budget is cumulative rather than live: bytes charged are never refunded,
// because a host cannot see when Lua's garbage collector frees a string. That is
// a deliberate over-approximation — cumulative allocation bounds peak live
// memory from above — and it is stated in [Limits] rather than implied.
//
// # Why the instrumentation cannot be evaded
//
// The charge functions are globals whose names are a single control byte
// followed by a letter. Lua's lexer cannot produce such an identifier, so no
// plugin can name one to shadow it, and the sandbox exposes no `_G`, no
// `getfenv`/`setfenv`, no `load`/`loadstring`/`dofile` and no `require`, so
// there is no way to reach the globals table by value either. Nor can a plugin
// compile uninstrumented code: every path that turns text into a function is
// removed. TestGlobalSurfaceIsExactlyTheAllowlist walks everything reachable and
// asserts it.
const (
	gasGlobal = "\x01g"
	catGlobal = "\x01c"
)

// sizeGlobal is `<size>(v)`: charge for v's length, then hand v back unchanged.
//
// # The hole it closes
//
// Three VM operators do work proportional to a string's length for a single
// instruction's charge: equality, ordering, and indexing a table by a string
// key. Measured against 64 KiB operands, with a hook spending the whole 5 M
// default budget and a wall clock pushed out of reach — unpriced, then priced:
//
//	s == s2         753 ms →  1 ms      t[s]            3.16 s →  2 ms
//	s < s2         40.07 s  → 68 ms     t[s] = 1        7.17 s →  4 ms
//	rawequal(s,s2)  803 ms  →  1 ms     empty Lua loop   128 ms → 130 ms
//
// The length is the caller's — §10.5b's masking filter exists to look at request
// text — so a hook that merely compares or indexes by something a caller sent
// could buy three hundred times its ceiling. The ordering row is the worst by
// far because gopher-lua's strCmp compares one byte at a time in a four-branch
// loop rather than through Go's vectorised primitives.
//
// # Why the operand and not the operator
//
// `..` is closed by replacing the operator with a host call. Doing that here
// would mean carrying `__eq`, `__lt`, `__le` and `__index` into the host — a
// reimplementation of four metamethod dispatches, and gopher-lua exports no
// entry point for `<=` at all — and it *still* would not cover `t[k] = v`, whose
// AttrGetExpr is an assignment target and cannot become a call.
//
// Wrapping an operand instead leaves every operator in the VM, so the
// metamethods keep their exact semantics with nothing to keep in step, and the
// same wrapper serves reads, writes and table constructors. Soundness comes from
// the shape of the work rather than from arithmetic: comparing two strings costs
// at most min(len) — Go's `==` compares lengths first and strCmp stops at the
// shorter — so charging *either* operand's length is an over-approximation, and
// hashing a key costs len(key).
//
// # Why most sites are not wrapped at all
//
// A site is wrapped only when neither side's cost is already fixed at load. If
// one operand is a literal, the work is bounded by that literal's length, which
// is bounded by the source; if one operand cannot be a string — `#x`, `not x`, a
// comparison, a table or function constructor — the operation is O(1). So
// `req.model == "gpt-4"`, `t.field`, `t[i]`, `#text > 0` and `i <= n` are
// untouched, which is nearly every comparison and nearly every index a plugin
// writes. TestFilterPluginPaysNothingForTheCharge asserts that the shipped
// masking filter is wrapped in zero places.
const sizeGlobal = "\x01s"

// cmpBytesPerGas is how many bytes of a string operand cost one unit of the
// instruction budget.
//
// The exchange rate is the same argument luapattern.go makes: a charged
// instruction costs about 22 ns, and the slowest of the three operations —
// gopher-lua's byte-at-a-time strCmp — runs at about 1.3 ns per byte, so
// sixteen bytes buy about one instruction. Equality (vectorised) and hashing are
// an order of magnitude cheaper than that, so the rate over-charges them; a
// ceiling that is generous to the operator in the cheap direction is the right
// way round.
//
// The integer division is also the threshold the trade needs: an operand under
// sixteen bytes costs nothing, so the short comparisons that make up almost all
// of a plugin's work are free of the budget as well as of the wrapper.
const cmpBytesPerGas = 16

// instrumentChunk rewrites a parsed chunk so that it charges for itself.
func instrumentChunk(stmts []ast.Stmt) []ast.Stmt {
	return instrumentBlock(stmts)
}

// instrumentBlock rewrites a block that may execute repeatedly, so it opens with
// a charge for its own static size.
func instrumentBlock(stmts []ast.Stmt) []ast.Stmt {
	line := 0
	if len(stmts) > 0 {
		line = stmts[0].Line()
	}
	out := make([]ast.Stmt, 0, len(stmts)+1)
	out = append(out, tick(blockCost(stmts), line))
	return append(out, rewriteStmts(stmts)...)
}

// rewriteStmts rewrites a block that runs at most once per enclosing charge, so
// it needs no charge of its own.
func rewriteStmts(stmts []ast.Stmt) []ast.Stmt {
	out := make([]ast.Stmt, 0, len(stmts))
	for _, s := range stmts {
		out = append(out, rewriteStmt(s)...)
	}
	return out
}

func rewriteStmt(s ast.Stmt) []ast.Stmt {
	switch st := s.(type) {
	case *ast.AssignStmt:
		rewriteExprs(st.Lhs)
		rewriteExprs(st.Rhs)
	case *ast.LocalAssignStmt:
		rewriteExprs(st.Exprs)
	case *ast.FuncCallStmt:
		st.Expr = rewriteExpr(st.Expr)
	case *ast.DoBlockStmt:
		st.Stmts = rewriteStmts(st.Stmts)
	case *ast.WhileStmt:
		st.Condition = rewriteExpr(st.Condition)
		st.Stmts = instrumentBlock(st.Stmts)
	case *ast.RepeatStmt:
		st.Condition = rewriteExpr(st.Condition)
		st.Stmts = instrumentBlock(st.Stmts)
	case *ast.IfStmt:
		st.Condition = rewriteExpr(st.Condition)
		st.Then = rewriteStmts(st.Then)
		st.Else = rewriteStmts(st.Else)
	case *ast.NumberForStmt:
		st.Init = rewriteExpr(st.Init)
		st.Limit = rewriteExpr(st.Limit)
		if st.Step != nil {
			st.Step = rewriteExpr(st.Step)
		}
		st.Stmts = instrumentBlock(st.Stmts)
	case *ast.GenericForStmt:
		rewriteExprs(st.Exprs)
		st.Stmts = instrumentBlock(st.Stmts)
	case *ast.FuncDefStmt:
		st.Func = rewriteExpr(st.Func).(*ast.FunctionExpr)
	case *ast.ReturnStmt:
		rewriteExprs(st.Exprs)
	case *ast.GotoStmt:
		// A backward goto is a loop with no loop statement, so the charge goes
		// in front of the jump rather than at the top of a body there isn't one
		// of.
		return []ast.Stmt{tick(1, st.Line()), st}
	}
	return []ast.Stmt{s}
}

func rewriteExprs(es []ast.Expr) {
	for i := range es {
		es[i] = rewriteExpr(es[i])
	}
}

func rewriteExpr(e ast.Expr) ast.Expr {
	switch ex := e.(type) {
	case *ast.StringConcatOpExpr:
		ex.Lhs = rewriteExpr(ex.Lhs)
		ex.Rhs = rewriteExpr(ex.Rhs)
		call := &ast.FuncCallExpr{
			Func: ident(catGlobal, ex.Line()),
			Args: []ast.Expr{ex.Lhs, ex.Rhs},
		}
		call.SetLine(ex.Line())
		call.SetLastLine(ex.LastLine())
		return call
	case *ast.AttrGetExpr:
		// The key and not the whole expression: an AttrGetExpr is also an
		// assignment target, and `t[k] = v` hashes k exactly as `t[k]` does.
		// Replacing the node with a call would compile the read and leave the
		// write — the more expensive of the two — unpriced.
		ex.Object = rewriteExpr(ex.Object)
		ex.Key = chargeSize(rewriteExpr(ex.Key))
	case *ast.TableExpr:
		for _, f := range ex.Fields {
			if f.Key != nil {
				f.Key = chargeSize(rewriteExpr(f.Key))
			}
			f.Value = rewriteExpr(f.Value)
		}
	case *ast.FuncCallExpr:
		if ex.Func != nil {
			ex.Func = rewriteExpr(ex.Func)
		}
		if ex.Receiver != nil {
			ex.Receiver = rewriteExpr(ex.Receiver)
		}
		rewriteExprs(ex.Args)
	case *ast.LogicalOpExpr:
		ex.Lhs = rewriteExpr(ex.Lhs)
		ex.Rhs = rewriteExpr(ex.Rhs)
	case *ast.RelationalOpExpr:
		ex.Lhs = rewriteExpr(ex.Lhs)
		ex.Rhs = rewriteExpr(ex.Rhs)
		// One side is enough: a string comparison costs at most the shorter
		// operand, so either length bounds it. If the *other* side already has a
		// fixed cost there is nothing to bound and the site stays untouched.
		if !fixedCost(ex.Rhs) {
			ex.Lhs = chargeSize(ex.Lhs)
		}
	case *ast.ArithmeticOpExpr:
		ex.Lhs = rewriteExpr(ex.Lhs)
		ex.Rhs = rewriteExpr(ex.Rhs)
	case *ast.UnaryMinusOpExpr:
		ex.Expr = rewriteExpr(ex.Expr)
	case *ast.UnaryNotOpExpr:
		ex.Expr = rewriteExpr(ex.Expr)
	case *ast.UnaryLenOpExpr:
		ex.Expr = rewriteExpr(ex.Expr)
	case *ast.FunctionExpr:
		ex.Stmts = instrumentBlock(ex.Stmts)
	}
	return e
}

// chargeSize wraps an expression in `<size>(e)` unless its cost is already fixed
// at load. The wrapper is transparent: it returns its argument.
//
// One value, deliberately. A call in an argument position expands to every value
// it returns, but a table key and a comparison operand each take exactly one, so
// truncating here is what plain Lua does a moment later anyway.
func chargeSize(e ast.Expr) ast.Expr {
	if fixedCost(e) {
		return e
	}
	call := &ast.FuncCallExpr{
		Func: ident(sizeGlobal, e.Line()),
		Args: []ast.Expr{e},
	}
	call.SetLine(e.Line())
	call.SetLastLine(e.LastLine())
	return call
}

// fixedCost reports that comparing against, or indexing by, this expression
// costs an amount decided when the plugin loaded.
//
// Two ways to qualify, and both are needed:
//
//   - A literal. Its length is its own, and the source it came from is capped at
//     [maxSourceBytes]. `req.model == "gpt-4"` compares at most five bytes
//     however long req.model is, because both `==` and strCmp stop at the
//     shorter operand; `t["family"]` hashes six.
//   - A value that cannot be a string. Comparison in Lua is typed — `equals`
//     returns false on a type mismatch without looking at either value, and
//     `lessThan` raises — so if one side is a number, a boolean, a fresh table
//     or a function, no string is ever walked. Indexing by one hashes a machine
//     word.
//
// `#x` is in the second class because OP_LEN always produces a number, even
// through `__len`. `-x` and `a + b` are *not*, because `__unm` and `__add` are
// plugin functions that may return anything; a metamethod's own body is charged
// Lua, but its result would arrive here unpriced.
func fixedCost(e ast.Expr) bool {
	switch e.(type) {
	case *ast.StringExpr, *ast.NumberExpr, *ast.TrueExpr, *ast.FalseExpr, *ast.NilExpr:
		return true
	case *ast.UnaryLenOpExpr, *ast.UnaryNotOpExpr, *ast.RelationalOpExpr:
		return true
	case *ast.TableExpr, *ast.FunctionExpr:
		return true
	}
	return false
}

func ident(name string, line int) *ast.IdentExpr {
	e := &ast.IdentExpr{Value: name}
	e.SetLine(line)
	e.SetLastLine(line)
	return e
}

// tick builds `<gas>(n)`.
func tick(n, line int) ast.Stmt {
	num := &ast.NumberExpr{Value: strconv.Itoa(n)}
	num.SetLine(line)
	num.SetLastLine(line)
	call := &ast.FuncCallExpr{Func: ident(gasGlobal, line), Args: []ast.Expr{num}}
	call.SetLine(line)
	call.SetLastLine(line)
	st := &ast.FuncCallStmt{Expr: call}
	st.SetLine(line)
	st.SetLastLine(line)
	return st
}

// blockCost is the static size of a block, counting nested conditionals and do
// blocks — which run under this block's charge — but not loop bodies or function
// bodies, which charge themselves.
func blockCost(stmts []ast.Stmt) int {
	n := 1
	for _, s := range stmts {
		n++
		switch st := s.(type) {
		case *ast.IfStmt:
			n += blockCost(st.Then) + blockCost(st.Else)
		case *ast.DoBlockStmt:
			n += blockCost(st.Stmts)
		case *ast.AssignStmt:
			n += len(st.Lhs) + len(st.Rhs)
		case *ast.LocalAssignStmt:
			n += len(st.Exprs)
		case *ast.ReturnStmt:
			n += len(st.Exprs)
		}
	}
	return n
}
