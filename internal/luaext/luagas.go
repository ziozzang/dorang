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
		ex.Object = rewriteExpr(ex.Object)
		ex.Key = rewriteExpr(ex.Key)
	case *ast.TableExpr:
		for _, f := range ex.Fields {
			if f.Key != nil {
				f.Key = rewriteExpr(f.Key)
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
