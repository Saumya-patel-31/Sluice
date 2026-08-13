package policy

import (
	"fmt"
	"strings"
)

// Binding powers for the Pratt expression parser. Higher binds tighter.
const (
	bpNone    = 0
	bpOr      = 1
	bpAnd     = 2
	bpCompare = 3
)

var infixBindingPower = map[tokenKind]int{
	tokOr:         bpOr,
	tokAnd:        bpAnd,
	tokEq:         bpCompare,
	tokNotEq:      bpCompare,
	tokLess:       bpCompare,
	tokLessEq:     bpCompare,
	tokGreater:    bpCompare,
	tokGreaterEq:  bpCompare,
	tokIn:         bpCompare,
	tokMatches:    bpCompare,
	tokStartsWith: bpCompare,
	tokEndsWith:   bpCompare,
	tokContains:   bpCompare,
}

// parser turns tokens into policies. Expressions use Pratt (precedence
// climbing) parsing; the surrounding policy blocks are a flat keyword grammar
// parsed by hand.
type parser struct {
	toks []token
	i    int
}

func newParser(src string) (*parser, error) {
	toks, err := newLexer(src).tokens()
	if err != nil {
		return nil, err
	}
	return &parser{toks: toks}, nil
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) peekAt(n int) token {
	if p.i+n >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.i+n]
}
func (p *parser) advance() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) errAt(t token, format string, args ...any) error {
	return &SyntaxError{Line: t.line, Col: t.col, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) expect(k tokenKind) (token, error) {
	t := p.peek()
	if t.kind != k {
		return t, p.errAt(t, "expected %s, found %s", k, describeToken(t))
	}
	return p.advance(), nil
}

func describeToken(t token) string {
	switch t.kind {
	case tokIdent, tokNumber:
		return fmt.Sprintf("%q", t.text)
	case tokString:
		return fmt.Sprintf("string %q", t.text)
	case tokEOF:
		return "end of input"
	}
	return t.kind.String()
}

// -----------------------------------------------------------------------------
// Expressions
// -----------------------------------------------------------------------------

// ParseExpr compiles a single standalone expression. It is exported so the
// dashboard can validate an expression as the operator types it, without
// wrapping it in a policy block.
func ParseExpr(src string) (Expr, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	e, err := p.parseExpr(bpNone)
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tokEOF {
		return nil, p.errAt(t, "unexpected %s after expression", describeToken(t))
	}
	return e, nil
}

func (p *parser) parseExpr(minBP int) (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}

	for {
		t := p.peek()

		// "not in" reads as a prefix 'not' at an infix position; pair it up
		// here rather than complicating the lexer.
		negate := false
		if t.kind == tokNot && p.peekAt(1).kind == tokIn {
			negate = true
			p.advance()
			t = p.peek()
		}

		bp, ok := infixBindingPower[t.kind]
		if !ok || bp < minBP {
			if negate {
				return nil, p.errAt(t, "expected 'in' after 'not'")
			}
			return left, nil
		}
		p.advance()

		// Left-associative: the right operand binds strictly tighter.
		right, err := p.parseExpr(bp + 1)
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{
			pos:    pos{t.line, t.col},
			Op:     t.kind,
			Negate: negate,
			L:      left,
			R:      right,
		}
	}
}

func (p *parser) parseUnary() (Expr, error) {
	if t := p.peek(); t.kind == tokNot {
		p.advance()
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{pos: pos{t.line, t.col}, X: x}, nil
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (Expr, error) {
	e, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch p.peek().kind {
		case tokDot:
			dot := p.advance()
			name, err := p.expect(tokIdent)
			if err != nil {
				return nil, err
			}
			e = &MemberExpr{pos: pos{dot.line, dot.col}, Recv: e, Name: name.text}
		case tokLBracket:
			br := p.advance()
			key, err := p.parseExpr(bpNone)
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokRBracket); err != nil {
				return nil, err
			}
			e = &IndexExpr{pos: pos{br.line, br.col}, Recv: e, Key: key}
		default:
			return e, nil
		}
	}
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	at := pos{t.line, t.col}

	switch t.kind {
	case tokNumber:
		p.advance()
		return &LiteralExpr{pos: at, V: Num(t.num)}, nil

	case tokString:
		p.advance()
		return &LiteralExpr{pos: at, V: Str(t.text)}, nil

	case tokLParen:
		p.advance()
		e, err := p.parseExpr(bpNone)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen); err != nil {
			return nil, err
		}
		return e, nil

	case tokLBracket:
		p.advance()
		var elems []Expr
		for p.peek().kind != tokRBracket {
			e, err := p.parseExpr(bpNone)
			if err != nil {
				return nil, err
			}
			elems = append(elems, e)
			if p.peek().kind == tokComma {
				p.advance()
				continue
			}
			break
		}
		if _, err := p.expect(tokRBracket); err != nil {
			return nil, err
		}
		return &ListExpr{pos: at, Elems: elems}, nil

	case tokIdent:
		p.advance()
		switch strings.ToLower(t.text) {
		case "true":
			return &LiteralExpr{pos: at, V: Bool(true)}, nil
		case "false":
			return &LiteralExpr{pos: at, V: Bool(false)}, nil
		case "null":
			return &LiteralExpr{pos: at, V: Null}, nil
		}
		// A '(' immediately after an identifier makes it a builtin call.
		if p.peek().kind == tokLParen {
			if _, ok := builtins[t.text]; !ok {
				return nil, p.errAt(t, "unknown function %q (available: %s)", t.text, builtinNames())
			}
			p.advance()
			var args []Expr
			for p.peek().kind != tokRParen {
				a, err := p.parseExpr(bpNone)
				if err != nil {
					return nil, err
				}
				args = append(args, a)
				if p.peek().kind == tokComma {
					p.advance()
					continue
				}
				break
			}
			if _, err := p.expect(tokRParen); err != nil {
				return nil, err
			}
			return &CallExpr{pos: at, Name: t.text, Args: args}, nil
		}
		return &IdentExpr{pos: at, Name: t.text}, nil
	}

	return nil, p.errAt(t, "unexpected %s in expression", describeToken(t))
}

