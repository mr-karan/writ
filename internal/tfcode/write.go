// Package tfcode reads and writes the OpenTofu code of one stack for writ
// import: it reads the providers a stack requires and configures, and the
// names it declares, and it writes literals as HCL tokens. It is the one
// package besides internal/provider that holds cty values, which stay
// inside it.
package tfcode

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/mr-karan/writ/internal/value"
)

// String is s as an HCL string that OpenTofu reads back exactly: a heredoc
// when s is lines of printable text that end in a line break and one of
// the markers EOT, EOT2 to EOT10 is free, else a quoted string. Every
// template sequence is escaped.
func String(s string) hclwrite.Tokens {
	marker, ok := heredocMarker(s)
	if !ok {
		return hclwrite.TokensForValue(cty.StringVal(s))
	}
	body := strings.NewReplacer("${", "$${", "%{", "%%{").Replace(s)
	return hclwrite.Tokens{
		{Type: hclsyntax.TokenOHeredoc, Bytes: []byte("<<" + marker + "\n")},
		{Type: hclsyntax.TokenStringLit, Bytes: []byte(body)},
		{Type: hclsyntax.TokenCHeredoc, Bytes: []byte(marker)},
	}
}

// Quoted is s as a quoted HCL string, never a heredoc, with every template
// sequence and every character that is not printable escaped. OpenTofu
// reads back s exactly when s is valid UTF-8 in normal form C.
func Quoted(s string) string {
	return string(hclwrite.TokensForValue(cty.StringVal(s)).Bytes())
}

// InNFC reports whether s is in Unicode normal form C. OpenTofu reads each
// string of its code into cty, which keeps strings in NFC, so a string in
// another form reaches the provider changed.
func InNFC(s string) bool { return cty.StringVal(s).AsString() == s }

// heredocMarker is a marker that closes no line of s early. HCL ends a
// heredoc at any line that is the marker once its white space is trimmed.
func heredocMarker(s string) (string, bool) {
	if !heredoc(s) {
		return "", false
	}
	lines := map[string]bool{}
	for line := range strings.Lines(s) {
		lines[strings.TrimSpace(line)] = true
	}
	for i := 1; i <= 10; i++ {
		marker := "EOT"
		if i > 1 {
			marker += strconv.Itoa(i)
		}
		if !lines[marker] {
			return marker, true
		}
	}
	return "", false
}

// heredoc reports whether a heredoc holds s exactly: two or more lines,
// each of printable characters or tabs, the last ended by a line break.
func heredoc(s string) bool {
	if !strings.HasSuffix(s, "\n") || strings.Count(s, "\n") < 2 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r != '\n' && r != '\t' && !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// Scalar is a string, number, or bool as an HCL literal. It panics on any
// other kind.
func Scalar(v value.Value) hclwrite.Tokens {
	switch v.Kind() {
	case value.String:
		return String(v.AsString())
	case value.Int:
		return number(strconv.FormatInt(v.AsInt(), 10))
	case value.Float:
		return number(strconv.FormatFloat(v.AsFloat(), 'f', -1, 64))
	case value.Bool:
		return hclwrite.TokensForIdentifier(strconv.FormatBool(v.AsBool()))
	}
	panic("tfcode: not a scalar: " + v.Kind().String())
}

func number(text string) hclwrite.Tokens {
	return hclwrite.Tokens{{Type: hclsyntax.TokenNumberLit, Bytes: []byte(text)}}
}
