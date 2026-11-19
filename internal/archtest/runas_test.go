package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"
)

// TestUserSwitchingStartsHoldTheDumpLock fails when a command that may run
// as another user is started other than through runas.Start or
// runas.StartMapped. Go starts such a child with CLONE_VM, so its
// setresuid marks writ itself not dumpable, which must not land while
// runas.StartMapped starts a user namespace whose id maps writ writes.
//
// It reads every non-test Go file outside internal/runas. Within one
// function, a command may switch users when its SysProcAttr is set, when
// it comes from a Command method other than package exec's, as
// runas.User.Command, or from a function or method of its package that
// sets a SysProcAttr, or when it is passed to one that sets its
// parameter's SysProcAttr. Such a command may not call Start, Run,
// Output, or CombinedOutput. A credential is built only by runas, so every
// one comes through runas.SysProcAttr.
func TestUserSwitchingStartsHoldTheDumpLock(t *testing.T) {
	pkgs := map[string][]*ast.File{}
	fset := token.NewFileSet()
	root := os.DirFS("../..")
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (strings.HasPrefix(d.Name(), ".") && p != "." || d.Name() == "testdata" || p == "internal/runas") {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		pkgs[path.Dir(p)] = append(pkgs[path.Dir(p)], f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, files := range pkgs {
		for _, v := range userSwitchingStarts(fset, files) {
			t.Error(v)
		}
	}
}

func userSwitchingStarts(fset *token.FileSet, files []*ast.File) []string {
	setters, makers := map[string]int{}, map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			params := paramNames(fn.Type)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				for _, x := range sysProcAttrOwners(n) {
					if i, ok := params[types.ExprString(x)]; ok {
						setters[fn.Name.Name] = i
					}
					makers[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	var out []string
	report := func(n ast.Node, msg string) {
		out = append(out, fset.Position(n.Pos()).String()+": "+msg)
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			switching := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				for _, x := range sysProcAttrOwners(n) {
					switching[types.ExprString(x)] = true
				}
				switch n := n.(type) {
				case *ast.AssignStmt:
					for i, lhs := range n.Lhs {
						if len(n.Rhs) == len(n.Lhs) && makesSwitchingCommand(n.Rhs[i], makers) {
							switching[types.ExprString(lhs)] = true
						}
						if selects(lhs, "Credential") {
							report(n, "sets a Credential; take one from runas.SysProcAttr")
						}
					}
				case *ast.CallExpr:
					if i, ok := setters[calledName(n)]; ok && i < len(n.Args) {
						switching[types.ExprString(n.Args[i])] = true
					}
				case *ast.CompositeLit:
					if types.ExprString(n.Type) == "syscall.Credential" {
						report(n, "builds a syscall.Credential; take one from runas.SysProcAttr")
					}
					for _, e := range n.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok && types.ExprString(kv.Key) == "Credential" {
							report(n, "sets a Credential; take one from runas.SysProcAttr")
						}
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "Start", "Run", "Output", "CombinedOutput":
					if x := types.ExprString(sel.X); switching[x] {
						report(call, x+" may run as another user and calls "+sel.Sel.Name+"; start it with runas.Start, then Wait")
					}
				}
				return true
			})
		}
	}
	return out
}

// sysProcAttrOwners returns x of each x.SysProcAttr that n assigns, as in
// x.SysProcAttr = a or x.SysProcAttr.Setpgid = true.
func sysProcAttrOwners(n ast.Node) []ast.Expr {
	as, ok := n.(*ast.AssignStmt)
	if !ok {
		return nil
	}
	var out []ast.Expr
	for _, lhs := range as.Lhs {
		for e := lhs; ; {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				break
			}
			if sel.Sel.Name == "SysProcAttr" {
				out = append(out, sel.X)
			}
			e = sel.X
		}
	}
	return out
}

// selects reports whether e selects name anywhere along its chain.
func selects(e ast.Expr, name string) bool {
	for {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if sel.Sel.Name == name {
			return true
		}
		e = sel.X
	}
}

// makesSwitchingCommand reports whether e calls a Command method other
// than package exec's, or a function or method of the package that sets
// a SysProcAttr.
func makesSwitchingCommand(e ast.Expr, makers map[string]bool) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	if f, ok := call.Fun.(*ast.SelectorExpr); ok && f.Sel.Name == "Command" && types.ExprString(f.X) != "exec" {
		return true
	}
	return makers[calledName(call)]
}

// calledName returns the name of the function or method call calls, as
// f of f(), x.f(), or pkg.f(); a function of another package with the
// same name as one of this package's counts too.
func calledName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func paramNames(ft *ast.FuncType) map[string]int {
	out := map[string]int{}
	i := 0
	for _, field := range ft.Params.List {
		if len(field.Names) == 0 {
			i++
		}
		for _, n := range field.Names {
			out[n.Name] = i
			i++
		}
	}
	return out
}

func TestUserSwitchingStartsCases(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"a SysProcAttr set and Start", `cmd := exec.Command("x"); cmd.SysProcAttr = runas.SysProcAttr(u); _ = cmd.Start()`, 1},
		{"a field of SysProcAttr set and Run", `cmd := exec.Command("x"); cmd.SysProcAttr.Setpgid = true; _ = cmd.Run()`, 1},
		{"a user's Command and Output", `cmd := u.Command(ctx, "x"); _, _ = cmd.Output()`, 1},
		{"a user's Command and CombinedOutput", `cmd := u.Command(ctx, "x"); _, _ = cmd.CombinedOutput()`, 1},
		{"a setter of the package and Start", `p.cmd = exec.Command("x"); setAttr(p.cmd, u); _ = p.cmd.Start()`, 1},
		{"a maker of the package and Start", `cmd := makeCmd(); _ = cmd.Start()`, 1},
		{"a Credential built", `cmd := exec.Command("x"); cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{}}; _ = runas.Start(cmd)`, 2},
		{"a Credential set", `cmd := exec.Command("x"); cmd.SysProcAttr.Credential = c; _ = runas.Start(cmd)`, 1},
		{"through runas.Start", `cmd := u.Command(ctx, "x"); err := runas.Start(cmd); _ = cmd.Wait()`, 0},
		{"through runas.StartMapped", `cmd := exec.Command("x"); cmd.SysProcAttr = attr(); _ = runas.StartMapped(cmd)`, 0},
		{"a plain command", `cmd := exec.Command("x"); _ = cmd.Run(); out, _ := exec.Command("y").Output()`, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := "package p\nfunc setAttr(c *exec.Cmd, u *runas.User) { c.SysProcAttr = runas.SysProcAttr(u) }\n" +
				"func makeCmd() *exec.Cmd { c := exec.Command(\"x\"); c.SysProcAttr = runas.SysProcAttr(nil); return c }\n" +
				"func f() {\n" + strings.ReplaceAll(c.body, "; ", "\n") + "\n}\n"
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "p.go", src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			if got := userSwitchingStarts(fset, []*ast.File{f}); len(got) != c.want {
				t.Errorf("found %d, want %d: %q", len(got), c.want, got)
			}
		})
	}
}
