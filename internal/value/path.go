package value

import (
	"slices"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/addr"
)

// Path addresses a location inside a value: subject.common_name, keepers.a,
// keepers["my key"], allowed_uses[0]. Attribute paths from the provider
// protocol, config field spans, change causes, and requires_replace all use it.
type Path []Step

// Step is one path element: a name, for a record field, attribute, or map
// key, or an index, for a list element. Use the constructors.
//
// Records and maps are both objects at runtime, so a value cannot tell a
// field from a map key. AttrStep(n) and KeyStep(n) therefore build the same
// step: they compare equal and print the same, so a path from the provider
// protocol matches one built by walking a value.
type Step struct {
	kind  stepKind
	name  string
	index int64
}

type stepKind uint8

const (
	stepName stepKind = iota + 1
	stepIndex
)

// AttrStep, KeyStep and IndexStep build steps.
func AttrStep(name string) Step { return Step{kind: stepName, name: name} }
func KeyStep(key string) Step   { return Step{kind: stepName, name: key} }
func IndexStep(i int64) Step    { return Step{kind: stepIndex, index: i} }

// Name returns the attribute or key a name step reads, and false for an
// index step.
func (s Step) Name() (string, bool) { return s.name, s.kind == stepName }

// Attr returns p extended with an attribute step. It never aliases p.
func (p Path) Attr(name string) Path { return p.with(AttrStep(name)) }

// Key returns p extended with a map key step.
func (p Path) Key(k string) Path { return p.with(KeyStep(k)) }

// Index returns p extended with a list index step.
func (p Path) Index(i int64) Path { return p.with(IndexStep(i)) }

func (p Path) with(s Step) Path {
	q := make(Path, len(p), len(p)+1)
	copy(q, p)
	return append(q, s)
}

// HasPrefix reports whether q is a prefix of p.
func (p Path) HasPrefix(q Path) bool {
	return len(q) <= len(p) && slices.Equal(p[:len(q)], q)
}

// String renders subject.common_name or keepers.a or keepers["my key"] or
// allowed_uses[0]. A name that is an identifier follows a dot; any other name
// is quoted in brackets like an address key. It is also the key of
// lang.Resource.Fields and of a Fingerprint.
func (p Path) String() string {
	var b strings.Builder
	for i, s := range p {
		switch {
		case s.kind == stepIndex:
			b.WriteByte('[')
			b.WriteString(strconv.FormatInt(s.index, 10))
			b.WriteByte(']')
		case addr.IsIdent(s.name):
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(s.name)
		default:
			b.WriteByte('[')
			b.WriteString(addr.Quote(s.name))
			b.WriteByte(']')
		}
	}
	return b.String()
}
