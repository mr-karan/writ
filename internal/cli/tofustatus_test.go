package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

// A plan of an OpenTofu stack beneath --dir is of the commit only when git
// status finds no entry in the stack's directory or the directory of a
// local module it calls. The fixture's stack calls ../module.
func TestMCPTofuPlanAsksGitStatus(t *testing.T) {
	cases := []struct {
		name string
		// attrs, when set, is the committed .gitattributes.
		attrs  string
		change func(t *testing.T, repo string)
		// want is the files the refusal names, "" for a plan of the commit.
		want string
	}{
		{name: "a clean work tree"},
		{name: "an executable bit", change: func(t *testing.T, repo string) {
			if err := os.Chmod(filepath.Join(repo, "stack", "main.tf"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, want: "in 1 file (stack/main.tf)"},
		{name: "a file replaced by a link to the same bytes", change: func(t *testing.T, repo string) {
			if err := os.Rename(filepath.Join(repo, "stack", "main.tf"), filepath.Join(repo, "copy")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../copy", filepath.Join(repo, "stack", "main.tf")); err != nil {
				t.Fatal(err)
			}
		}, want: "in 1 file (stack/main.tf)"},
		{name: "a new file in a local module", change: func(t *testing.T, repo string) {
			write(t, filepath.Join(repo, "module", "extra.tf"), "# new module code\n")
		}, want: "in 1 file (module/extra.tf)"},
		{name: "a clean CRLF checkout", attrs: "*.tf text eol=crlf\n", change: func(t *testing.T, repo string) {
			for _, p := range []string{"stack/main.tf", "module/main.tf"} {
				if err := os.Remove(filepath.Join(repo, p)); err != nil {
					t.Fatal(err)
				}
			}
			gitRun(t, repo, "checkout", "--", ".")
			if b, err := os.ReadFile(filepath.Join(repo, "stack", "main.tf")); err != nil || string(b) != strings.ReplaceAll(tofuStatusStack, "\n", "\r\n") {
				t.Fatalf("git checked out %q, %v", b, err)
			}
		}},
		{name: "an edit outside the stack and its modules", change: func(t *testing.T, repo string) {
			write(t, filepath.Join(repo, "README.md"), "edited\n")
			write(t, filepath.Join(repo, "other", "main.tf"), "# another stack\n")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := filepath.Join(gitBase(t), "root")
			write(t, filepath.Join(repo, "stack", "main.tf"), tofuStatusStack)
			write(t, filepath.Join(repo, "module", "main.tf"), "# module\n")
			write(t, filepath.Join(repo, "README.md"), "readme\n")
			if c.attrs != "" {
				write(t, filepath.Join(repo, ".gitattributes"), c.attrs)
			}
			gitRun(t, repo, "init", "-q")
			gitRun(t, repo, "add", ".")
			gitRun(t, repo, "commit", "-qm", "first")
			head := gitRun(t, repo, "rev-parse", "HEAD")
			if c.change != nil {
				c.change(t, repo)
			}
			plane, planned := tofuStatusPlane(t, repo)
			text, isError := callTool(t, serveMCP(t, stubOptions(repo, plane)), "plan", map[string]any{"dir": "stack", "env": "prod"})
			if c.want == "" {
				if !strings.Contains(text, "refused by the stub") || planned.Load() != 1 {
					t.Fatalf("no plan of the commit, %d plans sent: %s", planned.Load(), text)
				}
				return
			}
			if !isError || !strings.Contains(text, "the work tree differs from commit "+head[:8]+" "+c.want+"; commit it") || planned.Load() != 0 {
				t.Fatalf("isError %v, %d plans sent: %s", isError, planned.Load(), text)
			}
		})
	}
}

const tofuStatusStack = "resource \"random_pet\" \"name\" {}\nmodule \"shared\" {\n  source = \"../module\"\n}\n"

// tofuStatusPlane is a control plane that lists repo as a dir repository
// and refuses each plan, which it counts.
func tofuStatusPlane(t *testing.T, repo string) (string, *atomic.Int32) {
	t.Helper()
	repos, err := json.Marshal(server.ReposDoc{Format: server.FormatRepos, OK: true, Diagnostics: []server.Diagnostic{}, Repos: []server.RepoInfo{{Name: "r", Kind: "dir", ProjectID: repo}}})
	if err != nil {
		t.Fatal(err)
	}
	var planned atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/repos":
			w.Write(repos)
		case "/v1/plans":
			planned.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"format":"writ.remote-plan/v0","ok":false,"plan":null,"document":null,"text":"","diagnostics":[{"severity":"error","class":"usage","message":"refused by the stub"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &planned
}

// An agent can write .git/config, .gitattributes, and .git/info/attributes.
// git status of an OpenTofu stack re-reads a file whose stat data changed,
// and runs no filter program those name, even one whose driver's name
// holds an =, which -c cannot spell.
func TestMCPTofuStatusRunsNoPlantedFilter(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, repo, script string)
	}{
		{"clean filter", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "filter.x.clean", script)
			write(t, filepath.Join(repo, ".gitattributes"), "*.tf filter=x\n")
			gitRun(t, repo, "add", ".gitattributes")
			gitRun(t, repo, "commit", "-qm", "attrs")
		}},
		{"process filter in info/attributes", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "filter.x.process", script)
			write(t, filepath.Join(repo, ".git", "info", "attributes"), "*.tf filter=x\n")
		}},
		{"a driver whose name holds =", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "filter.a=b.clean", script)
			write(t, filepath.Join(repo, ".git", "info", "attributes"), "*.tf filter=a=b\n")
		}},
		{"a required clean filter", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "filter.x.clean", script)
			gitRun(t, repo, "config", "filter.x.required", "true")
			write(t, filepath.Join(repo, ".git", "info", "attributes"), "*.tf filter=x\n")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := gitBase(t)
			repo, marker := filepath.Join(base, "root"), filepath.Join(base, "MARKER")
			write(t, filepath.Join(repo, "stack", "main.tf"), tofuStatusStack)
			write(t, filepath.Join(repo, "module", "main.tf"), "# module\n")
			gitRun(t, repo, "init", "-q")
			gitRun(t, repo, "add", ".")
			gitRun(t, repo, "commit", "-qm", "first")
			c.plant(t, repo, markerScript(t, base, marker))
			future := time.Now().Add(time.Hour)
			for _, p := range []string{"stack/main.tf", "module/main.tf"} {
				if err := os.Chtimes(filepath.Join(repo, p), future, future); err != nil {
					t.Fatal(err)
				}
			}
			os.Remove(marker)
			plane, planned := tofuStatusPlane(t, repo)
			text, _ := callTool(t, serveMCP(t, stubOptions(repo, plane)), "plan", map[string]any{"dir": "stack", "env": "prod"})
			noMarker(t, marker, "the plan tool")
			if planned.Load() != 1 {
				t.Fatalf("no plan of the commit: %s", text)
			}
		})
	}
}

