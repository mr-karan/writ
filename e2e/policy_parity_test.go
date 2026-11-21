//go:build e2e && e2e_tofu && linux

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPolicyParity is the policy parity test of docs/write-a-policy.md: one .rego file gives the same
// verdict in writ serve and under conftest. writ serve --dev judges each
// stack's plan with examples/policy, and conftest judges the output of
// `tofu show -json` of a plan of the same files, made with tofu alone,
// with that same directory. The verdicts and the messages must be equal.
// It runs in namespaces with only loopback, as TestTofuPlanByCommit does.
const conftestEnv = "WRIT_E2E_CONFTEST"

// parityStacks are the stacks the test judges, by name, with the verdict
// examples/policy gives each.
var parityStacks = map[string]struct {
	body, verdict string
}{
	"clean": {`resource "random_pet" "name" {}

resource "tls_private_key" "key" {
  algorithm = "ED25519"
}

resource "local_file" "note" {
  filename        = "${path.module}/note.txt"
  content         = "a note"
  file_permission = "0600"
}
`, "pass"},
	"open": {`resource "local_file" "note" {
  filename = "${path.module}/note.txt"
  content  = "a note"
}

resource "local_sensitive_file" "secret" {
  filename = "${path.module}/secret.txt"
  content  = var.canary
}
`, "warn"},
	"weak": {`resource "tls_private_key" "key" {
  algorithm = "RSA"
  rsa_bits  = 2048
}

resource "local_file" "key" {
  filename = "${path.module}/key.pub"
  content  = tls_private_key.key.public_key_openssh
}
`, "deny"},
}

func TestPolicyParity(t *testing.T) {
	if os.Getenv(tofuChildEnv) != "" {
		parityChild(t)
		return
	}
	conftest, err := exec.LookPath("conftest")
	if err != nil {
		t.Fatalf("this test needs conftest on PATH, as just policy-parity gives: %v", err)
	}
	inTofuNamespace(t, "TestPolicyParity", tofuNamespace, conftestEnv+"="+realPath(t, conftest))
}

// verdict is a judgment of one plan: pass, warn, or deny, and its
// messages, sorted.
type verdict struct {
	outcome  string
	messages []string
}

func (v verdict) String() string {
	return v.outcome + " " + strings.Join(v.messages, " | ")
}

func parityChild(t *testing.T) {
	examples := policyExamples(t)
	conftest := os.Getenv(conftestEnv)
	lock := tofuLock(t)
	// conftest is under /nix, which the root directory holds.
	bin, shared := tofuChildRoot(t, examples)

	home := t.TempDir()
	cache := filepath.Join(home, "plugins")
	if err := os.CopyFS(cache, os.DirFS(shared)); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "dev.hcl")
	if err := os.WriteFile(config, fmt.Appendf(nil, "tofu {\n  binary = %q\n  plugin_cache = %q\n  scratch_dir = %q\n}\n\npolicy {\n  set \"examples\" {\n    dir = %q\n  }\n}\n",
		bin, cache, filepath.Join(home, "runs"), examples), 0o600); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(home, "infra")
	head := "terraform {\n  backend \"http\" {}\n" + tofuProviders + "}\n\nvariable \"canary\" {\n  type      = string\n  sensitive = true\n}\n\n"
	for name, s := range parityStacks {
		dir := filepath.Join(repoDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(head+s.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".terraform.lock.hcl"), lock, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, repoDir, "init", "-q", "-b", "main")
	gitIn(t, repoDir, "add", ".")
	gitIn(t, repoDir, "commit", "-q", "-m", "three stacks")

	env := []string{"XDG_DATA_HOME=" + home}
	srv := startDevAt(t, home, []string{"TF_VAR_canary=" + tofuCanary}, "--config", config)
	(&stackDir{t: t, dir: repoDir, env: env}).run(0, "repo", "add", ".")

	var served []string
	for _, name := range slices.Sorted(maps.Keys(parityStacks)) {
		want := parityStacks[name].verdict
		t.Run(name, func(t *testing.T) {
			code := 0
			if want == "deny" {
				code = 1
			}
			var p tofuPlanDoc
			r := (&stackDir{t: t, dir: filepath.Join(repoDir, name), env: env}).runJSON(code, &p, "plan", "--remote", "--json")
			served = append(served, r.stdout, r.stderr)
			if p.Policy == nil {
				t.Fatalf("writ serve judged no policy:\n%s", p.Text)
			}
			writ := verdict{outcome: p.Policy.Outcome}
			for _, res := range p.Policy.Results {
				if res.Rule == "error" {
					t.Fatalf("the policy failed in writ serve: %s", res.Message)
				}
				writ.messages = append(writ.messages, res.Message)
			}
			slices.Sort(writ.messages)
			local := conftestVerdict(t, bin, cache, conftest, examples, head+parityStacks[name].body, lock)
			t.Logf("writ serve: %s\nconftest:   %s", writ, local)
			if writ.outcome != want || writ.String() != local.String() {
				t.Fatalf("writ serve judged %s, conftest %s; want both %s", writ, local, want)
			}
		})
	}
	srv.stop(t)
	scanForCanary(t, filepath.Join(home, "writ", "dev"), served, srv.out.String())
}

// conftestVerdict plans main.tf with tofu alone, on the local backend,
// and judges `tofu show -json` of the plan with conftest and policy.
func conftestVerdict(t *testing.T, bin, cache, conftest, policy, main string, lock []byte) verdict {
	t.Helper()
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.tf", strings.Replace(main, "  backend \"http\" {}\n", "", 1))
	write(".terraform.lock.hcl", string(lock))
	write("tofurc", fmt.Sprintf("provider_installation {\n  filesystem_mirror {\n    path = %q\n  }\n}\n", cache))
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TF_CLI_CONFIG_FILE=" + filepath.Join(dir, "tofurc"), "TF_IN_AUTOMATION=1",
		"CHECKPOINT_DISABLE=1", "TF_VAR_canary=" + tofuCanary}
	run := func(name string, args ...string) ([]byte, int) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir, cmd.Env = dir, env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := cmd.ProcessState.ExitCode()
		if err != nil && code <= 0 {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, stderr.Bytes())
		}
		if code != 0 && name == bin {
			t.Fatalf("tofu %s: exit %d\n%s\n%s", strings.Join(args, " "), code, stdout.Bytes(), stderr.Bytes())
		}
		return stdout.Bytes(), code
	}
	run(bin, "init", "-input=false", "-lockfile=readonly", "-no-color")
	run(bin, "plan", "-input=false", "-out=plan.bin", "-no-color")
	shown, _ := run(bin, "show", "-json", "plan.bin")
	write("plan.json", string(shown))
	out, code := run(conftest, "test", "--no-color", "--output", "json", "--policy", policy, "plan.json")
	var results []struct {
		Failures []struct {
			Msg string `json:"msg"`
		} `json:"failures"`
		Warnings []struct {
			Msg string `json:"msg"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal(out, &results); err != nil || len(results) != 1 {
		t.Fatalf("conftest's output, exit %d: %v\n%s", code, err, out)
	}
	v := verdict{outcome: "pass"}
	for _, w := range results[0].Warnings {
		v.outcome = "warn"
		v.messages = append(v.messages, w.Msg)
	}
	for _, f := range results[0].Failures {
		v.outcome = "deny"
		v.messages = append(v.messages, f.Msg)
	}
	if (code == 1) != (v.outcome == "deny") {
		t.Fatalf("conftest exited %d with the verdict %s", code, v)
	}
	slices.Sort(v.messages)
	return v
}
