package forge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/forge/forgetest"
)

const ghToken = "github_pat_forgetest_0123456789"

func TestGitHubAPI(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://github.com", "https://api.github.com"},
		{"https://github.com/", "https://api.github.com"},
		{"https://GitHub.com", "https://api.github.com"},
		{"https://ghe.example.com", "https://ghe.example.com/api/v3"},
		{"https://ghe.example.com:8443", "https://ghe.example.com:8443/api/v3"},
		{"http://127.0.0.1:9000", "http://127.0.0.1:9000/api/v3"},
		// Only https://github.com is github.com's API: another scheme or
		// port is a server of its own.
		{"http://github.com", "http://github.com/api/v3"},
	} {
		if got, err := forge.GitHubAPI(tc.in); err != nil || got != tc.want {
			t.Errorf("GitHubAPI(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "github.com", "https://github.com/acme", "https://@github.com", "https://github.com?x=1"} {
		if got, err := forge.GitHubAPI(bad); err == nil {
			t.Errorf("GitHubAPI(%q) = %q", bad, got)
		}
	}
}

func newGitHub(t *testing.T, url, tok string) *forge.GitHub {
	t.Helper()
	c, err := forge.NewGitHub(forge.GitHubOptions{URL: url, TokenFile: tokenFile(t, tok)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGitHub(t *testing.T) {
	ctx := context.Background()
	g := forgetest.NewGitHub(t, ghToken)
	g.AddProject(4242, "acme/infra", "main")
	files := map[string]forgetest.File{"README.md": {Content: "hi\n"}, "infra/writ.estate": {Content: "{}\n"}, "infra/deep/net/main.tf": {Content: "# net\n"}}
	for i := range 7 {
		files["infra/s"+string(rune('a'+i))+"/main.tf"] = forgetest.File{Content: "# " + string(rune('a'+i)) + "\n"}
	}
	first := g.Commit(4242, "main", files)
	files["infra/sa/main.tf"] = forgetest.File{Content: "# changed\n"}
	sha := g.Commit(4242, "main", files)
	other := g.Commit(4242, "other", map[string]forgetest.File{"x": {Content: "x"}})
	c := newGitHub(t, g.URL, ghToken)

	for _, ref := range []string{"acme/infra", "4242"} {
		p, err := c.Project(ctx, ref)
		if err != nil || p.ID != "4242" || p.Path != "acme/infra" || p.DefaultBranch != "main" || p.WebURL != g.URL+"/acme/infra" {
			t.Fatalf("project %s: %+v, %v", ref, p, err)
		}
	}
	if head, err := c.Head(ctx, "4242", "main"); err != nil || head != sha {
		t.Fatalf("head %s, %v", head, err)
	}
	if got, err := c.Commit(ctx, "4242", sha); err != nil || got != sha {
		t.Fatalf("commit %s, %v", got, err)
	}
	for _, tc := range []struct {
		name, sha string
		want      bool
	}{{"the head", sha, true}, {"an ancestor", first, true}, {"another branch's commit", other, false}} {
		if on, err := c.OnRef(ctx, "4242", tc.sha, "refs/heads/main"); err != nil || on != tc.want {
			t.Errorf("on ref, %s: %v, %v", tc.name, on, err)
		}
	}
	if on, err := c.OnRef(ctx, "4242", sha, "refs/heads/gone"); err != nil || on {
		t.Errorf("on a ref that does not exist: %v, %v", on, err)
	}

	g.Calls()
	tree, err := c.Tree(ctx, "4242", sha, "infra")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range tree {
		names = append(names, f.Path)
	}
	slices.Sort(names)
	if len(names) != 9 || names[0] != "deep/net/main.tf" || names[8] != "writ.estate" {
		t.Fatalf("tree %v", names)
	}
	// The commit, the root's tree to find infra, and infra's tree.
	if calls := g.Calls(); len(calls) != 3 || !strings.Contains(calls[0], "/repositories/4242/git/commits/") || !strings.Contains(calls[2], "/git/trees/") {
		t.Fatalf("the tree took the calls %v", calls)
	}
	deep, err := c.Tree(ctx, "4242", sha, "infra/deep")
	if err != nil || len(deep) != 1 || deep[0].Path != "net/main.tf" {
		t.Fatalf("a nested directory: %+v, %v", deep, err)
	}
	if _, err := c.Tree(ctx, "4242", sha, "nope"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("a directory the commit lacks: %v", err)
	}
	b, err := c.Blob(ctx, "4242", tree[0].Blob, 1<<20)
	if err != nil || forge.BlobID(b) != tree[0].Blob {
		t.Fatalf("blob %q, %v", b, err)
	}
	if _, err := c.Blob(ctx, "4242", tree[0].Blob, 2); err == nil || !strings.Contains(err.Error(), "over 2 bytes") {
		t.Fatalf("a blob over the bound: %v", err)
	}

	// A repository that is not there and one the token may not see answer
	// alike: 404, never the forge's text.
	for _, tc := range []struct {
		name string
		call func() error
		want string
	}{
		{"no repository", func() error { _, err := c.Project(ctx, "acme/other"); return err }, "read repository acme/other: the forge answered 404; it has no such thing, or the token may not see it"},
		{"no repository by id", func() error { _, err := c.Project(ctx, "999"); return err }, "the forge answered 404"},
		{"no branch", func() error { _, err := c.Head(ctx, "4242", "nope"); return err }, "read refs/heads/nope: the forge answered 404"},
		{"no commit", func() error { _, err := c.Commit(ctx, "4242", strings.Repeat("0", 40)); return err }, "the forge answered 404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, forge.ErrNotFound) || strings.Contains(err.Error(), "Not Found") {
				t.Fatalf("got %v", err)
			}
		})
	}
	if _, err := c.Project(ctx, "acme/infra/sub"); err == nil || !strings.Contains(err.Error(), "must be OWNER/REPO") {
		t.Fatalf("a path of three parts: %v", err)
	}
	bad := newGitHub(t, g.URL, "github_pat_wrong")
	if _, err := bad.Project(ctx, "acme/infra"); err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "check the forge's read token") || strings.Contains(err.Error(), "github_pat") || strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("a wrong token: %v", err)
	}

	// A symbolic link under the path is refused; one elsewhere is not read.
	link := g.Commit(4242, "links", map[string]forgetest.File{"infra/a/main.tf": {Content: "x"}, "infra/b": {Content: "a", Mode: "120000"}, "docs/c": {Content: "x", Mode: "120000"}})
	if _, err := c.Tree(ctx, "4242", link, "infra"); err == nil || !strings.Contains(err.Error(), "infra/b is a symbolic link") {
		t.Fatalf("a symbolic link: %v", err)
	}
	if _, err := c.Tree(ctx, "4242", link, "infra/a"); err != nil {
		t.Fatalf("a directory beside a link: %v", err)
	}
}