// git status reports no edit to a file the index marks assume-unchanged
// or skip-worktree, so a plan of an OpenTofu stack refuses such a mark in
// the stack or a local module it calls.
func TestMCPTofuStatusRefusesMarkedFiles(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			repo := filepath.Join(gitBase(t), "root")
			write(t, filepath.Join(repo, "stack", "main.tf"), tofuStatusStack)
			write(t, filepath.Join(repo, "module", "main.tf"), "# module\n")
			gitRun(t, repo, "init", "-q")
			gitRun(t, repo, "add", ".")
			gitRun(t, repo, "commit", "-qm", "first")
			gitRun(t, repo, "update-index", flag, "module/main.tf")
			write(t, filepath.Join(repo, "module", "main.tf"), "resource \"random_uuid\" \"uncommitted\" {}\n")
			plane, planned := tofuStatusPlane(t, repo)
			text, isError := callTool(t, serveMCP(t, stubOptions(repo, plane)), "plan", map[string]any{"dir": "stack", "env": "prod"})
			if !isError || !strings.Contains(text, "git status cannot see an edit to 1 file (module/main.tf), which the index marks assume-unchanged or skip-worktree") || planned.Load() != 0 {
				t.Fatalf("isError %v, %d plans sent: %s", isError, planned.Load(), text)
			}
		})
	}
}
