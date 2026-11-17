// Package value is the value model of the importer and the provider
// client. The provider package converts values to and from cty at the gRPC
// boundary.
//
// A Value is known (null, a scalar, a list, or an object) or Unknown, a
// value the provider will compute during apply.
//
// Any value may carry the secret mark. A mark on a list or object covers
// everything inside it.
package value

import (
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/addr"
)

// Kind is the shape of a value.
type Kind uint8

const (
	Null Kind = iota
	Bool
	Int
	Float
	String
	List
	// Object is a string-keyed collection. Records and maps both evaluate to
	// objects; the static type says which one a slot wants.
	Object
	Unknown
)

// Value is immutable. The zero Value is a known null.
type Value struct {
	kind     Kind
	secret   bool
	observed map[string]Digest
	b        bool
	i        int64
	f        float64
	s        string
	keys     []string // Object: sorted keys
	elems    []Value  // List: elements. Object: values, parallel to keys.
}

// ObservedDigests holds keyed digests of observed variables omitted from
// resource attributes. It is storage metadata, never a provider attribute.
func (v Value) ObservedDigests() map[string]Digest { return maps.Clone(v.observed) }

func (v Value) WithObservedDigests(digests map[string]Digest) Value {
	v.observed = maps.Clone(digests)
	return v
}

// Constructors for known values.
func NullVal() Value           { return Value{} }
func BoolVal(b bool) Value     { return Value{kind: Bool, b: b} }
func IntVal(i int64) Value     { return Value{kind: Int, i: i} }
func FloatVal(f float64) Value { return Value{kind: Float, f: f} }
func StringVal(s string) Value { return Value{kind: String, s: s} }

// ListVal returns a list of elems. It copies elems, so the caller may reuse
// the slice.
func ListVal(elems []Value) Value { return Value{kind: List, elems: slices.Clone(elems)} }

// ObjectVal returns an object with the given fields. Records and maps both
// build objects.
func ObjectVal(fields map[string]Value) Value {
	keys := slices.Sorted(maps.Keys(fields))
	elems := make([]Value, len(keys))
	for i, k := range keys {
		elems[i] = fields[k]
	}
	return Value{kind: Object, keys: keys, elems: elems}
}

// UnknownVal is a value the provider will compute during apply.
func UnknownVal(secret bool) Value { return Value{kind: Unknown, secret: secret} }

// Kind returns the shape of v.
func (v Value) Kind() Kind { return v.kind }

// IsSecret reports whether this node carries the secret mark. A mark on a
// list or object covers everything inside it.
func (v Value) IsSecret() bool { return v.secret }

// HasSecret reports whether v or anything inside v is secret.
func (v Value) HasSecret() bool {
	if v.secret {
		return true
	}
	if v.kind == List || v.kind == Object {
		return slices.ContainsFunc(v.elems, Value.HasSecret)
	}
	return false
}

// IsWhollyKnown reports whether v contains no Unknown node, looking inside
// lists and objects.
func (v Value) IsWhollyKnown() bool {
	switch v.kind {
	case Unknown:
		return false
	case List, Object:
		return !slices.ContainsFunc(v.elems, func(e Value) bool { return !e.IsWhollyKnown() })
	}
	return true
}

// Secret returns v with the secret mark set.
func (v Value) Secret() Value { v.secret = true; return v }

// inherit marks v secret when the container it was read from is secret.
func (v Value) inherit(secret bool) Value {
	if secret {
		v.secret = true
	}
	return v
}

// Accessors. Each panics when called on the wrong kind, a bug in Writ.
// Reading out of a secret list or object yields a secret.
func (v Value) AsBool() bool     { v.must(Bool, "AsBool"); return v.b }
func (v Value) AsInt() int64     { v.must(Int, "AsInt"); return v.i }
func (v Value) AsFloat() float64 { v.must(Float, "AsFloat"); return v.f }
func (v Value) AsString() string { v.must(String, "AsString"); return v.s }
func (v Value) Index(i int) Value {
	v.must(List, "Index")
	return v.elems[i].inherit(v.secret)
}
func (v Value) Keys() []string { return v.keys }

// Len returns the number of elements of a list or fields of an object.
func (v Value) Len() int {
	if v.kind != List && v.kind != Object {
		panic("value: Len on " + v.kind.String())
	}
	return len(v.elems)
}

// Get returns the field key of an object, or null when it has none.
func (v Value) Get(key string) Value {
	v.must(Object, "Get")
	i, ok := slices.BinarySearch(v.keys, key)
	if !ok {
		return NullVal().inherit(v.secret)
	}
	return v.elems[i].inherit(v.secret)
}

func (v Value) must(k Kind, method string) {
	if v.kind != k {
		panic("value: " + method + " on " + v.kind.String())
	}
}

