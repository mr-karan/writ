package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

const (
	tofuCanary    = "canary-sensitive-var-5d1f"
	tofuEnvSecret = "canary-provider-env-8b2e"
)

const tofuLockFile = `provider "registry.opentofu.org/hashicorp/random" {
  version = "3.9.1"
  hashes  = ["h1:abc="]
}
`

// tofuPlanJSON is `tofu show -json` of a plan of tofuFiles' app stack,
// with the canary of a sensitive variable under a mark, and the value of
// a provider_env file in an output no mark covers.
const tofuPlanJSON = `{"format_version":"1.2","terraform_version":"1.13.9",
 "variables":{"canary":{"value":"` + tofuCanary + `"}},
 "configuration":{"provider_config":{"random":{"name":"random","full_name":"registry.opentofu.org/hashicorp/random"}},
  "root_module":{"variables":{"canary":{"sensitive":true}},"module_calls":{"cert":{"source":"../modules/cert","module":{}}}}},
 "resource_changes":[
  {"address":"random_pet.p","mode":"managed","type":"random_pet","name":"p","provider_name":"registry.opentofu.org/hashicorp/random",
   "change":{"actions":["create"],"before":null,"after":{"keepers":{"c":"` + tofuCanary + `"},"length":2},"after_unknown":{"id":true},"before_sensitive":false,"after_sensitive":{"keepers":{"c":true}}}},
  {"address":"module.cert.random_id.i","module_address":"module.cert","mode":"managed","type":"random_id","name":"i","provider_name":"registry.opentofu.org/hashicorp/random",
   "change":{"actions":["delete","create"],"before":{"byte_length":4},"after":{"byte_length":8},"after_unknown":{"hex":true},"before_sensitive":false,"after_sensitive":false,"replace_paths":[["byte_length"]]}}
 ],
 "output_changes":{"leak":{"actions":["create"],"before":null,"after":"` + tofuEnvSecret + `","before_sensitive":false,"after_sensitive":false}}
}`

// Canaries of the state an apply leaves, which no row of the state index
// may hold: one in an attribute the state marks sensitive, one in an
// attribute the index does not keep, and one in an attribute only a
// nomad_job keeps.
const (
	tofuMarked     = "canary-marked-attr-71c4"
	tofuNotListed  = "canary-unlisted-attr-0e9a"
	tofuNotForType = "canary-namespace-of-x-d2b6"
)

// tofuStateJSON is `tofu show -json` of the state of app after an apply,
// with a canary under a mark, among them an id, which the index keeps of
// every type, and canaries in attributes the index does not keep.
const tofuStateJSON = `{"format_version":"1.0","terraform_version":"1.13.9","values":{
 "outputs":{"pet":{"sensitive":false,"value":"keen-mole"}},
 "root_module":{"resources":[
  {"address":"random_pet.p","mode":"managed","type":"random_pet","name":"p","provider_name":"registry.opentofu.org/hashicorp/random",
   "values":{"id":"keen-mole","keepers":{"c":"` + tofuCanary + `"},"length":2},"sensitive_values":{"keepers":{"c":true}}},
  {"address":"aws_security_group.web","mode":"managed","type":"aws_security_group","name":"web","provider_name":"registry.opentofu.org/hashicorp/aws",
   "values":{"id":"sg-0a1b2c3d4e5f60718","arn":"arn:aws:ec2:ap-south-1:123456789012:security-group/sg-0a1b2c3d4e5f60718","region":"ap-south-1",
    "description":"` + tofuNotListed + `","namespace":"` + tofuNotForType + `","tags":{"Name":"web"}},"sensitive_values":{"tags":{}}},
  {"address":"aws_db_instance.orders","mode":"managed","type":"aws_db_instance","name":"orders","provider_name":"registry.opentofu.org/hashicorp/aws",
   "values":{"id":"` + tofuMarked + `","identifier":"orders-db","password":"` + tofuMarked + `-pw"},"sensitive_values":{"id":true,"password":true}},
  {"address":"nomad_job.web","mode":"managed","type":"nomad_job","name":"web","provider_name":"registry.opentofu.org/hashicorp/nomad",
   "values":{"id":"web","namespace":"apps","modify_index":"42","jobspec":"` + tofuNotListed + `"},"sensitive_values":{}}
 ],
 "child_modules":[{"address":"module.cert","resources":[
  {"address":"module.cert.random_id.i","mode":"managed","type":"random_id","name":"i","provider_name":"registry.opentofu.org/hashicorp/random",
   "values":{"id":"AbCdEf","byte_length":8,"hex":"` + tofuNotListed + `"},"sensitive_values":{}}]}]}}}`

