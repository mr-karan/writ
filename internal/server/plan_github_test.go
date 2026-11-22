package server_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
)

// TestGitHubActionsCommitPlan checks that a GitHub Actions job's token
// plans its own commit of the repository its repository_id names at the
// GitHub forge its issuer belongs to, here a GitHub Enterprise Server
// stand-in that issues tokens at HOST/_services/token and serves its API
// at HOST/api/v3, and that a token of another repository, or of a commit
// not on its ref, is refused.
func TestGitHubActionsCommitPlan(t *testing.T) {
	ghe := oidctest.New(t)
	issuer := ghe.ServeAt("/_services/token")
	gh := forgetest.GitHubHandler(githubToken)
	gh.URL = ghe.URL
	ghe.Mount("/api/v3/", gh)
	dir := t.TempDir()
	ca, tokenFile := filepath.Join(dir, "ghe.pem"), filepath.Join(dir, "github_read")
	if err := os.WriteFile(ca, ghe.CAPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(githubToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := planFixture(t, withForge(t, fmt.Sprintf(`
issuer "ghe" {
  kind = "github-actions"
  identity = "ghe"
  url = %q
  ca_file = %q
}

forge "ghe" {
  kind = "github"
  url = %q
  read_token_file = %q
  ca_file = %q
}

# Any repository of the owner may plan, so the control plane's own check
# of the repository id, not a team rule, refuses another repository's job.
team "web_ci" {
  issuer = "ghe"
  match = { repository_owner_id = ["65"] }
}

grant {
  teams = ["web_ci"]
  stacks = ["web/*"]
  envs = ["prod"]
  can = ["read", "plan"]
}
`, issuer, ca, ghe.URL, tokenFile, ca)))
	gh.AddProject(777001, "acme/web", "main")
	files := infraFiles()
	gh.Commit(777001, "main", files)
	add := repoRequest(func(m map[string]any) {
		m["name"], m["forge"], m["project"], m["prefix"] = "web", "ghe", "acme/web", "web/"
	})
	if r := f.do("POST", "/v1/repos", admin(f), add); r.status != http.StatusCreated {
		t.Fatalf("add: %d %s", r.status, r.body)
	}
	files["infra/network/main.tf"] = forgetest.File{Content: files["infra/network/main.tf"].Content + "resource \"random_pet\" \"pet2\" {}\n"}
	branch := gh.Commit(777001, "add-subnet-d", files)
	merge := gh.Commit(777001, "refs/pull/42/merge", files)
	gh.AddPull(777001, 42, 777001)
	// Pull request 9 comes from a fork, 888002; pull request 10 is one the
	// forge does not answer for.
	fork := gh.Commit(777001, "refs/pull/9/merge", files)
	gh.AddPull(777001, 9, 888002)
	unknown := gh.Commit(777001, "refs/pull/10/merge", files)
	other := gh.Commit(777001, "other", map[string]forgetest.File{"x": {Content: "x"}})

	token := func(edits map[string]any) string {
		c := oidctest.GitHubJob(issuer, audience, time.Now())
		c["repository_id"], c["repository"] = "777001", "acme/web"
		for k, v := range edits {
			c[k] = v
		}
		return oidctest.Sign(ghe.Keys()[0], c)
	}
	plan := func(tok string) server.RemotePlan {
		t.Helper()
		r := f.do("POST", "/v1/plans", tok, planRequest(func(m map[string]any) {
			m["stacks"], m["source"] = []string{"web/network"}, map[string]any{}
		}))
		if r.status != http.StatusAccepted {
			t.Fatalf("plan: %d %s", r.status, r.body)
		}
		return f.waitPlan(tok, r.doc["plan"].(map[string]any)["ref"].(string))
	}
	for _, tc := range []struct {
		name, sha, ref, event string
	}{
		{"a push", branch, "refs/heads/add-subnet-d", "push"},
		{"a pull request's merge commit", merge, "refs/pull/42/merge", "pull_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := plan(token(map[string]any{"sha": tc.sha, "ref": tc.ref, "event_name": tc.event}))
			if p.Plan.Status != "approved" || p.Plan.Source != (server.PlanSourceInfo{Kind: "commit", Repo: "web", Ref: tc.ref, Commit: tc.sha, RefCheck: "on-ref"}) {
				t.Fatalf("plan %+v: %s", p.Plan, p.Text)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		claims map[string]any
		source map[string]any
		status int
		want   string
	}{
		{"another repository's job", map[string]any{"repository_id": "999", "repository": "mallory/web", "sha": branch}, nil, 403, "project mallory/web (id 999) is not a repository of this control plane"},
		{"a commit not on its ref", map[string]any{"sha": other, "ref": "refs/heads/add-subnet-d"}, nil, 403, "is not on refs/heads/add-subnet-d of repository web"},
		{"another commit than its own", map[string]any{"sha": branch, "ref": "refs/heads/add-subnet-d"}, map[string]any{"repo": "web", "commit": other}, 403, "a CI job plans its own commit"},
		{"a ref that is not full", map[string]any{"sha": branch, "ref": "add-subnet-d"}, nil, 400, "is not a full ref"},
		{"a pull request from a fork", map[string]any{"sha": fork, "ref": "refs/pull/9/merge", "event_name": "pull_request"}, nil, 403, "pull request 9 of repository web comes from another repository, id 888002, such as a fork"},
		{"a pull request the forge cannot show", map[string]any{"sha": unknown, "ref": "refs/pull/10/merge", "event_name": "pull_request"}, nil, 404, "writ serve could not check that pull request 10 of repository web comes from the repository itself, so it refuses the plan"},
		{"a pull_request_target job", map[string]any{"sha": branch, "ref": "refs/heads/add-subnet-d", "event_name": "pull_request_target"}, nil, 403, "a job of a pull_request_target event may not plan"},
		{"a pull request number over int64", map[string]any{"sha": merge, "ref": "refs/pull/9999999999999999999/merge", "event_name": "pull_request"}, nil, 400, "names pull request 9999999999999999999, which is not a pull request's number"},
		{"a pull request number of 20 digits", map[string]any{"sha": merge, "ref": "refs/pull/99999999999999999999/merge", "event_name": "pull_request"}, nil, 400, "names pull request 99999999999999999999, which is not a pull request's number"},
		{"a negative pull request number", map[string]any{"sha": merge, "ref": "refs/pull/-1/merge", "event_name": "pull_request"}, nil, 400, "names pull request -1, which is not a pull request's number"},
		{"a pull request number that is not a number", map[string]any{"sha": merge, "ref": "refs/pull/x7/merge", "event_name": "pull_request"}, nil, 400, "names pull request x7, which is not a pull request's number"},
		{"a pull request number with a sign", map[string]any{"sha": merge, "ref": "refs/pull/+42/merge", "event_name": "pull_request"}, nil, 400, "is not a full ref"},
		{"pull request 0", map[string]any{"sha": merge, "ref": "refs/pull/0/merge", "event_name": "pull_request"}, nil, 400, "names pull request 0, which is not a pull request's number"},
		{"a pull ref of another shape", map[string]any{"sha": merge, "ref": "refs/pull/42/evil", "event_name": "pull_request"}, nil, 403, "is not a pull request's merge or head ref"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.do("POST", "/v1/plans", token(tc.claims), planRequest(func(m map[string]any) {
				m["stacks"], m["source"] = []string{"web/network"}, tc.source
				if tc.source == nil {
					m["source"] = map[string]any{}
				}
			}))
			if r.status != tc.status || !strings.Contains(string(r.body), tc.want) {
				t.Fatalf("got %d %s", r.status, r.body)
			}
		})
	}

	// A GitHub rate limit while the commit is checked, before the plan is
	// taken, is 503 with GitHub's Retry-After, so a job waits and asks
	// again. Last: the client then holds calls back.
	gh.Limit(1, 30)
	r := f.do("POST", "/v1/plans", token(map[string]any{"sha": branch, "ref": "refs/heads/add-subnet-d", "event_name": "push"}), planRequest(func(m map[string]any) {
		m["stacks"], m["source"] = []string{"web/network"}, map[string]any{}
	}))
	if ra, _ := strconv.Atoi(r.header.Get("Retry-After")); r.status != http.StatusServiceUnavailable || r.class() != "provider" || ra < 25 || ra > 30 || !strings.Contains(string(r.body), "limits the rate of calls") {
		t.Fatalf("a rate limit: %d Retry-After %q %s", r.status, r.header.Get("Retry-After"), r.body)
	}
}
