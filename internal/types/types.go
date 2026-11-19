// Package types is the type model of provider schemas: scalars, lists, maps,
// records, string-literal unions, typed references (Id, Pem, Arn, and Name),
// resource types, and the secret taint. The schema mapper, the importer, and
// `writ schema` share it.
//
// A Type is an immutable value. Build one with the constructors below; the
// fields are unexported so no caller can assemble an ill-formed type.
package types

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/addr"
)

// Kind is the shape of a type, ignoring the secret taint.
type Kind uint8

const (
	Invalid Kind = iota // error recovery; assignable to and from anything
	Null                // the type of the null literal
	// Never has no values: it is the element type of an empty collection.
	// It flows into every type, and Never? is Null.
	Never
	Bool
	Int
	Float
	String
	Enum     // "a" | "b": a string drawn from a fixed set
	Ref      // Id<p.T>, Pem<p.T | q.U>, Arn<p.T>, or Name<p.T>: a string that refers to a resource
	List     // [T]
	Map      // {String: T}
	Record   // { a: T, b?: U }
	Resource // p.TypeName
)

// RefKind says which family a typed reference belongs to. A reference of one
// kind is never accepted where another kind is expected: an Id is not a Pem,
// and an ARN is not a name.
type RefKind uint8

const (
	IdRef RefKind = iota + 1
	PemRef
	ArnRef  // an Amazon Resource Name
	NameRef // a name that identifies the resource, such as an IAM role name
)

// String returns the kind as Writ spells it in a type: Id, Pem, Arn, or Name.
func (k RefKind) String() string {
	switch k {
	case IdRef:
		return "Id"
	case PemRef:
		return "Pem"
	case ArnRef:
		return "Arn"
	case NameRef:
		return "Name"
	}
	return fmt.Sprintf("RefKind(%d)", uint8(k))
}

// Name is a qualified Writ resource type name, such as tls.PrivateKey.
type Name struct {
	Provider string // the provider's type name: tls
	Type     string // PascalCase: PrivateKey
}

// Type is one Writ type.
//
// Invariants, enforced by the constructors:
//   - enum is sorted, deduplicated, and non-empty for Enum.
//   - targets is sorted, deduplicated, and non-empty for Ref.
//   - fields is sorted by name for Record and Resource.
//   - elem is set for List and Map, and nil otherwise.
//   - Never is never nullable: Nullable makes Never? the Null type.
type Type struct {
	kind     Kind
	secret   bool // Secret<T>: this node is tainted
	nullable bool // T?
	elem     *Type
	fields   []Field
	enum     []string
	refKind  RefKind
	targets  []Name
	resource Name
	// alias is the name of the `type` declaration t was written as. Only
	// String reads it: an alias is the type it names, so Assign and Join
	// ignore it.
	alias string
}

// Field is one record field or resource attribute.
type Field struct {
	Name string
	Type Type
	// Optional fields may be omitted in a literal. Required = !Optional && !ReadOnly.
	Optional bool
	// ReadOnly fields are computed by the provider and may not be set.
	ReadOnly bool
	// Computed fields get a value from the provider when config leaves them
	// null. ReadOnly implies Computed.
	Computed bool
}

// Scalar constructors.
var (
	TInvalid = Type{kind: Invalid}
	TNull    = Type{kind: Null}
	TNever   = Type{kind: Never}
	TBool    = Type{kind: Bool}
	TInt     = Type{kind: Int}
	TFloat   = Type{kind: Float}
	TString  = Type{kind: String}
)

// EnumOf returns the union of the given string literals. A single literal is
// assignable to "RSA" | "ECDSA" and to Pem<...> when it is a member. It
// panics when given no members.
func EnumOf(members ...string) Type {
	if len(members) == 0 {
		panic("types: EnumOf needs at least one member")
	}
	m := slices.Clone(members)
	slices.Sort(m)
	return Type{kind: Enum, enum: slices.Compact(m)}
}

// RefTo returns Id<...>, Pem<...>, Arn<...>, or Name<...> over one or more
// resource type names. It panics on an unknown RefKind or when given no
// targets.
func RefTo(kind RefKind, targets ...Name) Type {
	if kind < IdRef || kind > NameRef {
		panic(fmt.Sprintf("types: RefTo with unknown RefKind %d", kind))
	}
	if len(targets) == 0 {
		panic("types: RefTo needs at least one target")
	}
	ts := slices.Clone(targets)
	slices.SortFunc(ts, compareName)
	t := Type{kind: Ref, refKind: kind, targets: slices.Compact(ts)}
	if len(t.targets) == 1 {
		t.resource = t.targets[0]
	}
	return t
}

