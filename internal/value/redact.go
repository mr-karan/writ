package value

import (
	"errors"
	"slices"
	"strings"
)

// Redactor removes secret strings from error text before it reaches a log
// or the terminal. Providers echo inputs in diagnostics, so
// any secret sent to or read from a provider may come back inside an error.
type Redactor []string

// minLineSecret is the shortest line of a multi-line secret that is redacted
// on its own; shorter lines, such as a PEM's closing dashes, are not secret.
const minLineSecret = 16

// With returns r plus every secret string inside vals, and the long lines of
// multi-line ones, longest first so a secret that contains another is
// removed whole.
func (r Redactor) With(vals ...Value) Redactor {
	out := slices.Clone(r)
	for _, v := range vals {
		Walk(v, func(_ Path, leaf Value) {
			if !leaf.IsSecret() || leaf.Kind() != String || leaf.AsString() == "" {
				return
			}
			s := leaf.AsString()
			out = append(out, s)
			for line := range strings.Lines(s) {
				if line = strings.TrimSpace(line); len(line) >= minLineSecret {
					out = append(out, line)
				}
			}
		})
	}
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return slices.Compact(out)
}

// Text replaces secret strings in s.
func (r Redactor) Text(s string) string {
	for _, secret := range r {
		s = strings.ReplaceAll(s, secret, "(secret)")
	}
	return s
}

// Error returns err with every secret replaced. The result keeps only the
// text, which is all a log and the terminal see.
func (r Redactor) Error(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(r.Text(err.Error()))
}
