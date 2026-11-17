package forge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/forge/forgetest"
)

const token = "glpat-forgetest-0123456789"

func tokenFile(t *testing.T, tok string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gitlab_read")
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBlobID(t *testing.T) {
	// git hash-object of an empty file, and of "hello\n".
	if got := forge.BlobID(nil); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Errorf("empty: %s", got)
	}
	if got := forge.BlobID([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Errorf("hello: %s", got)
	}
}

func TestGitLab(t *testing.T) {
	ctx := context.Background()
	g := forgetest.New(t, token)
	g.AddProject(4242, "acme/infra", "main")
	files := map[string]forgetest.File{"README.md": {Content: "hi\n"}, "infra/writ.estate": {Content: "{}\n"}}
	for i := range 7 {
		files["infra/s"+string(rune('a'+i))+"/main.writ"] = forgetest.File{Content: "// " + string(rune('a'+i)) + "\n"}
	}
	sha := g.Commit(4242, "main", files)
	g.PageSize(3)
	c, err := forge.NewGitLab(forge.GitLabOptions{URL: g.URL, TokenFile: tokenFile(t, token)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Project(ctx, "acme/infra")
	if err != nil || p.ID != "4242" || p.Path != "acme/infra" || p.DefaultBranch != "main" || p.WebURL != g.URL+"/acme/infra" {
		t.Fatalf("project %+v, %v", p, err)
	}
	if head, err := c.Head(ctx, p.ID, "main"); err != nil || head != sha {
		t.Fatalf("head %s, %v", head, err)
	}
	if got, err := c.Commit(ctx, p.ID, sha); err != nil || got != sha {
		t.Fatalf("commit %s, %v", got, err)
	}
	g.Calls()
	tree, err := c.Tree(ctx, p.ID, sha, "infra")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range tree {
		names = append(names, f.Path)
	}
	slices.Sort(names)
	if len(names) != 8 || names[0] != "sa/main.writ" || names[7] != "writ.estate" {
		t.Fatalf("tree %v", names)
	}
	if calls := g.Calls(); len(calls) < 4 {
		t.Fatalf("the tree came in %d pages of 3: %v", len(calls), calls)
	}
	b, err := c.Blob(ctx, p.ID, tree[0].Blob, 1<<20)
	if err != nil || forge.BlobID(b) != tree[0].Blob {
		t.Fatalf("blob %q, %v", b, err)
	}
	if _, err := c.Blob(ctx, p.ID, tree[0].Blob, 2); err == nil || !strings.Contains(err.Error(), "over 2 bytes") {
		t.Fatalf("a blob over the bound: %v", err)
	}

	// Refusals carry the status and Writ's reason, never the forge's text.
	for _, tc := range []struct {
		name string
		call func(c *forge.GitLab) error
		want string
		nf   bool
	}{
		{"no project", func(c *forge.GitLab) error { _, err := c.Project(ctx, "acme/other"); return err }, "read project acme/other: the forge answered 404; it has no such thing", true},
		{"no branch", func(c *forge.GitLab) error { _, err := c.Head(ctx, "4242", "nope"); return err }, "read branch nope: the forge answered 404", true},
		{"no commit", func(c *forge.GitLab) error {
			_, err := c.Commit(ctx, "4242", strings.Repeat("0", 40))
			return err
		}, "the forge answered 404", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, forge.ErrNotFound) != tc.nf || strings.Contains(err.Error(), "Not Found") {
				t.Fatalf("got %v", err)
			}
		})
	}
	bad, _ := forge.NewGitLab(forge.GitLabOptions{URL: g.URL, TokenFile: tokenFile(t, "glpat-wrong")})
	if _, err := bad.Project(ctx, "acme/infra"); err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "check the forge's read token") || strings.Contains(err.Error(), "glpat") {
		t.Fatalf("a wrong token: %v", err)
	}

	// A symbolic link under the path is refused; one elsewhere is not read.
	link := g.Commit(4242, "links", map[string]forgetest.File{"infra/a/main.writ": {Content: "x"}, "infra/b": {Content: "a", Mode: "120000"}, "docs/c": {Content: "x", Mode: "120000"}})
	if _, err := c.Tree(ctx, "4242", link, "infra"); err == nil || !strings.Contains(err.Error(), "infra/b is a symbolic link") {
		t.Fatalf("a symbolic link: %v", err)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Customer A", "GIT_AUTHOR_EMAIL=user@example.com", "GIT_COMMITTER_NAME=Customer A",
		"GIT_COMMITTER_EMAIL=user@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLocal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "trunk")
	for name, text := range map[string]string{"infra/writ.estate": "{}\n", "infra/net/main.writ": "// net\n", "README.md": "hi\n"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "first")
	sha := git(t, dir, "rev-parse", "HEAD")
	var l forge.Local
	p, err := l.Project(ctx, filepath.Join(dir, "infra"))
	top, _ := filepath.EvalSymlinks(dir)
	if err != nil || p.ID != top || p.DefaultBranch != "trunk" {
		t.Fatalf("project %+v, %v", p, err)
	}
	if head, err := l.Head(ctx, p.ID, "trunk"); err != nil || head != sha {
		t.Fatalf("head %s, %v", head, err)
	}
	if _, err := l.Commit(ctx, p.ID, strings.Repeat("1", 40)); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("a missing commit: %v", err)
	}
	// Uncommitted edits are not part of the commit.
	if err := os.WriteFile(filepath.Join(dir, "infra", "net", "main.writ"), []byte("// edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, err := l.Tree(ctx, p.ID, sha, "infra")
	if err != nil || len(tree) != 2 || tree[0].Path != "net/main.writ" || tree[1].Path != "writ.estate" {
		t.Fatalf("tree %+v, %v", tree, err)
	}
	b, err := l.Blob(ctx, p.ID, tree[0].Blob, 1<<20)
	if err != nil || string(b) != "// net\n" {
		t.Fatalf("blob %q, %v", b, err)
	}

	// A bare clone is read the same way.
	bare := filepath.Join(t.TempDir(), "infra.git")
	git(t, dir, "clone", "-q", "--bare", dir, bare)
	bp, err := l.Project(ctx, bare)
	if err != nil || bp.DefaultBranch != "trunk" {
		t.Fatalf("bare project %+v, %v", bp, err)
	}
	if head, err := l.Head(ctx, bp.ID, "trunk"); err != nil || head != sha {
		t.Fatalf("bare head %s, %v", head, err)
	}
	if _, err := l.Project(ctx, t.TempDir()); err == nil || !strings.Contains(err.Error(), "not in a Git repository") {
		t.Fatalf("not a repository: %v", err)
	}
}

// TestLocalWithoutGit checks that a machine with no git binary, as after
// a plain install of the release binary, gets the error that says how to
// install it, not one that blames the directory.
func TestLocalWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var l forge.Local
	ctx, dir, sha := context.Background(), t.TempDir(), strings.Repeat("a", 40)
	for name, call := range map[string]func() error{
		"project": func() error { _, err := l.Project(ctx, dir); return err },
		"head":    func() error { _, err := l.Head(ctx, dir, "main"); return err },
		"commit":  func() error { _, err := l.Commit(ctx, dir, sha); return err },
		"on ref":  func() error { _, err := l.OnRef(ctx, dir, sha, "refs/heads/main"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, forge.ErrNoGit) || !strings.Contains(err.Error(), "install git") {
				t.Fatalf("%s without git: %v", name, err)
			}
		})
	}
}

