package provider_test

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// forms lists the forms of s a provider may print: as it is, quoted with
// %q or strconv.Quote, with the quotes trimmed, JSON-escaped, base64, and
// hex.
func forms(s string) []string {
	q := strconv.Quote(s)
	j, _ := json.Marshal(s)
	return []string{s, q, q[1 : len(q)-1], string(j), string(j[1 : len(j)-1]),
		base64.StdEncoding.EncodeToString([]byte(s)), hex.EncodeToString([]byte(s))}
}

func TestShowWithholdsProviderText(t *testing.T) {
	const key = `synthetic"secret\key`
	const password = `synthetic"pa ss\word`
	block := &schema.Block{Attributes: []schema.Attribute{
		{Name: "credentials", Type: types.Secret(types.MapOf(types.TString)), Optional: true, Sensitive: true},
		{Name: "keepers", Type: types.MapOf(types.TString), Optional: true},
		{Name: "password", Type: types.Secret(types.TString), Optional: true, Sensitive: true},
		{Name: "name", Type: types.TString, Optional: true},
	}}
	obj := func(fields map[string]value.Value) value.Value {
		for _, n := range []string{"credentials", "keepers", "password", "name"} {
			if _, ok := fields[n]; !ok {
				fields[n] = value.NullVal()
			}
		}
		return value.ObjectVal(fields)
	}
	secretMap := obj(map[string]value.Value{"credentials": value.ObjectVal(map[string]value.Value{key: value.StringVal("synthetic-keeper-value")})})
	markedMap := obj(map[string]value.Value{"keepers": value.ObjectVal(map[string]value.Value{key: value.StringVal("synthetic-keeper-value")}).Secret()})
	scalar := obj(map[string]value.Value{"password": value.StringVal(password), "name": value.StringVal("web")})
	quoting := func(secret string, isError bool, p value.Path) provider.Diag {
		return provider.Diag{Error: isError, Summary: fmt.Sprintf("bad key %q", secret), Detail: fmt.Sprintf("invalid credential %q", secret), Path: p}
	}
	held := func(d provider.Diag) provider.Diag {
		d.Held = true
		return d
	}
	for _, tc := range []struct {
		name   string
		secret string
		state  value.Value
		d      provider.Diag
		want   string
	}{
		{"sensitive map key at its path", key, secretMap, quoting(key, true, value.Path{}.Attr("credentials").Key(key)), "credentials[(secret)]: " + provider.WithheldAtSecret},
		{"sensitive map key, no path", key, secretMap, quoting(key, false, nil), "the provider sent a warning, and Writ withholds its text because this object holds a secret"},
		{"marked map key at its path", key, markedMap, quoting(key, false, value.Path{}.Attr("keepers").Key(key)), "keepers[(secret)]: the provider sent a warning about a secret, and Writ withholds its text"},
		{"marked map key, no path", key, markedMap, quoting(key, true, nil), provider.WithheldOnSecret},
		{"scalar password, no path", password, scalar, quoting(password, true, nil), provider.WithheldOnSecret},
		{"scalar password, public path", password, scalar, quoting(password, true, value.Path{}.Attr("name")), "name: " + provider.WithheldOnSecret},
		{"scalar password, invented key", password, scalar, quoting(password, true, value.Path{}.Attr("keepers").Key(password)), "keepers[(secret)]: " + provider.WithheldOnSecret},
		{"an unknown password the provider fills in", password, obj(map[string]value.Value{"password": value.UnknownVal(true)}), quoting(password, true, nil), provider.WithheldOnSecret},
		{"a public object from a process that holds a secret", password, obj(map[string]value.Value{"name": value.StringVal("web")}), held(quoting(password, true, nil)), provider.WithheldInProcess},
		{"a key the provider invents, from a process that holds a secret", password, obj(map[string]value.Value{"name": value.StringVal("web")}), held(quoting(password, true, value.Path{}.Attr("keepers").Key(password))), "keepers[(secret)]: " + provider.WithheldInProcess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := provider.Subject{Block: block, Values: []value.Value{tc.state}}
			got := sub.Show(tc.d)
			if got.Text != tc.want || !got.Withheld {
				t.Fatalf("Show = %+v, want %q withheld", got, tc.want)
			}
			err := sub.Err(&provider.Error{Op: "PlanResourceChange", Type: "x_thing", Diags: []provider.Diag{tc.d}}, value.Redactor{})
			if _, ok := errors.AsType[*provider.WithheldError](err); tc.d.Error && !ok {
				t.Fatalf("Err = %T %v, want a *provider.WithheldError", err, err)
			}
			text := got.Text + "\n" + err.Error()
			for _, f := range forms(tc.secret) {
				if strings.Contains(text, f) {
					t.Fatalf("the text holds the secret as %s:\n%s", f, text)
				}
			}
		})
	}

	t.Run("an object with no secret keeps the provider's text", func(t *testing.T) {
		public := obj(map[string]value.Value{"name": value.StringVal("web"), "keepers": value.ObjectVal(map[string]value.Value{"k": value.StringVal("synthetic-keeper-value")})})
		sub := provider.Subject{Block: block, Values: []value.Value{public, obj(map[string]value.Value{"password": value.NullVal()})}}
		got := sub.Show(provider.Diag{Error: true, Summary: "bad name", Detail: `"web" is taken`, Path: value.Path{}.Attr("keepers").Key("k")})
		if want := `keepers.k: bad name: "web" is taken`; got.Text != want || got.Withheld {
			t.Fatalf("Show = %+v, want %q shown", got, want)
		}
	})
}

