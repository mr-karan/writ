//go:build e2e_import

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestImportHCL reads objects with writ import bulk into OpenTofu stacks and
// requires what the pivot's import promises: resource and import blocks
// that tofu fmt leaves alone, a tofu plan in an empty state that only
// imports, and writ import accept passing on that plan's JSON, or on the
// saved plan for the Nomad job, whose plan updates settings. The random
// objects come from another state that tofu applied, the Nomad job from
// tofu on a Nomad dev agent on loopback, and the AWS listener rule and
// launch template from a stand-in of the AWS API, in TestImportHCLAWS. The random steps after
// the plugin caches are filled run in a network namespace with no network. Run it
// with `just e2e-import-hcl`. It needs tofu 1.13.9, and nomad for the
// Nomad case.
func TestImportHCL(t *testing.T) {
	tofu, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatalf("this test needs OpenTofu 1.13.9 on PATH, as just e2e-import-hcl gives: %v", err)
	}
	if err := exec.Command("unshare", "-rn", "true").Run(); err != nil {
		t.Fatalf("unshare -rn: %v", err)
	}
	h := newImportHarness(t, tofu)
	t.Run("random", h.testHCLRandom)
	t.Run("nomad", h.testHCLNomad)
}

// offline is argv run in a user and network namespace with only loopback,
// which is down.
func offline(argv ...string) []string { return append([]string{"unshare", "-rn"}, argv...) }

// tofuLockFile is the lock of internal/tofu's tests: random, local, and
// tls.
func tofuLockFile(t *testing.T) string {
	t.Helper()
	return readFile(t, "../internal/tofu/testdata/lock.hcl")
}

const hclTestProviders = `terraform {
  required_providers {
    random = { source = "hashicorp/random", version = "3.9.1" }
    local  = { source = "hashicorp/local", version = "2.9.1" }
    tls    = { source = "hashicorp/tls", version = "4.4.2" }
  }
}
`

