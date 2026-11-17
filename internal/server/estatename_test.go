package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/control"
)

// TestNestedPrefix: two set prefixes that differ and start one another are
// nested, in either order. The same prefix, an unset one, and prefixes that
// merely share letters are not.
func TestNestedPrefix(t *testing.T) {
	repos := []control.Repo{{Name: "plain"}, {Name: "pay", Prefix: "payments/"}, {Name: "deep", Prefix: "a/b/"}}
	for _, tc := range []struct {
		prefix string
		other  string
	}{
		{"payments/eu/", "pay"},
		{"a/", "deep"},
		{"a/b/c/", "deep"},
		{"a/b/", ""},
		{"payments/", ""},
		{"pay/", ""},
		{"b/", ""},
		{"", ""},
	} {
		o, ok := nestedPrefix(repos, control.Repo{Name: "new", Prefix: tc.prefix})
		if o.Name != tc.other || ok != (tc.other != "") {
			t.Errorf("nestedPrefix(%q) = %q, %v, want %q", tc.prefix, o.Name, ok, tc.other)
		}
	}
}

// TestPrefixShadow: a stack name that starts with the prefix of another
// repository is shadowed, whichever repository holds the stack, and a name
// with its own repository's prefix, or one no prefix starts, is not.
func TestPrefixShadow(t *testing.T) {
	stacks := func(dirs ...string) []byte {
		var ds []discovered
		for _, d := range dirs {
			ds = append(ds, discovered{Dir: d, Tofu: true})
		}
		b, _ := json.Marshal(ds)
		return b
	}
	rs := func(name, prefix string, dirs ...string) control.RepoStacks {
		return control.RepoStacks{Repo: control.Repo{Name: name, Prefix: prefix}, Stacks: stacks(dirs...)}
	}
	for _, tc := range []struct {
		name   string
		self   control.RepoStacks
		others []control.RepoStacks
		both   bool
		want   string
	}{
		{"own prefix", rs("a", "a/", "x"), []control.RepoStacks{rs("b", "", "y")}, true, ""},
		{"an unprefixed stack under a prefix", rs("a", "", "q/x"), []control.RepoStacks{rs("b", "q/", "y")}, false, "stack q/x of repository a starts with prefix \"q/\" of repository b"},
		{"a new prefix over another's stack", rs("a", "q/", "y"), []control.RepoStacks{rs("b", "", "q/x")}, true, "stack q/x of repository b starts with prefix \"q/\" of repository a"},
		{"a head does not look at the others' stacks", rs("a", "q/", "y"), []control.RepoStacks{rs("b", "", "q/x")}, false, ""},
		{"the same prefix", rs("a", "q/", "x"), []control.RepoStacks{rs("b", "q/", "y")}, true, ""},
		{"no slash boundary", rs("a", "", "qx/y"), []control.RepoStacks{rs("b", "q/", "y")}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := prefixShadow(tc.self, tc.others, tc.both)
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Errorf("prefixShadow = %q, want %q", got, tc.want)
			}
		})
	}
}
