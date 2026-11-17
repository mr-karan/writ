// Package importer takes over objects that already exist into OpenTofu
// code. It reads each object through its provider, writes a resource block
// that sets the object's configurable attributes to their current values
// and an import block for it, and asks the provider to prove that a plan of
// the code changes nothing.
package importer

import (
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/value"
)

// Param is a variable import declared because a value was sensitive.
type Param struct {
	Name string
	Type string // map(string)
	From string // OpenTofu address of the resource
	Attr string // attribute path in that resource
}

// Problem is something about an object that the code could not represent.
type Problem struct {
	Kind   ProblemKind
	From   string // the selection entry, or the OpenTofu address
	Reason string
}

// ProblemKind says what a problem cost.
type ProblemKind uint8

const (
	// NotImported is an object Writ could not import.
	NotImported ProblemKind = iota + 1
	// Gap is an imported resource that needs attention before a plan.
	Gap
	// Skipped is an object a bulk import selected and did not import,
	// because the run already holds it, or an id template found no value.
	Skipped
)

func (k ProblemKind) String() string {
	switch k {
	case NotImported:
		return "not imported"
	case Gap:
		return "imported, needs attention"
	case Skipped:
		return "skipped"
	}
	return "invalid"
}

// node is one resource instance in the state, with what import decides
// about it.
type node struct {
	tf     string // aws_vpc.main
	source schema.Source
	schema *schema.Resource
	state  value.Value
	// config is what the generated code sets: see configure.
	config value.Value
	// defaults is the provider configuration the object was read through,
	// and null when the import does not know it.
	defaults value.Value
	// plan plans a config against the object, and is nil when the import
	// cannot plan, as from Terraform state, which records no provider
	// configuration. emptied lists the "" values configure left out, for
	// prove to check with plan.
	pruningStopped bool
	plan           Planner
	emptied        []emptied
	addr           addr.Addr
	// provider is the provider configuration the object was read through,
	// and "" for the default one.
	provider string
}

// configText writes a configuration address as the expression that reads
// it, which has the same spelling, or "" for the zero address.
func configText(a addr.Addr) string {
	if a.IsZero() {
		return ""
	}
	return a.String()
}

func sortProblems(ps []Problem) {
	slices.SortStableFunc(ps, func(a, b Problem) int { return strings.Compare(a.From, b.From) })
}
