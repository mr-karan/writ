package value

import (
	"slices"
	"strings"
)

// AlwaysSecret are parts of a name that hold a secret in any service,
// whatever a schema says: a request parameter, a field path, or a Nomad
// jobspec variable.
var AlwaysSecret = []string{"password", "passwd", "secret", "token", "privatekey", "credential", "passphrase", "userdata"}

// tokenSettings are the endings of a name that is a setting of the tokens
// an auth method issues, such as max_token_ttl or token_name_format in
// Nomad and token_policies in Vault, squashed. Such a name describes a
// token and never holds one.
var tokenSettings = []string{
	"ttl", "maxttl", "explicitmaxttl", "period", "numuses",
	"locality", "nameformat", "type", "policies", "boundcidrs", "nodefaultpolicy",
}

// DeniedName reports whether name contains one of AlwaysSecret, compared
// without case or punctuation, so DB_Password and api-token are denied.
// A name whose one token is followed by a word of tokenSettings that ends
// it, such as max_token_ttl, is not denied for its token.
func DeniedName(name string) bool {
	n := Squash(name)
	return slices.ContainsFunc(AlwaysSecret, func(d string) bool {
		return strings.Contains(n, d) && (d != "token" || !tokenSetting(n))
	})
}

func tokenSetting(n string) bool {
	return strings.Count(n, "token") == 1 &&
		slices.ContainsFunc(tokenSettings, func(s string) bool { return strings.HasSuffix(n, "token"+s) })
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
