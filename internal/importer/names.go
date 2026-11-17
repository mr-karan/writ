package importer

import (
	"strings"

	"github.com/mr-karan/writ/internal/addr"
)

// ident turns a Terraform name into an identifier. Terraform names may hold
// dashes, which Writ identifiers may not.
func ident(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || i > 0 && '0' <= c && c <= '9':
			b.WriteByte(c)
		case i == 0 && '0' <= c && c <= '9':
			b.WriteByte('_')
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// validLet reports whether name can name a resource and a variable.
func validLet(name string) bool {
	return addr.IsIdent(name)
}
