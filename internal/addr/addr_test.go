package addr_test

import (
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/addr"
)

func TestStringMatchesSpecExamples(t *testing.T) {
	tests := []struct {
		name string
		addr addr.Addr
		want string
	}{
		{"let", addr.Root("ca_key"), "ca_key"},
		{"record field", addr.Root("site").Field("key"), "site.key"},
		{"for key", addr.Root("files").Key("a"), `files["a"]`},
		{"nested", addr.Root("envs").Key("prod").Field("certs").Key("api"), `envs["prod"].certs["api"]`},
		{"field that is not an identifier", addr.Root("site").Field("my-key"), `site["my-key"]`},
		{"empty key", addr.Root("files").Key(""), `files[""]`},
		{"escaped key", addr.Root("files").Key("a\"b\\c\nd\te"), `files["a\"b\\c\nd\te"]`},
		{"interpolation is escaped", addr.Root("files").Key("${x}"), `files["\${x}"]`},
		{"lone dollar is literal", addr.Root("files").Key("$HOME"), `files["$HOME"]`},
		{"key with dots and brackets", addr.Root("files").Key(`a.b["c"]`), `files["a.b[\"c\"]"]`},
		{"unicode key", addr.Root("files").Key("café"), `files["café"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.addr.String(); got != tt.want {
				t.Fatalf("String() = %s, want %s", got, tt.want)
			}
			back, err := addr.Parse(tt.want)
			if err != nil {
				t.Fatalf("Parse(%s): %v", tt.want, err)
			}
			if back != tt.addr {
				t.Fatalf("Parse(%s) = %s, want the same address", tt.want, back)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		in      string
		errPart string
	}{
		{"", "empty address"},
		{"1x", "starts with a let name"},
		{".a", "starts with a let name"},
		{`["a"]`, "starts with a let name"},
		{"a.", "expected a field name"},
		{"a..b", "expected a field name"},
		{"a.1", "expected a field name"},
		{"a b", `unexpected ' '`},
		{"a-b", `unexpected '-'`},
		{"files[a]", "must be a quoted string"},
		{"files['a']", "must be a quoted string"},
		{"files[0]", "must be a quoted string"},
		{`files["a"`, `expected "]"`},
		{`files["a`, "unterminated key"},
		{`files["a\`, "unterminated key"},
		{`files["a\x"]`, `unknown escape \x`},
		{`files["a"]x`, `unexpected 'x'`},
		{`files[ "a"]`, "must be a quoted string"},
		// Accepted by the grammar but not what String prints.
		{`files["\$x"]`, `not in canonical form, write files["$x"]`},
		{`files["${x}"]`, `not in canonical form, write files["\${x}"]`},
		{"files[\"a\nb\"]", `not in canonical form, write files["a\nb"]`},
		{"files[\"a\tb\"]", `not in canonical form, write files["a\tb"]`},
		{`site["key"]x`, `unexpected 'x'`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			a, err := addr.Parse(tt.in)
			if err == nil {
				t.Fatalf("Parse(%q) = %s, want an error", tt.in, a)
			}
			if !strings.Contains(err.Error(), tt.errPart) {
				t.Fatalf("Parse(%q) error = %q, want it to contain %q", tt.in, err, tt.errPart)
			}
		})
	}
}

func TestFieldAndKeyAreDistinctAddresses(t *testing.T) {
	field := addr.Root("site").Field("key")
	key := addr.Root("site").Key("key")
	if field == key {
		t.Fatalf("site.key and site[\"key\"] must differ, both are %s", field)
	}
	if got, err := addr.Parse(`site["key"]`); err != nil || got != key {
		t.Fatalf(`Parse(site["key"]) = %s, %v; want the key address`, got, err)
	}
}

func TestLet(t *testing.T) {
	tests := []struct {
		addr addr.Addr
		want string
	}{
		{addr.Root("ca_key"), "ca_key"},
		{addr.Root("site").Field("key"), "site"},
		{addr.Root("files").Key("a.b"), "files"},
	}
	for _, tt := range tests {
		t.Run(tt.addr.String(), func(t *testing.T) {
			if got := tt.addr.Let(); got != tt.want {
				t.Fatalf("Let() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestZero(t *testing.T) {
	var zero addr.Addr
	if !zero.IsZero() {
		t.Fatal("the zero Addr must report IsZero")
	}
	if addr.Root("a").IsZero() {
		t.Fatal("a rooted address is not zero")
	}
}

func TestRootPanicsOnNonIdentifier(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Root(\"a-b\") must panic")
		}
	}()
	addr.Root("a-b")
}

func TestQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", `""`},
		{"plain", `"plain"`},
		{`say "hi"`, `"say \"hi\""`},
		{`back\slash`, `"back\\slash"`},
		{"line\nbreak", `"line\nbreak"`},
		{"tab\there", `"tab\there"`},
		{"${name}", `"\${name}"`},
		{"cost $5", `"cost $5"`},
		{"trailing $", `"trailing $"`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := addr.Quote(tt.in); got != tt.want {
				t.Fatalf("Quote(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsIdent(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"a", true},
		{"_", true},
		{"ca_key", true},
		{"Key2", true},
		{"", false},
		{"2a", false},
		{"a-b", false},
		{"a.b", false},
		{"café", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := addr.IsIdent(tt.in); got != tt.want {
				t.Fatalf("IsIdent(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestShellWord(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ca_key", "ca_key"},
		{"site.key", "site.key"},
		{"clients=workers", "clients=workers"},
		{"sg-0123abcd", "sg-0123abcd"},
		{"default/hello", "default/hello"},
		{`files["a"]`, `'files["a"]'`},
		{"two words", "'two words'"},
		{"it's", `'it'\''s'`},
		{"$HOME", "'$HOME'"},
		{"", "''"},
	}
	for _, tt := range tests {
		if got := addr.ShellWord(tt.in); got != tt.want {
			t.Errorf("ShellWord(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}