// importPlan runs writ import bulk with args in dir, which must read
// objects objects, requires that tofu fmt changes nothing in what it wrote,
// and plans the stack in its empty state. wrap runs each command, with or
// without a network. It returns the code, tofu plan's text, and the path
// of the plan's JSON.
func (h *importHarness) importPlan(t *testing.T, dir string, objects int, wrap func(...string) []string, args ...string) (code, plan, planJSON, bulk string) {
	t.Helper()
	r := h.must(t, dir, wrap(append([]string{h.writ, "import", "bulk"}, args...)...)...)
	t.Logf("writ import bulk:\n%s", r.stdout)
	if want := fmt.Sprintf("tofu plan should show %d to import, 0 to add", objects); !strings.Contains(r.stdout, want) {
		t.Fatalf("writ import bulk: want %q\n%s", want, r)
	}
	code = readFile(t, filepath.Join(dir, "imported.tf"))
	t.Logf("imported.tf:\n%s", code)
	t.Logf("imported.imports.json:\n%s", readFile(t, filepath.Join(dir, "imported.imports.json")))
	h.must(t, dir, h.tofu, "fmt", "-check", "-diff", "-no-color")
	if _, err := os.Stat(filepath.Join(dir, "terraform.tfstate")); err == nil {
		t.Fatal("the stack has state before its first plan")
	}
	h.must(t, dir, wrap(h.tofu, "init", "-input=false", "-lockfile=readonly", "-no-color")...)
	plan = h.must(t, dir, wrap(h.tofu, "plan", "-input=false", "-no-color", "-out=import.plan")...).stdout
	show := h.must(t, dir, h.tofu, "show", "-json", "import.plan")
	planJSON = filepath.Join(dir, "plan.json")
	if err := os.WriteFile(planJSON, []byte(show.stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	return code, plan, planJSON, r.stdout
}

// adopt is importPlan for objects whose plan must only import: it requires
// that, and that writ import accept passes on the plan and refuses it for
// one object more. It returns the code and what writ import bulk printed.
func (h *importHarness) adopt(t *testing.T, dir string, objects int, wrap func(...string) []string, args ...string) (code, bulk string) {
	t.Helper()
	code, plan, planJSON, bulk := h.importPlan(t, dir, objects, wrap, args...)
	if want := fmt.Sprintf("Plan: %d to import, 0 to add, 0 to change, 0 to destroy.", objects); !strings.Contains(plan, want) {
		t.Fatalf("tofu plan: want %q\n%s", want, plan)
	}
	var accepted struct {
		OK      bool `json:"ok"`
		Imports int  `json:"imports"`
	}
	imports := filepath.Join(dir, "imported.imports.json")
	h.writJSON(t, &accepted, "import", "accept", "--imports", imports, "--json", planJSON)
	if !accepted.OK || accepted.Imports != objects {
		t.Fatalf("writ import accept = %+v, want %d imports accepted", accepted, objects)
	}
	for name, edit := range map[string]func([]map[string]any) []map[string]any{
		"one object more": func(imps []map[string]any) []map[string]any {
			return append(imps, map[string]any{"type": imps[0]["type"], "name": "not_imported", "key": nil, "id": "not-imported"})
		},
		"another id": func(imps []map[string]any) []map[string]any {
			imps[0]["id"] = imps[0]["id"].(string) + "-other"
			return imps
		},
		"another address": func(imps []map[string]any) []map[string]any {
			imps[0]["name"] = "not_imported"
			return imps
		},
	} {
		var doc map[string]any
		if err := json.Unmarshal([]byte(readFile(t, imports)), &doc); err != nil {
			t.Fatal(err)
		}
		var imps []map[string]any
		for _, imp := range doc["imports"].([]any) {
			imps = append(imps, imp.(map[string]any))
		}
		doc["imports"] = edit(imps)
		other := filepath.Join(t.TempDir(), "other.imports.json")
		if err := os.WriteFile(other, []byte(mustJSON(t, doc)), 0o600); err != nil {
			t.Fatal(err)
		}
		if r := h.exec(t, dir, h.writ, "import", "accept", "--imports", other, planJSON); r.code != 1 || !strings.Contains(r.stdout, "Not accepted") {
			t.Fatalf("writ import accept with %s: %s", name, r)
		}
	}
	return code, bulk
}

// testHCLRandom applies random objects with tofu in one state and adopts
// them into another.
func (h *importHarness) testHCLRandom(t *testing.T) {
	lock := tofuLockFile(t)
	made := t.TempDir()
	writeFiles(t, made, map[string]string{".terraform.lock.hcl": lock, "main.tf": hclTestProviders + `
resource "random_uuid" "u" {
  count = 2
}

resource "random_integer" "i" {
  count = 2
  min   = 1
  max   = 100
  seed  = random_uuid.u[count.index].result
}

resource "random_string" "s" {
  length  = 12
  special = false
}

resource "random_password" "p" {
  length = 20
}

output "uuids" {
  value = random_uuid.u[*].result
}

output "integers" {
  value = [for i in random_integer.i : "${i.result},1,100,${i.seed}"]
}

output "string" {
  value = random_string.s.result
}

output "password" {
  value     = random_password.p.result
  sensitive = true
}
`})
	h.must(t, made, h.tofu, "init", "-input=false", "-lockfile=readonly", "-no-color")
	h.must(t, made, offline(h.tofu, "apply", "-auto-approve", "-input=false", "-no-color")...)
	var out struct {
		UUIDs    struct{ Value []string } `json:"uuids"`
		Integers struct{ Value []string } `json:"integers"`
		String   struct{ Value string }   `json:"string"`
		Password struct{ Value string }   `json:"password"`
	}
	if err := json.Unmarshal([]byte(h.must(t, made, h.tofu, "output", "-json").stdout), &out); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	selection, err := json.Marshal(map[string]any{"objects": []map[string]any{
		{"type": "random_uuid", "ids": out.UUIDs.Value},
		{"type": "random_integer", "ids": out.Integers.Value},
		{"type": "random_string", "ids": []string{out.String.Value}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{".terraform.lock.hcl": lock, "main.tf": hclTestProviders, "select.json": string(selection)})

	// A password's import id is its value, which no code may hold.
	refused := h.exec(t, dir, offline(h.writ, "import", "bulk", "--type", "random_password", "--id", out.Password.Value)...)
	if refused.code != 1 || !strings.Contains(refused.stdout, "OpenTofu refuses a sensitive import id") || strings.Contains(refused.stdout+refused.stderr, out.Password.Value) {
		t.Fatalf("import of a password: %s", refused)
	}
	if _, err := os.Stat(filepath.Join(dir, "imported.tf")); err == nil {
		t.Fatal("a refused import wrote imported.tf")
	}
	// tofu reads code in Unicode normal form C, so this id would import "é"
	// precomposed, and tofu would plan to replace the object.
	refused = h.exec(t, dir, offline(h.writ, "import", "bulk", "--type", "random_string", "--id", "e\u0301")...)
	if refused.code != 1 || !strings.Contains(refused.stdout, "the import id is not in Unicode normal form C") {
		t.Fatalf("import of an id outside NFC: %s", refused)
	}
	// tofu ignores imported.tf beside imported.tofu.
	writeFiles(t, dir, map[string]string{"imported.tofu": "# kept\n"})
	refused = h.exec(t, dir, offline(h.writ, "import", "bulk", "select.json")...)
	if refused.code == 0 || !strings.Contains(refused.stdout+refused.stderr, "imported.tofu exists, and tofu ignores imported.tf beside it") {
		t.Fatalf("import beside imported.tofu: %s", refused)
	}
	if err := os.Remove(filepath.Join(dir, "imported.tofu")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"imported.tf", "imported.imports.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("a refused import wrote %s", name)
		}
	}

	code, _ := h.adopt(t, dir, 5, offline, "select.json")
	for _, want := range []string{
		`resource "random_uuid" "random_uuid" {`,
		"seed = random_uuid.random_uuid[each.value.random_uuid].id",
		"to       = random_integer.random_integer[each.key]",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("imported.tf lacks %s", want)
		}
	}
}

// testHCLNomad imports a job that tofu registered on a Nomad dev agent. The
// Nomad provider keeps settings of its own, such as detach, that the agent
// does not return, so the first plan after the import sets them in place:
// the one exception writ import accept allows. The plan must
// change those settings and nothing else, the job's spec included, writ
// import accept must refuse the plan's JSON, pass the saved plan and print
// each setting, and applying the plan must leave the job as it was.
func (h *importHarness) testHCLNomad(t *testing.T) {
	if _, err := exec.LookPath("nomad"); err != nil {
		t.Fatalf("nomad is not on PATH: %v", err)
	}
	api := startImportNomad(t)
	made := t.TempDir()
	writeFiles(t, made, map[string]string{"main.tf": fmt.Sprintf(nomadMain, api)})
	h.apply(t, made)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		".terraform.lock.hcl": readFile(t, filepath.Join(made, ".terraform.lock.hcl")),
		"main.tf":             strings.Split(fmt.Sprintf(nomadMain, api), "resource \"nomad_job\"")[0],
	})
	code, plan, planJSON, _ := h.importPlan(t, dir, 1, func(argv ...string) []string { return argv }, "--type", "nomad_job", "--id", "import-web@default")
	if !strings.Contains(code, `resource "nomad_job" "nomad_job_import_web_default" {`) || !strings.Contains(code, "  jobspec = <<EOT\njob \"import-web\" {\n") {
		t.Errorf("imported.tf does not declare the job")
	}
	if want := "Plan: 1 to import, 0 to add, 1 to change, 0 to destroy."; !strings.Contains(plan, want) {
		t.Fatalf("tofu plan: want %q\n%s", want, plan)
	}
	var doc struct {
		ResourceChanges []struct {
			Address string
			Change  struct {
				Actions       []string
				Before, After map[string]any
				Importing     *struct{ ID string }
			}
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal([]byte(readFile(t, planJSON)), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.ResourceChanges) != 1 || doc.ResourceChanges[0].Change.Importing == nil {
		t.Fatalf("resource changes = %+v, want the job's import", doc.ResourceChanges)
	}
	c := doc.ResourceChanges[0].Change
	var changed []string
	for k, after := range c.After {
		if before, err1 := json.Marshal(c.Before[k]); err1 != nil || string(before) != mustJSON(t, after) {
			changed = append(changed, k)
		}
	}
	slices.Sort(changed)
	settings := []string{"deregister_on_destroy", "deregister_on_id_change", "detach", "preserve_counts", "read_allocation_ids", "rerun_if_dead"}
	if !slices.Equal(changed, settings) || c.Before["jobspec"] != c.After["jobspec"] {
		t.Fatalf("the update changes %v, want only the provider's own settings %v", changed, settings)
	}
	imports := filepath.Join(dir, "imported.imports.json")
	// Only the saved plan, which writ import accept reads with tofu show
	// -json itself, proves what the update changes.
	if refused := h.exec(t, dir, h.writ, "import", "accept", "--imports", imports, planJSON); refused.code != 1 || !strings.Contains(refused.stdout, "pass writ import accept the plan file that tofu plan -out saved") {
		t.Fatalf("writ import accept of the plan's JSON: want the update refused\n%s", refused)
	}
	saved := filepath.Join(dir, "import.plan")
	accepted := h.must(t, dir, h.writ, "import", "accept", "--imports", imports, saved)
	t.Logf("writ import accept:\n%s", accepted.stdout)
	for _, k := range settings {
		after, _ := json.Marshal(c.After[k])
		if want := fmt.Sprintf("~ nomad_job.nomad_job_import_web_default: sets %s = %s, a setting that registry.opentofu.org/hashicorp/nomad 2.6.1 keeps and the cloud does not return\n", k, after); !strings.Contains(accepted.stdout, want) {
			t.Errorf("writ import accept: want %q", want)
		}
	}
	if want := "Accepted: the plan imports 1 object, sets 6 provider-only settings that the cloud does not return, and changes nothing else."; !strings.Contains(accepted.stdout, want) {
		t.Fatalf("writ import accept: want %q", want)
	}
	var report struct {
		OK       bool
		Imports  int
		Settings []struct{ Address, Attribute string }
	}
	reportText := h.must(t, dir, h.writ, "import", "accept", "--imports", imports, "--json", saved).stdout
	t.Logf("writ import accept --json:\n%s", reportText)
	if err := json.Unmarshal([]byte(reportText), &report); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range report.Settings {
		got = append(got, s.Attribute)
	}
	if !report.OK || report.Imports != 1 || !slices.Equal(got, settings) {
		t.Fatalf("writ import accept --json = %+v, want the import and the settings %v", report, settings)
	}

	// The provider's update registers the jobspec again; Nomad keeps the
	// job as it was when its spec has not changed.
	before := nomadJobVersion(t, api, "import-web")
	h.must(t, dir, h.tofu, "apply", "-input=false", "-no-color", "import.plan")
	if after := nomadJobVersion(t, api, "import-web"); after != before {
		t.Errorf("the apply changed the job from %+v to %+v", before, after)
	}
	if again := h.must(t, dir, h.tofu, "plan", "-input=false", "-no-color", "-detailed-exitcode"); !strings.Contains(again.stdout, "No changes.") {
		t.Fatalf("tofu plan after the apply:\n%s", again.stdout)
	}
}

// nomadJobVersion is the version and modify indexes of the job id on the
// Nomad agent at api.
func nomadJobVersion(t *testing.T, api, id string) (v struct{ Version, JobModifyIndex, ModifyIndex uint64 }) {
	t.Helper()
	resp, err := http.Get(api + "/v1/job/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/job/%s: %s", id, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestImportHCLAWS adopts an ELB listener rule, a listener, and a launch template that
// a stand-in of the AWS API serves, through the real hashicorp/aws
// provider. The parent fills tofu's plugin cache, which needs the registry
// on the first run, and builds writ; then the test runs again in a user and
// network namespace where only loopback exists, with the stand-in on it.
func TestImportHCLAWS(t *testing.T) {
	const child, writEnv = "WRIT_E2E_IMPORT_HCL_AWS_CHILD", "WRIT_E2E_IMPORT_HCL_WRIT"
	tofu, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatalf("this test needs OpenTofu 1.13.9 on PATH, as just e2e-import-hcl gives: %v", err)
	}
	if os.Getenv(child) == "" {
		h := newImportHarness(t, tofu)
		fill := t.TempDir()
		writeFiles(t, fill, map[string]string{".terraform.lock.hcl": readFile(t, "testdata/import-hcl/aws.lock.hcl"), "main.tf": awsHCLProviders})
		h.must(t, fill, h.tofu, "init", "-input=false", "-lockfile=readonly", "-backend=false", "-no-color")
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("unshare", "-rn", self, "-test.run=^TestImportHCLAWS$", "-test.v", "-test.count=1")
		cmd.Env = append(h.env(), child+"=1", writEnv+"="+h.writ)
		out, err := cmd.CombinedOutput()
		t.Logf("in the namespace:\n%s", out)
		if err != nil || !strings.Contains(string(out), "--- PASS: TestImportHCLAWS") {
			t.Fatalf("the test failed in the namespace: %v", err)
		}
		return
	}
	if out, err := exec.Command("ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
		t.Fatalf("loopback: %v %s", err, out)
	}
	h := &importHarness{tofu: tofu, writ: os.Getenv(writEnv), cache: os.Getenv("WRIT_CACHE_DIR"), plugins: os.Getenv("TF_PLUGIN_CACHE_DIR"), tfrc: os.Getenv("TF_CLI_CONFIG_FILE")}
	t.Run("listener rule", h.testHCLListenerRule)
	t.Run("listener", h.testHCLListener)
	t.Run("launch template", h.testHCLLaunchTemplate)
}

// awsStandIn is a stack in a new directory whose aws provider sends the
// calls of service to a stand-in of the AWS API that answers with answer,
// keyed by the call's Action, and fails the test on any other call.
func awsStandIn(t *testing.T, service string, answers map[string]string) string {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		w.Header().Set("Content-Type", "text/xml")
		answer, ok := answers[r.Form.Get("Action")]
		if !ok {
			t.Errorf("unexpected cloud action %s", r.Form.Get("Action"))
			http.Error(w, "unexpected action", 400)
			return
		}
		fmt.Fprint(w, answer)
	}))
	t.Cleanup(api.Close)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		".terraform.lock.hcl": readFile(t, "testdata/import-hcl/aws.lock.hcl"),
		"main.tf": awsHCLProviders + fmt.Sprintf(`
provider "aws" {
  region                      = "us-east-1"
  access_key                  = "mock_access_key"
  secret_key                  = "mock_secret_key"
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = "true"
  skip_region_validation      = true
  endpoints {
    %s = %q
  }
}
`, service, api.URL),
	})
	return dir
}

