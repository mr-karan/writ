package policy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/policy"
)

// TestPolicyAllowlist holds the built-ins the security review of P1 ran,
// and the others Writ leaves out: each fails to compile.
func TestPolicyAllowlist(t *testing.T) {
	for _, call := range []string{
		`json.verify_schema({"$ref": "file:///etc/hostname"})`,
		`json.match_schema(input, {"$ref": "file:///etc/hostname"})`,
		`strings.render_template("{{.a}}", {"a": 1})`,
		`bits.lsh(1, 17179869184)`,
		`bits.and(1, 2)`,
		`numbers.range(1, 2)`,
		`numbers.range_step(1, 2, 1)`,
		`graphql.parse_query("{a}")`,
		`providers.aws.sign_req({}, {}, 0)`,
		`rego.parse_module("a.rego", "package a")`,
		`rego.metadata.rule()`,
		`http.send({"method": "get", "url": "http://127.0.0.1/"})`,
		`net.lookup_ip_addr("localhost")`,
		`opa.runtime()`,
		`time.now_ns()`,
		`rand.intn("a", 2)`,
		`uuid.rfc4122("a")`,
		`io.jwt.decode("a")`,
		`crypto.x509.parse_and_verify_certificates("a")`,
		`trace("a")`,
		`walk(input, [p, v])`,
		`yaml.unmarshal("a: 1")`,
		`base64.decode("YQ==")`,
	} {
		name := call[:strings.IndexByte(call, '(')]
		t.Run(name, func(t *testing.T) {
			src := fmt.Sprintf("deny contains \"x\" if {\n\tr := %s\n\tr != 0\n}\n", call)
			_, err := policy.Compile([]policy.Set{rules("s", src)}, policy.Options{})
			if err == nil || !strings.Contains(err.Error(), "undefined function "+name) {
				t.Fatalf("want undefined function %s, got %v", name, err)
			}
		})
	}
}

// TestPolicyBuiltinsDocumented keeps the allowlist and its documentation
// equal.
func TestPolicyBuiltinsDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/policy-reference.md")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(doc), "## Built-ins")
	if start < 0 {
		t.Fatal("docs/policy-reference.md has no section Built-ins")
	}
	section := string(doc)[start:]
	if end := strings.Index(section[3:], "\n## "); end >= 0 {
		section = section[:end+3]
	}
	for _, name := range policy.Allowed {
		if !strings.Contains(section, "`"+name+"`") {
			t.Errorf("the built-ins section does not name %s", name)
		}
	}
}

