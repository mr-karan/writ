package archtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An identity provider controls much of what internal/oidc reads, and any
// of it can hold a credential. Three reviews found that text reaching
// writ serve's answers, logs, and audit rows through error messages, each
// time at a new site. So the boundary is a type: internal/oidc reports a
// failure only as a *oidc.Refusal, whose fields are enums and numbers, and
// whose only text is its Message. These tests hold that shape.

const oidcPath = module + "/internal/oidc"

// renderers are the packages that turn a refusal into an answer, a log
// line, an audit row, or a page.
var renderers = []string{"internal/server", "internal/ui", "internal/cli", "cmd/writ"}

// TestOIDCReturnsNoErrors checks that no exported function or method of
// internal/oidc returns an error, that no exported type of it is an error,
// and that a Refusal holds only integers.
func TestOIDCReturnsNoErrors(t *testing.T) {
	pkg := typecheck(t, "internal/oidc")["internal/oidc"].pkg
	errType := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		switch o := obj.(type) {
		case *types.Func:
			checkResults(t, o, errType)
		case *types.TypeName:
			named, ok := o.Type().(*types.Named)
			if !ok {
				continue
			}
			if types.Implements(named, errType) || types.Implements(types.NewPointer(named), errType) {
				t.Errorf("oidc.%s is an error; internal/oidc reports failures only as a *Refusal", name)
			}
			for m := range named.Methods() {
				if m.Exported() {
					checkResults(t, m, errType)
				}
			}
		case *types.Var:
			if types.Implements(o.Type(), errType) {
				t.Errorf("oidc.%s is an error value; internal/oidc reports failures only as a *Refusal", name)
			}
		}
	}
	refusal := scope.Lookup("Refusal")
	if refusal == nil {
		t.Fatal("internal/oidc has no Refusal type")
	}
	st, ok := refusal.Type().Underlying().(*types.Struct)
	if !ok {
		t.Fatal("oidc.Refusal is not a struct")
	}
	for f := range st.Fields() {
		b, ok := f.Type().Underlying().(*types.Basic)
		if !ok || b.Info()&types.IsInteger == 0 {
			t.Errorf("oidc.Refusal.%s is a %s; a refusal holds only enums and numbers, so it cannot carry an identity provider's text", f.Name(), f.Type())
		}
	}
}

func checkResults(t *testing.T, fn *types.Func, errType *types.Interface) {
	t.Helper()
	sig := fn.Type().(*types.Signature)
	for v := range sig.Results().Variables() {
		if types.Implements(v.Type(), errType) {
			t.Errorf("%s returns %s; internal/oidc reports failures only as a *Refusal", fn.FullName(), v.Type())
		}
	}
}

// TestOIDCRefusalsStayTyped checks every package that renders refusals:
// a *oidc.Refusal never becomes an interface value, so it cannot reach
// fmt, log/slog, an error, or an audit row except through Message, and no
// value of an internal/oidc type has Error called on it.
func TestOIDCRefusalsStayTyped(t *testing.T) {
	pkgs := typecheck(t, renderers...)
	for _, dir := range renderers {
		p := pkgs[dir]
		for _, v := range boundaryViolations(p.fset, p.files, p.info) {
			t.Error(v)
		}
	}
}

