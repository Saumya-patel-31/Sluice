package policy

import "strings"

// Expr is a node in a compiled policy expression.
type Expr interface {
	// Eval computes the node's value in an environment.
	Eval(env *Env) (Value, error)
	// String renders the node back to source-equivalent text. Round-tripping
	// matters because the dashboard's policy editor shows the parsed form
	// back to the author.
	String() string
	// Pos returns the source position the node was parsed from.
	Pos() (line, col int)
}

type pos struct{ line, col int }

func (p pos) Pos() (int, int) { return p.line, p.col }

// LiteralExpr is a constant.
type LiteralExpr struct {
	pos
	V Value
}

func (e *LiteralExpr) String() string { return e.V.String() }

// IdentExpr is a root identifier such as "subject" or "backend".
type IdentExpr struct {
	pos
	Name string
}

func (e *IdentExpr) String() string { return e.Name }

// MemberExpr is dotted access: subject.trust_domain.
type MemberExpr struct {
	pos
	Recv Expr
	Name string
}

func (e *MemberExpr) String() string { return e.Recv.String() + "." + e.Name }

// IndexExpr is bracket access: subject.claims["tier"].
type IndexExpr struct {
	pos
	Recv Expr
	Key  Expr
}

func (e *IndexExpr) String() string { return e.Recv.String() + "[" + e.Key.String() + "]" }

// ListExpr is a list literal.
type ListExpr struct {
	pos
	Elems []Expr
}

func (e *ListExpr) String() string {
	parts := make([]string, 0, len(e.Elems))
	for _, x := range e.Elems {
		parts = append(parts, x.String())
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// UnaryExpr is logical negation.
type UnaryExpr struct {
	pos
	X Expr
}

func (e *UnaryExpr) String() string { return "not " + e.X.String() }

// BinaryExpr is any infix operator. Negate carries the "not in" form so the
// parser does not need a separate node type for it.
type BinaryExpr struct {
	pos
	Op     tokenKind
	Negate bool
	L, R   Expr
}

func (e *BinaryExpr) String() string {
	op := operatorText(e.Op)
	if e.Negate {
		op = "not " + op
	}
	return e.L.String() + " " + op + " " + e.R.String()
}

func operatorText(k tokenKind) string {
	switch k {
	case tokEq:
		return "=="
	case tokNotEq:
		return "!="
	case tokLess:
		return "<"
	case tokLessEq:
		return "<="
	case tokGreater:
		return ">"
	case tokGreaterEq:
		return ">="
	case tokAnd:
		return "and"
	case tokOr:
		return "or"
	case tokIn:
		return "in"
	case tokMatches:
		return "matches"
	case tokStartsWith:
		return "startswith"
	case tokEndsWith:
		return "endswith"
	case tokContains:
		return "contains"
	}
	return "?"
}

// CallExpr is a builtin function call.
type CallExpr struct {
	pos
	Name string
	Args []Expr
}

func (e *CallExpr) String() string {
	parts := make([]string, 0, len(e.Args))
	for _, a := range e.Args {
		parts = append(parts, a.String())
	}
	return e.Name + "(" + strings.Join(parts, ", ") + ")"
}
