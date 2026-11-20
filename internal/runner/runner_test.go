package runner_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/runas"
	"github.com/mr-karan/writ/internal/runner"
	"github.com/mr-karan/writ/internal/tfstate"
)

// held are the packages whose values never cross to an executor: the
// keys and sealing, the database, the forge, people's sign-in, grants,
// policy, the mask, the node's AWS identity, and uids.
var held = []string{
	"internal/control", "internal/tfstate", "internal/mask", "internal/oidc", "internal/authz",
	"internal/sqldb", "internal/policy", "internal/forge", "internal/awsrole", "internal/runas", "internal/server",
}

// notPlain lists each place in t, a field's path and its type, that is
// not plain data: a function, a channel, an interface other than error,
// or a type of a package in held or of net/http.
func notPlain(t reflect.Type) []string {
	var out []string
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type, at string)
	walk = func(t reflect.Type, at string) {
		if seen[t] {
			return
		}
		seen[t] = true
		pkg := t.PkgPath()
		if pkg == "net/http" || strings.HasPrefix(pkg, "github.com/mr-karan/writ/") && containsSuffix(held, pkg) {
			out = append(out, at+" "+t.String())
			return
		}
		switch t.Kind() {
		case reflect.Func, reflect.Chan, reflect.UnsafePointer:
			out = append(out, at+" "+t.String())
		case reflect.Interface:
			if t != reflect.TypeFor[error]() {
				out = append(out, at+" "+t.String())
			}
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(t.Elem(), at+"[]")
		case reflect.Map:
			walk(t.Key(), at+"{key}")
			walk(t.Elem(), at+"{}")
		case reflect.Struct:
			for i := range t.NumField() {
				f := t.Field(i)
				walk(f.Type, at+"."+f.Name)
			}
		}
	}
	walk(t, t.Name())
	return out
}

func containsSuffix(xs []string, s string) bool {
	for _, x := range xs {
		if strings.HasSuffix(s, "/"+x) {
			return true
		}
	}
	return false
}

// A job and its result are plain data, as they would cross a wire to a
// runner: no key, no handler, no uid, and nothing that runs code of writ
// serve's.
func TestJobIsPlainData(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[runner.Job](), reflect.TypeFor[runner.Result]()} {
		if bad := notPlain(typ); len(bad) > 0 {
			t.Errorf("%s is not plain data: %s", typ, strings.Join(bad, ", "))
		}
	}
}

// The check of TestJobIsPlainData finds each kind of field it guards
// against, in a job that gained one.
func TestNotPlainFindsHeldFields(t *testing.T) {
	type withKey struct {
		runner.Job
		Key *tfstate.Key
	}
	type withHandler struct {
		runner.Job
		Ports map[int]http.Handler
	}
	type withUser struct {
		runner.Job
		User *runas.User
	}
	type withFunc struct {
		runner.Job
		Served func() []string
	}
	for _, typ := range []reflect.Type{reflect.TypeFor[withKey](), reflect.TypeFor[withHandler](), reflect.TypeFor[withUser](), reflect.TypeFor[withFunc]()} {
		if bad := notPlain(typ); len(bad) != 1 {
			t.Errorf("%s: found %q, want the one field it added", typ, bad)
		}
	}
}
