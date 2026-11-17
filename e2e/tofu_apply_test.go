//go:build e2e && e2e_tofu && linux

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mr-karan/writ/internal/control/pgtest"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
)

// tofuPGEnv names the Postgres database of the child of
// TestTofuApproveApply, which the parent made.
const tofuPGEnv = "WRIT_E2E_TOFU_PG"

// tofuTeamNamespace adds the subordinate uids to tofuNamespace, so writ
// serve, root of the namespace, can run OpenTofu as a provider uid.
var tofuTeamNamespace = append(slices.Clone(tofuNamespace), "--map-users=auto", "--map-groups=auto")

// TestTofuApproveApply is steps 1, 3, 4, 5, and 6 of the end-to-end test,
// against a team's writ serve:
// a test issuer, a stand-in of GitLab, Postgres, and provider uids, since
// writ serve --dev has one person and approves every plan. A merge
// request job plans stack app by commit, its author cannot approve the
// plan, a maintainer approves it, and the job on main applies it with
// real tofu: state version 1, with an event for each resource, and the
// state index of each resource the state holds. A plan of
// the same files has no changes, and an edit has exactly one update. A
// plan that a newer plan of app@prod replaced is refused, and the newer
// one applies. A plan that only forgets or only imports a resource needs
// an approval too. After app's key changes, the drift check of a
// restarted writ serve and a plan mask the old key, which the state keeps
// only in an address keyed by nonsensitive(). Then neither the control
// database nor the data directory holds the canary or either key.
//
// The parent starts Postgres on a Unix socket, which the child, in its
// own network namespace, reaches through the socket's directory.
func TestTofuApproveApply(t *testing.T) {
	if os.Getenv(tofuChildEnv) != "" {
		tofuApplyChild(t)
		return
	}
	base, stop, err := pgtest.Start()
	if err != nil {
		t.Fatalf("this test needs Postgres's initdb, pg_ctl, and createdb on PATH, as just e2e gives: %v", err)
	}
	t.Cleanup(stop)
	inTofuNamespace(t, "TestTofuApproveApply", tofuTeamNamespace, tofuPGEnv+"="+pgtest.Database(t, base))
}

// tofuAppFiles is repository infra with stack app on the http backend,
// whose terraform_data.note holds note, and the module app calls.
func tofuAppFiles(lock []byte, note string) map[string]forgetest.File {
	return map[string]forgetest.File{
		"infra/app/.terraform.lock.hcl": {Content: string(lock)},
		"infra/app/main.tf": {Content: "terraform {\n  backend \"http\" {}\n" + tofuProviders + `}

variable "canary" {
  type      = string
  sensitive = true
}

resource "random_pet" "name" {
  keepers = { canary = var.canary }
}

resource "terraform_data" "secret" {
  input = base64encode(var.canary)
}

resource "terraform_data" "note" {
  input = "` + note + `"
}

module "cert" {
  source = "../modules/cert"
}

output "pet" {
  value = random_pet.name.id
}
`},
		"infra/modules/cert/main.tf": {Content: `terraform {
  required_providers {
    tls = { source = "hashicorp/tls", version = "4.4.2" }
  }
}

resource "tls_private_key" "key" {
  algorithm = "ED25519"
}
`},
	}
}

// tofuRun is writ.remote-run/v0 with its document read as
// writ.tofu-apply/v0.
type tofuRun struct {
	OK       bool              `json:"ok"`
	Run      server.RunInfo    `json:"run"`
	Document *server.TofuApply `json:"document"`
	Text     string            `json:"text"`
}

