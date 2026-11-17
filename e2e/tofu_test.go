//go:build e2e && e2e_tofu && linux

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/mr-karan/writ/internal/control/controltest"
)

// TestTofuPlanByCommit is the plan steps of the end-to-end test: writ serve
// --dev on loopback with SQLite, and the policy of examples/policy, plans
// two OpenTofu stacks of a local Git repository by commit, one of which
// calls a local module, with real tofu and hashicorp/random, local, and
// tls, and serves the masked plans. A commit the policy refuses is
// refused, with the policy's message. Then neither the control database
// nor the data directory holds the canary that both stacks take through a
// sensitive variable.
//
// The parent fills a plugin cache, which needs the registry, and runs
// this test again in user, network, mount, and pid namespaces, where only
// loopback exists. There the child makes a tmpfs its root directory, as
// internal/tofu's gate does, because tofu's scratch root must sit under
// directories that root or writ owns, and in a user namespace every host
// directory, / among them, belongs to the overflow uid.
const (
	tofuChildEnv = "WRIT_E2E_TOFU_CHILD"
	tofuCacheEnv = "WRIT_E2E_TOFU_CACHE"
	tofuBinEnv   = "WRIT_E2E_TOFU_BIN"
	tofuGitEnv   = "WRIT_E2E_TOFU_GIT"
	tofuRootEnv  = "WRIT_E2E_TOFU_ROOT"
)

// tofuKey is the value of the sensitive variable TestTofuApproveApply
// keys a resource by, and tofuKeyRotated the value it changes to.
const (
	tofuKey        = "e2e-key/7d41+aa90-old"
	tofuKeyRotated = "e2e-key/0b5c+e2f3-new"
)

// tofuCanary is the value of the stacks' sensitive variable. Its slash and
// plus sign make its URL-encoded forms differ from its plain one.
const tofuCanary = "e2e-canary/9f2c+7a1d-tofu"

// tofuHostMarker names the resource of a module on the server's host,
// which no plan may read.
const tofuHostMarker = "host_module_marker_5d3a"

var tofuNamespace = []string{"--user", "--map-root-user", "--net", "--mount", "--pid", "--fork", "--mount-proc"}

const tofuProviders = `  required_providers {
    random = { source = "hashicorp/random", version = "3.9.1" }
    local  = { source = "hashicorp/local", version = "2.9.1" }
    tls    = { source = "hashicorp/tls", version = "4.4.2" }
  }
`