// bounded compiles sets with a timeout and a memory limit, allowing the
// built-ins Writ leaves out, which the test binary's evaluation process
// allows too.
func bounded(t *testing.T, timeout time.Duration, memory int64, sets ...policy.Set) *policy.Engine {
	t.Helper()
	t.Cleanup(policy.AllowForTest(policy.Hazards...))
	e, err := policy.Compile(sets, policy.Options{Timeout: timeout, Memory: memory})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// timed decides and fails when Decide outlives its timeout by more than
// the time to kill the process.
func timed(t *testing.T, e *policy.Engine, timeout time.Duration) policy.Decision {
	t.Helper()
	start := time.Now()
	d := decide(t, e, planInput(t, "masked-create.json", "prod"))
	if took := time.Since(start); took > timeout+2*time.Second {
		t.Fatalf("Decide took %s, more than its %s timeout", took, timeout)
	}
	return d
}

func wantErrors(t *testing.T, d policy.Decision, n int, want string) {
	t.Helper()
	errs := messages(d, "error")
	if len(errs) != n || d.Outcome != policy.Deny {
		t.Fatalf("want %d errors and a deny, got %s %+v", n, d.Outcome, d.Results)
	}
	for _, e := range errs {
		if !strings.Contains(e, want) {
			t.Fatalf("want errors with %q, got %+v", want, d.Results)
		}
	}
}

// slowRule uses only allowed built-ins: a million-element array, from the
// largest width sprintf pads to, looped over twice.
const slowRule = `xs := split(sprintf("%01000000d", [0]), "")

deny contains "slow" if {
	count([1 | some a in xs; some b in xs; a != b]) > 0
}
`

// bigMemory is the memory of the decision of the largest input.
var bigMemory int64 = policy.DefaultMemory

func TestPolicyIsolation(t *testing.T) {
	const short = 3 * time.Second
	t.Run("a file reference to a FIFO", func(t *testing.T) {
		fifo := filepath.Join(t.TempDir(), "synthetic.json")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		e := bounded(t, short, policy.DefaultMemory, rules("fifo", fmt.Sprintf("deny contains sprintf(\"%%v\", [r]) if r := json.verify_schema({\"$ref\": \"file://%s\"})\n", fifo)))
		wantErrors(t, timed(t, e, short), 1, "did not end within")
	})
	t.Run("render_template past the deadline", func(t *testing.T) {
		tmpl := strings.Repeat("{{range $.a}}", 9) + "x" + strings.Repeat("{{end}}", 9)
		e := bounded(t, short, policy.DefaultMemory, rules("template", fmt.Sprintf("deny contains s if s := strings.render_template(%q, {\"a\": [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]})\n", tmpl)))
		wantErrors(t, timed(t, e, short), 1, "did not end within")
	})
	t.Run("an allocation past the memory limit", func(t *testing.T) {
		e := bounded(t, patient, 256<<20, rules("oom", "deny contains \"big\" if bits.lsh(1, 17179869184) > 0\n"))
		wantErrors(t, timed(t, e, patient), 1, "the evaluation process failed")
	})
	t.Run("a panic", func(t *testing.T) {
		e := bounded(t, patient, policy.DefaultMemory,
			rules("panic", "deny contains \"x\" if bits.lsh(1, 18446744073709551615) > 0\n"),
			rules("after", "deny contains \"after\" if true\n"),
		)
		d := timed(t, e, patient)
		if errs := messages(d, "error"); len(errs) != 1 || !strings.Contains(errs[0], "panicked") || d.Results[0].Set != "panic" {
			t.Fatalf("want one panic error, got %+v", d.Results)
		}
		if got := messages(d, "deny"); len(got) != 1 || got[0] != "after" {
			t.Fatalf("the set after the panic did not run: %+v", d.Results)
		}
	})
	t.Run("one deadline for three sets", func(t *testing.T) {
		e := bounded(t, short, policy.DefaultMemory, rules("a", slowRule), rules("b", slowRule), rules("c", slowRule))
		wantErrors(t, timed(t, e, short), 3, "did not end within the 3s limit")
	})
	t.Run("a kill makes every set an error", func(t *testing.T) {
		e := bounded(t, short, policy.DefaultMemory, rules("fast", "deny contains \"fast\" if true\n"), rules("slow", slowRule))
		d := timed(t, e, short)
		if len(messages(d, "deny")) != 0 || len(messages(d, "error")) != 2 {
			t.Fatalf("got %+v, want two errors", d.Results)
		}
	})
	t.Run("an input of 100,000 changes", func(t *testing.T) {
		var doc struct {
			FormatVersion   string `json:"format_version"`
			ResourceChanges []any  `json:"resource_changes"`
		}
		doc.FormatVersion = "1.2"
		for i := range 100000 {
			doc.ResourceChanges = append(doc.ResourceChanges, map[string]any{"address": fmt.Sprintf("random_pet.p%d", i), "type": "random_pet", "change": map[string]any{"actions": []string{"create"}}})
		}
		b, _ := json.Marshal(doc)
		in, err := policy.NewInput(b, policy.Writ{Decision: policy.PlanPoint, Now: wednesday, Environment: "prod", Stack: "s"})
		if err != nil {
			t.Fatal(err)
		}
		e, err := policy.Compile([]policy.Set{rules("a", "deny contains rc.address if {\n\tsome rc in input.resource_changes\n\trc.address == \"random_pet.p99999\"\n}\n")},
			policy.Options{Timeout: patient, Memory: bigMemory})
		if err != nil {
			t.Fatal(err)
		}
		d := decide(t, e, in)
		if got := messages(d, "deny"); len(got) != 1 || got[0] != "random_pet.p99999" {
			t.Fatalf("got %+v", d.Results)
		}
	})
	t.Run("Local is UTC", func(t *testing.T) {
		t.Setenv("TZ", "Asia/Tokyo")
		e := compile(t, rules("tz", `deny contains sprintf("local hour %d", [h]) if {
	[h, _, _] := time.clock([0, "Local"])
	h != 0
}

deny contains sprintf("local %s", [s]) if {
	s := time.format([0, "Local", "15:04 MST"])
	s != "00:00 UTC"
}

deny contains sprintf("tokyo hour %d", [h]) if [h, _, _] := time.clock([0, "Asia/Tokyo"])
`))
		d := decide(t, e, planInput(t, "masked-create.json", "prod"))
		if got := strings.Join(messages(d, "deny"), ","); got != "tokyo hour 9" || len(messages(d, "error")) != 0 {
			t.Fatalf("got %+v, want only the Tokyo hour", d.Results)
		}
	})
	t.Run("no environment", func(t *testing.T) {
		t.Setenv("WRIT_POLICY_CANARY", "synthetic-env-value")
		e := bounded(t, patient, policy.DefaultMemory, rules("env", "deny contains s if s := strings.render_template(\"{{env \\\"WRIT_POLICY_CANARY\\\"}}\", {})\n"))
		for _, r := range timed(t, e, patient).Results {
			if strings.Contains(r.Message, "synthetic-env-value") {
				t.Fatalf("the evaluation read the parent's environment: %+v", r)
			}
		}
	})
}

// TestPolicyChildDescriptors starts the test binary with a descriptor
// that has no FD_CLOEXEC, as a parent outside Go can leave one. The child
// inherits it, and closes it at its start.
func TestPolicyChildDescriptors(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fd, err := syscall.Dup(int(w.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	for _, c := range []struct{ mode, want string }{{"keep", strconv.Itoa(fd)}, {"close", ""}} {
		out, err := exec.Command(exe, fdsArg, c.mode).Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(out)); !strings.Contains(","+got+",", ","+c.want+",") || (c.want == "" && got != "") {
			t.Fatalf("%s: the child holds inherited descriptors %q, want %q", c.mode, got, c.want)
		}
	}
}

// BenchmarkDecide measures one Decide of the examples on the masked
// create plan, child process included.
func BenchmarkDecide(b *testing.B) {
	files, err := policy.LoadDir("../../examples/policy")
	if err != nil {
		b.Fatal(err)
	}
	e := compile(b, policy.Set{Name: "examples", Files: files})
	in := planInput(b, "masked-create.json", "prod")
	for b.Loop() {
		if d := e.Decide(context.Background(), in); d.Outcome != policy.Warn {
			b.Fatalf("got %+v", d)
		}
	}
}
