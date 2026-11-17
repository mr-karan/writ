// Package diag defines source positions and the diagnostics every layer of
// Writ reports: a class, a span, a message, and an optional fix. It is a
// leaf package so that every layer can speak it.
package diag

import (
	"cmp"
	"fmt"
	"slices"
)

// Pos is a position in a source file. Line and Col are 1-based; Col counts
// bytes. Offset is the 0-based byte offset.
type Pos struct {
	Line   int
	Col    int
	Offset int
}

// Span is a half-open byte range in one file. The zero Span means "no source
// location", for diagnostics about state or providers.
type Span struct {
	File  string
	Start Pos
	End   Pos
}

// IsZero reports whether s names no source location.
func (s Span) IsZero() bool { return s.File == "" }

// String renders "file:line:col", the form every human message uses.
func (s Span) String() string {
	if s.IsZero() {
		return "-"
	}
	return fmt.Sprintf("%s:%d:%d", s.File, s.Start.Line, s.Start.Col)
}

// Class groups diagnostics so agents can branch on them. The first eight
// come from a language Writ no longer has. Contract, State, and StateIO are engine classes: State
// means the plan no longer fits the state, so plan again; StateIO means the
// state database failed, so the same command may pass later. SavedPlan
// means apply refused a saved plan, most often because it was changed after
// writ plan wrote it, so make the plan again. Policy means a policy that
// writ serve judges with refused. Usage and
// Internal are for a command that could not run: Usage means the command line
// was wrong, Internal anything else, such as a file it could not read.
type Class string

const (
	Syntax     Class = "syntax"
	Name       Class = "name"
	Type       Class = "type"
	Field      Class = "field"
	SecretFlow Class = "secret-flow"
	Identity   Class = "identity"
	Goal       Class = "goal"
	Provider   Class = "provider"
	Contract   Class = "contract"
	State      Class = "state"
	StateIO    Class = "state-io"
	SavedPlan  Class = "saved-plan"
	Usage      Class = "usage"
	Internal   Class = "internal"
	Auth       Class = "auth"
	Denied     Class = "denied"
	Policy     Class = "policy"
)

// Severity is error or warning. Errors fail the command.
type Severity uint8

const (
	Error Severity = iota + 1
	Warning
)

func (s Severity) String() string {
	switch s {
	case Error:
		return "error"
	case Warning:
		return "warning"
	}
	return "invalid"
}

// Diagnostic is one finding. Message is one sentence without a trailing
// period. Fix is nil when Writ knows no fix.
type Diagnostic struct {
	Severity Severity
	Class    Class
	Span     Span
	Message  string
	Fix      *Fix
}

// Fix is advice on what to do next, such as a command to run.
type Fix struct {
	Message string
}

// List is an ordered set of diagnostics.
type List []Diagnostic

// Errorf appends an error diagnostic.
func (l *List) Errorf(class Class, span Span, format string, args ...any) {
	*l = append(*l, Diagnostic{Severity: Error, Class: class, Span: span, Message: fmt.Sprintf(format, args...)})
}

// Warnf appends a warning diagnostic.
func (l *List) Warnf(class Class, span Span, format string, args ...any) {
	*l = append(*l, Diagnostic{Severity: Warning, Class: class, Span: span, Message: fmt.Sprintf(format, args...)})
}

// HasErrors reports whether any diagnostic is an error.
func (l List) HasErrors() bool {
	return slices.ContainsFunc(l, func(d Diagnostic) bool { return d.Severity == Error })
}

// Sort orders diagnostics by file, line, column, then class, so output is
// stable across runs and parallel checks.
func (l List) Sort() {
	slices.SortStableFunc(l, func(a, b Diagnostic) int {
		return cmp.Or(
			cmp.Compare(a.Span.File, b.Span.File),
			cmp.Compare(a.Span.Start.Line, b.Span.Start.Line),
			cmp.Compare(a.Span.Start.Col, b.Span.Start.Col),
			cmp.Compare(a.Class, b.Class),
		)
	})
}
