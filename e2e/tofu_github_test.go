//go:build e2e && e2e_tofu && linux

package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control/pgtest"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
)

// githubPGEnv names the Postgres database of the child of TestTofuGitHub.
const githubPGEnv = "WRIT_E2E_GITHUB_PG"

const (
	// githubForgeToken is the read token of the GitHub stand-in.
	githubForgeToken = "github_pat_E2E_forge_read_0123"
	// actionsRequestToken is the token a GitHub Actions job holds to ask
	// for its ID token. No output may hold it.
	actionsRequestToken = "actions-E2E-request-token-4b1d"
	// githubHookSecret is the repository's webhook secret.
	githubHookSecret = "github-E2E-hook-secret-9a7e"
)

// TestTofuGitHub runs a team's writ serve against a GitHub Enterprise
// Server stand-in, which serves its REST API at /api/v3 and issues its
// Actions' ID tokens at /_services/token, with Postgres, provider uids,
// and real OpenTofu. writ ci github writes the workflow; a workflow job
// with no WRIT_ID_TOKEN asks its Actions endpoint for an ID token and
// plans stack app of the pinned GitHub repository by commit; a job of
// another repository is refused; bob, signed in as a person, approves the
// plan and applies it after the merge; and a signed push webhook makes
// writ serve read the new head of the default branch at once.
func TestTofuGitHub(t *testing.T) {
	if os.Getenv(tofuChildEnv) != "" {
		tofuGitHubChild(t)
		return
	}
	base, stop, err := pgtest.Start()
	if err != nil {
		t.Fatalf("this test needs Postgres's initdb, pg_ctl, and createdb on PATH, as just e2e gives: %v", err)
	}
	t.Cleanup(stop)
	inTofuNamespace(t, "TestTofuGitHub", tofuTeamNamespace, githubPGEnv+"="+pgtest.Database(t, base))
}

// actionsTokens is a stand-in for GitHub Actions' ID token endpoint: it
// signs a token of the current job for the audience asked for.
type actionsTokens struct {
	issuer string
	key    oidctest.Key
	mu     sync.Mutex
	job    map[string]any
	asked  []string
}

func (a *actionsTokens) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+actionsRequestToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	aud := r.URL.Query().Get("audience")
	a.asked = append(a.asked, aud)
	c := oidctest.GitHubJob(a.issuer, aud, time.Now())
	for k, v := range a.job {
		c[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"count": 1, "value": oidctest.Sign(a.key, c)})
}

func (a *actionsTokens) setJob(job map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.job = job
}

