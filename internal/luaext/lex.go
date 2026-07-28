package luaext

import (
	"fmt"
	"strconv"
	"strings"
)

// The policy language's lexer.
//
// The grammar is one statement per line, which is why newline is a token rather
// than whitespace: a rule that runs off the end of its line is a syntax error at
// load time instead of a surprise at request time.

type tokKind uint8

const (
	tokEOF tokKind = iota
	tokNewline
	tokIdent
	tokNumber
	tokString
	tokLParen
	tokRParen
	tokLBracket
	tokRBracket
	tokComma
	tokAssign
	tokEq
	tokNe
	tokLt
	tokLe
	tokGt
	tokGe
)

func (k tokKind) String() string {
	switch k {
	case tokEOF:
		return "end of file"
	case tokNewline:
		return "end of line"
	case tokIdent:
		return "identifier"
	case tokNumber:
		return "number"
	case tokString:
		return "string"
	case tokLParen:
		return "("
	case tokRParen:
		return ")"
	case tokLBracket:
		return "["
	case tokRBracket:
		return "]"
	case tokComma:
		return ","
	case tokAssign:
		return "="
	case tokEq:
		return "=="
	case tokNe:
		return "!="
	case tokLt:
		return "<"
	case tokLe:
		return "<="
	case tokGt:
		return ">"
	case tokGe:
		return ">="
	}
	return "token"
}

type token struct {
	kind tokKind
	text string
	num  int64
	line int
}

// maxSourceBytes caps one policy file. A ceiling on program size is the load
// time half of the memory ceiling: a program that cannot be large cannot hold
// much live at once either.
const maxSourceBytes = 256 << 10

type lexer struct {
	src  string
	pos  int
	line int
}

func newLexer(src string) *lexer { return &lexer{src: src, line: 1} }

// lex tokenizes the whole source. Policy files are small and loaded once, so
// tokenizing eagerly costs nothing worth streaming for.
func (l *lexer) lex() ([]token, error) {
	var out []token
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		if t.kind == tokEOF {
			return out, nil
		}
	}
}

func (l *lexer) errf(format string, args ...any) error {
	return fmt.Errorf("line %d: "+format, append([]any{l.line}, args...)...)
}

func (l *lexer) next() (token, error) {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			l.pos++
			continue
		case c == '\n':
			t := token{kind: tokNewline, line: l.line}
			l.pos++
			l.line++
			return t, nil
		case c == '#':
			l.skipLine()
			continue
		case c == '-' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '-':
			// Lua's comment marker is accepted too: an operator who reaches for
			// this file after reading §11.5 will type it.
			l.skipLine()
			continue
		}
		break
	}
	if l.pos >= len(l.src) {
		return token{kind: tokEOF, line: l.line}, nil
	}

	start := l.pos
	c := l.src[l.pos]
	switch {
	case isIdentStart(c):
		for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
			l.pos++
		}
		return token{kind: tokIdent, text: l.src[start:l.pos], line: l.line}, nil

	case c >= '0' && c <= '9', c == '-':
		l.pos++
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
		text := l.src[start:l.pos]
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return token{}, l.errf("%q is not an integer (the policy language has no floating point; "+
				"money is in nano-USD and durations in milliseconds)", text)
		}
		return token{kind: tokNumber, num: n, text: text, line: l.line}, nil

	case c == '"':
		return l.lexString()

	case c == '(':
		l.pos++
		return token{kind: tokLParen, line: l.line}, nil
	case c == ')':
		l.pos++
		return token{kind: tokRParen, line: l.line}, nil
	case c == '[':
		l.pos++
		return token{kind: tokLBracket, line: l.line}, nil
	case c == ']':
		l.pos++
		return token{kind: tokRBracket, line: l.line}, nil
	case c == ',':
		l.pos++
		return token{kind: tokComma, line: l.line}, nil
	case c == '=':
		l.pos++
		if l.pos < len(l.src) && l.src[l.pos] == '=' {
			l.pos++
			return token{kind: tokEq, line: l.line}, nil
		}
		return token{kind: tokAssign, line: l.line}, nil
	case c == '!':
		l.pos++
		if l.pos < len(l.src) && l.src[l.pos] == '=' {
			l.pos++
			return token{kind: tokNe, line: l.line}, nil
		}
		return token{}, l.errf("unexpected %q: did you mean != ?", "!")
	case c == '~':
		// Lua spells inequality ~=; accepting it costs one branch and saves a
		// confusing error.
		l.pos++
		if l.pos < len(l.src) && l.src[l.pos] == '=' {
			l.pos++
			return token{kind: tokNe, line: l.line}, nil
		}
		return token{}, l.errf("unexpected %q", "~")
	case c == '<':
		l.pos++
		if l.pos < len(l.src) && l.src[l.pos] == '=' {
			l.pos++
			return token{kind: tokLe, line: l.line}, nil
		}
		return token{kind: tokLt, line: l.line}, nil
	case c == '>':
		l.pos++
		if l.pos < len(l.src) && l.src[l.pos] == '=' {
			l.pos++
			return token{kind: tokGe, line: l.line}, nil
		}
		return token{kind: tokGt, line: l.line}, nil
	}
	return token{}, l.errf("unexpected character %q", string(rune(c)))
}

func (l *lexer) skipLine() {
	for l.pos < len(l.src) && l.src[l.pos] != '\n' {
		l.pos++
	}
}

func (l *lexer) lexString() (token, error) {
	l.pos++ // opening quote
	var sb strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case '"':
			l.pos++
			return token{kind: tokString, text: sb.String(), line: l.line}, nil
		case '\n':
			return token{}, l.errf("unterminated string")
		case '\\':
			l.pos++
			if l.pos >= len(l.src) {
				return token{}, l.errf("unterminated escape")
			}
			switch l.src[l.pos] {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case '\\':
				sb.WriteByte('\\')
			case '"':
				sb.WriteByte('"')
			default:
				return token{}, l.errf("unknown escape \\%s", string(rune(l.src[l.pos])))
			}
			l.pos++
		default:
			sb.WriteByte(c)
			l.pos++
		}
	}
	return token{}, l.errf("unterminated string")
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}
