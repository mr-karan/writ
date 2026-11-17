// Package forgetest serves a stand-in for GitLab's REST API in a test's
// process: the project, branch, commit, merge base, repository tree, and
// raw blob endpoints that writ serve reads, with the response shapes of
// GitLab's API documentation (docs.gitlab.com/api/projects, /api/branches, /api/commits,
// and /api/repositories, read 2026-11-10). Commits are made from maps of
// files. The stand-in records each call, so a test can say what was
// fetched, and can serve a tampered blob or a symbolic link.
package forgetest

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mr-karan/writ/internal/forge"
)

// GitLab is a running stand-in.
type GitLab struct {
	URL string
	// Token is the one token it accepts in PRIVATE-TOKEN.
	Token string

	mu       sync.Mutex
	projects map[string]*project
	blobs    map[string][]byte
	tampered map[string][]byte
	calls    []string
	// pageSize overrides per_page, so a test sees several pages.
	pageSize int
}

type project struct {
	id            int
	path          string
	defaultBranch string
	branches      map[string]string
	commits       map[string][]entry
	// parents maps a commit to the head its branch had before it.
	parents map[string]string
}

type entry struct {
	path, mode, blob string
}

// File is one file of a commit. Mode is 100644 when empty; 120000 is a
// symbolic link whose content is its target.
type File struct {
	Content string
	Mode    string
}

// New starts a stand-in that accepts token, and stops it when the test
// ends.
func New(t testing.TB, token string) *GitLab {
	g := Handler(token)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	g.URL = srv.URL
	return g
}

// Handler makes a stand-in to mount under /api/v4/ on another test
// server, such as an OIDC issuer's, as GitLab serves both from one host.
// The caller sets URL.
func Handler(token string) *GitLab {
	return &GitLab{Token: token, projects: map[string]*project{}, blobs: map[string][]byte{}, tampered: map[string][]byte{}}
}

// ServeHTTP answers the API's calls.
func (g *GitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.serve(w, r) }

// AddProject adds a project with no commits.
func (g *GitLab) AddProject(id int, path, defaultBranch string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.projects[strconv.Itoa(id)] = &project{id: id, path: path, defaultBranch: defaultBranch, branches: map[string]string{}, commits: map[string][]entry{}, parents: map[string]string{}}
}

// Commit makes a commit of files in the project and moves branch to it.
// Its parent is the branch's head before, if the branch had one. It
// returns the commit's id.
func (g *GitLab) Commit(projectID int, branch string, files map[string]File) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.projects[strconv.Itoa(projectID)]
	var entries []entry
	h := sha1.New()
	fmt.Fprintf(h, "%d %s %d\n", projectID, branch, len(p.commits))
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f := files[name]
		mode := f.Mode
		if mode == "" {
			mode = "100644"
		}
		id := forge.BlobID([]byte(f.Content))
		g.blobs[id] = []byte(f.Content)
		entries = append(entries, entry{path: name, mode: mode, blob: id})
		fmt.Fprintf(h, "%s %s %s\n", mode, id, name)
	}
	sha := hex.EncodeToString(h.Sum(nil))
	p.commits[sha] = entries
	if parent, ok := p.branches[branch]; ok {
		p.parents[sha] = parent
	}
	p.branches[branch] = sha
	return sha
}

// DeleteBranch removes a branch, as a merged merge request's source
// branch is removed. Its commits stay.
func (g *GitLab) DeleteBranch(projectID int, branch string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.projects[strconv.Itoa(projectID)].branches, branch)
}

// Tamper makes the stand-in serve content for blob, so the content no
// longer matches the id.
func (g *GitLab) Tamper(blob string, content string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tampered[blob] = []byte(content)
}

// PageSize sets the tree listing's page size.
func (g *GitLab) PageSize(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pageSize = n
}

// Calls returns the calls made since the last Calls, as "GET /path", and
// forgets them.
func (g *GitLab) Calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := g.calls
	g.calls = nil
	return out
}

func (g *GitLab) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, r.Method+" "+r.URL.EscapedPath())
	if r.Header.Get("PRIVATE-TOKEN") != g.Token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4/"), "/")
	if len(parts) < 2 || parts[0] != "projects" || r.Method != http.MethodGet {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "404 Not Found"})
		return
	}
	ref, _ := url.PathUnescape(parts[1])
	p := g.projects[ref]
	if p == nil {
		for _, q := range g.projects {
			if q.path == ref {
				p = q
			}
		}
	}
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	rest := parts[2:]
	switch {
	case len(rest) == 0:
		g.project(w, p)
	case len(rest) == 3 && rest[0] == "repository" && rest[1] == "branches":
		name, _ := url.PathUnescape(rest[2])
		sha, ok := p.branches[name]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Branch Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name": name, "merged": false, "protected": true, "default": name == p.defaultBranch,
			"developers_can_push": false, "developers_can_merge": false, "can_push": false,
			"web_url": g.URL + "/" + p.path + "/-/tree/" + name, "commit": commitDoc(g.URL, p, sha),
		})
	case len(rest) == 3 && rest[0] == "repository" && rest[1] == "commits":
		sha := rest[2]
		if _, ok := p.commits[sha]; !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Commit Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, commitDoc(g.URL, p, sha))
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "merge_base":
		g.mergeBase(w, r, p)
	case len(rest) == 2 && rest[0] == "repository" && rest[1] == "tree":
		g.tree(w, r, p)
	case len(rest) == 4 && rest[0] == "repository" && rest[1] == "blobs" && rest[3] == "raw":
		b, ok := g.blobs[rest[2]]
		if t, bad := g.tampered[rest[2]]; bad {
			b = t
		}
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Blob Not Found"})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(b)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "404 Not Found"})
	}
}

