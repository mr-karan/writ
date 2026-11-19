// Package diag defines source positions and the diagnostics every layer of
// Writ reports: a class, a span, a message, and an optional fix. It is a
// leaf package so that every layer can speak it.
package diag

import (
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

// Class groups diagnostics so agents can branch on them; docs/errors.md
// lists what each means. A plan or run kept by a build from before the
// first release can carry a class that no longer exists, such as type or
// goal, which is why Class is a string.
type Class string

const (
	Syntax    Class = "syntax"
	Provider  Class = "provider"
	State     Class = "state"
	StateIO   Class = "state-io"
	SavedPlan Class = "saved-plan"
	Usage     Class = "usage"
	Internal  Class = "internal"
	Auth      Class = "auth"
	Denied    Class = "denied"
	Policy    Class = "policy"
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