// TestOIDCBoundaryCatches runs the check of TestOIDCRefusalsStayTyped on
// code that breaks it, so the check is known to see each way.
func TestOIDCBoundaryCatches(t *testing.T) {
	src := `package probe

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/mr-karan/writ/internal/oidc"
)

type row struct{ why any }

func leak(r *oidc.Refusal, log *slog.Logger) (error, any) {
	_ = fmt.Sprintf("%v", r)
	log.Warn("refused", "err", r)
	var v any = r
	_ = v
	_ = row{why: r}
	_ = []any{r}
	_ = any(r)
	ok := errors.New("x: " + r.Message())
	return ok, r
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "probe.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := newInfo()
	conf := types.Config{Importer: exportImporter(t, fset, oidcPath, "fmt", "errors", "log/slog")}
	if _, err := conf.Check("probe", fset, []*ast.File{f}, info); err != nil {
		t.Fatal(err)
	}
	got := boundaryViolations(fset, []*ast.File{f}, info)
	want := []string{"probe.go:14:", "probe.go:15:", "probe.go:16:", "probe.go:18:", "probe.go:19:", "probe.go:20:", "probe.go:22:"}
	if len(got) != len(want) {
		t.Fatalf("got %d violations, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i], w) {
			t.Errorf("violation %d is %q, want one at %s", i, got[i], w)
		}
	}
}

// boundaryViolations finds each place in files where a value of type
// oidc.Refusal or *oidc.Refusal becomes an interface value, and each call
// of Error on a value of an internal/oidc type.
func boundaryViolations(fset *token.FileSet, files []*ast.File, info *types.Info) []string {
	var out []string
	report := func(n ast.Node, what string) {
		out = append(out, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), what))
	}
	isRefusal := func(e ast.Expr) bool {
		tv, ok := info.Types[e]
		return ok && refusalType(tv.Type)
	}
	toInterface := func(target types.Type, e ast.Expr, where string) {
		if target != nil && types.IsInterface(target) && isRefusal(e) {
			report(e, "a *oidc.Refusal becomes "+target.String()+" "+where+"; render it with Message")
		}
	}
	for _, f := range files {
		var results []*types.Tuple
		var walk func(n ast.Node) bool
		walk = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if obj, ok := info.Defs[n.Name].(*types.Func); ok && n.Body != nil {
					results = append(results, obj.Type().(*types.Signature).Results())
					ast.Inspect(n.Body, walk)
					results = results[:len(results)-1]
				}
				return false
			case *ast.FuncLit:
				if sig, ok := info.Types[n].Type.(*types.Signature); ok {
					results = append(results, sig.Results())
					ast.Inspect(n.Body, walk)
					results = results[:len(results)-1]
				}
				return false
			case *ast.CallExpr:
				if tv, ok := info.Types[n.Fun]; ok && tv.IsType() {
					if len(n.Args) == 1 {
						toInterface(tv.Type, n.Args[0], "in a conversion")
					}
					return true
				}
				sig, ok := info.Types[n.Fun].Type.(*types.Signature)
				if !ok {
					return true
				}
				params := sig.Params()
				for i, a := range n.Args {
					var pt types.Type
					switch {
					case sig.Variadic() && i >= params.Len()-1 && !n.Ellipsis.IsValid():
						pt = params.At(params.Len() - 1).Type().(*types.Slice).Elem()
					case i < params.Len():
						pt = params.At(i).Type()
					}
					toInterface(pt, a, "as an argument")
				}
			case *ast.SelectorExpr:
				if n.Sel.Name == "Error" {
					if tv, ok := info.Types[n.X]; ok && fromOIDC(tv.Type) {
						report(n, "Error is called on a value of "+tv.Type.String())
					}
				}
			case *ast.AssignStmt:
				if len(n.Lhs) == len(n.Rhs) {
					for i := range n.Lhs {
						if tv, ok := info.Types[n.Lhs[i]]; ok {
							toInterface(tv.Type, n.Rhs[i], "in an assignment")
						}
					}
				}
			case *ast.ValueSpec:
				if n.Type != nil && len(n.Values) == len(n.Names) {
					for _, v := range n.Values {
						toInterface(info.Types[n.Type].Type, v, "in a declaration")
					}
				}
			case *ast.ReturnStmt:
				if len(results) > 0 {
					res := results[len(results)-1]
					if res.Len() == len(n.Results) {
						for i, r := range n.Results {
							toInterface(res.At(i).Type(), r, "in a return")
						}
					}
				}
			case *ast.SendStmt:
				if ch, ok := info.Types[n.Chan].Type.Underlying().(*types.Chan); ok {
					toInterface(ch.Elem(), n.Value, "in a channel send")
				}
			case *ast.CompositeLit:
				t := info.Types[n].Type
				if t == nil {
					return true
				}
				if p, ok := t.Underlying().(*types.Pointer); ok {
					t = p.Elem()
				}
				for i, el := range n.Elts {
					val, key := el, ast.Expr(nil)
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						val, key = kv.Value, kv.Key
					}
					switch u := t.Underlying().(type) {
					case *types.Struct:
						if id, ok := key.(*ast.Ident); ok {
							for fld := range u.Fields() {
								if fld.Name() == id.Name {
									toInterface(fld.Type(), val, "in a struct field")
								}
							}
						} else if key == nil && i < u.NumFields() {
							toInterface(u.Field(i).Type(), val, "in a struct field")
						}
					case *types.Slice:
						toInterface(u.Elem(), val, "in a slice")
					case *types.Array:
						toInterface(u.Elem(), val, "in an array")
					case *types.Map:
						toInterface(u.Elem(), val, "in a map")
						if key != nil {
							toInterface(u.Key(), key, "as a map key")
						}
					}
				}
			}
			return true
		}
		ast.Inspect(f, walk)
	}
	return out
}

func refusalType(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == oidcPath && n.Obj().Name() == "Refusal"
}

func fromOIDC(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == oidcPath
}

type checked struct {
	pkg   *types.Package
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
}

func newInfo() *types.Info {
	return &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
}

// typecheck type-checks each package dir, relative to the module root,
// from its source files, which this test reads itself so the test cache
// sees them. Imports come from the export data go list builds.
func typecheck(t *testing.T, dirs ...string) map[string]*checked {
	t.Helper()
	var imports []string
	parsed := map[string][]*ast.File{}
	fset := token.NewFileSet()
	for _, dir := range dirs {
		names, err := filepath.Glob(filepath.Join("../..", dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			if ok, err := build.Default.MatchFile(filepath.Dir(name), filepath.Base(name)); err != nil {
				t.Fatal(err)
			} else if !ok {
				continue
			}
			src, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			parsed[dir] = append(parsed[dir], f)
			for _, spec := range f.Imports {
				imports = append(imports, strings.Trim(spec.Path.Value, `"`))
			}
		}
	}
	imp := exportImporter(t, fset, imports...)
	out := map[string]*checked{}
	for _, dir := range dirs {
		info := newInfo()
		conf := types.Config{Importer: imp}
		pkg, err := conf.Check(module+"/"+dir, fset, parsed[dir], info)
		if err != nil {
			t.Fatalf("type-check %s: %v", dir, err)
		}
		out[dir] = &checked{pkg: pkg, fset: fset, files: parsed[dir], info: info}
	}
	return out
}

// exportImporter imports packages from the export data that go list
// -export builds for imports and their dependencies.
func exportImporter(t *testing.T, fset *token.FileSet, imports ...string) types.Importer {
	t.Helper()
	args := append([]string{"list", "-export", "-deps", "-json=ImportPath,Export"}, unique(imports)...)
	cmd := exec.Command("go", args...)
	cmd.Dir = "../.."
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	exports := map[string]string{}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	for {
		var p struct{ ImportPath, Export string }
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
	}
	return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
}

func unique(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
