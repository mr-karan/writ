//go:build uirecord

package ui_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRecordTofuFixtures runs testdata/tofu/stack through OpenTofu and
// writes the raw output that the page tests give writ serve as tofu's:
//
//   - second-plan.json: a plan that forgets, imports, replaces with
//     replace_paths, creates in a module, changes outputs, and finds drift.
//   - second-apply.jsonl: the `apply -json` events of that plan.
//   - third-plan.json and third-apply.jsonl: a plan whose apply fails.
//
// It reads providers from a filesystem mirror, so after the providers are
// cached it runs with no network:
// `unshare -rn go test -tags uirecord -run TestRecordTofuFixtures ./internal/ui`.
func TestRecordTofuFixtures(t *testing.T) {
	tofu, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatal("tofu is not on PATH")
	}
	mirror := os.Getenv("TF_PLUGIN_CACHE_DIR")
	if mirror == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		mirror = filepath.Join(base, "writ-e2e", "tofu-plugins")
	}
	src := filepath.Join("testdata", "tofu", "stack")
	dir := t.TempDir()
	copyFile := func(from, to string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(src, from))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, to)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, to), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"providers.tf", "first.tfvars", "second.tfvars", "inner/main.tf"} {
		copyFile(f, f)
	}
	rc := filepath.Join(dir, "tofurc")
	cfg := "provider_installation {\n  filesystem_mirror {\n    path = \"" + mirror + "\"\n  }\n}\n"
	if err := os.WriteFile(rc, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TF_CLI_CONFIG_FILE=" + rc, "TF_IN_AUTOMATION=1", "CHECKPOINT_DISABLE=1"}
	run := func(fail bool, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, tofu, args...)
		cmd.Dir, cmd.Env = dir, env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); (err != nil) != fail {
			t.Fatalf("tofu %s: %v\n%s\n%s", strings.Join(args, " "), err, stdout.Bytes(), stderr.Bytes())
		}
		return stdout.Bytes()
	}
	write := func(name string, b []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join("testdata", "tofu", name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stage := func(name string) {
		t.Helper()
		copyFile(name, "main.tf")
		run(false, "init", "-input=false")
	}

	stage("first.tf")
	run(false, "apply", "-input=false", "-auto-approve", "-var-file=first.tfvars")

	stage("second.tf")
	if err := os.Remove(filepath.Join(dir, "note.txt")); err != nil {
		t.Fatal(err)
	}
	run(false, "plan", "-input=false", "-lock=false", "-var-file=second.tfvars", "-out=second.plan")
	write("second-plan.json", run(false, "show", "-json", "second.plan"))
	write("second-apply.jsonl", run(false, "apply", "-input=false", "-json", "second.plan"))

	stage("third.tf")
	run(false, "plan", "-input=false", "-lock=false", "-var-file=second.tfvars", "-out=third.plan")
	write("third-plan.json", run(false, "show", "-json", "third.plan"))
	write("third-apply.jsonl", run(true, "apply", "-input=false", "-json", "third.plan"))
}