// -----------------------------------------------------------------------------
// Policy blocks
// -----------------------------------------------------------------------------

// Parse compiles a policy document into an ordered set of policies.
func Parse(src string) ([]*Policy, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	var out []*Policy
	for p.peek().kind != tokEOF {
		t := p.peek()
		if t.kind != tokIdent || !strings.EqualFold(t.text, "policy") {
			return nil, p.errAt(t, "expected 'policy', found %s", describeToken(t))
		}
		p.advance()
		pol, err := p.parsePolicyBody()
		if err != nil {
			return nil, err
		}
		out = append(out, pol)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("policy: document contains no policies")
	}
	return out, nil
}

func (p *parser) parsePolicyBody() (*Policy, error) {
	nameTok, err := p.expect(tokString)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokLBrace); err != nil {
		return nil, err
	}

	pol := &Policy{Name: nameTok.text, Priority: 500, Effect: EffectDeny}
	seen := map[string]bool{}

	for p.peek().kind != tokRBrace {
		key := p.peek()
		if key.kind == tokEOF {
			return nil, p.errAt(key, "unterminated policy %q", pol.Name)
		}
		if key.kind != tokIdent {
			return nil, p.errAt(key, "expected a clause keyword, found %s", describeToken(key))
		}
		p.advance()

		clause := strings.ToLower(key.text)
		if seen[clause] {
			return nil, p.errAt(key, "duplicate %q clause in policy %q", clause, pol.Name)
		}
		seen[clause] = true

		switch clause {
		case "description":
			t, err := p.expect(tokString)
			if err != nil {
				return nil, err
			}
			pol.Description = t.text

		case "message":
			t, err := p.expect(tokString)
			if err != nil {
				return nil, err
			}
			pol.Message = t.text

		case "priority":
			t, err := p.expect(tokNumber)
			if err != nil {
				return nil, err
			}
			pol.Priority = int(t.num)

		case "effect":
			t, err := p.expect(tokIdent)
			if err != nil {
				return nil, err
			}
			eff, ok := parseEffect(t.text)
			if !ok {
				return nil, p.errAt(t, "unknown effect %q (want allow, deny, constrain or prefer)", t.text)
			}
			pol.Effect = eff

		case "when":
			e, err := p.parseExpr(bpNone)
			if err != nil {
				return nil, err
			}
			pol.When = e

		case "require":
			e, err := p.parseExpr(bpNone)
			if err != nil {
				return nil, err
			}
			pol.Require = e

		case "tags":
			e, err := p.parseExpr(bpNone)
			if err != nil {
				return nil, err
			}
			lst, ok := e.(*ListExpr)
			if !ok {
				return nil, p.errAt(key, "tags must be a list of strings")
			}
			for _, el := range lst.Elems {
				lit, ok := el.(*LiteralExpr)
				if !ok || lit.V.Kind != KindString {
					return nil, p.errAt(key, "tags must be a list of string literals")
				}
				pol.Tags = append(pol.Tags, lit.V.S)
			}

		case "prefer":
			w, err := p.parseWeightBlock()
			if err != nil {
				return nil, err
			}
			pol.Prefer = w

		default:
			return nil, p.errAt(key, "unknown clause %q in policy %q", key.text, pol.Name)
		}
	}
	if _, err := p.expect(tokRBrace); err != nil {
		return nil, err
	}

	if err := pol.validate(); err != nil {
		return nil, &SyntaxError{Line: nameTok.line, Col: nameTok.col, Msg: err.Error()}
	}
	return pol, nil
}

// parseWeightBlock reads `{ latency: 0.6, cost: 0.2 }`.
func (p *parser) parseWeightBlock() (map[string]float64, error) {
	if _, err := p.expect(tokLBrace); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for p.peek().kind != tokRBrace {
		k, err := p.expect(tokIdent)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokColon); err != nil {
			return nil, err
		}
		v, err := p.expect(tokNumber)
		if err != nil {
			return nil, err
		}
		if v.num < 0 {
			return nil, p.errAt(v, "objective weight for %q must not be negative", k.text)
		}
		out[strings.ToLower(k.text)] = v.num
		if p.peek().kind == tokComma {
			p.advance()
		}
	}
	if _, err := p.expect(tokRBrace); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("policy: empty prefer block")
	}
	return out, nil
}