// At walks p from v. A step through an Unknown yields Unknown, keeping the
// secret mark; a missing step yields null. Any secret container on the way
// marks the result.
func (v Value) At(p Path) Value {
	cur, secret := v, false
	for _, s := range p {
		secret = secret || cur.secret
		switch {
		case cur.kind == Unknown:
			return UnknownVal(secret)
		case s.kind == stepName && cur.kind == Object:
			cur = cur.Get(s.name)
		case s.kind == stepIndex && cur.kind == List && 0 <= s.index && s.index < int64(len(cur.elems)):
			cur = cur.elems[s.index]
		default:
			return NullVal().inherit(secret)
		}
	}
	return cur.inherit(secret)
}

// Equal reports deep equality of two known values, including secret marks.
// Unknown equals only Unknown.
//
// A mark on a container covers its contents, so marks are compared as they
// apply to each leaf and empty container: a secret list of strings equals a
// list of secret strings. For values Canonical accepts, Equal holds exactly
// when the canonical encodings match. All NaNs are equal, and -0 is not 0.
func Equal(a, b Value) bool { return equal(a, b, false, false) }

func equal(a, b Value, sa, sb bool) bool {
	sa, sb = sa || a.secret, sb || b.secret
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case List, Object:
		if len(a.elems) != len(b.elems) || !slices.Equal(a.keys, b.keys) {
			return false
		}
		if len(a.elems) == 0 {
			return sa == sb
		}
		for i := range a.elems {
			if !equal(a.elems[i], b.elems[i], sa, sb) {
				return false
			}
		}
		return true
	}
	if sa != sb {
		return false
	}
	switch a.kind {
	case Bool:
		return a.b == b.b
	case Int:
		return a.i == b.i
	case Float:
		return a.f == b.f && math.Signbit(a.f) == math.Signbit(b.f) || math.IsNaN(a.f) && math.IsNaN(b.f)
	case String:
		return a.s == b.s
	}
	return true
}

// Walk calls fn for every leaf of v with its path. Lists and objects are
// containers; everything else is a leaf, so an empty container yields
// nothing. A leaf inside a secret container is passed with the secret mark
// set, so a caller that seals or redacts leaves cannot miss an inherited mark.
func Walk(v Value, fn func(p Path, leaf Value)) { walk(v, nil, false, false, fn) }

// walk is Walk; with emptyIsLeaf set, an empty list or object is passed to fn
// as a leaf too, so a fingerprint tells [] from {} and from a missing value.
func walk(v Value, p Path, secret, emptyIsLeaf bool, fn func(Path, Value)) {
	secret = secret || v.secret
	isContainer := v.kind == List || v.kind == Object
	if !isContainer || emptyIsLeaf && len(v.elems) == 0 {
		fn(p, v.inherit(secret))
		return
	}
	for i, e := range v.elems {
		if v.kind == List {
			walk(e, p.Index(int64(i)), secret, emptyIsLeaf, fn)
		} else {
			walk(e, p.Key(v.keys[i]), secret, emptyIsLeaf, fn)
		}
	}
}

// FormatFloat renders a number the way every Writ output prints one. A whole
// number prints as an integer, never with a fraction or an exponent, so a
// port of 8000 reads 8000 wherever it appears. Any other number prints in the
// shortest form that parses back to the same float64.
func FormatFloat(f float64) string {
	if f == math.Trunc(f) && !math.IsInf(f, 0) {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// String renders v for debugging and test failures. It
// prints (secret) for anything secret, so a value logged by mistake does not
// leak one. Human output goes through the cli package, not this.
func (v Value) String() string {
	var b strings.Builder
	v.write(&b, false)
	return b.String()
}

func (v Value) write(b *strings.Builder, secret bool) {
	if secret = secret || v.secret; secret {
		b.WriteString("(secret)")
		return
	}
	switch v.kind {
	case Null:
		b.WriteString("null")
	case Bool:
		b.WriteString(strconv.FormatBool(v.b))
	case Int:
		b.WriteString(strconv.FormatInt(v.i, 10))
	case Float:
		b.WriteString(FormatFloat(v.f))
	case String:
		b.WriteString(addr.Quote(v.s))
	case List:
		b.WriteByte('[')
		for i, e := range v.elems {
			if i > 0 {
				b.WriteString(", ")
			}
			e.write(b, false)
		}
		b.WriteByte(']')
	case Object:
		b.WriteByte('{')
		for i, e := range v.elems {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte(' ')
			if addr.IsIdent(v.keys[i]) {
				b.WriteString(v.keys[i])
			} else {
				b.WriteString(addr.Quote(v.keys[i]))
			}
			b.WriteString(": ")
			e.write(b, false)
		}
		if len(v.elems) > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('}')
	case Unknown:
		b.WriteString("(known after apply)")
	}
}

// String names the kind, for error messages.
func (k Kind) String() string {
	switch k {
	case Null:
		return "null"
	case Bool:
		return "bool"
	case Int:
		return "int"
	case Float:
		return "float"
	case String:
		return "string"
	case List:
		return "list"
	case Object:
		return "object"
	case Unknown:
		return "unknown"
	}
	return "invalid"
}
