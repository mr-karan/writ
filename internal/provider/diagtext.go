package provider

import (
	"errors"
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/value"
)

// The texts Writ prints in place of a provider's summary and detail.
const (
	// WithheldAtSecret replaces an error at a path that touches a secret.
	WithheldAtSecret = "the provider reported an error about a secret, and Writ withholds its text"
	// withheldWarningAtSecret replaces a warning at such a path.
	withheldWarningAtSecret = "the provider sent a warning about a secret, and Writ withholds its text"
	// WithheldOnSecret replaces an error on an object that holds a secret.
	WithheldOnSecret = "the provider reported an error, and Writ withholds its text because this object holds a secret"
	// withheldWarningOnSecret replaces a warning on such an object.
	withheldWarningOnSecret = "the provider sent a warning, and Writ withholds its text because this object holds a secret"
	// WithheldInProcess replaces an error from a provider process that
	// holds a secret of another object or of its configuration.
	WithheldInProcess = "the provider reported an error, and Writ withholds its text because the provider holds a secret"
	// withheldWarningInProcess replaces a warning from such a process.
	withheldWarningInProcess = "the provider sent a warning, and Writ withholds its text because the provider holds a secret"
	// WithheldHint tells the user where the provider's own text is safe to read.
	WithheldHint = "the provider's text may quote the secret, so Writ never prints it; to read it, run the provider by hand, such as with OpenTofu, in a terminal that no log records"
)

// Subject is the object a provider call was about, as the rule that decides
// what Writ prints of the provider's text sees it: the object's schema, when
// Writ knows it, and every value of the object the call sent or returned,
// such as the config, the prior state, and the planned or new state.
type Subject struct {
	Block  *schema.Block
	Values []value.Value
}

// Shown is what Writ prints of one provider diagnostic.
type Shown struct {
	// Text is the path, with each step past a secret printed as
	// [(secret)], then the provider's summary and detail or the text that
	// replaces them.
	Text string
	// Withheld says the provider's summary and detail were replaced.
	Withheld bool
}

// Show applies the one rule for the text of a provider diagnostic. A
// provider may quote, escape, or encode a value it echoes, so no match on
// a secret's bytes can be trusted, and the rule decides by where the
// diagnostic points and what the object and the provider process hold:
//
//   - A path that touches a secret, through a sensitive attribute, an
//     attribute whose type holds a secret, or a value marked secret: the
//     summary and detail are replaced, and the path is printed redacted.
//   - Any other path, or no path, on an object any of whose values holds a
//     secret value or a key of a secret map: the summary and detail are
//     replaced.
//   - Any other diagnostic from a process that held a secret when it sent
//     it (Diag.Held): a value of its configuration, or of any object it
//     was sent or returned in this run. The summary and detail are
//     replaced.
//   - Otherwise the provider's text is printed as it came.
//
// Severity, and where the diagnostic points, are the caller's to keep.
func (s Subject) Show(d Diag) Shown {
	msg := d.Summary
	if d.Detail != "" {
		msg += ": " + d.Detail
	}
	withheld := true
	switch {
	case len(d.Path) > 0 && s.paths().SecretFrom(d.Path) >= 0:
		msg = pick(d.Error, WithheldAtSecret, withheldWarningAtSecret)
	case s.holdsSecret():
		msg = pick(d.Error, WithheldOnSecret, withheldWarningOnSecret)
	case d.Held:
		msg = pick(d.Error, WithheldInProcess, withheldWarningInProcess)
	default:
		withheld = false
	}
	if len(d.Path) > 0 {
		msg = s.pathText(d.Path, withheld) + ": " + msg
	}
	return Shown{Text: msg, Withheld: withheld}
}

func pick(isError bool, e, w string) string {
	if isError {
		return e
	}
	return w
}

// PathText prints p, a path into the object, with each step past a
// sensitive attribute, an attribute whose type holds a secret, or a value
// marked secret as [(secret)], so credentials["k"] prints as
// credentials[(secret)]. On an object that holds a secret, a map key the
// object's values do not hold, which only the provider could have written,
// prints as [(secret)] too.
func (s Subject) PathText(p value.Path) string { return s.pathText(p, s.holdsSecret()) }

// pathText is PathText, with held saying whether the text around p is
// withheld. value.PathPrinter hides each step past a secret. Past that,
// a held text hides every name the object cannot place, which only the
// provider could have written.
func (s Subject) pathText(p value.Path, held bool) string {
	pp := s.paths()
	if pp.SecretFrom(p) >= 0 || !held {
		return pp.String(p)
	}
	for i := range p {
		if name, isName := p[i].Name(); isName && !s.knownName(p[:i], name) {
			return p[:i].String() + strings.Repeat("[(secret)]", len(p)-i)
		}
	}
	return p.String()
}

// paths is the printer of paths into the object.
func (s Subject) paths() value.PathPrinter {
	pp := value.PathPrinter{Values: s.Values}
	if s.Block != nil {
		pp.Schema = *s.Block
	}
	return pp
}