// tofuLock pins the providers at the versions internal/tofu's gate runs.
// It is that gate's lock file, whose hashes hold runs of 12 digits that
// TestE2EHoldsNoAccountID would take for account ids in this directory.
func tofuLock(t *testing.T) []byte {
	t.Helper()
	lock, err := os.ReadFile("../internal/tofu/testdata/lock.hcl")
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

// tofuRepo is the repository's files: stacks app and net on the http
// backend, the module app calls, and stack bad, whose variable's
// validation fails with the canary in its error message, through
// nonsensitive(). The mask sweeps the canary itself, a TF_VAR_ value, but
// not its upper case, which only a plan would teach it.
func tofuRepo(lock []byte) map[string]string {
	return map[string]string{
		"app/.terraform.lock.hcl": string(lock),
		"app/main.tf": "terraform {\n  backend \"http\" {}\n" + tofuProviders + `}

variable "canary" {
  type      = string
  sensitive = true
}

resource "random_pet" "name" {
  keepers = { canary = var.canary }
}

resource "local_sensitive_file" "secret" {
  filename = "${path.module}/secret.txt"
  content  = base64encode(var.canary)
}

module "cert" {
  source = "../modules/cert"
}

output "pet" {
  value = random_pet.name.id
}
`,
		"modules/cert/main.tf": `terraform {
  required_providers {
    tls = { source = "hashicorp/tls", version = "4.4.2" }
  }
}

resource "tls_private_key" "key" {
  algorithm = "ED25519"
}
`,
		"net/.terraform.lock.hcl": string(lock),
		"net/main.tf": "terraform {\n  backend \"http\" {}\n" + tofuProviders + `}

variable "canary" {
  type      = string
  sensitive = true
}

resource "random_id" "id" {
  byte_length = 4
  keepers     = { canary = sha256(var.canary) }
}

resource "tls_private_key" "key" {
  algorithm = "ED25519"
}

resource "local_file" "note" {
  filename = "${path.module}/note.txt"
  content  = "id ${random_id.id.hex}"
}
`,
		"bad/.terraform.lock.hcl": string(lock),
		"bad/main.tf": "terraform {\n  backend \"http\" {}\n" + tofuProviders + `}

variable "canary" {
  type      = string
  sensitive = true

  validation {
    condition     = nonsensitive(var.canary) == "not the canary"
    error_message = "The canary ${nonsensitive(var.canary)}, ${nonsensitive(upper(var.canary))} in upper case, is not allowed."
  }
}

resource "random_pet" "name" {
  keepers = { canary = var.canary }
}
`,
		"README.md": "Three OpenTofu stacks.\n",
	}
}

func TestTofuPlanByCommit(t *testing.T) {
	if os.Getenv(tofuChildEnv) != "" {
		tofuChild(t)
		return
	}
	inTofuNamespace(t, "TestTofuPlanByCommit", tofuNamespace)
}

// inTofuNamespace fills the plugin cache and runs test again in the
// namespaces ns makes, with env added to its environment.
func inTofuNamespace(t *testing.T, test string, ns []string, env ...string) {
	t.Helper()
	bin, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatalf("this test needs OpenTofu 1.13.9 on PATH, as just e2e gives: %v", err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	cache := os.Getenv("TF_PLUGIN_CACHE_DIR")
	if cache == "" {
		cache = filepath.Join(base, "writ-e2e", "tofu-plugins")
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	fillTofuCache(t, bin, cache)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", append(slices.Clone(ns), self, "-test.run=^"+test+"$", "-test.v", "-test.count=1", "-test.timeout=9m")...)
	cmd.Env = append(os.Environ(), tofuChildEnv+"=1", childWritEnv+"="+writBin, tofuCacheEnv+"="+cache, tofuBinEnv+"="+realPath(t, bin),
		tofuGitEnv+"="+realPath(t, git), tofuRootEnv+"="+t.TempDir())
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	t.Logf("in the namespaces:\n%s", out)
	if err != nil || !strings.Contains(string(out), "--- PASS: "+test) {
		t.Fatalf("the test failed in the namespaces: %v", err)
	}
}

// fillTofuCache installs the stacks' providers into cache with tofu init,
// once: a later run finds them there and reaches no registry.
func fillTofuCache(t *testing.T, bin, cache string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte("terraform {\n"+tofuProviders+"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".terraform.lock.hcl"), tofuLock(t), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "init", "-input=false", "-backend=false", "-lockfile=readonly", "-no-color")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TF_PLUGIN_CACHE_DIR="+cache, "TF_CLI_CONFIG_FILE="+os.DevNull, "CHECKPOINT_DISABLE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fill the plugin cache: %v\n%s", err, out)
	}
}

// realPath is a binary's path with every symbolic link resolved, which the
// child's root directory still holds.
func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// tofuPlanDoc is the part of writ.remote-plan/v0 this test reads.
type tofuPlanDoc struct {
	OK   bool `json:"ok"`
	Plan struct {
		Ref       string   `json:"ref"`
		ID        string   `json:"id"`
		Status    string   `json:"status"`
		Risks     []string `json:"risks"`
		Approvals struct {
			Need int `json:"need"`
		} `json:"approvals"`
		Source struct {
			Kind   string `json:"kind"`
			Commit string `json:"commit"`
		} `json:"source"`
		Stacks []struct {
			Stack   string `json:"stack"`
			Changes int    `json:"changes"`
		} `json:"stacks"`
	} `json:"plan"`
	Document struct {
		FormatVersion    string   `json:"format_version"`
		TerraformVersion string   `json:"terraform_version"`
		Variables        []string `json:"variables"`
		ResourceChanges  []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string       `json:"actions"`
				After   map[string]any `json:"after"`
			} `json:"change"`
		} `json:"resource_changes"`
		Configuration struct {
			Modules []struct {
				Address string `json:"address"`
				Source  string `json:"source"`
			} `json:"modules"`
		} `json:"configuration"`
	} `json:"document"`
	Text   string `json:"text"`
	Policy *struct {
		Point   string `json:"point"`
		Outcome string `json:"outcome"`
		Results []struct {
			Set, Rule, Outcome, Message string
		} `json:"results"`
	} `json:"policy"`
}

