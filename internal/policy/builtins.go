package policy

import (
	"slices"

	"github.com/open-policy-agent/opa/v1/ast"
)

// allowed lists every built-in a policy may call, by family. A rule that
// calls another fails to compile. docs/policy-reference.md, "Built-ins",
// lists them for policy authors, and TestPolicyBuiltinsDocumented keeps the two lists equal.
var allowed = []string{
	// Operators, membership, and set arithmetic.
	"eq", "assign", "equal", "neq", "gt", "gte", "lt", "lte",
	"plus", "minus", "mul", "div", "rem", "and", "or",
	"internal.member_2", "internal.member_3",
	// Numbers.
	"abs", "round", "ceil", "floor", "to_number", "format_int",
	// Aggregates.
	"count", "sum", "max", "min", "sort",
	// Arrays, sets, and objects.
	"array.concat", "array.slice", "array.reverse", "intersection", "union",
	"object.get", "object.keys", "object.union",
	// Types.
	"is_string", "is_number", "is_boolean", "is_array", "is_set", "is_object", "is_null", "type_name",
	// Strings.
	"concat", "contains", "startswith", "endswith", "lower", "upper", "split", "replace",
	"trim", "trim_space", "trim_prefix", "trim_suffix", "indexof", "substring", "sprintf",
	// Patterns.
	"regex.match", "glob.match",
	// Time, read from the input's now, never from the clock.
	"time.parse_rfc3339_ns", "time.parse_ns", "time.parse_duration_ns",
	"time.date", "time.clock", "time.weekday", "time.add_date", "time.diff", "time.format",
	// Encoding.
	"json.marshal",
}

// capabilities is what a policy may use: the allowed built-ins and no
// network.
var capabilities = withBuiltins(allowed)

func withBuiltins(names []string) *ast.Capabilities {
	c := ast.CapabilitiesForThisVersion()
	c.Builtins = slices.DeleteFunc(c.Builtins, func(b *ast.Builtin) bool { return !slices.Contains(names, b.Name) })
	c.AllowNet = []string{}
	return c
}
