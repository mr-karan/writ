package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/mr-karan/writ/internal/diag"
)

func span(file string, line, col, off int) diag.Span {
	return diag.Span{File: file, Start: diag.Pos{Line: line, Col: col, Offset: off}, End: diag.Pos{Line: line, Col: col + 10, Offset: off + 10}}
}

func TestPrintDiags(t *testing.T) {
	tests := []struct {
		name string
		in   diag.List
		want string
	}{
		{
			name: "spec example with a fix",
			in: diag.List{{
				Severity: diag.Error, Class: diag.Syntax, Span: span("serve.hcl", 22, 25, 400),
				Message: "provider_uids must be a range such as \"61000-61063\"",
				Fix:     &diag.Fix{Message: "write provider_uids = \"61000-61063\""},
			}},
			want: "error[syntax] serve.hcl:22:25\n" +
				"  provider_uids must be a range such as \"61000-61063\"\n" +
				"  fix: write provider_uids = \"61000-61063\"\n",
		},
		{
			name: "no location and no fix",
			in:   diag.List{{Severity: diag.Warning, Class: diag.State, Message: "ca is being applied by run R"}},
			want: "warning[state]\n  ca is being applied by run R\n",
		},
		{
			name: "two diagnostics in order",
			in: diag.List{
				{Severity: diag.Error, Class: diag.Usage, Span: span("main.tf", 1, 5, 4), Message: "unknown name x"},
				{Severity: diag.Error, Class: diag.Provider, Span: span("main.tf", 8, 14, 90), Message: "the provider could not read the object"},
			},
			want: "error[usage] main.tf:1:5\n  unknown name x\nerror[provider] main.tf:8:14\n  the provider could not read the object\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			printDiags(&b, tt.in)
			if b.String() != tt.want {
				t.Errorf("got\n%s\nwant\n%s", b.String(), tt.want)
			}
		})
	}
}

func TestDiagJSONKeepsFixShape(t *testing.T) {
	d := diag.List{{
		Severity: diag.Error, Class: diag.Usage, Span: span("main.tf", 28, 21, 612), Message: "m",
		Fix: &diag.Fix{Message: "run writ help for usage"},
	}}
	var b bytes.Buffer
	if err := writeJSON(&b, toDiags(d)); err != nil {
		t.Fatal(err)
	}
	want := `[{"severity":"error","class":"usage","file":"main.tf","line":28,"col":21,"end_line":28,"end_col":31,"message":"m","fix":{"message":"run writ help for usage","safe":false,"edits":[]}}]` + "\n"
	if b.String() != want {
		t.Errorf("got  %s\nwant %s", b.String(), want)
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, 0},
		{"reported failure", ErrFailed, 1},
		{"operational error", errors.New("open state: permission denied"), 1},
		{"usage", usagef("--allow: unknown risk %q", "x"), 2},
		{"wrapped usage", fmt.Errorf("plan: %w", &UsageError{Msg: "bad"}), 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExitCode(tt.err); got != tt.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestHumanEscapesTerminalControls(t *testing.T) {
	attack := "safe\x1b[2J\r\b\x00\x7f\u009b\u202eend"
	var b bytes.Buffer
	printDiags(&b, diag.List{{Severity: diag.Error, Class: diag.Provider, Span: span(attack, 1, 1, 0), Message: attack, Fix: &diag.Fix{Message: attack}}})
	got := b.String()
	for _, r := range got {
		if r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			t.Errorf("unsafe rune %U in human output", r)
		}
	}
	if !strings.Contains(got, `\u202e`) {
		t.Errorf("missing escaped bidi override: %q", got)
	}
}

func TestHumanErrorsPreserveIdentity(t *testing.T) {
	original := fmt.Errorf("provider says %s: %w", "\x1b[2J\u202e", ErrFailed)
	err := original
	escapeError(&err, false)
	if strings.ContainsRune(err.Error(), '\x1b') || strings.ContainsRune(err.Error(), '\u202e') {
		t.Fatalf("unsafe error: %q", err)
	}
	if !errors.Is(err, ErrFailed) {
		t.Fatal("lost error identity")
	}
	err = original
	escapeError(&err, true)
	if err != original {
		t.Fatal("JSON error changed")
	}
}
