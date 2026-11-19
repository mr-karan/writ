package cli

import (
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/server"
)

// With --json, stdout is one JSON document whatever happens. A command that
// reports its own failure prints its report. This file prints the document
// for every other way a command stops: a wrong command line, a file that
// is not there, a control plane it cannot reach, a bug. It is the command's
// own shape, empty, with ok false and one diagnostic, so a reader of
// writ.remote-run/v0 reads writ.remote-run/v0 on every exit path.

const (
	// errorFormat is for a command line that names no command writ knows,
	// which has no shape of its own.
	errorFormat   = "writ.error/v0"
	versionFormat = "writ.version/v0"
)

// serverDiags is d as the API's documents carry diagnostics.
func serverDiags(d []jsonDiag) []server.Diagnostic {
	out := make([]server.Diagnostic, len(d))
	for i, x := range d {
		out[i] = server.Diagnostic{Severity: x.Severity, Class: x.Class, Message: x.Message}
	}
	return out
}

// errorReport is the document of a command line that names no command.
type errorReport struct {
	Format      string     `json:"format"`
	OK          bool       `json:"ok"`
	Diagnostics []jsonDiag `json:"diagnostics"`
}

type versionReport struct {
	Format  string `json:"format"`
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

// WriteVersion prints `writ version --json`.
func WriteVersion(w io.Writer, version string) error {
	return writeJSON(w, versionReport{Format: versionFormat, OK: true, Version: version})
}

// WriteFailure prints the document of a command that stopped on err before
// it printed one. command is the command's name and args the rest of its
// command line, which tells `import accept` from `import bulk`.
func WriteFailure(w io.Writer, command string, args []string, err error) error {
	return writeJSON(w, failureReport(command, args, diag.List{failureDiag(err)}))
}

// failureDiag classes an error no command reported: usage for a wrong
// command line, and internal for everything else.
func failureDiag(err error) diag.Diagnostic {
	if _, ok := errors.AsType[*UsageError](err); ok {
		return diag.Diagnostic{
			Severity: diag.Error, Class: diag.Usage, Message: err.Error(),
			Fix: &diag.Fix{Message: "run writ help for usage"},
		}
	}
	return diag.Diagnostic{Severity: diag.Error, Class: diag.Internal, Message: err.Error()}
}

// failureReport is the empty report of command with diags in it.
func failureReport(command string, args []string, diags diag.List) any {
	d := toDiags(diags)
	switch command {
	case "plan":
		return server.RemotePlan{Format: server.FormatRemotePlan, Document: json.RawMessage("null"), Diagnostics: serverDiags(d)}
	case "apply":
		return server.RemoteRun{Format: server.FormatRemoteRun, Document: json.RawMessage("null"), Diagnostics: serverDiags(d)}
	case "state":
		return errorReport{Format: server.FormatStateAdopt, Diagnostics: d}
	case "runs":
		return server.RemoteRuns{Format: server.FormatRemoteRuns, Runs: []server.RunInfo{}, Diagnostics: serverDiags(d)}
	case "audit":
		return server.AuditDoc{Format: server.FormatAudit, Rows: []server.AuditRow{}, Diagnostics: serverDiags(d)}
	case "import":
		if AcceptsPlan(slices.DeleteFunc(slices.Clone(args), func(a string) bool { return strings.HasPrefix(a, "-") })) {
			return importAcceptReport{Format: importAcceptFormat, Refusals: []jsonAcceptRefusal{}, Settings: []jsonAcceptSetting{}, Diagnostics: d}
		}
		if ImportsBulk(slices.DeleteFunc(slices.Clone(args), func(a string) bool { return strings.HasPrefix(a, "-") })) {
			return importBulkReport{Format: importBulkFormat, Objects: []jsonBulkObject{}, Params: []jsonDeclaredParam{}, Problems: []jsonImportProblem{}, Diagnostics: d}
		}
	case "agent":
		return agentInitReport{Format: agentInitFormat, Files: []jsonAgentFile{}, Diagnostics: d}
	case "login":
		return loginReport{Format: formatLogin, Diagnostics: d}
	case "logout":
		return logoutReport{Format: formatLogout, Diagnostics: d}
	}
	return errorReport{Format: errorFormat, Diagnostics: d}
}