// mergeBase answers GET /projects/:id/repository/merge_base?refs[]=A&refs[]=B.
// A ref is a commit id, a branch name, or refs/heads/NAME. A ref it cannot
// find is 400 and two commits with no common history are 404, as GitLab's
// own handler answers; that GitLab takes a full ref here is not verified
// against a real instance.
func (g *GitLab) mergeBase(w http.ResponseWriter, r *http.Request, p *project) {
	refs := r.URL.Query()["refs[]"]
	if len(refs) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Provide at least 2 refs"})
		return
	}
	var heads [2]string
	for i, ref := range refs {
		if _, ok := p.commits[ref]; ok {
			heads[i] = ref
		} else if sha, ok := p.branches[strings.TrimPrefix(ref, "refs/heads/")]; ok {
			heads[i] = sha
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Could not find ref: " + ref})
			return
		}
	}
	ancestors := map[string]bool{}
	for c := heads[0]; c != ""; c = p.parents[c] {
		ancestors[c] = true
	}
	for c := heads[1]; c != ""; c = p.parents[c] {
		if ancestors[c] {
			writeJSON(w, http.StatusOK, commitDoc(g.URL, p, c))
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Merge Base Not Found"})
}

func (g *GitLab) project(w http.ResponseWriter, p *project) {
	slash := strings.LastIndexByte(p.path, '/')
	writeJSON(w, http.StatusOK, map[string]any{
		"id": p.id, "description": nil, "name": p.path[slash+1:], "name_with_namespace": strings.ReplaceAll(p.path, "/", " / "),
		"path": p.path[slash+1:], "path_with_namespace": p.path, "created_at": "2026-11-01T10:00:00.000Z",
		"default_branch": p.defaultBranch, "visibility": "private",
		"ssh_url_to_repo": "git@example.com:" + p.path + ".git", "http_url_to_repo": g.URL + "/" + p.path + ".git",
		"web_url": g.URL + "/" + p.path, "empty_repo": len(p.commits) == 0, "archived": false,
	})
}

func commitDoc(base string, p *project, sha string) map[string]any {
	return map[string]any{
		"id": sha, "short_id": sha[:8], "title": "a commit", "message": "a commit\n",
		"author_name": "Customer A", "author_email": "user@example.com", "authored_date": "2026-11-10T09:00:00.000+00:00",
		"committer_name": "Customer A", "committer_email": "user@example.com", "committed_date": "2026-11-10T09:00:00.000+00:00",
		"created_at": "2026-11-10T09:00:00.000+00:00", "parent_ids": []string{},
		"web_url": base + "/" + p.path + "/-/commit/" + sha,
	}
}

// tree answers GET /projects/:id/repository/tree with recursive=true and
// keyset pagination: each page's Link header names the next page by an
// opaque page_token, here the path of the page's last entry.
func (g *GitLab) tree(w http.ResponseWriter, r *http.Request, p *project) {
	q := r.URL.Query()
	entries, ok := p.commits[q.Get("ref")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Tree Not Found"})
		return
	}
	dir := q.Get("path")
	type item struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	// Directories come before the files under them, as GitLab lists them.
	var all []item
	seen := map[string]bool{}
	for _, e := range entries {
		if dir != "" && !strings.HasPrefix(e.path, dir+"/") {
			continue
		}
		segs := strings.Split(e.path, "/")
		for i := 1; i < len(segs); i++ {
			d := strings.Join(segs[:i], "/")
			if seen[d] || dir != "" && len(d) <= len(dir) {
				continue
			}
			seen[d] = true
			sum := sha1.Sum([]byte("tree " + d))
			all = append(all, item{ID: hex.EncodeToString(sum[:]), Name: segs[i-1], Type: "tree", Path: d, Mode: "040000"})
		}
		all = append(all, item{ID: e.blob, Name: segs[len(segs)-1], Type: "blob", Path: e.path, Mode: e.mode})
	}
	if len(all) == 0 && dir != "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Tree Not Found"})
		return
	}
	size := 20
	if n, err := strconv.Atoi(q.Get("per_page")); err == nil && n > 0 {
		size = min(n, 100)
	}
	if g.pageSize > 0 {
		size = g.pageSize
	}
	start := 0
	if tok := q.Get("page_token"); tok != "" {
		start = slices.IndexFunc(all, func(it item) bool { return it.Path == tok }) + 1
		if start == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "400 Bad request - invalid page_token"})
			return
		}
	}
	end := min(start+size, len(all))
	page := all[start:end]
	if end < len(all) && q.Get("pagination") == "keyset" {
		next := url.Values{}
		for k, v := range q {
			next[k] = v
		}
		next.Set("page_token", page[len(page)-1].Path)
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, g.URL, r.URL.EscapedPath(), next.Encode()))
	}
	writeJSON(w, http.StatusOK, page)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
