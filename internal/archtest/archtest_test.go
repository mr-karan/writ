package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/mr-karan/writ"

// owned lists imports that only some packages may use, per AGENTS.md: wire
// types stay inside the package that owns that boundary.
var owned = []struct {
	prefix string
	owners []string
}{
	{module + "/internal/tfplugin5", []string{"internal/provider"}},
	{module + "/internal/tfplugin6", []string{"internal/provider"}},
	{"google.golang.org/grpc", []string{"internal/provider", "internal/tfplugin5", "internal/tfplugin6"}},
	{"google.golang.org/protobuf", []string{"internal/provider", "internal/tfplugin5", "internal/tfplugin6"}},
	// The mask parses a plan's addresses with HCL, which gives each index
	// key as a cty value; no cty value leaves it.
	{"github.com/zclconf/go-cty", []string{"internal/provider", "internal/tfcode", "internal/mask"}},
	// The importer's client of the plugin protocol. writ serve runs
	// providers only through OpenTofu, so the control plane never needs it.
	{module + "/internal/provider", []string{"internal/importer", "internal/cli"}},
	{"database/sql", []string{"internal/sqldb"}},
	{"modernc.org/sqlite", []string{"internal/sqldb"}},
	{"github.com/jackc/pgx", []string{"internal/sqldb"}},
	{"github.com/aws/aws-sdk-go-v2", []string{"internal/awsrole"}},
	{"github.com/aws/smithy-go", []string{"internal/awsrole"}},
	{"github.com/modelcontextprotocol/go-sdk", []string{"internal/mcp"}},
	// Authentication protocols live behind internal/oidc, which adds Writ's
	// policy around them; nothing else speaks OAuth or OIDC.
	{"github.com/coreos/go-oidc", []string{"internal/oidc"}},
	{"github.com/go-jose/go-jose", []string{"internal/oidc"}},
	{"golang.org/x/oauth2", []string{"internal/oidc"}},
	// OPA evaluates policies only. It links jwx as a second JWT library,
	// which no Writ package may use.
	{"github.com/open-policy-agent/opa", []string{"internal/policy"}},
	{"github.com/lestrrat-go/jwx", nil},
}

// pure lists the packages of the value and schema model that the importer
// and the provider client share. They may not do I/O or read the clock, so
// what they compute depends on their arguments alone.
var pure = []string{
	"internal/addr", "internal/diag", "internal/schema", "internal/types", "internal/value",
}

// thin lists what the control UI may not import: it renders the API's
// documents and reaches data only through the server, so a page holds no
// rule of its own.
var thin = []string{module + "/internal/control", "github.com/jackc/pgx"}

var impure = []string{
	"os", "os/exec", "os/signal", "net", "net/http", "io/fs", "path/filepath",
	"syscall", "time", "math/rand", "math/rand/v2", "crypto/rand",
}

// TestImportBoundaries parses the imports of every non-test Go file in the
// module. It reads the files itself, rather than running go list, so the test
// cache sees every file it depends on.
func TestImportBoundaries(t *testing.T) {
	root := os.DirFS("../..")
	fset := token.NewFileSet()
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (strings.HasPrefix(d.Name(), ".") && p != "." || d.Name() == "testdata") {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, p, src, parser.ImportsOnly)
		if err != nil {
			return err
		}
		pkg := path.Dir(p)
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			check(t, p, pkg, imp)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func check(t *testing.T, file, pkg, imp string) {
	t.Helper()
	for _, o := range owned {
		switch {
		case !strings.HasPrefix(imp, o.prefix) || contains(o.owners, pkg):
		case len(o.owners) == 0:
			t.Errorf("%s imports %s; no Writ package may", file, imp)
		default:
			t.Errorf("%s imports %s; only %v may", file, imp, o.owners)
		}
	}
	// A policy reads only the masked plan document, as bytes, so
	// internal/policy imports nothing of Writ that could hand it a run's
	// raw output.
	if pkg == "internal/policy" && strings.HasPrefix(imp, module+"/") {
		t.Errorf("%s imports %s; internal/policy imports nothing of writ, so a policy input comes only from the masked plan document", file, imp)
	}
	if contains(pure, pkg) && contains(impure, imp) {
		t.Errorf("%s imports %s; the value and schema packages do no I/O", file, imp)
	}
	if strings.HasPrefix(pkg, "internal/ui") && slices.ContainsFunc(thin, func(p string) bool { return strings.HasPrefix(imp, p) }) {
		t.Errorf("%s imports %s; the control UI reaches data only through internal/server", file, imp)
	}
	if pkg == "internal/tofu" && strings.HasPrefix(imp, module+"/") && imp != module+"/internal/runas" {
		t.Errorf("%s imports %s; internal/tofu imports only internal/runas of writ, so what runs OpenTofu depends on nothing else", file, imp)
	}
	if pkg == "internal/runas" && strings.Contains(strings.SplitN(imp, "/", 2)[0], ".") {
		t.Errorf("%s imports %s; internal/runas imports only the standard library, so both packages that start processes may share it", file, imp)
	}
	if pkg == "cmd/writ" && strings.HasPrefix(imp, module+"/internal/") && imp != module+"/internal/cli" {
		t.Errorf("%s imports %s; cmd/writ may call only internal/cli", file, imp)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
