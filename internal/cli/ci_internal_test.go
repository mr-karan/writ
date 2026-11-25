package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/server"
)

// repoServer is a writ serve that answers GET /v1/repos/{name} with doc,
// GET /v1/config with cfg when it is set, and 404 for anything else.
func repoServer(t *testing.T, doc server.RepoDoc, cfg ...server.ConfigDoc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/config" && len(cfg) > 0 {
			_ = json.NewEncoder(w).Encode(cfg[0])
			return
		}
		if r.URL.Path != "/v1/repos/"+doc.Repo.Name {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"format":"writ.repo/v0","ok":false,"diagnostics":[{"severity":"error","class":"usage","message":"no repository"}]}`))
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func webRepo(kind string) server.RepoDoc {
	return server.RepoDoc{Format: server.FormatRepo, OK: true,
		Repo: server.RepoInfo{Name: "web-app", Forge: "github", Kind: kind, ForgeURL: "https://github.com", Project: "acme/web-app", ProjectID: "777001", DefaultBranch: "main"},
		Stacks: []server.FoundStack{
			{Stack: "web/network", Served: []string{"prod", "staging"}},
			{Stack: "web/database", Served: []string{"prod"}},
			{Stack: "web/unserved"},
		},
	}
}

// TestCIGitHub checks writ ci github against a reviewed copy of its
// output, testdata/ci-github.golden.
func TestCIGitHub(t *testing.T) {
	srv := repoServer(t, webRepo("github"))
	var out bytes.Buffer
	a := precedenceApp(t, t.TempDir(), srv.URL, map[string]string{tokenCI: "ci-token"})
	a.opts.Stdout = &out
	if err := a.CI(context.Background(), "github", CIOptions{Repo: "web-app", WritVersion: "v0.1.9"}); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "ci-github.golden"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.ReplaceAll(out.String(), srv.URL, "http://127.0.0.1:PORT")
	if got != string(want) {
		t.Fatalf("writ ci github printed:\n%s\nwant:\n%s", got, want)
	}
}

// TestCIGitHubRunnersAndSlots checks the runner label, which a GitHub
// Enterprise Server repository must name, and max-parallel, which is the
// server's limits.plans_per_principal when an admin's token can read it.
func TestCIGitHubRunnersAndSlots(t *testing.T) {
	ghe := webRepo("github")
	ghe.Repo.ForgeURL = "https://ghe.example.com"
	limits := func(plans, perPrincipal int) []server.ConfigDoc {
		return []server.ConfigDoc{{Format: server.FormatConfig, OK: true, Admin: &server.AdminConfig{Limits: server.LimitsInfo{Plans: plans, PlansPerPrincipal: perPrincipal}}}}
	}
	for _, tc := range []struct {
		name string
		doc  server.RepoDoc
		cfg  []server.ConfigDoc
		o    CIOptions
		want []string
	}{
		{"github.com, limits unread", webRepo("github"), nil, CIOptions{}, []string{`    runs-on: "ubuntu-24.04"` + "\n", "      max-parallel: 1\n", "# max-parallel is 1: an admin who runs writ ci github again gets writ serve's own limit."}},
		{"an enterprise server's label", ghe, nil, CIOptions{RunsOn: "self-hosted-linux"}, []string{`    runs-on: "self-hosted-linux"` + "\n"}},
		{"the per-principal limit is the lesser", webRepo("github"), limits(8, 2), CIOptions{}, []string{"      # writ serve takes 2 plans of one actor at once: the lesser of its limits.plans and\n      # limits.plans_per_principal.\n      max-parallel: 2\n"}},
		{"the server's limit is the lesser", webRepo("github"), limits(3, 4), CIOptions{}, []string{"      max-parallel: 3\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := repoServer(t, tc.doc, tc.cfg...)
			var out bytes.Buffer
			a := precedenceApp(t, t.TempDir(), srv.URL, map[string]string{tokenCI: "ci-token"})
			a.opts.Stdout = &out
			tc.o.Repo, tc.o.WritVersion = "web-app", "v0.1.9"
			if err := a.CI(context.Background(), "github", tc.o); err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Fatalf("the workflow lacks %q:\n%s", w, out.String())
				}
			}
			if tc.cfg != nil && strings.Contains(out.String(), "could not read") {
				t.Fatalf("a hint to read the server's limit, which was read:\n%s", out.String())
			}
		})
	}
}

// TestCIGitHubDefaultsToItsOwnRelease checks that with no --writ-version,
// a writ that is a release installs itself.
func TestCIGitHubDefaultsToItsOwnRelease(t *testing.T) {
	srv := repoServer(t, webRepo("github"))
	var out bytes.Buffer
	a := precedenceApp(t, t.TempDir(), srv.URL, map[string]string{tokenCI: "ci-token"})
	a.opts.Stdout, a.opts.Version = &out, "v0.2.0"
	if err := a.CI(context.Background(), "github", CIOptions{Repo: "web-app"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `          WRIT_VERSION: "v0.2.0"`+"\n") {
		t.Fatalf("the workflow installs another release:\n%s", out.String())
	}
}

// TestCIRefusals checks what writ ci checks before it writes anything:
// the repository is registered and of the kind asked for, it has a served
// stack, and the release to install is one.
func TestCIRefusals(t *testing.T) {
	none := webRepo("github")
	none.Stacks = []server.FoundStack{{Stack: "web/unserved"}}
	ghe := webRepo("github")
	ghe.Repo.ForgeURL = "https://ghe.example.com"
	huge := webRepo("github")
	huge.Stacks = nil
	for i := range 129 {
		huge.Stacks = append(huge.Stacks, server.FoundStack{Stack: fmt.Sprintf("web/s%d", i), Served: []string{"prod", "staging"}})
	}
	for _, tc := range []struct {
		name, kind string
		doc        server.RepoDoc
		o          CIOptions
		want       string
	}{
		{"an unregistered repository", "github", webRepo("github"), CIOptions{Repo: "nope", WritVersion: "v0.1.9"}, "no repository"},
		{"a GitLab project", "github", webRepo("gitlab"), CIOptions{Repo: "web-app", WritVersion: "v0.1.9"}, "repository web-app is a gitlab repository, not a GitHub repository"},
		{"a GitHub repository for GitLab CI", "gitlab", webRepo("github"), CIOptions{Repo: "web-app", Component: DefaultComponent}, "repository web-app is a github repository, not a GitLab project"},
		{"no served stack", "github", none, CIOptions{Repo: "web-app", WritVersion: "v0.1.9"}, "has no stack this control plane serves"},
		{"an environment it does not serve", "github", webRepo("github"), CIOptions{Repo: "web-app", Envs: []string{"dev"}, WritVersion: "v0.1.9"}, "has no stack this control plane serves in dev"},
		{"a version that is no release", "github", webRepo("github"), CIOptions{Repo: "web-app", WritVersion: "dev; curl evil"}, "is not a Writ release"},
		{"another kind", "bitbucket", webRepo("github"), CIOptions{Repo: "web-app"}, "ci takes gitlab or github"},
		{"a release before the workflow's least", "github", webRepo("github"), CIOptions{Repo: "web-app", WritVersion: "v0.1.8"}, "writ v0.1.8 cannot run the workflow, which needs the GitHub Actions token fetch and --slot-wait of v0.1.9 or later"},
		{"a pre-release", "github", webRepo("github"), CIOptions{Repo: "web-app", WritVersion: "v0.2.0-rc1"}, "is not a Writ release, such as v0.1.9"},
		{"leading zeros", "github", webRepo("github"), CIOptions{Repo: "web-app", WritVersion: "v0.01.009"}, `--writ-version "v0.01.009" is not a Writ release`},
		{"no flag on a build of a commit", "github", webRepo("github"), CIOptions{Repo: "web-app"}, `this writ is "1a8c8a3", not a release, so writ ci github cannot tell which release the workflow should install: name one with --writ-version, v0.1.9 or later`},
		{"an enterprise server with no runner label", "github", ghe, CIOptions{Repo: "web-app", WritVersion: "v0.1.9"}, "repository web-app is at https://ghe.example.com, not github.com, so writ ci github cannot know its runners' labels: name the label of a Linux runner with --runs-on LABEL"},
		{"a matrix over 256 jobs", "github", huge, CIOptions{Repo: "web-app", WritVersion: "v0.1.9"}, "has 258 stacks and environments to plan, over the 256 jobs a GitHub Actions matrix makes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := repoServer(t, tc.doc)
			var out, errOut bytes.Buffer
			a := precedenceApp(t, t.TempDir(), srv.URL, map[string]string{tokenCI: "ci-token"})
			a.opts.Stdout, a.opts.Stderr = &out, &errOut
			// A Nix build of a commit says its short hash.
			a.opts.Version = "1a8c8a3"
			err := a.CI(context.Background(), tc.kind, tc.o)
			if err == nil || !strings.Contains(err.Error()+out.String()+errOut.String(), tc.want) {
				t.Fatalf("got %v, %s %s; want %q", err, out.String(), errOut.String(), tc.want)
			}
			if strings.Contains(out.String(), "jobs:") {
				t.Fatalf("a refusal printed a workflow:\n%s", out.String())
			}
		})
	}
}
