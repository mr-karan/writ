package importer

import (
	"testing"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/tfcode"
)

// The three quoting rules write three languages: addr.Quote Writ's address
// syntax, tofuQuote a key of an address as OpenTofu prints it, and
// tfcode.Quoted an HCL literal. The last two share hclwrite's escapes and
// differ only on a string not in NFC.
func TestQuotingRules(t *testing.T) {
	for _, tc := range []struct {
		name, in              string
		addr, tofu, tfcodeOut string
	}{
		{"empty", "", `""`, `""`, `""`},
		{"plain", "web-1", `"web-1"`, `"web-1"`, `"web-1"`},
		{"double quote", `a"b`, `"a\"b"`, `"a\"b"`, `"a\"b"`},
		{"backslash", `a\b`, `"a\\b"`, `"a\\b"`, `"a\\b"`},
		{"interpolation", "${x}", `"\${x}"`, `"$${x}"`, `"$${x}"`},
		{"escaped interpolation", "$${x}", `"$\${x}"`, `"$$${x}"`, `"$$${x}"`},
		{"lone dollar", "$x", `"$x"`, `"$x"`, `"$x"`},
		{"directive", "%{if}", `"%{if}"`, `"%%{if}"`, `"%%{if}"`},
		{"newline", "a\nb", `"a\nb"`, `"a\nb"`, `"a\nb"`},
		{"tab", "a\tb", `"a\tb"`, `"a\tb"`, `"a\tb"`},
		{"carriage return", "a\rb", "\"a\rb\"", `"a\rb"`, `"a\rb"`},
		{"control", "a\x01b", "\"a\x01b\"", `"a\u0001b"`, `"a\u0001b"`},
		{"delete", "a\x7fb", "\"a\x7fb\"", `"a\u007fb"`, `"a\u007fb"`},
		{"non-ASCII", "café 日本", `"café 日本"`, `"café 日本"`, `"café 日本"`},
		{"line separator", "a\u2028b", "\"a\u2028b\"", `"a\u2028b"`, `"a\u2028b"`},
		{"astral format character", "a\U000e0001b", "\"a\U000e0001b\"", `"a\U000e0001b"`, `"a\U000e0001b"`},
		{"decomposed", "cafe\u0301", "\"cafe\u0301\"", "\"cafe\u0301\"", "\"caf\u00e9\""},
		{"invalid UTF-8", "a\xffb", "\"a\xffb\"", "\"a\ufffdb\"", "\"a\ufffdb\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := addr.Quote(tc.in); got != tc.addr {
				t.Errorf("addr.Quote(%q) = %q, want %q", tc.in, got, tc.addr)
			}
			if got := tofuQuote(tc.in); got != tc.tofu {
				t.Errorf("tofuQuote(%q) = %q, want %q", tc.in, got, tc.tofu)
			}
			if got := tfcode.Quoted(tc.in); got != tc.tfcodeOut {
				t.Errorf("tfcode.Quoted(%q) = %q, want %q", tc.in, got, tc.tfcodeOut)
			}
		})
	}
}