func (h *importHarness) testHCLListenerRule(t *testing.T) {
	const arn = "arn:aws:elasticloadbalancing:us-east-1:123456789012:listener-rule/app/app/1234567890123456/1234567890123456/1234567890123456"
	dir := awsStandIn(t, "elbv2", map[string]string{
		"DescribeRules": fmt.Sprintf(`<DescribeRulesResponse><DescribeRulesResult><Rules><member><RuleArn>%s</RuleArn><Priority>1</Priority><IsDefault>false</IsDefault><Conditions><member><Field>path-pattern</Field><Values><member>/*</member></Values><PathPatternConfig><Values><member>/*</member></Values></PathPatternConfig></member></Conditions><Actions><member><Type>forward</Type><Order>1</Order><TargetGroupArn>arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/app/1234567890123456</TargetGroupArn><ForwardConfig><TargetGroups><member><TargetGroupArn>arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/app/1234567890123456</TargetGroupArn><Weight>1</Weight></member></TargetGroups><TargetGroupStickinessConfig><Enabled>false</Enabled><DurationSeconds>3600</DurationSeconds></TargetGroupStickinessConfig></ForwardConfig></member></Actions></member></Rules></DescribeRulesResult></DescribeRulesResponse>`, arn),
		"DescribeTags":  `<DescribeTagsResponse><DescribeTagsResult><TagDescriptions><member><ResourceArn>` + arn + `</ResourceArn><Tags/></member></TagDescriptions></DescribeTagsResult></DescribeTagsResponse>`,
	})
	code, _ := h.adopt(t, dir, 1, func(argv ...string) []string { return argv }, "--type", "aws_lb_listener_rule", "--id", arn)
	for _, want := range []string{`resource "aws_lb_listener_rule" "aws_lb_listener_rule_priority_1" {`, "  action {\n", "    forward {\n", "  condition {\n"} {
		if !strings.Contains(code, want) {
			t.Errorf("imported.tf lacks %q", want)
		}
	}
	if strings.Contains(code, "mock_secret_key") {
		t.Error("imported.tf holds the provider's secret key")
	}
}

