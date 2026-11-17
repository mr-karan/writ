package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/server"
)

// gitlabToken is the forge's read token. No response, log line, or audit
// row may hold it.
const gitlabToken = "glpat-SERVERTEST-read-0123456789"

var issuerRE = regexp.MustCompile(`url = "(https://[^"]+)"\n  ca_file = "([^"]+)"`)

// withForge adds the forge gitlab, at the issuer's own host, and more
// config text, and gives the reviewers team admin.
func withForge(t *testing.T, more string) func(string) string {
	tokenFile := filepath.Join(t.TempDir(), "gitlab_read")
	if err := os.WriteFile(tokenFile, []byte(gitlabToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(text string) string {
		m := issuerRE.FindStringSubmatch(text)
		text += fmt.Sprintf(`
forge "gitlab" {
  kind = "gitlab"
  url = %q
  read_token_file = %q
  ca_file = %q
}

limits {
  requests_per_second = 1000
  requests_per_address = 1000
}

%s

# Any merge request pipeline may plan, so the control plane's own check
# of the project, not a team rule, refuses a fork's.
team "any_mr" {
  issuer = "gitlab"
  match = { pipeline_source = ["merge_request_event"] }
}
`, m[1], tokenFile, m[2], more)
		return strings.Replace(text, "grant {", `grant {
  teams = ["reviewers"]
  stacks = `+everyStack+`
  envs = ["*"]
  can = ["admin", "read"]
}

grant {
  teams = ["any_mr"]
  stacks = ["*"]
  envs = ["prod"]
  can = ["plan"]
}

grant {`, 1)
	}
}

// everyStack is the stacks list that matches every stack name.
const everyStack = `["**"]`

// infraFiles is project acme/infra: two OpenTofu stacks at infra, network,
// which calls the local module at platform, and database.
func infraFiles() map[string]forgetest.File {
	return map[string]forgetest.File{
		"README.md":                          {Content: "not read\n"},
		"infra/platform/main.tf":             {Content: "output \"region\" {\n  value = \"ap-south-1\"\n}\n"},
		"infra/network/main.tf":              {Content: tofuBackend + "module \"platform\" {\n  source = \"../platform\"\n}\n\nresource \"random_pet\" \"pet\" {}\n"},
		"infra/network/.terraform.lock.hcl":  {Content: tofuLockFile},
		"infra/database/main.tf":             {Content: tofuBackend + "resource \"random_id\" \"id\" {\n  byte_length = 4\n}\n"},
		"infra/database/.terraform.lock.hcl": {Content: tofuLockFile},
		"infra/database/schema.sql":          {Content: "CREATE TABLE t (id int);\n"},
	}
}

// tofuStack is the files of an OpenTofu stack in dir.
func tofuStack(dir string) map[string]forgetest.File {
	return map[string]forgetest.File{dir + "/main.tf": {Content: tofuBackend + "resource \"random_pet\" \"x\" {}\n"}, dir + "/.terraform.lock.hcl": {Content: tofuLockFile}}
}

// tofuBackend is the terraform block of a stack whose state writ serve
// keeps.
const tofuBackend = "terraform {\n  backend \"http\" {}\n}\n\n"

func repoRequest(edit func(map[string]any)) *bytes.Reader {
	req := map[string]any{"format": "writ.repo-request/v0", "name": "infra", "forge": "gitlab", "project": "acme/infra", "path": "infra", "prefix": ""}
	if edit != nil {
		edit(req)
	}
	b, _ := json.Marshal(req)
	return bytes.NewReader(b)
}

func admin(f *fixture) string { return f.token(map[string]any{"user_access_level": "maintainer"}) }

func blobCalls(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.HasSuffix(c, "/raw") {
			n++
		}
	}
	return n
}

// logTo captures the server's log.
func logTo(b io.Writer) func(*server.Options) {
	return func(o *server.Options) {
		o.Log = slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
}

func TestRepoAdd(t *testing.T) {
	var logs bytes.Buffer
	f := newFixture(t, withForge(t, ""), logTo(&logs))
	f.gitlab.AddProject(4242, "acme/infra", "main")
	head := f.gitlab.Commit(4242, "main", infraFiles())
	var bodies []string
	do := func(method, path, tok string, body *bytes.Reader) response {
		var r response
		if body == nil {
			r = f.do(method, path, tok, nil)
		} else {
			r = f.do(method, path, tok, body)
		}
		bodies = append(bodies, string(r.body))
		return r
	}

	// The team that plans may not add a repository: that takes admin.
	if r := do("POST", "/v1/repos", f.token(nil), repoRequest(nil)); r.status != http.StatusForbidden || r.class() != "denied" {
		t.Fatalf("a token without admin: %d %s", r.status, r.body)
	}
	f.gitlab.Calls()
	r := do("POST", "/v1/repos", admin(f), repoRequest(nil))
	if r.status != http.StatusCreated {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	var doc server.RepoDoc
	if err := json.Unmarshal(r.body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Repo.ProjectID != "4242" || doc.Repo.DefaultBranch != "main" || doc.Repo.Head != head || doc.Commit == nil || doc.Commit.Files != 2 {
		t.Fatalf("repo %+v commit %+v", doc.Repo, doc.Commit)
	}
	if n := blobCalls(f.gitlab.Calls()); n != 1 {
		// Discovery reads the two lock files, which are one blob.
		t.Fatalf("fetched %d blobs", n)
	}
	if len(doc.Stacks) != 2 || doc.Stacks[0].Stack != "database" || doc.Stacks[1].Stack != "network" {
		t.Fatalf("stacks %+v", doc.Stacks)
	}
	net := doc.Stacks[1]
	if net.Dir != "infra/network" || len(net.Providers) != 1 || net.Providers[0].Source != "registry.opentofu.org/hashicorp/random" || net.Providers[0].Version != "3.9.1" {
		t.Fatalf("network %+v", net)
	}
	// This config has no tofu block, so it plans no OpenTofu stack in prod.
	if len(net.Served) != 0 || !slices.Equal(net.NotServed, []string{"prod"}) || net.Providers[0].Runs {
		t.Fatalf("network is served in %v and not in %v; its provider runs: %v", net.Served, net.NotServed, net.Providers[0].Runs)
	}

	if r := do("GET", "/v1/repos", f.token(nil), nil); r.status != 200 || !strings.Contains(string(r.body), `"stacks":2`) || !strings.Contains(string(r.body), `"origin":"api"`) {
		t.Fatalf("list: %d %s", r.status, r.body)
	}
	if r := do("GET", "/v1/repos/infra", f.token(nil), nil); r.status != 200 || !strings.Contains(string(r.body), `"stack":"network"`) {
		t.Fatalf("get: %d %s", r.status, r.body)
	}
	if r := do("GET", "/v1/config", f.token(nil), nil); r.status != 200 || !strings.Contains(string(r.body), `"forges":[{"name":"gitlab","kind":"gitlab","url":"`+f.is.URL+`"}]`) || strings.Contains(string(r.body), "gitlab_read") {
		t.Fatalf("the config document's forges: %d %s", r.status, r.body)
	}
	if r := do("GET", "/v1/repos/nope", f.token(nil), nil); r.status != http.StatusNotFound {
		t.Fatalf("get a missing repo: %d %s", r.status, r.body)
	}

	// Refusals.
	f.gitlab.AddProject(77, "acme/payments", "main")
	f.gitlab.Commit(77, "main", tofuStack("network"))
	f.gitlab.AddProject(76, "acme/legacy", "main")
	legacy := tofuStack("web")
	legacy["web/main.writ"], legacy["db/main.writ"] = forgetest.File{Content: "let x = 1\n"}, forgetest.File{Content: "let y = 2\n"}
	f.gitlab.Commit(76, "main", legacy)
	for _, tc := range []struct {
		name   string
		edit   func(map[string]any)
		status int
		want   string
	}{
		{"same name", nil, 409, "a repository named infra exists"},
		{"same project and path", func(m map[string]any) {
			m["name"] = "infra2"
			m["forge"] = "127.0.0.1" + f.is.URL[len("https://127.0.0.1"):]
		}, 409, "repository infra already reads acme/infra"},
		{"no such forge", func(m map[string]any) { m["forge"] = "github" }, 400, `no forge is named`},
		{"no such project", func(m map[string]any) { m["project"] = "acme/nope" }, 404, "read project acme/nope: the forge answered 404"},
		{"no stack at the path", func(m map[string]any) { m["name"] = "other"; m["path"] = "infra/platform" }, 400, "holds no OpenTofu stack"},
		{"code of Writ's retired language", func(m map[string]any) { m["name"] = "legacy"; m["project"] = "acme/legacy"; m["path"] = "" }, 400, "holds .writ files, the code of Writ's retired language, which writ serve no longer plans: db/main.writ, web/main.writ"},
		{"a token in the request", func(m map[string]any) { m["token"] = "glpat-x" }, 400, `unknown field`},
		{"a path that leaves", func(m map[string]any) { m["path"] = "../x" }, 400, "clean relative path"},
		{"a dir repository", func(m map[string]any) { m["forge"] = "dir"; m["project"] = "/srv/infra" }, 400, "only writ serve --dev reads"},
		{"a stack name another repository holds", func(m map[string]any) { m["name"] = "payments"; m["project"] = "acme/payments"; m["path"] = "" }, 409, "set a prefix, such as payments/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := do("POST", "/v1/repos", admin(f), repoRequest(tc.edit))
			if r.status != tc.status || !strings.Contains(string(r.body), tc.want) {
				t.Fatalf("got %d %s", r.status, r.body)
			}
		})
	}
	if r := do("POST", "/v1/repos", admin(f), repoRequest(func(m map[string]any) {
		m["name"], m["project"], m["path"], m["prefix"] = "payments", "77", "", "payments/"
	})); r.status != http.StatusCreated || !strings.Contains(string(r.body), `"stack":"payments/network"`) {
		t.Fatalf("with a prefix: %d %s", r.status, r.body)
	}

	// A prefix may not start another repository's prefix, or be the start of
	// one, and the refusal names the other repository.
	for _, id := range []int{78, 79} {
		f.gitlab.AddProject(id, fmt.Sprintf("acme/nest%d", id), "main")
		f.gitlab.Commit(id, "main", tofuStack("network"))
	}
	// A stack name may not start with another repository's prefix, whichever
	// repository was added first, and the refusal names both.
	for id, dir := range map[int]string{90: "q/x", 91: "r/x", 92: "s", 93: "t"} {
		f.gitlab.AddProject(id, fmt.Sprintf("acme/nest%d", id), "main")
		f.gitlab.Commit(id, "main", tofuStack(dir))
	}
	for _, tc := range []struct {
		name, repo, project, prefix string
		status                      int
		want                        string
	}{
		{"a prefix no stack name starts with", "qpre", "92", "q/", 201, `"stack":"q/s"`},
		{"an unprefixed stack under that prefix", "qx", "90", "", 409, `stack q/x of repository qx starts with prefix \"q/\" of repository qpre`},
		{"an unprefixed repository", "rx", "91", "", 201, `"stack":"r/x"`},
		{"a prefix an unprefixed stack starts with", "rpre", "93", "r/", 409, `stack r/x of repository rx starts with prefix \"r/\" of repository rpre`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := do("POST", "/v1/repos", admin(f), repoRequest(func(m map[string]any) {
				m["name"], m["project"], m["path"], m["prefix"] = tc.repo, tc.project, "", tc.prefix
			}))
			if r.status != tc.status || !strings.Contains(string(r.body), tc.want) {
				t.Fatalf("got %d %s, want %d %q", r.status, r.body, tc.status, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name, project, prefix string
		status                int
		want                  string
	}{
		{"inside another prefix", "78", "payments/eu/", 409, `of repository payments start one another`},
		{"a prefix that starts none", "78", "x/y/", 201, `"stack":"x/y/network"`},
		{"the start of another prefix", "79", "x/", 409, `of repository nest78 start one another`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := do("POST", "/v1/repos", admin(f), repoRequest(func(m map[string]any) {
				m["name"], m["project"], m["path"], m["prefix"] = "nest"+tc.project, tc.project, "", tc.prefix
			}))
			if r.status != tc.status || !strings.Contains(string(r.body), tc.want) {
				t.Fatalf("got %d %s, want %d %q", r.status, r.body, tc.status, tc.want)
			}
		})
	}

	// The token is in no answer, log line, or audit row.
	rows := f.audit(controltest.AuditFilter{})
	all := strings.Join(bodies, "\n") + logs.String()
	for _, e := range rows {
		all += e.Reason + e.Route + e.Principal
	}
	if strings.Contains(all, gitlabToken) || strings.Contains(all, "SERVERTEST") {
		t.Fatal("the forge token leaked")
	}
	if !slices.ContainsFunc(rows, func(e control.Entry) bool {
		return e.Route == "/v1/repos" && e.Status == 201 && strings.Contains(e.Reason, "repository infra")
	}) {
		t.Fatal("no audit row of the add")
	}
}

// waitPlan waits for a plan to leave planning.
func (f *fixture) waitPlan(tok, ref string) server.RemotePlan {
	f.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		r := f.do("GET", "/v1/plans/"+ref+"?wait=10s", tok, nil)
		var p server.RemotePlan
		if err := json.Unmarshal(r.body, &p); err != nil || p.Plan == nil {
			f.t.Fatalf("plans %s: %d %s", ref, r.status, r.body)
		}
		if p.Plan.Status != "planning" {
			return p
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("plans %s still planning: %s", ref, r.body)
		}
	}
}

func commitPlan(source map[string]any) *bytes.Reader {
	return planRequest(func(m map[string]any) {
		delete(m, "bundle")
		m["source"] = source
	})
}

func TestCommitPlan(t *testing.T) {
	f := planFixture(t, withForge(t, ""))
	f.gitlab.AddProject(4242, "acme/infra", "main")
	files := infraFiles()
	head := f.gitlab.Commit(4242, "main", files)
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	// A merge request's commit changes one file. The job names no source:
	// its token names the project and the commit.
	files["infra/network/main.tf"] = forgetest.File{Content: files["infra/network/main.tf"].Content + "resource \"random_pet\" \"pet2\" {}\n"}
	mr := f.gitlab.Commit(4242, "add-subnet-d", files)
	f.gitlab.Calls()
	tok := f.token(map[string]any{"sha": mr})
	r := f.do("POST", "/v1/plans", tok, commitPlan(map[string]any{}))
	if r.status != http.StatusAccepted {
		t.Fatalf("plan: %d %s", r.status, r.body)
	}
	p := f.waitPlan(tok, r.doc["plan"].(map[string]any)["ref"].(string))
	// The fixture's config has no approval rule, so the plan needs none.
	if p.Plan.Status != "approved" || p.Plan.Source != (server.PlanSourceInfo{Kind: "commit", Repo: "infra", Ref: "refs/heads/add-subnet-d", Commit: mr, RefCheck: "on-ref"}) {
		t.Fatalf("plan %+v: %s", p.Plan, p.Text)
	}
	if p.Plan.Stacks[0].SourceDigest == "" {
		t.Fatal("a commit plan has no source digest")
	}
	if n := blobCalls(f.gitlab.Calls()); n != 2 {
		t.Fatalf("the merge request's commit fetched %d blobs, want the stack's changed file and its module's", n)
	}
	if got := f.tofu.files[len(f.tofu.files)-1]; !slices.Equal(got, []string{"network/.terraform.lock.hcl", "network/main.tf", "platform/main.tf"}) {
		t.Fatalf("OpenTofu got the files %v", got)
	}
	// A person names the repository and the commit.
	if r := f.do("POST", "/v1/plans", tok, commitPlan(map[string]any{"commit": head})); r.status != http.StatusForbidden || !strings.Contains(string(r.body), "a CI job plans its own commit") {
		t.Fatalf("a CI job naming another commit: %d %s", r.status, r.body)
	}
	for _, tc := range []struct {
		name   string
		claims map[string]any
		source map[string]any
		status int
		want   string
	}{
		{"a fork's pipeline", map[string]any{"project_id": "9999", "project_path": "mallory/infra", "sha": mr}, map[string]any{}, 403, "a merge request from a fork runs in the fork's project"},
		{"a commit not pushed", map[string]any{"sha": strings.Repeat("0", 40)}, map[string]any{}, 404, "is not in repository infra: push it"},
		{"another repository", map[string]any{"sha": mr}, map[string]any{"repo": "payments"}, 403, "is not of the job's project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.do("POST", "/v1/plans", f.token(tc.claims), commitPlan(tc.source))
			if r.status != tc.status || !strings.Contains(string(r.body), tc.want) {
				t.Fatalf("got %d %s", r.status, r.body)
			}
		})
	}

	// A blob whose content does not match its id fails the plan.
	files["infra/network/main.tf"] = forgetest.File{Content: "resource \"random_pet\" \"tampered\" {}\n"}
	bad := f.gitlab.Commit(4242, "tamper", files)
	f.gitlab.Tamper(forge.BlobID([]byte(files["infra/network/main.tf"].Content)), "resource \"random_pet\" \"evil\" {}\n")
	tok = f.token(map[string]any{"sha": bad, "ref_path": "refs/heads/tamper"})
	r = f.do("POST", "/v1/plans", tok, commitPlan(map[string]any{}))
	if r.status != http.StatusAccepted {
		t.Fatalf("plan: %d %s", r.status, r.body)
	}
	p = f.waitPlan(tok, r.doc["plan"].(map[string]any)["ref"].(string))
	if p.Plan.Status != "failed" || !strings.Contains(p.Text, "does not match its blob id") {
		t.Fatalf("a tampered commit: %s %s", p.Plan.Status, p.Text)
	}
}

func TestPollReadsANewHead(t *testing.T) {
	f := newFixture(t, withForge(t, `poll_every = "1s"`))
	f.gitlab.AddProject(4242, "acme/infra", "main")
	files := infraFiles()
	f.gitlab.Commit(4242, "main", files)
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	maps.Copy(files, tofuStack("infra/apps"))
	next := f.gitlab.Commit(4242, "main", files)
	var doc server.RepoDoc
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(50 * time.Millisecond) {
		r := f.do("GET", "/v1/repos/infra", f.token(nil), nil)
		_ = json.Unmarshal(r.body, &doc)
		if doc.Repo.Head == next {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the poll did not read the new head: %s", r.body)
		}
	}
	if len(doc.Stacks) != 3 || doc.Stacks[0].Stack != "apps" || doc.Repo.Error != "" {
		t.Fatalf("after the poll %+v", doc)
	}
	// A failed poll keeps the head and says why.
	f.gitlab.Token = "glpat-rotated"
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(50 * time.Millisecond) {
		rp, err := f.store.Repo(context.Background(), "infra")
		if err == nil && strings.Contains(rp.Error, "401") {
			if rp.Head != next || strings.Contains(rp.Error, gitlabToken) {
				t.Fatalf("after a failed poll %+v", rp)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no failed poll recorded: %+v %v", rp, err)
		}
	}
}

func TestPinnedRepos(t *testing.T) {
	f := newFixture(t, withForge(t, `poll_every = "1s"
repo {
  name = "infra"
  forge = "gitlab"
  project = "acme/infra"
  path = "infra"
}`))
	f.gitlab.AddProject(4242, "acme/infra", "main")
	f.gitlab.Commit(4242, "main", infraFiles())
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(func(m map[string]any) { m["name"] = "other" })); r.status != http.StatusConflict || !strings.Contains(string(r.body), "the config pins the repositories") {
		t.Fatalf("add with a pinned list: %d %s", r.status, r.body)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(50 * time.Millisecond) {
		r := f.do("GET", "/v1/repos", f.token(nil), nil)
		if strings.Contains(string(r.body), `"origin":"config"`) && strings.Contains(string(r.body), `"pinned":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pinned repository was not added: %s", r.body)
		}
	}
}

// A pinned list whose second repository has a prefix that starts with the
// first's loads the first and logs why it refuses the second, which names
// the first.
func TestPinnedReposRefuseNestedPrefixes(t *testing.T) {
	var logs lockedBuffer
	f := newFixture(t, withForge(t, `poll_every = "1s"
repo {
  name = "infra"
  forge = "gitlab"
  project = "acme/infra"
  path = "infra"
  prefix = "a/"
}
repo {
  name = "deep"
  forge = "gitlab"
  project = "acme/deep"
  prefix = "a/b/"
}`), logTo(&logs))
	f.gitlab.AddProject(4242, "acme/infra", "main")
	f.gitlab.Commit(4242, "main", infraFiles())
	f.gitlab.AddProject(81, "acme/deep", "main")
	f.gitlab.Commit(81, "main", tofuStack("network"))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if strings.Contains(logs.String(), "start one another") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the nested prefix was not refused:\n%s", logs.String())
		}
	}
	if !strings.Contains(logs.String(), "repo=deep") || !strings.Contains(logs.String(), "of repository infra start one another") {
		t.Errorf("the log does not name both repositories:\n%s", logs.String())
	}
	r := f.do("GET", "/v1/repos", f.token(nil), nil)
	if !strings.Contains(string(r.body), `"name":"infra"`) || strings.Contains(string(r.body), `"name":"deep"`) {
		t.Errorf("the list of repositories: %s", r.body)
	}
}

// A later commit of a repository that adds a stack whose name starts with
// another repository's prefix is not recorded: the poll refuses it, says
// why in the log and the repository's error, and the head stays.
func TestPollRefusesAStackUnderAnotherPrefix(t *testing.T) {
	var logs bytes.Buffer
	f := newFixture(t, withForge(t, `poll_every = "1s"
repo {
  name = "pre"
  forge = "gitlab"
  project = "acme/pre"
  prefix = "p/"
}
repo {
  name = "plain"
  forge = "gitlab"
  project = "acme/plain"
}`), logTo(&logs))
	f.gitlab.AddProject(82, "acme/pre", "main")
	f.gitlab.Commit(82, "main", tofuStack("one"))
	f.gitlab.AddProject(83, "acme/plain", "main")
	first := f.gitlab.Commit(83, "main", tofuStack("net"))
	repo := func() server.RepoInfo {
		var doc server.RepoDoc
		r := f.do("GET", "/v1/repos/plain", f.token(nil), nil)
		if err := json.Unmarshal(r.body, &doc); err != nil {
			t.Fatalf("%s: %v", r.body, err)
		}
		return doc.Repo
	}
	for deadline := time.Now().Add(10 * time.Second); repo().Head != first; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the repository was not added:\n%s", logs.String())
		}
	}
	f.gitlab.Commit(83, "main", tofuStack("p/two"))
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(repo().Error, "starts with prefix"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the commit was not refused:\n%s", logs.String())
		}
	}
	if r := repo(); r.Head != first || !strings.Contains(r.Error, "stack p/two of repository plain") || !strings.Contains(r.Error, "of repository pre") {
		t.Errorf("head %s, error %q, want head %s and an error that names both repositories", r.Head, r.Error, first)
	}
}

// Two requests at once that each would pass a check made before the write
// cannot both be stored: of two nested prefixes, one repository is added.
func TestConcurrentNestedPrefixes(t *testing.T) {
	f := newFixture(t, withForge(t, ""))
	for _, id := range []int{84, 85} {
		f.gitlab.AddProject(id, fmt.Sprintf("acme/c%d", id), "main")
		f.gitlab.Commit(id, "main", tofuStack(fmt.Sprintf("s%d", id)))
	}
	tok := admin(f)
	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i, prefix := range []string{"c/", "c/d/"} {
		wg.Go(func() {
			statuses[i] = f.do("POST", "/v1/repos", tok, repoRequest(func(m map[string]any) {
				m["name"], m["project"], m["path"], m["prefix"] = fmt.Sprintf("c%d", i), fmt.Sprint(84+i), "", prefix
			})).status
		})
	}
	wg.Wait()
	slices.Sort(statuses)
	if statuses[0] != http.StatusCreated || statuses[1] != http.StatusConflict {
		t.Errorf("statuses %v, want one 201 and one 409", statuses)
	}
}

// TestForgeTextNeverLeaks checks that a forge that echoes the read token in
// its answers cannot put it in an API answer, an audit row, a log line, or
// a repository's poll error.
func TestForgeTextNeverLeaks(t *testing.T) {
	var logs bytes.Buffer
	f := newFixture(t, withForge(t, `poll_every = "1s"`), logTo(&logs))
	f.gitlab.AddProject(4242, "acme/infra", "main")
	f.gitlab.Commit(4242, "main", infraFiles())
	// mode 0 is the stand-in; 1 refuses with the token in its message; 2
	// answers 200 with the token in the body.
	var mode atomic.Int32
	f.is.Mount("/api/v4/projects/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "echo " + r.Header.Get("PRIVATE-TOKEN")})
		case 2:
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]string{"id": strings.Repeat("a", 40), "message": r.Header.Get("PRIVATE-TOKEN")}})
		default:
			f.gitlab.ServeHTTP(w, r)
		}
	}))
	var answers []string
	mode.Store(1)
	r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil))
	answers = append(answers, string(r.body))
	if r.status != http.StatusBadGateway || !strings.Contains(string(r.body), "the forge answered 403") {
		t.Fatalf("add against a refusing forge: %d %s", r.status, r.body)
	}
	mode.Store(0)
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	for _, m := range []int32{1, 2} {
		mode.Store(m)
		want := map[int32]string{1: "the forge answered 403", 2: "the forge's answer holds the read token"}[m]
		for deadline := time.Now().Add(time.Minute); ; time.Sleep(50 * time.Millisecond) {
			rp, err := f.store.Repo(context.Background(), "infra")
			if err == nil && strings.Contains(rp.Error, want) {
				answers = append(answers, rp.Error)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no failed poll with %q recorded: %+v %v", want, rp, err)
			}
		}
		answers = append(answers, string(f.do("GET", "/v1/repos/infra", f.token(nil), nil).body))
	}
	for _, row := range f.audit(controltest.AuditFilter{}) {
		answers = append(answers, row.Reason)
	}
	answers = append(answers, logs.String())
	for _, a := range answers {
		if strings.Contains(a, gitlabToken) || strings.Contains(a, "echo") {
			t.Fatalf("the forge's text leaked: %s", a)
		}
	}
}

// TestRepoAddTakesEveryStack checks that an admin grant whose patterns
// leave out nested stacks may not add a repository, whatever it spells.
func TestRepoAddTakesEveryStack(t *testing.T) {
	for _, stacks := range []string{`["*"]`, `["*", "*/*"]`, `["infra/**"]`, `["*", "*/*/*/**"]`} {
		t.Run(stacks, func(t *testing.T) {
			f := newFixture(t, func(text string) string {
				return strings.Replace(withForge(t, "")(text), everyStack, stacks, 1)
			})
			f.gitlab.AddProject(4242, "acme/infra", "main")
			f.gitlab.Commit(4242, "main", infraFiles())
			r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil))
			if r.status != http.StatusForbidden || !strings.Contains(string(r.body), "admin on every stack in every environment") {
				t.Fatalf("admin on %s: %d %s", stacks, r.status, r.body)
			}
		})
	}
}

// TestCommitPlanChecksTheRef checks that a plan's commit must be on the ref
// the token or the request names, as the forge shows it, and that the plan
// records the check.
func TestCommitPlanChecksTheRef(t *testing.T) {
	f := planFixture(t, withForge(t, ""))
	f.gitlab.AddProject(4242, "acme/infra", "main")
	files := infraFiles()
	head := f.gitlab.Commit(4242, "main", files)
	if r := f.do("POST", "/v1/repos", admin(f), repoRequest(nil)); r.status != http.StatusCreated {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	files["infra/network/main.tf"] = forgetest.File{Content: "resource \"random_pet\" \"unrelated\" {}\n"}
	unrelated := f.gitlab.Commit(4242, "unrelated", files)
	// A later commit on main keeps head on main, as its ancestor.
	files["infra/network/main.tf"] = forgetest.File{Content: "resource \"random_pet\" \"later\" {}\n"}
	later := f.gitlab.Commit(4242, "main", files)
	for _, tc := range []struct {
		name   string
		claims map[string]any
		status int
		want   string
	}{
		{"a commit of another branch", map[string]any{"sha": unrelated, "ref_path": "refs/heads/main"}, 403, "is not on refs/heads/main of repository infra"},
		{"a ref the forge lacks", map[string]any{"sha": head, "ref_path": "refs/heads/gone"}, 502, "could not check that commit " + head[:8] + " is on refs/heads/gone"},
		{"a ref that is not full", map[string]any{"sha": head, "ref_path": "main"}, 400, "is not a full ref"},
		{"an ancestor of the ref", map[string]any{"sha": head, "ref_path": "refs/heads/main"}, 202, ""},
		{"the ref's commit", map[string]any{"sha": later, "ref_path": "refs/heads/main"}, 202, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := f.token(tc.claims)
			r := f.do("POST", "/v1/plans", tok, commitPlan(map[string]any{}))
			if r.status != tc.status || !strings.Contains(string(r.body), tc.want) {
				t.Fatalf("got %d %s", r.status, r.body)
			}
			if r.status != http.StatusAccepted {
				return
			}
			p := f.waitPlan(tok, r.doc["plan"].(map[string]any)["ref"].(string))
			if p.Plan.Source.RefCheck != "on-ref" || p.Plan.Source.Ref != "refs/heads/main" {
				t.Fatalf("source %+v", p.Plan.Source)
			}
		})
	}
}
