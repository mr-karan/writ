package tfcode

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

func TestStringReadsBack(t *testing.T) {
	for name, tc := range map[string]struct {
		s       string
		heredoc bool
	}{
		"plain":                    {"eu-west-1", false},
		"quotes and backslashes":   {`a "b" \c`, false},
		"template sequences":       {"${var.x} %{if y}", false},
		"one line with a break":    {"line\n", false},
		"lines":                    {"job \"web\" {\n  count = 1\n}\n", true},
		"lines with templates":     {"a ${b}\n%{c}\n", true},
		"lines with the marker":    {"a\nEOT\nb\n", true},
		"lines without a last one": {"a\nb", false},
		"a carriage return":        {"a\r\nb\r\n", false},
		"a control character":      {"a\x01\nb\n", false},
		"tabs and backslashes":     {"a\tb\\n\n\tc\n", true},
		"empty":                    {"", false},
		"an indented marker":       {"first\n  EOT\nlast\n", true},
		"a marker after a tab":     {"first\n\tEOT\nlast\n", true},
		"a marker before spaces":   {"first\nEOT  \nlast\n", true},
		"a padded marker":          {" \t EOT \t\nlast\n", true},
		"padded markers":           {"  EOT\n EOT2\nEOT3\t\n", true},
		"every marker":             {everyMarker, false},
	} {
		t.Run(name, func(t *testing.T) {
			src := attribute(tc.s)
			if got := strings.Contains(string(src), "<<EOT"); got != tc.heredoc {
				t.Errorf("heredoc = %v, want %v:\n%s", got, tc.heredoc, src)
			}
			if got, err := readBack(src); err != nil || got != tc.s {
				t.Fatalf("%s reads back as %q, %v; want %q", src, got, err, tc.s)
			}
		})
	}
}

func TestQuotedReadsBack(t *testing.T) {
	for _, s := range []string{
		"refs/heads/main",
		"refs/heads/release%{prod}",
		"refs/heads/release%%{prod}",
		"refs/heads/a${b}",
		"refs/heads/a$${b}",
		"refs/heads/a$$${b}",
		"refs/heads/trailing$",
		"refs/heads/trailing%",
		`quote"back\slash`,
		"tab\tnew\nline\rcr\x01\x7f",
		"lines\nof\ntext\n",
		"émoji-🙂",
		"",
	} {
		q := Quoted(s)
		if !strings.HasPrefix(q, `"`) {
			t.Errorf("Quoted(%q) = %s, not a quoted string", s, q)
			continue
		}
		if got, err := readBack([]byte("v = " + q + "\n")); err != nil || got != s {
			t.Errorf("Quoted(%q) = %s reads back as %q, %v", s, q, got, err)
		}
	}
}

// everyMarker holds, indented, every heredoc marker String tries.
var everyMarker = " EOT\n EOT2\n EOT3\n EOT4\n EOT5\n EOT6\n EOT7\n EOT8\n EOT9\n EOT10\n"

func FuzzStringReadsBack(f *testing.F) {
	for _, s := range []string{"a\nb\n", "first\n  EOT\nlast\n", "a ${b}\n%{c}\n", everyMarker, "x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			t.Skip("a cty string is valid UTF-8")
		}
		src := attribute(s)
		// cty holds strings in NFC, so that is the string OpenTofu reads.
		want := cty.StringVal(s).AsString()
		if got, err := readBack(src); err != nil || got != want {
			t.Fatalf("%s reads back as %q, %v; want %q", src, got, err, want)
		}
	})
}

func attribute(s string) []byte {
	f := hclwrite.NewEmptyFile()
	f.Body().SetAttributeRaw("v", String(s))
	return hclwrite.Format(f.Bytes())
}

func readBack(src []byte) (string, error) {
	parsed, diags := hclsyntax.ParseConfig(src, "x.tf", hcl.InitialPos)
	if diags.HasErrors() {
		return "", diags
	}
	attrs, diags := parsed.Body.JustAttributes()
	if diags.HasErrors() {
		return "", diags
	}
	v, diags := attrs["v"].Expr.Value(nil)
	if diags.HasErrors() {
		return "", diags
	}
	return v.AsString(), nil
}
