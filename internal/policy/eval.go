package policy

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// EvalError carries the source position of the failing node so an operator can
// find the clause that broke.
type EvalError struct {
	Line, Col int
	Msg       string
}

func (e *EvalError) Error() string {
	return fmt.Sprintf("policy: line %d col %d: %s", e.Line, e.Col, e.Msg)
}

func evalErr(e Expr, format string, args ...any) error {
	l, c := e.Pos()
	return &EvalError{Line: l, Col: c, Msg: fmt.Sprintf(format, args...)}
}

// Eval returns the literal value.
func (e *LiteralExpr) Eval(*Env) (Value, error) { return e.V, nil }

// Eval resolves a root identifier from the environment.
func (e *IdentExpr) Eval(env *Env) (Value, error) {
	v, ok := env.Lookup(e.Name)
	if !ok {
		return Null, evalErr(e, "unknown identifier %q (available: %s)", e.Name, env.RootNames())
	}
	return v, nil
}

// Eval performs dotted member access.
//
// Access to a top-level root's field is strict: `subject.trustdomain` is a
// typo, not an absent value, and silently evaluating it to null would make the
// enclosing policy quietly stop matching. Access to a nested map is lenient,
// because those hold genuinely optional data such as JWT claims.
func (e *MemberExpr) Eval(env *Env) (Value, error) {
	recv, err := e.Recv.Eval(env)
	if err != nil {
		return Null, err
	}
	switch recv.Kind {
	case KindMap:
		if v, ok := recv.M[e.Name]; ok {
			return v, nil
		}
		if _, isRoot := e.Recv.(*IdentExpr); isRoot {
			keys := make([]string, 0, len(recv.M))
			for k := range recv.M {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return Null, evalErr(e, "%s has no field %q (available: %s)",
				e.Recv.String(), e.Name, strings.Join(keys, ", "))
		}
		return Null, nil
	case KindNull:
		return Null, nil
	}
	return Null, evalErr(e, "cannot read field %q from a %s", e.Name, recv.Kind)
}

// Eval performs bracket indexing.
func (e *IndexExpr) Eval(env *Env) (Value, error) {
	recv, err := e.Recv.Eval(env)
	if err != nil {
		return Null, err
	}
	key, err := e.Key.Eval(env)
	if err != nil {
		return Null, err
	}
	v, err := recv.Index(key)
	if err != nil {
		return Null, evalErr(e, "%v", err)
	}
	return v, nil
}

// Eval builds a list value.
func (e *ListExpr) Eval(env *Env) (Value, error) {
	out := make([]Value, 0, len(e.Elems))
	for _, x := range e.Elems {
		v, err := x.Eval(env)
		if err != nil {
			return Null, err
		}
		out = append(out, v)
	}
	return Value{Kind: KindList, L: out}, nil
}

// Eval negates a boolean.
func (e *UnaryExpr) Eval(env *Env) (Value, error) {
	v, err := e.X.Eval(env)
	if err != nil {
		return Null, err
	}
	b, err := v.Truthy()
	if err != nil {
		return Null, evalErr(e, "'not' %v", err)
	}
	return Bool(!b), nil
}

// Eval applies an infix operator. Logical operators short-circuit.
func (e *BinaryExpr) Eval(env *Env) (Value, error) {
	// Short-circuit before evaluating the right side. This is not only a
	// performance choice: it lets a policy write
	// `subject.mtls and subject.claims["spiffe"] startswith "..."` without the
	// right side erroring on an anonymous request.
	if e.Op == tokAnd || e.Op == tokOr {
		lv, err := e.L.Eval(env)
		if err != nil {
			return Null, err
		}
		lb, err := lv.Truthy()
		if err != nil {
			return Null, evalErr(e, "left side of %s: %v", operatorText(e.Op), err)
		}
		if (e.Op == tokAnd && !lb) || (e.Op == tokOr && lb) {
			return Bool(lb), nil
		}
		rv, err := e.R.Eval(env)
		if err != nil {
			return Null, err
		}
		rb, err := rv.Truthy()
		if err != nil {
			return Null, evalErr(e, "right side of %s: %v", operatorText(e.Op), err)
		}
		return Bool(rb), nil
	}

	lv, err := e.L.Eval(env)
	if err != nil {
		return Null, err
	}
	rv, err := e.R.Eval(env)
	if err != nil {
		return Null, err
	}

	switch e.Op {
	case tokEq:
		return Bool(lv.Equal(rv)), nil
	case tokNotEq:
		return Bool(!lv.Equal(rv)), nil

	case tokLess, tokLessEq, tokGreater, tokGreaterEq:
		c, err := lv.Compare(rv)
		if err != nil {
			return Null, evalErr(e, "%v", err)
		}
		switch e.Op {
		case tokLess:
			return Bool(c < 0), nil
		case tokLessEq:
			return Bool(c <= 0), nil
		case tokGreater:
			return Bool(c > 0), nil
		default:
			return Bool(c >= 0), nil
		}

	case tokIn:
		ok, err := rv.Contains(lv)
		if err != nil {
			return Null, evalErr(e, "%v", err)
		}
		if e.Negate {
			ok = !ok
		}
		return Bool(ok), nil

	case tokContains:
		ok, err := lv.Contains(rv)
		if err != nil {
			return Null, evalErr(e, "%v", err)
		}
		return Bool(ok), nil

	case tokMatches, tokStartsWith, tokEndsWith:
		if lv.Kind == KindNull {
			return Bool(false), nil
		}
		if lv.Kind != KindString || rv.Kind != KindString {
			return Null, evalErr(e, "%s requires two strings, got %s and %s",
				operatorText(e.Op), lv.Kind, rv.Kind)
		}
		switch e.Op {
		case tokMatches:
			return Bool(Glob(rv.S, lv.S)), nil
		case tokStartsWith:
			return Bool(strings.HasPrefix(lv.S, rv.S)), nil
		default:
			return Bool(strings.HasSuffix(lv.S, rv.S)), nil
		}
	}
	return Null, evalErr(e, "unsupported operator %s", operatorText(e.Op))
}

// Eval invokes a builtin function.
func (e *CallExpr) Eval(env *Env) (Value, error) {
	fn, ok := builtins[e.Name]
	if !ok {
		return Null, evalErr(e, "unknown function %q", e.Name)
	}
	if len(e.Args) < fn.minArgs || len(e.Args) > fn.maxArgs {
		return Null, evalErr(e, "%s takes %s arguments, got %d",
			e.Name, arity(fn.minArgs, fn.maxArgs), len(e.Args))
	}
	args := make([]Value, 0, len(e.Args))
	for _, a := range e.Args {
		v, err := a.Eval(env)
		if err != nil {
			return Null, err
		}
		args = append(args, v)
	}
	v, err := fn.fn(args)
	if err != nil {
		return Null, evalErr(e, "%s: %v", e.Name, err)
	}
	return v, nil
}

func arity(lo, hi int) string {
	if lo == hi {
		return fmt.Sprintf("%d", lo)
	}
	return fmt.Sprintf("%d-%d", lo, hi)
}

// -----------------------------------------------------------------------------
// Builtins
// -----------------------------------------------------------------------------

type builtin struct {
	minArgs, maxArgs int
	doc              string
	fn               func(args []Value) (Value, error)
}

func wantString(v Value, pos int) (string, error) {
	if v.Kind == KindNull {
		return "", nil
	}
	if v.Kind != KindString {
		return "", fmt.Errorf("argument %d must be a string, got %s", pos+1, v.Kind)
	}
	return v.S, nil
}

var builtins = map[string]builtin{
	"lower": {1, 1, "lower(s) lowercases a string", func(a []Value) (Value, error) {
		s, err := wantString(a[0], 0)
		return Str(strings.ToLower(s)), err
	}},
	"upper": {1, 1, "upper(s) uppercases a string", func(a []Value) (Value, error) {
		s, err := wantString(a[0], 0)
		return Str(strings.ToUpper(s)), err
	}},
	"len": {1, 1, "len(x) is the length of a string, list or map", func(a []Value) (Value, error) {
		switch a[0].Kind {
		case KindString:
			return Num(float64(len(a[0].S))), nil
		case KindList:
			return Num(float64(len(a[0].L))), nil
		case KindMap:
			return Num(float64(len(a[0].M))), nil
		case KindNull:
			return Num(0), nil
		}
		return Null, fmt.Errorf("%s has no length", a[0].Kind)
	}},
	"has": {2, 2, "has(m, k) reports whether map m contains key k", func(a []Value) (Value, error) {
		if a[0].Kind == KindNull {
			return Bool(false), nil
		}
		ok, err := a[0].Contains(a[1])
		return Bool(ok), err
	}},
	"ip_in_cidr": {2, 2, "ip_in_cidr(ip, cidr) tests network membership", func(a []Value) (Value, error) {
		ipStr, err := wantString(a[0], 0)
		if err != nil {
			return Null, err
		}
		cidrStr, err := wantString(a[1], 1)
		if err != nil {
			return Null, err
		}
		if ipStr == "" {
			return Bool(false), nil
		}
		addr, err := netip.ParseAddr(ipStr)
		if err != nil {
			// An unparseable source address must not match a private range.
			// Returning false rather than erroring keeps a malformed header
			// from taking down the decision path.
			return Bool(false), nil
		}
		pfx, err := netip.ParsePrefix(cidrStr)
		if err != nil {
			return Null, fmt.Errorf("invalid CIDR %q", cidrStr)
		}
		return Bool(pfx.Contains(addr)), nil
	}},
	"split": {2, 2, "split(s, sep) splits a string into a list", func(a []Value) (Value, error) {
		s, err := wantString(a[0], 0)
		if err != nil {
			return Null, err
		}
		sep, err := wantString(a[1], 1)
		if err != nil {
			return Null, err
		}
		if sep == "" {
			return Null, fmt.Errorf("separator must not be empty")
		}
		return StrList(strings.Split(s, sep)), nil
	}},
}

func builtinNames() string {
	names := make([]string, 0, len(builtins))
	for k := range builtins {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// BuiltinDocs returns the builtin catalogue for the policy editor's help pane.
func BuiltinDocs() map[string]string {
	out := make(map[string]string, len(builtins))
	for k, v := range builtins {
		out[k] = v.doc
	}
	return out
}