// hostileGitHub is a GitHub that answers every call with h.
func hostileGitHub(t *testing.T, h http.HandlerFunc) *forge.GitHub {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return newGitHub(t, s.URL, ghToken)
}

// TestGitHubNeverRepeatsTheForge checks that no error holds text the
// forge sent, and that an answer holding the token is refused.
func TestGitHubNeverRepeatsTheForge(t *testing.T) {
	ctx := context.Background()
	escaped := strings.ReplaceAll(ghToken, "g", `g`)
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"a refusal that echoes the token", 403, `{"message":"echo ` + ghToken + `"}`, "the forge answered 403; the token needs Contents: read and Metadata: read"},
		{"a refusal as plain text", 404, "no such repository for " + ghToken, "the forge answered 404"},
		{"a refusal of the token", 401, `{"message":"Bad credentials ` + ghToken + `"}`, "the forge answered 401; it refused the read token"},
		{"a redirect", 301, `{"message":"Moved Permanently","url":"https://evil.example.com/` + ghToken + `"}`, "the forge answered 301; writ serve follows no redirect"},
		{"a failure", 502, "<html>FORGE-INTERNAL-TEXT</html>", "the forge answered 502; the forge failed"},
		{"an answer that holds the token", 200, `{"id":1,"full_name":"` + ghToken + `","default_branch":"main"}`, "the forge's answer holds the read token"},
		{"an answer that spells the token in escapes", 200, `{"id":1,"full_name":"` + escaped + `","default_branch":"main"}`, "the forge's answer holds the read token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := hostileGitHub(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 301 {
					w.Header().Set("Location", "https://evil.example.com/")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := c.Project(ctx, "acme/infra")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			for _, leak := range []string{ghToken, "echo", "Bad credentials", "no such repository for", "FORGE-INTERNAL-TEXT", "evil"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("the error repeats the forge's text %q: %v", leak, err)
				}
			}
		})
	}
	c := hostileGitHub(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "let x = \""+ghToken+"\"\n") })
	if _, err := c.Blob(ctx, "1", strings.Repeat("a", 40), 1<<20); err == nil || strings.Contains(err.Error(), ghToken) || !strings.Contains(err.Error(), "holds the read token") {
		t.Fatalf("a blob that holds the token: %v", err)
	}
}

// TestGitHubSendsTheToken checks the headers each call carries.
func TestGitHubSendsTheToken(t *testing.T) {
	var got http.Header
	c := hostileGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, `{"id":7,"full_name":"acme/infra","default_branch":"main","html_url":"https://ghe.example.com/acme/infra"}`)
	})
	if _, err := c.Project(context.Background(), "acme/infra"); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer "+ghToken || got.Get("X-GitHub-Api-Version") != "2022-11-28" || got.Get("PRIVATE-TOKEN") != "" {
		t.Fatalf("headers %v", got)
	}
}

