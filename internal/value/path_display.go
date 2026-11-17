package value

import (
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/schema"
)

// PathPrinter renders paths for output. Paths used to read values remain raw.
type PathPrinter struct {
	Schema schema.Block
	Values []Value
}

// SecretFrom returns the length of the first secret prefix, or -1.
func (d PathPrinter) SecretFrom(p Path) int {
	for _, v := range d.Values {
		if v.IsSecret() {
			return 0
		}
	}
	b, elem := &d.Schema, false
	for i, s := range p {
		name, isName := s.Name()
		secret := false
		switch {
		case b == nil || elem:
			elem = false
		case isName:
			if j := slices.IndexFunc(b.Attributes, func(a schema.Attribute) bool { return a.Name == name }); j >= 0 {
				a := b.Attributes[j]
				secret, b = a.Sensitive || a.Type.HasSecret(), nil
			} else if j := slices.IndexFunc(b.Blocks, func(nb schema.NestedBlock) bool { return nb.Name == name }); j >= 0 {
				nb := b.Blocks[j]
				b, elem = &nb.Block, !nb.IsRecord()
			} else {
				b = nil
			}
		}
		at := p[:i+1]
		for _, v := range d.Values {
			v = v.At(at)
			secret = secret || v.IsSecret() || (v.Kind() == List || v.Kind() == Object) && v.HasSecret()
		}
		if secret {
			return i + 1
		}
	}
	return -1
}

// String hides every step below a secret prefix, including list indices.
func (d PathPrinter) String(p Path) string {
	if n := d.SecretFrom(p); n >= 0 {
		return p[:n].String() + strings.Repeat("[(secret)]", len(p)-n)
	}
	return p.String()
}

// Collapse returns the first secret prefix, so distinct secret keys share
// one display entry without changing the paths used to read or write values.
func (d PathPrinter) Collapse(p Path) Path {
	if n := d.SecretFrom(p); n >= 0 {
		return p[:n]
	}
	return p
}
