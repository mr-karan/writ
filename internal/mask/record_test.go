//go:build maskrecord

package mask

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

// TestRecordFixtures runs testdata/stack through OpenTofu and writes the raw
// output that the other tests mask: two plans, the plan and apply event
// streams, and the state after the first apply. It reads providers from a
// filesystem mirror, so after the providers are cached it runs with no
// network: `unshare -rn go test -tags maskrecord -run TestRecordFixtures ./internal/mask`.
func TestRecordFixtures(t *testing.T) {
	run := recorder(t, "stack", "main.tf", "first.tfvars", "second.tfvars")
	run("init", "-input=false")
	write(t, "plan-events.jsonl", run("plan", "-input=false", "-lock=false", "-json", "-var-file=first.tfvars", "-out=first.plan"))
	write(t, "plan.json", run("show", "-json", "first.plan"))
	write(t, "apply-events.jsonl", run("apply", "-input=false", "-json", "first.plan"))
	write(t, "state.json", run("show", "-json"))
	run("plan", "-input=false", "-lock=false", "-var-file=second.tfvars", "-out=second.plan")
	write(t, "update-plan.json", run("show", "-json", "second.plan"))
}

// TestRecordAddressFixture plans testdata/address, whose addresses carry
// secrets in their index keys and keys that need HCL's escapes. It uses
// only the built-in terraform_data, so it needs no provider.
func TestRecordAddressFixture(t *testing.T) {
	run := recorder(t, "address", "main.tf", "child/main.tf", "keys.tfvars")
	run("init", "-input=false")
	run("plan", "-input=false", "-lock=false", "-var-file=keys.tfvars", "-out=keys.plan")
	write(t, "address-plan.json", run("show", "-json", "keys.plan"))
}

// recorder copies the files of testdata/<stack> into a temp dir and returns
// a function that runs tofu there, with providers from a filesystem mirror.
func recorder(t *testing.T, stack string, files ...string) func(args ...string) []byte {
	t.Helper()
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
	dir := t.TempDir()
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join("testdata", stack, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rc := filepath.Join(dir, "tofurc")
	cfg := "provider_installation {\n  filesystem_mirror {\n    path = \"" + mirror + "\"\n  }\n}\n"
	if err := os.WriteFile(rc, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TF_CLI_CONFIG_FILE=" + rc, "TF_IN_AUTOMATION=1", "CHECKPOINT_DISABLE=1"}
	return func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, tofu, args...)
		cmd.Dir, cmd.Env = dir, env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("tofu %s: %v\n%s\n%s", strings.Join(args, " "), err, stdout.Bytes(), stderr.Bytes())
		}
		return stdout.Bytes()
	}
}

func write(t *testing.T, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join("testdata", name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}