func tofuGitHubChild(t *testing.T) {
	db := os.Getenv(githubPGEnv)
	u, err := url.Parse(db)
	if err != nil || u.Query().Get("host") == "" {
		t.Fatalf("the database %q names no socket directory", db)
	}
	lock := tofuLock(t)
	bin, shared := tofuChildRoot(t, u.Query().Get("host"))
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

	// People sign in at sso; GitHub is ghe.
	is := oidctest.New(t)
	ghe := oidctest.New(t)
	actionsIssuer := ghe.ServeAt("/_services/token")
	gh := forgetest.GitHubHandler(githubForgeToken)
	gh.URL = ghe.URL
	ghe.Mount("/api/v3/", gh)
	tokens := &actionsTokens{issuer: actionsIssuer, key: ghe.Keys()[0]}
	actions := httptest.NewServer(tokens)
	defer actions.Close()

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
	gheCA := write("ghe.pem", string(ghe.CAPEM()))
	tokenFile := write("secrets/github_read", githubForgeToken+"\n")
	hookFile := write("secrets/web_hook", githubHookSecret+"\n")
	canaryFile := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(canaryFile, []byte(tofuCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
poll_every    = "1h"

limits {
  requests_per_second  = 100
  requests_per_address = 1000
}

tofu {
  binary       = %[5]q
  plugin_cache = %[6]q
  scratch_dir  = %[7]q
}

issuer "github" {
  kind     = "github-actions"
  identity = "github"
  url      = %[8]q
  ca_file  = %[9]q
}

issuer "sso" {
  kind               = "people"
  identity           = "gitlab"
  url                = %[10]q
  ca_file            = %[11]q
  client_id          = "writ-e2e"
  client_secret_file = %[12]q
  groups             = "userinfo"
}

forge "ghe" {
  kind            = "github"
  url             = %[13]q
  read_token_file = %[14]q
  ca_file         = %[9]q
}

repo {
  name                = "web"
  forge               = "ghe"
  project             = "acme/web"
  path                = "infra"
  webhook_secret_file = %[15]q
}

# Any repository of the owner may plan, so the control plane's own check
# of the repository id refuses another repository's job.
team "acme_ci" {
  issuer = "github"
  match  = { repository_owner_id = ["65"] }
}

team "acme" {
  issuer = "sso"
  match  = { groups = ["acme"] }
}

grant {
  teams  = ["acme_ci"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "plan"]
}

grant {
  teams  = ["acme"]
  stacks = ["app"]
  envs   = ["prod"]
  can    = ["read", "approve", "apply"]
  risks  = ["create", "update", "replace", "delete"]
}

approval {
  stacks = ["*"]
  envs   = ["prod"]
  risks  = ["create", "update", "replace", "delete", "data-loss"]
  need   = 1
}

env "prod" {
  key          = "keys/prod.key"
  provider_env = { TF_VAR_canary = %[16]q }
}
`, srvURL, port, db, data, bin, cache, scratch, actionsIssuer, gheCA, is.URL, ca, clientSecretFile, ghe.URL, tokenFile, hookFile, canaryFile))
	gh.AddProject(777001, "acme/web", "main")
	gh.Commit(777001, "main", tofuAppFiles(lock, "v1"))
	jobDir := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &served{t: t, is: is, url: srvURL, db: db, data: data, log: &syncBuffer{}, stack: &stackDir{t: t, dir: jobDir}}
	if check := s.stack.exec(nil, "serve", "--config", cfg, "--check"); check.code != 0 {
		t.Fatalf("writ serve --check: %s", check)
	}
	serveAgain(t, s, cfg)

	laptop := []string{"XDG_CONFIG_HOME=" + t.TempDir(), "WRIT_SERVER=", "WRIT_ID_TOKEN="}
	if out := browserSignIn(t, s, laptop); !strings.Contains(out, "as bob (gitlab/user/2002)") {
		t.Fatalf("writ login printed:\n%s", out)
	}
	session := sessionTokenIn(t, laptop[0][len("XDG_CONFIG_HOME="):])
	person := func(want int, args ...string) result {
		t.Helper()
		r := s.stack.exec(append(laptop[:1:1], "WRIT_SERVER="+srvURL, "WRIT_ID_TOKEN="), args...)
		s.outsMu.Lock()
		s.outs = append(s.outs, r.stdout, r.stderr)
		s.outsMu.Unlock()
		if r.code != want {
			t.Fatalf("want exit %d\n%s\nserve log:\n%s", want, r, s.log.String())
		}
		return r
	}
	head := func() string {
		code, doc := s.api("GET", "/v1/repos/web", session, nil)
		c, _ := doc["commit"].(map[string]any)
		if code != 200 || c == nil {
			return ""
		}
		sha, _ := c["sha"].(string)
		return sha
	}
	for deadline := time.Now().Add(30 * time.Second); head() == ""; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the pinned GitHub repository was not read:\n%s", s.log.String())
		}
	}

	// Onboarding: writ ci github writes the workflow and the config.
	// The repository is at a GitHub Enterprise Server, so writ ci github
	// needs its runners' label.
	if r := person(2, "ci", "github", "--repo", "web", "--writ-version", "v0.1.8"); !strings.Contains(r.stderr+r.stdout, "--runs-on LABEL") {
		t.Fatalf("writ ci github with no runner label:\n%s", r)
	}
	ci := person(0, "ci", "github", "--repo", "web", "--writ-version", "v0.1.8", "--runs-on", "self-hosted-linux")
	for _, want := range []string{"  id-token: write\n", "    runs-on: \"self-hosted-linux\"\n", "github.event.pull_request.head.repo.full_name == github.repository", "  writ:\n    # The one check to require.", "--slot-wait 15m\n", "          - stack: \"app\"\n            env: \"prod\"\n", "  match  = { repository_id = [\"777001\"] }\n"} {
		if !strings.Contains(ci.stdout, want) {
			t.Fatalf("writ ci github lacks %q:\n%s", want, ci.stdout)
		}
	}
	t.Logf("writ ci github:\n%s", ci.stdout)

	// A workflow job: WRIT_ID_TOKEN is unset, so writ asks Actions for an
	// ID token whose audience is writ serve's URL.
	job := func(want int, claims map[string]any, args ...string) result {
		t.Helper()
		tokens.setJob(claims)
		env := []string{"WRIT_SERVER=" + srvURL, "WRIT_ID_TOKEN=", "XDG_CONFIG_HOME=" + t.TempDir(),
			"ACTIONS_ID_TOKEN_REQUEST_URL=" + actions.URL + "/_apis/pipelines/1/runs/1/idtoken?api-version=2.0", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=" + actionsRequestToken}
		r := s.stack.exec(env, args...)
		s.outsMu.Lock()
		s.outs = append(s.outs, r.stdout, r.stderr)
		s.outsMu.Unlock()
		if r.code != want {
			t.Fatalf("want exit %d\n%s\nserve log:\n%s", want, r, s.log.String())
		}
		return r
	}
	pr := gh.Commit(777001, "refs/pull/7/merge", tofuAppFiles(lock, "v1"))
	gh.AddPull(777001, 7, 777001)
	claims := map[string]any{"repository_id": "777001", "repository": "acme/web", "sha": pr, "ref": "refs/pull/7/merge", "event_name": "pull_request"}
	var p tofuPlanDoc
	r := job(0, claims, "plan", "--remote", "--json", "--env", "prod", "--stack", "app", "--repo", "web", "--commit", pr)
	if err := json.Unmarshal([]byte(r.stdout), &p); err != nil || !p.OK || p.Plan.Status != "pending" || p.Plan.Source.Commit != pr || p.Plan.Stacks[0].Changes != 4 {
		t.Fatalf("the workflow's plan: %v\n%s", err, r)
	}
	t.Logf("writ plan --remote in a GitHub Actions job:\n%s", p.Text)
	tokens.mu.Lock()
	if len(tokens.asked) != 1 || tokens.asked[0] != srvURL {
		t.Fatalf("the job asked Actions for the audiences %v, want %s", tokens.asked, srvURL)
	}
	tokens.mu.Unlock()
	other := map[string]any{"repository_id": "999", "repository": "mallory/web", "sha": pr, "ref": "refs/pull/7/merge"}
	if r := job(1, other, "plan", "--remote", "--env", "prod", "--stack", "app", "--repo", "web", "--commit", pr); !strings.Contains(r.stdout+r.stderr, "is not a repository of this control plane") {
		t.Fatalf("another repository's job:\n%s", r)
	}

	// bob approves and applies with real OpenTofu.
	if r := person(0, "approve", "--remote", p.Plan.ID); !strings.Contains(r.stdout, ", approved.") {
		t.Fatalf("bob's approval:\n%s", r)
	}
	// A pull request's plan applies after the merge: bob names the merged
	// commit of the default branch, whose sources are the plan's.
	merged := gh.Commit(777001, "main", tofuAppFiles(lock, "v1"))
	var run tofuRun
	r = person(0, "apply", "--remote", "--json", "--merged", "--env", "prod", "--stack", "app", "--repo", "web", "--commit", merged)
	if err := json.Unmarshal([]byte(r.stdout), &run); err != nil || !run.OK || run.Run.Status != "applied" || run.Document == nil || run.Document.StateVersion != 1 {
		t.Fatalf("bob's apply: %v\n%s", err, r)
	}
	t.Logf("writ apply --remote by bob:\n%s", run.Text)

	// A push to main: GitHub's signed delivery makes writ serve read the
	// new head now, though it polls once an hour.
	next := gh.Commit(777001, "main", tofuAppFiles(lock, "v2"))
	body := []byte(`{"ref":"refs/heads/evil","after":"` + strings.Repeat("e", 40) + `","repository":{"id":999,"full_name":"mallory/evil"}}`)
	mac := hmac.New(sha256.New, []byte(githubHookSecret))
	mac.Write(body)
	deliver := func(sig string) int {
		req, _ := http.NewRequest("POST", srvURL+"/v1/hooks/ghe/web", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", "6b5f3e2a-0000-4000-8000-00000000e2e0")
		req.Header.Set("X-Hub-Signature-256", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := deliver("sha256=" + strings.Repeat("0", 64)); code != http.StatusUnauthorized {
		t.Fatalf("a delivery with a wrong signature: %d", code)
	}
	if code := deliver("sha256=" + hex.EncodeToString(mac.Sum(nil))); code != http.StatusAccepted {
		t.Fatalf("a signed delivery: %d\n%s", code, s.log.String())
	}
	for deadline := time.Now().Add(30 * time.Second); head() != next; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the webhook did not read %s:\n%s", next, s.log.String())
		}
	}

	s.outsMu.Lock()
	outs := strings.Join(s.outs, "\n")
	s.outsMu.Unlock()
	for _, secret := range []string{githubForgeToken, actionsRequestToken, githubHookSecret, servedClientSecret, tofuCanary} {
		if strings.Contains(outs, secret) || strings.Contains(s.log.String(), secret) {
			t.Errorf("an output or the serve log holds a secret %.12s...", secret)
		}
	}
}