// tofuIndex is the state index of tofuStateJSON.
var tofuIndex = []control.IndexRow{
	{Address: "aws_db_instance.orders", Type: "aws_db_instance", Provider: "registry.opentofu.org/hashicorp/aws", Identifier: "orders-db"},
	{Address: "aws_security_group.web", Type: "aws_security_group", Provider: "registry.opentofu.org/hashicorp/aws", ID: "sg-0a1b2c3d4e5f60718",
		ARN: "arn:aws:ec2:ap-south-1:123456789012:security-group/sg-0a1b2c3d4e5f60718", Region: "ap-south-1"},
	{Address: "module.cert.random_id.i", Type: "random_id", Provider: "registry.opentofu.org/hashicorp/random", ID: "AbCdEf"},
	{Address: "nomad_job.web", Type: "nomad_job", Provider: "registry.opentofu.org/hashicorp/nomad", ID: "web", Namespace: "apps", ModifyIndex: "42"},
	{Address: "random_pet.p", Type: "random_pet", Provider: "registry.opentofu.org/hashicorp/random", ID: "keen-mole"},
}

// fakeTofu stands in for OpenTofu at the server's boundary. Inside each
// plan it reads the stack's state on the http backend with the run's
// token, as tofu does, and keeps the token to try it again after.
type fakeTofu struct {
	mu sync.Mutex
	// gate, when set, holds each plan until it is closed or its context
	// ends.
	gate   chan struct{}
	base   string
	fills  [][]byte
	stacks []tofu.Stack
	files  [][]string
	// during is the status the state backend answered inside the plan.
	during []int
	tokens []stateCall
	plan   func() (tofu.Plan, error)
	// applied are the stacks and saved plans of each apply, and apply
	// stands for tofu in each: nil writes one state version.
	applied []appliedPlan
	apply   func(ctx context.Context, c stateCall) ([]byte, error)
	// state is show -json of the state each apply leaves, tofuStateJSON
	// when "", and showErr the error of that show.
	state   string
	showErr error
	// shown are the stacks and states of each Show.
	shown []shownState
	// pulled are the stacks of each Pull, and pull its result: no state
	// when nil.
	pulled []tofu.Stack
	pull   func() (tofu.State, error)
	// applyS3 stands for tofu in each apply of a stack whose state is on
	// S3, with the state it pulled after.
	applyS3 func() (tofu.Applied, error)
}

func (f *fakeTofu) Pull(_ context.Context, s tofu.Stack) (tofu.State, error) {
	f.mu.Lock()
	f.pulled = append(f.pulled, s)
	pull := f.pull
	f.mu.Unlock()
	if pull == nil {
		return tofu.State{}, nil
	}
	return pull()
}

type shownState struct {
	stack tofu.Stack
	state []byte
}

// Show reports state, or tofuStateJSON, as the show of the state it is
// given.
func (f *fakeTofu) Show(_ context.Context, s tofu.Stack, state []byte) (tofu.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shown = append(f.shown, shownState{stack: s, state: state})
	return tofu.State{Unmasked: []byte(f.stateJSON())}, nil
}

func (f *fakeTofu) stateJSON() string {
	if f.state != "" {
		return f.state
	}
	return tofuStateJSON
}

type appliedPlan struct {
	stack tofu.Stack
	saved []byte
	token stateCall
}