// ListOf returns [elem].
func ListOf(elem Type) Type { return Type{kind: List, elem: &elem} }

// MapOf returns {String: elem}.
func MapOf(elem Type) Type { return Type{kind: Map, elem: &elem} }

// RecordOf returns a record type with the given fields. It panics on a
// duplicate field name.
func RecordOf(fields ...Field) Type { return Type{kind: Record, fields: sortedFields(fields)} }

// ResourceOf returns the resource type n whose attributes are fields.
func ResourceOf(n Name, fields ...Field) Type {
	return Type{kind: Resource, resource: n, fields: sortedFields(fields)}
}

func sortedFields(fields []Field) []Field {
	fs := slices.Clone(fields)
	slices.SortFunc(fs, func(a, b Field) int { return strings.Compare(a.Name, b.Name) })
	for i := range fs {
		if i > 0 && fs[i].Name == fs[i-1].Name {
			panic(fmt.Sprintf("types: duplicate field %q", fs[i].Name))
		}
		if fs[i].ReadOnly {
			fs[i].Computed = true
		}
	}
	return fs
}

// Secret returns t with the secret taint set.
func Secret(t Type) Type { t.secret = true; return t }

// Named returns t printed as name, for a type written through `type Name =
// T`. It changes nothing but how t prints.
func Named(name string, t Type) Type { t.alias = name; return t }

// Nullable returns t?. Never has no values, so Never? is Null, with the
// taint of t.
func Nullable(t Type) Type {
	if t.kind == Never {
		return Type{kind: Null, secret: t.secret}
	}
	t.nullable = true
	return t
}

// Kind returns the shape of t.
func (t Type) Kind() Kind { return t.kind }

// IsSecret reports whether this node carries the taint.
func (t Type) IsSecret() bool { return t.secret }

// IsNullable reports whether t admits null: T? or the null type itself.
func (t Type) IsNullable() bool { return t.nullable || t.kind == Null }

// HasSecret reports whether t or any component of t is tainted. This is the
// test for flowing into a non-sensitive slot or an `out`.
//
// A resource type is a handle, not data: HasSecret does not look at its
// attributes, because reading one through the handle yields that attribute's
// own type, taint included. Otherwise len(resources(tls.PrivateKey)) would be
// a secret.
func (t Type) HasSecret() bool {
	if t.secret {
		return true
	}
	switch t.kind {
	case List, Map:
		return t.elem.HasSecret()
	case Record:
		return slices.ContainsFunc(t.fields, func(f Field) bool { return f.Type.HasSecret() })
	}
	return false
}

// Elem returns the element type of a List or Map, Never for Never, and
// Invalid for any other kind.
func (t Type) Elem() Type {
	if t.kind == Never {
		return TNever
	}
	if t.elem == nil {
		return TInvalid
	}
	return *t.elem
}

// Fields returns the fields of a Record or Resource, sorted by name. The
// slice is shared; callers must not modify it.
func (t Type) Fields() []Field { return t.fields }

// Field looks up one field by name.
func (t Type) Field(name string) (Field, bool) {
	i, ok := slices.BinarySearchFunc(t.fields, name, func(f Field, n string) int { return strings.Compare(f.Name, n) })
	if !ok {
		return Field{}, false
	}
	return t.fields[i], true
}

// Required reports whether a literal must set the field.
func (f Field) Required() bool { return !f.Optional && !f.ReadOnly }

// ValueType is the type of reading f from a record value. A field that is
// not required may be absent, and an absent field reads as null.
func (f Field) ValueType() Type {
	if f.Required() {
		return f.Type
	}
	return Nullable(f.Type)
}

// Resource returns the resource type name of a Resource or the target of a
// single-target Ref.
func (t Type) Resource() Name { return t.resource }

// Enum returns the members of an Enum, sorted. The slice is shared; callers
// must not modify it.
func (t Type) Enum() []string { return t.enum }

// Mismatch explains why a value of type src cannot flow into dst. It is empty
// when the flow is allowed. SecretOnly is true when the only problem is the
// taint.
type Mismatch struct {
	// Reason is a phrase that follows the name of the slot at Path, such as
	// "expects String, got Int" or "is required".
	Reason     string
	SecretOnly bool
	// Path is the field or element path inside the types where they first
	// differ, for messages like "subject.common_name expects String, got Int".
	// Fields are joined by ".", and an element of a list or map is "[*]".
	// It is empty when the types differ at the top.
	Path string
}

