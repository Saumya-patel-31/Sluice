package policy

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// tokenKind enumerates lexical token types.
type tokenKind uint8

const (
	tokEOF tokenKind = iota
	tokIdent
	tokNumber
	tokString

	tokLParen
	tokRParen
	tokLBrace
	tokRBrace
	tokLBracket
	tokRBracket
	tokComma
	tokColon
	tokDot

	tokEq
	tokNotEq
	tokLess
	tokLessEq
	tokGreater
	tokGreaterEq
	tokAnd
	tokOr
	tokNot

	// Keyword operators.
	tokIn
	tokMatches
	tokStartsWith
	tokEndsWith
	tokContains
)

func (k tokenKind) String() string {
	switch k {
	case tokEOF:
		return "end of input"
	case tokIdent:
		return "identifier"
	case tokNumber:
		return "number"
	case tokString:
		return "string"
	case tokLParen:
		return "'('"
	case tokRParen:
		return "')'"
	case tokLBrace:
		return "'{'"
	case tokRBrace:
		return "'}'"
	case tokLBracket:
		return "'['"
	case tokRBracket:
		return "']'"
	case tokComma:
		return "','"
	case tokColon:
		return "':'"
	case tokDot:
		return "'.'"
	case tokEq:
		return "'=='"
	case tokNotEq:
		return "'!='"
	case tokLess:
		return "'<'"
	case tokLessEq:
		return "'<='"
	case tokGreater:
		return "'>'"
	case tokGreaterEq:
		return "'>='"
	case tokAnd:
		return "'and'"
	case tokOr:
		return "'or'"
	case tokNot:
		return "'not'"
	case tokIn:
		return "'in'"
	case tokMatches:
		return "'matches'"
	case tokStartsWith:
		return "'startswith'"
	case tokEndsWith:
		return "'endswith'"
	case tokContains:
		return "'contains'"
	}
	return "token"
}

// token is one lexical unit with its source position.
type token struct {
	kind tokenKind
	text string
	num  float64
	line int
	col  int
}

// Position renders "line:col" for diagnostics.
func (t token) Position() string { return fmt.Sprintf("%d:%d", t.line, t.col) }

// SyntaxError carries a source position so a bad policy file points at the
// offending line rather than failing opaquely at startup.
type SyntaxError struct {
	Line, Col int
	Msg       string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("policy: line %d col %d: %s", e.Line, e.Col, e.Msg)
}

var keywordOperators = map[string]tokenKind{
	"and":        tokAnd,
	"or":         tokOr,
	"not":        tokNot,
	"in":         tokIn,
	"matches":    tokMatches,
	"startswith": tokStartsWith,
	"endswith":   tokEndsWith,
	"contains":   tokContains,
}

// lexer converts policy source into tokens.
type lexer struct {
	src  string
	pos  int
	line int
	col  int
}

func newLexer(src string) *lexer { return &lexer{src: src, line: 1, col: 1} }

func (l *lexer) errf(format string, args ...any) *SyntaxError {
	return &SyntaxError{Line: l.line, Col: l.col, Msg: fmt.Sprintf(format, args...)}
}

func (l *lexer) peekRune() (rune, int) {
	if l.pos >= len(l.src) {
		return 0, 0
	}
	return utf8.DecodeRuneInString(l.src[l.pos:])
}

func (l *lexer) advance(n int) {
	for i := 0; i < n && l.pos < len(l.src); {
		r, w := utf8.DecodeRuneInString(l.src[l.pos:])
		if r == '\n' {
			l.line++
			l.col = 1
		} else {
			l.col++
		}
		l.pos += w
		i += w
	}
}

func (l *lexer) skipSpaceAndComments() error {
	for l.pos < len(l.src) {
		r, w := l.peekRune()
		switch {
		case unicode.IsSpace(r):
			l.advance(w)
		case r == '#':
			l.skipLine()
		case r == '/' && strings.HasPrefix(l.src[l.pos:], "//"):
			l.skipLine()
		case r == '/' && strings.HasPrefix(l.src[l.pos:], "/*"):
			end := strings.Index(l.src[l.pos+2:], "*/")
			if end < 0 {
				return l.errf("unterminated block comment")
			}
			l.advance(end + 4)
		default:
			return nil
		}
	}
	return nil
}

func (l *lexer) skipLine() {
	for l.pos < len(l.src) {
		r, w := l.peekRune()
		l.advance(w)
		if r == '\n' {
			return
		}
	}
}

