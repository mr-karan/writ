package archtest

import (
	"go/ast"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// stateSink is the one function that may read tofu.State's Unmasked
// outside internal/tofu.
const stateSink = "(*" + module + "/internal/mask.Mask).Index"

// TestStateOnlyToMask keeps a state's raw `tofu show -json`, which holds
// every value the state holds, out of every package but internal/tofu,
// which reads it, and internal/mask, which turns it into rows of the
// state index: outside internal/tofu, tofu.State's Unmasked may only be
// read as the first argument of a call of the mask's Index, and no
// non-test file may set it. The callee is resolved by its types, so a
// method of another type or package named Index is refused.
//
// It reads every non-test Go file itself, so the test cache sees each
// file it depends on, and type-checks the packages of those that name
// Unmasked.
func TestStateOnlyToMask(t *testing.T) {
	root := os.DirFS("../..")
	var naming []string
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (strings.HasPrefix(d.Name(), ".") && p != "." || d.Name() == "testdata") {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || path.Dir(p) == "internal/tofu" {
			return nil
		}
		src, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), "Unmasked") {
			naming = append(naming, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(naming) == 0 {
		t.Fatal("no file outside internal/tofu names Unmasked, so the mask indexes no state; the walk is broken")
	}
	var dirs []string
	for _, p := range naming {
		if d := "./" + path.Dir(p); !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	checked := map[string]bool{}
	for _, pkg := range loadTyped(t, dirs...) {
		for _, f := range pkg.CompiledGoFiles {
			checked[f] = true
		}
		for _, bad := range unmaskedUses(pkg) {
			t.Errorf("%s: Unmasked is used outside a call of %s", pkg.Fset.Position(bad), stateSink)
		}
	}
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range naming {
		if !checked[filepath.Join(abs, filepath.FromSlash(p))] {
			t.Errorf("%s names Unmasked but is in no package the default build type-checks, so the rule cannot check it", p)
		}
	}
}

// The rule refuses the state given to a method named Index of another
// type, and to one of another package that is also named mask.
func TestStateOnlyToMaskRefusesOtherSinks(t *testing.T) {
	pkgs := loadTyped(t, "./internal/archtest/testdata/leak")
	var lines []int
	for _, pos := range unmaskedUses(pkgs[0]) {
		lines = append(lines, pkgs[0].Fset.Position(pos).Line)
	}
	if want := []int{16, 18}; !slices.Equal(lines, want) {
		t.Fatalf("the rule refused Unmasked on lines %v of testdata/leak/leak.go, want %v", lines, want)
	}
}

// loadTyped type-checks the packages in dirs, relative to the module's
// root, from their source.
func loadTyped(t *testing.T, dirs ...string) []*packages.Package {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  "../..",
	}
	pkgs, err := packages.Load(cfg, dirs...)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != len(dirs) {
		t.Fatalf("loaded %d packages of %v", len(pkgs), dirs)
	}
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			t.Errorf("type-check %s: %v", pkg.PkgPath, e)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	return pkgs
}

// unmaskedUses returns where pkg names tofu.State's Unmasked other than as
// the first argument of a call of stateSink.
func unmaskedUses(pkg *packages.Package) []token.Pos {
	isField := func(id *ast.Ident) bool {
		v, ok := pkg.TypesInfo.ObjectOf(id).(*types.Var)
		return ok && v.IsField() && v.Name() == "Unmasked" && v.Pkg() != nil && v.Pkg().Path() == module+"/internal/tofu"
	}
	allowed := map[*ast.Ident]bool{}
	var bad []token.Pos
	for _, f := range pkg.Syntax {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			fn, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if callee, ok := pkg.TypesInfo.Uses[fn.Sel].(*types.Func); ok && callee.FullName() == stateSink {
				if arg, ok := call.Args[0].(*ast.SelectorExpr); ok {
					allowed[arg.Sel] = true
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && isField(id) && !allowed[id] {
				bad = append(bad, id.Pos())
			}
			return true
		})
	}
	return bad
}
