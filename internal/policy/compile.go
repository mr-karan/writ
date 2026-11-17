package policy

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
)

// schemaJSON is the input's schema: the keys of the masked plan document
// and input.writ. The type checker refuses a rule that reads a key the
// input never has, such as planned_values, which conftest sees in `tofu
// show -json` and the mask drops. Such a rule would never deny in writ
// serve.
//
//go:embed schema/input.json
var schemaJSON []byte

var inputSchema = func() any {
	var v any
	if err := json.Unmarshal(schemaJSON, &v); err != nil {
		panic(err)
	}
	return v
}()

// Namespace is the package whose rules Writ reads: conftest's default, so
// `conftest test plan.json` reads the same rules.
const Namespace = "main"

// The rules Writ reads in Namespace, by conftest's names: deny,
// violation, and warn, alone or with a suffix such as deny_public.
var (
	denyRule = regexp.MustCompile(`^(deny|violation)(_[a-zA-Z0-9]+)*$`)
	warnRule = regexp.MustCompile(`^warn(_[a-zA-Z0-9]+)*$`)
)

// prepared is a set's query and the rules it reads, in its bindings' order.
type prepared struct {
	query rego.PreparedEvalQuery
	rules []string
}

// compileSet parses a set's files, type checks them against the input's
// schema, and prepares the query of the rules Writ reads. It runs no
// built-in. Each problem names the set, and the file and place when OPA
// gives one.
func compileSet(name string, files map[string]string) (*prepared, []string) {
	var problems []string
	refuse := func(loc *ast.Location, format string, args ...any) {
		where := ""
		if loc != nil && loc.File != "" {
			where = fmt.Sprintf("%s:%d:%d: ", loc.File, loc.Row, loc.Col)
		}
		problems = append(problems, fmt.Sprintf("policy set %s: %s%s", name, where, fmt.Sprintf(format, args...)))
	}
	var modules []*ast.Module
	rules := map[string]bool{}
	for _, file := range slices.Sorted(maps.Keys(files)) {
		m, err := ast.ParseModuleWithOpts(file, files[file], ast.ParserOptions{RegoVersion: ast.RegoV1, Capabilities: capabilities})
		if err != nil {
			opaErrors(err, refuse)
			continue
		}
		// OPA erases a print call it is not asked to keep. Refuse it, so
		// the author learns that it prints nothing.
		ast.WalkExprs(m, func(x *ast.Expr) bool {
			if x.IsCall() && x.Operator().String() == "print" {
				refuse(x.Location, "rego_type_error: undefined function print; writ serve prints no policy input")
			}
			return false
		})
		if m.Package.Path.String() == "data."+Namespace {
			for _, r := range m.Rules {
				ref := r.Head.Ref()
				if len(ref) != 1 {
					continue
				}
				switch rule := ref.String(); {
				case denyRule.MatchString(rule), warnRule.MatchString(rule):
					rules[rule] = true
				case rule == "exception":
					refuse(r.Location, "writ serve reads no exception rules; write the exception into the condition of the rule it excepts")
				}
			}
		}
		modules = append(modules, m)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	if len(rules) == 0 {
		refuse(nil, "defines no deny, violation, or warn rule in package %s", Namespace)
		return nil, problems
	}
	p := &prepared{rules: slices.Sorted(maps.Keys(rules))}
	var query []string
	for i, rule := range p.rules {
		// A rule with no value for the input gives an empty list.
		query = append(query, fmt.Sprintf("r%d := [x | x := data.%s.%s]", i, Namespace, rule))
	}
	ss := ast.NewSchemaSet()
	ss.Put(ast.SchemaRootRef, inputSchema)
	opts := []func(*rego.Rego){
		rego.Query(strings.Join(query, "; ")),
		rego.Schemas(ss),
		rego.Capabilities(capabilities),
		rego.StrictBuiltinErrors(true),
		rego.SetRegoVersion(ast.RegoV1),
	}
	for _, m := range modules {
		opts = append(opts, rego.ParsedModule(m))
	}
	q, err := rego.New(opts...).PrepareForEval(context.Background())
	if err != nil {
		opaErrors(err, refuse)
		return nil, problems
	}
	p.query = q
	return p, nil
}

// opaErrors turns OPA's errors into problems at their locations.
func opaErrors(err error, refuse func(*ast.Location, string, ...any)) {
	var list ast.Errors
	if errors.As(err, &list) && len(list) > 0 {
		for _, e := range list {
			refuse(e.Location, "%s: %s", e.Code, e.Message)
		}
		return
	}
	refuse(nil, "%v", err)
}
