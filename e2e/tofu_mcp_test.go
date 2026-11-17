//go:build e2e && e2e_tofu && linux

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mr-karan/writ/internal/importer"
)

// tofuMCPUUID is the id of the object the test imports. The random
// provider imports any UUID and reads nothing.
const tofuMCPUUID = "6b0a7c4e-2f1d-4c3a-9e8b-5d7f1a2c3b4e"

// TestTofuMCP is an agent's loop through writ mcp on writ serve --dev with
// real tofu, in the namespaces TestTofuPlanByCommit runs in: plan a
// commit, read the masked plan, see a person's apply in runs and the
// state index, import an object and accept the plan, read the verdict of
// a commit a policy refuses and the policy's text, and be refused a plan
// of an edit that is not committed. No result holds the canary the stack
// takes through a sensitive variable.
func TestTofuMCP(t *testing.T) {
	if os.Getenv(tofuChildEnv) != "" {
		tofuMCPChild(t)
		return
	}
	inTofuNamespace(t, "TestTofuMCP", tofuNamespace)
}

// tofuMCPPolicy denies a pet name of more than three words.
const tofuMCPPolicy = `package main

import rego.v1

deny_long_pet contains msg if {
	some rc in input.resource_changes
	rc.type == "random_pet"
	"create" in rc.change.actions
	rc.change.after.length > 3
	msg := sprintf("%s: a pet name has at most 3 words", [rc.address])
}
`

