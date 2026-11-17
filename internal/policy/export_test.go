package policy

import (
	"os"
	"slices"
)

// Hazards are built-ins Writ leaves out. An evaluation process of the
// test binary allows them, so the isolation tests can show what the
// process bounds when one runs.
var Hazards = []string{"json.verify_schema", "strings.render_template", "bits.lsh", "numbers.range"}

// This runs before the package's init, which starts the evaluation.
var _ = func() bool {
	if len(os.Args) == 1 && os.Args[0] == childName {
		capabilities = withBuiltins(append(slices.Clone(allowed), Hazards...))
	}
	return true
}()

// AllowForTest lets policies compile with names beside the allowed
// built-ins in this process. It returns the function that undoes it.
func AllowForTest(names ...string) func() {
	old := capabilities
	capabilities = withBuiltins(append(slices.Clone(allowed), names...))
	return func() { capabilities = old }
}

// Allowed is the list of allowed built-ins.
var Allowed = allowed

// CloseInherited closes the descriptors the process inherited.
var CloseInherited = closeInherited
