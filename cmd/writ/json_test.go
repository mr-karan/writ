package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// document is what every --json document shares.
type document struct {
	Format      string `json:"format"`
	OK          *bool  `json:"ok"`
	Status      string `json:"status"`
	Diagnostics []struct {
		Severity string `json:"severity"`
		Class    string `json:"class"`
		Message  string `json:"message"`
	} `json:"diagnostics"`
}

// oneDocument decodes stdout as exactly one JSON document.
func oneDocument(t *testing.T, stdout []byte) document {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(stdout))
	var doc document
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("stdout is not a JSON document: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout holds more than one JSON document:\n%s", stdout)
	}
	if strings.Count(strings.TrimSpace(string(stdout)), "\n") != 0 {
		t.Fatalf("the document is not on one line:\n%s", stdout)
	}
	return doc
}

// TestEveryFailureIsOneJSONDocument runs each command into the ways it can
// stop before it reaches a control plane: a wrong command line, a stack
// directory that is not there, a stack that does not parse, and a command
// that has only a remote form. With --json, stdout must hold exactly one
// document in the command's own format, with ok false, a diagnostic that
// carries a class, an exit code that agrees with it, and no list that is
// null.
func TestEveryFailureIsOneJSONDocument(t *testing.T) {
	root := t.TempDir()
	write := func(name, text string) string {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := filepath.Join(root, "good")
	write("good/main.tf", "resource \"random_pet\" \"p\" {}\n")

	formats := map[string]string{
		"plan": "writ.remote-plan/v0", "apply": "writ.remote-run/v0", "state": "writ.state-adopt/v0",
		"runs": "writ.remote-runs/v0", "audit": "writ.audit/v0", "import-bulk": "writ.import-bulk/v0", "import-accept": "writ.import-accept/v0", "agent": "writ.agent-init/v0",
		"login": "writ.login/v0", "logout": "writ.logout/v0", "": "writ.error/v0",
	}
	type failure struct {
		name string
		// format keys formats; args is the command line after --json.
		format string
		args   []string
		code   int
		class  string
	}
	var tests []failure
	add := func(name, format string, code int, class string, args ...string) {
		tests = append(tests, failure{name: name, format: format, args: args, code: code, class: class})
	}
	for _, c := range []string{"plan", "apply", "state", "runs", "audit"} {
		add(c+": an unknown flag", c, 2, "usage", "--dir", good, c, "--remote", "--no-such-flag")
		add(c+": no --remote", c, 2, "usage", "--dir", good, c)
	}
	add("audit: --stack without --env", "audit", 2, "usage", "--dir", good, "audit", "--remote", "--stack", "network")
	add("audit: a time that is not RFC 3339", "audit", 2, "usage", "--dir", good, "audit", "--remote", "--since", "yesterday")
	add("apply: two plan ids", "apply", 2, "usage", "--dir", good, "apply", "--remote", "a", "b")
	add("plan: an argument it does not take", "plan", 2, "usage", "--dir", good, "plan", "--remote", "extra")
	add("import bulk: a stack with no lock file", "import-bulk", 2, "usage", "--dir", good, "import", "bulk", "--type", "random_id", "--id", "x")
	add("import accept: a plan without --imports", "import-accept", 2, "usage", "--dir", good, "import", "accept", "plan.json")
	add("import accept: a plan with a flag of bulk", "import-accept", 2, "usage", "--dir", good, "import", "accept", "plan.json", "--imports", "imported.imports.json", "--out", "x.tf")
	add("import accept: imports that do not exist", "import-accept", 1, "internal", "--dir", good, "import", "accept", "plan.json", "--imports", filepath.Join(root, "no.imports.json"))
	add("import: no subcommand", "", 2, "usage", "--dir", good, "import")
	add("import: a local import of one object", "", 2, "usage", "--dir", good, "import", "f", "an-id")
	add("agent: no subcommand", "agent", 2, "usage", "--dir", good, "agent")
	add("agent init: an unknown flag", "agent", 2, "usage", "--dir", good, "agent", "init", "--no-such-flag")
	add("agent init: --print with --json", "agent", 2, "usage", "--dir", good, "agent", "init", "--print")
	add("login: an unknown flag", "login", 2, "usage", "--dir", good, "login", "--no-such-flag")
	add("logout: an unknown flag", "logout", 2, "usage", "--dir", good, "logout", "--no-such-flag")
	add("a command that does not exist", "", 2, "usage", "frobnicate")
	add("a global flag that does not exist", "", 2, "usage", "--no-such-flag", "plan")
	add("no command", "", 2, "usage")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --json goes last when a command is named and first when none
			// is, so it is read both by a flag set and from the raw line.
			args := append(append([]string{}, tt.args...), "--json")
			if tt.format == "" && len(tt.args) == 0 {
				args = []string{"--json"}
			}
			var out, errs bytes.Buffer
			code := run(args, &out, &errs)
			doc := oneDocument(t, out.Bytes())
			if doc.Format != formats[tt.format] {
				t.Errorf("format = %q, want %q\n%s", doc.Format, formats[tt.format], &out)
			}
			if doc.OK == nil || *doc.OK {
				t.Errorf("ok is not false:\n%s", &out)
			}
			if len(doc.Diagnostics) == 0 {
				t.Fatalf("no diagnostic says why it failed:\n%s", &out)
			}
			for _, d := range doc.Diagnostics {
				if d.Class == "" || d.Message == "" || d.Severity == "" {
					t.Errorf("a diagnostic lacks a class, a message, or a severity:\n%s", &out)
				}
			}
			if tt.class != "" && doc.Diagnostics[0].Class != tt.class {
				t.Errorf("class = %q, want %q: %s", doc.Diagnostics[0].Class, tt.class, doc.Diagnostics[0].Message)
			}
			if p := nullList(out.Bytes()); p != "" {
				t.Errorf("%s is null; an empty list is []:\n%s", p, &out)
			}
			usage := doc.Diagnostics[0].Class == "usage"
			if code != tt.code || usage != (code == 2) {
				t.Errorf("exit code %d with class %q, want exit %d; exit 2 goes with class usage and no other\n%s", code, doc.Diagnostics[0].Class, tt.code, &errs)
			}
		})
	}
}