// Apply runs apply, or else locks the stack's state on the http backend,
// writes a version, and unlocks it, as tofu does, and reports the events
// of created resources, two of whose addresses hold a secret: the
// environment's, and the sensitive variable's, which only the plan marks.
func (f *fakeTofu) Apply(ctx context.Context, s tofu.Stack, plan []byte) (tofu.Applied, error) {
	c := stateCall{addr: envValue(s.Env, "TF_HTTP_ADDRESS"), token: envValue(s.Env, "TF_HTTP_PASSWORD")}
	f.mu.Lock()
	f.applied = append(f.applied, appliedPlan{stack: s, saved: plan, token: c})
	apply, applyS3 := f.apply, f.applyS3
	f.mu.Unlock()
	if s.StateCreds && applyS3 != nil {
		return applyS3()
	}
	if apply != nil {
		log, err := apply(ctx, c)
		return tofu.Applied{Plan: []byte(tofuPlanJSON), Log: log}, err
	}
	for _, step := range []struct{ method, path, body string }{
		{"LOCK", "/lock", `{"ID":"lock-1","Operation":"OperationTypeApply"}`},
		{"POST", "?ID=lock-1", `{"version":4,"serial":1,"lineage":"lineage-1","resources":[]}`},
		{"UNLOCK", "/lock", `{"ID":"lock-1"}`},
	} {
		if status := f.call(c, step.method, step.path, step.body); status != http.StatusOK {
			return tofu.Applied{}, fmt.Errorf("%s of the state: %d", step.method, status)
		}
	}
	f.mu.Lock()
	state, showErr := f.stateJSON(), f.showErr
	f.mu.Unlock()
	applied := tofu.Applied{Plan: []byte(tofuPlanJSON), ShowErr: showErr}
	if showErr == nil {
		applied.State.Unmasked = []byte(state)
	}
	applied.Log = []byte(`{"type":"apply_start","hook":{"resource":{"addr":"random_pet.p"},"action":"create"}}
{"type":"apply_complete","hook":{"resource":{"addr":"random_pet.p"},"action":"create","id_key":"id","id_value":"id-` + tofuCanary + `","elapsed_seconds":0}}
{"type":"apply_complete","hook":{"resource":{"addr":"random_id.i[\"` + tofuEnvSecret + `\"]"},"action":"create","elapsed_seconds":1}}
{"type":"apply_complete","hook":{"resource":{"addr":"random_id.k[\"` + tofuCanary + `\"]"},"action":"create","elapsed_seconds":0}}
{"type":"change_summary","changes":{"add":3,"change":0,"import":0,"remove":0,"operation":"apply"}}
`)
	return applied, nil
}

// holdApplies makes each apply of r wait until the returned channel is
// closed or the apply's context ends, and then end with no state written.
func holdApplies(r *fakeTofu) chan struct{} {
	gate := make(chan struct{})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.apply = func(ctx context.Context, _ stateCall) ([]byte, error) {
		select {
		case <-gate:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return gate
}

// appliedSoFar is the applies OpenTofu ran.
func (f *fakeTofu) appliedSoFar() []appliedPlan {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.applied)
}

// call sends one request of tofu's http backend client with c's token.
func (f *fakeTofu) call(c stateCall, method, suffix, body string) int {
	u, err := url.Parse(c.addr)
	if err != nil || c.token == "" {
		return 0
	}
	req, _ := http.NewRequest(method, f.base+u.EscapedPath()+suffix, strings.NewReader(body))
	req.SetBasicAuth("writ", c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

type stateCall struct{ addr, token string }

func (f *fakeTofu) Fill(_ context.Context, lock []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fills = append(f.fills, lock)
	return nil
}

func (f *fakeTofu) Plan(ctx context.Context, s tofu.Stack) (tofu.Plan, error) {
	var names []string
	_ = fs.WalkDir(s.Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return err
	})
	c := stateCall{addr: envValue(s.Env, "TF_HTTP_ADDRESS"), token: envValue(s.Env, "TF_HTTP_PASSWORD")}
	status := f.get(c)
	f.mu.Lock()
	f.stacks, f.files, f.during, f.tokens = append(f.stacks, s), append(f.files, names), append(f.during, status), append(f.tokens, c)
	plan, gate := f.plan, f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return tofu.Plan{}, ctx.Err()
		}
	}
	if plan != nil {
		return plan()
	}
	return tofu.Plan{File: []byte("saved"), JSON: []byte(tofuPlanJSON), Log: []byte(`{"type":"diagnostic","diagnostic":{"severity":"warning","summary":"Deprecated","detail":"uses ` + tofuEnvSecret + `"}}` + "\n")}, nil
}