// Message renders the mismatch after the slot that rejected the value:
// Message("subject") is "subject.common_name expects String, got Int".
func (m Mismatch) Message(slot string) string {
	at := slot
	switch {
	case m.Path == "":
	case slot == "":
		at = m.Path
	case strings.HasPrefix(m.Path, "["):
		at = slot + m.Path
	default:
		at = slot + "." + m.Path
	}
	if at == "" {
		return m.Reason
	}
	return at + " " + m.Reason
}

// Assign checks that a value of type src may flow into a slot of type dst.
//
// The rules:
//   - Invalid on either side is allowed: an error was already reported.
//   - Never flows into every type, because it has no values. Only Never
//     flows into Never.
//   - Null flows only into T?. T? flows only into U?.
//   - Int flows into Float.
//   - An Enum flows into an Enum holding all its members, into String, and
//     into any Ref: a string literal is accepted where a typed reference is
//     expected.
//   - String flows into a Ref, but not into an Enum.
//   - A Ref flows into String, and into a Ref of the same kind whose targets
//     include all of its own. A reference of one type is never accepted where
//     another is expected.
//   - Lists and maps flow element-wise. A record flows into a map when every
//     present field flows into the element type. A declared optional field
//     may be null. A key omitted by a record join adds no map value.
//   - A record flows into a record when every required destination field is
//     present, every present field flows, and no extra field is present. An
//     optional destination field accepts null, which means unset.
//   - A resource flows only into a resource of the same type.
//   - A secret node flows only into a secret node. A secret destination node
//     accepts anything inside it, since the whole slot is sensitive.
//
// The shape is checked before the taint, so SecretOnly is set only when the
// flow would be allowed without the taint.
func Assign(dst, src Type) (Mismatch, bool) {
	if m, ok := shapeFlows(dst, src, ""); !ok {
		return m, false
	}
	if m, ok := taintFlows(dst, src, ""); !ok {
		m.SecretOnly = true
		return m, false
	}
	return Mismatch{}, true
}

func shapeFlows(dst, src Type, path string) (Mismatch, bool) {
	if dst.kind == Invalid || src.kind == Invalid || src.kind == Never {
		return Mismatch{}, true
	}
	if src.IsNullable() && !dst.IsNullable() {
		return expects(dst, src, path)
	}
	if src.kind == Null {
		return Mismatch{}, true
	}
	switch dst.kind {
	case Bool, Int, String:
		if src.kind == dst.kind || dst.kind == String && (src.kind == Enum || src.kind == Ref) {
			return Mismatch{}, true
		}
	case Float:
		if src.kind == Float || src.kind == Int {
			return Mismatch{}, true
		}
	case Enum:
		if src.kind == Enum && isSubset(src.enum, dst.enum, strings.Compare) {
			return Mismatch{}, true
		}
	case Ref:
		switch src.kind {
		case String, Enum:
			return Mismatch{}, true
		case Ref:
			if src.refKind == dst.refKind && isSubset(src.targets, dst.targets, compareName) {
				return Mismatch{}, true
			}
		}
	case List:
		if src.kind == List {
			return shapeFlows(*dst.elem, *src.elem, elemPath(path))
		}
	case Map:
		switch src.kind {
		case Map:
			return shapeFlows(*dst.elem, *src.elem, elemPath(path))
		case Record:
			for _, f := range src.fields {
				if m, ok := shapeFlows(*dst.elem, f.ValueType(), fieldPath(path, f.Name)); !ok {
					return m, false
				}
			}
			return Mismatch{}, true
		}
	case Record:
		if src.kind == Record {
			return recordFlows(dst, src, path)
		}
	case Resource:
		if src.kind == Resource && src.resource == dst.resource {
			return Mismatch{}, true
		}
	}
	return expects(dst, src, path)
}