// nullable holds the keys whose value is an object that a failure
// document may leave null. Every other null is a list that must be [].
var nullable = map[string]bool{"plan": true, "document": true, "policy": true, "run": true, "session": true}

// nullList returns the path of the first null in doc that nullable does
// not name, or "".
func nullList(doc []byte) string {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return "(not JSON)"
	}
	var walk func(path string, v any) string
	walk = func(path string, v any) string {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if x == nil && !nullable[k] {
					return path + "." + k
				}
				if p := walk(path+"."+k, x); p != "" {
					return p
				}
			}
		case []any:
			for i, x := range v {
				if p := walk(fmt.Sprintf("%s[%d]", path, i), x); p != "" {
					return p
				}
			}
		}
		return ""
	}
	return walk("", v)
}

// TestVersionJSON: version takes --json like every other command.
func TestVersionJSON(t *testing.T) {
	var out, errs bytes.Buffer
	if code := run([]string{"version", "--json"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, &errs)
	}
	doc := oneDocument(t, out.Bytes())
	if doc.Format != "writ.version/v0" || doc.OK == nil || !*doc.OK || !strings.Contains(out.String(), `"version":"`+version+`"`) {
		t.Fatalf("got %s", &out)
	}
}

// TestPanicIsOneJSONDocument: a bug that panics on the command's goroutine
// still leaves one document on stdout, with class internal, and the stack
// on stderr.
func TestPanicIsOneJSONDocument(t *testing.T) {
	var out, errs bytes.Buffer
	inv := &invocation{stdout: &output{w: &out}, stderr: &errs, asJSON: true, command: "plan"}
	code := inv.guarded(func() int { panic("boom") })
	doc := oneDocument(t, out.Bytes())
	if code != 1 || doc.Format != "writ.remote-plan/v0" || len(doc.Diagnostics) != 1 || doc.Diagnostics[0].Class != "internal" ||
		!strings.Contains(doc.Diagnostics[0].Message, "boom") {
		t.Fatalf("exit %d: %s", code, &out)
	}
	if !strings.Contains(errs.String(), "goroutine") {
		t.Errorf("stderr has no stack:\n%s", &errs)
	}
}