// testHCLListener adopts a listener whose forward action has stickiness
// disabled with no DurationSeconds, as AWS Emulator answers. The provider
// reads that as duration 0, which its validator rejects, so bulk must
// leave the disabled block out rather than write duration = 0.
func (h *importHarness) testHCLListener(t *testing.T) {
	const (
		arn = "arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/app/1234567890123456/1234567890123456"
		lb  = "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/app/1234567890123456"
		tg  = "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/app/1234567890123456"
	)
	dir := awsStandIn(t, "elbv2", map[string]string{
		"DescribeListeners":          `<DescribeListenersResponse><DescribeListenersResult><Listeners><member><ListenerArn>` + arn + `</ListenerArn><LoadBalancerArn>` + lb + `</LoadBalancerArn><Port>80</Port><Protocol>HTTP</Protocol><Certificates></Certificates><DefaultActions><member><Type>forward</Type><TargetGroupArn>` + tg + `</TargetGroupArn><Order>1</Order><ForwardConfig><TargetGroups><member><TargetGroupArn>` + tg + `</TargetGroupArn><Weight>1</Weight></member></TargetGroups><TargetGroupStickinessConfig><Enabled>false</Enabled></TargetGroupStickinessConfig></ForwardConfig></member></DefaultActions><AlpnPolicy></AlpnPolicy></member></Listeners></DescribeListenersResult></DescribeListenersResponse>`,
		"DescribeListenerAttributes": `<DescribeListenerAttributesResponse><DescribeListenerAttributesResult><Attributes/></DescribeListenerAttributesResult></DescribeListenerAttributesResponse>`,
		"DescribeTags":               `<DescribeTagsResponse><DescribeTagsResult><TagDescriptions><member><ResourceArn>` + arn + `</ResourceArn><Tags/></member></TagDescriptions></DescribeTagsResult></DescribeTagsResponse>`,
	})
	code, _ := h.adopt(t, dir, 1, func(argv ...string) []string { return argv }, "--type", "aws_lb_listener", "--id", arn)
	for _, want := range []string{`resource "aws_lb_listener" "aws_lb_listener_http_80" {`, "  default_action {\n", "    forward {\n"} {
		if !strings.Contains(code, want) {
			t.Errorf("imported.tf lacks %q", want)
		}
	}
	if strings.Contains(code, "stickiness") {
		t.Errorf("imported.tf keeps the disabled stickiness block:\n%s", code)
	}
}

