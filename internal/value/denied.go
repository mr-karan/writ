package value

import (
	"slices"
	"strings"
)

// AlwaysSecret are parts of a name that hold a secret in any service,
// whatever a schema says: a request parameter, a field path, or a Nomad
// jobspec variable.
var AlwaysSecret = []string{"password", "passwd", "secret", "token", "privatekey", "credential", "passphrase", "userdata"}

// DeniedName reports whether name contains one of AlwaysSecret, compared
// without case or punctuation, so DB_Password and api-token are denied.
func DeniedName(name string) bool {
	n := Squash(name)
	return slices.ContainsFunc(AlwaysSecret, func(d string) bool { return strings.Contains(n, d) })
}

// Squash lowercases s and drops everything but letters and digits.
func Squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