func recordFlows(dst, src Type, path string) (Mismatch, bool) {
	for _, df := range dst.fields {
		p := fieldPath(path, df.Name)
		sf, ok := src.Field(df.Name)
		if !ok {
			if df.Required() {
				return Mismatch{Reason: "is required", Path: p}, false
			}
			continue
		}
		st := sf.ValueType()
		if !df.Required() && st.IsNullable() {
			if st.kind == Null {
				continue
			}
			st.nullable = false
		}
		if m, ok := shapeFlows(df.Type, st, p); !ok {
			return m, false
		}
	}
	for _, sf := range src.fields {
		if _, ok := dst.Field(sf.Name); !ok {
			return Mismatch{Reason: "is not an expected field", Path: fieldPath(path, sf.Name)}, false
		}
	}
	return Mismatch{}, true
}

// taintFlows checks only the secret taint. It runs after shapeFlows has
// accepted the flow, so it can pair up components without rechecking kinds.
func taintFlows(dst, src Type, path string) (Mismatch, bool) {
	if dst.kind == Invalid || dst.secret {
		return Mismatch{}, true
	}
	if src.secret {
		return expects(dst, src, path)
	}
	if src.kind == Invalid || src.kind == Never {
		return Mismatch{}, true
	}
	switch {
	case src.kind == List && dst.kind == List, src.kind == Map && dst.kind == Map:
		return taintFlows(*dst.elem, *src.elem, elemPath(path))
	case src.kind == Record && dst.kind == Map:
		for _, f := range src.fields {
			if m, ok := taintFlows(*dst.elem, f.ValueType(), fieldPath(path, f.Name)); !ok {
				return m, false
			}
		}
	case src.kind == Record && dst.kind == Record:
		for _, sf := range src.fields {
			df, _ := dst.Field(sf.Name)
			if m, ok := taintFlows(df.Type, sf.Type, fieldPath(path, sf.Name)); !ok {
				return m, false
			}
		}
	}
	return Mismatch{}, true
}

func expects(dst, src Type, path string) (Mismatch, bool) {
	return Mismatch{Reason: "expects " + dst.String() + ", got " + src.String(), Path: path}, false
}

func fieldPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func elemPath(path string) string { return path + "[*]" }

// isSubset reports whether every element of a is in b. Both are sorted.
func isSubset[T any](a, b []T, cmp func(T, T) int) bool {
	for _, x := range a {
		if _, ok := slices.BinarySearchFunc(b, x, cmp); !ok {
			return false
		}
	}
	return true
}

// String renders t for diagnostics and schemas: Secret<Pem<tls.PrivateKey>>, [String],
// { a: Int, b?: "x" | "y" }. `writ schema` prints with this. The taint is
// printed outside nullability: Secret<String?>.
func (t Type) String() string {
	var b strings.Builder
	t.write(&b)
	return b.String()
}

func (t Type) write(b *strings.Builder) {
	if t.secret {
		b.WriteString("Secret<")
		defer b.WriteByte('>')
	}
	if t.alias != "" && t.kind != Invalid {
		b.WriteString(t.alias)
		if t.nullable {
			b.WriteByte('?')
		}
		return
	}
	switch t.kind {
	case Invalid:
		b.WriteString("invalid")
	case Null:
		b.WriteString("null")
		return
	case Never:
		b.WriteString("Never")
	case Bool:
		b.WriteString("Bool")
	case Int:
		b.WriteString("Int")
	case Float:
		b.WriteString("Float")
	case String:
		b.WriteString("String")
	case Enum:
		for i, m := range t.enum {
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(addr.Quote(m))
		}
	case Ref:
		b.WriteString(t.refKind.String())
		b.WriteByte('<')
		for i, n := range t.targets {
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(n.String())
		}
		b.WriteByte('>')
	case List:
		b.WriteByte('[')
		t.elem.write(b)
		b.WriteByte(']')
	case Map:
		b.WriteString("{String: ")
		t.elem.write(b)
		b.WriteByte('}')
	case Record:
		writeFields(b, t.fields)
	case Resource:
		b.WriteString(t.resource.String())
	}
	if t.nullable {
		b.WriteByte('?')
	}
}

func writeFields(b *strings.Builder, fields []Field) {
	if len(fields) == 0 {
		b.WriteString("{}")
		return
	}
	b.WriteString("{ ")
	for i, f := range fields {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(f.Name)
		if f.Optional {
			b.WriteByte('?')
		}
		b.WriteString(": ")
		f.Type.write(b)
	}
	b.WriteString(" }")
}

// String renders tls.PrivateKey.
func (n Name) String() string { return n.Provider + "." + n.Type }

func compareName(a, b Name) int {
	return cmp.Or(strings.Compare(a.Provider, b.Provider), strings.Compare(a.Type, b.Type))
}