// tokens lexes the entire source.
func (l *lexer) tokens() ([]token, error) {
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

func (l *lexer) next() (token, error) {
	if err := l.skipSpaceAndComments(); err != nil {
		return token{}, err
	}
	line, col := l.line, l.col
	if l.pos >= len(l.src) {
		return token{kind: tokEOF, line: line, col: col}, nil
	}

	mk := func(k tokenKind, text string) token {
		return token{kind: k, text: text, line: line, col: col}
	}

	rest := l.src[l.pos:]

	// Two-character operators first so '<' does not shadow '<='.
	for _, op := range []struct {
		lit  string
		kind tokenKind
	}{
		{"==", tokEq}, {"!=", tokNotEq}, {"<=", tokLessEq}, {">=", tokGreaterEq},
		{"&&", tokAnd}, {"||", tokOr},
	} {
		if strings.HasPrefix(rest, op.lit) {
			l.advance(len(op.lit))
			return mk(op.kind, op.lit), nil
		}
	}

	r, w := l.peekRune()
	switch r {
	case '(':
		l.advance(w)
		return mk(tokLParen, "("), nil
	case ')':
		l.advance(w)
		return mk(tokRParen, ")"), nil
	case '{':
		l.advance(w)
		return mk(tokLBrace, "{"), nil
	case '}':
		l.advance(w)
		return mk(tokRBrace, "}"), nil
	case '[':
		l.advance(w)
		return mk(tokLBracket, "["), nil
	case ']':
		l.advance(w)
		return mk(tokRBracket, "]"), nil
	case ',':
		l.advance(w)
		return mk(tokComma, ","), nil
	case ':':
		l.advance(w)
		return mk(tokColon, ":"), nil
	case '.':
		l.advance(w)
		return mk(tokDot, "."), nil
	case '<':
		l.advance(w)
		return mk(tokLess, "<"), nil
	case '>':
		l.advance(w)
		return mk(tokGreater, ">"), nil
	case '!':
		l.advance(w)
		return mk(tokNot, "!"), nil
	case '"', '\'':
		return l.lexString(r)
	}

	if r >= '0' && r <= '9' {
		return l.lexNumber()
	}
	if r == '_' || unicode.IsLetter(r) {
		return l.lexIdent()
	}
	return token{}, l.errf("unexpected character %q", r)
}

func (l *lexer) lexString(quote rune) (token, error) {
	line, col := l.line, l.col
	l.advance(utf8.RuneLen(quote))

	var sb strings.Builder
	for {
		if l.pos >= len(l.src) {
			return token{}, &SyntaxError{Line: line, Col: col, Msg: "unterminated string literal"}
		}
		r, w := l.peekRune()
		if r == quote {
			l.advance(w)
			return token{kind: tokString, text: sb.String(), line: line, col: col}, nil
		}
		if r == '\\' {
			l.advance(w)
			e, ew := l.peekRune()
			switch e {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '\\', '"', '\'':
				sb.WriteRune(e)
			default:
				return token{}, l.errf("unknown escape sequence \\%c", e)
			}
			l.advance(ew)
			continue
		}
		sb.WriteRune(r)
		l.advance(w)
	}
}

func (l *lexer) lexNumber() (token, error) {
	line, col := l.line, l.col
	start := l.pos
	seenDot := false
	for l.pos < len(l.src) {
		r, w := l.peekRune()
		if r >= '0' && r <= '9' {
			l.advance(w)
			continue
		}
		if r == '.' && !seenDot && l.pos+1 < len(l.src) &&
			l.src[l.pos+1] >= '0' && l.src[l.pos+1] <= '9' {
			seenDot = true
			l.advance(w)
			continue
		}
		break
	}
	text := l.src[start:l.pos]
	var f float64
	if _, err := fmt.Sscanf(text, "%g", &f); err != nil {
		return token{}, &SyntaxError{Line: line, Col: col, Msg: "malformed number " + text}
	}
	return token{kind: tokNumber, text: text, num: f, line: line, col: col}, nil
}

func (l *lexer) lexIdent() (token, error) {
	line, col := l.line, l.col
	start := l.pos
	for l.pos < len(l.src) {
		r, w := l.peekRune()
		if r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			l.advance(w)
			continue
		}
		break
	}
	text := l.src[start:l.pos]

	// "not in" is two words but one operator; the parser handles the pairing,
	// so the lexer only needs to emit the keyword tokens.
	if k, ok := keywordOperators[text]; ok {
		return token{kind: k, text: text, line: line, col: col}, nil
	}
	return token{kind: tokIdent, text: text, line: line, col: col}, nil
}
