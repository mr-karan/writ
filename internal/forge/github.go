package forge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// GitHubOptions configure a GitHub client.
type GitHubOptions struct {
	// URL is the instance: https://github.com, or a GitHub Enterprise
	// Server's https://HOST.
	URL string
	// TokenFile holds the read token: a fine-grained personal access
	// token with Contents: read and Metadata: read on the repositories.
	// The client reads it for each call, so a rotated token needs no
	// restart.
	TokenFile string
	// CAFile names extra roots for the instance's certificate; "" trusts
	// the system roots.
	CAFile string
}

// GitHubURL is github.com, the URL of a GitHub forge that names none.
const GitHubURL = "https://github.com"

// GitHub reads repositories through GitHub's REST API. Every call after
// Project names the repository by its numeric id, under
// /repositories/ID, so a renamed or transferred repository is still the
// one that was added.
type GitHub struct{ rest *rest }

// githubAPIVersion is the REST API version every call asks for.
const githubAPIVersion = "2022-11-28"

// GitHubAPI is the REST API's base for a GitHub instance at u:
// api.github.com for github.com, and HOST/api/v3 for GitHub Enterprise
// Server.
func GitHubAPI(u string) (string, error) {
	pu, err := url.Parse(u)
	if err != nil || pu.Host == "" || pu.User != nil || pu.RawQuery != "" || pu.Fragment != "" || strings.Trim(pu.Path, "/") != "" {
		return "", fmt.Errorf("forge URL %q is not a URL of a host, such as https://github.com", trunc(u))
	}
	// The token goes to this host, so only HTTPS, or HTTP to a loopback
	// address, as a test's stand-in serves it.
	if pu.Scheme != "https" && !(pu.Scheme == "http" && loopback(pu.Hostname())) {
		return "", fmt.Errorf("forge URL %q must be https://, or http:// on a loopback address", trunc(u))
	}
	if pu.Scheme == "https" && strings.EqualFold(pu.Host, "github.com") {
		return "https://api.github.com", nil
	}
	return pu.Scheme + "://" + pu.Host + "/api/v3", nil
}

func NewGitHub(o GitHubOptions) (*GitHub, error) {
	api, err := GitHubAPI(o.URL)
	if err != nil {
		return nil, err
	}
	client, err := newHTTPClient(o.CAFile)
	if err != nil {
		return nil, err
	}
	return &GitHub{rest: &rest{
		api: api, tokenFile: o.TokenFile, http: client,
		auth: func(h http.Header, tok string) {
			h.Set("Authorization", "Bearer "+tok)
			h.Set("X-GitHub-Api-Version", githubAPIVersion)
			h.Set("User-Agent", "writ")
		},
		need:    "Contents: read and Metadata: read on the repository",
		limitOf: githubLimit,
	}}, nil
}

// githubRepoRE is a repository's OWNER/REPO, as GitHub allows them.
var githubRepoRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)

var githubIDRE = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

func repoPath(id string) string { return "/repositories/" + url.PathEscape(id) }

