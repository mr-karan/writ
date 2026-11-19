package cli

import (
	"encoding/json"
	"io"

	"github.com/mr-karan/writ/internal/diag"
)

// The JSON shapes of the diagnostics every document of the CLI carries,
// documented in docs/json-output.md.

type jsonDiag struct {
	Severity string   `json:"severity"`
	Class    string   `json:"class"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
	Col      int      `json:"col,omitempty"`
	EndLine  int      `json:"end_line,omitempty"`
	EndCol   int      `json:"end_col,omitempty"`
	Message  string   `json:"message"`
	Fix      *jsonFix `json:"fix,omitempty"`
}

// jsonFix is a diagnostic's fix. writ.diag/v0 keeps safe and edits from
// when a fix could edit code; no fix does now, so they are false and [].
type jsonFix struct {
	Message string     `json:"message"`
	Safe    bool       `json:"safe"`
	Edits   []struct{} `json:"edits"`
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func toDiags(l diag.List) []jsonDiag {
	out := make([]jsonDiag, 0, len(l))
	for _, d := range l {
		jd := jsonDiag{
			Severity: d.Severity.String(),
			Class:    string(d.Class),
			File:     d.Span.File,
			Message:  d.Message,
		}
		if !d.Span.IsZero() {
			jd.Line, jd.Col = d.Span.Start.Line, d.Span.Start.Col
			jd.EndLine, jd.EndCol = d.Span.End.Line, d.Span.End.Col
		}
		if d.Fix != nil {
			jd.Fix = &jsonFix{Message: d.Fix.Message, Edits: []struct{}{}}
		}
		out = append(out, jd)
	}
	return out
}
