package policy

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Kind is the dynamic type of a Value.
type Kind uint8

const (
	KindNull Kind = iota
	KindBool
	KindNumber
	KindString
	KindList
	KindMap
)

// String returns the type name, used in error messages.
func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "bool"
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindList:
		return "list"
	case KindMap:
		return "map"
	}
	return "unknown"
}

// Value is a dynamically typed policy value.
//
// The language is deliberately small: strings, numbers, booleans, lists and
// string-keyed maps. There is no arithmetic beyond comparison, no loops, and
// no user-defined functions. Every expression terminates, which is the
// property that matters when this evaluates on the request path.
type Value struct {
	Kind Kind
	B    bool
	N    float64
	S    string
	L    []Value
	M    map[string]Value
}

// Constructors.
var Null = Value{Kind: KindNull}

// Bool returns a boolean Value.
func Bool(b bool) Value { return Value{Kind: KindBool, B: b} }

// Num returns a numeric Value.
func Num(f float64) Value { return Value{Kind: KindNumber, N: f} }

// Str returns a string Value.
func Str(s string) Value { return Value{Kind: KindString, S: s} }

// List returns a list Value.
func List(vs ...Value) Value { return Value{Kind: KindList, L: vs} }

// Map returns a map Value.
func Map(m map[string]Value) Value { return Value{Kind: KindMap, M: m} }

// StrList builds a list Value from Go strings.
func StrList(ss []string) Value {
	vs := make([]Value, 0, len(ss))
	for _, s := range ss {
		vs = append(vs, Str(s))
	}
	return Value{Kind: KindList, L: vs}
}

// StrMap builds a map Value from a Go string map.
func StrMap(m map[string]string) Value {
	out := make(map[string]Value, len(m))
	for k, v := range m {
		out[k] = Str(v)
	}
	return Value{Kind: KindMap, M: out}
}

// Truthy reports the value's boolean interpretation.
//
// Only booleans are truthy. A non-empty string or non-zero number is a type
// error rather than an implicit true: in a policy language, `when subject.id`
// silently meaning "when the id is non-empty" is the kind of shortcut that
// produces an authorisation bug nobody can see by reading the source.
func (v Value) Truthy() (bool, error) {
	if v.Kind != KindBool {
		return false, fmt.Errorf("expected bool, got %s", v.Kind)
	}
	return v.B, nil
}

// String renders the value for traces and error messages.
func (v Value) String() string {
	switch v.Kind {
	case KindNull:
		return "null"
	case KindBool:
		return strconv.FormatBool(v.B)
	case KindNumber:
		if v.N == math.Trunc(v.N) && math.Abs(v.N) < 1e15 {
			return strconv.FormatInt(int64(v.N), 10)
		}
		return strconv.FormatFloat(v.N, 'g', -1, 64)
	case KindString:
		return strconv.Quote(v.S)
	case KindList:
		parts := make([]string, 0, len(v.L))
		for _, x := range v.L {
			parts = append(parts, x.String())
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case KindMap:
		keys := make([]string, 0, len(v.M))
		for k := range v.M {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+": "+v.M[k].String())
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return "?"
}

// Equal reports deep equality. Values of different kinds are never equal,
// so `1 == "1"` is false rather than a coercion.
func (v Value) Equal(o Value) bool {
	if v.Kind != o.Kind {
		return false
	}
	switch v.Kind {
	case KindNull:
		return true
	case KindBool:
		return v.B == o.B
	case KindNumber:
		return v.N == o.N
	case KindString:
		return v.S == o.S
	case KindList:
		if len(v.L) != len(o.L) {
			return false
		}
		for i := range v.L {
			if !v.L[i].Equal(o.L[i]) {
				return false
			}
		}
		return true
	case KindMap:
		if len(v.M) != len(o.M) {
			return false
		}
		for k, a := range v.M {
			b, ok := o.M[k]
			if !ok || !a.Equal(b) {
				return false
			}
		}
		return true
	}
	return false
}

// Compare orders two values of the same ordered kind. Numbers compare
// numerically and strings lexicographically; other kinds are unorderable.
func (v Value) Compare(o Value) (int, error) {
	if v.Kind != o.Kind {
		return 0, fmt.Errorf("cannot compare %s with %s", v.Kind, o.Kind)
	}
	switch v.Kind {
	case KindNumber:
		switch {
		case v.N < o.N:
			return -1, nil
		case v.N > o.N:
			return 1, nil
		}
		return 0, nil
	case KindString:
		return strings.Compare(v.S, o.S), nil
	}
	return 0, fmt.Errorf("values of type %s are not ordered", v.Kind)
}

// Contains reports membership: an element in a list, a key in a map, or a
// substring in a string.
func (v Value) Contains(needle Value) (bool, error) {
	switch v.Kind {
	case KindList:
		for _, x := range v.L {
			if x.Equal(needle) {
				return true, nil
			}
		}
		return false, nil
	case KindMap:
		if needle.Kind != KindString {
			return false, fmt.Errorf("map keys are strings, got %s", needle.Kind)
		}
		_, ok := v.M[needle.S]
		return ok, nil
	case KindString:
		if needle.Kind != KindString {
			return false, fmt.Errorf("cannot search for %s in a string", needle.Kind)
		}
		return strings.Contains(v.S, needle.S), nil
	}
	return false, fmt.Errorf("%s is not a container", v.Kind)
}

// Index performs member access: map key or list position.
func (v Value) Index(key Value) (Value, error) {
	switch v.Kind {
	case KindMap:
		if key.Kind != KindString {
			return Null, fmt.Errorf("map index must be a string, got %s", key.Kind)
		}
		// A missing key yields null rather than an error, so a policy can
		// safely test for an optional claim without guarding every access.
		return v.M[key.S], nil
	case KindList:
		if key.Kind != KindNumber {
			return Null, fmt.Errorf("list index must be a number, got %s", key.Kind)
		}
		i := int(key.N)
		if i < 0 || i >= len(v.L) {
			return Null, nil
		}
		return v.L[i], nil
	case KindNull:
		return Null, nil
	}
	return Null, fmt.Errorf("cannot index a %s", v.Kind)
}

// Glob reports whether s matches a pattern containing '*' (any run of
// characters) and '?' (exactly one character).
//
// Policy matching uses globs rather than regular expressions on purpose. This
// runs on every request, and an operator-authored regex is an operator-authored
// denial of service waiting to happen. Globs match what identity patterns
// actually need — "spiffe://prod.internal/ns/payments/*" — with a linear-time
// greedy algorithm and no catastrophic backtracking.
func Glob(pattern, s string) bool {
	var (
		p, i           int
		star, starMark = -1, 0
	)
	for i < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case p < len(pattern) && pattern[p] == '*':
			star = p
			starMark = i
			p++
		case star >= 0:
			// Backtrack: let the last '*' absorb one more character.
			p = star + 1
			starMark++
			i = starMark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