// TestGitHubRateLimit checks that an answer that limits the rate of calls
// is a refusal that says until when, and that the client sends no call
// before then.
func TestGitHubRateLimit(t *testing.T) {
	ctx := context.Background()
	reset := time.Now().Add(90 * time.Second).Unix()
	for _, tc := range []struct {
		name   string
		status int
		hdr    map[string]string
		until  time.Duration
	}{
		{"a primary limit", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset, 10)}, 90 * time.Second},
		{"a secondary limit", 403, map[string]string{"Retry-After": "30"}, 30 * time.Second},
		{"a 429 with retry-after", 429, map[string]string{"Retry-After": "45"}, 45 * time.Second},
		{"a 429 with neither", 429, nil, time.Minute},
		{"a reset far off is held to an hour", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10)}, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := hostileGitHub(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				for k, v := range tc.hdr {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for `+ghToken+`"}`)
			})
			_, err := c.Head(ctx, "4242", "main")
			rl, ok := errors.AsType[*forge.RateLimitError](err)
			if !ok || rl.Status != tc.status || !strings.Contains(err.Error(), "limits the rate of calls until") || strings.Contains(err.Error(), "exceeded") {
				t.Fatalf("got %v", err)
			}
			if d := time.Until(rl.Until); d < tc.until-5*time.Second || d > tc.until+time.Second {
				t.Fatalf("held back for %v, want about %v", d, tc.until)
			}
			_, err = c.Project(ctx, "acme/infra")
			if rl, ok := errors.AsType[*forge.RateLimitError](err); !ok || rl.Status != 0 || calls.Load() != 1 {
				t.Fatalf("the next call: %v, after %d calls", err, calls.Load())
			}
		})
	}
	// A 403 that is not a limit is the token's refusal, and holds nothing
	// back.
	var calls atomic.Int32
	c := hostileGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(403)
	})
	for range 2 {
		if _, err := c.Project(ctx, "acme/infra"); err == nil || !strings.Contains(err.Error(), "the token needs") {
			t.Fatalf("a plain 403: %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("a plain 403 held calls back: %d calls", calls.Load())
	}
}

// TestGitHubTreeBounds checks that a tree GitHub cut short, one of over
// MaxEntries, and one over the bytes an answer may take are refused.
func TestGitHubTreeBounds(t *testing.T) {
	sha, tree := strings.Repeat("b", 40), strings.Repeat("c", 40)
	entries := func(n int) []map[string]string {
		out := make([]map[string]string, n)
		for i := range out {
			out[i] = map[string]string{"sha": strings.Repeat("a", 40), "type": "blob", "mode": "100644", "path": fmt.Sprintf("f%d.tf", i)}
		}
		return out
	}
	for _, tc := range []struct {
		name string
		doc  any
		want string
	}{
		{"cut short", map[string]any{"sha": tree, "truncated": true, "tree": entries(3)}, "GitHub cut the listing short"},
		{"over the entries", map[string]any{"sha": tree, "tree": entries(forge.MaxEntries + 1)}, "it lists over 20000 entries"},
		{"over the bytes", map[string]any{"sha": tree, "tree": entries(forge.MaxEntries), "pad": strings.Repeat("x", 8<<20)}, "the answer is over 8388608 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := hostileGitHub(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/git/commits/") {
					fmt.Fprintf(w, `{"sha":%q,"tree":{"sha":%q}}`, sha, tree)
					return
				}
				_ = json.NewEncoder(w).Encode(tc.doc)
			})
			if _, err := c.Tree(context.Background(), "1", sha, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// A GitHub that answers with another commit or another ref than the one
// asked for must not swap its sources into a plan.
func TestGitHubAnswersWhatWasAskedFor(t *testing.T) {
	asked, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	c := hostileGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git/ref/") {
			fmt.Fprintf(w, `{"ref":"refs/heads/main-evil","object":{"sha":%q,"type":"commit"}}`, other)
			return
		}
		fmt.Fprintf(w, `{"sha":%q,"tree":{"sha":%q}}`, other, other)
	})
	if got, err := c.Commit(context.Background(), "4242", asked); err == nil || !strings.Contains(err.Error(), "names another commit") {
		t.Fatalf("another commit: %q, %v", got, err)
	}
	if got, err := c.Commit(context.Background(), "4242", asked[:12]); err == nil || !strings.Contains(err.Error(), "not a full SHA-1") {
		t.Fatalf("a short id: %q, %v", got, err)
	}
	if got, err := c.Head(context.Background(), "4242", "main"); err == nil || !strings.Contains(err.Error(), "names no commit of that ref") {
		t.Fatalf("another ref: %q, %v", got, err)
	}
}