func tofuMCPChild(t *testing.T) {
	lock := tofuLock(t)
	bin, shared := tofuChildRoot(t)
	home := t.TempDir()
	cache := filepath.Join(home, "plugins")
	if err := os.CopyFS(cache, os.DirFS(shared)); err != nil {
		t.Fatal(err)
	}
	policyDir := filepath.Join(home, "policy")
	if err := os.MkdirAll(policyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(policyDir, "pets.rego"), []byte(tofuMCPPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "dev.hcl")
	if err := os.WriteFile(config, fmt.Appendf(nil, "tofu {\n  binary = %q\n  plugin_cache = %q\n  scratch_dir = %q\n}\n\npolicy {\n  set \"pets\" {\n    dir = %q\n  }\n}\n", bin, cache, filepath.Join(home, "runs"), policyDir), 0o600); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(home, "infra")
	app := &stackDir{t: t, dir: filepath.Join(repoDir, "app"), env: []string{"XDG_DATA_HOME=" + home}}
	if err := os.MkdirAll(app.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	app.write(".terraform.lock.hcl", string(lock))
	app.write("main.tf", "terraform {\n  backend \"http\" {}\n"+tofuProviders+`}

variable "canary" {
  type      = string
  sensitive = true
}

resource "random_pet" "name" {
  keepers = { canary = var.canary }
}
`)
	gitIn(t, repoDir, "init", "-q", "-b", "main")
	gitIn(t, repoDir, "add", ".")
	gitIn(t, repoDir, "commit", "-q", "-m", "one stack")
	srv := startDevAt(t, home, []string{"TF_VAR_canary=" + tofuCanary}, "--config", config)
	repo := &stackDir{t: t, dir: repoDir, env: app.env}
	repo.run(0, "repo", "add", ".")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.Command(writBin, "mcp")
	cmd.Dir, cmd.Env = repoDir, append(os.Environ(), app.env...)
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "0"}, nil).Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	var served []string
	call := func(tool string, args map[string]any) (string, bool) {
		t.Helper()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		text := res.Content[0].(*sdk.TextContent).Text
		served = append(served, text)
		return text, res.IsError
	}
	plan := func() tofuPlanDoc {
		t.Helper()
		text, isError := call("plan", map[string]any{"dir": "app", "json": true})
		var p tofuPlanDoc
		if isError || json.Unmarshal([]byte(text), &p) != nil || p.Plan.ID == "" || p.Plan.Status != "approved" {
			t.Fatalf("plan: isError %v\n%s", isError, text)
		}
		return p
	}

	text, isError := call("plan", map[string]any{"dir": "app"})
	for _, want := range []string{"Policy: passed.\n\nOpenTofu 1.13.9 plan of app@dev\n", "  + random_pet.name (create)\n", "Plan: 1 to add, 0 to change, 0 to destroy.\n", "It needs no approval."} {
		if isError || !strings.Contains(text, want) {
			t.Errorf("the plan lacks %q: isError %v\n%s", want, isError, text)
		}
	}
	first := plan()
	text, isError = call("get_plan", map[string]any{"id": first.Plan.ID, "addresses": []string{"random_pet.name"}})
	var got tofuPlanDoc
	if isError || json.Unmarshal([]byte(text), &got) != nil || len(got.Document.ResourceChanges) != 1 {
		t.Fatalf("get_plan: isError %v\n%s", isError, text)
	}
	if keepers, _ := got.Document.ResourceChanges[0].Change.After["keepers"].(map[string]any); fmt.Sprint(keepers["canary"]) != "(sensitive)" {
		t.Errorf("get_plan shows the keeper as %v, want (sensitive)", keepers["canary"])
	}

	app.run(0, "apply", "--remote", first.Plan.ID)
	var runs struct {
		Runs []struct{ ID, Plan, Status string }
	}
	if text, isError := call("runs", map[string]any{"stack": "app"}); isError || json.Unmarshal([]byte(text), &runs) != nil || len(runs.Runs) != 1 || runs.Runs[0].Plan != first.Plan.ID {
		t.Fatalf("runs: isError %v\n%s", isError, text)
	}
	if text, isError := call("runs", map[string]any{"id": runs.Runs[0].ID}); isError || !strings.Contains(text, `"format":"writ.tofu-apply/v0"`) || !strings.Contains(text, `"address":"random_pet.name"`) {
		t.Fatalf("one run: isError %v\n%s", isError, text)
	}
	var index struct {
		Resources []struct{ Address, ID string }
	}
	if text, isError := call("state_index", map[string]any{"stack": "app"}); isError || json.Unmarshal([]byte(text), &index) != nil || len(index.Resources) != 1 || index.Resources[0].Address != "random_pet.name" || index.Resources[0].ID == "" {
		t.Fatalf("state_index: isError %v\n%s", isError, text)
	}
	if text, isError := call("drift", map[string]any{}); isError || !strings.Contains(text, `"stack":"app","env":"dev","backend":"http","resources":1`) {
		t.Fatalf("drift: isError %v\n%s", isError, text)
	}

	app.write("imported.tf", fmt.Sprintf("resource \"random_uuid\" \"u\" {}\n\nimport {\n  to = random_uuid.u\n  id = %q\n}\n", tofuMCPUUID))
	imports, err := importer.EncodeImports([]importer.Import{{Type: "random_uuid", Name: "u", ID: tofuMCPUUID}})
	if err != nil {
		t.Fatal(err)
	}
	app.write("imported.imports.json", string(imports))
	gitIn(t, repoDir, "add", ".")
	gitIn(t, repoDir, "commit", "-q", "-m", "import a uuid")
	imported := plan()
	var acc struct {
		OK      bool
		Imports int
	}
	if text, isError := call("import_accept", map[string]any{"dir": "app", "plan": imported.Plan.ID}); isError || json.Unmarshal([]byte(text), &acc) != nil || !acc.OK || acc.Imports != 1 {
		t.Fatalf("import_accept: isError %v\n%s", isError, text)
	}
	if text, isError := call("import_accept", map[string]any{"dir": "app", "plan": first.Plan.ID}); !isError || !strings.Contains(text, `"ok":false`) {
		t.Fatalf("import_accept of a plan without the import: isError %v\n%s", isError, text)
	}

	app.write("long.tf", "resource \"random_pet\" \"long\" {\n  length = 5\n}\n")
	gitIn(t, repoDir, "add", ".")
	gitIn(t, repoDir, "commit", "-q", "-m", "a long name")
	text, isError = call("plan", map[string]any{"dir": "app"})
	verdict := "Policy: denied, so this plan cannot be approved or applied, and nothing overrides a deny.\n" +
		"  deny: [pets, mandatory] deny_long_pet: random_pet.long: a pet name has at most 3 words\n\nOpenTofu 1.13.9 plan of app@dev\n"
	if !isError || !strings.HasPrefix(text, verdict) || strings.Count(text, "at most 3 words") != 1 || !strings.Contains(text, "is refused.") {
		t.Fatalf("the plan a policy refuses: isError %v\n%s", isError, text)
	}
	if text, isError := call("policy", map[string]any{"set": "pets"}); isError || !strings.Contains(text, `"name":"pets.rego"`) || !strings.Contains(text, "deny_long_pet contains msg if") {
		t.Fatalf("policy: isError %v\n%s", isError, text)
	}

	app.write("extra.tf", "resource \"random_pet\" \"extra\" {}\n")
	if text, isError := call("plan", map[string]any{"dir": "app"}); !isError || !strings.Contains(text, "the work tree differs from commit") || !strings.Contains(text, "app/extra.tf") {
		t.Fatalf("a plan of an edit that is not committed: isError %v\n%s", isError, text)
	}
	if slices.ContainsFunc(served, func(s string) bool { return strings.Contains(s, tofuCanary) }) {
		t.Fatal("a result holds the canary")
	}
	srv.stop(t)
	scanForCanary(t, filepath.Join(home, "writ", "dev"), served, srv.out.String())
}
