package forgetest

import (
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// GitHub is a running stand-in for GitHub's REST API, as GitHub
// Enterprise Server serves it under /api/v3/: the repository, ref, Git
// commit, Git tree, compare, and raw Git blob endpoints that writ serve
// reads, with the response shapes of GitHub's REST API documentation
// (docs.github.com/rest/repos/repos, /rest/git/refs, /rest/git/commits,
// /rest/git/trees, /rest/git/blobs, and /rest/commits/commits, read
// 2026-11-22). A repository is read by OWNER/REPO under /repos/ and by id
// under /repositories/. A repository the token may not see answers 404,
// as GitHub answers.
type GitHub struct {
	URL string
	// Token is the one token it accepts, as Authorization: Bearer.
	Token string
	*host
	// trees maps each tree id the stand-in sent to its commit and
	// directory.
	trees map[string]treeRef
}

type treeRef struct {
	project      *project
	commit, path string
}

// NewGitHub starts a GitHub stand-in that accepts token, and stops it
// when the test ends. Its URL is the instance's; the API is at
// URL/api/v3.
func NewGitHub(t testing.TB, token string) *GitHub {
	g := GitHubHandler(token)
	mux := http.NewServeMux()
	mux.Handle("/api/v3/", g)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	g.URL = srv.URL
	return g
}

// GitHubHandler makes a GitHub stand-in to mount under /api/v3/ on
// another test server, such as an OIDC issuer's, as GitHub Enterprise
// Server serves both from one host. The caller sets URL.
func GitHubHandler(token string) *GitHub {
	return &GitHub{Token: token, host: newHost(), trees: map[string]treeRef{}}
}

// ServeHTTP answers the API's calls.
func (g *GitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, r.Method+" "+r.URL.EscapedPath())
	if r.Header.Get("Authorization") != "Bearer "+g.Token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials", "documentation_url": "https://docs.github.com/rest"})
		return
	}
	notFound := func() {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found", "documentation_url": "https://docs.github.com/rest"})
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/api/v3/"), "/")
	if r.Method != http.MethodGet || len(parts) < 2 {
		notFound()
		return
	}
	var p *project
	var rest []string
	switch {
	case parts[0] == "repos" && len(parts) == 3:
		full, _ := url.PathUnescape(parts[1] + "/" + parts[2])
		for _, q := range g.projects {
			if strings.EqualFold(q.path, full) {
				p = q
			}
		}
	case parts[0] == "repositories":
		p, rest = g.projects[parts[1]], parts[2:]
	}
	if p == nil {
		notFound()
		return
	}
	switch {
	case len(rest) == 0:
		writeJSON(w, http.StatusOK, map[string]any{
			"id": p.id, "node_id": "R_kgDO" + strconv.Itoa(p.id), "name": p.path[strings.LastIndexByte(p.path, '/')+1:], "full_name": p.path,
			"private": true, "html_url": g.URL + "/" + p.path, "default_branch": p.defaultBranch,
			"clone_url": g.URL + "/" + p.path + ".git", "visibility": "private", "archived": false,
		})
	case len(rest) >= 3 && rest[0] == "git" && rest[1] == "ref":
		ref, _ := url.PathUnescape(strings.Join(rest[2:], "/"))
		sha, ok := p.branches[strings.TrimPrefix(ref, "heads/")]
		if !ok || !strings.HasPrefix(ref, "heads/") {
			notFound()
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ref": "refs/" + ref, "node_id": "REF_x", "url": g.URL + "/api/v3/repos/" + p.path + "/git/refs/" + ref,
			"object": map[string]any{"sha": sha, "type": "commit", "url": g.URL + "/api/v3/repos/" + p.path + "/git/commits/" + sha},
		})
	case len(rest) == 3 && rest[0] == "git" && rest[1] == "commits":
		if _, ok := p.commits[rest[2]]; !ok {
			notFound()
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"sha": rest[2], "node_id": "C_x", "message": "a commit\n", "parents": []any{},
			"author": map[string]any{"name": "Customer A", "email": "user@example.com", "date": "2026-11-22T09:00:00Z"},
			"tree":   map[string]any{"sha": g.treeID(p, rest[2], ""), "url": g.URL + "/api/v3/repos/" + p.path + "/git/trees/x"},
		})
	case len(rest) == 3 && rest[0] == "git" && rest[1] == "trees":
		t, ok := g.trees[rest[2]]
		if !ok || t.project != p {
			notFound()
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sha": rest[2], "truncated": false, "tree": g.list(t, r.URL.Query().Get("recursive") != "")})
	case len(rest) == 2 && rest[0] == "compare":
		base, head, ok := strings.Cut(rest[1], "...")
		if _, okb := p.commits[base]; !ok || !okb {
			notFound()
			return
		}
		if _, okh := p.commits[head]; !okh {
			notFound()
			return
		}
		ancestors := map[string]bool{}
		for c := base; c != ""; c = p.parents[c] {
			ancestors[c] = true
		}
		for c := head; c != ""; c = p.parents[c] {
			if ancestors[c] {
				writeJSON(w, http.StatusOK, map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": c}, "commits": []any{}, "files": []any{}})
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No common ancestor between " + base + " and " + head + "."})
	case len(rest) == 3 && rest[0] == "git" && rest[1] == "blobs":
		b, ok := g.blobs[rest[2]]
		if t, bad := g.tampered[rest[2]]; bad {
			b = t
		}
		if !ok {
			notFound()
			return
		}
		if r.Header.Get("Accept") != "application/vnd.github.raw+json" {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"message": "the stand-in serves blobs raw only"})
			return
		}
		w.Header().Set("Content-Type", "application/vnd.github.raw")
		_, _ = w.Write(b)
	default:
		notFound()
	}
}

// treeID is the id of the tree of dir in commit, which the stand-in
// makes up and remembers.
func (g *GitHub) treeID(p *project, commit, dir string) string {
	sum := sha1.Sum([]byte("tree " + commit + "\x00" + dir))
	id := hex.EncodeToString(sum[:])
	g.trees[id] = treeRef{project: p, commit: commit, path: dir}
	return id
}

// list is a tree's entries, named from its directory: one level, or every
// entry under it, a directory before the entries under it.
func (g *GitHub) list(t treeRef, recursive bool) []map[string]any {
	var out []map[string]any
	seen := map[string]bool{}
	for _, e := range t.project.commits[t.commit] {
		rel := e.path
		if t.path != "" {
			var ok bool
			if rel, ok = strings.CutPrefix(e.path, t.path+"/"); !ok {
				continue
			}
		}
		segs := strings.Split(rel, "/")
		for i := 1; i < len(segs); i++ {
			d := strings.Join(segs[:i], "/")
			if seen[d] || !recursive && i > 1 {
				continue
			}
			seen[d] = true
			out = append(out, map[string]any{"path": d, "mode": "040000", "type": "tree", "sha": g.treeID(t.project, t.commit, joinPath(t.path, d))})
		}
		if recursive || len(segs) == 1 {
			out = append(out, map[string]any{"path": rel, "mode": e.mode, "type": "blob", "sha": e.blob, "size": len(g.blobs[e.blob])})
		}
	}
	slices.SortStableFunc(out, func(a, b map[string]any) int { return strings.Compare(a["path"].(string), b["path"].(string)) })
	return out
}

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