func tofuApplyChild(t *testing.T) {
	db := os.Getenv(tofuPGEnv)
	u, err := url.Parse(db)
	if err != nil || u.Query().Get("host") == "" {
		t.Fatalf("the database %q names no socket directory", db)
	}
	lock := tofuLock(t)
	bin, shared := tofuChildRoot(t, u.Query().Get("host"))

	// A provider uid reads the plugin cache and makes its run's directory
	// in the scratch directory: neither may sit under a directory only
	// root of the namespace can enter, as t.TempDir's are.
	top, err := os.MkdirTemp("/tmp", "writ-tofu-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(top) })
	cache, scratch := filepath.Join(top, "plugins"), filepath.Join(top, "runs")
	if err := os.CopyFS(cache, os.DirFS(shared)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	for d, mode := range map[string]os.FileMode{top: 0o755, scratch: os.ModeSticky | 0o777} {
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
	}

	is := oidctest.New(t)
	g := forgetest.Handler(forgeToken)
	g.URL = is.URL
	is.Mount("/api/v4/", g)
	data := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(data, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ca := write("issuer.pem", string(is.CAPEM()))
	tokenFile := write("secrets/gitlab_read", forgeToken+"\n")
	// The canary's file is the team's secret, outside data, which the scan
	// reads.
	canaryFile := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(canaryFile, []byte(tofuCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(tofuKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// People sign in with an issuer like GitLab.com's: no device flow, and
	// the loopback redirect URI of writ login registered at the
	// application.
	is.SetClient("writ-e2e", servedClientSecret)
	is.SetPerson(oidctest.GitLabPerson())
	is.SetNoDevice(true)
	clientSecretFile := filepath.Join(t.TempDir(), "oidc_client_secret")
	if err := os.WriteFile(clientSecretFile, []byte(servedClientSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	srvURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	is.RegisterRedirects(srvURL+"/ui/callback", "http://127.0.0.1/callback")
	cfg := write("serve", fmt.Sprintf(`url           = %[1]q
listen        = "127.0.0.1:%[2]d"
database      = %[3]q
data          = %[4]q
provider_uids = "61000-61063"

limits {
  requests_per_second  = 100
  requests_per_address = 1000
}

tofu {
  binary       = %[5]q
  plugin_cache = %[6]q
  scratch_dir  = %[7]q
}

issuer "gitlab" {
  kind     = "gitlab-ci"
  identity = "gitlab"
  url      = %[8]q
  ca_file  = %[9]q
}

issuer "sso" {
  kind               = "people"
  identity           = "gitlab"
  url                = %[8]q
  ca_file            = %[9]q
  client_id          = "writ-e2e"
  client_secret_file = %[13]q
  groups             = "userinfo"
}

forge "gitlab" {
  kind            = "gitlab"
  url             = %[8]q
  read_token_file = %[10]q
  ca_file         = %[9]q
}

repo {
  name    = "infra"
  forge   = "gitlab"
  project = "acme/infra"
  path    = "infra"
}

team "infra_mr" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], pipeline_source = ["merge_request_event"] }
}

team "infra_reviewers" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], user_access_level = ["maintainer", "owner"] }
}

team "infra_main" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], ref_path = ["refs/heads/main"], ref_protected = ["true"] }
}

team "acme" {
  issuer = "sso"
  match  = { groups = ["acme"] }
}

grant {
  teams  = ["infra_mr"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "plan"]
}

grant {
  teams  = ["infra_reviewers"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["approve"]
}

grant {
  teams  = ["infra_main"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "apply"]
  risks  = ["create", "update", "replace", "delete"]
}

grant {
  teams  = ["acme"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read"]
}

approval {
  stacks = ["*"]
  envs   = ["prod"]
  risks  = ["create", "update", "replace", "delete", "data-loss"]
  need   = 1
}

env "prod" {
  key          = "keys/prod.key"
  provider_env = { TF_VAR_canary = %[11]q, TF_VAR_key = %[12]q }
}

`, srvURL, port, db, data, bin, cache, scratch, is.URL, ca, tokenFile, canaryFile, keyFile, clientSecretFile))
	g.AddProject(4242, "acme/infra", "main")
	// writ serve finds stack app on main when it reads the repository.
	v1 := tofuAppFiles(lock, "v1")
	g.Commit(4242, "main", v1)
	jobDir := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &served{t: t, is: is, url: srvURL, db: db, data: data, log: &syncBuffer{}, stack: &stackDir{t: t, dir: jobDir}}
	if r := s.stack.exec(nil, "serve", "--config", cfg, "--check"); r.code != 0 {
		t.Fatalf("writ serve --check: %s", r)
	}
	stop := serveAgain(t, s, cfg)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if code, doc := s.api("GET", "/v1/repos/infra", s.token(nil), nil); code == 200 && doc["commit"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pinned repository was not read:\n%s", s.log.String())
		}
	}
	// bob signs in from a laptop in a browser, since the issuer offers no
	// device flow, and gets the session the device flow would give.
	laptop := []string{"XDG_CONFIG_HOME=" + t.TempDir(), "WRIT_SERVER=", "WRIT_ID_TOKEN="}
	out := browserSignIn(t, s, laptop)
	if !strings.Contains(out, "Signed in to "+srvURL+" as bob (gitlab/user/2002).\nTeams: acme\n") {
		t.Fatalf("writ login printed:\n%s", out)
	}
	t.Logf("writ login in a browser:\n%s", out)
	whoami := s.stack.exec(laptop, "whoami")
	s.outsMu.Lock()
	s.outs = append(s.outs, whoami.stdout, whoami.stderr)
	s.outsMu.Unlock()
	t.Logf("writ whoami:\n%s", whoami.stdout)
	if whoami.code != 0 || !strings.Contains(whoami.stdout, "session of bob (gitlab/user/2002, issuer sso)") || !regexp.MustCompile(`(?m)^prod +app +read +-$`).MatchString(whoami.stdout) {
		t.Fatalf("writ whoami after a browser sign-in:\n%s", whoami)
	}
	session := sessionTokenIn(t, laptop[0][len("XDG_CONFIG_HOME="):])

	// Onboarding: the team sees stack app served in prod and writes the CI
	// jobs that plan, approve, and apply it.
	if r := s.writ(0, s.token(nil), "repo", "show", "infra"); !regexp.MustCompile(`(?m)^app +prod$`).MatchString(r.stdout) {
		t.Fatalf("writ repo show does not serve app in prod:\n%s", r)
	}
	ci := s.writ(0, s.token(nil), "ci", "gitlab", "--repo", "infra", "--image", "registry.example.com/writ:0.5.0")
	for _, want := range []string{
		"  - component: $CI_SERVER_FQDN/platform/writ-ci/writ-plan@0.1.0\n    inputs:\n      server: " + srvURL + "\n      env: prod\n      stack: app\n      image: registry.example.com/writ:0.5.0\n",
		"  - component: $CI_SERVER_FQDN/platform/writ-ci/writ-approve@0.1.0\n",
		"  - component: $CI_SERVER_FQDN/platform/writ-ci/writ-apply@0.1.0\n    inputs:\n      server: " + srvURL + "\n      env: prod\n      stack: app\n",
		"grant {\n  teams  = [\"infra_main\"]\n  stacks = [\"app\"]\n  envs   = [\"prod\"]\n  can    = [\"read\", \"apply\"]\n",
	} {
		if !strings.Contains(ci.stdout, want) {
			t.Fatalf("writ ci gitlab lacks %q:\n%s", want, ci)
		}
	}
	t.Logf("writ ci gitlab:\n%s", ci.stdout)

	pipeline := 1849400
	// pipelines are the pipeline of each plan, whose jobs alone may
	// approve it.
	pipelines := map[string]string{}
	// plan has a merge request job plan app at a new commit of branch with
	// files, and returns the plan.
	plan := func(branch string, files map[string]forgetest.File) tofuPlanDoc {
		t.Helper()
		head := g.Commit(4242, branch, files)
		pipeline++
		tok := s.token(map[string]any{"sha": head, "ref": branch, "ref_path": "refs/heads/" + branch, "pipeline_id": fmt.Sprint(pipeline)})
		var p tofuPlanDoc
		r := s.writ(0, tok, "plan", "--remote", "--json", "--env", "prod", "--stack", "app", "--commit", head)
		if err := json.Unmarshal([]byte(r.stdout), &p); err != nil || !p.OK || p.Plan.ID == "" || p.Plan.Source.Commit != head {
			t.Fatalf("the plan of app at %s: %v\n%s", branch, err, r)
		}
		t.Logf("writ plan --remote of app at %s:\n%s", branch, p.Text)
		pipelines[p.Plan.ID] = fmt.Sprint(pipeline)
		return p
	}
	approve := func(id string) {
		t.Helper()
		bob := s.token(map[string]any{"user_id": "2002", "user_login": "bob", "user_access_level": "maintainer", "pipeline_id": pipelines[id]})
		r := s.writ(0, bob, "approve", "--remote", id)
		if !strings.Contains(r.stdout, ", approved.") {
			t.Fatalf("bob's approval of %s:\n%s", id, r)
		}
		t.Logf("writ approve --remote by bob:\n%s", r.stdout)
	}
	mainToken := func(sha string) string {
		pipeline++
		return s.token(map[string]any{"sha": sha, "ref": "main", "ref_path": "refs/heads/main", "ref_protected": "true",
			"pipeline_source": "push", "pipeline_id": fmt.Sprint(pipeline)})
	}
	// merge commits files to main and has the job on main apply the plan
	// approved for them.
	merge := func(want int, files map[string]forgetest.File) result {
		t.Helper()
		merged := g.Commit(4242, "main", files)
		return s.writ(want, mainToken(merged), "apply", "--remote", "--merged", "--env", "prod", "--stack", "app")
	}
	applied := func(files map[string]forgetest.File) tofuRun {
		t.Helper()
		merged := g.Commit(4242, "main", files)
		var run tofuRun
		r := s.writ(0, mainToken(merged), "apply", "--remote", "--json", "--merged", "--env", "prod", "--stack", "app")
		if err := json.Unmarshal([]byte(r.stdout), &run); err != nil || run.Document == nil {
			t.Fatalf("the apply after the merge: %v\n%s", err, r)
		}
		t.Logf("writ apply --remote --merged:\n%s", run.Text)
		return run
	}
	actions := func(p tofuPlanDoc) map[string]string {
		out := map[string]string{}
		for _, c := range p.Document.ResourceChanges {
			out[c.Address] = strings.Join(c.Change.Actions, ",")
		}
		return out
	}

	// Step 1: a plan by commit, masked.
	p := plan("add-app", v1)
	want := map[string]string{"random_pet.name": "create", "terraform_data.secret": "create", "terraform_data.note": "create", "module.cert.tls_private_key.key": "create"}
	if got := actions(p); p.Plan.Status != "pending" || p.Plan.Stacks[0].Changes != 4 || !maps.Equal(got, want) {
		t.Fatalf("the first plan: %+v, changes %v", p.Plan, got)
	}
	for _, line := range []string{"OpenTofu 1.13.9 plan of app@prod\n", "Plan: 4 to add, 0 to change, 0 to destroy.\n"} {
		if !strings.Contains(p.Text, line) {
			t.Errorf("the first plan's text lacks %q:\n%s", line, p.Text)
		}
	}
	for _, c := range p.Document.ResourceChanges {
		var masked any
		switch c.Address {
		case "random_pet.name":
			keepers, _ := c.Change.After["keepers"].(map[string]any)
			masked = keepers["canary"]
		case "terraform_data.secret":
			masked = c.Change.After["input"]
		default:
			continue
		}
		if fmt.Sprint(masked) != "(sensitive)" {
			t.Errorf("the first plan: %s keeps %v where the canary went, want (sensitive)", c.Address, masked)
		}
	}

	// Step 3: the author cannot approve, a second person approves, and the
	// job on main applies.
	if r := s.writ(1, s.token(map[string]any{"user_access_level": "maintainer", "pipeline_id": pipelines[p.Plan.ID]}), "approve", "--remote", p.Plan.ID); !strings.Contains(r.stdout, "cannot approve it") {
		t.Fatalf("the author's approval:\n%s", r)
	} else {
		t.Logf("writ approve --remote by the plan's author:\n%s", r.stdout+r.stderr)
	}
	if r := merge(1, v1); !strings.Contains(r.stdout, "approve it with writ approve --remote "+p.Plan.ID) {
		t.Fatalf("an apply with no approval:\n%s", r)
	} else {
		t.Logf("writ apply --remote --merged before the approval:\n%s", r.stdout+r.stderr)
	}
	approve(p.Plan.ID)
	run := applied(v1)
	doc := run.Document
	if !run.OK || run.Run.Status != "applied" || run.Run.Plan != p.Plan.ID || len(run.Run.ApprovedBy) != 1 || !strings.Contains(run.Run.ApprovedBy[0].Display, "bob") {
		t.Fatalf("the first apply: %+v\n%s", run.Run, run.Text)
	}
	if doc.Format != server.FormatTofuApply || !doc.Applied || doc.StateVersion != 1 || doc.Summary == nil || *doc.Summary != (server.TofuApplySummary{Add: 4}) {
		t.Fatalf("the first apply's document: %+v", doc)
	}
	completed := map[string]string{}
	for _, ev := range doc.Events {
		if ev.Type == "apply_complete" {
			completed[ev.Address] = ev.Action
		}
	}
	if !maps.Equal(completed, want) {
		t.Fatalf("the first apply completed %v, want %v; events %+v", completed, want, doc.Events)
	}
	conn, err := pgx.Connect(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	// Each call of OpenTofu's http backend leaves an allowed audit row of
	// its run.
	stateRows := func(run string) map[string]int {
		t.Helper()
		rows, err := conn.Query(context.Background(), "SELECT op, decision FROM writ_control.audit WHERE run = $1 AND route LIKE '/v1/tfstate/%'", run)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var op, decision string
			if err := rows.Scan(&op, &decision); err != nil {
				t.Fatal(err)
			}
			if decision != "allowed" {
				t.Errorf("run %s: a %s of state was %s", run, op, decision)
			}
			out[op]++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	planned, applying := stateRows(p.Plan.Ref), stateRows(run.Run.ID)
	t.Logf("audit rows of the state backend: the first plan %v, its apply %v", planned, applying)
	if len(planned) != 1 || planned["state-read"] == 0 {
		t.Errorf("the first plan's state requests: %v, want reads only", planned)
	}
	for _, op := range []string{"state-lock", "state-read", "state-write", "state-unlock"} {
		if applying[op] == 0 {
			t.Errorf("the first apply's state requests: %v, want a %s", applying, op)
		}
	}
	// The index holds each resource the apply made, with its id, the one
	// attribute of the allow-list these types have.
	index := func(version int64) {
		t.Helper()
		var got int64
		if err := conn.QueryRow(context.Background(), "SELECT version FROM writ_control.state_index_stacks WHERE env = 'prod' AND stack = 'app'").Scan(&got); err != nil || got != version {
			t.Fatalf("the state index of app@prod is of state version %d, want %d: %v", got, version, err)
		}
		rows, err := conn.Query(context.Background(), `SELECT address, type, provider, id = '', arn || identifier || region || namespace || modify_index
			FROM writ_control.state_index WHERE env = 'prod' AND stack = 'app' ORDER BY address`)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for rows.Next() {
			var address, typ, provider, rest string
			var noID bool
			if err := rows.Scan(&address, &typ, &provider, &noID, &rest); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, fmt.Sprintf("%s %s %s id:%t %q", address, typ, provider, !noID, rest))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		wantRows := []string{
			`module.cert.tls_private_key.key tls_private_key registry.opentofu.org/hashicorp/tls id:true ""`,
			`random_pet.name random_pet registry.opentofu.org/hashicorp/random id:true ""`,
			`terraform_data.note terraform_data terraform.io/builtin/terraform id:true ""`,
			`terraform_data.secret terraform_data terraform.io/builtin/terraform id:true ""`,
		}
		if !slices.Equal(lines, wantRows) {
			t.Fatalf("the state index of app@prod:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(wantRows, "\n"))
		}
	}
	index(1)
	// writ runs --remote lists the apply.
	if r := s.writ(0, s.token(nil), "runs", "--remote", "--json", "--env", "prod", "--stack", "app"); !strings.Contains(r.stdout, `"id":"`+run.Run.ID+`"`) ||
		!strings.Contains(r.stdout, `"plan":"`+p.Plan.ID+`"`) {
		t.Fatalf("writ runs --remote after the first apply:\n%s", r)
	}

	// Step 4: the same files plan no changes, and an edit plans exactly one
	// update.
	if again := plan("again", v1); again.Plan.Stacks[0].Changes != 0 || !strings.Contains(again.Text, "No changes.") {
		t.Fatalf("the plan of the applied files: %+v, changes %v\n%s", again.Plan, actions(again), again.Text)
	}
	v2 := tofuAppFiles(lock, "v2")
	edit := plan("edit", v2)
	if got := actions(edit); edit.Plan.Stacks[0].Changes != 1 || got["terraform_data.note"] != "update" || len(nonNoop(got)) != 1 {
		t.Fatalf("the plan of the edit: %+v, changes %v", edit.Plan, got)
	}
	approve(edit.Plan.ID)

	// Step 5: a newer plan of app@prod makes the edit's plan stale.
	v3 := tofuAppFiles(lock, "v3")
	newer := plan("edit-again", v3)
	approve(newer.Plan.ID)
	r := merge(1, v2)
	if !strings.Contains(r.stdout, "plan "+edit.Plan.ID+" is stale: plan "+newer.Plan.ID+" of app@prod is newer") {
		t.Fatalf("the apply of the stale plan:\n%s", r)
	}
	t.Logf("writ apply --remote --merged of a stale plan:\n%s", r.stdout+r.stderr)
	run = applied(v3)
	if run.Run.Status != "applied" || run.Run.Plan != newer.Plan.ID || run.Document.StateVersion != 2 || run.Document.Summary == nil || *run.Document.Summary != (server.TofuApplySummary{Change: 1}) {
		t.Fatalf("the apply of the newer plan: %+v %+v\n%s", run.Run, run.Document, run.Text)
	}
	index(2)

	// A plan that only forgets a resource, as removed { lifecycle {
	// destroy = false } } does, or only imports one, has the risk class
	// of a delete or a create, and needs its approval before it applies.
	forget := tofuAppFiles(lock, "v3")
	forget["infra/app/main.tf"] = forgetest.File{Content: strings.Replace(forget["infra/app/main.tf"].Content, `resource "terraform_data" "note" {
  input = "v3"
}`, `removed {
  from = terraform_data.note
  lifecycle {
    destroy = false
  }
}`, 1)}
	imported := tofuAppFiles(lock, "v3")
	imported["infra/app/main.tf"] = forgetest.File{Content: imported["infra/app/main.tf"].Content + `
import {
  to = random_string.imported
  id = "abcdefgh"
}

resource "random_string" "imported" {
  length = 8
}
`}
	for _, tc := range []struct {
		branch, addr, action, risk, text string
		files                            map[string]forgetest.File
	}{
		{"forget-note", "terraform_data.note", "forget", "delete", "Plan: 0 to add, 0 to change, 0 to destroy, 1 to forget.\n", forget},
		{"import-string", "random_string.imported", "no-op", "create", "Plan: 1 to import, 0 to add, 0 to change, 0 to destroy.\n", imported},
	} {
		p := plan(tc.branch, tc.files)
		if got := actions(p); got[tc.addr] != tc.action || p.Plan.Stacks[0].Changes != 1 || p.Plan.Status != "pending" || p.Plan.Approvals.Need != 1 || !slices.Equal(p.Plan.Risks, []string{tc.risk}) || !strings.Contains(p.Text, tc.text) {
			t.Fatalf("the plan at %s: %+v, changes %v\n%s", tc.branch, p.Plan, got, p.Text)
		}
		if r := merge(1, tc.files); !strings.Contains(r.stdout, "approve it with writ approve --remote "+p.Plan.ID) {
			t.Fatalf("an apply of the plan at %s with no approval:\n%s", tc.branch, r)
		}
	}

	// A past secret: app gains a resource keyed by nonsensitive(var.key),
	// applied while the team's key is tofuKey, which no other resource
	// holds. Then the key changes, and writ serve restarts with drift
	// checks: the state keeps tofuKey in that resource's address with no
	// mark, and no input of the next plan names it. The drift check at
	// start and a plan each mask it with the secret set sealed with state
	// version 3, which they read.
	v4 := tofuAppFiles(lock, "v3")
	v4["infra/app/main.tf"] = forgetest.File{Content: v4["infra/app/main.tf"].Content + `
variable "key" {
  type      = string
  sensitive = true
}

resource "terraform_data" "keyed" {
  for_each = toset([nonsensitive(var.key)])
  input    = true
}
`}
	keyed := plan("keyed", v4)
	if got := actions(keyed); got[`terraform_data.keyed["(sensitive)"]`] != "create" || len(nonNoop(got)) != 1 {
		t.Fatalf("the plan of the keyed resource: %+v, changes %v", keyed.Plan, got)
	}
	approve(keyed.Plan.ID)
	if run := applied(v4); run.Run.Status != "applied" || run.Document.StateVersion != 3 {
		t.Fatalf("the apply of the keyed resource: %+v %+v\n%s", run.Run, run.Document, run.Text)
	}
	if err := os.WriteFile(keyFile, []byte(tofuKeyRotated+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop()
	cfgFile, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfgFile.WriteString("\ndrift {\n  every = \"1h\"\n}\n"); err != nil {
		t.Fatal(err)
	}
	if err := cfgFile.Close(); err != nil {
		t.Fatal(err)
	}
	serveAgain(t, s, cfg)
	keyedGone := `terraform_data.keyed["(sensitive)"] delete`
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
		code, doc := s.api("GET", "/v1/drift?env=prod&stack=app", s.token(nil), nil)
		b, _ := json.Marshal(doc)
		var d server.DriftDoc
		if code == 200 && json.Unmarshal(b, &d) == nil && len(d.Stacks) == 1 && !d.Stacks[0].Checking && d.Stacks[0].Check != nil {
			c := d.Stacks[0].Check
			var changes []string
			for _, ch := range c.Changes {
				changes = append(changes, ch.Address+" "+ch.Action)
			}
			if c.Status != "drifted" || !slices.Contains(changes, keyedGone) {
				t.Fatalf("the drift check after the key changed: %s, want drifted with %s", b, keyedGone)
			}
			t.Logf("the drift check after the key changed: %s", b)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no drift check of app@prod ended: %d %s\n%s", code, b, s.log.String())
		}
	}
	rotated := plan("rotated", v4)
	for _, line := range []string{`  - terraform_data.keyed["(sensitive)"] (delete)` + "\n", `  + terraform_data.keyed["(sensitive)"] (create)` + "\n"} {
		if !strings.Contains(rotated.Text, line) {
			t.Errorf("the plan after the key changed lacks %q:\n%s", line, rotated.Text)
		}
	}

	// Step 6: no state token outlives its run, and neither the canary nor
	// either key is in any row, file, output, or log line.
	var tokens, versions int
	if err := conn.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM writ_control.tfstate_tokens), (SELECT count(*) FROM writ_control.tfstate_versions WHERE stack = 'app')").Scan(&tokens, &versions); err != nil || tokens != 0 || versions != 3 {
		t.Fatalf("the control database holds %d state tokens and %d versions of app's state: %v", tokens, versions, err)
	}
	dump := dumpControl(t, conn)
	s.outsMu.Lock()
	outs := append(slices.Clone(s.outs), dump...)
	s.outsMu.Unlock()
	scanForCanary(t, data, outs, s.log.String(), tofuKey, tofuKeyRotated, session, servedClientSecret)
}

// dumpControl is every row of every table of the control database, as
// text.
func dumpControl(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	ctx := context.Background()
	rows, err := conn.Query(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = 'writ_control'")
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tables) == 0 {
		t.Fatalf("the control database's tables %v: %v", tables, err)
	}
	var out []string
	for _, table := range tables {
		rows, err := conn.Query(ctx, "SELECT t::text FROM writ_control."+pgx.Identifier{table}.Sanitize()+" t")
		if err != nil {
			t.Fatal(err)
		}
		texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, texts...)
	}
	return out
}

// nonNoop is the addresses of changes whose actions are not no-op.
func nonNoop(actions map[string]string) []string {
	var out []string
	for a, act := range actions {
		if act != "no-op" {
			out = append(out, a)
		}
	}
	return out
}