// refPath is a full ref's path under git/ref/, each part escaped and the
// slashes kept, as GitHub's ref endpoint takes it.
func refPath(ref string) string {
	parts := strings.Split(strings.TrimPrefix(ref, "refs/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// The JSON of GitHub's answers, the fields Writ reads.
type (
	githubRepo struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		HTMLURL       string `json:"html_url"`
	}
	githubRef struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	githubCommit struct {
		SHA  string `json:"sha"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	githubTree struct {
		SHA       string `json:"sha"`
		Truncated bool   `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	githubCompare struct {
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
)

// Project reads a repository by its OWNER/REPO, such as acme/infra, or
// its numeric id.
func (g *GitHub) Project(ctx context.Context, ref string) (Project, error) {
	var p string
	switch {
	case githubIDRE.MatchString(ref):
		p = repoPath(ref)
	case githubRepoRE.MatchString(ref):
		owner, name, _ := strings.Cut(ref, "/")
		p = "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	default:
		return Project{}, fmt.Errorf("repository %q must be OWNER/REPO, such as acme/infra, or its numeric id", trunc(ref))
	}
	var r githubRepo
	err := g.rest.call(func(tok string) error {
		_, err := g.rest.getJSON(ctx, tok, "read repository "+ref, p, nil, maxDoc, &r)
		return err
	})
	if err != nil {
		return Project{}, err
	}
	if r.ID <= 0 {
		return Project{}, fmt.Errorf("read repository %s: the answer has no id", ref)
	}
	if r.DefaultBranch == "" {
		return Project{}, fmt.Errorf("repository %s has no default branch", r.FullName)
	}
	return Project{ID: strconv.FormatInt(r.ID, 10), Path: r.FullName, DefaultBranch: r.DefaultBranch, WebURL: r.HTMLURL}, nil
}

// ref reads the commit a full ref points to.
func (g *GitHub) ref(ctx context.Context, tok, projectID, ref string) (string, error) {
	var r githubRef
	what := "read " + ref
	if _, err := g.rest.getJSON(ctx, tok, what, repoPath(projectID)+"/git/ref/"+refPath(ref), nil, maxDoc, &r); err != nil {
		return "", err
	}
	// The ref endpoint answers one ref, the one asked for; any other is
	// refused, so a prefix match cannot stand in for it.
	if r.Ref != ref || r.Object.Type != "commit" || !IsSHA(r.Object.SHA) {
		return "", fmt.Errorf("%s: the answer names no commit of that ref", what)
	}
	return r.Object.SHA, nil
}

// Head reads the commit a branch points to: one call.
func (g *GitHub) Head(ctx context.Context, projectID, branch string) (string, error) {
	var sha string
	err := g.rest.call(func(tok string) error {
		var err error
		sha, err = g.ref(ctx, tok, projectID, "refs/heads/"+branch)
		return err
	})
	return sha, err
}

// commit reads a commit by its full id and checks the answer names it.
func (g *GitHub) commit(ctx context.Context, tok, projectID, sha string) (githubCommit, error) {
	var c githubCommit
	if _, err := g.rest.getJSON(ctx, tok, "read commit "+sha, repoPath(projectID)+"/git/commits/"+sha, nil, maxDoc, &c); err != nil {
		return c, err
	}
	if !IsSHA(c.SHA) {
		return c, fmt.Errorf("read commit %s: the answer names no commit", sha)
	}
	if c.SHA != sha {
		return c, fmt.Errorf("read commit %s: the answer names another commit, %s", sha, Short(c.SHA))
	}
	return c, nil
}

// Commit checks that a commit exists and returns its full id. It asks
// only for full ids and takes only an answer naming that same commit.
func (g *GitHub) Commit(ctx context.Context, projectID, sha string) (string, error) {
	if !IsSHA(sha) {
		return "", fmt.Errorf("commit %q is not a full SHA-1", trunc(sha))
	}
	err := g.rest.call(func(tok string) error {
		_, err := g.commit(ctx, tok, projectID, sha)
		return err
	})
	if err != nil {
		return "", err
	}
	return sha, nil
}

// OnRef reads the ref's commit, and when it is another commit, compares
// the two: the commit is on the ref when it is their merge base. A ref or
// a comparison GitHub has not means the commit is not on it.
func (g *GitHub) OnRef(ctx context.Context, projectID, sha, ref string) (bool, error) {
	if !IsSHA(sha) || !strings.HasPrefix(ref, "refs/") {
		return false, fmt.Errorf("on ref: %q is not a full SHA-1 or %q not a full ref", trunc(sha), trunc(ref))
	}
	var on bool
	err := g.rest.call(func(tok string) error {
		head, err := g.ref(ctx, tok, projectID, ref)
		if err != nil {
			return err
		}
		if head == sha {
			on = true
			return nil
		}
		var c githubCompare
		what := "compare " + Short(sha) + " with " + ref
		// One commit a page: Writ reads only the merge base.
		if _, err := g.rest.getJSON(ctx, tok, what, repoPath(projectID)+"/compare/"+sha+"..."+head, url.Values{"per_page": {"1"}}, maxDoc, &c); err != nil {
			return err
		}
		if !IsSHA(c.MergeBase.SHA) {
			return fmt.Errorf("%s: the answer names no merge base", what)
		}
		on = c.MergeBase.SHA == sha
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		// The repository was found before: 404 is no such ref, or no
		// common history.
		return false, nil
	}
	return on, err
}

// Tree lists a commit's files under dir. It walks from the commit's tree
// to dir's, one level a call, then lists dir's tree recursively in one
// call. It refuses a listing GitHub cut short, or of over MaxEntries.
func (g *GitHub) Tree(ctx context.Context, projectID, sha, dir string) ([]File, error) {
	if !IsSHA(sha) {
		return nil, fmt.Errorf("commit %q is not a full SHA-1", trunc(sha))
	}
	what := "read the tree of " + Short(sha)
	var out []File
	err := g.rest.call(func(tok string) error {
		c, err := g.commit(ctx, tok, projectID, sha)
		if err != nil {
			return err
		}
		tree := c.Tree.SHA
		if dir != "" {
			for seg := range strings.SplitSeq(dir, "/") {
				if tree, err = g.subtree(ctx, tok, projectID, tree, seg, what); err != nil {
					return err
				}
			}
		}
		t, err := g.tree(ctx, tok, projectID, tree, true, what)
		if err != nil {
			return err
		}
		for _, e := range t.Tree {
			if e.Type != "blob" {
				continue
			}
			rel, err := under("", e.Path)
			if err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
			if e.Mode == "120000" {
				return fmt.Errorf("%s: %s is a symbolic link, which writ serve does not follow", what, path.Join(dir, rel))
			}
			if !IsSHA(e.SHA) {
				return fmt.Errorf("%s: %s has no blob id", what, path.Join(dir, rel))
			}
			out = append(out, File{Path: rel, Blob: e.SHA})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// tree reads one tree by its id, recursively or one level.
func (g *GitHub) tree(ctx context.Context, tok, projectID, sha string, recursive bool, what string) (githubTree, error) {
	var t githubTree
	if !IsSHA(sha) {
		return t, fmt.Errorf("%s: the answer names no tree", what)
	}
	var q url.Values
	if recursive {
		q = url.Values{"recursive": {"1"}}
	}
	if _, err := g.rest.getJSON(ctx, tok, what, repoPath(projectID)+"/git/trees/"+sha, q, maxDoc, &t); err != nil {
		return t, err
	}
	if t.Truncated {
		return t, fmt.Errorf("%s: GitHub cut the listing short, so it is over what one answer holds", what)
	}
	if len(t.Tree) > MaxEntries {
		return t, fmt.Errorf("%s: it lists over %d entries", what, MaxEntries)
	}
	return t, nil
}

// subtree is the id of the directory named seg in the tree sha.
func (g *GitHub) subtree(ctx context.Context, tok, projectID, sha, seg, what string) (string, error) {
	t, err := g.tree(ctx, tok, projectID, sha, false, what)
	if err != nil {
		return "", err
	}
	for _, e := range t.Tree {
		if e.Path != seg {
			continue
		}
		if e.Mode == "120000" {
			return "", fmt.Errorf("%s: %s is a symbolic link, which writ serve does not follow", what, seg)
		}
		if e.Type == "tree" {
			return e.SHA, nil
		}
	}
	return "", fmt.Errorf("%s: it has no directory %s: %w", what, seg, ErrNotFound)
}

// Blob reads a file's content by its blob id, raw.
func (g *GitHub) Blob(ctx context.Context, projectID, blob string, max int64) ([]byte, error) {
	if !IsSHA(blob) {
		return nil, fmt.Errorf("blob id %q is not a SHA-1", trunc(blob))
	}
	var b []byte
	err := g.rest.call(func(tok string) error {
		var err error
		b, _, err = g.rest.get(ctx, tok, "read blob "+Short(blob), repoPath(projectID)+"/git/blobs/"+blob, nil, "application/vnd.github.raw+json", max)
		return err
	})
	if err != nil {
		return nil, err
	}
	return b, nil
}