// hostile is a forge that answers every call with h, and records nothing.
func hostile(t *testing.T, h http.HandlerFunc) *forge.GitLab {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c, err := forge.NewGitLab(forge.GitLabOptions{URL: s.URL, TokenFile: tokenFile(t, token)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestGitLabNeverRepeatsTheForge checks that no error holds text the
// forge sent, so a forge that echoes the token cannot make Writ print it.
func TestGitLabNeverRepeatsTheForge(t *testing.T) {
	ctx := context.Background()
	escaped := strings.ReplaceAll(token, "g", `\u0067`)
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"a refusal that echoes the token", 403, `{"message":"echo ` + token + `"}`, "the forge answered 403; the token needs read_api"},
		{"a refusal as plain text", 404, "no such project for " + token, "the forge answered 404"},
		{"a refusal with an error field", 401, `{"error":"bad token ` + token + `"}`, "the forge answered 401; it refused the read token"},
		{"a failure", 502, "<html>FORGE-INTERNAL-TEXT</html>", "the forge answered 502; the forge failed"},
		{"an answer that holds the token", 200, `{"id":1,"path_with_namespace":"` + token + `","default_branch":"main"}`, "the forge's answer holds the read token"},
		{"an answer that spells the token in escapes", 200, `{"id":1,"path_with_namespace":"` + escaped + `","default_branch":"main"}`, "the forge's answer holds the read token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := hostile(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := c.Project(ctx, "acme/infra")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			for _, leak := range []string{token, "echo", "bad token", "no such project for", "FORGE-INTERNAL-TEXT"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("the error repeats the forge's text %q: %v", leak, err)
				}
			}
		})
	}
	// A blob is checked too, so the token never reaches a plan's sources.
	c := hostile(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "let x = \""+token+"\"\n") })
	if _, err := c.Blob(ctx, "1", strings.Repeat("a", 40), 1<<20); err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "holds the read token") {
		t.Fatalf("a blob that holds the token: %v", err)
	}
}