// get reads the state at c.addr, on the test's server, with c's token.
func (f *fakeTofu) get(c stateCall) int {
	u, err := url.Parse(c.addr)
	if err != nil || c.token == "" {
		return 0
	}
	req, _ := http.NewRequest("GET", f.base+u.EscapedPath(), nil)
	req.SetBasicAuth("writ", c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func envValue(env []string, name string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

// tofuFiles is project acme/infra with an OpenTofu stack at infra/app
// that calls a module under infra/modules.
func tofuFiles() map[string]forgetest.File {
	return map[string]forgetest.File{
		"infra/app/main.tf":             {Content: "terraform {\n  backend \"http\" {}\n}\n\nvariable \"canary\" {\n  sensitive = true\n}\n\nmodule \"cert\" {\n  source = \"../modules/cert\"\n}\n"},
		"infra/app/.terraform.lock.hcl": {Content: tofuLockFile},
		"infra/modules/cert/main.tf":    {Content: "resource \"random_id\" \"i\" {\n  byte_length = 8\n}\n"},
	}
}

// tofuFixture is a control plane with the forge, a provider_env file that
// holds tofuEnvSecret, provider_uids, and the stand-in for OpenTofu, or
// none when runner is nil.
func tofuFixture(t *testing.T, runner *fakeTofu) *fixture {
	t.Helper()
	return tofuFixtureUIDs(t, runner, `provider_uids = "61000-61063"`+"\n")
}

// tofuFixtureUIDs is tofuFixture with uids, the config's provider_uids
// and providers_dir, or none when empty.
func tofuFixtureUIDs(t *testing.T, runner *fakeTofu, uids string) *fixture {
	t.Helper()
	return tofuFixtureEdit(t, runner, uids, withForge(t, ""))
}

// tofuFixtureEdit is tofuFixtureUIDs with edit, which adds the forge and
// any other blocks to the config.
func tofuFixtureEdit(t *testing.T, runner *fakeTofu, uids string, edit func(string) string, setup ...func(*server.Options)) *fixture {
	t.Helper()
	return tofuFixtureOn(t, controltest.Default(), runner, uids, edit, setup...)
}

// tofuFixtureOn is tofuFixtureEdit with its control database on e.
func tofuFixtureOn(t *testing.T, e controltest.Engine, runner *fakeTofu, uids string, edit func(string) string, setup ...func(*server.Options)) *fixture {
	t.Helper()
	secretFile := filepath.Join(t.TempDir(), "tf_var")
	if err := os.WriteFile(secretFile, []byte(tofuEnvSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFixtureOn(t, e, func(text string) string {
		text = strings.Replace(text, `  key = "prod.key"`, `  key = "prod.key"
  provider_env = { TF_VAR_extra = "`+secretFile+`" }`, 1)
		return edit(text) + uids
	}, func(o *server.Options) {
		o.Reap = func(uint32) error { return nil }
		if runner != nil {
			o.Tofu = runner
		}
		for _, f := range setup {
			f(o)
		}
	})
	if runner != nil {
		runner.base = f.http.URL
		f.tofu = runner
	}
	f.gitlab.AddProject(4242, "acme/infra", "main")
	return f
}

// planCommit commits files to branch and plans app at that commit as
// the branch's CI job, whose token it returns too.
func (f *fixture) planCommit(files map[string]forgetest.File, branch string) (server.RemotePlan, string) {
	f.t.Helper()
	return f.planCommitStack(files, branch, "app")
}

// planCommitStack is planCommit of another stack.
func (f *fixture) planCommitStack(files map[string]forgetest.File, branch, stack string) (server.RemotePlan, string) {
	f.t.Helper()
	sha := f.gitlab.Commit(4242, branch, files)
	tok := f.token(map[string]any{"sha": sha, "ref_path": "refs/heads/" + branch})
	r := f.do("POST", "/v1/plans", tok, planRequest(func(m map[string]any) {
		delete(m, "bundle")
		m["source"] = map[string]any{}
		m["stacks"] = []string{stack}
	}))
	if r.status != http.StatusAccepted {
		f.t.Fatalf("plan: %d %s", r.status, r.body)
	}
	return f.waitPlan(tok, r.doc["plan"].(map[string]any)["ref"].(string)), tok
}

func TestTofuCommitPlan(t *testing.T) {
	runner := &fakeTofu{}
	f := tofuFixture(t, runner)
	files := tofuFiles()
	f.gitlab.Commit(4242, "main", files)
	r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil))
	var repo server.RepoDoc
	if r.status != http.StatusCreated || json.Unmarshal(r.body, &repo) != nil {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	if len(repo.Stacks) != 1 || repo.Stacks[0].Stack != "app" || !slices.Equal(repo.Stacks[0].Served, []string{"prod"}) || len(repo.Stacks[0].NotServed) != 0 {
		t.Fatalf("discovery did not find the OpenTofu stack served in prod: %+v", repo.Stacks)
	}
	p, _ := f.planCommit(files, "add-cert")
	if p.Plan.Status != "approved" || p.Plan.ID == "" || p.Plan.Stacks[0].Changes != 2 || !slices.Equal(p.Plan.Risks, []string{"create", "replace"}) {
		t.Fatalf("plan %+v\n%s", p.Plan, p.Text)
	}
	for _, want := range []string{
		"OpenTofu 1.13.9 plan of app@prod",
		"  + random_pet.p (create)\n",
		"-/+ module.cert.random_id.i (replace, because of byte_length)\n",
		"Plan: 2 to add, 0 to change, 1 to destroy.\n",
		"Outputs that change: leak.\n",
		"Warning: Deprecated: uses (sensitive)\n",
	} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("the plan's text lacks %q:\n%s", want, p.Text)
		}
	}
	got := f.do("GET", "/v1/plans/"+p.Plan.ID, f.token(nil), nil)
	for _, secret := range []string{tofuCanary, tofuEnvSecret} {
		if strings.Contains(string(got.body), secret) {
			t.Errorf("the plan the API serves holds %s:\n%s", secret, got.body)
		}
	}
	if !strings.Contains(string(got.body), `"resource_changes"`) || !strings.Contains(string(got.body), `"(sensitive)"`) {
		t.Fatalf("the API serves no masked plan document:\n%s", got.body)
	}

	if len(runner.fills) != 1 || string(runner.fills[0]) != tofuLockFile {
		t.Fatalf("the plugin cache was filled with %q, want the stack's lock file", runner.fills)
	}
	s := runner.stacks[0]
	if s.Dir != "app" || s.User == nil || s.User.UID < 61000 || s.User.UID > 61063 || s.User.GID != s.User.UID || !slices.Equal(runner.files[0], []string{"app/.terraform.lock.hcl", "app/main.tf", "modules/cert/main.tf"}) {
		t.Fatalf("the run got dir %q, user %v, files %v", s.Dir, s.User, runner.files[0])
	}
	if envValue(s.Creds, "TF_VAR_extra") != tofuEnvSecret || envValue(s.Env, "TF_VAR_extra") != "" || envValue(s.Env, "HOME") != "" || envValue(s.Env, "TF_HTTP_USERNAME") != "writ" {
		t.Fatalf("the run's environment %v and credentials %v", s.Env, s.Creds)
	}
	if !strings.HasSuffix(runner.tokens[0].addr, "/v1/tfstate/app") {
		t.Fatalf("the run's state address: %s", runner.tokens[0].addr)
	}
	if runner.during[0] != http.StatusNotFound {
		t.Fatalf("the state backend answered the run's token with %d during the plan, want 404 for a stack with no state yet", runner.during[0])
	}
	if after := runner.get(runner.tokens[0]); after != http.StatusUnauthorized {
		t.Fatalf("the state backend answered the run's token with %d after the plan, want 401", after)
	}
	if work, _ := os.ReadDir(filepath.Join(f.cfg.Data, "work")); len(work) != 0 {
		t.Fatalf("the plan's work directory outlived it: %v", work)
	}
}

// TestTofuPlanRefusedByTofu fails a step before the mask has read a plan,
// so it knows only the environment's values and the token. tofuCanary
// stands for a sensitive variable that nonsensitive() put into a
// diagnostic's detail and snippet, which the refused plan must not keep.
func TestTofuPlanRefusedByTofu(t *testing.T) {
	const diagnostics = `{"type":"diagnostic","diagnostic":{"severity":"warning","summary":"Deprecated","detail":"old"}}
{"type":"diagnostic","diagnostic":{"severity":"error","summary":"Invalid value for variable","detail":"the secret ` + tofuCanary + ` is wrong","range":{"filename":"main.tf","start":{"line":3,"column":5}},"snippet":{"code":"  error_message = \"${nonsensitive(var.canary)}\"","values":[{"traversal":"var.canary","statement":"is \"` + tofuCanary + `\""}]}}}
{"type":"diagnostic","diagnostic":{"severity":"error","summary":"Bad ` + tofuEnvSecret + `","detail":""}}
`
	for _, tc := range []struct {
		name      string
		err       *tofu.StepError
		want      []string
		forbidden []string
	}{
		{"json diagnostics", &tofu.StepError{Step: "plan", Code: 1, Stdout: []byte(diagnostics), Stderr: []byte("Error: the secret " + tofuCanary + "\n")},
			[]string{"writ: tofu plan exited with code 1\n", "warning: Deprecated\n", "error: main.tf:3:5: Invalid value for variable\n", "error: Bad (sensitive)\n"},
			[]string{tofuCanary, tofuEnvSecret, "nonsensitive", "is wrong", "old"}},
		{"no json diagnostics", &tofu.StepError{Step: "init", Code: 1, Stdout: []byte("Initializing the backend...\nthe secret " + tofuCanary + "\n"), Stderr: []byte("Error: the secret " + tofuCanary + "\n")},
			[]string{"writ: tofu init exited with code 1\n"},
			[]string{tofuCanary, "Initializing", "Error:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeTofu{plan: func() (tofu.Plan, error) { return tofu.Plan{}, tc.err }}
			f := tofuFixture(t, runner)
			files := tofuFiles()
			f.gitlab.Commit(4242, "main", files)
			if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
				t.Fatalf("add: %d %s", r.status, r.body)
			}
			p, tok := f.planCommit(files, "bad")
			if p.Plan.Status != "refused" || p.OK {
				t.Fatalf("plan %s:\n%s", p.Plan.Status, p.Text)
			}
			for _, want := range tc.want {
				if !strings.Contains(p.Text, want) {
					t.Errorf("the refused plan's text lacks %q:\n%s", want, p.Text)
				}
			}
			if lines := strings.Count(p.Text, "\n"); lines != len(tc.want) {
				t.Errorf("the refused plan's text has %d lines, want %d:\n%s", lines, len(tc.want), p.Text)
			}
			got := f.do("GET", "/v1/plans/"+p.Plan.Ref, tok, nil)
			if got.status != http.StatusOK || !strings.Contains(string(got.body), "exited with code 1") {
				t.Fatalf("GET the refused plan: %d %s", got.status, got.body)
			}
			for _, served := range []string{p.Text, string(got.body)} {
				for _, bad := range tc.forbidden {
					if strings.Contains(served, bad) {
						t.Errorf("the refused plan holds %q:\n%s", bad, served)
					}
				}
			}
			if after := runner.get(runner.tokens[0]); runner.during[0] != http.StatusNotFound || after != http.StatusUnauthorized {
				t.Fatalf("the run's token: %d during the plan and %d after, want 404 and 401", runner.during[0], after)
			}
		})
	}
}

func TestTofuPlanRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(map[string]forgetest.File)
		noTofu bool
		want   string
	}{
		{"no backend", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "resource \"random_pet\" \"p\" {}\n"}
		}, false, "declares no backend"},
		{"an s3 backend with no lock", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"s3\" {\n    bucket = \"b\"\n    use_lockfile = false\n  }\n}\n"}
		}, false, "s3 backend takes no lock: set use_lockfile = true"},
		{"an s3 backend with its own credentials", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"s3\" {\n    use_lockfile = true\n    profile = \"admin\"\n    assume_role {\n      role_arn = \"arn:aws:iam::123456789012:role/x\"\n    }\n  }\n}\n"}
		}, false, "sets assume_role, profile in its s3 backend block"},
		{"an s3 backend with a shared credentials file", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"s3\" {\n    use_lockfile = true\n    shared_credentials_file = \"/etc/aws/credentials\"\n  }\n}\n"}
		}, false, "sets shared_credentials_file in its s3 backend block, which writ serve does not run"},
		{"an s3 backend with a role of its own", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"s3\" {\n    use_lockfile = true\n    role_arn = \"arn:aws:iam::123456789012:role/x\"\n    session_name = \"x\"\n  }\n}\n"}
		}, false, "sets role_arn, session_name in its s3 backend block, which writ serve does not run"},
		{"an s3 backend with a proxy, no TLS check, and an ACL", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"s3\" {\n    use_lockfile = true\n    https_proxy = \"http://proxy.example:3128\"\n    insecure = true\n    acl = \"public-read\"\n  }\n}\n"}
		}, false, "sets acl, https_proxy, insecure in its s3 backend block"},
		{"an s3 backend with a deprecated custom endpoint", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"s3\" {\n    use_lockfile = true\n    endpoint = \"http://127.0.0.1:9000\"\n  }\n}\n"}
		}, false, "sets endpoint in its s3 backend block, which writ serve does not run"},
		{"an s3 backend with an endpoint the env does not list", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"s3\" {\n    use_lockfile = true\n    endpoints = { s3 = \"http://127.0.0.1:9000\" }\n  }\n}\n"}
		}, false, "sets endpoints.s3 = \"http://127.0.0.1:9000\" in its s3 backend block, which env prod's s3_endpoints does not list"},
		{"an s3 backend with a bucket by an expression", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"s3\" {\n    use_lockfile = true\n    bucket = var.bucket\n  }\n}\n"}
		}, false, "sets bucket in its s3 backend block by an expression"},
		{"an s3 backend with endpoints by an expression", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"s3\" {\n    use_lockfile = true\n    endpoints = { s3 = var.endpoint }\n  }\n}\n"}
		}, false, "sets endpoints in its s3 backend block by an expression"},
		{"an s3 backend with an empty dynamodb_table", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"s3\" {\n    use_lockfile = false\n    dynamodb_table = \"\"\n  }\n}\n"}
		}, false, "s3 backend takes no lock: set use_lockfile = true, or a dynamodb_table"},
		{"an s3 backend with use_lockfile by an expression", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"s3\" {\n    use_lockfile = var.lock\n  }\n}\n"}
		}, false, "sets use_lockfile in its s3 backend block by an expression"},
		{"an s3 backend in an env with no plan role", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"s3\" {\n    use_lockfile = true\n  }\n}\n"}
		}, false, "keeps its state in S3, and env prod has no plan_role to reach it with"},
		{"a gcs backend", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"gcs\" {}\n}\n"}
		}, false, `uses backend "gcs", which writ serve does not run`},
		{"an http backend with its own address", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"http\" {\n    address = \"https://elsewhere.example/state\"\n  }\n}\n"}
		}, false, "sets address in its http backend block"},
		{"a cloud block", func(m map[string]forgetest.File) {
			m["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  cloud {}\n}\n"}
		}, false, "declares a cloud block"},
		{"no lock file", func(m map[string]forgetest.File) { delete(m, "infra/app/.terraform.lock.hcl") }, false, "run tofu providers lock"},
		{"no OpenTofu", func(map[string]forgetest.File) {}, true, "runs no OpenTofu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeTofu{}
			if tc.noTofu {
				runner = nil
			}
			f := tofuFixture(t, runner)
			f.gitlab.Commit(4242, "main", tofuFiles())
			if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
				t.Fatalf("add: %d %s", r.status, r.body)
			}
			files := tofuFiles()
			tc.edit(files)
			p, _ := f.planCommit(files, "edit")
			if p.Plan.Status != "failed" || !strings.Contains(p.Text, tc.want) {
				t.Fatalf("plan %s:\n%s\nwant it to fail with %q", p.Plan.Status, p.Text, tc.want)
			}
			if runner != nil && (len(runner.stacks) != 0 || len(runner.fills) != 0) {
				t.Fatal("a refused stack reached OpenTofu")
			}
		})
	}
}