// knownName reports whether name, the step after at, names a schema
// attribute or block there, or a key one of the object's values holds.
func (s Subject) knownName(at value.Path, name string) bool {
	if b := s.blockAt(at); b != nil {
		if slices.ContainsFunc(b.Attributes, func(a schema.Attribute) bool { return a.Name == name }) ||
			slices.ContainsFunc(b.Blocks, func(nb schema.NestedBlock) bool { return nb.Name == name }) {
			return true
		}
	}
	return slices.ContainsFunc(s.Values, func(v value.Value) bool {
		v = v.At(at)
		return v.Kind() == value.Object && slices.Contains(v.Keys(), name)
	})
}

// blockAt returns the schema block at p, or nil when p does not end at one.
func (s Subject) blockAt(p value.Path) *schema.Block {
	b, elem := s.Block, false
	for _, step := range p {
		name, isName := step.Name()
		switch {
		case b == nil:
			return nil
		case elem:
			elem = false
		case isName:
			j := slices.IndexFunc(b.Blocks, func(nb schema.NestedBlock) bool { return nb.Name == name })
			if j < 0 {
				return nil
			}
			nb := b.Blocks[j]
			b, elem = &nb.Block, !nb.IsRecord()
		}
	}
	if elem {
		return nil
	}
	return b
}

// holdsSecret reports whether any of the object's values holds a value
// that is marked secret or sits under a sensitive attribute, or a key of a
// secret map. A null secret holds no bytes to echo. An unknown one counts,
// because the provider fills it in.
func (s Subject) holdsSecret() bool {
	return slices.ContainsFunc(s.Values, func(v value.Value) bool { return holds(s.Block, v, false) })
}

func holds(b *schema.Block, v value.Value, secret bool) bool {
	secret = secret || v.IsSecret()
	switch v.Kind() {
	case value.Null:
		return false
	case value.Unknown:
		// The provider fills in an unknown secret, so its text may quote it.
		return secret
	case value.List:
		for i := range v.Len() {
			if holds(b, v.Index(i), secret) {
				return true
			}
		}
		return false
	case value.Object:
		for _, k := range v.Keys() {
			if secret {
				return true
			}
			child, cb, cs := v.Get(k), (*schema.Block)(nil), false
			if b != nil {
				if j := slices.IndexFunc(b.Attributes, func(a schema.Attribute) bool { return a.Name == k }); j >= 0 {
					cs = b.Attributes[j].Sensitive || b.Attributes[j].Type.HasSecret()
				} else if j := slices.IndexFunc(b.Blocks, func(nb schema.NestedBlock) bool { return nb.Name == k }); j >= 0 {
					nb := b.Blocks[j]
					if !nb.IsRecord() {
						if holdsElems(&nb.Block, child) {
							return true
						}
						continue
					}
					cb = &nb.Block
				}
			}
			if holds(cb, child, cs) {
				return true
			}
		}
		return false
	}
	return secret
}

// holdsElems is holds over the elements of a repeated nested block: a
// list of records, or a map of them.
func holdsElems(b *schema.Block, v value.Value) bool {
	switch v.Kind() {
	case value.List:
		for i := range v.Len() {
			if holds(b, v.Index(i), v.IsSecret()) {
				return true
			}
		}
	case value.Object:
		for _, k := range v.Keys() {
			if v.IsSecret() || holds(b, v.Get(k), false) {
				return true
			}
		}
	default:
		return holds(nil, v, false)
	}
	return false
}

// Err is e as Error prints it, with each error diagnostic as Show prints
// it for s, and red applied to the text Show does not withhold. When Show
// withholds any of it, Err returns a *WithheldError.
func (s Subject) Err(e *Error, red value.Redactor) error {
	var msgs []string
	withheld := false
	for _, d := range e.Diags {
		if !d.Error {
			continue
		}
		shown := s.Show(d)
		if shown.Withheld {
			withheld = true
			msgs = append(msgs, shown.Text)
		} else {
			msgs = append(msgs, red.Text(shown.Text))
		}
	}
	what := e.Op
	if e.Type != "" {
		what += " " + e.Type
	}
	text := what + ": " + strings.Join(msgs, "; ")
	if withheld {
		return &WithheldError{Text: text}
	}
	return errors.New(text)
}

// Error is err as Writ prints it for s. A provider error with diagnostics
// prints as Err prints it. A *ProcessError from a process that held a
// secret, or about an object that holds one, prints without the provider's
// status message and stderr, as a *WithheldError. Any other error goes
// through red.
func (s Subject) Error(err error, red value.Redactor) error {
	if err == nil {
		return nil
	}
	if pe, ok := errors.AsType[*Error](err); ok && len(pe.Diags) > 0 {
		return s.Err(pe, red)
	}
	xe, ok := errors.AsType[*ProcessError](err)
	if !ok || !(xe.Held || s.holdsSecret()) {
		return red.Error(err)
	}
	full, inner := err.Error(), xe.Error()
	text := xe.withheld()
	if i := strings.Index(full, inner); i >= 0 {
		text = red.Text(full[:i]) + text + red.Text(full[i+len(inner):])
	}
	return &WithheldError{Text: text}
}

// WithheldError is a failed provider call whose text Writ withheld, by the
// rule of Subject.Show. Text holds no byte of the provider's summary or
// detail, so a caller that prints it adds WithheldHint.
type WithheldError struct{ Text string }

func (e *WithheldError) Error() string { return e.Text }