// TestGitLabTreeBounds checks that the tree's bound counts the entries the
// forge sends, not the pages it was asked for, and that one answer is
// bounded in bytes.
func TestGitLabTreeBounds(t *testing.T) {
	ctx := context.Background()
	entries := func(n int) []map[string]string {
		out := make([]map[string]string, n)
		for i := range out {
			out[i] = map[string]string{"id": strings.Repeat("a", 40), "type": "blob", "mode": "100644", "path": fmt.Sprintf("f%d.writ", i)}
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		h     http.HandlerFunc
		want  string
		pages int
	}{
		{"one page of 20,001 entries", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(entries(20001))
		}, "the answer is over 1048576 bytes", 1},
		{"pages that hold more than asked", func(w http.ResponseWriter, r *http.Request) {
			// 150 entries a page: the 134th page passes 20,000 entries, well
			// before the 201 pages that a count of pages of 100 allows.
			w.Header().Set("Link", `<http://forge.invalid/next?page_token=x>; rel="next"`)
			_ = json.NewEncoder(w).Encode(entries(150))
		}, "it lists over 20000 entries", 134},
		{"empty pages with no end", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", `<http://forge.invalid/next?page_token=x>; rel="next"`)
			_, _ = io.WriteString(w, "[]")
		}, "it takes over 201 pages", 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pages atomic.Int32
			_, err := hostile(t, func(w http.ResponseWriter, r *http.Request) { pages.Add(1); tc.h(w, r) }).Tree(ctx, "1", strings.Repeat("b", 40), "")
			if err == nil || !strings.Contains(err.Error(), tc.want) || int(pages.Load()) != tc.pages {
				t.Fatalf("got %v after %d pages, want %q after %d", err, pages.Load(), tc.want, tc.pages)
			}
		})
	}
}

// A forge that answers with another commit than the one asked for must not
// swap that commit's sources into a plan.
func TestGitLabCommitIsTheOneAskedFor(t *testing.T) {
	asked, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q}`, other)
	}))
	defer srv.Close()
	c, err := forge.NewGitLab(forge.GitLabOptions{URL: srv.URL, TokenFile: tokenFile(t, token)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, sha, want string
	}{
		{"another commit", asked, "names another commit"},
		{"a short id", asked[:12], "not a full SHA-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Commit(context.Background(), "4242", tc.sha)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}
