package cli

import (
	"io"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/diag"
)

// printDiags renders diagnostics in this form:
//
//	error[type] certs.writ:22:25
//	  cert_request_pem expects Pem<tls.CertRequest>, got Pem<tls.SelfSignedCert>
//	  fix: replace `ca.cert_pem` with `server_req.cert_request_pem`
//
// A diagnostic with no source location prints only its severity and class
// on the first line.
func printDiags(w io.Writer, l diag.List) {
	o := humanOut(w)
	for _, d := range l {
		head := []seg{severityStyle(d.Severity).of(d.Severity.String() + "[" + string(d.Class) + "]")}
		if !d.Span.IsZero() {
			head = append(head, txt(" "+d.Span.String()))
		}
		o.line(head...)
		for line := range strings.Lines(d.Message) {
			if line = strings.TrimRight(line, "\n"); line == "" {
				o.line()
				continue
			}
			o.line(txt("  " + line))
		}
		if d.Fix != nil && d.Fix.Message != "" {
			o.line(txt("  "), cyan.of("fix:"), txt(" "+d.Fix.Message))
		}
	}
}

func severityStyle(s diag.Severity) style {
	switch s {
	case diag.Error:
		return boldRed
	case diag.Warning:
		return boldYellow
	}
	return bold
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