// A team's control plane without provider_uids, whose config check a
// tofu block would fail, still runs no OpenTofu as writ's own user.
func TestTofuPlanWithoutUIDs(t *testing.T) {
	runner := &fakeTofu{}
	f := tofuFixtureUIDs(t, runner, "")
	f.gitlab.Commit(4242, "main", tofuFiles())
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	p, _ := f.planCommit(tofuFiles(), "edit")
	if want := "no provider_uids to run it as"; p.Plan.Status != "failed" || !strings.Contains(p.Text, want) {
		t.Fatalf("plan %s:\n%s\nwant it to fail with %q", p.Plan.Status, p.Text, want)
	}
	if len(runner.stacks) != 0 || len(runner.fills) != 0 {
		t.Fatal("OpenTofu ran as writ's own user")
	}
}

// TestTofuLargeArtifact is a repository whose stack app holds a large
// archive that its code does not read, and whose stack web holds none. The
// archive refuses at most app, so discovery and web's plan go on.
func TestTofuLargeArtifact(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int
		refused bool
	}{
		{"over the limit on one source file", 1<<20 + 1, false},
		{"over the limit on any other file", 16<<20 + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeTofu{}
			f := tofuFixture(t, runner)
			files := tofuFiles()
			files["infra/app/dist/function.zip"] = forgetest.File{Content: strings.Repeat("z", tc.size)}
			files["infra/web/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"http\" {}\n}\n\nresource \"random_pet\" \"p\" {}\n"}
			files["infra/web/.terraform.lock.hcl"] = forgetest.File{Content: tofuLockFile}
			f.gitlab.Commit(4242, "main", files)
			r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil))
			if r.status != http.StatusCreated || !strings.Contains(string(r.body), `"stack":"app"`) || !strings.Contains(string(r.body), `"stack":"web"`) {
				t.Fatalf("add: %d %s", r.status, r.body)
			}
			if p, _ := f.planCommitStack(files, "zip", "web"); p.Plan.Status != "approved" {
				t.Fatalf("plan of web %s:\n%s", p.Plan.Status, p.Text)
			}
			p, _ := f.planCommit(files, "zip")
			if tc.refused {
				if p.Plan.Status != "failed" || !strings.Contains(p.Text, "app/dist/function.zip of commit") || !strings.Contains(p.Text, "is over 16777216 bytes") {
					t.Fatalf("plan of app %s:\n%s", p.Plan.Status, p.Text)
				}
				if len(runner.stacks) != 1 {
					t.Fatalf("OpenTofu ran %d plans, want only web's", len(runner.stacks))
				}
				return
			}
			if p.Plan.Status != "approved" || !slices.Contains(runner.files[1], "app/dist/function.zip") {
				t.Fatalf("plan of app %s with files %v:\n%s", p.Plan.Status, runner.files, p.Text)
			}
		})
	}
}