// TestErrorWithholdsProcessText covers a call that failed with no
// diagnostics: a gRPC status whose message quotes a secret, and a crash
// whose stderr quotes it. On a process or an object that holds a secret,
// the status code and the facts of the crash stay, and the provider's text
// goes.
func TestErrorWithholdsProcessText(t *testing.T) {
	const secret = `synthetic"pa ss\word`
	quoted := status.Errorf(codes.Internal, "bad value %q", secret)
	stderr := fmt.Sprintf("panic: bad value %q\n", secret)
	secretObj := value.ObjectVal(map[string]value.Value{"password": value.StringVal(secret).Secret()})
	public := value.ObjectVal(map[string]value.Value{"name": value.StringVal("web")})
	for _, tc := range []struct {
		name string
		err  *provider.ProcessError
		obj  value.Value
		want string
	}{
		{"status, held process", &provider.ProcessError{What: "provider p: PlanResourceChange x_thing", Err: quoted, Held: true}, public,
			"provider p: PlanResourceChange x_thing: rpc error: code = Internal, and Writ withholds the provider's message"},
		{"status, secret object", &provider.ProcessError{What: "provider p: PlanResourceChange x_thing", Err: quoted}, secretObj,
			"provider p: PlanResourceChange x_thing: rpc error: code = Internal, and Writ withholds the provider's message"},
		{"crash", &provider.ProcessError{What: "provider p: ApplyResourceChange x_thing", Err: status.Error(codes.Unavailable, "error reading from server: EOF"), Stderr: stderr, Exited: true, Held: true}, public,
			"provider p: ApplyResourceChange x_thing: rpc error: code = Unavailable, and Writ withholds the provider's message; the provider exited; the provider wrote to stderr, and Writ withholds it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := provider.Subject{Values: []value.Value{tc.obj}}.Error(fmt.Errorf("start: %w", tc.err), value.Redactor{})
			if _, ok := errors.AsType[*provider.WithheldError](err); !ok || err.Error() != "start: "+tc.want {
				t.Fatalf("Error = %T %q, want a *provider.WithheldError %q", err, err, "start: "+tc.want)
			}
			for _, f := range forms(secret) {
				if strings.Contains(err.Error(), f) {
					t.Fatalf("the text holds the secret as %s: %s", f, err)
				}
			}
		})
	}
	t.Run("a process with no secret keeps its text", func(t *testing.T) {
		pe := &provider.ProcessError{What: "provider p: PlanResourceChange x_thing", Err: status.Error(codes.Internal, "boom"), Stderr: "trace\n"}
		if err := (provider.Subject{Values: []value.Value{public}}).Error(pe, value.Redactor{}); err.Error() != pe.Error() {
			t.Fatalf("Error = %q, want %q", err, pe.Error())
		}
	})
}