// testHCLLaunchTemplate adopts a launch template, the type for which
// Writ's own import kept update_default_version as a provider-only
// setting. hashicorp/aws 6.70.0 reads it as null on import and writ import
// bulk leaves it out, so the plan only imports. Code that sets it true
// plans default_version unknown, because the provider's update moves the
// default version to the latest; that is a cloud change, and accept
// refuses it.
func (h *importHarness) testHCLLaunchTemplate(t *testing.T) {
	dir := awsStandIn(t, "ec2", map[string]string{
		"DescribeLaunchTemplates":        `<DescribeLaunchTemplatesResponse><launchTemplates><item><launchTemplateId>lt-0123456789abcdef0</launchTemplateId><launchTemplateName>app</launchTemplateName><defaultVersionNumber>5</defaultVersionNumber><latestVersionNumber>5</latestVersionNumber><createdBy>arn:aws:iam::123456789012:root</createdBy><createTime>2026-01-01T00:00:00Z</createTime></item></launchTemplates></DescribeLaunchTemplatesResponse>`,
		"DescribeLaunchTemplateVersions": `<DescribeLaunchTemplateVersionsResponse><launchTemplateVersionSet><item><launchTemplateId>lt-0123456789abcdef0</launchTemplateId><launchTemplateName>app</launchTemplateName><versionNumber>5</versionNumber><defaultVersion>true</defaultVersion><launchTemplateData><imageId>ami-0123456789abcdef0</imageId><instanceType>t3.micro</instanceType></launchTemplateData></item></launchTemplateVersionSet></DescribeLaunchTemplateVersionsResponse>`,
	})
	code, _ := h.adopt(t, dir, 1, func(argv ...string) []string { return argv }, "--type", "aws_launch_template", "--id", "lt-0123456789abcdef0")
	if strings.Contains(code, "update_default_version") {
		t.Errorf("imported.tf sets update_default_version:\n%s", code)
	}
	pinned := regexp.MustCompile(`(?m)^  default_version += 5$`)
	if !pinned.MatchString(code) {
		t.Fatalf("imported.tf does not pin default_version to 5:\n%s", code)
	}
	if err := os.WriteFile(filepath.Join(dir, "imported.tf"), []byte(pinned.ReplaceAllString(code, "  update_default_version = true")), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := h.must(t, dir, h.tofu, "plan", "-input=false", "-no-color", "-out=import.plan").stdout
	if want := "Plan: 1 to import, 0 to add, 1 to change, 0 to destroy."; !strings.Contains(plan, want) {
		t.Fatalf("tofu plan with update_default_version: want %q\n%s", want, plan)
	}
	refused := h.exec(t, dir, h.writ, "import", "accept", "--imports", filepath.Join(dir, "imported.imports.json"), filepath.Join(dir, "import.plan"))
	if want := `plans update of "default_version", "update_default_version", which the named list of provider-only settings does not hold for aws_launch_template`; refused.code != 1 || !strings.Contains(refused.stdout, want) {
		t.Fatalf("writ import accept of update_default_version: want %q\n%s", want, refused)
	}
}

const awsHCLProviders = `terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "6.70.0" }
  }
}
`
