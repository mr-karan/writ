package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/confine"
	"github.com/mr-karan/writ/internal/server"
)

// gitRun runs git in dir with a fixed identity and no global config.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitRepo makes a repository at dir with one commit of stack/main.tf,
// and returns the commit.
func gitRepo(t *testing.T, dir, text string) string {
	t.Helper()
	write(t, filepath.Join(dir, "stack", "main.tf"), text)
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-qm", "first")
	return gitRun(t, dir, "rev-parse", "HEAD")
}

// reposRemote is a control plane that lists a dir repository for each
// project.
func reposRemote(t *testing.T, projects ...string) *remote {
	t.Helper()
	var repos []server.RepoInfo
	for i, p := range projects {
		repos = append(repos, server.RepoInfo{Name: fmt.Sprintf("r%d", i), Kind: "dir", ProjectID: p})
	}
	return reposRemoteOf(t, repos...)
}

// reposRemoteOf is a control plane that lists repos.
func reposRemoteOf(t *testing.T, repos ...server.RepoInfo) *remote {
	t.Helper()
	doc := server.ReposDoc{Format: server.FormatRepos, OK: true, Diagnostics: []server.Diagnostic{}, Repos: repos}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/repos" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return &remote{base: srv.URL, http: apiClient()}
}

// mcpApp returns the App writ mcp runs a tool on for the stack directory
// rel under root.
func mcpApp(t *testing.T, root, rel string) (*App, string) {
	t.Helper()
	files, err := confine.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	tools := &mcpTools{root: files.Dir(), base: Options{Files: files, LookupEnv: noEnv}}
	dir, err := tools.dir(rel)
	if err != nil {
		t.Fatal(err)
	}
	app, _, err := tools.app(rel, "")
	if err != nil {
		t.Fatal(err)
	}
	return app, dir
}

// Beneath --dir, the plan tool finds no Git work tree outside it: not one
// above --dir, and not one a directory is swapped for after the tool
// resolved it. Each would make the plan one of a commit the control plane
// has of that other repository.
func TestMCPWorkTreeStaysInsideDir(t *testing.T) {
	t.Run("a work tree above --dir", func(t *testing.T) {
		base, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		gitRepo(t, base, "out x = 1\n")
		write(t, filepath.Join(base, "root", "stack", "main.tf"), "out x = 1\n")
		gitRun(t, base, "add", ".")
		gitRun(t, base, "commit", "-qm", "root")
		app, dir := mcpApp(t, filepath.Join(base, "root"), "stack")
		wt := app.findWorkTree(context.Background(), reposRemote(t, base), dir)
		if wt.repo != nil || wt.source != nil {
			t.Fatalf("planned a commit of %s, above --dir: %+v", base, wt.source)
		}
		if want := "in no Git work tree beneath --dir"; !strings.Contains(wt.why, want) {
			t.Fatalf("why %q, want %q", wt.why, want)
		}
	})
	for _, link := range []string{"absolute", "relative"} {
		t.Run("a directory swapped for a link to another repository, "+link, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
			gitRepo(t, root, "out x = 1\n")
			outsideHead := gitRepo(t, filepath.Join(outside, "sub"), "out x = 2\n")
			app, dir := mcpApp(t, root, "stack")
			target := filepath.Join(outside, "sub", "stack")
			if link == "relative" {
				target = filepath.Join("..", "outside", "sub", "stack")
			}
			if err := os.Rename(filepath.Join(root, "stack"), filepath.Join(root, "held")); err != nil {
				t.Fatal(err)
			}
			symlink(t, target, filepath.Join(root, "stack"))
			wt := app.findWorkTree(context.Background(), reposRemote(t, filepath.Join(outside, "sub")), dir)
			if wt.repo != nil || wt.source != nil {
				t.Fatalf("planned commit %s of the repository outside --dir: %+v", outsideHead, wt.source)
			}
			if wt.why == "" || strings.Contains(wt.why, outside) {
				t.Fatalf("why %q: want a refusal that names nothing outside", wt.why)
			}
		})
	}
}

// git runs on the work tree and Git directory the plan tool opened. A
// work tree swapped for a link to another repository after that does not
// move it, as git -C on the work tree's path would.
func TestMCPGitRunsOnTheDirectoriesItOpened(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	head := gitRepo(t, filepath.Join(root, "repo"), "out x = 1\n")
	gitRepo(t, outside, "out x = 2\n")
	write(t, filepath.Join(outside, "OUTSIDE_UNTRACKED"), "")
	app, dir := mcpApp(t, root, filepath.Join("repo", "stack"))
	git, why := app.openGitTree(context.Background(), dir)
	if why != "" {
		t.Fatal(why)
	}
	defer git.close()
	if err := os.Rename(filepath.Join(root, "repo"), filepath.Join(root, "held")); err != nil {
		t.Fatal(err)
	}
	symlink(t, outside, filepath.Join(root, "repo"))
	if got, err := git.output(context.Background(), "rev-parse", "HEAD"); err != nil || got != head {
		t.Fatalf("HEAD %q, %v; want %s, the commit of the directory opened", got, err, head)
	}
	status, err := git.output(context.Background(), "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || strings.Contains(status, "OUTSIDE_UNTRACKED") {
		t.Fatalf("status %q, %v: git read the work tree outside", status, err)
	}
	if got, _ := gitOutput(context.Background(), git.top, "rev-parse", "HEAD"); got == head {
		t.Fatal("git -C on the path still reads the directory opened, so this test shows nothing")
	}
}