// policyExamples is the absolute path of examples/policy, which a child
// binds into its root directory.
func policyExamples(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../examples/policy")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func tofuChild(t *testing.T) {
	lock := tofuLock(t)
	files := tofuRepo(lock)
	examples := policyExamples(t)
	bin, shared := tofuChildRoot(t, examples)

	home := t.TempDir()
	cache := filepath.Join(home, "plugins")
	if err := os.CopyFS(cache, os.DirFS(shared)); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "dev.hcl")
	if err := os.WriteFile(config, fmt.Appendf(nil, "tofu {\n  binary = %q\n  plugin_cache = %q\n  scratch_dir = %q\n}\n\npolicy {\n  set \"examples\" {\n    dir = %q\n  }\n}\n", bin, cache, filepath.Join(home, "runs"), examples), 0o600); err != nil {
		t.Fatal(err)
	}
	// Stack host calls a Git repository on the server's host, outside the
	// repository, by a source that tofu init would clone.
	hostDir := filepath.Join(home, "host-module")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "main.tf"), []byte("resource \"random_pet\" \""+tofuHostMarker+"\" {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, hostDir, "init", "-q", "-b", "main")
	gitIn(t, hostDir, "add", ".")
	gitIn(t, hostDir, "commit", "-q", "-m", "a module on the host")
	hostSource := "git::file://" + hostDir
	files["host/.terraform.lock.hcl"] = string(lock)
	files["host/main.tf"] = "terraform {\n  backend \"http\" {}\n" + tofuProviders + fmt.Sprintf("}\n\nmodule \"x\" {\n  source = %q\n}\n", hostSource)
	// Stack checksum calls a remote module whose checksum parameter,
	// percent-encoded, names a file on the server's host: go-getter would
	// read that file before any network access.
	sums := filepath.Join(home, "host-sums")
	if err := os.WriteFile(sums, []byte(tofuHostMarker+"  module.zip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sumSource := "https://example.invalid/module.zip?checksum=" + strings.NewReplacer("f", "%66", ":", "%3A", "/", "%2F").Replace("file:file://"+sums)
	files["checksum/.terraform.lock.hcl"] = string(lock)
	files["checksum/main.tf"] = "terraform {\n  backend \"http\" {}\n" + tofuProviders + fmt.Sprintf("}\n\nmodule \"x\" {\n  source = %q\n}\n", sumSource)
	// Stack hostfile reads a file of the server's host, which every user
	// may read, by its absolute path, as tofu plans it: the run's view of
	// the filesystem holds no such file.
	hostFile := filepath.Join(home, "host-file")
	if err := os.WriteFile(hostFile, []byte(tofuHostMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files["hostfile/.terraform.lock.hcl"] = string(lock)
	files["hostfile/main.tf"] = "terraform {\n  backend \"http\" {}\n" + tofuProviders + fmt.Sprintf(`}

data "local_file" "host" {
  filename = %[1]q
}

output "data" {
  value = data.local_file.host.content
}

output "file" {
  value = file(%[1]q)
}
`, hostFile)
	repoDir := filepath.Join(home, "infra")
	for name, text := range files {
		p := filepath.Join(repoDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, repoDir, "init", "-q", "-b", "main")
	gitIn(t, repoDir, "add", ".")
	gitIn(t, repoDir, "commit", "-q", "-m", "three stacks")
	head := gitIn(t, repoDir, "rev-parse", "HEAD")

	env := []string{"XDG_DATA_HOME=" + home}
	srv := startDevAt(t, home, []string{"TF_VAR_canary=" + tofuCanary}, "--config", config)
	repo := &stackDir{t: t, dir: repoDir, env: env}
	added := repo.run(0, "repo", "add", ".")
	for _, want := range []string{"app", "bad", "net"} {
		if !strings.Contains(added.stdout, want) {
			t.Fatalf("writ repo add did not find stack %s:\n%s", want, added)
		}
	}

	want := map[string][]string{
		"app": {"local_sensitive_file.secret", "module.cert.tls_private_key.key", "random_pet.name"},
		"net": {"local_file.note", "random_id.id", "tls_private_key.key"},
	}
	var served []string
	for _, stack := range []string{"app", "net"} {
		s := &stackDir{t: t, dir: filepath.Join(repoDir, stack), env: env}
		var p tofuPlanDoc
		r := s.runJSON(0, &p, "plan", "--remote", "--json")
		served = append(served, r.stdout)
		if !p.OK || p.Plan.Status != "approved" || p.Plan.ID == "" || p.Plan.Source.Kind != "commit" || p.Plan.Source.Commit != head {
			t.Fatalf("plan of %s: %+v\n%s", stack, p.Plan, p.Text)
		}
		if len(p.Plan.Stacks) != 1 || p.Plan.Stacks[0].Stack != stack || p.Plan.Stacks[0].Changes != 3 {
			t.Fatalf("plan of %s names stacks %+v, want %s with 3 changes", stack, p.Plan.Stacks, stack)
		}
		d := p.Document
		if !strings.HasPrefix(d.FormatVersion, "1.") || d.TerraformVersion != "1.13.9" || !slices.Equal(d.Variables, []string{"canary"}) {
			t.Fatalf("plan of %s: format %q, OpenTofu %q, variables %v", stack, d.FormatVersion, d.TerraformVersion, d.Variables)
		}
		var addrs []string
		for _, c := range d.ResourceChanges {
			addrs = append(addrs, c.Address)
			if !slices.Equal(c.Change.Actions, []string{"create"}) {
				t.Errorf("plan of %s: %s has actions %v, want create", stack, c.Address, c.Change.Actions)
			}
		}
		slices.Sort(addrs)
		if !slices.Equal(addrs, want[stack]) {
			t.Fatalf("plan of %s changes %v, want %v", stack, addrs, want[stack])
		}
		for _, line := range append([]string{"OpenTofu 1.13.9 plan of " + stack + "@dev\n", "Plan: 3 to add, 0 to change, 0 to destroy.\n"}, plusLines(want[stack])...) {
			if !strings.Contains(p.Text, line) {
				t.Errorf("the text of %s's plan lacks %q:\n%s", stack, line, p.Text)
			}
		}
		for _, c := range d.ResourceChanges {
			var masked any
			switch c.Address {
			case "random_pet.name", "random_id.id":
				keepers, _ := c.Change.After["keepers"].(map[string]any)
				masked = keepers["canary"]
			case "local_sensitive_file.secret":
				masked = c.Change.After["content"]
			default:
				continue
			}
			if fmt.Sprint(masked) != "(sensitive)" {
				t.Errorf("plan of %s: %s keeps %v where the canary went, want (sensitive)", stack, c.Address, masked)
			}
		}
		if stack == "app" && (len(d.Configuration.Modules) != 1 || d.Configuration.Modules[0].Address != "module.cert" || d.Configuration.Modules[0].Source != "../modules/cert") {
			t.Errorf("plan of app names modules %+v, want module.cert from ../modules/cert", d.Configuration.Modules)
		}
		t.Logf("writ plan --remote in %s:\n%s", stack, p.Text)
	}

	// tofu refuses bad's variable before the mask has read a plan, so the
	// refused plan keeps only each diagnostic's summary and place.
	bad := &stackDir{t: t, dir: filepath.Join(repoDir, "bad"), env: env}
	var refused tofuPlanDoc
	r := bad.runJSON(1, &refused, "plan", "--remote", "--json")
	served = append(served, r.stdout, r.stderr)
	if refused.OK || refused.Plan.Status != "refused" || refused.Plan.ID != "" {
		t.Fatalf("plan of bad: %+v\n%s", refused.Plan, refused.Text)
	}
	if want := "writ: tofu plan exited with code 1\nerror: main.tf:10:1: Invalid value for variable\n"; refused.Text != want {
		t.Errorf("the text of bad's refused plan:\n%s\nwant:\n%s", refused.Text, want)
	}
	human := bad.run(1, "plan", "--remote")
	served = append(served, human.stdout, human.stderr)
	if !strings.Contains(human.stdout+human.stderr, "Invalid value for variable") {
		t.Fatalf("writ plan --remote in bad:\n%s", human)
	}
	t.Logf("writ plan --remote in bad:\n%s", human)

	host := &stackDir{t: t, dir: filepath.Join(repoDir, "host"), env: env}
	hr := host.exec(nil, "plan", "--remote")
	served = append(served, hr.stdout, hr.stderr)
	if want := fmt.Sprintf("host/main.tf:11: module \"x\" has source %q, which leaves", hostSource); hr.code == 0 || !strings.Contains(hr.stdout+hr.stderr, want) {
		t.Fatalf("writ plan --remote in host: want an error that says %q\n%s", want, hr)
	}

	hf := &stackDir{t: t, dir: filepath.Join(repoDir, "hostfile"), env: env}
	fr := hf.exec(nil, "plan", "--remote")
	served = append(served, fr.stdout, fr.stderr)
	if fr.code == 0 || !strings.Contains(fr.stdout+fr.stderr, "tofu plan exited with code 1") {
		t.Fatalf("writ plan --remote in hostfile: want tofu plan to fail, finding no file of the host\n%s", fr)
	}
	t.Logf("writ plan --remote in hostfile:\n%s", fr)

	sum := &stackDir{t: t, dir: filepath.Join(repoDir, "checksum"), env: env}
	sr := sum.exec(nil, "plan", "--remote")
	served = append(served, sr.stdout, sr.stderr)
	if want := fmt.Sprintf("checksum/main.tf:11: module \"x\" has source %q, which takes query parameter \"checksum\"", sumSource); sr.code == 0 || !strings.Contains(sr.stdout+sr.stderr, want) || strings.Contains(sr.stdout+sr.stderr, "tofu init") {
		t.Fatalf("writ plan --remote in checksum: want a refusal before tofu runs that says %q\n%s", want, sr)
	}

	// The CI component names the repository, the commit, and the stack,
	// and reads no file.
	ci := repo.run(0, "plan", "--remote", "--repo", "infra", "--commit", head, "--stack", "net")
	served = append(served, ci.stdout)
	for _, line := range []string{"commit plan of net@dev: " + head[:8], "OpenTofu 1.13.9 plan of net@dev\n", "Plan: 3 to add, 0 to change, 0 to destroy.\n", "It is a commit plan of infra at " + head[:8]} {
		if !strings.Contains(ci.stdout+ci.stderr, line) {
			t.Errorf("writ plan --remote --commit lacks %q:\n%s", line, ci)
		}
	}

	// Step 2: a commit that the policy refuses. net's key becomes an RSA
	// key of 2048 bits, which examples/policy/tls.rego denies.
	netMain := filepath.Join(repoDir, "net", "main.tf")
	text, err := os.ReadFile(netMain)
	if err != nil {
		t.Fatal(err)
	}
	rsa := strings.Replace(string(text), "algorithm = \"ED25519\"", "algorithm = \"RSA\"\n  rsa_bits  = 2048", 1)
	if err := os.WriteFile(netMain, []byte(rsa), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repoDir, "commit", "-q", "-am", "an RSA key for net")
	net := &stackDir{t: t, dir: filepath.Join(repoDir, "net"), env: env}
	var denied tofuPlanDoc
	r = net.runJSON(1, &denied, "plan", "--remote", "--json")
	served = append(served, r.stdout, r.stderr)
	const refusal = "tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048"
	if denied.OK || denied.Plan.Status != "refused" || denied.Plan.ID == "" || denied.Policy == nil || denied.Policy.Point != "plan" || denied.Policy.Outcome != "deny" {
		t.Fatalf("plan of the RSA key: %+v %+v\n%s", denied.Plan, denied.Policy, denied.Text)
	}
	found := false
	for _, res := range denied.Policy.Results {
		found = found || res == struct{ Set, Rule, Outcome, Message string }{"examples", "deny_rsa", "deny", refusal}
	}
	if !found || !strings.Contains(denied.Text, "Policy: denied, so this plan cannot be approved or applied.\n  deny: [examples] "+refusal+"\n") {
		t.Fatalf("the refusal of the RSA key: %+v\n%s", denied.Policy.Results, denied.Text)
	}
	deniedHuman := net.run(1, "plan", "--remote")
	served = append(served, deniedHuman.stdout, deniedHuman.stderr)
	if !strings.Contains(deniedHuman.stdout+deniedHuman.stderr, "deny: [examples] "+refusal) {
		t.Fatalf("writ plan --remote of the RSA key:\n%s", deniedHuman)
	}
	t.Logf("writ plan --remote of the RSA key:\n%s", deniedHuman)

	data := filepath.Join(home, "writ", "dev")
	tokens := 0
	if err := controltest.Raw(t, filepath.Join(data, "control.db")).QueryRowContext(context.Background(), "SELECT COUNT(*) FROM tfstate_tokens").Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("the control database holds %d state tokens after the plans ended: %v", tokens, err)
	}
	srv.stop(t)
	if _, err := os.Stat(filepath.Join(data, "control.db")); err != nil {
		t.Fatalf("the dev control database: %v", err)
	}
	scanForCanary(t, data, served, srv.out.String())
	for _, dir := range []string{data, filepath.Join(home, "runs")} {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			b, err := os.ReadFile(p)
			if err == nil && bytes.Contains(b, []byte(tofuHostMarker)) {
				t.Errorf("%s holds the module or the file from the host", p)
			}
			return err
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if strings.Contains(strings.Join(served, "")+srv.out.String(), tofuHostMarker) {
		t.Errorf("a plan or the server's log holds the module or the file from the host")
	}
}

// tofuChildRoot checks that the child has only loopback and brings it
// up, makes the child's root directory with keep bound in, and returns
// the paths of tofu and of the shared plugin cache.
func tofuChildRoot(t *testing.T, keep ...string) (bin, shared string) {
	t.Helper()
	dev, err := os.ReadFile("/proc/net/dev")
	lines := strings.Split(strings.TrimSpace(string(dev)), "\n")
	if err != nil || len(lines) != 3 || !strings.HasPrefix(strings.TrimSpace(lines[2]), "lo:") {
		t.Fatalf("the child's network interfaces: %v\n%s\nwant only lo", err, dev)
	}
	if out, err := exec.Command("ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
		t.Fatalf("ip link set lo up: %v %s", err, out)
	}
	bin, git, shared := os.Getenv(tofuBinEnv), os.Getenv(tofuGitEnv), os.Getenv(tofuCacheEnv)
	tofuRoot(t, os.Getenv(tofuRootEnv), append([]string{writBin, shared}, keep...)...)
	os.Setenv("PATH", filepath.Dir(git)+":"+filepath.Dir(bin))
	return bin, shared
}

func plusLines(addrs []string) []string {
	var out []string
	for _, a := range addrs {
		out = append(out, "  + "+a+" (create)\n")
	}
	return out
}

// scanForCanary fails if the canary or one of more, plain or encoded, is
// in any file under data, in what the API served or a dump of the control
// database holds, or in the server's log.
func scanForCanary(t *testing.T, data string, served []string, log string, more ...string) {
	t.Helper()
	forms := canaryForms(tofuCanary)
	for _, c := range more {
		for name, v := range canaryForms(c) {
			forms[c+" "+name] = v
		}
	}
	check := func(where string, b []byte) {
		for form, v := range forms {
			if bytes.Contains(b, []byte(v)) {
				t.Errorf("%s holds the canary, %s", where, form)
			}
		}
	}
	files := 0
	err := filepath.WalkDir(data, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files++
		check(p, b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatalf("the scan read no file under %s", data)
	}
	for i, s := range served {
		check(fmt.Sprintf("plan %d as the API served it", i+1), []byte(s))
	}
	check("the server's log", []byte(log))
	t.Logf("scanned %d files under %s for the canary in %d forms", files, data, len(forms))
}

// canaryForms is canary in each form a scan looks for, by name.
func canaryForms(canary string) map[string]string {
	return map[string]string{
		"plain":             canary,
		"base64":            base64.StdEncoding.EncodeToString([]byte(canary)),
		"unpadded base64":   base64.RawStdEncoding.EncodeToString([]byte(canary)),
		"URL-safe base64":   base64.RawURLEncoding.EncodeToString([]byte(canary)),
		"hex":               hex.EncodeToString([]byte(canary)),
		"query-encoded":     url.QueryEscape(canary),
		"path-encoded":      url.PathEscape(canary),
		"JSON-escaped half": strings.ReplaceAll(canary, "/", `\/`),
		"upper case":        strings.ToUpper(canary),
	}
}

// tofuRoot makes a tmpfs at dir the child's root directory, with the
// system's directories, /dev, /proc, this package's directory, and the
// files and directories of keep bound in, and points TMPDIR at its /tmp.
func tofuRoot(t *testing.T, dir string, keep ...string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("tmpfs", dir, "tmpfs", 0, "mode=0755"); err != nil {
		t.Fatalf("mount a tmpfs at %s: %v", dir, err)
	}
	for _, p := range append([]string{"/nix", "/bin", "/usr", "/lib", "/lib64", "/etc", "/dev", "/proc", wd}, keep...) {
		st, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		at := filepath.Join(dir, p)
		if st.IsDir() {
			err = os.MkdirAll(at, 0o755)
		} else if err = os.MkdirAll(filepath.Dir(at), 0o755); err == nil {
			err = os.WriteFile(at, nil, 0o755)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mount(p, at, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			t.Fatalf("bind %s: %v", p, err)
		}
	}
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	// pivot_root, not chroot: the kernel lets no chrooted process make the
	// user namespace of a run's filesystem view.
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := syscall.PivotRoot(".", "."); err != nil {
		t.Fatalf("pivot_root to %s: %v", dir, err)
	}
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		t.Fatalf("drop the old root: %v", err)
	}
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	os.Setenv("TMPDIR", "/tmp")
}
